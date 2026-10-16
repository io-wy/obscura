// Package devlab hosts the real control plane against a simulated native runtime.
package devlab

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	pb "github.com/obscura/agentruntime/control/gen/agentruntime/v1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/controller"
	"github.com/obscura/agentruntime/control/internal/devruntime"
	"github.com/obscura/agentruntime/control/internal/kubegateway"
	"github.com/obscura/agentruntime/control/internal/quota"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type Options struct {
	CRDDirectory   string
	GatewayAddress string
	AdminAddress   string
	APIKey         string
}

type Lab struct {
	GatewayURL     string
	AdminURL       string
	APIKey         string
	KubeconfigPath string
	Namespace      string
	Runtime        *devruntime.Server
	Client         client.Client
	env            *envtest.Environment
	config         *rest.Config
	scheme         *runtime.Scheme
	directory      string
	ctx            context.Context
	cancel         context.CancelFunc
	rpc            *grpc.Server
	servers        []*http.Server
	mu             sync.Mutex
	managerCancel  context.CancelFunc
	managerDone    chan error
	closeOnce      sync.Once
	closeError     error
}

func Start(ctx context.Context, opts Options) (_ *Lab, err error) {
	ctx, cancel := context.WithCancel(ctx)
	l := &Lab{Namespace: "obscura-dev", APIKey: opts.APIKey, ctx: ctx, cancel: cancel, Runtime: devruntime.New()}
	defer func() {
		if err != nil {
			_ = l.Close()
		}
	}()
	if l.APIKey == "" {
		l.APIKey = uuid.NewString()
	}
	if l.directory, err = os.MkdirTemp("/tmp", "obscura-lab-"); err != nil {
		return nil, err
	}
	// Never adopt the user's current cluster, even if envtest environment variables request it.
	useExisting := false
	l.env = &envtest.Environment{UseExistingCluster: &useExisting, CRDDirectoryPaths: []string{opts.CRDDirectory}, ErrorIfCRDPathMissing: true}
	// 独立单节点联调不验证 HA 身份发现，避免长主机名导致身份 Lease
	// 标签非法并阻塞 Kubernetes 1.35 的关闭流程；不修改生产 API Server。
	l.env.ControlPlane.GetAPIServer().Configure().Set("feature-gates", "APIServerIdentity=false")
	if l.config, err = l.env.Start(); err != nil {
		return nil, fmt.Errorf("start envtest (run make dev-bootstrap or set KUBEBUILDER_ASSETS): %w", err)
	}
	l.KubeconfigPath = filepath.Join(l.directory, "kubeconfig")
	if err = os.WriteFile(l.KubeconfigPath, l.env.KubeConfig, 0o600); err != nil {
		return nil, err
	}
	l.scheme = runtime.NewScheme()
	if err = api.AddToScheme(l.scheme); err != nil {
		return nil, err
	}
	if err = corev1.AddToScheme(l.scheme); err != nil {
		return nil, err
	}
	if l.Client, err = client.New(l.config, client.Options{Scheme: l.scheme}); err != nil {
		return nil, err
	}
	if err = l.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: l.Namespace}}); err != nil {
		return nil, err
	}
	authSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: l.Namespace}, Data: map[string][]byte{"api-key": []byte(l.APIKey)}}
	if err = l.Client.Create(ctx, authSecret); err != nil {
		return nil, err
	}
	tenant := &api.SandboxTenant{
		ObjectMeta: metav1.ObjectMeta{Name: "dev"},
		Spec: api.SandboxTenantSpec{
			Namespace:   l.Namespace,
			Credentials: []corev1.SecretKeySelector{{LocalObjectReference: corev1.LocalObjectReference{Name: authSecret.Name}, Key: "api-key"}},
			Quota:       api.TenantQuota{MaxSandboxes: 100, MaxTimeoutSeconds: 86400, MaxConcurrentExecutions: 10, MaxConcurrentPTY: 5},
		},
	}
	if err = l.Client.Create(ctx, tenant); err != nil {
		return nil, err
	}
	socket := filepath.Join(l.directory, "runtime.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	l.rpc = grpc.NewServer()
	pb.RegisterDataPlaneServer(l.rpc, l.Runtime)
	go func() { _ = l.rpc.Serve(listener) }()
	rt := &api.SandboxRuntime{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: l.Namespace}, Spec: api.SandboxRuntimeSpec{Config: api.RuntimeConfig{Type: "obscura", Endpoint: "unix://" + socket}, Templates: map[string]string{"dev": "local-simulation"}}}
	if err = l.Client.Create(ctx, rt); err != nil {
		return nil, err
	}
	if err = l.RestartController(ctx); err != nil {
		return nil, err
	}
	if err = wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		current := &api.SandboxTenant{}
		if err := l.Client.Get(ctx, client.ObjectKey{Name: tenant.Name}, current); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return condition != nil && condition.Status == metav1.ConditionTrue && len(current.Status.CredentialHashes) == 1, nil
	}); err != nil {
		return nil, fmt.Errorf("wait for tenant credentials: %w", err)
	}
	g := &kubegateway.Gateway{
		Client: l.Client, Factory: backend.NewRegistry(l.Client),
		Tenants: &kubegateway.KubernetesTenantResolver{Reader: l.Client},
		Quota:   &quota.Manager{Client: l.Client, Reader: l.Client},
	}
	if l.GatewayURL, err = l.serve(opts.GatewayAddress, g.Handler()); err != nil {
		return nil, err
	}
	if l.AdminURL, err = l.serve(opts.AdminAddress, l.adminHandler()); err != nil {
		return nil, err
	}
	return l, nil
}

// RestartController replaces the manager, cache and workqueue; API/runtime state survives.
func (l *Lab) RestartController(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.stopController(); err != nil {
		return err
	}
	// A stopped manager is recreated in this process; controller-runtime retains
	// the old name registration. Only this development harness disables the guard.
	skipNameValidation := true
	mgr, err := ctrl.NewManager(l.config, ctrl.Options{Scheme: l.scheme,
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{l.Namespace: {}}},
		Metrics:    metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", LeaderElection: false})
	if err != nil {
		return err
	}
	r := &controller.Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Factory: backend.NewRegistry(mgr.GetAPIReader()), PollInterval: 100 * time.Millisecond}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	tenant := &controller.TenantReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), AuthNamespace: l.Namespace, OrphanGracePeriod: time.Second}
	if err := tenant.SetupWithManager(mgr); err != nil {
		return err
	}
	managerCtx, cancel := context.WithCancel(l.ctx)
	l.managerCancel, l.managerDone = cancel, make(chan error, 1)
	done := l.managerDone
	go func() { done <- mgr.Start(managerCtx) }()
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if !mgr.GetCache().WaitForCacheSync(readyCtx) {
		return errors.New("controller cache failed to synchronize")
	}
	select {
	case err := <-done:
		// Preserve a completion result for stopController.
		done <- err
		return fmt.Errorf("controller stopped during startup: %v", err)
	default:
		return nil
	}
}

func (l *Lab) stopController() error {
	if l.managerCancel == nil {
		return nil
	}
	l.managerCancel()
	select {
	case err := <-l.managerDone:
		l.managerCancel = nil
		return err
	case <-time.After(10 * time.Second):
		return errors.New("controller did not stop within 10 seconds")
	}
}

func (l *Lab) serve(address string, handler http.Handler) (string, error) {
	if address == "" {
		address = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return "", errors.New("development servers must bind a loopback IP")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return "", err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	l.servers = append(l.servers, server)
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), nil
}

func (l *Lab) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l.Runtime.Snapshot())
	})
	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		var f devruntime.Faults
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&f); err != nil {
			http.Error(w, "invalid faults", http.StatusBadRequest)
			return
		}
		l.Runtime.SetFaults(f)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /controller/restart", func(w http.ResponseWriter, r *http.Request) {
		if err := l.RestartController(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(l.APIKey), []byte(r.Header.Get("X-API-Key"))) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (l *Lab) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		for _, server := range l.servers {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := server.Shutdown(ctx)
			cancel()
			if err != nil {
				_ = server.Close()
				l.closeError = errors.Join(l.closeError, err)
			}
		}
		l.mu.Lock()
		l.closeError = errors.Join(l.closeError, l.stopController())
		l.mu.Unlock()
		if l.rpc != nil {
			l.rpc.Stop()
		}
		if l.env != nil {
			l.closeError = errors.Join(l.closeError, l.env.Stop())
		}
		if l.directory != "" {
			l.closeError = errors.Join(l.closeError, os.RemoveAll(l.directory))
		}
	})
	return l.closeError
}
