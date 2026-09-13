package main

// workflowreplay.go — `kontra workflow replay` and `kontra workflow history`.
//
// WHAT REPLAY BUYS THAT ATTACHING DOES NOT: no clock. A replay reads a recorded history and runs
// workflow code against it, with no server waiting on a poller — no `HeartbeatTimeout`, no
// `StartToClose`. You can sit on a breakpoint for an hour. Attaching a debugger to a live activity
// gives you about two minutes before `runActivityOptions`' heartbeat declares it dead and retries.
//
// AND IT IS POST-MORTEM. A run that failed on a fleet three days ago can be replayed on a laptop,
// which is the only cheap answer to the class of bug this repo has already shipped: a chained
// Method→Method dispatch that isolated every unit at fleet scale, was invisible locally, and
// reported `completed` with an empty Dataset.
//
// WHAT IT DOES NOT COVER, and the docs say so out loud: activity code is NOT run. A kontra Method
// is an activity; its results come from the recorded history as values. This covers the CALLER's
// decisions — splitting, chaining, branching, early exit.
//
// The work is Python's, because the workflows are: `internals/replaycmd.py` drives
// `internals/replay.py`, which builds a Replayer with the same data converter and the same sandbox
// the worker serves under. Doing otherwise would answer a different question than the one asked.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// replayPython runs `python -m internals.<mod> …` with the PYTHONPATH the runtime needs.
//
// The same three entries `serve.go` sets, through `cliutil.Derive` rather than `append` — two
// appends onto one slice with spare capacity write the same index, and this repo has lost a
// PYTHONPATH exactly that way.
func replayPython(root, python string, argv []string) *exec.Cmd {
	cmd := exec.Command(pythonFor(root, python), argv...)
	cmd.Dir = filepath.Join(root, "runtime", "python")
	cmd.Env = cliutil.Derive(os.Environ(),
		"PYTHONPATH="+filepath.Join(root, "sdk", "python")+":"+
			filepath.Join(root, "runtime", "python")+":"+
			filepath.Join(root, "sdk", "python", "_gen"))
	cmd.Stdout = cliio.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

const historyUsage = "usage: kontra workflow history <workflow-id> [--run-id ATTEMPT] [-o FILE]"

func workflowHistory(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(historyUsage)
	}
	wfID, rest := args[0], args[1:]
	fs := flag.NewFlagSet("workflow history", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	out := fs.String("o", "", "write the history here instead of stdout")
	// THE POSITIONAL IS THE WORKFLOW ID; --run-id pins ONE attempt. They were briefly one argument,
	// and passing a workflow id in the run-id slot did not error — it SEGFAULTED the Rust core.
	runID := fs.String("run-id", "", "pin one attempt of that workflow")
	python := fs.String("python", "", "python interpreter")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("kontra workflow history needs the checkout: %w", err)
	}
	argv := []string{"-m", "internals.replaycmd", "history", wfID}
	if *runID != "" {
		argv = append(argv, "--run-id", *runID)
	}
	if *out != "" {
		argv = append(argv, "-o", *out)
	}
	return replayPython(root, *python, argv).Run()
}

const replayUsage = "usage: kontra workflow replay <workflow-file> (--workflow-id ID | --history FILE) [--json]"

func workflowReplay(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(replayUsage)
	}
	file, rest := args[0], args[1:]
	fs := flag.NewFlagSet("workflow replay", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	runID := fs.String("run-id", "", "pin one attempt")
	wfID := fs.String("workflow-id", "", "fetch this workflow's history from the server")
	history := fs.String("history", "", "replay a saved history file (no server contact)")
	asJSON := fs.Bool("json", false, "one JSON document instead of prose")
	python := fs.String("python", "", "python interpreter")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *wfID == "" && *runID == "" && *history == "" {
		return errors.New(replayUsage + "\n  --history replays a file somebody attached to an issue; " +
			"--workflow-id fetches one from the server")
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("no workflow file at %s: %w", abs, err)
	}
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("kontra workflow replay needs the checkout: %w", err)
	}

	argv := []string{"-m", "internals.replaycmd", "replay", abs}
	if *runID != "" {
		argv = append(argv, "--run-id", *runID)
	}
	if *wfID != "" {
		argv = append(argv, "--workflow-id", *wfID)
	}
	if *history != "" {
		argv = append(argv, "--history", *history)
	}
	if *asJSON {
		argv = append(argv, "--json")
	}

	err = replayPython(root, *python, argv).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// THE EXIT CODE IS THE ANSWER, and the three are not interchangeable. 1 says this code would
		// break a run already in flight; 2 says the replay could not be performed at all. Collapsing
		// them would send somebody to rewrite a workflow because a history file was missing.
		switch exit.ExitCode() {
		case 1:
			return errors.New("the history does NOT replay against this code — an execution already " +
				"in flight would fail on it (see above)")
		case 2:
			return errors.New("the replay could not run (see above) — this is a setup problem, not a " +
				"finding about the workflow")
		}
	}
	return err
}
