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
cross-language corpus (`shared/conformance/codec/fixtures.json`); what was missing was anything
asserting the codec is actually WIRED IN.
"""

from __future__ import annotations

import inspect

from internals import casstore
from internals.codec import DEFAULT_THRESHOLD, ClaimCheckCodec
from internals.temporal import host


def test_the_client_is_built_with_the_claim_check_codec(monkeypatch):
    """What reaches `Client.connect`, not what the source says.

    THIS USED TO GREP `host.serve_async` FOR THE STRING "data_converter", once per host. That
    passes on a host that spells it and says nothing about the client that is actually built — and
    it broke the moment the construction moved into `internals/temporal/connect.py`, which is the
    shape a text assertion always fails in: the code got better and the test got worse.

    Both hosts now connect through `connect()`, so asserting here covers the actor host, the
    workflow host, the per-Session worker and `catalog.client()` at once. Without the codec every
    payload over DEFAULT_THRESHOLD comes back as `Unknown payload encoding binary/claim-check-v1`.
    """
    import asyncio

    from temporalio.client import Client

    from internals.temporal import connect as kconnect

    seen: dict = {}

    async def fake_connect(address, **kw):
        seen["address"] = address
        seen.update(kw)
        return object()

    monkeypatch.setattr(Client, "connect", staticmethod(fake_connect))
    asyncio.run(kconnect.connect("nscheck-0.1.0-sessions"))

    dc = seen.get("data_converter")
    assert dc is not None, "no data_converter reached Client.connect"
    assert isinstance(dc.payload_codec, ClaimCheckCodec), dc.payload_codec
    # The identity rides on the CLIENT as well as the Worker — half of what a process did is
    # otherwise attributed to `<pid>@<hostname>` and the other half to the worker's name.
    assert seen.get("identity"), "the client must carry this process's Temporal identity"


def test_a_caller_can_override_what_the_client_is_built_with(monkeypatch):
    """Defaulted, not forced. `**kwargs` reaching `Client.connect` is what lets a test or another
    process build a client kontra did not anticipate — the whole point of exposing the object."""
    import asyncio

    from temporalio.client import Client

    from internals.temporal import connect as kconnect

    seen: dict = {}

    async def fake_connect(address, **kw):
        seen.update(kw)
        return object()

    monkeypatch.setattr(Client, "connect", staticmethod(fake_connect))
    asyncio.run(kconnect.connect("q", identity="mine", data_converter="theirs"))

    assert seen["identity"] == "mine"
    assert seen["data_converter"] == "theirs"


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
