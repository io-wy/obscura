// Package backend defines the provider-neutral sandbox lifecycle and session contracts.
package backend

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("backend sandbox not found")
	ErrUnsupported  = errors.New("backend capability not supported")
	ErrAmbiguous    = errors.New("creation outcome is unknown; refusing automatic recreation")
	ErrConflict     = errors.New("backend sandbox specification conflict")
	ErrPrecondition = errors.New("backend requires operator action before retry")
	ErrInvalid      = errors.New("backend rejected the request")
	ErrAuth         = errors.New("backend credentials or permissions are invalid")
	ErrRateLimited  = errors.New("backend rate limit reached")
	ErrCapacity     = errors.New("backend capacity exhausted")
	ErrUnavailable  = errors.New("backend temporarily unavailable")
)

type State string

const (
	Creating State = "Creating"
	Ready    State = "Ready"
	Paused   State = "Paused"
	Failed   State = "Failed"
	Deleted  State = "Deleted"
)

// Config contains no credential values and may be persisted with the binding.
type Config struct {
	Type             string `json:"type"`
	Endpoint         string `json:"endpoint"`
	APIKeySecretName string `json:"apiKeySecretName,omitempty"`
	APIKeySecretKey  string `json:"apiKeySecretKey,omitempty"`
}

type Resources struct {
	VCPUs     uint32 `json:"vcpus"`
	MemoryMiB uint32 `json:"memoryMiB"`
	DiskMiB   uint64 `json:"diskMiB"`
}

type NetworkPolicy struct {
	AllowEgress bool     `json:"allowEgress"`
	AllowHosts  []string `json:"allowHosts,omitempty"`
}

type Request struct {
	ID             string
	SpecHash       string
	Template       string
	Resources      Resources
	Network        NetworkPolicy
	TimeoutSeconds int64
	// AllowCreate is true only in the reconcile that durably records creation intent.
	// Idempotent backends can also ensure on subsequent reconciles. Others must
	// discover an existing instance or return ErrAmbiguous.
	AllowCreate bool
}

type Observation struct {
	Handle string
	State  State
}

type Execution struct {
	ID       string
	Command  string
	Input    []byte
	EnvVars  map[string]string
	Deadline time.Time
}

type Event struct {
	ExecutionID string `json:"executionID"`
	Sequence    uint64 `json:"sequence"`
	Type        string `json:"type"`
	Payload     string `json:"payload,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Backend interface {
	Validate(Request) error
	Ensure(context.Context, Request) (Observation, error)
	Observe(context.Context, string) (Observation, error)
	// Delete returns true only once backend resources are confirmed absent.
	Delete(context.Context, string) (bool, error)
	Exec(context.Context, string, Execution, func(Event) error) error
	Close() error
}

type Factory interface {
	Open(context.Context, string, Config) (Backend, error)
}
