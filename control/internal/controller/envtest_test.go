//go:build envtest

package controller

import (
	"context"
	"testing"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// This test uses a real API server for structural schema, CEL immutability,
// resourceVersion conflicts, status subresources and finalizer semantics.
func TestAPIValidationAndControllerRecovery(t *testing.T) {
	useExisting := false
	env := &envtest.Environment{UseExistingCluster: &useExisting, CRDDirectoryPaths: []string{"../../../deploy/crds"}, ErrorIfCRDPathMissing: true}
	// 单 API Server 测试不验证 HA 身份发现。CI 长主机名会使其 Lease
	// 标签非法，导致 Kubernetes 1.35 的 Lease 重试锁阻塞优雅退出。
	env.ControlPlane.GetAPIServer().Configure().Set("feature-gates", "APIServerIdentity=false")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenant := &api.SandboxTenant{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: api.SandboxTenantSpec{Namespace: "default", Credentials: []corev1.SecretKeySelector{{LocalObjectReference: corev1.LocalObjectReference{Name: "key"}, Key: "api-key"}}, Quota: api.TenantQuota{MaxSandboxes: 1, MaxTimeoutSeconds: 3600, MaxConcurrentExecutions: 1, MaxConcurrentPTY: 1}}}
	if err := c.Create(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	changedTenant := tenant.DeepCopy()
	changedTenant.Spec.Namespace = "other"
	if err := c.Update(ctx, changedTenant); !apierrors.IsInvalid(err) {
		t.Fatalf("immutable tenant namespace changed: %v", err)
	}
	changedTenant = tenant.DeepCopy()
	changedTenant.Spec.Quota.MaxSandboxes = 2
	if err := c.Update(ctx, changedTenant); err != nil {
		t.Fatalf("mutable tenant quota rejected: %v", err)
	}
	rt := &api.SandboxRuntime{ObjectMeta: metav1.ObjectMeta{Name: "native", Namespace: "default"}, Spec: api.SandboxRuntimeSpec{Config: api.RuntimeConfig{Type: "native", Endpoint: "unix:///run/node.sock"}, Templates: map[string]string{"dev": "base"}}}
	if err := c.Create(ctx, rt); err != nil {
		t.Fatal(err)
	}
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", Labels: map[string]string{TenantLabel: tenant.Name}}, Spec: api.SandboxSpec{RuntimeRef: "native", TemplateRef: "dev", Resources: &api.ResourceSpec{VCPUs: 1, MemoryMiB: 512, DiskMiB: 2048}, Network: api.NetworkPolicy{}, TimeoutSeconds: 300}}
	invalid := s.DeepCopy()
	invalid.Name = "invalid"
	invalid.Spec.TimeoutSeconds = -1
	if err := c.Create(ctx, invalid); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid lifetime accepted: %v", err)
	}
	if err := c.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: tenant.Name}, tenant); err != nil {
		t.Fatal(err)
	}
	tenant.Status.Allocations = []api.TenantAllocation{{SandboxID: s.Name, SpecHash: SpecHash(s.Spec), Resources: s.Spec.Resources.DeepCopy(), TimeoutSeconds: s.Spec.TimeoutSeconds, ReservedAt: metav1.Now()}}
	if err := c.Status().Update(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	changed := s.DeepCopy()
	changed.Spec.TemplateRef = "other"
	if err := c.Update(ctx, changed); !apierrors.IsInvalid(err) {
		t.Fatalf("immutable spec changed: %v", err)
	}
	b := &fakeBackend{}
	failing := &failStatus{Client: c, fail: true}
	r := &Reconciler{Client: failing, Reader: c, Factory: &fakeFactory{b: b}}
	q := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)}
	failed := false
	var failureErr error
	for range 6 {
		if _, err := r.Reconcile(ctx, q); err != nil {
			failed = true
			failureErr = err
			break
		}
	}
	if !failed || b.creates != 1 {
		t.Fatalf("post-create persistence failure not exercised: failed=%v error=%v creates=%d status=%+v", failed, failureErr, b.creates, sandbox(t, r, q).Status)
	}
	r = &Reconciler{Client: c, Reader: c, Factory: r.Factory}
	reconcile(t, r, q, 3)
	s = sandbox(t, r, q)
	if s.Status.Phase != "Ready" || b.creates != 1 {
		t.Fatalf("restart recovery: %+v", s.Status)
	}
	if err := c.Delete(ctx, s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 1)
	if len(sandbox(t, r, q).Finalizers) == 0 {
		t.Fatal("pending cleanup released finalizer")
	}
	b.deleteDone = true
	reconcile(t, r, q, 1)
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, q.NamespacedName, &api.Sandbox{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}); err != nil {
		t.Fatal(err)
	}
}
