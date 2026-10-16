package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	envd "github.com/obscura/agentruntime/control/gen/agentenv/envd"
	"google.golang.org/protobuf/proto"
)

func agentENVControlServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/sandboxes/sandbox" {
			t.Errorf("unexpected control request: %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(remoteSandbox{ID: "sandbox", State: "running", EnvdAccessToken: token})
	}))
}

func checkAgentENVDataHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("X-Agentenv-Sandbox-Id") != "sandbox" || r.Header.Get("X-Agentenv-Target-Port") != agentENVControlPlanePort || r.Header.Get("X-Access-Token") != "envd-secret" {
		t.Errorf("unexpected AgentENV data headers: %v", r.Header)
	}
}

func decodeConnectRequest(t *testing.T, r *http.Request, message proto.Message) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 5 || body[0] != agentENVConnectDataFlag || int(binary.BigEndian.Uint32(body[1:5])) != len(body)-5 {
		t.Fatalf("invalid Connect request envelope: %x", body)
	}
	if err := proto.Unmarshal(body[5:], message); err != nil {
		t.Fatalf("decode Connect request: %v", err)
	}
}

func decodeUnaryRequest(t *testing.T, r *http.Request, message proto.Message) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(body, message); err != nil {
		t.Fatalf("decode unary request: %v", err)
	}
}

func connectFrame(t *testing.T, flag byte, message proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = flag
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func TestAgentENVExecUsesConnectRPCAndTranslatesStream(t *testing.T) {
	control := agentENVControlServer(t, "envd-secret")
	defer control.Close()
	var input []byte
	stdinClosed := false
	var started atomic.Bool
	stdinDone := make(chan struct{})
	outputReceived := make(chan struct{})
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAgentENVDataHeaders(t, r)
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Errorf("missing Connect protocol header: %v", r.Header)
		}
		switch r.URL.EscapedPath() {
		case "/process.Process/Start":
			if got := r.Header.Get("Content-Type"); got != "application/connect+proto" {
				t.Errorf("Start content type = %q", got)
			}
			var request envd.StartRequest
			decodeConnectRequest(t, r, &request)
			if request.GetProcess().GetCmd() != "/bin/sh" || strings.Join(request.GetProcess().GetArgs(), " ") != "-lc printf hello" || request.GetProcess().GetEnvs()["A"] != "B" || request.GetTag() != "exec-1" || !request.GetStdin() {
				t.Errorf("unexpected start request: %+v", &request)
			}
			w.Header().Set("Content-Type", "application/connect+proto")
			events := []*envd.ProcessEvent{
				{Event: &envd.ProcessEvent_Start{Start: &envd.ProcessEvent_StartEvent{Pid: 42}}},
				{Event: &envd.ProcessEvent_Data{Data: &envd.ProcessEvent_DataEvent{Output: &envd.ProcessEvent_DataEvent_Stdout{Stdout: []byte("hello")}}}},
				{Event: &envd.ProcessEvent_Data{Data: &envd.ProcessEvent_DataEvent{Output: &envd.ProcessEvent_DataEvent_Stderr{Stderr: []byte("warn")}}}},
				{Event: &envd.ProcessEvent_End{End: &envd.ProcessEvent_EndEvent{ExitCode: 0, Exited: true, Status: "exited"}}},
			}
			for _, event := range events {
				frame := connectFrame(t, agentENVConnectDataFlag, &envd.StartResponse{Event: event})
				_, _ = w.Write(frame[:2])
				_, _ = w.Write(frame[2:])
				w.(http.Flusher).Flush()
				if data, ok := event.Event.(*envd.ProcessEvent_Data); ok && string(data.Data.GetStdout()) == "hello" {
					select {
					case <-stdinDone:
					case <-r.Context().Done():
						return
					}
				}
			}
			_, _ = w.Write([]byte{agentENVConnectEndFlag, 0, 0, 0, 2, '{', '}'})
		case "/process.Process/SendInput":
			if !started.Load() {
				t.Error("stdin sent before the Start event was received")
				w.WriteHeader(http.StatusConflict)
				return
			}
			if got := r.Header.Get("Content-Type"); got != "application/proto" {
				t.Errorf("SendInput content type = %q", got)
			}
			var request envd.SendInputRequest
			decodeUnaryRequest(t, r, &request)
			if request.GetProcess().GetTag() != "exec-1" {
				t.Errorf("unexpected process selector: %+v", request.GetProcess())
			}
			input = append([]byte(nil), request.GetInput().GetStdin()...)
			select {
			case <-outputReceived:
			case <-r.Context().Done():
				return
			}
			body, _ := proto.Marshal(&envd.SendInputResponse{})
			_, _ = w.Write(body)
		case "/process.Process/CloseStdin":
			var request envd.CloseStdinRequest
			decodeUnaryRequest(t, r, &request)
			if request.GetProcess().GetTag() != "exec-1" {
				t.Errorf("unexpected close selector: %+v", request.GetProcess())
			}
			stdinClosed = true
			close(stdinDone)
			body, _ := proto.Marshal(&envd.CloseStdinResponse{})
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected data request: %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer data.Close()

	opened, err := openAgentENV(context.Background(), "test", Config{Type: "agentenv", ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var events []Event
	err = opened.(*agentENVBackend).Exec(context.Background(), "sandbox", Execution{ID: "exec-1", Command: "printf hello", Input: []byte("input"), EnvVars: map[string]string{"A": "B"}, Deadline: time.Now().Add(time.Minute)}, func(event Event) error {
		if event.Type == "started" {
			started.Store(true)
		}
		if event.Type == "stdout" {
			close(outputReceived)
		}
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Type != "started" || events[1].Type != "stdout" || events[1].Payload != "hello" || events[2].Type != "stderr" || events[3].Type != "result" {
		t.Fatalf("unexpected events: %+v", events)
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) || event.ExecutionID != "exec-1" {
			t.Fatalf("invalid event sequence: %+v", events)
		}
	}
	if string(input) != "input" || !stdinClosed {
		t.Fatalf("stdin=%q closed=%v", input, stdinClosed)
	}
}

func TestAgentENVInputFailureInterruptsOutputStream(t *testing.T) {
	control := agentENVControlServer(t, "envd-secret")
	defer control.Close()
	streamClosed := make(chan struct{})
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/process.Process/SendInput" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		_, _ = w.Write(connectFrame(t, agentENVConnectDataFlag, &envd.StartResponse{Event: &envd.ProcessEvent{Event: &envd.ProcessEvent_Start{Start: &envd.ProcessEvent_StartEvent{Pid: 42}}}}))
		_, _ = w.Write(connectFrame(t, agentENVConnectDataFlag, &envd.StartResponse{Event: &envd.ProcessEvent{Event: &envd.ProcessEvent_End{End: &envd.ProcessEvent_EndEvent{Exited: true}}}}))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(streamClosed)
	}))
	defer data.Close()
	opened, err := openAgentENV(context.Background(), "test", Config{ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	err = opened.(*agentENVBackend).Exec(context.Background(), "sandbox", Execution{ID: "exec", Command: "cat", Input: []byte("hello"), Deadline: time.Now().Add(time.Second)}, func(event Event) error {
		if event.Type == "result" {
			t.Error("result emitted before stdin failure was reported")
		}
		return nil
	})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("error = %v, want input ErrAuth", err)
	}
	select {
	case <-streamClosed:
	case <-time.After(time.Second):
		t.Fatal("input failure left output stream open")
	}
}

func TestAgentENVExecRejectsMissingTokenAndTruncatedStream(t *testing.T) {
	t.Run("missing envd token", func(t *testing.T) {
		control := agentENVControlServer(t, "")
		defer control.Close()
		data := httptest.NewServer(http.NotFoundHandler())
		defer data.Close()
		opened, err := openAgentENV(context.Background(), "test", Config{ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		err = opened.(*agentENVBackend).Exec(context.Background(), "sandbox", Execution{ID: "exec", Command: "true"}, func(Event) error { return nil })
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("error = %v, want ErrAuth", err)
		}
	})

	t.Run("truncated Connect envelope", func(t *testing.T) {
		control := agentENVControlServer(t, "envd-secret")
		defer control.Close()
		data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/connect+proto")
			_, _ = w.Write([]byte{agentENVConnectDataFlag, 0, 0, 0, 10, 1, 2})
		}))
		defer data.Close()
		opened, err := openAgentENV(context.Background(), "test", Config{ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		err = opened.(*agentENVBackend).Exec(context.Background(), "sandbox", Execution{ID: "exec", Command: "true"}, func(Event) error { return nil })
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable", err)
		}
	})
}

func TestAgentENVExecMapsErrorsWithoutLeakingUpstreamDetails(t *testing.T) {
	control := agentENVControlServer(t, "envd-secret")
	defer control.Close()
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"unauthenticated","message":"envd-secret upstream detail"}`)
	}))
	defer data.Close()
	opened, err := openAgentENV(context.Background(), "test", Config{ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	err = opened.(*agentENVBackend).Exec(context.Background(), "sandbox", Execution{ID: "exec", Command: "true"}, func(Event) error { return nil })
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "envd-secret") || strings.Contains(err.Error(), "upstream detail") {
		t.Fatalf("error = %v, want redacted ErrAuth", err)
	}
}

func TestAgentENVExecHonorsCancellation(t *testing.T) {
	control := agentENVControlServer(t, "envd-secret")
	defer control.Close()
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer data.Close()
	opened, err := openAgentENV(context.Background(), "test", Config{ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = opened.(*agentENVBackend).Exec(ctx, "sandbox", Execution{ID: "exec", Command: "sleep 10"}, func(Event) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
}

func TestAgentENVConnectFrameAndTerminalErrorLimits(t *testing.T) {
	header := []byte{agentENVConnectDataFlag, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[1:], uint32(maxHTTPEventBytes+1))
	if _, err := readAgentENVConnectEnvelope(bytes.NewReader(header), &envd.StartResponse{}); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversized frame error = %v, want ErrLimit", err)
	}
	sequence := uint64(0)
	events, terminal, err := agentENVEvents("exec", &sequence, &envd.ProcessEvent{Event: &envd.ProcessEvent_End{End: &envd.ProcessEvent_EndEvent{ExitCode: 1, Exited: true, Status: "exited", Error: proto.String("command failed")}}})
	if err != nil || !terminal || len(events) != 1 || events[0].Type != "error" || events[0].Error != "command failed" {
		t.Fatalf("events=%+v terminal=%v err=%v", events, terminal, err)
	}
}

func TestAgentENVFilesUseEnvdHTTPContract(t *testing.T) {
	control := agentENVControlServer(t, "envd-secret")
	defer control.Close()
	var uploaded []byte
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/files" || r.URL.Query().Get("path") != "/work/a b.bin" {
			t.Errorf("unexpected file request: %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		checkAgentENVDataHeaders(t, r)
		if r.Method == http.MethodPost {
			mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
				t.Fatalf("invalid upload content type %q: %v", r.Header.Get("Content-Type"), err)
			}
			reader, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("read multipart request: %v", err)
			}
			part, err := reader.NextPart()
			if err != nil || part.FormName() != "file" {
				t.Fatalf("invalid multipart file part: %v", err)
			}
			uploaded, _ = io.ReadAll(part)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"path":"/work/a b.bin","name":"a b.bin","type":"file"}]`)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 1, 2, 255})
	}))
	defer data.Close()

	opened, err := openAgentENV(context.Background(), "test", Config{Type: "agentenv", ControlEndpoint: control.URL, DataEndpoint: data.URL}, agentENVHTTPProfile("v0.2.3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	backend := opened.(*agentENVBackend)
	want := []byte{0, 1, 2, 255}
	if err := backend.WriteFile(context.Background(), "sandbox", FileWrite{Path: "/work/a b.bin", Body: strings.NewReader(string(want)), Size: int64(len(want)), ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(backend.WriteFile(context.Background(), "sandbox", FileWrite{Path: "/work/a b.bin", Body: strings.NewReader("short"), Size: 10}), ErrInvalid) {
		t.Fatal("file size mismatch must return ErrInvalid")
	}
	if string(uploaded) != string(want) {
		t.Fatalf("uploaded %v", uploaded)
	}
	file, err := backend.ReadFile(context.Background(), "sandbox", "/work/a b.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Body.Close()
	body, err := io.ReadAll(file.Body)
	if err != nil || string(body) != string(want) || file.ContentType != "application/octet-stream" {
		t.Fatalf("file=%v content-type=%q err=%v", body, file.ContentType, err)
	}
}
