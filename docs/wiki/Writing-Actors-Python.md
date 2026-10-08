# Writing Actors — Python

An actor is a directory. **Two files are the actor** — `actor.py` and `actor.json` — and **four more
will tell the buildpack builder how to build it**: `pyproject.toml`, `uv.lock`, `.python-version` and
`Procfile`. The [layout](#the-actor-directory-and-where-dependencies-go) at the bottom of this page is
that target shape, and the shipped `kontra deploy` reads none of those four yet. Start with the two.

**`actor.json`** names it and states what it needs (the `name` **must equal the directory name**):

```json
{ "schemaVersion": "kontra.actor.v1", "name": "echo", "version": "0.1.0" }
```

Everything else lives in **`actor.py`**: typed I/O classes and a decorated lifecycle. The public package is `actorkit` (the flat files under `sdk/python/kontra/`); `internals`, under `runtime/python/`, is the private runtime — and the import arrow runs one way only, runtime to sdk.

```python
from dataclasses import dataclass
from kontra import actor

@dataclass
class EchoInput:
    msg: str
    id: int | None = None

@dataclass
class EchoOutput:
    msg: str
    shout: str
    by: str
    id: int | None = None

@actor.defn
class Echo:
    input = EchoInput          # JSON Schemas are DERIVED from these types
    output = EchoOutput        # (pydantic) — no hand-written schema files

    @actor.load
    async def open_resource(self):
        self.tag = "echoed"    # a browser / client / pool goes here

    @actor.method(takes=EchoInput, emits=EchoOutput)   # types live on the METHOD
    async def shout(self, batch, dataset):             # the Batch, and the caller's output Dataset
        async for unit in batch:                       # the whole Batch; YOU write the loop
            await dataset.push(EchoOutput(msg=unit.value.msg,
                                          shout=unit.value.msg.upper(), by=self.tag))

    @actor.close
    async def close_resource(self):
        self.tag = None

if __name__ == "__main__":
    actor.serve()              # starts the worker; a Python actor is run by Python — no CLI
```

## The lifecycle

| Decorator | Runs | Contract |
|---|---|---|
| `@actor.load` | once per **session**, on the host that owns the session | open non-serializable resources on `self` (browser, client, files); they stay visible to every **Method** of that session |
| `@actor.method(...)` | once per **Batch** — and again per isolated Unit | Receives the WHOLE Batch and the caller's output **Dataset**, and you write the loop (ADR 0023 §2, §3; ADR 0028 §2). Asking for the next Unit is what commits the last one, so a death mid-Batch resumes at the first Unit you had not finished. Results leave through `await dataset.push(x)` — never a return value (§18), and the push names no Unit, so provenance lives inside the record (ADR 0028 §1). Declare as MANY as the actor has jobs and the caller names the one it wants (§9); they share the loaded resource and `self.*`. Bodies must be idempotent: a Unit re-runs if a crash lands between the work and its commit |
| `@actor.close` | always at the end — success or failure | release what `load` opened |

`self` is an instance of *your* class with framework state mixed in: `self.params` (run-wide config from the dispatcher), `self.run_id` / `self.idempotency_key` (run lineage), and `self.rebuilds` (how many times this session's resource was rebuilt after a `SessionLost`).

## Knobs

### Concurrency is yours

There is no `concurrent_aruns` knob any more — the framework stopped owning the window (ADR 0023
§18), and the sliding window it used to schedule is gone with it. Run Units concurrently by taking them yourself:

```python
async def crawl(self, batch, dataset):
    async def one(unit):
        await dataset.push(await fetch(unit.value.url))
    await asyncio.gather(*(one(u) for u in batch.units))
```

`push` works from a spawned task precisely because it names no Unit (ADR 0028 §1) — a record's
provenance lives inside it, not in your position in the loop, which is why a parameter beats a
`yield` you cannot call from a task. **What you give up is the position**: with no iterator there
is nothing to commit by, so the whole Batch commits when the Method returns and a failure has no
Unit to blame. That is the honest price of owning the window, and it is the only thing it costs.

`param.get(key, default)` is the one canonical accessor: in a decorator (no run yet) it returns a deferred reference resolved per run from `params`; inside a Method or `load` body it returns the live value.

## Failure signals (both optional)

```python
from kontra import NonRetryableError, SessionLost

raise NonRetryableError("permanently bad unit")   # skip retries → isolate immediately
raise SessionLost("the browser died")             # END the Session; the caller's scope raises
```

Required author error-handling is **zero** — see [[Durability-and-Failures]] for what the framework does with every failure class.

## `unit_state` — per-Unit resume scratch (fat Units)

For a **fat Unit** — one whose processing is real incremental work (a deep crawl, a long export) —
per-Unit exactly-once is too coarse: a death would re-run the whole Unit from the start.
**`unit_state`** gives that Unit **keyed** durable resume scratch, so one Unit can carry several
independent slots:

```python
frontier = await self.unit_state.get("frontier")   # None on a fresh Unit; last snapshot after a death
await self.unit_state.set("frontier", frontier)    # persist this Unit's resume scratch
await self.unit_state.set("cursor", offset)        # a second, independent slot for the same Unit
await self.unit_state.delete("cursor")             # drop one slot early (optional)
```

**Who actually reads it** is worth being precise about, because ADR 0023 §19 got this wrong once
and retired the tier before the amendment put it back. The reader is a Unit that was **in flight
when the activity died** and re-runs on the handler's retry — same Session queue, same Batch hash,
so the same slot. A **committed** Unit is skipped by the commit map and an **isolated** Unit is
never resumed, so neither of those ever reads it.

All of a Unit's keys live in **one** blob (state key `{slot}-ckpt`) and the framework **deletes
the whole blob when the Unit commits** — so a finished Unit never resumes, and you rarely need
`delete()` yourself. It is resume scratch, **not a result store**: output goes through
`await dataset.push(x)`, never here. Outside the host (plain unit tests) `get()` returns `None` and
`set()`/`delete()` are no-ops — no mocking needed.

`set()` is at-least-once: a death between the work and the `set()` replays that slice, so
snapshots must tolerate replay (e.g. a visited-set skip).

All tiers share **one keyed `get(key)`/`set(key, value)`/`delete(key)` shape**, differing only in
scope: `unit_state` per-Unit (this), `object_state` per dispatch **key**, `global_state` per actor
**name** — see [[Durability-and-Failures]] for the survival table, and
[legacy ADR 0022](../adr/legacy/0022-object-state-and-keyed-dispatch.md) for keyed dispatch. The
cross-call tier that used to sit between them, `session_state`, is retired: within a Session
`self.*` already survives, and across Sessions nothing should (ADR 0023 §19).

The crawl4ai resumable tier wires crawl4ai's **native** frontier-only state straight to a named
`unit_state` slot (`python/crawl4ai/actor.py`) — a small frontier, never page content:

```python
strategy = BFSDeepCrawlStrategy(
    max_depth=..., max_pages=...,
    resume_state=await self.unit_state.get("frontier"),        # rehydrate the frontier after a death
    on_state_change=lambda st: self.unit_state.set("frontier", st),  # persist as it advances
)
```

(crawl4ai awaits `on_state_change`, so the lambda returns the coroutine from the async `set`.)
This resume path is gated on `self.emit_durable` — the actor wires it only when
`resumable and self.emit_durable`, and otherwise falls back to the atomic default (below).

Pages leave via `await dataset.push(x)` (below), not the resume slot. Verified end-to-end:
`kill -9` of the worker mid-crawl → restart → the crawl resumes from its frontier, no page
re-crawled from scratch.

## 1 → N is just pushing twice

A Unit that fans out to many records does not need a special shape. `push` is a call, so call it
as often as the Unit has results:

```python
@actor.method(takes=Seed, emits=Page)
async def crawl(self, batch, dataset):
    async for unit in batch:
        async for page in self.deep_crawl(unit.value.url):
            await dataset.push(Page(url=page.url, status=page.status, markdown=page.text))
```

**A Method is NOT a generator** (ADR 0023 §18) — `yield` used to be the streaming path and is now
rejected at import, because a generator that is built and never iterated is a Batch that silently
pushes nothing. Every push is durable the moment it is produced (with an object store configured),
so a downstream reader sees records before the Unit finishes, exactly as the yield plane did.

Each record lands at `units/run={run}/dt={date}/actor={actor}/shard={n}/unit={i}/{sha}.json`,
where the sha is over the record itself — so re-pushing the same record on a resume is an
**idempotent overwrite**. Records must be **content-deterministic**: no timestamps, no volatile
fields, or the same page gets a new id every fetch. See [[Data-Plane]].

## Counters, sets, and what survives a re-entry

**A Method is entered several times per Batch** — once per isolated Unit failure (§13). The
framework records the failure and re-invokes your Method with the remaining Units, because
everything pushed before the raise is already committed. That re-invocation is an ordinary call,
so your LOCALS are rebuilt:

```python
async def crawl(self, batch, dataset):
    count = 0                  # rebuilt on EVERY entry — counts since the last failure
    self.total = getattr(self, "total", 0)   # the Session — survives re-entry
    async for unit in batch:
        count += 1
        self.total += 1
```

It never raises and the number looks plausible, which is what makes it worth stating. For a count
that must survive the scope — or be right across a fleet of workers doing the same job — use the
atomic ops below. `self.*` is enough when the fact belongs to this scope and you read it before
the scope ends.

[[Durability-and-Failures]] has the full table of what survives what.

## `global_state` — cross-session state (all sessions of an actor)

`global_state` is durable state shared across **every session of an actor** (every actor id),
scoped by actor **name** — so a version bump deliberately shares it. Because concurrent sessions
race the same keys, plain `get`/`set` is last-write-wins and drops updates; reach for the
**atomic ops** instead:

```python
# the dedupe flagship — a shared visited SET, never double-processing a URL across sessions
if await self.global_state.add_to_set("seen_urls", url):   # True = newly added
    await self.process(url)

n = await self.global_state.incr("pages_total")            # atomic counter → new value
ok = await self.global_state.compare_and_set("leader", None, self.run_id)
val = await self.global_state.get("config")                # LWW get/set also exist
```

`add_to_set` returns whether the member was newly added; `incr` returns the new count;
`compare_and_set` returns whether it won. These are ETag optimistic-CAS under the hood (bounded
retries, then a loud error) so no concurrent update is silently lost. The backing store is the
same shared Redis the other two tiers use — one small hash per key, compare-and-set by a Lua
script so the swap is atomic *on the server*; its client opens lazily on first use, so an actor
that never touches `global_state` pays nothing. Outside the host, reads return `None` and writes
are no-ops.

## `object_state` — cross-session state for ONE key

`object_state` is `global_state`'s operations — same atomics, same store, same lazy client —
under a namespace scoped to the actor's **key** rather than its name. The key is whatever the
caller bound with `handle["acme.com"]` from their own workflow ([ADR
0021](../adr/legacy/0021-callers-side-workflows-and-the-activity-kind.md)); with no key, the actor id
falls back to a fresh run/node coordinate and the tier is simply private to that dispatch.

```python
# a per-target frontier, still there on the NEXT dispatch of the same key — next week, even
if await self.object_state.add_to_set("seen", path):
    await self.crawl(path)

spent = await self.object_state.incr("budget_spent", by=cost)
```

The point is what the two tiers separate: `global_state.add_to_set("seen", url)` dedupes across
**every** target the actor has ever crawled, `object_state.add_to_set("seen", url)` dedupes
within **this** target. Both are one line; picking the wrong one is a silent correctness bug in
either direction, which is why they are different accessors rather than a scope argument.

Because a keyed dispatch is exclusive per key (the backing workflow id carries the key, so two
batches for one key cannot run concurrently), this tier is what makes a kontra actor a *virtual
object* in the Restate / Durable Objects sense — see
[ADR 0022](../adr/legacy/0022-object-state-and-keyed-dispatch.md).

**The in-scope tiers do not survive a new dispatch.** `unit_state` clears the moment its Unit
commits, and `self.*` dies with the Session; a second dispatch on the same key starts both empty.
Only `object_state` and `global_state` cross that line, which is the whole distinction between
what a Session remembers and what an object remembers.

## Params (run-wide config)

Declare a typed `params` class on `@actor.defn` for a schema-driven form in the UI; the dispatcher's values arrive as `self.params` and feed every `param.get(...)` binding:

```python
@dataclass
class CrawlParams:
    depth: int = 1
    concurrent_crawls: int = 2

@actor.defn
class Crawler:
    input = CrawlInput
    output = PageOut
    params = CrawlParams
```

Rule of thumb: knobs belong in `params` / the input schema — actors run **zero AI mid-execution**; AI/human-in-the-loop tuning happens *between* runs at the orchestrator.

## The actor directory, and where dependencies go

```
actor.py            your code
actor.json          identity, and the runtime it needs
pyproject.toml      dependencies — including kontra-sdk==<pinned>
uv.lock             what those resolved to. This file is why a code change is cheap
.python-version     the interpreter. REQUIRED — see below
Procfile            worker: python actor.py
```

**Dependencies are declared only through `pyproject.toml` + `uv.lock`** (or `requirements.txt`). The
builder's Python buildpack understands them, puts them in their own layer, and **reuses that layer
whenever the lockfile is unchanged** — which is where the whole saving comes from. Measured: a one-line
change to `actor.py` against an unchanged lockfile adds **0.133 MiB** of new blobs and logs
`Reusing layer 'heroku/python:venv'`, where the old path reinstalled every dependency including
Chromium.

> [!IMPORTANT]
> **`.python-version` is not optional**, and the error if it is missing does not say so usefully.
> Heroku's Python buildpack refuses a uv project without it: *"When using the package manager uv on
> Heroku, you must specify your app's Python version with a .python-version file."* Measured — the
> build failed with exit status 51, and succeeded once the file existed. One line is the whole file:
>
> ```
> 3.12
> ```
>
> Which versions are available comes from the pinned builder, not from kontra.

**`Procfile`** names the process to start. A Python actor is run by Python — the same run-by-language
boundary as before — so the line is `worker: python actor.py`.

**`actor.json`'s `runtime` field** picks the OS and the system packages: `python:1` by default,
`python-browser:1` for a headless browser. System packages are chosen there, never installed per actor.
See [[Runtimes]].

### `deploy.sh` is not run any more

A buildpack build **does not run `deploy.sh`**, so an actor that depended on it would build clean and
be missing whatever the script installed — a failure at run time, in a container, far from the change
that caused it. So the author is told at build time: a warning, and a refusal under
`KONTRA_DEPLOY_SH=refuse` (`cli/packbuild.go`).

Migrating one is a split, and the two halves go to different places:

| what the script did | where it goes now |
|---|---|
| `apt-get install` of system libraries, fonts, a browser | a **runtime** that `provides` them |
| `pip install` of a library | `requirements.txt`, or a `pyproject.toml` you resolve yourself |
| fetching a binary the actor shells out to | a runtime, for the same reason as apt |

[[Runtimes]] walks through `webcrawl`'s script, which is all three at once.

An author's own `Dockerfile` beside `actor.py` is **no longer an escape hatch** — nothing reads it.
Native dependencies are a runtime now, and a runtime is a directory in `kontra-runtimes`, which is
the point: one image provides them for every actor on it instead of each actor installing its own.

### What `kontra deploy` adds to your directory, and why it is a copy

Your directory is what you publish, so **nothing is written into it**. `kontra deploy` stages a copy
(`cli/packstage.go`) and adds what the image needs and the actor does not carry:

| added to the staged copy | why |
|---|---|
| `requirements.txt` | so heroku/python participates in detection. If you ship one it is **extended**, never replaced; a `pyproject.toml` is refused with the line to add, because your resolver would ignore an appended pip requirement |
| `Procfile` | the process definition, which is the only place a runtime variable can be baked — `pack build --env` is build-time only and a CNB image has no `ENTRYPOINT` to set |
| `.python-version` | pinned, so two builds of one commit get the same interpreter |
| the SDK, vendored | `vendor/sdk/python` + `vendor/runtime/python`, installed **by path**. Never by name: `kontra-sdk` is on no index, and `kontra` on PyPI is an unrelated project |
| `kontra-handler` + `worker-entrypoint.sh` | the workflow half and the supervisor that runs both halves. An image without the handler polls the sessions queue, answers no workflow task, and looks healthy — so a missing one is a refusal |

So a pure-Python actor needs `actor.json` and `actor.py` and nothing else, which is what the shipped
`hello` is. Add a `requirements.txt` when you have a dependency of your own.

Two consequences of the lifecycle doing the build, worth knowing when you read a container:

- **The app lands at `/workspace`**, not `/actor/<name>/`. The supervisor takes `KONTRA_ACTOR_ROOT`
  and `KONTRA_HANDLER_BIN` for exactly this reason, both defaulting to the old absolute paths so an
  image built before the switch keeps working.
- **The identity rides in the Procfile** — `KONTRA_ACTOR_NAME`, `_VERSION`, `_ENGINE`, `_KIND`,
  `_ENTRY` — rather than in `ENV` layers. The Warden still sets `KONTRA_ACTOR_NAME` and `_VERSION`
  from the **assignment** it was given, `KONTRA_NAMESPACE` from the Machine's own certificate and
  `KONTRA_ACTOR_DIGEST` from the pull; nothing sets the other three at container start, which is why
  they have to come from the image.

## When the work is a function

An Actor is a Session — a resource loaded once, a commit map, the durable tiers, per-Unit
heartbeats and isolation. When the work is just a function, none of that machinery costs
anything you have to think about: write the Method and skip `@actor.load`.

```python
from kontra import actor

@actor.method(takes=Host, emits=Probe)
async def http_probe(self, batch, dataset):
    async for unit in batch:
        await dataset.push(probe(unit.value.host))

if __name__ == "__main__":
    actor.serve()
```

That IS the light grain (ADR 0023 §9). There used to be a second deployed kind for this — an
**Activity** bundle, `"kind": "activity"` in `actor.json`, one process and no handler, on its
own `{name}-{version}-activities` queue. It retired with v2: one kind means one deploy path,
one queue derivation, one catalog entry and one thing to learn, and a Method with no load
already has an Activity's cost. `kontra deploy` REFUSES a manifest that still says
`"kind": "activity"` and tells you this.

**What you still choose is the retry grain, and it is a Method-body decision.** A Method that
pushes per Unit resumes where it died; a Method that does all its work and pushes once at the end
starts that work over. Push as you go when losing half the work matters.

Call either from your own workflow — see [[Execution-Model]] and `workflows/`.

## Testing

Your code never sees Temporal — a **Method** is a plain async function, so tests are fast unit tests over your `@actor.method`/`@actor.load` logic, no cluster. (The *process* is a Temporal activity worker since ADR 0018, but that lives in `internals/temporal/host.py`, entirely below the author surface; outside a hosted session all three state tiers are safe no-ops.) Repo-wide: `just test` / `pytest -m 'not e2e'`. See [[Dev-Cycle]].
