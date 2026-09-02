// Package cosignstub fabricates a cosign on PATH, for tests on both sides of the trust boundary.
//
// IT IS A PACKAGE AND NOT A TEST HELPER because two packages need it: `internal/trustpolicy`'s own
// suite, which drives the verifier directly, and `warden`'s podman-driver suite, which checks that
// an image is admitted before it is run. A `func stubCosign` in one `_test.go` file is reachable
// from neither of the others, and copying it would give the two sides different fakes — which is
// the one thing a shared fixture must not do.
package cosignstub

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A SHELL SCRIPT AND NOT A Go BINARY, because the thing under test is `exec.LookPath` plus an exit
// status, and building a binary per case would put a `go build` on the critical path of a test whose
// whole point is that it is cheap. It writes its argv one per line so an argument containing a space
// is still readable.
//
// CALLING IT AGAIN REPLACES THE STUB, which is how the positive controls in this file work: the same
// policy, the same reference, a different exit status.
func Stub(t *testing.T, code int, say string) func(*testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> %q; done\n[ -n %q ] && echo %q >&2\nexit %d\n",
		log, say, say, code)
	bin := filepath.Join(dir, "cosign")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(t *testing.T) string {
		t.Helper()
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("cosign was never run: %v", err)
		}
		return string(b)
	}
}
