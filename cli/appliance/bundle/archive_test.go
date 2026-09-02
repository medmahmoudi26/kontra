package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stageFixture builds a small tree with the awkward shapes a real bundle contains: a nested
// directory, an executable, a relative symlink and an empty file.
func stageFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "node", "bin"))
	mkdir(t, filepath.Join(root, "orchestrator", "dist", "src"))
	mkdir(t, filepath.Join(root, "orchestrator", "node_modules", ".bin"))

	write(t, filepath.Join(root, "node", "bin", "node"), "#!/bin/sh\necho v22.13.0\n", 0o755)
	write(t, filepath.Join(root, "orchestrator", "dist", "src", "main.js"), "console.log('hi')\n", 0o644)
	write(t, filepath.Join(root, "orchestrator", "package.json"), `{"name":"@kontra/orchestrator","version":"0.1.0"}`, 0o644)
	write(t, filepath.Join(root, "orchestrator", "node_modules", "empty"), "", 0o644)
	if err := os.Symlink("../typescript/bin/tsc", filepath.Join(root, "orchestrator", "node_modules", ".bin", "tsc")); err != nil {
		t.Fatal(err)
	}
	return root
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func archive(t *testing.T, root string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	digest, err := writeDeterministicTarGz(root, &buf)
	if err != nil {
		t.Fatalf("writeDeterministicTarGz: %v", err)
	}
	return digest, buf.Bytes()
}

// THE ACCEPTANCE CRITERION: "the bundle's own digest is stable across two builds from the same
// inputs on the same platform". The three inputs that make it unstable by default are all
// exercised here at once, because in a real build they change together: a rebuild rewrites files
// (new mtimes), may run under a different umask, and reads directories in whatever order the
// filesystem hands back.
func TestArchiveDigestSurvivesMtimeUmaskAndRewrite(t *testing.T) {
	root := stageFixture(t)
	first, firstBytes := archive(t, root)

	// Move every mtime a decade forward and rewrite one file with identical CONTENT.
	future := time.Date(2036, 6, 1, 12, 0, 0, 0, time.UTC)
	walk(t, root, func(p string) {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	})
	write(t, filepath.Join(root, "orchestrator", "dist", "src", "main.js"), "console.log('hi')\n", 0o600)

	second, secondBytes := archive(t, root)
	if first != second {
		t.Errorf("the same tree produced two digests:\n  %s\n  %s\n"+
			"  a content address that moves on its own describes nothing", first, second)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Errorf("the archives differ byte for byte (%d vs %d bytes)", len(firstBytes), len(secondBytes))
	}
}

// The other half of the same property: a digest that never moves is just as useless. One byte of
// CONTENT must move it.
func TestArchiveDigestMovesWhenContentDoes(t *testing.T) {
	root := stageFixture(t)
	before, _ := archive(t, root)
	write(t, filepath.Join(root, "orchestrator", "dist", "src", "main.js"), "console.log('hi!')\n", 0o644)
	after, _ := archive(t, root)
	if before == after {
		t.Error("changing a file did not change the bundle digest")
	}
}

// The executable bit is the ONE mode that has to survive normalisation: `node/bin/node` that
// arrives without it fails at exec, on the machine that hydrated it, as "permission denied" on a
// file that visibly exists.
func TestArchiveKeepsTheExecutableBitAndDropsTheRest(t *testing.T) {
	root := stageFixture(t)
	_, raw := archive(t, root)
	modes := map[string]int64{}
	links := map[string]string{}
	forEachHeader(t, raw, func(name string, mode int64, uid int, uname, linkname string) {
		modes[name] = mode
		if linkname != "" {
			links[name] = linkname
		}
		if uid != 0 || uname != "" {
			t.Errorf("%s carries an owner (uid %d, uname %q) from the machine that built it", name, uid, uname)
		}
	})
	if got := modes["node/bin/node"]; got != 0o755 {
		t.Errorf("node/bin/node is mode %o, want 755", got)
	}
	if got := modes["orchestrator/dist/src/main.js"]; got != 0o644 {
		t.Errorf("a plain file is mode %o, want 644 regardless of umask", got)
	}
	if got := links["orchestrator/node_modules/.bin/tsc"]; got != "../typescript/bin/tsc" {
		t.Errorf(".bin symlink target is %q; a bundle's symlinks must stay relative", got)
	}
}

// An absolute symlink is a machine-specific path that no CONTENT scan can see — it lives in the
// tar header, not in any file's bytes.
func TestAbsoluteSymlinkIsRefused(t *testing.T) {
	root := stageFixture(t)
	if err := os.Symlink("/usr/local/bin/node", filepath.Join(root, "node", "bin", "node-abs")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err := writeDeterministicTarGz(root, &buf)
	if err == nil {
		t.Fatal("an absolute symlink was archived")
	}
	if !strings.Contains(err.Error(), "/usr/local/bin/node") {
		t.Errorf("the message does not name the target it objects to: %v", err)
	}
}

func TestReadManifestNeedsAManifest(t *testing.T) {
	root := stageFixture(t)
	path := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, path)
	if _, err := ReadManifest(path); err == nil || !strings.Contains(err.Error(), ManifestName) {
		t.Errorf("a tarball with no manifest was accepted as a bundle: %v", err)
	}
}

// The manifest is only worth writing if something reads it back. A digest recorded once and never
// re-derived is documentation, and documentation of a native addon's provenance is exactly what
// goes stale without anyone noticing.
func TestVerifyBundleReportsBothDigestsOnAMismatch(t *testing.T) {
	root := stageFixture(t)
	m := &Manifest{
		Schema:   SchemaID,
		Bundle:   "orchestrator",
		Platform: "linux/amd64",
		NativeAddons: []NativeAddon{{
			Package: "@duckdb/node-bindings-linux-x64",
			Path:    "node/bin/node",
			SHA256:  strings.Repeat("00", 32), // deliberately not the file's digest
		}},
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, ManifestName), string(body), 0o644)

	path := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, path)

	_, err = Verify(path)
	if err == nil {
		t.Fatal("a manifest whose digest does not match the archive verified")
	}
	if !strings.Contains(err.Error(), "manifest:") || !strings.Contains(err.Error(), "archive:") {
		t.Errorf("the message does not show both digests, so it says a mismatch without saying which is which:\n%v", err)
	}
}

func TestVerifyBundleNoticesAMissingMember(t *testing.T) {
	root := stageFixture(t)
	m := &Manifest{
		Schema:       SchemaID,
		NativeAddons: []NativeAddon{{Package: "gone", Path: "orchestrator/node_modules/absent.node", SHA256: strings.Repeat("ab", 32)}},
	}
	body, _ := json.MarshalIndent(m, "", "  ")
	write(t, filepath.Join(root, ManifestName), string(body), 0o644)
	path := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, path)

	_, err := Verify(path)
	if err == nil || !strings.Contains(err.Error(), "does not contain it") {
		t.Errorf("a manifest naming an absent addon verified: %v", err)
	}
}

// A DIRECTORY COMPONENT WHOSE PATH IS WRONG, which is the shape ADR 0035's rename produced and the
// shape four platform jobs each burned five minutes on. The old code compared the empty tree's
// digest and reported two hashes, which reads as "the dependency tree changed" — the one diagnosis
// that sends you into a 31,825-file archive instead of at the one string that moved.
func TestVerifyBundleSaysAnEmptyTreeIsAWrongPathNotAChangedTree(t *testing.T) {
	root := stageFixture(t)
	m := &Manifest{
		Schema:   SchemaID,
		Platform: "linux/amd64",
		Components: []Component{{
			Name: "orchestrator-dependencies",
			Kind: "dependencies",
			// The repo's spelling since ADR 0035. The BUNDLE has never used it.
			Path:       "backend/node_modules",
			TreeSHA256: strings.Repeat("cd", 32),
		}},
	}
	body, _ := json.MarshalIndent(m, "", "  ")
	write(t, filepath.Join(root, ManifestName), string(body), 0o644)
	path := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, path)

	_, err := Verify(path)
	if err == nil {
		t.Fatal("a manifest naming a directory the archive does not have verified")
	}
	if !strings.Contains(err.Error(), "nothing under that path") {
		t.Errorf("the message blames the digest rather than the path:\n%v", err)
	}
	// The empty tree's digest must not appear: quoting it invites a reader to go looking for the
	// file whose content changed, and no file did.
	if strings.Contains(err.Error(), "e3b0c44298fc1c14") {
		t.Errorf("the message still shows the empty-tree digest, which is the misleading part:\n%v", err)
	}
}

func TestVerifyBundleAcceptsAConsistentBundle(t *testing.T) {
	root := stageFixture(t)
	nodeSum, err := sha256File(filepath.Join(root, "node", "bin", "node"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Schema:       SchemaID,
		Platform:     "linux/amd64",
		Components:   []Component{{Name: "node-runtime", Path: "node/bin/node", SHA256: nodeSum}},
		NativeAddons: []NativeAddon{{Package: "fake", Path: "node/bin/node", SHA256: nodeSum}},
	}
	body, _ := json.MarshalIndent(m, "", "  ")
	write(t, filepath.Join(root, ManifestName), string(body), 0o644)
	path := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, path)

	got, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Platform != "linux/amd64" {
		t.Errorf("the manifest read back wrong: %+v", got)
	}
}

func writeArchiveTo(t *testing.T, root, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := writeDeterministicTarGz(root, f); err != nil {
		t.Fatal(err)
	}
}

// forEachHeader streams the archive's headers, which is where the properties this file is about
// actually live: a mode, an owner and a link target are metadata, and reading them back out is the
// only way to assert that normalisation happened.
func forEachHeader(t *testing.T, raw []byte, fn func(name string, mode int64, uid int, uname, linkname string)) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !hdr.ModTime.Equal(bundleEpoch) {
			t.Errorf("%s carries mtime %s, not the fixed epoch", hdr.Name, hdr.ModTime)
		}
		fn(strings.TrimSuffix(hdr.Name, "/"), hdr.Mode, hdr.Uid, hdr.Uname, hdr.Linkname)
	}
}

// walk visits every path under root, symlinks excepted: `os.Chtimes` follows a link, and the
// fixture's `.bin/tsc` points at a package that is not there — which is exactly what a real
// pnpm tree looks like when the `.bin` shim outlives a prune.
func walk(t *testing.T, root string, fn func(string)) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			walk(t, p, fn)
		}
		fn(p)
	}
}

// THE TREE DIGESTS MUST BE RE-DERIVABLE TOO, and this is the test that was missing when the first
// version of {@link Verify} shipped. Every fixture above set only single-file digests, so
// the tree path was never exercised — and it was wrong: it sorted the assembled LINES, each of
// which starts with a hash, where `treeDigest` sorts by the relative PATH. With four files the two
// orderings can coincide; over a real bundle's 31,823 they never do, and every tree reported as
// changed.
//
// So this fixture is deliberately built so that path order and hash order disagree, and it asserts
// both directions: a manifest whose tree digests were taken from the staged tree verifies, and one
// file's content changing breaks it.
func TestVerifyBundleReDerivesTreeDigests(t *testing.T) {
	root := stageFixture(t)
	// Enough files, with contents chosen so their digests do not ascend with their names, that a
	// sort on the wrong key cannot pass by luck.
	for i := 0; i < 24; i++ {
		write(t, filepath.Join(root, "orchestrator", "dist", "src", "mod"+string(rune('a'+i))+".js"),
			strings.Repeat("payload", 24-i), 0o644)
	}

	distTree, _, _, err := treeDigest(filepath.Join(root, "orchestrator", "dist"))
	if err != nil {
		t.Fatal(err)
	}
	wholeTree, files, _, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Schema:     SchemaID,
		Platform:   "linux/amd64",
		Components: []Component{{Name: "orchestrator", Path: "orchestrator/dist", TreeSHA256: distTree}},
		Tree:       TreeSummary{Files: files, SHA256: wholeTree},
	}
	body, _ := json.MarshalIndent(m, "", "  ")
	write(t, filepath.Join(root, ManifestName), string(body), 0o644)

	path := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, path)
	if _, err := Verify(path); err != nil {
		t.Fatalf("a bundle whose tree digests came from its own staged tree did not verify:\n%v", err)
	}

	// And one byte of one file, deep in the tree, must break it — with BOTH trees named, because
	// the file is inside both.
	write(t, filepath.Join(root, "orchestrator", "dist", "src", "modc.js"), "tampered", 0o644)
	tampered := filepath.Join(t.TempDir(), "b.tar.gz")
	writeArchiveTo(t, root, tampered)
	err = verifyBundleErr(tampered)
	if err == nil {
		t.Fatal("a tampered file inside a verified tree went unnoticed")
	}
	for _, want := range []string{"orchestrator/dist", "the whole bundle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name the %s tree:\n%v", want, err)
		}
	}
}

// verifyBundleErr is Verify with the manifest discarded — the tests above only ever want the
// error, and `_, err :=` at every call site reads worse than a name.
func verifyBundleErr(path string) error {
	_, err := Verify(path)
	return err
}
