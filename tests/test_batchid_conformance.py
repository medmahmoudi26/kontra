"""The PYTHON ARM of the Batch content-hash contract (shared/conformance/batchid.json).

WHY THIS CORPUS EXISTS, and why the test it replaces did not do its job. Both SDKs key a committed
Unit by the Batch's content hash (ADR 0023 §17), and the hash is a canonical JSON encoding of
`[method, units, params]`. The two encoders did not agree: Go's `encoding/json` escapes `<`, `>`
and `&` for browser safety, and Python's `json.dumps` escapes every non-ASCII character. So the
same Batch hashed to different keys the moment a Unit carried a query string or an accent.

MEASURED 2026-08-27, method "probe", empty params:

    https://acme.com/?a=1&b=2   py d8868b6200c19340   go 6250da42654ec10a
    café                        py 4fc02e4dd607c61d   go d0de34ff30eafbc4
    <script>                    py 7e16c1cbdab1e24b   go 6513f87dedc4a622
    plain-ascii                 both 76457373b3bbfe8c

The single golden that guarded this asserted `{"url": "a"}`, `{"url": "b"}` and `{"depth": 2}` —
plain ASCII with no symbols, the ONE input class where the two encoders cannot differ — while a URL
with a query string is this repo's canonical Unit. Its own comment claimed "a drift in either
encoder shows up here". It could not. That golden is gone; this corpus is what replaced it, and the
Go arm asserts the same file.

WHAT IS AND IS NOT AT STAKE, stated plainly because the honest bound belongs beside the fix: one
Batch is served by one process in one language, so this hash does not cross the SDK boundary at
run time. What was broken was a guard that advertised protection it did not provide — and the
commit-key derivation that every recovery path depends on.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from internals.engine import batch_id

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "shared" / "conformance" / "batchid.json"

CASES = json.loads(FIXTURE.read_text(encoding="utf-8"))["cases"]


def test_the_corpus_is_not_empty_and_covers_the_characters_that_diverged() -> None:
    """A corpus that silently shrank to nothing would pass every case below.

    The named characters are the point of the file: an ASCII-only corpus is exactly the corpus
    that let this bug live, so their presence is asserted rather than assumed.
    """
    assert len(CASES) >= 10
    blob = json.dumps(CASES, ensure_ascii=False)
    for ch in ("&", "<", ">", "café", "\U0001f4e6"):
        assert ch in blob, f"the corpus no longer exercises {ch!r}"


@pytest.mark.parametrize("case", CASES, ids=[c["why"][:48] for c in CASES])
def test_the_hash_matches_the_corpus(case: dict) -> None:
    assert batch_id(case["method"], case["units"], case["params"]) == case["expect"]


def test_the_encoder_escapes_nothing() -> None:
    """The PROPERTY behind every row above, asserted directly.

    A future maintainer who reinstates `ensure_ascii` (or its Go peer's HTML escaping) makes every
    non-ASCII row fail, which is loud. This says WHY they failed, in one line, so the next reader
    does not have to re-derive it from thirteen mismatched digests.
    """
    import hashlib

    raw = json.dumps(["m", ["a&b<c>"], {}], sort_keys=True, default=str,
                     ensure_ascii=False, separators=(",", ":"))
    assert "\\u0026" not in raw and "\\u003c" not in raw, "HTML escaping is back"
    assert "café" in json.dumps(["m", ["café"], {}], ensure_ascii=False)
    assert batch_id("m", ["a&b<c>"], {}) == hashlib.sha256(raw.encode()).hexdigest()[:16]
