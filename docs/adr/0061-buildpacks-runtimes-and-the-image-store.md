# 61. Buildpacks, runtimes and the image store

Date: 2026-10-07

## Status

**Accepted. The image store is landed; the build pipeline is decided and measured, not yet built.**

Numbering: the spec proposed `0054`, which is taken (session scopes). 0055 is a gap in the sequence
and 0053 is claimed by uncommitted work, so this appends rather than fills.

## Context

Two problems that turned out to be one, and the second is what made the first urgent.

**The build reinstalls everything, every time.** The generated Dockerfile does `COPY . /actor/<name>/`
then `RUN deploy.sh` (`deploy.go:926-947`), so every version stores its own copy of its dependencies,
Chromium included. Go actors are worse: they use the repo root as build context and `COPY . .`
(`deploy.go:624-684`), because their `go.mod` `replace`s point outside the actor directory — so any
change anywhere in the repo invalidates the build. The classic builder is used **on purpose**
(`deploy.go:955`), which means no cache mounts and intermediate images that pile up.

**Nothing ever deleted anything.** `registry:2` has no online garbage collection and this install had
no retention policy at all. Measured at the time of writing: **7.9 GiB** in 20 repositories, on a
154 GiB disk that was **92% full**, with **28 dangling build intermediates** averaging 2.2 GiB behind
it — 30 GiB of pure leak, which is what `deploy.go:955` leaves behind.

The sizes are the argument. A Python actor built by the current path is **1.44–2.26 GiB**
(`kontra/webcrawl-worker:0.2.3`, `kontra/reddit-worker:0.1.0`).

## Decision

### Cloud Native Buildpacks, with a pinned stock builder

`pack` with `heroku/builder:24`, pinned by digest. Measured against a real actor published to a real
zot:

| | generated Dockerfile | buildpacks |
|---|---|---|
| actor image | 1.44–2.26 GiB | **183 MiB** |
| one-line code change | reinstalls every dependency | **0.133 MiB of new blobs** |
| dependency layer | rebuilt | `Reusing layer 'heroku/python:venv'` |

Rejected, with reasons rather than preferences:

- **BuildKit only** (cache mounts on the existing Dockerfiles) would fix the cache and keep ~700 lines
  of Dockerfile generation, three escape hatches and the two-images-per-version shape. It fixes the
  symptom this ADR is named after and none of the structure.
- **Railpack** is young and its provider set is narrower than Heroku's; the thing being bought here is
  someone else's maintained Python and Go providers.
- **`ko`** builds Go only. Half the actors are Python.
- **Nix** would be a second package manager, a second lockfile format and a second thing to teach, for
  reproducibility stronger than this needs.
- **A custom layer assembler** is the thing CNB already is, and writing one is how the 700 lines got
  here.

### Runtimes are CNB run images in their own repository

`kontra-runtimes`, declared in `actor.json` by name and major (`python-browser:1`), resolved against
`KONTRA_RUNTIMES_PREFIX`, and **pinned by digest at build time** — the digest goes in the catalog
entry. Discovery is a registry query under `kontra-runtimes/`, so adding a runtime is adding a
directory, never editing a list in kontra.

### Rebase: a version maps to a sequence of digests

`pack rebase` moves an actor image onto a newer digest of the same runtime major by rewriting its
manifest. ADR 0032's "the digest is the identity" holds unchanged — what changes is that a *version*
no longer names one digest. The catalog says which is current and keeps the rest in a history list.

**This is the one piece with nowhere to land yet.** `catalog.ts:195-203` and `repo.ts:351-381` hold one
overwritable `digest` per `name@version`. Rebase before that is a digest nothing can account for, so
the catalog's `runtime` and `builderDigest` fields come first.

### Untrusted builder mode is a security requirement, not a default

`--trust-builder=false`, always. In untrusted mode the lifecycle runs its phases in separate
containers and **only the exporter gets registry credentials**, so a package install script running
during a build cannot read a push credential. An actor is third-party code by construction; this is
the property that makes building it on the controller defensible at all.

Two consequences the spec did not anticipate, both measured:

- The lifecycle phases are **separate containers**, so the registry address must resolve from inside
  them. `127.0.0.1:5000` is their own loopback. This is the same two-spellings problem
  `docker-compose.yml:137-141` already documents, arriving somewhere new.
- `--insecure-registry` is **required**, because zot speaks plain HTTP by construction
  (`ociref.json`'s `push` section already pins `plainHTTP` per case for this reason), and the
  builder's credential must be **provisioned as a file** — `docker login` refuses plain HTTP to a
  non-loopback host.

### zot replaces registry:2

Pinned to `v2.1.21` by digest. `registry:2` floated, and `Pulumi.yaml:376` called it "the ONE unpinned
third-party image". `v2.1.22` existed when this was written, one day old; the registry holds every
image the install owns, so a month of soak is worth more here than a day of news.

**The service keeps the name `registry`.** `images.ts:62` hardcodes `http://registry:5000` and
`KONTRA_REGISTRY_URL` — the variable that would redirect it — is set by nothing in any compose or env
file. A rename silently empties `/api/images`.

The configuration is **rendered by a one-shot** into a volume, not added to the repo:
`assert-install-is-self-contained.py` permits three paths on disk and no other bind mount, which is
the same reason `temporal-dynamicconfig` and `ducklake-init` are one-shots.

Four things are load-bearing and were each found by running it, not reading:

| | why |
|---|---|
| `http.compat: ["docker2s2"]` | without it zot answers `manifest invalid` to every `docker push`, and every image here is schema-2 |
| healthcheck on `/v2/_zot/ext/mgmt` | `/v2/` answers 401 once auth is on, and `orchestrator-api` and `cli` both gate on `registry: service_healthy` |
| a static busybox carried into the image | zot's image holds exactly one executable and no shell |
| `\\.` in the retention regex | `\.` is not a legal JSON escape, so a single backslash means zot never binds its port |

#### Retention, and why it reports before it deletes

| repository | keep |
|---|---|
| `actors/*-cache`, `*-cache` | the most recent only |
| `kontra-runtimes/**` | every `<major>`, plus 3 most recent `<major>.<minor>.<patch>` |
| `bundles/**` | every tag — a Bundle is not an actor image |
| `actors/**`, `*` | 5 most recently pushed, **plus every `^inuse-`** |
| untagged | deleted after 24 h (`retention.delay`, not `gcDelay`) |

**`actors/<name>` does not exist yet.** This install has twelve bare repository names and one
`bundles/` prefix. The policies name both shapes, because keying only on the spec's future naming made
retention match nothing — which is the one thing this step exists to prevent.

`KONTRA_REGISTRY_RETENTION` defaults to **`dryrun`**. "Keep the 5 most recently pushed" is only safe
once something protects a version older than those five that is still placed on a Machine, and that is
what `inuse-` tags are for. Enforcing before that reconciler exists deletes a running actor's image.

**One divergence that cannot be expressed:** "the last 3 patch tags **per major**" has no equivalent —
`mostRecentlyPushedCount` counts within a repository, not within a group. Encoded per repository, which
is equivalent while a runtime has one active major and divergent the moment it has two.

**And one non-determinism worth knowing:** zot records the push timestamp **per manifest digest**, not
per tag. Several tags on one digest therefore have no order, and `mostRecentlyPushedCount` picks an
arbitrary subset. Any flow that puts two names on one manifest — a no-op rebase, a retag,
`inuse-<digest>` beside its version tag — inherits that.

#### Auth is opt-in, and a partial credential set is refused

Three roles (`push-actors`, `push-runtimes`, `pull`) when all three passwords are set; anonymous when
none are. Anonymous is what `registry:2` always was and the loopback publish is its control, so this
breaks no existing client — and the CLI has no registry-credential plumbing at all, so unconditional
auth would 401 every push and pull in the system at once. The VPC overlay makes the passwords `:?`
required, beside the publish that creates the exposure, exactly as `KONTRA_REDIS_PASSWORD` does.

Two refusals rather than defaults:

- **A partial set is refused.** A config with `http.auth` and an empty htpasswd answers 401 to
  everything, which reads as a broken install rather than a locked one.
- **No `adminPolicy`.** One granting `push-runtimes` delete would make it registry-root over
  `actors/**` — and that is the credential handed to whoever is iterating on a runtime. The
  least-trusted of the three would have been the most powerful.

#### zot accepts a delete where registry:2 refused one

Measured: anonymous `DELETE /v2/<repo>/manifests/<digest>` answers **202** and the repository is gone;
`registry:2` answered 405 `UNSUPPORTED`. The swap therefore converts an append-only store into a
destructive one, on the port the VPC overlay publishes to the fleet network.

So even the anonymous rendering carries an `accessControl` of `["read","create","update"]`. Every
existing push path still works with no credential; delete is refused.

### The store is migrated, and the migration is proved

`kontra registry migrate` copies by tag and then **by digest**, using `oras-go`, which was already a
dependency.

The by-digest pass exists because of a measurement: 27 of 27 tags copied with matching digests and
**two catalog digests still did not resolve** — `desync@177f80c8` and `webcrawl@e09d6df4`, both
present in the old store, both reachable by no tag, because a later push had moved the tag while the
catalog kept the older digest. ADR 0052 names the stake exactly: this store "holds the digests
Placements are pinned to … rebuilding an image yields a NEW digest, so a Fleet recorded against the
old one can never be re-run as recorded."

**An unmigrated install refuses to start.** The one-shot mounts both stores read-only and compares
them: old store has repositories, new store has none → refuse, naming the count and the command. The
condition is the state itself rather than a marker file, because a marker in a named volume is a
release condition nothing can reach.

Why a refusal and not a warning: nothing here distinguishes an empty registry from a fresh one.
`GET /api/images` answers 200 with `[]` and no `unreachable` flag; the console's drift reads `unknown`
and draws `?`; `kontra doctor` has no registry check at all; and `versionDeployed()` — the `--override`
guard — returns false for every version, so version immutability stops guarding without a word.

## Consequences

- **Dependency layers are shared and the cache lives in the registry.** 0.133 MiB for a code-only
  change, measured.
- **`pack rebase` patches the OS under every actor without rebuilding any of them**, once there is
  somewhere to record a sequence of digests.
- **Actors remain ordinary OCI images**, so ADR 0032, signing and trust policy are untouched in shape.
- **`/workspace` replaces `/actor/<name>/`**, and the identity env vars stop being baked in with `ENV`
  — the Warden sets them at container start from the catalog entry. Every reader of the old path has
  to move with it.
- **The install gained two one-shots and a second registry volume.** `registry-data` stays until the
  operator removes it; it is declared and mounted read-only so it is inside the project, because ADR
  0052 left it without `protect: true` precisely *because* retention bounded it, and an unreferenced
  volume is what `docker volume prune` takes.
- **zot stores the same content smaller.** 4.7 GiB against `registry:2`'s 7.9 GiB for identical
  content — 40%, from dedupe.

### What this does NOT do, and the reason is concrete

- **Nothing writes an `inuse-` tag yet**, so retention stays in `dryrun`.
- **The Pulumi program still declares `registry:2`.** ADR 0052 says the install *is* a Pulumi program,
  so `kontra control up` converges the registry backwards. The check that would catch it — `parity.py`
  — had to be repaired first: a bare `next()` turned its whole report into a `StopIteration` the moment
  compose depended on a service its map did not know.
- **No signing.** Nothing in kontra signs an actor image today; cosign exists only for kontra's own CI
  images. §7's "sign it" is net-new work, not reuse.
- **`images:admin` is not implemented and contradicts ADR 0054 as specified.** Two scope strings exist,
  `mint()` grants only `console`, and there is no user→scope mapping. ADR 0054's model is a
  service-token capability a session can never inherit; a scope the console must *use* needs its own
  position before the Images page is built.
- **The builder service is unbuilt.** There is no rootless podman socket on the controller and root
  cannot have one, so "the builder holds the rootless podman socket, not docker.sock" needs a non-root
  build user created first.

## Tests

- `cli/registrymigrate_test.go` — Link paging is followed on both the catalog and the tag list
  (a registry that pages against a client that does not is a silently partial migration); an
  undecodable page and a 500 are errors rather than empty catalogs; migrating a registry onto itself
  is refused by name.
- Verified by hand against the live store, because a 7.9 GiB two-registry copy is not a unit test:
  20 repositories and 27 tags on both sides, **27 identical by digest, 0 not**, plus the two untagged
  catalog digests, ending in "all 4 catalog digests resolve".
- `cli/envexample_test.go` covers the five new compose variables; it caught all of them.
- Compose behaviour was verified by running the real service definition under a throwaway project
  name: `Healthy` on the exec-form healthcheck, anon 401 / `pull` 200, `pull` refused a push with 403,
  `push-actors` refused a runtime push with 403, a real schema-2 `docker push` accepted, GraphQL
  search answering, and anonymous `DELETE` refused.
- **A note on the retention test.** `zot verify-feature retention` exists and stalled; the honest
  check was an enforce run with a short `gcInterval` against real repository shapes, reading the tag
  list before and after.
