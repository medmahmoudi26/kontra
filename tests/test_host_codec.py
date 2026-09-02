"""The actor host must install the claim-check codec on its Temporal client.

THE BUG THIS PINS. The actor is a Temporal activity worker, so it sits directly on the payload
path — but its client was built with `Client.connect(address, namespace=...)` and no
data_converter. The handler encodes any payload over 128 KiB as `binary/claim-check-v1`, so
every over-threshold batch died with

    Failed decoding arguments: 'Unknown payload encoding binary/claim-check-v1'

retried to exhaustion. Reproduced live at 200 KB: 10/10 attempts failed. Small batches passed
throughout, which is why nothing caught it — the tests, the suites and a 6-seed smoke run are all
under the threshold.

These are cheap structural checks on purpose. The wire format itself is already pinned by the
cross-language corpus (`conformance/codec/fixtures.json`); what was missing was anything
asserting the codec is actually WIRED IN.
"""

from __future__ import annotations

import inspect

from internals import casstore
from internals.codec import DEFAULT_THRESHOLD, ClaimCheckCodec
from internals.temporal import host


def test_the_host_builds_its_client_with_a_data_converter():
    src = inspect.getsource(host.serve_async)
    assert "data_converter" in src, (
        "the actor host must pass a data_converter to Client.connect, or every batch over "
        f"{DEFAULT_THRESHOLD} bytes fails to decode"
    )


def test_the_converter_carries_the_claim_check_codec():
    dc = casstore.data_converter()
    assert isinstance(dc.payload_codec, ClaimCheckCodec), dc.payload_codec


def test_no_object_store_means_passthrough_not_a_crash():
    """A local run with no KONTRA_S3_ENDPOINT must still build a converter. The handler is
    passthrough under the same condition, so neither side offloads and neither has anything to
    fetch — but a codec that raised here would break every local run instead."""
    import os

    saved = os.environ.pop("KONTRA_S3_ENDPOINT", None)
    try:
        assert casstore.from_env() is None
        dc = casstore.data_converter()
        assert isinstance(dc.payload_codec, ClaimCheckCodec)
    finally:
        if saved is not None:
            os.environ["KONTRA_S3_ENDPOINT"] = saved


def test_the_cas_key_matches_every_other_implementation():
    """`cas/<sha[:2]>/<sha>` — the address the handler wrote to and the actor reads from. A
    disagreement here is a decode failure on another machine, not a test failure here."""
    from internals.codec import cas_key

    sha = "a" * 64
    assert cas_key(sha) == f"cas/aa/{sha}"
