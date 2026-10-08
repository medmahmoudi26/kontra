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

## The build path that replaced it, and two things it forced

ADR 0061 decided Cloud Native Buildpacks and measured them; `packBuild` was written and **never
called**. Wiring it deleted the Dockerfile path — `ensureBase`, `buildActor`, `buildGoActor`,
`buildWorker`, `ensureWorkerBase`, their two generated Dockerfiles, `control/images/Dockerfile.pyworker`
and `control/images/Dockerfile.workerbase` — and with them the SDK-digest mechanism
(`sdkLabel`/`sdkDigest`/`isCheckout`), which existed only to tell a stale Python base image from a
current one. An actor's SDK comes from its runtime now, pinned by digest and recorded in the catalog.

**`pack` BUILDS INTO THE DAEMON AND DOES NOT PUBLISH.** This is the one decision here that is not
obvious, and it is a split this repository has paid for before — `registryAddress`'s header calls it
"the oldest bug on this path". `pack --publish` makes the *lifecycle* push, from inside a container,
so the image reference would have to be `registry:5000`: a name only the compose network resolves.
The thing that *pulls* an actor image is the Docker daemon, on the host, for which the resolvable
name is `127.0.0.1:5000`. One reference cannot be both, and a push the puller cannot reach shows up
as `no such image` at scale time, three commands from its cause. So the build lands in the daemon
and the daemon pushes. The cost is `--cache-image`, which pack accepts only with `--publish`: the
dependency cache is lost and the correctness is kept.

**THE PUSH CREDENTIAL WAS `base64("{}")`.** That is the Engine API's canonical "no credentials", and
it had been there for as long as the install has generated zot accounts — `push-actors`,
`push-runtimes` and `pull`, with a per-repository policy. An anonymous registry accepts it; an
authenticated one answers 401, from inside a push, with nothing in the message about a credential.
So: the account and password reach the two services that push (`cli` and `orchestrator-infra`, where
`activities/buildActor.ts` spawns `kontra deploy`); the Engine API header carries them; `pack` gets
them through a `DOCKER_CONFIG` directory this process makes at 0700 and removes; and a **preflight**
asks `GET /v2/` *before* the build, so a credential problem is a sentence naming the user, the
registry and the variable rather than a 401 after minutes of lifecycle. CI now runs the
authenticated shape, because the anonymous default is the one configuration in which this bug cannot
happen.

**"ANONYMOUS PULL" DOES NOT MEAN "NO AUTHORIZATION HEADER".** Measured: ghcr answers an
unauthenticated manifest read of a *public* repository with `401` and a
`WWW-Authenticate: Bearer realm=…` challenge. `docker` and `pack` follow it; this CLI did not, so a
runtime a human can open in a browser reported as "not in the registry". `registryManifestDigest`
now follows a Bearer challenge to the realm the registry named, for the scope it asked for, sending
no credential of its own.

**THE RUNTIMES ARE MIRRORED IN, NOT RESOLVED WHERE THEY ARE PUBLISHED.** Reading ghcr was the first
answer: point `KONTRA_RUNTIMES_PREFIX` at `ghcr.io/medmahmoudi26/kontra-runtimes` and a fresh
install can build the actor it ships. The install refused it, and correctly — `runtime
ghcr.io/…/python:1@sha256:712b8d2e… is not admissible under this install's trust policy, and it
would be the base of every actor built on it: image registry is not on this Machine's allowlist`.
Admitting `ghcr.io` is not expressible: the published runtimes are keyless-signed PER RELEASE —
measured, identity
`https://github.com/medmahmoudi26/kontra-runtimes/.github/workflows/publish.yml@refs/tags/python/1.0.0`,
issuer `https://token.actions.githubusercontent.com` — and `trustpolicy` takes one exact
`--certificate-identity` with no regexp, so one string cannot cover three runtimes signed under
three tag refs. Adding ghcr to `KONTRA_TRUST_UNSIGNED` would discard a signature that exists.

So `kontra runtime import` copies them into this install's registry, where the address is already on
both the allowlist and the unsigned list, and `KONTRA_RUNTIMES_PREFIX` keeps its computed default.
Three decisions inside it are not obvious:

- **The copy is `oras.Copy`, not the daemon.** `docker pull && tag && push` would have put a docker
  CLI and a socket in the path and flattened a multi-arch runtime to whatever the local daemon runs.
- **The destination address is resolved for THIS PROCESS, not for the daemon.** `127.0.0.1:5000` is
  zot as the host sees it; the copy runs inside the `cli` container, where that is its own loopback.
  `reachableRegistryHost` picks the way in that answers. Only the transport differs — a manifest is
  stored under its repository, so the actor build that later pulls through the daemon gets these
  bytes.
- **A major that has MOVED at the source is reported, never replaced.** First boot runs the import
  on every start; silently re-copying would advance the base image under every actor already built
  here, which ADR 0061 makes `kontra rebase`'s decision and not a side effect of a restart.

`KONTRA_RUNTIMES_IMPORT` names the set, in configuration rather than in Go: 0061 §3.4's invariant is
that adding a runtime is pushing one. Discovery cannot carry it — `GET /v2/_catalog` against ghcr
answers **403 DENIED**, measured — so the published set is named and a fork edits one `.env` line.

**"AUTH" WAS NOT ONLY ABOUT PUSHING, AND THAT IS WHERE THE 401 ACTUALLY LANDED.** With the three
passwords set, the rendered `accessControl` gives every repository tree `"defaultPolicy": []` — so
there is no anonymous READ either, and every registry read in this system was unauthenticated. The
install proved it: with the runtimes mirrored in, `kontra deploy` still refused with
`runtime "python:1" (127.0.0.1:5000/kontra-runtimes/python:1) is not in the registry: registry
127.0.0.1:5000 answered 401 Unauthorized`. Three consequences were silent rather than loud:

- `versionDeployed` skipped every 401 base and answered "not deployed", which turns **version
  immutability off** on exactly the installs that locked their registry down.
- `listRuntimes` returned nothing, so the refusal above said "NO runtimes are published" about a
  registry holding three.
- The orchestrator's Images surface — `images.ts`, `images/zot.ts` and the `inuse-` reconciler, which
  *writes* tags — swallows its own failures by design, so an authenticated install showed an empty
  Images page and protected nothing while retention was armed against it.

So a read carries a credential now, in both languages (`cli/registryauth.go:readCredential`,
`control/orchestrator/src/images/registryAuth.ts`), with the least-privileged account that is set —
`pull` before either push account — and **only to this install's registry**, by every spelling
`registryProbeBases` knows. A read of ghcr still answers its challenge with a token and no
credential of its own, because sending an install's password to somebody else's registry on the
strength of a 401 is the leak that a "fix the 401" instinct produces. A tag write picks its account
by namespace, because `push-actors` and `push-runtimes` cannot write each other's tree and one
client credential therefore cannot cover both.

**A BARE ADDRESS MEANS PLAIN HTTP ONLY FOR THIS INSTALL.** `registryBase` read every scheme-less
address as `http://`, which is right for `127.0.0.1:5000` (ADR 0036 keeps loopback without TLS) and
wrong for a public one: `http://ghcr.io` redirects to HTTPS, the token did not survive the hop, and
an anonymous read of a PUBLIC image came back `401 unauthorized: authentication required` —
indistinguishable from a private package. A host that is neither loopback nor one of this install's
own names is HTTPS, with HTTP kept as a second base so an operator's plain-HTTP VPC registry named
without a scheme still resolves.

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
