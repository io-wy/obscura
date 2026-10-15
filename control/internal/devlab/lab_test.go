//go:build envtest

package devlab

import (
	"context"
	"testing"
	"time"
)

func TestControlPlaneLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	lab, err := Start(ctx, Options{CRDDirectory: "../../../deploy/crds", APIKey: "integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lab.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := Smoke(ctx, lab.GatewayURL, lab.APIKey); err != nil {
		t.Fatal(err)
	}
	if err := ExerciseFaults(ctx, lab); err != nil {
		t.Fatal(err)
	}
}
