// packstage.go — the build context `pack` is handed, which is NOT the actor's own directory.
//
// WHY A STAGED COPY AT ALL. The deleted Dockerfile path assembled three things into the actor
// image: the actor's code, the Python SDK (`sdk/python` + `runtime/python/internals`, on PYTHONPATH
// rather than pip-installed), and the compiled Go handler plus the two-process supervisor that runs
// both halves. The CNB lifecycle has no equivalent of a `COPY` from outside the build context, so
// anything the image needs has to BE in the context — and the actor's own directory must stay
// publishable, which means nothing may be written into it.
//
// WHERE THE SDK AND THE HANDLER COME FROM. `$KONTRA_SDK_ROOT`, which the `kontra` image already
// populates: `/opt/kontra/sdk/python`, `/opt/kontra/runtime/python` and `/opt/kontra/handler`
// (`Dockerfile.selfcontained`, and `cli/serve.go:prebuiltHandler` reads the same tree). So the two
// services that build actors — `cli` and `orchestrator-infra` — already carry them, and nothing has
// to be published anywhere for a build to work.
//
// THE SDK KEEPS ITS LAYOUT, AND THAT IS LOAD-BEARING. `sdk/python/pyproject.toml` maps
// `internals = "../../runtime/python/internals"`, so a vendored SDK whose sibling is missing
// installs a `kontra` that cannot import its own actor host — and the failure arrives at load()
// on a Machine, not at install. Both trees are copied with their relative positions intact.
//
// THE DEPENDENCY IS A PATH AND NEVER A NAME. `pip install kontra-sdk` would resolve nothing (it is
// published to no index) and `pip install kontra` resolves to an UNRELATED project that exists on
// PyPI — a live "developer-first data quality engine". The old path could not be bitten by this
// because it resolved no names; this one can, so the requirement is a directory.
package main

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// workerEntrypoint is the two-process supervisor, carried in the binary because the image it has
// to land in is built by the lifecycle rather than by a Dockerfile this repo controls.
//
//go:embed assets/worker-entrypoint.sh
var workerEntrypoint embed.FS

const (
	// stagedSDK / stagedRuntime keep `../../runtime/python/internals` resolvable from the SDK's
	// pyproject. Changing either without the other is the wheel-drops-the-actor-host bug.
	stagedSDK     = "vendor/sdk/python"
	stagedRuntime = "vendor/runtime/python"
	stagedHandler = "kontra-handler"
	stagedEntry   = "worker-entrypoint.sh"

	// stagedPython is what the heroku/python buildpack installs when the actor pins nothing. It is
	// written rather than omitted because an unpinned build takes whatever the buildpack's default
	// is that week, and the actor images this produces are supposed to be comparable by digest.
	stagedPython = "3.12"
)

// dependencyManifests are the files heroku/python detects on. If an actor has one, it owns its
// dependencies and this only appends the SDK; if it has none, one is written.
var dependencyManifests = []string{"requirements.txt", "pyproject.toml", "Pipfile", "setup.py"}

// stageParent is WHERE the staged context goes, and the answer is not `/tmp`.
//
// THE PATH HAS TO SPELL THE SAME INSIDE AND OUTSIDE THIS CONTAINER. `pack` drives the host's daemon
// through the mounted socket, so every path it hands the lifecycle is resolved by the HOST. The
// workspaces tree is mounted `${KONTRA_WORKSPACES}:${KONTRA_WORKSPACES}` — the same spelling on both
// sides, deliberately — and the actor directories under it are what the old build context was. A
// context in the `cli` container's own `/tmp` has no host counterpart, and what came back was not a
// missing-file error but
//
//	[exporter] ERROR: failed to export: saving image: failed to commit cache: committing cache:
//	           rename /launch-cache/staging /launch-cache/committed: no such file or directory
//
// after a build that had otherwise completely succeeded — on the GitHub runner, and not on the box
// this was written on, which is what a path that resolves by accident looks like.
//
// DOT-PREFIXED, so `workspace watch` and `workspace list` skip it: both enumerate children of this
// directory and both already ignore a leading dot, which is the same reason `workspace seed` stages
// as `.seed-*` here rather than in the system temp directory.
//
// Empty means os.MkdirTemp's default, which is right for a laptop: no container, no socket, no
// second spelling of any path.
func stageParent() string {
	root := workspacesParent()
	if root == "" {
		return ""
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return ""
	}
	return root
}

// stageActorBuild copies the actor into a temporary directory, adds everything the image needs that
// the actor does not carry, and returns the directory to hand `pack`. The caller removes it.
func stageActorBuild(actorDir string, m actorManifest, engine, sdkRoot string) (string, error) {
	if sdkRoot == "" {
		return "", fmt.Errorf("no SDK tree to build this actor against: set KONTRA_SDK_ROOT, or run "+
			"from the checkout.\n  The `cli` and `orchestrator-infra` services have it at /opt/kontra, "+
			"which is where %s and the handler are shipped", stagedSDK)
	}
	staged, err := os.MkdirTemp(stageParent(), ".kontra-build-")
	if err != nil {
		return "", fmt.Errorf("could not make a build directory: %w", err)
	}
	cleanup := func(cause error) (string, error) {
		os.RemoveAll(staged)
		return "", cause
	}

	if err := copyTree(actorDir, staged, stageSkip); err != nil {
		return cleanup(fmt.Errorf("staging %s: %w", actorDir, err))
	}

	// THE HANDLER IS REQUIRED AND ITS ABSENCE IS NOT A WARNING. An image without it starts the
	// actor half, polls `<actor>-<version>-sessions`, and never answers a workflow task — a worker
	// that looks healthy and runs nothing, which is the failure mode this repo keeps paying for.
	handler, herr := prebuiltHandler(sdkRoot)
	if herr != nil {
		return cleanup(herr)
	}
	if handler == "" {
		return cleanup(fmt.Errorf("no compiled handler under %s, so this image would have no workflow "+
			"half — it would poll the sessions queue and never answer a workflow task.\n"+
			"  The `cli` and `orchestrator-infra` services ship one at /opt/kontra/handler. From a "+
			"checkout:\n    (cd runtime/handler && GOWORK=off go build -o ../../handler .)\n"+
			"  or point KONTRA_HANDLER_BIN at one.", sdkRoot))
	}
	if err := copyFile(handler, filepath.Join(staged, stagedHandler), 0o755); err != nil {
		return cleanup(fmt.Errorf("staging the handler from %s: %w", handler, err))
	}

	if engine != "go" {
		if err := stagePythonSDK(staged, sdkRoot); err != nil {
			return cleanup(err)
		}
	}

	if err := writeIfAbsent(filepath.Join(staged, ".python-version"), stagedPython+"\n", 0o644); err != nil {
		return cleanup(err)
	}
	if err := stageProcfile(staged, m, engine); err != nil {
		return cleanup(err)
	}
	script, err := workerEntrypoint.ReadFile("assets/worker-entrypoint.sh")
	if err != nil {
		return cleanup(err)
	}
	if err := os.WriteFile(filepath.Join(staged, stagedEntry), script, 0o755); err != nil {
		return cleanup(err)
	}
	return staged, nil
}

// stagePythonSDK vendors the SDK and the runtime tree it maps into, and makes the actor's
// requirements install it BY PATH.
func stagePythonSDK(staged, sdkRoot string) error {
	for _, pair := range [][2]string{
		{filepath.Join(sdkRoot, "sdk", "python"), filepath.Join(staged, stagedSDK)},
		{filepath.Join(sdkRoot, "runtime", "python"), filepath.Join(staged, stagedRuntime)},
	} {
		if _, err := os.Stat(pair[0]); err != nil {
			return fmt.Errorf("the Python SDK is not at %s, so `from kontra import actor` would fail "+
				"inside the built image: %w\n  KONTRA_SDK_ROOT names the tree that holds "+
				"sdk/python and runtime/python (the `kontra` image ships them at /opt/kontra)", pair[0], err)
		}
		if err := copyTree(pair[0], pair[1], stageSkip); err != nil {
			return fmt.Errorf("vendoring %s: %w", pair[0], err)
		}
	}

	req := filepath.Join(staged, "requirements.txt")
	line := "./" + stagedSDK + "\n"
	existing, err := firstPresent(staged, dependencyManifests)
	switch {
	case err != nil:
		return err
	case existing == "":
		// The SDK's own `dependencies` pull temporalio, jsonschema and pydantic, so this one line is
		// the whole requirement set for an actor that declares nothing.
		return os.WriteFile(req, []byte(
			"# written by `kontra deploy`: the SDK is vendored into this build context, never resolved\n"+
				"# by name — `kontra-sdk` is on no index and `kontra` on PyPI is a different project.\n"+
				line), 0o644)
	case existing == "requirements.txt":
		body, rerr := os.ReadFile(req)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), stagedSDK) {
			return nil
		}
		return os.WriteFile(req, append(withTrailingNewline(body), []byte(line)...), 0o644)
	default:
		// An actor using pyproject/Pipfile owns its resolver, and appending a pip requirement beside
		// it would be ignored at best. Say so instead of producing an image missing the SDK.
		return fmt.Errorf("this actor declares its dependencies in %s, and the vendored SDK has to be "+
			"added there: a path dependency on ./%s.\n  (`requirements.txt` is the form `kontra deploy` "+
			"can extend on its own.)", existing, stagedSDK)
	}
}

// stageProcfile writes the process the image runs. CNB has no ENTRYPOINT to set, so the Procfile IS
// the contract — and the actor's identity rides in it because `pack build --env` is BUILD-time only
// and nothing else in this path can bake a runtime variable into the image.
func stageProcfile(staged string, m actorManifest, engine string) error {
	entry := m.entryFile()
	cmd := fmt.Sprintf("worker: KONTRA_ACTOR_NAME=%s KONTRA_ACTOR_VERSION=%s KONTRA_ACTOR_ENGINE=%s "+
		"KONTRA_ACTOR_KIND=%s KONTRA_ACTOR_ENTRY=%s KONTRA_ACTOR_ROOT=. KONTRA_HANDLER_BIN=./%s ./%s\n",
		m.Name, m.Version, engine, m.kindOf(), entry, stagedHandler, stagedEntry)
	return writeIfAbsent(filepath.Join(staged, "Procfile"), cmd, 0o644)
}

// stageSkip is what never enters a build context: a checkout, a local virtualenv, caches, and a
// previous stage's vendor directory. Copying `.venv` was measured at over a hundred MB of layer for
// files the lifecycle then ignores.
func stageSkip(name string, dir bool) bool {
	if !dir {
		return name == ".DS_Store"
	}
	switch name {
	case ".git", ".venv", "venv", "__pycache__", ".mypy_cache", ".pytest_cache", "vendor", "node_modules":
		return true
	}
	return false
}

func firstPresent(dir string, names []string) (string, error) {
	for _, n := range names {
		_, err := os.Stat(filepath.Join(dir, n))
		switch {
		case err == nil:
			return n, nil
		case !os.IsNotExist(err):
			return "", err
		}
	}
	return "", nil
}

func withTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return b
	}
	return append(b, '\n')
}

func writeIfAbsent(path, body string, mode os.FileMode) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(body), mode)
}

// copyTree copies a directory, following no symlink out of it: a link in an actor's tree pointing at
// `/etc` or at the operator's home would otherwise be dereferenced into the image.
func copyTree(src, dst string, skip func(name string, dir bool) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(d.Name(), d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			// Not followed and not recreated: a symlink is the one entry whose meaning changes when
			// the tree moves, and an actor does not need one to build.
			return nil
		case !d.Type().IsRegular():
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
