# 10. CONTRACT_VERSION as a single Python constant the wire strings reference (without changing the bytes)

## Status

Settled as a decision; the constant is **not yet introduced** (today both strings are independent literals).

## Context

Two wire strings carry the contract version today, each defined as its own literal:

- `runtime/python/internals/codec.py:35` — `_CLAIM_CHECK = b"binary/claim-check-v1"` (the claim-check marker).
- `runtime/python/internals/manifest.py:13` — `SCHEMA_VERSION = "kontra.actor.v1"` (the manifest schemaVersion).

Both are pinned by the conformance corpus (`shared/conformance/codec/fixtures.json`) and by decode logic that matches on `metadata["encoding"] == utf8("binary/claim-check-v1")`. Any byte change is dangerous: with namespace-per-author, the caller (orchestrator) decodes what the handler (author) encoded, so a changed marker silently passes an old ref through un-rehydrated — data loss across the seam, not a loud failure — and breaks the cross-language gate. A changed manifest string makes `manifest.py` reject every existing actor.

## Decision

Factor only the **version substring** into one Python constant in `actorkit` (`CONTRACT_VERSION = "v1"`, natural home a new low-dependency module re-exported from `actorkit/__init__.py`) that both the codec marker and the manifest schemaVersion reference:

- `_CLAIM_CHECK = f"binary/claim-check-{CONTRACT_VERSION}".encode()`
- `SCHEMA_VERSION = f"kontra.actor.{CONTRACT_VERSION}"`

The produced strings MUST stay byte-identical: `"binary/claim-check-v1"` and `"kontra.actor.v1"`. Only the bare `v1` token is shared; each call site keeps its own prefix and separator (`-v` for the codec marker, `.v` for the manifest).

## Consequences

- One place to bump the contract version, while today's strings stay byte-exact — a pure refactor breaks nothing.
- The constant is deliberately **not** a single shared knob across all forms. The independent copies stay independent and must NOT be wired to `CONTRACT_VERSION`, because they are the cross-language / cross-tool checks that would otherwise become tautological:
  - `shared/conformance/codec/build_fixtures.py:28` `MARKER` and `fixtures.json` (the corpus is the source of truth the implementations are checked against).
  - `control/orchestrator/src/codec/claimCheck.ts:33` `CLAIM_CHECK_MARKER` (separate TS language seam).
  - `shared/contracts/kontra/v1/step.proto:3` / `run.proto:3` `package kontra.v1` (buf-owned; the `v1` token coincides but is not derived from the Python constant).
- The TS manifest-version sites (`folderUpload.ts`, `server.ts`, and tests) and the Python literal-assert tests remain independent peers that must equal the produced strings.
- Bumping `CONTRACT_VERSION` is a real wire-format change: it would break the corpus, the decode-of-existing-data path, and manifest loading, so it is intentionally a deliberate, reviewed act — not an incidental refactor.
