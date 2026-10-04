# Writing Temporal Workflows Here

**Read this before you write or restructure a workflow in `workspaces/*/workflows/`.**

kontra's workflows are plain [Temporal](https://docs.temporal.io/develop/python) workflows. There is
no kontra workflow framework, no phase library, no decorator of ours beside `@workflow.defn`. When
you need structure, reach for **Temporal's own primitive** — not a Python one.

This file exists because the opposite keeps happening: a body gets long, and the instinct is to add
helper methods, a context dataclass, a `plan()` function. None of those are a history boundary or a
retry boundary, so none of them buy anything Temporal understands. They just move lines.

---

## The rule

> **A workflow is a unit of history and retry — not a unit of code.**
> Split along those two axes and nothing else.

If two things need different retry policies, different timeouts, or independent resumability, they
are two workflows. If they don't, they are one body, written inline, top to bottom.

---

## Decomposition, in the order you should reach for it

**1. Inline.** The default. `@workflow.run` reads as the orchestration: each `await` is a durable
step and you read them in order. A long body is not automatically a problem.

**2. A child workflow** — `workflow.execute_child_workflow(Cls.run, arg, id=…)`. Reach for this when
a part needs its *own* retry policy, timeout, or resumability, or when its history would otherwise
bloat the parent's. Each child gets its own event history, its own row in the UI, and
`continue_as_new` available to it.

**3. `continue_as_new`** — for an unbounded loop. Page N batches, then continue carrying the cursor.
History stays flat regardless of corpus size.

**What NOT to do:** turn a paging loop into one activity. An activity is retried *wholesale*, so a
crash re-runs the loop from the first page — which in this repo means **re-sending attack traffic to
hosts already probed**. The workflow body is already durable; that is the point of it.

**Also not:** helper methods on the workflow class that take everything as parameters and touch no
`self`. Those are module-level functions wearing `self` as a costume. A pure helper (`_split_url`,
a SQL builder) belongs at module level; the class holds `@workflow.run` plus any
`@workflow.signal` / `@workflow.query` / `@workflow.update` handlers and the state *they* mutate.

---

## Per-phase timeout and retry

Declare policy **with the phase**, not at the call site, using Temporal's own types:

```python
@workflow.defn
class Sweep:
    """Per-class smuggling over the hosts the screen moved on. The long phase."""

    EXECUTION_TIMEOUT = timedelta(hours=6)   # whole phase: retries + every continue_as_new
    RUN_TIMEOUT       = timedelta(hours=1)   # one continue_as_new segment
    RETRY = RetryPolicy(maximum_attempts=1)  # re-sending attack traffic IS the cost
```

The layering, which is the thing most often got wrong:

| knob | bounds |
|---|---|
| `execution_timeout` (child) | the whole phase, retries and `continue_as_new` included |
| `run_timeout` (child) | one `continue_as_new` segment |
| `task_timeout` (child) | one workflow task — leave at the default |
| `schedule_to_close_timeout` (activity) | one Batch dispatch |
| `heartbeat_timeout` | liveness *inside* a Batch |

One timeout shared across phases has to fit the slowest, so the fast phase carries a useless ceiling
and a wedged one takes that long to notice. `hunt` records the measurement: the screen finishes in
43 seconds, the sweep runs 21 minutes, and both sat under one 90-minute bound.

`parent_close_policy` defaults to **`TERMINATE`** — correct for phases, and worth knowing.

---

## Capacity: who holds the Fleet

> **The outermost workflow holds the Lease. Children `attach` and hold nothing.**

This works because **the Lease governs lifetime, billing and teardown — not routability.** A Method's
queue is `f"{name}-{version}"`, a pure function of the Actor's identity, so a child holding no Lease
still dispatches onto Workers the parent provisioned. `fleet.py` says it directly: *"ready() and
catalog.actor() need none of your own."*

```python
async with fleet.hold(tag=..., machines=n) as f:
    await f.place(*ACTOR, sessions=s)
    await f.ready()                                  # BEFORE any child is spawned
    out = await workflow.execute_child_workflow(
        Phase.run, {**arg, "attach": True}, id=f"{workflow.info().workflow_id}-phase")
    totals = await read_totals(...)                  # still inside the scope
```

Two constraints that are not style:

- **`ready()` before spawning.** Dispatch into an unserved queue does not raise — the Batch waits for
  `ScheduleToStart`, so the run hangs before its first probe.
- **Do not leave the `async with` while children still need the Fleet.** Dataset pages need a serving
  Actor to dereference; after the scope exits the same read hangs.

**If you write an attached path, gate it:**

```python
await fleet.serving(*ACTOR)        # raises NotServing, naming the actor
```

`attach` is kontra's, not Temporal's — a plain boolean on the input meaning "somebody else
provisioned capacity." It is an assertion, and `fleet.serving()` is what verifies it.

---

## Registration

`catalog.serve()` must register **every workflow type this worker may be asked to run**, children
included — that is Temporal's requirement, not ours. An unregistered child fails at dispatch, late
and far from the edit that caused it.

```python
if __name__ == "__main__":
    catalog.serve()              # every @workflow.defn bound in this module
    catalog.serve([Campaign])    # or name them, to serve less than you imported
```

Discovery reads the module **namespace**, not the definition site — `campaign/workflow.py` *imports*
`Surface` and `Hunt`, so a "defined here" filter would drop exactly the types it starts.
Over-registering costs nothing; under-registering hangs a run.

The manifest's `workflow` field names the **default** class to start. A folder may declare several;
the HTTP start API takes a `type` to pick one, so a phase can be re-run on its own from the console.
The task queue is `wf-<name>-<version>` off the manifest — adding a class does not change it.

---

## Determinism, briefly

- A workflow body must replay to the same command sequence. No wall-clock, no `random`, no I/O —
  `workflow.now()`, `workflow.uuid4()`, activities.
- **Child workflow ids must be deterministic.** Derive from `workflow.info().workflow_id`.
- Pass context as a **frozen** dataclass if you pass one at all; a mutable bag threaded through
  phases is the easiest way to lose replay, and `frozen=True` turns that into a `TypeError` at the
  moment it is written.
- **Never `raise` a bare exception for bad input.** That is a workflow *task* failure, retried
  forever, and the run wedges with nothing to read. Use
  `ApplicationError(msg, type="BadThing", non_retryable=True)`, and pair it with
  `non_retryable_error_types` on the phase's `RetryPolicy`.

---

**See also:** `docs/wiki/Writing-Workflows.md` (the long-form guide), `CONTEXT.md` (Execution ↔ Fleet),
`docs/adr/0037` (capacity and dispatch are two verbs).
