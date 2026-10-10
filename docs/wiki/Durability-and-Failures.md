# Durability & Failures

The checkpoint is two independent save points (ADR 0028 §1): the **input cursor** (which **Unit**
was last processed) and the **output offset** (how many records were pushed to the caller's
**Dataset**). The resume story below is told against both.

**Required author error-handling is zero.** The naive loop with no `try` does the right thing:
one bad Unit cannot sink its siblings, and nothing the framework decides fails your Batch.

What this page is really about is the one question a v2 author has to answer for themselves:
**your loop is ordinary code — so what in it survives a death, and what does not?**

## The short answer

```python
@actor.method(takes=Target, emits=Page)
async def crawl(self, batch, dataset):
    seen = set()                     # ✗ a LOCAL — resets when the Method is re-entered
    self.opened = self.opened or 0   # ✓ the SESSION — survives re-entry, dies with the scope
    async for unit in batch:
        n = await self.global_state.incr("pages_total")   # ✓ DURABLE — survives everything
        await dataset.push(Page(url=unit.value.url, n=n))
```

| Where you put it | Survives a re-entry (§13) | Survives host death | Shared across Sessions |
|---|---|---|---|
| a local (`seen = set()`) | **no** | no | no |
| `self.*` | **yes** | no — the scope fails (§7) | no |
| `self.unit_state` | yes, for THAT Unit | yes, on the activity retry | no |
| `self.object_state` | yes | yes | yes, per dispatch **key** |
| `self.global_state` | yes | yes | yes, per actor **name** |

The rule in one sentence: **durable means keyed, in-memory means scoped.**

## Why a local resets — the trap worth knowing

**A Method is entered several times per Batch**, once per isolated Unit failure
([ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §13). When a Unit raises, the
framework records it as a failure and **re-invokes your Method with the remaining Units** —
because everything you pushed before the raise is already committed, so replaying it would be a
second execution of finished work.

That re-invocation is an ordinary function call. Your locals are built again from scratch:

```python
async def crawl(self, batch, dataset):
    count = 0                   # rebuilt on EVERY entry
    async for unit in batch:
        count += 1              # counts units since the last failure, not units in the Batch
```

With one isolated Unit in a 100-Unit Batch, `count` ends at whatever followed the failure. It
never raises and the number looks plausible, which is what makes it worth stating. Anything you
accumulate **across** Units belongs on `self.*` or in a durable tier.

## Counters and sets that mean something

`self.*` is enough when the count is a fact about *this scope* and you read it before the scope
ends. When the count must survive the scope, or be right across a fleet of workers doing the same
job, use the atomic ops — they are ETag compare-and-set, so concurrent Sessions never lose an
update:

```python
n     = await self.global_state.incr("pages_total")          # atomic counter -> the new value
fresh = await self.global_state.add_to_set("seen", url)      # True = newly added (the dedupe flagship)
```

```go
n, _     := s.GlobalState().Incr("pages_total", 1)
fresh, _ := s.GlobalState().AddToSet("seen", url)
```

**Reach for the atomics, not `get` + your own `set`.** A read-modify-write from the client is
exactly the lost-update race they exist to prevent, and plain `get`/`set` is last-write-wins
across Sessions — it will silently undercount under any real concurrency. See [[Data-Plane]] for
the key scheme and the Lua CAS both SDKs share. `python/probe` runs both in a Method.

**Neither a counter value nor an `add_to_set` result belongs in a pushed record.** A record is
addressed by the sha of its own bytes, so a re-push after a death must produce the *same* bytes to
overwrite itself. `incr` returns a bigger number the second time and `add_to_set` returns `False`,
so either one in a field forks the record into a duplicate blob. Durable counters are accounting;
they are not output.

`object_state` is the same operations scoped to the dispatch **key** rather than the actor name
(`crawler["acme.com"]`), which is what makes a keyed Actor a virtual object: its dedupe set is
still there on the next dispatch, next week. That longevity is a trap of its own — a genuine
re-run of a sweep whose Method opens by checking a keyed dedupe set will do nothing and finish
in seconds, correctly. `CONTEXT.md` carries the worked example.

## `unit_state` — resume scratch for a FAT Unit

Per-Unit exactly-once is too coarse when one Unit is minutes of incremental work (a deep crawl,
a long export): a death re-runs the whole thing. `unit_state` is that Unit's own keyed scratch.

```python
frontier = await self.unit_state.get("frontier")   # None on a fresh Unit; the last snapshot after a death
await self.unit_state.set("frontier", frontier)
```

**Who reads it** is worth being precise about, because ADR 0023 §19 got this wrong once and
retired the tier before the amendment put it back: the reader is a Unit that was **in flight when
the activity died** and re-runs on the handler's retry, against the same session queue and the
same Batch hash — hence the same slot. A **committed** Unit is folded back from its commit object
and an **isolated** Unit is never resumed, so neither of those reads it.

It is resume scratch, **not a result store** — output goes through `await dataset.push(x)`, never
here. All of a Unit's keys live in one blob (`{slot}-ckpt`) and the framework deletes the whole
blob when the Unit commits, so a finished Unit never resumes.

**Gate a resume on `self.emit_durable`.** In the store-less mode (an in-process test; `serve()` no
longer starts a committing actor without S3) a record is not durable until its Unit commits, so
skipping already-pushed work on a resume would silently drop it.
`python/crawl4ai` does exactly this, and its DEFAULT tier deliberately leaves
`unit_state` unwired: re-crawling a whole seed is safe because every page is keyed by content
sha, so a re-push overwrites idempotently.

## The failure classes

The discriminator is: *is the resource `@actor.load` opened still alive?* — answered by
`@actor.healthcheck`.

| Failure | Resource alive? | What happens | What you write |
|---|---|---|---|
| **bad input** (dead URL, missing field) | yes | the Unit is **isolated** into `failures`; siblings finish, the Method is re-invoked with the remainder | nothing (or `raise NonRetryableError` to mark it terminal) |
| **the payload does not fit `takes=`** | yes | same — the coercion raises at the iterator boundary, so it isolates that Unit alone | nothing |
| **resource death** (the browser crashed) | **no** | the **Session ENDS** and the caller's scope raises | `@actor.healthcheck` reports it dead (or raise `SessionLost`) |
| **host death** | — | the scope's calls sit until `ScheduleToStart` fires, then the scope raises | nothing |

### A dead resource ends the Session — it does not reload

This reversed in v2 and it is the most important line on this page. `@actor.healthcheck` used to
mean *"reload me"*, and the framework rebuilt the resource in place — which silently reset
`self.*` underneath a Method that kept running. That is precisely the failure shape §7 rejects
for host loss, so §20 gave it the same answer: **the Session ends, and the caller reopens.**

Barely more expensive (a reload re-runs `@actor.load` anyway), and the gain is that a Session's
promise has no third case: while it lives, `self.*` is coherent — it either survives or you get
an exception. Nothing silently continues against a corpse.

A reopen is a **new activity execution**, so it has no heartbeat to resume from. On the same
instance — a scope keyed `actor["acme.com"]`, reopened on that key — it **lists** the Batch's commit
objects and folds back what the lost scope finished, then runs only the rest (ADR 0060, owner decision
A7). An unkeyed scope reopens as a new Session id, which is a new instance, so it re-runs the Batch;
records re-push by content sha into the same blobs, and only the Batch the successful call returns is
published. The **poison** counter survives the reopen too: a Unit that has killed N scopes is recorded
as a failure and skipped (§21) — without that, §20 plus §17 is a tight loop.

## Exactly-once, honestly

**Execution is at-least-once; result recording is exactly-once.** A death between your work and
its commit re-runs that Unit, which is why Method bodies must tolerate replay. External side
effects inside a Method (a POST, a charge) are the author's idempotency problem — the framework
deduplicates *results*, not *actions*.

A committed Unit is keyed by the **Batch's content hash plus its index** (§17), not by a sequence
number: the hash is the same on every attempt and every execution of one Batch, so a retry or a
re-dispatch finds what was already finished, and two Batches under one Session cannot read each
other's slots.

### Where the record of what finished lives

**In the activity's heartbeat, and in the object store — not in Redis** (ADR 0060). It used to be a
commit map in the actor's Redis hash under a 24 h TTL, and until ADR 0059 the store ran
`maxmemory-policy volatile-lru` — which evicts keys *that have a TTL*, so under memory pressure the
first thing dropped was the record of what had committed. A retry then re-ran finished work or skipped
unfinished work, and nothing raised.

Every beat carries a **checkpoint** saying WHICH Units finished:

```json
{"v":1,"batch_id":"b1","done":[[0,1]],"failed":[4],"manifest_ref":"commits/run=r/actor=crawl/actor_id=acme.com/batch=b1/"}
```

`done` is a **range set** — merged inclusive `[lo, hi]` pairs — because a heartbeat payload is bounded
and a per-unit list of 10,000 integers would be a batch-size ceiling in disguise. `batch_id` is the
Batch's content hash and it is a **guard**: unit indices are positions within one batch, so a reader
discards a checkpoint whose id does not match rather than applying it by index to units it never saw.
A version the reader does not know is discarded whole for the same reason. The encoding is pinned
across both SDKs and the orchestrator by `shared/conformance/checkpoint.json`.

What each finished Unit PRODUCED does not fit in a heartbeat, so it is one **commit object** under the
prefix `manifest_ref` names, written *before* the beat that reports it (`shared/conformance/commit.json`).
The prefix is keyed by the run, the actor, the **instance** (`actor_id`: the idempotency key, else the
Session, else run and node joined) and the Batch's content hash — not by the dispatch, so every attempt
and every execution of one Batch on one instance writes to one place.

Temporal hands the last beat to the next attempt; the engine folds the finished Units back from their
objects and hands the Method only the rest. A **new execution** has no beat to read, so it **lists** the
Batch's prefix instead and folds back what an earlier execution finished. A Unit the checkpoint (or the
listing) calls finished whose object is missing or does not decode fails the attempt as **`CommitLost`**,
non-retryable — that is the store having lost something, and neither re-running it nor folding it as
empty would say so.

**What this covers, and what it does not:**

- **Within one execution, across attempts** — a worker killed under an unscoped dispatch (the retry
  lands on another worker of the shared queue), a dead resource on Go — a retry resumes from the
  heartbeat. A scoped call's queue is polled only by the worker that died, so its scope raises instead
  (§7) and the caller reopens.
- **Across executions, on the same instance and run** — a re-dispatch on the same idempotency key, a
  reopened keyed scope, a retried workflow — the new execution resumes from the listing (owner decision
  A7). An identical call made again on purpose is the same Batch and replays, as `batch_id` has always
  said.
- **Across instances or runs, nothing resumes.** Another key's commits are not this key's, and a Unit's
  output is refs into its own run's `units/` prefix, so another run folding them would return rows its
  Dataset does not hold.
- **A hard kill re-runs up to one heartbeat throttle window** of Units within an execution: their
  objects were written but the beat naming them was never sent. They overwrite their objects and
  re-push by content sha.
- **S3 is mandatory for an actor that commits** (owner decision A8): `serve()` refuses to start one
  with `KONTRA_S3_ENDPOINT` unset, because nothing else holds a finished Unit's output.

## What a commit holds

With `KONTRA_S3_ENDPOINT` set, each pushed record is written to the object store **at push
time** under the hive key `units/run={run}/dt={date}/actor={actor}/shard={n}/unit={i}/{sha}.json`,
and the Unit's commit object holds only `{"$ref": {key, size, sha256}}`. Blob write **first**, then
the commit object, then the beat — so a committed ref always points at written bytes, and a beat
always names a Unit whose commit object exists. Redis never holds payloads, or commits.

The record's sha is its identity, so re-pushing the same record on a resume is an idempotent
overwrite — which is why records must be **content-deterministic** (no timestamps, no random ids).
Store unset ⇒ `serve()` refuses a committing actor; only an in-process engine test runs without one,
with records inline in the result and nothing durable committed. See [[Data-Plane]].

## The image a Placement is pinned to

A Placement resolves to `<repo>@<digest>` and pins **that digest**, not a tag — so what keeps a running
Fleet runnable is the digest still being in the registry. The registry now has retention, which means it
is now possible for something to delete it.

**"Keep the 5 most recently pushed" is unsafe on its own.** A version older than those five that is
still placed on a Machine is exactly the case that breaks: the Machine restarts a Worker, pulls by
digest, and the digest is gone. Rebuilding does not recover it — a rebuild yields a *new* digest, so a
Fleet recorded against the old one can never be re-run as recorded.

What makes it safe is an **`inuse-` tag**: retention keeps every tag matching `^inuse-` regardless of
age, so a tag is how the control plane says *not this one* to a garbage collector that runs inside zot
with no callback and no way to ask a question. The tag namespace is the only vocabulary the two share.

**A reconciler writes them.** It lives in the orchestrator, is armed in the **API** role on every
start (the materializer has no periodic loop to join), runs one pass immediately and then every
10 minutes, and is disarmed only by `KONTRA_INUSE_TAGS=off`. The tag is `inuse-` plus the first 12 hex
characters of the digest, written on the **bare repository name** (`webcrawl:inuse-…`) — which is what
the shipped `kontra deploy` pushes to. The policy table names both that and the `actors/<name>` shape
the buildpack path will create, so neither is left unprotected.

**It covers one of the three things retention needs it to.** The tag is written for the digest the
catalog currently records for each actor. A digest that is *placed on a Machine*, and a digest a *run
still inside its retention window* references, are **not** tagged — neither is readable: placements
live in Pulumi stack state rather than a table, and an enrolled Fleet's assignments are
operator-authored files that no code path writes.

That gap is narrower than it reads, because a Placement resolves its digest *from the catalog*. So the
dangerous case is not "placed but untagged", it is a catalog entry whose **tag has since moved**: the
migration of this install found `desync@177f80c8` and `webcrawl@e09d6df4` recorded in the catalog,
present in the store, and reachable by no tag at all. Those are exactly the digests the reconciler
protects.

**Removing a tag is best effort, and the asymmetry is deliberate.** Without a registry credential the
store permits read, create and update but not delete, so a stale `inuse-` tag can outlive its reason.
The consequence is retention keeping more than it must — recoverable. Deleting the image a running
actor was placed from is not.

> [!NOTE]
> **A running install may hold no `inuse-` tags yet.** The loop arms at process start, so an install
> whose `kontra-api` container predates the reconciler has never run a pass however long it has been
> up. Ask the registry rather than the code — `curl -s http://127.0.0.1:5000/v2/<actor>/tags/list`
> should show an `inuse-` tag beside the version tags — and recreate the container if it does not.
> Then read a `dryrun` pass before setting `KONTRA_REGISTRY_RETENTION=enforce`. [[Deployment]] §2a has
> the policy table.

## Close, determinism, replay

`@actor.close` runs on every exit path of an unscoped dispatch — including a failure, which is
exactly when a loaded resource must not leak. Inside a caller's `async with`, the close belongs
to the scope: the handler does not tear the resource down between two Method calls of one
Session.

The handler workflow is deterministic Temporal code, so a workflow-worker restart replays from
history and continues. The actor's resume is the activity heartbeat plus its commit objects (ADR
0060), which reverses [legacy ADR 0018](../adr/legacy/0018-temporal-native-actor-runtime.md) §4. Its
three objections still describe the edges: details are **throttled and dropped on a hard kill** (so
up to one window of Units re-runs — work redone, not lost), they survive **attempts rather than
executions** (so a re-dispatch resumes from a listing of the commit objects instead, owner decision
A7), and `RecordHeartbeat` cannot carry author-sized state — which is why the checkpoint is a range set
and a pointer, and `unit_state` stays in Redis for now.
