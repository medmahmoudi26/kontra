# Writing Actors — Go

The Go SDK mirrors the Python author surface as plain registration calls. A Go actor is **run by Go** (`go run .` locally, the compiled binary in its image) — same run-by-language boundary as Python. The public package is `kontra`, and it is `sdk/go`:

```go
package main

import (
    kontra "github.com/medmahmoudi26/kontra/sdk/go"
    // The actor RUNTIME, imported for its side effect: its init() registers the Temporal actor
    // host behind a.Serve(). The author surface declares the seam and never imports across it
    // (runtime -> sdk is one-way), so this blank import is what puts a host in your binary.
    // Leave it out and Serve() exits at startup naming this line, rather than hanging.
    _ "github.com/medmahmoudi26/kontra/runtime/go"
)
```

Both are modules in this checkout, so an actor's `go.mod` requires and replaces both:

```
require (
    github.com/medmahmoudi26/kontra/runtime/go v0.0.0
    github.com/medmahmoudi26/kontra/sdk/go v0.0.0
)

replace github.com/medmahmoudi26/kontra/sdk/go => ../../../sdk/go
replace github.com/medmahmoudi26/kontra/runtime/go => ../../../runtime/go
```

A **caller** — a workflow that drives actors rather than serving one — needs `sdk/go` alone; `go/dnssweep` is one, and its dependency graph carries no Redis and no S3 client at all.

```go
package main

import (
    kontra "github.com/medmahmoudi26/kontra/sdk/go"
    _ "github.com/medmahmoudi26/kontra/runtime/go"
)

type In struct {
    Msg string `json:"msg"`
}
type Out struct {
    Msg   string `json:"msg"`
    Shout string `json:"shout"`
}

func main() {
    a := kontra.New()

    a.Load(func(s *kontra.Session) error {
        s.Set("tag", "echoed") // resources live on the Session (the peer of Python's self)
        return nil
    })

    a.Method("shout", shout, kontra.Takes(In{}), kontra.Emits(Out{}))
    a.Close(func(s *kontra.Session) error { return nil })

    a.Serve() // serve RunBatch/Close as an activity worker; blocks
}

func shout(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
    for unit := range b.All() { // YOU write the loop; the framework commits as it moves
        msg := unit.Str("msg")
        ds.Push(Out{Msg: msg, Shout: strings.ToUpper(msg)}) // to the caller's Dataset (ADR 0028 §2)
    }
    return nil
}
```

**`Serve`, not `Run`.** This call does not execute your actor's code — it boots a worker and blocks forever waiting to be given a Batch. `run` names the **caller's** direction, and this file is the **callee**: the verb for "make this actor do the work" is `catalog.Actor(name, version).Addrs(ctx, batch)`, called from a workflow. Python's `actor.serve()` carries the same correction.

Go has **both** halves. [ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §22 originally
gave Go the callee only and was reversed: `sdk/go/catalog` is the peer of
`sdk/python/kontra/catalog.py`, so a Go workflow pages a Dataset and drives a Python actor exactly
as a Python one drives a Go actor. See [[Execution-Model]].

`a.Serve()` serves the actor as a **Temporal activity worker** — run it directly, exactly like a Python actor: `go run .`, or `kontra serve --actor go/<name> --engine go` for the actor plus its handler in one command (that form executes the compiled binary `<actor-dir>/<name>`, so build it first — with `GOWORK=off`, since example actors are standalone modules). It registers `RunBatch` and `Close` and polls `{name}-{version}-sessions`; there is no sidecar to launch it under, no app port and no actor type name. The batches it serves are scheduled by the actor's Go **handler** ([`/handler`](../../handler)), which owns the workflow on the shared `{name}-{version}` queue — Temporal splits workflow and activity across languages by design, which is what keeps the two halves decoupled.

`actor.json` sits beside `main.go`, same as Python: `{ "schemaVersion": "kontra.actor.v1", "name": "...", "version": "..." }`.

## Methods and the Batch

An Actor declares **as many Methods as it has jobs** and the caller names the one it wants, so chaining a transformation no longer costs a second actor and an edge between them. Two Methods of one Actor share the loaded resource and the Session — `extract` sees the browser `crawl` opened.

```go
a.Method("crawl", crawl)
a.Method("extract", extract)
```

Each Method declares **its own** input and output, because each has its own signature — `fetch` takes a host and emits a page, `title` takes a page and emits a title. The catalog registers **one operation per Method** from those declarations, named after the Method. `Emits` cannot be inferred from the body: push is a call, so a Method returns only an `error`. A Method that declares neither still registers and stays dispatchable by name — it simply contributes an operation with no schemas. `a.Params` stays Actor-level, because run-wide config is not a per-Method signature.

Declaration **order means nothing**. It is not a pipeline: an unnamed dispatch against several Methods is *refused* rather than resolved by order, because a caller who forgot the name would otherwise silently get whichever body was written first ([ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §16). An Actor with a single Method needs no name at the call site.

A Method receives the **whole Batch** and the author writes the loop:

```go
func crawl(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
    for unit := range b.All() {
        page, err := fetch(unit.Str("url"))
        if err != nil {
            return err // blamed on THIS Unit; the Method resumes with the remainder
        }
        ds.Push(page)
    }
    return nil
}
```

Three things follow, and they are the whole model:

- **Asking for the next Unit commits the last one.** The iterator is framework-owned, so position is what says a Unit is finished — which is how a death mid-Batch resumes at the first Unit you had not finished.
- **`ds.Push(x)` names no Unit** (ADR 0028 §1). Provenance lives inside the record, not in your position in control flow, so a push from any goroutine attributes correctly — which is what makes author-written concurrency safe. `Push` is safe from concurrent goroutines; Python's needs no lock because asyncio is not parallel, and goroutines are. It returns nothing: a write that failed to persist surfaces at the next checkpoint or at Method exit (ADR 0028 §3), not as a per-call error you thread through the loop.
- **A Method is entered several times per Batch** — once per isolated Unit failure. Author **locals reset** between entries; the **Session does not**. Anything you accumulate across Units belongs on the Session, not in a local. This is the sharpest thing you must know and would not guess.

The naive loop with no error handling does the right thing: a returned error is attributed to the Unit the loop was on, recorded as a failure, and the Method is re-invoked with only the remaining Units. Nothing pushed before the failure is re-pushed.

### Owning the concurrency

Take every Unit at once when you want to run them yourself:

```go
func scan(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
    units := b.Units()
    sem := make(chan struct{}, 4)
    var wg sync.WaitGroup
    errs := make([]error, len(units))
    for i, unit := range units {
        wg.Add(1)
        go func(i int, unit *kontra.Unit) {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()
            errs[i] = scanOne(s, unit, ds)
        }(i, unit)
    }
    wg.Wait()
    return errors.Join(errs...)
}
```

**What that costs, exactly:** with no position there is nothing to commit by, so the whole Batch commits when the Method returns, and a failure has no Unit to blame — it fails the call and the retry redoes it. `Push` is unaffected — it names no Unit, so a goroutine can call it — and each Unit's resume scratch is still its own. This is the honest price of owning the window, and it is the only thing it costs. There is no `concurrent_aruns` knob any more: the framework stopped owning the window ([ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §18).

### Composing two stages

Chaining is ordinary function calls in the one loop — `ds.Push(extract(crawl(unit.Value)))`. Zero history, nothing to declare. Only the dispatched Method commits, which is right: intermediates should not be durable.

## The surface, mapped to Python

| Go | Python peer |
|---|---|
| `a.Load(fn)` | `@actor.load` |
| `a.Method(name, fn)` | `@actor.method` — a named, individually dispatchable Method taking the whole Batch. Go names it explicitly because it cannot read a function's declared name |
| `a.Close(fn)` | `@actor.close` |
| `a.Healthcheck(fn)` | `@actor.healthcheck` |
| `kontra.Takes(T{})` / `kontra.Emits(T{})` on a Method | `@actor.method(takes=T, emits=T)` |
| `a.Params(p)` | `params = ...` on `@actor.defn` — Actor-level, run-wide config |
| `a.Serve()` | `actor.serve()` |
| `kontra.NonRetryable(msg)` | `raise NonRetryableError(msg)` |
| `kontra.SessionLost(msg)` | `raise SessionLost(msg)` |
| `for unit := range b.All()` | `async for unit in batch` |
| `b.Units()` | `batch.units` — every remaining Unit at once |
| `unit.Value` / `unit.Str(field)` | `unit.value` |
| `ds.Push(record)` | `await dataset.push(record)` — the caller's output Dataset (ADR 0028 §2) |
| `Session.Params` / `Session.RunID` / `Session.Rebuilds` | `self.params` / `self.run_id` / `self.rebuilds` |
| `unit.State()` (`Get`/`Set`/`Delete`) | `self.unit_state` — per-Unit resume scratch |
| `s.GlobalState()` (`Get`/`Set`/`AddToSet`/`Incr`/`CompareAndSet`) | `self.global_state` — cross-Session, actor-**name**-scoped atomic state |
| — **not implemented** | `self.object_state` — cross-Session, dispatch-**key**-scoped. A keyed Go actor pins its resource and its commit map, but has no key-scoped durable tier yet |
| `s.EmitDurable()` | `self.emit_durable` — are emits durable at emit-time? (gate a resume cursor on it) |

The one place the two SDKs are spelled differently is the per-Unit scratch: Python binds `self.unit_state` to the current Unit through a task-local, and Go hangs it off the **Unit** (`unit.State()`). With the author owning the loop there is no per-Unit view of the Session to bind it to, and a "current unit" pointer on the Session would be wrong the moment you ran your Units concurrently. Resume scratch is the one thing that still belongs to a specific Unit, so it rides the Unit receiver — output does not, which is why `Push` names no Unit and this does (ADR 0028 §1).

`a.Step`, `a.Arun` and `kontra.ConcurrentAruns` are **gone**. `a.Step`'s contract was declared topology — "declaration order IS the data-flow contract" — which is the graph drawn in decorators; `a.Arun` was the framework's per-unit callback, which is what a Method replaces.

## The state tiers

Go accessors are the peers of Python's tiers. The Go idiom differs: reads take an `out any` pointer to unmarshal into and return `(ok, err)`.

```go
// per-Unit resume scratch (a crawl frontier, a cursor). A Unit can hold several independent
// slots; the whole Unit's scratch is cleared when the Unit commits.
var frontier Frontier
if ok, _ := unit.State().Get("frontier", &frontier); !ok {
    frontier = NewFrontier(unit.Str("url"))   // fresh Unit
}
_ = unit.State().Set("frontier", frontier)
_ = unit.State().Delete("frontier")           // drop one slot early (optional)

// Anything that must merely outlive a Unit and not the Session is a FIELD, not a tier:
// s.Set("robots_txt", robots) / s.Get("robots_txt") is in-memory, survives a re-entry, and
// dies with the scope — which is exactly what the retired session_state tier was for.

// global_state — cross-Session (all Sessions of the actor), name-scoped. Prefer the atomics.
if added, _ := s.GlobalState().AddToSet("seen_urls", url); added {   // dedupe flagship
    // ... url is new across ALL sessions; process it
}
n, _ := s.GlobalState().Incr("pages_total", 1)                       // atomic counter
ok, _ := s.GlobalState().CompareAndSet("leader", nil, s.RunID)       // ETag CAS
```

`AddToSet`/`Incr`/`CompareAndSet` are ETag optimistic-CAS, so concurrent sessions never lose an
update — use them, not `Get`+`Set` read-modify-write (which is last-write-wins). `global_state`
reuses the same shared Redis the other tiers use (`runtime/go/rediskv`, compare-and-set by a Lua
script byte-identical to Python's) and opens its client lazily. Outside a hosted session all of
them are safe no-ops, so Method bodies unit-test without a host. See
[legacy ADR 0015](../adr/legacy/0015-three-tier-state-and-naming.md) and
[legacy ADR 0018](../adr/legacy/0018-temporal-native-actor-runtime.md).

> **`SessionState` is gone.** [ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §19 retired it: within a Session the Session's own fields already survive, and across Sessions nothing should. What survives is `unit_state` (a Unit's resume scratch), `object_state` (key) and `global_state` (name). **Durable means keyed; in-memory means scoped** — see [[Durability-and-Failures]] for the survival table. Of those, Go implements `unit_state` and `global_state`; **`object_state` is Python-only so far**, so a keyed Go actor gets the pinning without the key-scoped tier.

## Streaming (`ds.Push`)

A fat 1→N Method **streams** each output record as its own durable blob, the moment it exists:

```go
a.Method("scan", func(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
    for unit := range b.All() {
        for _, rec := range scan(unit.Str("url")) {
            ds.Push(rec) // one durable blob per record; no error to check (ADR 0028 §3)
        }
    }
    return nil
})
```

- Each `Push` writes the record to `units/{run}/{node}/u{i}/{sha256}.json` — the **same blob/key/`$ref`
  contract as Python** (`runtime/go/unitstore`), so the orchestrator's streaming cursor
  consumes Go- and Python-pushed records identically. Records must be **content-deterministic**
  (the sha is the record id; a re-push overwrites idempotently). See [[Data-Plane]].
- It needs **`KONTRA_S3_*`** configured for the blob plane; with no object store the host collects
  pushed records inline (the dev/test fallback, matching Python).
- **A resume cursor must be gated on `s.EmitDurable()`.** A cursor that lets a resume **skip**
  already-pushed work is safe **only when pushes are durable at push-time** (a store is
  configured). In the inline mode `EmitDurable()` returns **false**: pushed records sit in memory
  until the Unit commits while the cursor is durable, so skipping them on a resume would
  **silently lose** them. The rule: when it is false, **disable the cursor and re-do the Unit
  atomically**; use the cursor only when it is true.

  ```go
  resumable := s.EmitDurable()          // false in inline mode → re-scan atomically
  var cur Cursor
  if resumable { _, _ = unit.State().Get("phase", &cur) }
  for i := cur.Phase; i < len(phases); i++ {
      // … push this phase's records …
      if resumable { cur.Phase = i + 1; _ = unit.State().Set("phase", cur) }
  }
  ```

  The nuclei example gates exactly this way (see [[Examples]]).

## Execution (same semantics as Python)

Both SDKs are Temporal activity workers, byte-compatible with each other: same queue derivation, same activity names, same payload keys, so the Go handler cannot tell which SDK is on the other end. Each serves `RunBatch` / `Close` on its own sessions queue; the actor's Go **handler** owns the workflow that schedules them and owns retry = exactly-once reload. `load` / Methods / `close` all run on the one live instance pinned to the actor id — a map plus a mutex in the worker process, which is the whole of "activation" now that Temporal serializes work per id. Durable state and per-Unit progress live in Redis, which is durable WITH A BOUND worth knowing: the actor hash carries a 24 h TTL, and the store must run `maxmemory-policy noeviction` — under the old `volatile-lru` the TTL made that hash the first thing evicted under memory pressure, which silently deleted the record of what had committed.

A committed Unit is keyed by the **Batch's content hash plus its index**, and the Method's name is part of that hash — so two Methods handed identical Units are two Batches and cannot replay each other's outputs ([ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §17). Both SDKs compute that hash the same way, which the Go suite asserts against goldens taken from the Python peer.

A panic in a Method body is caught and classified like any other failure, so one bad Unit isolates rather than taking down the worker process and every Session on it.

One behavioural difference is worth calling out because it was a live hazard: the Go host never had Python's turn loop, so a batch that outran `StartToCloseTimeout` was killed with no cooperative escape. Heartbeating per committed Unit is what removed that, and it landed with ADR 0018 — the long batches this SDK was always able to run are only now actually safe.

Both SDKs are held to the same behavior by the wire congruence tests — see [[Contracts]].

## Examples

`go/nuclei` is the reference Go actor — a warm ProjectDiscovery **nuclei** engine scanned over a fleet of targets, using `Load` / `Method("scan", …)` / `Close` and all the state and streaming primitives: it takes the whole Batch to scan targets concurrently over the one thread-safe engine, **pushes each finding to the caller's Dataset as a durable blob**, resumes mid-target from a **per-Unit severity-phase cursor** (safe because pushed findings are already durable), and dedupes findings fleet-wide with `global_state.AddToSet`. See [[Examples]] for the walk-through.
