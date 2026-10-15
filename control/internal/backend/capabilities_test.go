package backend

import (
	"context"
	"testing"
)

type lifecycleOnlyBackend struct{}

func (lifecycleOnlyBackend) Validate(Request) error { return nil }
func (lifecycleOnlyBackend) Ensure(context.Context, Request) (Observation, error) {
	return Observation{}, nil
}
func (lifecycleOnlyBackend) Observe(context.Context, string) (Observation, error) {
	return Observation{}, nil
}
func (lifecycleOnlyBackend) Delete(context.Context, string) (bool, error) { return true, nil }
func (lifecycleOnlyBackend) Close() error                                 { return nil }

func TestLifecycleBackendDoesNotNeedSessionCapabilities(t *testing.T) {
	var lifecycle Backend = lifecycleOnlyBackend{}
	if _, ok := lifecycle.(Executor); ok {
		t.Fatal("lifecycle-only backend unexpectedly implements execution")
	}
}
