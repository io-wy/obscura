package kubegateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/controller"
	"github.com/obscura/agentruntime/control/internal/quota"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	tenant := gatewayTenant("tenant", "tenant", 100, "secret")
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.Sandbox{}, &api.SandboxTenant{}).WithObjects(tenant).Build()
	return &Gateway{Client: c, Tenants: &KubernetesTenantResolver{Reader: c}, Quota: &quota.Manager{Client: c, Reader: c}}
}

func reserveSandbox(t *testing.T, g *Gateway, s *api.Sandbox) {
	t.Helper()
	if err := g.Quota.ReserveSandbox(context.Background(), "tenant", quota.SandboxRequest{SandboxID: s.Name, SpecHash: controller.SpecHash(s.Spec), Resources: s.Spec.Resources, TimeoutSeconds: s.Spec.TimeoutSeconds}); err != nil {
		t.Fatal(err)
	}
}

type executionBackend struct {
	backend.Backend
	calls         int
	deadline      time.Time
	handle        string
	failAfterEmit bool
}

func (b *executionBackend) Close() error { return nil }
func (b *executionBackend) Exec(ctx context.Context, id string, e backend.Execution, emit func(backend.Event) error) error {
	b.calls++
	b.handle = id
	b.deadline, _ = ctx.Deadline()
	if err := emit(backend.Event{ExecutionID: e.ID, Type: "stdout", Payload: "hello", Sequence: 1}); err != nil {
		return err
	}
	if b.failAfterEmit {
		return backend.ErrUnavailable
	}
	return nil
}

type executionFactory struct{ b *executionBackend }

func (f executionFactory) Open(context.Context, string, backend.Config) (backend.Backend, error) {
	return f.b, nil
}

func TestExecutionStreamsUsingPinnedHandleAndClampsLifetime(t *testing.T) {
	g := testGateway(t)
	b := &executionBackend{}
	g.Factory = executionFactory{b: b}
	expires := metav1.NewTime(time.Now().Add(time.Minute))
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "tenant"}, Spec: api.SandboxSpec{RuntimeRef: "native", TemplateRef: "dev", TimeoutSeconds: 300}}
	s.Status = api.SandboxStatus{Phase: "Ready", ExpiresAt: &expires, Binding: &api.RuntimeBinding{ID: "uid", Handle: "actual-backend-id", SpecHash: controller.SpecHash(s.Spec), Config: api.RuntimeConfig{Type: "obscura", Endpoint: "unix:///run/original.sock"}}}
	if err := g.Client.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reserveSandbox(t, g, s)
	w := call(t, g, "POST", "/v1/sandboxes/ready/execute", `{"command":"echo hello","timeoutSeconds":300}`, "secret", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"payload":"hello"`) || b.handle != "actual-backend-id" || b.deadline.After(expires.Time) {
		t.Fatalf("execution: %d %s deadline=%v", w.Code, w.Body.String(), b.deadline)
	}
	if w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatal("not streaming NDJSON")
	}
	expires = metav1.NewTime(time.Now().Add(-time.Minute))
	s.Status.ExpiresAt = &expires
	if err := g.Client.Status().Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	w = call(t, g, "POST", "/v1/sandboxes/ready/execute", `{"command":"echo hello"}`, "secret", "")
	if w.Code != http.StatusConflict || b.calls != 1 {
		t.Fatal("expired sandbox executed")
	}
}

func TestExecutionInterruptedAfterOutputReportsTransportError(t *testing.T) {
	g := testGateway(t)
	b := &executionBackend{failAfterEmit: true}
	g.Factory = executionFactory{b: b}
	expires := metav1.NewTime(time.Now().Add(time.Minute))
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "tenant"}, Spec: api.SandboxSpec{RuntimeRef: "native", TemplateRef: "dev", TimeoutSeconds: 300}}
	s.Status = api.SandboxStatus{Phase: "Ready", ExpiresAt: &expires, Binding: &api.RuntimeBinding{ID: "uid", Handle: "backend-id", SpecHash: controller.SpecHash(s.Spec), Config: api.RuntimeConfig{Type: "obscura", Endpoint: "unix:///run/node.sock"}}}
	if err := g.Client.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reserveSandbox(t, g, s)
	w := call(t, g, "POST", "/v1/sandboxes/ready/execute", `{"command":"echo hello"}`, "secret", "")
	if w.Code != http.StatusOK || b.calls != 1 || !strings.Contains(w.Body.String(), `"type":"stdout"`) || !strings.Contains(w.Body.String(), `"type":"transport_error"`) {
		t.Fatalf("interrupted execution: status=%d body=%s calls=%d", w.Code, w.Body.String(), b.calls)
	}
}

func TestIdempotencyWithRESTDecoderDoesNotRetainIncomingOptionalFields(t *testing.T) {
	g := testGateway(t)
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion})
	mapper.Add(api.GroupVersion.WithKind("Sandbox"), meta.RESTScopeNamespace)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409}`))
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"sandbox.obscura.dev/v1alpha1","kind":"Sandbox","metadata":{"name":"existing","namespace":"tenant"},"spec":{"runtimeRef":"native","templateRef":"dev","network":{"allowEgress":true},"timeoutSeconds":300}}`))
	}))
	defer server.Close()
	c, err := client.New(&rest.Config{Host: server.URL}, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	g.Client = c
	body := `{"runtimeRef":"native","templateRef":"dev","resources":{"vcpus":1,"memoryMiB":512,"diskMiB":2048},"network":{"allowEgress":true},"timeoutSeconds":300}`
	if w := call(t, g, "POST", "/v1/sandboxes", body, "secret", "same-key"); w.Code != http.StatusConflict {
		t.Fatalf("optional fields retained: %d %s", w.Code, w.Body.String())
	}
}
func call(t *testing.T, g *Gateway, method, path, body, key, idempotency string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Idempotency-Key", idempotency)
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, req)
	return w
}
func TestGatewayRequiresKeyForLifecycleAndExecution(t *testing.T) {
	g := testGateway(t)
	for _, path := range []string{"/v1/sandboxes", "/v1/sandboxes/task/execute"} {
		if w := call(t, g, "POST", path, `{}`, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s allowed unauthenticated request", path)
		}
	}
}
func TestCreationWritesDesiredStateAndRetriesAreIdempotent(t *testing.T) {
	g := testGateway(t)
	body := `{"runtimeRef":"native","templateRef":"dev","resources":{"vcpus":1,"memoryMiB":512,"diskMiB":2048},"timeoutSeconds":300}`
	var id string
	for range 2 {
		w := call(t, g, "POST", "/v1/sandboxes", body, "secret", "request-1")
		if w.Code != http.StatusAccepted {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var result struct {
			SandboxID string `json:"sandboxID"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if id != "" && id != result.SandboxID {
			t.Fatal("retry changed identity")
		}
		id = result.SandboxID
	}
	list := &api.SandboxList{}
	if err := g.Client.List(context.Background(), list, client.InNamespace("tenant")); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Status.Binding != nil {
		t.Fatal("gateway directly created runtime state")
	}
	w := call(t, g, "POST", "/v1/sandboxes", strings.Replace(body, "300", "600", 1), "secret", "request-1")
	if w.Code != http.StatusConflict {
		t.Fatalf("changed retry accepted: %d", w.Code)
	}
}
func TestGatewayDoesNotAccessOtherNamespace(t *testing.T) {
	g := testGateway(t)
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "task"}}
	if err := g.Client.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if w := call(t, g, "GET", "/v1/sandboxes/task", "", "secret", ""); w.Code != http.StatusNotFound {
		t.Fatal("foreign sandbox exposed")
	}
}
func TestGatewayRejectsInvalidOrUnknownFields(t *testing.T) {
	g := testGateway(t)
	for _, body := range []string{`{"runtimeRef":"native","templateRef":"dev","timeoutSeconds":-1}`, `{"runtimeRef":"native","templateRef":"dev","timeoutSeconds":300,"namespace":"other"}`, `{"runtimeRef":"native","templateRef":"dev","timeoutSeconds":300} {}`} {
		if w := call(t, g, "POST", "/v1/sandboxes", body, "secret", ""); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid request accepted: %s", body)
		}
	}
}
