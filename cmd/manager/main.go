/*
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
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	"github.com/openshift/library-go/pkg/features"
	capimachine "github.com/openshift/machine-api-operator/pkg/controller/machine"
	"github.com/openshift/machine-api-operator/pkg/metrics"
	ibmclient "github.com/openshift/machine-api-provider-ibmcloud/pkg/actuators/client"
	"github.com/openshift/machine-api-provider-ibmcloud/pkg/actuators/machine"
	machinesetcontroller "github.com/openshift/machine-api-provider-ibmcloud/pkg/actuators/machineset"
	"github.com/openshift/machine-api-provider-ibmcloud/pkg/apis"
	"github.com/openshift/machine-api-provider-ibmcloud/pkg/version"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
	klog "k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	ctrl "sigs.k8s.io/controller-runtime"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// The default durations for the leader electrion operations.
var (
	leaseDuration = 120 * time.Second
	renewDealine  = 110 * time.Second
	retryPeriod   = 20 * time.Second
)

func main() {
	printVersion := flag.Bool(
		"version",
		false,
		"print version and exit",
	)

	leaderElectResourceNamespace := flag.String(
		"leader-elect-resource-namespace",
		"",
		"The namespace of resource object that is used for locking during leader election. If unspecified and running in cluster, defaults to the service account namespace for the controller. Required for leader-election outside of a cluster.",
	)

	leaderElect := flag.Bool(
		"leader-elect",
		false,
		"Start a leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability.",
	)

	leaderElectLeaseDuration := flag.Duration(
		"leader-elect-lease-duration",
		leaseDuration,
		"The duration that non-leader candidates will wait after observing a leadership renewal until attempting to acquire leadership of a led but unrenewed leader slot. This is effectively the maximum duration that a leader can be stopped before it is replaced by another candidate. This is only applicable if leader election is enabled.",
	)

	watchNamespace := flag.String(
		"namespace",
		"",
		"Namespace that the controller watches to reconcile machine-api objects. If unspecified, the controller watches for machine-api objects across all namespaces.",
	)

	healthAddr := flag.String(
		"health-addr",
		":9440",
		"The address for health checking.",
	)

	metricsAddress := flag.String(
		"metrics-bind-address",
		metrics.DefaultMachineMetricsAddress,
		"Address for hosting metrics",
	)

	logToStderr := flag.Bool(
		"logtostderr",
		true,
		"Log to Stderr instead of files",
	)
	// Sets up feature gates
	defaultMutableGate := feature.DefaultMutableFeatureGate
	gateOpts, err := features.NewFeatureGateOptions(defaultMutableGate, apifeatures.SelfManaged, apifeatures.FeatureGateMachineAPIMigration)
	if err != nil {
		klog.Fatalf("Error setting up feature gates: %v", err)
	}
	// Add the --feature-gates flag
	gateOpts.AddFlagsToGoFlagSet(nil)

	textLoggerConfig := textlogger.NewConfig()
	textLoggerConfig.AddFlags(flag.CommandLine)
	ctrl.SetLogger(textlogger.NewLogger(textLoggerConfig))

	flag.Parse()
	if logToStderr != nil {
		klog.LogToStderr(*logToStderr)
	}

	if *printVersion {
		fmt.Println(version.String)
		os.Exit(0)
	}

	opts := manager.Options{
		LeaderElection:          *leaderElect,
		LeaderElectionNamespace: *leaderElectResourceNamespace,
		LeaderElectionID:        "machine-api-provider-ibmcloud-leader",
		LeaseDuration:           leaderElectLeaseDuration,
		HealthProbeBindAddress:  *healthAddr,
		Metrics: metricsserver.Options{
			BindAddress: *metricsAddress,
		},
		// Slow the default retry and renew election rate to reduce etcd writes at idle: BZ 1858400
		RetryPeriod:   &retryPeriod,
		RenewDeadline: &renewDealine,
	}

	if *watchNamespace != "" {
		opts.Cache.DefaultNamespaces = map[string]crcache.Config{
			*watchNamespace: {},
		}
		klog.Infof("Watching machine-api objects only in namespace %q for reconciliation.", *watchNamespace)
	}

	// Setup a Manager
	mgr, err := manager.New(config.GetConfigOrDie(), opts)
	if err != nil {
		klog.Fatalf("Failed to set up overall controller manager: %v", err)
	}

	// Sets feature gates from flags
	klog.Infof("Initializing feature gates: %s", strings.Join(defaultMutableGate.KnownFeatures(), ", "))
	warnings, err := gateOpts.ApplyTo(defaultMutableGate)
	if err != nil {
		klog.Fatalf("Error setting feature gates from flags: %v", err)
	}
	if len(warnings) > 0 {
		klog.Infof("Warnings setting feature gates from flags: %v", warnings)
	}

	klog.Infof("FeatureGateMachineAPIMigration initialised: %t", defaultMutableGate.Enabled(featuregate.Feature(apifeatures.FeatureGateMachineAPIMigration)))

	// Initialize machine actuator.
	machineActuator := machine.NewActuator(machine.ActuatorParams{
		Client:           mgr.GetClient(),
		EventRecorder:    mgr.GetEventRecorderFor("ibmcloudcontroller"),
		IbmClientBuilder: ibmclient.NewClient,
		// TODO: Implement NewClient func - returns new validated ibmcloud Client
	})

	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	if err := machinev1.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	if err := configv1.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	if err := capimachine.AddWithActuator(mgr, machineActuator, defaultMutableGate); err != nil {
		klog.Fatal(err)
	}

	setupLog := ctrl.Log.WithName("setup")
	if err = (&machinesetcontroller.Reconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("MachineSet"),
	}).SetupWithManager(mgr, controller.Options{}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MachineSet")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		klog.Fatal(err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		klog.Fatal(err)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.Fatalf("Failed to run manager: %v", err)
	}
}

// TODO: Do we need controller-runtime client to access the openshift-config-managed
