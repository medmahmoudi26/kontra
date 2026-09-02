# @kontra/orchestrator

The control plane around kontra's actors: an HTTP API over the **catalog**, the **Datasets** in
the lake, the **Runs** Temporal is executing, and the **provisioning** of the machines they run
on. It never imports or runs actor code, and it executes no Batch on their behalf.

> **It starts workflows; it does not execute Batches.** It used to do both: one generic
> `interpreter` workflow executed a whole graph of actors, routed each node by `(name, version)`,
> threaded field-mapped data between nodes as claim-check refs, and owned sharding, failure
> policy, materialization and a streaming cursor. ADR 0023 §12 removed THAT — the interpreter and
> the arbitrary GRAPH it took, not the verb. A **Run** is now one execution of a **caller's**
> workflow — the caller's own code, on the caller's own task queue, dispatching **Method** calls
> on **Actors** with ordinary control flow (`actorkit.catalog` on the Python side). That
> removed the graph, and with it Node, Chunk and Manifest as words.
>
> **`POST /api/runs` is not a 404**, and has not been since 2026-08-15: it starts one execution of
> a REGISTERED caller's workflow, named by folder and type, on a queue derived from that folder's
> content, and it refuses outright when no worker polls that queue. The test that replaced the
> absent route is a COUNT (ADR 0033 §1) — how many Methods can one request name? One is a probe;
> two is a topology, and a server that executes a topology is the interpreter again, whatever the
> route is called.

## What a Run is now

```
your workflow (your machine, your queue)          ← the Run; its workflow id IS the run id
   └─ Nexus kontra.actor:run                      ← one per Method call (Go handler)
        └─ RunBatch activity                      ← the Actor itself (Python or Go)
```

This package STARTS a Run and then READS it. Nothing below the start is its business — the
dispatch, the Batch and the Actor all happen in processes it does not host:

| Method + path | Purpose |
|---|---|
| `GET /api/health` | liveness |
| `GET/POST /api/actors`, `DELETE /api/actors/:key` | catalog CRUD |
| `POST /api/actors/:key/digest` | worker self-registration of its OCI digest (ADR 0011) |
| `GET/POST /api/graphs`, `GET/DELETE /api/graphs/:id` | saved design documents (opaque to the server) |
| `POST /api/runs` | start ONE execution of a registered caller's workflow, by folder + type — never a queue string (GitHub #15), never a graph |
| `POST /api/sources/actor/:id/probe`, `GET /api/probes/:runId` | the Actor probe: one Actor, one version, one Method, one Batch, and no field for a second (ADR 0033) |
| `GET /api/runs` | the runs kontra can see, newest first (Temporal Visibility) |
| `GET /api/runs/:runId` | one run: both status dimensions + the projection (ADR 0017) |
| `GET/POST /api/runs/:runId/progress` | the actor host's `@actor.healthcheck` beats |
| `GET /api/runs/:runId/heartbeats` | the `RunBatch` per-Batch heartbeat, read off pending activities |
| `GET /api/runs/:runId/explore` | token-gated presigned manifest for THIS run's data files |
| `GET /api/datasets`, `/api/datasets/:name/preview`, `/api/datasets/runs` | the lake |
| `POST /api/datasets/query`, `GET /api/datasets/schema`, `GET /api/datasets/export` | the SQL workbench (token-gated) |
| `GET /api/summaries/*` | bounded operational scalars for dashboards |
| `GET /api/state/:tier` | raw actor state inspection (token-gated) |
| `POST /api/infra/stacks/:fqn/:op`, `GET /api/infra/*` | provisioning (ADR 0019, token-gated) |

**How a run is discovered.** The caller's workflow is the caller's — this process cannot
enumerate it by type or by id prefix. What it can see is the work the run dispatched: the Go
handler stamps `KontraRunId` (the caller's workflow id) onto every backing workflow, so the
distinct ids are the runs, and each is then described BY that id. The stated limit: a run that
dispatched nothing is not listed, because nothing in the cluster records that it existed.

**Two status dimensions, never merged** (ADR 0017). `execution` is Temporal's answer to "did the
caller's workflow finish"; `materialization` is the status store's answer to "is the output
queryable". `lifecycle` is a projection over both and is never a `RunStatus` member — one label
over three outcomes is the defect that model exists to prevent.

## Roles

Three roles, each pinned by its task queue — and since ADR 0031 §1 a role is not a process:

```
api           the HTTP API above + the built SPA (one origin); polls no queue, it is a client
materializer  the ONLY writer of typed DuckLake output — kontra-materializer + kontra-datasets
infra         the controller-pinned queue: the Monitor's session converge + the retention sweep
```

`node dist/src/main.js` runs any subset, named by `KONTRA_ORCHESTRATOR_ROLES` (unset = all three).
**Merging the processes does not merge the queues** — [src/roles.ts](src/roles.ts) refuses to boot
if two roles were handed one name, because two pollers on one queue is a failure every surface
reports as healthy.

Which subset is a deployment fact:

- **The appliance** (`kontra up`) runs all three in one process. It can, because it ships no Pulumi
  engine — the process-global rejection handlers that made `orchestrator-infra` a separate PID
  leave with the engine (ADR 0031 §4). Its infra role registers `stackWorkflow` as a **refusal**
  ([src/workflows/appliance.ts](src/workflows/appliance.ts)): a type nobody registered does not
  fail a `fleet.up()`, it hangs one.
- **The compose controller** runs `api,materializer` in `orchestrator-api` and keeps
  `orchestrator-infra` — `node dist/src/infra.js`, the Pulumi engine and the cloud credential —
  as its own container (ADR 0034 §1). Cloud provisioning is that deployment's job.

There is no `orchestrator-worker`. It hosted the interpreter, its `mapAndRoute` activity and the
scheduled-graph-run workflow; all three are gone.

## Refs are the currency (claim-check)

An Actor's bulk output never enters this process. The codec port
([src/codec/claimCheck.ts](src/codec/claimCheck.ts)) is **byte-compatible** with the Go handler's
(handler/internal/codec) — marker `binary/claim-check-v1`, ref `{sha256,size,meta}`, sha256
integrity check — so both read and write the same `cas/<sha[:2]>/<sha>` objects. The materializer
resolves an output envelope by its `sha256` and reads it with DuckDB, page by page, without the
payload ever crossing into Node.

## Layout

```
types.ts           the contract the API speaks: RunStatus, PerUnitFailure, JsonSchemaDoc
entry.contract.ts  compile-time congruence guard: the claim-check ref ⇄ generated BareRef
_gen/              generated proto stubs (kontra/v1, buf-owned — never hand-edited)
src/
  server.ts          Fastify: everything in the table above, plus the SPA
  runs.ts            the run read surface: both dimensions and the projection over them
  temporalClient.ts  describe/list runs + read RunBatch heartbeats. It starts nothing.
  materializer.ts    the isolated materializer worker (activities/materialize.ts)
  infra.ts           the Pulumi worker (workflows/stack.ts, activities/infra.ts)
  codec/             claim-check codec + object store, byte-compatible with the Python codec
  data/              the lake: DuckLake writer, dataset listing, query engine, explore manifest
  db/repo.ts         SQLite: the actor catalog and saved design documents. No run records.
web/               React Flow design tool (its own Vite app)
Dockerfile         one Node image; the compose services choose the entrypoint
```

## Develop

```bash
pnpm install
pnpm test          # vitest
pnpm run typecheck # tsc --noEmit
pnpm run build     # tsc -> dist/ (production only; tests excluded)
pnpm run serve     # API + SPA on :8088
```

## Configuration (env)

| Variable | Default | Purpose |
|---|---|---|
| `KONTRA_ADDRESS` | `localhost:7233` | Temporal gRPC endpoint |
| `KONTRA_NAMESPACE` | `default` | Temporal namespace |
| `KONTRA_ORCHESTRATOR_PORT` | `8088` | the API/SPA HTTP port (8080 is SeaweedFS) |
| `KONTRA_ORCHESTRATOR_DB` | `orchestrator.db` | SQLite catalog + design store (`:memory:` for none) |
| `KONTRA_MATERIALIZATION_DB` | `$KONTRA_ORCHESTRATOR_DB` | the materialization status store (postgres:// in production) |
| `KONTRA_ORCHESTRATOR_ROLES` | `api,materializer,infra` | which roles this process serves |
| `KONTRA_MATERIALIZER_QUEUE` | `kontra-materializer` | the materializer role's task queue |
| `KONTRA_INFRA_QUEUE` | `kontra-infra` | the provisioning worker's task queue |
| `KONTRA_S3_ENDPOINT` | _(unset)_ | object store; unset ⇒ codec passthrough |
| `KONTRA_S3_BUCKET` / `KONTRA_S3_PREFIX` / `KONTRA_S3_THRESHOLD` / `KONTRA_S3_REGION` / `KONTRA_S3_ACCESS_KEY` / `KONTRA_S3_SECRET_KEY` | see [src/codec/objectStore.ts](src/codec/objectStore.ts) | shared with the Python side |
| `KONTRA_EXPLORE_TOKEN` / `KONTRA_STATE_TOKEN` | _(unset)_ | bearer tokens for the workbench/explore and state/infra routes |
