# 58. The API surface declares its auth posture, and undeclared is closed

Date: 2026-10-06

## Status

**Accepted, and deliberately partial.** Closes the deny-by-default hole for routes nobody has written
yet, and writes down an exposure it does **not** close. Builds on **0054** (session scopes).

## Context

Admission on `/api/*` was a call each route made for itself — `checkBearer` or `checkOptionalBearer`,
inside the handler — across **115 routes in 30 files**. A route that forgets is open, and nothing
anywhere says so. The posture of the surface as a whole was not written down and could not be read
off any single file.

So it was measured. `apiSurface.test.ts` boots the real `buildServer()`, enumerates every `/api`
route Fastify registered, and probes each one with **no credential while all four service tokens are
configured** — which is the distinction that matters and the one a grep cannot make, because
`checkOptionalBearer` admits anonymous callers on any surface whose token is blank.

Of 111 routes (at this commit):

| | count | |
|---|---|---|
| refuse an anonymous caller | 49 | secrets, slots, infra, state, logs, audit, uploads, the dataset workbench, run and workspace mutations |
| public by design | 4 | `GET /api/health`, `GET /api/login`, `POST /api/login`, `POST /api/logout` |
| **reach the handler with no credential** | **58** | including `DELETE /api/actors/:key`, `DELETE /api/datasets/:name`, `GET /api/datasets/:name/preview`, `GET /api/datasets/rows/stream`, `POST /api/workflows/catalog` |

### Why the 58 are not closed here

Because **their clients send no credential.** This is not inferred; it is written in the clients:

```
cli/api.go:121    "the rest of this API predates admission control and is unauthenticated"
                  — newAPI() sets no bearer; only newAuthAPI() does
runtime/go/registrar/registrar.go:146    POST /api/actors            Content-Type only
runtime/python/internals/catalog.py:348  POST /api/workflows/catalog Content-Type only
sdk/python/kontra/secrets.py:252         POST /api/slots/declare     Content-Type only
runtime/python/internals/engine.py:207   POST /api/runs/:id/progress Content-Type only
```

Gating them now would break the CLI for most of its verbs and — worse — break worker registration
**silently**. `catalog.py` is best-effort by design ("never block serving, but never go quiet
either"), so an actor would serve traffic while its workflows never appeared in the catalog and its
slots were never bindable. On a fleet Machine that is a run that looks healthy and does nothing.

That makes closing them a credential change in four languages, not a routing change in one file. It
belongs with the scoped-credential work, and this ADR says so rather than pretending the gate is
complete.

## Decision

**A route's auth posture is declared in one table, and a route that declares nothing is refused.**

1. `src/auth/apiGate.ts` holds `POSTURE`: one entry per `METHOD /path`, valued `public`, `gated`,
   `worker` or `legacy`.
2. `installApiGate(app)` is an `onRequest` hook. An `/api` route with **no entry** is answered
   **403**. That is the deny-by-default property, and it is the half that holds for code that does
   not exist yet.
3. `apiSurface.test.ts` enforces the table against the running server **in both directions**: a route
   the server has and the table does not fails *naming it*; an entry that is no longer a route fails;
   a `gated` entry that does not actually refuse fails; a `public` entry that does not actually admit
   fails.

So a new route is **red in CI and closed in production at the same moment**, and the fix is to say
what it is.

### What the hook does not do

It does not re-check the `gated` routes. They call `checkBearer` themselves against the token vars
their own surface uses, and **four different service tokens are in play** (`state`, `explore`, `run`,
`secrets`). A second generic check here would either duplicate that correctly or weaken it. The test
proves each `gated` route really does refuse, which is the assurance the hook would have been
pretending to give.

### Why a table and not a flag per route

`config.public` on each route is the more idiomatic Fastify answer and was rejected: it spreads the
one question across 30 files, which is the situation that produced this ADR. It also cannot be read
as a whole, so "what is open?" stays unanswerable. A table has one failure mode — drift — and the
bidirectional test is precisely the thing that removes it.

### 403, not 401

No credential would help: the route has not said what it wants, so inviting the caller to
authenticate is a lie. And a 401 clears the console's token (ADR 0054), which would sign an operator
out over a developer's omission.

## Consequences

- **The exposure is now a number in a test** (`legacy: 54, worker: 4`). It cannot shrink or grow
  quietly: closing a route fails the count until someone updates it, which is the point.
- The `worker` four are named apart from `legacy` only because a fleet Machine depends on them, so
  their closure sequences with the **worker's** credential rather than the CLI's.
- **Routes added by concurrent work are 403 until classified.** At this commit four shared-dataset
  routes exist in an uncommitted branch and are unclassified; the test names them, which is the
  intended workflow rather than a conflict.
- `HEAD` is Fastify's free companion to every `GET` and carries no posture of its own; the walk skips
  it.
- Two SSE routes (`GET /api/runs/:runId/stream`, `GET /api/workflows/stream`) never end, so the probe
  classifies them by *reaching* them — which is the finding anyway.

## Tests

`control/orchestrator/src/auth/apiSurface.test.ts` — 12. Besides the four agreement assertions and
the two counts, it pins the hook itself on a bare Fastify instance: an undeclared `/api` route is 403
and the message names the file to edit; it is 403 even with a bearer attached; everything outside
`/api` is untouched (a gate that 403s the SPA is a blank page); and a declared route is admitted —
without which the 403 assertions would pass over a gate that simply refused everything.

The walk asserts it found more than 100 routes before judging any of them. A walk that returns
nothing passes every assertion that loops over it, which is this repository's most repeated failure
shape.
