# Local cluster installation and Pulumi Docker Fleet

## Status

Approved on 2026-09-14. This design reverses the local packaging decision in ADR 0031. The
supported first-time installation is a Docker Compose cluster, not a single appliance process.
Kubernetes is explicitly out of scope.

## Goals

- A first-time user can install kontra by following one documented Docker Compose path.
- Postgres, Temporal, SeaweedFS, the state store, registry, and kontra processes have separate
  service boundaries and health checks.
- A bind-mounted workspace is the source of truth for actor and workflow registration.
- An empty workspace is seeded once with a runnable hello-world actor and workflow.
- The sample workflow provisions a local Fleet through Pulumi, places the actor, writes
  `hello world` to a Dataset, and destroys the Fleet when the Run finishes.
- CI executes the documented commands in a clean Linux environment. A macOS script verifies the
  same path under Docker Desktop.

## Non-goals

- Kubernetes, k3d, or another local orchestrator.
- Keeping the appliance as a second supported installation path.
- Automatically serving or running user workflows.
- Automatically overwriting or repairing files in a non-empty workspace.
- Changing the DigitalOcean Fleet's public behavior.

## Architecture

The installation uses one Compose project and one private network. Only operator-facing ports are
published, and every published port binds to `127.0.0.1` by default.

Long-lived services:

- `postgres`: persistence for Temporal and kontra's shared DuckLake catalog/materialization ledger.
- `temporal`: Temporal frontend, history, matching, and worker services, backed by Postgres.
- `seaweed-master`, `seaweed-volume`, `seaweed-filer`, `seaweed-s3`: S3-compatible Dataset and
  claim-check storage.
- `redis`: actor and global state using the existing RESP contract.
- `registry`: OCI storage for actor artifacts.
- `orchestrator-api`: catalog, console API, Dataset query surface, and SPA.
- `orchestrator-materializer`: Dataset materialization worker.
- `orchestrator-infra`: controller-pinned workflows and Pulumi Automation API for both
  DigitalOcean and local Docker Fleets.
- `orchestrator-probe`: the one-shot actor Method-call workflow.
- `workspace`: recursive manifest discovery and actor artifact reconciliation.

The existing kontra binary remains the CLI and Warden executable. Its embedded control-plane
services and appliance install path are retired from user-facing documentation and release gates.
Code may be removed in stages where immediate deletion would make the migration unsafe, but it is
not a supported topology after this change.

## Workspace

Compose bind-mounts `${KONTRA_WORKSPACES}` at the same absolute path in every service that reads
code. Blank env defaults to `../workspaces.kontra` beside the kontra checkout (sibling of `kontra/`
and `kontra-console/`). Using the same path preserves the catalog contract: a registered source
path means the same file to the CLI, discovery process, and orchestrator.

Each child of that parent is a named workspace. `.current` names the active child. On first launch,
`cluster-init` seeds `hello/` when the parent is empty:

- `hello/actors/hello/actor.json`
- `hello/actors/hello/actor.py`
- `hello/workflows/hello/workflow.json`
- `hello/workflows/hello/workflow.py`

It never replaces files in a non-empty parent. The console rail switches the current workspace;
Datasets and runs stay cluster-wide.

The `cli` service watches the current child thereafter. A directory containing `actor.json` is an
Actor; a directory containing `workflow.json` is a workflow. It handles create, change, rename, and
delete events idempotently. Invalid manifests appear in status with their exact path and error; one
invalid entry does not block valid siblings.

Workflow discovery updates catalog metadata only. The user must explicitly serve and start a
workflow. Actor discovery updates catalog metadata and builds/publishes the actor artifact needed by
Fleet placement, because the promised first-run path has no separate deploy step. A successful
actor registration is therefore deployment-ready and records an immutable digest. A failed build
is visible as a registration error and cannot leave a stale digest labelled current.

## Pulumi dockerFleet

`dockerFleet` is a provider-specific Fleet sibling to `do_fleet`, implemented as a Pulumi program
using `@pulumi/docker`.

It adds:

- a separate Pulumi project, `kontra-docker-fleet`;
- a Docker provider/program file, separate from the DigitalOcean program;
- provider-specific input coercion and SDK construction;
- no cloud credential or provider environment variable.

The existing `stackWorkflow` remains the only converge door. `fleet.hold(docker_fleet(...))` starts
it as a child workflow on `kontra-infra`; the program creates the requested Warden containers and
publishes provider-neutral Machine inventory. Existing Lease workflows own cleanup. Dropping the
last Lease asks the same stack workflow to destroy the Pulumi stack.

Each local Machine is a Warden container attached to the kontra network. It receives a stable
Machine identity and the control-plane endpoints. It mounts the host Docker socket and creates actor
worker containers as siblings on the same network. This socket grants host-level Docker authority;
the Compose file and security documentation must state that clearly. It is acceptable for the
single-operator local development topology and is not presented as tenant isolation.

Pulumi state lives in a named volume that `docker compose down` does not remove. The documented
destructive reset names both Dataset loss and possible Fleet state loss before using `down -v`.

## Starter Run

The seeded workflow takes no required input:

1. Create an output Dataset with a deterministic starter name.
2. Acquire a one-Machine `dockerFleet`.
3. Place the seeded `hello@0.1.0` actor on it.
4. Wait for its poller to become ready.
5. Dispatch one Unit to the Actor.
6. The Actor pushes one record containing `{"message": "hello world"}`.
7. Return the Dataset name.
8. Exit the Fleet scope, drop the Lease, and wait for Pulumi destruction.

Cancellation runs the same scope exit. Termination retains its existing unsafe semantics and is not
used in the walkthrough.

## Installation interface

README and First Run expose one installation:

1. Create an empty installation directory.
2. Download the version-pinned Compose and environment files using the documented authenticated or
   public-release URL.
3. Create `workspace/`.
4. Pull images.
5. Run `docker compose up -d --wait`.
6. Read the one-time console login.
7. Serve the discovered starter workflow from the workspace runner.
8. Start it from the CLI service.
9. Query the output Dataset and confirm the local Fleet has been removed.

Commands use the shipped containers and require Docker only. They do not depend on a source
checkout, a host Python, Go, Node, or a separately installed kontra binary.

## Failure handling

- Every dependency has a health check; dependent kontra services wait for health rather than
  container creation.
- Schema/bootstrap jobs are idempotent and fail before kontra workers start when persistence cannot
  be prepared.
- Workspace seeding is atomic and never writes into a non-empty workspace.
- Discovery reports per-entry failures without discarding the last known-good catalog entry until a
  replacement artifact is ready.
- `dockerFleet` validates Docker availability and image digest before creating Machines.
- A partial Pulumi converge is compensated by the existing stack saga and remains addressable by
  the same stack FQN.
- Fleet teardown is asserted, not fire-and-forget, before the starter Run reports success.

## Verification

The clean-install gate executes the literal documentation path in a fresh temporary directory and
asserts:

- Compose configuration parses with no checkout-specific values.
- Every required image can be pulled and every service becomes healthy.
- Postgres backs Temporal and kontra; S3, Redis, registry, API, materializer, infra, and probe answer
  real operations.
- The printed password signs in and a wrong password does not.
- The empty workspace is seeded once and a restart does not modify it.
- Recursive discovery registers both starter manifests.
- Serving and starting the starter workflow completes successfully.
- The output Dataset contains exactly one `hello world` record.
- Pulumi creates a Warden Machine, the actor poller becomes ready, and the Warden and actor
  containers are gone after completion.
- Recreating the Compose project preserves credentials, catalog state, and Dataset state.
- Every published port is loopback-only by default.

Ubuntu runs this as a required CI job. A repository script runs the same assertions on macOS Docker
Desktop; CI may perform the macOS smoke when Docker is available, while the script remains the
release checklist fallback where hosted macOS runners cannot provide nested Docker reliably.

## Documentation and decisions

- Add an ADR that supersedes ADR 0031's single-appliance packaging decision while retaining its
  historical measurements.
- Amend ADR 0034: the local provider is now `dockerFleet`, and it is Pulumi-backed.
- Update README, Getting Started, First Run, Deployment, and security documentation to one topology.
- Remove statements that Postgres, Temporal, and SeaweedFS are embedded or have evaporated.
- Replace appliance installer/release-path gates with the modular cluster clean-install gate.

## Compatibility

The Actor, workflow, Dataset, Warden, Lease, and provider-neutral Machine contracts remain intact.
`do_fleet` remains supported. The intentional breaking change is operational: `kontra up` and the
single `kontra` Compose service are no longer the supported local control plane.
