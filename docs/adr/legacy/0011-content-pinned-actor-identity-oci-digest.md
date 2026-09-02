# 11. Content-pinned actor identity via OCI image digest

## Status

**Implemented** (env-gated — off until digests are supplied). The full loop:

- **A — contract + worker verify.** `EntryInput.expected_digest` (entry.proto field 10),
  threaded `EntryInput → SessionInput → load`; `load` self-verifies via
  `runtime.py:_verify_actor_digest` against its own `KONTRA_ACTOR_DIGEST` and fails
  **non-retryably** (`ActorDigestDrift`) before opening resources.
- **B — orchestrator stamp + drift check.** `ActorRecord.digest`; `POST /api/runs` stamps each
  node's `expectedDigest` from the catalog and **409s** when a graph's design-pinned digest no
  longer matches the catalog's current one.
- **C — build supply.** `scripts/build.sh` inspects each image's digest → a per-actor
  `.digest.env` that compose hands the worker as `KONTRA_ACTOR_DIGEST`.
- **C2 — worker self-registration.** On startup the worker best-effort POSTs its digest to
  `/api/actors/{key}/digest` (`actorkit/catalog.py`), updating only the digest (preserving
  schemas), so the catalog learns it without a design-tool round-trip.
- **D — designer pin.** The SPA syncs server digests into its catalog on load
  (`fetchActorDigests`) and pins the actor's digest onto a node when placed; `toExecutionGraph`
  emits it. A later rebuild → new worker digest → catalog moves → the saved graph's stale pin
  trips B's 409.

## Context

Arch-review theme 1 (the most structural finding): `(name, version)` is the only cross-plane
key, but it is a **free string on a `:latest` image with no digest**. A redeploy-without-bump
silently routes a graph — typed and field-mapped against the *old* schema — to *new* code on
the same `{name}-{version}` task queue. Nothing proves the worker now serving that queue is the
same content the graph was designed against. (Decided over a schema/manifest content-hash: the
**image** digest pins code identity, not just the declared schema.)

## Decision

- Actor identity is content-pinned by the **OCI image digest** (locally the image ID; in the
  cloud the registry RepoDigest). `(name, version)` still routes; the digest verifies.
- The pinned digest travels on the wire as `EntryInput.expected_digest`, stamped by the
  orchestrator from the catalog. The graph captures the digest it was designed against.
- Enforcement is **at the worker**, where the running digest actually is: `load` compares its
  own `KONTRA_ACTOR_DIGEST` (stamped at deploy time) to the pinned one and fails fast,
  **non-retryably**, before opening resources — retrying the same wrong image is futile.
- **Env-gated**, matching the repo convention (claim-check codec, object store): either side
  empty ⇒ unpinned (CLI / dev inner loop / unbuilt worker) ⇒ no-op. Pinning is opt-in via the
  build/deploy supplying digests, never a hard requirement of the dev loop.

## Consequences

- The worker is the enforcement point (not the orchestrator guessing what serves a queue), so
  the check is sound even with no liveness registry — but it requires the deploy to stamp
  `KONTRA_ACTOR_DIGEST` for enforcement to engage.
- A drift is a clean, non-retryable node failure that the interpreter's per-node failure policy
  (ADR 0009) already surfaces — no new error path.
- Pairs with the reconciled lifecycle verbs + `kontra status` (the next P2 item): the digest is
  the key those reconcile on.
- Local-only by design (no auth): the worker→orchestrator registration is a plain local POST.
  Cloud hardening (auth, a registry RepoDigest instead of the local image id) is deferred.
