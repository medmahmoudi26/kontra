// workflow.go — `kontra workflow`: the CALLER's side of the runtime.
//
// Everything else in this CLI drives the callee — build an actor, push it, place it on a
// Machine, dispatch a graph at it. This drives the thing that decides what runs: a Temporal
// workflow you wrote, on a queue you own, calling deployed actors over Nexus and deployed
// deployed Actors from the caller's side.
//
//	kontra workflow serve  <folder>   # run YOUR workflows here (queue derived from workflow.json)
//	kontra workflow serve  <folder> --watch   # …and re-register the contract on every save
//	kontra workflow start  <Type>     # kick one off
//
// The graph interpreter is not going anywhere — it is the right tool for a designed pipeline
// with typed ports, a manifest and a materialized dataset. This is for the other shape: a loop,
// a branch, a wait, a fan-out whose width is a function of what the last actor returned. Code.
//
// THIS FILE IS THE VERBS AND NOTHING ELSE. It used to be 1,069 lines and four modules, and the
// three that left are named here so the next reader does not go looking:
//
//	identity.go        which queue — the Actor's and the Workflow's derivations, and the digests
//	workflowworker.go  how a served worker is launched, and how a boot failure is reported
//	claimcheck.go      how an offloaded `--wait` result is read back (the handler's codec)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/warden"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// pollerCount reports live WORKFLOW pollers on a queue — an injectable func var so tests never
// dial Temporal. The real one dials $KONTRA_ADDRESS. It lived in dispatch.go until the dispatch
// verb went; `start` is now its only caller, and a load-bearing one: it is what refuses a start
// against a queue nothing serves instead of leaving the run `running` forever.
var pollerCount = livePollerCount

func livePollerCount(queue string) (int, error) {
	d, err := newDescriber()
	if err != nil {
		return 0, err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ps, err := d.Pollers(ctx, queue, enumspb.TASK_QUEUE_TYPE_WORKFLOW)
	if err != nil {
		return 0, err
	}
	ids := map[string]struct{}{}
	for _, p := range ps {
		ids[p.Identity] = struct{}{}
	}
	return len(ids), nil
}

// leadingPositional pulls a leading non-flag argument off the front so that both
// `serve <folder> --python p` and `serve --python p <folder>` work. Go's flag package stops parsing
// at the first positional, so without this the natural spelling — subject first, options after,
// the way every other CLI reads — silently parses as stray arguments and no flags.
func leadingPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func cmdWorkflow(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra workflow <init|serve|start|replay|history> …")
	}
	switch args[0] {
	case "init":
		return cmdSourceInit("workflow", args[1:])
	case "register":
		// FIRST IN THE LIST, and first in the sentence above, because it is now the first thing you
		// do with a workflow folder — it is what records the version, the digest and the
		// `@workflow.defn` class that `start` takes.
		return cmdRegister("workflow", args[1:])
	case "serve":
		return workflowServe(args[1:])
	case "start":
		return workflowStart(args[1:])
	case "pause":
		return workflowPause(args[1:])
	case "resume":
		return workflowResume(args[1:])
	case "cancel":
		return workflowStop(args[1:], false)
	case "terminate":
		return workflowStop(args[1:], true)
	case "replay":
		// Post-mortem debugging: a recorded history, replayed against the code on disk, with no
		// clock. See workflowreplay.go for what it does and does not cover.
		return workflowReplay(args[1:])
	case "history":
		return workflowHistory(args[1:])
	default:
		return fmt.Errorf("unknown workflow subcommand %q (want register|serve|start|pause|resume|cancel|terminate|replay|history)", args[0])
	}
}

// --- cancel / terminate ------------------------------------------------------------------------

// workflowStop stops a Run. `escalate` is the difference between `cancel` and `terminate`.
//
// THE TWO ARE NOT INTERCHANGEABLE, and the difference is measured in Droplets.
//
//	CANCEL is cooperative. Temporal delivers a cancellation into the workflow, its `async with`
//	scopes run their exits, and `fleet.up`'s exit DESTROYS the Machines.
//
//	TERMINATE is unilateral. The workflow closes where it stands and no code in it runs again — so
//	no scope exit, no teardown, and whatever it provisioned keeps billing.
//
// Which is why `terminate` here cancels FIRST and only escalates if that does not land within the
// grace period. If it did not, the ordinary way to stop a run would be the way that strands
// infrastructure — and an operator reaching for the more forceful-sounding verb would be choosing
// the more expensive outcome without being told.
//
// A cancel that never lands is real, not hypothetical: a workflow blocked in an activity with no
// heartbeat cannot be interrupted until that activity returns or times out. `--force` is for the
// case where the worker is gone entirely and waiting only costs a minute.
func workflowStop(args []string, escalate bool) error {
	verb := "cancel"
	if escalate {
		verb = "terminate"
	}
	fs := flag.NewFlagSet("workflow "+verb, flag.ContinueOnError)
	force := fs.Bool("force", false, "terminate immediately, WITHOUT asking it to cancel first — scope exits will not run")
	grace := fs.Duration("grace", 60*time.Second, "how long a cancel is given to land before terminating")
	api := fs.String("api", orchestratorURL(), "orchestrator base URL")
	runID, rest := leadingPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if runID == "" && fs.NArg() == 1 {
		runID = fs.Arg(0)
	}
	if runID == "" {
		return fmt.Errorf("usage: kontra workflow %s <run-id>", verb)
	}
	if *force && !escalate {
		return errors.New("--force is a terminate: use `kontra workflow terminate --force`")
	}

	body := map[string]any{"escalate": escalate, "force": *force, "graceMs": grace.Milliseconds()}
	var out struct {
		RunID    string `json:"runId"`
		Outcome  string `json:"outcome"`
		WaitedMs int64  `json:"waitedMs"`
		Detail   string `json:"detail"`
	}
	// The run token gates it, for the same reason it gates `start`: stopping a run that holds a
	// fleet is an action that costs money either way it goes.
	client := newAuthAPI(*api, os.Getenv("KONTRA_RUN_TOKEN"))
	if err := client.postJSON("/api/runs/"+url.PathEscape(runID)+"/stop", body, &out); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "%s: %s\n", out.RunID, out.Outcome)
	// Only when there was a wait worth reporting. `--force` sends no cancel at all, and a run that
	// was already closed is answered from one describe — "waited 0.0s for the cancel to land"
	// describes a cancel that never happened.
	if out.WaitedMs >= 500 {
		fmt.Fprintf(cliio.Stdout, "  waited %.1fs for the cancel to land\n", float64(out.WaitedMs)/1000)
	}
	fmt.Fprintf(cliio.Stdout, "  %s\n", out.Detail)
	return nil
}

// --- pause / resume ----------------------------------------------------------------------------
//
// A SERVED WORKER IS A PROCESS, and pausing it is interrupting that process. There is no Temporal
// primitive for "pause a workflow": what exists is a worker that polls, and one that does not.
//
// WHAT PAUSING ACTUALLY DOES, stated because the consequences are not obvious. Stopping the worker
// stops WORKFLOW TASKS being processed, so the run makes no progress and resumes from history when
// the worker comes back — that part is exactly as durable as it sounds. But:
//
//   • Activities ALREADY DISPATCHED keep running. They are on the actors' workers, not this one, so
//     a paused caller does not pause the fleet; it stops deciding what to do next.
//   • ScheduleToStart and StartToClose timers KEEP TICKING. A long pause therefore does not hold a
//     run, it FAILS one — which is the opposite of what the word promises, so it is said here and
//     printed by the command.
//
// It is local-only and deliberately so. This writes to a tmux pane, which is the one thing the
// panels path may never do (ADR 0020 finding 3: `send-keys` executes through a read-only client).
// Keeping it in the CLI, against this host's own tmux server, is what keeps that boundary a
// boundary — the Monitor still cannot write to anything, and this is not the Monitor.

func workflowPause(args []string) error {
	fs := flag.NewFlagSet("workflow pause", flag.ContinueOnError)
	name, rest := leadingPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("usage: kontra workflow pause <file.py|session>")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev, err := warden.NewDevDriver(ctx)
	if err != nil {
		return err
	}
	worker := workflowWorkerName(name)
	if err := dev.Pause(ctx, worker, ""); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "paused %s — the worker stopped polling; the run makes no progress and resumes from history.\n", worker)
	fmt.Fprint(cliio.Stdout, "  activities already dispatched KEEP RUNNING, and ScheduleToStart/StartToClose timers keep\n"+
		"  ticking — a long pause fails a run rather than holding it.\n")
	fmt.Fprintf(cliio.Stdout, "  resume:  kontra workflow resume %s\n", name)
	fmt.Fprintf(cliio.Stdout, "  logs:    kontra logs --part workflow\n")
	return nil
}

func workflowResume(args []string) error {
	fs := flag.NewFlagSet("workflow resume", flag.ContinueOnError)
	// NO `--python`. Resume is SIGCONT on the process that is already there, holding the
	// interpreter it was served with; a flag that could not change anything would be a lie.
	name, rest := leadingPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("usage: kontra workflow resume <folder|file.py>")
	}
	// RESOLVED FIRST, AND THE WORKER NAMED OFF THE RESOLUTION. A folder has to become its
	// `workflow.py` regardless, and naming the worker from that same file is what makes every
	// spelling of one workflow reach the worker serve created. Off the raw argument,
	// `kontra workflow resume .` from inside the folder names nothing at all (`.` has no stem)
	// while the worker is recorded under the folder's name.
	file, err := workflowFileOf(name)
	if err != nil {
		return fmt.Errorf("%w — resume needs the workflow serve was given", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev, err := warden.NewDevDriver(ctx)
	if err != nil {
		return err
	}
	worker := workflowWorkerName(file)

	/*
	 * SIGCONT, NOT A RESPAWN — and this is where the mechanism change pays.
	 *
	 * The tmux implementation killed the pane and started `python <file>` fresh, which meant it had
	 * to re-derive the task queue. Its own comment named the hazard: the queue is built on the
	 * folder's CONTENT DIGEST, so editing anything between pause and resume moves it, and the
	 * "resumed" worker polls a queue the paused run is not waiting on. The run stays stuck and
	 * nothing says so.
	 *
	 * A frozen container cannot drift. It is the same container holding the same code it imported at
	 * boot, polling the same queue it has polled since.
	 */
	if err := dev.Unpause(ctx, worker, ""); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "resumed %s — the worker is polling again and the run continues from history.\n", worker)
	fmt.Fprintf(cliio.Stdout, "  logs:    kontra logs --part workflow\n")
	return nil
}

// --- serve -----------------------------------------------------------------------------------

// workflowServeArgs parses the command line, resolves the workflow file, and locates the
// checkout root. It is the pure, testable front half of `workflowServe`: every refusal that
// can be decided from the filesystem happens here.
func workflowServeArgs(args []string) (target, file, root, py, mode string, watch bool, err error) {
	fs := flag.NewFlagSet("workflow serve", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo root containing docker-compose.yml (default: walk up from CWD)")
	python := fs.String("python", "", "python interpreter (default: .venv/bin/python, else python3) — --mode local only")
	modePtr := fs.String("mode", modeLocal, "where the worker runs: `local` (this terminal, foreground) or `dev` (serve-dev: a container holding this folder, returns immediately)")
	watchPtr := fs.Bool("watch", false, "stay up and RE-REGISTER the contract on every save, so the browser form tracks your editor")

	target, rest := leadingPositional(args)
	if err = fs.Parse(rest); err != nil {
		return "", "", "", "", "", false, err
	}
	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	if target == "" {
		return "", "", "", "", "", false, errors.New("usage: kontra workflow serve <folder|file.py> [--mode local|dev]")
	}
	// ONLY TWO OF THE FOUR. `docker` needs a published worker image and `fleet` places on Machines;
	// neither exists for a caller workflow, which is somebody's file and not an Artifact. Named
	// rather than silently accepted, because `--mode fleet` reaching the local branch would run it
	// here while reading as a placement.
	if *modePtr != modeLocal && *modePtr != modeDev {
		return "", "", "", "", "", false, fmt.Errorf("unknown --mode %q for a workflow — `local` or `dev`.\n"+
			"  A caller workflow is a file you are editing, not a published Artifact, so `docker` and\n"+
			"  `fleet` (which place one) do not apply to it", *modePtr)
	}

	file, err = workflowFileOf(target)
	if err != nil {
		return "", "", "", "", "", false, err
	}
	// THE SDK COMES FROM THE IMAGE IN dev, AND FROM THE CHECKOUT IN local.
	//
	// serve-dev mounts the WORKFLOW FOLDER and nothing else, so a PYTHONPATH derived from this host's
	// checkout would point at directories the container does not have — `from kontra import
	// workflows` then fails on a path that exists perfectly well outside. `Dockerfile.orchestrator`
	// sets `KONTRA_SDK_ROOT=/opt/kontra` and asserts `/opt/kontra/sdk/python/kontra` is there, so
	// that is the root, and `python3` is its interpreter.
	//
	// Somebody iterating on the SDK ITSELF wants `--mode local`, which runs against this tree. That
	// is the line between the two: dev is for the author of the workflow, local for the author of
	// kontra.
	if *modePtr == modeDev {
		return target, file, devSDKRoot, devPython, *modePtr, *watchPtr, nil
	}
	root, err = sdkRootForServe(*repo)
	if err != nil {
		return "", "", "", "", "", false, err
	}
	py = pythonFor(root, *python)
	return target, file, root, py, *modePtr, *watchPtr, nil
}

// Where the SDK and its interpreter live INSIDE the control-plane image — `Dockerfile.orchestrator`
// sets the first as `KONTRA_SDK_ROOT` and installs the second, and its own build step asserts both.
// Stated here rather than read from the environment: this process may be running on a host with a
// checkout, and what these name is the CONTAINER serve-dev is about to start.
const (
	devSDKRoot = "/opt/kontra"
	devPython  = "python3"
)

func sdkRootForServe(explicit string) (string, error) {
	if explicit != "" {
		if hasSDK(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("--repo %s has no sdk/python (the workflow worker puts actorkit on PYTHONPATH)", explicit)
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_SDK_ROOT")); v != "" && hasSDK(v) {
		return v, nil
	}
	root, err := cliutil.FindRepoRoot("")
	if err == nil && hasSDK(root) {
		return root, nil
	}
	return "", fmt.Errorf("kontra workflow serve needs the checkout or KONTRA_SDK_ROOT (it puts actorkit on PYTHONPATH)")
}

func hasSDK(root string) bool {
	_, err := os.Stat(filepath.Join(root, "sdk", "python", "kontra"))
	return err == nil
}

// workflowServe runs the author's workflow module as a local worker. It is `python file.py`
// with the two things that are easy to get wrong done for you: the checkout's PYTHONPATH (so
// `from kontra import workflows` resolves to THIS tree, not to whatever is pip-installed) and
// the Temporal/S3 env the codec reads. The module itself calls workflows.serve().
func workflowServe(args []string) error {
	target, file, root, py, mode, watch, err := workflowServeArgs(args)
	if err != nil {
		return err
	}

	// DERIVED, NOT TYPED — see workflowQueue in identity.go. Resolved before anything starts, so a
	// folder with no manifest is a sentence here rather than a worker on the wrong queue an hour later.
	serveQueue, err := queueForWorkflow(file)
	if err != nil {
		return err
	}

	/* THE REPORT TEMPLATE, CHECKED BEFORE A WORKER STARTS (ADR 0055, spec §6.1). The same reasoning as
	   the queue above and the slot refusal in `startRun`: a report renders only when a run ENDS, so a
	   typo in `report.md` is otherwise discovered after however long the work took. A folder with no
	   template is the common case and says nothing. */
	if problems := lintReportInFolder(filepath.Dir(file)); len(problems) > 0 {
		return reportLintRefusal(problems)
	}

	// The DELTA this command adds (workflowworker.go), kept separate from the inherited environment
	// because the two paths below need it differently: the foreground child inherits and overrides,
	// while a tmux pane inherits the tmux SERVER's environment and must be told each variable
	// explicitly.
	delta := append(serveEnvDelta(root, serveQueue), watchEnv(watch)...)
	env := append(os.Environ(), delta...)

	// serve-dev: the worker in a container holding this folder, and this command RETURNS.
	//
	// THIS REPLACED `--tmux` AND THEN `--detach`, and the two dead ends are worth one sentence each.
	// tmux held the worker in a detached session that was answering four questions, three of which
	// already had better answers: whether anything is serving is the task queue's POLLERS, what the
	// worker printed is the Logs surface, and how to stop it is this command with `--replicas 0`.
	// `--detach` then answered the fourth — surviving the shell — with `setsid` and a registry of
	// pids, which was ~400 lines re-deriving what a container runtime already does, and which left
	// the worker running as a bare process inside whichever container invoked it. A container
	// answers all four (ADR 0036: one Target, and it is a container), so that is what this does.
	if mode == modeDev {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dev, err := warden.NewDevDriver(ctx)
		if err != nil {
			return err
		}
		name := workflowWorkerName(file)
		/*
		 * THE CONTAINER'S COMMAND IS THIS SAME VERB IN `--mode local`, and the recursion is the point
		 * rather than a trick.
		 *
		 * Running `python <file>` directly would mean computing the interpreter, the PYTHONPATH and
		 * the SDK root OUT HERE for a filesystem that is IN THERE — and those differ: this process may
		 * be on a host with a checkout, while the container has the image's `/opt/kontra`. Every one
		 * of those is already resolved correctly by `workflowServeArgs` when it runs inside, so the
		 * container runs the command and the command resolves its own world.
		 *
		 * `--watch` SURVIVES BECAUSE OF IT. A watcher has to be a process that outlives the save, and
		 * serve-dev leaves none out here — but the container's PID 1 is exactly that, and the folder
		 * is mounted, so an edit on the operator's disk is an edit the loop inside sees. That is the
		 * whole "fast debugging" case: save, and the Worker re-execs in a container you did not have
		 * to rebuild.
		 */
		inner := []string{"kontra", "workflow", "serve", filepath.Base(file), "--mode", modeLocal}
		if watch {
			inner = append(inner, "--watch")
		}
		// ONE HALF, NOT A PAIR. A caller workflow is a single process; the driver skips a half with
		// no argv rather than starting an empty container that would exit at once and read as a
		// crash-looping handler.
		spec := warden.Spec{
			Name: name,
			Actor: warden.ProcSpec{
				// THE FOLDER, mounted at itself and entered. `filepath.Base` above is then enough to
				// name the file, and nothing in the argv is a path that means two different things on
				// the two sides of the mount.
				Dir:  filepath.Dir(file),
				Argv: inner,
				Env:  cliutil.Derive(delta),
			},
		}
		h, err := dev.Start(ctx, spec)
		if err != nil {
			return err
		}
		fmt.Fprintf(cliio.Stdout, "serving %s\n", name)
		for _, half := range h.Halves {
			fmt.Fprintf(cliio.Stdout, "  container:  %s\n", half.Ref)
		}
		fmt.Fprintf(cliio.Stdout, "queue:      %s\n", serveQueue)
		fmt.Fprintf(cliio.Stdout, "logs:       kontra logs --part workflow\n")
		fmt.Fprintf(cliio.Stdout, "reload:     kontra workflow serve %s --mode dev   (replaces it)\n", target)
		fmt.Fprintf(cliio.Stdout, "stop:       kontra workflow unserve %s\n", target)
		// `start` TAKES THE FOLDER, never the queue. It re-derives the same queue from the same
		// folder and refuses if this code is not the one being served, so the queue above is a FACT
		// to read (a check that serve and start agree), not a string anybody pastes into a flag.
		if m := readWorkflowManifest(file); m.Workflow != "" {
			fmt.Fprintf(cliio.Stdout, "start one:  kontra workflow start %s\n", target)
		} else {
			fmt.Fprintf(cliio.Stdout, "start one:  register the folder (kontra workflow register --init) so start can derive its queue\n")
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, py, file)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = cliio.Stdout, os.Stderr
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", file, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	started := time.Now()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("worker exited: %w", err)
		}
		// A WORKER THAT EXITS 0 IMMEDIATELY DID NOT SERVE, AND SAYING NOTHING IS THE WORST ANSWER.
		//
		// `serve` runs the file and blocks on it, so a clean return means the process reached the
		// end of the module and stopped. The usual cause is the one thing a workflow file cannot
		// be written without and that nothing checks for: no `if __name__ == "__main__":
		// catalog.serve([...])`. A file that only DEFINES a workflow defines it and exits.
		//
		// MEASURED: `kontra workflow serve <folder>` on such a file printed NOTHING, exited 0, and
		// left `workflow start` to fail later against a queue nobody polls — which reads as a
		// broken control plane rather than as four missing lines. The same shape as a healthcheck
		// that passes over a dead process.
		if time.Since(started) < 10*time.Second {
			return fmt.Errorf(
				"%s exited cleanly after %s without serving — a worker is supposed to block.\n"+
					"  The usual cause is a missing entry point. A workflow file needs:\n"+
					"      if __name__ == \"__main__\":\n"+
					"          catalog.serve([YourWorkflow])\n"+
					"  without it the module defines its workflow and stops, and nothing ever polls %s",
				file, time.Since(started).Round(time.Millisecond), serveQueue)
		}
		return nil
	case <-ctx.Done():
		fmt.Fprintln(cliio.Stdout, "\nstopping…")
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
		return nil
	}
}

// --- start -----------------------------------------------------------------------------------

func workflowStart(args []string) error {
	fs := flag.NewFlagSet("workflow start", flag.ContinueOnError)
	input := fs.String("input", "", "JSON for the workflow's ONE argument (@file to read it from disk)")
	id := fs.String("id", "", "workflow id (default: <type>-<unix seconds>)")
	wait := fs.Bool("wait", false, "block until it finishes and print the result")
	timeout := fs.Duration("timeout", time.Hour, "how long --wait waits before giving up on the RESULT (the run keeps going)")
	api := fs.String("api", orchestratorURL(), "orchestrator base URL (where this Run's workflow identity is recorded)")
	target, rest := leadingPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	if target == "" {
		return errors.New("usage: kontra workflow start <folder> [--input <json>] [--wait]")
	}

	// A FOLDER, NOT A TYPE+QUEUE. The incident this closes was `--queue dhmonitor` — a nickname
	// Temporal accepts for any workflow. So start reads BOTH the @workflow.defn class and the queue
	// from the folder as it is on disk; there is no queue string to type, and no type to mistype.
	file, err := workflowFileOf(target)
	if err != nil {
		return err
	}
	// The @workflow.defn class to dispatch: the manifest names it for a registered folder, and a flat
	// file (no manifest) has it read straight from the source, the same regex `register --init` uses.
	// The SAME read carries `name` and `version` — the caller-workflow identity this Run's Datasets
	// are named after (ADR 0029 §2) — which is why the manifest is kept rather than one field taken.
	manifest := readWorkflowManifest(file)
	wfType := manifest.Workflow
	if wfType == "" {
		wfType = workflowClassIn(file)
	}
	if wfType == "" {
		return fmt.Errorf(
			"%s has no @workflow.defn class to start — add one to the file, or name it in %s (kontra workflow register %s --init)",
			filepath.Dir(file), manifestFile("workflow"), filepath.Dir(file))
	}
	queue, err := queueForWorkflow(file)
	if err != nil {
		return err
	}

	// REFUSE IF NOTHING IS SERVING THIS DIGEST — do not accept a folder and hope. Editing the folder
	// changes its digest and therefore its queue, so a stale or un-served checkout derives a queue no
	// worker polls; a start there sits `running` forever with no error, which is the exact failure
	// deriving-instead-of-typing exists to remove. Naming the fix is the point.
	n, err := pollerCount(queue)
	if err != nil {
		return fmt.Errorf("cannot verify a worker is serving %s on %q: %w\n"+
			"  serve this code first:  kontra workflow serve %s --tmux", wfType, queue, err, target)
	}
	if n == 0 {
		return fmt.Errorf("no worker is serving this folder's code — queue %q has no pollers.\n"+
			"  Copy-pasting a queue from an old chat cannot fix this: the queue is derived from the\n"+
			"  folder's content digest, so serve THIS code and start will find it:\n"+
			"    kontra workflow serve %s --mode dev", queue, target)
	}

	arg, hasArg, err := parseWorkflowInput(*input)
	if err != nil {
		return err
	}

	wfID := *id
	if wfID == "" {
		wfID = fmt.Sprintf("%s-%d", strings.ToLower(wfType), time.Now().Unix())
	}

	c, err := dialWithClaimCheck()
	if err != nil {
		return err
	}
	defer c.Close()

	ctx := context.Background()
	opts := client.StartWorkflowOptions{ID: wfID, TaskQueue: queue}
	var run client.WorkflowRun
	if hasArg {
		run, err = c.ExecuteWorkflow(ctx, opts, wfType, arg)
	} else {
		run, err = c.ExecuteWorkflow(ctx, opts, wfType)
	}
	if err != nil {
		return fmt.Errorf("start %s: %w", wfType, err)
	}
	fmt.Fprintf(cliio.Stdout, "started %s\n  id:  %s\n  run: %s\n", wfType, run.GetID(), run.GetRunID())

	// SNAPSHOT THE CALLER'S IDENTITY, so this Run's Datasets can be named after the WORKFLOW rather
	// than after whichever Actor happened to write a table (ADR 0029 §2). Temporal forgets the
	// execution after its retention window and a kept Dataset outlives it, so the identity has to be
	// recorded while it is known — ADR 0025's pattern, and the same record `POST /api/runs` writes
	// from inside `startRun` so both start paths name a Run's output identically.
	//
	// AFTER THE START, AND NEVER FATAL. The run is already going; failing the command here would
	// report a started Run as a failed start, and a retry would collide with the workflow id that
	// now exists. What is lost when the orchestrator is unreachable is bounded and visible: the
	// Dataset renders an Actor-grain name instead. Said out loud rather than swallowed, because
	// "why is my Dataset called wf-crawl4ai-… and not wf-recon-…" is otherwise unanswerable.
	if err := recordRunWorkflow(newAuthAPI(*api, os.Getenv("KONTRA_RUN_TOKEN")), run.GetID(), manifest); err != nil {
		fmt.Fprintf(cliio.Stderr, "note: could not record this run's workflow identity (%v)\n"+
			"  the run is fine; its Datasets will be named after the Actor that wrote them, not %q.\n",
			err, manifest.Name)
	}
	if !*wait {
		fmt.Fprintf(cliio.Stdout, "  wait: kontra workflow start … --wait   (or: temporal workflow show -w %s)\n", run.GetID())
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var result any
	if err := run.Get(waitCtx, &result); err != nil {
		// Deliberately not wrapped as "failed to start": the workflow DID start, and saying so
		// is the difference between looking for a bug in this command and looking at the run.
		return fmt.Errorf("run %s did not complete: %w", run.GetID(), err)
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(cliio.Stdout, "%v\n", result)
		return nil
	}
	fmt.Fprintln(cliio.Stdout, string(out))
	return nil
}

// recordRunWorkflow stamps a Run's caller-workflow identity on the orchestrator (ADR 0029 §2), the
// CLI's half of a record `POST /api/runs` writes for itself.
//
// WHY OVER HTTP AND NOT INTO THE DATABASE. The record is the orchestrator's — Postgres on a real
// install, reachable from the controller — and this CLI runs on whatever machine an operator is
// sitting at. It already mutates the Dataset record (`kontra dataset tag`, `rename`) over this same
// API for exactly that reason; a second path into the same table, holding database credentials, would
// be strictly worse.
//
// A FLAT `.py` WORKFLOW RECORDS NOTHING and that is not an error. It has no `workflow.json` to name
// or version it (its queue is digested from its own bytes), so there is no manifest identity to
// snapshot, and the Dataset name falls back to the producing Actor's — which is the honest answer
// rather than a name invented from a filename.
func recordRunWorkflow(api *apiClient, runID string, m workflowManifest) error {
	if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Version) == "" {
		return nil
	}
	body := map[string]any{"workflow": m.Name, "version": m.Version}
	// PUT: an upsert keyed by the run id, so a retried stamp states the same fact twice rather than
	// racing. No response is read — there is nothing here a caller could act on that the note above
	// does not already say.
	return api.putJSON("/api/runs/"+url.PathEscape(runID)+"/workflow", body, nil)
}

// parseWorkflowInput turns --input into the ONE argument the workflow's run method takes.
// A JSON array is one list argument, not several arguments — the ambiguity is settled here
// rather than in a caller's head, because guessing wrong sends a two-element list to a
// one-parameter method and the failure appears inside the workflow, far from the flag.
func parseWorkflowInput(in string) (any, bool, error) {
	if in == "" {
		return nil, false, nil
	}
	raw := []byte(in)
	if strings.HasPrefix(in, "@") {
		b, err := os.ReadFile(in[1:])
		if err != nil {
			return nil, false, fmt.Errorf("--input %s: %w", in, err)
		}
		raw = b
	}
	var arg any
	if err := json.Unmarshal(raw, &arg); err != nil {
		return nil, false, fmt.Errorf("--input is not JSON: %w", err)
	}
	return arg, true, nil
}
