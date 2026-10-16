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
	"path"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// httpBackendClient is the shared HTTP runtime transport. Provider-specific
// paths, authentication, and stream protocols come from profile; this client
// owns lifecycle safety, limits, cancellation, and error mapping.
type httpBackendClient struct {
	endpoint     string
	dataEndpoint string
	key          string
	profile      httpRuntimeProfile
	client       *http.Client
}
type remoteSandbox struct {
	ID                 string            `json:"sandboxID"`
	State              string            `json:"state"`
	Metadata           map[string]string `json:"metadata"`
	EnvdAccessToken    string            `json:"envdAccessToken,omitempty"`
	TrafficAccessToken string            `json:"trafficAccessToken,omitempty"`
	Domain             string            `json:"domain,omitempty"`
}

const (
	maxHTTPEventBytes  = 256 << 10
	maxHTTPOutputBytes = 16 << 20
	maxHTTPFileBytes   = 32 << 20
	maxHTTPProxyBytes  = 32 << 20
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
		return fmt.Errorf("%w: resources must be configured in the %s template", ErrUnsupported, b.profile.Name)
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
	b.profile.auth(req, b.key)
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
		headers, err := b.request(ctx, "GET", b.profile.DiscoveryPath+"?"+query.Encode(), nil, &items)
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
			obs, err := b.httpObservation(*match)
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
	if _, err := b.request(ctx, "POST", b.profile.CreatePath, body, &created); err != nil {
		return Observation{}, ErrAmbiguous
	}
	if created.ID == "" {
		return Observation{}, ErrAmbiguous
	}
	// The v1 create response omits state; creation completion signifies running.
	if created.State == "" {
		created.State = "running"
	}
	return b.httpObservation(created)
}
func (b *httpBackendClient) httpObservation(s remoteSandbox) (Observation, error) {
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
	capabilities := make(map[string]bool, len(b.profile.Capabilities))
	for name, supported := range b.profile.Capabilities {
		capabilities[name] = supported
	}
	return Observation{Handle: s.ID, State: state, Capabilities: capabilities}, nil
}
func (b *httpBackendClient) Observe(ctx context.Context, id string) (Observation, error) {
	if id == "" {
		return Observation{}, fmt.Errorf("sandbox handle is required")
	}
	var sandbox remoteSandbox
	_, err := b.request(ctx, "GET", b.profile.SandboxPath(url.PathEscape(id)), nil, &sandbox)
	if err != nil {
		return Observation{}, err
	}
	if sandbox.ID != id {
		return Observation{}, ErrConflict
	}
	return b.httpObservation(sandbox)
}
func (b *httpBackendClient) Delete(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("sandbox handle is required")
	}
	_, err := b.request(ctx, "DELETE", b.profile.SandboxPath(url.PathEscape(id)), nil, nil)
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
	if b.profile.ExecuteProtocol != "ndjson" {
		return fmt.Errorf("%w: %s execution codec is not installed", ErrUnsupported, b.profile.Name)
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.dataEndpoint+b.profile.ExecutePath(url.PathEscape(id)), bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("%w: invalid execution request", ErrInvalid)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("Content-Type", "application/json")
	b.profile.auth(req, b.key)
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

func validateFilePath(value string) error {
	if value == "" || !strings.HasPrefix(value, "/") || path.Clean(value) != value || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: file path must be a normalized absolute path", ErrInvalid)
	}
	return nil
}

func (b *httpBackendClient) ReadFile(ctx context.Context, id, filePath string) (FileRead, error) {
	if b.profile.FileProtocol != "raw-http" {
		return FileRead{}, fmt.Errorf("%w: %s file codec is not installed", ErrUnsupported, b.profile.Name)
	}
	if id == "" {
		return FileRead{}, fmt.Errorf("%w: sandbox handle is required", ErrInvalid)
	}
	if err := validateFilePath(filePath); err != nil {
		return FileRead{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	query := url.Values{"path": {filePath}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.dataEndpoint+b.profile.FilesPath(url.PathEscape(id))+"?"+query, nil)
	if err != nil {
		return FileRead{}, fmt.Errorf("%w: invalid file request", ErrInvalid)
	}
	req.Header.Set("Accept", "application/octet-stream")
	b.profile.auth(req, b.key)
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return FileRead{}, ctx.Err()
		}
		return FileRead{}, fmt.Errorf("%w: runtime file request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	if err := httpResponseError(resp.StatusCode); err != nil {
		return FileRead{}, err
	}
	if resp.ContentLength > maxHTTPFileBytes {
		return FileRead{}, fmt.Errorf("%w: runtime file exceeds %d bytes", ErrLimit, maxHTTPFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPFileBytes+1))
	if err != nil {
		return FileRead{}, fmt.Errorf("%w: runtime file read failed", ErrUnavailable)
	}
	if len(data) > maxHTTPFileBytes {
		return FileRead{}, fmt.Errorf("%w: runtime file exceeds %d bytes", ErrLimit, maxHTTPFileBytes)
	}
	return FileRead{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ContentType: resp.Header.Get("Content-Type")}, nil
}

func (b *httpBackendClient) WriteFile(ctx context.Context, id string, file FileWrite) error {
	if b.profile.FileProtocol != "raw-http" {
		return fmt.Errorf("%w: %s file codec is not installed", ErrUnsupported, b.profile.Name)
	}
	if id == "" || file.Body == nil {
		return fmt.Errorf("%w: sandbox handle and file body are required", ErrInvalid)
	}
	if err := validateFilePath(file.Path); err != nil {
		return err
	}
	if file.Size > maxHTTPFileBytes {
		return fmt.Errorf("%w: file exceeds %d bytes", ErrLimit, maxHTTPFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file.Body, maxHTTPFileBytes+1))
	if err != nil {
		return fmt.Errorf("%w: file body cannot be read", ErrInvalid)
	}
	if len(data) > maxHTTPFileBytes {
		return fmt.Errorf("%w: file exceeds %d bytes", ErrLimit, maxHTTPFileBytes)
	}
	if file.Size >= 0 && file.Size != int64(len(data)) {
		return fmt.Errorf("%w: file size does not match body", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	query := url.Values{"path": {file.Path}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.dataEndpoint+b.profile.FilesPath(url.PathEscape(id))+"?"+query, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: invalid file request", ErrInvalid)
	}
	req.Header.Set("Content-Type", file.ContentType)
	req.Header.Set("Accept", "application/json")
	b.profile.auth(req, b.key)
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: runtime file request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	return httpResponseError(resp.StatusCode)
}

type httpPTYSession struct {
	conn *websocket.Conn
}

func (s *httpPTYSession) Send(ctx context.Context, message PTYMessage) error {
	switch message.Type {
	case "input":
	case "resize":
		if message.Columns == 0 || message.Rows == 0 {
			return fmt.Errorf("%w: PTY resize dimensions must be positive", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported PTY client message", ErrInvalid)
	}
	if err := wsjson.Write(ctx, s.conn, message); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: PTY write failed", ErrUnavailable)
	}
	return nil
}

func (s *httpPTYSession) Receive(ctx context.Context) (PTYMessage, error) {
	var message PTYMessage
	if err := wsjson.Read(ctx, s.conn, &message); err != nil {
		if ctx.Err() != nil {
			return PTYMessage{}, ctx.Err()
		}
		return PTYMessage{}, fmt.Errorf("%w: PTY read failed", ErrUnavailable)
	}
	switch message.Type {
	case "output", "exit", "error":
		return message, nil
	default:
		return PTYMessage{}, fmt.Errorf("%w: unsupported PTY runtime message", ErrUnavailable)
	}
}

func (s *httpPTYSession) Close() error {
	s.conn.CloseNow()
	return nil
}

func (b *httpBackendClient) OpenPTY(ctx context.Context, id string, request PTYRequest) (PTYSession, error) {
	if !b.profile.Capabilities["pty"] {
		return nil, fmt.Errorf("%w: %s PTY is not advertised", ErrUnsupported, b.profile.Name)
	}
	if id == "" || request.ID == "" || request.Command == "" || request.Columns == 0 || request.Rows == 0 {
		return nil, fmt.Errorf("%w: sandbox, session, command and terminal dimensions are required", ErrInvalid)
	}
	if request.Deadline.IsZero() || !request.Deadline.After(time.Now()) {
		return nil, context.DeadlineExceeded
	}
	endpoint, err := url.Parse(b.dataEndpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid PTY endpoint", ErrInvalid)
	}
	switch endpoint.Scheme {
	case "http":
		endpoint.Scheme = "ws"
	case "https":
		endpoint.Scheme = "wss"
	default:
		return nil, fmt.Errorf("%w: invalid PTY endpoint scheme", ErrInvalid)
	}
	ptyPath := b.profile.PTYPath(url.PathEscape(id))
	endpoint.RawPath = strings.TrimRight(endpoint.EscapedPath(), "/") + ptyPath
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + b.profile.PTYPath(id)
	headers := http.Header{}
	if b.key != "" {
		headers.Set(b.profile.AuthHeader, b.key)
	}
	conn, response, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			if statusErr := httpResponseError(response.StatusCode); statusErr != nil {
				return nil, statusErr
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: PTY handshake failed", ErrUnavailable)
	}
	conn.SetReadLimit(maxHTTPEventBytes)
	start := struct {
		Type             string            `json:"type"`
		SessionID        string            `json:"sessionID"`
		Command          string            `json:"command"`
		EnvVars          map[string]string `json:"envVars,omitempty"`
		Columns          uint16            `json:"columns"`
		Rows             uint16            `json:"rows"`
		DeadlineUnixNano int64             `json:"deadlineUnixNano"`
	}{"start", request.ID, request.Command, request.EnvVars, request.Columns, request.Rows, request.Deadline.UnixNano()}
	if err := wsjson.Write(ctx, conn, start); err != nil {
		conn.CloseNow()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: PTY start failed", ErrUnavailable)
	}
	return &httpPTYSession{conn: conn}, nil
}

type limitedResponseBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *limitedResponseBody) Read(buffer []byte) (int, error) {
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			return 0, ErrLimit
		}
		return 0, err
	}
	if int64(len(buffer)) > b.remaining {
		buffer = buffer[:b.remaining]
	}
	n, err := b.body.Read(buffer)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedResponseBody) Close() error { return b.body.Close() }

func (b *httpBackendClient) Proxy(ctx context.Context, id string, request ProxyRequest) (ProxyResponse, error) {
	if !b.profile.Capabilities["ports"] {
		return ProxyResponse{}, fmt.Errorf("%w: %s port proxy is not advertised", ErrUnsupported, b.profile.Name)
	}
	if id == "" || request.Port == 0 || request.Method == "" || request.Path == "" || !strings.HasPrefix(request.Path, "/") || path.Clean(request.Path) != request.Path || strings.ContainsRune(request.Path, 0) {
		return ProxyResponse{}, fmt.Errorf("%w: invalid proxy target", ErrInvalid)
	}
	if _, err := http.NewRequest(request.Method, "http://runtime.invalid", nil); err != nil {
		return ProxyResponse{}, fmt.Errorf("%w: invalid proxy method", ErrInvalid)
	}
	if _, err := url.ParseQuery(request.RawQuery); err != nil {
		return ProxyResponse{}, fmt.Errorf("%w: invalid proxy query", ErrInvalid)
	}
	if request.ContentLength > maxHTTPProxyBytes {
		return ProxyResponse{}, fmt.Errorf("%w: proxy request exceeds %d bytes", ErrLimit, maxHTTPProxyBytes)
	}
	var data []byte
	if request.Body != nil {
		var err error
		data, err = io.ReadAll(io.LimitReader(request.Body, maxHTTPProxyBytes+1))
		if err != nil {
			return ProxyResponse{}, fmt.Errorf("%w: proxy request body cannot be read", ErrInvalid)
		}
		if len(data) > maxHTTPProxyBytes {
			return ProxyResponse{}, fmt.Errorf("%w: proxy request exceeds %d bytes", ErrLimit, maxHTTPProxyBytes)
		}
		if request.ContentLength >= 0 && request.ContentLength != int64(len(data)) {
			return ProxyResponse{}, fmt.Errorf("%w: proxy content length does not match body", ErrInvalid)
		}
	}
	endpoint, err := url.Parse(b.dataEndpoint)
	if err != nil {
		return ProxyResponse{}, fmt.Errorf("%w: invalid proxy endpoint", ErrInvalid)
	}
	portPath := b.profile.PortPath(id, request.Port)
	rawPortPath := b.profile.PortPath(url.PathEscape(id), request.Port)
	prefixPath := strings.TrimRight(endpoint.Path, "/") + portPath
	prefixRawPath := strings.TrimRight(endpoint.EscapedPath(), "/") + rawPortPath
	requestURL := &url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host, Path: prefixPath + request.Path, RawPath: prefixRawPath + (&url.URL{Path: request.Path}).EscapedPath(), RawQuery: request.RawQuery}
	req, err := http.NewRequestWithContext(ctx, request.Method, requestURL.String(), bytes.NewReader(data))
	if err != nil {
		return ProxyResponse{}, fmt.Errorf("%w: invalid proxy request", ErrInvalid)
	}
	for name, values := range request.Header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	b.profile.auth(req, b.key)
	response, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ProxyResponse{}, ctx.Err()
		}
		return ProxyResponse{}, fmt.Errorf("%w: runtime port proxy failed", ErrUnavailable)
	}
	if response.ContentLength > maxHTTPProxyBytes {
		response.Body.Close()
		return ProxyResponse{}, fmt.Errorf("%w: proxy response exceeds %d bytes", ErrLimit, maxHTTPProxyBytes)
	}
	headers := map[string][]string{}
	for name, values := range response.Header {
		headers[name] = append([]string(nil), values...)
	}
	return ProxyResponse{StatusCode: response.StatusCode, Header: headers, Body: &limitedResponseBody{body: response.Body, remaining: maxHTTPProxyBytes}}, nil
}
