//go:build unix

package hydrate

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/handler/internal/cas"
)

// ONE REAL ARTIFACT, END TO END, ON AN EMPTY STORE.
//
// The artifact is the pinned Node runtime — the appliance's actual first customer, since
// the orchestrator is carried rather than rewritten (ADR 0031 §1) and has to run on
// something. Not a fixture: 54 MB fetched from nodejs.org over TLS, verified against
// upstream's own SHASUMS256.txt entry, expanded, materialized twice, and then EXECUTED,
// because a hydrated runtime that unpacks but does not run is the failure this whole slice
// exists to prevent.
//
// It is behind an env var rather than -short so that `go test ./...` stays hermetic: the
// package's other tests serve their bytes from httptest and need no network at all.
//
//	KONTRA_HYDRATE_REAL=1 go test ./internal/hydrate -run TestRealArtifactNodeRuntime -v
func TestRealArtifactNodeRuntime(t *testing.T) {
	if os.Getenv("KONTRA_HYDRATE_REAL") != "1" {
		t.Skip("set KONTRA_HYDRATE_REAL=1 to hydrate the real Node runtime over the network")
	}

	a, err := NodeRuntime(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	t.Logf("artifact   %s", a.Name)
	t.Logf("url        %s", a.URL)
	t.Logf("pin        sha256:%s", a.Digest)

	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if has, err := s.CAS().Has(a.Digest); err != nil || has {
		t.Fatalf("the store is not empty (has=%v err=%v)", has, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	installA := filepath.Join(root, "installs", "a")
	first, err := s.Hydrate(ctx, a, installA, cas.Shared)
	if err != nil {
		t.Fatalf("first hydration: %v", err)
	}
	object, _ := s.CAS().Path(a.Digest)
	objectInfo, err := os.Stat(object)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fetched    %d bytes in %s -> %s", objectInfo.Size(), first.Fetching.Round(time.Millisecond), object)
	t.Logf("install A  %d files, %d bytes, weakest rung %s, %s",
		first.Files, first.Bytes, first.Method, first.Elapsed.Round(time.Millisecond))
	if !first.Fetched {
		t.Error("an empty store did not fetch")
	}

	// THE PROOF THAT MATTERS: the hydrated runtime runs, and it is the pinned version.
	node := filepath.Join(installA, "bin", "node")
	out, err := exec.CommandContext(ctx, node, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("the hydrated runtime does not run: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	t.Logf("exec       %s --version -> %s", node, got)
	if got != "v"+NodeVersion {
		t.Fatalf("hydrated runtime reports %s, want v%s", got, NodeVersion)
	}

	// Second install, same artifact: no network, and on a filesystem with reflink or
	// hardlink, no bytes either.
	installB := filepath.Join(root, "installs", "b")
	second, err := s.Hydrate(ctx, a, installB, cas.Shared)
	if err != nil {
		t.Fatalf("second hydration: %v", err)
	}
	t.Logf("install B  %d files, %d bytes, weakest rung %s, %s (fetched=%v)",
		second.Files, second.Bytes, second.Method, second.Elapsed.Round(time.Millisecond), second.Fetched)
	if second.Fetched {
		t.Error("the second install went back to the network")
	}
	if second.Files != first.Files {
		t.Errorf("second install has %d files, first had %d", second.Files, first.Files)
	}

	apparent, unique := apparentAndUnique(t, installA, installB)
	t.Logf("sharing    two installs are %d bytes of files, %d bytes of distinct inodes (%.1f%% shared)",
		apparent, unique, 100*(1-float64(unique)/float64(apparent)))
	if second.Method != cas.MethodCopy && unique >= apparent {
		t.Errorf("materialized by %s but the two installs share no inodes", second.Method)
	}

	// A modified working copy must not reach the store. Writable, because that is the mode
	// whose whole point is that writing is allowed.
	installC := filepath.Join(root, "installs", "c")
	if _, err := s.Hydrate(ctx, a, installC, cas.Writable); err != nil {
		t.Fatal(err)
	}
	changelog := filepath.Join(installC, "CHANGELOG.md")
	if err := os.WriteFile(changelog, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(s.Root(), "trees", a.Digest, "CHANGELOG.md")); err != nil {
		t.Fatal(err)
	} else if bytes.Equal(body, []byte("tampered\n")) {
		t.Fatal("writing a working copy reached the golden tree")
	}
	if _, err := os.Stat(node); err != nil {
		t.Fatalf("install A damaged by a write to install C: %v", err)
	}

	// And the stored object still hashes to its pin, streamed rather than read whole.
	f, err := s.CAS().Open(a.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := newHasher()
	if _, err := copyInto(h, f); err != nil {
		t.Fatal(err)
	}
	if h.hex() != a.Digest {
		t.Fatalf("the stored object now hashes to %s", h.hex())
	}
	t.Logf("verified   the stored object still hashes to its pin")
	assertClean(t, s)
}

// apparentAndUnique sums the file sizes across the given trees, and separately sums each
// distinct inode once. The gap between them is what copy-on-write bought.
func apparentAndUnique(t *testing.T, dirs ...string) (apparent, unique int64) {
	t.Helper()
	seen := map[uint64]bool{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			apparent += info.Size()
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				unique += info.Size()
				return nil
			}
			if !seen[uint64(st.Ino)] {
				seen[uint64(st.Ino)] = true
				unique += info.Size()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return apparent, unique
}
