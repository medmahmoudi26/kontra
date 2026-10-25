package e2e

import (
	"context"
	"testing"
)

// start brings the harness up for one test. Under -short every e2e test is skipped; without -short a
// missing tool or a harness that cannot start fails the test, so `go test ./...` never reports an e2e
// pass that tested nothing.
func start(t *testing.T, o Options) *Harness {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: needs Docker, the temporal CLI and two gate replicas; skipped under -short")
	}
	h, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}
