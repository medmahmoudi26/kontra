package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCheckout is the smallest tree BuildOptions.resolve and BuildSPA will accept.
func fakeCheckout(t *testing.T, spa map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("control/orchestrator/package.json", `{"name":"orchestrator","version":"0.1.0"}`)
	write("frontend/package.json", `{"name":"kontra-web","version":"0.2.0"}`)
	for rel, body := range spa {
		write("frontend/dist/"+rel, body)
	}
	return root
}

// The SPA bundle is checkable by the same verifier as the orchestrator one, because it is the
// same shape. A second artifact format would be a second thing to verify, and the one that is
// verified less is the one that rots.
func TestSPABundleVerifiesAgainstItsOwnManifest(t *testing.T) {
	root := fakeCheckout(t, map[string]string{
		"index.html":            "<html><script src=/assets/app-abc123.js></script></html>",
		"assets/app-abc123.js":  "console.log('spa');",
		"assets/app-abc123.css": "body{}",
	})
	out := t.TempDir()

	res, err := BuildSPA(BuildOptions{RepoRoot: root, OutDir: out})
	if err != nil {
		t.Fatalf("BuildSPA: %v", err)
	}
	if filepath.Base(res.BundlePath) != SPAName+".tar.gz" {
		t.Errorf("the bundle is not named %s: %s", SPAName, res.BundlePath)
	}
	m, err := Verify(res.BundlePath)
	if err != nil {
		t.Fatalf("the freshly built SPA bundle does not match its own manifest: %v", err)
	}
	if m.Bundle != "spa" {
		t.Errorf("the manifest says the subject is %q", m.Bundle)
	}
	if m.Platform != "any" {
		t.Errorf("the SPA carries no native code and must not be pinned to a platform, got %q", m.Platform)
	}
	if len(m.Components) != 1 || m.Components[0].Path != SPARootInBundle {
		t.Errorf("the manifest does not name %s as its one component: %+v", SPARootInBundle, m.Components)
	}
	if m.Components[0].Version != "0.2.0" {
		t.Errorf("the manifest lost the package version: %q", m.Components[0].Version)
	}

	// The sidecar is what makes a second `kontra up` free, so it has to be there and it has to be
	// in the format `sha256sum -c` reads.
	sum, err := os.ReadFile(res.DigestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(sum), res.SHA256+"  ") {
		t.Errorf("the sidecar is not `<digest>  <name>`: %q", sum)
	}
}

// Two builds of the same input are the same bytes, or the digest describes nothing.
func TestSPABundleDigestIsStable(t *testing.T) {
	root := fakeCheckout(t, map[string]string{"index.html": "<html></html>", "assets/x.js": "1"})
	first, err := BuildSPA(BuildOptions{RepoRoot: root, OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildSPA(BuildOptions{RepoRoot: root, OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("two builds of one tree differ: %s vs %s", first.SHA256, second.SHA256)
	}
}

// It archives what a toolchain produced; it does not become the toolchain. The refusal has to
// carry the command, because "no built SPA" without one is a dead end for anybody who has not
// read this file.
func TestSPABundleRefusesWithTheBuildCommandWhenThereIsNothingToShip(t *testing.T) {
	root := fakeCheckout(t, nil)
	_, err := BuildSPA(BuildOptions{RepoRoot: root, OutDir: t.TempDir()})
	if err == nil {
		t.Fatal("a checkout with no built SPA produced a bundle")
	}
	// THE COMMAND, AND WHERE THE SOURCE WENT. `frontend/` is not in this repository any more
	// (ADR 0038), so a refusal naming only a build command sends the reader to a directory that
	// does not exist. It has to name the repository too.
	for _, want := range []string{"pnpm --dir ../kontra-console run build", "kontra-console"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say how to fix it (missing %q): %v", want, err)
		}
	}
	// AND WHERE IT LOOKED. Three candidates now, so "not found" without the list is a guess.
	if !strings.Contains(err.Error(), "Looked in:") {
		t.Errorf("the refusal does not say where it looked: %v", err)
	}
}
