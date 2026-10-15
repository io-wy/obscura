// Package quota provides optimistic, Kubernetes-backed tenant quota reservations.
package quota

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrQuotaExceeded       = errors.New("tenant quota exceeded")
	ErrReservationConflict = errors.New("reservation identity conflicts with an existing request")
)

const maxConflictRetries = 12

type SessionKind string

const (
	SessionExecution SessionKind = "execution"
	SessionPTY       SessionKind = "pty"
)

type SandboxRequest struct {
	SandboxID      string
	SpecHash       string
	Resources      *api.ResourceSpec
	TimeoutSeconds int64
}

type SessionRequest struct {
	ID        string
	SandboxID string
	Kind      SessionKind
	TTL       time.Duration
}

type Manager struct {
	Client client.Client
	// Reader must bypass informer lag. Quota decisions cannot use a stale cache.
	Reader client.Reader
	Now    func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) Get(ctx context.Context, name string) (*api.SandboxTenant, error) {
	reader := m.Reader
	if reader == nil {
		reader = m.Client
	}
	if reader == nil || m.Client == nil {
		return nil, fmt.Errorf("quota Kubernetes client is required")
	}
	tenant := &api.SandboxTenant{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, tenant); err != nil {
		return nil, err
	}
	return tenant, nil
}

func (m *Manager) update(ctx context.Context, name string, mutate func(*api.SandboxTenant) (bool, error)) error {
	var lastConflict error
	for range maxConflictRetries {
		tenant, err := m.Get(ctx, name)
		if err != nil {
			return err
		}
		before := tenant.DeepCopy()
		changed, err := mutate(tenant)
		if err != nil || !changed {
			return err
		}
		err = m.Client.Status().Patch(ctx, tenant, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if !apierrors.IsConflict(err) {
			return err
		}
		lastConflict = err
	}
	return fmt.Errorf("quota update conflict after %d retries: %w", maxConflictRetries, lastConflict)
}

func (m *Manager) ReserveSandbox(ctx context.Context, tenantName string, req SandboxRequest) error {
	if tenantName == "" || req.SandboxID == "" || req.SpecHash == "" || req.TimeoutSeconds < 1 {
		return fmt.Errorf("tenant, sandbox identity, specification hash and timeout are required")
	}
	return m.update(ctx, tenantName, func(tenant *api.SandboxTenant) (bool, error) {
		for i := range tenant.Status.Allocations {
			existing := &tenant.Status.Allocations[i]
			if existing.SandboxID != req.SandboxID {
				continue
			}
			if existing.SpecHash == req.SpecHash && existing.TimeoutSeconds == req.TimeoutSeconds && reflect.DeepEqual(existing.Resources, req.Resources) {
				return false, nil
			}
			return false, ErrReservationConflict
		}
		if err := checkSandbox(tenant, req); err != nil {
			return false, err
		}
		var resources *api.ResourceSpec
		if req.Resources != nil {
			resources = req.Resources.DeepCopy()
		}
		tenant.Status.Allocations = append(tenant.Status.Allocations, api.TenantAllocation{
			SandboxID: req.SandboxID, SpecHash: req.SpecHash, Resources: resources,
			TimeoutSeconds: req.TimeoutSeconds, ReservedAt: metav1.NewTime(m.now()),
		})
		return true, nil
	})
}

func checkSandbox(tenant *api.SandboxTenant, req SandboxRequest) error {
	q := tenant.Spec.Quota
	if int32(len(tenant.Status.Allocations)) >= q.MaxSandboxes || req.TimeoutSeconds > q.MaxTimeoutSeconds {
		return ErrQuotaExceeded
	}
	var vcpus, memory, disk int64
	for _, allocation := range tenant.Status.Allocations {
		if allocation.Resources != nil {
			vcpus += int64(allocation.Resources.VCPUs)
			memory += int64(allocation.Resources.MemoryMiB)
			disk += allocation.Resources.DiskMiB
		}
	}
	if r := req.Resources; r != nil {
		if exceeds(int64(r.VCPUs), int64(q.MaxVCPUsPerSandbox)) || exceeds(int64(r.MemoryMiB), int64(q.MaxMemoryMiBPerSandbox)) || exceeds(r.DiskMiB, q.MaxDiskMiBPerSandbox) {
			return ErrQuotaExceeded
		}
		vcpus += int64(r.VCPUs)
		memory += int64(r.MemoryMiB)
		disk += r.DiskMiB
	}
	if exceeds(vcpus, q.MaxTotalVCPUs) || exceeds(memory, q.MaxTotalMemoryMiB) || exceeds(disk, q.MaxTotalDiskMiB) {
		return ErrQuotaExceeded
	}
	return nil
}

func exceeds(value, limit int64) bool { return limit > 0 && value > limit }

func (m *Manager) ReleaseSandbox(ctx context.Context, tenantName, sandboxID string) error {
	if sandboxID == "" {
		return fmt.Errorf("sandbox identity is required")
	}
	return m.update(ctx, tenantName, func(tenant *api.SandboxTenant) (bool, error) {
		changed := removeAllocation(&tenant.Status.Allocations, sandboxID)
		sessions := tenant.Status.Sessions[:0]
		for _, session := range tenant.Status.Sessions {
			if session.SandboxID == sandboxID {
				changed = true
				continue
			}
			sessions = append(sessions, session)
		}
		tenant.Status.Sessions = sessions
		return changed, nil
	})
}

func removeAllocation(items *[]api.TenantAllocation, sandboxID string) bool {
	out := (*items)[:0]
	changed := false
	for _, item := range *items {
		if item.SandboxID == sandboxID {
			changed = true
			continue
		}
		out = append(out, item)
	}
	*items = out
	return changed
}

func (m *Manager) AcquireSession(ctx context.Context, tenantName string, req SessionRequest) error {
	if req.ID == "" || req.SandboxID == "" || req.TTL <= 0 || (req.Kind != SessionExecution && req.Kind != SessionPTY) {
		return fmt.Errorf("valid session identity, sandbox, kind and TTL are required")
	}
	now := m.now()
	return m.update(ctx, tenantName, func(tenant *api.SandboxTenant) (bool, error) {
		changed := pruneExpired(&tenant.Status.Sessions, now)
		allocationFound := false
		for _, allocation := range tenant.Status.Allocations {
			allocationFound = allocationFound || allocation.SandboxID == req.SandboxID
		}
		if !allocationFound {
			return false, fmt.Errorf("sandbox has no quota allocation")
		}
		var count int32
		for _, existing := range tenant.Status.Sessions {
			if existing.ID == req.ID {
				if existing.SandboxID == req.SandboxID && existing.Kind == string(req.Kind) {
					return changed, nil
				}
				return false, ErrReservationConflict
			}
			if existing.Kind == string(req.Kind) {
				count++
			}
		}
		limit := tenant.Spec.Quota.MaxConcurrentExecutions
		if req.Kind == SessionPTY {
			limit = tenant.Spec.Quota.MaxConcurrentPTY
		}
		if count >= limit {
			return false, ErrQuotaExceeded
		}
		tenant.Status.Sessions = append(tenant.Status.Sessions, api.TenantSessionLease{
			ID: req.ID, SandboxID: req.SandboxID, Kind: string(req.Kind), ExpiresAt: metav1.NewTime(now.Add(req.TTL)),
		})
		return true, nil
	})
}

func pruneExpired(items *[]api.TenantSessionLease, now time.Time) bool {
	out := (*items)[:0]
	changed := false
	for _, item := range *items {
		if !now.Before(item.ExpiresAt.Time) {
			changed = true
			continue
		}
		out = append(out, item)
	}
	*items = out
	return changed
}

func (m *Manager) ReleaseSession(ctx context.Context, tenantName, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session identity is required")
	}
	return m.update(ctx, tenantName, func(tenant *api.SandboxTenant) (bool, error) {
		out := tenant.Status.Sessions[:0]
		changed := false
		for _, item := range tenant.Status.Sessions {
			if item.ID == sessionID {
				changed = true
				continue
			}
			out = append(out, item)
		}
		tenant.Status.Sessions = out
		return changed, nil
	})
}
