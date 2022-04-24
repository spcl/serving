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
	filteredinformerfactory "knative.dev/pkg/client/injection/kube/informers/factory/filtered"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/signals"
	"knative.dev/serving/pkg/apis/serving"
	// The set of controllers this controller process runs.
	"knative.dev/serving/pkg/reconciler/autoscaling/ppa"

	// This defines the shared main for injected controllers.
	"knative.dev/pkg/injection/sharedmain"
)

var ctors = []injection.ControllerConstructor{
	ppa.NewController,
	ppa.NewMetricController,
}

func main() {
	ctx := signals.NewContext()
	ctx = filteredinformerfactory.WithSelectors(ctx, serving.RevisionUID)

	sharedmain.MainWithContext(ctx, "ppaautoscaler", ctors...)
}
