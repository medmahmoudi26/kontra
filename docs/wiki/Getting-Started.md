# Getting Started

## Install (once)

The supported path is a Docker Compose cluster (ADR 0047). Clone, build the images, and run
`docker compose --env-file .env.quickstart up -d --wait` — see the repository README. Docker is the
only host prerequisite. `kontra up` (the install) is not a supported install.

A **development checkout** still uses `install.sh` below: a venv, the editable SDK, the buf
toolchain and proto codegen. It needs Go, Docker and a clone.

```bash
./install.sh
```

Creates `.venv` — the SDK, editable (`pip install -e ./sdk/python[dev,seaweed]`), plus the actor-host runtime (`temporalio`, `redis`, `boto3`, `pydantic`) — and builds the `kontra` CLI onto your `PATH`. **One** venv: there used to be a second, `.venv-actor`, because the old actor runtime pinned a protobuf the Temporal SDK could not share an interpreter with. That runtime is gone and the split went with it.

**On a bare machine it installs its own prerequisites.** A stock Ubuntu 24.04 has `git`, `python3` and `curl` and none of the rest, so preflight finds everything missing at once and (on apt systems, with sudo) installs `python3-venv`, Docker with the v2 compose plugin, and Go — Go from upstream and checksummed, because the distro package is 1.22 and `go.work` says 1.26.4. `KONTRA_BOOTSTRAP=no ./install.sh` reports the bill and installs nothing.

Node is **not** needed to run kontra, and the reason changed with v2: the orchestrator used to serve
from a container, and now it is a supervised child of the `kontra` process, hydrated from a bundle
that already carries its own Node (ADR 0031). The install build fetches and checksums that Node
itself, which is why building a release needs Go and nothing else. Node 22 + `corepack enable` for
pnpm is needed to work on `control/orchestrator/` or `shared/core/`. The console is the separate
[kontra-console](https://github.com/medmahmoudi26/kontra-console) repository since ADR 0038; a
the image build needs the console's built `dist`, which this repository deliberately does not
build for you —
clone it as a sibling and `pnpm run build`, or point at a build you have with
`KONTRA_CONSOLE_DIST`. Python 3.10+.

## The 5-minute run

An actor is a **Temporal activity worker** with a Go handler beside it. **Dispatching is the
caller's**: you write a workflow, serve it, and start it — there is no dispatch verb on the CLI and
no server-side interpreter behind one (ADR 0023 §12). Swap `<actor>` below for an example —
`beacon` or `crawl4ai` (Python), `nuclei` (Go) (see [[Examples]]).

```bash
# 1. the control plane — ONE PROCESS (Temporal, the object store, the state store, the payload
#    codec, the OCI registry, and the orchestrator as a supervised child). No containers.
kontra up

> [!NOTE]
> **Actor paths like `python/probe` are relative to a
> [kontra-actors](https://github.com/medmahmoudi26/kontra-actors) checkout**, not to this
> repository. kontra ships no actors — clone or fork that repo and run the commands from
> inside it.

# 2. the actor — BOTH halves (the actor process + its Go handler), one command
kontra serve --actor python/<actor>
#   a Go actor:  kontra serve --actor go/<actor> --engine go
#   --redis host:port points at the state store (default $KONTRA_REDIS_HOST, else 127.0.0.1:6379)

# 3. YOUR workflow is what dispatches. Serve it, then start it.
kontra workflow serve workflows/ping
kontra workflow start workflows/ping --wait --input '{"note": "hello"}'
```

`ping` dispatches nothing — it is the wiring check, and it separates "the control plane is broken"
from "my run is broken". For one that actually drives a Method over a **Batch**, serve
`workflows/dnssweep.py`; for one that provisions its own fleet and tears it down
inside the same scope, `workflows/nscheck`.

The task queue is **derived from the folder's content**, never typed — there is no `--queue` to get
wrong, and a served folder and a started folder that disagree cannot silently miss each other.

By hand, that step 2 is two terminals — the Go handler
(`KONTRA_ACTOR_NAME=<actor> KONTRA_ACTOR_VERSION=0.1.0 go run .` from `handler/`) and the actor
itself (`python3 python/<actor>/actor.py`). `kontra serve` just supervises the pair, and
kills one if the other dies: a half-dead worker is worse than a dead one, because it keeps its
Temporal lease and units time out one by one.

A **Run** appears in the list the moment it *starts* — not when it first dispatches an Actor — so a
workflow that is still paging, or one that dispatches nothing at all, is visible rather than absent.
See [[Dev-Cycle]] for the loop and [[Execution-Model]] for what happens between the call and the
result.

## The full stack (orchestrator + datasets)

Kontra runs as **a control plane and the actors** (see [[Deployment]]). Locally the control plane
is `kontra up` — one process holding Temporal, the object store, the state store, the payload
codec, the OCI registry and the orchestrator (ADR 0031). The actors are what stay containers:
each is an actor process plus its Go handler.

`docker-compose.yml` is **not** the local path any more. It survives for the **cloud controller**,
which holds the Pulumi engine and the cloud credential the install deliberately does not ship.

```bash
kontra up        # control plane
# then, per actor, the two processes above (kontra serve --actor <dir>)
# then open http://localhost:8088
```

Both halves run out of the one `.venv` — see [[Dev-Cycle]].

| What | Where |
|---|---|
| Orchestrator — **Workflows**, **Actors**, **Monitor**, **Datasets** | http://localhost:8088 |
| Temporal Web UI | http://localhost:8233 |
| SeaweedFS filer (browse stored objects) | http://localhost:8888/buckets/kontra/ |

Every surface has its own URL (`/workflows`, `/runs/<id>`, `/datasets/<name>`, `/monitor/<id>`), so a
reload keeps your place and a **Run** can be pasted to somebody.

`kontra doctor` prints this table live — each service, its port, and up/down state, plus
the actors currently registered.

Load a list, then drive it from a workflow you serve. `dnssweep` pages a **Dataset**, resolves each
**Batch** with one Actor and probes the results with another — two languages, one caller-owned loop:

```bash
printf '%s\n' '{"host":"example.com"}' '{"host":"iana.org"}' > units.jsonl
kontra dataset create units.jsonl targets     # a standalone Dataset — the run's INPUT

kontra serve --actor go/dnsfacts     # the two Actors it calls
kontra serve --actor python/probe

kontra workflow serve workflows/dnssweep.py
kontra workflow start workflows/dnssweep.py --wait \
    --input '{"dataset": "targets", "into": "live"}'

kontra dataset query live --sql "SELECT count(*) FROM live"   # …while the sweep is still running
```

Or open http://localhost:8088 → **Workflows**, which lists what is served and starts a run through a
**form generated from the workflow's own declared input type** — the same coercion the CLI's
`--input` uses, not a second copy of it. The code panes beside it are **read-only viewers of what is
deployed, at its digest** (ADR 0030): the browser shows you what is running, your editor is where you
change it.

The run's output appears on the **Datasets** page — rows land there *while the run is still open*,
and every row says which **Run** made it.

## Local vs Docker

Same actors, same code — only the wiring differs.

The control plane is the same either way — `kontra up`, one process — and what differs is where
the **actor** runs.

| | **Local** — fastest to iterate | **Docker** — the actor as an image |
|---|---|---|
| Control plane | `kontra up` | `kontra up --bind 172.17.0.1` (a container cannot reach loopback) |
| Temporal, object store, state store | in that process | the same process, on a bridge-visible address |
| Actor | `kontra serve --actor python/<actor>` (the actor + its handler) | `kontra deploy --actor <dir>` then `kontra serve --actor <dir> --mode docker --replicas N` |
| Reach for it when | hacking on one actor | proving what will run on a fleet |

`scripts/parity-gate.sh` runs the right-hand column end to end against a throwaway install and
asserts the unit and drop counts it produces.

## Next

- Write your own actor: [[Writing-Actors-Python]] (or [[Writing-Actors-Go]])
- The full day-to-day loop (test → actor + handler → dispatch): [[Dev-Cycle]]
- What actually happens when you dispatch: [[Execution-Model]]
