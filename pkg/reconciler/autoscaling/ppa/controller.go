package ppa

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	networkingclient "knative.dev/networking/pkg/client/injection/client"
	sksinformer "knative.dev/networking/pkg/client/injection/informers/networking/v1alpha1/serverlessservice"
	"knative.dev/pkg/apis/duck"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	filteredpodinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/pod/filtered"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/serving/pkg/apis/autoscaling"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	"knative.dev/serving/pkg/autoscaler/config/autoscalerconfig"
	asmetrics "knative.dev/serving/pkg/autoscaler/metrics"
	"knative.dev/serving/pkg/autoscaler/scaling"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	"knative.dev/serving/pkg/client/injection/ducks/autoscaling/v1alpha1/podscalable"
	metricinformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/metric"
	painformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/podautoscaler"
	pareconciler "knative.dev/serving/pkg/client/injection/reconciler/autoscaling/v1alpha1/podautoscaler"
	"knative.dev/serving/pkg/client/listers/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/deployment"
	areconciler "knative.dev/serving/pkg/reconciler/autoscaling"
	"knative.dev/serving/pkg/reconciler/autoscaling/config"
	"knative.dev/serving/pkg/reconciler/metric"
)

type CollectorKey struct{}

func WithCollector(ctx context.Context) context.Context {
	logger := logging.FromContext(ctx)
	podLister := filteredpodinformer.Get(ctx, serving.RevisionUID).Lister()
	collector := asmetrics.NewFullMetricCollector(statsScraperFactoryFunc(podLister), logger)

	return context.WithValue(ctx, CollectorKey{}, collector)
}

func GetCollector(ctx context.Context) *asmetrics.FullMetricCollector {
	untyped := ctx.Value(CollectorKey{})
	if untyped == nil {
		logging.FromContext(ctx).Panic("Unable to fetch metrics from context.")
	}
	return untyped.(*asmetrics.FullMetricCollector)
}

// NewController returns a new PPA reconcile controller.
func NewController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {
	logger := logging.FromContext(ctx)
	logger.Info("Create new PPA controller")

	paInformer := painformer.Get(ctx)
	sksInformer := sksinformer.Get(ctx)
	kubeClient := kubeclient.Get(ctx).CoreV1()
	metricInformer := metricinformer.Get(ctx)
	psInformerFactory := podscalable.Get(ctx)
	podsInformer := filteredpodinformer.Get(ctx, serving.RevisionUID)
	podLister := podsInformer.Lister()
	collector := GetCollector(ctx)

	multiScaler := scaling.NewMultiScaler(ctx.Done(),
		uniScalerFactoryFunc(ctx, collector, logger, psInformerFactory, paInformer.Lister()),
		logger,
	)

	c := &Reconciler{
		Base: &areconciler.Base{
			Client:           servingclient.Get(ctx),
			NetworkingClient: networkingclient.Get(ctx),
			SKSLister:        sksInformer.Lister(),
			MetricLister:     metricInformer.Lister(),
		},
		podsLister: podLister,
		collector:  collector,
		deciders:   multiScaler,
		kubeClient: kubeClient,
	}

	onlyPPAClass := pkgreconciler.AnnotationFilterFunc(autoscaling.ClassAnnotationKey, autoscaling.PPA, false)
	impl := pareconciler.NewImpl(ctx, c, autoscaling.PPA, func(impl *controller.Impl) controller.Options {
		logger.Info("Setting up ConfigMap receivers")
		configsToResync := []interface{}{
			&autoscalerconfig.Config{},
			&deployment.Config{},
		}
		resync := configmap.TypeFilter(configsToResync...)(func(string, interface{}) {
			impl.FilteredGlobalResync(onlyPPAClass, paInformer.Informer())
		})
		configStore := config.NewStore(logger.Named("config-store"), resync)
		configStore.WatchConfigs(cmw)
		return controller.Options{ConfigStore: configStore}
	})
	c.scaler = newScaler(ctx, psInformerFactory, impl.EnqueueAfter)

	// And here we should set up some event handlers
	logger.Info("Setting up PPA Class event handlers")

	paInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: onlyPPAClass,
		// Handler:    controller.HandleAll(impl.Enqueue),
		Handler: controller.HandleAll(func(obj interface{}) {
			// logger.Infof("pa Informer called the handler")
			impl.Enqueue(obj)
		}),
	})

	onlyPAControlled := controller.FilterController(&autoscalingv1alpha1.PodAutoscaler{})
	handleMatchingControllers := cache.FilteringResourceEventHandler{
		FilterFunc: pkgreconciler.ChainFilterFuncs(onlyPPAClass, onlyPAControlled),
		Handler: controller.HandleAll(func(obj interface{}) {
			// logger.Info("SKS event")
			impl.EnqueueControllerOf(obj)
		}),
	}
	sksInformer.Informer().AddEventHandler(handleMatchingControllers)

	// Watch the knative pods.
	handlerFunc := impl.EnqueueLabelOfNamespaceScopedResource("", serving.RevisionLabelKey)
	podsInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: pkgreconciler.LabelExistsFilterFunc(serving.RevisionLabelKey),
		Handler: controller.HandleAll(func(obj interface{}) {
			queueLen := impl.WorkQueue().Len()
			// Only handle this if there are no other work items
			if queueLen == 0 {
				// logger.Infof("podsInformer event")
				handlerFunc(obj)
			} else {
				logger.Debug("There are items in the work queue podsInformer was ignored")
			}
		}),
	})

	// Have the Deciders enqueue the PAs whose decisions have changed.
	multiScaler.Watch(func(k types.NamespacedName) {
		// logger.Info("Decider queued PPA")
		impl.EnqueueKey(k)
	})

	return impl
}

func NewMetricController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {
	collector := GetCollector(ctx)
	return metric.NewCustomController(ctx, cmw, collector)
}

func uniScalerFactoryFunc(
	ctx context.Context,
	collector *asmetrics.FullMetricCollector,
	logger *zap.SugaredLogger,
	psInformer duck.InformerFactory,
	paLister v1alpha1.PodAutoscalerLister,
) scaling.UniScalerFactory {
	return func(decider *scaling.Decider) (scaling.UniScaler, error) {
		key := types.NamespacedName{
			Namespace: decider.Namespace,
			Name:      decider.Name,
		}
		return MakeDecider(ctx, logger, collector, key, psInformer, paLister), nil
	}
}

func statsScraperFactoryFunc(podLister corev1listers.PodLister) asmetrics.FullStatsScraperFactory {
	return func(metric *autoscalingv1alpha1.Metric, logger *zap.SugaredLogger) (asmetrics.FullStatsScraper, error) {
		if metric.Spec.ScrapeTarget == "" {
			return nil, nil
		}

		revisionName := metric.Labels[serving.RevisionLabelKey]
		if revisionName == "" {
			return nil, fmt.Errorf("label %q not found or empty in Metric %s", serving.RevisionLabelKey, metric.Name)
		}

		podAccessor := asmetrics.NewCustomPodLister(podLister, metric.Namespace, revisionName)
		return asmetrics.NewCustomStatsScraper(metric, revisionName, podAccessor, logger), nil
	}
}
