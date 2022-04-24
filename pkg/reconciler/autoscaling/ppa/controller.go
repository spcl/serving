package ppa

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	network "knative.dev/networking/pkg"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	filteredpodinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/pod/filtered"
	"knative.dev/pkg/system"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	asmetrics "knative.dev/serving/pkg/autoscaler/metrics"
	"knative.dev/serving/pkg/client/injection/ducks/autoscaling/v1alpha1/podscalable"
	"knative.dev/serving/pkg/reconciler/metric"
	"knative.dev/serving/pkg/resources"
	"runtime"

	networkingclient "knative.dev/networking/pkg/client/injection/client"
	sksinformer "knative.dev/networking/pkg/client/injection/informers/networking/v1alpha1/serverlessservice"
	"knative.dev/pkg/logging"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	metricinformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/metric"
	painformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/podautoscaler"
	pareconciler "knative.dev/serving/pkg/client/injection/reconciler/autoscaling/v1alpha1/podautoscaler"
	"knative.dev/serving/pkg/deployment"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/serving/pkg/apis/autoscaling"
	"knative.dev/serving/pkg/autoscaler/config/autoscalerconfig"
	areconciler "knative.dev/serving/pkg/reconciler/autoscaling"
	"knative.dev/serving/pkg/reconciler/autoscaling/config"
)

// NewController returns a new PPA reconcile controller.
func NewController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {
	logger := logging.FromContext(ctx)
	logger.Info("Create new PPA controller")

	onlyPPAClass := pkgreconciler.AnnotationFilterFunc(autoscaling.ClassAnnotationKey, autoscaling.PPA, false)

	// Copied from HPA (hopefully for now)
	paInformer := painformer.Get(ctx)
	sksInformer := sksinformer.Get(ctx)
	metricInformer := metricinformer.Get(ctx)
	psInformerFactory := podscalable.Get(ctx)
	podsInformer := filteredpodinformer.Get(ctx, serving.RevisionUID)

	c := &Reconciler{
		Base: &areconciler.Base{
			Client:           servingclient.Get(ctx),
			NetworkingClient: networkingclient.Get(ctx),
			SKSLister:        sksInformer.Lister(),
			MetricLister:     metricInformer.Lister(),
		},
		podsLister: podsInformer.Lister(),
	}

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
			logger.Infof("pa Informer called the handler")
			impl.Enqueue(obj)
		}),
	})

	onlyPAControlled := controller.FilterController(&autoscalingv1alpha1.PodAutoscaler{})
	handleMatchingControllers := cache.FilteringResourceEventHandler{
		FilterFunc: pkgreconciler.ChainFilterFuncs(onlyPPAClass, onlyPAControlled),
		Handler: controller.HandleAll(func(obj interface{}) {
			logger.Info("Handle matching controllers")
			impl.EnqueueControllerOf(obj)
		}),
	}
	sksInformer.Informer().AddEventHandler(handleMatchingControllers)
	metricInformer.Informer().AddEventHandler(handleMatchingControllers)

	// Watch the knative pods.
	podsInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: pkgreconciler.LabelExistsFilterFunc(serving.RevisionLabelKey),
		Handler:    controller.HandleAll(impl.EnqueueLabelOfNamespaceScopedResource("", serving.RevisionLabelKey)),
	})

	return impl
}

func NewMetricController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {
	logger := logging.FromContext(ctx)

	logger.Info("New custom metric controller")

	podLister := filteredpodinformer.Get(ctx, serving.RevisionUID).Lister()
	networkCM, err := kubeclient.Get(ctx).CoreV1().ConfigMaps(system.Namespace()).Get(ctx, network.ConfigName, metav1.GetOptions{})
	if err != nil {
		logger.Fatalw("Failed to fetch network config", zap.Error(err))
	}
	networkConfig, err := network.NewConfigFromConfigMap(networkCM)
	if err != nil {
		logger.Fatalw("Failed to construct network config", zap.Error(err))
	}
	collector := asmetrics.NewMetricCollector(
		statsScraperFactoryFunc(podLister, networkConfig.EnableMeshPodAddressability, networkConfig.MeshCompatibilityMode),
		logger)
	return metric.NewController(ctx, cmw, collector)

	//metricInformer := metricinformer.Get(ctx)
	//c := &reconciler{
	//	collector: collector,
	//}
	//impl := metricreconciler.NewImpl(ctx, c)
	//
	//// Watch all the Metric objects.
	//metricInformer.Informer().AddEventHandler(
	//	controller.HandleAll(func(obj interface{}) {
	//		logger.Info("Event by metric informer in custom metric controller")
	//		impl.Enqueue(obj)
	//	}))
	//
	//collector.Watch(func(name types.NamespacedName) {
	//	logger.Info("Event by collector in custom metric controller")
	//	impl.EnqueueKey(name)
	//})
	//
	//return impl
}

func statsScraperFactoryFunc(podLister corev1listers.PodLister, usePassthroughLb bool, meshMode network.MeshCompatibilityMode) asmetrics.StatsScraperFactory {
	return func(metric *autoscalingv1alpha1.Metric, logger *zap.SugaredLogger) (asmetrics.StatsScraper, error) {
		logger.Info("Create custom stat scraper for ", metric.Name)
		buf := make([]byte, 1<<16)
		runtime.Stack(buf, true)
		logger.Infof("Stacktrace: %s", buf)
		if metric.Spec.ScrapeTarget == "" {
			return nil, nil
		}

		revisionName := metric.Labels[serving.RevisionLabelKey]
		if revisionName == "" {
			return nil, fmt.Errorf("label %q not found or empty in Metric %s", serving.RevisionLabelKey, metric.Name)
		}

		podAccessor := resources.NewPodAccessor(podLister, metric.Namespace, revisionName)
		return asmetrics.NewCustomStatsScraper(metric, revisionName, podAccessor, usePassthroughLb, meshMode, logger), nil
	}
}
