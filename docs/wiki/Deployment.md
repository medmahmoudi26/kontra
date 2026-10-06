# Deployment — the two topologies, and the two clusters inside each

**Locally, the control plane is one process: `kontra up`.** Temporal, the object store, the state
store, the payload codec, the OCI registry and the orchestrator all live in it, with one data
directory and no containers (ADR 0031). `docker-compose.yml` is **not** the local path; it is the
**cloud controller**, which is a different job and is not going away — see §0.

Either way, kontra runs as **a control plane and the actors**, so actors can live on remote
machines that connect back to one controller. Each actor deploys as a **pair**: a Go **handler**
(the workflow half — serves the Nexus op, owns retry) and the **actor process** itself, which is a
Temporal activity worker.

```
  ┌────────────────── CONTROL PLANE — `kontra up`, ONE PROCESS (ADR 0031) ──────────────┐
  │  runs on the machine the operator is sitting at; binds 127.0.0.1 unless told otherwise│
  │   Temporal      embedded server                        gRPC  --temporal-port 7233   │
  │   object store  an S3 API over the data directory      S3    --s3-port      8333    │
  │   state store   the same RESP wire, embedded           RESP  --kv-port      6379    │
  │   payload codec the claim-check encoder as a handler   HTTP  --codec-port   18234   │
  │   OCI registry  actor image layers, in the shared CAS  HTTP  --registry-port 5000   │
  │   orchestrator  API + SPA + materializer + infra roles, a SUPERVISED CHILD  :8088   │
  │                 ├─ POST /api/actors is the schema compat gate (ADR 0027)            │
  │                 └─ embeds DuckDB for the lake; the catalog is a file, not a server  │
  └─────────────────────────────────────────────────────────────────────────────────────┘
     ▲ Temporal   ▲ S3   ▲ state   ▲ the catalog API (self-register)
     │            │      │         │
  ┌──┴────────────┴──────┴─────────┴──────────────── ACTORS (one machine per actor) ────┐
  │  same host as a CONTAINER  OR  a REMOTE Machine that connects back to the controller │
  │                                                                                     │
  │   handler (Go)  ──Nexus kontra.actor:run──▶ the backing workflow, on {name}-{ver}   │
  │      │            owns retry / the blob activities (claim-check)                    │
  │      │ schedules RunBatch / Close BY NAME onto {name}-{ver}-sessions                │
  │      ▼                                                                              │
  │   the actor (Python OR Go) — a Temporal activity worker polling that queue          │
  │      └─ no sidecar, no placement, no app port; state OUT to the control plane       │
  └─────────────────────────────────────────────────────────────────────────────────────┘
```

## 0. Which topology, and why there are two

| | **the appliance** — `kontra up` | **the controller** — `docker-compose.yml` |
|---|---|---|
| What it is for | local development, and any install that runs no cloud fleet | the deployment that runs **cloud runs** |
| What it is | one process, one data directory | `orchestrator-infra` (Pulumi, the fleet key, the cloud credential) + `orchestrator-probe`, beside a `kontra up` |
| Start it with | `kontra up` | `kontra infra up` (or `make up-d`) |
| Cloud fleet | **no.** `kontra.fleet.up()` fails legibly: this control plane has no provisioner | yes — ADR 0034 §1 keeps provisioning here |
| Exposure | binds `127.0.0.1` by default; nothing published (ADR 0031 §3) | published ports, each naming an address; GitHub #19 §2 is still open there |
| Memory bounds | `KONTRA_DUCKDB_MEMORY_LIMIT` + the activity-slot count | those **plus** per-service `mem_limit` cgroups (ADR 0031 §1) |

Nine services left `docker-compose.yml` one slice at a time and every one left a block behind
saying where it went; that file's header is the authority on what survives and why.

**Running both on one box** — the parity window, and a real case: the appliance runs all three
orchestrator roles because it ships no Pulumi engine, so tell it to leave the third one to compose,
and give it an address the containers can reach.

```sh
KONTRA_ORCHESTRATOR_ROLES=api,materializer kontra up --bind 172.17.0.1
```

The clusters are decoupled by design: the control plane knows nothing about which actors exist until an actor boots and **self-registers**; an actor knows only the controller's address. Everything durable an actor touches lives on the control plane and is reached **outbound**: Temporal, S3, the catalog, and Redis. That last one is the change ADR 0018 made most visible — every worker used to bundle its own Redis, which silently made the fleet-wide `global_state` tier per-container; there is now one store and no local fallback (see [[Data-Plane]] §3). DuckDB is not a service — it's embedded, in the orchestrator for reads and in the **materializer** for the write path; the materializer is a separate process on its own task queue and belongs on a worker host, not the controller (§3). handler and actorkit never import each other; they meet only at the queue name + the JSON wire.

### 0a. Worker containers, the bind address, and the one firewall rule nobody expects

An actor Worker is a **container** (ADR 0031 §2) and the appliance binds **loopback** (§3), and a
container's loopback is its own. So the local actor path needs an address the container can reach:

```sh
kontra up --bind 172.17.0.1        # docker0's gateway. Host-local; there is no route to it from off-box.
```

`kontra deploy` and `kontra serve --mode docker` then resolve everything from the appliance's own
`<data-dir>/endpoints.json` — Temporal, the object store, the state store, the API — so there is
nothing to keep in step by hand. `kontra serve --mode docker` **refuses** a loopback-bound
appliance by name rather than starting workers that poll nothing.

**The registry is the exception, and it is deliberate.** `kontra up` serves it on the bind address
*and* on `127.0.0.1`, and publishes the loopback one — because the registry's client is this host's
**Docker daemon**, which will not push plain HTTP to anything outside `127.0.0.0/8`. Bound
elsewhere, `kontra deploy` dies on `http: server gave HTTP response to HTTPS client`, which reads
as a TLS problem and is really the daemon's insecure-registry list.

**And a default-deny host firewall blocks the whole thing, silently.** ufw's `deny (incoming)` is
the common case, and a container's packets to the *bridge gateway* go through the host's **INPUT**
chain — not FORWARD/DOCKER-USER, which is the docker-bypasses-ufw case everybody knows. Measured:
`kontra deploy` fine, `kontra serve --mode docker` fine, containers `Up`, both processes alive
inside them, **zero bytes of log**, no actor in the catalog, no poller on the queue, and
`docker ps` reporting healthy replicas. Nothing anywhere says why.

Two fixes, and they are not equivalent:

```sh
ufw allow in on docker0 to 172.17.0.1 port 7233 proto tcp    # and 8333, 6379, 8088
kontra serve --actor <dir> --mode docker --network host      # dev answer: costs the netns
```

`scripts/parity-gate.sh` probes this from a real container before it starts any worker, and says
which of the two topologies it proved.

## 1. Control plane

```sh
kontra up                     # start it; Ctrl-C stops the child first, then the services
kontra up --temporal-ui       # also Temporal's own Web UI, hydrated from the CAS (opt-in)
kontra up --bind 172.17.0.1   # an address worker CONTAINERS can reach (their loopback is their own)
```

It prints every address it bound, and publishes them into its data directory so that
`kontra deploy` and `kontra serve --mode docker` resolve the same installation by construction
rather than by three matching defaults (`<data-dir>/endpoints.json`).

Workflows UI at http://localhost:8088. `kontra doctor` probes it. **`kontra infra up | down |
status`** is the *other* topology — the compose controller of §0 — with a health table.

> **The sections below still describe compose services that left the file.** `temporal`,
> `seaweed`, `redis`, `registry`, the codec server, `ducklake-postgres` and `orchestrator-api` are
> all inside `kontra up` now, and `temporal:7233` / `seaweed:8333` / `redis:6379` resolve to
> nothing. Read them for the SHAPE — which process talks to which, and why the materializer is
> routed to by queue — and take the addresses from `kontra up`'s own output.

## 2. Deploying an actor — the `{actor + handler}` pair

An actor is not a control-plane service: deploying it means running its two processes — the Go **handler** (the workflow half, owns retry) and the **actor itself** (a Temporal activity worker). Same host or remote, the wiring is env only.

Same host (on the control plane's `kontra` network):

```sh
make up-d        # control plane first (creates the network)

kontra serve --actor python/<actor>  # both halves, supervised
```

Or, by hand, one terminal each:

```sh
# the handler for this actor — the workflow + the blob activities on {name}-{version}
cd handler && KONTRA_ACTOR_NAME=<actor> KONTRA_ACTOR_VERSION=0.1.0 \
  KONTRA_ADDRESS=localhost:7233 KONTRA_S3_ENDPOINT=http://localhost:8333 GOWORK=off go run .

# the actor — RunBatch/Close on {name}-{version}-sessions
KONTRA_ACTOR_NAME=<actor> KONTRA_ACTOR_VERSION=0.1.0 KONTRA_ADDRESS=localhost:7233 \
  KONTRA_REDIS_HOST=<controller>:6379 KONTRA_S3_ENDPOINT=http://localhost:8333 \
  python3 python/<actor>/actor.py
```

A Go actor is the peer: run its compiled binary (or `kontra serve --actor go/<actor> --engine go`), same env wiring on the handler.

The two halves take their identity from different places and must agree: the handler from `KONTRA_ACTOR_NAME`/`KONTRA_ACTOR_VERSION`, the actor from the `actor.json` beside its code (`KONTRA_ACTOR_NAME` on the actor labels its blobs and metrics, and is baked into the worker image by `kontra deploy`).

### `kontra deploy` — build & push a self-contained worker image

```sh
kontra deploy --actor python/<actor> [--registry host:port] [--controller <host>]
```

Deploy builds a **fully self-contained worker image** and pushes it to a registry, then
prints the address + how to run it anywhere:

```
actor deployed: beacon@0.2.0
  image:  localhost:5000/beacon:0.2.0
  pull:   docker pull localhost:5000/beacon:0.2.0
  run:    docker run -d --name kontra-beacon \
            -e KONTRA_ADDRESS=<controller>:7233 \
            -e KONTRA_ORCHESTRATOR_URL=http://<controller>:8088 \
            -e KONTRA_S3_ENDPOINT=http://<controller>:8333 \
            -e KONTRA_REDIS_HOST=<controller>:6379 \
            localhost:5000/beacon:0.2.0
  then:   kontra workers list   # the worker appears, wherever it runs
```

`KONTRA_REDIS_HOST` is **required**, not an upgrade: a worker started without it points at a
localhost Redis that is not there and fails on its first commit.

Images built over the Docker Engine API, layered so a re-deploy is cheap:

1. `kontra-host:1` (`infra/Dockerfile.pyworker`) — the Python actor runtime: actorkit on
   `PYTHONPATH` plus `temporalio` / `redis` / `boto3` / `pydantic`. Built once
   (rebuilt when `sdk/python` or `runtime/python` changes: `docker rmi kontra-host:1`).
2. `kontra-worker-base:1` — the actor-**agnostic** worker parts: the Go **handler**
   (compiled here, ONCE) and the entrypoint. Built once (rebuilt when `handler/` changes:
   `docker rmi kontra-worker-base:1`). This is what keeps deploys fast — the handler compile
   (memory-heavy) does not run per deploy.
3. the **host** image `kontra/<name>:<version>` — `FROM kontra-host:1` + the actor's code
   (a `Dockerfile`-less actor gets a synthesized `COPY`; one with a `Dockerfile` adds its
   deps). A Go actor is compiled instead, into a slim runtime image. `--host-only` stops here.
4. the **worker** image `kontra/<name>-worker:<version>`, pushed as `<registry>/<name>:<version>`.
   `FROM` the host image (so it carries the actor's deps) +
   `COPY --from=kontra-worker-base:1` (the pre-built handler + entrypoint) —
   no handler recompile. A single `docker run` is a complete worker, reaching OUT only to the
   controller (Temporal / S3 / Redis / orchestrator via `KONTRA_*`).

That worker is **two** processes now (`infra/worker-entrypoint.sh`), and the entrypoint exits
non-zero the moment either dies so the container restarts — a half-dead worker keeps its Temporal
lease and times its units out one by one. It used to be five, three of them a broker,
the actor and the handler. Removing the middle three took **270 MB off every worker image** and
freed five ports; ADR 0018 has the measurements.

`kontra deploy` **refuses to overwrite an already-deployed version** (a registry tag already
exists) — bump the version (schema changes need one; the catalog's registration gate refuses a
re-registration whose I/O schema moved, with a 409 naming the Method)
or pass `--override` to replace it. `--registry` defaults to a local `registry:2` at
`localhost:5000`; point it at a droplet-reachable `host:port` for remote deploys.
`--controller` sets the host printed in the run command (else `KONTRA_ORCHESTRATOR_URL`).
`--engine go` compiles a Go actor instead of layering `actor.py`.

This is the distributed model: `kontra deploy` once, then `docker pull` + `docker run` the
image on any droplet (pointed at the controller) and it self-registers + shows up in
`kontra workers list` — no per-machine runtime install, venv, or repo checkout.

## 3. The materializer — routed by task queue, placed off the controller

Typed-output materialization runs in its **own process** (`control/orchestrator/src/materializer.ts` →
`dist/src/materializer.js`, the same image as the API and the interpreter worker), and
in production on its **own host**. Embedded DuckDB is the largest memory consumer in the system,
and the controller is a 4 GB box already running Temporal, SeaweedFS and Postgres —
production materialization does not belong on it.

**Placement is enforced by routing, not by convention.** A caller's workflow dispatches
`publishBatch` to the Temporal task queue `kontra-datasets` and does not poll it; the materializer
worker registers **activities only** — no `workflowsPath` — so it cannot run a workflow even by
accident.

> Until 2026-09-26 this read `materializeNode` on a `kontra-materializer` queue. Both were removed
> as uncalled: ADR 0023 §1 took materialization off the graph interpreter, which had been their only
> caller, and nothing replaced it. Measured before removal — a live poller on that queue with an add
> rate and a dispatch rate of zero, while every activity a real run scheduled landed on
> `kontra-datasets`. Moving it is therefore a deployment fact, not a code change: run
the same image with `command: ["node", "dist/src/materializer.js"]` on a worker host, point
`KONTRA_ADDRESS` at the controller, and work follows.

The `orchestrator-materializer` service in `docker-compose.yml` is the single-host developer stack
(and what `kontra infra up` brings up). On a fleet, run that service definition on a worker instead:

```sh
docker run -d --name kontra-materializer \  # container name only; the queue is kontra-datasets
  -e KONTRA_ADDRESS=<controller>:7233 \
  -e KONTRA_S3_ENDPOINT=http://<controller>:8333 \
  -e KONTRA_MATERIALIZATION_DB='postgresql://kontra:…@<pg-host>:5432/kontra_ducklake' \
  -e KONTRA_DUCKLAKE_CATALOG='postgres:dbname=kontra_ducklake host=<pg-host> port=5432 user=kontra password=…' \
  -e KONTRA_DUCKDB_TEMP_DIR=/var/tmp/duckdb \
  --tmpfs /var/tmp/duckdb:size=2g --memory 512m \
  kontra-orchestrator:latest node dist/src/materializer.js
```

Both the materializer and `orchestrator-api` must point at the **same** `KONTRA_DUCKLAKE_CATALOG`
and the same `KONTRA_MATERIALIZATION_DB` — one writes, the other reads, and in production they are
different hosts, so only a shared database carries the **Lease** workflow. A Postgres catalog keeps its
`ducklake_*` tables in schema `public`; the status **Lease** workflow and summaries live in their own
application-owned schema `kontra`, beside them and never inside them.

The status store is checked at **boot** (`ensureSchema`): a materializer that cannot record status
is worse than one that is down — it would decode successfully and leave every run stuck in
`finalizing`.

### The layered memory boundary

Four boundaries, in order of how hard they bite:

1. **The cgroup is the hard RSS limit** — `mem_limit` on the container (512m). A setting alone has
   never stopped an OOM kill.
2. **DuckDB's `memory_limit` sits under it** (256MB), so DuckDB **spills to disk** before the kernel
   reaches for the OOM killer. It bounds DuckDB's *buffer manager*, not the process — it is not an
   OOM guard, and the same caveat applies to the `SET memory_limit` in `kontra explore`'s generated
   init script.
3. **The spill directory is size-capped** — a `size=2g` tmpfs plus `max_temp_directory_size`, so a
   runaway sort fills its own volume instead of the host's disk.
4. **One activity slot** — concurrency multiplies peak RSS directly. Raise it only against measured
   headroom.

Every compose service carries a hard `mem_limit` for the same reason: what must hold is the
*measured simultaneous p95* under 75% of physical RAM with ~1 GB left for the kernel and page cache.
Budgeting service-by-service without ever measuring the total is how a host OOMs while every
individual limit still looks reasonable. Measure with:

```sh
docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}\t{{.MemPerc}}'
```

### Materializer env

| Env var | Purpose | Default |
|---|---|---|
| `KONTRA_MATERIALIZER_SLOTS` | concurrent activity slots — multiplies peak RSS | `1` |
| `KONTRA_DUCKDB_MEMORY_LIMIT` | DuckDB buffer-manager budget (NOT an RSS cap) | `256MB` |
| `KONTRA_DUCKDB_THREADS` | DuckDB worker threads | `1` |
| `KONTRA_DUCKDB_TEMP_DIR` | spill directory — point it at a quota'd volume | _(unset ⇒ DuckDB default)_ |
| `KONTRA_DUCKDB_MAX_TEMP_SIZE` | ceiling on that spill | `2GB` |
| `KONTRA_MATERIALIZE_BATCH` | unit objects per verify+insert batch — trades object GETs against peak RSS | `256` |
| `KONTRA_MATERIALIZATION_DB` | the status **Lease** workflow + summary tables; `postgres://…` ⇒ schema `kontra`, else SQLite | `KONTRA_ORCHESTRATOR_DB`, else `orchestrator.db` |
| `KONTRA_DUCKLAKE_CATALOG` | the DuckLake catalog — must match `orchestrator-api` | `orchestrator-datasets.ducklake` |

Set on the **API** side:

| Env var | Purpose | Default |
|---|---|---|
| `KONTRA_EXPLORE_TOKEN` | gates `GET /api/runs/:id/explore` (presigned URLs) | falls back to `KONTRA_STATE_TOKEN`; neither ⇒ `503` |
| `KONTRA_MAX_INFLIGHT_NODES` | ceiling on concurrently in-flight node operations; each is one pending Nexus operation, and the server caps those per workflow | `64` |

`KONTRA_MAX_INFLIGHT_NODES` is stamped onto every graph at admission, so it is lowered without a
redeploy — keep it comfortably under the server's `limit.numPendingNexusOperations`. Exceeding that
cap does not fail loudly: the workflow task retries forever while every dashboard still reads
"running".

## 4. Running actors — remote machine

Point `KONTRA_*` at the controller — no shared network needed:

```sh
# both halves take the same block; the handler on the shared queue, the actor on -sessions
KONTRA_ADDRESS=<controller-host>:7233 \
  KONTRA_S3_ENDPOINT=http://<controller-host>:8333 \
  KONTRA_ORCHESTRATOR_URL=http://<controller-host>:8088 \
  KONTRA_REDIS_HOST=<controller-host>:6379 \
  KONTRA_ACTOR_NAME=<actor> KONTRA_ACTOR_VERSION=0.1.0 go run .   # from handler/
# + the actor process on the same machine, same env
```

Both processes reach the control plane **outbound only** — and both speak Temporal now, because the actor is itself an activity worker. The actor additionally needs S3 (per-unit blobs), Redis (its durable state) and the catalog (self-register); the handler needs S3 for the claim-check codec.

### On a fleet Machine

`control/orchestrator/src/infra/programs/machine.ts` renders **two units per Worker** and **one per Machine**:
`kontra-actor-<actor>` and `kontra-handler-<actor>` (the pair — the handler `Requires=` the actor, so
a handler never accepts workflows whose `RunBatch` nothing is polling for), plus `kontra-vmagent`
(scrapes localhost and remote-writes to the Controller, because a fleet Machine accepts no inbound
connections).

The unit names carry the actor because a Machine holds **several Workers** now (ADR 0037, packing).
Everything a Worker owns is named after it — its units, its environment at
`/etc/kontra/worker-<actor>.env`, its Bundle at `/opt/kontra/w/<actor>/`, and its metrics target at
`/etc/kontra/scrape.d/<actor>.json`. Two things follow that are easy to miss. The Bundle root is
per-Worker because the install does `rm -rf "$ROOT/actor"` before it unpacks, which under the old
singleton `/opt/kontra` would have deleted a co-tenant's code. And each packed Worker gets its own
`KONTRA_METRICS_ADDR` port (9110, 9111, …) because both actor hosts default to 9110 and the loser of
that race *catches the bind error and carries on serving nothing* — a Worker that runs perfectly and
can never be judged. The one agent scrapes them all through `file_sd_configs` over `scrape.d`, and
the `actor` label comes from the target file rather than from the agent's environment, which is the
only place it can be true when a Machine holds two.

A Machine placed before packing carries the old singleton `kontra-actor.service` pair; every converge
disables them, so the two layouts never both hold a poller on one queue.

There were four. `kontra-watchdog` and its 5-minute timer are gone (ADR 0037), and the reason is worth keeping: this page used to say the watchdog "is the one that earns its keep — it restarts an actor that is *sick but not dead* by reading the failure ratio out of the actor's own journal". It could not. `watchdog.sh` counted `grep -ci 'unit failed\|SessionLost\|engine dead'` over `grep -ci 'unit ok\|committed'`, and a sweep of `runtime/`, `sdk/` and `handler/` finds three of those five strings nowhere in the repo and the fourth only in comments — so the denominator was permanently zero and, with its `MIN_UNITS=10` gate, the timer could only fire on ten stray tracebacks, always at 100%. It never once counted 81 of 82.

Judging a Worker that is running and not working is the **Warden's** job now, because "the thing that decides a Worker is sick should be the thing that can restart it". `cli/warden/sickworker.go` scrapes the Worker's own `kontra_resource_loads_total` / `kontra_resource_load_failures_total` counters on the Warden's five-second turn and differences two readings, so the five-minute window is a fact about the Warden's clock rather than a hope about a log's retention; the verdict is three-valued and its `cannot tell` restarts nothing and reaches the Monitor as `loads: unknown` with the reason. Installing it is `kontra warden join`, not a placement — a Machine that has not enrolled has its Worker under `Restart=always` and no health authority of its own, which is what its chip then says.

## 5. Tracing — an endpoint you point at, not a service kontra runs

kontra ships no trace backend. The processes that trace — `orchestrator-api`,
`orchestrator-materializer` and the handler — are OTLP **clients**, and the endpoint is yours to
choose (ADR 0031). Jaeger used to be a compose service behind `--profile extras`; it is gone, and
with it the assumption that "the controller runs a collector".

**With no endpoint configured, nothing is exported and nothing is logged.** That is a property,
not an accident: no exporter is constructed at all, so there is no gRPC channel retrying an
address nobody listens on and no per-span error line. `OTEL_EXPORTER_OTLP_ENDPOINT` (or the
signal-specific `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) is the on/off switch, and **unset means
unset** — nothing anywhere defaults it.

To get traces locally, run any OTLP collector and point kontra at it. The receiver is the
contract; the backend is your choice (Jaeger all-in-one, an OpenTelemetry Collector, Tempo, a
hosted endpoint):

```sh
# 1. a collector, on the control-plane network so compose services resolve it by name
docker run -d --name otlp --network kontra \
  -p 127.0.0.1:16686:16686 jaegertracing/all-in-one:1.60.0

# 2. the control plane exports to it (the compose env passes these through from your shell)
OTEL_EXPORTER_OTLP_ENDPOINT=http://otlp:4317 \
  docker compose up -d orchestrator-api orchestrator-materializer

# 3. and so do the actor's worker containers
KONTRA_OTEL_ENDPOINT=otlp:4317 kontra serve --actor python/webcrawl --mode docker

# 4. one trace id, whole path: orchestrator → Temporal → handler workflow → blob activities
open http://localhost:16686
```

Two things decide whether the trace stitches rather than fragments:

- **Reachability from every exporter.** The control-plane services resolve the collector by
  compose DNS on the `kontra` network; a managed worker container needs the same; a **remote
  fleet Machine** needs a *routable* address, so a collector bound to loopback is unreachable
  from one no matter what you configure. Nothing in `machine.ts` sets a collector address today
  — a fleet Machine's `/etc/kontra/worker.env` carries Temporal, S3, Redis and the catalog and
  no OTLP endpoint — so fleet tracing is an explicit thing you add, not something that stops
  working when the collector moves.
- **One sample rate everywhere.** `KONTRA_OTEL_SAMPLING` is head sampling applied at the root;
  a kept parent with dropped children reads as a *missing hop*, not as a sampling decision.
  Leave it unset (1.0) locally; dial it down on high-fan-out fleet runs, on every process.

How deep the trace goes, stated plainly: the orchestrator's client interceptor and the handler's
Temporal interceptor cover the run start, the backing workflow, the blob activities and the
**scheduling** of each `RunBatch`. The actor host itself is not instrumented in either SDK — no
tracer provider, no activity interceptor — so what happens *inside* a batch is logs and
`/metrics`, not spans. `OTEL_SERVICE_NAME=kontra-actor-<name>`, which the worker entrypoint
exports, is read by nothing today.

If you run Jaeger all-in-one, keep `MEMORY_MAX_TRACES` bounded (e.g. `20000`). Its memory store
defaults to unbounded and was measured at 1.84 GiB — 48% of a 4 GB controller — which is what
drove load to 7.4 during a DuckDB extraction and OOM-killed the report generator.

## Replacing an actor mid-run

The handler's workflow owns retry = **exactly-once reload** (see [[Durability-and-Failures]]): if the actor dies mid-batch, its activity stops heartbeating, `HeartbeatTimeout` fires, and the retry lands on the same actor id (on whichever worker is polling the sessions queue) and replays its per-unit state from Redis, so the run completes — which holds only while that state is still there: the hash has a 24 h TTL, so the store runs `noeviction` rather than `volatile-lru`, under which a TTL'd key is exactly what gets evicted first. Self-asserting demo:

```sh
kontra workflow serve workflows/dnssweep.py
kontra workflow start workflows/dnssweep.py --wait \
  --input '{"dataset": "targets", "into": "live"}'
# actor log shows two RunBatch executions on the same actor id: failed (resource died) → ok (reloaded)
```

## Wiring reference

| Env var | Points at | Set on | Default (same host) |
|---|---|---|---|
| `KONTRA_ACTOR_NAME` / `KONTRA_ACTOR_VERSION` | the identity BOTH halves derive their queue from — they must agree | handler + actor | (required on the handler) |
| `KONTRA_ADDRESS` | Temporal gRPC | handler + actor | `localhost:7233` |
| `KONTRA_NAMESPACE` | Temporal namespace | handler + actor | `default` |
| `KONTRA_S3_ENDPOINT` | SeaweedFS S3 (codec + per-unit blobs + datasets) | handler + actor | _(unset ⇒ codec passthrough, inline commits)_ |
| `KONTRA_REDIS_HOST` | the shared state store, `host:port` — **required** | actor | `localhost:6379` |
| `KONTRA_MAX_PARALLEL_SESSIONS` | how many live Sessions one actor process holds at once (a host at its cap refuses an open, and the fleet takes it) | actor | `4` |
| `KONTRA_METRICS_ADDR` | the actor's own `/metrics` listener; `off` disables it | actor | `9110` (loopback in Go, `0.0.0.0` in Python) |
| `KONTRA_ORCHESTRATOR_URL` | catalog self-register (best-effort) — an actor's descriptor, and a workflow worker's one-per-`@workflow.defn` | actor + workflow worker | `http://orchestrator-api:8088` |
| `KONTRA_ACTOR_DIGEST` | this image's OCI digest (ADR 0011 pin) | actor | from the image build |
| `KONTRA_OTEL_ENDPOINT` | an OTLP collector **you** run, `host:port` — the entrypoint turns it into `OTEL_EXPORTER_OTLP_ENDPOINT` for both processes | handler + actor (via the entrypoint) | _(unset ⇒ no exporter, no tracing, §5)_ |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | the same collector, read directly by the orchestrator services and the handler (`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` also counts) | orchestrator-api + materializer | _(unset ⇒ no exporter, no tracing, §5)_ |
| `KONTRA_OTEL_SAMPLING` | head sample rate 0.0–1.0, applied at the root — keep it the SAME on every process of a run | every process that traces | `1.0` |

| Port | Service | Who talks to it |
|---|---|---|
| `7233` | Temporal gRPC | every handler, every actor, + dispatch |
| `8233` | Temporal UI | you |
| `8333` | SeaweedFS S3 | the handler (codec), the actor (per-unit blobs), the browser (presigned parquet) |
| `5000` | the OCI registry | `kontra deploy`/`scale` for Images, **and every fleet Machine**, which fetches its Bundle's layer from `/v2/bundles/<actor>/blobs/sha256:<sha>` (ADR 0036). A control plane without one cannot run a fleet run. |
| `8088` | Orchestrator | the UI + actor self-registration (which IS the schema compat gate — ADR 0027) |
| `6379` | Redis — the shared state store | every actor process, outbound to the controller |
| `9110` | the actor's `/metrics` | a local agent (`kontra-vmagent`), which pushes to the controller |

ADR 0018 deleted two env vars from the first table and five ports altogether — the actor's app port, the broker's `3001`/`3500`, its Prometheus `9090`/`9091`, and placement's `50005`. An actor serves no HTTP to the platform at all now; the only listener it opens is `/metrics`.

> **One SeaweedFS at a time**: the compose `seaweed` binds 8333/8888/9333/8080. If a standalone `docker run … chrislusf/seaweedfs` already holds them, `make up` fails on the port bind — remove the standalone (`docker rm -fv kontra-seaweed`); compose keeps objects in the named `seaweed-data` volume, so nothing depends on the one you delete.
