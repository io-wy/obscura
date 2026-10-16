package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	envd "github.com/obscura/agentruntime/control/gen/agentenv/envd"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	agentENVControlPlanePort = "49983"
	agentENVProcessService   = "/process.Process/"
	agentENVConnectDataFlag  = byte(0)
	agentENVConnectEndFlag   = byte(2)
)

type agentENVBackend struct{ *httpBackendClient }

func openAgentENV(ctx context.Context, namespace string, config Config, profile httpRuntimeProfile, reader client.Reader) (Backend, error) {
	base, err := openHTTPRuntime(ctx, namespace, config, profile, reader)
	if err != nil {
		return nil, err
	}
	httpBackend := base.(*httpBackendClient)
	dataURL, err := url.Parse(httpBackend.dataEndpoint)
	if err != nil || (dataURL.Path != "" && dataURL.Path != "/") {
		httpBackend.Close()
		return nil, fmt.Errorf("%w: AgentENV data endpoint must not contain a path", ErrInvalid)
	}
	return &agentENVBackend{httpBackendClient: httpBackend}, nil
}

func (b *agentENVBackend) envdSandbox(ctx context.Context, id string) (remoteSandbox, error) {
	if id == "" {
		return remoteSandbox{}, fmt.Errorf("%w: sandbox handle is required", ErrInvalid)
	}
	var sandbox remoteSandbox
	if _, err := b.request(ctx, http.MethodGet, b.profile.SandboxPath(url.PathEscape(id)), nil, &sandbox); err != nil {
		if ctx.Err() != nil {
			return remoteSandbox{}, ctx.Err()
		}
		return remoteSandbox{}, err
	}
	if sandbox.ID != id {
		return remoteSandbox{}, ErrConflict
	}
	if sandbox.EnvdAccessToken == "" {
		return remoteSandbox{}, fmt.Errorf("%w: AgentENV did not return an envd access token", ErrAuth)
	}
	return sandbox, nil
}

func (b *agentENVBackend) Exec(ctx context.Context, id string, execution Execution, emit func(Event) error) error {
	if execution.ID == "" || execution.Command == "" || emit == nil {
		return fmt.Errorf("%w: execution, command and event receiver are required", ErrInvalid)
	}
	if !execution.Deadline.IsZero() {
		if !execution.Deadline.After(time.Now()) {
			return context.DeadlineExceeded
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, execution.Deadline)
		defer cancel()
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	ctx, cancelStream := context.WithCancelCause(ctx)
	var inputDone chan struct{}
	defer func() {
		cancelStream(nil)
		if inputDone != nil {
			<-inputDone
		}
	}()
	sandbox, err := b.envdSandbox(ctx, id)
	if err != nil {
		return err
	}
	stdinEnabled := len(execution.Input) > 0
	start := &envd.StartRequest{
		Process: &envd.ProcessConfig{Cmd: "/bin/sh", Args: []string{"-lc", execution.Command}, Envs: execution.EnvVars},
		Tag:     &execution.ID,
		Stdin:   &stdinEnabled,
	}
	response, err := b.agentENVConnectStream(ctx, sandbox, b.profile.ExecutePath(url.PathEscape(id)), start)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var sequence uint64
	var outputBytes int
	for {
		var message envd.StartResponse
		end, err := readAgentENVConnectEnvelope(response.Body, &message)
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return err
		}
		if end {
			return fmt.Errorf("%w: AgentENV execution stream ended before a terminal event", ErrUnavailable)
		}
		events, terminal, err := agentENVEvents(execution.ID, &sequence, message.GetEvent())
		if err != nil {
			return err
		}
		if terminal {
			if inputDone != nil {
				<-inputDone
			}
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
		}
		for _, event := range events {
			outputBytes += len(event.Payload) + len(event.Error)
			if outputBytes > maxHTTPOutputBytes {
				return fmt.Errorf("%w: AgentENV execution output limit exceeded", ErrLimit)
			}
			if err := emit(event); err != nil {
				return err
			}
			if event.Type == "started" && stdinEnabled && inputDone == nil {
				inputDone = make(chan struct{})
				// Drain output while writing stdin: the child may write enough
				// output to fill its pipe before it reads the remaining input.
				go func() {
					defer close(inputDone)
					selector := &envd.ProcessSelector{Selector: &envd.ProcessSelector_Tag{Tag: execution.ID}}
					err := b.agentENVUnary(ctx, sandbox, agentENVProcessService+"SendInput", &envd.SendInputRequest{
						Process: selector,
						Input:   &envd.ProcessInput{Input: &envd.ProcessInput_Stdin{Stdin: execution.Input}},
					}, &envd.SendInputResponse{})
					if err == nil {
						err = b.agentENVUnary(ctx, sandbox, agentENVProcessService+"CloseStdin", &envd.CloseStdinRequest{Process: selector}, &envd.CloseStdinResponse{})
					}
					if err != nil {
						cancelStream(err)
					}
				}()
			}
		}
		if terminal {
			return nil
		}
	}
}

func (b *agentENVBackend) agentENVConnectStream(ctx context.Context, sandbox remoteSandbox, requestPath string, input proto.Message) (*http.Response, error) {
	payload, err := proto.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("%w: encode AgentENV Connect request", ErrInvalid)
	}
	body := make([]byte, 5+len(payload))
	body[0] = agentENVConnectDataFlag
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)
	response, err := b.agentENVRequest(ctx, sandbox, requestPath, "application/connect+proto", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if err := agentENVHTTPError(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/connect+proto" {
		response.Body.Close()
		return nil, fmt.Errorf("%w: AgentENV returned a non-Connect execution stream", ErrUnavailable)
	}
	return response, nil
}

func (b *agentENVBackend) agentENVUnary(ctx context.Context, sandbox remoteSandbox, requestPath string, input, output proto.Message) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	payload, err := proto.Marshal(input)
	if err != nil {
		return fmt.Errorf("%w: encode AgentENV unary request", ErrInvalid)
	}
	response, err := b.agentENVRequest(ctx, sandbox, requestPath, "application/proto", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := agentENVHTTPError(response); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPEventBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read AgentENV unary response", ErrUnavailable)
	}
	if len(data) > maxHTTPEventBytes {
		return fmt.Errorf("%w: AgentENV unary response is too large", ErrLimit)
	}
	if err := proto.Unmarshal(data, output); err != nil {
		return fmt.Errorf("%w: decode AgentENV unary response", ErrUnavailable)
	}
	return nil
}

func (b *agentENVBackend) agentENVRequest(ctx context.Context, sandbox remoteSandbox, requestPath, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.dataEndpoint+requestPath, body)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid AgentENV data request", ErrInvalid)
	}
	b.agentENVDataHeaders(req, sandbox)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Content-Type", contentType)
	response, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: AgentENV data request failed", ErrUnavailable)
	}
	return response, nil
}

func readAgentENVConnectEnvelope(reader io.Reader, output proto.Message) (bool, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return false, fmt.Errorf("%w: AgentENV Connect stream was truncated", ErrUnavailable)
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > maxHTTPEventBytes {
		return false, fmt.Errorf("%w: AgentENV Connect frame is too large", ErrLimit)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return false, fmt.Errorf("%w: AgentENV Connect frame was truncated", ErrUnavailable)
	}
	switch header[0] {
	case agentENVConnectDataFlag:
		if err := proto.Unmarshal(payload, output); err != nil {
			return false, fmt.Errorf("%w: decode AgentENV Connect message", ErrUnavailable)
		}
		return false, nil
	case agentENVConnectEndFlag:
		if len(payload) == 0 || bytes.Equal(payload, []byte("{}")) {
			return true, nil
		}
		var trailer struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &trailer); err != nil || trailer.Error == nil {
			return false, fmt.Errorf("%w: invalid AgentENV Connect end frame", ErrUnavailable)
		}
		return false, agentENVConnectCodeError(trailer.Error.Code)
	default:
		return false, fmt.Errorf("%w: unknown AgentENV Connect frame flag", ErrUnavailable)
	}
}

func agentENVHTTPError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &body) == nil && body.Code != "" {
		return agentENVConnectCodeError(body.Code)
	}
	return httpResponseError(response.StatusCode)
}

func agentENVConnectCodeError(code string) error {
	var category error
	switch code {
	case "not_found":
		category = ErrNotFound
	case "invalid_argument", "out_of_range":
		category = ErrInvalid
	case "unauthenticated", "permission_denied":
		category = ErrAuth
	case "resource_exhausted":
		category = ErrCapacity
	case "already_exists", "aborted", "failed_precondition":
		category = ErrConflict
	case "unimplemented":
		category = ErrUnsupported
	default:
		category = ErrUnavailable
	}
	return fmt.Errorf("%w: AgentENV envd request failed", category)
}

func agentENVEvents(executionID string, sequence *uint64, event *envd.ProcessEvent) ([]Event, bool, error) {
	if event == nil {
		return nil, false, fmt.Errorf("%w: AgentENV returned an empty process event", ErrUnavailable)
	}
	next := func(eventType, payload, message string) Event {
		(*sequence)++
		return Event{ExecutionID: executionID, Sequence: *sequence, Type: eventType, Payload: payload, Error: message}
	}
	switch value := event.Event.(type) {
	case *envd.ProcessEvent_Start:
		return []Event{next("started", strconv.FormatUint(uint64(value.Start.GetPid()), 10), "")}, false, nil
	case *envd.ProcessEvent_Data:
		switch output := value.Data.Output.(type) {
		case *envd.ProcessEvent_DataEvent_Stdout:
			return []Event{next("stdout", string(output.Stdout), "")}, false, nil
		case *envd.ProcessEvent_DataEvent_Stderr:
			return []Event{next("stderr", string(output.Stderr), "")}, false, nil
		case *envd.ProcessEvent_DataEvent_Pty:
			return []Event{next("stdout", string(output.Pty), "")}, false, nil
		default:
			return nil, false, fmt.Errorf("%w: AgentENV returned empty process data", ErrUnavailable)
		}
	case *envd.ProcessEvent_End:
		payload, _ := json.Marshal(map[string]any{"exitCode": value.End.GetExitCode(), "exited": value.End.GetExited(), "status": value.End.GetStatus()})
		if message := value.End.GetError(); message != "" {
			return []Event{next("error", string(payload), message)}, true, nil
		}
		return []Event{next("result", string(payload), "")}, true, nil
	case *envd.ProcessEvent_Keepalive:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("%w: AgentENV returned an unknown process event", ErrUnavailable)
	}
}

func (b *agentENVBackend) ReadFile(ctx context.Context, id, filePath string) (FileRead, error) {
	if err := validateFilePath(filePath); err != nil {
		return FileRead{}, err
	}
	sandbox, err := b.envdSandbox(ctx, id)
	if err != nil {
		return FileRead{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	query := url.Values{"path": {filePath}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.dataEndpoint+b.profile.FilesPath(url.PathEscape(id))+"?"+query, nil)
	if err != nil {
		return FileRead{}, fmt.Errorf("%w: invalid AgentENV file request", ErrInvalid)
	}
	b.agentENVDataHeaders(req, sandbox)
	req.Header.Set("Accept", "application/octet-stream")
	response, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return FileRead{}, ctx.Err()
		}
		return FileRead{}, fmt.Errorf("%w: AgentENV file request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	if err := httpResponseError(response.StatusCode); err != nil {
		return FileRead{}, err
	}
	if response.ContentLength > maxHTTPFileBytes {
		return FileRead{}, fmt.Errorf("%w: AgentENV file exceeds %d bytes", ErrLimit, maxHTTPFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPFileBytes+1))
	if err != nil {
		return FileRead{}, fmt.Errorf("%w: AgentENV file read failed", ErrUnavailable)
	}
	if len(data) > maxHTTPFileBytes {
		return FileRead{}, fmt.Errorf("%w: AgentENV file exceeds %d bytes", ErrLimit, maxHTTPFileBytes)
	}
	return FileRead{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), ContentType: response.Header.Get("Content-Type")}, nil
}

func (b *agentENVBackend) WriteFile(ctx context.Context, id string, file FileWrite) error {
	if file.Body == nil {
		return fmt.Errorf("%w: file body is required", ErrInvalid)
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
	sandbox, err := b.envdSandbox(ctx, id)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", path.Base(file.Path))
	if err != nil {
		return fmt.Errorf("%w: create AgentENV multipart upload", ErrInvalid)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("%w: build AgentENV multipart upload", ErrInvalid)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("%w: finish AgentENV multipart upload", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	query := url.Values{"path": {file.Path}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.dataEndpoint+b.profile.FilesPath(url.PathEscape(id))+"?"+query, &body)
	if err != nil {
		return fmt.Errorf("%w: invalid AgentENV file request", ErrInvalid)
	}
	b.agentENVDataHeaders(req, sandbox)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: AgentENV file request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	return httpResponseError(response.StatusCode)
}

func (b *agentENVBackend) agentENVDataHeaders(req *http.Request, sandbox remoteSandbox) {
	req.Header.Set("X-Agentenv-Sandbox-Id", sandbox.ID)
	req.Header.Set("X-Agentenv-Target-Port", agentENVControlPlanePort)
	req.Header.Set("X-Access-Token", sandbox.EnvdAccessToken)
}
