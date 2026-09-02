# 32. Actors are images: the digest is the identity, and the fleet stays native

## Status

**Accepted.** Supersedes **one clause** of `legacy/0019` — its consequence that a machine-target
actor's content pin is "the sha256 of the shipped **Bundle**" *because it has no image* — and keeps
every other clause, including the one that matters most here: fleet **Machines** run actor code
natively, with no container runtime. The clause-by-clause account is its own section below, and it
is the reason this record exists rather than being folded into the appliance ADR.

Makes **0024**'s carry-forward row shipped reality: *"Identity is immutable and content-pinned … it
pins to an OCI digest rather than a mutable tag."* Today that row describes nothing that runs.
**0011** is the argument for it; under **0024** rule 6 it is read here as evidence, never as
authority, and its file paths are dead.

Companion to the appliance decision (`.scratch/appliance/PRD.md`, locked decision 3 — Docker leaves
the control plane and stays for actors) and to **0031**, which keeps cloud provisioning on the
compose controller. That second one is load-bearing for the fleet question and is not re-decided
here.

## Context

Seven things were established against this checkout before deciding anything.

1. **Identity is a digest in the table and a tag in every line of code.** `cli/deploy.go` builds
   `kontra/<name>:<version>` and `kontra/<name>-worker:<version>`, tags the worker
   `<registry>/<name>:<version>` and pushes it; `deployResult` carries `Name`, `Version`,
   `HostImage`, `Image`, `Registry` — five strings, none of them a digest — even though the engine's
   push stream reports the digest it wrote. The re-deploy guard `versionDeployed()` asks
   `/v2/<name>/tags/list` and matches the *version string*, and returns `false` when the registry
   cannot answer, so it fails open. Nothing on this path can tell you what content a version
   currently names.

2. **`cli/scale.go` prefers a local tag over the pushed one.** `resolveWorkerImage` returns
   `kontra/<name>-worker:<ver>` whenever the daemon already holds it, and only falls through to
   `<registry>/<name>:<ver>` otherwise. A stale local build therefore shadows what was pushed,
   silently, on the Controller — which is exactly the drift **0011** was written about, in the one
   path that never leaves the box.

3. **Both SDKs already self-register a digest, and nothing anywhere supplies one.**
   `runtime/go/registrar/registrar.go:102` and `runtime/python/internals/catalog.py:61`
   read `KONTRA_ACTOR_DIGEST`; `control/orchestrator/src/catalog.ts` preserves it on re-registration
   (`a.digest ?? prev.digest`), and the three-way conformance fixture pins absent-versus-empty
   because an empty string would unpin on every restart. The producer **0011** named — a
   `scripts/build.sh` writing a per-actor `.digest.env` — is not in the tree; `.gitignore:17` still
   ignores its output. `cli/scale.go` writes five environment variables into a worker container and
   this is not one of them; `machine.ts` writes eleven into `/etc/kontra/worker.env` and it is not
   one of those either. Every actor in the catalog is therefore `unpinned`, which is precisely what
   **0030**'s `SourceProvenance` strip displays.

4. **The enforcement half of 0011 went with the interpreter, and the field stayed — still
   describing the check.** `expected_digest` is `entry.proto:76`, `ActorRunInput` field 7 in
   `actor.proto`, and `runtime/handler/internal/wire/wire.go:33`, and the proto comment states that "the
   worker self-verifies its running digest and fails fast (non-retryable, ActorDigestDrift) on
   drift". Nothing in `handler/` reads the field; neither SDK verifies it; `_verify_actor_digest`
   and `ActorDigestDrift` do not exist anywhere in the tree. A wire field with no gate behind it is
   worse than no field, because a reader finds it — with that comment attached — and concludes
   pinning is on.

5. **A Bundle's pin is the only content pin that is switched on.** `cli/bundle.go` tars the
   actor, the actorkit seam and the compiled handler with a **fixed mtime** so identical inputs
   produce an identical sha, identifies it by the sha256 of the tar.gz, and keys it
   `<name>/<version>/<sha>.tar.gz`. `machine.ts` re-hashes on arrival and `exit 1`s on mismatch.
   That is content pinning working, today, on the path 0019 predicted would get it first.

6. **The two Targets are already in the manifest, and only one of them has a dependency hook.**
   `actor.json` carries a `targets: { container, machine }` block (`cli/fleet.go`,
   `examples/python/webcrawl/actor.json`). One script serves both: the synthesized Dockerfile runs
   `deploy.sh` with `KONTRA_TARGET=container`, `machine.ts` runs the same file with
   `KONTRA_TARGET=machine`, and `cli/deploy.go` states the rule in its own words — *"an actor whose
   deps only exist in a Dockerfile cannot be placed on a Machine, and one whose deps only exist in
   deploy.sh silently ships a broken image."* Two hooks escape it, and both are container-only: an
   **author-supplied `Dockerfile`**, which suppresses the synthesized one and with it the `deploy.sh`
   step entirely, and **`runtime.Dockerfile`** for Go actors. Two of the repo's own examples use
   them. `examples/python/crawl4ai` installs `crawl4ai`, `simhash` and a Playwright Chromium in its
   own Dockerfile and ships no `deploy.sh`; as a **Bundle** it gets the machine install's four
   framework packages and nothing else, and dies at import — while `worker.env` helpfully sets
   `PLAYWRIGHT_BROWSERS_PATH` to a directory nothing populated. `examples/go/subfinder` installs the
   `subfinder` binary in `runtime.Dockerfile` and resolves it with `exec.LookPath` in `Load`; as a
   **Bundle** it fails with `subfinder not on PATH`. Both failures land after provisioning, minutes
   into a campaign.

7. **The registry service is opt-in and load-bearing at once, and its stated reason for existing is
   stale.** `docker-compose.yml` says *"every fleet Machine pulls it from `<controller>:5000`, so a
   control plane without a registry cannot run a fleet campaign at all"* — and then gives the
   service `profiles: ["extras"]`, so `docker compose up -d` does not start it. The sentence has
   also not been true since 0019: nothing under `control/orchestrator/src/infra/` mentions a registry, a
   port 5000 or Docker, because a **Machine** fetches a **Bundle** from the Controller's object
   store. Meanwhile `cli/deploy.go` defaults to `localhost:5000`, refuses to build when `/v2/` does
   not answer, and prints a `docker run … registry:2` telling the operator to hand-start the
   component the compose file claims to own. The registry's real and only customer is the local
   `deploy` → `scale` pair.

## Decision

- **An actor's shipped form on the container Target is an OCI image, and its identity is the
  digest that registry returned.** `kontra deploy` records the manifest digest from the push into
  `deployResult` and hands it on; a tag is a human convenience that keeps being pushed, and a run
  records the digest. This is **0024**'s row, implemented rather than tabulated.

- **One identity field, one shape, compared as an opaque string.** `KONTRA_ACTOR_DIGEST` carries
  `sha256:<hex>` on both **Targets**: the OCI manifest digest for an **Image**, the **Bundle**'s
  sha256 for a **Bundle** (finding 5 — already computed, already verified on arrival, currently
  written nowhere). Nothing parses it, nothing branches on which **Target** produced it, and no
  surface shows a tag where a digest belongs. This is not a weakening of "an OCI digest": an OCI
  descriptor digest *is* sha256 over the Artifact's bytes, and on the container **Target** it is
  literally that digest. The rule is the hash of the bytes that were placed, never a mutable name.

- **The pin is supplied at placement and enforced at the actor.** `cli/scale.go` writes the digest
  into the worker container's environment; `machine.ts` writes `KONTRA_ACTOR_DIGEST=sha256:$BUNDLE_SHA`
  into `/etc/kontra/worker.env` beside the check it already performs. Both SDKs then self-register
  it unchanged (finding 3) — the catalog gains a real digest with no client change. Enforcement
  returns where **0011** put it, at v2's boundary: the host compares the dispatch's
  `expected_digest` (`ActorRunInput` field 7, whose comment already promises this) against its own
  before the author's `Load` runs (`runtime/python/internals/temporal/host.py`,
  `runtime/go/temporalhost/host.go`) and fails **non-retryably** — retrying the wrong
  image is futile. **Unpinned stays legal and stays a no-op**: either side empty means no check,
  which is what keeps the dev inner loop and `kontra run` working exactly as they do now.

- **Images live inside the appliance.** `distribution` is embedded as a Go library over the CAS the
  binary already has (issues 11 and 12), serving one address that push and pull both resolve. A
  mismatch fails naming the address rather than surfacing as `no such image`. The two alternatives
  and why they lost are in *Alternatives considered*; the short forms are that an external registry
  means `docker compose` never fully goes away, and a remote one buys a rate limit this repo has
  already measured.

- **The fleet path stays native. Machines do not gain a container runtime.** Images become the
  unit on the container **Target** only. `legacy/0019`'s argument for this is untouched by anything
  the appliance does — the **Machine** is already the isolation boundary, one Worker per **Machine**
  because its value is a unique egress address, so a container there adds a Docker install in
  cloud-init and an insecure-registry exception on every **Machine** in exchange for isolation the
  **Machine** already provides. **0031** makes it worse than merely unnecessary: with cloud
  provisioning outside the binary, the appliance's embedded registry is not reachable from a
  **Machine** at all, so images-on-the-fleet would require running a registry container on the
  compose controller forever — the alternative rejected one bullet up, reintroduced through the back
  door.

- **So the two Targets diverge in Artifact and converge in identity, and that boundary is
  deliberate.** Two Artifact kinds (**Image**, **Bundle**), two build paths, two verification
  points — one identity rule, one field, one catalog column, one wire field. `infra/CONTEXT.md`
  already says every **Artifact** is content-pinned; this makes the pin the same *kind of thing* on
  both sides rather than two vocabularies that happen to rhyme.

- **`deploy.sh` is the portable dependency contract, and an actor that skips it is a container-only
  actor that must say so.** `kontra fleet up` refuses to build a **Bundle** for an actor whose only
  dependency hook is container-only — an author-supplied `Dockerfile` or a `runtime.Dockerfile` with
  no `deploy.sh` beside it — and the refusal names the file and the missing script. Finding 6 is why
  this is a decision and not a lint: the alternative to refusing at build time is discovering it as
  an `ImportError` or a `not on PATH` inside a systemd journal on a **Machine** that has already been
  provisioned and paid for.

- **The Docker split has one boundary and it is the CLI.** The binary contains no Docker client and
  no Docker dependency; `kontra up` runs on a machine that has never installed it. `kontra deploy`
  and `kontra scale` talk to the operator's daemon through the CLI's own client, as they do today,
  and are the only commands that can fail for want of it. Their failure message says Docker is
  required to *run actors*, not to run kontra — the distinction the whole appliance rests on.

## What survives from `legacy/0019`, clause by clause

0019 is cited as evidence (**0024** rule 6). Its clauses, each decided rather than left to be
discovered:

| `legacy/0019` clause | Fate |
|---|---|
| Pulumi replaces OpenTofu; programs are inline Automation API TypeScript under `control/orchestrator/src/infra/programs/` | **Kept**, untouched here |
| The engine runs in its own `orchestrator-infra` process, never `orchestrator-api` | **Kept**; where it runs after the appliance is **0031**'s |
| A Temporal Entity Workflow keyed by the stack fqn is the mutex | **Kept** |
| `DIGITALOCEAN_TOKEN` in `orchestrator-infra`'s environment only; `/api/infra/*` `checkBearer`-gated | **Kept**; **0031** restates where the credential lives |
| The control plane stays `docker-compose.yml`; Pulumi provisions the Controller and runs compose | **Split.** Superseded for the *local* control plane, which the appliance replaces; kept for the cloud Controller, which **0031** leaves on compose. The Pulumi-provisions-the-Controller half never shipped — there is no controller program under `infra/programs/`, and `scripts/provision-controller.sh` does that job |
| **Building stays operator-side**; moving `docker build` server-side would need the Docker socket mounted into a service on the machine holding the cloud credential | **Kept, and load-bearing.** It is why `kontra deploy` may hold a Docker client at all, and why the appliance's registry is a *store*, not a builder |
| Execution still never provisions; the infra queue is not addressable from a graph | **Kept**, as amended by `CONTEXT-MAP.md`'s `fleet.up()` door — not this ADR's seam |
| Cloud authority widens and is closed by `checkBearer` in the same change | **Kept** |
| The credential never reaches a worker **Machine** | **Kept.** Nothing here sends a credential to a **Machine**: a **Bundle** is world-readable on an anonymous store by design, and the digest that now rides beside it is a hash, not a secret |
| ADR 0018 is a prerequisite for the machine target; the native install is one handler binary, the actor, and a Redis endpoint | **Kept in full — this is the clause the collision was about.** See below |
| 0011 digest pinning switches on first on the fleet; a machine-target actor **has no image and therefore no git-SHA tag**, so the pin becomes the **Bundle** sha in `KONTRA_ACTOR_DIGEST` | **Superseded in form, kept in substance.** See below |
| Ansible's rolling `serial: 3` health gate must be rebuilt inside the Entity Workflow | **Kept**, still in flight |
| Pulumi asserts convergence rather than re-converging; the **Watchdog** is the only drift detector | **Kept** |
| Pulumi CLI and provider plugins baked into the orchestrator image at pinned versions | **Kept**; **0031** owns which deployment carries them |

**The Machine clause is kept in full, deliberately.** The appliance removes Docker from the control
plane and keeps it for actors; 0019 removed it from **Machines**. Read as slogans those collide.
Read as decisions they do not, because they are answers to different questions: the appliance asks
*what must be installed for the control plane to start*, and 0019 asks *what a Machine needs in
order to run one Worker with a unique egress address*. Both answers are still "as little as
possible", and for a **Machine** that is still a **Bundle** and two systemd units. A fleet
**Machine** is the one place in this system where a container was never buying isolation, because
nothing else shares the host.

**The identity clause is superseded in form.** 0019 derived the **Bundle** sha *from the absence of
an image* — "a machine-target actor has no image and therefore no git-SHA tag". That derivation
makes identity a property of the **Target**, which is the thing this ADR refuses: on the container
side the same reasoning is what leaves a tag standing in for a digest (findings 1 and 2). Restated:
the pin is the hash of the **Artifact**'s bytes on both **Targets**, the field is one field, and
0019's *value* for the machine **Target** is unchanged. What changes is that it stops being an
exception and starts being the general rule, and that the container **Target** stops being exempt
from it.

## Consequences

- **`kontra deploy` gains a digest and `kontra scale` starts from one.** `deployResult` carries the
  pushed manifest digest; `scale` resolves and runs `<registry>/<name>@sha256:…` rather than a tag,
  which closes finding 2 — a local `kontra/<name>-worker:<ver>` can no longer shadow the pushed
  image, because the reference names content. The local tag preference survives only for an
  unpinned dev build, where there is nothing to shadow.

- **The re-deploy guard becomes a content question.** `versionDeployed` currently means "this
  version string exists". It comes to mean "this version resolves to a different digest", so
  re-deploying identical content is a no-op that reports the digest, and re-deploying *changed*
  content under a used version is refused with both digests in the message. Its fail-open branch —
  cannot reach the registry, therefore proceed — stops being reachable in the appliance, where the
  registry is in the same process.

- **`registry` and `registry-data` leave `docker-compose.yml`, and the stale sentence leaves with
  them.** No **Machine** has pulled from `<controller>:5000` since 0019 (finding 7); nothing should
  be written that says otherwise. Cloud campaigns keep the compose controller (**0031**) and can
  keep running that service until the appliance's registry is reachable to them, but its comment
  must describe the local `deploy`/`scale` pair, which is what it serves.

- **Docker Hub does not leave, and this ADR does not claim it does.** Base layers still come from
  it: `infra/Dockerfile.pyworker` is `FROM python:3.13-slim`, and `cli/deploy.go`'s synthesized
  Dockerfiles use `golang:1.25`, `golang:1.26.4`, `alpine:3` and `debian:12-slim`. What the embedded
  registry removes is *actor image distribution* from a metered path — the part that runs on every
  deploy and every scale-out — leaving only base pulls, which the daemon caches.

- **The catalog's `unpinned` becomes rare, and therefore meaningful.** **0030**'s `SourceProvenance`
  strip starts showing a real digest beside a source file, which is the whole point of that strip: a
  disk that no longer matches what is serving becomes visible. Until this lands it shows the same
  word for every actor, and a field with one value is not information.

- **A drift becomes a clean non-retryable failure again, and that is a new way for a run to fail.**
  An operator who rebuilds without bumping and re-serves will see `ActorDigestDrift` where they
  previously saw a run that quietly used new code against an old contract. That is the trade 0011
  argued for and it is still the right one, but it is a behaviour change on a path that currently
  cannot fail, and it is why unpinned must remain a legal, silent state.

- **The same source has two digests when an actor targets both.** An **Image** and a **Bundle** are
  different bytes, so the same commit pins differently per **Target** — correct, since they are two
  different things that run, but it means "the actor's digest" is only well defined once the
  **Target** is known. The catalog holds the digest of what actually registered, which is the
  running Worker's own, so no surface has to reconcile the two.

- **A container-only actor is now named as such at build time.** `crawl4ai` and `subfinder` are
  container **Target** actors today and always have been (finding 6); the refusal makes that a
  property the manifest and the CLI agree on instead of a discovery three minutes into a campaign.
  The cost is real: making either of them fleet-placeable means moving their dependency install into
  `deploy.sh`, and for `crawl4ai`'s Chromium that is work, not a rename.

- **`kontra up` must be provable on a Docker-less machine.** The parity gate
  (`.scratch/appliance/PRD.md` decision 6) covers the actor path end to end, which needs a daemon;
  the *control plane* claim needs the opposite test — a box with no Docker installed, where
  `kontra up` serves the SPA and a workflow runs, and only `deploy`/`scale` fail, by name.

## Alternatives considered

- **Give fleet Machines a container runtime, so images are the unit everywhere.** Rejected on
  0019's own argument, which nothing in the appliance disturbs: one Worker per **Machine** means the
  container is isolating a process from nothing, and buying that costs a Docker install in
  cloud-init, an insecure-registry exception on every **Machine**, and a registry that must be
  reachable across the VPC for the lifetime of a campaign. **0031** adds the decisive fact — the
  appliance does not provision **Machines**, so the registry they would pull from is the compose
  container this decision is removing. The uniformity purchased is cosmetic; the identity rule is
  what actually needed to be uniform, and that is achieved above without a runtime.

- **Keep `registry:2` as a compose service and point the appliance at it.** Rejected because it
  makes `docker compose` permanent for the local path, which is the one thing the appliance exists
  to end: the operator would install a single binary and still need a compose file to deploy an
  actor. Finding 7 also shows the service is already the weakest link in the current stack — behind
  `--profile extras`, so a `docker compose up -d` does not start it, while `kontra deploy` refuses
  to build without it and prints a `docker run` to hand-start what compose was supposed to own.
  Shipping that shape forward preserves the bug and the topology together.

- **Push actor images to a remote registry (ghcr.io, Docker Hub).** Rejected on three counts, the
  first measured in this repo. **Rate limits:** anonymous pulls are budgeted per IP per hour in the
  low tens, and `examples/private/docker_leaks/registry_monitor/actor.py` records the measurement —
  2026-08-08, 25 of 30 requests returned 429 on a running deployment. That budget is shared with the
  operator's own `docker build`, so a busy box breaks its own builds. **Credential:** it puts a
  registry login in the appliance, contradicting the "`go install` from GitHub and it works"
  requirement (PRD user story 8) and the loopback-only posture. **Offline:** a local-first binary
  that cannot deploy an actor without the internet is not local-first.

- **Make identity the sha256 of the actor's source tree, uniform across both Targets.**
  Rejected on 0011's original ground, restated: the **Image** is what runs, and it contains a base
  image, a `deploy.sh` install and a pip resolution the source tree does not describe. Two builds of
  one commit can differ, and the digest must move when they do. A source hash pins what was written;
  an artifact hash pins what runs, and "what is actually running" is the question `infra/CONTEXT.md`
  makes every **Artifact** answer.

- **Leave `expected_digest` unenforced and treat the digest as display metadata.** Rejected: that is
  the current state (findings 3 and 4), and it is the failure mode **0024** was written about — a
  field, a proto comment and an ADR row all describing a check nothing performs. Either the digest
  is identity, in which case something compares it, or it is a label, in which case the wire field
  and the carry-forward row should go. This ADR takes the first branch.
