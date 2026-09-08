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
	"github.com/medmahmoudi26/kontra/cli/internal/tmux"
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
		return errors.New("usage: kontra workflow <register|serve|start> …")
	}
	switch args[0] {
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
	default:
		return fmt.Errorf("unknown workflow subcommand %q (want register|serve|start|pause|resume|cancel|terminate)", args[0])
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
	session := workflowSession(name)
	if !tmux.HasSession(session) {
		return fmt.Errorf("no served worker in tmux session %q — `kontra workflow serve %s --tmux` starts one", session, name)
	}
	// C-c, to the WINDOW the serve command created. Not the session: a session's "current window"
	// is whatever somebody last looked at, and interrupting the wrong pane is unrecoverable.
	if out, err := exec.Command("tmux", "send-keys", "-t", session+":workflow", "C-c").CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(cliio.Stdout, "paused %s — the worker stopped polling; the run makes no progress and resumes from history.\n", session)
	fmt.Fprint(cliio.Stdout, "  activities already dispatched KEEP RUNNING, and ScheduleToStart/StartToClose timers keep\n"+
		"  ticking — a long pause fails a run rather than holding it.\n")
	fmt.Fprintf(cliio.Stdout, "  resume:  kontra workflow resume %s\n", name)
	fmt.Fprintf(cliio.Stdout, "  watch:   tmux attach -t %s\n", session)
	return nil
}

func workflowResume(args []string) error {
	fs := flag.NewFlagSet("workflow resume", flag.ContinueOnError)
	python := fs.String("python", "", "interpreter to run the file with")
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
	// RESOLVED FIRST, AND THE SESSION NAMED OFF THE RESOLUTION. Resume respawns the pane with
	// `python <file>`, so a folder has to become its `workflow.py` regardless — and naming the
	// session from that same file is what makes every spelling of one workflow reach the session
	// serve created. Off the raw argument, `kontra workflow resume .` from inside the folder names
	// nothing at all (`.` has no stem) while the worker sits in a session named after the folder.
	file, err := workflowFileOf(name)
	if err != nil {
		return fmt.Errorf("%w — resume needs the workflow serve was given", err)
	}
	session := workflowSession(file)
	if !tmux.HasSession(session) {
		return fmt.Errorf("no session %q to resume — `kontra workflow serve %s --tmux` starts one", session, name)
	}

	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("kontra workflow resume needs the checkout (it puts actorkit on PYTHONPATH): %w", err)
	}
	py := pythonFor(root, *python)

	// RESPAWN, not send-keys. After a C-c the pane is sitting in `tmux.Hold`'s trailing `read`, so
	// typing the command back into it would run it inside that wrapper and lose the hold. `-k` kills
	// what is in the pane and starts the command fresh, in the same window, keeping the scrollback
	// an operator paused in order to read.
	argv := tmux.Hold([]string{py, file})
	cmd := []string{"respawn-pane", "-k", "-t", session + ":workflow"}
	// The SAME derivation serve uses. A resume that recomputed the queue differently would respawn
	// the pane onto another queue and leave the run it was resuming unserved. Editing the folder
	// between serve and resume moves the digest and therefore the queue — a resumed worker then
	// polls a NEW queue and the live run stays put; restarting for new code is an explicit re-serve.
	resumeQueue, err := queueForWorkflow(file)
	if err != nil {
		return err
	}
	for _, kv := range serveEnvDelta(root, resumeQueue) {
		cmd = append(cmd, "-e", kv)
	}
	cmd = append(cmd, argv)
	if out, err := exec.Command("tmux", cmd...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux respawn-Pane: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(cliio.Stdout, "resumed %s — the worker is polling again and the run continues from history.\n", session)
	fmt.Fprintf(cliio.Stdout, "  watch:   tmux attach -t %s\n", session)
	return nil
}

// --- serve -----------------------------------------------------------------------------------

// workflowServe runs the author's workflow module as a local worker. It is `python file.py`
// with the two things that are easy to get wrong done for you: the checkout's PYTHONPATH (so
// `from kontra import workflows` resolves to THIS tree, not to whatever is pip-installed) and
// the Temporal/S3 env the codec reads. The module itself calls workflows.serve().
func workflowServe(args []string) error {
	fs := flag.NewFlagSet("workflow serve", flag.ContinueOnError)
	python := fs.String("python", "", "python interpreter (default: .venv/bin/python, else python3)")
	useTmux := fs.Bool("tmux", false, "run the worker in a DETACHED tmux session and return, instead of holding this terminal")
	watch := fs.Bool("watch", false, "stay up and RE-REGISTER the contract on every save, so the browser form tracks your editor")
	target, rest := leadingPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	if target == "" {
		return errors.New("usage: kontra workflow serve <folder|file.py>")
	}
	file, err := workflowFileOf(target)
	if err != nil {
		return err
	}
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("kontra workflow serve needs the checkout (it puts actorkit on PYTHONPATH): %w", err)
	}
	py := pythonFor(root, *python)

	// DERIVED, NOT TYPED — see workflowQueue in identity.go. Resolved before anything starts, so a
	// folder with no manifest is a sentence here rather than a worker on the wrong queue an hour later.
	serveQueue, err := queueForWorkflow(file)
	if err != nil {
		return err
	}

	// The DELTA this command adds (workflowworker.go), kept separate from the inherited environment
	// because the two paths below need it differently: the foreground child inherits and overrides,
	// while a tmux pane inherits the tmux SERVER's environment and must be told each variable
	// explicitly.
	delta := append(serveEnvDelta(root, serveQueue), watchEnv(*watch)...)
	env := append(os.Environ(), delta...)

	// --tmux: hand the terminal back, and leave the worker somewhere it can be WATCHED.
	//
	// The same gesture `kontra serve --tmux` makes for an actor's Worker, for the same reason — and
	// with one more consequence here. A local `kontra-*` session IS the Dashboard's inventory
	// (`control/orchestrator/src/panels/local.ts`), so serving this way is also what makes the workflow
	// worker appear as a Terminal without anything else being registered.
	//
	if *useTmux {
		session := workflowSession(file)
		if err := tmux.Start(session, tmux.KontraWorkflowTag(session), []tmux.Proc{{
			Window: "workflow",
			Dir:    root,
			Env:    delta,
			Argv:   []string{py, file},
		}}); err != nil {
			return err
		}
		if err := confirmTmuxWorker(session, "workflow", py); err != nil {
			return err
		}
		tmux.PrintHelp(cliio.Stdout, session, []string{"workflow"})
		// `start` TAKES THE FOLDER, never the queue. It re-derives the same queue from the same
		// folder and refuses if this code is not the one being served, so the queue below is a FACT
		// to read (a check that serve and start agree), not a string anybody pastes into a flag.
		if m := readWorkflowManifest(file); m.Workflow != "" {
			fmt.Fprintf(cliio.Stdout, "\nstart one:  kontra workflow start %s\n", target)
		} else {
			fmt.Fprintf(cliio.Stdout, "\nstart one:  register the folder (kontra workflow register --init) so start can cliutil.Derive its queue\n")
		}
		fmt.Fprintf(cliio.Stdout, "queue:      %s\n", serveQueue)
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
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("worker exited: %w", err)
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
			"    kontra workflow serve %s --tmux", queue, target)
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
