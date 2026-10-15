package kubegateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"testing"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/quota"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func gatewayTenant(name, namespace string, max int32, keys ...string) *api.SandboxTenant {
	hashes := make([]string, 0, len(keys))
	for _, key := range keys {
		hashes = append(hashes, keyHash(key))
	}
	return &api.SandboxTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       api.SandboxTenantSpec{Namespace: namespace, Quota: api.TenantQuota{MaxSandboxes: max, MaxTimeoutSeconds: 3600, MaxConcurrentExecutions: 2, MaxConcurrentPTY: 1}},
		Status:     api.SandboxTenantStatus{CredentialHashes: hashes, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}}},
	}
}

func multiTenantGateways(t *testing.T, tenants ...*api.SandboxTenant) (*Gateway, *Gateway, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, len(tenants))
	for i := range tenants {
		objects[i] = tenants[i]
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.SandboxTenant{}, &api.Sandbox{}).WithObjects(objects...).Build()
	newGateway := func() *Gateway {
		return &Gateway{Client: c, Tenants: &KubernetesTenantResolver{Reader: c}, Quota: &quota.Manager{Client: c, Reader: c}}
	}
	return newGateway(), newGateway(), c
}

func TestTenantKeysRotateRevokeAndFailClosedOnDuplicates(t *testing.T) {
	a := gatewayTenant("acme", "tenant-acme", 2, "old-key", "new-key")
	b := gatewayTenant("beta", "tenant-beta", 2, "beta-key")
	g, _, c := multiTenantGateways(t, a, b)
	for _, key := range []string{"old-key", "new-key"} {
		if w := call(t, g, http.MethodGet, "/v1/sandboxes/missing", "", key, ""); w.Code != http.StatusNotFound {
			t.Fatalf("valid rotating key rejected: key=%s status=%d body=%s", key, w.Code, w.Body.String())
		}
	}
	current := &api.SandboxTenant{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "acme"}, current); err != nil {
		t.Fatal(err)
	}
	current.Status.CredentialHashes = []string{keyHash("new-key")}
	if err := c.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if w := call(t, g, http.MethodGet, "/v1/sandboxes/missing", "", "old-key", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key accepted: %d %s", w.Code, w.Body.String())
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "beta"}, b); err != nil {
		t.Fatal(err)
	}
	b.Status.CredentialHashes = []string{keyHash("new-key")}
	if err := c.Status().Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if w := call(t, g, http.MethodGet, "/v1/sandboxes/missing", "", "new-key", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("ambiguous key accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestTenantSandboxLookupUsesAuthenticatedNamespace(t *testing.T) {
	a := gatewayTenant("acme", "tenant-acme", 2, "acme-key")
	b := gatewayTenant("beta", "tenant-beta", 2, "beta-key")
	g, _, c := multiTenantGateways(t, a, b)
	foreign := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "shared-id", Namespace: "tenant-beta"}, Spec: api.SandboxSpec{RuntimeRef: "runtime", TemplateRef: "template", TimeoutSeconds: 300}}
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if w := call(t, g, http.MethodGet, "/v1/sandboxes/shared-id", "", "acme-key", ""); w.Code != http.StatusNotFound {
		t.Fatalf("foreign sandbox exposed: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, g, http.MethodGet, "/v1/sandboxes/shared-id", "", "beta-key", ""); w.Code != http.StatusOK {
		t.Fatalf("own sandbox hidden: %d %s", w.Code, w.Body.String())
	}
}

func TestTenantResolverRejectsDuplicateNamespaceBeforeStatusReconcile(t *testing.T) {
	one := gatewayTenant("one", "shared", 2, "one-key")
	two := gatewayTenant("two", "shared", 2, "two-key")
	g, _, _ := multiTenantGateways(t, one, two)
	for _, key := range []string{"one-key", "two-key"} {
		if w := call(t, g, http.MethodGet, "/v1/sandboxes/missing", "", key, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("duplicate namespace key accepted: key=%s status=%d", key, w.Code)
		}
	}
}

func TestConcurrentGatewaysCannotExceedTenantSandboxQuota(t *testing.T) {
	tenant := gatewayTenant("acme", "tenant-acme", 1, "secret")
	one, two, c := multiTenantGateways(t, tenant)
	body := `{"runtimeRef":"native","templateRef":"dev","resources":{"vcpus":1,"memoryMiB":128,"diskMiB":128},"timeoutSeconds":300}`
	start := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for i, g := range []*Gateway{one, two} {
		wg.Add(1)
		go func(i int, g *Gateway) {
			defer wg.Done()
			<-start
			results <- call(t, g, http.MethodPost, "/v1/sandboxes", body, "secret", string(rune('a'+i))).Code
		}(i, g)
	}
	close(start)
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[http.StatusAccepted] != 1 || counts[http.StatusTooManyRequests] != 1 {
		t.Fatalf("statuses: %v", counts)
	}
	sandboxes := &api.SandboxList{}
	if err := c.List(context.Background(), sandboxes, client.InNamespace("tenant-acme")); err != nil {
		t.Fatal(err)
	}
	if len(sandboxes.Items) != 1 {
		t.Fatalf("sandboxes: %d", len(sandboxes.Items))
	}
	got := &api.SandboxTenant{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "acme"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Allocations) != 1 {
		t.Fatalf("allocations: %d", len(got.Status.Allocations))
	}
}
