package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// WHAT THESE PIN. The buildpack path came up and the lifecycle refused the actor this install
// ships: `No buildpack groups passed detection` — `examples/python/hello` is `actor.json` and
// `actor.py`, which heroku/python does not detect on and heroku/procfile has no Procfile to read.
// The deleted Dockerfile had assembled the rest; nothing had replaced it. Every assertion here is
// about one of the four ways an image can come out of this looking built and be unrunnable.

// sdkTree builds a stand-in $KONTRA_SDK_ROOT with the layout the real one has.
func sdkTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []struct{ path, body string }{
		{"sdk/python/pyproject.toml", "[project]\nname = \"kontra-sdk\"\n"},
		{"sdk/python/kontra/__init__.py", "# the author surface\n"},
		{"runtime/python/internals/__init__.py", "# what the SDK's package-dir maps to\n"},
	} {
		p := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "handler"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func pyActor(t *testing.T, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"actor.json": `{"name":"hello","version":"0.1.0"}`,
		"actor.py":   "from kontra import actor\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func stage(t *testing.T, actorDir, sdkRoot, engine string) string {
	t.Helper()
	m := actorManifest{Name: "hello", Version: "0.1.0"}
	staged, err := stageActorBuild(actorDir, m, engine, sdkRoot, resolvedRuntime{})
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(staged) })
	return staged
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestTheStagedContextCarriesWhatTheLifecycleDetectsOn(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")

	// `requirements.txt` makes heroku/python participate and `Procfile` makes heroku/procfile
	// participate. Without BOTH the detector resolves no group and the build fails before it starts.
	for _, want := range []string{"requirements.txt", "Procfile", ".python-version", stagedEntry, stagedHandler} {
		if _, err := os.Stat(filepath.Join(staged, want)); err != nil {
			t.Errorf("%s is missing from the build context: %v", want, err)
		}
	}
	if got := read(t, filepath.Join(staged, "actor.py")); !strings.Contains(got, "from kontra import actor") {
		t.Error("the actor's own code must be in the context")
	}
}

func TestTheSDKIsVendoredByPathAndNeverByName(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")

	req := read(t, filepath.Join(staged, "requirements.txt"))
	if !strings.Contains(req, "./"+stagedSDK) {
		t.Errorf("the requirement must be a path into the context, got:\n%s", req)
	}
	// `kontra-sdk` is published to no index, and `kontra` on PyPI is an unrelated live project — so
	// a bare name here is either an unresolvable build or somebody else's code in the image.
	for _, line := range strings.Split(req, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "./") {
			continue
		}
		if strings.HasPrefix(line, "kontra") {
			t.Errorf("a bare dependency name on the SDK resolves to another project: %q", line)
		}
	}
}

func TestTheRuntimeTreeTravelsWithTheSDKBecauseThePyprojectMapsIntoIt(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")

	// `sdk/python/pyproject.toml` has `internals = "../../runtime/python/internals"`. If the sibling
	// is missing, pip installs a `kontra` that cannot import its own actor host — and nothing fails
	// until load() on a Machine.
	sdk := filepath.Join(staged, stagedSDK, "pyproject.toml")
	internals := filepath.Join(staged, stagedRuntime, "internals", "__init__.py")
	for _, p := range []string{sdk, internals} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s did not travel: %v", p, err)
		}
	}
	rel, err := filepath.Rel(filepath.Dir(sdk), filepath.Join(staged, stagedRuntime, "internals"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != filepath.Join("..", "..", "runtime", "python", "internals") {
		t.Errorf("the two trees must keep the layout the pyproject maps across, got %q", rel)
	}
}

func TestAnImageWithNoHandlerIsRefusedRatherThanBuilt(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	root := sdkTree(t)
	if err := os.Remove(filepath.Join(root, "handler")); err != nil {
		t.Fatal(err)
	}
	_, err := stageActorBuild(pyActor(t, nil), actorManifest{Name: "hello", Version: "0.1.0"}, "py", root, resolvedRuntime{})
	if err == nil {
		t.Fatal("an actor image with no workflow half must be refused: it would poll the sessions " +
			"queue, answer no workflow task, and look healthy")
	}
	for _, want := range []string{"workflow half", "/opt/kontra/handler"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say what is missing and where one comes from; %q absent:\n%v", want, err)
		}
	}
}

func TestTheProcfileCarriesTheActorsIdentityBecauseNothingElseCan(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")
	proc := read(t, filepath.Join(staged, "Procfile"))

	// `pack build --env` is BUILD-time only and there is no ENTRYPOINT to set, so the process
	// definition is the only place a runtime variable can be baked. The entrypoint REFUSES without
	// KONTRA_ACTOR_NAME, which is the failure this prevents.
	for _, want := range []string{
		"KONTRA_ACTOR_NAME=hello", "KONTRA_ACTOR_VERSION=0.1.0", "KONTRA_ACTOR_ENGINE=py",
		"KONTRA_ACTOR_ENTRY=actor.py", "KONTRA_ACTOR_ROOT=.", "KONTRA_HANDLER_BIN=./" + stagedHandler,
	} {
		if !strings.Contains(proc, want) {
			t.Errorf("Procfile is missing %q:\n%s", want, proc)
		}
	}
}

// builtOn is a runtime as resolveRuntime hands it back: the corpus's own values, so the Procfile is
// asserted against the same strings `shared/conformance/catalog.json` expects a registrar to echo.
var builtOn = resolvedRuntime{
	Name:   "python-browser",
	Major:  1,
	Ref:    "127.0.0.1:5000/kontra-runtimes/python-browser:1",
	Digest: "sha256:5669d5a8cccfbb5a2f553d4ea9defce603f13ec010f5940fb9438c7a7f24f8a1b",
}

// procfileEnv runs the staged Procfile's command the way the launcher does — through a shell — with
// the supervisor swapped for one that prints its environment, and returns what the worker would see.
//
// RUN, NOT GREPPED, because the property is a SHELL's: `${VAR:-baked}` means "the container's value
// if it has one", and a substring check would pass just as happily on `VAR=baked`, which means the
// opposite. `sh` and not `bash` because the default form is POSIX and the entrypoint is `#!/bin/sh`.
func procfileEnv(t *testing.T, staged string, env ...string) map[string]string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH; the Procfile is a shell command and cannot be exercised without one")
	}
	line := strings.TrimSpace(read(t, filepath.Join(staged, "Procfile")))
	cmdline, ok := strings.CutPrefix(line, "worker: ")
	if !ok {
		t.Fatalf("the Procfile does not define the worker process:\n%s", line)
	}
	if err := os.WriteFile(filepath.Join(staged, stagedEntry), []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", cmdline)
	cmd.Dir = staged
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the Procfile's command: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			got[k] = v
		}
	}
	return got
}

func TestTheProcfileCarriesWhatTheImageWasBuiltOn(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged, err := stageActorBuild(pyActor(t, nil), actorManifest{Name: "hello", Version: "0.1.0"},
		"py", sdkTree(t), builtOn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(staged) })

	// Both registrars read exactly these four and echo them into the catalog, and before this
	// nothing set them: every deployed actor's row had no runtime, so `kontra rebase` skipped it.
	got := procfileEnv(t, staged)
	for k, want := range map[string]string{
		"KONTRA_RUNTIME_NAME":   builtOn.Name,
		"KONTRA_RUNTIME_MAJOR":  "1",
		"KONTRA_RUNTIME_DIGEST": builtOn.Digest,
		"KONTRA_BUILDER_DIGEST": builderDigest,
		"KONTRA_ACTOR_NAME":     "hello",
	} {
		if got[k] != want {
			t.Errorf("the worker sees %s=%q, want %q", k, got[k], want)
		}
	}

	// A REBASED IMAGE STILL CARRIES THIS PROCFILE, so the baked digest is only a default: a placer
	// that knows the runtime moved underneath must be able to say so with the container's own
	// environment. An assignment would silently win over it.
	moved := "sha256:" + strings.Repeat("e", 64)
	if got := procfileEnv(t, staged, "KONTRA_RUNTIME_DIGEST="+moved); got["KONTRA_RUNTIME_DIGEST"] != moved {
		t.Errorf("the container's KONTRA_RUNTIME_DIGEST must win over the baked one after a rebase; "+
			"the worker saw %q", got["KONTRA_RUNTIME_DIGEST"])
	}
}

func TestAnUnknownRuntimeIsLeftOutRatherThanBakedBlank(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")
	proc := read(t, filepath.Join(staged, "Procfile"))
	// The registrars omit `runtime` when the NAME is empty, and the catalog keeps a previous value
	// only when the key is absent — so an empty name baked here would erase a good row on restart.
	if strings.Contains(proc, "KONTRA_RUNTIME_") {
		t.Errorf("no runtime was resolved, so none may be baked:\n%s", proc)
	}
	// The builder is a constant of this binary, known whether or not a runtime was resolved.
	if !strings.Contains(proc, "KONTRA_BUILDER_DIGEST=${KONTRA_BUILDER_DIGEST:-"+builderDigest+"}") {
		t.Errorf("the builder digest must ride the Procfile:\n%s", proc)
	}
}

func TestAValueThatIsNotANameOrADigestIsNeverBakedIntoTheShellLine(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	hostile := builtOn
	hostile.Digest = "sha256:$(touch /tmp/pwned)"
	_, err := stageActorBuild(pyActor(t, nil), actorManifest{Name: "hello", Version: "0.1.0"},
		"py", sdkTree(t), hostile)
	// The Procfile is run by a shell in every container of the image. resolveRuntime already holds
	// a digest to the OCI grammar; this is the refusal that keeps that true for a caller that does not.
	if err == nil || !strings.Contains(err.Error(), "KONTRA_RUNTIME_DIGEST") {
		t.Fatalf("a shell-active value must be refused by name, got %v", err)
	}
}

func TestAnActorThatDeclaresItsOwnDependenciesIsExtendedOrTold(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	// requirements.txt: the SDK is appended and the actor's own pins are kept.
	staged := stage(t, pyActor(t, map[string]string{"requirements.txt": "httpx==0.27.0\n"}), sdkTree(t), "py")
	req := read(t, filepath.Join(staged, "requirements.txt"))
	if !strings.Contains(req, "httpx==0.27.0") || !strings.Contains(req, "./"+stagedSDK) {
		t.Errorf("both the actor's pins and the SDK must be present:\n%s", req)
	}

	// pyproject.toml: that resolver would IGNORE an appended pip line, so producing an image
	// without the SDK is the quiet outcome this refuses.
	_, err := stageActorBuild(
		pyActor(t, map[string]string{"pyproject.toml": "[project]\nname = \"hello\"\n"}),
		actorManifest{Name: "hello", Version: "0.1.0"}, "py", sdkTree(t), resolvedRuntime{})
	if err == nil || !strings.Contains(err.Error(), stagedSDK) {
		t.Errorf("an actor on pyproject must be told what to add, got %v", err)
	}
}

func TestTheActorsOwnDirectoryIsNeverWrittenTo(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	dir := pyActor(t, nil)
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stage(t, dir, sdkTree(t), "py")
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The actor directory is what an author publishes. A generated Procfile or a vendored SDK
	// appearing in it would be this command editing someone else's tree.
	if len(before) != len(after) {
		var names []string
		for _, e := range after {
			names = append(names, e.Name())
		}
		t.Errorf("staging added files to the actor's own directory: %v", names)
	}
}

func TestACheckoutAndAVirtualenvDoNotEnterTheImage(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	dir := pyActor(t, map[string]string{
		".git/config":               "[core]\n",
		".venv/lib/python3.12/x.py": "# a hundred MB of layer the lifecycle then ignores\n",
		"__pycache__/actor.cpython": "\x00",
	})
	staged := stage(t, dir, sdkTree(t), "py")
	for _, gone := range []string{".git", ".venv", "__pycache__"} {
		if _, err := os.Stat(filepath.Join(staged, gone)); err == nil {
			t.Errorf("%s must not enter the build context", gone)
		}
	}
}

func TestTheHandlerKeepsItsExecutableBit(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")
	for _, p := range []string{stagedHandler, stagedEntry} {
		st, err := os.Stat(filepath.Join(staged, p))
		if err != nil {
			t.Fatal(err)
		}
		// A copy that dropped the bit produces an image whose process cannot start, and the
		// message is `permission denied` from a launcher.
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (%v)", p, st.Mode().Perm())
		}
	}
}

func TestAGoActorGetsNoPythonSDK(t *testing.T) {
	t.Setenv("KONTRA_HANDLER_BIN", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "actor.json"), []byte(`{"name":"hello","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := stage(t, dir, sdkTree(t), "go")
	if _, err := os.Stat(filepath.Join(staged, stagedSDK)); err == nil {
		t.Error("a Go actor must not carry the Python SDK — it is the run image's language that differs")
	}
	if !strings.Contains(read(t, filepath.Join(staged, "Procfile")), "KONTRA_ACTOR_ENGINE=go") {
		t.Error("the Procfile must say which engine, or the entrypoint runs python against a binary")
	}
}

func TestTheContextIsStagedWhereTheHOSTCanAlsoSeeIt(t *testing.T) {
	// `pack` drives the host's daemon through the mounted socket, so every path it hands the
	// lifecycle is resolved by the HOST. The workspaces tree is mounted at the same spelling on both
	// sides; the `cli` container's own /tmp has no host counterpart. What that cost was not a
	// missing-file error but `failed to commit cache: rename /launch-cache/staging …: no such file
	// or directory`, after a build that had otherwise succeeded.
	t.Setenv("KONTRA_HANDLER_BIN", "")
	ws := t.TempDir()
	t.Setenv("KONTRA_WORKSPACES", ws)
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")

	if filepath.Dir(staged) != ws {
		t.Errorf("staged at %q, want a child of the workspaces tree %q", staged, ws)
	}
	// Dot-prefixed, because `workspace watch` and `workspace list` enumerate this directory and
	// both skip a leading dot — otherwise a build context would read as a workspace to deploy.
	if !strings.HasPrefix(filepath.Base(staged), ".") {
		t.Errorf("%q would be enumerated as a workspace", filepath.Base(staged))
	}
}

func TestWithNoWorkspacesTreeTheSystemTempDirIsFine(t *testing.T) {
	// A laptop: no container, no socket, no second spelling of any path.
	t.Setenv("KONTRA_HANDLER_BIN", "")
	t.Setenv("KONTRA_WORKSPACES", filepath.Join(t.TempDir(), "does-not-exist"))
	if got := stageParent(); got != "" {
		t.Errorf("stageParent() = %q, want the system default when the tree is absent", got)
	}
	staged := stage(t, pyActor(t, nil), sdkTree(t), "py")
	if _, err := os.Stat(filepath.Join(staged, "Procfile")); err != nil {
		t.Errorf("staging must still work: %v", err)
	}
}
