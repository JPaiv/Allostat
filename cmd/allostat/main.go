// Command allostat runs the coordinator.
//
// `allostat replay` checks decision records instead; see replay.go.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/JPaiv/Allostat/api/v1alpha1"
	"github.com/JPaiv/Allostat/internal/controller"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "replay" {
		if err := replay(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	var metricsAddr, probeAddr string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address of the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the health endpoints.")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"Make sure only one coordinator decides. Turn this off only for a single local process.")
	var releaseRetry time.Duration
	flag.DurationVar(&releaseRetry, "release-retry", controller.DefaultReleaseRetry,
		"Longest wait before the coordinator looks again for evidence that closes an overdue or failed grant. A new observation makes it look at once.")
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "coordinator.allostat.io",
		// A coordinator that lost the lock must stop before another one
		// starts to decide.
		LeaderElectionReleaseOnCancel: false,
	})
	if err != nil {
		log.Error(err, "create the manager")
		os.Exit(1)
	}

	reconciler := &controller.ActionRequestReconciler{
		Reader:       mgr.GetAPIReader(),
		Writer:       mgr.GetClient(),
		Now:          time.Now,
		ReleaseRetry: releaseRetry,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "set up the controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "add the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "add the ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "run the manager")
		os.Exit(1)
	}
}
