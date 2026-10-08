# Claim-check codec conformance corpus

The claim-check codec is Kontra's **one cross-language wire contract**: independent
implementations encode/decode the same references so a payload offloaded in one runtime
rehydrates in another. The handler encodes on the batch boundary (via a Temporal
DataConverter); the orchestrator decodes to thread data between graph nodes; and BOTH actor
hosts must decode what the handler encoded — so any byte-level disagreement silently drops
data across the boundary.

The actor hosts are the newest, and were missing for a while: this note used to read
*"the actor host does NOT codec"*, which stopped being true the moment each actor became a
Temporal activity worker and landed directly on the payload path. Without the codec, every batch
over the threshold failed with `Unknown payload encoding binary/claim-check-v1`, retried to
exhaustion — loud, but only once a batch was big enough, so small runs passed.

**One implementation left this list without being on it.** `cli/workflow.go` re-derived the marker,
the ref shape, the metadata decoding and the CAS key so that `kontra workflow start --wait` could
read an offloaded result — an implementation of this contract whose header cited this corpus and
whose tests never opened it. It is gone: the CLI reaches the handler's codec through
`runtime/handler/claimcheck`, the way the install already reached the handler's store types through
`casstore` and `hydratestore`, and what is left in the CLI is a transport (one unsigned HTTP GET)
rather than a copy of this contract.

The sentence that used to open this file said how many implementations there were, and the number
was wrong for as long as that fifth one existed. `shared/conformance/README.md` names that failure mode —
a count is a fact about the repo that only a human can check and only a human can update — so the
number is gone from here too. **The table below is the list.** An implementation not in it is a
finding, not a gap in the prose.

This corpus turns "byte-compatible" from a header comment into an executable gate.
Every implementation runs the **same** [`fixtures.json`](./fixtures.json):

| Language | Test | Runner |
|----------|------|--------|
| Go (handler) | [`runtime/handler/internal/codec/conformance_test.go`](../../handler/internal/codec/conformance_test.go) | `go test ./internal/codec/` in `handler/` |
| Go (runtime) | [`runtime/go/codec/conformance_test.go`](../../runtime/go/codec/conformance_test.go) | `go test ./codec/` in `runtime/go/` |
| Go (the `kontra` binary) | [`cli/claimcheck_test.go`](../../cli/claimcheck_test.go) | `go test .` in `cli/` |
| Python (actorkit) | [`runtime/python/internals/test_codec_conformance.py`](../../runtime/python/internals/test_codec_conformance.py) | `pytest` |
| TypeScript | [`control/orchestrator/src/codec/conformance.test.ts`](../../backend/src/codec/conformance.test.ts) | `vitest` (`pnpm test` in `orchestrator/`) |

The `cli/` arm was added in 2026-08 and is **decode-side**: it runs the **offloaded** cases through
the codec `kontra workflow start --wait` reads a result with, and asserts that the URL that codec
builds lands on the `cas_key` recorded here — which is the assertion an implementation's own suite
could not make, because it rebuilt the address the same wrong way the code did.

A fifth arm served the same cases over `POST /encode` + `/decode` from inside the binary. It went
with the process that served them; nothing is served from the `kontra` binary now.

## The `prefixCases` rows, and what six green arms were not saying

Until 2026-08-29 **no row in this file carried a store prefix**. Every case ran with the default
empty one — the single input class in which `prefix + key` and `join(prefix, key)` produce
identical bytes — so all six arms were green while two of the four key derivations under them
concatenated. Found live, not by reading: with `KONTRA_S3_PREFIX=slice11` a Python-served workflow
wrote `slice11cas/df/df5b…` and the CLI asked for `slice11/cas/df/df5b…`, and a claim-check written
by either actor SDK under a non-empty prefix could not be read back by anything else.

That is [ADR 0035](../../docs/adr/0035-the-repo-shape-corpus-not-source-scrape.md) finding 1 with
a different file name: *a golden pinned on the one input class where the implementations cannot
differ*. The eight `prefixCases` rows are the answer, and they are adversarial rather than
exhaustive — measured against the code as it stood, `plain-prefix` went red on the two SDK arms
ONLY, and `prefix-with-a-trailing-slash` went red on the other four ONLY. **Either row alone would
have missed half of it.**

**And one of the six arms was not running at all.** `pyproject.toml` had `testpaths = ["tests"]`,
which excludes `runtime/python` — whose only two test files are the Python arms of this corpus and
of `blobkey.json` — so neither had ever executed in CI, under a workflow header reading "non-e2e
pytest, incl. the codec corpus". A listed arm that does not run is worse than a missing one,
because the table above reads as coverage. It is collected now.

`wireFormat.marker` is now read out of this file by both `cli/` arms rather than restated as a Go
constant beside them. The headers on those constants argued the restatement was deliberate, "so a
drift in the moved code shows up as a failing test and not as a test that agrees with the bug" —
right about importing, wrong about the alternative. Reading the corpus keeps all of that and
removes the copy.

## The wire contract

A Temporal `Payload` whose `data` exceeds `threshold` is offloaded to the object
store and replaced by a tiny content-addressed reference.

**Byte-exact (the actual interop surface):**
- **Marker** — a ref payload has `metadata["encoding"] == utf8("binary/claim-check-v1")`.
- **CAS key** — `cas/<sha256[:2]>/<sha256>` (under the store prefix), derived from
  the sha256 of the original `data` on both write and read; never carried in the ref.
- **Store prefix** — `KONTRA_S3_PREFIX` is a **path segment**, not a string glued to the front
  of the key: strip leading and trailing `/` from every segment including the prefix, drop the
  ones that are then empty, join with a single `/`. So `p`, `p/`, `/p` and `/p/` all name the one
  namespace `p/cas/…`, and a prefix of `/` is an empty prefix. Pinned by the **`prefixCases`**
  rows, and see `wireFormat.prefixTrailingSlash` for why the trailing slash resolves that way
  rather than to a literal `p//cas/…`.
- **Stored object** — the original `data` bytes, verbatim.
- **Threshold** — offload **iff `len(data) > threshold`** (`<=` stays inline).
- **sha256** — lowercase hex, over the **raw data bytes** (not the ref JSON).
- **Metadata values** — base64 **std** (not url-safe) of each original metadata
  value's raw bytes; restored byte-identically on decode.

**Structure-exact, NOT byte-exact:**
- The ref JSON `{sha256,size,meta}`. `json.dumps` (Python) and `JSON.stringify`
  (TS) differ on whitespace, but both parse identically and the object is addressed
  by the **data** sha, not the ref bytes. Harnesses compare the *parsed* ref, never
  its raw bytes. Do not "canonicalize" one side to match the other — it is not part
  of the contract.

**Idempotency / passthrough:**
- A payload already carrying the marker is returned **unchanged** on encode
  (double-encode guard). _This corpus caught a real divergence where one encoder
  lacked this guard the other had — now fixed (case `skip-already-marked`)._
- When the store is disabled (no endpoint, no injected backing), encode/decode
  return payloads unchanged.

## Editing the corpus

`fixtures.json` is generated. Edit the cases in
[`build_fixtures.py`](./build_fixtures.py) and regenerate:

```bash
python shared/conformance/codec/build_fixtures.py
```

Then run both harnesses. A change here is a change to the wire contract — it must
be reviewed as one, and every language harness must stay green.
