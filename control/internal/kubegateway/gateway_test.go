package kubegateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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

type fileBackend struct {
	backend.Backend
	readPath, writePath, written, contentType string
}

func (b *fileBackend) Close() error { return nil }
func (b *fileBackend) ReadFile(_ context.Context, _ string, path string) (backend.FileRead, error) {
	b.readPath = path
	data := []byte{0, 1, 2, 3}
	return backend.FileRead{Body: io.NopCloser(strings.NewReader(string(data))), Size: int64(len(data)), ContentType: "application/octet-stream"}, nil
}
func (b *fileBackend) WriteFile(_ context.Context, _ string, file backend.FileWrite) error {
	b.writePath, b.contentType = file.Path, file.ContentType
	data, err := io.ReadAll(file.Body)
	b.written = string(data)
	return err
}

type backendFactory struct{ b backend.Backend }

func (f backendFactory) Open(context.Context, string, backend.Config) (backend.Backend, error) {
	return f.b, nil
}

func readySandbox(t *testing.T, g *Gateway, name string) *api.Sandbox {
	t.Helper()
	expires := metav1.NewTime(time.Now().Add(time.Minute))
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"}, Spec: api.SandboxSpec{RuntimeRef: "http", TemplateRef: "dev", TimeoutSeconds: 300}}
	s.Status = api.SandboxStatus{Phase: "Ready", ExpiresAt: &expires, Binding: &api.RuntimeBinding{ID: "uid", Handle: "remote-id", SpecHash: controller.SpecHash(s.Spec), Config: api.RuntimeConfig{Type: "e2b", Endpoint: "http://runtime.test"}}}
	if err := g.Client.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reserveSandbox(t, g, s)
	return s
}

type fakePTYSession struct {
	out  chan backend.PTYMessage
	sent chan backend.PTYMessage
}

func (s *fakePTYSession) Send(ctx context.Context, message backend.PTYMessage) error {
	select {
	case s.sent <- message:
	case <-ctx.Done():
		return ctx.Err()
	}
	if message.Type == "input" {
		s.out <- backend.PTYMessage{Type: "exit", Payload: "0"}
	}
	return nil
}
func (s *fakePTYSession) Receive(ctx context.Context) (backend.PTYMessage, error) {
	select {
	case message := <-s.out:
		return message, nil
	case <-ctx.Done():
		return backend.PTYMessage{}, ctx.Err()
	}
}
func (s *fakePTYSession) Close() error { return nil }

type ptyBackend struct {
	backend.Backend
	request backend.PTYRequest
	session *fakePTYSession
}

func (b *ptyBackend) Close() error { return nil }
func (b *ptyBackend) OpenPTY(_ context.Context, _ string, request backend.PTYRequest) (backend.PTYSession, error) {
	b.request = request
	b.session = &fakePTYSession{out: make(chan backend.PTYMessage, 2), sent: make(chan backend.PTYMessage, 2)}
	b.session.out <- backend.PTYMessage{Type: "output", Payload: "ready"}
	return b.session, nil
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

func TestFileReadWriteRoutes(t *testing.T) {
	g := testGateway(t)
	b := &fileBackend{}
	g.Factory = backendFactory{b: b}
	readySandbox(t, g, "files")
	w := call(t, g, http.MethodGet, "/v1/sandboxes/files/files?path=%2Fwork%2Fa.txt", "", "secret", "")
	if w.Code != http.StatusOK || !strings.EqualFold(w.Header().Get("Content-Type"), "application/octet-stream") || w.Body.Len() != 4 || b.readPath != "/work/a.txt" {
		t.Fatalf("read: status=%d headers=%v body=%v path=%q", w.Code, w.Header(), w.Body.Bytes(), b.readPath)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/files/files?path=%2Fwork%2Fa.txt", strings.NewReader("hello"))
	req.Header.Set("X-API-Key", "secret")
	req.Header.Set("Content-Type", "text/plain")
	w = httptest.NewRecorder()
	g.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || b.writePath != "/work/a.txt" || b.written != "hello" || b.contentType != "text/plain" {
		t.Fatalf("write: status=%d body=%s path=%q data=%q type=%q", w.Code, w.Body.String(), b.writePath, b.written, b.contentType)
	}
}

func TestPTYWebSocketBridgeAndLeaseRelease(t *testing.T) {
	g := testGateway(t)
	b := &ptyBackend{}
	g.Factory = backendFactory{b: b}
	readySandbox(t, g, "pty")
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/sandboxes/pty/pty"
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: http.Header{"X-API-Key": []string{"secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	start := map[string]any{"type": "start", "command": "/bin/sh", "envVars": map[string]string{"TERM": "xterm"}, "columns": 80, "rows": 24, "timeoutSeconds": 30}
	if err := wsjson.Write(context.Background(), conn, start); err != nil {
		t.Fatal(err)
	}
	var message backend.PTYMessage
	if err := wsjson.Read(context.Background(), conn, &message); err != nil || message.Type != "output" || message.Payload != "ready" {
		t.Fatalf("output=%+v err=%v", message, err)
	}
	if err := wsjson.Write(context.Background(), conn, backend.PTYMessage{Type: "input", Payload: "echo hi\n"}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(context.Background(), conn, &message); err != nil || message.Type != "exit" {
		t.Fatalf("exit=%+v err=%v", message, err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if b.request.Command != "/bin/sh" || b.request.Columns != 80 || b.request.Rows != 24 || b.request.EnvVars["TERM"] != "xterm" {
		t.Fatalf("request=%+v", b.request)
	}
	select {
	case sent := <-b.session.sent:
		if sent.Type != "input" || sent.Payload != "echo hi\n" {
			t.Fatalf("sent=%+v", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("client input not forwarded")
	}
	deadline := time.Now().Add(time.Second)
	for {
		tenant := &api.SandboxTenant{}
		if err := g.Client.Get(context.Background(), client.ObjectKey{Name: "tenant"}, tenant); err != nil {
			t.Fatal(err)
		}
		if len(tenant.Status.Sessions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PTY lease retained: %+v", tenant.Status.Sessions)
		}
		time.Sleep(10 * time.Millisecond)
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
