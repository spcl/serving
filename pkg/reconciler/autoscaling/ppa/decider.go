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
	"go.uber.org/zap"
	asmetrics "knative.dev/serving/pkg/autoscaler/metrics"
	"knative.dev/serving/pkg/autoscaler/scaling"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

type Decider struct {
	logger         *zap.SugaredLogger
	collector      *asmetrics.FullMetricCollector
	key            types.NamespacedName
	currentStatLen int
	runExp         bool // TODO: remove these
}

func (d *Decider) Scale(logger *zap.SugaredLogger, t time.Time) scaling.ScaleResult {
	newScale := int32(1) //d.currentPodCount //(int32(t.Minute()/4))%10 + 1
	stats, err := d.collector.LatestCustomStats(d.key)
	if err == nil && len(stats) > 0 {
		logger.Infof("Scale based on: %v", stats)
		if d.runExp || len(stats) != d.currentStatLen {
			newScale = int32(5)
			d.runExp = true
		} else {
			newScale = int32(6)
		}
		d.currentStatLen = len(stats)
	}
	return scaling.ScaleResult{
		DesiredPodCount:     newScale,
		ExcessBurstCapacity: 1,
		ScaleValid:          true,
	}
}

func (d *Decider) Update(spec *scaling.DeciderSpec) {
	d.logger.Warnf("Need to update decider with: %v", spec)
}

// MakeDecider constructs a Decider resource from a PodAutoscaler taking
// into account the PA's ContainerConcurrency and the relevant
// autoscaling annotation.
func MakeDecider(logger *zap.SugaredLogger, metrics *asmetrics.FullMetricCollector, key types.NamespacedName) *Decider {
	logger.Info("Created new decider")
	return &Decider{
		key:            key,
		logger:         logger,
		collector:      metrics,
		currentStatLen: 1,
		runExp:         false,
	}
}
