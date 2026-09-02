"""Claim-check payload codec — the Python peer of handler/internal/codec and the
orchestrator's claimCheck.ts.

Payloads over a threshold are offloaded to the CAS and replaced by a small ref payload,
transparently rehydrated on decode. All three implementations MUST agree byte-for-byte:
`shared/conformance/codec/fixtures.json` is the oracle, and any disagreement silently drops data
across the namespace boundary (see shared/conformance/codec/README.md — that drift has happened
once already).

This module owns only the Payload-level concerns: the threshold rule, the marker, the
metadata base64 round-trip and the double-encode guard. Storage is injected, so the wire
format is testable with no S3 and no temporalio import.

The Temporal adapter lives at the bottom and imports temporalio lazily, so an actor host
that never becomes a Temporal client keeps working unchanged.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
from typing import Protocol

# VERBATIM — byte-identical across Go, Python and TypeScript.
MARKER = "binary/claim-check-v1"

# Offload iff len(data) > threshold (128 KiB). Note STRICTLY greater: len == threshold
# stays inline, which fixtures.json pins with a dedicated case.
DEFAULT_THRESHOLD = 128 * 1024

# Temporal's own metadata key for the payload encoding. Spelled out rather than imported
# so this module has no hard temporalio dependency.
METADATA_ENCODING = "encoding"


def threshold_from_env() -> int:
    """KONTRA_S3_THRESHOLD (base-10) else DEFAULT_THRESHOLD. Mirrors Go's ThresholdFromEnv."""
    raw = os.environ.get("KONTRA_S3_THRESHOLD", "")
    if raw:
        try:
            return int(raw)
        except ValueError:
            pass
    return DEFAULT_THRESHOLD


def cas_key(sha256_hex: str) -> str:
    """`cas/<sha[:2]>/<sha>` — the two-char shard prefix keeps any one directory small."""
    return f"cas/{sha256_hex[:2]}/{sha256_hex}"


def digest(data: bytes) -> str:
    """Lower-hex sha256. The case matters: it is part of the CAS key."""
    return hashlib.sha256(data).hexdigest()


class BlobStore(Protocol):
    """The slice of an object store the codec needs. Injected so the wire format is
    testable without S3."""

    def put(self, key: str, data: bytes) -> None: ...

    def get(self, key: str) -> bytes: ...


def encode_payload(
    metadata: dict[str, bytes], data: bytes, store: BlobStore, threshold: int
) -> tuple[dict[str, bytes], bytes]:
    """Offload one payload if it is over threshold and not already a ref.

    Returns the (metadata, data) to put on the wire — unchanged when inline.
    """
    # Double-encode guard: an already-marked payload is a ref, never re-offloaded.
    if metadata.get(METADATA_ENCODING) == MARKER.encode():
        return metadata, data
    if len(data) <= threshold:
        return metadata, data

    sha = digest(data)
    store.put(cas_key(sha), data)
    # The ORIGINAL metadata rides in the ref, base64-std per value, so non-UTF-8 values
    # (fixtures pins an `x-bin` case) survive a JSON round-trip.
    meta = {k: base64.standard_b64encode(v).decode("ascii") for k, v in metadata.items()}
    ref = {"sha256": sha, "size": len(data), "meta": meta}
    return {METADATA_ENCODING: MARKER.encode()}, json.dumps(ref).encode("utf-8")


def decode_payload(
    metadata: dict[str, bytes], data: bytes, store: BlobStore
) -> tuple[dict[str, bytes], bytes]:
    """Rehydrate one ref payload, restoring the original metadata. Non-ref payloads pass
    through untouched — only the marker triggers a fetch, never the payload's shape."""
    if metadata.get(METADATA_ENCODING) != MARKER.encode():
        return metadata, data

    ref = json.loads(data)
    sha = ref["sha256"]
    blob = store.get(cas_key(sha))
    # Verify: a silent mismatch here is the failure mode the whole conformance corpus exists
    # to prevent, so it is checked rather than trusted.
    got = digest(blob)
    if got != sha:
        raise ValueError(f"claim-check digest mismatch: want {sha}, got {got}")
    meta = {k: base64.standard_b64decode(v) for k, v in (ref.get("meta") or {}).items()}
    return meta, blob


class ClaimCheckCodec:
    """temporalio PayloadCodec adapter. Constructed with a store (None => passthrough).

    Kept separate from the pure functions above so the wire format can be conformance-tested
    without importing temporalio.
    """

    def __init__(self, store: BlobStore | None, threshold: int | None = None) -> None:
        self._store = store
        self._threshold = threshold if threshold is not None else threshold_from_env()

    async def encode(self, payloads):
        if self._store is None:
            return list(payloads)
        out = []
        for p in payloads:
            meta, data = encode_payload(dict(p.metadata), p.data, self._store, self._threshold)
            out.append(_payload(meta, data))
        return out

    async def decode(self, payloads):
        if self._store is None:
            return list(payloads)
        out = []
        for p in payloads:
            meta, data = decode_payload(dict(p.metadata), p.data, self._store)
            out.append(_payload(meta, data))
        return out


def _payload(metadata: dict[str, bytes], data: bytes):
    """Build a temporalio Payload. Imported lazily so this module stays usable (and
    testable) in a host that has no temporalio installed."""
    from temporalio.api.common.v1 import Payload

    return Payload(metadata=metadata, data=data)
