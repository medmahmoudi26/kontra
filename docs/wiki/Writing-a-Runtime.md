# Writing a runtime

A **Runtime** is a CNB run image with a `runtime.json` beside it. Writing one means adding a directory
to a copy of `kontra-runtimes`, building it, and pushing it where your install looks — never editing
kontra. If adding a runtime required a change in this repository, a fork could not add one.

> [!IMPORTANT]
> **Not built.** The `kontra-runtimes` repository does not exist yet, and neither do
> `kontra runtime build`, `kontra runtime test` and `kontra runtime import`. This page is the
> contract a runtime has to meet — the parts kontra already enforces are marked **live**; everything
> else is what the three verbs will do.
> [ADR 0061](../adr/0061-buildpacks-runtimes-and-the-image-store.md) carries the decision.

Read [[Runtimes]] first for what a runtime is and how an actor picks one.

## The repository

```
kontra-runtimes/
  README.md                 # what a runtime is, how to add or fork one
  builder.json              # {"image": "heroku/builder:24", "digest": "sha256:…"}
  runtimes/
    base/                   # the builder's stock run image, re-tagged — Go actors, plain binaries
      runtime.json
    python/
      Dockerfile            # FROM the builder's run image; usually just labels
      runtime.json
    python-browser/
      Dockerfile            # FROM python's; apt-installs Chromium and the fonts it needs
      runtime.json
      test/                 # a fixture actor that must build AND run on this runtime
  .github/workflows/
    publish.yml             # build, test, sign, push on tag
```

`builder.json` is in the repository rather than in kontra because the set of runtimes is what targets a
builder. One builder, one set, verifiable in one place.

## `runtime.json`

```json
{
  "name": "python-browser",
  "major": 1,
  "description": "Python run image with Chromium and the fonts headless browsers need.",
  "builder": "heroku/builder:24",
  "languages": ["python"],
  "provides": ["chromium"]
}
```

| field | why it is there |
|---|---|
| `name` · `major` | what an actor writes in `actor.json` (`python-browser:1`). The major is the unit of rebase |
| `description` | one line. What the Images page and `kontra runtime list` will show — neither exists yet |
| `builder` | the builder this runtime is compatible with. The field `kontra runtime build` is specified to check — see the rule below, including what does not exist yet |
| `languages` | which engines this runtime can carry. Advisory: the buildpacks decide what they can detect |
| `provides` | the system capabilities an author is choosing this runtime **for** — `chromium`, `tesseract`. A name an actor's README can point at |

## The builder compatibility rule

The design is **one builder per install, pinned by digest**. **Nothing in kontra pins one today** —
`cli/packbuild.go` has a `Builder` field and passes it to `pack build --builder`, and no caller sets
it; there is no builder constant, no `KONTRA_BUILDER_*` variable and no `builder.json` in this
repository. That pin is `kontra-runtimes`' to carry, which is one reason the build path is not wired to
`kontra deploy` yet.

| | |
|---|---|
| builder | `heroku/builder:24` is the intended one. It resolved to `sha256:97aa835c2e0528c623bd500a9e16030dacc08be1cddd34ce78918974de4098c1` when that was measured — a measurement, not a pin. Until something records it, a build handed the *tag* gets whatever the tag points at that day |
| `pack` | `0.40.9`, downloaded and **SHA256-verified** into the install image the release builds — **live**, in `control/images/Dockerfile.install`. `make image`'s Dockerfile does not carry it, so a from-clone install needs a `pack` on `PATH` or `KONTRA_PACK_BIN`. The CLI picks the first `pack` it finds and does not check its version, so that pin holds only where the release image is what runs |

**Every runtime must be compatible with that builder**, and the check is not a courtesy. A CNB run
image carries metadata the lifecycle matches against the build image it was built by; a run image from
another builder family either is refused by the lifecycle or carries different system libraries than the
dependency layers were compiled against. Both failures arrive late and name a manifest or a missing
`.so`, not the runtime anybody chose — which is why the check belongs in front of the build.

So two refusals, both of them ahead of any build and **neither of them written yet** — they are
`kontra runtime build`'s, and that verb does not exist:

- a `runtime.json` whose `builder` is not the install's pinned builder;
- a runtime image that lacks the run-image labels the pinned builder's platform API requires.

The exact label names depend on the builder's platform API version, so **read them off the pinned
builder and record them in your fork's README** rather than copying a list from here — a stale label
name is the one error in this area that produces a confusing message instead of a clear one.

`pack` itself is pinned for the same reason the builder is: a re-pointed tag means a different
lifecycle than the one a recorded `builderDigest` can be compared against.

## Tags, and which ones survive

| tag | meaning |
|---|---|
| `<prefix>/<name>:<major>` | **moving.** This is what actors reference and what rebase detection reads |
| `<prefix>/<name>:<major>.<minor>.<patch>` | **immutable.** The thing a `:<major>` currently points at |

The registry's retention policy for `kontra-runtimes/**` keeps **every `<major>` tag** plus the **3
most recently pushed** semver tags, and deletes untagged manifests after 24 h.

> One divergence worth knowing before you publish a second major: "the last 3 patch tags **per
> major**" cannot be expressed — zot's `mostRecentlyPushedCount` counts within a *repository*, not
> within a group. It is encoded per repository, which is equivalent while a runtime has one active
> major and divergent the moment it has two.
>
> And zot records the push timestamp **per manifest digest**, not per tag. Several tags on one
> manifest therefore have no order among them, and the count picks an arbitrary subset.

## Annotations on every runtime image

```
dev.kontra.runtime.name          python-browser
dev.kontra.runtime.major         1
dev.kontra.runtime.description   Python run image with Chromium and …
dev.kontra.runtime.provides      chromium
```

plus the CNB run-image labels the pinned builder requires. `provides` is a comma list.

The `dev.kontra.*` set is what lets a runtime describe *itself* instead of being described in kontra:
the Images page's Runtimes view reads them off the manifests under the prefix ([[The-Images-Page]]).
The CLI's own listing reads only repository and tag names — a name is all a refusal message needs, and
reading annotations would mean a manifest fetch per repository to print one line.

## Publishing to your own registry

```sh
export KONTRA_RUNTIMES_PREFIX=registry.example.com/acme/kontra-runtimes
```

That one variable is the whole of forking. Unset, runtimes resolve against the install's own registry
under `kontra-runtimes/`; set, every bare `name:major` in every `actor.json` resolves against your
prefix instead, and a fully qualified reference in an `actor.json` still wins over both. **Live** —
this is the resolution the CLI already does.

Pushing needs the `push-runtimes` credential, which is the second of the registry's three roles:

| role | may |
|---|---|
| `push-actors` | read, create, update **and delete** under `actors/**`; **read** everywhere else |
| `push-runtimes` | the same four under `kontra-runtimes/**`; **read** everywhere else |
| `pull` | read everything, write nothing |

The roles are separate so a build that can publish an actor cannot replace a runtime underneath every
other actor — and `push-actors` keeps *read* on `kontra-runtimes/**` because a build has to pull the run
image it is laying layers onto. There is deliberately **no `adminPolicy`**: one granting `push-runtimes`
registry-root, delete over `actors/**` included, would have made the least-trusted of the three
credentials the most powerful, since that is the credential handed to whoever is iterating on a runtime.
Delete goes no wider than the prefix a role owns.

Credentials are off unless **all three** passwords are set; a partial set is refused, because a
registry with auth enabled and no users answers 401 to everything and reads as a broken install rather
than a locked one. See [[Configuration]].

> The registry speaks **plain HTTP** by construction, and `docker login` refuses plain HTTP to a
> non-loopback host. So a push credential has to be **provisioned as a file** (`DOCKER_CONFIG`
> pointing at a directory with a `config.json`), not created interactively.

## The CI gate

Each runtime's `test/` directory holds a fixture actor, and nothing is pushed until that actor
`pack build`s against the new runtime **and runs one Method successfully**. A runtime that builds and
cannot run is the failure mode this catches: a missing shared library shows up when the browser opens,
not when the layer exports.

**Signing.** Runtime images are meant to be cosign-signed, and that is **net-new work, not reuse**:
nothing in kontra signs an actor or runtime image today. `cosign` exists only for kontra's own
control-plane images and release blobs, in CI, with GitHub OIDC. Until that lands, the Images page's
`signed` column has nothing true to report.

## Local iteration

All three verbs are **specified, not implemented**:

```sh
kontra runtime build ./runtimes/python-gis   # build the Dockerfile, push as <prefix>/python-gis:1
kontra runtime test  ./runtimes/python-gis   # run the test/ fixture actor against it
kontra runtime import runtimes.tar           # load runtimes into an air-gapped install
```

`kontra runtime build` is where the two compatibility refusals live, and it is also the command with
the hardest unresolved dependency: the build belongs on a **rootless** podman socket, and there
is no rootless podman socket on a controller running as root — `/etc/subuid` has no entry for root, so
rootless podman has no subuid range to map into. A non-root build user has to exist first. The same
blocker is why the `builder` compose service does not exist yet.

Until the verbs land, a runtime is built and pushed with `pack`/`docker` by hand and the install picks
it up the moment it is under the prefix — discovery is a registry query, so nothing needs to be told.

---

**See also:** [[Runtimes]] · [[The-Images-Page]] · [[Configuration]] · [[Deployment]] ·
[ADR 0061](../adr/0061-buildpacks-runtimes-and-the-image-store.md)
