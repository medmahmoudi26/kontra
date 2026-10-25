package relay

import (
	"os"
	"testing"

	"github.com/medmahmoudi26/kontra/internal/temporaltest"
)

// TestMain stops the Temporal development server the integration tests over the lifecycle started.
func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}
