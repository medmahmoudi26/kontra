package hydrate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
)

// THE FAILURES ISSUE 15 IS ABOUT ARE ALL FAILURES THAT LOOK LIKE SUCCESS. Every test in this
// file starts from a store or a working directory that passes the check it replaced — the
// directory is there, the manifest is there, the object is at its address — and asserts that
// the artifact is nonetheless refused and re-hydrated.

// --- 1. a truncated download ------------------------------------------------------------------

// A transfer that stops halfway stores nothing, which is the store working correctly and the
// appliance still not starting. The recovery is to fetch again, once.
func TestATruncatedDownloadIsFetchedAgain(t *testing.T) {
	body := tarGz(t, []tarEntry{
		{name: "pkg/bin/node", body: strings.Repeat("x", 64<<10), mode: 0o755},
		{name: "pkg/README", body: "readme", mode: 0o644},
	})
	srv, hits := truncatingOnce(body)
	defer srv.Close()

	s := newStore(t)
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	dest := filepath.Join(t.TempDir(), "work")

	res, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatalf("a truncated first transfer was not recovered from: %v", err)
	}
	if n := atomic.LoadInt64(hits); n != 2 {
		t.Errorf("the artifact was fetched %d time(s); a truncated transfer must be retried exactly once", n)
	}
	if !res.Repaired || res.Damage == nil {
		t.Errorf("the retry was not reported: %+v", res)
	}
	if !strings.Contains(res.Damage.Error(), "unexpected EOF") {
		t.Errorf("the reported cause does not name the truncation: %v", res.Damage)
	}
	mustVerify(t, s, a, dest)
}

// --- 2. a partial materialization ---------------------------------------------------------------

// THE ONE THE ISSUE CALLS OUT: a working directory that EXISTS and is incomplete. Every check
// of the form "is it there" passes on it, which is why the check is not of that form.
func TestAPartialMaterializationIsNotTreatedAsPresent(t *testing.T) {
	s, a, dest, hits := hydrated(t)

	// What a process killed between the first and the last file of a clone leaves behind.
	victim := filepath.Join(dest, "lib", "node_modules", "deep", "index.js")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}

	// The check this replaced. Both of these still pass, which is the whole problem.
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the partial working directory is not even there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Fatalf("the partial working directory has no manifest.json, so this is not the case under test: %v", err)
	}

	before := atomic.LoadInt64(hits)
	res, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatalf("a partial working directory was not repaired: %v", err)
	}
	if !res.Repaired {
		t.Fatal("a working directory missing a file was reported as usable")
	}
	if got := res.Damage.Error(); !strings.Contains(got, "lib/node_modules/deep/index.js") || !strings.Contains(got, "missing") {
		t.Errorf("the damage does not name the missing file: %s", got)
	}
	if n := atomic.LoadInt64(hits) - before; n != 0 {
		t.Errorf("repairing a working directory went back to the network %d time(s); the store already had the bytes", n)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the repaired working directory is still missing the file: %v", err)
	}
	mustVerify(t, s, a, dest)
}

// A file that is present and SHORT is the disk-filled-mid-write shape of the same failure, and
// nothing cheaper than its length notices it.
func TestATruncatedFileInAWorkingCopyIsDetected(t *testing.T) {
	s, a, _, _ := hydrated(t)
	// Writable, so the working copy is its own bytes: truncating a Shared hardlink would be
	// truncating the golden tree, which is a different test (below).
	dest := filepath.Join(t.TempDir(), "writable")
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Writable); err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(dest, "bin", "node")
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(victim, 8); err != nil {
		t.Fatal(err)
	}

	err := s.Check(a, dest, Structure)
	if err == nil {
		t.Fatal("a truncated file in a working copy passed the check")
	}
	if !errors.Is(err, ErrDamaged) {
		t.Errorf("a truncated file was not reported as damage: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "bin/node") || !strings.Contains(got, "8 bytes") {
		t.Errorf("the damage does not say what is short: %s", got)
	}

	res, err := s.EnsureHydrated(context.Background(), a, dest, cas.Writable)
	if err != nil {
		t.Fatalf("a truncated working copy was not repaired: %v", err)
	}
	if !res.Repaired {
		t.Error("the repair was not reported")
	}
	mustVerify(t, s, a, dest)
}

// --- 3. a mutated store -------------------------------------------------------------------------

// Something changed the object under the store. It is detected on the read that was going to
// happen anyway — the expansion — and the answer is to throw the object away and fetch again.
func TestAMutatedStoredObjectIsDetectedOnReadAndFetchedAgain(t *testing.T) {
	s, a, _, hits := hydrated(t)

	// The tree is derived from the object and would be used instead of it, so this is a
	// machine whose store was damaged and whose cache was evicted — the ordinary way the
	// object gets read a second time at all.
	if err := os.RemoveAll(filepath.Join(s.Root(), "trees")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Root(), "trees"), 0o755); err != nil {
		t.Fatal(err)
	}
	mutateObject(t, s, a.Digest)

	// The store still says it has it: the address is taken and the file is a regular file.
	if has, err := s.CAS().Has(a.Digest); err != nil || !has {
		t.Fatalf("the mutated object is not at its address, so this is not the case under test (has=%v err=%v)", has, err)
	}

	before := atomic.LoadInt64(hits)
	dest := filepath.Join(t.TempDir(), "second")
	res, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatalf("a mutated stored object was not recovered from: %v", err)
	}
	if n := atomic.LoadInt64(hits) - before; n != 1 {
		t.Errorf("the artifact was fetched %d time(s) after its object was mutated, want 1", n)
	}
	if res.Damage == nil || !errors.Is(res.Damage, cas.ErrMutated) {
		t.Errorf("the recovery does not name the mutation: %v", res.Damage)
	}
	if got := res.Damage.Error(); !strings.Contains(got, "changed it under the store") {
		t.Errorf("the message does not say what happened: %s", got)
	}
	mustVerify(t, s, a, dest)
	if err := s.CAS().Verify(a.Digest); err != nil {
		t.Errorf("the store still holds the mutated object: %v", err)
	}
}

// A malformed archive is NOT a mutated object, and the difference decides whether fetching
// again is worth anything. The object here hashes to exactly what it claims; the bytes
// upstream published are simply not a gzip, so a second fetch would produce the same file and
// the same failure, and the message has to say that rather than blame the disk.
func TestAnArchiveThatIsNotAnArchiveIsNotBlamedOnTheDisk(t *testing.T) {
	body := []byte("this is not gzip, and it is exactly what upstream published")
	srv, hits := serving(map[string][]byte{"/pkg.tar.gz": body})
	defer srv.Close()

	s := newStore(t)
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz}
	_, err := s.EnsureHydrated(context.Background(), a, filepath.Join(t.TempDir(), "work"), cas.Shared)
	if err == nil {
		t.Fatal("a bundle that is not an archive was hydrated")
	}
	if errors.Is(err, cas.ErrMutated) {
		t.Errorf("a malformed artifact was reported as a mutated store: %v", err)
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("the failure does not say what is wrong with the artifact: %v", err)
	}
	// FETCHED ONCE, THOUGH IT WAS ATTEMPTED TWICE. The retry runs — the artifact is not a full
	// disk and not a bad pin — but it does not go back to the network, because the object
	// hashes to its own address and the store has no reason to distrust it. That is the whole
	// value of separating "the disk changed the bytes" from "the bytes are like this".
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Errorf("fetched %d time(s), want 1: a verified object must not be re-downloaded to fail the same way", n)
	}
	if err := s.CAS().Verify(a.Digest); err != nil {
		t.Errorf("the object was discarded even though it hashes to its own address: %v", err)
	}
}

// The inventory is the thing every other check is made against, so a corrupted inventory would
// be a way to make a damaged working copy verify — and, worse, a way to wedge an appliance into
// re-hydrating on every start forever.
func TestAMutatedInventoryIsReplacedRatherThanBelieved(t *testing.T) {
	s, a, dest, _ := hydrated(t)

	inv, err := s.inventoryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	mutateObject(t, s, inv)

	if err := s.Check(a, dest, Structure); err == nil {
		t.Fatal("a working copy verified against an inventory that does not verify")
	}
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared); err != nil {
		t.Fatalf("a corrupted inventory was not recovered from: %v", err)
	}
	if err := s.CAS().Verify(inv); err != nil {
		t.Errorf("the corrupted inventory is still in the store: %v", err)
	}
	mustVerify(t, s, a, dest)
}

// --- 4. the golden tree -------------------------------------------------------------------------

// The tree is the SOURCE of every working directory, so a damaged tree does not produce a
// detectable failure — it produces a faithful copy of a broken artifact, in every working
// directory, until somebody deletes it.
func TestADamagedGoldenTreeIsNotClonedIntoAWorkingCopy(t *testing.T) {
	s, a, _, hits := hydrated(t)

	tree := filepath.Join(s.Root(), "trees", a.Digest)
	if err := os.Chmod(filepath.Join(tree, "README"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tree, "README")); err != nil {
		t.Fatal(err)
	}

	before := atomic.LoadInt64(hits)
	dest := filepath.Join(t.TempDir(), "from-damaged-tree")
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared); err != nil {
		t.Fatalf("hydrating from a damaged tree failed instead of rebuilding it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README")); err != nil {
		t.Errorf("the working copy inherited the tree's damage: %v", err)
	}
	if n := atomic.LoadInt64(hits) - before; n != 0 {
		t.Errorf("rebuilding a tree went to the network %d time(s); the object was in the store", n)
	}
	mustVerify(t, s, a, dest)
}

// --- 5. adoption --------------------------------------------------------------------------------

// The first start after this verification ships finds working directories that are complete and
// unvouched-for. Rebuilding 450 MB to discover that 450 MB was already correct is not a safety
// property, it is an upgrade nobody would forgive.
func TestACompleteCopyWithNoReceiptIsAdoptedRatherThanRebuilt(t *testing.T) {
	body, srv, hits := servedArtifact(t)
	defer srv.Close()
	s := newStore(t)
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	dest := filepath.Join(t.TempDir(), "work")

	// Hydrate, not EnsureHydrated: a working directory exactly as the binary before issue 15
	// would have left it — whole, and with nothing vouching for it.
	if _, err := s.Hydrate(context.Background(), a, dest, cas.Shared); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(receiptPath(dest)); err == nil {
		t.Fatal("Hydrate wrote a receipt, so this test is not testing adoption")
	}
	was := inodeOf(t, filepath.Join(dest, "bin", "node"))

	before := atomic.LoadInt64(hits)
	res, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatalf("a complete unreceipted working directory was rejected: %v", err)
	}
	if !res.Adopted {
		t.Errorf("the working directory was rebuilt rather than adopted: %+v", res)
	}
	if res.Repaired {
		t.Error("adopting a complete copy was reported as a repair")
	}
	if now := inodeOf(t, filepath.Join(dest, "bin", "node")); now != was {
		t.Errorf("the file was rewritten (inode %d -> %d); adoption must not copy anything", was, now)
	}
	if n := atomic.LoadInt64(hits) - before; n != 0 {
		t.Errorf("adoption fetched %d time(s)", n)
	}
	mustVerify(t, s, a, dest)

	// And the second start is now the cheap one: receipted, so no file is read at all.
	second, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Existing || second.Adopted || second.Repaired {
		t.Errorf("the start after an adoption did work it did not need to: %+v", second)
	}
}

// EVERYTHING NAMED MUST BE THERE; ANYTHING ELSE MAY BE. `kontra up` publishes the SPA as a
// symlink INSIDE the hydrated orchestrator, so a verifier that demanded "and nothing else"
// would call every appliance that serves its own UI damaged.
func TestExtraEntriesInAWorkingCopyAreNotDamage(t *testing.T) {
	s, a, dest, _ := hydrated(t)

	link := filepath.Join(dest, "orchestrator", "web", "dist")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "spa"), link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "operator-put-this-here"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Check(a, dest, Contents); err != nil {
		t.Errorf("a working copy with the SPA symlink in it was called damaged: %v", err)
	}
}

// --- 6. the last word ---------------------------------------------------------------------------

// Re-hydration is attempted once. When that fails too, the failure names the cause rather than
// leaving an operator with "hydration failed".
func TestTheFinalFailureNamesTheCause(t *testing.T) {
	body, srv, _ := servedArtifact(t)
	s := newStore(t)
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	dest := filepath.Join(t.TempDir(), "work")
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared); err != nil {
		t.Fatal(err)
	}

	// The disk lost the working copy and the store, and the mirror is gone: nothing left to
	// hydrate from and nothing left to hydrate.
	srv.Close()
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.Root(), "trees", a.Digest)); err != nil {
		t.Fatal(err)
	}
	if err := s.CAS().Discard(a.Digest); err != nil {
		t.Fatal(err)
	}

	_, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
	if err == nil {
		t.Fatal("hydration succeeded with no source and no store")
	}
	var failed *RepairFailed
	if !errors.As(err, &failed) {
		t.Fatalf("the failure is not a RepairFailed, so the retry did not happen: %T %v", err, err)
	}
	for _, want := range []string{"pkg", dest, "connect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the final failure does not mention %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "hydrated again") {
		t.Errorf("the final failure does not say a re-hydration was tried: %v", err)
	}
}

// Two different causes are two different facts, and either one alone is a story an operator
// cannot act on.
func TestRepairFailedNamesBothCausesWhenTheyDiffer(t *testing.T) {
	both := (&RepairFailed{
		Artifact: "pkg", Dest: "/data/pkg",
		Found: errors.New("the working copy is missing bin/node"),
		Then:  errors.New("HTTP 404 Not Found"),
	}).Error()
	for _, want := range []string{"missing bin/node", "404", "hydrated again and that failed too"} {
		if !strings.Contains(both, want) {
			t.Errorf("%q is not in %q", want, both)
		}
	}
	same := (&RepairFailed{Artifact: "pkg", Dest: "/data/pkg", Found: errors.New("boom"), Then: errors.New("boom")}).Error()
	if strings.Count(same, "boom") != 1 {
		t.Errorf("one cause was printed twice: %q", same)
	}
}

// --- 7. a single-file artifact --------------------------------------------------------------------

// A file artifact leaves the store WITHOUT being read — a reflink shares extents, a hardlink
// shares the inode — so it is the one kind whose object nothing would ever notice had changed.
func TestASingleFileArtifactIsVerifiedOnItsWayOut(t *testing.T) {
	body := []byte(strings.Repeat("#!/bin/sh\necho hello\n", 512))
	srv, hits := serving(map[string][]byte{"/tool": body})
	defer srv.Close()

	s := newStore(t)
	a := Artifact{Name: "tool", URL: srv.URL + "/tool", Digest: cas.Sha256Hex(body), Kind: KindFile, Executable: true}
	dest := filepath.Join(t.TempDir(), "bin", "tool")
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Writable); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(a, dest, Contents); err != nil {
		t.Fatalf("a freshly hydrated file does not verify: %v", err)
	}

	// Mutate the STORE and ask for a second working copy: nothing reads the object on the way
	// out, so only an explicit verification catches this.
	mutateObject(t, s, a.Digest)
	before := atomic.LoadInt64(hits)
	second := filepath.Join(t.TempDir(), "bin", "tool")
	if _, err := s.EnsureHydrated(context.Background(), a, second, cas.Writable); err != nil {
		t.Fatalf("a mutated file object was not recovered from: %v", err)
	}
	if n := atomic.LoadInt64(hits) - before; n != 1 {
		t.Errorf("fetched %d time(s) after the object was mutated, want 1", n)
	}
	if err := s.Check(a, second, Contents); err != nil {
		t.Errorf("the second working copy is not the artifact: %v", err)
	}

	// And a damaged working copy is repaired like any other.
	if err := os.WriteFile(second, []byte("replaced"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := s.EnsureHydrated(context.Background(), a, second, cas.Writable)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Repaired {
		t.Error("a replaced single-file working copy was not repaired")
	}
	if err := s.Check(a, second, Contents); err != nil {
		t.Errorf("the repaired file is not the artifact: %v", err)
	}
}

// --- 8. a concurrent racing hydration -------------------------------------------------------------

// Repair is a delete and a rebuild, and the delete is why this test exists: several `kontra up`
// invocations arriving at one damaged working directory must not tear it out from under each
// other. Every one of them has to come back with a directory that verifies.
func TestConcurrentRepairsOfOneWorkingDirectory(t *testing.T) {
	s, a, dest, _ := hydrated(t)
	if err := os.RemoveAll(filepath.Join(dest, "lib")); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	repaired := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			res, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared)
			errs[i], repaired[i] = err, res.Repaired
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racing hydration %d failed: %v", i, err)
		}
	}
	if err := s.Check(a, dest, Contents); err != nil {
		t.Errorf("the working directory is not the artifact after %d racing repairs: %v", n, err)
	}
	fixed := 0
	for _, r := range repaired {
		if r {
			fixed++
		}
	}
	t.Logf("%d racing hydrations of one damaged directory, %d of them rebuilt it", n, fixed)
	assertClean(t, s)
}

// The same race between separate PROCESSES, which is what two `kontra up` invocations actually
// are. A goroutine race shares a heap and could be made safe by something in memory; this can
// only be made safe by what happens on the filesystem.
func TestConcurrentVerifiedHydrationAcrossProcesses(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		t.Skip("running as a child")
	}
	gz := tarGz(t, []tarEntry{
		{name: "top/bin/node", body: strings.Repeat("payload ", 1<<15), mode: 0o755},
		{name: "top/README", body: "readme", mode: 0o644},
	})
	digest := cas.Sha256Hex(gz)
	srv, hits := serving(map[string][]byte{"/pkg.tar.gz": gz})
	defer srv.Close()

	root, work := t.TempDir(), t.TempDir()
	dest := filepath.Join(work, "shared")
	const n = 6
	var wg sync.WaitGroup
	out := make([]string, n)
	fail := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b, err := childHydration(root, dest, srv.URL+"/pkg.tar.gz", digest)
			out[i], fail[i] = b, err
		}(i)
	}
	wg.Wait()
	for i := range fail {
		if fail[i] != nil {
			t.Errorf("child %d failed: %v\n%s", i, fail[i], out[i])
		}
	}
	t.Logf("%d racing processes into one working directory, %d download(s)", n, atomic.LoadInt64(hits))

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: digest, Kind: KindTarGz, StripComponents: 1}
	if err := s.Check(a, dest, Contents); err != nil {
		t.Errorf("the shared working directory is not the artifact after %d racing processes: %v", n, err)
	}
	assertClean(t, s)
}

// TestVerifiedHydrationChildProcess is not a test. It is one `kontra up`.
func TestVerifiedHydrationChildProcess(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("helper process for TestConcurrentVerifiedHydrationAcrossProcesses")
	}
	s, err := Open(os.Getenv(childRoot))
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.EnsureHydrated(context.Background(), Artifact{
		Name: "pkg", URL: os.Getenv(childURL), Digest: os.Getenv(childDigest),
		Kind: KindTarGz, StripComponents: 1,
	}, os.Getenv(childDest), cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified && !res.Existing {
		t.Fatalf("a hydration returned without verifying anything: %+v", res)
	}
	fmt.Printf("hydrated pid=%d fetched=%v existing=%v repaired=%v adopted=%v\n",
		os.Getpid(), res.Fetched, res.Existing, res.Repaired, res.Adopted)
}

// --- helpers --------------------------------------------------------------------------------------

// hydrated is the starting point most of these tests damage: a store, an artifact, a verified
// working directory, and the counter that says whether anything went back to the network.
func hydrated(t *testing.T) (*Store, Artifact, string, *int64) {
	t.Helper()
	body, srv, hits := servedArtifact(t)
	t.Cleanup(srv.Close)
	s := newStore(t)
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	dest := filepath.Join(t.TempDir(), "work")
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared); err != nil {
		t.Fatal(err)
	}
	return s, a, dest, hits
}

// servedArtifact is a bundle-shaped archive: a manifest first (so that "the manifest is there"
// keeps being true for a partial copy), an executable, a symlink and a few nested files.
func servedArtifact(t *testing.T) ([]byte, *httptest.Server, *int64) {
	t.Helper()
	body := tarGz(t, []tarEntry{
		{name: "pkg/manifest.json", body: `{"schema":"kontra.appliance.bundle/v1"}`, mode: 0o644},
		{name: "pkg/bin/node", body: strings.Repeat("ELF", 4096), mode: 0o755},
		{name: "pkg/README", body: "readme", mode: 0o644},
		{name: "pkg/lib/node_modules/deep/index.js", body: "module.exports = 1\n", mode: 0o644},
		{name: "pkg/lib/node_modules/deep/data.json", body: `{"a":1}`, mode: 0o644},
		{name: "pkg/bin/nodejs", link: "node"},
	})
	srv, hits := serving(map[string][]byte{"/pkg.tar.gz": body})
	return body, srv, hits
}

func mustVerify(t *testing.T, s *Store, a Artifact, dest string) {
	t.Helper()
	if err := s.Check(a, dest, Contents); err != nil {
		t.Errorf("%s at %s is not the artifact: %v", a.Name, dest, err)
	}
}

// mutateObject changes a stored object WITHOUT changing its length, which is the shape that
// defeats every check short of reading it: the address is taken, the file is regular, and a
// stat says exactly what it said before.
func mutateObject(t *testing.T, s *Store, digest string) {
	t.Helper()
	p, err := s.CAS().Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tampered"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("the mutation changed the object's length, which is not the case under test")
	}
}

// truncatingOnce serves half the body on the first request, with a Content-Length that promises
// all of it, and the whole thing afterwards. An interrupted transfer, exactly.
func truncatingOnce(body []byte) (*httptest.Server, *int64) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if n == 1 {
			_, _ = w.Write(body[:len(body)/2])
			return
		}
		_, _ = w.Write(body)
	}))
	return srv, &hits
}

// childHydration runs this same test binary as a REAL second process, pointed at the helper
// above. It is the only way to prove that what makes concurrent repair safe is the rename and
// the atomic publish rather than something that happens to be shared in one heap.
func childHydration(root, dest, url, digest string) (string, error) {
	out := &strings.Builder{}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVerifiedHydrationChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(),
		childEnv+"=1",
		childURL+"="+url,
		childDigest+"="+digest,
		childRoot+"="+root,
		childDest+"="+dest,
	)
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	return out.String(), err
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode on this platform")
	}
	return sys.Ino
}
