# bruno — the orchestrator API, executable

Every route on `backend/src/server.ts`, as runnable requests with assertions and docs.
Plain-text `.bru` files, so the API contract diffs in review like code.

This is documentation that fails when it goes stale, which is the only kind worth keeping.

## Run it

```sh
# whole collection against a local control plane
cd backend/bruno
npx @usebruno/cli run --env local -r

# one folder
npx @usebruno/cli run 03-Runs --env local -r

# the GUI (open the `bruno` folder as a collection)
```

`--env controller` points at `10.124.0.2:8088` instead of localhost.

## Tokens

Five routes are authenticated; the rest of the API is open. That is recorded, not endorsed —
see `src/auth.ts`.

| routes | token | env var |
|---|---|---|
| `datasets/query`, `datasets/schema`, `datasets/export`, `runs/:id/explore` | `exploreToken` | `KONTRA_EXPLORE_TOKEN` or `KONTRA_STATE_TOKEN` |
| `state/:tier` | `stateToken` | `KONTRA_STATE_TOKEN` |

Both are declared `vars:secret` so they never land in git. Set them in the Bruno GUI, or
export `BRUNO_ENV_VAR_exploreToken=…` for the CLI.

`checkBearer` **fails closed**: with no token configured server-side it answers `503`, not
`401`. So `503` here means "the server has no token set", while `401` means "your token is
wrong". A run against a fresh control plane legitimately shows `401`/`503` on those five.

## What the assertions assume

The collection is **order-independent and re-runnable**. Fixture-creating requests
(`register actor`, `save graph`) are followed by their own cleanup, and lookups assert
`lte 404` so a clean machine and a loaded one both pass. Requests that need a real run use
`{{runId}}`, captured by `03-Runs/list runs`; with no runs on the box those correctly resolve
to 404.

Assertions are deliberately loose on *presence* (a fresh box has no data) and strict on
*contract* — status codes, shapes, and the error semantics below.

## Contract facts these tests pin

- **`/api/health` does not prove Temporal is up.** The server touches Temporal lazily, so it
  boots and serves catalog/graph with no cluster. `03-Runs/list runs` is the route that
  actually asks Temporal — it describes every run it discovers against the cluster.
- **An unknown workflow is `404`, not `502`.** Both workflow-reading routes previously caught
  every error and answered `502`, making a typo indistinguishable from a Temporal outage. Now
  `WorkflowNotFoundError` (matched by type, not message) is a `404`; a genuine upstream failure
  is still a `502`. Regression-tested in `src/server.test.ts`.
- **Identity is required.** `POST /api/actors` without `version` is `400` — `(name, version)`
  is the cross-plane key, so a partial identity must be refused rather than stored.
- **A version describes ONE shape.** Re-registering a catalogued `(name, version)` with a
  different `input`/`output`/`params` schema is `409`, naming the Method (ADR 0004). Re-posting
  the same descriptor is `200`, because that is what a restarting worker does, and the digest
  route is not gated at all. There is no force flag: the bypass is the silent overwrite.
- **Empty is not the same as failed.** `runs/:id/explore` reports a FAILED dataset as failed
  *with its reason*, never as an empty result. That distinction is how silent data loss is
  caught.

## Adding a request

Copy the nearest `.bru`, bump `seq`, and write the `docs` block for a reader who does not know
the system. Prefer one assertion that encodes a real contract over five that restate the
response shape.
