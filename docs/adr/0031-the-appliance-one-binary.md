# 31. The appliance: one binary owns the control plane, and Docker keeps the actors

## Status

**Superseded by 0063, 2026-10-08.** The appliance is deleted: `cli/appliance/` and every command
that only served it are gone, and the compose install (**0047**) plus the host Pulumi engine
(**0052**) are the only control plane. The decisions below are kept as the record of what was tried
and why — several of them outlived the packaging and are cited by live code, notably §1b (the
DuckLake catalog is a file under the install's data directory) and §4 (a workflow type absent from a
bundle HANGS rather than failing, so it is registered as a refusal).

**Accepted, 2026-08-25.** Records the nine decisions locked in `.scratch/appliance/PRD.md` before
slicing, with their reasons and their costs, so that every slice after this one cites a decision
instead of re-deriving it.

Supersedes no ADR. It is a **packaging** decision, not a design reversal: **0024**'s carry-forward
entry "the orchestrator owns provisioning, Pulumi in TypeScript, in-process, durable like every
other operation" (`legacy/0019`) is untouched — the appliance does not *ship* it, which is a
different statement from not believing it. Leans on **0023** §6/§12 for what a queue is and what a
Run is, on **0017** for the materialization ledger's separate authority, on **0020** for the Monitor,
on **0029** §3/§5 for the retention sweep, and on `legacy/0018` for the one that makes the whole
collapse possible: the actor process *is* a Temporal activity worker, so there is no second runtime
to embed.

It **closes GitHub #19 §2** rather than answering it, and by doing so removes the blocker #19 §3
named for arming the sweeper.

## Context

Running kontra means running twelve containers, and every one can be the wrong version, fail to
start, silently fill a disk, or be missing on a fresh machine. The repo carries the scars in its own
comments: a fresh SeaweedFS has no bucket and every write returns `403`; a named volume is
root-owned on first `up` and Temporal reports `unable to open database file: out of memory (14)` on a
box with 7.9 GB free; a volume-slot ceiling turns every write to a new bucket into a bare HTTP 500;
`ducklake-postgres` used to point at a container from an unrelated stack that happened to run on one
developer's machine. None of these are bugs in kontra. They are the cost of the topology, and the
topology is what this ADR removes.

Eight things were established against the tree before deciding, cited by file because several
contradict the shape the PRD assumed.

1. **The twelve are not twelve on a default `up`, and the missing ones are the ones that bite.**
   `registry`, `codec-server` and `jaeger` carry `profiles: ["extras"]`; `seaweed-bucket` and
   `temporal-data-owner` are one-shots that exit. A default control plane is seven long-lived
   containers. And yet `cli/deploy.go` tags and pushes every actor image to `localhost:5000`
   (`defaultRegistry`), pre-checking `/v2/` through `registryReachable` — so **the default local
   actor path needs a service the default `up` does not start**. The compose file's own `registry`
   comment already argues this ADR's thesis about one service: "a control plane that asks you to
   hand-start one of its own components is not installed, it is assembled."

2. **Both Postgres customers already default to a local file, and both say why Postgres exists.**
   `parquet.ts:resolveLakeConfig` defaults the DuckLake catalog to `'orchestrator-datasets.ducklake'`
   unless `KONTRA_DUCKLAKE_CATALOG` overrides it; `sql.ts:resolveStoreUrl` defaults the
   **materialization ledger** to `KONTRA_MATERIALIZATION_DB ?? KONTRA_ORCHESTRATOR_DB ??
   'orchestrator.db'` over `node:sqlite`. `materializationStore.ts`'s header gives the reason in one
   sentence: "the materializer runs OFF the controller … so the status writer and the API reader are
   different hosts; only a shared database can carry that." Two customers, one reason, and the reason
   is process separation.

3. **Three workflows share the infra queue, not two.** `control/orchestrator/src/workflows/infra.ts` exports
   `stackWorkflow`, `tmuxSessionWorkflow` **and** `sweepDatasetsWorkflow`. `infra.ts:119` says why
   the bundle exists: "`stackWorkflow` and `tmuxSessionWorkflow` share this queue because both need
   the cloud credential or the fleet key, and neither may run anywhere else." The sweep was added on
   the third reason — it is the controller-pinned workflow host. So a change that removed the queue
   along with Pulumi would silently take the Monitor (**0020**) and the retention sweep (**0029** §5)
   with it.

4. **Today's exposure posture is per-port binding discipline, and it is one mistake wide.** The
   compose header records the measurement — ten ports answered from a second droplet, including
   `8088` (unauthenticated by default: "remote code execution and someone else's bill") and `8333`
   ("SeaweedFS's S3 gateway which runs with NO identities config — every Dataset readable and
   writable") — and why the host firewall is not the control: Docker writes `DOCKER-USER`, consulted
   before ufw's `INPUT`. That posture's correctness rests on eleven `ports:` entries each naming the
   right address.

5. **`make test-examples` does not dial a control plane.** Every example suite imports
   `actorkit.testing`'s `stub_batch` / `collecting_dataset` and runs the actor in-process; the target
   deselects `-m 'not e2e'`; and the CI job that runs it is a bare `ubuntu-latest` with no compose.
   Not one example test references `KONTRA_ADDRESS`, `temporalio` or `:8088`. It is a real gate on
   the **SDK-side contract** and it is not, as written, a gate on the binary at all.

6. **`install.sh` is a one-platform pattern, not a four-platform one.** Go is pinned *and*
   checksummed — but the URL is `go${GO_VERSION}.linux-${arch}.tar.gz`. `buf` reads `uname -s`, and
   fetches `/releases/latest/download/` with **no version pin and no checksum**. The DuckDB CLI is
   `duckdb_cli-linux-${duck_arch}.gz`. The shape to copy is the Go one; the file does not yet contain
   a darwin leg, and one of its three fetches is not reproducible at all.

7. **The Node surface that cannot be Go is five names, and one that would have been is already
   built in.** Native: `@duckdb/node-api` (1.5.4-r.1) and `@temporalio/core-bridge` (1.18.1, pulled
   in by `@temporalio/worker`), plus the three `pnpm.onlyBuiltDependencies` entries — `@swc/core`,
   `esbuild`, `protobufjs`. The ledger's default backend is `node:sqlite`, part of the runtime, so it
   costs the artifact set nothing. `engines.node` is `>=22.13.0`.

8. **The SPA no longer range-reads parquet, so the "must sign" requirement has different owners than
   the PRD names.** `run/api.ts:1091` and `data/datasets.ts:779` both record the change — the browser
   used to presign every file and pull 36–76 MB of DuckDB-WASM; `previewDataset` now answers
   server-side JSON. Presigning survives for `kontra explore` (`data/explore.ts:166`, behind the
   token-gated manifest route at `server.ts:1062`), whose whole threat model is that "the URL, not
   the file, is the secret". S3 *semantics* are still load-bearing for the three SDKs' object stores,
   for remote fleet workers reaching the controller over the VPC, and for DuckDB's httpfs reading an
   `s3://` DATA_PATH.

## Decision

### 1. What collapses, service by service

| compose service | in the appliance | why it can collapse |
|---|---|---|
| `temporal` | **in-process** | `go.temporal.io/server` is a library. The dev server we run is that library plus a `main`. |
| `temporal-data-owner` | **evaporates** | It exists to `chown` a Docker named volume to uid 1000. No container, no volume, no uid mismatch. |
| `seaweed` | **in-process** | Replaced by a minimal S3 over the appliance's data directory. See §1a — this is the one replacement, not a re-hosting. |
| `seaweed-bucket` | **evaporates** | A bucket that the store creates on open cannot be missing. This one-shot is a workaround for a store that is not ours. |
| `redis` | **in-process** | An embedded KV speaking enough RESP that `statekv.py`, `internals/redis_kv.py`, `runtime/go/*` and `control/orchestrator/src/state.ts` are all unchanged. The two key shapes (`kontra-actor:<entity>` hash, `kontra-global:<actor>:<key>`) are the contract; RESP is the transport. |
| `registry` | **in-process** | `distribution` is a library. It becomes the second customer of the CAS (§2). This also fixes finding 1: the registry stops being opt-in for a path that is not. |
| `codec-server` | **in-process** | Already Go, already in `handler/`, already `CGO_ENABLED=0` (`infra/Dockerfile.codec-server`). It is an HTTP handler in a container; it becomes an HTTP handler. |
| `jaeger` | **evaporates as a service** | It is an OTLP endpoint. The binary exports to whatever `OTEL_EXPORTER_OTLP_ENDPOINT` names, and shipping a trace *store* was never the control plane's job. |
| `ducklake-postgres` | **evaporates** | §1b. Not a migration. |
| `orchestrator-api` | **one Node child** | Hydrated from the CAS, exec'd, supervised. |
| `orchestrator-materializer` | same child | The three roles are three `command:` entries over one image today; they become three workers in one process. |
| `orchestrator-infra` | same child, **minus Pulumi** | §4. The queue stays, the engine and the credential go. |

**The orchestrator is carried, not rewritten** (locked decision 1). A Go rewrite discards ~60 source
files, their suites, and `catalog.contract.ts`'s compile-time congruence guard, and it would move
under UX v2's feet while UX v2 builds on top. The binary owns Temporal, the object store, the KV,
the codec and the registry; the Node orchestrator ships as a pinned runtime plus prebuilt native
addons, extracted copy-on-write from the CAS on first run.

**Task queues do not merge when processes do.** `kontra-datasets` and
`kontra-infra` stay separate queues in one process, because a queue is *how work is routed* and a
process is only *where it runs* (**0023** §6 followed to its end: the queue name is the address).
(There were three; `kontra-materializer` was removed on 2026-09-26 as uncalled — see `queues.ts`.)
`queues.ts` already argues the remaining split on routing grounds — a caller's page read must
not queue behind a forty-minute decode — and that argument does not care how many processes exist.

**What is lost, and it is not nothing.** `queues.ts` says materializer placement is "enforced by
routing rather than by convention", and today routing is only half of it: the other half is
`mem_limit: 512m`, `KONTRA_DUCKDB_MEMORY_LIMIT: 256MB`, one activity slot, and a size-capped tmpfs
spill directory. One process means one heap, and the cgroup half of that enforcement goes away. The
DuckDB memory limit and the slot count survive as settings; the hard RSS boundary does not. On a
developer laptop that is the right trade. On the 4 GB controller it is not, which is one more reason
compose keeps the cloud job (§5).

#### 1a. The in-process S3 must keep S3 semantics, for consumers the PRD names wrongly

"Replaced by a minimal in-process S3 over the local filesystem" is right about the store and wrong
about who is watching. Finding 8: the SPA stopped range-reading parquet. Three consumers still
require real S3 and none of them is the browser —

- **The three SDKs' object stores** (`runtime/handler/internal/objectstore`, `sdk/python`,
  `control/orchestrator/src/codec/objectStore.ts`) speak SigV4 against an endpoint, and the golden blob-key
  fixture pins their key layout byte-for-byte across all three.
- **Remote fleet workers** reach the controller's S3 over the VPC — an address, not a filesystem
  path.
- **`kontra explore`** presigns GETs to an operator's workstation, where the URL *is* the credential
  (`data/explore.ts`). Dropping presigning would not degrade that surface; it would delete it.

So: keys, SigV4 request parsing, and presigned GET. Whether a signature is *enforced* is a separate
question this ADR does not settle — SeaweedFS today runs with no identities config (finding 4), so
signatures are currently accepted and ignored, and the appliance inherits that posture rather than
quietly changing it under §3's loopback bind.

#### 1b. `ducklake-postgres` evaporates because the topology does, not because the lake changed

This is the entry most likely to be misread later, so it is stated at length.

**Postgres is not a dependency of the lake.** `resolveLakeConfig` defaults the DuckLake catalog to a
local DuckDB file, and `KONTRA_DUCKLAKE_CATALOG=postgres:…` is an *override*. The override exists for
exactly one reason, and `docker-compose.yml` states it on the `orchestrator-api` entry: the catalog
"MUST match the materializer, so this process (the lister) reads the catalog that one writes." Two
processes, one catalog, therefore a server.

The same holds for the second customer, which the PRD does not mention and which would otherwise be
discovered as a surprise. `KONTRA_MATERIALIZATION_DB` carries the **materialization ledger**
(**0017**'s second authority) in its own `kontra` schema — never `public`, because "the DuckLake
catalog shares this database, and an unqualified table would sit among the `ducklake_*` tables where
catalog maintenance would happily drop it." `resolveStoreUrl` falls back to `KONTRA_ORCHESTRATOR_DB`
and then to `orchestrator.db`, over `node:sqlite`, with the same SQL, the same transitions and the
same compare-and-set assertions.

One process, therefore one catalog file and one ledger file, therefore no server. **Nothing about
DuckLake, the ledger's schema, its transitions, or ADR 0017's two-dimensional status changes.** This
is not a migration to SQLite; it is the removal of the override that made Postgres necessary, and
both defaults it falls back to have been in the tree the whole time.

Anyone who later re-splits the API and the materializer into two processes — and §1's cost note says
when they should — must bring the connstrings back. That is the correct reading: the setting is
alive and unset, not deleted.

### 2. One CAS with two customers, and **copy-on-write is not a security boundary**

`runtime/handler/internal/cas` is already the protocol as one deep module: `sha256` → store-if-absent on
write → fetch-and-integrity-check on read, with the caller supplying only an error noun. Two
customers use it in the appliance:

- the **appliance's own hydrated artifacts** — the Node runtime, the native addons, the built SPA,
  and (opt-in) Temporal's Web UI;
- the **embedded registry's OCI layers**, which are content-addressed by construction.

Do not build a second store. The intent is to point `distribution`'s storage driver at the same
object store the CAS wraps, so one set of bytes on disk serves both; a thin driver shim over
`objectstore.Store` is the fallback if the driver's own S3 configuration proves awkward, and it is a
shim either way, never a second store.

**Copy-on-write is not a security boundary. It is a deduplication and startup-cost mechanism.**
Anything that needs isolation gets it from the container runtime, exactly as it does today. This
sentence is here because the proposal it refuses is *attractive*: running untrusted actor code
directly against a COW overlay is cheap, fast, and removes a container start. It is also wrong. A
COW overlay shares a page cache, a kernel, a network namespace and a filesystem view with the
process that made it; all it guarantees is that a write does not corrupt the golden copy — a
property about *our* bytes, not about *their* reach. Actors are the one part of this system running
code we did not write, against targets hostile by definition. They stay in containers (locked
decision 3), deployed and scaled by the CLI's own Docker client, which `cli/go.mod` already carries
(`github.com/docker/docker v27.5.1+incompatible`) and `cli/scale.go:resolveWorkerImage` already
drives, preferring a local daemon image and pulling only when the daemon lacks one.

**Hydration is verified by digest, and a mismatch re-hydrates rather than proceeding.** `GetVerified`
already re-checks integrity on read and names the failure; the appliance's addition is that a
truncated or wrong-digest artifact is *re-fetched*, never exec'd. A half-hydrated Node runtime that
starts is a worse failure than one that does not.

### 3. Loopback only. This **closes** #19 §2 by deleting the caller

The appliance binds `127.0.0.1`.

GitHub #19 §2 asks a real question — "may the browser bundle carry a bearer token at all, and if so
which one?" — with the constraint that makes it hard: not the credential that also reaches
`POST /api/infra/stacks/:fqn/:op`, because "a credential a browser can reach must not be able to
spend money." The appliance does not answer that question. It removes it. **There is no browser
credential because there is no remote caller**: the SPA, the API, Temporal, the object store and the
KV all sit on the loopback interface of the machine the operator is sitting at, and anything that can
reach the API can already read the process's memory.

This is **0020**'s and **0030**'s argument about writes, applied to authentication: *a guarantee that
rests on there being nothing to reach is stronger than one that rests on the credential being handled
carefully.* Eleven `ports:` entries each naming the right address (finding 4) become one bind
address, and the `DOCKER-USER`-before-`INPUT` trap stops applying because nothing is published.

Two things follow immediately:

- **`VITE_KONTRA_EXPLORE_TOKEN` stops being baked into the SPA.** Today the build arg puts a bearer
  token in a browser bundle (`frontend/src/run/query.ts:token()`) so the query workbench can
  call its token-gated routes. On loopback there is nothing for it to protect against.
- **#19 §3's blocker is cleared, and only that one.** The sequence #19 states is "settle the posture,
  then arm." The posture is settled here. Arming the retention sweep is still the change that must
  also fix `buildRunDiscoveryQuery`'s missing `TemporalNamespaceDivision is null` clause and
  `SweepReport.kept`'s unbounded workflow result — this ADR does not touch either, and neither is
  made safe by loopback.

**The cost, stated plainly: the controller-on-a-droplet deployment is not covered by this posture,
and it keeps the compose stack.** That deployment is not hypothetical — it is how every cloud
campaign in this repo has run, it is what `provision-controller.sh` builds, and it is precisely the
case where a remote caller exists and #19 §2's question is still open and still unanswered. The
appliance does not make that installation worse; it does not help it either. A future decision to
serve a non-loopback appliance re-opens #19 §2 in full, and re-opens it *before* it binds an address,
not after.

### 4. The cloud fleet is out; the infra **queue** stays

No Pulumi, no provider plugins, no cloud credential anywhere in the binary (locked decision 4). That
is also what makes §1's three-roles-into-one-process merge free rather than a weakening:
`orchestrator-infra` is a separate process because Pulumi's Node language host installs
process-global `unhandledRejection` / `uncaughtException` handlers for the duration of every inline
run, so an unrelated rejected promise anywhere in the process fails the in-flight `up`. Remove Pulumi
and that hazard leaves with it — which also means **the Dashboard streamer no longer needs to be a
forked child**, since it was forked for the same measured reason (`infra.ts`'s header: an injected
rejection made `up()` throw and "the process survived every time"). No engine, no fork.

**The carve-out is precise, because finding 3 says a careless reading breaks two surfaces.** The
`kontra-infra` queue survives with `stackWorkflow` and the cloud credential removed from it, because
two of its three workflows have nothing to do with provisioning:

- **`tmuxSessionWorkflow`** — session existence on a machine is Fleet authority, and it is what the
  **Monitor** (`DashboardPage`, **0020**) is built on. Local panes are exactly the case the appliance
  serves best: a served worker's `kontra-wf-<queue>` tmux session on the operator's own host.
- **`sweepDatasetsWorkflow`** — **0029** §5's retention sweep, hosted there because that is the
  controller-pinned workflow host, with its one activity proxied onto `DATASET_QUEUE`.

So the queue is not "the Pulumi queue". It is the controller-pinned queue, and provisioning was its
first tenant, not its purpose.

`actorkit.fleet.up()` — Execution's one door into Fleet, per `CONTEXT-MAP.md` — therefore fails on
an appliance, and must fail *legibly*: "this control plane has no provisioner; run the compose
controller." A workflow that retries an activity which cannot succeed is the failure mode this repo
has already paid for twice, and it looks like a hung campaign.

### 5. Fresh start, and what the parity gate must actually run

The binary writes its own data directory. `docker-compose.yml` keeps running the old stack side by
side. **No migration code** — export anything worth keeping through the query surface.

The gate (locked decision 6) is two conditions, and **the second one carries the weight**:

1. `make test-examples` green against the binary, **and**
2. the local actor path proven end to end: `kontra deploy` → `kontra scale` → dispatch → a **Dataset**
   that lists, previews and exports.

Finding 5 is why, and it is recorded here rather than discovered by the slice that tries to claim
parity. As it stands `make test-examples` runs every example actor in-process against
`actorkit.testing` stubs, deselects `-m 'not e2e'`, and passes on a bare CI runner with no control
plane: **it would go green against a binary that never started.** It is a real gate on the SDK-side
contract — that the actor-facing API has not moved — and no evidence that the appliance serves it.

So the parity slice must either add an e2e leg to the examples suite that dials the binary's Temporal
address and object store, making `test-examples` the gate it is described as, or name the runnable
command that does. Until then condition 2 *is* the gate, and it must be a script anyone can run, not
a session someone remembers running.

The three component-level suites named in the PRD stay exactly as stated, and they are the strongest
available evidence because they are byte-level rather than behavioural: the **three-SDK golden
blob-key fixture** against the in-process S3, the **codec conformance suite** against the folded-in
codec, and `stateStore`'s existing suite against the embedded KV — all three unchanged, with no edits
to any client.

**Each slice deletes a service and proves a run.** Not "the component exists" but "the compose
service is gone and a real run still completes."

**Compose is retired as the *local development* path, not deleted.** Cloud campaigns still run on it
(§3, §4), so it keeps a job with a name.

### 6. Four platform artifact sets, and the reproducible build is the risk

linux and macOS, both arches (locked decision 7). Temporal's own Web UI is opt-in behind
`kontra up --temporal-ui`, hydrated from the CAS like every other artifact (locked decision 8).
Desktop packaging is later; `kontra up` serves the SPA and opens a browser (locked decision 9).

**The hard part is "buildable from GitHub", not copy-on-write.** COW is a well-understood mechanism
whose failure set §2 already bounds by digest. The artifact matrix is four platforms × {a pinned Node
runtime ≥22.13.0, `@duckdb/node-api`, `@temporalio/core-bridge`, and the three
`onlyBuiltDependencies` builds — `@swc/core`, `esbuild`, `protobufjs`} (finding 7), each of which
must resolve to the *same bytes* on two machines, or `go install` produces appliances that differ by
platform in ways nobody sees until a run fails.

`install.sh`'s **Go** leg is the pattern to copy — pinned version, per-arch sha256, `sha256sum -c`
before anything is unpacked — and it is the only leg worth copying. Finding 6: its `buf` leg fetches
`/releases/latest/download/` with no pin and no checksum, and its Go and DuckDB URLs hardcode
`linux`. **The four-platform matrix is new work, not an existing pattern generalised**, and calling
it a generalisation is how the checksums get skipped for the legs that do not have them yet.

One artifact the PRD does not count: `kontra dataset|db|monitor` drive a **standalone `duckdb` CLI as
a subprocess** (`cli/db.go:duckdbBin`), pinned at 1.5.5 against `@duckdb/node-api` 1.5.4-r.1 because
the two read one catalog. Carry it as a fifth per-platform artifact or degrade those three commands
with a message naming it — left open below. What must not happen is the pair drifting apart silently.

## Consequences

- **The failure surface a new user meets shrinks to one process.** Every scar in the Context section
  — the missing bucket, the root-owned volume, the volume-slot ceiling, the borrowed Postgres, the
  hand-started registry — is a property of a container that is not there, is the wrong version, or
  was started by hand. "Which container is unhealthy" stops being a question because there is one
  answer. And an upgrade becomes replacing one file: a stale `kontra-orchestrator` image has cost
  this repo whole campaigns, and `kontra infra up` reverting a container to its image is how a
  `make api` gets silently undone — neither is expressible against a single binary.
- **The `serve` mount tangle evaporates.** `orchestrator-api` today bind-mounts the host checkout *at
  its own path*, the host's tmux socket and `/usr/local/bin/kontra`, and re-derives
  `KONTRA_SERVE_ENV` because a host process cannot inherit compose DNS names — a fix paid for by a
  run that spun on `dial tcp: lookup redis on 127.0.0.53:53` until ScheduleToClose. A host process
  starting a host process needs none of it, nor `KONTRA_HOST_NAME`, which exists only because
  `os.hostname()` inside a container is a container id.
- **Two authorities keep separate stores in one process, deliberately.** The DuckLake catalog and the
  materialization ledger become two local files, not one table space. **0017**'s point is that
  execution status and materialization status are different questions with different owners;
  collapsing the processes must not collapse the authorities.
- **The infra queue outlives its first tenant.** `kontra-infra` now means "controller-pinned", so a
  reader who finds a queue named for Pulumi with no Pulumi in the binary should read §4 rather than
  delete it.
- **Isolation is unchanged, and it is the property most worth protecting.** Actors are containers
  before and after; the only thing this ADR adds is §2's sentence about why the cheap alternative is
  not available.
- **The cloud controller keeps every cost the appliance removes.** Twelve services, eleven bind
  addresses, an unauthenticated 8088 and an identity-less 8333 remain exactly as they are for the
  deployment that runs campaigns. A second supported topology, not a replaced one — pretending
  otherwise is how the compose file rots.

## Considered and rejected

- **Rewrite the orchestrator in Go.** Rejected on the size of what would be discarded: ~60 source
  files, their suites, and `catalog.contract.ts`'s compile-time congruence guard — while UX v2 is
  actively building on the surfaces those files serve. A carried runtime is ugly and reversible; a
  rewrite is neither.
- **Keep Postgres and run it embedded.** Rejected on finding 2: there is nothing to embed *for*. Both
  consumers already default to a local file, and embedding a server to satisfy an override one
  topology needs is the assembly this ADR removes.
- **Merge the three task queues along with the three processes.** Rejected: a queue is how work is
  routed (**0023** §6), and `queues.ts` argues the materializer/dataset split on tuning grounds that
  survive the merge. Merged, a caller's page read queues behind a forty-minute decode and looks like
  a hung workflow.
- **Bind `0.0.0.0` and answer #19 §2 with a browser token.** Rejected for the local appliance: it
  buys a remote caller nobody asked for and re-introduces the exact question — which credential, and
  can it spend money — that loopback deletes. The compose controller is where that question must be
  answered, because that is where the remote caller actually exists.
- **Ship the fleet provisioner in the binary anyway, since Pulumi is already TypeScript.** Rejected:
  it puts a cloud credential and four provider plugins into an artifact whose premise is that a new
  user runs it on their laptop a minute after `curl`, and it would make the three-role merge unsafe
  (§4).
- **Run actors on the COW overlay instead of in containers.** Rejected; §2 exists so this is found
  rather than re-derived. Cheap, fast, and it hands untrusted third-party-facing code a shared kernel
  and namespace.
- **Migrate data from an existing compose deployment.** Rejected (locked decision 5). Migration code
  for a topology being retired has a known end date and an unknown bug count; export through the
  query surface instead.

## Open

- **The DuckDB CLI as a fifth pinned artifact.** §6. Carry it, or degrade `kontra dataset|db|monitor`
  with a message that names the tool and its pinned version. Not decided here; what is decided is
  that the CLI and `@duckdb/node-api` must be pinned as a *pair*, since they read one catalog.
- **Whether the in-process S3 enforces signatures.** §1a keeps today's posture (accepted and ignored)
  so the appliance does not silently change a security property while it changes a topology. A
  loopback bind makes enforcement cheap to add and cheap to defer; deferring is not the same as
  deciding.
- **What `kontra up` does on a machine that also runs the compose stack.** Both want a data
  directory, and on a developer box both will exist during the parity window (locked decision 5). Port
  collisions are the visible half; the silent half is two control planes with two catalogs and one
  operator.
- **Whether the appliance ever serves a non-loopback address.** If it does, #19 §2 re-opens in full
  and must be answered before an address is bound, not after.
