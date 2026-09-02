# 28. Emit is decoupled from the Unit, and the caller redirects it

## Status

**Accepted — ready to build.** Amends **0023** §3, §13, §15, §16, §18, and §23 to separate the
input cursor from the output dataset, move output redirection to the caller, and restore the
caller/callee symmetry that `async with` and explicit **Method** parameters achieve. Settles one
piece of vocabulary inherited by all downstream work: **Batch** survives as a distinct term from
an unnamed **Dataset**.

## Context

**0023** described a **Method** receiving a **Batch**, looping over its **Units**, and calling
`unit.emit()` to produce output. That coupling served **resume** — the framework needs "unit 7 is
finished, keep what it produced" — but **resume** is a weaker claim than "this record belongs to
unit 7". Nothing in the framework reads that link. The input position (the cursor through the
**Batch**) and the output position (offset in an append-only target) are independent save points.

The same read of the PRD, `actor-ergonomics/PRD.md`, notes a second asymmetry: the caller opens a
**Session** with `async with`, passes a **Batch** as the first **Method** argument, but cannot
pass the output target. The **Method**'s output finds a home by way of framework machinery (`@out`
decorators, materialization activities) rather than as a parameter the caller hands to a callee.
That is the shape this ADR resolves.

## Decision

### 1. Emit is decoupled from the Unit

An input cursor and an output dataset are two independent concerns.

**Input:** The position in the **Batch** — which **Unit** is current — is the save point. The
framework knows which **Unit** is being processed, so advancing the loop (the framework's re-invoke
boundary at 0023 §13) is the checkpoint. This is what 0023 §17 already records: the commit key is
(batch content hash, unit index). A checkpoint names a position, and position is input.

**Output:** Records append to an offset-addressed dataset independent of which **Unit** produced
them. The save point for output is the offset: "I have durably written N records." An author who
writes one record per **Unit**, N records per **Unit**, or one record per three **Units** all use
the same API with no framework flag to distinguish them, because the flag was only necessary when
output had to name a **Unit**.

A crash mid-batch: the input cursor says "unit 7 was the last one I processed", the output offset
says "I wrote 12 records". Resume input at 8, keep the first 12 records. The answer to "what
granularity can a Method emit at?" is the API itself — any granularity. The question disappears.

**Why:** Provenance does not require the link. Actors put the provenance they care about *inside*
the record (the input value, the hostname, the timestamp). The framework-level link coupled a
record to a Unit only to answer "which Unit failed?" when rolling back, which 0023 §13 already
handles — the Unit that fails is re-invoked with the same Batch and crashes in the same place. An
implicit link silently propagates the wrong information when **Units** share output (a Batch of N
Units producing M << N records is normal; one fails; what does "blame this output on that Unit"
mean?). An explicit link inside the record cannot propagate the wrong information because the
author controls it.

### 2. The third parameter is the caller's output dataset

A **Method** receives three things, all handed by the caller:

```python
@actor.method(takes=Pair, emits=Verdict)
async def ask(self, batch, dataset):
    async for unit in batch:
        p = unit.value
        await dataset.push(Verdict(domain=p.domain, ns=p.ns, ok=await self.probe(p)))
```

| actor param | is | caller supplies it |
|---|---|---|
| `self` | the loaded resource | once, with `async with` |
| `batch` | the work for this call | as the first argument |
| `dataset` | where results go | as the second argument |

A caller decides whether that destination has a name:

```python
async with catalog.dataset("lame").writer() as lame:
    async for batch in domains.batches(100, order_by="domain"):
        pairs,    dropped = await ns.delegation(batch)      # unnamed Batch — chains
        verdicts, dropped = await ns.ask(pairs, lame)       # named Dataset  — queryable
```

**Named and unnamed datasets in Apify's sense** — every **Method** pushes to a dataset; the caller
decides whether it has a name, and `await out.publish(...)` disappears from the caller's loop.
Every actor parameter except `self` is now something the caller hands over.

A **Batch** (the return value of a **Method** call that names no destination) is a ~110-byte ref
living in the caller's workflow history, chainable to the next **Method**. A **Dataset** is always
a named, materialized thing in the lake — there is no such thing as an unnamed **Dataset**. The
author's parameter stays spelled `dataset` in both cases, because from inside a **Method** the
destination must be indistinguishable — a surface that diverged on the caller's choice is the
defect this whole change removes.

### 3. `push` does not return an error

Records go into the dataset, and a failure surfaces at the next checkpoint or at the end of the
**Method** — the Kafka producer model, where you check the flush and not every send. This deletes
six lines of `if emitErr := unit.Emit(...); emitErr != nil` plumbing from each Go **Method**.

### 4. A Method call returns `(results, dropped)`

The tuple is the forcing function: you cannot reach `results` without naming the second value, so
a caller who does not care has to say so in a way that is visible in a diff.

```python
results, dropped = await ns.delegation(batch)
```

This replaces 0023 §15's raise-by-default and `isolate=True`. That design kept the caller deciding
("a caller who forgets to check IS the failure mode"), but now you cannot reach `results` without
explicitly binding `dropped`, which is even more visible. `dropped` is falsy when nothing was lost,
`len(dropped)` costs no fetch, and `await dropped.rows()` gets the units back for a retry.

### 5. Page width stays with the caller — §3 of 0023 is unaffected

Once output is offset-addressed, a **Method** call *could* re-page a fanned-out result itself, and
the unconditional `async for chunk in pairs.batches(size)` loop in every example would collapse to
one line. It is not taking it. The caller orchestrates its own batching, as it does everywhere
else in this model, and 0023 §3 is untouched. The framework's 200/1000 guard stays what it is —
a limit it refuses on, not a width it chooses.

The question why the loop is still there reads as boilerplate if this redesign does not address it
at the same time. Recorded as considered-and-declined to avoid a second agent wondering if it was
an oversight.

## Consequences

1. **For a call that names a dataset, the publish folds into the call — the caller's beat to inspect
   before committing is gone.** Naming the destination makes the call publish its whole returned
   **Batch** before it hands back `(results, dropped)`, so there is no point where the caller holds
   the results and decides whether to commit them. It is NOT a per-push stream to the lake, and the
   records do not bypass the ref. The author never learns the name — `entry_input` carries no dataset
   field and the **Method** body always receives an unnamed **Dataset**, which is exactly the
   invariant §2 rests on (the `dataset` parameter must read the same whether or not the caller named
   a destination, so only the caller knows the name and only the caller can publish). Publishing is
   entirely caller-side, the `publishBatch` activity over the returned ref
   (`sdk/python/actorkit/catalog.py` `_publish_to`/`_publish_batch`, `sdk/go/catalog/
   dataset.go` `publishBatch`): the path stays actor → blob → ref → the caller's workflow → publish
   activity → lake, and the named **Dataset** materializes once per **Method** call (per chunk when
   the caller re-pages), not once per push. Mid-run, a reader sees the pushed records at the
   `units/run={run_id}/…` blob level `kontra monitor --query` reads — not as rows in the named
   **Dataset**, which appear a chunk at a time only as each call returns. This is why the argument is
   optional rather than the only form — unnamed **Batches** stay in the workflow's history, and a
   caller can still hold them.

2. **Readers see a half-written batch as the normal case.** Rows appear mid-batch. The `open` /
   `sealed` / `abandoned` lifecycle in 0023 §11 already expresses this, but it becomes the normal
   case rather than the exceptional one. A reader of a live **Dataset** does not know when the
   **Method** will finish.

3. **Output from a failed Unit stays.** A **Unit** that pushes three records then raises leaves
   those three behind and is still counted as dropped. For this workload that is right —
   `examples/python/workflows/nscheck` documents this as the intended behaviour, where findings
   from failures *are* the output — but it is a behaviour change. An author who wants to discard
   output on failure must not have pushed it in the first place, which means pushing after the
   probe succeeds rather than before.

4. **The framework-level record→input link is gone for good.** Anything that wants "which input
   produced this row" puts it in the record. The return value reads what was pushed, so provenance
   must not be left implicit — the input value, the hostname, the decision, anything that matters
   must be in the record.

5. **Retries must not double-write.** The commit map already keys on (batch content hash, unit
   index) and skips committed units. The machinery exists — but it moves from nice-to-have to
   load-bearing. A host dies mid-batch, a unit is retried, and it must not re-push records
   already written. The test that kills a host mid-batch and counts rows is now essential.

6. **An out-of-loop push is identified by an EXPLICIT KEY, not by its position.** The commit map in
   §consequence 5 covers a push made against a current **Unit**; a push made with no current
   **Unit** — before the loop, after it drains, or from a task spawned under the **Units** iterator
   — belongs to no **Unit**'s commit and rides the **Batch** tail. That push is re-executed every
   time the framework re-invokes the **Method** body to isolate a failed **Unit** (0023 §13): the
   re-invoke re-runs the body from the top while `self.*`/inst fields survive and locals reset, so
   the same out-of-loop push runs again, with the same or different bytes. The framework CANNOT
   infer whether the author re-pushed a changed version of the same record or deliberately pushed a
   different one — both are the same control-flow position with different bytes. So it is TOLD, the
   principle 0023 §18 already settled for this class of problem (Restate's `ctx.run(key, …)`,
   Lambda's `batchItemFailures` by `itemIdentifier`: name the durable thing, never infer it from
   where you are in control flow):

   ```python
   await dataset.push(summary, key="batch-summary")   # ds.Push(summary, kontra.Key("batch-summary"))
   ```

   Each keyed tail push is reconciled BY THAT KEY, first-write-wins across re-invokes: a key already
   written in an earlier entry is skipped BEFORE the durable write, so results and the store agree
   with no orphan blob; a key new to this call is written and folded
   (`sdk/python/actorkit/batch.py`, `sdk/go/core/batch.go`). **The rule an author must follow: an
   out-of-loop push's key must be STABLE across re-invokes** — the same durable record carries the
   same key every time it is (re-)pushed. Its bytes may legitimately vary (the first write wins) and
   a push may drop out entirely on a later entry (its key's slot is retained, never truncated). **An
   out-of-loop push with NO key is REFUSED** — it raises `MissingPushKey` in Python and panics in Go
   (the driver's recover turns that into a whole-call error), at the call site, naming the fix. A
   generated key would infer identity from position again and be wrong the same way.

   **Why inference was abandoned.** Three mechanisms each inferred identity from content or position
   and each traded one failure for another: truncate-and-re-execute LOST a self-guarded push;
   accumulate-and-content-dedup DUPLICATED a content-varying push; (group, ordinal) first-wins
   DUPLICATED *and* LOST on a prefix shift (a push appearing only on the re-invoke drew an ordinal
   an earlier entry had filled, dropping it while shoving its unconditional successor to a fresh
   ordinal that duplicated). Neither content nor position is a knowable identity — only the author
   knows whether two pushes at one position are the same record changed or two different records — so
   the framework stops guessing. With identity keyed, the prefix shift resolves: the re-invoke-only
   push carries a key nobody has written (kept), and its successor carries a key already written
   (skipped), both surviving exactly once regardless of what the loop mutated.

## Considered and rejected

**`batch.push(...)`, one parameter.** Perfectly symmetric and free in Go, since `b.All()`,
`b.Push()`, `b.Err()` would sit together and the signature would never change. Rejected because
it makes "Batch" mean both the input units and the output records, and `CONTEXT.md` already guards
against this hazard. The glossary notes: *"catalog.Batch vs internals.Batch is one word at
opposite ends of the call."* A third meaning is where that stops being manageable. Moreover, once
output is offset-addressed a **Batch** could serve as both input and output only if they have the
same width, which is the re-paging question this change explicitly declines (§5).

**`yield` instead of a parameter.** The prettiest form in Python, and 0023 §18's commit-point
objection genuinely evaporates under this design — the save point is the input loop now, not the
yield. Rejected because an author who runs **Units** concurrently cannot `yield` from a spawned
task, and author-written concurrency is what replaced `concurrent_aruns` (0023 §18). A parameter
can be called from anywhere in the function. In Go it would also be `func(yield func(any) bool)`,
which nobody would write. Go is the peer of Python here, not a second-class citizen.

**A bare callable named `emit`.** One fewer concept than an object with a method, but it names the
act and not the destination, and it does not restore the caller/callee symmetry — the caller
cannot hand over a function. `dataset.push` says where the record goes, and `dataset` is a word
the model already has. The object form lets a caller pass different implementations (a lake writer,
a test spy, a queue); the callable form does not.

## Vocabulary decision: Batch survives beside Dataset

A **Method** call always hands its caller a **Batch** — a ref, ~110 bytes in a workflow's history
— whether or not the caller named a destination. A **Dataset** is only what a caller *names*, so
"unnamed Dataset" is not a term of this model.

Both are safe:
- A **Batch** costs ~110 bytes inside a workflow and chains easily to the next **Method**.
- A **Dataset** involves materialization to the lake and queryability.

The hazard either way: making the cheap thing look expensive (calling a named **Dataset** a
**Batch** when it is materialized) or the expensive thing look free (calling an unnamed ref a
**Dataset** when it has no persistence or queryability). The distinction matters for a caller
deciding whether to re-page a result, and for a cost model.

**Chosen: Batch survives.** An unnamed call returns a **Batch**; a named call publishes a
**Dataset**. The author's parameter stays spelled `dataset` in both cases because from inside a
**Method** the destination must be indistinguishable — the entire defect this change removes is a
surface that diverged on the caller's choice.

Why not follow 0023's precedent of **Manifest** / **Dataset** retiring the first word? Those two
cost the same to read or write — one word paid for both. These two do not: **Batch** is an order of
magnitude cheaper and reading one vs. reading the other shapes a caller's decision. The answer
lives in the caller's world, not in the framework's.

Downstream work (`02`–`09`) inherits this vocabulary choice: when they speak of "a **Batch** is
returned" they mean the cheap ref; when they speak of "the **Dataset**" they mean the named,
materialized thing. If a later slice finds this distinction unworkable, it fails at vocabulary and
the whole change fails — do not work around it with a synonym.

## Amendments to 0023

### Status block

Amend the first paragraph to add:

> **Amended 2026-08-18** by **0028** to decouple emit from the Unit, move output direction to the
> caller, and settle the Batch/Dataset vocabulary.

### §3 — "Materialization is caller-invokable"

The first half stands. The second half ("without this the model can consume datasets but never
produce one") is sharpened: it is now the caller who consumes and produces, which is how 0028 §2
inverts the flow — the author pushes to a dataset (the caller's parameter), and only the caller's
choice to name it makes it durable.

Add a note after the decision:

> Amended 2026-08-18 by 0028: the caller, not the interpreter, decides whether output is named.
> The author always pushes; the caller decides whether the destination has a name.

### §13 — "The framework isolates at the iterator boundary"

The isolation remains. The phrase "cheap and equivalent, because everything yielded so far is
already committed" is amended to "cheap and equivalent, because everything pushed so far is
already committed". Replace "yield" with "push" since 0028 §3 removes the yield/emit equivalence
(emit is now always a method call on the dataset parameter, not a side effect of the loop).

### §15 — "A Method call raises by default"

This section is superseded by 0028 §4 and §3. The raise-by-default design and `isolate=True` flag
both retire.

Replace the entire section with:

> **Amended 2026-08-18 by 0028:** A **Method** call returns `(results, dropped)`, a tuple the
> caller must destructure. You cannot reach `results` without binding `dropped`, which makes the
> choice visible in review. Raises-by-default retires. The three-parameter shape (self, batch,
> dataset) moves output redirection to the caller.

### §16 — "Composition is the author's"

The composition within a **Method** remains unchanged (function calls, no graph). The phrase "zero
history, nothing to design" is sharpened: no history because only the **Method** call itself is
durable, and composition is author-written function calls, not a declared topology.

### §18 — "Emit is a call, and it names its Unit"

This section is amended to describe the new shape. Replace with:

> **Amended 2026-08-18 by 0028:** Emit is decoupled from the Unit. The **Method** receives a
> dataset parameter (the caller's third argument) and pushes records via `await dataset.push(x)` /
> `dataset.Push(x)`. The framework's save point is the input cursor (which **Unit** was last
> processed) and the output offset (how many records were written). An author is free to emit zero,
> one, or many records per **Unit**; the question "what granularity does emit support?" disappears
> because output no longer names a **Unit**. Provenance is author-supplied inside each record, not
> implicit in the framework.

### §23 — "Go gets the Batch too"

The per-unit callback shape (`fn(*Session, Unit)`) retires because emit no longer names a Unit.
Replace with:

> **Amended 2026-08-18 by 0028:** Go receives the same three-parameter shape as Python:
> `func(s *Session, b *Batch, ds *Dataset) error`. The dataset parameter is the caller's; emit
> becomes `ds.Push(x)`. Author-written concurrency is supported because push is a method call on
> a parameter, not a side effect of the loop.

---

**What 0023 §3 means to downstream work:** Once output is offset-addressed and decoupled from
input (0028 §1), re-paging on the framework's side *could* happen inside a **Method** call
without the caller re-looping. It is not happening. The caller still sizes its own input pages
(0023 §3 unchanged), orchestrates batching, and chooses when to loop. The framework's 200/1000
guard is a ceiling it refuses at, not a width it chooses.

