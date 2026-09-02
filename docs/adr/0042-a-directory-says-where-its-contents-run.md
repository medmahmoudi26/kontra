# 42. A directory says where its contents run

## Status

**Accepted.** Completes the restructure **0038** started and **0041** continued. Supersedes nothing; it moves directories and changes one Go module path.

## Context

After 0038 took the actors, the workflows and the console out, and 0041 extracted the shared kernel, the root of `kontra` held thirteen directories:

```
backend  cli  conformance  contracts  core  docs  handler
infra  runtime  scripts  sdk  testdata  tests
```

Every name says what its contents ARE — a language, a role, a file type. None says **where they run**, and that is the distinction this system is built on: a control plane the operator owns, and Machines it provisions that run code it did not write. The two have different trust boundaries, different failure modes and different lifecycles, and a reader could not tell which half a directory belonged to.

Three specifics made it worse than untidy.

1. **There were two directories called `infra`.** The root one held two Dockerfiles, an entrypoint script and a glossary. The Pulumi programs it was named for are in the orchestrator, at `src/infra/`. So the one at the root was not the one that provisions anything.

2. **`cli/` is both halves and looked like neither.** Forty-eight files in one `package main`: `up.go` and `deploy.go` are the control plane, `warden.go` and `driver_podman.go` run on a Machine, and nothing separates them but a filename convention.

3. **The repo and its own artifact disagreed.** A hydrated appliance bundle has always laid the server out at `orchestrator/dist/src`; the repository called the same thing `backend/`.

## Decision

**A directory says where its contents run.** There are three answers, and the root is nine entries:

| | |
|---|---|
| `sdk/` | what an actor AUTHOR imports |
| `runtime/` | what runs on a MACHINE, beside the actor — `go/`, `python/`, and `handler/` |
| `control/` | what runs where `kontra up` runs — `orchestrator/`, `images/` |
| `cli/` | the one binary, which is both — `appliance/` and `warden/` |
| `shared/` | NEITHER: `core/`, `contracts/`, `conformance/` |

- **`handler/` moves under `runtime/`.** It is compiled into a **Bundle** with the actor and both Python SDK seams and runs beside them on a Machine; it was at the root because it is Go, which is a fact about its language and not about where it executes. Its module path becomes `github.com/medmahmoudi26/kontra/runtime/handler`.

- **Root `infra/` is dissolved.** The Dockerfiles are `control/images/`; `CONTEXT.md` moves next to the Pulumi programs it is the glossary for.

- **`backend/` becomes `control/orchestrator/`,** which also ends the disagreement with the bundle: both are `orchestrator` now.

- **The Warden becomes `cli/warden/`, a package rather than a file-naming convention** — so a control-plane command cannot reach into a container driver by accident.

## The constraint that shaped this, and it is not a preference

**`sdk/` and `runtime/` cannot move.** An actor in another repository imports them BY PATH:

```go
require (
    github.com/medmahmoudi26/kontra/runtime/go v0.0.0
    github.com/medmahmoudi26/kontra/sdk/go     v0.0.0
)
```

Go has no registry: the import path is the repository path plus the subdirectory, and the git tag that publishes a module carries the same prefix (`git tag sdk/go/v0.1.0`, ADR 0038). **Their directory is their API.** Moving `runtime/` under a `machine/` would break every actor in `kontra-actors`, every actor a client has written, and the tags that publish both.

That is why there is **no `machine/` directory**, however symmetrical it would look beside `control/`. A Machine's side of this system is split between a published surface that is frozen and a binary that is also the operator's, and no directory tree can put those two in one box. `runtime/` is the machine side under a name it cannot change; the layout works with that rather than against it.

## Considered options

**`control/` and `machine/`, symmetrically.** Rejected on the constraint above. It was the first shape drawn and it cannot be built.

**Leave the layout and add a map to the README.** Rejected: the complaint is that there are too many directories saying nothing, and a document explaining a confusing tree is a second thing to keep in step with it. It would also have left the two `infra` directories.

**Keep module paths stable by declaring them in `go.mod` independently of the directory.** Go permits this — a module at `machine/handler/` may declare itself `…/kontra/handler`. Rejected: `go get` resolves a path to a repository subdirectory, so a published module whose path lies is one nobody can fetch. It is only safe for modules nothing outside the repo imports, and buying a tidier tree with an import path that does not describe where the code is trades one confusion for a worse one.

**Split `kontra` into two binaries, `kontra` and `kontra-warden`.** Genuinely attractive — a Machine would stop carrying `kontra up`, the MCP server and the embedded Temporal. Deferred, not rejected: one binary everywhere is a deliberate property of the install (ADR 0031), and the change touches fleet provisioning, `provision-controller.sh` and every install document. The package boundary below is most of the benefit at a fraction of the risk.

## Consequences

- **The move is mechanical and the DEPTH is not.** Every relative path that climbed to the repo root from inside a moved directory needed one more `..` — nineteen files in the orchestrator, two in the handler. `contract/datasets.test.ts`'s `REPO` resolved to `control/` and would have read nothing. This is the third bullet of `tests/test_conformance_tree.py`'s header, and it is why the sweeps matched `../…/<root-dir>/` rather than a directory name.

- **Two guards were wrong and were fixed by walking into them.** `test_conformance_tree.py` checked that a corpus a driver NAMES exists, never that the driver's path arrives there — it passed while thirty drivers were broken. `backend/vitest.config.ts` reached outside its package with a glob that matched nothing, and the run silently went from 109 files to 105. Both now fail loudly, and both were verified by breaking them on purpose.

- **Some strings must NOT follow the repo.** `Component.Path` in a bundle manifest is relative to the extracted archive; `/kontra/handler` is a path inside an image; `kontra-handler-<actor>` is a systemd unit. Each is documented where it lives, because a previous rename got one of them wrong in the direction that still compiles.

- **`Dockerfile.orchestrator` was deleted rather than moved.** Nothing had built it since `orchestrator-api` left `docker-compose.yml`, and it could not have: it copies a `web/` that stopped existing when the console became a sibling package.

- **kontra-console follows twice.** Its `link:` target and its e2e harness's imports both name paths in this repository. That is the cost of an unpublished package and a harness that constructs a real `PanelServer`; publishing `@kontra/core` removes the first.
