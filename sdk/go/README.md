# sdk/go — the Go author surface

The Go actor SDK — the parallel of `sdk/python`. A Go actor process is a **Temporal activity
worker** (ADR 0018): it registers `RunBatch` + `Close` and polls its own sessions queue — no
sidecar, no app port, no HTTP. It is **run by Go** (`go run .` locally, the compiled binary in its
image), never by a CLI. Author code imports only `kontra`; the Temporal SDK is the host's business.
The one Go Temporal handler that owns the workflow lives in `/handler`, fully decoupled — the two
meet only at the queue name and the JSON wire.

**This module imports nothing of `runtime/go`, and `arrow_test.go` is what keeps that true**
(ADR 0035 §2): `go list -deps ./...` names no runtime package, no Redis client and no object-store
client — 439 packages, zero matching `aws|redis`. The engine, the codec, the state tiers and the
Temporal actor host all live in `runtime/go` and depend on THIS module, never the reverse. Which is
why an actor's `main` links the runtime itself, with a blank import:

```go
package main

import (
	"strings"

	kontra "github.com/medmahmoudi26/kontra-local/sdk/go"
	// Registers the actor host behind a.Serve(). Serving is a runtime act; the author surface
	// declares the seam (kontra.Host) and the runtime fills it from init(), the way database/sql
	// takes its drivers. Leave this out and Serve() exits naming this exact line.
	_ "github.com/medmahmoudi26/kontra-local/runtime/go"
)

func main() {
	a := kontra.New()
	a.Load(func(s *kontra.Session) error { s.Set("tag", "echoed"); return nil })
	a.Method("shout", shout, kontra.Takes(EchoIn{}), kontra.Emits(EchoOut{}))
	a.Serve()                                       // serves the activity worker; blocks
}

func shout(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	for unit := range b.All() {                     // YOU own the loop; the framework commits
		ds.Push(EchoOut{Shout: strings.ToUpper(unit.Str("msg"))}) // to the caller's Dataset
	}
	return nil
}
```

Identity (`name`/`version`) is read from the `actor.json` beside the program. Just start the
binary, with the controller endpoints in the environment:

```sh
KONTRA_ADDRESS=localhost:7233 KONTRA_REDIS_HOST=127.0.0.1:6379 \
  KONTRA_S3_ENDPOINT=http://localhost:8333 ./actor
```

Locally, `kontra serve --actor <dir> --engine go` starts that binary (`<dir>/<name>`, already built)
**and** the handler — the two processes a fleet Machine runs.

Wiring is env-driven: `KONTRA_ADDRESS` / `KONTRA_NAMESPACE` (Temporal), `KONTRA_REDIS_HOST`
(**required** in practice — the durable state below lives there; it points at the Controller so a
whole fleet shares one store), `KONTRA_S3_*` (the sub-unit blob plane; unset = inline mode),
`KONTRA_MAX_PARALLEL_SESSIONS` (live-session cap, default 4), `KONTRA_ORCHESTRATOR_URL`
(best-effort dev discovery self-register), `KONTRA_ACTOR_DIGEST` (OCI pin).

## The author surface

`kontra.New()`, then register a `Load → Method* → Close` lifecycle and `Serve()`:

- **`Load`** — open the once-per-session resource (a browser, a client, a pool); visible to every
  Method of the session.
- **`Method(name, fn, kontra.Takes(In{}), kontra.Emits(Out{}))`** — a named, individually dispatchable Method that receives the whole
  **Batch** and the caller's output **Dataset**, loops over the Batch (`for unit := range b.All()`), and pushes per Unit with `ds.Push(x)`. Declare as many as
  the Actor has jobs; the caller names one, and declaration **order means nothing** — an unnamed
  dispatch against several Methods is refused rather than resolved by order (ADR 0023 §16).
  Take `b.Units()` instead to run them concurrently yourself; that gives up per-Unit commit and
  nothing else.
- **`Close`** — release what `Load` opened; the handler invokes it when the run completes.
- **`Healthcheck`** — optional liveness probe: return progress while alive, return an error (or
  `false`) when the resource is dead → the host reloads via the handler retry.
- **`kontra.NonRetryable(msg)` / `kontra.SessionLost(msg)`** — the two optional failure signals
  (isolate-this-Unit / end-the-Session).

**`Serve`, not `Run`.** It does not run your code — it boots a worker and blocks. `run` names the
CALLER's direction, and Go has no caller's side: Go implements the **callee half only**
(ADR 0023 §22), so the verb for "make this actor do the work" lives in Python.

Two things Go must do that Python does not, both from ADR 0023 §23: `ds.Push` takes a **mutex**
(goroutines are genuinely parallel where asyncio is not), and the per-Unit resume scratch hangs off
the **Unit** (`unit.State()`) rather than the Session, because with the author owning the loop there
is no per-Unit view of the Session to bind it to.

## How it runs (the cross-language contract)

The Go host byte-matches the Python host, so a Go actor is dispatched and discovered identically:

- Serves `RunBatch` + `Close` on `{name}-{version}-sessions` (`{name}-shared-sessions` with no
  version). One `RunBatch` activity per Batch — one activity, no turns. The dispatch names which
  Method it means (`method` on the payload), resolved against the registry per Batch, which is what
  lets one loaded Session serve several Methods.
- **Heartbeats are the liveness check.** The activity beats once per unit outcome
  (`{node, done, total, isolated}`, read by the orchestrator's monitor), so a wedged unit simply
  stops beating and `HeartbeatTimeout` catches it. Nothing kills a slow-but-progressing batch.
- **Exactly-once** via the per-actor Redis state hash (`runtime/go/statekv`), keyed by the **Batch's
  content hash plus the Unit's index** (ADR 0023 §17) — a hash is stable across a retry, distinct
  across Batches, and survives a reopened scope, where a sequence number would restart at zero on
  exactly the recovery path v2 makes routine. A reload (worker/host death, or `SessionLost`) skips
  the committed Units. Owned
  by the handler's retry, not author code. Tiers 1+2 share one `kontra-actor:{actorID}` hash with a
  24 h TTL (one `EXPIRE` slides all of it); `global_state` is tier 3, in ETag hashes
  `kontra-global:{actorName}:{key}` (`data` + `ver`) with a Lua CAS (`runtime/go/rediskv`).
- **Per-instance activation** is a map plus a mutex in this process (`runtime/go/temporalhost`): one
  live instance per actor id, one batch at a time for that id. That is all activation ever needed —
  Temporal serialises turns per actor id via the backing workflow.
- **Isolation at the iterator boundary** (ADR 0023 §13): a failure in the Method body is blamed on
  the Unit the loop was on, recorded into the `{results, failures}` envelope with the
  `PerUnitFailure` shape (`unit` / `error.{type,message}` / `category`), and the Method re-invoked
  with the remainder. So a Method is entered several times per Batch — author **locals reset**
  between entries, the **Session does not**. A Unit that keeps killing the resource is isolated
  after two reloads (§21). A panic is classified like any other failure rather than taking the
  worker down.
- **`describe()`** (`runtime/go/registrar`) reflects ONE CATALOG OPERATION PER METHOD from its
  `Takes`/`Emits` — an Actor has many Methods with different signatures, so one Actor-level pair
  cannot describe it — and
  best-effort self-registers it for dev discovery — no per-language registration CLI.

## Verified

`go build ./... && go vet ./... && go test -race ./...` — the author's loop (range, push, per-Unit
commit, mid-Batch resume, isolation and re-invoke), `Push` under real parallelism, the Method
registry, the sessions-queue derivation (shared with the handler + Python host), the commit-key and
Batch-hash congruence with the Python peer, the Redis CAS semantics and key layout, and the
cross-SDK blob-key fixture.

Build per module: there is no root module, so `GOWORK=off GOTOOLCHAIN=go1.26.4`.

## Not yet

Session fan-out / co-location (`parallel_sessions` / `local`) and a sub-unit checkpoint plane are
deferred. When co-location lands it must **not** be Temporal Worker Sessions: they are Go-only (two
of the three languages here could not use them) and give no host-death durability, so they cannot
back kontra's exactly-once reload — see the Sessions note in the durability docs.
