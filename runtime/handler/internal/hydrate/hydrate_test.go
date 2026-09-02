package hydrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
)

func TestHydrateAFileArtifact(t *testing.T) {
	payload := []byte("#!/bin/sh\necho a pinned single-file artifact\n")
	srv, hits := serving(map[string][]byte{"/tool": payload})
	defer srv.Close()

	s := newStore(t)
	a := Artifact{
		Name: "tool", URL: srv.URL + "/tool", Digest: cas.Sha256Hex(payload),
		Kind: KindFile, Size: int64(len(payload)), Executable: true,
	}
	dest := filepath.Join(t.TempDir(), "bin", "tool")

	res, err := s.Hydrate(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fetched {
		t.Error("first hydration did not fetch")
	}
	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("working copy wrong (err=%v)", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("executable artifact materialized %v", st.Mode().Perm())
	}
	if res.Method == cas.MethodHardlink {
		t.Error("an executable artifact must not share the stored object's inode; chmod would reach the store")
	}

	// Second install: the store already has it, and the second working copy is independent.
	dest2 := filepath.Join(t.TempDir(), "bin", "tool")
	res2, err := s.Hydrate(context.Background(), a, dest2, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Fetched {
		t.Error("second hydration went back to the network for bytes it already had")
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}

	// Third: dest exists, so it is left alone.
	res3, err := s.Hydrate(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if !res3.Existing {
		t.Error("re-hydrating over an existing working copy did not report Existing")
	}
}

// TestHydrateRefusesADigestMismatch: the mirror serves something else. Nothing is stored,
// nothing is materialized, and the message names both sides.
func TestHydrateRefusesADigestMismatch(t *testing.T) {
	served := []byte("what the mirror actually has")
	srv, _ := serving(map[string][]byte{"/artifact.tar.gz": served})
	defer srv.Close()

	s := newStore(t)
	want := cas.Sha256Hex([]byte("what the pin says"))
	a := Artifact{Name: "swapped", URL: srv.URL + "/artifact.tar.gz", Digest: want, Kind: KindTarGz}
	dest := filepath.Join(t.TempDir(), "work")

	_, err := s.Hydrate(context.Background(), a, dest, cas.Shared)
	if err == nil {
		t.Fatal("hydrated an artifact that did not match its pin")
	}
	var mismatch *cas.DigestMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is %T (%v), want *cas.DigestMismatch", err, err)
	}
	for _, must := range []string{want, cas.Sha256Hex(served), srv.URL} {
		if !strings.Contains(err.Error(), must) {
			t.Errorf("error %q does not name %q", err, must)
		}
	}
	if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a working copy exists after a refused hydration (err=%v)", err)
	}
	if has, _ := s.CAS().Has(cas.Sha256Hex(served)); has {
		t.Error("the wrong bytes were stored under their own digest")
	}
	assertClean(t, s)
}

func TestHydrateATree(t *testing.T) {
	gz := tarGz(t, []tarEntry{
		{name: "pkg/bin/node", body: "ELF-ish", mode: 0o755},
		{name: "pkg/bin/npm", link: "../lib/npm-cli.js"},
		{name: "pkg/lib/npm-cli.js", body: "console.log(1)", mode: 0o644},
		{name: "pkg/README.md", body: "readme", mode: 0o644},
	})
	srv, _ := serving(map[string][]byte{"/pkg.tar.gz": gz})
	defer srv.Close()

	s := newStore(t)
	a := Artifact{
		Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(gz),
		Kind: KindTarGz, StripComponents: 1,
	}

	root := t.TempDir()
	first := filepath.Join(root, "install-a")
	res, err := s.Hydrate(context.Background(), a, first, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d files, %d bytes, weakest rung %s", res.Files, res.Bytes, res.Method)
	if res.Files != 3 {
		t.Errorf("materialized %d regular files, want 3", res.Files)
	}
	// StripComponents removed "pkg/", so the layout starts at bin/.
	if st, err := os.Stat(filepath.Join(first, "bin", "node")); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/node lost its execute bit: %v", st.Mode().Perm())
	}
	link, err := os.Readlink(filepath.Join(first, "bin", "npm"))
	if err != nil || link != "../lib/npm-cli.js" {
		t.Errorf("symlink = %q (err=%v)", link, err)
	}

	// A second install of the same artifact: no fetch, and where the filesystem allows it,
	// no bytes either.
	second := filepath.Join(root, "install-b")
	res2, err := s.Hydrate(context.Background(), a, second, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Fetched {
		t.Error("second install fetched again")
	}
	if res2.Method == cas.MethodHardlink || res2.Method == cas.MethodReflink {
		golden := filepath.Join(s.Root(), "trees", a.Digest, "README.md")
		gi, _ := os.Stat(golden)
		si, _ := os.Stat(filepath.Join(second, "README.md"))
		if res2.Method == cas.MethodHardlink && !os.SameFile(gi, si) {
			t.Error("hardlink reported but the inode differs")
		}
	}

	// A modified working copy must leave the golden tree alone. Writable, because Shared is
	// a promise not to do this.
	third := filepath.Join(root, "install-c")
	if _, err := s.Hydrate(context.Background(), a, third, cas.Writable); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(third, "README.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join(s.Root(), "trees", a.Digest, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(golden) != "readme" {
		t.Fatalf("the golden tree changed to %q when a working copy was written", golden)
	}
	// And the object the tree came from still verifies byte for byte.
	if _, err := s.CAS().ReadVerified(a.Digest, "artifact"); err != nil {
		t.Fatalf("the stored archive no longer verifies: %v", err)
	}
	assertClean(t, s)
}

func TestExpansionRefusesAnEscapingEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{"traversal", []tarEntry{{name: "../escaped", body: "x", mode: 0o644}}, "escapes"},
		{"absolute-symlink", []tarEntry{{name: "link", link: "/etc/passwd"}}, "leaves the destination"},
		{"traversing-symlink", []tarEntry{{name: "a/link", link: "../../../etc/passwd"}}, "leaves the destination"},
		{"escaping-hardlink", []tarEntry{{name: "hl", linkTo: "../../etc/passwd"}}, "leaves the destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gz := tarGz(t, tc.entries)
			srv, _ := serving(map[string][]byte{"/a.tar.gz": gz})
			defer srv.Close()
			s := newStore(t)
			a := Artifact{Name: "hostile", URL: srv.URL + "/a.tar.gz", Digest: cas.Sha256Hex(gz), Kind: KindTarGz}
			_, err := s.Hydrate(context.Background(), a, filepath.Join(t.TempDir(), "w"), cas.Shared)
			if err == nil {
				t.Fatal("expanded an archive that writes outside its destination")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(s.Root(), "trees", a.Digest)); !errors.Is(err, fs.ErrNotExist) {
				t.Error("a refused expansion was published anyway")
			}
		})
	}
}

func TestExpansionRefusesADeviceNode(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()

	srv, _ := serving(map[string][]byte{"/a.tar.gz": buf.Bytes()})
	defer srv.Close()
	s := newStore(t)
	a := Artifact{Name: "devnode", URL: srv.URL + "/a.tar.gz", Digest: cas.Sha256Hex(buf.Bytes()), Kind: KindTarGz}
	_, err := s.Hydrate(context.Background(), a, filepath.Join(t.TempDir(), "w"), cas.Shared)
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("error = %v, want a refusal naming the entry type", err)
	}
}

func TestExpansionRefusesToFillTheDisk(t *testing.T) {
	gz := tarGz(t, []tarEntry{{name: "big", body: strings.Repeat("a", 4096), mode: 0o644}})
	srv, _ := serving(map[string][]byte{"/a.tar.gz": gz})
	defer srv.Close()
	s := newStore(t)
	a := Artifact{Name: "bomb", URL: srv.URL + "/a.tar.gz", Digest: cas.Sha256Hex(gz), Kind: KindTarGz, MaxExpanded: 1024}
	_, err := s.Hydrate(context.Background(), a, filepath.Join(t.TempDir(), "w"), cas.Shared)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error = %v, want a refusal naming the expansion cap", err)
	}
}

func TestValidateRefusesAnUnpinnedOrPlaintextURL(t *testing.T) {
	base := Artifact{Name: "x", Digest: cas.Sha256Hex(nil), Kind: KindFile}
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/bufbuild/buf/releases/latest/download/buf-Linux-x86_64", "not pinned"},
		{"http://mirror.example.com/a.tar.gz", "loopback"},
		{"ftp://mirror.example.com/a.tar.gz", "not https"},
		{"://nonsense", "unparseable"},
	} {
		a := base
		a.URL = tc.url
		err := a.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%q) = %v, want an error containing %q", tc.url, err, tc.want)
		}
	}
	// And the pinned form of the same fetch is fine.
	ok := base
	ok.URL = "https://github.com/bufbuild/buf/releases/download/v1.47.2/buf-Linux-x86_64"
	if err := ok.Validate(); err != nil {
		t.Errorf("a pinned https URL was refused: %v", err)
	}
}

func TestFetchNamesAnHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	s := newStore(t)
	a := Artifact{Name: "missing", URL: srv.URL + "/a.tar.gz", Digest: cas.Sha256Hex(nil), Kind: KindTarGz}
	_, err := s.Fetch(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error = %v, want one naming the status and the URL", err)
	}
}

// TestConcurrentHydrationInOneProcess races goroutines. The cross-process case — two
// `kontra up` invocations — is TestConcurrentHydrationAcrossProcesses.
func TestConcurrentHydrationInOneProcess(t *testing.T) {
	gz := tarGz(t, []tarEntry{
		{name: "top/bin/node", body: strings.Repeat("payload ", 4096), mode: 0o755},
		{name: "top/README", body: "readme", mode: 0o644},
	})
	srv, hits := serving(map[string][]byte{"/pkg.tar.gz": gz})
	defer srv.Close()

	s := newStore(t)
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: cas.Sha256Hex(gz), Kind: KindTarGz, StripComponents: 1}
	root := t.TempDir()

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.Hydrate(context.Background(), a, filepath.Join(root, fmt.Sprintf("w%d", i)), cas.Shared)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
	}
	t.Logf("%d concurrent hydrations, %d download(s)", n, atomic.LoadInt64(hits))
	assertOneCorrectStore(t, s, a, root, n)
}

// --- helpers ---

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func serving(files map[string][]byte) (*httptest.Server, *int64) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	return srv, &hits
}

type tarEntry struct {
	name   string
	body   string
	link   string // symlink target
	linkTo string // hardlink target
	mode   int64
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		var hdr *tar.Header
		switch {
		case e.link != "":
			hdr = &tar.Header{Name: e.name, Typeflag: tar.TypeSymlink, Linkname: e.link, Mode: 0o777}
		case e.linkTo != "":
			hdr = &tar.Header{Name: e.name, Typeflag: tar.TypeLink, Linkname: e.linkTo, Mode: 0o644}
		default:
			hdr = &tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: e.mode, Size: int64(len(e.body))}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// assertClean: no debris. A store that works but leaves a temporary per attempt fills a
// disk on a machine that retries.
func assertClean(t *testing.T, s *Store) {
	t.Helper()
	for _, dir := range []string{filepath.Join(s.Root(), "tmp"), filepath.Join(s.Root(), "trees")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "put-") {
				t.Errorf("debris left in %s: %s", dir, e.Name())
			}
		}
	}
}

// assertOneCorrectStore is the shared assertion for both concurrency tests: one object, one
// golden tree, every working directory complete, and nothing half-written anywhere.
func assertOneCorrectStore(t *testing.T, s *Store, a Artifact, root string, n int) {
	t.Helper()

	objects := 0
	err := filepath.WalkDir(filepath.Join(s.Root(), "cas"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			objects++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// TWO OBJECTS, AND THE SECOND ONE IS THE POINT OF ISSUE 15: the artifact, and the
	// inventory of what it expands to. Both are content-addressed, so six racing processes
	// that each build the inventory independently still store exactly one of it — a listing
	// that differed between two expansions of one archive would be a second address for one
	// fact, and the store would hold two answers to "what does complete mean".
	if objects != 2 {
		t.Errorf("store holds %d objects after a race, want 2 (the artifact and its inventory)", objects)
	}
	if _, err := s.CAS().ReadVerified(a.Digest, "artifact"); err != nil {
		t.Errorf("the stored object does not verify after a race: %v", err)
	}

	trees, err := os.ReadDir(filepath.Join(s.Root(), "trees"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(trees))
	for i, e := range trees {
		names[i] = e.Name()
	}
	sort.Strings(names)
	if want := []string{a.Digest, a.Digest + ".hydrated"}; !reflect.DeepEqual(names, want) {
		t.Errorf("trees directory holds %v, want exactly %v", names, want)
	}

	var reference []byte
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("w%d", i))
		got, err := os.ReadFile(filepath.Join(dir, "bin", "node"))
		if err != nil {
			t.Errorf("working directory %s is incomplete: %v", dir, err)
			continue
		}
		if reference == nil {
			reference = got
		} else if !bytes.Equal(reference, got) {
			t.Errorf("working directory %s differs from the others", dir)
		}
		if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
			t.Errorf("working directory %s is missing README: %v", dir, err)
		}
		// "Complete" now means something a test can actually assert: every entry of the
		// artifact's own inventory, with the right contents. Reading two files by name was
		// the strongest check available before there was a list of what should be there.
		if inv, err := s.inventoryFor(a); err != nil {
			t.Errorf("no inventory for %s after a race: %v", a.Name, err)
		} else if err := s.checkAgainst(a, dir, inv, Contents); err != nil {
			t.Errorf("working directory %s does not match the artifact: %v", dir, err)
		}
	}
	assertClean(t, s)
}
