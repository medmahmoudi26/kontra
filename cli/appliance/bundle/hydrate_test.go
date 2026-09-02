package bundle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/handler/casstore"
	"github.com/medmahmoudi26/kontra/runtime/handler/hydratestore"
)

// fakeBundle writes a real bundle — same deterministic tar writer, same manifest member, same
// sidecar — around whatever files a test wants in it. Real rather than mocked, because every
// property being tested here (the digest names the directory, the manifest is read from inside
// the archive, the sidecar is checked against the bytes) is a property of the FORMAT, and a stub
// would be a test of the stub.
func fakeBundle(t *testing.T, dir, name string, m *Manifest, files map[string]string) string {
	t.Helper()
	stage := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(stage, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, "/node") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := summarizeTree(stage)
	if err != nil {
		t.Fatal(err)
	}
	m.Tree = summary
	manifestBytes, err := encodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, ManifestName), manifestBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := writeDeterministicTarGz(stage, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(digest+"  "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func orchestratorManifest() *Manifest {
	return &Manifest{
		Schema:     SchemaID,
		Bundle:     "orchestrator",
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
		Entrypoint: []string{"node/bin/node", "orchestrator/dist/src/main.js"},
		Toolchain:  map[string]string{},
	}
}

func orchestratorFiles() map[string]string {
	return map[string]string{
		"node/bin/node":                 "#!/bin/sh\necho fake node\n",
		"orchestrator/dist/src/main.js": "console.log('hello');\n",
		"orchestrator/package.json":     `{"name":"orchestrator","version":"0.1.0"}`,
	}
}

func spaBundle(t *testing.T, dir, body string) string {
	t.Helper()
	return fakeBundle(t, dir, SPAName+".tar.gz", &Manifest{
		Schema:    SchemaID,
		Bundle:    "spa",
		Platform:  "any",
		Toolchain: map[string]string{},
	}, map[string]string{SPARootInBundle + "/index.html": body})
}

// THE TWO CRITERIA IN ONE TEST, because they are one mechanism: the working directory is named by
// the bundle's digest, so "already hydrated" is answered locally — one lstat per file of the
// bundle's own inventory since issue 15, and no read of the archive at all.
//
// THE ARCHIVE IS DELETED BETWEEN THE RUNS. That is the whole point — a second `kontra up` that
// re-read 125 MB to discover it had nothing to do would pass a weaker version of this test, and
// an operator would be paying for it on every start.
func TestHydrateOrchestratorRunsOnceAndSkipsAfterwards(t *testing.T) {
	data := t.TempDir()
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())

	first, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle})
	if err != nil {
		t.Fatalf("first hydration: %v", err)
	}
	if !first.Fresh {
		t.Error("the first hydration did not report itself as fresh")
	}
	if filepath.Base(first.Root) != first.Digest {
		t.Errorf("the working directory %s is not named by the digest %s", first.Root, first.Digest)
	}
	for _, p := range []string{first.Node, first.Entry} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the entrypoint %s is not there: %v", p, err)
		}
	}

	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	second, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle})
	if err != nil {
		t.Fatalf("second hydration (the archive is gone on purpose): %v", err)
	}
	if second.Fresh {
		t.Error("the second hydration did work; it must be a verification and nothing else")
	}
	if second.Root != first.Root || second.Entry != first.Entry {
		t.Errorf("the second run resolved somewhere else: %s vs %s", second.Root, first.Root)
	}
}

// THE FIRST START AFTER VERIFICATION SHIPPED. An appliance that was hydrated by a binary which
// wrote no receipts has a complete working directory and no evidence about it — and, if its
// operator cleaned up after the install, no 125 MB tarball either. Rebuilding is not an option
// (there is nothing to rebuild from) and neither is trusting it, so the copy is READ and checked
// against an inventory derived from the bytes already in the store, and then vouched for.
//
// The sidecar is what survives, because it is 65 bytes and the archive is 125 MB.
func TestHydrateAdoptsAWorkingDirectoryWhoseArchiveIsGone(t *testing.T) {
	data := t.TempDir()
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())

	first, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle})
	if err != nil {
		t.Fatalf("first hydration: %v", err)
	}

	// Exactly what a pre-issue-15 appliance looks like: the working directory, the store, and no
	// receipt anywhere.
	for _, p := range []string{first.Root, filepath.Join(data, "trees")} {
		matches, err := filepath.Glob(p + "*.hydrated")
		if err != nil {
			t.Fatal(err)
		}
		inner, err := filepath.Glob(filepath.Join(p, "*.hydrated"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range append(matches, inner...) {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}

	second, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle})
	if err != nil {
		t.Fatalf("an unreceipted working directory with no archive was not adopted: %v", err)
	}
	if second.Root != first.Root {
		t.Errorf("adoption resolved somewhere else: %s vs %s", second.Root, first.Root)
	}
	if second.Manifest == nil || second.Manifest.Schema != SchemaID {
		t.Errorf("the manifest was not read from the working directory: %+v", second.Manifest)
	}
	// And the run after it is the cheap one again.
	third, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle})
	if err != nil {
		t.Fatalf("third hydration: %v", err)
	}
	if third.Fresh || third.Repaired {
		t.Errorf("the start after an adoption did work it did not need to: %+v", third)
	}
}

// THE BUG THIS SLICE EXISTS FOR, AT THE SEAM WHERE IT WOULD HAVE BITTEN. A working directory that
// died partway through its clone still has manifest.json — the archive stages it first — and still
// has both halves of the entrypoint, so the two checks `kontra up` used to make both pass on
// exactly the artifact they most needed to refuse. The failure arrived later as a Node stack trace
// about a module path, thirty seconds and one bound port after the point where it could have been
// named.
func TestHydrateRefusesAHalfMaterializedWorkingDirectory(t *testing.T) {
	data := t.TempDir()
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())

	first, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle})
	if err != nil {
		t.Fatalf("first hydration: %v", err)
	}

	victim := filepath.Join(first.Root, "orchestrator", "package.json")
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}

	// The three checks that came before this slice, all still passing on a broken artifact.
	for _, p := range []string{first.Root, filepath.Join(first.Root, ManifestName), first.Node, first.Entry} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s is missing, so this is not the case under test: %v", p, err)
		}
	}

	var out strings.Builder
	second, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, Progress: &out})
	if err != nil {
		t.Fatalf("a half-materialized working directory was not repaired: %v", err)
	}
	if !second.Repaired {
		t.Fatal("a working directory missing a file was used as it stood")
	}
	if second.Damage == nil || !strings.Contains(second.Damage.Error(), "orchestrator/package.json") {
		t.Errorf("the repair does not name what was wrong: %v", second.Damage)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the repaired working directory is still missing the file: %v", err)
	}
	// A REPAIR IS NEVER SILENT: it means this disk lost part of an artifact, and an operator who
	// is not told will notice only that starts got slow.
	if !strings.Contains(out.String(), "hydrated again") {
		t.Errorf("the repair was not reported to the operator:\n%s", out.String())
	}
}

// The SPA gets the same check, and `stat index.html` was the old one. A browser bundle whose
// index.html arrived before the process died is exactly the artifact that passed it — and the
// appliance would then serve an index that 404s on every file it names.
func TestHydrateRefusesAHalfMaterializedSPA(t *testing.T) {
	data := t.TempDir()
	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())
	spa := fakeBundle(t, dir, SPAName+".tar.gz", &Manifest{
		Schema: SchemaID, Bundle: "spa", Platform: "any", Toolchain: map[string]string{},
	}, map[string]string{
		SPARootInBundle + "/index.html":       "<!doctype html><script src=/assets/app.js></script>",
		SPARootInBundle + "/assets/app.js":    "console.log(1)",
		SPARootInBundle + "/assets/style.css": "body{}",
	})

	first, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, SPA: spa})
	if err != nil {
		t.Fatalf("first hydration: %v", err)
	}
	if first.SPARoot == "" {
		t.Fatal("no SPA was hydrated")
	}
	victim := filepath.Join(first.SPARoot, "assets", "app.js")
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first.SPARoot, "index.html")); err != nil {
		t.Fatalf("index.html is gone, so this is not the case under test: %v", err)
	}

	second, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, SPA: spa})
	if err != nil {
		t.Fatalf("a half-materialized SPA was not repaired: %v", err)
	}
	if second.SPADigest != first.SPADigest {
		t.Errorf("the repaired SPA is a different artifact: %s vs %s", second.SPADigest, first.SPADigest)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the SPA is still missing the file its index.html names: %v", err)
	}
}

// A first run is not instant and must say so; a second one has nothing to say. A progress line
// printed on every start is noise, and noise is what makes the one line that mattered invisible.
func TestHydrateReportsProgressOnTheFirstRunOnly(t *testing.T) {
	data := t.TempDir()
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())

	var first bytes.Buffer
	if _, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, Progress: &first}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "hydrating the orchestrator") {
		t.Errorf("the first run said nothing about hydrating:\n%s", first.String())
	}
	if !strings.Contains(first.String(), "once per bundle") {
		t.Errorf("the first run does not say the cost is one-off:\n%s", first.String())
	}

	var second bytes.Buffer
	if _, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, Progress: &second}); err != nil {
		t.Fatal(err)
	}
	if second.Len() != 0 {
		t.Errorf("the second run printed progress for work it did not do:\n%s", second.String())
	}
}

// A bundle for another platform hydrates perfectly and then dies at its first `require`, on a
// native addon, with a message about an ELF header. Refuse it where the fix is obvious.
func TestHydrateRefusesABundleBuiltForAnotherPlatform(t *testing.T) {
	m := orchestratorManifest()
	m.Platform = "plan9/mips"
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", m, orchestratorFiles())

	_, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: bundle})
	if err == nil {
		t.Fatal("a foreign-platform bundle was hydrated")
	}
	for _, want := range []string{"plan9/mips", runtime.GOOS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// The schema is read before anything is spent. A future bundle shape must fail in a millisecond
// with the two versions in the message, not thirty seconds later inside a tar reader.
func TestHydrateRefusesAnUnknownSchema(t *testing.T) {
	m := orchestratorManifest()
	m.Schema = "kontra.appliance.bundle/v99"
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", m, orchestratorFiles())

	_, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: bundle})
	if err == nil || !strings.Contains(err.Error(), "v99") {
		t.Fatalf("an unknown schema was accepted or unnamed: %v", err)
	}
}

// THE SIDECAR IS A CLAIM, NOT AN AUTHORITY. It is read to avoid hashing 125 MB on every start —
// and then the bytes are hashed on their way into the store anyway, so a sidecar that does not
// describe its archive is caught rather than believed.
func TestHydrateRefusesASidecarThatDisagreesWithItsArchive(t *testing.T) {
	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())
	wrong := strings.Repeat("ab", 32)
	if err := os.WriteFile(bundle+".sha256", []byte(wrong+"  orchestrator.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: bundle})
	if err == nil {
		t.Fatal("a bundle whose sidecar lies about it was hydrated")
	}
	if !strings.Contains(err.Error(), wrong) {
		t.Errorf("the refusal does not name the digest that was expected: %v", err)
	}
}

// A manifest that names an entrypoint the archive does not contain must fail as that sentence,
// not as `node: cannot find module`.
func TestHydrateNamesAnEntrypointTheBundleDoesNotContain(t *testing.T) {
	files := orchestratorFiles()
	delete(files, "orchestrator/dist/src/main.js")
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", orchestratorManifest(), files)

	_, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: bundle})
	if err == nil {
		t.Fatal("a bundle with no entrypoint was accepted")
	}
	if !strings.Contains(err.Error(), "main.js") {
		t.Errorf("the refusal does not name the missing file: %v", err)
	}
}

// The SPA is a separate artifact with its own digest, and the orchestrator finds it through the
// symlink at the path its own walk-up looks for.
//
// THE SECOND HALF IS THE ONE THAT MATTERS. Rebuilding only the SPA must change what is served —
// a control plane that keeps serving the previous bundle while every surface looks correct is
// the stale-deploy failure this repo has already confirmed the wrong way round once.
func TestHydrateSPAIsPointedAtAndRepointedWhenItChanges(t *testing.T) {
	data := t.TempDir()
	bundleDir := t.TempDir()
	bundle := fakeBundle(t, bundleDir, "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())
	spa := spaBundle(t, t.TempDir(), "<html>first</html>")

	first, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, SPA: spa})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(first.Root, "orchestrator", "web", "dist")
	served, err := os.ReadFile(filepath.Join(link, "index.html"))
	if err != nil {
		t.Fatalf("the SPA is not served through %s: %v", link, err)
	}
	if string(served) != "<html>first</html>" {
		t.Errorf("the wrong SPA is at %s: %q", link, served)
	}
	if first.SPADigest == "" {
		t.Error("the hydration did not report which SPA it placed")
	}

	// A different SPA, same orchestrator bundle.
	rebuilt := spaBundle(t, t.TempDir(), "<html>second</html>")
	second, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: data, Bundle: bundle, SPA: rebuilt})
	if err != nil {
		t.Fatal(err)
	}
	if second.SPADigest == first.SPADigest {
		t.Fatal("two different SPAs hashed to one digest, so this test proves nothing")
	}
	served, err = os.ReadFile(filepath.Join(link, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(served) != "<html>second</html>" {
		t.Errorf("the previous SPA is still being served: %q", served)
	}
}

// Two archives of the same shape behind two flags: handing the orchestrator bundle to --spa must
// not hydrate 450 MB of Node into a static file root and serve it.
func TestHydrateRefusesTheWrongSubjectAsASPA(t *testing.T) {
	dir := t.TempDir()
	bundle := fakeBundle(t, dir, "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())

	_, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: bundle, SPA: bundle})
	if err == nil {
		t.Fatal("an orchestrator bundle was accepted as the SPA")
	}
	if !strings.Contains(err.Error(), "spa") {
		t.Errorf("the refusal does not say which subject was wanted: %v", err)
	}
}

// Without a SPA the control plane still starts — the API is the control plane and the pages are a
// surface on top of it — and the caller is left able to say so.
func TestHydrateWithoutASPAStillYieldsARunnableOrchestrator(t *testing.T) {
	bundle := fakeBundle(t, t.TempDir(), "orchestrator.tar.gz", orchestratorManifest(), orchestratorFiles())

	h, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if h.SPARoot != "" {
		t.Errorf("a SPA was reported where none was given: %q", h.SPARoot)
	}
	if h.Entry == "" {
		t.Error("no entrypoint was resolved")
	}
}

// A bundle that is not there fails with the path, not with a digest of nothing.
func TestHydrateNamesAMissingBundle(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nowhere.tar.gz")
	_, err := HydrateOrchestrator(context.Background(), HydrateOptions{DataDir: t.TempDir(), Bundle: missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("a missing bundle was not named: %v", err)
	}
}

// ONE STORE, TWO CUSTOMERS (ADR 0031 §2). The embedded registry keeps OCI layers in the CAS and
// hydration keeps the orchestrator bundle in it, and "do not build a second store" is an
// invariant about BYTES ON DISK — so the test is that something written through one handle is
// readable through the other, not that two constructors were called with the same string.
func TestOneStoreServesBothTheRegistryAndHydration(t *testing.T) {
	dir := t.TempDir()

	hydration, err := hydratestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := casstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}

	digest, _, err := registry.Put(strings.NewReader("an actor image layer"), "test")
	if err != nil {
		t.Fatal(err)
	}
	has, err := hydration.CAS().Has(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatalf("the hydration store cannot see a layer the registry stored; %s and %s are two stores",
			hydration.Root(), registry.Root())
	}
	if hydration.Root() != registry.Root() {
		t.Errorf("two roots: %s and %s", hydration.Root(), registry.Root())
	}
}
