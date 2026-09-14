# 47. The supported local install is a Compose cluster, and dockerFleet is Pulumi

## Status

**Accepted, 2026-09-14.** Supersedes **0031**'s packaging decision that the supported first-time
install is one appliance process. It does not reopen **0031**'s measurements (uid mismatch,
Seaweed 403 on a missing bucket, volume-slot 500, SIGKILL of a DuckLake lock, loopback vs ufw).
Those remain true of the topology they described. This ADR changes which topology we ship.

Amends **0034** §3: the reserved local provider is now `dockerFleet`, and it is Pulumi-backed.

## Context

**0031** collapsed twelve Compose services into one binary so a first-time installer could not
mis-assemble them. That collapse also hid service boundaries: Postgres, Temporal, SeaweedFS, Redis,
the registry, and three orchestrator roles became one PID, one healthcheck, and one volume. Fleet
provisioning stayed off that PID (**0031** §4, **0034** §1) because Pulumi's Node host installs
process-global rejection handlers.

Two pressures reversed the packaging choice without reversing those measurements:

1. A first-time user still has to bring up Temporal, an object store, and a catalog, and they need
   those to fail independently. One container that is "up" with a child that never bound is the
   failure **0031** already named; splitting services with `depends_on: condition: service_healthy`
   is how Compose answers it.
2. The first workflow we seed must provision capacity on the same machine. **0034** reserved the
   bare word `fleet` for a local Docker case it did not build. Building that case as a second
   runtime path (direct Docker API, no Pulumi) would have made `do_fleet` and local capacity two
   different kinds of thing. They are not: both are a Pulumi stack keyed by FQN, converged by
   `stackWorkflow` on `kontra-infra`, cleaned up by Lease drop.

Kubernetes is out of scope. The appliance remains in the binary so a migration is not a delete, and
it is not a supported install path.

## Decision

### 1. One Compose project is the supported local control plane

The documented install copies two files into an empty directory and runs `docker compose up -d
--wait`. Services are separate processes on one private network named `kontra`:

- Postgres 16 (Temporal auto-setup databases, plus `kontra_ducklake`)
- Temporal (`temporalio/auto-setup`)
- SeaweedFS master / volume / filer / s3, with an idempotent bucket create
- Redis (existing RESP contract)
- `registry:2`
- `orchestrator-api`, `orchestrator-materializer`, `orchestrator-infra` as separate PIDs
- `orchestrator-probe`
- `workspace-init` (once) and `workspace-watch`
- `cli` (exec target for serve/start)

Operator ports bind `127.0.0.1` by default. The Docker socket is mounted on `orchestrator-infra`,
`workspace-watch`, and each local Warden. That is host-level Docker authority, documented as
acceptable only for a single-operator laptop.

### 2. `dockerFleet` is a Pulumi sibling of `do_fleet`

Project `kontra-docker-fleet`. Program `@pulumi/docker`. No cloud credential (`providerEnvVar` is
empty). Each Machine is a Warden container on the `kontra` network; actor Workers are sibling
containers started through the host Docker socket. Inventory is the same `{name, host, publicIp,
tag}` shape DigitalOcean publishes. Lease drop destroys the stack. Cancellation is scope exit;
termination is not used in the walkthrough.

SDK: `docker_fleet(...)` / `class Docker`. Implicit `fleet.up()` without a provider object remains
DigitalOcean.

### 3. The workspace is the source of truth, and discovery does not run code

`${KONTRA_WORKSPACE:-./workspace}` is bind-mounted at the same absolute path in every service that
reads code. An empty workspace is seeded once with `actors/hello` and `workflows/hello`. A
non-empty workspace is never overwritten. Recursive discovery registers manifests; it does not
serve or start workflows. Actor discovery builds and publishes a digest-pinned worker image so the
first-run path has no separate deploy step.

### 4. Documented commands are the CI gate

Ubuntu CI copies the two files into a fresh directory (curl stand-in), points images at tags built
from this commit, runs the literal `docker compose up -d --wait`, and asserts login, seed-once,
starter Run, Dataset content, Fleet teardown, restart persistence, and loopback publishes.
`scripts/install-cluster.macos.sh` is the same path under Docker Desktop.

## Consequences

- `kontra up` and a single `kontra` Compose service are no longer the supported local control
  plane. Docs, README, and the clean-install job describe one topology.
- `orchestrator-infra` runs in the local cluster so `docker_fleet()` can converge. It still must
  not share a PID with the API or the materializer (**0019**). A DigitalOcean token is optional and
  unused for `dockerFleet`.
- Pulumi state lives in a named volume that `docker compose down` keeps. `down -v` is documented as
  destroying Datasets **and** local Fleet state.
- Seaweed's missing-bucket 403 and Temporal's uid mismatch are handled as Compose jobs
  (`seaweed-bucket`, official images creating their own files), not by collapsing the store into
  the binary.
