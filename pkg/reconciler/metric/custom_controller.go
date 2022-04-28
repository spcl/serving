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

package metric

import (
	"context"
	"knative.dev/pkg/logging"

	"knative.dev/serving/pkg/autoscaler/metrics"
	metricreconciler "knative.dev/serving/pkg/client/injection/reconciler/autoscaling/v1alpha1/metric"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
)

// NewCustomController initializes the controller and is called by the generated code.
// Registers eventhandlers to enqueue events.
func NewCustomController(
	ctx context.Context,
	cmw configmap.Watcher,
	collector metrics.FullCollector,
) *controller.Impl {
	logger := logging.FromContext(ctx)
	// metricInformer := metricinformer.Get(ctx)

	logger.Info("New custom metric controller")

	c := &customReconciler{
		collector: collector,
	}
	impl := metricreconciler.NewImpl(ctx, c)

	// Watch all the Metric objects.
	//metricInformer.Informer().AddEventHandler(
	//	controller.HandleAll(func(obj interface{}) {
	//		logger.Info("Event by metric informer in metric controller")
	//		impl.Enqueue(obj)
	//	}))
	//
	//collector.Watch(func(name types.NamespacedName) {
	//	logger.Info("Event by collector in metric controller")
	//	impl.EnqueueKey(name)
	//})

	return impl
}
