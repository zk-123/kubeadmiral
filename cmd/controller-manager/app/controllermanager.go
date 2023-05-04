/*
Copyright 2023 The KubeAdmiral Authors.

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

package app

import (
	"context"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/kubewharf/kubeadmiral/cmd/controller-manager/app/options"
	"github.com/kubewharf/kubeadmiral/pkg/apis/core/v1alpha1"
	"github.com/kubewharf/kubeadmiral/pkg/controllermanager"
	"github.com/kubewharf/kubeadmiral/pkg/controllermanager/healthcheck"
	fedleaderelection "github.com/kubewharf/kubeadmiral/pkg/controllermanager/leaderelection"
	controllercontext "github.com/kubewharf/kubeadmiral/pkg/controllers/context"
)

const (
	FederatedClusterControllerName = "federated-cluster-controller"
	TypeConfigControllerName       = "type-config-controller"
	MonitorControllerName          = "monitor-controller"
	FollowerControllerName         = "follower-controller"
)

var knownControllers = map[string]controllermanager.StartControllerFunc{
	FederatedClusterControllerName: startFederatedClusterController,
	TypeConfigControllerName:       startTypeConfigController,
	MonitorControllerName:          startMonitorController,
	FollowerControllerName:         startFollowerController,
}

var controllersDisabledByDefault = sets.New(MonitorControllerName)

// Run starts the controller manager according to the given options.
func Run(ctx context.Context, opts *options.Options) {
	controllerCtx, err := createControllerContext(opts)
	if err != nil {
		klog.Fatalf("Error creating controller context: %v", err)
	}

	if opts.EnableProfiling {
		go func() {
			if err := http.ListenAndServe("0.0.0.0:6060", nil); err != nil {
				klog.Errorf("Failed to start pprof server: %v", err)
			}
		}()
	}

	handler := healthcheck.NewMutableHealthCheckHandler()
	handler.AddLivezChecker("ping", healthz.Ping)

	var healthzAdaptor *leaderelection.HealthzAdaptor
	if opts.EnableLeaderElect {
		healthzAdaptor = &leaderelection.HealthzAdaptor{}
		handler.AddLivezChecker("leaderElection", healthzAdaptor.Check)
	}

	run := func(ctx context.Context) {
		defer klog.Infoln("Ready to stop controllers")
		klog.Infoln("Ready to start controllers")

		err := startControllers(ctx, controllerCtx, knownControllers, knownFTCSubControllers, opts.Controllers, handler)
		if err != nil {
			klog.Fatalf("Error starting controllers %s: %v", opts.Controllers, err)
		}

		controllerCtx.StartFactories(ctx)

		<-ctx.Done()
	}

	go func() {
		if err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", opts.Port), handler); err != nil {
			klog.Fatalf("Failed to start healthz endpoint: %v", err)
		}
	}()

	if opts.EnableLeaderElect {
		elector, err := fedleaderelection.NewFederationLeaderElector(
			controllerCtx.RestConfig,
			run,
			controllerCtx.FedSystemNamespace,
			opts.LeaderElectionResourceName,
			healthzAdaptor,
		)
		if err != nil {
			klog.Fatalf("Cannot elect leader: %v", err)
		}
		elector.Run(ctx)
	} else {
		run(ctx)
	}
}

// startControllers loops through startControllerFuncs in sequence and starts the given controller if it is enabled.
// An error is returned if one of the controller fails to start. startControllers will not block on the controllers
// and will return once they have all been successfully started.
func startControllers(
	ctx context.Context,
	controllerCtx *controllercontext.Context,
	startControllerFuncs map[string]controllermanager.StartControllerFunc,
	ftcSubControllerInitFuncs map[string]controllermanager.FTCSubControllerInitFuncs,
	enabledControllers []string,
	healthCheckHandler *healthcheck.MutableHealthCheckHandler,
) error {
	klog.Infof("Start controllers %v", enabledControllers)

	for controllerName, initFn := range startControllerFuncs {
		if !isControllerEnabled(controllerName, controllersDisabledByDefault, enabledControllers) {
			klog.Warningf("Skipped %q, is disabled", controllerName)
			continue
		}

		controller, err := initFn(ctx, controllerCtx)
		if err != nil {
			return fmt.Errorf("error starting %q: %w", controllerName, err)
		}
		klog.Infof("Started %q", controllerName)

		healthCheckHandler.AddReadyzChecker(controllerName, controllermanager.HealthzCheckerAdaptor(controllerName, controller))
	}

	manager := NewFederatedTypeConfigManager(
		controllerCtx.FedInformerFactory.Core().V1alpha1().FederatedTypeConfigs(),
		controllerCtx,
		healthCheckHandler,
		controllerCtx.Metrics,
	)
	for controllerName, initFuncs := range ftcSubControllerInitFuncs {
		manager.RegisterSubController(controllerName, initFuncs.StartFunc, func(typeConfig *v1alpha1.FederatedTypeConfig) bool {
			if !isControllerEnabled(controllerName, controllersDisabledByDefault, enabledControllers) {
				return false
			}
			if initFuncs.IsEnabledFunc != nil {
				return initFuncs.IsEnabledFunc(typeConfig)
			}
			return true
		})
	}
	go manager.Run(ctx)

	return nil
}
