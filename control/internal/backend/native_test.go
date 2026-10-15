package backend

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/obscura/agentruntime/control/gen/agentruntime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeNative struct {
	pb.UnimplementedDataPlaneServer
	capability      bool
	protocolVersion string
	createID, hash  string
	state           pb.SandboxState
	deleted         bool
	truncated       bool
}

func (s *fakeNative) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	caps := []string{}
	if s.capability {
		caps = append(caps, "idempotent-create-v1")
	}
	version := s.protocolVersion
	if version == "" {
		version = "agentruntime.v1"
	}
	return &pb.HealthResponse{ProtocolVersion: version, Capabilities: caps}, nil
}
func (s *fakeNative) CreateSandbox(_ context.Context, r *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	s.createID = r.SandboxId
	s.hash = r.SpecHash
	return &pb.CreateSandboxResponse{Sandbox: &pb.Sandbox{SandboxId: r.SandboxId, State: pb.SandboxState_SANDBOX_STATE_READY}}, nil
}
func (s *fakeNative) GetSandbox(_ context.Context, r *pb.GetSandboxRequest) (*pb.GetSandboxResponse, error) {
	if s.deleted {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return &pb.GetSandboxResponse{Sandbox: &pb.Sandbox{SandboxId: r.SandboxId, State: s.state}}, nil
}
func (s *fakeNative) DeleteSandbox(context.Context, *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	if s.deleted {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return &pb.DeleteSandboxResponse{}, nil
}
func (s *fakeNative) Execute(_ *pb.ExecuteRequest, stream grpc.ServerStreamingServer[pb.AgentEvent]) error {
	if err := stream.Send(&pb.AgentEvent{SandboxId: "a", ExecutionId: "exec", Sequence: 1, Type: pb.EventType_EVENT_TYPE_STDOUT, Payload: []byte("hello")}); err != nil {
		return err
	}
	if s.truncated {
		return nil
	}
	return stream.Send(&pb.AgentEvent{SandboxId: "a", ExecutionId: "exec", Sequence: 2, Type: pb.EventType_EVENT_TYPE_RESULT, Payload: []byte(`{"exitCode":0}`)})
}

type executableBackend interface {
	Backend
	Executor
}

func nativeBackend(t *testing.T, s *fakeNative) executableBackend {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "obscura-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	socket := filepath.Join(directory, "r.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterDataPlaneServer(server, s)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	b, err := NewRegistry(nil).Open(context.Background(), "test", Config{Type: "native", Endpoint: "unix://" + socket})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	executable, ok := b.(executableBackend)
	if !ok {
		t.Fatal("native backend does not implement execution")
	}
	return executable
}
func nativeRequest() Request {
	return Request{ID: "11111111-1111-4111-8111-111111111111", SpecHash: "hash", Template: "test", Resources: Resources{VCPUs: 1, MemoryMiB: 128, DiskMiB: 128}, TimeoutSeconds: 60}
}
func TestNativeEnsureStableIDAndHealthCapability(t *testing.T) {
	s := &fakeNative{capability: true}
	b := nativeBackend(t, s)
	r := nativeRequest()
	for range 2 {
		got, err := b.Ensure(context.Background(), r)
		if err != nil || got.Handle != r.ID {
			t.Fatalf("got %v %v", got, err)
		}
	}
	if s.createID != r.ID || s.hash != r.SpecHash {
		t.Fatalf("unstable create %s %s", s.createID, s.hash)
	}
	s.capability = false
	if _, err := b.Ensure(context.Background(), r); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}
func TestNativeEnsureRejectsIncompatibleProtocol(t *testing.T) {
	s := &fakeNative{capability: true, protocolVersion: "agentruntime.v2"}
	b := nativeBackend(t, s)
	if _, err := b.Ensure(context.Background(), nativeRequest()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("incompatible protocol accepted: %v", err)
	}
	if s.createID != "" {
		t.Fatal("create sent to an incompatible runtime")
	}
}
func TestNativeDeleteConfirmationAndStreaming(t *testing.T) {
	s := &fakeNative{capability: true, state: pb.SandboxState_SANDBOX_STATE_READY}
	b := nativeBackend(t, s)
	if done, err := b.Delete(context.Background(), "a"); done || err != nil {
		t.Fatalf("premature delete %v %v", done, err)
	}
	s.state = pb.SandboxState_SANDBOX_STATE_DELETED
	if done, err := b.Delete(context.Background(), "a"); !done || err != nil {
		t.Fatalf("unconfirmed delete %v %v", done, err)
	}
	s.deleted = true
	if done, err := b.Delete(context.Background(), "a"); !done || err != nil {
		t.Fatalf("not idempotent %v %v", done, err)
	}
	var events []Event
	if err := b.Exec(context.Background(), "a", Execution{ID: "exec", Command: "echo hello"}, func(e Event) error { events = append(events, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "stdout" || events[0].Payload != "hello" || events[1].Type != "result" {
		t.Fatalf("events %v", events)
	}
}
func TestNativeExecutionTruncatedStreamIsNotSuccess(t *testing.T) {
	b := nativeBackend(t, &fakeNative{truncated: true})
	var events []Event
	err := b.Exec(context.Background(), "a", Execution{ID: "exec", Command: "echo hello"}, func(e Event) error {
		events = append(events, e)
		return nil
	})
	if !errors.Is(err, ErrUnavailable) || len(events) != 1 || events[0].Type != "stdout" {
		t.Fatalf("truncated stream: events=%v error=%v", events, err)
	}
}
func TestNativeRejectAllowHosts(t *testing.T) {
	b := nativeBackend(t, &fakeNative{})
	r := nativeRequest()
	r.Network.AllowHosts = []string{"example.com"}
	if err := b.Validate(r); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestNativeErrorCategories(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		want error
	}{
		{codes.AlreadyExists, ErrConflict},
		{codes.FailedPrecondition, ErrPrecondition},
		{codes.InvalidArgument, ErrInvalid},
		{codes.Unauthenticated, ErrAuth},
		{codes.PermissionDenied, ErrAuth},
		{codes.ResourceExhausted, ErrCapacity},
		{codes.Unavailable, ErrUnavailable},
		{codes.DeadlineExceeded, ErrUnavailable},
	} {
		if err := nativeError(status.Error(tc.code, "runtime detail")); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.code, err, tc.want)
		}
	}
}
