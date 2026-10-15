package main

import (
	"flag"
	"fmt"
	"os"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var namespace, metrics, probes string
	var leader bool
	flag.StringVar(&namespace, "namespace", "default", "namespace managed by this controller")
	flag.StringVar(&metrics, "metrics-bind-address", "127.0.0.1:8080", "metrics address")
	flag.StringVar(&probes, "health-probe-bind-address", ":8081", "health and readiness address")
	flag.BoolVar(&leader, "leader-elect", true, "enable controller leader election")
	logOptions := zap.Options{}
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))
	if err := run(namespace, metrics, probes, leader); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(namespace, metrics, probes string, leader bool) error {
	if namespace == "" {
		return fmt.Errorf("namespace must be nonempty")
	}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}, Metrics: metricsserver.Options{BindAddress: metrics}, HealthProbeBindAddress: probes, LeaderElection: leader, LeaderElectionID: "obscura-controller.sandbox.obscura.dev", LeaderElectionNamespace: namespace})
	if err != nil {
		return err
	}
	r := &controller.Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Factory: backend.NewRegistry(mgr.GetAPIReader())}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
