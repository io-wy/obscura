package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	if resp.StatusCode == 404 {
		return resp.Header, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var category error
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			category = ErrInvalid
		case http.StatusUnauthorized, http.StatusForbidden:
			category = ErrAuth
		case http.StatusConflict:
			category = ErrConflict
		case http.StatusTooManyRequests:
			category = ErrRateLimited
		default:
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
				category = ErrUnavailable
			} else {
				category = ErrUnsupported
			}
		}
		return resp.Header, fmt.Errorf("%w: runtime HTTP status %d", category, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
			return resp.Header, fmt.Errorf("invalid runtime JSON response")
		}
	}
	return resp.Header, nil
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
func (b *httpBackendClient) Exec(context.Context, string, Execution, func(Event) error) error {
	return fmt.Errorf("%w: envd sessions are not implemented", ErrUnsupported)
}
