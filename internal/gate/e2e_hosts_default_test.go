//go:build !kontra_e2e

package gate

import (
	"log/slog"
	"testing"
)

// A gate built without the kontra_e2e tag (every release) never resolves names from KONTRA_E2E_HOSTS.
func TestReleaseIgnoresE2EHosts(t *testing.T) {
	t.Setenv(E2EHostsEnv, "webhook.private.e2e.invalid=10.0.0.1")
	if r := pushResolver(slog.Default()); r != nil {
		t.Errorf("a release build resolves %s: %T", E2EHostsEnv, r)
	}
}
