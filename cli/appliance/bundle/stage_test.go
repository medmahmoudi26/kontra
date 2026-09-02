package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- the tree digest --------------------------------------------------------------------

func TestTreeDigestIsStableAndContentSensitive(t *testing.T) {
	root := stageFixture(t)
	first, files, bytesN, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 || bytesN == 0 {
		t.Fatalf("treeDigest counted nothing: %d files, %d bytes", files, bytesN)
	}
	second, _, _, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("treeDigest is not stable: %s vs %s", first, second)
	}

	write(t, filepath.Join(root, "orchestrator", "dist", "src", "main.js"), "console.log('changed')\n", 0o644)
	third, _, _, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Error("treeDigest did not move when a file's content did")
	}
}

// A symlink has no content to hash, and a tree digest that ignored them would be blind to a `.bin`
// shim being repointed — which is how a bundle ends up running something other than what it says.
func TestTreeDigestNoticesARepointedSymlink(t *testing.T) {
	root := stageFixture(t)
	before, _, _, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "orchestrator", "node_modules", ".bin", "tsc")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../somewhere/else", link); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("repointing a symlink did not change the tree digest")
	}
}

// --- the per-platform prune -------------------------------------------------------------

func fakeCoreBridge(t *testing.T, modules string, triples ...string) {
	t.Helper()
	pkg := filepath.Join(modules, "@temporalio", "core-bridge")
	mkdir(t, pkg)
	write(t, filepath.Join(pkg, "package.json"), `{"name":"@temporalio/core-bridge","version":"1.18.1"}`, 0o644)
	for _, triple := range triples {
		dir := filepath.Join(pkg, "releases", triple)
		mkdir(t, dir)
		write(t, filepath.Join(dir, "index.node"), "ELF for "+triple, 0o644)
	}
}

// One npm package, five prebuilt Rust binaries, 140 MB, and exactly one of them ever loaded.
// Keeping all five would leave a "per-platform" bundle four-fifths of whose largest component is
// unreachable — and a manifest that cannot honestly say which build it carries.
func TestPruneForeignPrebuildsKeepsOnlyTheHostsAndSaysWhatItDropped(t *testing.T) {
	root := t.TempDir()
	modules := filepath.Join(root, "node_modules")
	fakeCoreBridge(t, modules,
		"x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu",
		"x86_64-apple-darwin", "aarch64-apple-darwin", "x86_64-pc-windows-msvc")

	dropped, err := pruneForeignPrebuilds(modules, Platform{OS: "linux", Arch: "amd64"}, func(string, ...any) {})
	if err != nil {
		t.Fatalf("pruneForeignPrebuilds: %v", err)
	}
	if len(dropped) != 4 {
		t.Errorf("dropped %v, want the four non-host prebuilds", dropped)
	}
	// Sorted, because it goes in a manifest whose bytes must be a function of its values alone.
	if !isSorted(dropped) {
		t.Errorf("the dropped list is not sorted, so the manifest is not deterministic: %v", dropped)
	}
	releases := filepath.Join(modules, "@temporalio", "core-bridge", "releases")
	left, err := os.ReadDir(releases)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Name() != "x86_64-unknown-linux-gnu" {
		t.Errorf("after pruning, releases/ holds %v", names(left))
	}
}

// A bundle whose Temporal core is missing still archives, still hydrates, and fails at the first
// `NativeConnection.connect` with a PrebuildError naming a path — a long way from the build, in a
// process nobody is watching. So the build refuses instead.
func TestPruneForeignPrebuildsRefusesWhenTheHostsIsAbsent(t *testing.T) {
	root := t.TempDir()
	modules := filepath.Join(root, "node_modules")
	fakeCoreBridge(t, modules, "aarch64-apple-darwin", "x86_64-pc-windows-msvc")

	_, err := pruneForeignPrebuilds(modules, Platform{OS: "linux", Arch: "amd64"}, func(string, ...any) {})
	if err == nil {
		t.Fatal("a bundle with no loadable Temporal core was built")
	}
	if !strings.Contains(err.Error(), "x86_64-unknown-linux-gnu") {
		t.Errorf("the message does not name the prebuild it looked for:\n%v", err)
	}
}

// If upstream moves its prebuilds, the prune must say so rather than silently doing nothing —
// "nothing to prune" and "the layout changed" look identical from a `ReadDir` that returns ENOENT.
func TestPruneForeignPrebuildsRefusesWhenTheLayoutChanges(t *testing.T) {
	root := t.TempDir()
	modules := filepath.Join(root, "node_modules")
	mkdir(t, filepath.Join(modules, "@temporalio", "core-bridge"))

	_, err := pruneForeignPrebuilds(modules, Platform{OS: "linux", Arch: "amd64"}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "no longer ships prebuilt binaries") {
		t.Errorf("a missing releases/ directory was accepted: %v", err)
	}
}

// Two of these three would break the digest outright: `.modules.yaml` carries an absolute path
// from the building machine and `.pnpm-workspace-state-v1.json` carries a wall clock.
func TestPrunePnpmMetadataRemovesTheNonDeterministicFiles(t *testing.T) {
	modules := filepath.Join(t.TempDir(), "node_modules")
	mkdir(t, filepath.Join(modules, ".pnpm"))
	write(t, filepath.Join(modules, ".pnpm", "lock.yaml"), "x\n", 0o644)
	write(t, filepath.Join(modules, ".modules.yaml"), "virtualStoreDir: /home/ci/checkout/node_modules/.pnpm\n", 0o644)
	write(t, filepath.Join(modules, ".pnpm-workspace-state-v1.json"), `{"lastValidatedTimestamp":1787753441810}`, 0o644)
	write(t, filepath.Join(modules, "fastify.js"), "module.exports = {}\n", 0o644)

	if err := prunePnpmMetadata(modules, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{".pnpm", ".modules.yaml", ".pnpm-workspace-state-v1.json"} {
		if _, err := os.Lstat(filepath.Join(modules, gone)); err == nil {
			t.Errorf("%s survived the prune", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(modules, "fastify.js")); err != nil {
		t.Errorf("the prune removed a real package: %v", err)
	}
	// Idempotent: a second call on a pruned tree must not fail on the files it already removed.
	if err := prunePnpmMetadata(modules, func(string, ...any) {}); err != nil {
		t.Errorf("pruning twice failed: %v", err)
	}
}

// --- enumeration ------------------------------------------------------------------------

func TestCollectNativeAddonsNamesThePackageAndItsBuild(t *testing.T) {
	root := t.TempDir()
	modules := filepath.Join(root, "orchestrator", "node_modules")
	fakeCoreBridge(t, modules, "x86_64-unknown-linux-gnu")

	duck := filepath.Join(modules, "@duckdb", "node-bindings-linux-x64")
	mkdir(t, duck)
	write(t, filepath.Join(duck, "package.json"), `{"name":"@duckdb/node-bindings-linux-x64","version":"1.5.4-r.1"}`, 0o644)
	write(t, filepath.Join(duck, "duckdb.node"), "addon", 0o644)
	write(t, filepath.Join(duck, "libduckdb.so"), "shared library", 0o644)

	// A plain JS file must not be mistaken for an addon, and neither must a directory called
	// something.node.
	write(t, filepath.Join(duck, "index.js"), "module.exports = {}", 0o644)

	m := &Manifest{excludedPrebuilds: []string{"aarch64-apple-darwin", "x86_64-pc-windows-msvc"}}
	if err := collectNativeAddons(root, m); err != nil {
		t.Fatal(err)
	}
	if len(m.NativeAddons) != 3 {
		t.Fatalf("found %d addons, want 3: %+v", len(m.NativeAddons), m.NativeAddons)
	}
	// Sorted by path, so the manifest's bytes are a function of its values.
	if !isSorted(addonPaths(m)) {
		t.Errorf("addons are not sorted by path: %v", addonPaths(m))
	}

	byPath := map[string]NativeAddon{}
	for _, a := range m.NativeAddons {
		byPath[a.Path] = a
	}

	bridge := byPath["orchestrator/node_modules/@temporalio/core-bridge/releases/x86_64-unknown-linux-gnu/index.node"]
	if bridge.Package != "@temporalio/core-bridge" || bridge.Version != "1.18.1" {
		t.Errorf("the Rust bridge was not attributed to its package: %+v", bridge)
	}
	if bridge.Build != "x86_64-unknown-linux-gnu" {
		t.Errorf("the bridge's build is %q, want the vendor's own triple", bridge.Build)
	}
	if len(bridge.ExcludedBuilds) != 2 {
		t.Errorf("the bridge does not record what was dropped: %+v", bridge.ExcludedBuilds)
	}

	so := byPath["orchestrator/node_modules/@duckdb/node-bindings-linux-x64/libduckdb.so"]
	if so.Package != "@duckdb/node-bindings-linux-x64" {
		t.Errorf("libduckdb.so was not attributed: %+v", so)
	}
	// The 68 MB shared library is the bigger half of DuckDB's addon; a manifest that recorded only
	// the 376 KB `.node` in front of it would be naming the smaller one.
	if so.Build != "linux-x64" {
		t.Errorf("the platform package's build is %q, want linux-x64", so.Build)
	}
	// Only the core bridge gets the excluded list — it is the only package that ships siblings.
	if len(so.ExcludedBuilds) != 0 {
		t.Errorf("an unrelated addon was given the bridge's dropped list: %+v", so.ExcludedBuilds)
	}
	if _, isAddon := byPath["orchestrator/node_modules/@duckdb/node-bindings-linux-x64/index.js"]; isAddon {
		t.Error("a JavaScript file was recorded as a native addon")
	}
}

func TestVendorBuildName(t *testing.T) {
	cases := []struct{ rel, pkg, want string }{
		{"orchestrator/node_modules/@temporalio/core-bridge/releases/aarch64-apple-darwin/index.node", "@temporalio/core-bridge", "aarch64-apple-darwin"},
		{"orchestrator/node_modules/@swc/core-linux-x64-gnu/swc.linux-x64-gnu.node", "@swc/core-linux-x64-gnu", "linux-x64-gnu"},
		{"orchestrator/node_modules/@duckdb/node-bindings-darwin-arm64/duckdb.node", "@duckdb/node-bindings-darwin-arm64", "darwin-arm64"},
		{"orchestrator/node_modules/something/native.node", "something", ""},
	}
	for _, c := range cases {
		if got := vendorBuildName(c.rel, c.pkg); got != c.want {
			t.Errorf("vendorBuildName(%q, %q) = %q, want %q", c.rel, c.pkg, got, c.want)
		}
	}
}

// --- the manifest -----------------------------------------------------------------------

func TestManifestEncodesDeterministicallyAndReadably(t *testing.T) {
	m := &Manifest{
		Schema:    SchemaID,
		Bundle:    "orchestrator",
		Platform:  "linux/amd64",
		Toolchain: map[string]string{"tsc": "5.9.3", "pnpm": "10.33.2", "node": "v22.13.0"},
		Components: []Component{{
			Name:     "node-runtime",
			Version:  NodeVersion,
			Upstream: &Pin{Name: "node-runtime", URL: "https://nodejs.org/dist/v22.13.0/node-v22.13.0-linux-x64.tar.gz"},
		}},
	}
	first, err := encodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		again, err := encodeManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		// Map iteration order in Go is randomised per run AND per range. If `Toolchain` leaked
		// that order into the bytes, the manifest would differ between two encodings of one value
		// — and the bundle's digest with it.
		if !bytes.Equal(first, again) {
			t.Fatalf("two encodings of one manifest differ:\n%s\n%s", first, again)
		}
	}
	// A URL whose `&` has become `\u0026` is a URL somebody has to un-mangle before pasting it.
	if bytes.Contains(first, []byte(`\u003c`)) || bytes.Contains(first, []byte(`\u0026`)) {
		t.Errorf("the manifest is HTML-escaped, and it is meant to be read:\n%s", first)
	}
	var back Manifest
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("the manifest does not round-trip: %v", err)
	}
	if back.Components[0].Upstream == nil || back.Components[0].Upstream.URL == "" {
		t.Error("the upstream pin did not survive the round trip")
	}
}

// --- fetching ---------------------------------------------------------------------------

func bundleDigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFetchPinnedVerifiesBeforeItStores(t *testing.T) {
	body := []byte("the real artifact")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	cache := t.TempDir()

	path, err := fetchPinned(Pin{Name: "node-runtime", URL: srv.URL, Digest: bundleDigestOf(body)}, cache, func(string, ...any) {})
	if err != nil {
		t.Fatalf("fetchPinned: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the cached artifact is not what was served: %v", err)
	}
}

// THE ACCEPTANCE CRITERION: "a missing or mismatched upstream digest fails the build with the
// expectation and what arrived". `sha256sum -c` prints `FAILED` and leaves you to work out which
// of the two numbers was supposed to be right.
func TestFetchPinnedRefusesAMismatchAndShowsBothDigests(t *testing.T) {
	served := []byte("something else entirely")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(served)
	}))
	defer srv.Close()
	cache := t.TempDir()
	expected := bundleDigestOf([]byte("what upstream published"))

	_, err := fetchPinned(Pin{Name: "node-runtime", URL: srv.URL, Digest: expected}, cache, func(string, ...any) {})
	if err == nil {
		t.Fatal("unverified bytes entered the bundle")
	}
	for _, want := range []string{expected, bundleDigestOf(served), srv.URL, "node-runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message omits %q:\n%v", want, err)
		}
	}
	// And nothing was left behind under the name a later build would trust.
	entries, _ := os.ReadDir(filepath.Join(cache, "sha256"))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".fetching-") {
			t.Errorf("a failed fetch published %s", e.Name())
		}
	}
}

// A cache is a directory on somebody's laptop, and the cheapest way to build a bundle whose
// manifest is a lie is to have a truncated download sitting in one.
func TestFetchPinnedReverifiesTheCacheAndRepairsIt(t *testing.T) {
	body := []byte("the real artifact")
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cache := t.TempDir()
	pin := Pin{Name: "node-runtime", URL: srv.URL, Digest: bundleDigestOf(body)}
	dir := filepath.Join(cache, "sha256")
	mkdir(t, dir)
	// A truncated download wearing the right name.
	write(t, filepath.Join(dir, pin.Digest), "trunc", 0o644)

	path, err := fetchPinned(pin, cache, func(string, ...any) {})
	if err != nil {
		t.Fatalf("fetchPinned: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, body) {
		t.Error("a corrupt cache entry was used")
	}
	if served != 1 {
		t.Errorf("the corrupt entry was not refetched (%d requests)", served)
	}

	// The second call must not hit the network at all.
	if _, err := fetchPinned(pin, cache, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if served != 1 {
		t.Errorf("a verified cache hit still fetched (%d requests)", served)
	}
}

func TestFetchPinnedReportsANon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := fetchPinned(Pin{Name: "node-runtime", URL: srv.URL, Digest: bundleDigestOf(nil)}, t.TempDir(), func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a 404 was not reported as one: %v", err)
	}
}

// --- extraction -------------------------------------------------------------------------

func TestExtractMemberTakesOneFileAndRefusesWhenItIsAbsent(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{
		"node-v22.13.0-linux-x64/bin/node":     "ELF",
		"node-v22.13.0-linux-x64/README.md":    "docs",
		"node-v22.13.0-linux-x64/include/node": "headers",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "node.tar.gz")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "node")
	n, err := extractMember(archivePath, "node-v22.13.0-linux-x64/bin/node", dst)
	if err != nil {
		t.Fatalf("extractMember: %v", err)
	}
	if n != 3 {
		t.Errorf("extracted %d bytes, want 3", n)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "ELF" {
		t.Errorf("extracted %q", got)
	}

	// A tarball whose layout is not what the pin assumes must fail HERE, not with a mysteriously
	// empty file three steps later.
	if _, err := extractMember(archivePath, "node-v99.0.0-linux-x64/bin/node", dst); err == nil {
		t.Error("extracting an absent member succeeded")
	}
}

// --- options ----------------------------------------------------------------------------

func TestBuildRefusesSomethingThatIsNotTheCheckout(t *testing.T) {
	_, err := BuildOrchestrator(BuildOptions{RepoRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "control/orchestrator/package.json") {
		t.Errorf("a directory with no orchestrator was accepted as the checkout: %v", err)
	}
}

func TestBuildOptionsDefaultToTheHostAndTheCheckout(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "control", "orchestrator"))
	write(t, filepath.Join(root, "control", "orchestrator", "package.json"), `{"version":"0.1.0"}`, 0o644)

	got, err := BuildOptions{RepoRoot: root}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform.OS != runtime.GOOS || got.Platform.Arch != runtime.GOARCH {
		t.Errorf("platform defaulted to %s, not this host", got.Platform)
	}
	if got.OutDir != filepath.Join(root, "build", "bundles") {
		t.Errorf("OutDir defaulted to %s", got.OutDir)
	}
	// The staging directory must be inside the output directory, not beside the checkout: it is
	// half a gigabyte and it is the thing `--out` exists to relocate.
	if !strings.HasPrefix(got.StageDir, got.OutDir) {
		t.Errorf("StageDir %s is not under OutDir %s", got.StageDir, got.OutDir)
	}
	// And it is per-platform, or two platforms built in one directory would stage over each other.
	if !strings.Contains(got.StageDir, got.Platform.OS) || !strings.Contains(got.StageDir, got.Platform.Arch) {
		t.Errorf("StageDir %s is not per-platform", got.StageDir)
	}
}

// The machine-path list is what the scan refuses to find inside the artifact, so an empty entry in
// it would refuse every build with a message naming nothing.
func TestMachinePathsAreAbsoluteAndNonEmpty(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "control", "orchestrator"))
	write(t, filepath.Join(root, "control", "orchestrator", "package.json"), `{"version":"0.1.0"}`, 0o644)
	opts, err := BuildOptions{RepoRoot: root}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range opts.machinePaths(nil) {
		if p == "" || !filepath.IsAbs(p) {
			t.Errorf("machinePaths returned %q", p)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1 << 20: "1.0 MB", 3 << 30: "3.0 GB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func isSorted(in []string) bool {
	for i := 1; i < len(in); i++ {
		if in[i-1] > in[i] {
			return false
		}
	}
	return true
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func addonPaths(m *Manifest) []string {
	out := make([]string, 0, len(m.NativeAddons))
	for _, a := range m.NativeAddons {
		out = append(out, a.Path)
	}
	return out
}

// --- the whole thing, off by default ------------------------------------------------------

// THE END-TO-END BUILD, GATED. It fetches ~50 MB, resolves the production dependency tree, writes
// a ~120 MB archive and needs about half a gigabyte of working space — none of which belongs in a
// suite somebody runs while they are thinking. Everything it would assert about the SHAPE of a
// bundle is asserted above against fixtures; what only this can prove is that the real
// orchestrator, the real lockfile and the real prebuilt addons still fit together.
//
//	KONTRA_BUNDLE_E2E=1 go test ./appliance/bundle/ -run EndToEnd -timeout 30m
//
// It builds TWICE, because the reproducibility claim is the one thing a single build cannot check
// and the one thing the whole file is arranged to deliver.
func TestBuildEndToEndIsReproducible(t *testing.T) {
	if os.Getenv("KONTRA_BUNDLE_E2E") != "1" {
		t.Skip("set KONTRA_BUNDLE_E2E=1 to build a real bundle (several minutes, ~500 MB of working space)")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()

	build := func() *BuildResult {
		t.Helper()
		res, err := BuildOrchestrator(BuildOptions{RepoRoot: repo, OutDir: out, Progress: os.Stderr})
		if err != nil {
			t.Fatalf("BuildOrchestrator: %v", err)
		}
		return res
	}

	first := build()
	// Every digest the manifest claims, re-derived from the archive — files, native addons and the
	// tree digests.
	if _, err := Verify(first.BundlePath); err != nil {
		t.Fatalf("the bundle does not match its own manifest: %v", err)
	}
	if len(first.Manifest.NativeAddons) == 0 {
		t.Error("no native addons were recorded; the manifest cannot then say which build it carries")
	}
	for _, a := range first.Manifest.NativeAddons {
		if strings.Contains(a.Path, "core-bridge/releases/") && a.Build != mustTriple(t) {
			t.Errorf("the bundle carries a foreign Temporal prebuild: %s", a.Path)
		}
	}

	second := build()
	if first.SHA256 != second.SHA256 {
		t.Errorf("two builds of the same inputs produced different bundles:\n  %s\n  %s\n"+
			"  tree digests: %s vs %s", first.SHA256, second.SHA256, first.Manifest.Tree.SHA256, second.Manifest.Tree.SHA256)
	}
}

// THE CROSS BUILD, GATED THE SAME WAY, because it is the claim issue 17 adds and the one that
// cannot be checked against a fixture. It builds for a platform this machine is NOT and asserts
// that everything in the result is for that platform — the Rust prebuild, the npm platform
// packages, and the carried runtime.
//
// WHY A TEST WHEN CI ALREADY DOES THIS. CI builds all four on a linux runner and compares the
// digests to four native builds, which is a stronger check and a twenty-minute one. This is the
// version a developer can run before pushing, on the platform they are least likely to have: it
// takes one cross build and answers the only question that usually goes wrong, which is whether
// pnpm resolved somebody else's tree.
//
//	KONTRA_BUNDLE_E2E=1 go test ./appliance/bundle/ -run CrossBuild -timeout 30m
func TestCrossBuildCarriesTheOtherPlatformsBinaries(t *testing.T) {
	if os.Getenv("KONTRA_BUNDLE_E2E") != "1" {
		t.Skip("set KONTRA_BUNDLE_E2E=1 to build a real cross-platform bundle (several minutes, ~500 MB of working space)")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}

	// Any of the four that is not this one. Chosen from the pin table rather than hard-coded, so
	// this test keeps working on whichever of the four somebody runs it from.
	var target Platform
	for _, p := range Platforms() {
		if p != HostPlatform() {
			target = p
			break
		}
	}
	if target == (Platform{}) {
		t.Fatal("every pinned platform is this host, which cannot be true of four")
	}

	res, err := BuildOrchestrator(BuildOptions{
		RepoRoot: repo, OutDir: t.TempDir(), Platform: target, Progress: os.Stderr,
	})
	if err != nil {
		t.Fatalf("cross-building for %s: %v", target, err)
	}
	if res.Manifest.Platform != target.String() {
		t.Fatalf("the manifest says %s for a build of %s", res.Manifest.Platform, target)
	}
	if _, err := Verify(res.BundlePath); err != nil {
		t.Fatalf("the bundle does not match its own manifest: %v", err)
	}

	// THE ADDONS ARE THE WHOLE POINT. A cross build that quietly resolved the host's dependency
	// tree produces a bundle that is correct in every other respect — right runtime, right
	// JavaScript, every digest accurate — and unusable.
	triple, err := rustTriple(target)
	if err != nil {
		t.Fatal(err)
	}
	var sawBridge bool
	for _, a := range res.Manifest.NativeAddons {
		if strings.Contains(a.Path, "core-bridge/releases/") {
			sawBridge = true
			if a.Build != triple {
				t.Errorf("the %s bundle carries the %s Temporal core", target, a.Build)
			}
		}
		if strings.Contains(a.Package, runtime.GOOS) && !strings.Contains(target.OS, runtime.GOOS) {
			t.Errorf("the %s bundle carries %s, which is this host's platform package", target, a.Package)
		}
	}
	if !sawBridge {
		t.Errorf("no @temporalio/core-bridge prebuild was recorded for %s", target)
	}
}

func mustTriple(t *testing.T) string {
	t.Helper()
	triple, err := rustTriple(Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	return triple
}

// A PRECONDITION THAT REFUSES WORK IT COULD HAVE DONE is not a safety check, it is an outage with a
// helpful message. `fetchPinned` first asked for {@link minFreeBytes} — a floor sized for a 450 MB
// staged tree — before downloading anything, and four of the tests above failed on a box with
// 760 MB free while fetching seventeen-byte fixtures. The floor belongs where the build-sized
// writes are; a download asks about the download.
func TestAFetchDoesNotAskForTheWholeBuildsWorkingSpace(t *testing.T) {
	small := fetchNeed(17)
	if small >= minFreeBytes {
		t.Errorf("fetching 17 bytes demands %s, which is the whole build's floor (%s)",
			humanBytes(small), humanBytes(minFreeBytes))
	}
	if small <= 17 {
		t.Errorf("fetching 17 bytes demands %s, which leaves no headroom at all", humanBytes(small))
	}
	// A 50 MB Node tarball must still be asked about honestly.
	if big := fetchNeed(50 << 20); big <= 50<<20 {
		t.Errorf("fetching 50 MB demands only %s", humanBytes(big))
	}
	// No Content-Length is not "no check" — it is "assume something bigger than anything pinned".
	if unknown := fetchNeed(-1); unknown <= fetchNeed(50<<20) {
		t.Errorf("an unknown size (%s) is treated as smaller than a known 50 MB one", humanBytes(unknown))
	}
}
