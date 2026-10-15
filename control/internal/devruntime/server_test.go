package devruntime

import (
	"context"
	"testing"

	pb "github.com/obscura/agentruntime/control/gen/agentruntime/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestStableIdentityAndDeletionFaults(t *testing.T) {
	s := New()
	ctx := context.Background()
	r := &pb.CreateSandboxRequest{SandboxId: "one", SpecHash: "hash", TemplateRef: "dev"}
	for range 2 {
		if _, err := s.CreateSandbox(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if s.Snapshot().Created != 1 {
		t.Fatal("retry created duplicate instance")
	}
	conflict := proto.Clone(r).(*pb.CreateSandboxRequest)
	conflict.SpecHash = "different"
	if _, err := s.CreateSandbox(ctx, conflict); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	s.SetFaults(Faults{HoldDeletes: true})
	if _, err := s.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{SandboxId: "one"}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Active != 1 {
		t.Fatal("held delete removed instance")
	}
	s.SetFaults(Faults{})
	if _, err := s.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{SandboxId: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSandbox(ctx, &pb.GetSandboxRequest{SandboxId: "one"}); status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
	if _, err := s.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{SandboxId: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSandbox(ctx, r); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("deleted identity recreated", err)
	}
	s.SetFaults(Faults{Unavailable: true})
	if _, err := s.Health(ctx, &pb.HealthRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
}
