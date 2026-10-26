package gate

import (
	"os"
	"testing"

	"kontra.sh/kontra/internal/temporaltest"
)

// TestMain stops the Temporal development server the replica tests started.
func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}
