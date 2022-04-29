package ppa

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	nv1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	pkgreconciler "knative.dev/pkg/reconciler"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	"knative.dev/serving/pkg/autoscaler/metrics"
	"knative.dev/serving/pkg/autoscaler/scaling"
	pareconciler "knative.dev/serving/pkg/client/injection/reconciler/autoscaling/v1alpha1/podautoscaler"
	areconciler "knative.dev/serving/pkg/reconciler/autoscaling"
	"knative.dev/serving/pkg/reconciler/autoscaling/config"
	"knative.dev/serving/pkg/reconciler/autoscaling/kpa/resources"
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
	//deciders   Deciders
	deciders resources.Deciders
}

// Check that our Reconciler implements pareconciler.Interface
var _ pareconciler.Interface = (*Reconciler)(nil)

// ReconcileKind implements Interface.ReconcileKind.
func (c *Reconciler) ReconcileKind(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) pkgreconciler.Event {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	logger := logging.FromContext(ctx)

	decider, err := c.reconcileDecider(ctx, pa)
	// metricKey := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
	//logger.Infof("Decider wants %d scale", decider.Status.DesiredScale)
	//logger.Infof("Trying to get metrics for key: %v", metricKey)
	//stableVal, panicVal, err := c.collector.StableAndPanicConcurrency(metricKey, time.Now())
	//if err != nil {
	//	logger.Errorf("Failed to retrieve data from collector, err: %v", err)
	//} else {
	//	logger.Infof("stable: %f, panic, %f", stableVal, panicVal)
	//}
	desiredScale := decider.Status.DesiredScale

	// Compare the desired and observed resources to determine our situation.
	podCounter := resourceutil.NewPodAccessor(c.podsLister, pa.Namespace, pa.Labels[serving.RevisionLabelKey])
	ready, notReady, pending, terminating, err := podCounter.PodCountsByState()
	if err != nil {
		return fmt.Errorf("error scaling target: %w", err)
	}

	if ready >= 1 && !pa.Status.IsScaleTargetInitialized() {
		pa.Status.MarkScaleTargetInitialized()
	}

	sksName := anames.SKS(pa.Name)
	sks, err := c.SKSLister.ServerlessServices(pa.Namespace).Get(sksName)
	if err != nil && !errors.IsNotFound(err) {
		logger.Warnw("Error retrieving SKS for Scaler", zap.Error(err))
	}

	want, err := c.scaler.scale(ctx, pa, sks, desiredScale)
	if err != nil {
		return fmt.Errorf("error scaling target: %w", err)
	}

	logger.Infof("PPA want %d pods and %d is ready, %d is not ready, %d is pending, %d is terminating", want, ready, notReady, pending, terminating)

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
	pa.Status.DesiredScale = ptr.Int32(desiredScale)
	pa.Status.ActualScale = ptr.Int32(int32(ready))
	if int32(ready) == desiredScale && sks.IsReady() {
		pa.Status.MarkActive()
		// logger.Infof("PA (%v) is set to active", pa.Name)
	} else {
		pa.Status.MarkActivating("Queued", "Requests to the target are being buffered as resources are provisioned.")
		// logger.Infof("PA (%v) is set to activating", pa.Name)
	}
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

func (c *Reconciler) reconcileDecider(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) (*scaling.Decider, error) {
	desiredDecider := resources.MakeDecider(pa, config.FromContext(ctx).Autoscaler)
	decider, err := c.deciders.Get(ctx, desiredDecider.Namespace, desiredDecider.Name)
	if errors.IsNotFound(err) {
		decider, err = c.deciders.Create(ctx, desiredDecider)
		if err != nil {
			return nil, fmt.Errorf("error creating Decider: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("error fetching Decider: %w", err)
	}

	// Ignore status when reconciling
	desiredDecider.Status = decider.Status
	if !equality.Semantic.DeepEqual(desiredDecider, decider) {
		decider, err = c.deciders.Update(ctx, desiredDecider)
		if err != nil {
			return nil, fmt.Errorf("error updating decider: %w", err)
		}
	}

	if decider.Status.DesiredScale == -1 {
		decider.Status.DesiredScale = 1
	}
	return decider, nil
}
