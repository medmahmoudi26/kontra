// serve_test.go — `kontra serve`'s process wiring, and the verb table that reaches it.
package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// WHICH ENGINE AN ACTOR FOLDER RUNS UNDER, which defaulted to "py" and so served every Go actor by
// running `python <dir>/actor.py` against a directory that has never held one.
//
// THE FAILURE WAS UNREADABLE, which is why this is pinned rather than left to the flag. The error is
// `can't open file …/examples/go/nscheck/actor.py` — it names a Python file, in a Go actor's folder,
// so it reads as a missing file rather than as the wrong runtime, and it sends the reader looking
// for something that should not exist. And the Actors page's Serve button passes no engine at all
// (there is no runtime picker on that surface, deliberately), so under the old default that button
// could only ever serve Python actors.
func TestEngineIsDetectedFromTheFolder(t *testing.T) {
	goActor := t.TempDir()
	if err := os.WriteFile(filepath.Join(goActor, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pyActor := t.TempDir()
	if err := os.WriteFile(filepath.Join(pyActor, "actor.py"), []byte("# actor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := actorManifest{Name: "nscheck", Version: "0.1.0"}

	if got, err := engineFor(goActor, "", m); err != nil || got != "go" {
		t.Errorf("a folder with go.mod: engineFor = %q, %v; want go", got, err)
	}
	if got, err := engineFor(pyActor, "", m); err != nil || got != "py" {
		t.Errorf("a folder with actor.py: engineFor = %q, %v; want py", got, err)
	}

	// THE FLAG STILL WINS. Detection is a better default, not a new authority — an author who says
	// `-engine go` about a folder this cannot read has to be able to.
	if got, _ := engineFor(pyActor, "go", m); got != "go" {
		t.Errorf("the flag must beat detection: engineFor = %q, want go", got)
	}
	// …and so does the manifest, which is how a folder that generates its entry at build time says
	// what it is without passing a flag on every command.
	if got, _ := engineFor(pyActor, "", actorManifest{Engine: "go"}); got != "go" {
		t.Errorf("actor.json engine must beat detection: engineFor = %q, want go", got)
	}
	if _, err := engineFor(pyActor, "rust", m); err == nil {
		t.Error("an unknown engine must be refused, not silently treated as py")
	}
}

// BOTH FILES PRESENT IS THE ONE CASE WORTH REFUSING: the folder would serve different code
// depending on a flag nobody passed, and a coin-flip between two real programs is worse than a
// sentence asking which one.
func TestEngineRefusesAnAmbiguousFolder(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"go.mod", "actor.py"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := engineFor(dir, "", actorManifest{Name: "both", Version: "0.1.0"})
	if err == nil {
		t.Fatal("a folder holding go.mod AND actor.py must be refused rather than guessed")
	}
	if !strings.Contains(err.Error(), "-engine py|go") {
		t.Errorf("the refusal must say how to resolve it, got %q", err)
	}
}

// NEITHER FILE FALLS BACK TO PY, and that is deliberate compatibility rather than an oversight.
// Detection exists to stop a Go actor being run as Python; it is not a new place to fail. A folder
// with neither file is broken either way, and refusing here would turn a manifest-only fixture —
// and any actor whose entry is generated at build time — into an error where it used to work.
func TestEngineFallsBackRatherThanRefusingAnEmptyFolder(t *testing.T) {
	got, err := engineFor(t.TempDir(), "", actorManifest{Name: "echo", Version: "0.1.0"})
	if err != nil || got != "py" {
		t.Errorf("engineFor on a bare folder = %q, %v; want py, nil", got, err)
	}
}

// THE VERB. `kontra run --actor` became `kontra serve --actor` because `run` is the CALLER's word —
// a dispatch runs; this command stands a Worker up and waits — and because `run`/`runs` were one
// letter apart on a CLI where they name opposite ends of the same pipeline.
func TestServeIsTheVerbThatStartsAWorker(t *testing.T) {
	// Reached through dispatch, not by calling cmdServe: the switch IS the thing the rename could
	// get wrong, and a test that calls the function directly passes with the case still spelled
	// `run`. The empty argv makes cmdServe stop at its own usage line, which is all this needs to
	// prove — the word arrived somewhere.
	err := dispatch([]string{"serve"})
	if err == nil {
		t.Fatal("`kontra serve` with no --actor must refuse")
	}
	if !strings.Contains(err.Error(), "kontra serve --actor") {
		t.Errorf("the usage line must name the new verb, got: %v", err)
	}
	if errors.Is(err, errUsage) {
		t.Errorf("`serve` reached the unknown-command case — it is not wired to cmdServe: %v", err)
	}
}

// `kontra run` is a REDIRECT and NOT an alias, the same shape `kontra monitor` has. An alias would
// leave one word meaning both "make work happen" and "wait for work", which is the ambiguity the
// rename exists to remove.
func TestRunRedirectsToCommandsThatRun(t *testing.T) {
	// `-h` LAST, so a `run` wired back to cmdServe answers `flag.ErrHelp`: its parser read the flags,
	// which is the old verb working again. The redirect reads none of them, and `-h` means nothing
	// was started either way.
	err := dispatch([]string{"run", "--actor", t.TempDir(), "-h"})
	if err == nil {
		t.Fatal("`kontra run` must fail — a working alias keeps the old word alive")
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Fatalf("`kontra run` parsed its flags, so it reached a command instead of the redirect: %v", err)
	}
	// Not the unknown-command path: that one prints the whole usage and exits 2, which buries the
	// one sentence that answers the question.
	if errors.Is(err, errUsage) {
		t.Errorf("`run` must be a named redirect, not an unknown command: %v", err)
	}
	// An operator who typed the old word learns the new one here or nowhere, so every command the
	// redirect names — other than the old spelling it quotes — has to be one the CLI accepts.
	named := 0
	for _, argv := range adviceIn(err.Error()) {
		if argv[0] == "run" {
			continue
		}
		named++
		if e := adviceRuns(t, argv); e != nil {
			t.Errorf("the redirect names a command that does not run: %v", e)
		}
	}
	if named == 0 {
		t.Errorf("the redirect names no command to run instead: %v", err)
	}
}

func TestUnknownVerbIsTheUsagePath(t *testing.T) {
	// The sentinel is what makes exit 2 (a word this CLI does not have) different from exit 1 (a
	// command that ran and failed), and scripts read that difference.
	err := dispatch([]string{"nonsense"})
	if !errors.Is(err, errUsage) {
		t.Fatalf("an unknown word must carry errUsage, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"nonsense"`) {
		t.Errorf("the message must quote the word, got: %v", err)
	}
}

// `--mode fleet` refuses, and renaming the verb does not change what the modes do. The refusal is
// load-bearing: `fleet up` spends money and `fleet deploy` places the Bundle, and a second
// implementation of those two here is what this error exists to prevent.
func TestServeRefusesFleetWithTheTwoCommandsThatDoIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "actor.json"),
		[]byte(`{"name":"probe","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := dispatch([]string{"serve", "--actor", dir, "--mode", "fleet"})
	if err == nil {
		t.Fatal("`kontra serve --mode fleet` must refuse")
	}
	for _, want := range []string{"kontra serve --mode fleet", "kontra fleet up", "kontra fleet deploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q, got: %v", want, err)
		}
	}
}

// TestDeriveDoesNotShareABackingArray is a regression test for a bug that cost an afternoon and
// left no trace: two `append(env, …)` calls on one slice with spare capacity write the SAME
// index, so the second derivation silently overwrote the first's variable. Concretely, the
// handler's GOWORK=off replaced the actor's PYTHONPATH, and the actor was started without it.
//
// It hid because the repo's .venv has the SDK pip-installed, which makes PYTHONPATH redundant
// precisely where it was being dropped. On any interpreter that does not — a bare python3, a
// fresh clone, a git worktree — it is an immediate ModuleNotFoundError: actorkit.
func TestDeriveDoesNotShareABackingArray(t *testing.T) {
	base := append(make([]string, 0, 8), "A=1", "B=2") // spare capacity, as os.Environ() has

	first := cliutil.Derive(base, "PYTHONPATH=/checkout")
	second := cliutil.Derive(base, "GOWORK=off")

	if !has(first, "PYTHONPATH=/checkout") {
		t.Errorf("the first derivation lost its own entry: %v", first)
	}
	if has(first, "GOWORK=off") {
		t.Errorf("the second derivation leaked into the first: %v", first)
	}
	if !has(second, "GOWORK=off") || has(second, "PYTHONPATH=/checkout") {
		t.Errorf("the second derivation is wrong: %v", second)
	}
	if len(base) != 2 {
		t.Errorf("the base environment was mutated: %v", base)
	}
}

func TestDeriveKeepsTheBaseEnvironment(t *testing.T) {
	got := cliutil.Derive([]string{"A=1"}, "B=2", "C=3")
	if strings.Join(got, " ") != "A=1 B=2 C=3" {
		t.Errorf("cliutil.Derive lost or reordered entries: %v", got)
	}
}

func has(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
