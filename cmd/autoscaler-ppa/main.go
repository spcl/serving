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

package main

import (
	"go.uber.org/zap"
	filteredinformerfactory "knative.dev/pkg/client/injection/kube/informers/factory/filtered"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/signals"
	"knative.dev/serving/pkg/apis/serving"
	"log"

	// The set of controllers this controller process runs.
	"knative.dev/serving/pkg/reconciler/autoscaling/ppa"

	// This defines the shared main for injected controllers.
	"knative.dev/pkg/injection/sharedmain"
)

const (
	component = "ppaautoscaler"
)

var ctors = []injection.ControllerConstructor{
	ppa.NewController,
	ppa.NewMetricController,
}

func main() {
	ctx := signals.NewContext()
	ctx = filteredinformerfactory.WithSelectors(ctx, serving.RevisionUID)

	ctx, startInformers := injection.EnableInjectionOrDie(ctx, nil)

	// Set up our logger.
	loggingConfig, err := sharedmain.GetLoggingConfig(ctx)
	if err != nil {
		log.Fatal("Error loading/parsing logging configuration: ", err)
	}
	logger, _ := logging.NewLoggerFromConfig(loggingConfig, component)
	defer flush(logger)
	ctx = logging.WithLogger(ctx, logger)

	ctx = ppa.WithCollector(ctx)

	startInformers()

	sharedmain.MainWithConfig(ctx, component, injection.GetConfig(ctx), ctors...)
}

func flush(logger *zap.SugaredLogger) {
	logger.Sync()
	metrics.FlushExporter()
}
