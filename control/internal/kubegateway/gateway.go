// Package kubegateway exposes desired-state APIs and runtime execution sessions.
package kubegateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Gateway struct {
	Client  client.Client
	Factory backend.Factory
	// Each gateway is restricted to one tenant namespace and one API key.
	Namespace string
	APIKey    string
}

func (g *Gateway) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	m.HandleFunc("POST /v1/sandboxes", g.create)
	m.HandleFunc("GET /v1/sandboxes/{sandboxID}", g.get)
	m.HandleFunc("DELETE /v1/sandboxes/{sandboxID}", g.delete)
	m.HandleFunc("POST /v1/sandboxes/{sandboxID}/execute", g.execute)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && (g.APIKey == "" || subtle.ConstantTimeCompare([]byte(g.APIKey), []byte(r.Header.Get("X-API-Key"))) != 1) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		m.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing data")
	}
	return nil
}
func jsonResponse(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func public(s *api.Sandbox) any {
	return struct {
		SandboxID  string             `json:"sandboxID"`
		Phase      string             `json:"phase"`
		Conditions []metav1.Condition `json:"conditions,omitempty"`
		ExpiresAt  *metav1.Time       `json:"expiresAt,omitempty"`
	}{s.Name, s.Status.Phase, s.Status.Conditions, s.Status.ExpiresAt}
}

func (g *Gateway) create(w http.ResponseWriter, r *http.Request) {
	var spec api.SandboxSpec
	if err := decode(w, r, &spec); err != nil || strings.TrimSpace(spec.RuntimeRef) == "" || strings.TrimSpace(spec.TemplateRef) == "" || spec.TimeoutSeconds < 1 || spec.TimeoutSeconds > 86400 {
		http.Error(w, "invalid sandbox request", http.StatusBadRequest)
		return
	}
	if v := spec.Resources; v != nil && (v.VCPUs < 1 || v.MemoryMiB < 1 || v.DiskMiB < 1) {
		http.Error(w, "invalid resource limits", http.StatusBadRequest)
		return
	}
	name := "sbx-" + uuid.NewString()
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if len(key) > 256 {
			http.Error(w, "idempotency key too long", http.StatusBadRequest)
			return
		}
		h := sha256.Sum256([]byte(g.Namespace + "\x00" + key))
		name = "sbx-" + hex.EncodeToString(h[:])[:40]
	}
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: g.Namespace}, Spec: spec}
	if err := g.Client.Create(r.Context(), s); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			http.Error(w, "unable to persist sandbox", http.StatusServiceUnavailable)
			return
		}
		existing := &api.Sandbox{}
		if err := g.Client.Get(r.Context(), client.ObjectKeyFromObject(s), existing); err != nil {
			http.Error(w, "unable to read sandbox", http.StatusServiceUnavailable)
			return
		}
		s = existing
		if controller.SpecHash(s.Spec) != controller.SpecHash(spec) {
			http.Error(w, "idempotency key already used for a different request", http.StatusConflict)
			return
		}
		if !s.DeletionTimestamp.IsZero() {
			http.Error(w, "sandbox is terminating", http.StatusConflict)
			return
		}
	}
	if s.Status.Phase == "" {
		s.Status.Phase = "Pending"
	}
	w.Header().Set("Location", "/v1/sandboxes/"+s.Name)
	jsonResponse(w, http.StatusAccepted, public(s))
}
func (g *Gateway) load(w http.ResponseWriter, r *http.Request) *api.Sandbox {
	id := r.PathValue("sandboxID")
	s := &api.Sandbox{}
	if err := g.Client.Get(r.Context(), client.ObjectKey{Namespace: g.Namespace, Name: id}, s); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "sandbox not found", http.StatusNotFound)
		} else {
			http.Error(w, "sandbox state unavailable", http.StatusServiceUnavailable)
		}
		return nil
	}
	return s
}
func (g *Gateway) get(w http.ResponseWriter, r *http.Request) {
	if s := g.load(w, r); s != nil {
		jsonResponse(w, http.StatusOK, public(s))
	}
}
func (g *Gateway) delete(w http.ResponseWriter, r *http.Request) {
	s := g.load(w, r)
	if s == nil {
		return
	}
	if err := g.Client.Delete(r.Context(), s); err != nil && !apierrors.IsNotFound(err) {
		http.Error(w, "unable to request deletion", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{"sandboxID": s.Name, "phase": "Terminating"})
}

func (g *Gateway) execute(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Command        string            `json:"command"`
		Input          string            `json:"input,omitempty"`
		EnvVars        map[string]string `json:"envVars,omitempty"`
		TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	}
	if err := decode(w, r, &input); err != nil || input.Command == "" || input.TimeoutSeconds < 0 || input.TimeoutSeconds > 3600 {
		http.Error(w, "invalid execution request", http.StatusBadRequest)
		return
	}
	s := g.load(w, r)
	if s == nil {
		return
	}
	if !s.DeletionTimestamp.IsZero() || s.Status.Phase != "Ready" || s.Status.Binding == nil || s.Status.Binding.Handle == "" || s.Status.Binding.SpecHash != controller.SpecHash(s.Spec) {
		http.Error(w, "sandbox is not ready", http.StatusConflict)
		return
	}
	if g.Factory == nil {
		http.Error(w, "runtime unavailable", http.StatusServiceUnavailable)
		return
	}
	ttl := time.Duration(input.TimeoutSeconds) * time.Second
	if ttl == 0 {
		ttl = 300 * time.Second
	}
	deadline := time.Now().Add(ttl)
	if s.Status.ExpiresAt == nil || !time.Now().Before(s.Status.ExpiresAt.Time) {
		http.Error(w, "sandbox has expired", http.StatusConflict)
		return
	}
	if deadline.After(s.Status.ExpiresAt.Time) {
		deadline = s.Status.ExpiresAt.Time
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	b, err := g.Factory.Open(ctx, g.Namespace, controller.Config(s.Status.Binding.Config))
	if err != nil {
		http.Error(w, "runtime unavailable", http.StatusServiceUnavailable)
		return
	}
	defer b.Close()
	execution := backend.Execution{ID: uuid.NewString(), Command: input.Command, Input: []byte(input.Input), EnvVars: input.EnvVars, Deadline: deadline}
	started := false
	emit := func(e backend.Event) error {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if err := json.NewEncoder(w).Encode(e); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	if err := b.Exec(ctx, s.Status.Binding.Handle, execution, emit); err != nil {
		if started {
			_ = emit(backend.Event{ExecutionID: execution.ID, Type: "transport_error", Error: "runtime execution interrupted"})
		} else if errors.Is(err, backend.ErrUnsupported) {
			http.Error(w, "runtime does not support execution through this gateway", http.StatusNotImplemented)
		} else {
			http.Error(w, "runtime execution unavailable", http.StatusBadGateway)
		}
		return
	}
	if !started {
		w.WriteHeader(http.StatusNoContent)
	}
}
