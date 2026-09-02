package hydrate

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/medmahmoudi26/kontra/handler/internal/cas"
)

// A FULL DISK IS NOT A LOUD FAILURE ON THIS SYSTEM, and that is not a hypothesis. A SeaweedFS
// volume-slot ceiling turned every write into a bare HTTP 500 while the isolation counters
// reported nothing wrong; a root-owned Temporal volume answered "out of memory (14)" on a box
// with 7.9 GB free; this box hit 99% disk twice while this branch was being written. Every one
// of those cost hours because the message named a symptom instead of the disk.
//
// THESE TESTS USE A REAL FULL FILESYSTEM, not an injected errno. A fake ENOSPC proves that the
// classification function works on the error somebody remembered to pass it; a tmpfs sized to
// run out proves it on whichever syscall actually fails first — which is not the obvious one.
// The clone below fails inside copy_file_range, a call no hand-written fake would have thought
// to poison, and on a buffered write the failure often arrives out of close() rather than
// write(). Each of the three tests is a different one of the three places bytes get written.

// tinyFilesystem mounts a tmpfs of the given size and returns its path. It skips — never fails
// — when the environment will not allow a mount, because a store that cannot be tested on a
// full disk is still a store worth testing everywhere else.
func tinyFilesystem(t *testing.T, size string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("mounting a tmpfs needs root; run this suite as root to exercise a real full disk")
	}
	if _, err := exec.LookPath("mount"); err != nil {
		t.Skip("no mount(8) here")
	}
	dir := filepath.Join(t.TempDir(), "tiny")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size="+size, "tmpfs", dir).CombinedOutput(); err != nil {
		t.Skipf("cannot mount a tmpfs here (%v): %s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("umount", dir).CombinedOutput(); err != nil {
			t.Logf("umount %s: %v: %s", dir, err, out)
		}
	})
	return dir
}

// A fetch whose size is known and does not fit is refused BEFORE it spends the download, and
// the refusal says out of disk and which filesystem.
func TestAFetchThatCannotFitIsRefusedBeforeItStarts(t *testing.T) {
	root := tinyFilesystem(t, "2m")

	body := make([]byte, 6<<20)
	if _, err := rand.Read(body); err != nil { // incompressible: 6 MB is 6 MB
		t.Fatal(err)
	}
	srv, hits := serving(map[string][]byte{"/pkg.tar.gz": body})
	defer srv.Close()

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz}
	_, err = s.EnsureHydrated(context.Background(), a, filepath.Join(root, "work"), cas.Shared)
	if err == nil {
		t.Fatal("6 MB was hydrated onto a 2 MB filesystem")
	}
	assertSaysDisk(t, err)
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the refusal does not name the filesystem: %v", err)
	}
	// AND IT IS NOT RETRIED. A disk that is full is still full a millisecond later; a retry
	// would double the download and end with the same sentence.
	if n := atomic.LoadInt64(hits); n > 1 {
		t.Errorf("a full disk was retried: %d fetches", n)
	}
}

// The expansion is the step whose size NOTHING knows in advance, so it is the one that has to
// discover the disk. This is the failure the issue calls out by name: a process that runs out
// of room between the first and the last file of a copy-on-write expansion.
func TestAnExpansionThatFillsTheDiskSaysSo(t *testing.T) {
	// Big enough that the fetch's own preflight is satisfied — the archive is a few kilobytes
	// — and far too small for what those kilobytes turn into. That gap is the point: an
	// expansion's size is the one number nothing knows before reading it.
	root := tinyFilesystem(t, "48m")

	// Compresses to a few kilobytes and expands to 100 MB.
	entries := make([]tarEntry, 0, 100)
	for i := 0; i < 100; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("pkg/lib/chunk%03d", i), body: strings.Repeat("0", 1<<20), mode: 0o644})
	}
	body := tarGz(t, entries)
	srv, _ := serving(map[string][]byte{"/pkg.tar.gz": body})
	defer srv.Close()

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	_, err = s.EnsureHydrated(context.Background(), a, filepath.Join(root, "work"), cas.Shared)
	if err == nil {
		t.Fatal("100 MB was expanded onto a 48 MB filesystem")
	}
	assertSaysDisk(t, err)
	if !strings.Contains(err.Error(), "pkg") {
		t.Errorf("the failure does not name the artifact: %v", err)
	}
	// Nothing half-expanded is left under a name a caller reads.
	if _, err := os.Stat(filepath.Join(root, "trees", a.Digest)); err == nil {
		t.Error("a tree that ran out of disk was published anyway")
	}
}

// The clone is the third place bytes get written, and on a filesystem without reflink it is
// where most of them get written. A Writable working copy cannot be hardlinked, so this is a
// real 40 MB of copying onto a filesystem that cannot hold it.
func TestACloneThatFillsTheDiskSaysSo(t *testing.T) {
	root := tinyFilesystem(t, "64m")

	entries := make([]tarEntry, 0, 40)
	for i := 0; i < 40; i++ {
		entries = append(entries, tarEntry{name: fmt.Sprintf("pkg/lib/chunk%03d", i), body: strings.Repeat("0", 1<<20), mode: 0o644})
	}
	body := tarGz(t, entries)
	srv, _ := serving(map[string][]byte{"/pkg.tar.gz": body})
	defer srv.Close()

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	ctx := context.Background()

	// The tree fits (40 of 64 MB). A second, WRITABLE copy of it does not.
	if _, err := s.EnsureHydrated(ctx, a, filepath.Join(root, "first"), cas.Shared); err != nil {
		t.Fatalf("the first hydration should fit: %v", err)
	}
	_, err = s.EnsureHydrated(ctx, a, filepath.Join(root, "second"), cas.Writable)
	if err == nil {
		t.Skip("this filesystem gave the writable copy away for free (reflink); nothing was written to run out of")
	}
	assertSaysDisk(t, err)
	if _, err := os.Stat(filepath.Join(root, "second")); err == nil {
		t.Error("a working directory that ran out of disk was published anyway")
	}
}

// A disk-full failure must survive being wrapped in the repair path's own error, or the caller
// that branches on it — `kontra up`, deciding whether to tell an operator to free space — sees
// a generic hydration failure.
func assertSaysDisk(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, cas.ErrDiskFull) {
		t.Errorf("a full disk did not report as ErrDiskFull: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "out of disk space") {
		t.Errorf("the message does not say out of disk space: %v", err)
	}
	t.Logf("full disk reported as: %v", err)
}
