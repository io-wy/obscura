package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func quotaManager(t *testing.T, maxSandboxes int32) (*Manager, *api.SandboxTenant) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tenant := &api.SandboxTenant{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: api.SandboxTenantSpec{
			Namespace: "tenant-acme",
			Quota: api.TenantQuota{
				MaxSandboxes:            maxSandboxes,
				MaxVCPUsPerSandbox:      4,
				MaxMemoryMiBPerSandbox:  4096,
				MaxDiskMiBPerSandbox:    8192,
				MaxTotalVCPUs:           4,
				MaxTotalMemoryMiB:       4096,
				MaxTotalDiskMiB:         8192,
				MaxTimeoutSeconds:       3600,
				MaxConcurrentExecutions: 1,
				MaxConcurrentPTY:        1,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.SandboxTenant{}).WithObjects(tenant).Build()
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	return &Manager{Client: c, Reader: c, Now: func() time.Time { return now }}, tenant
}

func request(id, hash string, vcpus int32) SandboxRequest {
	return SandboxRequest{SandboxID: id, SpecHash: hash, Resources: &api.ResourceSpec{VCPUs: vcpus, MemoryMiB: 512, DiskMiB: 1024}, TimeoutSeconds: 300}
}

func TestReserveSandboxEnforcesLimitsAndIdempotency(t *testing.T) {
	m, _ := quotaManager(t, 2)
	ctx := context.Background()
	if err := m.ReserveSandbox(ctx, "acme", request("one", "hash-one", 2)); err != nil {
		t.Fatal(err)
	}
	if err := m.ReserveSandbox(ctx, "acme", request("one", "hash-one", 2)); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if err := m.ReserveSandbox(ctx, "acme", request("one", "changed", 2)); !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if err := m.ReserveSandbox(ctx, "acme", request("large", "hash-large", 5)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("per-instance limit: %v", err)
	}
	if err := m.ReserveSandbox(ctx, "acme", request("two", "hash-two", 3)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("aggregate limit: %v", err)
	}
	if err := m.ReserveSandbox(ctx, "acme", SandboxRequest{SandboxID: "long", SpecHash: "hash", TimeoutSeconds: 7200}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("timeout limit: %v", err)
	}
}

func TestConcurrentReservationsCannotCrossLimit(t *testing.T) {
	m, _ := quotaManager(t, 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			results <- m.ReserveSandbox(context.Background(), "acme", request(id, "hash-"+id, 1))
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	var accepted, rejected int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQuotaExceeded):
			rejected++
		default:
			t.Fatalf("unexpected reservation result: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	tenant, err := m.Get(context.Background(), "acme")
	if err != nil || len(tenant.Status.Allocations) != 1 {
		t.Fatalf("allocations=%d err=%v", len(tenant.Status.Allocations), err)
	}
}

func TestReleaseAndSessionLeases(t *testing.T) {
	m, _ := quotaManager(t, 2)
	ctx := context.Background()
	if err := m.ReserveSandbox(ctx, "acme", request("one", "hash-one", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.AcquireSession(ctx, "acme", SessionRequest{ID: "exec-1", SandboxID: "one", Kind: SessionExecution, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := m.AcquireSession(ctx, "acme", SessionRequest{ID: "exec-1", SandboxID: "one", Kind: SessionExecution, TTL: time.Minute}); err != nil {
		t.Fatalf("idempotent lease: %v", err)
	}
	if err := m.AcquireSession(ctx, "acme", SessionRequest{ID: "exec-2", SandboxID: "one", Kind: SessionExecution, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("execution limit: %v", err)
	}
	if err := m.AcquireSession(ctx, "acme", SessionRequest{ID: "pty-1", SandboxID: "one", Kind: SessionPTY, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseSession(ctx, "acme", "exec-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.AcquireSession(ctx, "acme", SessionRequest{ID: "exec-2", SandboxID: "one", Kind: SessionExecution, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleaseSandbox(ctx, "acme", "one"); err != nil {
		t.Fatal(err)
	}
	tenant, err := m.Get(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(tenant.Status.Allocations) != 0 || len(tenant.Status.Sessions) != 0 {
		t.Fatalf("release left allocations=%d sessions=%d", len(tenant.Status.Allocations), len(tenant.Status.Sessions))
	}
}
