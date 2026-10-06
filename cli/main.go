// The kontra CLI — the local control surface for the kontra control plane.
//
// stdlib-only routing (flag.FlagSet + a switch), no cobra. Every command talks to
// things that are already running — orchestrator HTTP, Temporal gRPC, the Docker
// Engine API. The ONE sanctioned shell-out is `docker compose` in `kontra infra`
// (compose IS the deployment contract); actors are NEVER exec'd by this CLI — an
// actor is run by its language inside its own image.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/medmahmoudi26/kontra/cli/internal/buildinfo"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/cli/warden"
)

// Hand-drawn on purpose. Printed to cliio.Stderr so piped cliio.Stdout (JSONL output) stays clean.
const banner = `
 _  _____  _  _ _____ ___    _
| |/ / _ \| \| |_   _| _ \  /_\
| ' < (_) | .' | | | |   / / _ \
|_|\_\___/|_|\_| |_| |_|_\/_/ \_\
`

func usage() { fmt.Fprint(os.Stderr, usageText) }

// printBanner writes the banner to cliio.Stderr, and ONLY when cliio.Stderr is a terminal.
//
// Two gates, for two different readers. cliio.Stderr is the stream so that piped cliio.Stdout (JSONL output)
// stays clean; the terminal check is for the process nobody is watching as a terminal at all.
// `kontra mcp` holds stdio open for a whole agent session and the agent keeps cliio.Stderr as that
// server's log, where four lines of ASCII art per session are noise it has to read past — and a
// redirect, a cron line or a CI step buy the same nothing. The banner is decoration for a human,
// so it is printed when there is one.
func printBanner() {
	if !isTTY(os.Stderr) {
		return
	}
	fmt.Fprint(os.Stderr, banner)
}

// --- terminal presentation ---
//
// These three lived in console.go until the console went with the dispatch routes it wrapped.
// They are not the console's: `paint` styles `kontra doctor`'s status columns and `kontra panels
// list`'s header, and `isTTY` is what decides whether styling happens at all.

// useColor gates ANSI styling on a real terminal (NO_COLOR / non-tty → plain).
var useColor = os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// usageText is a CONST rather than a literal inside usage(), so a test can read what this CLI
// claims to offer. Help is the one surface nothing else checks — the switch below is exercised by
// every command's tests and this text by none of them — and a rename that reaches the switch but
// not this block advertises a word that now only answers with a redirect.
const usageText = `kontra — local control surface

  kontra version                                   # which kontra this is; "dev (<rev>)" when unreleased
  kontra init                                      # create ~/.kontra/: config.yaml, workflows/, actors/
                                                   # GENERATES the console login into
                                                   #   <home>/console-password (0600), never stdout
  kontra user add <name>                           # a second console login; only the hash is stored
  kontra token mint <state|explore|panel|run>      # fill a BLANK token in an EXISTING config
                                                   # run blank means the Run surface is OPEN: serve,
                                                   #   start, stop and a Dataset's tag/rename admit
                                                   #   anyone who can reach the API
                                                   # state/panel blank means that surface is DISABLED
                                                   #   and answers 503 — this is the fleet-stranding
                                                   #   recovery, without hand-editing YAML
                                                   # init mints these on a NEW install only, so this
                                                   #   is how an older installation closes the gap

  kontra doctor [--api <url>]                      # infra state: services, web consoles, actors
  kontra up [--data-dir <dir>] [--bind <ip>] [--temporal-port 7233] [--s3-port 8333]
               [--kv-port 6379] [--codec-port 18234] [--registry-port 5000]
               [--orchestrator auto|local|bundle|none|<path>] [--api-port 8088]
               [--temporal-ui] [--temporal-ui-port 8233]
               # THE APPLIANCE (ADR 0031): runs Temporal, the object store, the state
               # store, the payload codec and the OCI registry IN THIS PROCESS, and the
               # orchestrator as a supervised child hydrated from the bundle. No
               # containers. Persists to <data-dir>, so history, objects, global_state and
               # the lake's catalog all survive a restart. Blocks; Ctrl-C stops the child
               # first, then the services.
               # --orchestrator: in a compiled checkout, auto runs YOUR build and says so;
               #   bundle forces the hydrated artifact, none starts the services alone.
               # --temporal-ui: OFF by default. Hydrates Temporal's own Web UI from the CAS
               #   and serves it on loopback — stack traces, pending-activity detail and a
               #   manual signal/terminate console, for the failures kontra's own surfaces
               #   cannot yet show. Its codec address is derived, never configured.
  kontra infra up|down|status [--repo <dir>]       # the compose control plane (the other topology)
  kontra control up [--preview] [--json] [--stack local] [--workspaces <dir>]
        [--bind 127.0.0.1] [--api-port 8088] [--program <dir>]
               # THE HOST ENGINE (ADR 0052 §1): converges kontra-control — 15 containers on
               # one private Docker network, declared as Pulumi YAML — by shelling to the host
               # pulumi. Its state is file://<.kontra>/state, and the backend is ASSERTED
               # before any operation: a failed "pulumi login" does not stop pulumi, it
               # creates an ephemeral Pulumi Cloud account and deploys THERE.
               # ONE PROJECT, AND EVERY OTHER IS REFUSED. --stack kontra-fleet/<tag> is not
               #   honoured here — a Fleet goes API -> Temporal -> infra worker so it leaves a
               #   Lease, a run record and an audit trail; this path would leave none.
               # --preview: the same converge through "pulumi preview". Prints per-operation
               #   counts, NAMES what would be replaced, and exits 0 only when the converge is
               #   a no-op, so CI can gate on it. Takes no lock.
  kontra control down [--stack local]              # destroy the containers and the network; KEEPS
               # all 13 volumes (retainOnDelete) and the Pulumi state. "compose down", not "down -v".
  kontra update [--check] [--to <tag>] [--stack local] [--program <dir>] [--workspaces <dir>]
               # MOVE THIS INSTALLATION TO NEWER IMAGES (ADR 0052 §7). The SAME converge as
               # "kontra control up" — same program, same engine, same lock, same volume gate —
               # with a different desired state. An install nobody can upgrade is one people
               # pin and abandon.
               # EVERY VOLUME IS ACCOUNTED FOR BEFORE ANYTHING IS APPLIED, twice: nine of the
               #   thirteen carry protect: true in the program, so pulumi refuses to PLAN their
               #   deletion (measured — it fails in preview, for any caller). And the identity
               #   of every volume this install has is asserted against "pulumi stack export":
               #   still named what it was, still attached to the same service. Not redundant —
               #   a volume MOVED to another service is a plan with no volume step in it at all,
               #   nothing refuses it, and the stack comes up healthy and empty.
               # "pulumi stack export" is written before every converge and its path printed,
               #   for kontra_pulumi-state: it records every cloud Machine the control plane
               #   owns, so losing it loses the ability to destroy Machines that keep billing.
               # --check: preview only, exits non-zero if anything would change, so CI can gate.
               # --to: one release tag for every kontra-owned image, read out of the program.
               # NO SCHEMA MIGRATION, deliberately (§7). An image expecting a shape its volume
               #   does not have fails its own health gate and fails the converge with that
               #   error in it — loud, where a half-migration is data nothing can read.
  kontra serve --actor <dir> [--mode local|dev|docker] [--engine py|go] [--python <bin>]
               [--redis <host:port>] [--replicas N] [--network <name>] [--watch]
               # serve the actor HERE (actor + handler), no image build — it waits for a dispatch
               # --watch: RE-EXEC the pair on save. Nothing builds and nothing uploads — local mode
               #   runs python <dir>/actor.py from the directory, the Go handler is generic, and the
               #   queue is <name>-<version> off the manifest so an edit does not move it. In-flight
               #   Units are DRAINED before the swap. Local mode only.
               # --mode dev: serve-dev — the same pair in CONTAINERS holding this folder on a bind
               #   mount, from the control plane's own image. Returns immediately; re-run it to
               #   reload after an edit, --replicas 0 to stop. What it printed is kontra logs.
               # --mode docker: N managed worker CONTAINERS, wired to whichever control plane
               #   this box runs (kontra up's bound addresses, else the compose service names)
               # --network: docker mode only. "host" is the answer when a host firewall drops a
               #   container's packets to the bridge gateway — the worker then comes up polling
               #   nothing, and nothing reports it
  kontra build --actor <dir> [--push <ref>] [--registry <host:port>] [--json]
               # the actor's ARTIFACT: a Bundle, published as an OCI artifact (ADR 0036)
               # KONTRA OWNS NO REGISTRY. --push takes any OCI reference — ghcr, GitLab,
               #   Harbor, an airgap mirror — and the reference MUST name a registry host:
               #   one with no host is Docker Hub, whose 100 manifest reads/hour/IP one
               #   Fleet exhausts for every other build on that address.
               # with no --push: <registry>/bundles/<name>:<version>, which is the address
               #   a Fleet placement resolves. --registry names only that first component.
               # --json: one document on cliio.Stdout (the digest is a field, not a line to grep)
  kontra bundle orchestrator [--out <dir>] [--platform goos/goarch|list|all]
               # THE APPLIANCE BUNDLE (ADR 0031 §2): a pinned Node runtime, the compiled
               # orchestrator and its native addons, as one content-addressed tar.gz with
               # a manifest naming every component, version and digest. Builds for this
               # host by default. Nothing is fetched that is not checksummed first.
               # --platform all builds the four the appliance ships — linux and macOS on
               #   amd64 and arm64 — from any one of them; every compiled file in each
               #   tree is checked against the platform its manifest claims.
  kontra bundle spa [--out <dir>]                  # the built SPA as its own content-addressed
               # tar.gz. Separate from the orchestrator bundle because it is platform-neutral
               # and changes when a page does; kontra up hydrates both and prints both digests.
  kontra bundle verify <bundle.tar.gz>             # re-cliutil.Derive every digest the manifest claims
  kontra release [--version <v>] [--platform goos/goarch|list|all] [--out <dir>]
               # ONE FILE PER PLATFORM: the kontra binary, the orchestrator bundle for that
               # platform and the browser bundle, packed where an installed binary already
               # looks for them, plus a SHA256SUMS an installer can check. 'all' releases
               # the four; the Go half cross-builds (no cgo) and CI still runs this
               # natively on four runners, because an artifact nobody executed is a claim.
  kontra rebase [<actor>[@<version>]] [--runtime <name>:<major>] [--dry-run]
               # MOVE AN IMAGE ONTO A NEWER RUNTIME WITHOUT REBUILDING IT (ADR 0061). "pack rebase"
               #   rewrites the manifest: the deps and app layers are untouched and only the run-image
               #   layers below them change. Seconds, not a rebuild per actor.
               # THE VERSION DOES NOT CHANGE. A version now names a SEQUENCE of digests; the catalog
               #   keeps the rest in "history" and the "inuse-" reconciler tags all of it, because a
               #   Lease keeps its old digest until it drops.
               # With no argument it LISTS what is behind and rebases nothing.
  kontra registry migrate --from <host:port> [--to <host:port>] [--dry-run]
               # COPY A registry:2 STORE INTO ZOT, by tag and then by digest. Verifies every catalog
               #   digest resolves at the destination before it says it is done, and refuses to report
               #   success while any does not — an untagged manifest the catalog still references is a
               #   digest a Placement is pinned to.
  kontra deploy --actor <dir> [--engine py|go] [--registry host:port]
               [--controller <host>] [--host-only] [--override]   # the container-Image spelling
  kontra workers list
  kontra actor register <dir> [--init] [--json]
  kontra actor schema <dir> [--method NAME]        # what each Method TAKES and EMITS, as JSON Schema,
               # derived from the code ON DISK — no orchestrator, no registration, no deploy.
               # The same derivation the catalog publishes (kontra.schema.schema_of), so a form
               # built from this cannot disagree with what the Method will accept.
  kontra workspace seed|watch|list|use|create [--dir <path>]
               # named workspaces under KONTRA_WORKSPACES; seed hello on empty parent;
               # watch the current child and publish actor artifacts.
  kontra workflow register <dir> [--init] [--workflow <Class>] [--json]
               # DECLARE it, without serving or running it: records the path, the manifest,
               # the version and a content digest, and creates the Actor's Nexus endpoint
  kontra workflow serve <folder|file.py> [--mode local|dev] [--repo <dir>] [--python <bin>] [--watch]
               # run YOUR Temporal workflows here; they dispatch deployed Actors
               # the queue is DERIVED from the folder's content, never typed (no --queue)
               # --repo: checkout root containing docker-compose.yml (default: walk up from CWD)
               # --watch: RE-REGISTER the contract on every save, so the browser form tracks
               #          your editor; a file that no longer imports becomes a visible STATE
  kontra workflow start <folder> [--input <json|@file>] [--id <id>] [--wait]
  kontra workflow pause | resume <file.py>         # stop / restart the SERVED WORKER, in its pane
               # the run makes no progress and resumes from history; dispatched activities keep
               # running, and its timeouts keep ticking — a long pause fails a run, it does not hold one
  kontra workflow history <run-id> [-o FILE]       # save a run's history as JSON (shareable, replayable)
  kontra report preview <run-id> [--template report.md] [--open]
               # render a report template against a FINISHED run and print the Markdown
               # nothing is stored; workflow serve lints report.md, this shows what it SAYS
               # a workflow puts things in its report by RETURNING them
  kontra workflow replay <workflow.py> (--run-id ID | --history FILE) [--json]
               # REPLAY a recorded history against the code on disk. NO CLOCK: no heartbeat, no
               #   StartToClose, nothing times out while you sit on a breakpoint — unlike attaching
               #   to a live activity, which gets ~2 minutes.
               # Post-mortem: a run that failed on a fleet days ago, stepped through on a laptop.
               # ACTIVITY CODE IS NOT RUN — a Method's results come from the history as values, so
               #   this covers the CALLER's decisions (splitting, chaining, branching), not a Method.
               # exit 0 clean · 1 non-deterministic against this history · 2 could not run
  kontra workflow cancel <run-id>                  # graceful: scope exits run, so a fleet is DESTROYED
  kontra workflow terminate <run-id> [--force]     # cancel, then terminate if it does not settle
               # terminating alone skips scope exits, so a fleet it held would keep billing
  kontra fleet up --count N --actor <dir> [--tag <t>] [--fleet <name>]
               # converge Machines through Pulumi, as a Temporal workflow on the Controller
               # a Fleet is named after what it places: <actor>-<version>
  kontra fleet deploy --actor <dir> [--image <ref>]   # place the Artifact
  kontra fleet preview | status | down [--fleet <name>]
  kontra warden join --controller https://<controller>:8443 --token <kw1....>
               # ON A MACHINE (ADR 0037): exchange a one-time token for an mTLS identity that
               # persists, then install and start kontra-warden.service. The token carries the
               # Fleet CA's fingerprint, so the secret is never sent to a Controller that cannot
               # prove it holds that CA.
  kontra warden serve [--driver podman|process] [--state <dir>]
               # the reconcile loop: what the control plane asked for, against what the runtime
               # actually holds. Outbound only — a Machine opens no listening socket.
  kontra warden status                             # who this Machine is, and what is running on it
  kontra warden ca serve|token|list [--dir <dir>]  # ON THE CONTROLLER: the Fleet CA and enrolment
  kontra dataset list                              # every dataset: standalone (loaded) + output (actor-produced)
  kontra dataset query <name> --sql "SELECT ..."   # query any dataset; runs on the orchestrator (--local for here)
  kontra dataset create|anew <file.csv|.jsonl|.parquet> <name>   # load a standalone dataset (anew = append new rows)
  kontra dataset delete <name>                     # (db is an alias for dataset)
  kontra dataset tag <name> --add TAG [--remove TAG] [--version V] [--dt P] [--run ID]
               # KEEP this output past its TTL. Tags are a SET; the record is keyed by runId
  kontra dataset rename <name> (--to NEWNAME | --reset)          # override the DERIVED run-grain name
  kontra explore <run-id>                          # QUERY A RUN'S OUTPUT: typed views per node, in local DuckDB
               [--sql "SELECT ..."] [--ttl 900]    # one-shot SQL; presigned-URL lifetime
               [--ui] [--print-init]               # DuckDB web UI (fetches assets online) | show the init script
  kontra explore --catalog                         # cross-run, READ-ONLY over the shared DuckLake catalog
  kontra runs list [--tenant T] [--status S]      # runs (Temporal Visibility): status, tenant, stages, S3 path
  kontra runs --run-id <id>                       # run detail: per-node state + live heartbeat progress + blob counts
  kontra explore <actor[@version]> [--dt <date>] --sql "SQL"   # query an actor's output dataset by name
  kontra runs --run-id <id> --duckdb              # materialize output & open it in the DuckDB UI
  kontra runs [--run-id <id>] --state [--tenant T] [--status S]     # live run/node state, real-time (all runs, or one)
  kontra schedule list | describe --id <n> | pause|resume|trigger --id <n> | delete --id <n>
               # read/steer the Temporal Schedules this installation has. There is no create:
               # see schedule.go. The Dataset retention sweep ('kontra-dataset-retention') is
               # registered at boot by orchestrator-infra and appears here; it PREVIEWS and
               # deletes nothing until KONTRA_RETENTION_COLLECT=1 is set on the worker holding
               # the lake (the materializer). 'trigger' runs a sweep now, in that same mode.
  kontra mcp [--api <url>]                         # MCP server (stdio) for agents: list/describe actors, read a
               # Scratch, poll run status, query datasets, drive a fleet. NO dispatch tool: dispatching
               # is the caller's, so an agent writes a workflow and runs workflow serve|start

  Env: KONTRA_ORCHESTRATOR_URL (http://localhost:8088)
       KONTRA_ADDRESS (localhost:7233)   KONTRA_NAMESPACE (default)
       KONTRA_EXPLORE_TOKEN or KONTRA_STATE_TOKEN   # explore is token-gated: it mints presigned URLs
       KONTRA_HOME (~/.kontra)                      # this installation's config.yaml, workflows/, actors/
`

func main() {
	args := os.Args[1:]
	printBanner()
	if len(args) == 0 {
		// Bare `kontra` prints usage. It used to drop a terminal into the interactive console;
		// the console was a REPL over the dispatch routes and went with them.
		usage()
		os.Exit(2)
	}
	// `.kontra/config.yaml` fills gaps in the environment, and never overrides it — so this is
	// safe to do before dispatch, for every command, including the ones that need nothing.
	// Best-effort: a malformed config must fail the command that needs a value, not `kontra help`.
	config.LoadAndApplyConfig()

	err := dispatch(args)
	// A WORD THIS CLI DOES NOT HAVE exits 2 and prints the whole usage; a command that ran and
	// FAILED exits 1 and prints one line. Scripts distinguish the two, so the sentinel carries the
	// difference back out of dispatch rather than each case calling os.Exit itself.
	if errors.Is(err, errUsage) {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// errUsage marks "that is not a command here" — see main for why it is a sentinel and not an exit.
var errUsage = errors.New("unknown command")

// dispatch is the verb table. Split out of main so the routing itself is testable: a rename that
// left the old word working, or a new one wired to nothing, is a switch-statement bug and there
// was no way to write a test that saw it.
func dispatch(args []string) error {
	var err error
	switch args[0] {
	case "version", "--version", "-v":
		// A VERB AND TWO FLAGS, because all three are what people type and "unknown command" to any
		// of them is a bad first impression from a tool whose next question is "which version are
		// you on". They are spelled here rather than parsed elsewhere: `dispatch` is the one place
		// that decides what a word means.
		fmt.Fprintln(os.Stdout, buildinfo.Version())
	case "init":
		err = config.CmdInit(args[1:])
	case "user":
		// `kontra user add <name>` — a second console login, for the second engineer. The login
		// itself is ADR 0045; this is the only way to make another after install.
		if len(args) < 2 || args[1] != "add" {
			err = fmt.Errorf("usage: kontra user add <name>")
		} else {
			err = config.CmdUserAdd(os.Stdout, args[2:])
		}
	case "token":
		// `kontra token mint <key>` — fill a BLANK token in an existing config. The one thing
		// `kontra init` cannot do, because it writes the file only when it is absent: an install
		// made before a key was minted keeps the blank, and for `run` a blank means the Run surface
		// is OPEN. Every message that reports one of these blanks ("Set KONTRA_RUN_TOKEN to gate
		// them", "set one of KONTRA_STATE_TOKEN") named no command until this one.
		if len(args) < 2 || args[1] != "mint" {
			err = fmt.Errorf("usage: kontra token mint <state|explore|panel|run>")
		} else {
			err = config.CmdTokenMint(os.Stdout, args[2:])
		}
	case "doctor":
		err = cmdDoctor(args[1:])
	case "up":
		err = cmdUp(args[1:])
	case "infra":
		err = cmdInfra(args[1:])
	case "control":
		// THE HOST ENGINE (ADR 0052 §1-§2): `kontra-control` as a Pulumi YAML program, converged by
		// shelling to the host `pulumi`. It is a third word for a third topology only until issue 08
		// retires the appliance — §2 spells this command `kontra up`, and it cannot be spelled that way
		// while `case "up":` above routes to `cli/up.go` and eight assertions in `cli/up_test.go` pin
		// that behaviour. `cli/control.go`'s header records the one-line swap and why it is one line.
		err = cmdControl(args[1:])
	case "update":
		// ADR 0052 §7. A TOP-LEVEL WORD AND NOT `kontra control update`, because §7 spells it
		// `kontra update` and because the noun an operator is updating is the installation, not one
		// subsystem of it — the word has to survive issue 08 retiring `control` back into `up`, and
		// `cli/update.go` ends in the same `converge` this case's neighbour does.
		err = cmdUpdate(args[1:])
	case "rebase":
		// Moving an actor image onto a newer digest of the SAME runtime major, by rewriting its manifest.
		// The version does not change — see ADR 0061 on why a version now names a sequence of digests.
		err = cmdRebase(args[1:])
	case "registry":
		// The OCI store, not the actor catalog. One subcommand today: `migrate`, which moves a
		// `registry:2` store into zot by digest and proves every catalog digest arrived.
		err = cmdRegistry(args[1:])
	case "build":
		err = cmdBuild(args[1:])
	case "bundle":
		// The APPLIANCE bundle (ADR 0031 §2) — a pinned Node, the compiled orchestrator and its
		// native addons. `kontra build` produces the other kind, an actor's Bundle for a fleet
		// Machine; see cli/bundlecmd.go for why one word carries both.
		err = cmdBundle(args[1:])
	case "release":
		// One file per platform: the binary, the orchestrator bundle and the SPA, packed the way
		// an installed binary already expects to find them (ADR 0031 §2, issue 17).
		err = cmdRelease(args[1:])
	case "deploy":
		err = cmdDeploy(args[1:])
	case "serve":
		err = cmdServe(args[1:])
	case "report":
		err = reportCmd(args[1:])
	case "workers":
		err = cmdWorkers(args[1:])
	case "workflow", "wf":
		err = cmdWorkflow(args[1:])
	case "workspace":
		err = cmdWorkspace(args[1:])
	case "actor":
		err = cmdActor(args[1:])
	case "fleet":
		err = cmdFleet(args[1:])
	case "warden":
		// The Machine's own process (ADR 0037), and the Controller's half of its enrolment. Both
		// under one word because they are two ends of ONE protocol, and a reader who finds one has
		// to be able to find the other.
		err = warden.Command(args[1:])
	case "dataset", "ds":
		err = cmdDataset(args[1:])
	case "db":
		// Alias: `db list/create/anew/delete` is `dataset` by another name. Kept so existing
		// scripts and muscle memory keep working while `dataset` is the one surface.
		err = cmdDB(args[1:])
	case "explore":
		// Alias: `explore <actor>` maps to querying that actor's output dataset.
		err = cmdExplore(args[1:])
	case "runs":
		err = cmdMonitor(args[1:])
	case "schedule":
		err = cmdSchedule(args[1:])
	case "monitor":
		// Deliberately a REDIRECT, not an alias. `Monitor` is one of the web app's four surfaces
		// — the read-only wall that WATCHES Workers; leaving it as a working alias here would keep
		// the word meaning two opposite things -- read a run, and produce data -- which is exactly
		// the ambiguity the rename exists to remove. Fail loudly with the new spelling.
		err = fmt.Errorf("`kontra monitor` is now `kontra runs` (same flags).\n" +
			"  \"Monitor\" names the web surface that WATCHES Workers, not a command.")
	case "run":
		// A REDIRECT for the same reason `monitor` is one, and not an alias. `run` is the
		// CALLER's word — a workflow you serve and start is what makes work happen — while this
		// command only stands a Worker up and waits for somebody else to call it. Left as an
		// alias, `run` would mean both halves of that at once, one letter from `runs`, which
		// reads the OUTPUT of the thing it does not do.
		err = fmt.Errorf("`kontra run --actor` is now `kontra serve --actor` (same flags).\n" +
			"  An actor SERVES: the command stands a Worker up and waits. What RUNS is YOUR workflow\n" +
			"  (`kontra workflow serve|start`), and `kontra runs` is where you watch one.")
	case "mcp":
		err = cmdMCP(args[1:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("%w %q", errUsage, args[0])
	}
	return err
}
