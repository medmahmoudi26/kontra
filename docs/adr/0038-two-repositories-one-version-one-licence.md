# 38. Two repositories, one version, and a licence that funds the work

## Status

**Accepted.** Supersedes nothing; this is the first decision about how kontra is *distributed* rather than how it works.

Depends on **0036** (kontra owns no registry; an **Artifact** goes to a registry the user controls) — which is what makes a separate actors repository possible at all.

## Context

kontra was written in one private repository, `kontra-local`, by one author. It is going open source while remaining commercially licensed, and that combination forces several decisions that are cheap now and expensive or impossible later.

1. **There has never been a version.** Measured at the time of writing: **zero git tags**, and `sdk/python/pyproject.toml` declares `version = "0.0.0"`. Nothing has been published, so there are no consumers to break — a window that closes on the first tag.

2. **The SDKs cannot be used outside the checkout.** `examples/go/nscheck/go.mod` carries `replace github.com/…/sdk/go => ../../../sdk/go` with the comment *"The SDK lives in this checkout; there is no published module."* An actor in another repository cannot resolve it. The Python SDK is installed as an editable local path.

3. **Building an Artifact requires the checkout.** `cli/bundle.go` calls `findRepoRoot("")` and refuses with *"building a Bundle needs the checkout (handler/ + sdk/ + runtime/)"*, because it cross-compiles the handler from source. A fork of an actors repository has none of those directories.

4. **History is a liability that code is not.** The repository has held DigitalOcean tokens, a fleet SSH key and a Pulumi passphrase in its working tree for its whole life. Two patterns were swept — `.env` was never committed, and the only `dop_v1_` hits are `dop_v1_SENTINEL` test fixtures — but that is two patterns across ~100 commits.

5. **Some actors are the product, not examples.** `examples/go/` tracked seven actors. `desync` probes request-smuggling primitives and its technique is recorded elsewhere in the authors' notes as a competitive moat; `nuclei`, `subfinder` and `linkfind` are operational tooling. `examples/private/` was already gitignored, but those seven were not.

6. **The business is running kontra for people.** A permissive licence would let a cloud provider or a competitor host it and owe nothing — which is the whole business, given away.

## Decision

- **Two repositories, and `kontra-local` is retired.** `kontra` holds the framework; `kontra-actors` holds actors. `kontra-local` is archived private as the historical record and referenced from `kontra`'s README as "available on request".

- **`kontra` starts from a single initial commit.** Not a history migration. Certifying ~100 commits of a repository that has handled live cloud credentials is work that squashing simply removes, and publication cannot be undone. The cost — `git blame` pointing at one commit, and the loss of unusually load-bearing commit messages — is paid to `kontra-local`, which keeps them.

- **`kontra` ships NO actors.** Not a curated subset: none. `examples/` is deleted, and every actor moves to `kontra-actors`, which stays private until a publication split is decided. People fork `kontra-actors`; they do not inherit somebody else's examples forever.

- **One fixture actor stays, in `testdata/`.** `scripts/parity-gate.sh` dispatches to a real Worker, and the `build-actor` CI templates build a real actor. Both need one to exist. It is deliberately boring, undocumented, and named `testdata` so nobody copies it as a template.

- **One version for everything, and stay on `0.x`.** A single tag `v0.4.0` moves `sdk/go`, `runtime/go`, `sdk/python`, the CLI and the handler together. They are not independently useful: an actor imports the SDK, links the runtime, is built by the CLI and runs against the handler, and **all four must agree on the wire**. Versioning them separately invents a compatibility matrix that nothing tests. `0.x` is the honest signal that this will move, and it costs nothing.

- **AGPL-3.0 for the control plane, runtime, CLI and console; Apache-2.0 for `sdk/go` and `sdk/python`.** The AGPL network clause is the one that matters: anyone running kontra as a service for third parties must publish their modifications. Commercial exceptions are sold by the copyright holder. **The SDKs are permissive because a client's actor links them** — a copyleft SDK would reach into code that is not ours to claim, which is not the intent and would make kontra unadoptable for the customers it is for.

## Considered options

**Keep `kontra-local` private and export to `kontra`.** Rejected: a one-way sync must be maintained forever, and the first forgotten filter either leaks a secret or silently diverges the public repository. The same objection retires the subtree variant.

**Migrate the full history.** Rejected on the cost of certifying it, not on principle. `gitleaks`/`trufflehog` over ~100 commits plus a decision about every hit is work that a squash makes unnecessary.

**Publish all seven Go actors.** Rejected: `desync` publishes a technique the authors rely on, and the `subfinder`/`nuclei` wrappers encode operating rules — one worker per droplet, unique IP, concurrency 1 — that are part of how results are obtained. Publishing *some* was also rejected, in favour of publishing none, so that `kontra-actors` is a repository people fork rather than a gallery they read.

**Independent semver per module.** Rejected: it creates a compatibility matrix on day one and no CI run covers a mixed combination.

**Apache-2.0 throughout.** Rejected for the reason in context §6. **BSL 1.1** was rejected because it is not OSI-approved, and choosing "source-available" before having users to protect buys the backlash without the benefit.

## Consequences

- **`kontra-actors` is not usable by a fork until two blockers are fixed** — the released binary must carry prebuilt handlers so `kontra build` needs no checkout, and the control plane must store a reference so an Artifact pushed anywhere but the derived address is placeable. Both are ADR 0039's subject. Until then the repository is a place to keep actors, not a place to build them.

- **The Go SDK needs no registry, only tags** — `git tag sdk/go/v0.1.0`. While `kontra` is private, consumers additionally need `GOPRIVATE=github.com/medmahmoudi26/*` and their own git credentials, because `proxy.golang.org` cannot fetch a private repository. The Python SDK does need an index; a PyPI token is the only publishing credential this project requires.

- **Contribution terms are unresolved, and the trigger is publication, not the first pull request.** AGPL-with-exceptions only works while one party holds the copyright. Both repositories are private, so no outside contribution can arrive yet — but the day either goes public, a CLA becomes a one-file fix that later becomes impossible to retrofit without tracking down every contributor. `CONTRIBUTING.md` says pull requests are not yet accepted rather than leaving it unstated.

- **CI's `examples/` dependencies must be repointed before the split lands.** `parity-gate.sh` serves `examples/python/beacon`, and `ci.yml` feeds `examples/python/probe` to the build-actor job and builds seven standalone Go modules. All three break the moment `examples/` is deleted.

- **The data plane must be hardened before publication.** Publishing is a disclosure: the code shows that a Worker receives `KONTRA_S3_ENDPOINT` with no credential, from which a reader infers the gateway is open. Measured on the development controller: an unauthenticated S3 `LIST` returns `200`, Redis `requirepass` is empty, and the OCI registry is bound to `0.0.0.0` on a public IP. That is filed as work, not as a trade-off.
