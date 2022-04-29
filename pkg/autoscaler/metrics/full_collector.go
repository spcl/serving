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
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"knative.dev/pkg/logging/logkey"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
)

// FullStatsScraperFactory creates a FullStatsScraper for a given Metric.
type FullStatsScraperFactory func(*autoscalingv1alpha1.Metric, *zap.SugaredLogger) (FullStatsScraper, error)

// FullCollector starts and stops metric collection for a given entity.
type FullCollector interface {
	// CreateOrUpdate either creates a collection for the given metric or update it, should
	// it already exist.
	CreateOrUpdate(metric *autoscalingv1alpha1.Metric) error
	// Record allows stats to be captured that came from outside the Collector.
	Record(key types.NamespacedName, now time.Time, stat *[]CustomStat)
	// Delete deletes a Metric and halts collection.
	Delete(string, string)
	// Watch registers a singleton function to call when a specific collector's status changes.
	// The passed name is the namespace/name of the metric owned by the respective collector.
	Watch(func(types.NamespacedName))
}

type CustomMetricClient interface {
	LatestCustomStats(key types.NamespacedName) (*[]CustomStat, error)
}

// FullMetricCollector manages collection of metrics for many entities.
type FullMetricCollector struct {
	logger *zap.SugaredLogger

	statsScraperFactory FullStatsScraperFactory
	clock               clock.Clock

	collectionsMutex sync.RWMutex
	collections      map[types.NamespacedName]*customCollection

	watcherMutex sync.RWMutex
	watcher      func(types.NamespacedName)
}

func (c *FullMetricCollector) LatestCustomStats(key types.NamespacedName) (*[]CustomStat, error) {
	c.collectionsMutex.RLock()
	defer c.collectionsMutex.RUnlock()

	collection, exists := c.collections[key]
	if !exists {
		return nil, ErrNotCollecting
	}

	return collection.stat, nil
}

var _ FullCollector = (*FullMetricCollector)(nil)
var _ CustomMetricClient = (*FullMetricCollector)(nil)

// NewFullMetricCollector creates a new metric collector that returns Stats for all pods.
func NewFullMetricCollector(statsScraperFactory FullStatsScraperFactory, logger *zap.SugaredLogger) *FullMetricCollector {
	return &FullMetricCollector{
		logger:              logger,
		collections:         make(map[types.NamespacedName]*customCollection),
		statsScraperFactory: statsScraperFactory,
		clock:               clock.RealClock{},
	}
}

var emptyCustomStat = CustomStat{}

// CreateOrUpdate either creates a collection for the given metric or update it, should
// it already exist.
func (c *FullMetricCollector) CreateOrUpdate(metric *autoscalingv1alpha1.Metric) error {
	logger := c.logger.With(zap.String(logkey.Key, types.NamespacedName{
		Namespace: metric.Namespace,
		Name:      metric.Name,
	}.String()))
	// TODO(#10751): Thread the config in from the reconciler and set usePassthroughLb.
	scraper, err := c.statsScraperFactory(metric, logger)
	if err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: metric.Namespace, Name: metric.Name}

	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	collection, exists := c.collections[key]
	if exists {
		// logger.Infof("collector: CreateOrUpdate metric with key: %v", key)
		collection.updateScraper(scraper)
		collection.updateMetric(metric)
		return collection.lastError()
	}

	c.collections[key] = newCustomCollection(metric, scraper, c.clock, c.Inform, logger)
	return nil
}

// Delete deletes a Metric and halts collection.
func (c *FullMetricCollector) Delete(namespace, name string) {
	c.collectionsMutex.Lock()
	defer c.collectionsMutex.Unlock()

	key := types.NamespacedName{Namespace: namespace, Name: name}
	if collection, ok := c.collections[key]; ok {
		collection.close()
		delete(c.collections, key)
	}
}

// Record records a stat that's been generated outside of the metric collector.
func (c *FullMetricCollector) Record(key types.NamespacedName, now time.Time, stat *[]CustomStat) {
	c.collectionsMutex.RLock()
	defer c.collectionsMutex.RUnlock()

	if collection, exists := c.collections[key]; exists {
		c.logger.Infof("collector: Record metric with key: %v", key)
		collection.record(now, stat)
	}
}

// Watch registers a singleton function to call when collector status changes.
func (c *FullMetricCollector) Watch(fn func(types.NamespacedName)) {
	c.watcherMutex.Lock()
	defer c.watcherMutex.Unlock()

	if c.watcher != nil {
		c.logger.Panic("Multiple calls to Watch() not supported")
	}
	c.watcher = fn
}

// Inform sends an update to the registered watcher function, if it is set.
func (c *FullMetricCollector) Inform(event types.NamespacedName) {
	c.watcherMutex.RLock()
	defer c.watcherMutex.RUnlock()

	if c.watcher != nil {
		c.watcher(event)
	}
}

type (
	// customCollection represents the collection of custom metrics for one specific entity.
	customCollection struct {
		// mux guards access to all of the collection's state.
		mux sync.RWMutex

		metric *autoscalingv1alpha1.Metric

		// Fields relevant to metric collection in general.
		stat *[]CustomStat

		// Fields relevant for metric scraping specifically.
		scraper FullStatsScraper
		lastErr error
		grp     sync.WaitGroup
		stopCh  chan struct{}
	}
)

func (c *customCollection) updateScraper(ss FullStatsScraper) {
	c.mux.Lock()
	defer c.mux.Unlock()
	c.scraper = ss
}

func (c *customCollection) getScraper() FullStatsScraper {
	c.mux.RLock()
	defer c.mux.RUnlock()
	return c.scraper
}

// newCollection creates a new collection, which uses the given scraper to
// collect stats every scrapeTickInterval.
func newCustomCollection(metric *autoscalingv1alpha1.Metric, scraper FullStatsScraper, clock clock.Clock,
	callback func(types.NamespacedName), logger *zap.SugaredLogger) *customCollection {
	c := &customCollection{
		metric:  metric,
		scraper: scraper,

		stopCh: make(chan struct{}),
	}

	key := types.NamespacedName{Namespace: metric.Namespace, Name: metric.Name}
	logger = logger.Named("collector").With(zap.String(logkey.Key, key.String()))
	logger.Infof("collector: newCollection with key %v", key)

	c.grp.Add(1)
	go func() {
		defer c.grp.Done()

		scrapeTicker := clock.NewTicker(scrapeTickInterval)
		defer scrapeTicker.Stop()
		for {
			select {
			case <-c.stopCh:
				return
			case <-scrapeTicker.C():
				scraper := c.getScraper()
				if scraper == nil {
					// Don't scrape empty target service.
					if c.updateLastError(nil) {
						callback(key)
					}
					continue
				}

				stat, err := scraper.Scrape(c.currentMetric().Spec.StableWindow)
				if err != nil {
					logger.Errorw("Failed to scrape metrics", zap.Error(err))
				}
				if c.updateLastError(err) {
					callback(key)
				}
				if stat != nil {
					c.record(clock.Now(), stat)
				}
			}
		}
	}()

	return c
}

// close stops collecting metrics, stops the scraper.
func (c *customCollection) close() {
	close(c.stopCh)
	c.grp.Wait()
}

// updateMetric safely updates the metric stored in the collection.
func (c *customCollection) updateMetric(metric *autoscalingv1alpha1.Metric) {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.metric = metric
}

// currentMetric safely returns the current metric stored in the collection.
func (c *customCollection) currentMetric() *autoscalingv1alpha1.Metric {
	c.mux.RLock()
	defer c.mux.RUnlock()

	return c.metric
}

// updateLastError updates the last error returned from the scraper
// and returns true if the error or error state changed.
func (c *customCollection) updateLastError(err error) bool {
	c.mux.Lock()
	defer c.mux.Unlock()

	if errors.Is(err, c.lastErr) {
		return false
	}
	c.lastErr = err
	return true
}

func (c *customCollection) lastError() error {
	c.mux.RLock()
	defer c.mux.RUnlock()

	return c.lastErr
}

// record adds a stat to the current collection.
func (c *customCollection) record(now time.Time, stat *[]CustomStat) {
	// Proxied requests have been counted at the activator. Subtract
	// them to avoid double counting.
	c.stat = stat
}
