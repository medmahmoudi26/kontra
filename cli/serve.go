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

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/cli/internal/queues"
	"github.com/medmahmoudi26/kontra/cli/warden"
)

// The four modes. One vocabulary: a mode is a PLACE a Worker runs.
//
// `dev` IS LOCAL'S SIBLING, NOT DOCKER'S, and the grouping below is where that matters. It needs
// everything `local` needs — the manifest, the repo root, both argvs, both environments — and
// differs in exactly one thing: the driver. `docker` and `fleet` need none of that, because they
// hand a published Artifact to something else. So `dev` takes the local branch and swaps the driver
// at the end of it, which is why there is no `runDev` beside `runElsewhere`.
const (
	modeLocal  = "local"
	modeDev    = "dev"
	modeDocker = "docker"
	modeFleet  = "fleet"
)

var runModes = []string{modeLocal, modeDev, modeDocker, modeFleet}

// elsewhere reports whether a mode hands the Worker to something that is not this checkout. It is
// the test that decides how much of `cmdServe` runs, and it is a function rather than
// `mode != modeLocal` because that spelling silently swallowed `dev` the moment it existed.
func elsewhere(mode string) bool { return mode == modeDocker || mode == modeFleet }

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
		"  local:  the actor and handler as processes on this machine, in the foreground\n"+
		"  dev:    serve-dev — the same two, in containers, from THIS folder on a bind mount.\n"+
		"          Returns immediately; re-run it to reload after an edit\n"+
		"  docker: a managed worker container from a PUBLISHED image (`kontra deploy` first)\n"+
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
			"    kontra fleet deploy --actor %s               # build the Bundle and place the Worker\n"+
			"  Then `kontra panels list` shows them as fleet:<machine>/kontra-%s/{actor,handler}.\n"+
			"  The mode exists in ids and in the Dashboard; the PLACEMENT verb is `kontra fleet`, which owns the\n"+
			"  stack, the key and the converge — this command will not grow a second implementation of it.",
			actorDir, m.Name)
	}

	// --mode docker. The flags that only mean something locally are named rather than ignored: a
	// worker container's control plane is RESOLVED for it (`resolveWorkerPlane` — the running
	// install's bound addresses, else compose DNS), so a host-shaped `--redis 127.0.0.1:6379`
	// passed through would point it at its own loopback rather than at this box's.
	//
	// `fs.Visit` and NOT a comparison against the default: `--redis`'s default is
	// `cliutil.EnvOr("KONTRA_REDIS_HOST", "127.0.0.1:6379")`, so an operator who typed the default value
	// EXPLICITLY would have been ignored by a value comparison — measured, and it started a container
	// while looking like it had refused. Visit reports what was typed, which is the actual question.
	var localOnly []string
	local := map[string]bool{"python": true, "engine": true, "redis": true}
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
	fmt.Fprintf(cliio.Stdout, "%s@%s — %d worker container(s) on %s (image %s)\n",
		res.Actor, res.Version, res.Running, res.Network, res.Image)
	// WHICH CONTROL PLANE, printed on every scale. A worker pointed at the wrong one looks
	// identical to a healthy one everywhere else — it runs, it restarts, and it polls a queue
	// nobody dispatches to — so the address it was given is output, not a debug detail.
	fmt.Fprintf(cliio.Stdout, "  polling:  %s   (%s)\n", res.ControlPlane, res.Source)
	for _, w := range res.Workers {
		fmt.Fprintf(cliio.Stdout, "  %s\n", w)
	}
	if len(res.Stopped) > 0 {
		fmt.Fprintf(cliio.Stdout, "  stopped: %s\n", strings.Join(res.Stopped, ", "))
	}
	fmt.Fprintf(cliio.Stdout, "\n  logs:     docker logs -f %s\n", firstOr(res.Workers, "<container>"))
	fmt.Fprintf(cliio.Stdout, "  workers:  kontra workers list\n")
	fmt.Fprintf(cliio.Stdout, "  stop:     kontra serve --actor %s --mode docker --replicas 0\n", actorDir)
	fmt.Fprintf(cliio.Stdout, "\ndispatch with:\n  kontra actor %s dispatch --input <dataset|file>\n", m.Name)
	return nil
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
	}
	return fallback
}

// serveRoot is the tree `--mode local` reads the SDK and the handler out of.
//
// THE SAME TWO ANSWERS `workflow serve` ALREADY RESOLVES THROUGH (sdkRootForServe), in the same
// order: KONTRA_SDK_ROOT is the container's, a checkout is the laptop's. They were NOT the same
// here — this path went straight to FindRepoRoot — and the difference was the Actors page's Serve
// button. That button runs inside orchestrator-api, which has no checkout and no shell, and it
// answered `no docker-compose.yml found walking up from CWD (pass --repo <dir>)`: a sentence about
// an operator's working directory, produced by a process that has none, naming a flag this command
// does not have.
func serveRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv("KONTRA_SDK_ROOT")); v != "" && hasSDK(v) {
		return v, nil
	}
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return "", fmt.Errorf("kontra serve needs the checkout or KONTRA_SDK_ROOT "+
			"(it runs the handler and puts actorkit on PYTHONPATH): %w", err)
	}
	return root, nil
}

// prebuiltHandler is the compiled handler to run, or "" when the caller should build from source.
//
// `<root>/handler` is where a tree that has no `runtime/handler` sources keeps the binary
// `control/images/Dockerfile.workerbase` builds — the same handler, compiled once at image build
// instead of on every serve. KONTRA_HANDLER_BIN overrides it, and a value that is set and wrong is
// a REFUSAL rather than a silent fall back to `go run .`: falling back would answer a typo in a
// path with an error about a missing Go toolchain.
func prebuiltHandler(root string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("KONTRA_HANDLER_BIN")); v != "" {
		if !isExecutableFile(v) {
			return "", fmt.Errorf("KONTRA_HANDLER_BIN=%s is not an executable file", v)
		}
		return v, nil
	}
	if cand := filepath.Join(root, "handler"); isExecutableFile(cand) {
		return cand, nil
	}
	return "", nil
}

func isExecutableFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	actorDir := fs.String("actor", "", "actor directory (contains actor.json)")
	python := fs.String("python", "", "python interpreter (default: .venv/bin/python, else python3)")
	// EMPTY MEANS DETECT, not "py". The default was `py`, so serving a GO actor without saying so
	// ran `python <dir>/actor.py` against a directory that has never held one — and the failure is
	// `can't open file …/actor.py`, which reads as a missing file rather than as the wrong engine.
	// The Actors page's Serve button passes no engine at all (there is no control for it, on
	// purpose — see control/orchestrator/src/actorControl.ts), so under the old default that button could
	// only ever serve Python actors, and it reported a Python error for every Go one.
	engine := fs.String("engine", "", "actor engine: py|go (default: detected from the folder)")
	// NOT localhost by default: compose publishes Redis on the VPC address, and the actor's
	// durable state (the commit map, session_state) lives there.
	redis := fs.String("redis", cliutil.EnvOr("KONTRA_REDIS_HOST", "127.0.0.1:6379"),
		"host:port of the Redis holding actor state")
	mode := fs.String("mode", modeLocal,
		"where to run the worker: local (this machine) | docker (a managed worker container) | fleet (a Machine)")
	replicas := fs.Int("replicas", 1, "--mode docker only: how many worker containers to run (0 stops them)")
	watch := fs.Bool("watch", false,
		"--mode local only: re-exec the Worker when a file in the actor directory changes")
	// THE NETWORK, AND WHY A FLAG RATHER THAN A DEFAULT THAT ALWAYS WORKS. `resolveWorkerPlane`
	// picks `bridge` against a running install and the compose `kontra` network otherwise, and
	// that is right on a machine whose firewall lets a container reach the host. MEASURED on a box
	// with ufw's default `deny (incoming)`: a container's packets to the bridge GATEWAY go through
	// the host's INPUT chain — unlike a published port, which goes through FORWARD and DOCKER-USER
	// — so every connection to the install times out. The worker container stays up, both
	// processes keep running, nothing is logged, and `docker ps` shows a healthy replica.
	// `--network host` is the escape: the container shares this host's network stack, so the
	// install's address is reachable the same way it is from a shell here.
	//
	// IT COSTS THE NETWORK NAMESPACE, which is a real part of what a container gives you, so it is
	// typed rather than fallen back to.
	network := fs.String("network", "", "--mode docker only: the docker network for the worker containers (default: resolved from the control plane; `host` when a firewall blocks the bridge)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *actorDir == "" {
		return errors.New("usage: kontra serve --actor <dir> [--mode local|dev|docker|fleet]")
	}
	if err := checkRunMode(*mode); err != nil {
		return err
	}
	// The mirror of `runElsewhere`'s local-only check, and for the same reason: a flag that only
	// means something in the OTHER mode must be refused rather than ignored. `fs.Visit` reports
	// what was typed, so `--network bridge` — which happens to be the resolved default — is still
	// caught.
	if !elsewhere(*mode) {
		typedNetwork := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "network" {
				typedNetwork = true
			}
		})
		if typedNetwork {
			return errors.New("--network only applies to --mode docker; local and dev workers take the control plane's own network")
		}
	}
	m, err := readManifest(*actorDir)
	if err != nil {
		return err
	}
	// `--mode` is resolved BEFORE anything local is prepared: the two other modes need the manifest
	// and nothing else, and `findRepoRoot` below is a local-mode requirement (it runs handler/ from
	// source) that would otherwise fail for a mode that never touches this checkout's handler.
	if elsewhere(*mode) {
		// REFUSED RATHER THAN IGNORED. `--watch` re-execs a pair this process supervises; docker and
		// fleet hand the worker to something else, so honouring it would mean watching files here
		// and reloading nothing. A flag that is silently a no-op is worse than one that is absent.
		if *watch {
			return fmt.Errorf("--watch applies to --mode local only; %s runs the Worker elsewhere, "+
				"so nothing here could re-exec it", *mode)
		}
		return runElsewhere(*mode, *actorDir, m, *replicas, *network, fs)
	}
	root, err := serveRoot()
	if err != nil {
		return err
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
		"KONTRA_ADDRESS="+config.TemporalAddress(),
		"KONTRA_NAMESPACE="+config.TemporalNamespace(),
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
	// Mirrors the Bundle's PYTHONPATH so an author's `from kontra import actor` resolves to
	// the checkout, not to whatever happens to be pip-installed.
	//
	// cliutil.Derive(), not append(env, …), and that is not style. Two appends onto one slice with spare
	// capacity write the SAME index: `handlerEnv := append(env, "GOWORK=off")` silently replaced
	// the PYTHONPATH this line had just added, so the actor process was started without it. It
	// went unnoticed because the repo venv has the SDK pip-installed, which makes PYTHONPATH
	// redundant exactly where it was being lost — it only surfaces on an interpreter that
	// doesn't (a bare python3, a fresh checkout, a worktree).
	actorEnv := cliutil.Derive(env,
		"PYTHONPATH="+filepath.Join(root, "sdk", "python")+":"+
			filepath.Join(root, "runtime", "python")+":"+
			filepath.Join(root, "sdk", "python", "_gen"))
	// runtime/handler, not handler/ — the handler moved and this call site was missed when
	// bundle.go:237 was updated. A stale path here fails at `kontra serve`, which is the verb
	// the README calls "the one you use ninety percent of the time".
	handlerArgv := []string{"go", "run", "."}
	handlerDir := filepath.Join(root, "runtime", "handler")
	// A COMPILED HANDLER WHEN THE TREE SHIPS ONE, `go run .` only when it does not. See serveRoot:
	// this command runs in the control plane as well as in a checkout, and a container has no Go
	// toolchain — `go run .` there fails with `exec: "go": executable file not found`, which reads
	// as a broken install rather than as a missing build dependency for one code path.
	prebuilt, err := prebuiltHandler(root)
	if err != nil {
		return err
	}
	if prebuilt != "" {
		handlerArgv = []string{prebuilt}
		handlerDir = root
	}
	handlerEnv := cliutil.Derive(env, "GOWORK=off")

	// THE DRIVER, AND WHY THERE IS ONE. Everything above is `--mode local`'s ARGUMENT — which
	// interpreter, which environment, which two argvs — and everything below is running it. ADR 0036
	// keeps `process` (code executed directly, no runtime required) as a DRIVER rather than as a
	// Target, and this is the only caller of it today: the pair below is what a **Warden** will start
	// on a **Machine** through the same four verbs, with `podman` behind them instead. driver.go
	// holds the seam and the reasoning; nothing about what this command does changed when it grew one.
	//
	// WHO HOLDS THE PAIR IS THE DRIVER'S BUSINESS AND NOT THE SPEC'S: the same actor and the same
	// handler run under every one of them, and the only difference is who their parent is. That is
	// why `--mode dev` above swaps this line and nothing else.
	drv := warden.NewProcessDriver(cliio.Stdout, os.Stderr)
	spec := warden.Spec{
		Name:    m.Name,
		Version: m.Version,
		// the actor — a Temporal activity worker. It self-registers to the catalog on boot.
		Actor: warden.ProcSpec{Dir: root, Argv: actorArgv, Env: actorEnv},
		// the handler — the workflow half. This is what `kontra workers list` counts on the shared
		// queue; the actor counts on its own.
		//
		// `go run .` from inside handler/, not `go run ./handler` from the root: handler/ is its own
		// module and the repo root has no go.mod, so the root form only works when the workspace is
		// active — and GOWORK=off above is what makes the build reproducible.
		Handler: warden.ProcSpec{Dir: handlerDir, Argv: handlerArgv, Env: handlerEnv},
	}
	// The banner, as a closure, because `--watch` prints it again after every reload: the queue is
	// the line that matters there — an operator's next `start` goes to a remembered name, and the
	// whole loop depends on that name NOT having moved (it is derived from the manifest, not from
	// the folder's bytes; see `workflowQueue`'s history for what the alternative cost).
	announce := func() {
		fmt.Fprintf(cliio.Stdout, "%s@%s — actor on %s-sessions, handler on %s (temporal %s)\n",
			m.Name, m.Version, queues.Shared(m.Name, m.Version), queues.Shared(m.Name, m.Version),
			config.TemporalAddress())
	}

	// serve-dev: the same spec, in containers, and this command RETURNS.
	//
	// AFTER `announce` AND BEFORE THE PROCESS DRIVER, because everything above is the ARGUMENT — which
	// interpreter, which two argvs, which environment — and it is identical for both. The only thing
	// that differs is who holds the pair, which is the whole reason `workerDriver` is a seam.
	if *mode == modeDev {
		if *watch {
			// `--watch` re-execs a pair THIS process supervises, and serve-dev leaves nothing here to
			// supervise. The reload is the command itself: `Start` replaces a pair of the same name,
			// so running it again is what picks up an edit.
			return fmt.Errorf("--watch applies to --mode local only; serve-dev returns immediately, " +
				"so nothing would be left watching. Re-run `--mode dev` to reload after an edit")
		}
		dev, err := warden.NewDevDriver(ctx)
		if err != nil {
			return err
		}
		// `--replicas 0` IS THE STOP VERB, spelled the way `--mode docker` spells it. serve-dev has no
		// other count — a pair is one pair — so any other value is refused rather than rounded down to
		// "the pair", which would make `--replicas 3` look like it did something.
		if *replicas == 0 {
			dev.Remove(ctx, m.Name, m.Version)
			fmt.Fprintf(cliio.Stdout, "%s@%s — serve-dev pair stopped\n", m.Name, m.Version)
			return nil
		}
		if *replicas != 1 {
			return fmt.Errorf("--replicas %d does not apply to --mode dev: a serve-dev Worker is one "+
				"pair holding one folder. Use 0 to stop it, or --mode docker to run several", *replicas)
		}
		h, err := dev.Start(ctx, spec)
		if err != nil {
			return err
		}
		announce()
		for _, half := range h.Halves {
			fmt.Fprintf(cliio.Stdout, "  %s  %s\n", half.Part, half.Ref)
		}
		fmt.Fprintf(cliio.Stdout, "\n  reload:   kontra serve --actor %s --mode dev   (replaces this pair)\n", *actorDir)
		fmt.Fprintf(cliio.Stdout, "  stop:     kontra serve --actor %s --mode dev --replicas 0\n", *actorDir)
		fmt.Fprintf(cliio.Stdout, "\ndispatch with:\n  kontra actor %s dispatch --input <dataset|file>\n", m.Name)
		return nil
	}

	if *watch {
		return serveWatchLoop(ctx, drv, spec, absActor, announce)
	}

	h, err := drv.Start(ctx, spec)
	if err != nil {
		return err
	}

	announce()
	fmt.Fprintf(cliio.Stdout, "dispatch with:\n  kontra actor %s dispatch --input <dataset|file>\n", m.Name)

	select {
	case err := <-drv.Exited(h):
		// A half that ended ends the command, and the OTHER half goes with it immediately — no
		// drain, because the pair is already broken and a lingering handler holds its Temporal lease
		// while units time out one by one (this file's header).
		stop()
		_ = drv.Stop(context.Background(), h, 0)
		return err
	case <-ctx.Done():
		fmt.Fprintln(cliio.Stdout, "\nstopping…")
		// The exit code belongs to the WORKER: Ctrl-C on a healthy pair is a successful stop, so a
		// teardown that goes wrong is reported and does not become this command's status.
		if err := drv.Stop(context.Background(), h, serveDrain); err != nil {
			fmt.Fprintf(cliio.Stderr, "note: %v\n", err)
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
