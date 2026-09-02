# 12. Split EntryInput: the actor runs one batch, the dispatcher shards (single-skeleton SDK binding)

## Status

**Accepted — staged migration; Phase C complete both SDKs.**
- **Phase A (contract) — done:** `contracts/kontra/v1/actor.proto`
  (`ActorRunInput` / `ActorRunResult` / `PerUnitFailure` / `ErrorInfo`), Go codegen + wire-congruence,
  and the behavioral corpus at `conformance/execution/`.
- **Phase C (Go) — done:** `actorkit/go` has `ActorWorkflow` (one batch, no sharding) with **SessionLost
  rebuild-once + SessionUnrecoverable** (previously a Go deferral), and a Go conformance harness that
  passes **all 4** `conformance/execution/scenarios.json` (`Session.Rebuilds` exposes the rebuild count;
  `SessionLost` is workflow-driven, non-retryable to the activity). The old `RunWorkflow` stays until E.
- **Phase C (Python) — done:** `runtime/python/internals/workflows.py` now has a Python `ActorWorkflow`
  (`ActorRunInput → load → one-unit steps → close`) congruent with Go — **SessionLost rebuild-once →
  SessionUnrecoverable**, language-agnostic dedicated-queue pinning (NOT Go Worker Sessions).
  `tests/test_execution_conformance.py` runs the SAME corpus against it and passes **all 4** scenarios;
  the earlier `session-unrecoverable` **strict xfail is removed** — the slim `ActorWorkflow` delivers the
  contract `terminal`/`SessionUnrecoverable` the old 3-tier hosted-worker model got wrong. Both SDKs now
  produce identical envelopes/attempt-counts/failure semantics across the corpus. `EntryWorkflow` /
  `Session`/`StepWorkflow` stay until E.
- **Pending:** D (orchestrator fan-out over `ActorWorkflow` per `DispatchSpec`), E (retire the old
  `EntryInput`/Session/`RunWorkflow` paths), F (DIY sharding example).

## Context

The Python and Go SDKs **drift** because each re-implements the orchestration-heavy parts:

- **Sharding + session fan-out lives in the worker.** Python nests `EntryWorkflow → SessionWorkflow
  → StepWorkflow`; the Go SDK is one flat `run` workflow + Temporal **Worker Sessions**. Worker
  Sessions is a **Go-only feature** — Python has no `CreateSession` API, so the two SDKs *cannot*
  share that pinning mechanism. The drift-prone logic is duplicated across two languages.
- **The actor is not first-class.** There is no `actor.proto` and no `ActorWorkflow`; the actor's
  invocation contract hides inside `EntryInput` + a workflow generically named `"run"`.
- **`EntryInput` conflates two concerns:** *dispatch* (`parallel_sessions` / `chunk_size` — how to
  shard) and *actor-run* (units, params, lineage, digest, return_ref — run one batch).

All three contradict the documented intent — *"polyglot via single-lang workflow skeleton"*: one
orchestration skeleton, thin per-language actors. What got built is the opposite, which is exactly
why binding the SDKs is hard.

## Decision

1. **Split `EntryInput`** into the dispatcher's `DispatchSpec` (sharding knobs) and the actor's
   **`ActorRunInput`** (one batch — `actor.proto`).
2. **Make the actor first-class** — `actor.proto`: `ActorRunInput`, `ActorRunResult`,
   `PerUnitFailure`, `ErrorInfo`. (The envelope + failure shapes lived only in each SDK before; now
   they are proto, congruence-tested per language.)
3. **The actor SDK implements one thing — `ActorWorkflow`:**
   `ActorRunInput → load → ordered steps (one-unit activities with the author's RetryPolicy + per-unit
   isolation + checkpoint) → close → ActorRunResult`. **One batch. No sharding, no session fan-out,
   no entry-level continue-as-new.** This surface is small and *identical* across languages.
4. **The orchestrator (single TS) owns the fan-out** — shard a node's input per `DispatchSpec`,
   start N `ActorWorkflow`s (one per batch, bounded), aggregate the N envelopes. The drift-prone
   logic lives **once**.
5. **Session pinning is language-agnostic** — a dedicated per-run task queue + worker (what Python
   already does), **not** Go Worker Sessions (Go-only). The Go SDK drops `CreateSession` so the two
   SDKs *can* match.
6. **Bind what remains with two artifacts:**
   - `actor.proto` — pins the **shapes** (congruence-tested both sides, like `EntryInput`).
   - `conformance/execution/` — the **behavioral** corpus: given a step that fails unit X retryably
     and Y non-retryably under RetryPolicy Z, both SDKs must produce the same envelope, same attempt
     counts, same `terminal`/`exhausted`; `SessionLost → rebuild-once → SessionUnrecoverable`;
     checkpoint resume. CI fails on divergence. (The codec corpus, but for the value surface.)
7. **DIY sharding is not in the SDK.** A DIY example shows client-side fan-out (shard + start N runs);
   the pure `shard()` function stays shared and conformance-tested.

## Consequences

**Gains:** thin, bindable SDKs (one small identical surface); the drift-prone fan-out lives once in
TS; the actor becomes a first-class contract; the design matches the documented single-skeleton
intent; the codec-style conformance discipline now covers execution too.

**Costs (accepted):**
- The dispatcher does **data-plane sharding** — `mapAndRoute` emits N session sub-batches/refs per
  node instead of one (a natural extension of what it already does), and the interpreter aggregates
  N runs per node.
- `max_sessions` becomes **worker capacity** (Temporal queues excess runs); `parallel_sessions`
  becomes *how many runs the dispatcher starts at once*.
- **Continue-as-new relocates** — the dispatcher's fan-out loop CANs for huge N; the actor still CANs
  *inside a step* for huge per-step unit counts. It moves to where each concern lives.
- The **DIY persona** loses built-in fan-out → an example, not a default.

**Migration (staged, each verifiable):**
- **A — contract (done):** `actor.proto` + Go congruence + the execution corpus.
- **B — corpus harness target:** the conformance actor + scenarios are the behavioral contract both
  SDK harnesses will run.
- **C — SDKs add `ActorWorkflow`** accepting `ActorRunInput` (additive, both languages,
  language-agnostic dedicated-queue pinning; Go drops Worker Sessions). Pass the execution corpus.
- **D — orchestrator shards + fans out** `ActorRunInput` per `DispatchSpec`, aggregates per node.
- **E — retire** the `EntryInput` sharding path, Python's `Session`/`StepWorkflow` tiers, and Go's
  Worker-Sessions pinning.
- **F — DIY sharding example.**

This supersedes the actor-side fan-out in the current EntryWorkflow path; `entry.proto` remains until
Phase E. Reinforces ADR 0006 (server-minted runId), 0009 (per-step failure policy), 0011 (digest pin).
Refined by ADR 0013 (a step processes one unit; `parallel` replaces chunk_size/max_concurrency).
