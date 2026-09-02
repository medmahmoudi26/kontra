//go:build linux

package cas

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The two rungs a normal test run cannot reach, because they are properties of a filesystem
// and not of the code: extent sharing needs btrfs or XFS-with-reflink (the box this repo is
// developed on is ext4), and a real ENOSPC needs a filesystem small enough to fill.
//
// They MOUNT, so they are behind KONTRA_CAS_FS_TESTS=1 rather than skipping silently on a
// machine that could actually run them. `go test ./...` never touches the mount table;
// `KONTRA_CAS_FS_TESTS=1 go test ./internal/cas -run TestRealFilesystem -v` does.

func requireFSTests(t *testing.T, tool string) {
	t.Helper()
	if os.Getenv("KONTRA_CAS_FS_TESTS") != "1" {
		t.Skip("set KONTRA_CAS_FS_TESTS=1 to run the tests that mount a filesystem")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount")
	}
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s not installed: %v", tool, err)
	}
}

func mountAt(t *testing.T, args ...string) string {
	t.Helper()
	mnt := filepath.Join(t.TempDir(), "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("mount", append(args, mnt)...).CombinedOutput()
	if err != nil {
		t.Fatalf("mount %v: %v\n%s", args, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			t.Errorf("umount %s: %v\n%s", mnt, err, out)
		}
	})
	return mnt
}

// TestRealFilesystemReflink proves the top rung on a filesystem that has it, and proves the
// property that matters with it: extents shared with the store, and a write to the working
// copy that the store does not see.
func TestRealFilesystemReflink(t *testing.T) {
	requireFSTests(t, "mkfs.btrfs")

	img := filepath.Join(t.TempDir(), "reflink.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(300 << 20); err != nil { // btrfs refuses anything much smaller
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.btrfs", "-q", "-f", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.btrfs: %v\n%s", err, out)
	}
	mnt := mountAt(t, "-o", "loop", img)

	l, err := NewLocal(filepath.Join(mnt, "store"))
	if err != nil {
		t.Fatal(err)
	}
	golden := bytes.Repeat([]byte("reflinked bytes."), 1<<16) // 1 MiB, several extents
	digest, _, err := l.Put(bytes.NewReader(golden), "test")
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []Mode{Writable, Shared} {
		dest := filepath.Join(mnt, "work", mode.String(), "artifact")
		method, err := l.Materialize(digest, dest, mode)
		if err != nil {
			t.Fatal(err)
		}
		if method != MethodReflink {
			t.Fatalf("%s materialization on btrfs used %s, want reflink", mode, method)
		}
		t.Logf("%-8s -> %s", mode, method)

		si, _ := os.Stat(dest)
		p, _ := l.Path(digest)
		gi, _ := os.Stat(p)
		if os.SameFile(si, gi) {
			t.Fatal("a reflink must be a separate inode, not the stored object")
		}
	}

	// Overwrite the writable working copy. Extents were shared a moment ago; the store must
	// still hold the golden bytes.
	dest := filepath.Join(mnt, "work", Writable.String(), "artifact")
	if err := os.WriteFile(dest, bytes.Repeat([]byte("tampered!"), 1<<16), 0o644); err != nil {
		t.Fatal(err)
	}
	stored, err := l.ReadVerified(digest, "artifact")
	if err != nil {
		t.Fatalf("the stored object no longer verifies after its reflink was overwritten: %v", err)
	}
	if !bytes.Equal(stored, golden) {
		t.Fatal("writing a reflinked working copy reached the store")
	}
}

// TestRealFilesystemDiskFull fills a filesystem for real and requires the store to say so.
func TestRealFilesystemDiskFull(t *testing.T) {
	requireFSTests(t, "mount")

	mnt := mountAt(t, "-t", "tmpfs", "-o", "size=1M", "tmpfs")
	l, err := NewLocal(filepath.Join(mnt, "store"))
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = l.Put(bytes.NewReader(bytes.Repeat([]byte("x"), 8<<20)), "https://mirror.invalid/big.tar.gz")
	if err == nil {
		t.Fatal("stored 8 MiB on a 1 MiB filesystem")
	}
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("error = %v, want ErrDiskFull — a generic failure here is what costs the hours", err)
	}
	if !strings.Contains(err.Error(), "out of disk space") {
		t.Errorf("error %q does not say out of disk space", err)
	}
	t.Logf("full disk reads: %v", err)

	// The refusal must not leave the wreckage of the attempt behind on an already-full disk.
	assertNoTemporaries(t, l)
	if n := countFiles(t, filepath.Join(l.Root(), "cas")); n != 0 {
		t.Errorf("store holds %d objects after a disk-full write, want 0", n)
	}
}
