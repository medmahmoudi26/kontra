# 02 — Shared Dapr state store (fix `global_state`)

Status: wontfix — **the problem is solved, by a different route than this issue proposed.**
ADR 0018 removed Dapr entirely, so there is no `statestore.yaml` to fix and no bundled
`redis-server` per worker to remove: a worker now points `KONTRA_REDIS_HOST` at the Controller
and the whole fleet shares that one store. `global_state` works, and is covered by real-Redis
concurrency tests on both SDKs (`tests/test_redis_kv.py`, `runtime/go/rediskv`).
Kept for the problem statement, which is still the clearest description of why per-worker state
was wrong.
**Tier:** 1 | **Effort:** M | **Depends on:** —

## Problem
`infra/dapr/statestore.yaml` hard-codes `redisHost: localhost:6379`, and every worker container
starts its **own** `redis-server` (`infra/worker-entrypoint.sh:23`). So each worker is an island:
`global_state` (`kontra-global:<actor>:*`, actor-name-scoped, meant to be cross-session) is actually
**per-worker**. Any actor using it for a dedup set, crawl cursor, or rate-limit budget is silently
inconsistent across the fleet. It is also the reason `kontra monitor --state` cannot show the tiers
centrally.

## Native capability to use
Dapr state store is *designed* to point at a shared backend. Point every worker's `statestore`
component at one Redis on the control plane over the private VPC — this is using the primitive as
intended, not building anything.

## Approach
- Template `redisHost` from `KONTRA_REDIS_HOST` (default the controller VPC IP, e.g.
  `10.124.0.2:6379`); `worker-entrypoint.sh` substitutes it into the component at boot.
- Add a **shared Redis** to docker-compose bound to the VPC IP (private only — respects
  "no external ports"). Stop bundling a per-worker `redis-server` (or keep it only as an explicit
  `KONTRA_REDIS_HOST=localhost` fallback for standalone `docker run`).
- `cli/deploy.go`: pass `KONTRA_REDIS_HOST` through to deployed workers.
- Decide tier scope: `global_state` **must** be shared; `arun_state`/`session_state` are run-scoped
  and may stay effectively local, but sharing them is simplest and enables central inspection.

## Files
- `infra/dapr/statestore.yaml`, `infra/worker-entrypoint.sh`, `docker-compose.yml`, `cli/deploy.go`

## Verify (local, no fleet)
- Run **two** worker containers of the same actor, both `KONTRA_REDIS_HOST=<controller>`.
- Have the actor `global_state.Set` a key on worker A and read it on worker B → same value.
- Confirm `arun_state`/`session_state` keys don't collide across concurrent chunks (unique actor
  ids per chunk already guarantee this).

## Risks
- Actor single-activation with a shared store: Temporal already serializes a chunk's activity, and
  chunk actor ids are unique, so cross-worker collisions shouldn't occur — verify under retries.
- Shared Redis becomes a control-plane dependency; size it and set an eviction/TTL policy
  (`ActorStateTTL` is already enabled in `config.yaml`).

## Comments
