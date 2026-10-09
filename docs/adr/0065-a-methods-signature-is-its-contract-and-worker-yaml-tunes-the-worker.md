# 65. A Method's signature is its contract, and `worker.yaml` tunes the Worker

Date: 2026-10-09

## Status

**Accepted.** Implements PRD *the simplified platform* (`.scratch/simplified-platform/PRD.md`) D2,
bullets 2–4. Numbering: 0064 is held by an uncommitted ADR in the owner's tree, so this appends.

## Context

D2's promise is that an author who knows Temporal reads a kontra actor and meets one new concept,
the Batch. Two things stood in the way.

**The contract was said twice.** `@actor.method(takes=Product, emits=Enriched)` restated what a typed
signature already says, and nothing could read the signature: `Batch` and `Dataset` were not
generic, so `Batch[Product]` was a `TypeError`, and neither was importable from `kontra`. An author
who wrote the hints got a Method that advertised no schema.

**Worker tuning was a kontra knob or nothing.** `KONTRA_MAX_PARALLEL_SESSIONS` set
`max_concurrent_activities` and every other Worker option was unreachable from `actor.serve()`.

## Decision

1. **`Batch[T]` and `Dataset[R]` are generic, and exported as `kontra.Batch` / `kontra.Dataset`.**
   `@actor.method` reads the second and third positional parameters' annotations as `takes` and
   `emits`. Explicit `takes=`/`emits=` stay accepted and win; a disagreement warns, naming both. A
   hint naming a class defined later in the module is resolved when the catalog or the engine first
   reads it. The caller-side `kontra.catalog.Batch` keeps its name and its module.
2. **`worker.yaml` beside `actor.py` (or `workflow.py`) holds Worker options under the SDK's own
   names.** Python keys are `temporalio.worker.Worker` keyword arguments, validated against the
   installed SDK's signature at boot, before connecting. The PRD's draft key
   `max_concurrent_activity_task_pollers` is not a Python option (`..._polls` is) and is refused
   with that suggestion. kontra adds no aliases.
3. **Tuning only.** A key is accepted when the SDK types it as an int, float, bool or duration.
   `task_queue`, `identity` and `build_id` are refused by name: the queue is derived from the
   actor's name and version (`catalog.shared_queue`) and is not a knob, and the identity is the
   Worker name the logs and console carry.
4. **Precedence:** a programmatic `**worker_kwargs` > `worker.yaml` > the host's default.
   `KONTRA_MAX_PARALLEL_SESSIONS` stays an environment variable: it caps live Sessions, which is not
   a Worker option.
5. **Per-Session workers are not configured by it.** They serve one Session each with fixed slots;
   tuning them is a different question, left for the in-process worker (Phase 2).

## Consequences

- PyYAML joins the `[actor]` extra, which every actor image already installs. A missing PyYAML is
  an error only when a `worker.yaml` exists.
- Go actors are unchanged. Go's Worker options are `worker.Options` fields with different names,
  and signature derivation needs generic handles the Go SDK does not have yet; both are follow-ups,
  and the corpus for them is this ADR's tests.
