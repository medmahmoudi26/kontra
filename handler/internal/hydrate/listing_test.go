package hydrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/handler/internal/cas"
)

// A member path may contain a space, a quote or a newline-shaped escape, and a listing that
// parsed one of those into a DIFFERENT path would produce a verifier that checks the wrong
// file — which is worse than no verifier, because it reports success.
func TestTheListingRoundTripsAwkwardPaths(t *testing.T) {
	l := &listing{}
	paths := []string{
		"bin/node",
		"lib/a file with spaces.js",
		`lib/quote".js`,
		`lib/back\slash.js`,
		"lib/héllo.js",
	}
	for _, p := range paths {
		l.addFile(p, cas.Sha256Hex([]byte(p)), int64(len(p)), strings.HasPrefix(p, "bin/"))
	}
	l.addSymlink("bin/node js", "node")
	l.addDir("lib/empty dir")

	got, err := parseListing(l.encode())
	if err != nil {
		t.Fatalf("a listing this package wrote does not parse: %v", err)
	}
	if got.files != len(paths) || got.symlinks != 1 || got.dirs != 1 {
		t.Fatalf("round trip lost entries: %d files, %d symlinks, %d dirs", got.files, got.symlinks, got.dirs)
	}
	for i, want := range l.entries {
		if got.entries[i] != want {
			t.Errorf("entry %d round-tripped as %+v, want %+v", i, got.entries[i], want)
		}
	}
}

// The bytes are the address, so two expansions of one archive have to produce the same listing
// object — otherwise the store holds two answers to "what does complete mean" and a working
// copy verified against one of them is unverified against the other.
func TestTheListingIsOrderedByPathNotByArchiveOrder(t *testing.T) {
	forwards, backwards := &listing{}, &listing{}
	names := []string{"z", "a", "m", "b"}
	for _, n := range names {
		forwards.addFile(n, cas.Sha256Hex([]byte(n)), 1, false)
	}
	for i := len(names) - 1; i >= 0; i-- {
		backwards.addFile(names[i], cas.Sha256Hex([]byte(names[i])), 1, false)
	}
	if a, b := string(forwards.encode()), string(backwards.encode()); a != b {
		t.Errorf("the same contents in a different order encoded differently:\n%s\n---\n%s", a, b)
	}
}

// A listing whose format this binary does not know is REFUSED rather than parsed leniently. A
// lenient parse would silently shrink the set of things being checked, which is a verification
// that passes because it stopped looking.
func TestAnUnknownListingFormatIsRefused(t *testing.T) {
	for _, body := range []string{
		"",
		"kontra.hydration.listing/v2\nf " + cas.Sha256Hex(nil) + " 1 - \"a\"\n",
		listingSchema + "\nq \"a\"\n",
		listingSchema + "\nf notadigest 1 - \"a\"\n",
		listingSchema + "\nf " + cas.Sha256Hex(nil) + " notanumber - \"a\"\n",
		listingSchema + "\nf " + cas.Sha256Hex(nil) + " 1 - unquoted\n",
	} {
		if _, err := parseListing([]byte(body)); err == nil {
			t.Errorf("this parsed and should not have: %q", body)
		}
	}
}

// WHAT THE CHECK COSTS, MEASURED. Verification is no longer free — the second `kontra up` used
// to be one stat and is now one lstat per entry of the bundle's inventory — so the number
// belongs in the suite rather than in a comment nobody can re-run. The real artifact is 31,823
// files; this builds a tenth of that and reports both.
func TestWhatVerificationCosts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a few thousand files")
	}
	const n = 3000
	entries := make([]tarEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, tarEntry{
			name: fmt.Sprintf("pkg/lib/mod%03d/file%d.js", i%64, i),
			body: fmt.Sprintf("module.exports = %d\n", i),
			mode: 0o644,
		})
	}
	body := tarGz(t, entries)
	src := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	url, err := FileURL(src)
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	a := Artifact{Name: "pkg", URL: url, Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	dest := filepath.Join(t.TempDir(), "work")
	if _, err := s.EnsureHydrated(context.Background(), a, dest, cas.Shared); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := s.Check(a, dest, Structure); err != nil {
		t.Fatal(err)
	}
	structure := time.Since(start)

	start = time.Now()
	if err := s.Check(a, dest, Contents); err != nil {
		t.Fatal(err)
	}
	contents := time.Since(start)

	inv, err := s.inventoryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := s.CAS().ReadVerified(inv, "artifact listing")
	t.Logf("%d entries: structure %v (%.1f µs/entry), contents %v; inventory %d bytes (%d B/entry)",
		n+64, structure, float64(structure.Microseconds())/float64(n), contents, len(size), len(size)/(n+64))

	// Not a benchmark assertion, a regression guard: the structural check must stay in the
	// "one syscall per entry" class. A check that started reading files at Structure would
	// blow through this by orders of magnitude and would still pass every other test here.
	if structure > contents {
		t.Errorf("the structural check (%v) is not cheaper than reading everything (%v)", structure, contents)
	}
}
