"""The claim-check blob store — the actor side of the codec's CAS (ADR 0005).

`internals/codec.py` owns the wire format (the threshold, the marker, the ref shape) but takes a
`BlobStore` it never constructs. This is that store: plain S3 get/put under the SAME
`KONTRA_S3_*` contract the handler and the orchestrator read, so all three address one CAS.

WHY THIS EXISTS AT ALL — do not remove it.

The handler's client encodes any payload over 128 KiB into a `binary/claim-check-v1` ref, and the
worker that executes the activity must be able to decode it. A worker without this codec fails
EVERY over-threshold batch with

    Failed decoding arguments: 'Unknown payload encoding binary/claim-check-v1'

retried to exhaustion — which is a hard failure, not a silent one, but it only appears once a
batch is big enough, so small runs pass and a real scope run does not.
"""

from __future__ import annotations

import os
from typing import TYPE_CHECKING, Optional

if TYPE_CHECKING:  # typing only — `internals.codec` is imported lazily below, and stays that way
    from internals.codec import BlobStore


def object_key(prefix: str, *parts: str) -> str:
    """Join a store prefix and key parts as PATH SEGMENTS — the Python side of one
    cross-language address.

    Strip leading and trailing '/' from EVERY segment, the prefix included; drop the ones that
    are then empty; join what is left with a single '/'. An empty prefix yields a key with no
    leading slash.

    WHAT IT REPLACES. `put` and `get` used to spell the address
    `self._prefix + key`, plain concatenation with nothing between the two — so with
    KONTRA_S3_PREFIX=`slice11` a Python-served workflow wrote `slice11cas/df/df5b…` while the
    handler, the orchestrator and the CLI all asked for `slice11/cas/df/df5b…`. A claim-check
    written by a Python actor under a non-empty prefix could not be read back by anything else,
    including a second Python worker reading through any other implementation's key.

    The peers are `handler/internal/objectstore.Key`, `runtime/go/codec.objectKey` and
    `control/orchestrator/src/codec/objectStore.ts:key`. Nothing imports across those boundaries, so what
    holds the four to one answer is the `prefixCases` rows of shared/conformance/codec/fixtures.json —
    which did not exist until this bug did, because every row carried the empty prefix, the one
    input class where concatenation and segment-joining CANNOT differ (ADR 0035 finding 1).
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


class PrefixedStore:
    """A `codec.BlobStore` that addresses another one under a store prefix.

    THE TRANSPORT IS ONE THING AND THE ADDRESS IS ANOTHER, which is the split
    `handler/claimcheck` already argues for in Go: a backing knows how to move bytes to a
    bucket, and where in that bucket they go is the codec's contract with three other
    languages. Keeping the join here rather than inside `S3CasStore.put` is also what makes it
    testable — the conformance arm wraps an in-memory store and drives the real join, with no
    boto3 and no S3.
    """

    def __init__(self, inner: "BlobStore", prefix: str) -> None:
        self._inner = inner
        self._prefix = prefix

    def put(self, key: str, data: bytes) -> None:
        self._inner.put(object_key(self._prefix, key), data)

    def get(self, key: str) -> bytes:
        return self._inner.get(object_key(self._prefix, key))


class S3CasStore:
    """A `codec.BlobStore` over S3. Sync on purpose: the codec's own interface is sync, and it
    runs on the worker's payload path, not inside an author's Method.

    It takes keys VERBATIM. The prefix is `PrefixedStore`'s, not this class's — see there.
    """

    def __init__(self, endpoint: str, bucket: str,
                 access: str, secret: str, region: str) -> None:
        import boto3  # actor-image / [seaweed] extra dep, not a base SDK requirement

        self._s3 = boto3.client(
            "s3", endpoint_url=endpoint, region_name=region,
            aws_access_key_id=access, aws_secret_access_key=secret,
        )
        self._bucket = bucket

    def put(self, key: str, data: bytes) -> None:
        self._s3.put_object(Bucket=self._bucket, Key=key, Body=data,
                            ContentType="application/octet-stream")

    def get(self, key: str) -> bytes:
        return self._s3.get_object(Bucket=self._bucket, Key=key)["Body"].read()


def from_env() -> Optional[PrefixedStore]:
    """Build the store, or None when no object store is configured.

    None means the codec runs in PASSTHROUGH — correct for a local run with no S3, and safe
    because the handler's codec is passthrough under the same condition: neither side offloads,
    so neither side has anything to fetch. It NEVER falls back to a default endpoint, because a
    codec pointed at the wrong store fails at decode time on another machine.
    """
    endpoint = os.environ.get("KONTRA_S3_ENDPOINT")
    if not endpoint:
        return None
    return PrefixedStore(
        S3CasStore(
            endpoint=endpoint,
            bucket=os.environ.get("KONTRA_S3_BUCKET", "kontra"),
            access=os.environ.get("KONTRA_S3_ACCESS_KEY", "kontra"),
            secret=os.environ.get("KONTRA_S3_SECRET_KEY", "kontra"),
            region=os.environ.get("KONTRA_S3_REGION", "us-east-1"),
        ),
        os.environ.get("KONTRA_S3_PREFIX", ""),
    )


def data_converter():
    """The DataConverter an actor's Temporal client must use.

    Byte-identical in effect to the handler's `converter.NewCodecDataConverter(default, codec)`
    (handler/main.go): same threshold, same `cas/<sha[:2]>/<sha>` key, same marker — all three
    pinned by the shared corpus in `shared/conformance/codec/`.
    """
    import dataclasses

    from temporalio.converter import DataConverter, default as default_converter

    from internals.codec import ClaimCheckCodec

    return dataclasses.replace(default_converter(), payload_codec=ClaimCheckCodec(from_env()))
