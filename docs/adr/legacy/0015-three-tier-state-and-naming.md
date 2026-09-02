# 15. Three-tier state model + the `arun_state` / `concurrent_aruns` naming

## Status

**Accepted — implemented.** This ADR owns the vocabulary: it names the three durable state
tiers, freezes the state-key scheme both SDKs honor, and records the naming table that maps the
wire concept to each SDK surface. What has shipped, in **both SDKs** unless noted:

- **`arun_state`** (tier 1) — the per-unit resume-scratch rename of `checkpoint`, with deprecation
  aliases (`self.checkpoint`, `concurrency=`) kept one release.
- **`session_state`** (tier 2) — cross-arun, same-session durable state with a sliding TTL.
- **`global_state`** (tier 3) — cross-session, actor-name-scoped durable state with ETag atomic
  ops. It **reuses the existing `statestore` component** through the regular Dapr state API — not a
  new named component (superseding the earlier plan note; reuse avoids a sidecar reload).
- **Streaming sub-units** — each record streams as its own durable blob, in **both SDKs**: Python
  via a `yield`-ing async-generator arun, Go via **`session.Emit(record)`**. Go's blob plane
  (`runtime/go/unitstore`, aws-sdk-go-v2) shares the same hive-partitioned key and
  `$ref` contract as Python's, so the orchestrator's streaming cursor consumes Go- and
  Python-emitted sub-units identically. **Amended** — the key is now
  `units/run={run}/dt={run-date}/actor={actor}/shard={nnnn}/unit={nnnnn}/{sha}.json`; see §7a.
- **`max_parallel_sessions`** — a per-worker cap on concurrent live sessions, enforced by a
  dedicated `{queue}-sessions` Temporal task queue. The **advisory `ActorDescriptor` field is
  deferred** (no orchestrator consumer until session fan-out lands; a proto-gen toolchain drift also
  made a clean additive regen infeasible this pass).

`checkpoint` was never ADR'd, so this supersedes nothing; it **refines ADR 0009 and ADR 0013**.

## Context

Kontra has **one** durable state tier today: the per-unit `checkpoint` (Dapr actor state, keyed
`u{i}-ckpt`, deleted on commit). Two gaps follow from that:

- **The vocabulary is asymmetric.** `checkpoint` is per-*unit* resume scratch, but nothing names
  the two tiers authors keep reaching for — durable state that survives across aruns *within a
  session*, and state shared *across sessions* of the same actor. Cross-arun state is in-memory
  only today (`self._inst`, nulled on reload); cross-session state does not exist. Adding those
  tiers under ad-hoc names would entrench the asymmetry.
- **`checkpoint` reads like a result store, and gets abused as one.** The name invited authors to
  thread output through it — the original crawl4ai `ResumableBFS` reference did exactly this (full
  page markdown through the resume slot, O(pages²)). The slot is **resume scratch**, deleted on
  commit; the rename makes that role legible, and the streaming `yield` contract (§7) now gives fat
  aruns a real output plane — crawl4ai's resumable tier keeps only a frontier in `arun_state` and
  streams pages as sub-units.

Separately, the in-session concurrency knob is named `concurrency` in Python but `Parallel(n)` in
Go and `parallel` on the wire — three names for one concept, with nothing recording that the
divergence is deliberate. This ADR fixes the vocabulary before any of the new tiers are built, so
every later phase speaks one language.

## The three tiers

| Tier | Scope | Lifetime | Backing store | Ships |
|---|---|---|---|---|
| **`arun_state`** | one unit (this arun) | deleted on commit | per-actor state manager (`u{i}-ckpt`) | shipped (both SDKs) |
| **`session_state`** | cross-arun, same session | sliding TTL; NOT deleted on commit | per-actor state manager, `s-` key prefix | shipped (both SDKs) |
| **`global_state`** | cross-session (= cross-actor-id) | durable, explicit ops | the shared `statestore` component via a `DaprClient`, ETag atomics | shipped (both SDKs) |

- **`arun_state` — per-unit resume scratch, NOT a result store.** **Keyed** `get(key)` /
  `set(key, value)` / `delete(key)`, so one unit can carry several independent slots (a frontier
  *and* a cursor). Rehydrated on reload, and the framework **deletes the whole unit's scratch when
  it commits**. Renamed from `checkpoint`; the capability grew from a single opaque slot to the
  shared keyed shape all three tiers use.
- **`session_state` — cross-arun, same session, durable.** Survives across aruns of one loaded
  session and is **not** cleared on commit. Sliding TTL (renewed on write and at turn boundaries)
  so an idle session's state never expires under it. This is the durable peer of today's in-memory
  `self._inst`.
- **`global_state` — cross-session, actor-NAME-scoped.** Shared across every session (every actor
  id) of an actor. It **cannot** use the per-actor state manager — that state is scoped to one actor
  id and unreadable by another — so it goes through the **regular Dapr state API over a `DaprClient`**
  with ETag atomic ops (`get`/`set` + `add_to_set`/`incr`/`compare_and_set`). It **reuses the
  existing `statestore` component** (keys namespaced so they never collide with actor state), so no
  new Dapr component or sidecar reload is needed. Its flagship shape is a **dedupe SET**, never a
  last-write-wins cursor.

**Author-pool rule (records the boundary between the tiers and `self.*`).** Live handles (a
browser, a client, a pool) live on `self.*` and are rebuilt by `@actor.load` on reload — never in
any state tier. Per-unit resume goes to `arun_state`. `session_state` is only for facts still true
after a from-scratch reload — never pool routing like "unit X is pinned to member 3," which a
rebuilt pool invalidates.

## Decision

### 1. The three tiers

Adopt the tier model above. All three tiers now ship in both SDKs; describing them in one ADR fixed
the key scheme (below) so each slots in without a second naming pass.

### 2. The state-key scheme (frozen)

| Tier | Python key | Go key | Reserved by |
|---|---|---|---|
| `arun_state` | `u{i}-ckpt` | `s{si}-u{i}-ckpt` | the `-ckpt` suffix |
| `session_state` | `s-{key}` (+ `s-index`) | `s-{key}` (+ `s-index`) | the `s-` prefix; `index` reserved |
| `global_state` | `kontra-global:{actor}:{key}` | `kontra-global:{actor}:{key}` | actor name (not name+version) |

- **`arun_state` keys carry the `-ckpt` suffix.** Python is single-arun, so its key is `u{i}-ckpt`
  (`i` = the unit's position in the input batch). Go processes per step, so its key is
  `s{si}-u{i}-ckpt` (`si` = step index). The author's **keyed slots all live inside that one
  per-unit blob** (a dict), so there is exactly one `-ckpt` key per unit regardless of how many
  author keys it holds. The two SDKs legitimately differ in shape — Python has one arun, Go has a
  step pipeline — so the congruence contract asserts the **scheme** (the `-ckpt` suffix), not
  identical strings.
- **`session_state` reserves the `s-` prefix.** Author keys are namespaced `s-{key}` so the tier is
  enumerable and can never collide with a `u{i}-ckpt` slot. A durable **`s-index`** list tracks the
  live keys so the host can re-touch every key's TTL at turn boundaries (Dapr has no TTL-touch
  primitive). Consequence: **`index` is a RESERVED session_state key** — `session_state.set("index", …)`
  is rejected loud (raises) in both SDKs.
- **`global_state` keys are `kontra-global:{actor}:{key}`, scoped by actor NAME, not name+version.**
  **A version bump deliberately shares the state** — a dedupe SET that reset on every version would
  be useless. This is a decision, not an oversight: name-scoping is what makes `global_state`
  cross-session *and* cross-version. The keys live in the shared `statestore` component but in a
  namespace distinct from actor state.

**The cross-SDK congruence contract asserts the SCHEME, not identical strings.** The congruence
test (extending the `actortype` pattern) pins the `-ckpt` suffix and the `s-` prefix, because the
Python single-arun and Go per-step models produce legitimately different key strings for the same
tier. Asserting string equality would encode an accidental match, not the real contract.

### 3. The naming table (ADR-owned)

One concept — **in-flight units per session** — surfaces under three names, and that divergence is
deliberate:

| Concept | Proto (wire) | Go surface | Python surface |
|---|---|---|---|
| in-flight units per session | `StepOptions.parallel` (field 2, **name frozen**) | `kontra.Parallel(n)` | `@actor.arun(concurrent_aruns=)` |
| per-unit resume scratch | `StepOptions.checkpoint` (field 3, string) | `Session.ArunState()` (new) | `self.arun_state` (was `self.checkpoint`) |
| cross-session cap | `DispatchOptions.parallel_sessions` (field 2, unwired) → `params` key | host slot count | `params` key, advisory descriptor |

- **`StepOptions.parallel` is frozen** — renaming the proto field would change the proto-JSON keys
  and break cross-SDK congruence. The wire name does not move.
- **`kontra.Parallel(n)` is unchanged** — it already matches the wire concept.
- **Python's `concurrent_aruns` intentionally diverges** from proto/Go. Rather than force Go to
  adopt an arun-flavored name, each SDK keeps the surface that reads naturally in its own idiom;
  all three denote the same thing (in-flight units per session, `window.py`'s `k`). The ADR is the
  record that the three names are one concept on purpose.

### 4. Decisions to pin

- **All three tiers share one keyed `get(key)`/`set(key, value)`/`delete(key)` shape.**
  `arun_state` was originally a single opaque slot with `delete()` deferred; it is now **keyed**
  like `session_state` and `global_state`, so a unit can hold several independent slots (a frontier
  *and* a cursor). All of a unit's keys live in **one** `u{i}-ckpt` / `s{si}-u{i}-ckpt` blob, so
  clear-on-commit still drops the whole unit's scratch with a single delete and the `-ckpt` key
  scheme is unchanged. The earlier asymmetry (arun_state single-slot, the wider tiers keyed) is gone.
- **`concurrent_aruns` stays dispatch-tunable via `param.get`.** `@actor.arun(concurrent_aruns=N)`
  takes a static int or a `param.get("workers", 4)` binding resolved per run — same mechanism
  `concurrency=` had. The rename does not touch how the value is bound.
- **Deprecation aliases live for one release.** `self.checkpoint` becomes a property that returns
  `self.arun_state` and warns; `@actor.arun(concurrency=)` stays an accepted kwarg alias for
  `concurrent_aruns=`. This keeps the rename incremental — docs and examples migrate without a flag
  day — and the aliases are removed in a later docs-consolidation pass (Phase 9).

### 5. `session_state` — cross-arun durable state

Tier 2 ships in both SDKs. Keyed `s-{key}` in the per-actor state manager, **not cleared on commit**,
and it survives a reload (same actor-id state namespace) — the durable peer of a fact an author would
otherwise keep on `self.*`, which a reload rebuilds from scratch. **Sliding TTL:** a write refreshes
the key's lifetime, and the host re-touches every live key at each turn boundary (the `s-index` list
enumerates them). Author surface: Python `self.session_state.get(key)/set(key, value)/delete(key)`;
Go `session.SessionState().Get(key, out)/Set(key, value)/Delete(key)`.

### 6. `global_state` — cross-session atomic state, on the shared store

Tier 3 ships in both SDKs. It reuses the existing `statestore` Dapr component through the regular
state API — a `DaprClient` opened **lazily on first use**, so an actor that never touches
`global_state` opens no extra channel — **not** a new named component; this supersedes the plan's
earlier "new named store component" note (reuse avoids a sidecar reload). Beyond `get`/`set` (LWW)
it offers **ETag optimistic-CAS atomics** so concurrent sessions never lose updates: `add_to_set`
(the dedupe flagship — reports whether the member was newly added), `incr`, and `compare_and_set`.
A contended atomic retries a bounded number of times (16), then fails loud rather than going stale.
Author surface: Python `self.global_state.get/set/add_to_set/incr/compare_and_set`; Go
`session.GlobalState().Get/Set/AddToSet/Incr/CompareAndSet`.

### 7. Streaming sub-units — both SDKs

An arun emits **one output-typed record per emit**, each committed as its own durable blob at
`units/run={run}/dt={run-date}/actor={actor}/shard={nnnn}/unit={nnnnn}/{sha256}.json` (§7a)
**before the frontier advances**. Python emits by
`yield`ing (an async-generator arun); Go calls **`session.Emit(record)`** (the peer of `yield`).
The `sha256` is over the canonical record, so the sub-unit id **is** its content — re-emitting the
same record on a resume is an idempotent overwrite. Records must therefore be
**content-deterministic** (no timestamps, no volatile headers), the author-visible identity rule.
The unit's `u{i}` done-marker is written only after the whole stream drains (holding the sub-unit
refs); the non-generator `-> [results]` (Python) / list-return (Go) path keeps the flat
`u{i}.json` key. The orchestrator's `pollParentUnits` cursor accepts both `u{i}.json` and
`u{i}/{hash}.json`, so a downstream node can consume sub-units as they land.

**Go streaming shipped** (previously deferred): `session.Emit` writes through a new Go blob plane
(`runtime/go/unitstore`, aws-sdk-go-v2 s3, path-style) under the **same `KONTRA_S3_*`
env contract** as Python's `unitstore` and the handler, and produces the identical
key/body/`$ref` shape — so the orchestrator consumes Go- and Python-emitted sub-units through one
cursor. With no object store configured the Go host collects emitted records inline (the dev/test
fallback, matching Python).

**Emit-durability gate (a resume-safety boundary).** Emits are durable **at emit-time only when an
object store is configured**. In the inline (no-`KONTRA_S3_*`) mode, emitted records live in memory
until the unit commits — but `arun_state` is durable. So a resumable streaming arun (one that keeps
an `arun_state` cursor to **skip already-emitted work** on a resume) would, in inline mode, durably
advance the cursor while losing the in-memory records → resume skips them → **silent loss**. The
gate the author checks is **Go `session.EmitDurable()` / Python `self.emit_durable`** (true iff a
store is configured): a resumable arun MUST disable its cursor when it is false and re-do the unit
atomically (re-emitting everything on the retry), and may use the cursor only when it is true. Both
example actors gate exactly this way (nuclei's phase cursor, crawl4ai's resumable tier).

### 8. `max_parallel_sessions` — a dedicated admission queue

Concurrent live sessions per worker are capped by a **dedicated `{queue}-sessions` Temporal task
queue** whose worker runs **only** `RunBatchOnDapr`; its activity-slot count
(`MaxConcurrentActivityExecutionSize`, from `KONTRA_MAX_PARALLEL_SESSIONS`; 0/unset = uncapped) is
the cap. `Close`/`StoreBlob`/`FetchBlob` and the backing workflow stay on the shared queue, so a
capped `RunBatch` turn can never starve the `Close` that frees a slot (a single queue would
deadlock). Workflow routing is **`GetVersion`-gated** (`route-runbatch-to-sessions-queue`) so
in-flight histories replay identically — and `RunBatchOnDapr` is registered on the shared worker too,
so pre-change runs still find a poller and drain. Slots bound active **turns**, not resident Dapr
instances (strict residency capping is future). The **advisory `ActorDescriptor` field is deferred**
— there is no orchestrator consumer until session fan-out lands, and a proto-gen toolchain drift made
a clean additive regen infeasible this pass.

### 9. No proto change

No tier in this effort touched the proto. `parallel`'s field name is frozen, and the one field a
phase would have added — the advisory `max_parallel_sessions` descriptor — is deferred, so
`buf generate` still produces no diff.

## Consequences

- **One vocabulary across the stack.** Every seam (`session_state`, streaming `yield`,
  `global_state`, `max_parallel_sessions`, the future MCP server) speaks the tier names and the key
  scheme fixed here instead of re-deriving them. The asymmetric `checkpoint`-only model is gone.
- **The key scheme is a contract, congruence-tested by scheme.** The `-ckpt` suffix and `s-` prefix
  are asserted across SDKs; the tests tolerate the Python/Go key-shape difference by construction.
- **`arun_state` legibly is not a result store.** The name removes the invitation to thread output
  through the resume slot; the streaming `yield` sub-unit plane (§7) is where fat-arun output
  belongs, and this ADR names the seam between them.
- **The atomics are the safe cross-session primitive.** `global_state` get/set is last-write-wins
  and silently drops concurrent updates; `add_to_set`/`incr`/`compare_and_set` are the ETag-CAS ops
  authors should reach for. The dedupe-SET is the flagship; a shared cursor is the anti-pattern.
- **A `global_state` version bump shares the set (accepted).** Name-scoping means two versions of an
  actor read/write the same `global_state`. For the dedupe-SET flagship that is the desired
  behavior; an author who wants per-version isolation must namespace their own keys.
- **The rename was a no-op; the new tiers are additive.** The `arun_state` rename is mechanical and
  covered by the existing suite (`test_checkpoint.py`, `test_dapr_host.py` pass via the aliases).
  `session_state` / `global_state` / streaming are opt-in — an actor that ignores them is unaffected.
- **One deferral remains.** The advisory `max_parallel_sessions` descriptor is deferred (see §8);
  the cap itself is enforced hard by the sessions queue in the meantime. Go streaming, previously
  deferred, now ships via `session.Emit` (§7), so the SDKs are at streaming parity.

Refines ADR 0009 (per-record isolation + per-unit checkpoint — this names that checkpoint
`arun_state` and sets its scheme) and ADR 0013 (`parallel` as the concurrency knob — this records
the per-SDK surface names for it). Supersedes nothing: `checkpoint` was never ADR'd. Author-surface
detail lives in [Writing Actors — Python](../wiki/Writing-Actors-Python.md) /
[Go](../wiki/Writing-Actors-Go.md); the state tiers in [Data Plane](../wiki/Data-Plane.md) and
[Durability & Failures](../wiki/Durability-and-Failures.md).


### 7a. The unit blob key is hive-partitioned (amends §7)

```
units/run={run}/dt={run-date}/actor={actor}/shard={nnnn}/unit={nnnnn}/{sha256}.json
```

Every segment is a `key=value` partition, so DuckDB's `hive_partitioning=true` returns run, dt,
actor, shard and unit as **columns**. This replaces `units/{run}/{node}/u{i}/{sha}.json`, which
encoded identity positionally and could not answer "what ran on the 1st", "which actor produced
this", or "which units produced nothing".

Three rules, each of which has a failure behind it:

- **`run=` leads.** An object store prunes a LIST only by literal prefix, and every interactive
  query filters by run. Measured on a 188,275-object bucket: `units/run=<run>/**` takes 0.17s;
  put `dt=` first and the same lookup takes 8.4s, because it must list the whole bucket. The
  conventional date-first hive order is the wrong one here.
- **`shard` and `unit` are zero-padded.** Unpadded, a glob for `n1` also matches `n10`-`n19`.
  That is not theoretical: on run `42aa8509` it returned 2,424 blobs where 63 were n1's, and
  the overcount reached a published report. A chunk-run suffix is split off *before* padding
  (`n1.2` -> `0001.2`), or it defeats the padding it appends to.
- **`dt` is the RUN's date**, passed down from the handler, not read from the writing worker's
  clock. Workers write independently; a run crossing midnight would otherwise split across two
  partitions and disagree between hosts.

**Three implementations, one fixture.** Go and Python write these keys; the TypeScript
orchestrator derives `shard=` to drive the streaming cursor. All three assert
`conformance/blobkey.json` in their own suites. This is not ceremony — the SDKs have
drifted before (isolation counters shipped Go-only), and the fixture caught a TS drift on its
first run. A drift here does not throw: the cursor matches nothing and the child node completes
**empty**.

**Both layouts are readable; there is no migration.** Existing data stays in the pre-hive layout
and every reader accepts both, reporting `actor` as NULL for legacy keys rather than inventing
one. A run that straddles the change is read as the union of the two.
