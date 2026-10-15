package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/kubegateway"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	var namespace, listen string
	flag.StringVar(&namespace, "namespace", "default", "tenant namespace")
	flag.StringVar(&listen, "listen", "127.0.0.1:9000", "HTTP listen address")
	flag.Parse()
	if err := run(namespace, listen); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(namespace, listen string) error {
	if namespace == "" || os.Getenv("OBSCURA_API_KEY") == "" {
		return fmt.Errorf("namespace and OBSCURA_API_KEY are required")
	}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	g := &kubegateway.Gateway{Client: c, Factory: backend.NewRegistry(c), Namespace: namespace, APIKey: os.Getenv("OBSCURA_API_KEY")}
	server := &http.Server{Addr: listen, Handler: g.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, ReadTimeout: 15 * time.Second}
	ctx := ctrl.SetupSignalHandler()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
