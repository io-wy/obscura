package devlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"github.com/obscura/agentruntime/control/internal/devruntime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type gatewayClient struct{ url, key string }
type sandboxView struct {
	SandboxID string `json:"sandboxID"`
	Phase     string `json:"phase"`
}

func (g gatewayClient) request(ctx context.Context, method, path, body, key string, want int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.url+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", g.key)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != want {
		return nil, fmt.Errorf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, want, data)
	}
	return data, nil
}
func spec(ttl int) string {
	return fmt.Sprintf(`{"runtimeRef":"local","templateRef":"dev","resources":{"vcpus":1,"memoryMiB":128,"diskMiB":128},"network":{"allowEgress":false},"timeoutSeconds":%d}`, ttl)
}
func (g gatewayClient) create(ctx context.Context, ttl int, key string) (string, error) {
	data, err := g.request(ctx, "POST", "/v1/sandboxes", spec(ttl), key, http.StatusAccepted)
	if err != nil {
		return "", err
	}
	var view sandboxView
	if err := json.Unmarshal(data, &view); err != nil {
		return "", err
	}
	if view.SandboxID == "" {
		return "", fmt.Errorf("missing sandboxID: %s", data)
	}
	return view.SandboxID, nil
}
func poll(ctx context.Context, check func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (g gatewayClient) phase(ctx context.Context, id, phase string) error {
	return poll(ctx, func() (bool, error) {
		data, err := g.request(ctx, "GET", "/v1/sandboxes/"+id, "", "", http.StatusOK)
		if err != nil {
			return false, err
		}
		var view sandboxView
		if err := json.Unmarshal(data, &view); err != nil {
			return false, err
		}
		return view.Phase == phase, nil
	})
}
func (g gatewayClient) absent(ctx context.Context, id string) error {
	return poll(ctx, func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", g.url+"/v1/sandboxes/"+id, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("X-API-Key", g.key)
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return false, err
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return true, nil
		}
		if response.StatusCode != http.StatusOK {
			return false, fmt.Errorf("waiting for deletion: HTTP %d", response.StatusCode)
		}
		return false, nil
	})
}
func (g gatewayClient) cleanup(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = g.request(ctx, "DELETE", "/v1/sandboxes/"+id, "", "", http.StatusAccepted)
}

// Smoke runs against a running local lab; it creates uniquely identified resources.
func Smoke(ctx context.Context, url, key string) error {
	g := gatewayClient{url: strings.TrimRight(url, "/"), key: key}
	unauthorized := g
	unauthorized.key = "invalid-" + key
	if _, err := unauthorized.request(ctx, "POST", "/v1/sandboxes", spec(60), "", http.StatusUnauthorized); err != nil {
		return err
	}
	requestID := "smoke-" + uuid.NewString()
	id, err := g.create(ctx, 120, requestID)
	if err != nil {
		return err
	}
	defer g.cleanup(id)
	retry, err := g.create(ctx, 120, requestID)
	if err != nil {
		return err
	}
	if retry != id {
		return fmt.Errorf("idempotency changed sandbox identity")
	}
	if _, err := g.request(ctx, "POST", "/v1/sandboxes", spec(121), requestID, http.StatusConflict); err != nil {
		return err
	}
	if err := g.phase(ctx, id, "Ready"); err != nil {
		return fmt.Errorf("wait Ready: %w", err)
	}
	for _, command := range []string{"echo hello", "demo:fail"} {
		body, _ := json.Marshal(map[string]any{"command": command, "timeoutSeconds": 5})
		data, err := g.request(ctx, "POST", "/v1/sandboxes/"+id+"/execute", string(body), "", http.StatusOK)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		var events []backend.Event
		for decoder.More() {
			var event backend.Event
			if err := decoder.Decode(&event); err != nil {
				return err
			}
			events = append(events, event)
		}
		last := "result"
		if command == "demo:fail" {
			last = "error"
		}
		if len(events) != 3 || events[0].Type != "started" || events[1].Type != "stdout" || events[2].Type != last {
			return fmt.Errorf("unexpected execution stream: %s", data)
		}
		for i, event := range events {
			if event.ExecutionID == "" || event.ExecutionID != events[0].ExecutionID || event.Sequence != uint64(i+1) {
				return fmt.Errorf("invalid event identity/sequence: %s", data)
			}
		}
	}
	if _, err := g.request(ctx, "DELETE", "/v1/sandboxes/"+id, "", "", http.StatusAccepted); err != nil {
		return err
	}
	if err := g.absent(ctx, id); err != nil {
		return err
	}
	expiring, err := g.create(ctx, 2, "")
	if err != nil {
		return err
	}
	defer g.cleanup(expiring)
	return g.absent(ctx, expiring)
}

// ExerciseFaults additionally checks the manager, binding recovery and finalizers.
func ExerciseFaults(ctx context.Context, l *Lab) error {
	g := gatewayClient{url: l.GatewayURL, key: l.APIKey}
	id, err := g.create(ctx, 120, "")
	if err != nil {
		return err
	}
	defer g.cleanup(id)
	defer l.Runtime.SetFaults(devruntime.Faults{})
	if err := g.phase(ctx, id, "Ready"); err != nil {
		return err
	}
	var before api.Sandbox
	objectKey := client.ObjectKey{Namespace: l.Namespace, Name: id}
	if err := l.Client.Get(ctx, objectKey, &before); err != nil {
		return err
	}
	created := l.Runtime.Snapshot().Created
	if err := l.RestartController(ctx); err != nil {
		return err
	}
	if err := g.phase(ctx, id, "Ready"); err != nil {
		return err
	}
	l.Runtime.SetFaults(devruntime.Faults{Unavailable: true})
	if err := g.phase(ctx, id, "Unknown"); err != nil {
		return fmt.Errorf("runtime outage not observed: %w", err)
	}
	l.Runtime.SetFaults(devruntime.Faults{})
	if err := g.phase(ctx, id, "Ready"); err != nil {
		return err
	}
	var after api.Sandbox
	if err := l.Client.Get(ctx, objectKey, &after); err != nil {
		return err
	}
	if after.Status.Binding.Handle != before.Status.Binding.Handle || l.Runtime.Snapshot().Created != created {
		return fmt.Errorf("restart/outage replaced the bound instance")
	}
	changed := after.DeepCopy()
	changed.Spec.TemplateRef = "different"
	if err := l.Client.Update(ctx, changed); !apierrors.IsInvalid(err) {
		return fmt.Errorf("immutable spec update should fail: %v", err)
	}
	l.Runtime.SetFaults(devruntime.Faults{HoldDeletes: true})
	if _, err := g.request(ctx, "DELETE", "/v1/sandboxes/"+id, "", "", http.StatusAccepted); err != nil {
		return err
	}
	if err := g.phase(ctx, id, "Terminating"); err != nil {
		return err
	}
	if err := l.Client.Get(ctx, objectKey, &after); err != nil {
		return err
	}
	if after.DeletionTimestamp.IsZero() || len(after.Finalizers) == 0 {
		return fmt.Errorf("cleanup finalizer released before runtime confirmed deletion")
	}
	l.Runtime.SetFaults(devruntime.Faults{})
	if err := g.absent(ctx, id); err != nil {
		return err
	}
	if l.Runtime.Snapshot().Active != 0 {
		return fmt.Errorf("runtime resource leak: %+v", l.Runtime.Snapshot())
	}
	return nil
}
