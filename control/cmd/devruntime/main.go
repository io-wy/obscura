// devruntime is a Kind-only simulated native backend. It never starts a VM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	pb "github.com/obscura/agentruntime/control/gen/agentruntime/v1"
	"github.com/obscura/agentruntime/control/internal/devruntime"
	"google.golang.org/grpc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", "/run/obscura/node.sock", "native gRPC Unix socket")
	health := flag.String("health-listen", "127.0.0.1:9001", "health probe address")
	flag.Parse()
	if err := os.MkdirAll(filepath.Dir(*socket), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(*socket); err == nil {
		conn, dialErr := net.DialTimeout("unix", *socket, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return fmt.Errorf("socket already has a listener: %s", *socket)
		}
		if err := os.Remove(*socket); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close(); _ = os.Remove(*socket) }()
	if err := os.Chmod(*socket, 0o600); err != nil {
		return err
	}
	healthListener, err := net.Listen("tcp", *health)
	if err != nil {
		return err
	}
	defer healthListener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rpc := grpc.NewServer()
	pb.RegisterDataPlaneServer(rpc, devruntime.New())
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), ReadHeaderTimeout: 5 * time.Second}
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- rpc.Serve(listener) }()
	go func() { serverErrors <- server.Serve(healthListener) }()
	select {
	case <-ctx.Done():
	case err = <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	rpc.GracefulStop()
	return nil
}
