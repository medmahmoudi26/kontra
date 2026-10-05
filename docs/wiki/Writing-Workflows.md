# Writing Workflows

An Actor is the work. A **workflow** is what decides *which* work, *in what order*, and *on what
capacity*. Since ADR 0023 §12 deleted the graph interpreter, your workflow is the **only**
dispatcher — there is no `dispatch` verb anywhere in the CLI or the MCP server.

It is a [Temporal](https://temporal.io) workflow, written in Python, that you serve and start
yourself. That buys the thing a shell script cannot give you: **a script that dies leaves the
Machines standing; a workflow that dies runs its scope exits and the Machines go away.**

---

## The directory

```
workflows/canary/
  workflow.json      the manifest
  workflow.py        the code
```

### `workflow.json`

```json
{
  "entry": "workflow.py",
  "name": "canary",
  "version": "1.0.0",
  "workflow": "Canary"
}
```

| field | meaning |
|---|---|
| `entry` | the file to import |
| `name` | the workflow's name in the catalog and the console |
| `version` | bump it when the contract changes |
| `workflow` | the `@workflow.defn` **class name** inside `entry` |

> The task queue is **derived from the folder's content**, never typed. There is no `--queue` flag
> and no queue field here — an edit that does not change the manifest does not move the queue.

---

## The skeleton

```python
from datetime import timedelta

from pydantic import Field
from temporalio import workflow
from typing_extensions import Annotated, TypedDict

from kontra import catalog, fleet, progress
from kontra.fleet import docker_fleet, do_fleet

#: The Actor this run places and calls. Pinned, so the run resolves against something built.
ACTOR = ("canary", "1.1.1")


class CanaryInput(TypedDict, total=False):
    """Every field carries a default and a description, and that is the whole point."""

    targets: Annotated[list[str], Field(
        default=["alpha", "beta"],
        description="What to sweep. One target is one Unit — the unit of failure and the "
                    "unit of resumption.")]
    machines: Annotated[int, Field(
        default=1,
        description="How many Machines the Fleet has.")]


@workflow.defn
class Canary:
    """Provisions a Fleet, sweeps on it, and lets you watch every part of it happen.

    The first paragraph of this docstring is the workflow's description, everywhere.
    """

    @workflow.run
    async def run(self, req: CanaryInput | None = None) -> dict:
        req = req or {}
        ...
```

### Four things in that skeleton are load-bearing

**1. `Annotated[..., Field(default=…, description=…)]`, not a bare type.**
`catalog.py::workflow_descriptor` derives the input shape with `TypeAdapter(tp).json_schema()`. A
bare `targets: list[str]` reaches the console as `{"title": "Targets", "type": "array"}` — a label
and nothing else, which renders as a column of empty boxes with no help and no pre-filled values.
A `#` comment cannot help you here: **comments do not exist at runtime.**

**2. The *class* docstring is the description.** It is derived with `first_paragraph(cls.__doc__)`
and published on the descriptor, so it is what the console's launch form prints above the fields and
what the Workflows page shows beside the name. A `@workflow.defn` class with no docstring reaches
every reader as a bare type name.

Use `run`'s docstring for notes to whoever edits the file — not for whoever is deciding to press Run.

**3. `req` must default.** `kontra workflow start` with no `--input` passes **no argument at all**,
and a required parameter makes that a workflow-task failure:

```
TypeError: Canary.run() missing 1 required positional argument: 'req'
```

`Field(default=...)` populates the console's *form*; it does not make the argument optional at the
call boundary. Hence `req: CanaryInput | None = None` and `req = req or {}`.

**4. `total=False` on the TypedDict**, so a partial input is valid.

---

## Output: a Dataset

```python
out = catalog.dataset("canary_signals")
```

That handle is the third parameter every Method call takes. Rows are readable **while the Run is
still open** — you do not wait for the run to finish to query it.

At the end, say which ending it was:

```python
try:
    await out.seal() if complete else await out.abandon()
except Exception as err:  # noqa: BLE001
    workflow.logger.warning("could not close the dataset: %s", err)
```

> `sealed` and `abandoned` are different claims — see [[Glossary]]. Leaving a Dataset `open` for
> ever is what made every dataset on an install exempt from retention.
>
> **One marker covers a whole NAME.** Two overlapping Runs writing the same Dataset name share one
> marker, so sealing because *this* run finished can declare a Dataset whole while another is still
> appending.

---

## Capacity: the Fleet scope

```python
async with fleet.hold(docker_fleet(machines=machines), tag=f"canary-{workflow.info().workflow_id}") as f:
    await f.place(ACTOR[0], ACTOR[1], sessions=sessions)
    await f.ready()
    ...
```

**The scope is the Fleet's lifetime.** Exiting it drops the Lease, and the Machines die when the
last Lease goes. It is a replayable step in a durable program rather than a line in a script that
might not run.

`fleet.hold(...)` takes a provider — `docker_fleet(...)` for Warden containers on the Compose
network (no credential needed), `do_fleet(...)` for DigitalOcean.

### Two mistakes this scope invites

**`await f.ready()` is not optional.** `place()` returns while systemd (or the container) is still
starting. A Batch dispatched into that gap waits on a queue nobody is serving, which is
indistinguishable from a hung run.

**Give each Run its own `tag`.** A placement is the *whole* Fleet's desired state, so if two Runs
share a tag the second one's `place()` is refused — and a plain `RuntimeError` raised in workflow
code is retried as a workflow **task** for ever. Measured on the canary: 301 seconds at `RUNNING`
with an empty run page, against 69 seconds for the Run that won.

> Skipping `place()` when the Fleet looks shared is **not** the fix: `shared` means another Run
> holds a *Lease*, not that it has *placed*. During a race neither has, and `ready()` then fails
> with "0 Machine(s) with nothing on them."

### Running on somebody else's Fleet: `attach`

**The Lease governs lifetime, billing and teardown — not routability.** A Method's queue is
`<actor>-<version>`, a pure function of the Actor's identity, so a Run holding no Lease still
dispatches onto Workers another Run provisioned. That is what `attach` means, and it is how a
parent workflow can own capacity while its children do the work:

```python
async with fleet.hold(tag=..., machines=n) as f:
    await f.place(*ACTOR, sessions=s)
    await f.ready()                                      # BEFORE any child is spawned
    await workflow.execute_child_workflow(
        Phase.run, {**arg, "attach": True}, id=f"{workflow.info().workflow_id}-phase")
    totals = await read_totals(...)                      # still inside the scope
```

Two constraints that are not style. **`ready()` before spawning** — a dispatch into an unserved
queue does not raise, the Batch waits for `ScheduleToStart` and the run hangs before its first
probe. And **do not leave the `async with` while children still need the Fleet** — a Dataset page
needs a serving Actor to dereference, so the same read hangs after the scope exits.

**If you write an attached path, gate it.** `attach` is an assertion that capacity exists; this is
what verifies it:

```python
await fleet.serving(*ACTOR)        # raises NotServing, naming the actor, in seconds
```

A provisioning run gets `f.ready()`. An attached run makes the *stronger* claim — that capacity it
cannot see is already up — so it is the one that most needs the check.

---

## Calling a Method

```python
async with catalog.actor(*ACTOR) as c:
    rows, dropped = await c.sweep(
        units, out,
        params={"every": every, "fail_on": fail_on},
        schedule_to_close_timeout=timedelta(minutes=10),
    )
```

- **First argument**: the Batch. A plain `list` of dicts works — `_iter_chunks` accepts
  `Iterable[Any] | Batch`.
- **Second argument**: the output `Dataset`.
- **`params=`**: the Method's own knobs, passed straight through.
- **Returns** `(rows, dropped)` — a Batch out and the Units that did not make it.

Everything else is Temporal activity options (`schedule_to_close_timeout`,
`heartbeat_timeout`, …).

### Paging a large input

The caller owns Batches. There is no sharding knob on the wire — the paging is your loop:

```python
async for batch in catalog.dataset("exchanges").batches(100, order_by="url"):
    rows, dropped = await c.scan(batch, out, params={"tier": 1})
```

---

## When one workflow becomes several

> **A workflow is a unit of history and retry — not a unit of code.**

Split along those two axes and nothing else. Two things that need different retry policies, different
timeouts, or independent resumability are two workflows. Two things that don't are one body.

Reach for these **in order**:

**1. Inline.** The default, and a long body is not automatically a problem. `@workflow.run` reads as
the orchestration: every `await` is a durable step and you read them top to bottom.

**2. A child workflow**, when a part needs its own retry policy, timeout or resumability — or when
its history would otherwise bloat the parent's. It gets its own event history, its own row in the
UI, and `continue_as_new` available to it.

```python
@workflow.defn
class Sweep:
    """Per-class smuggling over the hosts the screen moved on. The long phase."""

    # Policy lives WITH the phase, not at the call site, in Temporal's own types.
    EXECUTION_TIMEOUT = timedelta(hours=6)   # whole phase: retries + every continue_as_new
    RUN_TIMEOUT       = timedelta(hours=1)   # one continue_as_new segment
    RETRY = RetryPolicy(maximum_attempts=1)  # re-sending attack traffic IS the cost

    @workflow.run
    async def run(self, req: SweepInput) -> PhaseResult: ...
```

```python
out = await workflow.execute_child_workflow(
    Sweep.run, arg,
    id=f"{workflow.info().workflow_id}-sweep",     # MUST be deterministic, or replay diverges
    execution_timeout=Sweep.EXECUTION_TIMEOUT,
    retry_policy=Sweep.RETRY,
)
```

Register every class the worker may be asked to run — `catalog.serve()` with no list discovers
them, including ones you imported (see below). `parent_close_policy` defaults to `TERMINATE`, which
is what you want for a phase.

**3. `continue_as_new`**, for a loop with no fixed end. Page N batches, then continue carrying the
cursor; the history stays flat regardless of corpus size.

```python
@workflow.run
async def run(self, req: SweepInput) -> PhaseResult:
    cursor, total = req.get("cursor", 0), req.get("total", 0)
    async with catalog.actor(*DESYNC) as d:
        for _ in range(PAGES_PER_RUN):                 # a bounded segment
            page = await self._next(cursor)
            if page is None:
                return PhaseResult(observations=total)
            obs, _ = await d.smuggle(page, out, params=req["params"], **opts)
            cursor += 1
            total += len(obs)
    workflow.continue_as_new({**req, "cursor": cursor, "total": total})
```

### What not to do

**Never turn a paging loop into an activity.** An activity is retried *wholesale*, so a crash
re-runs the loop from page one — which here means re-sending attack traffic to hosts already
probed. The workflow body is already durable; that is the point of it.

**Helper methods are not decomposition.** A method on the workflow class that takes everything as
parameters and touches no `self` is a module-level function wearing `self` as a costume. It is
neither a history boundary nor a retry boundary, so it buys nothing Temporal understands. Pure
helpers belong at module level; the class holds `@workflow.run` plus any `@workflow.signal` /
`@workflow.query` / `@workflow.update` handlers and the state *they* mutate.

### The timeouts, layered

| knob | bounds |
|---|---|
| `execution_timeout` (child) | the whole phase, retries and `continue_as_new` included |
| `run_timeout` (child) | one `continue_as_new` segment |
| `task_timeout` (child) | one workflow task — leave at the default |
| `schedule_to_close_timeout` (Method call) | one Batch dispatch |
| `heartbeat_timeout` | liveness *inside* a Batch |

One timeout shared across phases has to fit the slowest, so the fast phase carries a useless ceiling
and a wedged one takes that long to notice.

---

## Making the run watchable

A run that says nothing for ninety seconds is indistinguishable from a run that is stuck.

```python
# The denominator FIRST, before anything happens.
workflow.logger.info(
    "canary: %d target(s) x %d step(s) = %d record(s)", len(targets), steps, total)

# And as a FACT, not only as a sentence.
progress("sweep", "records", total=total, program="canary")
```

`progress(...)` rides the log record's `extra=`, which the formatter turns into indexed
VictoriaLogs fields — so the console reads `done`/`total` as **numbers** instead of parsing them
back out of prose, and they are still there tomorrow because the log store outlives the execution.

Name the handover, too. Every line between `place()` and "sweep finished" comes from the *Actor*, on
a Machine, carrying that Worker's identity — so a reader who sees the rail go quiet knows which
process went quiet.

---

## Serving and starting

At the bottom of the file:

```python
if __name__ == "__main__":
    catalog.serve()              # every @workflow.defn bound in this module
    catalog.serve([Canary])      # or name them, to serve less than you imported
```

Temporal needs **every type this worker may be asked to run** registered up front — children
included — so a phase split into its own class has to reach this call. Discovery reads the module
namespace rather than the definition site, because a parent typically *imports* the children it
starts and a "defined here" filter would drop exactly those. Over-registering costs nothing;
under-registering hangs a run at dispatch.

```sh
# Run the worker here. --watch re-registers the contract on every save.
kontra workflow serve workspaces/default/workflows/canary --watch

# Start a run.
kontra workflow start workspaces/default/workflows/canary --id my-run-1
kontra workflow start workspaces/default/workflows/canary --input '{"machines": 2}' --wait
```

Stopping one:

```sh
kontra workflow cancel <run-id>      # graceful: scope exits run, so the Fleet is DESTROYED
kontra workflow terminate <run-id>   # skips scope exits — a Fleet it held would keep billing
```

**Always prefer `cancel`.**

---

## Running it yourself: the client library

`catalog.serve()` is right for a process whose **job** is to serve. It blocks forever, which is
exactly what a test, a notebook or an embedding process cannot use. For everything else, take the
objects instead:

```python
from kontra import catalog

client = await catalog.client()                                   # a real temporalio Client
worker = catalog.worker(client, workflows=[Mine], task_queue=q)   # a real temporalio Worker

async with worker:                                                # start, use, stop
    out = await client.execute_workflow(Mine.run, arg, id="t-1", task_queue=q)
```

and the actor side is the twin, because an Actor is a Temporal activity worker and nothing about it
requires kontra's process:

```python
worker = actor.worker(client)
```

**A Fleet is capacity; a Worker is a poller.** Running a workflow against a local actor, stepping
through one in a debugger, or asserting on its output in CI needs the second and never the first.

Both builders return the real `temporalio` classes with `**kwargs` forwarded untouched, so every
`Worker` and `Client` option is yours — including `workflow_runner=UnsandboxedWorkflowRunner()` when
you want a debugger to see inside the workflow. What they *default* is the pair you cannot guess:
the claim-check data converter (without it everything works until a payload passes 128 KiB, then
fails to decode) and the sandbox passthrough for this SDK's own modules.

> **One gotcha, and it is Temporal's rather than kontra's:** a workflow defined in a script's
> `__main__` fails sandbox validation, because the sandbox re-imports the module. Put workflows in
> an importable module and import them into the runner.

| you want to | use |
|---|---|
| run a worker as this process's job | `catalog.serve()` |
| start a run, query one, drive the API | `catalog.client()` |
| test a workflow, embed a worker, debug | `catalog.worker()` / `actor.worker()` |

---

## Debugging after the fact

```sh
kontra workflow history <run-id> -o run.json
kontra workflow replay workflow.py --history run.json
```

Replay runs the recorded history against the code on disk with **no clock** — nothing times out
while you sit on a breakpoint. It covers the *caller's* decisions (splitting, chaining, branching);
activity code is not re-run, so a Method's results come from the history as values.

Exit `0` clean · `1` non-deterministic against this history · `2` could not run.

---

**See also:** [[Execution-Model]] · [[Writing-Actors-Python]] · [[Fleet-and-the-Warden]] ·
[[CLI-Reference]] · `sdk/python/kontra/catalog.py`
