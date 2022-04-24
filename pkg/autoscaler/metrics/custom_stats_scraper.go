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
	"errors"
	"go.opencensus.io/stats"
	"net/http"
	"strconv"
	"time"

	"go.opencensus.io/stats/view"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	network "knative.dev/networking/pkg"
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

// serviceScraper scrapes Revision metrics via a K8S service by sampling. Which
// pod to be picked up to serve the request is decided by K8S. Please see
// https://kubernetes.io/docs/concepts/services-networking/network-policies/
// for details.
type customServiceScraper struct {
	serviceScraper
}

// NewCustomStatsScraper creates a new StatsScraper for the Revision which
// the given Metric is responsible for.
func NewCustomStatsScraper(
	metric *autoscalingv1alpha1.Metric,
	revisionName string,
	podAccessor resources.PodAccessor,
	usePassthroughLb bool,
	meshMode network.MeshCompatibilityMode,
	logger *zap.SugaredLogger) StatsScraper {
	logger.Infof("Create custom stat scaper for revision: %v", revisionName)
	directClient := newHTTPScrapeClient(client)
	meshClient := newHTTPScrapeClient(noKeepaliveClient)
	return newCustomServiceScraperWithClient(metric, revisionName, podAccessor, usePassthroughLb, meshMode, directClient, meshClient, logger)
}

func newCustomServiceScraperWithClient(
	metric *autoscalingv1alpha1.Metric,
	revisionName string,
	podAccessor resources.PodAccessor,
	usePassthroughLb bool,
	meshMode network.MeshCompatibilityMode,
	directClient, meshClient scrapeClient,
	logger *zap.SugaredLogger) *customServiceScraper {
	svcName := metric.Labels[serving.ServiceLabelKey]
	cfgName := metric.Labels[serving.ConfigurationLabelKey]

	ctx := metrics.RevisionContext(metric.ObjectMeta.Namespace, svcName, cfgName, revisionName)

	return &customServiceScraper{
		serviceScraper{
			meshMode:         meshMode,
			directClient:     directClient,
			meshClient:       meshClient,
			host:             metric.Spec.ScrapeTarget + "." + metric.ObjectMeta.Namespace,
			url:              urlFromTarget(metric.Spec.ScrapeTarget, metric.ObjectMeta.Namespace),
			podAccessor:      podAccessor,
			podsAddressable:  true,
			usePassthroughLb: usePassthroughLb,
			statsCtx:         ctx,
			logger:           logger,
		},
	}
}

// Scrape calls the destination service then sends it
// to the given stats channel.
func (s *customServiceScraper) Scrape(window time.Duration) (stat Stat, err error) {
	startTime := time.Now()
	defer func() {
		// No errors and an empty stat? We didn't scrape at all because
		// we're scaled to 0.
		if stat == emptyStat && err == nil {
			return
		}
		scrapeTime := time.Since(startTime)
		pkgmetrics.RecordBatch(s.statsCtx, scrapeTimeM.M(float64(scrapeTime.Milliseconds())))
	}()

	switch s.meshMode {
	case network.MeshCompatibilityModeEnabled:
		s.logger.Debug("Scraping via service due to meshMode setting")
		return s.scrapeService(window)
	case network.MeshCompatibilityModeDisabled:
		s.logger.Debug("Scraping pods directly due to meshMode setting")
		return s.scrapePods(window)
	default:
		if s.podsAddressable || s.usePassthroughLb {
			stat, err := s.scrapePods(window)
			// Return here if some pods were scraped, but not enough or if we're using a
			// passthrough loadbalancer and want no fallback to service-scrape logic.
			if !errors.Is(err, errDirectScrapingNotAvailable) || s.usePassthroughLb {
				return stat, err
			}
			// Else fall back to service scrape.
		}
		stat, err = s.scrapeService(window)
		if err == nil && s.podsAddressable {
			s.logger.Info("Direct pod scraping off, service scraping, on")
			// If err == nil, this means that we failed to scrape all pods, but service worked
			// thus it is probably a mesh case.
			s.podsAddressable = false
		}
		return stat, err
	}
}

func (s *customServiceScraper) scrapePods(window time.Duration) (Stat, error) {
	pods, youngPods, err := s.podAccessor.PodIPsSplitByAge(window, time.Now())
	if err != nil {
		s.logger.Infow("Error querying pods by age", zap.Error(err))
		return emptyStat, err
	}
	lp := len(pods)
	lyp := len(youngPods)
	// s.logger.Debugf("|OldPods| = %d, |YoungPods| = %d", lp, lyp)
	total := lp + lyp
	if total == 0 {
		return emptyStat, nil
	}

	results := make(chan Stat, total)

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
	s.logger.Infof("(Custom) Scraped %d pods", len(results))

	// We only get here if one of the scrapers failed to scrape
	// at least one pod.
	if err != nil {
		// Got some (but not enough) successful pods.
		if len(results) > 0 {
			s.logger.Warn("Too many pods failed scraping for meaningful interpolation")
			return emptyStat, errPodsExhausted
		}
		// We didn't get any pods, but we don't want to fall back to service
		// scraping because we saw an error which was not mesh-related.
		if sawNonMeshError.Load() {
			s.logger.Warn("0 pods scraped, but did not see a mesh-related error")
			return emptyStat, errPodsExhausted
		}
		// No pods, and we only saw mesh-related errors, so infer that mesh must be
		// enabled and fall back to service scraping.
		s.logger.Warn("0 pods were successfully scraped out of ", strconv.Itoa(len(pods)))
		return emptyStat, errDirectScrapingNotAvailable
	}

	return computeAverages(results, float64(total), float64(total)), nil
}
