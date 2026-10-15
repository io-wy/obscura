package observability

import (
	"log/slog"
	"os"
	"sync/atomic"
)

type Fields struct {
	SandboxID   string
	ExecutionID string
	RequestID   string
}

type Metrics struct {
	CreatesTotal    atomic.Uint64
	DeletesTotal    atomic.Uint64
	ExecutionsTotal atomic.Uint64
	CleanupFailures atomic.Uint64
}

func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: true}))
}

func (m *Metrics) Create() { m.CreatesTotal.Add(1) }

func (m *Metrics) Delete() { m.DeletesTotal.Add(1) }

func (m *Metrics) Execute() { m.ExecutionsTotal.Add(1) }

func (m *Metrics) CleanupFailed() { m.CleanupFailures.Add(1) }
