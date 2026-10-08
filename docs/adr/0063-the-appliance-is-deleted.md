# 63. The appliance is deleted, and there is one control plane again

Date: 2026-10-08

## Status

**Accepted.** Supersedes **0031** (the appliance: one binary owns the control plane). Leaves
**0047** (the local compose cluster) and **0052** (the install is a Pulumi program and the host runs
it) as the only two topologies, which is what both of those ADRs already assumed.

Numbering: 0062 is claimed by the live-report work in progress, so this appends rather than fills.

## Context

ADR 0031 collapsed the compose services into one process: Temporal embedded as a library, an S3 API
over the install's own data directory, a key-value store speaking the Redis wire protocol, the
payload codec as an HTTP handler, an OCI registry over the shared CAS, and the orchestrator carried
as a content-addressed bundle that `kontra up` hydrated and supervised as a child.

Then 0047 brought the compose cluster back as the documented install, and 0052 made the host Pulumi
engine the thing that converges it. From that point there were two control planes in one binary, and
the costs were not theoretical:

- **`kontra up` named the wrong one.** `cli/control.go`'s header recorded the swap it could not make
  — "ADR 0052 §2 is unambiguous that this IS `kontra up`" — and the word was held by the appliance,
  so the engine shipped as `kontra control up` with a comment explaining why.
- **Two release paths raced for one tag.** `release.yml` and `publish.yml` both triggered on `v*`
  and both pushed `<registry>/kontra:<version>`; every tag paid for the four native builds twice,
  including two billable macOS runners at ten linux minutes per minute.
- **A four-platform reproducibility matrix gated every pull request.** `appliance.yml` fired on six
  path patterns with `cancel-in-progress: true`, so on an active branch it was killed by the next
  push every time: eight runs on #24, seven cancelled between 18 and 58 minutes, and not one result.
  One of its jobs — `built twice, one digest` — was nonetheless counted as a red on a pull request
  about actor images.
- **Resolution orders grew a rung nothing could reach.** `registryAddress` and `resolveWorkerPlane`
  each consulted a running appliance's published endpoints before falling back to the compose
  defaults, and `lakeCatalog` tried the appliance's file catalog last. On a compose install those
  rungs were dead code that still had to be read, tested and explained.

## Decision

Delete it. Not deprecate, not keep behind a flag.

**Gone:** `cli/appliance/` (54 Go files); `kontra up`'s appliance, `kontra bundle`, `kontra release`
and the Temporal-UI wiring, with their tests; `.github/workflows/appliance.yml`;
`.github/workflows/release.yml`; `control/images/Dockerfile.appliance`; `get.sh`;
`install-appliance.sh` and its two test scripts; `publish.yml`'s `binaries` and `release` jobs;
`go.temporal.io/server` from `cli/go.mod`, which only the embedded server imported.

**`kontra up` is the host engine.** `case "up":` calls `cmdControlUp` and a new `case "down":` calls
`cmdControlDown`, exactly the one-line swap `control.go` described. `kontra control up|down` stays as
an alias so a script written against it does not break on a rename.

**The `kontra` image is built from source, in Docker.** `Dockerfile.selfcontained` compiled the
binary by running `kontra release` inside itself and then unpacking its own tarball; it now runs
`go build` and packs one archive — `bundles/kontra-spa.tar.gz` — because that member path is the
contract `Dockerfile.orchestrator` reads the console out of. The stage lost its Node and pnpm
install, which existed only to deploy the orchestrator bundle's production dependencies.

**Two things that look like the appliance are kept, renamed.**

- `cli/internal/testregistry` is the in-process OCI registry, moved out of `cli/appliance/registry`.
  zot serves this install's images; this package is imported only from `_test.go` files, so it never
  enters a shipped binary. It survives because `kontra build`'s Bundle push and `kontra deploy`'s
  manifest round trip need a registry to be tested against, and a unit test may not require a
  running zot. The package name is the invariant.
- `applianceDataDir` is `installDataDir` in Go (`cli/datadir.go`) and TypeScript
  (`control/orchestrator/src/data/dataDir.ts`). It resolves `$KONTRA_DATA_DIR`, else
  `$KONTRA_HOME/data`, and it is live: ADR 0031 §1b made the DuckLake catalog a file under it, and
  `parquet.ts:defaultCatalogPath` still resolves it that way. The two functions must stay twins.

**`workflows/appliance.ts` is `workflows/noProvisioner.ts`.** It is the workflow bundle the API role
registers, and what distinguishes it from `workflows/infra.ts` is that `stackWorkflow` is present as
a REFUSAL rather than absent — because an absent workflow type does not fail, it fails the task, and
Temporal retries that for ever (0031 §4). The new name says which of the two bundles it is.

## Consequences

**A tagged release carries no binary assets until GoReleaser lands.** release-please still creates
the tag and the GitHub release, and `publish.yml` still publishes every image, but nothing attaches a
`kontra` binary to it. This is stated in `publish.yml`'s header because an empty assets list reads
like a failed upload rather than a gap. GoReleaser publishing `kontra` and `kontra-warden` is the
next step of this phase.

**The four-platform reproducibility and cross-build claims go with the workflow.** `appliance.yml`
proved that a bundle was a function of the commit and the platform and of nothing about the build
host. Nothing asserts that now, because nothing ships a bundle; when GoReleaser does, the claim
belongs to it.

**`registryAddress` is configuration, not discovery.** With the appliance rung gone the order is
`--registry`, then `KONTRA_REGISTRY` (which compose sets for every service, so an install that moved
`KONTRA_REGISTRY_PORT` is followed rather than guessed at), then `localhost:5000`. A stopped registry
therefore still resolves, and `registryReachable` is what reports it — asserted, so that a future
change cannot quietly reintroduce retargeting.

**`make ui` and `make api` fail when the stack is not running.** Both had a branch that printed a
note and exited 0 when `orchestrator-api` was absent, because on the appliance it was not a
container. It is one again, so a missing container means the stack is down — and a recipe that
reports success while deploying nothing is the trap `ORCH_WEB` and `ORCH_DIST` already exist to
correct one level up.

**ADR 0031 is kept, not removed.** Several of its decisions outlived the packaging and are cited by
live code; §1b and §4 are named above. A reader who finds a reference to it should find the document.
