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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/serving/pkg/apis/autoscaling"
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
	Scrape(time.Duration) ([]CustomStat, error)
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
	directClient customScrapeClient
	statsCtx     context.Context
	podLister    CustomPodLister
	logger       *zap.SugaredLogger
	endpoint     string
}

// NewCustomStatsScraper creates a new StatsScraper for the Revision which
// the given Metric is responsible for.
func NewCustomStatsScraper(
	metric *autoscalingv1alpha1.Metric,
	revisionName string,
	podLister CustomPodLister,
	logger *zap.SugaredLogger) FullStatsScraper {
	directClient := newCustomHTTPScrapeClient(client)
	svcName := metric.Labels[serving.ServiceLabelKey]
	cfgName := metric.Labels[serving.ConfigurationLabelKey]

	ctx := metrics.RevisionContext(metric.ObjectMeta.Namespace, svcName, cfgName, revisionName)

	annotations := metric.Annotations
	if class, ok := annotations[autoscaling.ClassAnnotationKey]; ok {
		if class == autoscaling.PPA {
			endpoint := annotations["autoscaling.knative.dev/endpoint"]
			path := ":" + annotations["autoscaling.knative.dev/port"]
			if endpoint[0] != '/' {
				path += "/"
			}
			path += endpoint

			return &customServiceScraper{
				directClient: directClient,
				endpoint:     path,
				podLister:    podLister,
				statsCtx:     ctx,
				logger:       logger,
			}
		}
	}
	return nil
}

// Scrape calls the destination service then sends it
// to the given stats channel.
func (s *customServiceScraper) Scrape(window time.Duration) (stat []CustomStat, err error) {
	startTime := time.Now()
	defer func() {
		// No errors and an empty stat? We didn't scrape at all because
		// we're scaled to 0.
		if stat == nil && err == nil {
			return
		}
		scrapeTime := time.Since(startTime)
		pkgmetrics.RecordBatch(s.statsCtx, scrapeTimeCustom.M(float64(scrapeTime.Milliseconds())))
	}()
	return s.scrapePods(window)
}

func (s *customServiceScraper) scrapePods(window time.Duration) ([]CustomStat, error) {
	pods, youngPods, err := s.podLister.podsSplitByAge(window, time.Now())
	if err != nil {
		s.logger.Infow("Error querying pods by age", zap.Error(err))
		return nil, err
	}
	lp := len(pods)
	lyp := len(youngPods)
	// s.logger.Debugf("|OldPods| = %d, |YoungPods| = %d", lp, lyp)
	total := lp + lyp
	if total == 0 {
		return nil, nil
	}

	if lp == 0 && lyp > 0 {
		s.logger.Warnf("Did not scrape, waiting for %d young pods", lyp)
	}

	resultArr := make([]CustomStat, lp)

	grp, egCtx := errgroup.WithContext(context.Background())
	idx := atomic.NewInt32(-1)
	succesCount := atomic.NewInt32(0)
	var sawNonMeshError atomic.Bool
	// Start |lp| threads to scan in parallel.
	for i := 0; i < lp; i++ {
		grp.Go(func() error {
			// If a given pod failed to scrape, we want to continue
			// scanning pods down the line.
			for {
				// Acquire next pod.
				myIdx := int(idx.Inc())
				// All out?
				if myIdx >= lp {
					return errPodsExhausted
				}
				pod := pods[myIdx]

				// Scrape!
				target := "http://" + pod.Status.PodIP + s.endpoint
				req, err := http.NewRequestWithContext(egCtx, http.MethodGet, target, nil)
				if err != nil {
					return err
				}

				stat, err := s.directClient.Do(req)
				if err == nil {
					stat.PodName = pod.Name
					resultArr[myIdx] = stat
					succesCount.Inc()
					return nil
				}

				if !isPotentialMeshError(err) {
					sawNonMeshError.Store(true)
				}

				s.logger.Infow("Failed scraping pod "+pod.Name, zap.Error(err))
			}
		})
	}

	err = grp.Wait()

	// We only get here if one of the scrapers failed to scrape
	// at least one pod.
	if err != nil {
		// Got some (but not enough) successful pods.
		if succesCount.Load() > 0 {
			s.logger.Warnf("Too many pods failed scraping for meaningful interpolation error: %v", err)
			return nil, errPodsExhausted
		}
		// We didn't get any pods, but we don't want to fall back to service
		// scraping because we saw an error which was not mesh-related.
		if sawNonMeshError.Load() {
			s.logger.Warn("0 pods scraped, but did not see a mesh-related error")
			return nil, errPodsExhausted
		}
		// No pods, and we only saw mesh-related errors, so infer that mesh must be
		// enabled and fall back to service scraping.
		s.logger.Warn("0 pods were successfully scraped out of ", strconv.Itoa(len(pods)))
		return nil, errDirectScrapingNotAvailable
	}

	return resultArr, nil
}

type CustomPodLister struct {
	podsLister corev1listers.PodNamespaceLister
	selector   labels.Selector
}

func NewCustomPodLister(lister corev1listers.PodLister, namespace, revisionName string) CustomPodLister {
	return CustomPodLister{
		podsLister: lister.Pods(namespace),
		selector: labels.SelectorFromSet(labels.Set{
			serving.RevisionLabelKey: revisionName,
		}),
	}
}

// podReady checks whether pod's Ready status is True.
func podReady(p *v1.Pod) bool {
	for _, cond := range p.Status.Conditions {
		if cond.Type == v1.PodReady {
			return cond.Status == v1.ConditionTrue
		}
	}
	// No ready status, probably not even running.
	return false
}

func (c *CustomPodLister) podsSplitByAge(window time.Duration, now time.Time) (older, younger []*v1.Pod, err error) {
	pods, err := c.podsLister.List(c.selector)
	if err != nil {
		return nil, nil, err
	}

	for _, p := range pods {
		// Make sure pod is ready
		if !podReady(p) {
			continue
		}

		// Make sure pod is running
		if !(p.Status.Phase == v1.PodRunning && p.DeletionTimestamp == nil) {
			continue
		}

		if now.Sub(p.Status.StartTime.Time) > window {
			older = append(older, p)
		} else {
			younger = append(younger, p)
		}
	}

	return older, younger, nil
}
