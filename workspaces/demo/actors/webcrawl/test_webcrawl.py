"""webcrawl unit tests — the parts that are wrong silently.

Everything here runs without a network and without a browser except the one test marked
`e2e`, because the failures that actually cost a run are the quiet ones: a header set
that blows up a column store, a seed that is not a URL, an event that cannot be joined back
to its request.

    .venv-actor/bin/python -m pytest examples/python/webcrawl/test_webcrawl.py -q
"""

import asyncio
import json
import sys
from pathlib import Path

import pytest

def _find_sdk() -> Path:
    """Walk up for `sdk/python/kontra` instead of counting `parents[N]`.

    A COUNTED DEPTH IS WRONG THE MOMENT THE FILE MOVES, and this one has moved twice already
    (examples/python/webcrawl -> workspaces/bugbounty/actors/webcrawl -> the tracked actor
    source). Worse, getting it wrong does not raise: kontra-local's venv carries its own copy of
    the SDK, so a missing root silently tests a DIFFERENT kontra than this actor ships against.
    Searching for the thing itself survives every move; the assert makes a miss loud.
    """
    here = Path(__file__).resolve()
    for parent in here.parents:
        for cand in (parent / "sdk" / "python", parent / "kontra" / "sdk" / "python"):
            if (cand / "kontra").is_dir():
                return cand
    raise AssertionError(f"no sdk/python/kontra above {here}")


sys.path[:0] = [str(_find_sdk()), str(Path(__file__).resolve().parent)]

import actor as wc  # noqa: E402
import kontra  # noqa: E402
from kontra.testing import collecting_dataset, stub_batch  # noqa: E402

_lib = sys.modules[kontra.actor.__class__.__module__]


@pytest.fixture(autouse=True)
def _bound_params():
    """`param.get()` returns a lazy ParamRef outside a hosted run — binding an empty param
    map is what a real turn does, and without it every default reads back as a ref."""
    token = _lib._run_params.set({})
    yield
    _lib._run_params.reset(token)


def test_headers_are_lowercased_and_json():
    out = json.loads(wc._headers_json({"Content-Type": "text/html", "X-A": "1"}))
    assert out == {"content-type": "text/html", "x-a": "1"}


def test_a_giant_header_is_truncated_visibly():
    # A CSP or Set-Cookie can run to kilobytes. Truncation must be visible IN the value:
    # a silently-clipped header reads as a real header that happens to be malformed.
    out = json.loads(wc._headers_json({"content-security-policy": "x" * 9000}))
    v = out["content-security-policy"]
    assert len(v) < 9000 and v.endswith("[truncated]")


def test_missing_content_length_is_minus_one_not_zero():
    # 0 would mean "an empty body was served". -1 means "we were not told".
    assert wc._int_header({}, "content-length") == -1
    assert wc._int_header({"content-length": "nope"}, "content-length") == -1
    assert wc._int_header({"content-length": "42"}, "content-length") == 42


def test_every_event_carries_the_full_output_shape():
    # The output type is one flat record; a missing key becomes a NULL column for every row
    # in the parquet file, which is discovered a run too late.
    inst = wc.WebCrawl()
    inst._seq = 0
    ev = inst._event("https://s", "prog", "request", "https://u")
    assert set(ev) == {f.name for f in wc.HttpEvent.__dataclass_fields__.values()}


def test_a_non_url_seed_emits_one_event_rather_than_vanishing():
    # Scope rows are not all navigable ("com.stubhubandroid" is a real scope_paid row). A
    # dropped seed is indistinguishable from a seed that produced nothing.
    inst = wc.WebCrawl()
    inst._seq = 0

    async def go():
        ds = collecting_dataset()
        await inst.crawl(stub_batch([{"seed": "com.stubhubandroid"}], takes=wc.Seed), ds)
        return ds.records

    out = asyncio.run(go())
    assert len(out) == 1
    assert out[0]["error"] == "not an http(s) url"
    assert out[0]["seed"] == "com.stubhubandroid"


def test_tx_ids_are_unique_within_a_session():
    inst = wc.WebCrawl()
    inst._seq = 0
    assert len({inst._tx() for _ in range(1000)}) == 1000


@pytest.mark.e2e
def test_live_one_context_per_unit_and_requests_pair_with_responses():
    """The whole contract in one test: a browser at load, a context per unit, concurrent
    contexts, and request/response objects that join on tx."""

    async def go():
        inst = wc.WebCrawl()
        await inst.open_browser()
        try:
            batches = await asyncio.gather(*(
                _collect(inst, u) for u in
                ("https://example.com", "https://example.org", "https://www.iana.org")
            ))
        finally:
            await inst.close_browser()
        return batches

    batches = asyncio.run(go())
    assert all(b for b in batches), "every seed produced at least one event"
    for events in batches:
        reqs = {e["tx"] for e in events if e["kind"] == "request"}
        resps = {e["tx"] for e in events if e["kind"] == "response"}
        assert reqs, "a navigation always issues at least one request"
        assert resps & reqs, "responses join back to their requests on tx"


async def _collect(inst, url):
    """Drive `crawl` the way a caller does (ADR 0028): the Batch is the input the caller passes
    first, the Dataset the destination it passes second, and `.records` is what the body pushed."""
    ds = collecting_dataset()
    await inst.crawl(stub_batch([{"seed": url, "program": "test"}], takes=wc.Seed), ds)
    return ds.records


def test_tx_is_unique_across_loads():
    """`tx` joins a request to its response across a whole DATASET, so a counter that restarts
    on every `@actor.load` is not unique enough — and it was not.

    MEASURED on surface-1789866474: 799 request events carried 282 distinct `tx`, three separate
    seeds all minted `00000056`, and `_exchanges` (which joins on `tx` alone) turned 799 requests
    into 2,349 rows. `injection_points` inherited the same fan-out.
    """
    import actor as m

    a, b = m.WebCrawl(), m.WebCrawl()
    a._seq, a._nonce = 0, "aaaaaaaa"
    b._seq, b._nonce = 0, "bbbbbbbb"

    first = [a._tx() for _ in range(50)]
    second = [b._tx() for _ in range(50)]

    assert len(set(first)) == 50, "ids collide within one load"
    assert not (set(first) & set(second)), (
        "two loads minted the same tx — this is the fan-out that produced 2,349 exchanges "
        "from 799 requests"
    )


def test_tx_works_before_load_has_run():
    """`_event` falls back to `self._tx()`, and a Method exercised without `@actor.load` must
    fail on the browser it lacks, not on an attribute the id generator forgot to default."""
    import actor as m

    got = m.WebCrawl()._tx()
    assert got.endswith("00000001") and len(got) == 16
