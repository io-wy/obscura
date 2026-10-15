// devlab runs local-only development tooling; it is not included in production images.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/obscura/agentruntime/control/internal/devlab"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "up", "up (long-running lab), smoke (existing lab), or check (start, test, stop)")
	crds := flag.String("crds", "../deploy/crds", "CRD directory")
	listen := flag.String("listen", "127.0.0.1:9000", "Gateway loopback address")
	admin := flag.String("admin-listen", "127.0.0.1:9002", "development control loopback address")
	url := flag.String("url", "http://127.0.0.1:9000", "existing lab Gateway URL (smoke mode)")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	if *mode == "smoke" {
		key := os.Getenv("OBSCURA_API_KEY")
		if key == "" {
			return fmt.Errorf("smoke mode requires OBSCURA_API_KEY from the running lab")
		}
		ctx, stop := context.WithTimeout(ctx, time.Minute)
		defer stop()
		if err := devlab.Smoke(ctx, *url, key); err != nil {
			return err
		}
		fmt.Println("PASS: authentication, idempotency, Ready, execution streams, deletion and expiry")
		return nil
	}
	if *mode != "up" && *mode != "check" {
		return fmt.Errorf("unknown mode %q", *mode)
	}
	lab, err := devlab.Start(ctx, devlab.Options{CRDDirectory: *crds, GatewayAddress: *listen, AdminAddress: *admin, APIKey: os.Getenv("OBSCURA_API_KEY")})
	if err != nil {
		return err
	}
	if *mode == "check" {
		checkCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		err = devlab.Smoke(checkCtx, lab.GatewayURL, lab.APIKey)
		if err == nil {
			err = devlab.ExerciseFaults(checkCtx, lab)
		}
		if closeErr := lab.Close(); closeErr != nil {
			return fmt.Errorf("check=%v; cleanup=%w", err, closeErr)
		}
		if err != nil {
			return err
		}
		fmt.Println("PASS: control-plane integration and failure/restart recovery")
		return nil
	}
	fmt.Printf("\nLocal lab ready (simulated runtime; no VM/host commands).\nexport KUBECONFIG=%q\nexport OBSCURA_API_KEY=%q\nexport OBSCURA_GATEWAY_URL=%q\nexport OBSCURA_DEV_ADMIN_URL=%q\nNamespace: %s\nCtrl-C stops and removes this temporary environment.\n\n", lab.KubeconfigPath, lab.APIKey, lab.GatewayURL, lab.AdminURL, lab.Namespace)
	<-ctx.Done()
	return lab.Close()
}
