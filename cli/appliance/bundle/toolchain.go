// toolchain.go — the programs the bundle build runs, fetched and pinned like everything else
// (ADR 0031 §2, issue 17).
//
// WHAT THIS FILE IS FOR, IN ONE SENTENCE: "buildable from a clean checkout of GitHub with nothing
// installed but Go" is a claim about what the build ASKS OF THE MACHINE, and until this file
// existed the build asked for a Node and a pnpm it did not name.
//
// The old shape looked pinned and was not. `stageDependencies` began with `exec.LookPath("pnpm")`
// and `stageOrchestrator` began with a `stat` of `control/orchestrator/node_modules/typescript` — so the
// two programs that decide what the bundle CONTAINS (pnpm resolves 330 MB of dependencies, tsc
// emits every byte of the JavaScript) arrived from whatever the operator happened to have. The
// manifest recorded their versions afterwards, which is the wrong way round: recording the version
// of a tool you did not choose documents the accident instead of preventing it.
//
// So both are fetched, both are verified against a pinned digest, and neither is looked for on
// PATH:
//
//	node    the same {@link NodeVersion} the bundle carries, built for the HOST — because on a
//	        cross build the bundled runtime is a Mach-O binary this machine cannot exec
//	pnpm    the {@link PnpmVersion} npm tarball, which is JavaScript and runs on that node
//
// tsc is not in that list and cannot be: it is the project's own, from the orchestrator's
// dependency tree, pinned by `pnpm-lock.yaml`. What this file adds there is the bootstrap — a
// clean checkout has no `node_modules`, so the toolchain installs one rather than telling the
// operator to go and run something first.
//
// THE HOST NODE IS FETCHED EVEN WHEN IT IS THE SAME PLATFORM AS THE TARGET, and that is a
// deliberate ~120 MB in the cache. The alternative — reuse the staged runtime when host and target
// agree, fetch a second one when they do not — makes the cross build a different code path from
// the native one, and a different code path is one that is exercised by one CI runner instead of
// four. It is extracted once and shared by every platform's build and every rebuild.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// toolchain is the resolved set of programs one build runs, all of them under CacheDir and none of
// them from PATH.
type toolchain struct {
	// Node runs on the HOST. Not the runtime that goes into the bundle — on a cross build those
	// are different files for different architectures, and confusing them is an `exec format
	// error` several minutes into a build.
	Node string

	// Pnpm is an argv PREFIX rather than a path, because pnpm ships as JavaScript: running it is
	// `<node> <pnpm.cjs>`, and callers that need to append arguments should not have to know that.
	Pnpm []string

	NodeVersion string
	PnpmVersion string
}

// pnpmCmd builds a pnpm invocation in a directory.
func (t *toolchain) pnpmCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(t.Pnpm[0], append(append([]string{}, t.Pnpm[1:]...), args...)...)
	cmd.Dir = dir
	// A build must not become interactive and must not consult a per-user update notifier: both
	// turn a CI failure into a hang. `CI=1` is the switch pnpm itself documents for that.
	//
	// COREPACK_ENABLE_STRICT=0 is here because this pnpm is NOT corepack's. Running `pnpm.cjs`
	// directly is what makes the version a pin rather than a consequence of the operator's
	// `packageManager` shim, and pnpm 10 will otherwise try to hand off to the version named in
	// package.json — a network round trip to arrive at the version it is already running.
	//
	// AND THE BUILD'S OWN NODE GOES ON PATH, because pnpm is not the only thing that runs it.
	// A package's `postinstall` is a SHELL LINE — `node postinstall.js` — spawned by pnpm through
	// `sh`, which resolves `node` from PATH and knows nothing about the interpreter pnpm itself
	// was launched with. On a machine that has a node the two silently differ; on the machine this
	// whole code path exists for they do not differ, they are absent, and `@swc/core`,
	// `protobufjs` and `esbuild` all fail with `sh: 1: node: not found`. MEASURED on the
	// clean-checkout job, 2026-08-27: node fetched, pnpm fetched, 518 packages resolved, install
	// dead at the first postinstall. Prepended rather than appended, so a machine that does have
	// one still builds against the pinned version — which is the claim this toolchain makes.
	cmd.Env = append(os.Environ(),
		"CI=1",
		"COREPACK_ENABLE_STRICT=0",
		"npm_config_manage_package_manager_versions=false",
		"PATH="+filepath.Dir(t.Node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return cmd
}

// resolveToolchain fetches and verifies the build's own programs.
//
// IDEMPOTENT AND CHEAP ON THE SECOND CALL. Everything lands under `<cache>/toolchain/<name>-<version>`,
// a path that names the pin, so "is it already here" is a stat and a version check rather than a
// re-download. The verification that matters — the digest — happened when the artifact entered
// {@link fetchPinned}'s content-addressed cache, and that cache re-verifies on every hit.
func resolveToolchain(opts BuildOptions, p func(string, ...any)) (*toolchain, error) {
	host := HostPlatform()

	nodePin, err := NodePin(host)
	if err != nil {
		return nil, fmt.Errorf("the build needs a Node of its own to run pnpm and tsc with: %w", err)
	}
	node, err := hostNode(opts, nodePin, host, p)
	if err != nil {
		return nil, err
	}

	pnpmPin, err := PnpmPin()
	if err != nil {
		return nil, err
	}
	pnpm, err := hostPnpm(opts, pnpmPin, p)
	if err != nil {
		return nil, err
	}

	tc := &toolchain{
		Node:        node,
		Pnpm:        []string{node, pnpm},
		NodeVersion: NodeVersion,
		PnpmVersion: PnpmVersion,
	}

	// THE VERSION IS ASKED OF THE PROGRAM, NOT ASSUMED FROM THE PATH IT WAS WRITTEN TO. A
	// half-extracted or hand-edited toolchain directory would otherwise be reported in the
	// manifest as the version its directory name claims, which is a checkable statement that
	// happens to be false — the worst kind to put in an artifact.
	out, err := runCapture(tc.Pnpm[0], append(tc.Pnpm[1:], "--version")...)
	if err != nil {
		return nil, fmt.Errorf("the fetched pnpm does not run: %w", err)
	}
	if got := strings.TrimSpace(out); got != PnpmVersion {
		return nil, fmt.Errorf("the fetched pnpm reports %s, but the pin says %s", got, PnpmVersion)
	}
	return tc, nil
}

// hostNode extracts `bin/node` for the building machine out of the pinned tarball.
//
// It runs `--version` on what it extracted every time, not only after a fresh extraction: the
// cheap failure this catches is a truncated file left by a killed build, and the expensive one it
// catches is somebody having built for a platform whose tarball layout moved.
func hostNode(opts BuildOptions, pin Pin, host Platform, p func(string, ...any)) (string, error) {
	dir := filepath.Join(opts.CacheDir, "toolchain", fmt.Sprintf("node-%s-%s-%s", NodeVersion, host.OS, host.Arch))
	exe := filepath.Join(dir, "bin", "node")

	if !nodeRuns(exe, NodeVersion) {
		archive, err := fetchPinned(pin, opts.CacheDir, p)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
			return "", err
		}
		p("extracting the build's own node %s", NodeVersion)
		if _, err := extractMember(archive, nodeTarballPrefix(NodeVersion, host)+"/bin/node", exe); err != nil {
			return "", err
		}
		if err := os.Chmod(exe, 0o755); err != nil {
			return "", err
		}
		if !nodeRuns(exe, NodeVersion) {
			return "", fmt.Errorf("%s was extracted from the pinned tarball and does not report v%s when run", exe, NodeVersion)
		}
	}
	return exe, nil
}

func nodeRuns(exe, version string) bool {
	out, err := exec.Command(exe, "--version").Output()
	return err == nil && strings.TrimSpace(string(out)) == "v"+version
}

// hostPnpm unpacks the pinned npm tarball and returns the entry script.
//
// THE WHOLE PACKAGE, not one file: `bin/pnpm.cjs` is a launcher that requires `../dist/pnpm.cjs`,
// and the trick {@link stageNode} uses — lift one self-contained binary out of a 180 MB
// distribution — has no equivalent for a JavaScript program with an internal require.
func hostPnpm(opts BuildOptions, pin Pin, p func(string, ...any)) (string, error) {
	dir := filepath.Join(opts.CacheDir, "toolchain", "pnpm-"+PnpmVersion)
	entry := filepath.Join(dir, "bin", "pnpm.cjs")

	if _, err := os.Stat(entry); err == nil {
		return entry, nil
	}
	archive, err := fetchPinned(pin, opts.CacheDir, p)
	if err != nil {
		return "", err
	}
	p("unpacking pnpm %s", PnpmVersion)
	// Into a sibling and renamed, so an interrupted unpack cannot leave a directory that the stat
	// above will later accept as complete. Same rule as fetchPinned's temporary file, for the same
	// reason: a half-written artifact wearing the right name is the failure that survives a retry.
	tmp := dir + ".unpacking"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := extractTree(archive, "package/", tmp); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(tmp, "bin", "pnpm.cjs")); err != nil {
		return "", fmt.Errorf("the pnpm %s tarball has no package/bin/pnpm.cjs: its layout is not what this pin assumes", PnpmVersion)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return entry, nil
}

// bootstrapOrchestratorModules installs the orchestrator's DEV dependency tree, if a clean
// checkout has none.
//
// THIS IS THE ONE THING THE BUILD WRITES INTO THE CHECKOUT, and it is worth saying why that is
// allowed here when `stageOrchestrator` goes to some trouble to keep `dist` out of it. `dist` is
// build OUTPUT: overwriting it destroys something a developer made and cannot be recovered from
// the repository. `node_modules` is build INPUT, it is gitignored, and `--frozen-lockfile` makes
// its contents a pure function of a committed file — so writing it is reproducing a derived thing,
// not clobbering an authored one.
//
// AND ONLY WHEN IT IS ABSENT. Never a refresh, never a repair, and above all never `--prod` —
// which would PRUNE the dev half of a working tree and leave a developer's checkout unable to run
// its own tests as a side effect of building an artifact. If typescript is there, this does
// nothing at all.
func bootstrapOrchestratorModules(src string, tc *toolchain, p func(string, ...any)) error {
	if _, err := os.Stat(filepath.Join(src, "node_modules", "typescript", "bin", "tsc")); err == nil {
		return nil
	}
	p("no control/orchestrator/node_modules in this checkout — installing it (pnpm install --frozen-lockfile)")
	cmd := tc.pnpmCmd(src, "install", "--frozen-lockfile", "--config.confirmModulesPurge=false")
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pnpm install failed in %s: %w\n%s", src, err, indent(string(combined)))
	}
	if _, err := os.Stat(filepath.Join(src, "node_modules", "typescript", "bin", "tsc")); err != nil {
		return fmt.Errorf("pnpm install succeeded in %s but left no typescript: %w", src, err)
	}
	return nil
}

// buildSharedCore compiles `@kontra/core` so the orchestrator's `tsc` can resolve it.
//
// AN UNBUILT CORE IS NOT A SLOW PATH, IT IS A HARD FAILURE. `@kontra/core` is a `workspace:*`
// dependency whose `exports` point into `dist/`, so with no `dist` every import of it is
// `TS2307: Cannot find module '@kontra/core/contract/datasets'` — once per import, for a reason
// that names the module and not the missing build.
//
// WHICH IS WHY EVERY SCRIPT IN `control/orchestrator/package.json` IS PREFIXED WITH THIS COMMAND.
// `build`, `typecheck` and `test` all begin `pnpm --filter @kontra/core run build &&`.
// `stageOrchestrator` does not use those scripts — it runs `tsc` directly to put the output in
// staging rather than in the checkout — and skipping the prefix with it is how this step came to
// fail on every clean machine while passing on every developer's.
//
// FROM THE WORKSPACE ROOT, because `--filter` resolves against the workspace and
// `pnpm-workspace.yaml` is at the repository root, not under `control/orchestrator`.
//
// Core's `dist` is build INPUT by the same argument `bootstrapOrchestratorModules` makes for
// node_modules: derived, gitignored, a pure function of committed sources. And likewise only when
// absent — never a refresh over a tree somebody is working in.
func buildSharedCore(repoRoot string, tc *toolchain, p func(string, ...any)) error {
	built := filepath.Join(repoRoot, "shared", "core", "dist", "cjs", "index.js")
	if _, err := os.Stat(built); err == nil {
		return nil
	}
	p("no shared/core/dist in this checkout — building @kontra/core (pnpm --filter @kontra/core run build)")
	cmd := tc.pnpmCmd(repoRoot, "--filter", "@kontra/core", "run", "build")
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building @kontra/core failed in %s: %w\n%s", repoRoot, err, indent(string(combined)))
	}
	if _, err := os.Stat(built); err != nil {
		return fmt.Errorf("@kontra/core reported success but left no %s: %w", built, err)
	}
	return nil
}

// extractTree unpacks a .tar.gz, keeping the members under stripPrefix and dropping that prefix.
//
// It refuses a member whose path escapes the destination. That is not a theoretical concern about
// a hostile registry — it is the ordinary way an extractor written in an afternoon writes over
// `/etc` when somebody hands it a tarball built by a tool with a different idea of relative paths.
func extractTree(archive, stripPrefix, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var members int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if !strings.HasPrefix(name, stripPrefix) {
			continue
		}
		rel := strings.TrimPrefix(name, stripPrefix)
		if rel == "" {
			continue
		}
		out := filepath.Join(root, filepath.FromSlash(rel))
		if !strings.HasPrefix(out, root+string(filepath.Separator)) {
			return fmt.Errorf("%s contains a member that escapes the destination: %q", archive, hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			w, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, tr)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			members++
		default:
			// Symlinks, devices and hard links. An npm tarball has none, and silently
			// materialising one from an archive this code does not expect to contain them would
			// be a way for a member to point somewhere the path check above already refused.
			continue
		}
	}
	if members == 0 {
		return fmt.Errorf("%s contains no files under %q", archive, stripPrefix)
	}
	return nil
}
