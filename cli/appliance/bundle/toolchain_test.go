package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, dir string, entries []tar.Header, bodies []string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i, h := range entries {
		hdr := h
		hdr.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.tgz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTreeStripsThePrefixAndKeepsTheLayout(t *testing.T) {
	dir := t.TempDir()
	archive := tarGz(t, dir,
		[]tar.Header{
			{Name: "package/bin/pnpm.cjs", Mode: 0o755, Typeflag: tar.TypeReg},
			{Name: "package/dist/pnpm.cjs", Mode: 0o644, Typeflag: tar.TypeReg},
			{Name: "package/package.json", Mode: 0o644, Typeflag: tar.TypeReg},
			// Not under the prefix — npm tarballs are all `package/`, and a member outside it is
			// not something to guess about.
			{Name: "elsewhere/README", Mode: 0o644, Typeflag: tar.TypeReg},
		},
		[]string{"launcher", "the whole program", `{"name":"pnpm"}`, "no"})

	dst := filepath.Join(dir, "out")
	if err := extractTree(archive, "package/", dst); err != nil {
		t.Fatalf("extractTree: %v", err)
	}
	// The launcher requires `../dist/pnpm.cjs`, which is exactly why this takes the tree and not
	// one member the way the Node runtime does.
	for name, want := range map[string]string{
		"bin/pnpm.cjs":  "launcher",
		"dist/pnpm.cjs": "the whole program",
		"package.json":  `{"name":"pnpm"}`,
	} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "README")); err == nil {
		t.Error("a member outside the prefix was extracted")
	}
}

// A tarball whose layout is not what the pin assumes must fail HERE, not by leaving an empty
// directory that the next `stat` accepts as a complete unpack.
func TestExtractTreeRefusesAnArchiveWithNothingUnderThePrefix(t *testing.T) {
	dir := t.TempDir()
	archive := tarGz(t, dir,
		[]tar.Header{{Name: "somethingelse/bin/pnpm.cjs", Mode: 0o755, Typeflag: tar.TypeReg}},
		[]string{"launcher"})

	err := extractTree(archive, "package/", filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("an archive with no members under the prefix unpacked successfully")
	}
	if !strings.Contains(err.Error(), "package/") {
		t.Errorf("the error does not say what was looked for: %v", err)
	}
}

// The ordinary way an extractor written in an afternoon writes over somebody's `/etc`. Not a
// theory about a hostile registry — `..` in a member name is what a tool with a different idea of
// relative paths produces by accident.
func TestExtractTreeRefusesAMemberThatEscapes(t *testing.T) {
	dir := t.TempDir()
	archive := tarGz(t, dir,
		[]tar.Header{
			{Name: "package/bin/pnpm.cjs", Mode: 0o755, Typeflag: tar.TypeReg},
			{Name: "package/../../escaped", Mode: 0o644, Typeflag: tar.TypeReg},
		},
		[]string{"launcher", "should never be written"})

	dst := filepath.Join(dir, "deep", "out")
	err := extractTree(archive, "package/", dst)
	if err == nil {
		t.Fatal("a member that escapes the destination was extracted")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("the error does not say what happened: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); err == nil {
		t.Error("the escaping member was written before the check refused it")
	}
}

// Symlinks, devices and hard links are how a member points somewhere the path check already
// refused. An npm tarball has none, so materialising one would only ever be a surprise.
func TestExtractTreeIgnoresEverythingThatIsNotAFileOrDirectory(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range []tar.Header{
		{Name: "package/bin/pnpm.cjs", Mode: 0o755, Typeflag: tar.TypeReg, Size: 8},
		{Name: "package/evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		hdr := h
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, "launcher"); err != nil {
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
	archive := filepath.Join(dir, "a.tgz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "out")
	if err := extractTree(archive, "package/", dst); err != nil {
		t.Fatalf("extractTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "evil")); err == nil {
		t.Error("a symlink member was materialised")
	}
}

// THE ONE THING THE BUILD WRITES INTO SOMEBODY'S CHECKOUT, and the guard that stops it writing
// there at all when the checkout already has what it needs. Never a refresh and never a repair:
// running `pnpm install` over a working tree is how a build takes an afternoon off somebody.
func TestBootstrapDoesNothingWhenTypescriptIsAlreadyThere(t *testing.T) {
	src := t.TempDir()
	tsc := filepath.Join(src, "node_modules", "typescript", "bin", "tsc")
	if err := os.MkdirAll(filepath.Dir(tsc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tsc, []byte("#!/usr/bin/env node"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A toolchain whose pnpm does not exist. If the bootstrap tried to run anything at all this
	// would fail, which is a sharper assertion than counting how many times it did not.
	var said []string
	tc := &toolchain{Node: "/nonexistent/node", Pnpm: []string{"/nonexistent/node", "/nonexistent/pnpm.cjs"}}
	err := bootstrapOrchestratorModules(src, tc, func(f string, a ...any) { said = append(said, f) })
	if err != nil {
		t.Fatalf("the bootstrap ran against a checkout that did not need it: %v", err)
	}
	if len(said) != 0 {
		t.Errorf("the bootstrap reported work it did not do: %v", said)
	}
}

// And when there is nothing to compile with, the failure has to name the directory it tried — a
// clean-checkout build that fails here is somebody's first five minutes with the repository.
func TestBootstrapReportsAFailedInstallWithItsDirectory(t *testing.T) {
	src := t.TempDir()
	tc := &toolchain{Node: "/nonexistent/node", Pnpm: []string{"/nonexistent/node", "/nonexistent/pnpm.cjs"}}
	err := bootstrapOrchestratorModules(src, tc, func(string, ...any) {})
	if err == nil {
		t.Fatal("a bootstrap with no working pnpm reported success")
	}
	if !strings.Contains(err.Error(), src) {
		t.Errorf("the error does not name the directory it tried: %v", err)
	}
}

// TestPnpmRunsWithTheBuildsOwnNodeOnPath pins the reason the clean-checkout job exists.
//
// A PACKAGE'S postinstall IS A SHELL LINE, not something pnpm interprets: `node postinstall.js`,
// spawned through `sh`, resolved from PATH. So supplying pnpm with an interpreter is not the same
// as supplying the install with one — on a machine that already has a node the two silently
// differ, and on the machine this whole toolchain exists for (no JavaScript on it at all) the
// install dies at the first postinstall with `sh: 1: node: not found`. OBSERVED 2026-08-27: node
// fetched, pnpm fetched, 518 packages resolved, dead at `@swc/core`.
//
// PREPENDED, NOT APPENDED, and that is the assertion below rather than a detail: a developer
// machine has its own node, and a build that claimed a pinned toolchain while running the
// operator's node would be making a false claim quietly.
func TestPnpmRunsWithTheBuildsOwnNodeOnPath(t *testing.T) {
	nodeDir := filepath.Join(t.TempDir(), "node-22", "bin")
	tc := &toolchain{Node: filepath.Join(nodeDir, "node"), Pnpm: []string{filepath.Join(nodeDir, "node"), "pnpm.cjs"}}

	t.Setenv("PATH", "/usr/bin:/bin")
	cmd := tc.pnpmCmd(t.TempDir(), "install")

	// The LAST assignment wins in os/exec, so the effective PATH is the last PATH= in Env.
	var path string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if path == "" {
		t.Fatal("pnpmCmd set no PATH; a postinstall that shells out to node has nothing to find")
	}
	want := nodeDir + string(os.PathListSeparator)
	if !strings.HasPrefix(path, want) {
		t.Errorf("PATH is %q; the build's own node must come FIRST, so %q had to prefix it", path, want)
	}
	if !strings.Contains(path, "/usr/bin") {
		t.Errorf("PATH is %q; the machine's own entries must survive — the build adds to PATH, it does not replace it", path)
	}
}

// @kontra/core IS A BUILT DEPENDENCY, and the orchestrator's compile cannot resolve it unbuilt.
// Same guard as the bootstrap above, for the same reason: never a refresh over a working tree.
func TestSharedCoreIsNotRebuiltWhenItIsAlreadyBuilt(t *testing.T) {
	root := t.TempDir()
	built := filepath.Join(root, "shared", "core", "dist", "cjs", "index.js")
	if err := os.MkdirAll(filepath.Dir(built), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built, []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A pnpm that does not exist, so running anything at all is the failure.
	var said []string
	tc := &toolchain{Node: "/nonexistent/node", Pnpm: []string{"/nonexistent/node", "/nonexistent/pnpm.cjs"}}
	if err := buildSharedCore(root, tc, func(f string, a ...any) { said = append(said, f) }); err != nil {
		t.Fatalf("rebuilt a core that was already built: %v", err)
	}
	if len(said) != 0 {
		t.Errorf("reported work it did not do: %v", said)
	}
}

// And when it cannot build, the failure names the directory — this runs on a clean machine, where
// `TS2307: Cannot find module '@kontra/core/...'` fifty times over is the alternative message.
func TestSharedCoreReportsAFailedBuildWithItsDirectory(t *testing.T) {
	root := t.TempDir()
	tc := &toolchain{Node: "/nonexistent/node", Pnpm: []string{"/nonexistent/node", "/nonexistent/pnpm.cjs"}}
	err := buildSharedCore(root, tc, func(string, ...any) {})
	if err == nil {
		t.Fatal("a core build with no working pnpm reported success")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the error does not name the directory it tried: %v", err)
	}
	if !strings.Contains(err.Error(), "@kontra/core") {
		t.Errorf("the error does not name what it was building: %v", err)
	}
}
