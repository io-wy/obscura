// Package devruntime simulates the native protocol without VMs or host commands.
// It is development/test tooling, never a sandbox isolation boundary.
package devruntime

import (
	"context"
	"sort"
	"sync"

	pb "github.com/obscura/agentruntime/control/gen/agentruntime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Faults struct {
	Unavailable bool `json:"unavailable"`
	HoldDeletes bool `json:"holdDeletes"`
}

type Snapshot struct {
	Faults     Faults   `json:"faults"`
	Created    int      `json:"created"`
	Active     int      `json:"active"`
	Executions int      `json:"executions"`
	IDs        []string `json:"ids"`
}

type Server struct {
	pb.UnimplementedDataPlaneServer
	mu         sync.Mutex
	faults     Faults
	instances  map[string]*pb.Sandbox
	hashes     map[string]string
	executions int
}

func New() *Server {
	return &Server{instances: make(map[string]*pb.Sandbox), hashes: make(map[string]string)}
}

func (s *Server) SetFaults(f Faults) { s.mu.Lock(); defer s.mu.Unlock(); s.faults = f }
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := Snapshot{Faults: s.faults, Created: len(s.hashes), Executions: s.executions, IDs: []string{}}
	for id := range s.instances {
		v.IDs = append(v.IDs, id)
	}
	sort.Strings(v.IDs)
	v.Active = len(v.IDs)
	return v
}
func (s *Server) available() error {
	if s.faults.Unavailable {
		return status.Error(codes.Unavailable, "simulated runtime outage")
	}
	return nil
}
func (s *Server) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	return &pb.HealthResponse{ProtocolVersion: "agentruntime.v1", Version: "dev-simulator", Capabilities: []string{"idempotent-create-v1"}}, nil
}
func (s *Server) CreateSandbox(_ context.Context, r *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	if r.SandboxId == "" || r.SpecHash == "" || r.TemplateRef == "" {
		return nil, status.Error(codes.InvalidArgument, "identity, hash and template required")
	}
	if hash, ok := s.hashes[r.SandboxId]; ok {
		if hash != r.SpecHash {
			return nil, status.Error(codes.AlreadyExists, "specification conflict")
		}
		if s.instances[r.SandboxId] == nil {
			return nil, status.Error(codes.FailedPrecondition, "identity already deleted")
		}
	} else {
		s.hashes[r.SandboxId] = r.SpecHash
		s.instances[r.SandboxId] = &pb.Sandbox{SandboxId: r.SandboxId, TemplateRef: r.TemplateRef, Resources: r.Resources, State: pb.SandboxState_SANDBOX_STATE_READY}
	}
	return &pb.CreateSandboxResponse{Sandbox: proto.Clone(s.instances[r.SandboxId]).(*pb.Sandbox)}, nil
}
func (s *Server) GetSandbox(_ context.Context, r *pb.GetSandboxRequest) (*pb.GetSandboxResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	v := s.instances[r.SandboxId]
	if v == nil {
		return nil, status.Error(codes.NotFound, "sandbox absent")
	}
	return &pb.GetSandboxResponse{Sandbox: proto.Clone(v).(*pb.Sandbox)}, nil
}
func (s *Server) DeleteSandbox(_ context.Context, r *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	if !s.faults.HoldDeletes {
		delete(s.instances, r.SandboxId)
	}
	return &pb.DeleteSandboxResponse{}, nil
}
func (s *Server) Execute(r *pb.ExecuteRequest, stream grpc.ServerStreamingServer[pb.AgentEvent]) error {
	s.mu.Lock()
	if err := s.available(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.instances[r.SandboxId] == nil {
		s.mu.Unlock()
		return status.Error(codes.NotFound, "sandbox absent")
	}
	s.executions++
	s.mu.Unlock()
	// Deliberately fixed output: command/input/env are never passed to a host process.
	events := []*pb.AgentEvent{
		{Type: pb.EventType_EVENT_TYPE_STARTED},
		{Type: pb.EventType_EVENT_TYPE_STDOUT, Payload: []byte("simulated: " + r.Command + "\n")},
		{Type: pb.EventType_EVENT_TYPE_RESULT, Payload: []byte(`{"exitCode":0}`)},
	}
	if r.Command == "demo:fail" {
		events[2] = &pb.AgentEvent{Type: pb.EventType_EVENT_TYPE_ERROR, Error: "simulated execution failure"}
	}
	for i, event := range events {
		event.SandboxId, event.ExecutionId, event.Sequence = r.SandboxId, r.ExecutionId, uint64(i+1)
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	return nil
}
