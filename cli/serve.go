package main

// `kontra serve --actor <dir> --mode <fleet|docker|local>` — ONE verb, three places a Worker runs.
//
// IT WAS `kontra run`, and the word named the wrong half of the system. `run` is the CALLER's verb
// — a dispatch runs, and leaves output behind — while this command sends nothing: it stands a
// Worker up and waits for somebody else to dispatch to it. Carrying both meanings also left `run`
// and `runs` one letter apart while naming opposite ends of the same pipeline. An actor SERVES; the
// old word is a redirect in main.go rather than an alias, so it cannot mean both again.
//
// `--mode local` (the default) runs the actor and the handler on THIS machine, exactly the way a
// fleet Machine runs them; with `--tmux` they go into a detached `kontra-<actor>` session, one
// window per process, which is also what the Dashboard discovers as `local:<host>/…` (ADR 0020,
// slice 6). It is kontra's DEV ENVIRONMENT: a peer of the other two modes, not a debug affordance.
//
// `--mode docker` runs the same Worker as a managed worker CONTAINER (`scale.go`), which is the
// same thing `scale_actor` does over MCP — one replica by default.
//
// `--mode fleet` is deliberately NOT routed here: placing a Worker on a Machine is two infra
// operations with money attached, and `kontra fleet` already owns them. The error names them.
//
// A worker is TWO processes (ADR 0018): the actor itself, which is a Temporal activity worker
// polling `{actor}-{version}-sessions`, and the Go handler, which owns the workflow on the shared
// queue. Everything else a worker needs — Temporal, Redis, S3, the catalog — it reaches OUT to on
// the Controller.
//
// Whichever half dies takes the other with it. A half-dead worker is worse than a dead one: it
// keeps its Temporal lease and units time out one by one.
//
// THE SAFETY ASYMMETRY BETWEEN THE MODES IS REAL AND IT POINTS AT LOCAL (ADR 0020, finding 2). A
// fleet session's panes hold `journalctl -fu` and systemd supervises the Worker, so a viewer that
// crashes that tmux server costs the view. A `--tmux` session's panes hold the REAL actor and
// handler, so there the same crash costs a running Worker. `--tmux` is still the right default for
// development — it is what makes the output readable — but it is why `kontra panels list` and the
// Dashboard label a local Terminal as such.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The three modes, spelled the same here, in the streamer's Terminal ids and in
// `kontra panels list`. One vocabulary: a mode is a place a Worker runs.
const (
	modeLocal  = "local"
	modeDocker = "docker"
	modeFleet  = "fleet"
)

var runModes = []string{modeLocal, modeDocker, modeFleet}

// checkRunMode refuses an unknown mode by name rather than falling back to local. A typo that
// silently ran the Worker HERE — on a laptop, against the production Temporal — is the failure this
// prevents.
func checkRunMode(mode string) error {
	for _, m := range runModes {
		if mode == m {
			return nil
		}
	}
	return fmt.Errorf("unknown --mode %q — one of %s.\n"+
		"  local:  the actor and handler on this machine (add --tmux for a detached session)\n"+
		"  docker: a managed worker container on this host's engine\n"+
		"  fleet:  a Machine, which `kontra fleet` owns", mode, strings.Join(runModes, ", "))
}

// runElsewhere is `--mode docker` and `--mode fleet`: the two destinations that are not this
// machine.
//
// docker ROUTES — `runScale` is the container path and it is already the thing `scale_actor` calls,
// so `kontra serve --mode docker` is one replica of exactly that and nothing new. fleet REFUSES with
// the two commands that do it today, because placing a Worker on a Machine is `fleet up` (which
// creates droplets and costs money) followed by `fleet deploy` (which builds a Bundle and places
// it). Half-rewriting that here would give the same operation two implementations, and the one in
// `fleet.go` is the one with the Pulumi stack, the SSH key and the tests.
func runElsewhere(mode, actorDir string, m actorManifest, replicas int, network string, fs *flag.FlagSet) error {
	if mode == modeFleet {
		return fmt.Errorf("`kontra serve --mode fleet` is not a shortcut for the two commands that place a Worker on a Machine:\n"+
			"    kontra fleet up --count <N> --role <role>            # make the Machines (this spends money)\n"+
			"    kontra fleet deploy --actor %s [--tmux]   # build the Bundle and place the Worker\n"+
			"  Then `kontra panels list` shows them as fleet:<machine>/kontra-%s/{actor,handler}.\n"+
			"  The mode exists in ids and in the Dashboard; the PLACEMENT verb is `kontra fleet`, which owns the\n"+
			"  stack, the key and the converge — this command will not grow a second implementation of it.",
			actorDir, m.Name)
	}

	// --mode docker. The flags that only mean something locally are named rather than ignored: a
	// worker container's control plane is RESOLVED for it (`resolveWorkerPlane` — the running
	// appliance's bound addresses, else compose DNS), so a host-shaped `--redis 127.0.0.1:6379`
	// passed through would point it at its own loopback rather than at this box's.
	//
	// `fs.Visit` and NOT a comparison against the default: `--redis`'s default is
	// `envOr("KONTRA_REDIS_HOST", "127.0.0.1:6379")`, so an operator who typed the default value
	// EXPLICITLY would have been ignored by a value comparison — measured, and it started a container
	// while looking like it had refused. Visit reports what was typed, which is the actual question.
	var localOnly []string
	local := map[string]bool{"python": true, "engine": true, "tmux": true, "redis": true}
	fs.Visit(func(f *flag.Flag) {
		if local[f.Name] {
			localOnly = append(localOnly, "--"+f.Name)
		}
	})
	sort.Strings(localOnly)
	if len(localOnly) > 0 {
		return fmt.Errorf("%s %s only apply to --mode local; a worker container is configured by its image and by the\n"+
			"  control plane this box is running — `kontra up`'s bound addresses if one is up, else the compose\n"+
			"  service names. Drop them, or run without --mode docker.",
			strings.Join(localOnly, ", "), "flag"+plural(len(localOnly)))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := runScale(ctx, scaleOpts{name: m.Name, version: m.Version, replicas: replicas, network: network})
	if err != nil {
		return fmt.Errorf("running %s@%s as worker container(s): %w\n"+
			"  the image comes from `kontra deploy --actor %s` (local `kontra/%s-worker:%s`, else the registry)",
			m.Name, m.Version, err, actorDir, m.Name, m.Version)
	}
	fmt.Fprintf(stdout, "%s@%s — %d worker container(s) on %s (image %s)\n",
		res.Actor, res.Version, res.Running, res.Network, res.Image)
	// WHICH CONTROL PLANE, printed on every scale. A worker pointed at the wrong one looks
	// identical to a healthy one everywhere else — it runs, it restarts, and it polls a queue
	// nobody dispatches to — so the address it was given is output, not a debug detail.
	fmt.Fprintf(stdout, "  polling:  %s   (%s)\n", res.ControlPlane, res.Source)
	for _, w := range res.Workers {
		fmt.Fprintf(stdout, "  %s\n", w)
	}
	if len(res.Stopped) > 0 {
		fmt.Fprintf(stdout, "  stopped: %s\n", strings.Join(res.Stopped, ", "))
	}
	fmt.Fprintf(stdout, "\n  logs:     docker logs -f %s\n", firstOr(res.Workers, "<container>"))
	fmt.Fprintf(stdout, "  workers:  kontra workers list\n")
	fmt.Fprintf(stdout, "  stop:     kontra serve --actor %s --mode docker --replicas 0\n", actorDir)
	// A container carries no tmux session, so it has no Terminal to watch yet — said here rather
	// than left for someone to discover from an empty wall.
	fmt.Fprintf(stdout, "\n  A worker container has no tmux session, so `kontra panels list` shows it with SESSION=NO-TMUX:\n")
	fmt.Fprintf(stdout, "  nothing creates one inside the image. `--mode local --tmux` is the mode with live Terminals today.\n")
	fmt.Fprintf(stdout, "\ndispatch with:\n  kontra actor %s dispatch --input <dataset|file>\n", m.Name)
	return nil
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
	}
	return fallback
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	actorDir := fs.String("actor", "", "actor directory (contains actor.json)")
	python := fs.String("python", "", "python interpreter (default: .venv/bin/python, else python3)")
	// EMPTY MEANS DETECT, not "py". The default was `py`, so serving a GO actor without saying so
	// ran `python <dir>/actor.py` against a directory that has never held one — and the failure is
	// `can't open file …/actor.py`, which reads as a missing file rather than as the wrong engine.
	// The Actors page's Serve button passes no engine at all (there is no control for it, on
	// purpose — see backend/src/actorControl.ts), so under the old default that button could
	// only ever serve Python actors, and it reported a Python error for every Go one.
	engine := fs.String("engine", "", "actor engine: py|go (default: detected from the folder)")
	// NOT localhost by default: compose publishes Redis on the VPC address, and the actor's
	// durable state (the commit map, session_state) lives there.
	redis := fs.String("redis", envOr("KONTRA_REDIS_HOST", "127.0.0.1:6379"),
		"host:port of the Redis holding actor state")
	useTmux := fs.Bool("tmux", false,
		"run the worker in a detached tmux session (one window per process) instead of the foreground")
	mode := fs.String("mode", modeLocal,
		"where to run the worker: local (this machine) | docker (a managed worker container) | fleet (a Machine)")
	replicas := fs.Int("replicas", 1, "--mode docker only: how many worker containers to run (0 stops them)")
	// THE NETWORK, AND WHY A FLAG RATHER THAN A DEFAULT THAT ALWAYS WORKS. `resolveWorkerPlane`
	// picks `bridge` against a running appliance and the compose `kontra` network otherwise, and
	// that is right on a machine whose firewall lets a container reach the host. MEASURED on a box
	// with ufw's default `deny (incoming)`: a container's packets to the bridge GATEWAY go through
	// the host's INPUT chain — unlike a published port, which goes through FORWARD and DOCKER-USER
	// — so every connection to the appliance times out. The worker container stays up, both
	// processes keep running, nothing is logged, and `docker ps` shows a healthy replica.
	// `--network host` is the escape: the container shares this host's network stack, so the
	// appliance's address is reachable the same way it is from a shell here.
	//
	// IT COSTS THE NETWORK NAMESPACE, which is a real part of what a container gives you, so it is
	// typed rather than fallen back to.
	network := fs.String("network", "", "--mode docker only: the docker network for the worker containers (default: resolved from the control plane; `host` when a firewall blocks the bridge)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *actorDir == "" {
		return errors.New("usage: kontra serve --actor <dir> [--mode local|docker|fleet]")
	}
	if err := checkRunMode(*mode); err != nil {
		return err
	}
	// The mirror of `runElsewhere`'s local-only check, and for the same reason: a flag that only
	// means something in the OTHER mode must be refused rather than ignored. `fs.Visit` reports
	// what was typed, so `--network bridge` — which happens to be the resolved default — is still
	// caught.
	if *mode == modeLocal {
		typedNetwork := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "network" {
				typedNetwork = true
			}
		})
		if typedNetwork {
			return errors.New("--network only applies to --mode docker; a local worker is two processes on this machine and has no container network")
		}
	}
	m, err := readManifest(*actorDir)
	if err != nil {
		return err
	}
	// `--mode` is resolved BEFORE anything local is prepared: the two other modes need the manifest
	// and nothing else, and `findRepoRoot` below is a local-mode requirement (it runs handler/ from
	// source) that would otherwise fail for a mode that never touches this checkout's handler.
	if *mode != modeLocal {
		return runElsewhere(*mode, *actorDir, m, *replicas, *network, fs)
	}
	root, err := findRepoRoot("")
	if err != nil {
		return fmt.Errorf("kontra serve needs the checkout (it runs handler/ from source): %w", err)
	}
	absActor, err := filepath.Abs(*actorDir)
	if err != nil {
		return err
	}

	// ONE venv: the actor host and the Temporal SDK share an interpreter — and ONE RULE for picking
	// it, `pythonFor`, rather than this copy of the probe without the KONTRA_PYTHON escape hatch.
	//
	// THE COPY WAS BROKEN IN THE ONE PLACE THE ESCAPE HATCH EXISTS FOR, and it is the Serve button:
	// this command runs inside orchestrator-api, whose tmux client drives the HOST's tmux server, so
	// the interpreter is chosen in one filesystem and executed in another. `<root>/.venv/bin/python`
	// is a symlink that resolves on the host and does not exist in the container — so the probe fell
	// through to `python3`, the host ran that, and the worker died on `import temporalio` with the
	// folder saved, the serve returned 200 and every count on the Actors grid unchanged.
	// `workflowworker.go:pythonFor` had already been fixed for exactly this; serving an ACTOR went through
	// here and had not.
	py := pythonFor(root, *python)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := append(os.Environ(),
		"KONTRA_ACTOR_NAME="+m.Name,
		"KONTRA_ACTOR_VERSION="+m.Version,
		"KONTRA_ADDRESS="+temporalAddress(),
		"KONTRA_NAMESPACE="+temporalNamespace(),
		"KONTRA_REDIS_HOST="+*redis,
		// WHERE TO REGISTER. `publish_catalog` returns immediately when this is unset, so an actor
		// run locally never told the catalog anything — and the failure is silent by design (a
		// worker must serve whether or not the orchestrator is reachable), so nothing said so.
		//
		// MEASURED: a catalog of twenty-three actors, every one of them a leftover from some
		// earlier session, several still in a registration format that predates per-Method
		// schemas. Running an actor is the ONE moment the truth is available — the schemas are
		// reflected from the types the running code declares — and it was the moment kontra threw
		// it away.
		//
		// It is set here rather than left to the shell for the tmux path in particular: a pane
		// inherits the tmux SERVER's environment, so a variable the operator exported reaches the
		// actor not at all unless it is passed explicitly.
		"KONTRA_ORCHESTRATOR_URL="+orchestratorURL(),
	)

	// The actor's argv and the extra env it needs, shared by both the foreground and the tmux
	// path so the two cannot drift into starting different processes.
	eng, err := engineFor(absActor, *engine, m)
	if err != nil {
		return err
	}
	actorArgv := []string{filepath.Join(absActor, m.Name)}
	if eng != "go" {
		actorArgv = []string{py, filepath.Join(absActor, "actor.py")}
	}
	// Mirrors the Bundle's PYTHONPATH so an author's `from actorkit import actor` resolves to
	// the checkout, not to whatever happens to be pip-installed.
	//
	// derive(), not append(env, …), and that is not style. Two appends onto one slice with spare
	// capacity write the SAME index: `handlerEnv := append(env, "GOWORK=off")` silently replaced
	// the PYTHONPATH this line had just added, so the actor process was started without it. It
	// went unnoticed because the repo venv has the SDK pip-installed, which makes PYTHONPATH
	// redundant exactly where it was being lost — it only surfaces on an interpreter that
	// doesn't (a bare python3, a fresh checkout, a worktree).
	actorEnv := derive(env,
		"PYTHONPATH="+filepath.Join(root, "sdk", "python")+":"+
			filepath.Join(root, "runtime", "python")+":"+
			filepath.Join(root, "sdk", "python", "_gen"))
	handlerArgv := []string{"go", "run", "."}
	handlerDir := filepath.Join(root, "handler")
	handlerEnv := derive(env, "GOWORK=off")

	// THE DRIVER, AND WHY THERE IS ONE. Everything above is `--mode local`'s ARGUMENT — which
	// interpreter, which environment, which two argvs — and everything below is running it. ADR 0036
	// keeps `process` (code executed directly, no runtime required) as a DRIVER rather than as a
	// Target, and this is the only caller of it today: the pair below is what a **Warden** will start
	// on a **Machine** through the same four verbs, with `podman` behind them instead. driver.go
	// holds the seam and the reasoning; nothing about what this command does changed when it grew one.
	//
	// The `--tmux` choice belongs to the DRIVER and not to the spec: the same actor and the same
	// handler run either way, and the only difference is who is their parent.
	drv := &processDriver{out: stdout, err: os.Stderr, tmux: *useTmux}
	spec := workerSpec{
		Name:    m.Name,
		Version: m.Version,
		// the actor — a Temporal activity worker. It self-registers to the catalog on boot.
		Actor: procSpec{Dir: root, Argv: actorArgv, Env: actorEnv},
		// the handler — the workflow half. This is what `kontra workers list` counts on the shared
		// queue; the actor counts on its own.
		//
		// `go run .` from inside handler/, not `go run ./handler` from the root: handler/ is its own
		// module and the repo root has no go.mod, so the root form only works when the workspace is
		// active — and GOWORK=off above is what makes the build reproducible.
		Handler: procSpec{Dir: handlerDir, Argv: handlerArgv, Env: handlerEnv},
	}
	h, err := drv.start(ctx, spec)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s@%s — actor on %s-sessions, handler on %s (temporal %s)\n",
		m.Name, m.Version, sharedQueue(m.Name, m.Version), sharedQueue(m.Name, m.Version),
		temporalAddress())
	if *useTmux {
		printTmuxHelp(stdout, tmuxSession(m.Name, m.Version), []string{"actor", "handler"})
		fmt.Fprintf(stdout, "\ndispatch with:\n  kontra actor %s dispatch --input <dataset|file>\n", m.Name)
		return nil
	}
	fmt.Fprintf(stdout, "dispatch with:\n  kontra actor %s dispatch --input <dataset|file>\n", m.Name)

	select {
	case err := <-drv.exited(h):
		// A half that ended ends the command, and the OTHER half goes with it immediately — no
		// drain, because the pair is already broken and a lingering handler holds its Temporal lease
		// while units time out one by one (this file's header).
		stop()
		_ = drv.stop(context.Background(), h, 0)
		return err
	case <-ctx.Done():
		fmt.Fprintln(stdout, "\nstopping…")
		// The exit code belongs to the WORKER: Ctrl-C on a healthy pair is a successful stop, so a
		// teardown that goes wrong is reported and does not become this command's status.
		if err := drv.stop(context.Background(), h, serveDrain); err != nil {
			fmt.Fprintf(stderr, "note: %v\n", err)
		}
		return nil
	}
}

// serveDrain is how long a foreground pair gets to answer SIGTERM before it is killed.
//
// It is a BACKSTOP and almost never the thing that ends them: `exec.CommandContext` kills both the
// moment the signal handler cancels ctx, which is what has ended `kontra serve` on Ctrl-C since it
// was written. What this covers is the case that had no cover before — a half that ignores the
// signal, which used to leave the command waiting on it forever with the terminal already gone.
const serveDrain = 10 * time.Second

// derive returns base + extra as a FRESH slice, so two derivations of one environment cannot
// overwrite each other through a shared backing array.
func derive(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}
