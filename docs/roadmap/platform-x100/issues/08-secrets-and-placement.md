# 08 — Dapr Secrets API + placement decision

Status: wontfix — **the mechanism this issue is built on no longer exists.** ADR 0018 removed
Dapr, and with it the Secrets API and the placement service. Any future secrets work has to be
designed against the current runtime (systemd `EnvironmentFile` on a Machine, container env on
the Controller), not resumed from here. Kept as the record of what was staged and never read.
**Tier:** 3 | **Effort:** S–M | **Depends on:** —

Two independent hygiene items bundled because both are "use the platform, stop improvising".

## 8a — Secrets via the Dapr Secrets API
### Problem
S3 keys, registry creds, and Temporal/orchestrator endpoints are passed as plaintext env
(`worker-entrypoint.sh`, `docker run -e …`). Fine for a demo, wrong for anything shared.

### Native capability
Dapr **Secrets API** + a secret store component (local file store now; a real KV/Vault later).
The actor host and handler reference secrets by name instead of reading env.

### Approach
- Add a `secretstore` Dapr component; put S3/registry creds there.
- Have the host/handler resolve creds via the Dapr secrets API (or Dapr component `secretKeyRef`).
- Keep env override for standalone `docker run`.

### Files
- new `infra/dapr/secretstore.yaml`, `infra/worker-entrypoint.sh`, host/handler cred lookup,
  `cli/deploy.go`

## 8b — Placement decision
### Problem
Every worker runs its **own** placement service (`infra/worker-entrypoint.sh:28`,
`--placement-host-address 127.0.0.1:50005`) → each worker is a Dapr actor cluster of one. Real
distribution happens at Temporal (task-queue fan-out), so per-worker placement is vestigial overhead
and makes the "distributed actor" story misleading.

### Native capability / decision
Pick one deliberately:
- **(A) Drop it** — commit to "Dapr = local actor runtime, Temporal = distribution". Simplest;
  remove the placement process from the worker; document the stance in an ADR.
- **(B) Centralize it** — one placement service on the control plane, all workers point at it →
  genuine Dapr actor distribution + single-activation. Needed only if we want cross-worker actor
  routing independent of Temporal.

Recommendation: **(A)** unless there's a concrete need for Dapr-level routing — it removes a moving
part per container. Requires shared state store (issue 02) to be coherent either way.

### Files
- `infra/worker-entrypoint.sh`, a new ADR under `docs/adr/`, `docker-compose.yml` (if B)

## Verify (local, no fleet)
- 8a: actor starts and reaches S3 with creds sourced from the secret store, none in `docker inspect` env.
- 8b(A): worker runs without a local placement process; a run still executes end-to-end.

## Comments
Placement stance (8b) should be an ADR since it fixes the project's "are we actually distributed?"
narrative. Pairs with issue 02.
