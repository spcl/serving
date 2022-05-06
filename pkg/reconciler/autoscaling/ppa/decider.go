/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ppa

import (
	"context"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/apis/duck"
	asmetrics "knative.dev/serving/pkg/autoscaler/metrics"
	"knative.dev/serving/pkg/autoscaler/scaling"
	"knative.dev/serving/pkg/client/listers/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/resources"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

type Decider struct {
	logger        *zap.SugaredLogger
	metrics       asmetrics.CustomMetricClient
	key           types.NamespacedName
	listerFactory func(gvr schema.GroupVersionResource) (cache.GenericLister, error)
	paLister      v1alpha1.PodAutoscalerLister
}

func (d *Decider) Scale(logger *zap.SugaredLogger, t time.Time) scaling.ScaleResult {
	pa, err := d.paLister.PodAutoscalers(d.key.Namespace).Get(d.key.Name)
	if err != nil {
		logger.Errorw("Failed to get pa", zap.Error(err))
	}
	ps, err := resources.GetScaleResource(d.key.Namespace, pa.Spec.ScaleTargetRef, d.listerFactory)
	stats, err := d.metrics.LatestCustomStats(d.key)
	toDelete := int32(0)
	if err == nil && len(stats) > 0 {
		for _, stat := range stats {
			if stat.CanDownScale {
				toDelete++
			}
		}
	}
	return scaling.ScaleResult{
		DesiredPodCount:     ps.Status.Replicas - toDelete,
		ExcessBurstCapacity: 1,
		ScaleValid:          true,
	}
}

func (d *Decider) Update(spec *scaling.DeciderSpec) {
	d.logger.Debugf("Need to update decider with: %v", spec)
}

// MakeDecider constructs a Decider resource from a PodAutoscaler
func MakeDecider(
	ctx context.Context,
	logger *zap.SugaredLogger,
	metrics asmetrics.CustomMetricClient,
	key types.NamespacedName,
	psInformerFactory duck.InformerFactory,
	paLister v1alpha1.PodAutoscalerLister,
) *Decider {
	logger.Infof("Created new decider for %v", key)
	return &Decider{
		key:      key,
		logger:   logger,
		metrics:  metrics,
		paLister: paLister,
		listerFactory: func(gvr schema.GroupVersionResource) (cache.GenericLister, error) {
			_, l, err := psInformerFactory.Get(ctx, gvr)
			return l, err
		},
	}
}
