package ppa

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	pareconciler "knative.dev/serving/pkg/client/injection/reconciler/autoscaling/v1alpha1/podautoscaler"
	areconciler "knative.dev/serving/pkg/reconciler/autoscaling"
	"knative.dev/serving/pkg/reconciler/autoscaling/config"
	anames "knative.dev/serving/pkg/reconciler/autoscaling/resources/names"
	"time"
)

// Reconciler implements the control loop for the HPA resources.
type Reconciler struct {
	*areconciler.Base

	podsLister corev1listers.PodLister
	scaler     *scaler
}

// Check that our Reconciler implements pareconciler.Interface
var _ pareconciler.Interface = (*Reconciler)(nil)

// ReconcileKind implements Interface.ReconcileKind.
func (c *Reconciler) ReconcileKind(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) pkgreconciler.Event {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	logger := logging.FromContext(ctx)

	logger.Info("PPA ReconcileKind")
	sksName := anames.SKS(pa.Name)
	sks, err := c.SKSLister.ServerlessServices(pa.Namespace).Get(sksName)
	if err != nil && !errors.IsNotFound(err) {
		logger.Warnw("Error retrieving SKS for Scaler", zap.Error(err))
	}

	want, err := c.scaler.scale(ctx, pa, sks, 2)
	if err != nil {
		return fmt.Errorf("error scaling target: %w", err)
	}

	logger.Infof("Want %d pods", want)

	// Compare the desired and observed resources to determine our situation.
	//podCounter := resourceutil.NewPodAccessor(c.podsLister, pa.Namespace, pa.Labels[serving.RevisionLabelKey])
	//ready, _, _, _, err := podCounter.PodCountsByState()
	//if err != nil {
	//	return fmt.Errorf("error scaling target: %w", err)
	//}

	// Update status
	// pa.Status.DesiredScale = ptr.Int32(2)
	// pa.Status.ActualScale = ptr.Int32(int32(ready))
	ready := 2
	if ready == 2 {
		pa.Status.MarkActive()
	} else {
		pa.Status.MarkActivating("Queued", "Requests to the target are being buffered as resources are provisioned.")
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

func computeStatus(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, logger *zap.SugaredLogger) {
	// reportMetrics(pa, pc)
	// computeActiveCondition(ctx, pa, pc)
	logger.Debugf("PA Status after reconcile: %#v", pa.Status.Status)
}
