package ppa

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	nv1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	pkgreconciler "knative.dev/pkg/reconciler"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	"knative.dev/serving/pkg/autoscaler/metrics"
	pareconciler "knative.dev/serving/pkg/client/injection/reconciler/autoscaling/v1alpha1/podautoscaler"
	areconciler "knative.dev/serving/pkg/reconciler/autoscaling"
	"knative.dev/serving/pkg/reconciler/autoscaling/config"
	anames "knative.dev/serving/pkg/reconciler/autoscaling/resources/names"
	resourceutil "knative.dev/serving/pkg/resources"
	"time"
)

// Reconciler implements the control loop for the PPA resources.
type Reconciler struct {
	*areconciler.Base

	podsLister corev1listers.PodLister
	scaler     *scaler
	collector  *metrics.FullMetricCollector
	kubeClient v1.CoreV1Interface
}

// Check that our Reconciler implements pareconciler.Interface
var _ pareconciler.Interface = (*Reconciler)(nil)

// ReconcileKind implements Interface.ReconcileKind.
func (c *Reconciler) ReconcileKind(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) pkgreconciler.Event {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	logger := logging.FromContext(ctx)

	// decider, err := c.reconcileDecider(ctx, pa)

	// Compare the desired and observed resources to determine our situation.
	podCounter := resourceutil.NewPodAccessor(c.podsLister, pa.Namespace, pa.Labels[serving.RevisionLabelKey])
	ready, notReady, pending, terminating, err := podCounter.PodCountsByState()
	if err != nil {
		return fmt.Errorf("error counting pods: %w", err)
	}

	if ready >= 1 && !pa.Status.IsScaleTargetInitialized() {
		pa.Status.MarkScaleTargetInitialized()
	}

	sksName := anames.SKS(pa.Name)
	sks, err := c.SKSLister.ServerlessServices(pa.Namespace).Get(sksName)
	if err != nil && !errors.IsNotFound(err) {
		logger.Warnw("Error retrieving SKS for Scaler", zap.Error(err))
	}

	metricKey := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
	latestStats, err := c.collector.LatestCustomStats(metricKey)
	if err == nil && latestStats != nil {
		podNames := make([]string, 0)
		for _, stat := range latestStats {
			if stat.CanDownScale {
				podNames = append(podNames, stat.PodName)
			}
		}
		desiredScale, err := c.scaler.scaleDownPods(ctx, c.kubeClient, pa, sks, podNames)
		if err != nil {
			return fmt.Errorf("error deleted pods: %v (%v)", podNames, err)
		}
		pa.Status.DesiredScale = ptr.Int32(desiredScale)
	}

	logger.Infof("PPA: %d is ready, %d is not ready, %d is pending, %d is terminating", ready, notReady, pending, terminating)

	mode := nv1alpha1.SKSOperationModeServe // This could also be proxy (if scale is 0)
	numActivators := int32(1)               // I guess?
	sks, err = c.ReconcileSKS(ctx, pa, mode, numActivators)
	if err != nil {
		return fmt.Errorf("error reconciling SKS: %w", err)
	}
	pa.Status.MetricsServiceName = sks.Status.PrivateServiceName

	// Propagate service name.
	pa.Status.ServiceName = sks.Status.ServiceName
	if err := c.ReconcileMetric(ctx, pa, pa.Status.MetricsServiceName); err != nil {
		return fmt.Errorf("error reconciling Metric: %w", err)
	}

	// If SKS is not ready — ensure we're not becoming ready.
	if sks.IsReady() {
		logger.Debug("SKS is ready, marking SKS status ready")
		pa.Status.MarkSKSReady()
	} else {
		logger.Debug("SKS is not ready, marking SKS status not ready")
		pa.Status.MarkSKSNotReady(sks.Status.GetCondition(nv1alpha1.ServerlessServiceConditionReady).GetMessage())
	}

	// Update status
	pa.Status.ActualScale = ptr.Int32(int32(ready))
	pa.Status.MarkActive()
	return nil
}

func resolveTBC(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) float64 {
	if v, ok := pa.TargetBC(); ok {
		return v
	}

	return config.FromContext(ctx).Autoscaler.TargetBurstCapacity
}

func intMax(a, b int32) int32 {
	if a < b {
		return b
	}
	return a
}
