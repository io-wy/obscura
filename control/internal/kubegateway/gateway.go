// Package kubegateway exposes desired-state APIs and runtime execution sessions.
package kubegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/controller"
	"github.com/obscura/agentruntime/control/internal/quota"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const maxFileBytes = 32 << 20

type Gateway struct {
	Client  client.Client
	Factory backend.Factory
	Tenants TenantResolver
	Quota   *quota.Manager
}

func (g *Gateway) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	m.HandleFunc("POST /v1/sandboxes", g.create)
	m.HandleFunc("GET /v1/sandboxes/{sandboxID}", g.get)
	m.HandleFunc("DELETE /v1/sandboxes/{sandboxID}", g.delete)
	m.HandleFunc("POST /v1/sandboxes/{sandboxID}/execute", g.execute)
	m.HandleFunc("GET /v1/sandboxes/{sandboxID}/files", g.files)
	m.HandleFunc("PUT /v1/sandboxes/{sandboxID}/files", g.files)
	m.HandleFunc("GET /v1/sandboxes/{sandboxID}/pty", g.pty)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			m.ServeHTTP(w, r)
			return
		}
		if g.Tenants == nil {
			writeError(w, http.StatusServiceUnavailable, "tenant_registry_unavailable", "tenant registry is unavailable")
			return
		}
		tenant, err := g.Tenants.Resolve(r.Context(), r.Header.Get("X-API-Key"))
		if err != nil {
			if errors.Is(err, ErrTenantUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "tenant_registry_unavailable", "tenant registry is unavailable")
			} else {
				writeError(w, http.StatusUnauthorized, "unauthorized", "request is not authenticated")
			}
			return
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantContextKey{}, tenant)))
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	jsonResponse(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
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
	tenant := tenantFromContext(r.Context())
	var spec api.SandboxSpec
	if err := decode(w, r, &spec); err != nil || strings.TrimSpace(spec.RuntimeRef) == "" || strings.TrimSpace(spec.TemplateRef) == "" || spec.TimeoutSeconds < 1 || spec.TimeoutSeconds > 86400 {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid sandbox request")
		return
	}
	if v := spec.Resources; v != nil && (v.VCPUs < 1 || v.MemoryMiB < 1 || v.DiskMiB < 1) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid resource limits")
		return
	}
	name := "sbx-" + uuid.NewString()
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if len(key) > 256 {
			writeError(w, http.StatusBadRequest, "invalid_request", "idempotency key too long")
			return
		}
		h := sha256.Sum256([]byte(tenant.Name + "\x00" + key))
		name = "sbx-" + hex.EncodeToString(h[:])[:40]
	}
	if g.Quota == nil {
		writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "tenant quota service is unavailable")
		return
	}
	reservation := quota.SandboxRequest{SandboxID: name, SpecHash: controller.SpecHash(spec), Resources: spec.Resources, TimeoutSeconds: spec.TimeoutSeconds}
	if err := g.Quota.ReserveSandbox(r.Context(), tenant.Name, reservation); err != nil {
		switch {
		case errors.Is(err, quota.ErrQuotaExceeded):
			writeError(w, http.StatusTooManyRequests, "quota_exceeded", "tenant sandbox quota exceeded")
		case errors.Is(err, quota.ErrReservationConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key already used for a different request")
		default:
			writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "tenant quota service is unavailable")
		}
		return
	}
	s := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenant.Spec.Namespace, Labels: map[string]string{"sandbox.obscura.dev/tenant": tenant.Name}}, Spec: spec}
	if err := g.Client.Create(r.Context(), s); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsMethodNotSupported(err) {
				_ = g.Quota.ReleaseSandbox(r.Context(), tenant.Name, name)
			}
			writeError(w, http.StatusServiceUnavailable, "sandbox_persistence_unavailable", "unable to persist sandbox")
			return
		}
		existing := &api.Sandbox{}
		if err := g.Client.Get(r.Context(), client.ObjectKeyFromObject(s), existing); err != nil {
			writeError(w, http.StatusServiceUnavailable, "sandbox_state_unavailable", "unable to read sandbox")
			return
		}
		s = existing
		if controller.SpecHash(s.Spec) != controller.SpecHash(spec) {
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency key already used for a different request")
			return
		}
		if !s.DeletionTimestamp.IsZero() {
			writeError(w, http.StatusConflict, "sandbox_terminating", "sandbox is terminating")
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
	tenant := tenantFromContext(r.Context())
	s := &api.Sandbox{}
	if err := g.Client.Get(r.Context(), client.ObjectKey{Namespace: tenant.Spec.Namespace, Name: id}, s); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "sandbox_not_found", "sandbox not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "sandbox_state_unavailable", "sandbox state unavailable")
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
		writeError(w, http.StatusServiceUnavailable, "sandbox_deletion_unavailable", "unable to request deletion")
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
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid execution request")
		return
	}
	s := g.load(w, r)
	if s == nil {
		return
	}
	if !s.DeletionTimestamp.IsZero() || s.Status.Phase != "Ready" || s.Status.Binding == nil || s.Status.Binding.Handle == "" || s.Status.Binding.SpecHash != controller.SpecHash(s.Spec) {
		writeError(w, http.StatusConflict, "sandbox_not_ready", "sandbox is not ready")
		return
	}
	if g.Factory == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable")
		return
	}
	ttl := time.Duration(input.TimeoutSeconds) * time.Second
	if ttl == 0 {
		ttl = 300 * time.Second
	}
	deadline := time.Now().Add(ttl)
	if s.Status.ExpiresAt == nil || !time.Now().Before(s.Status.ExpiresAt.Time) {
		writeError(w, http.StatusConflict, "sandbox_expired", "sandbox has expired")
		return
	}
	if deadline.After(s.Status.ExpiresAt.Time) {
		deadline = s.Status.ExpiresAt.Time
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	tenant := tenantFromContext(r.Context())
	execution := backend.Execution{ID: uuid.NewString(), Command: input.Command, Input: []byte(input.Input), EnvVars: input.EnvVars, Deadline: deadline}
	if g.Quota == nil {
		writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "tenant quota service is unavailable")
		return
	}
	if err := g.Quota.AcquireSession(ctx, tenant.Name, quota.SessionRequest{ID: execution.ID, SandboxID: s.Name, Kind: quota.SessionExecution, TTL: time.Until(deadline)}); err != nil {
		if errors.Is(err, quota.ErrQuotaExceeded) {
			writeError(w, http.StatusTooManyRequests, "session_quota_exceeded", "tenant execution session quota exceeded")
		} else {
			writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "tenant quota service is unavailable")
		}
		return
	}
	defer func() { _ = g.Quota.ReleaseSession(context.Background(), tenant.Name, execution.ID) }()
	b, err := g.Factory.Open(ctx, tenant.Spec.Namespace, controller.Config(s.Status.Binding.Config))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable")
		return
	}
	defer b.Close()
	executor, ok := b.(backend.Executor)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unsupported", "runtime does not support execution through this gateway")
		return
	}
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
	if err := executor.Exec(ctx, s.Status.Binding.Handle, execution, emit); err != nil {
		if started {
			_ = emit(backend.Event{ExecutionID: execution.ID, Type: "transport_error", Error: "runtime execution interrupted"})
		} else {
			writeBackendError(w, err, "execution")
		}
		return
	}
	if !started {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (g *Gateway) files(w http.ResponseWriter, r *http.Request) {
	s := g.load(w, r)
	if s == nil {
		return
	}
	if !s.DeletionTimestamp.IsZero() || s.Status.Phase != "Ready" || s.Status.Binding == nil || s.Status.Binding.Handle == "" || s.Status.Binding.SpecHash != controller.SpecHash(s.Spec) {
		writeError(w, http.StatusConflict, "sandbox_not_ready", "sandbox is not ready")
		return
	}
	if s.Status.ExpiresAt == nil || !time.Now().Before(s.Status.ExpiresAt.Time) {
		writeError(w, http.StatusConflict, "sandbox_expired", "sandbox has expired")
		return
	}
	deadline := time.Now().Add(time.Minute)
	if deadline.After(s.Status.ExpiresAt.Time) {
		deadline = s.Status.ExpiresAt.Time
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	if g.Factory == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable")
		return
	}
	tenant := tenantFromContext(r.Context())
	b, err := g.Factory.Open(ctx, tenant.Spec.Namespace, controller.Config(s.Status.Binding.Config))
	if err != nil {
		writeBackendError(w, err, "file operation")
		return
	}
	defer b.Close()
	files, ok := b.(backend.FileSystem)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unsupported", "runtime does not support file operations through this gateway")
		return
	}
	filePath := r.URL.Query().Get("path")
	if r.Method == http.MethodGet {
		file, err := files.ReadFile(ctx, s.Status.Binding.Handle, filePath)
		if err != nil {
			writeBackendError(w, err, "file read")
			return
		}
		defer file.Body.Close()
		if file.Size > maxFileBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "transfer_limit_exceeded", "file exceeds the transfer limit")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file.Body, maxFileBytes+1))
		if err != nil {
			writeError(w, http.StatusBadGateway, "runtime_file_unavailable", "runtime file read failed")
			return
		}
		if len(data) > maxFileBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "transfer_limit_exceeded", "file exceeds the transfer limit")
			return
		}
		contentType := file.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, bytes.NewReader(data))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "transfer_limit_exceeded", "file exceeds the transfer limit")
		return
	}
	if err := files.WriteFile(ctx, s.Status.Binding.Handle, backend.FileWrite{Path: filePath, Body: bytes.NewReader(data), Size: int64(len(data)), ContentType: r.Header.Get("Content-Type")}); err != nil {
		writeBackendError(w, err, "file write")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeBackendError(w http.ResponseWriter, err error, operation string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "runtime_timeout", "runtime "+operation+" timed out")
	case errors.Is(err, backend.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "capability_unsupported", "runtime does not support this operation")
	case errors.Is(err, backend.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "runtime rejected the request")
	case errors.Is(err, backend.ErrNotFound):
		writeError(w, http.StatusNotFound, "file_not_found", "requested runtime resource was not found")
	case errors.Is(err, backend.ErrLimit):
		writeError(w, http.StatusRequestEntityTooLarge, "transfer_limit_exceeded", "operation exceeds the transfer limit")
	case errors.Is(err, backend.ErrRateLimited):
		writeError(w, http.StatusServiceUnavailable, "runtime_rate_limited", "runtime is rate limited")
	case errors.Is(err, backend.ErrAuth):
		writeError(w, http.StatusBadGateway, "runtime_authentication_failed", "runtime authentication failed")
	default:
		writeError(w, http.StatusBadGateway, "runtime_unavailable", "runtime "+operation+" is unavailable")
	}
}

func (g *Gateway) pty(w http.ResponseWriter, r *http.Request) {
	s := g.load(w, r)
	if s == nil {
		return
	}
	if !s.DeletionTimestamp.IsZero() || s.Status.Phase != "Ready" || s.Status.Binding == nil || s.Status.Binding.Handle == "" || s.Status.Binding.SpecHash != controller.SpecHash(s.Spec) {
		writeError(w, http.StatusConflict, "sandbox_not_ready", "sandbox is not ready")
		return
	}
	if s.Status.ExpiresAt == nil || !time.Now().Before(s.Status.ExpiresAt.Time) {
		writeError(w, http.StatusConflict, "sandbox_expired", "sandbox has expired")
		return
	}
	if g.Factory == nil || g.Quota == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime or quota service is unavailable")
		return
	}
	tenant := tenantFromContext(r.Context())
	b, err := g.Factory.Open(r.Context(), tenant.Spec.Namespace, controller.Config(s.Status.Binding.Config))
	if err != nil {
		writeBackendError(w, err, "PTY session")
		return
	}
	defer b.Close()
	pty, ok := b.(backend.PTY)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unsupported", "runtime does not support PTY sessions through this gateway")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 << 10)
	startContext, cancelStart := context.WithTimeout(r.Context(), 10*time.Second)
	var start struct {
		Type           string            `json:"type"`
		Command        string            `json:"command"`
		EnvVars        map[string]string `json:"envVars,omitempty"`
		Columns        uint16            `json:"columns"`
		Rows           uint16            `json:"rows"`
		TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	}
	err = wsjson.Read(startContext, conn, &start)
	cancelStart()
	if err != nil || start.Type != "start" || start.Command == "" || start.Columns == 0 || start.Rows == 0 || start.TimeoutSeconds < 0 || start.TimeoutSeconds > 1800 {
		_ = wsjson.Write(context.Background(), conn, backend.PTYMessage{Type: "error", Error: "invalid PTY start request"})
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid PTY start")
		return
	}
	ttl := time.Duration(start.TimeoutSeconds) * time.Second
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	deadline := time.Now().Add(ttl)
	if deadline.After(s.Status.ExpiresAt.Time) {
		deadline = s.Status.ExpiresAt.Time
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	sessionID := uuid.NewString()
	if err := g.Quota.AcquireSession(ctx, tenant.Name, quota.SessionRequest{ID: sessionID, SandboxID: s.Name, Kind: quota.SessionPTY, TTL: time.Until(deadline)}); err != nil {
		message := "PTY quota is unavailable"
		if errors.Is(err, quota.ErrQuotaExceeded) {
			message = "PTY session quota exceeded"
		}
		_ = wsjson.Write(context.Background(), conn, backend.PTYMessage{Type: "error", Error: message})
		_ = conn.Close(websocket.StatusPolicyViolation, "PTY unavailable")
		return
	}
	defer func() {
		releaseContext, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		_ = g.Quota.ReleaseSession(releaseContext, tenant.Name, sessionID)
	}()
	session, err := pty.OpenPTY(ctx, s.Status.Binding.Handle, backend.PTYRequest{ID: sessionID, Command: start.Command, EnvVars: start.EnvVars, Columns: start.Columns, Rows: start.Rows, Deadline: deadline})
	if err != nil {
		_ = wsjson.Write(context.Background(), conn, backend.PTYMessage{Type: "error", Error: "runtime PTY session unavailable"})
		_ = conn.Close(websocket.StatusInternalError, "runtime PTY unavailable")
		return
	}
	defer session.Close()
	type bridgeResult struct {
		terminal bool
		err      error
	}
	results := make(chan bridgeResult, 2)
	go func() {
		for {
			var message backend.PTYMessage
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				results <- bridgeResult{err: err}
				return
			}
			if message.Type != "input" && message.Type != "resize" {
				results <- bridgeResult{err: backend.ErrInvalid}
				return
			}
			if err := session.Send(ctx, message); err != nil {
				results <- bridgeResult{err: err}
				return
			}
		}
	}()
	go func() {
		for {
			message, err := session.Receive(ctx)
			if err != nil {
				results <- bridgeResult{err: err}
				return
			}
			if err := wsjson.Write(ctx, conn, message); err != nil {
				results <- bridgeResult{err: err}
				return
			}
			if message.Type == "exit" || message.Type == "error" {
				results <- bridgeResult{terminal: true}
				return
			}
		}
	}()
	result := <-results
	cancel()
	_ = session.Close()
	if result.terminal {
		_ = conn.Close(websocket.StatusNormalClosure, "PTY complete")
		return
	}
	_ = conn.Close(websocket.StatusInternalError, "PTY interrupted")
}
