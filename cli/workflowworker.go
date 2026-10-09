// workflowworker.go — HOW A SERVED WORKFLOW WORKER IS LAUNCHED, and how you find out it died.
//
// `kontra workflow serve` and `kontra workflow resume` both start the same process: an
// interpreter, a working directory, and an environment the author's module cannot supply for
// itself. This file is that process's launch profile — nothing here decides WHAT is served (that
// is identity.go) or WHEN (that is workflow.go).
//
// IT IS ONE FILE BECAUSE TWO COMMANDS NEED IT. `serve` builds a worker and `resume` rebuilds the
// same one in the same pane, and a `resume` that reconstructed the environment separately would
// eventually bring a worker back with a different one from the one that was paused — a difference
// nothing would report. The queue is the sharpest instance: `resume` re-derives it from the same
// folder, so an edit between serve and resume respawns onto a NEW queue and leaves the live run
// unserved, which is a decision rather than an accident precisely because both paths run through
// here.
//
// AND BECAUSE A TMUX PANE IS NOT A CHILD PROCESS. The foreground path inherits this shell's
// environment and overrides it; a tmux pane inherits the tmux SERVER's — a daemon that outlives
// any one client, usually carrying some other shell's environment from whenever it first started.
// So everything a worker needs has to be expressible as a DELTA that can be handed to `tmux -e`
// one variable at a time, which is why these are lists of `KEY=VALUE` and not an `exec.Cmd`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
)

// codecEnv is the environment the claim-check codec reads, forwarded to a served worker.
//
// Only what is SET is forwarded — casstore.py treats an unset endpoint as "run in passthrough"
// and never falls back to a default, because a codec pointed at the wrong store fails at decode
// time on a different machine. Passing an empty value would look like a configured store.
var codecEnv = []string{
	"KONTRA_S3_ENDPOINT",
	"KONTRA_S3_BUCKET",
	"KONTRA_S3_PREFIX",
	"KONTRA_S3_ACCESS_KEY",
	"KONTRA_S3_SECRET_KEY",
	"KONTRA_S3_REGION",
}

// serveEnvDelta is the environment a served workflow worker needs, ON TOP of whatever it inherits.
//
// The S3 endpoint is here for a failure that is silent and late. The claim-check codec falls back
// to PASSTHROUGH without it, and passthrough only shows up when a payload crosses 128 KiB — so a
// a run starts, pages fine, and then a big Batch comes back undecodable. A workflow worker is
// exactly where that bites: `delegation` fans one domain out to every nameserver it has, and the
// Batch that returns is the one over the line.
func serveEnvDelta(root, namespace, queue string) []string {
	delta := []string{
		"KONTRA_ADDRESS=" + config.TemporalAddress(),
		// The FOLDER'S workspace namespace (ADR 0051), resolved by the caller, not the current one:
		// serving hello's workflow while the console looks at default must still poll ws-hello.
		"KONTRA_NAMESPACE=" + namespace,
		// WHERE TO REGISTER — the same line `kontra serve` passes an actor, and for the same
		// reason. A workflow worker self-registers what each of its `@workflow.defn` classes
		// takes, returns and is for (`internals/catalog.py:publish_workflow_catalog`), and that
		// call returns immediately when this is unset. A tmux pane inherits the tmux SERVER's
		// environment, so a variable the operator exported in their shell reaches the worker not
		// at all unless it is passed explicitly — and the failure is silent by design, because a
		// worker must serve whether or not the orchestrator is reachable.
		"KONTRA_ORCHESTRATOR_URL=" + orchestratorURL(),
		"PYTHONPATH=" + filepath.Join(root, "sdk", "python") + ":" +
			filepath.Join(root, "runtime", "python") + ":" +
			filepath.Join(root, "sdk", "python", "_gen"),
	}
	for _, name := range codecEnv {
		if v := os.Getenv(name); v != "" {
			delta = append(delta, name+"="+v)
		}
	}
	if queue != "" {
		delta = append(delta, "KONTRA_WORKFLOW_QUEUE="+queue)
	}
	return delta
}

// watchEnv is the environment a `serve --watch` adds ON TOP of serveEnvDelta, and nothing when the
// flag is absent — which is what makes a plain serve behave exactly as it did before the flag existed.
//
// IT RIDES THE ENV, NOT THE ARGUMENT. The module the worker runs calls `catalog.serve([...])` with no
// watch flag, so the only way the signal reaches `serve_workflows_async` is a variable it reads. Kept
// off serveEnvDelta because `resume` shares that function to rebuild a paused worker and must NOT
// inherit watch: resuming is bringing a specific paused run back, not re-arming a save loop.
//
// A DELTA, so BOTH serve paths get it: the foreground child inherits and overrides its environment,
// while a tmux pane inherits the tmux SERVER's and must be told each variable explicitly.
func watchEnv(watch bool) []string {
	if !watch {
		return nil
	}
	return []string{"KONTRA_WORKFLOW_WATCH=1"}
}

// pythonFor picks the interpreter to run the workflow module with: the flag, then KONTRA_PYTHON,
// then the checkout's venv, then whatever `python3` is on PATH.
//
// KONTRA_PYTHON sits ABOVE the venv probe on purpose. This command can run somewhere other than
// where the process will — the Dashboard's Serve button runs it inside orchestrator-api, whose
// tmux client drives the HOST's tmux server, so the interpreter is chosen in one filesystem and
// executed in another. `<root>/.venv/bin/python` is a symlink to a path outside the mount there:
// it resolves on the host and dangles in the container, so the probe silently falls through to
// `python3`, which on the host has no temporalio and dies on import. A caller that knows where
// the process will actually run has to be able to say so, rather than have a guess made from the
// wrong side of the boundary.
func pythonFor(root, override string) string {
	if override != "" {
		return override
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_PYTHON")); v != "" {
		return v
	}
	if cand := filepath.Join(root, ".venv", "bin", "python"); cliutil.FileExists(cand) {
		return cand
	}
	return "python3"
}

// confirmWorker turns a worker that died on boot into an error, instead of a success.
//
// A `docker run --detach` that returns an id means the container was CREATED, which is not the same
// claim as "the worker is running" — the gap between them is every import error, every missing
// dependency, every wrong interpreter. Without this the two are indistinguishable from outside: the
// command prints a container id for something that is already gone.
//
// THE CONTAINER'S OWN LOG IS THE REPORT, and it is strictly better than the two things this
// replaced. A tmux window had to be held open to keep a dead worker's output on screen, and the
// session was killed on the way out so the next attempt would not be refused — taking the output
// with it. A pid file outlived that but had to be written, rotated and reaped by us. A stopped
// container keeps its log until something removes it, and `--rm` does not fire on a container that
// never started successfully.
//
// NOT REMOVED ON FAILURE, deliberately. The next serve replaces the pair (`devDriver.Start`), so a
// retry is never refused, and leaving the corpse means `docker logs` still answers after this error
// has scrolled away.
func confirmWorker(ctx context.Context, ref, interpreter string) error {
	const grace = 2500 * time.Millisecond
	deadline := time.Now().Add(grace)
	for {
		running, err := containerRunning(ctx, ref)
		if err != nil {
			// A failure to ASK is not a dead worker. Reported as itself rather than as a boot failure,
			// which would send somebody reading their own code for a fault in the engine.
			return fmt.Errorf("could not confirm the worker started: %w", err)
		}
		if !running {
			tail := containerTail(ctx, ref)
			if strings.TrimSpace(tail) == "" {
				return fmt.Errorf("worker exited immediately and wrote nothing (interpreter %s).\n"+
					"  if that is not where temporalio is installed in the control-plane image, this is a\n"+
					"  `--mode local` job — that one runs against this checkout\n"+
					"  logs: docker logs %s", interpreter, ref)
			}
			return fmt.Errorf("worker exited on startup (interpreter %s):\n%s\n  logs: docker logs %s",
				interpreter, indentTail(tail, 12), ref)
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// containerRunning asks the runtime, which is the only thing that knows.
func containerRunning(ctx context.Context, ref string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", ref).CombinedOutput()
	if err != nil {
		// A container that has been removed is not running, and that is an answer rather than a
		// failure to ask.
		if strings.Contains(string(out), "No such object") {
			return false, nil
		}
		return false, fmt.Errorf("docker inspect %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// containerTail is whatever the dead container printed, best effort: this is already an error path
// and a second error here would replace the reason with the failure to read it.
func containerTail(ctx context.Context, ref string) string {
	out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", "40", ref).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

// indentTail returns the last n non-empty lines, indented, for quoting inside an error.
func indentTail(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, "    "+strings.TrimRight(line, " \t"))
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}
