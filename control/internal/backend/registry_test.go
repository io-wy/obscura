package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func httpBackend(t *testing.T, handler http.HandlerFunc) Backend {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	b, err := NewRegistry(nil).Open(context.Background(), "test", Config{Type: "e2b", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}
func e2bRequest() Request {
	return Request{ID: "uid", SpecHash: "hash", Template: "template", Network: NetworkPolicy{AllowEgress: true}, TimeoutSeconds: 60, AllowCreate: true}
}

func TestHTTPPlatformRequestContract(t *testing.T) {
	var calls []string
	deleted := false
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
			q := r.URL.Query()
			if q.Get("metadata") != "obscura_uid=uid" || q.Get("state") != "running,paused" || q.Get("limit") != "100" {
				t.Errorf("unexpected discovery query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
				t.Errorf("unexpected create headers: %v", r.Header)
			}
			var body struct {
				TemplateID string            `json:"templateID"`
				Timeout    int64             `json:"timeout"`
				Metadata   map[string]string `json:"metadata"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil || body.TemplateID != "template" || body.Timeout != 60 || !reflect.DeepEqual(body.Metadata, map[string]string{"obscura_uid": "uid", "spec_hash": "hash"}) {
				t.Errorf("unexpected create body: %+v, %v", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"remote-id"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/remote-id":
			if deleted {
				w.WriteHeader(http.StatusNotFound)
			} else {
				_, _ = w.Write([]byte(`{"sandboxID":"remote-id","state":"paused"}`))
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/remote-id":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	created, err := b.Ensure(context.Background(), e2bRequest())
	if err != nil || created.Handle != "remote-id" || created.State != Ready {
		t.Fatalf("create observation: %+v, %v", created, err)
	}
	observed, err := b.Observe(context.Background(), created.Handle)
	if err != nil || observed.State != Paused {
		t.Fatalf("get observation: %+v, %v", observed, err)
	}
	done, err := b.Delete(context.Background(), created.Handle)
	if err != nil || !done {
		t.Fatalf("delete confirmation: %v, %v", done, err)
	}
	want := []string{"GET /v2/sandboxes", "POST /sandboxes", "GET /sandboxes/remote-id", "DELETE /sandboxes/remote-id", "GET /sandboxes/remote-id"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("request sequence: got %v, want %v", calls, want)
	}
}

func TestHTTPDiscoverAfterCreateTimeout(t *testing.T) {
	var posts atomic.Int32
	var found atomic.Bool
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			if r.URL.Path != "/v2/sandboxes" {
				t.Errorf("list path %s", r.URL.Path)
			}
			if r.URL.Query().Get("metadata") != "obscura_uid=uid" || r.URL.Query().Get("state") != "running,paused" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			if found.Load() {
				json.NewEncoder(w).Encode([]map[string]any{{"sandboxID": "created", "state": "running", "metadata": map[string]string{"obscura_uid": "uid", "spec_hash": "hash"}}})
			} else {
				w.Write([]byte("[]"))
			}
		case "POST":
			posts.Add(1)
			found.Store(true)
			time.Sleep(40 * time.Millisecond)
			w.WriteHeader(201)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := b.Ensure(ctx, e2bRequest()); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("error %v", err)
	}
	req := e2bRequest()
	req.AllowCreate = false
	got, err := b.Ensure(context.Background(), req)
	if err != nil || got.Handle != "created" || posts.Load() != 1 {
		t.Fatalf("got %+v err %v posts %d", got, err, posts.Load())
	}
}
func TestHTTPDiscoveryConflicts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"duplicate", `[{"sandboxID":"a","state":"running","metadata":{"obscura_uid":"uid","spec_hash":"hash"}},{"sandboxID":"b","state":"running","metadata":{"obscura_uid":"uid","spec_hash":"hash"}}]`, ErrConflict},
		{"wrong hash", `[{"sandboxID":"a","state":"running","metadata":{"obscura_uid":"uid","spec_hash":"other"}}]`, ErrConflict},
		{"absent", `[]`, ErrAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Fatal("unexpected creation")
				}
				w.Write([]byte(tc.body))
			})
			req := e2bRequest()
			req.AllowCreate = false
			if _, err := b.Ensure(context.Background(), req); !errors.Is(err, tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func TestHTTPDeleteConfirmsAbsence(t *testing.T) {
	absent := false
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		if absent {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"sandboxID":"a","state":"running"}`))
	})
	if done, err := b.Delete(context.Background(), "a"); done || err != nil {
		t.Fatalf("premature completion %v %v", done, err)
	}
	absent = true
	if done, err := b.Delete(context.Background(), "a"); !done || err != nil {
		t.Fatalf("not completed %v %v", done, err)
	}
}
func TestHTTPRejectsUnsupportedRequests(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP") })
	for _, mutate := range []func(*Request){func(r *Request) { r.Resources.VCPUs = 1 }, func(r *Request) { r.Resources.MemoryMiB = 128 }, func(r *Request) { r.Resources.DiskMiB = 100 }, func(r *Request) { r.Network.AllowEgress = false }, func(r *Request) { r.Network.AllowHosts = []string{"example.com"} }} {
		r := e2bRequest()
		mutate(&r)
		if err := b.Validate(r); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("got %v", err)
		}
	}
	executor, ok := b.(Executor)
	if !ok {
		t.Fatal("HTTP backend does not expose its execution capability")
	}
	if err := executor.Exec(context.Background(), "a", Execution{}, func(Event) error { return nil }); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("exec %v", err)
	}
}
func TestRegistryLoadsNamespacedSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: "tenant"}, Data: map[string][]byte{"token": []byte("secret-value")}}).Build()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret-value" {
			t.Error("missing credential")
		}
		w.Write([]byte(`{"sandboxID":"a","state":"running","envdAccessToken":"sensitive"}`))
	}))
	defer server.Close()
	config := Config{Type: "agentenv", Endpoint: server.URL, APIKeySecretName: "key", APIKeySecretKey: "token"}
	b, err := NewRegistry(reader).Open(context.Background(), "tenant", config)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := b.Observe(context.Background(), "a")
	if err != nil || got.Handle != "a" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := NewRegistry(reader).Open(context.Background(), "other", config); err == nil {
		t.Fatal("cross namespace credential accepted")
	}
}

type failingSecretReader struct {
	client.Reader
	err error
}

func (r failingSecretReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func TestSecretReadOutageIsNotAnAuthFailure(t *testing.T) {
	r := NewRegistry(failingSecretReader{err: apierrors.NewServiceUnavailable("API server unavailable")})
	_, err := r.Open(context.Background(), "tenant", Config{Type: "e2b", Endpoint: "http://runtime.test", APIKeySecretName: "key"})
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrAuth) {
		t.Fatalf("API outage misclassified: %v", err)
	}
}
func TestHTTPDoesNotFollowRedirect(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) })
	if _, err := b.Observe(context.Background(), "a"); err == nil {
		t.Fatal("accepted redirect")
	}
	if leaked {
		t.Fatal("followed redirect")
	}
}

func TestHTTPDiscoveryPaginatesBeforeCreation(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("unexpected creation")
		}
		if r.URL.Query().Get("nextToken") == "" {
			w.Header().Set("X-Next-Token", "next")
			w.Write([]byte("[]"))
			return
		}
		w.Write([]byte(`[{"sandboxID":"a","state":"paused","metadata":{"obscura_uid":"uid","spec_hash":"hash"}}]`))
	})
	got, err := b.Ensure(context.Background(), e2bRequest())
	if err != nil || got.Handle != "a" || got.State != Paused {
		t.Fatalf("got %v %v", got, err)
	}
}
func TestHTTPMalformedDiscoveryNeverCreates(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("unexpected creation")
		}
		w.Write([]byte("null"))
	})
	if _, err := b.Ensure(context.Background(), e2bRequest()); err == nil {
		t.Fatal("accepted null list")
	}
}

func TestMissingDiscoveryRouteDoesNotProveSandboxAbsent(t *testing.T) {
	b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("unexpected create")
		}
		w.WriteHeader(http.StatusNotFound)
	})
	r := e2bRequest()
	r.AllowCreate = false
	_, err := b.Ensure(context.Background(), r)
	if !errors.Is(err, ErrUnsupported) || errors.Is(err, ErrNotFound) {
		t.Fatalf("missing discovery route misclassified as absent sandbox: %v", err)
	}
}

func TestHTTPErrorCategories(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, ErrInvalid},
		{http.StatusUnauthorized, ErrAuth},
		{http.StatusForbidden, ErrAuth},
		{http.StatusConflict, ErrConflict},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusServiceUnavailable, ErrUnavailable},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			b := httpBackend(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })
			if _, err := b.Observe(context.Background(), "sandbox"); !errors.Is(err, tc.want) {
				t.Fatalf("status %d: got %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}
