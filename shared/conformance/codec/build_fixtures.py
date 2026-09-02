#!/usr/bin/env python3
"""Generate the language-neutral claim-check conformance corpus (fixtures.json).

The corpus pins the claim-check WIRE CONTRACT that every codec implementation must
reproduce: the Go handler (runtime/handler/internal/codec) and the TS orchestrator
(control/orchestrator/src/codec/claimCheck.ts). Each language's test runs its REAL codec against
this one fixtures.json -- that is what keeps the encoders byte-compatible.

What is byte-exact (the actual interop contract):
  - the marker bytes  (metadata["encoding"] == utf8("binary/claim-check-v1"))
  - the CAS key       (cas/<sha256[:2]>/<sha256>, derived from the data sha)
  - the stored object (== the original payload data, verbatim)

What is structure-exact, NOT byte-exact:
  - the ref JSON {sha256,size,meta}. json.dumps and JSON.stringify differ on
    whitespace, but both parse identically and the object is addressed by the
    DATA sha, not the ref bytes -- so the ref is defined by its parsed structure.

Run: python shared/conformance/codec/build_fixtures.py   (writes fixtures.json)
"""
from __future__ import annotations

import base64
import hashlib
import json
from pathlib import Path

MARKER = "binary/claim-check-v1"


def b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def cas_key(digest: str) -> str:
    return f"cas/{digest[:2]}/{digest}"


# (name, threshold, data bytes, metadata {key: value bytes}, note)
CASES = [
    ("inline-small", 16, b"hello", {"encoding": b"json/plain"},
     "under threshold -> returned unchanged, nothing offloaded"),
    ("inline-at-threshold", 16, b"0123456789abcdef", {"encoding": b"json/plain"},
     "len == threshold -> inline (offload is STRICTLY > threshold)"),
    ("offload-just-over", 16, b"0123456789abcdefg", {"encoding": b"json/plain"},
     "len == threshold+1 -> offloaded"),
    ("inline-empty", 16, b"", {"encoding": b"binary/null"},
     "empty payload -> inline"),
    ("offload-binary-meta", 8, b"the quick brown fox jumps",
     {"encoding": b"json/plain", "x-bin": bytes([0xFF, 0xFE, 0x00, 0x80])},
     "non-UTF-8 metadata VALUE is base64-std encoded in ref.meta and restored on decode"),
    ("offload-payload-looks-like-ref", 8,
     b'{"sha256":"deadbeef","size":3,"meta":{}}', {"encoding": b"json/plain"},
     "payload bytes that LOOK like a ref are opaque; only the marker triggers decode"),
    ("skip-already-marked", 8, b"already a claim-check ref!!",
     {"encoding": MARKER.encode()},
     "a payload already marked claim-check is returned UNCHANGED (double-encode guard)"),
]

# The store prefix (KONTRA_S3_PREFIX). ONE payload -- `offload-just-over`'s -- under a set of
# prefixes, so a failure names the JOIN and nothing else.
#
# Every row in CASES above runs with no prefix, which is the one input class where
# `prefix + key` and `join(prefix, key)` produce identical bytes. Six green arms therefore said
# nothing while two of the implementations concatenated: with KONTRA_S3_PREFIX=slice11 a
# Python-served workflow wrote `slice11cas/df/df5b…` and the CLI asked for `slice11/cas/df/df5b…`.
# That is ADR 0035 finding 1 exactly -- a golden pinned on the input class where the
# implementations cannot differ -- so both spellings of a slash are here on purpose.
#
# (name, prefix, why)
PREFIX_CASES = [
    ("empty-prefix", "",
     "the default, and the only class the corpus covered before these rows: no prefix, no "
     "leading slash"),
    ("plain-prefix", "slice11",
     "THE LIVE BUG. With KONTRA_S3_PREFIX=slice11 a Python-served workflow wrote "
     "slice11cas/df/df5b... while the CLI asked for slice11/cas/df/df5b... A concatenating "
     "implementation fails on this row and only on this class."),
    ("prefix-with-a-trailing-slash", "slice11/",
     "the decision row (wireFormat.prefixTrailingSlash): same namespace as 'plain-prefix', NOT "
     "a second one reached through a double slash. A concatenating implementation passes this "
     "row by accident, which is exactly why it cannot be the only prefix row."),
    ("prefix-with-a-leading-slash", "/slice11",
     "the other edge: a key is relative to the bucket, so a leading slash is a spelling and not "
     "an absolute path. No implementation may emit a key beginning with '/'."),
    ("prefix-slashed-both-ends", "/slice11/",
     "both edges at once, so an implementation that trims only one end is caught"),
    ("nested-prefix", "tenants/acme",
     "a prefix may itself be several segments (a tenant under a namespace); the INTERIOR slash "
     "is content and is never touched"),
    ("nested-prefix-with-a-trailing-slash", "tenants/acme/",
     "interior slash kept, edge slash dropped, in one input"),
    ("prefix-that-is-only-a-slash", "/",
     "trimming leaves nothing, so the segment is dropped: '/' is an EMPTY prefix, byte-identical "
     "to the default. The degenerate case a total rule has to answer."),
]

# The payload every PREFIX_CASES row carries -- `offload-just-over`, so the digest and the
# un-prefixed key are already pinned by a row above and only the prefix is under test.
PREFIX_PAYLOAD = b"0123456789abcdefg"
PREFIX_META = {"encoding": b"json/plain"}
PREFIX_THRESHOLD = 16


def object_key(prefix: str, *parts: str) -> str:
    """The join the corpus records: strip leading/trailing '/' from EVERY segment, the prefix
    included; drop the ones that are then empty; join what is left with a single '/'.

    The generator computes the expectation rather than hard-coding it so a row cannot be added
    with a hand-typed key that quietly disagrees with the rule the header states -- but it is a
    FOURTH implementation of that rule and is deliberately the only one no runtime imports.
    """
    bits = []
    p = prefix.strip("/")
    if p:
        bits.append(p)
    for part in parts:
        seg = part.strip("/")
        if seg:
            bits.append(seg)
    return "/".join(bits)


def build() -> None:
    cases = []
    for name, threshold, data, meta, note in CASES:
        input_obj = {
            "data_b64": b64(data),
            "meta_b64": {k: b64(v) for k, v in meta.items()},
        }
        if meta.get("encoding") == MARKER.encode():
            expect = {"action": "unchanged"}
        elif len(data) <= threshold:
            expect = {"action": "inline"}
        else:
            digest = hashlib.sha256(data).hexdigest()
            expect = {
                "action": "offload",
                "ref": {
                    "sha256": digest,
                    "size": len(data),
                    "meta": {k: b64(v) for k, v in meta.items()},
                },
                "cas_key": cas_key(digest),
                "object_b64": b64(data),
            }
        cases.append({
            "name": name,
            "note": note,
            "threshold": threshold,
            "input": input_obj,
            "expect": expect,
        })

    prefix_digest = hashlib.sha256(PREFIX_PAYLOAD).hexdigest()
    prefix_cases = [
        {
            "name": name,
            "why": why,
            "prefix": prefix,
            "threshold": PREFIX_THRESHOLD,
            "input": {
                "data_b64": b64(PREFIX_PAYLOAD),
                "meta_b64": {k: b64(v) for k, v in PREFIX_META.items()},
            },
            "expect": {
                "sha256": prefix_digest,
                "key": object_key(prefix, cas_key(prefix_digest)),
            },
        }
        for name, prefix, why in PREFIX_CASES
    ]

    doc = {
        "wireFormat": {
            "marker": MARKER,
            "casKeyLayout": "cas/{sha256[:2]}/{sha256}",
            "thresholdRule": "offload iff len(data) > threshold (<= stays inline)",
            "base64": "std (not url-safe)",
            "shaCase": "lower-hex",
            "metaValueEncoding": "base64-std of the raw metadata VALUE bytes",
            "refJson": "structure-defined (sha256/size/meta); whitespace is NOT part of "
                       "the contract. Byte-exact surfaces: marker, casKey, object bytes.",
            "prefixJoin": "The store prefix (KONTRA_S3_PREFIX) is a PATH SEGMENT, never a "
                          "string glued to the front of the key. Algorithm, identical in every "
                          "language: strip leading and trailing '/' from EVERY segment "
                          "including the prefix, drop the segments that are then empty, join "
                          "what is left with a single '/'. An empty prefix yields "
                          "'cas/<sha[:2]>/<sha>' with no leading slash.",
            "prefixTrailingSlash": "NOT SIGNIFICANT -- this is the decision prefixCases exists "
                                   "to record. 'p', 'p/', '/p' and '/p/' all name the ONE "
                                   "namespace 'p/cas/...', and a prefix of '/' is an empty "
                                   "prefix. The alternative (a slash is a literal character in "
                                   "the prefix, so 'p/' addresses 'p//cas/...') was what "
                                   "runtime/handler/internal/objectstore.Key and "
                                   "control/orchestrator/src/codec/objectStore.ts:key did before this corpus "
                                   "was extended, and it is rejected: two spellings an operator "
                                   "reads as identical addressing two different namespaces, "
                                   "with no error, is the same defect class as the "
                                   "concatenation these rows were added to catch. Measured "
                                   "against the appliance's own object store, a 'p//cas/...' "
                                   "key is not merely odd -- it is REFUSED, 'object key ... has "
                                   "an empty path segment'.",
        },
        "prefixNote": "Every row below is the SAME payload as the 'offload-just-over' case "
                      "('0123456789abcdefg', 17 bytes, threshold 16, sha 8378b19d...). Only the "
                      "prefix varies, so a failure names the join and nothing else. Until "
                      "2026-08-29 no row in this file carried a prefix at all, and the "
                      "empty-prefix case is the one input class where concatenation and "
                      "segment-joining CANNOT differ -- so six green arms said nothing about a "
                      "live bug in two of them (ADR 0035 finding 1).",
        "prefixCases": prefix_cases,
        "cases": cases,
    }
    out = Path(__file__).parent / "fixtures.json"
    out.write_text(json.dumps(doc, indent=2) + "\n")
    print(f"wrote {out} ({len(cases)} cases, {len(prefix_cases)} prefix cases)")


if __name__ == "__main__":
    build()
