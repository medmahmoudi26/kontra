# 4. Immutable, content-pinned (name, version) identity

## Status

Settled as a decision. Digest-pinning of images is P2 and **not yet enforced**.

## Context

`(name, version)` is the only cross-plane key: it ties together the graph schema, the Apicurio entry, the Nexus endpoint, and the Temporal task queue. Today `version` is a free string on a `:latest` image with no digest. A redeploy without a version bump silently routes graphs typed against a stale schema to new code.

## Decision

- `version` is **immutable**: any input/output/params schema change MUST bump it.
- Build **refuses to overwrite** an existing `(name, version)`.
- The repo-level `kontra.yaml` references actors by glob (`examples/python/*`) and **inherits** each actor's `(name, version)` from its `actor.json`; it never restates per-actor identity.
- Identity should ultimately pin to an **image digest**, not a mutable `:latest` tag.
- The immutability rule is encoded in the `Actor.version` comment in `run.proto`.

## Consequences

- A typed graph becomes reproducible: a given `(name, version)` always resolves to the same schema and (once digest-pinning lands) the same code.
- Authors must bump `version` on every schema change, and build will reject re-publishing an existing pair.
- Until digest-pinning (P2) lands, identity still rests on a mutable `:latest` tag, so the reproducibility guarantee is only partial.
- This decision underpins routing in ADR 0001 — the `{name}-{version}` queue/endpoint is only stable because the pair is immutable.
