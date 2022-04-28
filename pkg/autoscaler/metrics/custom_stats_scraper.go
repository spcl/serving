/*
Copyright 2019 The Knative Authors

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

package metrics

import (
	"context"
	"go.opencensus.io/stats"
	"knative.dev/serving/pkg/networking"
	"net/http"
	"strconv"
	"time"

	"go.opencensus.io/stats/view"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	pkgmetrics "knative.dev/pkg/metrics"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	"knative.dev/serving/pkg/metrics"
	"knative.dev/serving/pkg/resources"
)

var (
	scrapeTimeCustom = stats.Float64(
		"custom_scrape_time",
		"Time to scrape custom metrics in milliseconds",
		stats.UnitMilliseconds)
)

func init() {
	if err := pkgmetrics.RegisterResourceView(
		&view.View{
			Description: "The time to scrape metrics in milliseconds",
			Measure:     scrapeTimeCustom,
			Aggregation: view.Distribution(pkgmetrics.Buckets125(1, 100000)...),
		},
	); err != nil {
		panic(err)
	}
}

// FullStatsScraper defines the interface for collecting Revision metrics
type FullStatsScraper interface {
	// Scrape scrapes the Revision queue metric endpoint. The duration is used
	// to cutoff young pods, whose stats might skew lower.
	Scrape(time.Duration) (CustomStat, error)
}

// customScrapeClient defines the interface for collecting Revision metrics for a given
// URL. Internal used only.
type customScrapeClient interface {
	// Do executes the given request.
	Do(*http.Request) (CustomStat, error)
}

// customServiceScraper scrapes Revision metrics via a K8S service by sampling. Which
// pod to be picked up to serve the request is decided by K8S. Please see
// https://kubernetes.io/docs/concepts/services-networking/network-policies/
// for details.
type customServiceScraper struct {
	directClient     customScrapeClient
	statsCtx         context.Context
	podAccessor      resources.PodAccessor
	logger           *zap.SugaredLogger
	host             string
	usePassthroughLb bool
}

// NewCustomStatsScraper creates a new StatsScraper for the Revision which
// the given Metric is responsible for.
func NewCustomStatsScraper(
	metric *autoscalingv1alpha1.Metric,
	revisionName string,
	podAccessor resources.PodAccessor,
	usePassthroughLb bool,
	logger *zap.SugaredLogger) FullStatsScraper {
	directClient := newCustomHTTPScrapeClient(client)
	svcName := metric.Labels[serving.ServiceLabelKey]
	cfgName := metric.Labels[serving.ConfigurationLabelKey]

	ctx := metrics.RevisionContext(metric.ObjectMeta.Namespace, svcName, cfgName, revisionName)

	return &customServiceScraper{
		directClient:     directClient,
		host:             metric.Spec.ScrapeTarget + "." + metric.ObjectMeta.Namespace,
		podAccessor:      podAccessor,
		usePassthroughLb: usePassthroughLb,
		statsCtx:         ctx,
		logger:           logger,
	}
}

// Scrape calls the destination service then sends it
// to the given stats channel.
func (s *customServiceScraper) Scrape(window time.Duration) (stat CustomStat, err error) {
	startTime := time.Now()
	defer func() {
		// No errors and an empty stat? We didn't scrape at all because
		// we're scaled to 0.
		if len(stat.Values) == 0 && err == nil {
			return
		}
		scrapeTime := time.Since(startTime)
		pkgmetrics.RecordBatch(s.statsCtx, scrapeTimeCustom.M(float64(scrapeTime.Milliseconds())))
	}()
	return s.scrapePods(window)
}

func (s *customServiceScraper) scrapePods(window time.Duration) (CustomStat, error) {
	s.logger.Info("scrape pods")
	pods, youngPods, err := s.podAccessor.PodIPsSplitByAge(window, time.Now())
	if err != nil {
		s.logger.Infow("Error querying pods by age", zap.Error(err))
		return emptyCustomStat, err
	}
	lp := len(pods)
	lyp := len(youngPods)
	// s.logger.Debugf("|OldPods| = %d, |YoungPods| = %d", lp, lyp)
	total := lp + lyp
	if total == 0 {
		return emptyCustomStat, nil
	}

	results := make(chan CustomStat, total)
	// s.logger.Infof("Try to scrape %d pods", total)

	grp, egCtx := errgroup.WithContext(context.Background())
	idx := atomic.NewInt32(-1)
	var sawNonMeshError atomic.Bool
	// Start |total| threads to scan in parallel.
	for i := 0; i < total; i++ {
		grp.Go(func() error {
			// If a given pod failed to scrape, we want to continue
			// scanning pods down the line.
			for {
				// Acquire next pod.
				myIdx := int(idx.Inc())
				// All out?
				if myIdx >= len(pods) {
					return errPodsExhausted
				}

				portAndPath = strconv.Itoa(networking.AutoscalingQueueCustomMetricsPort) + "/custom_metrics"

				// Scrape!
				target := "http://" + pods[myIdx] + ":" + portAndPath
				req, err := http.NewRequestWithContext(egCtx, http.MethodGet, target, nil)
				if err != nil {
					return err
				}

				if s.usePassthroughLb {
					req.Host = s.host
					req.Header.Add("Knative-Direct-Lb", "true")
				}

				stat, err := s.directClient.Do(req)
				//stat := CustomStat{
				//	PodName: "ASD",
				//	Values: []*CustomStatValue{
				//		{StatName: "custom_stat1", StatValue: 2.3},
				//		{StatName: "custom_stat2", StatValue: 3.2},
				//		{StatName: "custom_stat3", StatValue: 1.3},
				//		{StatName: "custom_stat4", StatValue: 4.5},
				//	},
				//}
				// s.logger.Infof("Scrape endpoint: %s (%s) returned: %f %f", target, stat.PodName, stat.RequestCount, stat.ProxiedRequestCount)
				if err == nil {
					results <- stat
					return nil
				}

				if !isPotentialMeshError(err) {
					sawNonMeshError.Store(true)
				}

				s.logger.Infow("Failed scraping pod "+pods[myIdx], zap.Error(err))
			}
		})
	}

	err = grp.Wait()
	close(results)
	s.logger.Infof("(Custom) Scraped %d", len(results))

	// We only get here if one of the scrapers failed to scrape
	// at least one pod.
	if err != nil {
		// Got some (but not enough) successful pods.
		if len(results) > 0 {
			s.logger.Warnf("Too many pods failed scraping for meaningful interpolation error: %v", err)
			return emptyCustomStat, errPodsExhausted
		}
		// We didn't get any pods, but we don't want to fall back to service
		// scraping because we saw an error which was not mesh-related.
		if sawNonMeshError.Load() {
			s.logger.Warn("0 pods scraped, but did not see a mesh-related error")
			return emptyCustomStat, errPodsExhausted
		}
		// No pods, and we only saw mesh-related errors, so infer that mesh must be
		// enabled and fall back to service scraping.
		s.logger.Warn("0 pods were successfully scraped out of ", strconv.Itoa(len(pods)))
		return emptyCustomStat, errDirectScrapingNotAvailable
	}

	return <-results, nil
	//return computeAverages(results, float64(total), float64(total)), nil
}
