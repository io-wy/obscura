package observability_test

import (
	"testing"

	"github.com/obscura/agentruntime/control/internal/observability"
)

func TestMetricsAreMonotonic(t *testing.T) {
	var metrics observability.Metrics
	metrics.Create()
	metrics.Execute()
	metrics.CleanupFailed()
	if metrics.CreatesTotal.Load() != 1 || metrics.ExecutionsTotal.Load() != 1 || metrics.CleanupFailures.Load() != 1 {
		t.Fatalf("metrics did not record lifecycle events: creates=%d executions=%d cleanup_failures=%d", metrics.CreatesTotal.Load(), metrics.ExecutionsTotal.Load(), metrics.CleanupFailures.Load())
	}
}
