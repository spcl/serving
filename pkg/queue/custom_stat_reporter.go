/*
Copyright 2020 The Knative Authors

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

package queue

import (
	"net/http"
	"time"

	"go.uber.org/atomic"

	"github.com/gogo/protobuf/proto"

	network "knative.dev/networking/pkg"
	"knative.dev/serving/pkg/autoscaler/metrics"
)

// CustomStatsReporter structure represents a protobuf stats reporter.
type CustomStatsReporter struct {
	startTime time.Time
	stat      atomic.Value
	podName   string

	// RequestCount and ProxiedRequestCount need to be divided by the reporting period
	// they were collected over to get a "per-second" value.
	reportingPeriodSeconds float64
}

// NewCustomStatsReporter creates a reporter that collects and reports queue metrics.
func NewCustomStatsReporter(pod string, reportingPeriod time.Duration) *CustomStatsReporter {
	r := &CustomStatsReporter{
		startTime: time.Now(),
		podName:   pod,

		reportingPeriodSeconds: reportingPeriod.Seconds(),
	}

	// Start with an empty value in case we're scraped before Report has been called.
	// This matches the prometheus reporter where the gauges would just be empty
	// in this case.
	r.stat.Store(metrics.CustomStat{PodName: pod})

	return r
}

// Report captures request metrics.
func (r *CustomStatsReporter) Report() {
	r.stat.Store(metrics.CustomStat{
		PodName:       r.podName,
		ProcessUptime: time.Since(r.startTime).Seconds(),

		Values: []*metrics.CustomStatValue{
			{StatName: "custom_stat_1", StatValue: 2.5},
			{StatName: "custom_stat_2", StatValue: 4.3},
		},
	})
}

// ServeHTTP serves the stats in protobuf format over HTTP.
func (r *CustomStatsReporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	data := r.stat.Load().(metrics.CustomStat)
	buffer, err := proto.Marshal(&data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set(contentTypeHeader, network.ProtoAcceptContent)
	w.Write(buffer)
}
