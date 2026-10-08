# Local cluster and Pulumi dockerFleet Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single-install install with a Docker Compose cluster and a Pulumi `dockerFleet` sibling of `do_fleet`, so a first-time installer can bring up Postgres, Temporal, SeaweedFS, and the rest of the control plane as separate services, then serve and start a seeded hello workflow that writes `hello world` into a Dataset and tears the local Fleet down.

**Architecture:** One Compose project on a private `kontra` network. Operator ports bind `127.0.0.1`. Orchestrator API, materializer, and infra stay separate PIDs. `docker_fleet()` starts `stackWorkflow` on `kontra-infra` against project `kontra-docker-fleet` with no cloud credential. Each Machine is a Warden container with the host Docker socket; actor Workers are sibling containers. An empty workspace is seeded once; discovery is recursive and does not auto-run workflows.

**Tech Stack:** Docker Compose, Temporal auto-setup, Postgres 16, SeaweedFS, Redis, registry:2, Node 22 orchestrator, `@pulumi/docker`, Go CLI/Warden, Python SDK.

## Global Constraints

- Work on `dev`-based branch `feat/local-cluster-docker-fleet`. Never commit to `main`.
- No Kubernetes. No second supported install install path.
- `do_fleet` public behaviour is unchanged.
- Published ports default to `127.0.0.1`.
- Seed only when the workspace is empty; never overwrite user files.
- Discovery must not serve or start workflows. Actor discovery must build/publish a placement-ready artifact.
- `dockerFleet` is Pulumi (`@pulumi/docker`), project `kontra-docker-fleet`, no cloud credential.
- Host Docker socket on Warden Machines is documented as host-level authority, acceptable only for single-operator local use.
- Documented install commands are the CI gate. Keep existing `kontra workflow serve --repo` CLI work.

---

## File map

| File | Responsibility |
| --- | --- |
| `sdk/python/kontra/fleet.py` | `Docker` class, `docker_fleet()`, provider-aware `Fleet.fqn` |
| `tests/test_fleet_client.py` | Pin `DOCKER_FLEET_PROJECT`; docker provider construction |
| `control/orchestrator/src/infra/stacks.ts` | Second project, coerce docker args, empty credential |
| `control/orchestrator/src/infra/programs/dockerFleet.ts` | Pulumi Docker Warden Machines + assignment files |
| `control/orchestrator/src/activities/infra.ts` | Skip secret resolution when `providerEnvVar` is empty |
| `cli/warden/driver_docker.go` | Docker CLI driver: pair of sibling containers, no runtime socket to workers |
| `cli/warden/warden_local.go` | `--local` identity bootstrap + file assignment |
| `cli/workspace.go` | `kontra workspace seed` / `watch` |
| `docker-compose.quickstart.yml` | Documented cluster |
| `control/images/Dockerfile.orchestrator` | Node 22 + Pulumi CLI |
| `examples/python/hello/*`, `examples/workflows/hello/*` | Seeded starter |
| `docs/adr/0047-local-cluster-and-docker-fleet.md` | Supersede 0031 packaging |
| `.github/workflows/ci.yml` | Literal documented install path |

---

## Task 1: SDK `docker_fleet`

**Files:** `sdk/python/kontra/fleet.py`, `tests/test_fleet_client.py`

- [ ] Add a failing test that `docker_fleet(machines=1)` is a `Docker`, its `args()` has `machines` and no `credential`, and `fleet.up(docker_fleet(machines=1), actor="hello", version="0.1.0").fqn == "kontra-docker-fleet/hello-0.1.0"`.
- [ ] Pin `DOCKER_FLEET_PROJECT` against `stacks.ts` the same way `FLEET_PROJECT` is pinned.
- [ ] Implement `class Docker` (`machines`, optional `image`, `network`, `docker_sock`) and `docker_fleet(**kwargs)`.
- [ ] Give `DigitalOcean` a `project = FLEET_PROJECT` attribute; `Docker.project = DOCKER_FLEET_PROJECT`.
- [ ] Change `Fleet.fqn` to `f"{self.provider.project}/{self.name}"`.
- [ ] Widen `hold` / `up` / `_fleet` to `DigitalOcean | Docker`. Refuse DigitalOcean-only knobs (`region`, `size`, `credential`) beside a `Docker` object. Default implicit provider remains `DigitalOcean`.
- [ ] Export `Docker`, `docker_fleet`, `DOCKER_FLEET_PROJECT`.
- [ ] Run `pytest tests/test_fleet_client.py tests/test_fleet_hold_place.py -q`.

## Task 2: Infra dispatch + dockerFleet program

**Files:** `control/orchestrator/src/infra/stacks.ts`, `programs/dockerFleet.ts`, `programs/dockerFleet.test.ts`, `activities/infra.ts`, `control/orchestrator/package.json`

- [ ] Add failing tests: `planFor({ stackFqn: 'kontra-docker-fleet/hello' })` returns empty `providerEnvVar`; unknown projects still refuse; coerce keeps `tag`/`machines`/`network`/`image`/`dockerSock` and drops `evil`.
- [ ] Add `@pulumi/docker`. `DOCKER_FLEET_PROJECT = 'kontra-docker-fleet'`.
- [ ] `planFor` second case: `dockerFleetProgram(coerceDockerFleetArgs)`, `credential: { name: '' }`, `providerEnvVar: ''`.
- [ ] `checkCloudCredential` / `workspaceFor`: when `providerEnvVar` is empty, skip the secret store and pass `{}` as provider env.
- [ ] Program: name Machines `kf-<tag>-NN`; `docker.Container` on network `kontra` (configurable); mount `/var/run/docker.sock`; command `kontra warden serve --driver docker --local --assignment <file>`; env `KONTRA_ADDRESS=temporal:7233`, Redis, S3, registry, trust-unsigned for the cluster registry. Write assignment JSON for placements (`workerImage` digest-pinned). Inventory `{name, host, publicIp, tag}` with container name as host. Echo placements like the DO program. No cloud-init, no SSH, no credential interpolation.
- [ ] Add optional placement key `workerImage` (not `image` — that is the DO droplet slug). Update `shared/conformance/placement.json` optional list. `resolveBundle` may return `workerImage` when a runnable worker image exists at `<registry>/<actor>:<version>`.
- [ ] Run orchestrator vitest for stacks/dockerFleet/credential.

## Task 3: Warden docker driver + `--local`

**Files:** `cli/warden/driver_docker.go`, `warden.go`, `warden_local.go`, tests

- [ ] Failing tests: `--driver docker` is accepted; `--local` writes identity whose cert URI SAN scopes the namespace; docker driver argv never mounts a runtime socket into a Worker.
- [ ] `wardenDriver` case `"docker"`. Start/stop/list/logs via `docker` CLI. Pair = two containers on a dedicated docker network, labels `KONTRA_WORKER=<name>/<version>/<part>`. `--cap-drop ALL`, `no-new-privileges`. No userns requirement (Docker Desktop). Reuse trustpolicy `Admit`. `--entrypoint` same as podman.
- [ ] `--local` mints a self-signed cert with `kontra:///ns/<ns>/warden/<wdn-…>` and writes `warden.json`. `--assignment <file>` reads desired state instead of `GET /warden/assignment`. Document as local-only bootstrap.
- [ ] `go test ./...` in `cli`.

## Task 4: Workspace seed/watch + recursive discovery

**Files:** `cli/workspace.go`, `control/orchestrator/src/sources.ts`, `sourceStore.ts`, tests, seed files under `examples/`

- [ ] Recursive `discover`: walk for `actor.json` / `workflow.json`. `SourceStore.list` also scans `KONTRA_WORKSPACE`.
- [ ] `kontra workspace seed`: if workspace has no entries besides install metadata, copy versioned `actors/hello` and `workflows/hello`. Atomic. No-op when non-empty.
- [ ] `kontra workspace watch`: debounce recursive watch; register via API; for actors run `kontra deploy --actor` so a digest-pinned worker image exists. Invalid manifests log path + error and do not block siblings.
- [ ] Hello actor Method pushes `{"message": "hello world"}`. Hello workflow: Dataset, `docker_fleet(machines=1)`, place `hello@0.1.0`, `ready()`, one Unit, return Dataset name, scope exit drops the Lease.

## Task 5: Compose cluster + orchestrator image

**Files:** `docker-compose.quickstart.yml`, `docker-compose.yml`, `.env.quickstart`, `control/images/Dockerfile.orchestrator`, `control/images/entrypoint.sh` (cluster init, not `kontra up`)

Services, each with a healthcheck, dependents using `condition: service_healthy`:

- postgres 16 (dbs: temporal via auto-setup, `kontra_ducklake`)
- temporal (`temporalio/auto-setup`)
- seaweed master / volume / filer / s3 + idempotent create-bucket
- redis
- registry:2
- orchestrator-api / materializer / infra (infra mounts docker.sock + pulumi state volume)
- orchestrator-probe (`kontra-host`)
- workspace-init (`kontra init` into named home volume + `workspace seed`; prints console login once)
- workspace-watch
- optional warden-ca (not required for `--local`)

Loopback publish for API, Temporal, S3, Redis, codec if still separate, registry. Infra Pulumi state in a named volume that `compose down` keeps. Bind `${KONTRA_WORKSPACE:-./workspace}` at the same absolute path. Document the Docker socket grant on infra and Warden.

`Dockerfile.orchestrator`: Node 22 bookworm, Pulumi CLI, compiled orchestrator, `@pulumi/docker` + digitalocean plugins. Roles via `KONTRA_ORCHESTRATOR_ROLES`.

Kontra image: keep building from the release tarball; install `docker-ce-cli` for the Warden docker driver; first-boot entrypoint becomes init/seed, not `kontra up`.

## Task 6: Docs and ADRs

- [ ] ADR 0047: local cluster is the supported install; 0031's measurements stay; packaging decision is superseded.
- [ ] Amend ADR 0034: local provider is now `dockerFleet`, Pulumi-backed; bare `fleet` remains reserved.
- [ ] Rewrite README Install, `docs/first-run.md`, wiki Getting Started / First Run / Deployment / Security. One topology. Documented commands require Docker only.
- [ ] Remove "Postgres evaporated" / embedded Temporal/Seaweed claims from user-facing docs.
- [ ] Destructive reset docs name Dataset loss and Pulumi Fleet state loss for `down -v`.

## Task 7: CI + install scripts

- [ ] Ubuntu job copies the two documented files into a temp dir (curl stand-in), points images at CI-built tags, runs the literal `docker compose up -d --wait`, then asserts spec verification bullets.
- [ ] `scripts/install-cluster.macos.sh` same assertions for Docker Desktop.
- [ ] Retire install-specific healthcheck of a `kontra` service. Assert every published HostIp is `127.0.0.1`.

## Task 8: PR against `dev`

- [ ] Logical commits. Push. `gh pr create` against `dev`. Return the URL.
