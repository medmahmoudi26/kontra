package gate

import (
	"os"
	"testing"
)

// offlineEnv, set to 1, skips (saying why) a test whose pinned npm package cannot be installed; otherwise a failed
// install fails the test, as in test/e2e (D189, M3 review robustness-6): CI never passes an M3 test it did not run.
const offlineEnv = "KONTRA_E2E_OFFLINE"

// npmFailed fails t for an npm install that failed, or skips it under KONTRA_E2E_OFFLINE=1.
func npmFailed(t *testing.T, what string, err error, out []byte) {
	t.Helper()
	if os.Getenv(offlineEnv) == "1" {
		t.Skipf("%s: npm cannot install the pinned package here (%s=1): %v\n%s", what, offlineEnv, err, out)
	}
	t.Fatalf("%s: npm cannot install the pinned package (set %s=1 to skip instead): %v\n%s", what, offlineEnv, err, out)
}
