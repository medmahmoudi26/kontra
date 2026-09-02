# 1. Nexus operations + namespace-per-author tenancy

## Status

**Settled — implemented (both SDKs).** Dispatch is Nexus-only: the interpreter calls the
`kontra.actor` service's `run` operation at endpoint `kontra-{name}-{version}`. Python
(`internals/nexus.py`) and Go (`internals/runtime/nexus.go`) both serve the handler and
auto-create the endpoint on boot from the SAME `endpoint_name(name, version)` derivation, so
Python **and** Go actors are dispatchable through the orchestrator. The `tenant` field is
carried as `"default"`; real multi-tenant (namespace-per-author) deployment is deferred to P3.

## Context

Actors are author-supplied code: the authors are security researchers running arbitrary tooling (raw sockets, `NET_ADMIN`, browser automation). Per-actor isolation — resource limits, crash blast-radius, kernel capabilities — is only enforceable at the worker/container edge, not inside a shared workflow process. We also need a routing key that ties a graph node to the exact code+schema it was typed against.

Why Nexus over a plain `executeChild` in one namespace (the question the arch review left open, now settled): Temporal disables cross-namespace child workflows by default and steers cross-namespace calls to Nexus, which is the transport namespace-per-author needs. Adopting it now — while still single-namespace locally — keeps one dispatch path (no second transport to drift) and makes the P3 tenant split a config change, not a rewrite. The operation payload is `EntryInput` either way, so it stayed a transport choice, not a contract change.

## Decision

- Each actor becomes a Temporal **Nexus operation** (workflow-backed / async, one actor per worker/image) at build time.
- Dispatch is routed by `(name, version)` to a `{name}-{version}` task queue / Nexus endpoint.
- Production tenancy is **namespace-per-AUTHOR**. Locally there is a single namespace, with the tenant field carried as `"default"`.
- `RunEnvelope.tenant` (`shared/contracts/kontra/v1/run.proto`, field 4) selects the **namespace**. It does **not** scope the CAS key.
- **CAS is global content-addressed, by deliberate choice.** `cas_key(digest)` is a pure content hash (`cas/<sha[:2]>/<sha>`) with no tenant component, so dedup is **cross-run AND cross-tenant** — identical bytes collapse to one object regardless of who produced them (ADR 0007). This is a feature: it is the property that makes re-runs and shared inputs free. CAS holds opaque, integrity-checked content, not the tenant isolation boundary; that boundary is the Temporal namespace above.
- **If hard tenant isolation of stored bytes is ever required** (e.g. a tenant must not even share physical objects), it comes via a separate reserved bucket or key-prefix per tenant — a tenant-scoped store, not a tenant component mixed into the content hash. Design noted; **not built**, and not needed while namespace isolation is the boundary.

## Consequences

- Each tenant gets a hard Temporal isolation boundary at the namespace level, matching the per-worker/per-container isolation the authors' code requires.
- Carrying `tenant="default"` in the envelope today means introducing real tenants later requires no history migration — the field already exists on every dispatch.
- Both SDKs derive the endpoint from one `endpoint_name(name, version)` = `kontra-{name}-{version}`, so a Go actor and a Python actor with the same identity land on the same endpoint — cross-language parity is by construction, guarded by `wire_congruence_test`.
- Routing by `(name, version)` makes this decision dependent on the immutable content-pinned identity decision (ADR 0004): the queue/endpoint name is only stable if `(name, version)` is.
