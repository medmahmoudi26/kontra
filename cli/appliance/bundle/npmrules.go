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
