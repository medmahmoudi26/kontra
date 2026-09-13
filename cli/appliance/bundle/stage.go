// stage.go — building the artifact the appliance binary carries (ADR 0031 §2,
// issues 13 and 17).
//
// FOUR PLATFORMS, AND WHY THAT IS THE RISKY PART RATHER THAN THE COPY-ON-WRITE STORE. The PRD says
// so outright, and the reason is that every way of getting a per-platform build wrong produces an
// artifact that is INTERNALLY CONSISTENT. A tree pnpm resolved for the build host, a prune that
// kept the wrong Rust triple, a tarball member from the wrong artifact set — each of them archives
// cleanly, and each produces a manifest whose every digest is accurate, because the manifest
// describes the wrong bytes correctly. Nothing fails until somebody runs it.
//
// Three things stop that, and they are the whole of issue 17 in this file. pnpm is told the TARGET
// architecture rather than left to read `process.platform` ({@link stageDependencies}). The
// programs that do the building are fetched and pinned rather than found on PATH
// (toolchain.go), so a bundle is a function of its target and its commit and of nothing about
// the machine. And every compiled file in the staged tree is read back and checked against the
// platform the manifest is about to claim (binfmt.go), which is the only proof a cross build can
// offer where a native build simply runs the thing.
//
// WHAT IS IN IT AND WHY THAT IS THE HARD PART. Three things: a pinned Node runtime, the
// orchestrator's compiled JavaScript, and the production dependency tree that JavaScript needs.
// The third is where the difficulty lives, because four of those dependencies are not JavaScript
// at all:
//
//	@temporalio/core-bridge      a 31 MB Rust `.node` — the Temporal SDK's whole core
//	@duckdb/node-bindings-*      a 68 MB libduckdb.so plus a small `.node` shim
//	@swc/core-linux-x64-gnu      28 MB, pulled in by the worker's workflow bundler
//	esbuild / @esbuild-*         a platform binary (dev-only here, prod-only elsewhere)
//
// A bundle that merely "contains node_modules" says nothing about which of those it has. That is
// how "works on my machine" comes back as "works on the machine that built the bundle", which is
// strictly worse, because it looks reproducible. So every native artifact is enumerated in the
// manifest by package, version, path, size and digest, and {@link Verify} re-derives those
// digests from the archive.
//
// PER-PLATFORM IS NOT AUTOMATIC — ONE PACKAGE FIGHTS IT. `@duckdb/node-bindings-linux-x64` and
// `@swc/core-linux-x64-gnu` are separate npm packages with `os`/`cpu` fields, so pnpm installs
// only the host's and the tree is per-platform for free. `@temporalio/core-bridge` is NOT: one
// package ships FIVE prebuilt `.node` files, 140 MB of them, and picks one at require time from
// `os.arch()`. Left alone, every bundle would carry four binaries it can never load and the
// manifest's claim to name "which addon build" would be four-fifths untrue. {@link pruneForeign
// Prebuilds} keeps the one this platform will ask for and the manifest records the four it
// dropped by name — so the statement is exact, and a reader can see that it was a choice.
//
// WHAT IS DELIBERATELY NOT IN IT: the built SPA. `runtime/handler/internal/hydrate` lists it as its own
// pinned artifact, and the API degrades to serving no static files when it is absent
// (`defaultWebRoot` returns undefined) rather than failing to boot. Two artifacts because the SPA
// changes on a different clock from the server, and shipping them as one means a CSS fix rebuilds
// a 300 MB bundle.
//
// AND NOT PULUMI'S ENGINE, though `@pulumi/*` is still in the dependency tree. `main.ts` argues at
// length that no `@pulumi` module is reachable from the appliance's three roles; the packages are
// installed because the committed lockfile lists them, and the bundle's contents are "what
// `pnpm install --prod --frozen-lockfile` resolves", which is checkable, rather than "what I
// believed was reachable", which is not. Removing them is a `package.json` change and belongs
// with the change that splits the compose controller's dependencies from the appliance's.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// SchemaID versions the manifest. Issue 14 teaches the binary to hydrate a bundle and reads
// this field before anything else, so a shape change is a bump here rather than a surprise there.
//
// STILL v1 AFTER FOUR PLATFORMS, and that is worth stating because it looks like the kind of
// change that would move it. It does not: `platform` was always in the manifest and always
// per-platform, `nativeAddons` already recorded which build it carried, and issue 17 changed which
// VALUES appear in those fields rather than which fields exist. A reader written against v1 reads
// a darwin/arm64 bundle correctly, which is exactly what a schema version is for.
const SchemaID = "kontra.appliance.bundle/v1"

// Manifest is what a human reads to answer "which Node, which addon build, which digest" without
// unpacking anything. It is written into the archive as `manifest.json` AND beside it as
// `<bundle>.manifest.json`, byte-identical, because those are two different readers: the appliance
// opens the one inside, an operator looking at an artifact store reads the one outside.
//
// IT DOES NOT CARRY ITS OWN CONTAINER'S DIGEST, and cannot: the digest is over bytes that include
// this file. The tarball's sha256 goes in `<bundle>.tar.gz.sha256` beside it, which is also the
// name the fetcher will pin.
//
// IT DOES NOT CARRY A GIT COMMIT EITHER, and that is a choice worth stating. A commit id changes
// when the contents do not (an unrelated commit, a dirty tree) and stays the same when they do
// (a rebuild from a modified working tree), so putting one in a content-addressed artifact makes
// its address a function of something that is not its content. What identifies the orchestrator
// here is the digest of the JavaScript that was compiled and the digest of the lockfile that
// resolved its dependencies — both exact, both in the file.
type Manifest struct {
	Schema   string `json:"schema"`
	Bundle   string `json:"bundle"`
	Platform string `json:"platform"`

	// Entrypoint is the command that runs this bundle once unpacked, relative to its root. Issue
	// 14 execs it; a human reading the manifest can run it by hand, which is how this slice's
	// acceptance criterion was checked.
	Entrypoint []string `json:"entrypoint"`

	// Toolchain names the programs that produced the bundle. They are inputs: a different tsc
	// emits different JavaScript, and a digest that did not move across a toolchain change would
	// be describing the wrong thing.
	Toolchain map[string]string `json:"toolchain"`

	Components   []Component   `json:"components"`
	NativeAddons []NativeAddon `json:"nativeAddons"`
	Tree         TreeSummary   `json:"tree"`

	// excludedPrebuilds is scratch, not manifest: pruning learns which sibling binaries it
	// dropped, and enumeration — which runs later, over a tree those files are no longer in —
	// needs to attach the list to the addon it is about. Unexported, so it cannot reach the JSON
	// and cannot become a second, drifting copy of ExcludedBuilds.
	excludedPrebuilds []string
}

// Component is one named part of the bundle.
type Component struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`    // runtime | built | dependencies
	Version string `json:"version"` // exact; never a range
	Path    string `json:"path"`    // where it lives inside the bundle

	// Upstream is the pinned artifact this component was cut from, when there is one: the URL
	// that was fetched and the sha256 that was required of it BEFORE it was opened. Present for
	// the Node runtime and absent for anything built here.
	Upstream *Pin `json:"upstream,omitempty"`

	// Exactly one of these is set. SHA256 is the file's own digest for a single-file component;
	// TreeSHA256 is {@link treeDigest} for a directory.
	SHA256     string `json:"sha256,omitempty"`
	TreeSHA256 string `json:"treeSha256,omitempty"`

	Files int64  `json:"files,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	Note  string `json:"note,omitempty"`
}

// NativeAddon is one compiled artifact — the things that make a bundle platform-specific and the
// reason the manifest exists at all.
type NativeAddon struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`

	// Build is the vendor's own name for the target, verbatim: `x86_64-unknown-linux-gnu` for the
	// Rust bridge, `linux-x64` for the npm platform packages. Not normalised — the point of this
	// field is that it matches what you would see on the vendor's release page.
	Build string `json:"build,omitempty"`

	// ExcludedBuilds are sibling prebuilds this bundle DROPPED. Recorded rather than merely
	// removed: the difference between "this package only ships one binary" and "we chose one of
	// five" is exactly the thing a reader of a per-platform bundle needs to know.
	ExcludedBuilds []string `json:"excludedBuilds,omitempty"`
}

// TreeSummary is the whole staged tree, so the manifest states its own scale.
type TreeSummary struct {
	Files    int64  `json:"files"`
	Symlinks int64  `json:"symlinks"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"` // treeDigest over everything except manifest.json
}

// BuildOptions configures one build. Every path is absolute by the time it reaches the steps.
type BuildOptions struct {
	RepoRoot  string   // the checkout: orchestrator/ is read from here
	OutDir    string   // where the bundle and its manifest are written
	CacheDir  string   // verified upstream artifacts, keyed by digest
	StageDir  string   // the tree that becomes the archive; wiped and rebuilt each run
	Platform  Platform // defaults to the host
	Progress  io.Writer
	KeepStage bool // leave StageDir behind, for looking at what was staged
}

// BuildResult is what the command prints and what the tests assert on.
type BuildResult struct {
	BundlePath   string
	ManifestPath string
	DigestPath   string
	SHA256       string
	Bytes        int64
	Manifest     *Manifest
	StageDir     string
}

// minFreeBytes is the floor this build refuses to cross.
//
// A FULL DISK IS NOT A LOUD FAILURE HERE. This repo has already paid for that lesson once: a full
// disk turned every SeaweedFS write into a bare HTTP 500 that no isolation counter caught. A
// staged tree is ~450 MB before compression and the archive is another ~200 MB, so a build that
// starts with a gigabyte free finishes with nothing — and the thing that breaks is whatever else
// is running on the box, not this. Check before, and refuse with the number.
const minFreeBytes = 1500 << 20

// What a single pinned FETCH asks for: the artifact, plus enough headroom that a build does not
// drive the filesystem to zero on its way to a checksum. Nothing like the build floor — see
// {@link fetchPinned}, where getting this wrong turned a low-disk box into five failing tests.
const (
	fetchHeadroom    = 64 << 20  // above the artifact's own size
	unknownFetchSize = 256 << 20 // when the server sends no Content-Length
)

// BuildOrchestrator stages, verifies and archives the orchestrator bundle.
func BuildOrchestrator(opts BuildOptions) (*BuildResult, error) {
	opts, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	p := progress(opts.Progress)

	// EVERY PIN FIRST, BEFORE ANY WORK AND BEFORE THE NETWORK. Bumping NodeVersion without its
	// digests has to fail here — in a millisecond, naming the file to edit — and not forty
	// seconds into a download that then fails a checksum with no statement of what was expected.
	//
	// ALL THREE, not just the target's. From issue 17 a build resolves three pins: the runtime it
	// will CARRY, the runtime it will RUN (the host's — see toolchain.go), and pnpm. Asking
	// for them one at a time as each step needs them would mean a `--platform all` run fetching
	// 120 MB and compiling for four minutes before discovering that the fourth platform has no
	// digest row.
	pin, err := NodePin(opts.Platform)
	if err != nil {
		return nil, err
	}
	if _, err := NodePin(HostPlatform()); err != nil {
		return nil, fmt.Errorf("the build needs a Node for the machine it is running on: %w", err)
	}
	if _, err := PnpmPin(); err != nil {
		return nil, err
	}
	if _, _, _, err := npmArchitecture(opts.Platform); err != nil {
		return nil, err
	}
	if _, err := rustTriple(opts.Platform); err != nil {
		return nil, err
	}

	if err := requireFreeSpace(opts.OutDir, minFreeBytes); err != nil {
		return nil, err
	}

	tc, err := resolveToolchain(opts, p)
	if err != nil {
		return nil, err
	}

	if err := os.RemoveAll(opts.StageDir); err != nil {
		return nil, fmt.Errorf("clear staging directory %s: %w", opts.StageDir, err)
	}
	if !opts.KeepStage {
		defer func() { _ = os.RemoveAll(opts.StageDir) }()
	}
	if err := os.MkdirAll(opts.StageDir, 0o755); err != nil {
		return nil, err
	}

	m := &Manifest{
		Schema:     SchemaID,
		Bundle:     "orchestrator",
		Platform:   opts.Platform.String(),
		Entrypoint: []string{"node/bin/node", "orchestrator/dist/src/main.js"},
		Toolchain:  map[string]string{},
	}

	// 1. The runtime the bundle CARRIES. Not the one that compiles it: on a cross build that is a
	//    Mach-O binary this machine cannot exec, and the two were the same file only for as long
	//    as there was one platform.
	if err := stageNode(opts, pin, m, p); err != nil {
		return nil, err
	}

	// 2. The orchestrator's JavaScript.
	if err := stageOrchestrator(opts, tc, m, p); err != nil {
		return nil, err
	}

	// 3. Its production dependencies, and the pruning that makes them per-platform.
	if err := stageDependencies(opts, tc, m, p); err != nil {
		return nil, err
	}

	// 4. The gates. Nothing below this line can add bytes to the tree.
	p("scanning the staged tree for credentials and build paths")
	if err := scanStaged(opts.StageDir, opts.machinePaths(tc)); err != nil {
		return nil, err
	}

	//    AND THE ONE THAT MAKES FOUR PLATFORMS SAFE. A native build proves its runtime by running
	//    it; a cross build cannot, so it reads the headers instead. Everything that could put a
	//    foreign binary in this tree has already happened by now — the tarball member, pnpm's
	//    architecture resolution and the prebuild prune — and every one of those failures produces
	//    an archive that is internally consistent and unusable. See binfmt.go.
	p("checking that every compiled file in the tree is for %s", opts.Platform)
	if err := verifyStagedBinaries(opts.StageDir, opts.Platform); err != nil {
		return nil, err
	}

	// 5. The native artifacts, enumerated — the reason a per-platform bundle needs a manifest.
	if err := collectNativeAddons(opts.StageDir, m); err != nil {
		return nil, err
	}

	// 6. Summarise the tree, then write the manifest INTO it. The summary deliberately excludes
	//    manifest.json, which does not exist yet and could not include itself if it did.
	summary, err := summarizeTree(opts.StageDir)
	if err != nil {
		return nil, err
	}
	m.Tree = summary

	manifestBytes, err := encodeManifest(m)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.StageDir, ManifestName), manifestBytes, 0o644); err != nil {
		return nil, err
	}

	// 7. The archive.
	base := fmt.Sprintf("kontra-orchestrator-%s-%s-%s", NodeVersion, opts.Platform.OS, opts.Platform.Arch)
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	bundlePath := filepath.Join(opts.OutDir, base+".tar.gz")
	p("writing %s", bundlePath)

	f, err := os.Create(bundlePath)
	if err != nil {
		return nil, err
	}
	digest, err := writeDeterministicTarGz(opts.StageDir, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(bundlePath)
		return nil, err
	}
	st, err := os.Stat(bundlePath)
	if err != nil {
		return nil, err
	}

	// The manifest beside the archive, byte-identical to the one inside it, plus the archive's own
	// digest in the format `sha256sum -c` reads. Both exist so that an operator looking at an
	// artifact store can answer every question this bundle can answer without downloading it.
	manifestPath := filepath.Join(opts.OutDir, base+".manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return nil, err
	}
	digestPath := bundlePath + ".sha256"
	if err := os.WriteFile(digestPath, []byte(digest+"  "+base+".tar.gz\n"), 0o644); err != nil {
		return nil, err
	}

	return &BuildResult{
		BundlePath:   bundlePath,
		ManifestPath: manifestPath,
		DigestPath:   digestPath,
		SHA256:       digest,
		Bytes:        st.Size(),
		Manifest:     m,
		StageDir:     opts.StageDir,
	}, nil
}

// --- staging steps ------------------------------------------------------------------------

// stageNode fetches the pinned runtime, verifies it, and takes ONE FILE out of it.
//
// One file, not the tarball. Node's distribution is ~180 MB expanded and most of it is npm, the
// C++ headers and the docs — none of which an appliance that runs one compiled program will ever
// open. `bin/node` is self-contained, so the bundle carries 110 MB instead of 180 and the manifest
// records BOTH digests: the upstream tarball's, which is what was verified, and the extracted
// binary's, which is what is actually in the bundle. Recording only the second would be a digest
// of our own extraction; recording only the first would describe bytes that are not here.
func stageNode(opts BuildOptions, pin Pin, m *Manifest, p func(string, ...any)) error {
	archive, err := fetchPinned(pin, opts.CacheDir, p)
	if err != nil {
		return err
	}

	dst := filepath.Join(opts.StageDir, "node", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	member := nodeTarballPrefix(NodeVersion, opts.Platform) + "/bin/node"
	p("extracting %s", member)
	n, err := extractMember(archive, member, dst)
	if err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return err
	}
	sum, err := sha256File(dst)
	if err != nil {
		return err
	}

	// PROVE IT RUNS, WHEN IT CAN RUN HERE. A Node for the wrong architecture unpacks perfectly and
	// fails at the first exec, hours later, inside whatever hydrated it. A native build turns that
	// into a build failure with the version it actually got; a cross build cannot exec this file
	// at all and must not pretend to — {@link verifyStagedBinaries} reads its Mach-O header
	// instead, which is a weaker statement made honestly rather than a stronger one made up.
	if opts.Platform == HostPlatform() {
		out, err := exec.Command(dst, "--version").Output()
		if err != nil {
			return fmt.Errorf("the extracted node does not run: %w", err)
		}
		if got := strings.TrimSpace(string(out)); got != "v"+NodeVersion {
			return fmt.Errorf("the extracted node reports %s, but the pin says v%s", got, NodeVersion)
		}
	}

	m.Components = append(m.Components, Component{
		Name:     "node-runtime",
		Kind:     "runtime",
		Version:  NodeVersion,
		Path:     "node/bin/node",
		Upstream: &pin,
		SHA256:   sum,
		Bytes:    n,
		Note:     "one file lifted from the upstream tarball; upstream.sha256 is what was verified on fetch",
	})
	// THE PIN, NOT THE MACHINE. This says which Node compiled the bundle, and it is the same
	// sentence whether that Node ran on the target's architecture or on a build host's — because
	// it IS the same Node, {@link NodeVersion}, fetched from the same digest, and the JavaScript
	// tsc emits does not depend on which build of the interpreter ran it.
	//
	// It has to be the same sentence, or the manifest changes with the build host and so does the
	// bundle's digest. Then a darwin/arm64 bundle built on a macOS runner and one cross-built on
	// linux are two different artifacts that contain identical files, and "reproducible" quietly
	// narrows to "reproducible on the machine that built it first".
	m.Toolchain["node"] = "v" + NodeVersion + " (the pinned runtime, used to compile)"
	return nil
}

// stageOrchestrator compiles `orchestrator/` into the staging tree.
//
// INTO STAGING, NOT INTO control/orchestrator/dist. The checkout is somebody's working tree — on this box
// three containers are running out of it — and a build tool that overwrites `dist` as a side
// effect of producing an artifact is a build tool that loses somebody's afternoon. `--outDir` is
// the whole difference and it costs nothing.
//
// tsc IS THE PROJECT'S OWN, from `control/orchestrator/node_modules`, run by the pinned Node. Not a tsc
// from PATH: `package.json` pins the version, the version decides the emitted JavaScript, and the
// emitted JavaScript is what the digest is over.
//
// AND IT IS INSTALLED IF IT IS NOT THERE, which is issue 17's change to this step. Refusing with
// "run `pnpm install` first" was a correct instruction and a broken promise: "buildable from a
// clean checkout with nothing but Go" cannot be true of a build that stops to ask for a
// dependency tree. See {@link bootstrapOrchestratorModules} for why writing node_modules into the
// checkout is allowed where writing `dist` is not.
func stageOrchestrator(opts BuildOptions, tc *toolchain, m *Manifest, p func(string, ...any)) error {
	src := filepath.Join(opts.RepoRoot, "control", "orchestrator")
	if err := bootstrapOrchestratorModules(src, tc, p); err != nil {
		return err
	}
	tsc := filepath.Join(src, "node_modules", "typescript", "bin", "tsc")
	if _, err := os.Stat(tsc); err != nil {
		return fmt.Errorf("control/orchestrator/node_modules/typescript is missing, so there is nothing to compile with: %w", err)
	}
	nodeExe := tc.Node

	out := filepath.Join(opts.StageDir, "orchestrator", "dist")
	p("compiling the orchestrator (tsc --outDir %s)", out)
	cmd := exec.Command(nodeExe, tsc, "--project", "tsconfig.json", "--outDir", out)
	cmd.Dir = src
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tsc failed in %s: %w\n%s", src, err, indent(string(combined)))
	}

	// package.json travels with it, and is not decoration. Node reads the nearest one to decide
	// whether `.js` is CommonJS or ESM, and every file tsc just emitted is CommonJS. Without it,
	// `main.js` fails at its first `require` on a machine whose Node defaults differ.
	if err := copyFile(filepath.Join(src, "package.json"), filepath.Join(opts.StageDir, "orchestrator", "package.json")); err != nil {
		return err
	}

	tree, files, bytes, err := treeDigest(out)
	if err != nil {
		return err
	}
	version, err := packageVersion(filepath.Join(src, "package.json"))
	if err != nil {
		return err
	}
	tscVersion, err := runCapture(nodeExe, tsc, "--version")
	if err != nil {
		return err
	}
	m.Toolchain["tsc"] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tscVersion), "Version"))

	m.Components = append(m.Components, Component{
		Name:       "orchestrator",
		Kind:       "built",
		Version:    version,
		Path:       "orchestrator/dist",
		TreeSHA256: tree,
		Files:      files,
		Bytes:      bytes,
		Note:       "tsc output for control/orchestrator/tsconfig.json; the SPA is a separate artifact and is not here",
	})
	return nil
}

// stageDependencies resolves the production tree from the committed lockfile.
//
// `--frozen-lockfile` IS THE REPRODUCIBILITY. It is what makes "the bundle contains the
// dependencies this repo committed" a checkable sentence rather than "the bundle contains
// whatever the registry was serving this morning"; without it a caret range silently floats and
// two builds a week apart differ for reasons no digest explains.
//
// `--node-linker=hoisted` IS THE RELOCATABILITY. pnpm's default layout is a symlink farm into
// `node_modules/.pnpm`, and a tree of symlinks is a tree whose meaning depends on being copied
// with the right flags by every tool that ever touches it. Hoisted is a plain directory that
// Node's own resolution walks, which is what an unpacked bundle has to be.
//
// `--prod` IS THE SIZE, and it is not only size: vitest, typescript's dev copy and the whole test
// toolchain have no business in a shipped artifact, and every one of them is a package that could
// carry a platform binary the manifest would then have to explain.
//
// `--os`, `--cpu` AND `--libc` ARE THE FOUR PLATFORMS. Without them pnpm resolves the tree for the
// process it is running IN — `@duckdb/node-bindings-linux-x64` and `@swc/core-linux-x64-gnu` on
// this box, whatever `--platform` said — and the resulting bundle is a linux bundle wearing a
// darwin manifest. It archives, it verifies against itself, and it fails at the first `require` on
// the machine it was supposedly built for. The lockfile already carries every variant's integrity
// hash, so nothing about the pinning weakens; see {@link npmArchitecture} for why libc is answered
// for linux and withheld for macOS.
//
// `--ignore-scripts` IS NOT AN OPTIMISATION, IT IS THE macOS FALLBACK PATH THE SPEC WARNED ABOUT.
// `@swc/core`'s postinstall exists to "check if corresponding optional dependencies for native
// binary is installed and can be loaded properly. If it fails, it'll internally try to install
// `@swc/wasm` as fallback" — its own words. On a cross build that check runs on the BUILDING
// machine against the TARGET's binding, so it always fails, and the repair it reaches for is an
// unpinned package the lockfile never resolved. A silent wasm substitution for a 28 MB native
// addon is the exact failure a per-platform manifest exists to make impossible.
//
// Nothing in the production tree needs a script to run: swc's is a self-check, protobufjs's writes
// a warning about version schemes, and esbuild's is dev-only and not installed under `--prod`.
// Skipping them makes the native and cross paths produce identical trees, which is worth more than
// either path being slightly more thorough on its own.
//
// AND THIS IS WHERE THE "EVERY FETCHED ARTIFACT IS VERIFIED AGAINST A PINNED DIGEST" PROMISE IS
// SOMEBODY ELSE'S. {@link fetchPinned} verifies the artifacts this file fetches itself — the Node
// runtimes and pnpm. The other ~330 MB arrives through pnpm, which checks each package tarball
// against the `integrity` hash the committed lockfile pins before it enters its store. That is the
// same property by a different mechanism, and it is worth writing down rather than letting the
// promise read as this file's: what backs it here is `pnpm-lock.yaml`, which is why the lockfile's
// own sha256 is a component of the manifest and why `--frozen-lockfile` is not optional. Drop the
// flag and the pins stop being pins.
// The orchestrator's package NAME, which is not its directory name and never has been. The
// directory is `control/orchestrator`; the package is `@kontra/backend`, and `pnpm --filter` takes
// the package. Spelled once here because getting it wrong deploys NOTHING and says so only as an
// empty target directory two steps later.
const orchestratorPackage = "@kontra/backend"

func stageDependencies(opts BuildOptions, tc *toolchain, m *Manifest, p func(string, ...any)) error {
	dst := filepath.Join(opts.StageDir, "orchestrator")

	// ── THE LOCKFILE IS AT THE WORKSPACE ROOT, AND THIS LOOKED IN THE PACKAGE ──────────────────
	//
	// It read `control/orchestrator/pnpm-lock.yaml`, which has not existed since `@kontra/core`
	// became a workspace package: pnpm keeps ONE lockfile at the root of a workspace, with an
	// `importers:` entry per package. So `kontra release` failed at this line, every time, with
	// `no such file or directory` — which is why this repository has zero tags and why
	// `release.yml` has never executed. The first release is what found it.
	lock := filepath.Join(opts.RepoRoot, "pnpm-lock.yaml")
	lockSum, err := sha256File(lock)
	if err != nil {
		return fmt.Errorf("the orchestrator's lockfile is what pins the dependency tree: %w", err)
	}
	if err := copyFile(lock, filepath.Join(dst, "pnpm-lock.yaml")); err != nil {
		return err
	}

	npmOS, cpu, libc, err := npmArchitecture(opts.Platform)
	if err != nil {
		return err
	}
	if err := requireFreeSpace(opts.StageDir, minFreeBytes); err != nil {
		return err
	}

	// ── `pnpm deploy`, NOT `pnpm install`, AND THE WORKSPACE IS WHY ────────────────────────────
	//
	// Copying the lockfile into a bare directory and installing there cannot work in a workspace,
	// for two reasons that are both fatal and neither of which is about the path:
	//
	//   THE IMPORTERS DO NOT MATCH. A workspace lockfile keys its `importers:` by package path
	//   (`.`, `control/orchestrator`, `shared/core`). A standalone directory is importer `.`, so
	//   `--frozen-lockfile` refuses with "specifiers in the lockfile don't match specifiers in
	//   package.json" and lists all 33 — MEASURED, not predicted.
	//
	//   AND `@kontra/core` IS `workspace:*`. There is no registry tarball for it; the only thing
	//   that can resolve it is a workspace. A standalone install would fail on it even if the
	//   importers lined up.
	//
	// `pnpm deploy` is pnpm's own answer: it resolves FROM the workspace and writes a
	// self-contained tree, with the workspace dependency MATERIALISED rather than symlinked out to
	// a directory the bundle will not contain. `--legacy` because this workspace does not set
	// `inject-workspace-packages`.
	//
	// DEPLOYED ASIDE, THEN ONLY `node_modules` MOVED IN. `deploy` also copies the package's own
	// source — `src/`, `contract/`, `bruno/`, the tests — because the orchestrator's package.json
	// declares no `files`. None of that belongs in a runtime bundle, and adding a `files` field to
	// satisfy this build would change what publishing means elsewhere. The bundle's layout is a
	// published contract (`orchestrator/dist/src/main.js`), so it stays exactly as it was and this
	// takes the one directory it came for.
	work, err := os.MkdirTemp(filepath.Dir(opts.StageDir), "kontra-deploy-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	// `deploy` REQUIRES AN EMPTY OR ABSENT TARGET, and MkdirTemp just made the parent.
	target := filepath.Join(work, "out")

	// ── DEPLOY FROM A THROWAWAY COPY OF THE WORKSPACE, NOT FROM THE CHECKOUT ──────────────────
	//
	// `pnpm deploy --prod` run in the repository PRUNES THAT REPOSITORY'S `node_modules` to
	// production-only as a side effect. MEASURED here, the hard way: after a release build the
	// orchestrator's own test suite failed with `Cannot find module '@protobufjs/aspromise'` — a
	// dev dependency the build had quietly removed from the developer's working tree. A release
	// that breaks the checkout it was cut from is not a release step, it is a trap.
	//
	// So the deploy runs against a workspace built for it: the metadata that decides resolution,
	// and nothing else. Verified to produce the identical 287-package tree while leaving the source
	// workspace's `node_modules` untouched (0 entries).
	if err := copyWorkspaceMetadata(opts.RepoRoot, work); err != nil {
		return err
	}

	args := []string{"deploy", "--filter", orchestratorPackage, "--prod", "--legacy",
		"--frozen-lockfile", "--ignore-scripts", "--node-linker=hoisted",
		"--config.confirmModulesPurge=false", "--os", npmOS, "--cpu", cpu}
	if libc != "" {
		args = append(args, "--libc", libc)
	}
	args = append(args, target)
	p("deploying production dependencies for %s (pnpm deploy --prod --frozen-lockfile --os %s --cpu %s)", opts.Platform, npmOS, cpu)
	if combined, err := tc.pnpmCmd(work, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("pnpm deploy failed for %s: %w\n%s", opts.Platform, err, indent(string(combined)))
	}
	if err := os.Rename(filepath.Join(target, "node_modules"), filepath.Join(dst, "node_modules")); err != nil {
		return fmt.Errorf("move the deployed dependencies into the bundle: %w", err)
	}

	// THE PIN, NOT `pnpm --version`. The version used to be asked of the program on PATH, in the
	// staging directory rather than the repo root, because `pnpm` there is a corepack shim that
	// resolves `packageManager` from the NEAREST package.json — and asking at the wrong depth
	// answered 11.24.0 for an install that ran under 10.33.2. That whole hazard is gone now that
	// the build runs a pnpm it fetched itself: {@link resolveToolchain} has already executed
	// `--version` on it and refused if it disagreed with the pin, so this records a number that
	// was checked rather than one that was observed.
	m.Toolchain["pnpm"] = tc.PnpmVersion

	modules := filepath.Join(dst, "node_modules")
	if err := prunePnpmMetadata(modules, p); err != nil {
		return err
	}
	excluded, err := pruneForeignPrebuilds(modules, opts.Platform, p)
	if err != nil {
		return err
	}
	// BEFORE THE DIGEST, like the two prunes above it: what is measured has to be what ships.
	if _, err := pruneBuildSource(modules, p); err != nil {
		return err
	}
	tree, files, bytes, err := treeDigest(modules)
	if err != nil {
		return err
	}
	m.Components = append(m.Components, Component{
		Name:       "orchestrator-dependencies",
		Kind:       "dependencies",
		Version:    "sha256:" + lockSum[:16],
		// INSIDE THE ARCHIVE, WHICH IS NOT WHERE THE REPO KEEPS IT. Every `Path` on a Component is
		// relative to the extracted bundle, and the bundle's layout is a published contract — the
		// `run it:` line tells people to exec `orchestrator/dist/src/main.js`. The repo's own copy
		// moved to `backend/` in ADR 0035; this did not, and must not. It read `control/orchestrator/node_modules`
		// for one commit because that string IS the right answer twice elsewhere in this file (the
		// tsc lookup and the install probe, both genuinely repo-relative), so a rewrite that checked
		// its work locally looked correct. The neighbouring component two lines down still said
		// `orchestrator/pnpm-lock.yaml`, which is what the pair now asserts.
		Path:       "orchestrator/node_modules",
		TreeSHA256: tree,
		Files:      files,
		Bytes:      bytes,
		Note:       "pnpm deploy --prod --frozen-lockfile --legacy --node-linker=hoisted; version is the head of the lockfile's own sha256",
	})
	m.Components = append(m.Components, Component{
		Name:    "pnpm-lock.yaml",
		Kind:    "built",
		Version: "lockfileVersion " + lockfileVersion(lock),
		Path:    "orchestrator/pnpm-lock.yaml",
		SHA256:  lockSum,
		Note:    "carried so the dependency tree in this bundle can be re-resolved and compared",
	})
	m.excludedPrebuilds = excluded
	return nil
}

// treeDigest is a stable digest over a directory, and it is defined so a human can reproduce it.
//
// It is the sha256 of one line per entry, sorted by path:
//
//	<sha256 of the file>            <slash/relative/path>\n
//	symlink:<target>                <slash/relative/path>\n
//
// So `find . -type f | sort | xargs sha256sum` and this agree on the file half, which is what
// makes the number checkable by somebody who does not trust this program. Directories contribute
// nothing: an empty directory carries no content, and Node's resolution never notices one.
func treeDigest(root string) (string, int64, int64, error) {
	type line struct {
		rel  string
		text string
	}
	var lines []line
	var files, bytes int64

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." || d.IsDir() {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			lines = append(lines, line{rel, "symlink:" + filepath.ToSlash(target) + "  " + rel + "\n"})
			return nil
		}
		sum, err := sha256File(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		lines = append(lines, line{rel, sum + "  " + rel + "\n"})
		return nil
	})
	if err != nil {
		return "", 0, 0, err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].rel < lines[j].rel })
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l.text))
	}
	return hex.EncodeToString(h.Sum(nil)), files, bytes, nil
}

// summarizeTree states the whole bundle's scale in the manifest.
func summarizeTree(stage string) (TreeSummary, error) {
	digest, files, bytes, err := treeDigest(stage)
	if err != nil {
		return TreeSummary{}, err
	}
	var symlinks int64
	err = filepath.WalkDir(stage, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			symlinks++
		}
		return nil
	})
	if err != nil {
		return TreeSummary{}, err
	}
	return TreeSummary{Files: files, Symlinks: symlinks, Bytes: bytes, SHA256: digest}, nil
}

// encodeManifest renders the manifest deterministically.
//
// Indented and with `SetEscapeHTML(false)`, because this file is read by people: a URL whose `&`
// has become `&` is a URL somebody has to un-mangle before they can paste it. Go's encoder
// writes struct fields in declaration order and map keys sorted, so the bytes are a function of
// the values alone.
func encodeManifest(m *Manifest) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// --- plumbing -----------------------------------------------------------------------------

func (o BuildOptions) resolve() (BuildOptions, error) {
	if o.Platform == (Platform{}) {
		o.Platform = HostPlatform()
	}
	if o.RepoRoot == "" {
		return o, errors.New("BuildOptions.RepoRoot is required: the bundle is built from a checkout")
	}
	abs, err := filepath.Abs(o.RepoRoot)
	if err != nil {
		return o, err
	}
	o.RepoRoot = abs
	if _, err := os.Stat(filepath.Join(o.RepoRoot, "control", "orchestrator", "package.json")); err != nil {
		return o, fmt.Errorf("%s does not look like the kontra checkout (no control/orchestrator/package.json)", o.RepoRoot)
	}
	if o.OutDir == "" {
		o.OutDir = filepath.Join(o.RepoRoot, "build", "bundles")
	}
	if o.OutDir, err = filepath.Abs(o.OutDir); err != nil {
		return o, err
	}
	if o.CacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return o, err
		}
		o.CacheDir = filepath.Join(home, ".cache", "kontra", "appliance-artifacts")
	}
	if o.CacheDir, err = filepath.Abs(o.CacheDir); err != nil {
		return o, err
	}
	if o.StageDir == "" {
		o.StageDir = filepath.Join(o.OutDir, ".stage-"+o.Platform.OS+"-"+o.Platform.Arch)
	}
	if o.StageDir, err = filepath.Abs(o.StageDir); err != nil {
		return o, err
	}
	return o, nil
}

// machinePaths are the directories this build used, and the ones {@link scanStaged} refuses to
// find inside the artifact. See scan.go's header for why `$HOME` is not among them.
//
// THE STORE IS ASKED OF THE PNPM THAT PRODUCED THE TREE. pnpm's store is wherever the user
// configured it, and its path would arrive in the staged tree the same way any other build path
// would — but two pnpms can have two stores, and a scan that clears the tree of the WRONG store's
// path has checked nothing, quietly and while reporting success.
//
// nil FOR THE SPA, AND THAT IS NOT AN OVERSIGHT. `BuildSPA` runs no package manager at all:
// it archives a vite output somebody else built, so the store that could have left a path in it is
// the developer's, on PATH. Fetching a 120 MB Node to ask a question about somebody else's build
// would be the wrong trade, and a best-effort `pnpm` is the honest answer to a best-effort
// question.
func (o BuildOptions) machinePaths(tc *toolchain) []string {
	paths := []string{o.RepoRoot, o.StageDir, o.OutDir, o.CacheDir}
	name, args := "pnpm", []string{"store", "path"}
	if tc != nil {
		name, args = tc.Pnpm[0], append(append([]string{}, tc.Pnpm[1:]...), args...)
	}
	if store, err := runCapture(name, args...); err == nil {
		paths = append(paths, strings.TrimSpace(store))
	}
	return paths
}

func requireFreeSpace(path string, floor int64) error {
	return requireFreeSpaceFor(path, floor, "build")
}

// requireFreeSpaceFor is requireFreeSpace with the WORK NAMED, because the two callers are two
// different moments in an operator's day. A build that refuses says "free some space before
// building"; a hydration that refuses is happening inside `kontra up`, where the same sentence
// would send somebody looking for a build they did not run.
func requireFreeSpaceFor(path string, floor int64, what string) error {
	// The directory may not exist yet; ask about the nearest ancestor that does.
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // nothing to ask about; let the write fail with its own message
		}
		dir = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return nil // an unreadable statfs must not stop a build; the write will say so instead
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free >= floor {
		return nil
	}
	return fmt.Errorf("only %s free on %s and this %s needs about %s of working space.\n"+
		"  A full disk on this system does not fail loudly — it turns object-store writes into bare 500s.\n"+
		"  Free some space (or point --data-dir / --out at a larger filesystem) first",
		humanBytes(free), dir, what, humanBytes(floor))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func packageVersion(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var meta struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return meta.Version, nil
}

// runCapture asks a program a question and returns its answer.
//
// NO WORKING DIRECTORY, and there used to be one. `runCaptureIn` existed because `pnpm --version`
// had to be asked INSIDE the staging tree: the `pnpm` on PATH is a corepack shim, corepack
// resolves `packageManager` from the nearest package.json, and asking at the repo root answered
// 11.24.0 for an install that ran under 10.33.2. The build now runs a pnpm it fetched and verified
// itself (toolchain.go), so the question has one answer wherever it is asked and the
// directory-carrying variant had no callers left.
func runCapture(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// lockfileVersion reads pnpm's own schema version off the top of the lockfile. It is a property of
// the FORMAT, not of the dependencies — those are identified by the file's sha256 in the component
// beside this one — and it is what tells a reader whether a future pnpm can still consume this.
func lockfileVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "lockfileVersion:")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(rest), "'\"")
	}
	return "unknown"
}

func hasAnySuffix(name string, suffixes []string) bool {
	lower := strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func progress(w io.Writer) func(string, ...any) {
	if w == nil {
		return func(string, ...any) {}
	}
	start := time.Now()
	return func(format string, args ...any) {
		fmt.Fprintf(w, "  [%5.1fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
}
