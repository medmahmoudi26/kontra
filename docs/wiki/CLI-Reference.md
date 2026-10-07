# CLI Reference

Every verb the `kontra` binary exposes, grouped by what you are doing. Run `kontra --help` for the
authoritative text — this page is that surface organised, with the reasoning kept where it changes
what you would type.

```
kontra <noun> <verb> [flags]
```

**Conventions used below:** `<required>` · `[optional]` · `a|b` means pick one.

---

## Environment

Four variables cover almost everything:

| Variable | Default | What it addresses |
|---|---|---|
| `KONTRA_ORCHESTRATOR_URL` | `http://localhost:8088` | the control plane's API |
| `KONTRA_ADDRESS` | `localhost:7233` | Temporal |
| `KONTRA_NAMESPACE` | `default` | Temporal namespace |
| `KONTRA_HOME` | `~/.kontra` | this installation's `config.yaml`, `workflows/`, `actors/` |

Token variables are documented in [[Configuration]]. The one worth knowing here: **`kontra explore`
is token-gated** (`KONTRA_EXPLORE_TOKEN` or `KONTRA_STATE_TOKEN`) because it mints presigned URLs.

---

## Install and bootstrap

### `kontra init`
Creates `~/.kontra/`: `config.yaml`, `workflows/`, `actors/`.

> **It generates the console login into `~/.kontra/console-password` (mode 0600), never stdout** —
> stdout in the Compose install is `docker compose logs cli`. Deleted that file? `kontra user add
> <name>`. It also mints the four tokens on a *new* install only.

### `kontra user add <name>`
A second console login. Only the scrypt hash is stored, so a leaked config yields something to
attack offline, never something to sign in with.

### `kontra token mint <state|explore|panel|run>`
Fill a **blank** token in an existing config — how an older installation closes a gap `init` would
have covered.

- **`run` blank means the Run surface is OPEN**: serve, start, stop and a Dataset's tag/rename
  admit anyone who can reach the API.
- **`state` / `panel` blank means that surface is DISABLED** and answers `503`. This is the
  fleet-stranding recovery path, without hand-editing YAML.

### `kontra version`
Which kontra this is — `dev (<rev>)` when unreleased.

### `kontra doctor [--api <url>]`
Infra state: services, web consoles, actors. The first thing to run when something looks wrong.

---

## Running the stack

There are **two topologies**, and picking the wrong verb is the common first mistake.

### `kontra up` — the appliance
```
kontra up [--data-dir <dir>] [--bind <ip>] [--api-port 8088]
          [--orchestrator auto|local|bundle|none|<path>] [--temporal-ui]
```
Runs Temporal, the object store, the state store, the payload codec and the OCI registry **in this
process**, with the orchestrator as a supervised child. **No containers.** Persists to `<data-dir>`,
so history, objects, global state and the lake's catalog survive a restart. Blocks; Ctrl-C stops the
child first.

`--temporal-ui` is off by default and hydrates Temporal's own Web UI on loopback — for the failures
kontra's surfaces cannot yet show (stack traces, pending-activity detail, manual signal/terminate).

### `kontra infra up|down|status [--repo <dir>]` — the compose control plane
The other topology: the Docker Compose stack.

### `kontra control up [--preview] [--check] [--to <tag>]` — the host engine
Converges `kontra-control` — 13 containers on one private Docker network, declared as Pulumi YAML.

> **Every volume is accounted for before anything is applied, twice.** Eight of eleven carry
> `protect: true` so Pulumi refuses to *plan* their deletion, and the identity of every volume is
> asserted against `pulumi stack export` — because a volume **moved** to another service produces a
> plan with no volume step in it, nothing refuses it, and the stack comes up healthy and empty.
>
> `--check` previews only and exits non-zero if anything would change, so CI can gate on it.

### `kontra registry migrate --from <host:port> [--to <host:port>] [--dry-run]`
Copies every repository and tag out of a `registry:2` store into **zot**, by digest, then re-reads each
one at the destination and compares — and then checks that every digest *the catalog* knows about
resolves, which is a separate pass because a tag that moved leaves the catalog's older digest reachable
by no tag at all.

`--to` defaults to `--registry` / `KONTRA_REGISTRY`. The same address on both sides is refused by name:
it would copy every tag onto itself and report success. An install that still has an unmigrated
`registry:2` store **refuses to start** until this has run ([[Deployment]] §2a).

---

## Authoring an Actor

### `kontra serve --actor <dir>`
```
kontra serve --actor <dir> [--mode local|dev|docker] [--engine py|go]
             [--replicas N] [--network <name>] [--watch]
```
Serve the actor **here** — actor + handler, no image build. It waits for a dispatch.

| mode | what it does |
|---|---|
| `local` *(default)* | runs `python <dir>/actor.py` from the directory |
| `dev` | the same pair in **containers** on a bind mount, from the control plane's image. Returns immediately; `--replicas 0` to stop |
| `docker` | N managed worker containers wired to whichever control plane this box runs |

`--watch` re-execs the pair on save. Nothing builds, nothing uploads, and the queue is
`<name>-<version>` off the manifest so an edit does not move it. **In-flight Units are drained
before the swap.** Local mode only.

`--network host` is the answer when a host firewall drops a container's packets to the bridge
gateway — otherwise the worker comes up polling nothing and nothing reports it.

### `kontra actor register <dir> [--init] [--json]`
Declare the Actor to the catalog.

### `kontra actor schema <dir> [--method NAME]`
What each Method **takes** and **emits**, as JSON Schema, derived from the code **on disk** — no
orchestrator, no registration, no deploy. The same derivation the catalog publishes, so a form built
from this cannot disagree with what the Method will accept.

### `kontra build --actor <dir> [--push <ref>] [--registry <host:port>] [--json]`
The Actor's **artifact**: a Bundle, published as an OCI artifact (ADR 0036).

> **kontra owns no registry.** `--push` takes any OCI reference — ghcr, GitLab, Harbor, an airgap
> mirror — and **the reference must name a registry host**: one without a host is Docker Hub, whose
> 100 manifest reads/hour/IP a single Fleet exhausts for everyone else on that address.

With no `--push`: `<registry>/bundles/<name>:<version>`, the address a Fleet placement resolves.

### `kontra deploy --actor <dir> [--engine py|go] [--registry host:port]`
The container-**Image** spelling: builds and pushes a self-contained worker image.

> Cloud Native Buildpacks replace the generated Dockerfile this still uses, and `actor.json`'s
> `runtime` field is how an actor picks what it is built on. The resolver and the `pack` invocation are
> committed and tested; **neither is wired into this verb**, so the `runtime` field changes nothing
> today. `kontra runtime build|test|import` and `kontra rebase` do not exist — [[Runtimes]].

### `kontra workers list`
What is polling.

---

## Authoring a Workflow

### `kontra workflow register <dir> [--init] [--workflow <Class>] [--json]`
Declare it **without serving or running it**: records the path, manifest, version and a content
digest, and creates the Actor's Nexus endpoint.

### `kontra workflow serve <folder|file.py> [--mode local|dev] [--repo <dir>] [--watch]`
Run **your** Temporal workflows here; they dispatch deployed Actors.

> **The queue is derived from the folder's content, never typed** — there is no `--queue`.

`--watch` re-registers the contract on every save, so the browser form tracks your editor; a file
that no longer imports becomes a visible **state**, not silence.

### `kontra workflow start <folder> [--input <json|@file>] [--id <id>] [--wait]`
Start a run. `--input` is JSON for the workflow's **one** argument.

### `kontra workflow pause | resume <file.py>`
Stop / restart the **served worker**, in its pane.

> The run makes no progress and resumes from history — but **dispatched activities keep running and
> their timeouts keep ticking**. A long pause *fails* a run; it does not hold one.

### `kontra workflow cancel <run-id>`
**Graceful**: scope exits run, so a Fleet is destroyed.

### `kontra workflow terminate <run-id> [--force]`
Cancel, then terminate if it does not settle.

> **Terminating alone skips scope exits**, so a Fleet it held would keep billing. Prefer `cancel`.

### `kontra workflow history <run-id> [-o FILE]`
Save a run's history as JSON — shareable, replayable.

### `kontra workflow replay <workflow.py> (--run-id ID | --history FILE) [--json]`
Replay a recorded history against the code on disk.

**No clock**: no heartbeat, no StartToClose, nothing times out while you sit on a breakpoint —
unlike attaching to a live activity, which gets about two minutes. A run that failed on a fleet days
ago, stepped through on a laptop.

> **Activity code is not run.** A Method's results come from the history as values, so this covers
> the *caller's* decisions — splitting, chaining, branching — not a Method.

Exit codes: `0` clean · `1` non-deterministic against this history · `2` could not run.

---

## Workspaces

### `kontra workspace seed|watch|list|use|create [--dir <path>]`
Named workspaces under `KONTRA_WORKSPACES`. `seed` puts hello on an empty parent; `watch` follows
the current child and publishes actor artifacts. See [[Glossary]] on why a workspace is also an
isolation boundary.

---

## Fleets and Machines

### `kontra fleet up --count N --actor <dir> [--tag <t>] [--fleet <name>]`
Converge Machines through Pulumi, as a Temporal workflow on the Controller. **A Fleet is named
after what it places**: `<actor>-<version>`.

### `kontra fleet deploy --actor <dir> [--image <ref>]`
Place the artifact.

### `kontra fleet preview | status | down [--fleet <name>]`

### `kontra warden join --controller https://<host>:8443 --token <kw1...>`
**On a Machine** (ADR 0037): exchange a one-time token for an mTLS identity that persists, then
install and start `kontra-warden.service`.

> The token carries the Fleet CA's **fingerprint**, so the secret is never sent to a Controller that
> cannot prove it holds that CA.

### `kontra warden serve [--driver podman|process] [--state <dir>]`
The reconcile loop. **Outbound only** — a Machine opens no listening socket.

### `kontra warden status`
Who this Machine is, and what is running on it.

### `kontra warden ca serve|token|list [--dir <dir>]`
**On the Controller**: the Fleet CA and enrolment.

---

## Data

### `kontra dataset list`
Every dataset: standalone (loaded) **and** output (actor-produced). One noun for both directions.

### `kontra dataset query <name> --sql "SELECT ..."`
Runs on the orchestrator's already-open connection (~50 ms). `--local` runs DuckDB on your
workstation instead. The web **Query** workbench runs the same engine.

### `kontra dataset create|anew <file.csv|.jsonl|.parquet> <name>`
Load a standalone dataset. `anew` appends new rows.

### `kontra dataset delete <name>`
(`db` is an alias for `dataset`.)

### `kontra dataset tag <name> --add TAG [--remove TAG] [--version V] [--dt P] [--run ID]`
**Keep this output past its TTL.** Tags are a set; the record is keyed by `runId`.

### `kontra dataset rename <name> (--to NEWNAME | --reset)`
Override the derived run-grain name.

---

## Querying and inspection

### `kontra explore <run-id> [--sql "..."] [--ttl 900] [--ui] [--print-init]`
A run's output as typed views per node, in **local** DuckDB over short-lived presigned URLs.

### `kontra explore <actor[@version]> [--dt <date>] --sql "SQL"`
One dispatch's typed output — the actor's own columns, nested output kept as `STRUCT`/`LIST`.
Addressed **by actor and time, never by a run UUID**.

### `kontra explore --catalog`
Cross-run, read-only, over the shared DuckLake catalog.

### `kontra runs list [--tenant T] [--status S]`
Runs from Temporal Visibility: status, tenant, stages, S3 path.

### `kontra runs --run-id <id>`
Run detail: per-node state, live heartbeat progress, blob counts.

### `kontra runs [--run-id <id>] --state`
Live run/node state, real time — all runs, or one.

### `kontra runs --run-id <id> --duckdb`
Materialize output and open it in the DuckDB UI.

---

## Schedules

### `kontra schedule list | describe | pause | resume | trigger | delete --id <n>`
Read and steer this installation's Temporal Schedules.

> **There is no `create`.** The Dataset retention sweep (`kontra-dataset-retention`) is registered
> at boot by `orchestrator-infra` and appears here. It **previews and deletes nothing** until
> `KONTRA_RETENTION_COLLECT=1` is set on the worker holding the lake. `trigger` runs a sweep now, in
> that same mode.

---

## Packaging and release

### `kontra bundle orchestrator [--out <dir>] [--platform goos/goarch|list|all]`
The appliance bundle (ADR 0031 §2): a pinned Node runtime, the compiled orchestrator and its native
addons, as one content-addressed `tar.gz` with a manifest naming every component, version and
digest. **Nothing is fetched that is not checksummed first.**

### `kontra bundle spa [--out <dir>]`
The built SPA as its own content-addressed tarball — separate because it is platform-neutral and
changes when a page does.

### `kontra bundle verify <bundle.tar.gz>`
Re-derive every digest the manifest claims.

### `kontra release [--version <v>] [--platform ...] [--out <dir>]`
One file per platform: the binary, the orchestrator bundle for that platform and the browser bundle,
packed where an installed binary already looks, plus a `SHA256SUMS`.

> CI still runs this natively on four runners, because **an artifact nobody executed is a claim**.

---

## Agents

### `kontra mcp [--api <url>]`
MCP server over stdio: list/describe actors, read a Scratch, poll run status, query datasets, drive
a fleet.

> **There is no dispatch tool.** Dispatching is the caller's, so an agent writes a workflow and runs
> `workflow serve|start` like anyone else.

---

**See also:** [[Getting-Started]] · [[Dev-Cycle]] · [[Writing-Workflows]] · [[Configuration]]
