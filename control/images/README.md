# control/images — the deployment seam

Container definitions for running kontra locally, and the shape the cloud mirrors. This sits under
`control/` because what it builds runs on the CONTROL PLANE; a Machine's side of the deployment is
`runtime/` and the Warden, which install from a **Bundle** and use no image at all.

> **`Dockerfile.orchestrator` was deleted here, not moved.** It built "the Node orchestrator: the
> HTTP API + built SPA, one image with two roles" from a `./backend` context, and it had been
> unbuildable for some time: `COPY web/ ./` names a directory that stopped existing when the
> console became a sibling package (ADR 0035) and then another repository (ADR 0038), and no
> compose service or CI job has referenced it since `orchestrator-api` left `docker-compose.yml`
> (ADR 0031 §1). The control plane is `kontra up`'s supervised child now, and the SPA is a
> content-addressed artifact of its own. It is in git history if it is ever wanted.

## Images

- **`Dockerfile.pyworker`** — the canonical `kontra-host:1` base: the Python runtime only
  (`actorkit` + the Temporal SDK + the S3/seaweed extra), no actor deps. Per-actor images
  are `FROM kontra-host:1` and add only their own deps (`examples/python/<actor>/Dockerfile`).
  The base carries no entrypoint; the build step stamps `ENTRYPOINT ["python3",
  "/actor/<name>/actor.py"]` per actor — a Python actor is run by Python, no CLI wrapper.
There is no Go base image: a Go actor is a self-contained binary, so `kontra deploy --engine go`
compiles it and synthesises its own Dockerfile rather than layering on a shared base.

## Build & run — two clusters

The infra splits into a **control plane** and the **actors**, so actors can run on
remote machines that connect back to the controller (full guide:
[../docs/wiki/Deployment.md](../docs/wiki/Deployment.md)):

- `docker-compose.yml` — the **control plane**: Temporal + SeaweedFS + Redis + the
  orchestrator (UI+API, which embeds DuckDB). `make up` starts it; it owns the `kontra`
  network same-host actors attach to. (It listed a schema registry until ADR 0027; there was no
  such service in the compose file, so this line described a control plane nobody could start.)
- an **actor** is not a compose service: it is TWO processes (the actor, which is a Temporal
  activity worker, and the Go handler, which owns the workflow). `kontra serve --actor <dir>`
  starts both here and tears them down together; on a fleet **Machine** they are `kontra-actor-<actor>`
  and `kontra-handler-<actor>`, two units installed from a **Bundle**, beside ONE per-Machine metrics
  agent. The units carry the actor's name because a **Machine** holds several **Workers** since ADR
  0037 (packing) — as do its environment file, its Bundle root and its scrape target, so a second
  placement adds a **Worker** instead of overwriting one. It was three
  processes and six units until ADR 0018 removed the sidecar and its placement service, and four
  units until ADR 0037 retired the watchdog into the **Warden**. That last one is not a move of
  working code: the watchdog counted log lines that no actor host has ever emitted (`unit failed`,
  `unit ok`, `engine dead` match nothing in this repo), so its denominator was permanently zero and
  it could never compute the 81-of-82 ratio it was written for. `cli/warden/sickworker.go` is the first
  version of that check that can, and it is installed by `kontra warden join` rather than by a
  placement — so a **Machine** that has not enrolled has no health authority of its own, which is
  what its `loads` chip then says.

## Two Artifacts, and kontra owns no registry

There are two **Artifact** kinds and one mechanism carrying both — and since ADR 0036 the registry
they go to is **the customer's**, not kontra's:

| Verb | Artifact | Where it goes | Who runs it |
|---|---|---|---|
| `kontra deploy` | an **Image** | `<registry>/<name>:<version>` | Docker, on the Controller (`kontra scale`) |
| `kontra build` | a **Bundle** | `--push <ref>`, any OCI reference — one layer, one config blob | systemd, on a fleet Machine (`kontra fleet deploy`) |

`--push` accepts ghcr.io, GitLab, Harbor, ECR, an airgap mirror or `localhost:5000`. **What kontra
keeps is meaning — which digest a version currently names — not storage or transport.** There is no
kontra registry, no kontra versioning scheme and no kontra deployment store to be inside of.

TWO RULES A DESTINATION HAS AND A PULL DOES NOT, both refused before the build is spent:

- **It must name a registry host.** A reference with no host is Docker Hub in the OCI grammar, and
  docker.io rate-limits manifest reads at 100/hour **per IP** — one Fleet pulling one actor
  exhausts it for every unrelated build on that egress address. A first component with no dot and
  no port is not a host: `myregistry/x` is a Docker Hub *user* called `myregistry`.
- **It must not carry a digest.** `@sha256:…` names content that does not exist until the push
  computes it.

An operator's `--push` is HTTPS unless they write `http://` or the host is loopback. That inverts
the default for kontra's own conventional address, which is plain HTTP by construction — see
`cli/bundle.go:pushTransport`, and `shared/conformance/ociref.json` for the pinned table.

WITH NO `--push`, the address is `<registry>/bundles/<name>:<version>` at the Controller's own
registry, and **that is the only address a Fleet placement can resolve**: `resolveBundle` derives it
from the actor and the version alone (`shared/conformance/bundleref.json`). An Artifact pushed anywhere
else is publishable and mirrorable and is not placeable until it is copied there; `kontra build`
prints that rather than leaving it to be found as a 404 three minutes into a run.

CI: `.github/actions/build-actor/action.yml` and `.gitlab/kontra-build-actor.yml` are the same three
commands, so Jenkins or a shell script work identically. **Push, not pull** — kontra never holds a
token to a customer's source; the job runs in their pipeline with their credential, and the only
thing that leaves it is an artifact going to an address they named.

A fleet Machine still runs the actor **natively** — no Docker, no container runtime. What changed
is only where its bytes come from: `control/orchestrator/src/infra/programs/machine.ts` fetches the Bundle's
layer from the registry's blob endpoint with one anonymous `curl`, verifies its sha, runs the
actor's own `deploy.sh`, and hands its two units plus the Machine's metrics agent to systemd.
**No OCI client runs on a Machine**;
resolving the tag and verifying the manifest happen once per run in the control plane
(`control/orchestrator/src/activities/fleet.ts:resolveBundle`).

Two names, and they are not interchangeable. The **manifest digest** is the artifact's address —
what a tag resolves to and what `@sha256:…` pins. The **layer digest** is the sha256 of the tar.gz
and is what a Machine checks; it is a Bundle's identity in ADR 0032's sense, and it is the value
that ADR assigns to `KONTRA_ACTOR_DIGEST` for a **Bundle** — unchanged by this move, since it is
the same hash of the same bytes. (Nothing writes that variable yet: 0032's enforcement half is
still unimplemented on both **Targets**.) `shared/conformance/bundleref.json` pins every address derived
from the two digests, in Go and in TypeScript.

### Airgap

An airgap is a **registry mirror and nothing else**. There is no kontra-specific copy step on this
path any more, because there is no kontra-specific store on it:

```sh
kontra build --actor examples/go/nscheck        # prints the pinned reference
oras copy <controller>:5000/bundles/nscheck@sha256:… <mirror>/bundles/nscheck
#   skopeo copy / crane copy / `docker pull && docker push` all work; it is a standard artifact
export KONTRA_REGISTRY=<mirror>                 # both halves resolve here: push and every Machine

# …or skip the copy entirely and build straight into the mirror. The repository must still be
# `bundles/<actor>`, because that is what a placement derives.
kontra build --actor examples/go/nscheck --push <mirror>/bundles/nscheck
```

The mirror needs the actor's **Image** too if anything runs on the container Target, and the base
images those are built `FROM`. Those were always registry copies; the **Bundle** is the one that
used to need an object-store sync with a hand-built key, and does not any more.

**Why that reasoning stopped holding (ADR 0036).** "The Machine is already the isolation boundary"
is a claim about **Workers**, not about *authors*. It is true while every actor is first-party, and
false the moment a **Machine** runs code kontra did not write — because what shares that kernel is
kontra's own handler, its Temporal credential and its object-store keys.
