# The Dev Cycle

The end-to-end path: **write an actor → test it → run its two processes (the actor + the Go handler) → call it from a workflow you serve.** Every command runs against the live stack.

> [!NOTE]
> **Actor paths like `python/probe` are relative to a
> [kontra-actors](https://github.com/medmahmoudi26/kontra-actors) checkout**, not to this
> repository. kontra ships no actors — clone or fork that repo and run the commands from
> inside it.

```
author writes actor.py ─▶ pytest ─▶ kontra serve --actor python/myactor
                                    (starts BOTH processes)
author writes workflow.py ─▶ kontra workflow serve <folder> ─▶ kontra workflow start <folder>
                                    │
        YOUR workflow ──Nexus kontra.actor:run──▶ handler workflow
                                              ──RunBatch activity on myactor-0.1.0-sessions──▶ your actor
```

**Dispatching is the caller's.** There is no dispatch verb and no server-side interpreter between
your workflow and the handler — ADR 0023 §12 made a **Run** one execution of a caller's workflow and
took the interpreter with it. The orchestrator's role is to *observe* runs and serve the catalog, not
to run them.

Each actor runs as **two processes**: the Go **handler** (the workflow half — it serves the Nexus op, owns retry, and holds the blob activities on `{name}-{version}`) and the **actor itself**, a Temporal activity worker serving `RunBatch`/`Close` on `{name}-{version}-sessions`. They are decoupled and never import each other; they meet only at the queue name and the JSON wire. Everything else a worker needs — Temporal, Redis, S3, the catalog — it reaches OUT to on the Controller.

## 0. Prerequisites

- **Docker** (Temporal + SeaweedFS + Redis + orchestrator run as containers).
- **Redis** — the `redis` service in the control plane, holding all three durable state tiers. It is **required**, and it is shared: `KONTRA_REDIS_HOST` (`host:port`) points every actor at the one store. Compose does not publish `6379` on localhost by default (in-cluster workers reach it as `redis:6379`); to reach it from a process on the host, publish it on the controller's **private** VPC IP with an override — never `0.0.0.0`, since it is auth-less.
- **Go 1.22+** to run the handler (`go run .` from `handler/`) and to build the `kontra` CLI once: `cd cli && go build -o kontra .` (or `./install.sh`, which puts it on `PATH`).
- **Python 3.10+** with the venv: `./install.sh` (or `just install`) — editable `actorkit` + dev/seaweed extras.
- **Node 22** for the orchestrator worker (`corepack enable` for pnpm). Host Node 23 breaks the TS worker — run it in Docker if your host is newer. Node is not needed to *use* kontra: the orchestrator serves from its container.

In a repo checkout, `import actorkit` resolves from the editable install, or from `PYTHONPATH=sdk/python:runtime/python:sdk/python/_gen` — one entry per package: `actorkit` (the author surface), `internals` (the runtime) and the generated `kontra.v1` stubs. There is no repo-root shim any more; the package directory is named after the package, so nothing has to point at it.

## 1. Write the actor

Two files — see [[Writing-Actors-Python]] for the full surface:

```
python/myactor/
  actor.py     # @actor.defn class: typed methods (takes=/emits=) + @actor.load/@actor.method/@actor.close
  actor.json   # name + version: {"schemaVersion": "kontra.actor.v1", "name": "myactor", "version": "0.1.0"}
```

The `name` must equal the directory name — that's the identity everywhere (the handler's Temporal queue `{name}-{version}`, the actor's own `{name}-{version}-sessions`, image `kontra/{name}:{version}`, Nexus endpoint `kontra-{name}-{version}`).

## 2. Test it

```sh
pytest -m 'not e2e'        # fast unit tests, no cluster
```

Repo-wide targets: `just test` / `just ts-test` (orchestrator) / `just conformance` (the codec corpus). Go: `cd sdk/go && go test ./...` and `cd runtime/go && go test ./...` (two modules now — the arrow test lives in the first).

## 3. Run the two processes locally

An actor needs the **actor itself** and its **handler**. One command starts both:

```sh
kontra up                                    # the control plane, one process, no containers

kontra serve --actor python/myactor
# myactor@0.1.0 — actor on myactor-0.1.0-sessions, handler on myactor-0.1.0 (temporal localhost:7233)
#   a Go actor:  kontra serve --actor go/myactor --engine go
#   flags: --actor --python --engine --redis
```

Or one terminal each, which is what `kontra serve` is doing:

```sh
# the handler for this actor — serves Nexus kontra.actor:run, owns retry
cd handler && KONTRA_ACTOR_NAME=myactor KONTRA_ACTOR_VERSION=0.1.0 \
  KONTRA_ADDRESS=localhost:7233 KONTRA_S3_ENDPOINT=http://localhost:8333 GOWORK=off go run .
# serving myactor@0.1.0: workflow on "myactor-0.1.0", actor activities on "myactor-0.1.0-sessions" …

# the actor — a Temporal activity worker; no sidecar, no app port. It takes its (name, version)
# from the actor.json beside actor.py; KONTRA_ACTOR_NAME here only labels its blobs and metrics.
KONTRA_ACTOR_NAME=myactor KONTRA_REDIS_HOST=<controller>:6379 \
  python3 python/myactor/actor.py
# [host] myactor@0.1.0 -> myactor-0.1.0-sessions @ localhost:7233
```

The handler serves its `{name}-{version}` queue and auto-registers the Nexus endpoint; the actor polls `{name}-{version}-sessions` and holds its durable state in Redis. A booting actor also **self-registers** its descriptor (identity + derived schemas) with the catalog (`KONTRA_ORCHESTRATOR_URL`), so a running actor is auto-discovered by the Workflows UI — no upload. The real cycle dispatches through the orchestrator (step 4).

### Registration & env gotchas

Manual registration (the catalog, which IS the schema gate), from the repo root:

```sh
PYTHONPATH=sdk/python:runtime/python:sdk/python/_gen .venv/bin/python -c \
  "from internals.loader import load_actor; from internals.catalog import operations_of, register_actor_catalog; \
   l = load_actor('python/beacon'); \
   print(register_actor_catalog('http://localhost:8088', l.manifest, operations_of(l.registry)))"
```

- A **changed schema needs a version bump** — `POST /api/actors` answers 409 when a `(name, version)` the catalog already holds comes back with a different input/output/params schema, naming the Method that changed. There is no force flag: a bypass is the silent overwrite again under a responsible-sounding name.
- The gate does **not** compare one version against the next, so a bump whose schema breaks an existing caller still registers (ADR 0027 records the gap). This snippet used to go through `internals/registration.py`, which also pushed to an external schema registry that was never running; that module and its registry client are deleted.
- The two halves derive their queues from **different sources** and must still agree: the handler builds `{name}-{version}` from `KONTRA_ACTOR_NAME` / `KONTRA_ACTOR_VERSION`, while the actor reads its own `actor.json` (both SDKs do; a Go binary looks in the working directory, then beside itself). A mismatch does not error — the actor registers, polls a queue nobody schedules onto, and looks like a healthy idle worker while every run hangs to `StartToClose`. `kontra serve` passes the manifest's values into the handler's env, which is the reason to prefer it.
- The actor needs **`KONTRA_REDIS_HOST`** reachable, or its first commit fails. There is no bundled fallback.
- The one `.venv` carries `temporalio`, `redis`, `boto3` and `pydantic` — plus the actor's own libs (e.g. crawl4ai + playwright chromium) for local runs.

## 4. Call it from a workflow

**There is no dispatch verb.** `kontra actor <ref> dispatch`, `kontra graph <id> dispatch`,
`kontra console` and `pnpm dispatch` are all gone: they built a K-node graph and POSTed it to
`POST /api/runs`, the route the server-side interpreter answered, and ADR 0023 §12 deleted that
interpreter. Work starts by **serving a workflow you wrote and starting it**.

```sh
kontra workflow serve <folder|file.py>   # run YOUR Temporal workflows here
kontra workflow start <folder> --input '{"dataset": "scope"}' --wait
kontra workers list                      # catalog × live Temporal pollers per {name}-{version} queue
```

The task queue is **derived from the folder's content, never typed** — there is no `--queue`, so a
served folder and a started folder cannot silently miss each other. `--watch` re-registers the
contract on every save, and a file that no longer imports becomes a visible **state** on the
Workflows page rather than a silence.

Inside the workflow, the caller owns the loop and the paging:

```python
targets = catalog.dataset("scope")

async with catalog.actor("dnsfacts", "0.1.0") as dns, \
           catalog.actor("probe", "0.1.0") as probe, \
           catalog.dataset("live").writer() as out:
    # `order_by` is required: a materialized dataset stamps no row id, so paging
    # without one may overlap or skip units and nothing would raise.
    async for batch in targets.batches(200, order_by="host"):
        resolved, drops = await dns.addrs(batch)     # a Method call → (results, dropped)
        async for chunk in resolved.batches(200):    # re-page: fan-out is the author's business
            checked, drops = await probe.head(chunk, out)   # `out` is the OUTPUT Dataset
```

Three things to notice. `page size` guards of **200** (soft) and **1000** (hard) are enforced on
`batches()`. A Method call hands back `dropped` beside `results` (ADR 0028 §4) — you cannot bind the
survivors without naming the drops — so a run that dropped everything cannot read like one that found
nothing. And the call's **third parameter is the caller's output Dataset**: the Actor pushes straight
into `live`, so `live` is queryable *while this loop is still running* and each row carries the
Machine that produced it.

Load the input list with `kontra dataset create scope.parquet scope` ([[Query-Surface]]).
`kontra deploy --actor <dir>` builds and pushes a self-contained worker image (see [[Deployment]]).
`kontra up` IS the local control plane; `kontra infra up|down|status` manages the compose
**controller** — the cloud deployment that keeps the Pulumi engine (ADR 0031 §4, ADR 0034 §1). Env:
`KONTRA_ORCHESTRATOR_URL` / `KONTRA_ADDRESS` / `KONTRA_NAMESPACE`.

Or start it from the UI at http://localhost:8088 → **Workflows**, which generates the input form from
the workflow's own declared input type and shares the CLI's coercion core rather than copying it.

## 5. Inspect

| What | Where |
|---|---|
| **What did it find?** | `kontra dataset query <actor> --dt <date> --sql "…"` (`--export out.parquet` to keep it), or the web **Query** workbench. Runs on the orchestrator by default — no env to export, nothing to install. `--local` runs it here instead and needs `duckdb` on `PATH`. See [[Query-Surface]] |
| **One dispatch, no catalog credential** | `kontra explore <actor[@version]> [--dt 2026-08-03T17]` — typed views over presigned URLs, DuckDB **on your machine**; `--sql "…"` one-shot, `--list` to see what output exists. Needs `KONTRA_EXPLORE_TOKEN` (or `KONTRA_STATE_TOKEN`) |
| What datasets exist at all? | `kontra dataset list` — standalone (loaded) + output (produced), from catalog metadata |
| Is the output queryable yet? | `curl -s localhost:8088/api/runs/<run-id>/lifecycle \| jq` — `executing` / `finalizing` / `completed` / `output_failed` |
| Stack health / live workers | `kontra doctor` · `kontra infra status` · `kontra workers list` |
| Workflows, histories, per-node status | Temporal Web UI — http://localhost:8233 |
| Catalog, workflows, run output (`/api/runs/:id/output`) | Orchestrator — http://localhost:8088 |
| Offloaded payloads | SeaweedFS filer — http://localhost:8888/buckets/kontra/ |
| Is it moving? | Grafana — http://localhost:3030 — bounded summaries + the catalog-only `kontra_runs` view |

A dataset reported `failed` by `kontra explore` means that output is **missing**, not empty — a
query over the rest under-reports and will not say so. `state=complete` with `rows=0` is the real
empty result.
