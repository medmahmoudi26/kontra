package mcprelay

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// offlineEnv, set to 1, skips (saying why) a test whose pinned npm package cannot be installed; otherwise a failed
// install fails the test, as in test/e2e (D189, M3 review robustness-6): CI never passes an M3 test it did not run.
const offlineEnv = "KONTRA_E2E_OFFLINE"

// requireNode24Env, set to 1 (CI), fails a test that needs Node 24 when none is found, instead of skipping it.
const requireNode24Env = "KONTRA_REQUIRE_NODE24"

// npmFailed fails t for an npm install that failed, or skips it under KONTRA_E2E_OFFLINE=1.
func npmFailed(t *testing.T, what string, err error, out []byte) {
	t.Helper()
	if os.Getenv(offlineEnv) == "1" {
		t.Skipf("%s: npm cannot install the pinned package here (%s=1): %v\n%s", what, offlineEnv, err, out)
	}
	t.Fatalf("%s: npm cannot install the pinned package (set %s=1 to skip instead): %v\n%s", what, offlineEnv, err, out)
}

// node24 is a Node 24 (or later) binary: KONTRA_NODE24 when set, else node on PATH. Without one the test is skipped,
// saying why, or fails under KONTRA_REQUIRE_NODE24=1.
func node24(t *testing.T, what string) string {
	t.Helper()
	node := os.Getenv("KONTRA_NODE24")
	if node == "" {
		node = "node"
	}
	out, err := exec.Command(node, "--version").Output()
	major := 0
	if err == nil {
		v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
		major, _ = strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	}
	path, perr := exec.LookPath(node)
	if major < 24 || perr != nil {
		msg := what + " needs Node 24 (D171); " + node + " reports " + strconv.Quote(strings.TrimSpace(string(out)))
		if os.Getenv(requireNode24Env) == "1" {
			t.Fatalf("%s (%s=1)", msg, requireNode24Env)
		}
		t.Skip(msg)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
