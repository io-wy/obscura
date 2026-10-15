package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"
)

// httpBackendClient implements the E2B lifecycle subset, not envd or full SDK compatibility.
// GET /v2/sandboxes uses URL-encoded metadata, comma-separated states and X-Next-Token.
// See https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml. Deployments lacking
// this versioned discovery contract fail closed; there is no unsafe v1 list fallback.
type httpBackendClient struct {
	endpoint string
	key      string
	client   *http.Client
}
type remoteSandbox struct {
	ID       string            `json:"sandboxID"`
	State    string            `json:"state"`
	Metadata map[string]string `json:"metadata"`
}

const (
	maxHTTPEventBytes  = 256 << 10
	maxHTTPOutputBytes = 16 << 20
)

func (b *httpBackendClient) Close() error { b.client.CloseIdleConnections(); return nil }
func (b *httpBackendClient) Validate(r Request) error {
	if r.ID == "" || r.SpecHash == "" || r.Template == "" {
		return fmt.Errorf("sandbox ID, specification hash and template are required")
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 2147483647 {
		return fmt.Errorf("timeout must be between 1 and 2147483647 seconds")
	}
	if r.Resources != (Resources{}) {
		return fmt.Errorf("%w: resources must be configured in the E2B template", ErrUnsupported)
	}
	// E2B deployments differ in network support. Reject policy requests until the
	// selected deployment has an explicitly tested network capability adapter.
	if !r.Network.AllowEgress || len(r.Network.AllowHosts) > 0 {
		return fmt.Errorf("%w: E2B network enforcement is not verified", ErrUnsupported)
	}
	return nil
}
func (b *httpBackendClient) request(ctx context.Context, method, path string, body any, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var data io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint+path, data)
	if err != nil {
		return nil, fmt.Errorf("invalid runtime request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.key != "" {
		req.Header.Set("X-API-Key", b.key)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: runtime HTTP request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	if err := httpResponseError(resp.StatusCode); err != nil {
		return resp.Header, err
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
			return resp.Header, fmt.Errorf("invalid runtime JSON response")
		}
	}
	return resp.Header, nil
}

func httpResponseError(status int) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	var category error
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		category = ErrInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		category = ErrAuth
	case http.StatusConflict:
		category = ErrConflict
	case http.StatusTooManyRequests:
		category = ErrRateLimited
	default:
		if status >= 500 || status == http.StatusRequestTimeout {
			category = ErrUnavailable
		} else {
			category = ErrUnsupported
		}
	}
	return fmt.Errorf("%w: runtime HTTP status %d", category, status)
}
func (b *httpBackendClient) discover(ctx context.Context, r Request) (Observation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	filter := url.Values{"obscura_uid": {r.ID}}.Encode()
	query := url.Values{"metadata": {filter}, "state": {"running,paused"}, "limit": {"100"}}
	seenTokens := map[string]bool{}
	var match *remoteSandbox
	for pages := 0; pages < 1000; pages++ {
		var items []remoteSandbox
		headers, err := b.request(ctx, "GET", "/v2/sandboxes?"+query.Encode(), nil, &items)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Missing discovery route is not evidence that an instance is absent.
				return Observation{}, false, fmt.Errorf("%w: runtime lacks v2 discovery", ErrUnsupported)
			}
			return Observation{}, false, err
		}
		if items == nil {
			return Observation{}, false, fmt.Errorf("runtime discovery must return a JSON array")
		}
		for _, item := range items {
			if item.Metadata["obscura_uid"] != r.ID {
				continue
			}
			if item.Metadata["spec_hash"] != r.SpecHash || item.ID == "" || match != nil {
				return Observation{}, false, ErrConflict
			}
			copy := item
			match = &copy
		}
		next := headers.Get("X-Next-Token")
		if next == "" {
			if match == nil {
				return Observation{}, false, nil
			}
			obs, err := httpObservation(*match)
			return obs, true, err
		}
		if seenTokens[next] {
			return Observation{}, false, fmt.Errorf("invalid repeated runtime pagination token")
		}
		seenTokens[next] = true
		query.Set("nextToken", next)
	}
	return Observation{}, false, fmt.Errorf("runtime discovery pagination limit exceeded")
}
func (b *httpBackendClient) Ensure(ctx context.Context, r Request) (Observation, error) {
	if err := b.Validate(r); err != nil {
		return Observation{}, err
	}
	if obs, found, err := b.discover(ctx, r); err != nil {
		return Observation{}, err
	} else if found {
		return obs, nil
	}
	if !r.AllowCreate {
		return Observation{}, ErrAmbiguous
	}
	body := map[string]any{"templateID": r.Template, "timeout": r.TimeoutSeconds, "metadata": map[string]string{"obscura_uid": r.ID, "spec_hash": r.SpecHash}}
	var created remoteSandbox
	if _, err := b.request(ctx, "POST", "/sandboxes", body, &created); err != nil {
		return Observation{}, ErrAmbiguous
	}
	if created.ID == "" {
		return Observation{}, ErrAmbiguous
	}
	// The v1 create response omits state; creation completion signifies running.
	if created.State == "" {
		created.State = "running"
	}
	return httpObservation(created)
}
func httpObservation(s remoteSandbox) (Observation, error) {
	if s.ID == "" {
		return Observation{}, fmt.Errorf("runtime response has no sandbox ID")
	}
	state := State("")
	switch s.State {
	case "running":
		state = Ready
	case "paused":
		state = Paused
	default:
		return Observation{}, fmt.Errorf("unsupported runtime sandbox state")
	}
	return Observation{Handle: s.ID, State: state}, nil
}
func (b *httpBackendClient) Observe(ctx context.Context, id string) (Observation, error) {
	if id == "" {
		return Observation{}, fmt.Errorf("sandbox handle is required")
	}
	var sandbox remoteSandbox
	_, err := b.request(ctx, "GET", "/sandboxes/"+url.PathEscape(id), nil, &sandbox)
	if err != nil {
		return Observation{}, err
	}
	if sandbox.ID != id {
		return Observation{}, ErrConflict
	}
	return httpObservation(sandbox)
}
func (b *httpBackendClient) Delete(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("sandbox handle is required")
	}
	_, err := b.request(ctx, "DELETE", "/sandboxes/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_, err = b.Observe(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return false, err
}
func (b *httpBackendClient) Exec(ctx context.Context, id string, execution Execution, emit func(Event) error) error {
	if id == "" || execution.ID == "" || execution.Command == "" || emit == nil {
		return fmt.Errorf("%w: sandbox, execution, command and event receiver are required", ErrInvalid)
	}
	if !execution.Deadline.IsZero() {
		if !execution.Deadline.After(time.Now()) {
			return context.DeadlineExceeded
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, execution.Deadline)
		defer cancel()
	}
	body := struct {
		ExecutionID      string            `json:"executionID"`
		Command          string            `json:"command"`
		Input            string            `json:"input,omitempty"`
		EnvVars          map[string]string `json:"envVars,omitempty"`
		DeadlineUnixNano int64             `json:"deadlineUnixNano"`
	}{execution.ID, execution.Command, string(execution.Input), execution.EnvVars, execution.Deadline.UnixNano()}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: encode execution request", ErrInvalid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint+"/sandboxes/"+url.PathEscape(id)+"/commands", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("%w: invalid execution request", ErrInvalid)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("Content-Type", "application/json")
	if b.key != "" {
		req.Header.Set("X-API-Key", b.key)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: runtime execution request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	if err := httpResponseError(resp.StatusCode); err != nil {
		return err
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-ndjson" {
		return fmt.Errorf("%w: runtime execution response is not NDJSON", ErrUnavailable)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), maxHTTPEventBytes)
	var sequence uint64
	var outputBytes int
	for scanner.Scan() {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var event Event
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("%w: invalid runtime execution event", ErrUnavailable)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: invalid trailing runtime event data", ErrUnavailable)
		}
		if event.ExecutionID != execution.ID || event.Sequence != sequence+1 || !validEventType(event.Type) {
			return fmt.Errorf("%w: invalid runtime execution event identity or sequence", ErrUnavailable)
		}
		sequence = event.Sequence
		outputBytes += len(event.Payload) + len(event.Error)
		if outputBytes > maxHTTPOutputBytes {
			return fmt.Errorf("%w: runtime execution output limit exceeded", ErrUnavailable)
		}
		if err := emit(event); err != nil {
			return err
		}
		if event.Type == "result" || event.Type == "error" {
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: runtime execution stream failed", ErrUnavailable)
	}
	return fmt.Errorf("%w: runtime execution stream ended before a terminal event", ErrUnavailable)
}

func validEventType(eventType string) bool {
	switch eventType {
	case "started", "stdout", "stderr", "result", "error":
		return true
	default:
		return false
	}
}
