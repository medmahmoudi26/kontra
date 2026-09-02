package hydrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/medmahmoudi26/kontra-local/handler/internal/cas"
)

// AN ARTIFACT THIS MACHINE ALREADY HAS is the appliance's own bundle: `kontra bundle
// orchestrator` writes a tar.gz into the checkout, and issue 17 is what will give those bytes a
// release URL. Everything the digest promises is unchanged — the file is streamed through the
// same hash into the same store — so this is a second SOURCE, not a second rule.
func TestHydrateAFileURLArtifact(t *testing.T) {
	body := tarGz(t, []tarEntry{
		{name: "pkg/bin/tool", body: "#!/bin/sh\n", mode: 0o755},
		{name: "pkg/lib/data.json", body: `{"a":1}`, mode: 0o644},
	})
	src := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	url, err := FileURL(src)
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	a := Artifact{Name: "bundle", URL: url, Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1}
	dest := filepath.Join(t.TempDir(), "work")

	res, err := s.Hydrate(context.Background(), a, dest, cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fetched {
		t.Error("the first hydration did not read the file")
	}
	if got, err := os.ReadFile(filepath.Join(dest, "lib", "data.json")); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("the working directory is wrong (err=%v, got=%q)", err, got)
	}

	// The store holds it now, so a second working directory costs no read of the source at all —
	// which is what makes a second `kontra up` free. Proven by deleting the source first.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	second, err := s.Hydrate(context.Background(), a, filepath.Join(t.TempDir(), "work2"), cas.Shared)
	if err != nil {
		t.Fatalf("second hydration after the source was removed: %v", err)
	}
	if second.Fetched {
		t.Error("the second hydration went back to the source")
	}
	assertClean(t, s)
}

// A file that is not what its pin says is refused before anything can run it, exactly like a
// substituted download. The source being local is not a reason to trust it: a stale artifact
// beside a stale sidecar is the ordinary way this goes wrong.
func TestHydrateRefusesALocalFileThatIsNotItsDigest(t *testing.T) {
	src := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(src, []byte("not the pinned bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	url, err := FileURL(src)
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	_, err = s.Hydrate(context.Background(), Artifact{
		Name: "bundle", URL: url, Digest: cas.Sha256Hex([]byte("something else")), Kind: KindTarGz,
	}, filepath.Join(t.TempDir(), "work"), cas.Shared)
	if err == nil {
		t.Fatal("a local file that does not match its pin was hydrated")
	}
	assertClean(t, s)
}

// A relative `file:` URL would be resolved against whatever directory the process happened to be
// started in, which is the one thing a pin must never be a function of.
func TestValidateRefusesARelativeOrRemoteFileURL(t *testing.T) {
	digest := cas.Sha256Hex([]byte("x"))
	for _, tc := range []struct{ name, url, want string }{
		{"relative", "file:build/bundle.tar.gz", "absolute"},
		{"host", "file://elsewhere/bundle.tar.gz", "local to this machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Artifact{Name: "bundle", URL: tc.url, Digest: digest, Kind: KindTarGz}.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.url)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not explain itself: %v", err)
			}
		})
	}
}

// FileURL exists so callers stop pasting "file://" in front of a path that might be relative —
// which parses, hydrates the wrong file, and then blames the digest.
func TestFileURLIsAbsolute(t *testing.T) {
	got, err := FileURL("relative/path.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "file:///") {
		t.Errorf("FileURL produced %q, which is not an absolute file URL", got)
	}
	if err := (Artifact{Name: "b", URL: got, Digest: cas.Sha256Hex(nil), Kind: KindTarGz}).Validate(); err != nil {
		t.Errorf("FileURL produced a URL Validate refuses: %v", err)
	}
}

// A FIRST RUN IS NOT INSTANT AND MUST SAY SO. 125 MB into the store, 457 MB expanded over 31,823
// files: the phases are only visible from in here, and a binary that prints nothing for thirty
// seconds is indistinguishable from a hung one.
func TestProgressReportsEveryPhase(t *testing.T) {
	body := tarGz(t, []tarEntry{
		{name: "pkg/a", body: strings.Repeat("a", 4096), mode: 0o644},
		{name: "pkg/b", body: strings.Repeat("b", 4096), mode: 0o644},
	})
	src := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	url, err := FileURL(src)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seen := map[Phase]Progress{}
	s := newStore(t)
	s.SetProgress(func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		seen[p.Phase] = p
	})

	if _, err := s.Hydrate(context.Background(), Artifact{
		Name: "bundle", URL: url, Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1,
	}, filepath.Join(t.TempDir(), "work"), cas.Shared); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, phase := range []Phase{PhaseFetch, PhaseExpand, PhaseMaterialize} {
		p, ok := seen[phase]
		if !ok {
			t.Errorf("nothing was reported for the %s phase", phase)
			continue
		}
		if p.Artifact != "bundle" {
			t.Errorf("%s reported the wrong artifact: %q", phase, p.Artifact)
		}
		if p.Bytes == 0 {
			t.Errorf("%s reported no progress at all: %+v", phase, p)
		}
	}
	if got := seen[PhaseFetch].Total; got != int64(len(body)) {
		t.Errorf("the fetch phase does not know the size of a local file: %d, want %d", got, len(body))
	}
	if got := seen[PhaseMaterialize].Files; got != 2 {
		t.Errorf("the materialize phase counted %d files, want 2", got)
	}
}

// Silence is the default and must stay free: every existing caller passes no reporter, and a
// hydration that pays for progress nobody asked for is a cost with no reader.
func TestProgressIsOptional(t *testing.T) {
	body := tarGz(t, []tarEntry{{name: "pkg/a", body: "a", mode: 0o644}})
	src := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	url, err := FileURL(src)
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	if _, err := s.Hydrate(context.Background(), Artifact{
		Name: "bundle", URL: url, Digest: cas.Sha256Hex(body), Kind: KindTarGz, StripComponents: 1,
	}, filepath.Join(t.TempDir(), "work"), cas.Shared); err != nil {
		t.Fatal(err)
	}
}
