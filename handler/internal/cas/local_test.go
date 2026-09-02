package cas

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/medmahmoudi26/kontra-local/handler/internal/objectstore"
)

// TestKeyLayoutIsCongruent is the guard the package header promises: the appliance's local
// store and the S3-backed one must address an object identically, or the embedded registry
// and the object store hold the same bytes under two names.
func TestKeyLayoutIsCongruent(t *testing.T) {
	digest := Sha256Hex([]byte("congruence"))

	if got, want := RelKey(digest), objectstore.NewMem().CasKey(digest); got != want {
		t.Fatalf("RelKey(%s) = %q, objectstore CasKey = %q", digest, got, want)
	}
	// And with a prefix, CasKey is RelKey with the prefix in front — nothing else.
	prefixed := objectstore.New(nil, "kontra").CasKey(digest)
	if want := "kontra/" + RelKey(digest); prefixed != want {
		t.Fatalf("prefixed CasKey = %q, want %q", prefixed, want)
	}

	l := newStore(t)
	p, err := l.Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(l.Root(), filepath.FromSlash(RelKey(digest))); p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
}

func TestPutIsStoreIfAbsent(t *testing.T) {
	l := newStore(t)
	payload := []byte("the store everything else hydrates from")

	digest, size, err := l.Put(bytes.NewReader(payload), "test")
	if err != nil {
		t.Fatal(err)
	}
	if digest != Sha256Hex(payload) || size != int64(len(payload)) {
		t.Fatalf("Put = %s/%d, want %s/%d", digest, size, Sha256Hex(payload), len(payload))
	}

	p, _ := l.Path(digest)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := before.Mode().Perm(); perm != 0o444 {
		t.Fatalf("stored object mode is %v, want 0444 — the Shared rung hardlinks it", perm)
	}

	// The second Put must not replace the inode: a working copy hardlinked to the first one
	// would silently stop tracking the store.
	if _, _, err := l.Put(bytes.NewReader(payload), "test"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("a second Put of identical bytes replaced the stored inode")
	}
	if n := countFiles(t, filepath.Join(l.Root(), "cas")); n != 1 {
		t.Fatalf("store holds %d objects after two identical Puts, want 1", n)
	}
	assertNoTemporaries(t, l)
}

// TestPutExpectingRefusesAndNamesBothSides is the acceptance criterion in one test: a
// mismatch stores nothing, and the message carries the expectation AND what arrived.
func TestPutExpectingRefusesAndNamesBothSides(t *testing.T) {
	l := newStore(t)
	arrived := []byte("what the mirror actually served")
	want := Sha256Hex([]byte("what the pin said"))

	_, err := l.PutExpecting(bytes.NewReader(arrived), want, "https://example.invalid/artifact.tar.gz")
	if err == nil {
		t.Fatal("PutExpecting stored bytes that did not match the pin")
	}
	var mismatch *DigestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is %T (%v), want *DigestMismatch", err, err)
	}
	msg := err.Error()
	for _, must := range []string{want, Sha256Hex(arrived), "https://example.invalid/artifact.tar.gz"} {
		if !strings.Contains(msg, must) {
			t.Errorf("error %q does not name %q", msg, must)
		}
	}

	// Nothing stored under EITHER digest, and no temporary left behind.
	for _, d := range []string{want, Sha256Hex(arrived)} {
		if has, err := l.Has(d); err != nil || has {
			t.Errorf("store holds %s after a refusal (err=%v)", d, err)
		}
	}
	if n := countFiles(t, filepath.Join(l.Root(), "cas")); n != 0 {
		t.Errorf("store holds %d objects after a refusal, want 0", n)
	}
	assertNoTemporaries(t, l)
}

func TestDigestValidationRefusesTraversal(t *testing.T) {
	l := newStore(t)
	for _, bad := range []string{
		"../../etc/passwd",
		"",
		strings.ToUpper(Sha256Hex([]byte("x"))),
		Sha256Hex([]byte("x"))[:63],
		Sha256Hex([]byte("x"))[:63] + "g",
	} {
		if _, err := l.Path(bad); !errors.Is(err, ErrBadDigest) {
			t.Errorf("Path(%q) error = %v, want ErrBadDigest", bad, err)
		}
		if _, err := l.Has(bad); !errors.Is(err, ErrBadDigest) {
			t.Errorf("Has(%q) error = %v, want ErrBadDigest", bad, err)
		}
		if _, err := l.PutExpecting(bytes.NewReader(nil), bad, "test"); !errors.Is(err, ErrBadDigest) {
			t.Errorf("PutExpecting(%q) error = %v, want ErrBadDigest", bad, err)
		}
	}
}

// TestModifiedWorkingCopyLeavesTheStoreAlone is the property the whole slice rests on.
func TestModifiedWorkingCopyLeavesTheStoreAlone(t *testing.T) {
	l := newStore(t)
	golden := []byte("#!/bin/sh\necho hydrated\n")
	digest, _, err := l.Put(bytes.NewReader(golden), "test")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "work", "run.sh")

	method, err := l.Materialize(digest, dest, Writable)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("materialized by %s", method)
	if method == MethodHardlink {
		t.Fatal("Writable materialization used a hardlink: the working copy shares the store's inode")
	}

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("a Writable working copy must be writable: %v", err)
	}
	if _, err := f.WriteString("echo tampered\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Truncating writes too, not only appends.
	if err := os.WriteFile(dest, []byte("replaced entirely"), 0o644); err != nil {
		t.Fatal(err)
	}

	stored, err := l.ReadVerified(digest, "artifact")
	if err != nil {
		t.Fatalf("the stored object no longer verifies after the working copy was modified: %v", err)
	}
	if !bytes.Equal(stored, golden) {
		t.Fatalf("stored object changed: %q", stored)
	}
}

// TestSharedMaterializationSharesBytes: two installs of the same artifact cost one copy,
// and the promise Shared asks for is enforced by the store's 0444 rather than trusted.
func TestSharedMaterializationSharesBytes(t *testing.T) {
	l := newStore(t)
	golden := bytes.Repeat([]byte("shared bytes "), 4096)
	digest, _, err := l.Put(bytes.NewReader(golden), "test")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	var methods []Method
	for _, name := range []string{"install-a/node", "install-b/node"} {
		m, err := l.Materialize(digest, filepath.Join(root, name), Shared)
		if err != nil {
			t.Fatal(err)
		}
		methods = append(methods, m)
	}
	t.Logf("two installs materialized by %v on this filesystem", methods)

	for _, m := range methods {
		if m == MethodCopy {
			t.Log("filesystem gave neither reflink nor hardlink; bytes are not shared here")
		}
	}

	a := filepath.Join(root, "install-a/node")
	if got, err := os.ReadFile(a); err != nil || !bytes.Equal(got, golden) {
		t.Fatalf("working copy content wrong (err=%v)", err)
	}

	if methods[0] == MethodHardlink {
		si, _ := os.Stat(a)
		p, _ := l.Path(digest)
		gi, _ := os.Stat(p)
		if !os.SameFile(si, gi) {
			t.Fatal("MethodHardlink reported but the working copy is a different inode")
		}

		// The 0444 seal turns a broken Shared promise into EACCES — for everyone except uid
		// 0, which ignores mode bits entirely. That caveat is documented on Mode and pinned
		// here, because this repo runs as root on every controller it has ever provisioned
		// and a test asserting EACCES would simply never run there.
		f, err := os.OpenFile(a, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			f.Close()
		}
		if os.Geteuid() == 0 {
			if err != nil {
				t.Logf("uid 0 and the open still failed: %v", err)
			} else {
				t.Log("uid 0: the 0444 backstop is not enforced, so Shared rests on the caller's promise alone")
			}
		} else if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("a hardlinked Shared working copy opened for writing (err=%v) — that write would hit the store", err)
		}
	}
}

func TestMaterializeMissingNamesTheDigest(t *testing.T) {
	l := newStore(t)
	digest := Sha256Hex([]byte("never stored"))
	_, err := l.Materialize(digest, filepath.Join(t.TempDir(), "x"), Writable)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("error = %v, want ErrMissing", err)
	}
	if !strings.Contains(err.Error(), digest) {
		t.Errorf("error %q does not name the digest", err)
	}
}

// TestConcurrentPutProducesOneStore races the publish. The store must end with exactly one
// object and no debris — never two half-written ones.
func TestConcurrentPutProducesOneStore(t *testing.T) {
	l := newStore(t)
	payload := bytes.Repeat([]byte("race "), 200_000) // big enough that the writes overlap
	want := Sha256Hex(payload)

	const n = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = l.PutExpecting(bytes.NewReader(payload), want, fmt.Sprintf("racer-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
	}
	if n := countFiles(t, filepath.Join(l.Root(), "cas")); n != 1 {
		t.Fatalf("store holds %d objects after %d concurrent identical Puts, want 1", n, n)
	}
	got, err := l.ReadVerified(want, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the surviving object is not the payload")
	}
	assertNoTemporaries(t, l)
}

// TestDiskFullIsNamedNotGeneric feeds the classifier a REAL kernel ENOSPC — obtained by
// writing to /dev/full, not by constructing an error value — and requires the message to
// say out of disk rather than something the operator has to decode.
func TestDiskFullIsNamedNotGeneric(t *testing.T) {
	real := realENOSPC(t)

	err := DiskError("write object to", filepath.Join(t.TempDir(), "put-123"), real)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("errors.Is(%v, ErrDiskFull) = false", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the errno did not survive wrapping: %v", err)
	}
	var full *DiskFullError
	if !errors.As(err, &full) {
		t.Fatalf("error is %T, want *DiskFullError", err)
	}
	if !strings.Contains(err.Error(), "out of disk space") {
		t.Errorf("error %q does not say out of disk space", err)
	}
	if !strings.Contains(err.Error(), "put-123") {
		t.Errorf("error %q does not name the path it failed to write", err)
	}
	if !full.HaveFree {
		t.Error("no free-space figure: the message cannot tell a full disk from a spent quota")
	}
	t.Logf("disk-full error reads: %v", err)

	// And an unrelated errno must NOT be dressed up as a full disk.
	if e := DiskError("write object to", "/x", syscall.EACCES); errors.Is(e, ErrDiskFull) {
		t.Errorf("EACCES classified as a full disk: %v", e)
	}
}

// TestWriteErrorIsAttributedToTheRightSide: io.Copy returns one error for a dropped source
// and for a failed write. The message has to say which, or the operator checks the wrong
// thing first.
func TestWriteErrorIsAttributedToTheRightSide(t *testing.T) {
	l := newStore(t)
	boom := errors.New("connection reset by peer")
	_, _, err := l.put(io.MultiReader(bytes.NewReader([]byte("half")), errReader{boom}), "", "https://mirror.invalid/a.tar.gz")
	if err == nil {
		t.Fatal("a truncated source was stored")
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "read https://mirror.invalid/a.tar.gz") {
		t.Fatalf("error %q does not blame the source", err)
	}
	if errors.Is(err, ErrDiskFull) {
		t.Fatalf("a source failure was reported as a disk failure: %v", err)
	}
	assertNoTemporaries(t, l)
	if n := countFiles(t, filepath.Join(l.Root(), "cas")); n != 0 {
		t.Fatalf("store holds %d objects after a truncated fetch, want 0", n)
	}
}

func TestCopyOnWriteRefusesANonRegularSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := CopyOnWrite(dir, filepath.Join(dir, "out"), Writable); err == nil {
		t.Fatal("CopyOnWrite accepted a directory as its source")
	}
}

// --- helpers ---

func newStore(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// realENOSPC gets a genuine ENOSPC from the kernel. /dev/full exists precisely so that this
// does not have to be faked.
func realENOSPC(t *testing.T) error {
	t.Helper()
	f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full on this platform: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("x")); err != nil {
		return err
	}
	t.Skip("/dev/full accepted a write")
	return nil
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return n
}

func assertNoTemporaries(t *testing.T, l *Local) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(l.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("%d temporary file(s) left in the store: %v", len(entries), names)
	}
}
