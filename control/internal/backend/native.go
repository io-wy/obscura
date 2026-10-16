package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	pb "github.com/obscura/agentruntime/control/gen/agentruntime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type nativeBackendClient struct {
	conn *grpc.ClientConn
	rpc  pb.DataPlaneClient
}

const nativeProtocolVersion = "agentruntime.v1"

func openNative(endpoint string) (Backend, error) {
	socket := strings.TrimPrefix(endpoint, "unix://")
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("native runtime requires an absolute Unix socket path")
	}
	conn, err := grpc.NewClient("passthrough:///runtime", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}))
	if err != nil {
		return nil, fmt.Errorf("open native runtime client: %w", err)
	}
	return &nativeBackendClient{conn: conn, rpc: pb.NewDataPlaneClient(conn)}, nil
}
func (b *nativeBackendClient) Close() error { return b.conn.Close() }
func (b *nativeBackendClient) Validate(r Request) error {
	if _, err := uuid.Parse(r.ID); err != nil {
		return fmt.Errorf("native sandbox ID must be a UUID")
	}
	if r.SpecHash == "" || r.Template == "" {
		return fmt.Errorf("specification hash and template are required")
	}
	if r.Resources.VCPUs == 0 || r.Resources.MemoryMiB == 0 || r.Resources.DiskMiB == 0 {
		return fmt.Errorf("native CPU, memory and disk resources must be positive")
	}
	if r.TimeoutSeconds <= 0 || r.TimeoutSeconds > math.MaxInt64/int64(time.Second) {
		return fmt.Errorf("native timeout is outside supported range")
	}
	if len(r.Network.AllowHosts) > 0 {
		if r.Network.Enforcement != "auditOnly" {
			return fmt.Errorf("%w: native allowHosts enforcement is not implemented", ErrUnsupported)
		}
	}
	if r.CredentialPolicy.PolicyRef != "" && !r.CredentialPolicy.Brokered {
		return fmt.Errorf("%w: credential policy must use broker", ErrInvalid)
	}
	return nil
}
func nativeError(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		return ErrNotFound
	case codes.AlreadyExists:
		return ErrConflict
	case codes.FailedPrecondition:
		return ErrPrecondition
	case codes.InvalidArgument:
		return ErrInvalid
	case codes.Unauthenticated, codes.PermissionDenied:
		return ErrAuth
	case codes.ResourceExhausted:
		return ErrCapacity
	case codes.Unimplemented:
		return ErrUnsupported
	default:
		return fmt.Errorf("%w: native runtime RPC failed (%s)", ErrUnavailable, status.Code(err))
	}
}
func nativeObservation(s *pb.Sandbox) (Observation, error) {
	if s == nil || s.SandboxId == "" {
		return Observation{}, fmt.Errorf("native runtime response has no sandbox")
	}
	var state State
	switch s.State {
	case pb.SandboxState_SANDBOX_STATE_CREATING:
		state = Creating
	case pb.SandboxState_SANDBOX_STATE_READY, pb.SandboxState_SANDBOX_STATE_RUNNING:
		state = Ready
	case pb.SandboxState_SANDBOX_STATE_PAUSED:
		state = Paused
	case pb.SandboxState_SANDBOX_STATE_FAILED:
		state = Failed
	case pb.SandboxState_SANDBOX_STATE_DELETED:
		state = Deleted
	default:
		return Observation{}, fmt.Errorf("unknown native runtime state")
	}
	return Observation{Handle: s.SandboxId, State: state}, nil
}
func (b *nativeBackendClient) Ensure(ctx context.Context, r Request) (Observation, error) {
	if err := b.Validate(r); err != nil {
		return Observation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	health, err := b.rpc.Health(ctx, &pb.HealthRequest{})
	if err != nil {
		return Observation{}, nativeError(err)
	}
	if health.GetProtocolVersion() != nativeProtocolVersion {
		return Observation{}, fmt.Errorf("%w: native runtime protocol version %q is incompatible", ErrUnsupported, health.GetProtocolVersion())
	}
	supported := false
	for _, capability := range health.GetCapabilities() {
		if capability == "idempotent-create-v1" {
			supported = true
		}
	}
	if !supported {
		return Observation{}, fmt.Errorf("%w: native runtime must advertise idempotent-create-v1", ErrUnsupported)
	}
	result, err := b.rpc.CreateSandbox(ctx, &pb.CreateSandboxRequest{RequestId: r.ID, SandboxId: r.ID, SpecHash: r.SpecHash, TemplateRef: r.Template, Resources: &pb.ResourceSpec{Vcpus: r.Resources.VCPUs, MemoryMib: r.Resources.MemoryMiB, DiskMib: r.Resources.DiskMiB, MaxRuntimeNanos: r.TimeoutSeconds * int64(time.Second)}, NetworkPolicy: &pb.NetworkPolicy{AllowEgress: r.Network.AllowEgress, Enforcement: networkEnforcement(r.Network.Enforcement)}, VersionPin: &pb.VersionPin{RuntimeVersion: r.VersionPin.RuntimeVersion, ImageDigest: r.VersionPin.ImageDigest, DependencyLock: r.VersionPin.DependencyLock, DatasetVersion: r.VersionPin.DatasetVersion, ScorerVersion: r.VersionPin.ScorerVersion}, Workspace: &pb.WorkspaceSpec{Mode: workspaceMode(r.Workspace.Mode), Reference: r.Workspace.Reference}, CredentialPolicy: &pb.CredentialPolicy{PolicyRef: r.CredentialPolicy.PolicyRef, Brokered: r.CredentialPolicy.Brokered}})
	if err != nil {
		return Observation{}, nativeError(err)
	}
	if result.GetSandbox().GetSandboxId() != r.ID {
		return Observation{}, ErrConflict
	}
	return nativeObservation(result.GetSandbox())
}

func networkEnforcement(v string) pb.NetworkEnforcement {
	switch v {
	case "auditOnly":
		return pb.NetworkEnforcement_NETWORK_ENFORCEMENT_AUDIT_ONLY
	case "enforced":
		return pb.NetworkEnforcement_NETWORK_ENFORCEMENT_ENFORCED
	default:
		return pb.NetworkEnforcement_NETWORK_ENFORCEMENT_REJECT_UNSUPPORTED
	}
}
func workspaceMode(v string) pb.WorkspaceMode {
	switch v {
	case "persistent":
		return pb.WorkspaceMode_WORKSPACE_MODE_PERSISTENT
	case "snapshot":
		return pb.WorkspaceMode_WORKSPACE_MODE_SNAPSHOT
	default:
		return pb.WorkspaceMode_WORKSPACE_MODE_EPHEMERAL
	}
}
func (b *nativeBackendClient) Observe(ctx context.Context, id string) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	response, err := b.rpc.GetSandbox(ctx, &pb.GetSandboxRequest{SandboxId: id})
	if err != nil {
		return Observation{}, nativeError(err)
	}
	if response.GetSandbox().GetSandboxId() != id {
		return Observation{}, ErrConflict
	}
	return nativeObservation(response.GetSandbox())
}
func (b *nativeBackendClient) Delete(ctx context.Context, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	_, err := b.rpc.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{RequestId: id, SandboxId: id})
	err = nativeError(err)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	observed, err := b.Observe(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return observed.State == Deleted, err
}
func (b *nativeBackendClient) Exec(ctx context.Context, id string, e Execution, emit func(Event) error) error {
	if id == "" || e.ID == "" || e.Command == "" || emit == nil {
		return fmt.Errorf("sandbox, execution, command and event consumer are required")
	}
	if !e.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, e.Deadline)
		defer cancel()
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	var deadline int64
	if d, ok := ctx.Deadline(); ok {
		deadline = d.UnixNano()
	}
	stream, err := b.rpc.Execute(ctx, &pb.ExecuteRequest{RequestId: e.ID, SandboxId: id, ExecutionId: e.ID, Command: e.Command, Input: e.Input, EnvVars: e.EnvVars, DeadlineUnixNanos: deadline})
	if err != nil {
		return nativeError(err)
	}
	terminal := false
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if terminal {
				return nil
			}
			return fmt.Errorf("%w: runtime execution ended without a result", ErrUnavailable)
		}
		if err != nil {
			return nativeError(err)
		}
		if terminal {
			return fmt.Errorf("%w: runtime execution sent an event after its result", ErrUnavailable)
		}
		kind := strings.ToLower(strings.TrimPrefix(event.Type.String(), "EVENT_TYPE_"))
		if err := emit(Event{ExecutionID: event.ExecutionId, Sequence: event.Sequence, Type: kind, Payload: string(event.Payload), Error: event.Error}); err != nil {
			return err
		}
		terminal = event.Type == pb.EventType_EVENT_TYPE_RESULT || event.Type == pb.EventType_EVENT_TYPE_ERROR
	}
}
