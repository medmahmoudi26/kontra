# 22. A dispatch can be keyed; `object_state` is that key's state

## Status

**Accepted.** Extends **ADR 0015** with a fourth state tier — it does not revise the three, and
their scopes are unchanged. Extends **ADR 0021** with `handle[key]` on the caller's side. Nothing
here changes the wire, the handler, or any deployed image; the identity machinery it exposes has
been in `runtime/handler/workflow.go` since ADR 0018.

## Context

Kontra's **Actor** is not an actor in the Hewitt/Erlang sense and not an Orleans virtual actor;
`CONTEXT.md` records that measurement and the reasons it was made deliberately. But a narrower,
more practical pattern — Restate's **Virtual Object**, the same shape as Cloudflare's Durable
Objects — asks for much less, and it turned out we already satisfied most of it by accident.

A Virtual Object is: a unique key, K/V state attached to that key retained indefinitely, and at
most one write handler running at a time per key. Crucially there is **no activation** — no
perpetual in-memory instance, no mailbox. State lives in the runtime's store and handlers are
ordinary functions. Measured against that:

| Restate Virtual Object | kontra, before this ADR |
|---|---|
| Unique key identity | **held** — `idempotency_key` is the key |
| One write handler at a time per key | **held** — server-enforced by workflow-id uniqueness |
| Concurrency across different keys | **held** — capped by `KONTRA_MAX_PARALLEL_SESSIONS` |
| K/V state attached to the key | missing — `global_state` is name-scoped, `session_state` dies with the batch |
| Shared read-only handlers | missing — every dispatch is a write batch |

The first three were already true because of a line that has been there since ADR 0018:

```go
// handler/nexus.go
base := in.IdempotencyKey
if base == "" { base = joinNonEmpty(in.RunID, in.NodeID) }
return "actor-" + os.Getenv("KONTRA_ACTOR_NAME") + "-" + base
```

Note also what `concurrent_aruns` does *not* violate. Orleans promises an activation executes one
turn at a time, and running N units inside one instance inverts that — the reason we fail the
Orleans comparison. Restate serializes *handler invocations*, and a batch is one invocation whose
internal concurrency is the handler's own business. Same code, compliant with one model and not
the other. This ADR pursues the model we already fit.

Two things stood between that and the pattern being usable, and a third only appeared once the
first was fixed.

**The caller could not reach the key.** `ActorHandle.dispatch` defaults `node_id` to
`workflow.uuid4()`, so every dispatch minted a *fresh* identity — the exact opposite of a virtual
object. `idempotency_key` was a keyword argument on two methods, documented as a retry affordance,
and nothing suggested it was an identity.

**The key had no state.** `session_state` is scoped to a session the handler closes when the batch
ends. `global_state` is scoped to the actor *name*, deliberately, so that the dedupe-set flagship
survives a version bump. Neither answers "what does this actor know about acme.com".

## Decision

### 1. `handle[key]` binds a virtual-object key

`crawler["acme.com"]` returns a new `ActorHandle` carrying the key, which becomes the dispatch's
`idempotency_key`. It is `__getitem__` and not a `key=` argument because the subscript reads as
*addressing* rather than *configuring*, which is what it is — the same spelling Restate and Durable
Objects use to name an object.

The handle is **copied, not mutated**. Handles live at module scope beside a workflow class, so an
in-place `a["x"]` would silently key every other workflow sharing that module.

Keys are validated at the call site — non-empty, no control characters, capped at 400 chars —
because each is spliced into a Temporal workflow id, and the failure otherwise surfaces from inside
a Nexus start, naming neither the key nor the actor.

### 2. `object_state` is the key's durable state

A fourth tier, and the scoping ladder now reads: `arun_state` (unit) ⊂ `session_state` (session) ⊂
**`object_state` (key)** ⊂ `global_state` (name). It is `global_state`'s implementation verbatim —
the same `GlobalStore` class, the same ETag CAS atomics, the same lazily-opened client, now shared
between both tiers so two tiers never mean two connections. Only the namespace differs:

    kontra-global:{actor}:{key}                 <- tier 3, actor NAME
    kontra-object:{actor}:{actor_id}:{key}      <- tier 4, actor KEY

The actor name stays in the tier-4 namespace because the key is the *caller's* string:
`beacon["acme.com"]` and `crawler["acme.com"]` are different objects and must not collide. Version
stays out for the same reason it is out of tier 3 — a frontier that reset on every deploy would be
worse than useless.

A **fourth accessor**, not a scope argument on `global_state`. The choice between "dedupe across
every target I have ever crawled" and "dedupe within this target" is a correctness decision the
author makes per call, both spellings are one line, and getting it wrong is silent in both
directions. That deserves two names.

### 3. The commit map is owned by a batch, and the reader enforces it

Making keying reachable exposed a latent hazard. The commit map — `u{i}`, what makes a retry a
replay rather than a re-run — lives in the actor-id-scoped state hash and is keyed by unit
**index**. That was safe only because every dispatch minted a fresh actor id, making "this actor
id" and "this batch" the same thing. A keyed dispatch points many batches at one id, and a stale
map then replays the *previous* batch's outputs by index and never runs the author at all. The
caller gets a full, plausible, entirely wrong result — the failure mode this repo already knows
from the 15,814-target run that reported `completed` in seven minutes.

So a batch records its owner (`run_id/node_id`) in the hash, and `run_batch` clears tiers 1 and 2
when the incoming owner differs. A new batch is a new session, which is exactly what
`session_state` is scoped to; tiers 3 and 4 live in another store and are untouched, which is the
point of keying.

**The reader enforces this, not `close()`.** Cleaning up when the session closes would be simpler
and is not sufficient: the handler's Close is best-effort (`_ = workflow.ExecuteActivity(...)` with
a bounded retry, error discarded), so a batch whose close never landed would poison the next one. A
guard on the read path cannot be skipped.

## Consequences

- **Keyed dispatches ATTACH, they do not queue.** A second dispatch to a key whose batch is still
  running joins that execution and returns *its* results — it does not run your units. Restate
  queues instead. This is right for a retry, and wrong if you meant "queue behind it"; the fix is
  to await the first dispatch. Documented on `__getitem__` because it cannot be discovered from the
  return value, which looks entirely normal.
- **`object_state` outlives everything that used to bound it.** It has the sliding TTL of the
  shared store and no owner, so a key accumulating a frontier accumulates it forever unless the
  author prunes. That is the tier's purpose and also its only footgun.
- **The Go SDK has neither half.** `sdk/go` gets no `object_state`, and a Go actor
  dispatched under a reused key still has the tier-3 commit-map hazard §3 fixes for Python. The
  identity derivation is in the shared Go handler, so keying *routes* correctly for both; it is the
  engine halves that diverge. Consistent with ADR 0021 leaving the caller's side Python-only, and
  it should not stay that way.
- **A new field appears in the operator state projection.** `batch-owner` lands in the default tier
  of `control/orchestrator/src/state.ts` `fieldInTier`, so it renders beside the `u{i}` commit markers. It
  is left there on purpose: which batch owns this state is worth seeing, not worth hiding.
- **Shared read-only handlers remain unimplemented.** Restate's concurrent `@Shared` reads have no
  peer here — every dispatch is a write batch and takes the key exclusively. Nothing needs them
  yet; the gap is recorded so the compliance claim stays honest.

## Verified

Against the live stack, four dispatches from one caller workflow — `probe["acme.com"]` twice,
`probe["other.com"]`, and one un-keyed:

```
actor-vo-probe-acme.com                            Completed   <- one backing workflow id
actor-vo-probe-acme.com                            Completed   <-   per key, reused
actor-vo-probe-other.com                           Completed
actor-vo-probe-vokey-1786546322-vo-probe-ca1c467e  Completed   <- un-keyed run/node fallback

kontra-object:vo-probe:acme.com:seen    ["a1", "a2"]                 <- accumulated across batches
kontra-object:vo-probe:other.com:seen   ["o1"]                       <- another key sees none of it
kontra-object:vo-probe:…-ca1c467e:seen  ["u1"]                       <- un-keyed, private
kontra-global:vo-probe:everseen         ["a1", "a2", "o1", "u1"]     <- tier 3 unchanged
```

`["a1", "a2"]` is the load-bearing line: it proves both that tier 4 accumulates across separate
dispatches *and* that the second batch actually ran. Without §3 it would read `["a1"]`.
