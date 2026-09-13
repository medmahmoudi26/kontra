// npmrules.go — what the npm ecosystem says about a dependency tree, separated from the pipeline
// that stages one.
//
// TWO MODULES SHARED A FILE. `stage.go` answers "what goes into the bundle, in what order, and
// what does the manifest then claim"; this answers a different question entirely — which files in
// a `node_modules` belong to THIS platform, and which are somebody else's. That second question
// is not about kontra at all. It is about `os`/`cpu`/`libc` in package.json, about the naming
// conventions prebuild-install and node-gyp-build use, about Rust target triples, and about
// pnpm's own bookkeeping. Those conventions change when npm changes, on a schedule nothing in
// this repo controls, and the edit they force should not be an edit to the staging order.
//
// THE STAKES ARE THE DIGEST AND THE SIZE, in that order. A file with a wall clock in it makes the
// bundle's address a function of when it was built (prunePnpmMetadata); a foreign prebuild makes
// it 300 MB of binaries that will never load on the target (pruneForeignPrebuilds) — and a
// prebuild pruned by mistake is an addon that is missing at runtime on the machine that pulls it,
// which is why every exclusion is RECORDED in the manifest rather than merely done.
//
// NOTHING HERE KNOWS WHAT A BUNDLE IS. The functions take a directory and a Platform and answer
// about files. That is the seam: they can be read, and got wrong, without reading stage.go.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// prunePnpmMetadata removes pnpm's own bookkeeping from the staged tree.
//
// THREE FILES, AND TWO OF THEM WOULD BREAK THE DIGEST OUTRIGHT. Measured on this box:
//
//	.modules.yaml                   contains an absolute path from the building machine
//	.pnpm-workspace-state-v1.json   contains `lastValidatedTimestamp`, a wall clock
//	.pnpm/lock.yaml                 pnpm's install lock, meaningless once the tree is frozen
//
// A timestamp in a content-addressed artifact makes its address a function of when it was built,
// which is the failure this whole file is arranged to avoid. None of the three is read by Node's
// module resolution — they exist so a LATER `pnpm install` can be incremental, and there is never
// a later install into a bundle.
func prunePnpmMetadata(modules string, p func(string, ...any)) error {
	for _, name := range []string{".modules.yaml", ".pnpm-workspace-state-v1.json", ".pnpm"} {
		path := filepath.Join(modules, name)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		p("dropping pnpm metadata node_modules/%s", name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// pruneBuildSource removes source trees a dependency ships for BUILDING itself and that nothing
// loads at runtime.
//
// ONE PACKAGE TODAY, AND IT BLOCKED THE FIRST RELEASE. `@temporalio/core-bridge` publishes its
// whole Rust workspace — `sdk-core/`, 11 MB of crates — beside the prebuilt `.node` files that
// actually get loaded. `common.js:getPrebuiltPath` resolves `releases/<triple>/index.node` and
// nothing else; the crates are there so somebody can build the bridge themselves, which is not a
// thing that happens inside a bundle.
//
// IT IS NOT PRIMARILY A SIZE FIX. Inside that tree is
// `crates/client/tests/testdata/ca.pem`, and `scanStaged` refuses to publish any bundle carrying a
// `*.pem` — correctly, because it cannot tell a test fixture from a private key and the failure
// direction of guessing is a credential in a world-readable artifact. So `kontra release` stopped
// dead here, and the honest fix is to not ship build material rather than to teach the scanner
// which certificates are harmless.
//
// ABSENT IS FINE. A dependency that stops shipping its source is not a problem to report — this
// removes what is there and says what it removed.
func pruneBuildSource(modules string, p func(string, ...any)) ([]string, error) {
	// Package-relative paths, so the rule reads as "this package ships this, and we do not need
	// it" rather than as a glob that might match something else entirely one day.
	sources := []string{
		filepath.Join("@temporalio", "core-bridge", "sdk-core"),
	}
	var dropped []string
	for _, rel := range sources {
		path := filepath.Join(modules, rel)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.IsDir() {
			continue
		}
		p("dropping build source node_modules/%s (not loaded at runtime)", filepath.ToSlash(rel))
		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("remove %s: %w", path, err)
		}
		dropped = append(dropped, filepath.ToSlash(rel))
	}
	return dropped, nil
}

// pruneForeignPrebuilds keeps the one `@temporalio/core-bridge` prebuild this platform loads and
// removes the rest, returning what it removed.
//
// The package ships five `releases/<triple>/index.node` files totalling 140 MB and resolves one at
// require time from `os.arch()`/`os.platform()` — see its `common.js`, which {@link rustTriple}
// mirrors. Keeping all five would mean a "per-platform" bundle four-fifths of whose largest
// component is unreachable, and a manifest that could not honestly say which build it carries.
//
// IT REFUSES RATHER THAN GUESSES if the one it wants is absent: a bundle missing its Temporal core
// still archives, still hydrates, and fails at the first `NativeConnection.connect` with a
// PrebuildError naming a path — a long way from here, in a process nobody is watching.
func pruneForeignPrebuilds(modules string, platform Platform, p func(string, ...any)) ([]string, error) {
	releases := filepath.Join(modules, "@temporalio", "core-bridge", "releases")
	entries, err := os.ReadDir(releases)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist: @temporalio/core-bridge no longer ships prebuilt binaries where this expects them,\n"+
			"  so the bundle's Temporal core is unaccounted for. Check the package layout before changing this", releases)
	}
	if err != nil {
		return nil, err
	}
	want, err := rustTriple(platform)
	if err != nil {
		return nil, err
	}
	var kept bool
	var dropped []string
	for _, e := range entries {
		if e.Name() == want {
			kept = true
			continue
		}
		dropped = append(dropped, e.Name())
		if err := os.RemoveAll(filepath.Join(releases, e.Name())); err != nil {
			return nil, err
		}
	}
	if !kept {
		return nil, fmt.Errorf("@temporalio/core-bridge ships no prebuild for %s (looked for releases/%s, found %s)",
			platform, want, strings.Join(dropped, ", "))
	}
	sort.Strings(dropped)
	if len(dropped) > 0 {
		p("keeping the %s Temporal core; dropped %d other prebuilds", want, len(dropped))
	}
	return dropped, nil
}

// --- enumeration --------------------------------------------------------------------------

// nativeSuffixes are what a compiled artifact looks like on disk. `.node` is a Node addon;
// `.so`/`.dylib` are the shared libraries those addons link against — `@duckdb/node-bindings`
// splits itself that way, with a 376 KB addon in front of a 68 MB libduckdb.so, and a manifest
// that recorded only the addon would be naming the smaller half.
var nativeSuffixes = []string{".node", ".so", ".dylib", ".dll"}

// collectNativeAddons walks the staged tree and records every compiled artifact in it.
//
// BY WALKING, NOT BY LIST. A hand-maintained list of the four packages we know about is a list
// that is wrong the first time a dependency gains a binary, and it would be wrong silently — the
// bundle would carry an addon the manifest never mentions, which is precisely the "works on the
// machine that built it" failure this manifest exists to prevent.
func collectNativeAddons(stage string, m *Manifest) error {
	err := filepath.WalkDir(stage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || !hasAnySuffix(d.Name(), nativeSuffixes) {
			return nil
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := sha256File(path)
		if err != nil {
			return err
		}
		pkg, version := owningPackage(path, stage)
		m.NativeAddons = append(m.NativeAddons, NativeAddon{
			Package:        pkg,
			Version:        version,
			Path:           filepath.ToSlash(rel),
			SHA256:         sum,
			Bytes:          info.Size(),
			Build:          vendorBuildName(filepath.ToSlash(rel), pkg),
			ExcludedBuilds: excludedFor(pkg, m.excludedPrebuilds),
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(m.NativeAddons, func(i, j int) bool { return m.NativeAddons[i].Path < m.NativeAddons[j].Path })
	return nil
}

// excludedFor attaches the pruned sibling list to the addon it is about, and to nothing else.
func excludedFor(pkg string, excluded []string) []string {
	if pkg != "@temporalio/core-bridge" || len(excluded) == 0 {
		return nil
	}
	return excluded
}

// vendorBuildName is the vendor's own target name for an artifact, read off the path rather than
// re-derived: `releases/x86_64-unknown-linux-gnu/index.node` for the Rust bridge, and the platform
// suffix of the npm package name for the ones npm splits by platform.
func vendorBuildName(rel, pkg string) string {
	if i := strings.Index(rel, "/core-bridge/releases/"); i >= 0 {
		rest := rel[i+len("/core-bridge/releases/"):]
		if triple, _, ok := strings.Cut(rest, "/"); ok {
			return triple
		}
	}
	// `@duckdb/node-bindings-linux-x64` → `linux-x64`; `@swc/core-linux-x64-gnu` → `linux-x64-gnu`.
	for _, marker := range []string{"-linux-", "-darwin-", "-win32-", "-android-", "-freebsd-"} {
		if i := strings.Index(pkg, marker); i >= 0 {
			return pkg[i+1:]
		}
	}
	return ""
}

// owningPackage finds the nearest enclosing package.json and reads its name and version, so the
// manifest can say `@duckdb/node-bindings-linux-x64 1.5.4-r.1` rather than repeating a path.
func owningPackage(path, stage string) (string, string) {
	dir := filepath.Dir(path)
	for {
		if !strings.HasPrefix(dir, stage) {
			return "", ""
		}
		pj := filepath.Join(dir, "package.json")
		if b, err := os.ReadFile(pj); err == nil {
			var meta struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if json.Unmarshal(b, &meta) == nil && meta.Name != "" {
				return meta.Name, meta.Version
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// copyWorkspaceMetadata builds the smallest workspace pnpm needs to resolve the same tree.
//
// WHY A COPY AT ALL: `pnpm deploy --prod` prunes the workspace it runs in to production-only. Run
// against the checkout that is being released, it deletes the developer's dev dependencies —
// measured, as an orchestrator suite that started failing on `@protobufjs/aspromise` after a
// release build. The deploy needs the metadata that decides resolution and nothing else, so it gets
// exactly that, in a directory that is thrown away.
//
// WHAT RESOLUTION ACTUALLY NEEDS: the lockfile (which pins every version and integrity hash), the
// workspace file (which says where the packages are), and one `package.json` per package. Plus
// `shared/core`'s BUILT OUTPUT, because `@kontra/core` is a `workspace:*` dependency that `deploy`
// materialises by copying — a package.json alone would copy an empty package and the bundle would
// fail at its first `require` on a machine nobody is watching.
func copyWorkspaceMetadata(repo, dst string) error {
	for _, name := range []string{"pnpm-lock.yaml", "pnpm-workspace.yaml", "package.json"} {
		if err := copyFile(filepath.Join(repo, name), filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("the deploy workspace needs %s: %w", name, err)
		}
	}

	packages, err := workspacePackages(filepath.Join(repo, "pnpm-workspace.yaml"))
	if err != nil {
		return err
	}
	// A WORKSPACE WITH NO PACKAGES WOULD DEPLOY NOTHING AND SAY SO ONLY AS AN EMPTY TARGET — the
	// guard on this guard, because a parser that quietly found none is exactly the shape that
	// passes every test written against it.
	if len(packages) == 0 {
		return fmt.Errorf("no packages found in %s — the deploy workspace would resolve nothing",
			filepath.Join(repo, "pnpm-workspace.yaml"))
	}

	for _, rel := range packages {
		if err := os.MkdirAll(filepath.Join(dst, rel), 0o755); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(repo, rel, "package.json"),
			filepath.Join(dst, rel, "package.json")); err != nil {
			return fmt.Errorf("workspace package %s: %w", rel, err)
		}
		// The built output of a workspace package that others DEPEND on. `deploy` copies what the
		// dependency's `files`/directory holds; an unbuilt one copies nothing that can be required.
		src := filepath.Join(repo, rel, "dist")
		if info, err := os.Stat(src); err == nil && info.IsDir() {
			if err := copyTree(src, filepath.Join(dst, rel, "dist")); err != nil {
				return fmt.Errorf("workspace package %s dist: %w", rel, err)
			}
		}
	}
	return nil
}

// workspacePackages reads the `packages:` list out of a pnpm-workspace.yaml.
//
// A LINE SCAN AND NOT A YAML PARSER, because the file is four lines of literal paths and adding a
// YAML dependency to the appliance build — whose whole claim is that it needs nothing but Go — to
// read them would be the wrong trade. It REFUSES A GLOB rather than trying to expand one: a
// `packages: - control/*` would silently resolve to nothing here, and a bundle missing a workspace
// package fails at require time in somebody else's process.
func workspacePackages(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the workspace file: %w", err)
	}
	var out []string
	inPackages := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "packages:") {
			inPackages = true
			continue
		}
		if inPackages && !strings.HasPrefix(trimmed, " ") && !strings.HasPrefix(trimmed, "-") &&
			strings.TrimSpace(trimmed) != "" {
			break // a new top-level key ends the list
		}
		if !inPackages {
			continue
		}
		item := strings.TrimSpace(trimmed)
		if !strings.HasPrefix(item, "- ") {
			continue
		}
		item = strings.Trim(strings.TrimSpace(strings.TrimPrefix(item, "- ")), `"'`)
		if item == "" {
			continue
		}
		if strings.ContainsAny(item, "*?[") {
			return nil, fmt.Errorf(
				"%s lists the glob %q; this build copies workspace packages by literal path and "+
					"would silently miss whatever it matches — expand it, or teach "+
					"workspacePackages to", path, item)
		}
		out = append(out, filepath.FromSlash(item))
	}
	return out, nil
}
