# Runtimes

A **Runtime** is the operating system an actor runs on: the OS plus the system packages, published as
an image and chosen by name in `actor.json`. It is a *Cloud Native Buildpacks* **run image** —
the thing the builder lays your dependencies and your code on top of, not a base image you write a
`FROM` against.

> [!IMPORTANT]
> **Shipped.** `kontra deploy` is the buildpack path and nothing else — the generated Dockerfile and
> its worker-base image are deleted — `kontra runtime import|list` and `kontra rebase` exist, and
> first boot mirrors the published set into this install's registry.
> [ADR 0061](../adr/0061-buildpacks-runtimes-and-the-image-store.md) records the decision and
> [ADR 0063](../adr/0063-the-appliance-is-deleted.md) what wiring it forced.
>
> One gap is left and it is in the *packaging*, not here: a tagged release carries no `kontra`
> binary until GoReleaser lands.

## Why this exists

The build path a runtime replaces reinstalled every dependency on every code change, because the
generated Dockerfile did `COPY . /actor/<name>/` and then ran `deploy.sh`. Measured on the install
this was written against:

| | generated Dockerfile | buildpacks |
|---|---|---|
| a Python actor image | 1.44–2.26 GiB | **175.4 MiB** (`actors/probe:0.2.0`, 9 layers) |
| a one-line code change | reinstalls everything, Chromium included | **0.133 MiB of new blobs** |
| the dependency layer | rebuilt | `Reusing layer 'heroku/python:venv'` |

The runtime is where the *shared* half of that lives. Layers are content-addressed, so every actor on
the same runtime references the same runtime layers and the registry stores them once — which is the
difference between ten actors costing ten OS images and costing one.

## The three layers of an actor image

An actor image is three kinds of layer, stacked bottom to top. Which one a change lands in is what
decides whether a deploy is cheap.

| kind | holds | changes when |
|---|---|---|
| `runtime` | the OS and its system packages — an interpreter, Chromium, fonts, CA certificates | the runtime is republished. **Shared by every actor on it** |
| `deps` | one buildpack layer per dependency set, e.g. `heroku/python:venv` | the lockfile changes |
| `app` | your code | you edit a file. This is the 0.133 MiB |

The kinds are **read from the image**, never guessed: they come out of the
`io.buildpacks.lifecycle.metadata` label on the image's config blob. See
[[The-Images-Page]] for the two measured traps in reading it.

## Declaring one

One field in `actor.json`:

```json
{
  "schemaVersion": "kontra.actor.v1",
  "name": "webcrawl",
  "version": "0.3.0",
  "runtime": "python-browser:1"
}
```

**A name and a MAJOR**, not a version. That is the point of the field: the runtime can be patched
underneath an actor — a CVE in a system library, a newer interpreter patch — without rebuilding the
actor, because `pack rebase` rewrites the manifest onto a newer digest of the same major. A pinned
`1.2.3` would make every OS patch a rebuild of every actor on it.

A fully qualified reference is also legal, and then nothing is resolved against the prefix:

```json
{ "runtime": "registry.example.com/team/runtimes/gpu:2" }
```

**Absent means the default for the engine** — `python:1` for a Python actor, `base:1` for a Go one.
Every actor written before runtimes existed therefore keeps building with nothing added.

## Resolution: a name and a major become a digest

Resolution is designed to happen **before the build starts**, and that is deliberate: a typo discovered
by the lifecycle is a failure several containers deep with a message about a manifest, and discovered
here it is one line naming the runtimes that do exist. `kontra deploy` resolves `runtime` out of
`actor.json` (`cli/runtimes.go`), puts the resulting **digest** through the same trust gate an actor
image goes through, and hands `pack build --run-image` the pinned form — so a build is reproducible
even if the major tag moves between resolution and the lifecycle's own pull.

```
python-browser:1                      →  <KONTRA_RUNTIMES_PREFIX>/python-browser:1  →  @sha256:…
registry.example.com/team/rt/gpu:2    →  used as written                            →  @sha256:…
```

The two shapes are told apart by asking whether the first path element **looks like a registry host**
(it needs a dot, a port, or to be `localhost`) — the same question this repository already answers for
every other image reference, rather than a second rule invented for runtimes.

`KONTRA_RUNTIMES_PREFIX` defaults to the install's own registry under `kontra-runtimes/`, and
**first boot mirrors the published set into it** (`docker-compose.yml`'s `cli` service) using the
`push-runtimes` account — the one account allowed to write that namespace, so a compromised actor
build cannot replace the base every other actor is layered on.

It is copied rather than resolved from ghcr directly, and that is a constraint rather than a
preference: admitting `ghcr.io` would need `KONTRA_TRUST_REGISTRIES` to include it, and the
published runtimes are signed per release — identity
`…/kontra-runtimes/.github/workflows/publish.yml@refs/tags/python/1.0.0` — while the trust policy
takes one exact `--certificate-identity` and no regexp. One string cannot cover three runtimes
signed under three tag refs, and adding ghcr to `KONTRA_TRUST_UNSIGNED` would discard a signature
that exists.

Point the prefix at a fork's registry to use your own, and add that registry to
`KONTRA_TRUST_REGISTRIES`; see [[Writing-a-Runtime]].

**The digest is what the build is to use and what the catalog has room for**, as
`runtime: {name, major, digest}` beside `builderDigest`. Both halves are kept because they answer
different questions: the *major* is what the author asked for, and a digest that differs from the
major tag's current one **is** rebase detection. The registrars only ever *echo* what the deploying
CLI sends, and `kontra deploy` now sends both ([[Configuration]]).

### The refusals

Each of these is refused **before any build**, by `cli/runtimes.go`, on the path `kontra deploy`
takes. Each message says what to do instead:

| you wrote | what happens |
|---|---|
| `"runtime": "python-browser"` | refused — no major. The message says to write `python-browser:1` |
| `"runtime": "python:3.12"` | refused by name. A runtime's tag is its **major**; an interpreter version belongs in the actor's `.python-version` |
| `"runtime": "team/gpu:2"` | refused — `team` is not a registry host. A bare name resolves against the prefix; anything else must be fully qualified |
| a runtime nobody published | refused, **listing the runtimes that do exist**. With none published at all, it names `kontra runtime import` instead |

## What ships

All three are published at `:1` and `:1.0.0`, each keyless-signed, and all three are what first boot
mirrors:

| runtime | for | published digest of `:1` |
|---|---|---|
| `base` | Go actors and plain binaries — the builder's stock run image, re-tagged | `sha256:da6d5c3f…` |
| `python` | the default for a Python actor | `sha256:712b8d2e…` |
| `python-browser` | Chromium and the fonts a headless browser needs | `sha256:e1d77faa…` |

**Which runtimes an install mirrors is `KONTRA_RUNTIMES_IMPORT`, and that is configuration rather
than a list inside kontra** — a fork mirrors its own set by editing one `.env` line. Discovery cannot
replace it at the source: `GET /v2/_catalog` against ghcr answers **403 DENIED**, measured, so the
published set has to be named. Against *this install's* registry discovery does work, and it is what
`kontra runtime list` and every "available: …" message read — never a list inside kontra. That is
load-bearing rather than tidy: a fork that had to edit kontra to add a runtime could not add one.

## What a runtime is NOT

- **Not where your dependencies go.** Those are your lockfile — `pyproject.toml` + `uv.lock`, or
  `go.mod`. A runtime that carried a pinned library would pin it for every actor on that runtime.
- **Not where an interpreter version goes.** That is `.python-version`, and the Heroku Python
  buildpack refuses a uv project without one ([[Writing-Actors-Python]]).
- **Not a per-actor `apt install`.** CNB's Dockerfile extensions are a declared non-goal. Extra
  system packages mean choosing a runtime that provides them, or writing one.
- **Not a Dockerfile base.** You do not `FROM` a runtime. The builder picks it up as the run image
  and lays buildpack layers onto it, which is why the deps layer can be shared across actors at all.

## Rebase — patching the OS under an actor

**Not built.** The shape, recorded so the catalog fields make sense:

1. A runtime publishes a new digest under a major that actors reference.
2. The orchestrator compares each runtime's `:<major>` digest against the `runtime.digest` recorded on
   each actor version, and marks the ones behind.
3. `pack rebase` rewrites the manifest onto the new run image. **No rebuild**: the deps and app layer
   digests are unchanged, and the result is a new digest for the *same actor version*.

ADR 0032's "the digest is the identity" survives that unchanged. What changes is that a **version maps
to a sequence of digests**, with the catalog saying which is current — which is exactly why the
catalog had to learn `runtime` and `builderDigest` before rebase could exist at all.

## Migrating off `deploy.sh`

`deploy.sh` is how an actor installed system packages before runtimes. A buildpack build **does not
run it**, so an actor that depended on it would build clean and be missing what the script installed —
at run time, in a container, far from the change. So it is reported at build time: a warning, and a
refusal under `KONTRA_DEPLOY_SH=refuse` (`cli/packbuild.go`).

A `Dockerfile` or `runtime.Dockerfile` in the actor's folder is the same problem, and `kontra deploy`
refuses the actor outright: nothing on the buildpack path reads either file, so whatever it installed
would be missing at run time. There is no warning period because no actor in this repository carries
one — `deploy.sh`'s exists for `webcrawl`, below. The fix is the same: a runtime.

The migration is to read what the script installed and pick the runtime that provides it. The
worked example is `workspaces/demo/actors/webcrawl/deploy.sh`, which apt-installs Chromium's
shared libraries and fonts and then `playwright install chromium`:

- the apt half is `python-browser:1` — that is what the runtime is for;
- the `playwright==1.58.0` pip install is a **dependency**, so it moves to `pyproject.toml` and
  `uv.lock`;
- the pinned browser build Playwright refuses to run without is the runtime's job to keep in step
  with the Playwright version it ships against. A runtime that provides `chromium` states which.

That script is the one `deploy.sh` left in this repository, so a deploy of `webcrawl` takes the
warning path until it moves to `python-browser:1`.

---

**See also:** [[Writing-a-Runtime]] · [[The-Images-Page]] · [[Writing-Actors-Python]] ·
[[Writing-Actors-Go]] · [[Configuration]] ·
[ADR 0061](../adr/0061-buildpacks-runtimes-and-the-image-store.md)
