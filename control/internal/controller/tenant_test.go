package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func tenantReconciler(t *testing.T, objects ...runtime.Object) (*TenantReconciler, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.SandboxTenant{}, &api.Sandbox{}).WithRuntimeObjects(objects...).Build()
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	return &TenantReconciler{Client: c, Reader: c, AuthNamespace: "obscura-system", Now: func() time.Time { return now }, OrphanGracePeriod: time.Minute}, scheme
}

func tenantObject(name, namespace string, credentials ...string) *api.SandboxTenant {
	selectors := make([]corev1.SecretKeySelector, 0, len(credentials))
	for _, credential := range credentials {
		selectors = append(selectors, corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: credential}, Key: "api-key"})
	}
	return &api.SandboxTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       api.SandboxTenantSpec{Namespace: namespace, Credentials: selectors, Quota: api.TenantQuota{MaxSandboxes: 2, MaxTimeoutSeconds: 3600, MaxConcurrentExecutions: 1, MaxConcurrentPTY: 1}},
	}
}

func secret(name, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "obscura-system"}, Data: map[string][]byte{"api-key": []byte(value)}}
}

func reconcileTenant(t *testing.T, r *TenantReconciler, name string) *api.SandboxTenant {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatal(err)
	}
	got := &api.SandboxTenant{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestTenantReconcilesCredentialRotationAndRevocation(t *testing.T) {
	tenant := tenantObject("acme", "tenant-acme", "old", "new")
	r, _ := tenantReconciler(t, tenant, secret("old", "old-key"), secret("new", "new-key"))
	got := reconcileTenant(t, r, "acme")
	if len(got.Status.CredentialHashes) != 2 || !contains(got.Status.CredentialHashes, digest("old-key")) || !contains(got.Status.CredentialHashes, digest("new-key")) {
		t.Fatalf("credential hashes: %v", got.Status.CredentialHashes)
	}
	if condition := meta.FindStatusCondition(got.Status.Conditions, "Ready"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("ready condition: %+v", condition)
	}
	got.Spec.Credentials = got.Spec.Credentials[1:]
	if err := r.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got = reconcileTenant(t, r, "acme")
	if len(got.Status.CredentialHashes) != 1 || got.Status.CredentialHashes[0] != digest("new-key") {
		t.Fatalf("revoked key retained: %v", got.Status.CredentialHashes)
	}
}

func TestTenantDuplicateNamespaceFailsClosed(t *testing.T) {
	one := tenantObject("one", "shared", "one-key")
	two := tenantObject("two", "shared", "two-key")
	r, _ := tenantReconciler(t, one, two, secret("one-key", "secret-one"), secret("two-key", "secret-two"))
	got := reconcileTenant(t, r, "one")
	condition := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "DuplicateNamespace" || len(got.Status.CredentialHashes) != 0 {
		t.Fatalf("duplicate tenant accepted: status=%+v", got.Status)
	}
}

func TestTenantReapsOnlyExpiredOrphansAndSessions(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	tenant := tenantObject("acme", "tenant-acme", "key")
	tenant.Status.Allocations = []api.TenantAllocation{
		{SandboxID: "orphan", SpecHash: "one", TimeoutSeconds: 300, ReservedAt: metav1.NewTime(now.Add(-2 * time.Minute))},
		{SandboxID: "existing", SpecHash: "two", TimeoutSeconds: 300, ReservedAt: metav1.NewTime(now.Add(-2 * time.Minute))},
		{SandboxID: "pending", SpecHash: "three", TimeoutSeconds: 300, ReservedAt: metav1.NewTime(now.Add(-30 * time.Second))},
	}
	tenant.Status.Sessions = []api.TenantSessionLease{
		{ID: "expired", SandboxID: "existing", Kind: "execution", ExpiresAt: metav1.NewTime(now.Add(-time.Second))},
		{ID: "live", SandboxID: "existing", Kind: "pty", ExpiresAt: metav1.NewTime(now.Add(time.Minute))},
	}
	sandbox := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "tenant-acme"}, Spec: api.SandboxSpec{RuntimeRef: "runtime", TemplateRef: "template", TimeoutSeconds: 300}}
	r, _ := tenantReconciler(t, tenant, sandbox, secret("key", "secret"))
	got := reconcileTenant(t, r, "acme")
	if allocationIDs(got.Status.Allocations) != "existing,pending" {
		t.Fatalf("allocations: %s", allocationIDs(got.Status.Allocations))
	}
	if len(got.Status.Sessions) != 1 || got.Status.Sessions[0].ID != "live" {
		t.Fatalf("sessions: %+v", got.Status.Sessions)
	}
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func allocationIDs(items []api.TenantAllocation) string {
	result := ""
	for _, item := range items {
		if result != "" {
			result += ","
		}
		result += item.SandboxID
	}
	return result
}
