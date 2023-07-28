/*
Copyright 2018 The Kubernetes Authors.

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
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	goruntime "runtime"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/fsnotify.v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/flags"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlsig "sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	vmwarev1b1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/controllers"
	"sigs.k8s.io/cluster-api-provider-vsphere/feature"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/constants"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/manager"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/version"
)

var (
	setupLog       = ctrl.Log.WithName("entrypoint")
	logOptions     = logs.NewOptions()
	controllerName = "cluster-api-vsphere-manager"

	enableContentionProfiling   bool
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
	managerOpts                 manager.Options
	profilerAddress             string
	restConfigBurst             int
	restConfigQPS               float32
	syncPeriod                  time.Duration
	webhookOpts                 webhook.Options
	watchNamespace              string

	tlsOptions = flags.TLSOptions{}

	defaultProfilerAddr      = os.Getenv("PROFILER_ADDR")
	defaultSyncPeriod        = manager.DefaultSyncPeriod
	defaultLeaderElectionID  = manager.DefaultLeaderElectionID
	defaultPodName           = manager.DefaultPodName
	defaultWebhookPort       = manager.DefaultWebhookServiceContainerPort
	defaultEnableKeepAlive   = constants.DefaultEnableKeepAlive
	defaultKeepAliveDuration = constants.DefaultKeepAliveDuration
)

// InitFlags initializes the flags.
func InitFlags(fs *pflag.FlagSet) {
	// Flags specific to CAPV

	fs.StringVar(
		&managerOpts.LeaderElectionID,
		"leader-election-id",
		defaultLeaderElectionID,
		"Name of the config map to use as the locking resource when configuring leader election.")

	fs.IntVar(
		&managerOpts.MaxConcurrentReconciles,
		"max-concurrent-reconciles",
		10,
		"The maximum number of allowed, concurrent reconciles.")

	fs.StringVar(
		&managerOpts.PodName,
		"pod-name",
		defaultPodName,
		"The name of the pod running the controller manager.")

	fs.StringVar(
		&managerOpts.CredentialsFile,
		"credentials-file",
		"/etc/capv/credentials.yaml",
		"path to CAPV's credentials file",
	)
	fs.BoolVar(
		&managerOpts.EnableKeepAlive,
		"enable-keep-alive",
		defaultEnableKeepAlive,
		"feature to enable keep alive handler in vsphere sessions. This functionality is enabled by default.")
	fs.DurationVar(
		&managerOpts.KeepAliveDuration,
		"keep-alive-duration",
		defaultKeepAliveDuration,
		"idle time interval(minutes) in between send() requests in keepalive handler",
	)
	fs.StringVar(
		&managerOpts.NetworkProvider,
		"network-provider",
		"",
		"network provider to be used by Supervisor based clusters.",
	)

	// Flags common between CAPI and CAPV

	logsv1.AddFlags(logOptions, fs)

	fs.StringVar(&managerOpts.MetricsBindAddress, "metrics-bind-addr", "localhost:8080",
		"The address the metric endpoint binds to.")

	fs.BoolVar(&managerOpts.LeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")

	fs.DurationVar(&leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)")

	fs.DurationVar(&leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string)")

	fs.DurationVar(&leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)")

	fs.StringVar(&watchNamespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")

	fs.StringVar(&managerOpts.PprofBindAddress, "profiler-address", defaultProfilerAddr,
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")

	fs.BoolVar(&enableContentionProfiling, "contention-profiling", false,
		"Enable block profiling, if profiler-address is set.")

	fs.DurationVar(&syncPeriod, "sync-period", defaultSyncPeriod,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)")

	fs.Float32Var(&restConfigQPS, "kube-api-qps", 20,
		"Maximum queries per second from the controller client to the Kubernetes API server. Defaults to 20")

	fs.IntVar(&restConfigBurst, "kube-api-burst", 30,
		"Maximum number of queries that should be allowed in one burst from the controller client to the Kubernetes API server. Default 30")

	fs.IntVar(&webhookOpts.Port, "webhook-port", defaultWebhookPort,
		"Webhook Server port")

	fs.StringVar(&webhookOpts.CertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs/",
		"Webhook cert dir, only used when webhook-port is specified.")

	fs.StringVar(&managerOpts.HealthProbeBindAddress, "health-addr", ":9440",
		"The address the health endpoint binds to.",
	)

	flags.AddTLSOptions(fs, &tlsOptions)

	feature.MutableGates.AddFlag(fs)
}

func main() {
	InitFlags(pflag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	if err := pflag.CommandLine.Set("v", "2"); err != nil {
		setupLog.Error(err, "failed to set log level: %v")
		os.Exit(1)
	}
	pflag.Parse()

	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// klog.Background will automatically use the right logger.
	ctrl.SetLogger(klog.Background())

	managerOpts.KubeConfig = ctrl.GetConfigOrDie()
	managerOpts.KubeConfig.QPS = restConfigQPS
	managerOpts.KubeConfig.Burst = restConfigBurst
	managerOpts.KubeConfig.UserAgent = remote.DefaultClusterAPIUserAgent(controllerName)

	if watchNamespace != "" {
		managerOpts.Cache.Namespaces = []string{watchNamespace}
		setupLog.Info(
			"Watching objects only in namespace for reconciliation",
			"namespace", watchNamespace)
	}

	if profilerAddress != "" && enableContentionProfiling {
		goruntime.SetBlockProfileRate(1)
	}

	setupLog.V(1).Info(fmt.Sprintf("feature gates: %+v\n", feature.Gates))

	managerOpts.Cache.SyncPeriod = &syncPeriod
	managerOpts.LeaseDuration = &leaderElectionLeaseDuration
	managerOpts.RenewDeadline = &leaderElectionRenewDeadline
	managerOpts.RetryPeriod = &leaderElectionRetryPeriod

	// Create a function that adds all the controllers and webhooks to the manager.
	addToManager := func(ctx *context.ControllerManagerContext, mgr ctrlmgr.Manager) error {
		// Check for non-supervisor VSphereCluster and start controller if found
		gvr := v1beta1.GroupVersion.WithResource(reflect.TypeOf(&v1beta1.VSphereCluster{}).Elem().Name())
		isLoaded, err := isCRDDeployed(mgr, gvr)
		if err != nil {
			return err
		}
		if isLoaded {
			if err := setupVAPIControllers(ctx, mgr); err != nil {
				return fmt.Errorf("setupVAPIControllers: %w", err)
			}
		} else {
			setupLog.Info(fmt.Sprintf("CRD for %s not loaded, skipping.", gvr.String()))
		}

		// Check for supervisor VSphereCluster and start controller if found
		gvr = vmwarev1b1.GroupVersion.WithResource(reflect.TypeOf(&vmwarev1b1.VSphereCluster{}).Elem().Name())
		isLoaded, err = isCRDDeployed(mgr, gvr)
		if err != nil {
			return err
		}
		if isLoaded {
			if err := setupSupervisorControllers(ctx, mgr); err != nil {
				return fmt.Errorf("setupSupervisorControllers: %w", err)
			}
		} else {
			setupLog.Info(fmt.Sprintf("CRD for %s not loaded, skipping.", gvr.String()))
		}

		return nil
	}

	tlsOptionOverrides, err := flags.GetTLSOptionOverrideFuncs(tlsOptions)
	if err != nil {
		setupLog.Error(err, "unable to add TLS settings to the webhook server")
		os.Exit(1)
	}
	webhookOpts.TLSOpts = tlsOptionOverrides
	managerOpts.WebhookServer = webhook.NewServer(webhookOpts)

	setupLog.Info("creating controller manager", "version", version.Get().String())
	managerOpts.AddToManager = addToManager
	mgr, err := manager.New(managerOpts)
	if err != nil {
		setupLog.Error(err, "problem creating controller manager")
		os.Exit(1)
	}

	setupChecks(mgr)

	sigHandler := ctrlsig.SetupSignalHandler()
	setupLog.Info("starting controller manager")
	if err := mgr.Start(sigHandler); err != nil {
		setupLog.Error(err, "problem running controller manager")
		os.Exit(1)
	}

	// initialize notifier for capv-manager-bootstrap-credentials
	watch, err := manager.InitializeWatch(mgr.GetContext(), &managerOpts)
	if err != nil {
		setupLog.Error(err, "failed to initialize watch on CAPV credentials file")
		os.Exit(1)
	}
	defer func(watch *fsnotify.Watcher) {
		_ = watch.Close()
	}(watch)
	defer session.Clear()
}

func setupVAPIControllers(ctx *context.ControllerManagerContext, mgr ctrlmgr.Manager) error {
	if err := (&v1beta1.VSphereClusterTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	if err := (&v1beta1.VSphereMachine{}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	if err := (&v1beta1.VSphereMachineTemplateWebhook{}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	if err := (&v1beta1.VSphereVM{}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	if err := (&v1beta1.VSphereDeploymentZone{}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	if err := (&v1beta1.VSphereFailureDomain{}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	if err := controllers.AddClusterControllerToManager(ctx, mgr, &v1beta1.VSphereCluster{}); err != nil {
		return err
	}
	if err := controllers.AddMachineControllerToManager(ctx, mgr, &v1beta1.VSphereMachine{}); err != nil {
		return err
	}
	if err := controllers.AddVMControllerToManager(ctx, mgr); err != nil {
		return err
	}
	if err := controllers.AddVsphereClusterIdentityControllerToManager(ctx, mgr); err != nil {
		return err
	}

	return controllers.AddVSphereDeploymentZoneControllerToManager(ctx, mgr)
}

func setupSupervisorControllers(ctx *context.ControllerManagerContext, mgr ctrlmgr.Manager) error {
	if err := controllers.AddClusterControllerToManager(ctx, mgr, &vmwarev1b1.VSphereCluster{}); err != nil {
		return err
	}

	if err := controllers.AddMachineControllerToManager(ctx, mgr, &vmwarev1b1.VSphereMachine{}); err != nil {
		return err
	}

	if err := controllers.AddServiceAccountProviderControllerToManager(ctx, mgr); err != nil {
		return err
	}

	return controllers.AddServiceDiscoveryControllerToManager(ctx, mgr)
}

func setupChecks(mgr ctrlmgr.Manager) {
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create ready check")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create health check")
		os.Exit(1)
	}
}

func isCRDDeployed(mgr ctrlmgr.Manager, gvr schema.GroupVersionResource) (bool, error) {
	_, err := mgr.GetRESTMapper().KindFor(gvr)
	if err != nil {
		discoveryErr, ok := errors.Unwrap(err).(*discovery.ErrGroupDiscoveryFailed)
		if !ok {
			return false, err
		}
		gvrErr, ok := discoveryErr.Groups[gvr.GroupVersion()]
		if !ok {
			return false, err
		}
		if apierrors.IsNotFound(gvrErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
