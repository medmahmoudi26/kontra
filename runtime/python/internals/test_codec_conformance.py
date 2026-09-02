"""The Python codec against shared/conformance/codec/fixtures.json — the SAME corpus the Go codec
and the orchestrator's claimCheck.ts must pass.

This file is the reason the three implementations can be trusted to agree. It asserts the
byte-exact surfaces the fixture header names: the marker, the CAS key, and the stored object
bytes. Ref JSON whitespace is explicitly NOT part of the contract, so the ref is compared
structurally.
"""

from __future__ import annotations

import base64
import json
from pathlib import Path

import pytest

from internals.casstore import PrefixedStore, object_key
from internals.codec import MARKER, cas_key, decode_payload, encode_payload

# parents[3] is the repo root: internals/ -> python/ -> runtime/ -> <root>, the same depth the
# old actorkit/python/internals was.
FIXTURES = Path(__file__).resolve().parents[3] / "shared" / "conformance" / "codec" / "fixtures.json"


class MemStore:
    """In-memory BlobStore. Records exactly what key each object landed under, which is
    what lets the test assert the CAS layout rather than just the round-trip."""

    def __init__(self) -> None:
        self.objects: dict[str, bytes] = {}

    def put(self, key: str, data: bytes) -> None:
        self.objects[key] = data

    def get(self, key: str) -> bytes:
        return self.objects[key]


def _cases():
    doc = json.loads(FIXTURES.read_text())
    return [(c["name"], c) for c in doc["cases"]]


def _prefix_cases():
    doc = json.loads(FIXTURES.read_text())
    return [(c["name"], c) for c in doc["prefixCases"]]


@pytest.mark.parametrize("name,case", _cases(), ids=[n for n, _ in _cases()])
def test_encode_matches_fixture(name, case):
    store = MemStore()
    meta = {k: base64.standard_b64decode(v) for k, v in case["input"]["meta_b64"].items()}
    data = base64.standard_b64decode(case["input"]["data_b64"])
    expect = case["expect"]

    out_meta, out_data = encode_payload(meta, data, store, case["threshold"])

    if expect["action"] in ("inline", "unchanged"):
        assert out_meta == meta, f"{name}: metadata must pass through untouched"
        assert out_data == data, f"{name}: data must pass through untouched"
        assert store.objects == {}, f"{name}: nothing may be offloaded"
        return

    # --- offload ---
    assert out_meta == {"encoding": MARKER.encode()}, f"{name}: marker must be stamped verbatim"

    ref = json.loads(out_data)
    assert ref["sha256"] == expect["ref"]["sha256"], f"{name}: digest"
    assert ref["size"] == expect["ref"]["size"], f"{name}: size"
    assert ref["meta"] == expect["ref"]["meta"], f"{name}: metadata base64 round-trip"

    key = expect["cas_key"]
    assert key == cas_key(ref["sha256"]), f"{name}: CAS key layout"
    assert key in store.objects, f"{name}: object must be stored at the fixture's key"
    assert store.objects[key] == base64.standard_b64decode(expect["object_b64"]), (
        f"{name}: stored object bytes must be the ORIGINAL payload, byte for byte"
    )


@pytest.mark.parametrize("name,case", _cases(), ids=[n for n, _ in _cases()])
def test_round_trip_restores_original(name, case):
    """encode -> decode returns exactly what went in, for every case including the
    non-UTF-8 metadata one.

    The `unchanged` case is excluded on purpose: it is a payload already STAMPED with the
    marker whose body is deliberately not ref JSON, so it exercises the encode-side
    double-encode guard only. Feeding it to decode asserts nothing about the round trip —
    and correctly raises, exactly as Go's json.Unmarshal would.
    """
    if case["expect"]["action"] == "unchanged":
        pytest.skip("double-encode guard fixture: body is not a real ref by construction")
    store = MemStore()
    meta = {k: base64.standard_b64decode(v) for k, v in case["input"]["meta_b64"].items()}
    data = base64.standard_b64decode(case["input"]["data_b64"])

    enc_meta, enc_data = encode_payload(meta, data, store, case["threshold"])
    dec_meta, dec_data = decode_payload(enc_meta, enc_data, store)

    assert dec_data == data, f"{name}: payload bytes must survive the round trip"
    assert dec_meta == meta, f"{name}: metadata must survive the round trip"


def test_payload_that_looks_like_a_ref_is_opaque():
    """Only the MARKER triggers a decode. A payload whose bytes happen to look like a ref
    must be returned untouched, or user data becomes a fetch."""
    store = MemStore()
    looks_like = json.dumps({"sha256": "deadbeef", "size": 3, "meta": {}}).encode()
    meta = {"encoding": b"json/plain"}

    out_meta, out_data = decode_payload(meta, looks_like, store)

    assert out_data == looks_like
    assert out_meta == meta


@pytest.mark.parametrize("name,case", _prefix_cases(), ids=[n for n, _ in _prefix_cases()])
def test_the_store_prefix_is_a_path_segment(name, case):
    """The prefix rows: encode a real payload through a REAL prefixed store and assert the
    object landed at the address the corpus names.

    This is the arm that was missing. Every row above runs with no prefix, and with no prefix
    `prefix + key` and `join(prefix, key)` are the same bytes — so six green arms said nothing
    about a Python worker writing `slice11cas/df/df5b…` where everything else read
    `slice11/cas/df/df5b…`. The store is driven, not `object_key`, because the bug was in the
    store's spelling of the address and a test of the helper alone would have passed over it.
    """
    store = MemStore()
    prefixed = PrefixedStore(store, case["prefix"])
    data = base64.standard_b64decode(case["input"]["data_b64"])
    meta = {k: base64.standard_b64decode(v) for k, v in case["input"]["meta_b64"].items()}
    want = case["expect"]["key"]

    out_meta, out_data = encode_payload(meta, data, prefixed, case["threshold"])

    ref = json.loads(out_data)
    assert ref["sha256"] == case["expect"]["sha256"], f"{name}: digest"
    assert list(store.objects) == [want], (
        f'{name}: with prefix {case["prefix"]!r} the object must land at {want!r}, '
        f"got {list(store.objects)!r}"
    )
    assert store.objects[want] == data, f"{name}: stored bytes"

    # And the READ side derives the same address — a writer and a reader that disagree is the
    # same outage in the other direction.
    dec_meta, dec_data = decode_payload(out_meta, out_data, prefixed)
    assert dec_data == data, f"{name}: round trip through the prefixed store"
    assert dec_meta == meta, f"{name}: metadata round trip"


@pytest.mark.parametrize("name,case", _prefix_cases(), ids=[n for n, _ in _prefix_cases()])
def test_object_key_matches_the_corpus(name, case):
    """The join itself, called the way the peers' own key functions are called. `cas_key` gives
    the un-prefixed address; `object_key` puts it under the prefix."""
    got = object_key(case["prefix"], cas_key(case["expect"]["sha256"]))
    assert got == case["expect"]["key"], f'{name}: prefix {case["prefix"]!r}'


def test_decode_rejects_a_tampered_object():
    """A digest mismatch must raise, not silently hand back the wrong bytes."""
    store = MemStore()
    data = b"x" * 64
    enc_meta, enc_data = encode_payload({"encoding": b"json/plain"}, data, store, 8)
    key = cas_key(json.loads(enc_data)["sha256"])
    store.objects[key] = b"tampered"

    with pytest.raises(ValueError, match="digest mismatch"):
        decode_payload(enc_meta, enc_data, store)
