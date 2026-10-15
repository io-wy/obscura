package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeBackend struct {
	creates     int
	deletes     int
	exists      bool
	unavailable bool
	deleteDone  bool
	observeErr  error
	deleteErr   error
	requests    []backend.Request
}

func (b *fakeBackend) Validate(backend.Request) error { return nil }
func (b *fakeBackend) Ensure(_ context.Context, r backend.Request) (backend.Observation, error) {
	b.requests = append(b.requests, r)
	if !b.exists {
		b.creates++
		b.exists = true
	}
	return backend.Observation{Handle: r.ID, State: backend.Ready}, nil
}
func (b *fakeBackend) Observe(_ context.Context, id string) (backend.Observation, error) {
	if b.observeErr != nil {
		return backend.Observation{}, b.observeErr
	}
	if b.unavailable {
		return backend.Observation{}, errors.New("unavailable")
	}
	if !b.exists {
		return backend.Observation{}, backend.ErrNotFound
	}
	return backend.Observation{Handle: id, State: backend.Ready}, nil
}
func (b *fakeBackend) Delete(context.Context, string) (bool, error) {
	b.deletes++
	return b.deleteDone, b.deleteErr
}
func (b *fakeBackend) Exec(context.Context, string, backend.Execution, func(backend.Event) error) error {
	return nil
}
func (b *fakeBackend) Close() error { return nil }

type fakeFactory struct {
	b       *fakeBackend
	configs []backend.Config
}

func (f *fakeFactory) Open(_ context.Context, _ string, c backend.Config) (backend.Backend, error) {
	f.configs = append(f.configs, c)
	return f.b, nil
}

func setup(t *testing.T) (*Reconciler, *fakeBackend, ctrl.Request) {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	sbx := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "test", UID: "e45e68b9-7172-49cd-af43-d4d870095bef", Generation: 1, CreationTimestamp: metav1.NewTime(time.Now())}, Spec: api.SandboxSpec{RuntimeRef: "native", TemplateRef: "dev", TimeoutSeconds: 300, Network: api.NetworkPolicy{}}}
	rt := &api.SandboxRuntime{ObjectMeta: metav1.ObjectMeta{Name: "native", Namespace: "test", UID: "runtime-uid"}, Spec: api.SandboxRuntimeSpec{Config: api.RuntimeConfig{Type: "obscura", Endpoint: "unix:///run/node.sock"}, Templates: map[string]string{"dev": "base@sha256:test"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.Sandbox{}).WithObjects(sbx, rt).Build()
	b := &fakeBackend{}
	r := &Reconciler{Client: c, Reader: c, Factory: &fakeFactory{b: b}}
	return r, b, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sbx)}
}
func reconcile(t *testing.T, r *Reconciler, q ctrl.Request, n int) {
	t.Helper()
	for range n {
		if _, err := r.Reconcile(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}
func sandbox(t *testing.T, r *Reconciler, q ctrl.Request) *api.Sandbox {
	t.Helper()
	s := &api.Sandbox{}
	if err := r.Get(context.Background(), q.NamespacedName, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRepeatedReconcileAndControllerRestartUsePersistedBinding(t *testing.T) {
	r, b, q := setup(t)
	reconcile(t, r, q, 5)
	s := sandbox(t, r, q)
	if s.Status.Phase != "Ready" || s.Status.Binding == nil || b.creates != 1 {
		t.Fatalf("status=%+v creates=%d", s.Status, b.creates)
	}
	r = &Reconciler{Client: r.Client, Reader: r.Client, Factory: r.Factory}
	reconcile(t, r, q, 3)
	if b.creates != 1 {
		t.Fatal("restart created duplicate")
	}
}

type failStatus struct {
	client.Client
	fail bool
}
type failingWriter struct {
	client.SubResourceWriter
	parent *failStatus
}

func (c *failStatus) Status() client.SubResourceWriter {
	return &failingWriter{SubResourceWriter: c.Client.Status(), parent: c}
}
func (w *failingWriter) Patch(ctx context.Context, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	if s, ok := o.(*api.Sandbox); ok && s.Status.Binding != nil && s.Status.Binding.Handle != "" && w.parent.fail {
		w.parent.fail = false
		return errors.New("status write interrupted")
	}
	return w.SubResourceWriter.Patch(ctx, o, p, opts...)
}
func TestCreateSuccessBeforeStatusWriteFailureDoesNotDuplicate(t *testing.T) {
	r, b, q := setup(t)
	c := &failStatus{Client: r.Client, fail: true}
	r.Client = c
	var failed bool
	for range 5 {
		if _, err := r.Reconcile(context.Background(), q); err != nil {
			failed = true
			break
		}
	}
	if !failed || b.creates != 1 {
		t.Fatal("failure scenario not exercised")
	}
	r.Client = c.Client
	reconcile(t, r, q, 3)
	if b.creates != 1 {
		t.Fatal("creation duplicated")
	}
	if b.requests[len(b.requests)-1].AllowCreate {
		t.Fatal("retry incorrectly permitted non-idempotent create")
	}
}
func TestBackendUnavailablePreservesBindingAndDoesNotRecreate(t *testing.T) {
	r, b, q := setup(t)
	reconcile(t, r, q, 5)
	id := sandbox(t, r, q).Status.Binding.Handle
	b.unavailable = true
	_, _ = r.Reconcile(context.Background(), q)
	s := sandbox(t, r, q)
	if s.Status.Phase != "Unknown" || s.Status.Binding.Handle != id || b.creates != 1 {
		t.Fatalf("unsafe disconnected state: %+v", s.Status)
	}
}

func TestBackendErrorsHaveDistinctConditions(t *testing.T) {
	for _, tc := range []struct {
		name, phase, reason string
		err                 error
	}{
		{"precondition", "Failed", "BackendPrecondition", backend.ErrPrecondition},
		{"credentials", "Unknown", "CredentialsUnavailable", backend.ErrAuth},
		{"rate limit", "Unknown", "RateLimited", backend.ErrRateLimited},
		{"capacity", "Failed", "CapacityUnavailable", backend.ErrCapacity},
		{"invalid", "Failed", "InvalidRequest", backend.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, b, q := setup(t)
			reconcile(t, r, q, 4)
			b.observeErr = tc.err
			reconcile(t, r, q, 1)
			s := sandbox(t, r, q)
			if s.Status.Phase != tc.phase || len(s.Status.Conditions) != 1 || s.Status.Conditions[0].Reason != tc.reason || b.creates != 1 {
				t.Fatalf("status=%+v creates=%d", s.Status, b.creates)
			}
		})
	}
}

func TestDeletePreconditionKeepsFinalizerAndSignalsOperator(t *testing.T) {
	r, b, q := setup(t)
	reconcile(t, r, q, 4)
	b.deleteErr = backend.ErrPrecondition
	if err := r.Delete(context.Background(), sandbox(t, r, q)); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 1)
	s := sandbox(t, r, q)
	if len(s.Finalizers) == 0 || s.Status.Phase != "Terminating" || s.Status.Conditions[0].Reason != "CleanupBlocked" {
		t.Fatalf("unsafe cleanup state: %+v", s)
	}
}
func TestDeletionRetriesUntilResourcesConfirmedAbsent(t *testing.T) {
	r, b, q := setup(t)
	reconcile(t, r, q, 5)
	s := sandbox(t, r, q)
	if err := r.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 2)
	if len(sandbox(t, r, q).Finalizers) == 0 || b.deletes != 2 {
		t.Fatal("cleanup prematurely completed")
	}
	b.deleteDone = true
	reconcile(t, r, q, 1)
	if err := r.Get(context.Background(), q.NamespacedName, &api.Sandbox{}); err == nil {
		t.Fatal("finalizer not released")
	}
}
func TestRuntimeConfigEditCannotRedirectExistingSandbox(t *testing.T) {
	r, _, q := setup(t)
	reconcile(t, r, q, 5)
	rt := &api.SandboxRuntime{}
	key := client.ObjectKey{Namespace: "test", Name: "native"}
	if err := r.Get(context.Background(), key, rt); err != nil {
		t.Fatal(err)
	}
	rt.Spec.Config.Endpoint = "unix:///other.sock"
	if err := r.Update(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 1)
	f := r.Factory.(*fakeFactory)
	if f.configs[len(f.configs)-1].Endpoint != "unix:///run/node.sock" {
		t.Fatal("binding redirected")
	}
}
func TestExpiredSandboxIsDeletedWithoutCreating(t *testing.T) {
	r, b, q := setup(t)
	s := sandbox(t, r, q)
	s.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	if err := r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 3)
	if b.creates != 0 {
		t.Fatal("expired sandbox created")
	}
}

func TestInvalidLifetimeDoesNotRequeueImmediately(t *testing.T) {
	r, b, q := setup(t)
	s := sandbox(t, r, q)
	s.Spec.TimeoutSeconds = 0
	if err := r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	result, err := r.Reconcile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 5*time.Second || sandbox(t, r, q).Status.Phase != "Failed" || b.creates != 0 {
		t.Fatalf("invalid lifetime caused immediate retry: result=%+v creates=%d", result, b.creates)
	}
}

func TestRuntimeRetryWakesAtSandboxExpiry(t *testing.T) {
	r, b, q := setup(t)
	now := time.Now()
	r.Now = func() time.Time { return now }
	s := sandbox(t, r, q)
	s.CreationTimestamp = metav1.NewTime(now.Add(-298 * time.Second))
	if err := r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 4)
	b.unavailable = true
	result, err := r.Reconcile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > 2*time.Second || sandbox(t, r, q).Status.Phase != "Unknown" {
		t.Fatalf("runtime retry missed expiry: %+v", result)
	}
}

func TestExpiredInvalidResourcesAreDeletedWithoutCreating(t *testing.T) {
	r, b, q := setup(t)
	s := sandbox(t, r, q)
	s.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	s.Spec.Resources = &api.ResourceSpec{VCPUs: 0, MemoryMiB: 128, DiskMiB: 128}
	if err := r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, q, 1)
	if err := r.Get(context.Background(), q.NamespacedName, &api.Sandbox{}); err == nil || b.creates != 0 {
		t.Fatalf("expired invalid sandbox retained or created: err=%v creates=%d", err, b.creates)
	}
}

func TestNativeDeletionAfterUncertainCreationNeverCallsEnsure(t *testing.T) {
	for _, runtimeType := range []string{"obscura", "native"} {
		t.Run(runtimeType, func(t *testing.T) {
			r, b, q := setup(t)
			reconcile(t, r, q, 2)
			s := sandbox(t, r, q)
			s.Status.Binding.Config.Type = runtimeType
			s.Status.Binding.CreationStarted = true
			if err := r.Status().Update(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, q, 1)
			if b.creates != 0 || len(b.requests) != 0 || b.deletes != 1 {
				t.Fatalf("deletion created an instance: %+v", b)
			}
		})
	}
}
