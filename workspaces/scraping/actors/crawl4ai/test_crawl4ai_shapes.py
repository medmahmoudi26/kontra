"""Pure-function tests for the crawl4ai actor: what is suppressed, and what decides "the same shape".

WHY THERE ARE TWO TEST FILES HERE, and it is not a split anybody designed. When ADR 0038 moved the
actors out of kontra, this actor's tests existed in two places and the STALER one travelled: an
eleven-test file describing the two-tier streaming design. The thirty-four tests below stayed
behind in kontra's `tests/`, where the actor they load no longer existed, so they could not run at
all — and nobody saw it, because kontra's CI has never been billed to run.

Neither file is a superset. `test_crawl4ai.py` uniquely covers browser-context isolation, the
resumable tier's wiring to unit state, and three real-crawl e2e tests. This one covers the
canonical request shape, the simhash radius, and the record contract. So they are both kept rather
than one being chosen, and this paragraph is here so the next person does not have to work that out
from the overlap.

There used to be two variants — `crawl4ai-canon` (exact md5 dedupe) and `crawl4ai-simhash`
(near-duplicate dedupe) — plus a cache/value yield gate. Both variants collapsed into ONE
crawler, and the value gate is gone. So the tests here defend exactly two things:

  WHAT IS SUPPRESSED — one thing, and only one thing: a page whose canonical REQUEST shape is
    within `simhash_distance` bits of a shape ALREADY EMITTED on this seed. There is no status
    gate, no cache gate and no static-asset gate any more, and the "…IS EMITTED" tests below
    exist to keep it that way: every one of them covers a page the old value gate silently
    DROPPED, which is the failure mode that costs a finding rather than a unit.

  WHAT DECIDES "THE SAME SHAPE" — the canonical string's three v1 design choices (host from the
    fetched url, query NAMES without values, header NAMES without values) each fixed a real
    incident, and the id-masking + Hamming radius on top of them is the whole reason a generated
    path space (/product/1001, /product/1002, …) collapses to one unit. Both errors are
    expensive and NOT symmetric: too wide MERGES two endpoints and the second is never probed;
    too narrow only costs an extra unit.

The record shape is a third, quieter contract: the streaming sub-unit id is the sha of the WHOLE
record (ADR 0015), so a volatile field in it breaks idempotent re-emission.

No crawl4ai and no browser — the actor imports crawl4ai inside the Method body, so the module
loads (and these functions run) on a plain interpreter.
"""
from __future__ import annotations

import asyncio
import dataclasses
import importlib.util
import sys
import types
from pathlib import Path

import pytest

_ACTOR_PY = Path(__file__).resolve().parent / "actor.py"


def _load():
    """Load the actor under its own module name (NOT `crawl4ai` — that name belongs to the real
    package, which the Method imports from sys.modules and the tests below replace with a fake)."""
    from kontra import actor

    actor.load_fn = actor.close_fn = actor.healthcheck_fn = actor.methods = {}
    actor.input_type = actor.output_type = actor.params_type = actor.actor_class = None
    spec = importlib.util.spec_from_file_location("crawl4ai_actor", _ACTOR_PY)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mod.ACTOR_CLASS = actor.actor_class      # the registry is a module SINGLETON — capture now
    return mod


C4 = _load()


# ---------------------------------------------------------------------------------
# The canonical request string — ported byte-for-byte from the v1 proxy addon.

def test_the_canonical_string_keeps_the_v1_byte_layout():
    """`method host path?names [header names] body`, including the Python list repr of the header
    names and the trailing empty body. Every fingerprint in the run is a hash of this string, so
    a layout drift re-shapes all of them at once — and silently."""
    assert C4.canonical_string("GET", "https://ex.com/a?b=1", ["Accept"]) == \
        "GET ex.com /a?b ['accept'] "


def test_query_values_are_dropped_but_names_are_kept():
    """/api?x=1 and /api?x=2 are ONE shape to a header-reflection probe; ?x= and ?y= are two."""
    c = C4.canonical_string
    assert c("GET", "https://ex.com/api?x=1") == c("GET", "https://ex.com/api?x=2")
    assert c("GET", "https://ex.com/api?x=") != c("GET", "https://ex.com/api?y=")


def test_query_name_order_does_not_matter_but_the_name_set_does():
    c = C4.canonical_string
    assert c("GET", "https://ex.com/a?x=1&y=2") == c("GET", "https://ex.com/a?y=9&x=8")
    assert c("GET", "https://ex.com/a?x=1&y=2") != c("GET", "https://ex.com/a?x=1")


def test_header_values_are_excluded_so_sessions_collapse():
    """Same request under two session tokens is one shape — this is why values are excluded."""
    c = C4.canonical_string
    a = c("GET", "https://ex.com/a", {"cookie": "sid=aaa", "accept": "*/*"}.keys())
    b = c("GET", "https://ex.com/a", {"cookie": "sid=bbb", "accept": "*/*"}.keys())
    assert a == b
    # …but the header SET is part of the shape: an extra header is a different probe surface.
    assert a != c("GET", "https://ex.com/a", ["cookie", "accept", "x-forwarded-host"])


def test_header_names_are_case_folded_and_deduped():
    c = C4.canonical_string
    assert c("GET", "https://ex.com/a", ["Accept", "ACCEPT", "accept"]) == \
           c("GET", "https://ex.com/a", ["accept"])


def test_host_comes_from_the_fetched_url_port_stripped_and_lowercased():
    """v1 keyed on a client-sent Host and collided distinct subdomains of one SPA build. The host
    must come from the URL we actually fetched — and :443/:8080 must not split one host in two."""
    c = C4.canonical_string
    assert c("GET", "https://EX.com/a") == c("GET", "https://ex.com/a")
    assert c("GET", "https://ex.com:443/a") == c("GET", "https://ex.com/a")
    assert c("GET", "https://a.ex.com/x") != c("GET", "https://b.ex.com/x")
    # A forged Host HEADER cannot move the shape: only the header NAME is canonicalized.
    assert c("GET", "https://a.ex.com/x", {"host": "b.ex.com"}.keys()) == \
           c("GET", "https://a.ex.com/x", {"host": "a.ex.com"}.keys())


def test_method_path_and_body_separate_shapes():
    c = C4.canonical_string
    assert c("GET", "https://ex.com/a") != c("POST", "https://ex.com/a")
    assert c("GET", "https://ex.com/a") != c("GET", "https://ex.com/b")
    assert c("POST", "https://ex.com/a", (), "x=1") != c("POST", "https://ex.com/a", (), "x=2")
    assert c("get", "https://ex.com/a") == c("GET", "https://ex.com/a")


def test_fingerprint_request_prefers_the_browsers_own_captured_request():
    """The captured request carries the real header NAMES, which are part of the shape; without
    capture the synthesized GET must still produce a usable fingerprint."""
    captured = {"method": "GET", "url": "https://ex.com/a", "headers": {"accept": "*/*"}}
    record = {"url": "https://ex.com/a", "requests": [{"method": "GET", "url": "https://ex.com/z"},
                                                      captured]}
    assert C4.fingerprint_request(record) is captured
    assert C4.fingerprint_request({"url": "https://ex.com/a", "requests": []}) == \
        {"method": "GET", "url": "https://ex.com/a", "headers": {}}


# ---------------------------------------------------------------------------------
# simhash — near-duplicate collapse in REQUEST space.

def test_simhash_is_64_bit_and_deterministic():
    fp = C4.simhash64("GET ex.com /product/1001 ['accept'] ")
    assert 0 <= fp < 2 ** 64
    assert fp == C4.simhash64("GET ex.com /product/1001 ['accept'] ")
    assert C4.simhash64("") == 0


def test_identical_requests_have_distance_zero():
    s = C4.canonical_string("GET", "https://ex.com/a?x=1", ["accept"])
    t = C4.canonical_string("GET", "https://ex.com/a?x=2", ["accept"])   # values dropped
    assert C4.hamming_distance(C4.simhash64(s), C4.simhash64(t)) == 0


@pytest.mark.parametrize("path,masked", [
    ("/product/1001", "/product/#"),
    ("/orders/7/invoice", "/orders/#/invoice"),
    ("/u/9f3a2b1c/profile", "/u/#/profile"),
    ("/p/550e8400-e29b-41d4-a716-446655440000", "/p/#"),
    # NOT ids: a version, a hex-looking WORD (no digit), a partially numeric slug. Masking one of
    # these would merge two distinct endpoints and the second would never be probed — the
    # asymmetry that keeps this rule narrow.
    ("/api/v2/users", "/api/v2/users"),
    ("/decade/facade", "/decade/facade"),
    ("/news/story-77", "/news/story-77"),
])
def test_only_unambiguous_object_ids_are_masked(path, masked):
    assert C4.mask_object_ids(path) == masked


def test_a_generated_path_space_collapses_under_simhash():
    """The reason the fingerprint is a simhash of the MASKED string and not an md5 of the raw
    one: /product/1001… are one endpoint to a probe. Exact dedupe cannot see that — all four
    canonical strings differ — while the masked fingerprints land on top of each other."""
    urls = ["https://ex.com/product/1001", "https://ex.com/product/1002",
            "https://ex.com/product/98765", "https://ex.com/product/7"]
    reqs = [{"method": "GET", "url": u, "headers": {"accept": "*/*"}} for u in urls]
    assert len({C4.canonical_string("GET", u, ["accept"]) for u in urls}) == 4   # exact: 4 units
    fps = [C4.request_simhash(r) for r in reqs]
    assert all(C4.hamming_distance(fps[0], other) <= 3 for other in fps[1:])     # simhash: 1


def test_distinct_endpoints_stay_outside_the_default_radius():
    """A radius that swallows distinct endpoints silently drops targets, which is worse than an
    extra unit — so each of these must sit well outside the default 3."""
    def fp(url, headers=("accept",), method="GET"):
        return C4.request_simhash(
            {"method": method, "url": url, "headers": dict.fromkeys(headers, "x")})

    base = fp("https://ex.com/a")
    for other, why in [
        (fp("https://ex.com/b"), "different route"),
        (fp("https://a.ex.com/a"), "different host"),
        (fp("https://ex.com/a", method="POST"), "different method"),
        (fp("https://ex.com/a", headers=("accept", "x-forwarded-host")), "different header set"),
        (fp("https://ex.com/a?q="), "an extra query param name"),
        (fp("https://ex.com/a/items"), "an extra path segment"),
    ]:
        assert C4.hamming_distance(base, other) > 3, why


def test_no_safe_radius_collapses_sibling_leaves():
    """The rung ordering, MEASURED against the library implementation, is the reason the default
    radius is small — and it is not the ordering the old hand-rolled docstring claimed.

        different METHOD .................. 11 bits
        different HOST .................... 15 bits
        sibling route (/email vs /phone) .. 16 bits

    A radius wide enough to collapse two sibling pages (16) therefore ALSO collapses GET with
    POST (11) and two different hosts (15) — it would silently merge distinct targets into one
    unit. There is no safe setting above ~10, so the sibling rung is unreachable, not merely
    off by default. If this ordering ever changes, the `simhash_distance` comment block in the
    actor must change with it."""
    def fp(url, method="GET", headers=("accept",)):
        return C4.request_simhash(
            {"method": method, "url": url, "headers": dict.fromkeys(headers, "*/*")})

    email, phone = "https://ex.com/account/settings/email", "https://ex.com/account/settings/phone"
    sibling = C4.hamming_distance(fp(email), fp(phone))
    method = C4.hamming_distance(fp(email), fp(email, method="POST"))
    host = C4.hamming_distance(fp(email), fp("https://other.com/account/settings/email"))

    assert sibling > 10, f"sibling rung {sibling} — a small radius would now merge real targets"
    assert method < sibling, (
        f"different METHOD ({method}) must stay below the sibling rung ({sibling}); if it rises "
        "above, a radius tuned for siblings would no longer merge GET with POST and the actor's "
        "comment block is wrong")
    assert host < sibling, (
        f"different HOST ({host}) must stay below the sibling rung ({sibling}) — this is the "
        "ordering that makes any sibling-collapsing radius unsafe")


def test_near_duplicate_honours_the_radius():
    seen = [0b0000, 0b1111]
    assert C4.near_duplicate(0b0001, seen, 1) is True     # 1 bit from 0
    assert C4.near_duplicate(0b0011, seen, 1) is False    # 2 from 0, 2 from 15
    assert C4.near_duplicate(0b0011, seen, 2) is True
    assert C4.near_duplicate(0b0001, [], 64) is False     # nothing seen -> never a dup
    assert C4.near_duplicate(0b0001, seen, 0) is False    # radius 0 == exact match only
    assert C4.near_duplicate(0b0000, seen, 0) is True


def test_hamming_distance_is_symmetric_and_bounded():
    assert C4.hamming_distance(0, 2 ** 64 - 1) == 64
    assert C4.hamming_distance(2 ** 64 - 1, 0) == 64
    assert C4.hamming_distance(5, 5) == 0


# ---------------------------------------------------------------------------------
# The record: STABLE fields only (the sub-unit id is the sha of the whole record, ADR 0015).

def _result(**kw):
    base = dict(url="https://ex.com/a", success=True, status_code=200, html="<html>x</html>",
                markdown="x", links={"internal": [{"href": "https://ex.com/b"}]},
                metadata={"depth": 1}, response_headers={}, network_requests=[])
    return types.SimpleNamespace(**{**base, **kw})


_RECORD_KEYS = {"seed", "url", "depth", "status", "request", "response", "markdown", "links",
                "requests", "error"}


def test_the_record_has_exactly_the_documented_keys():
    """The output type is a dataclass the host validates against — an extra key is a schema
    failure downstream, a missing one is a silently absent column in the dataset."""
    record = C4._page_out(_result(), "https://ex.com/", False)
    assert set(record) == _RECORD_KEYS
    assert {f.name for f in dataclasses.fields(C4.PageOut)} == _RECORD_KEYS


def test_the_record_carries_no_cache_verdict_at_all():
    """The cache fields are GONE. A `Cache-Control` header states an intention, not the deployed
    behaviour, so carrying it downstream invites the next actor to trust it; whether a response
    is really cacheable is the cachebuster's job and it proves that behaviourally."""
    record = C4._page_out(_result(response_headers={
        "Cache-Control": "public, max-age=60", "Vary": "Accept-Encoding",
        "Age": "37", "X-Cache": "HIT"}), "https://ex.com/", False)
    assert "cache_control" not in record and "vary" not in record and "cached" not in record
    # …and no cache header leaks in under another name either.
    assert "max-age" not in repr(record) and "Accept-Encoding" not in repr(record)


def test_the_record_is_byte_identical_across_two_fetches_of_one_page():
    """Same page, different volatile headers — the record (and therefore the sub-unit id) must
    not move, or every re-crawl re-emits the same page under a brand new id."""
    a = C4._page_out(_result(response_headers={"content-type": "text/html", "Age": "1",
                                               "Date": "Thu, 01 Jan 2026 00:00:00 GMT"}),
                     "https://ex.com/", False)
    b = C4._page_out(_result(response_headers={"content-type": "text/html", "Age": "9999",
                                               "Date": "Fri, 02 Jan 2026 11:11:11 GMT"}),
                     "https://ex.com/", False)
    assert a == b
    assert "9999" not in repr(b) and "Jan 2026" not in repr(b)


def test_bad_seed_record_has_the_same_shape_as_a_page_record():
    """Both flow through one output type; a missing key would fail schema validation downstream."""
    assert set(C4._bad("nope")) == set(C4._page_out(_result(), "https://ex.com/", False))
    assert set(C4._bad("nope")) == _RECORD_KEYS


def test_discovery_mode_drops_the_markdown_and_nothing_else():
    """discovery_mode maps links fast. It must cost the markdown ONLY — when the body gate still
    existed, an empty markdown was also what made a page look worthless."""
    result = _result(html="<html>plenty</html>", markdown="# plenty")
    fast = C4._page_out(result, "https://ex.com/", True)
    full = C4._page_out(result, "https://ex.com/", False)
    assert fast["markdown"] == "" and full["markdown"] == "# plenty"
    assert {k: v for k, v in fast.items() if k != "markdown"} == \
           {k: v for k, v in full.items() if k != "markdown"}


def test_the_params_no_longer_expose_a_cache_knob():
    """`require_cache` was the operator's escape hatch from the cache gate. With the gate gone the
    knob must go too — a leftover no-op param reads like a filter that is still there."""
    names = {f.name for f in dataclasses.fields(C4.CrawlParams)}
    assert "require_cache" not in names
    assert "simhash_distance" in names      # the only knob over the only remaining filter


# ---------------------------------------------------------------------------------
# The seen-fingerprint checkpoint is BOUNDED (an unbounded one is what blew Temporal history).

def test_the_seen_list_is_bounded_and_round_trips_as_hex():
    seen = C4.load_seen(None)
    for i in range(C4._SEEN_CAP + 50):
        C4.remember(seen, i)
    assert len(seen) == C4._SEEN_CAP
    assert seen[-1] == C4._SEEN_CAP + 49 and 0 not in seen        # oldest evicted
    # A checkpoint written by an older/looser build is truncated on the way back in, too.
    assert len(C4.load_seen([f"{i:016x}" for i in range(C4._SEEN_CAP * 3)])) == C4._SEEN_CAP
    # 64-bit values leave as hex, never as JSON numbers (53-bit floats in the TS orchestrator).
    dumped = C4.dump_seen([2 ** 64 - 1, 5])
    assert dumped == ["ffffffffffffffff", "0000000000000005"]
    assert C4.load_seen(dumped) == [2 ** 64 - 1, 5]
    assert C4.load_seen(["zz", "1f"]) == [0x1f]     # a corrupt entry is skipped, not fatal


# ---------------------------------------------------------------------------------
# The Method, driven against a FAKE crawl4ai and a fake stream — no crawl4ai package, no browser,
# no live runtime. The dedupe, the yield cap and the checkpoint all live in the Method body, where a
# compile check proves nothing.

def _fake_crawl4ai(monkeypatch, captured):
    c4 = types.ModuleType("crawl4ai")
    c4.CacheMode = types.SimpleNamespace(BYPASS="bypass")

    class CrawlerRunConfig:
        session_id = None

        def __init__(self, **kw):
            self.kw = kw

    c4.CrawlerRunConfig = CrawlerRunConfig
    dc = types.ModuleType("crawl4ai.deep_crawling")

    class DFSDeepCrawlStrategy:
        def __init__(self, **kw):
            captured["strategy"] = kw

    dc.DFSDeepCrawlStrategy = DFSDeepCrawlStrategy
    # No BFSDeepCrawlStrategy on the fake module: importing it must fail loudly if the crawler
    # ever slips back to breadth-first.
    monkeypatch.setitem(sys.modules, "crawl4ai", c4)
    monkeypatch.setitem(sys.modules, "crawl4ai.deep_crawling", dc)


_HTML = {"content-type": "text/html"}


def _res(url, *, status=200, html="<html>ok</html>", markdown=None, headers=None, depth=1,
         req_headers=None):
    """`req_headers` fakes the browser's OWN captured request for the page. That is the
    production path — `capture_requests` defaults on — and it is the one whose header NAMES land
    in the canonical string. Left None the fingerprint falls back to a synthesized bare GET,
    which is a materially shorter string (see the sibling-rung test)."""
    captured = [] if req_headers is None else [
        {"event_type": "request", "resource_type": "document", "method": "GET", "url": url,
         "headers": dict(req_headers)}]
    return types.SimpleNamespace(
        url=url, success=status == 200, status_code=status, html=html,
        markdown=f"# {url}" if markdown is None else markdown,
        metadata={"depth": depth}, links={"internal": [{"href": "/x"}]}, error_message=None,
        response_headers=_HTML if headers is None else headers, network_requests=captured)


class _Stream:
    """Records how many results were actually pulled, so "the cap stopped the traversal" is an
    observation and not an inference."""

    def __init__(self, results):
        self.results, self.served, self.closed = results, 0, False

    def __aiter__(self):
        return self

    async def __anext__(self):
        if self.served >= len(self.results):
            raise StopAsyncIteration
        self.served += 1
        return self.results[self.served - 1]

    async def aclose(self):
        self.closed = True


class _Crawler:
    ready = True

    def __init__(self, results):
        self.stream = _Stream(results)

    async def arun(self, url, config):
        return self.stream


class _Ckpt:
    """Stands in for the host's per-unit unit_state IO (a no-op outside a run)."""

    def __init__(self, initial=None):
        self.store, self.writes = dict(initial or {}), []

    async def get(self, key):
        return self.store.get(key)

    async def set(self, key, value):
        self.store[key] = value
        self.writes.append(key)

    async def delete(self, key):
        self.store.pop(key, None)


class _Unit:
    """The Unit the host hands a Method — just a value now. Output no longer names a Unit; it goes
    to the Dataset (ADR 0028), which the test substitutes below."""

    def __init__(self, value):
        self.value = value


def _drive(monkeypatch, results, params=None, *, unit=None, emit_durable=True, ckpt=None):
    """Run one seed through the Method and return (pushed records, crawler, captured strategy).

    The Dataset is the substitute-and-read seam (ADR 0028): a collecting `Dataset()` stands in for
    the lake, and `.records` is what the body pushed — no host, no store."""
    from kontra.actor import _ckpt_io, _run_params
    from kontra.testing import collecting_dataset

    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)

    async def _resolves(_url):
        return True                     # no DNS in a unit test

    monkeypatch.setattr(C4, "_resolves", _resolves)
    crawler = _Crawler(results)
    inst = C4.ACTOR_CLASS()
    inst.params = params = {**{"depth": 2, "max_pages": 10}, **(params or {})}
    inst.crawler = crawler
    inst.emit_durable = emit_durable

    async def go():
        _run_params.set(params)         # the host sets both of these per unit
        if ckpt is not None:
            _ckpt_io.set(ckpt)
        u = _Unit(unit or {"url": "https://ex.com/"})
        ds = collecting_dataset()
        await inst._crawl_seed(u, ds)
        return ds.records

    return asyncio.run(go()), crawler, captured


def test_the_method_walks_depth_first_with_a_traversal_cap_wider_than_the_yield_cap(monkeypatch):
    """max_pages is a YIELD cap: the strategy must be allowed to keep walking, or dedupe would
    have nothing left to collapse."""
    _, _, captured = _drive(monkeypatch, [_res("https://ex.com/a")], {"depth": 3, "max_pages": 7})
    assert captured["strategy"]["max_depth"] == 3
    assert captured["strategy"]["max_pages"] == 7 * C4._TRAVERSAL_FANOUT


def test_the_method_stops_at_the_yield_cap_and_closes_the_stream(monkeypatch):
    """Reaching the cap must STOP the crawl: crawl4ai keeps traversing until its generator is
    closed, so a seed that has filled its cap would otherwise go on fetching for free."""
    results = [_res(f"https://ex.com/page/{c}") for c in "abcdef"]
    pages, crawler, _ = _drive(monkeypatch, results, {"max_pages": 2})
    assert len(pages) == 2
    assert crawler.stream.served == 2      # the remaining four were never fetched
    assert crawler.stream.closed is True


def test_an_invalid_seed_still_yields_one_bad_record(monkeypatch):
    pages, _, captured = _drive(monkeypatch, [], unit={"url": "not-a-url"})
    assert len(pages) == 1 and pages[0]["error"] == "invalid url"
    assert "strategy" not in captured      # bailed before touching crawl4ai


# ---------------------------------------------------------------------------------
# THE INVARIANT: near-duplication is the ONLY reason a crawled page is not emitted.
#
# Every case below was DROPPED by the old value gate. Each one is a page the data plane never
# saw, and a drop is not a wasted unit — it is a target that no downstream actor can ever reach,
# with nothing in the output to show it was considered. So each gets its own test, driven as the
# only page of its crawl so that dedupe cannot be mistaken for the cause.

@pytest.mark.parametrize("result,why", [
    (_res("https://ex.com/gone", status=404), "a non-200: the 404 body still reflects headers"),
    (_res("https://ex.com/moved", status=302), "a redirect: its Location can be poisoned"),
    (_res("https://ex.com/empty", html="", markdown=""), "an empty body is still a response"),
    (_res("https://ex.com/app.a1b2c3.js"), "a hashed bundle — 'usually inert' is not a filter"),
    (_res("https://ex.com/a/b.css"), "a stylesheet"),
    (_res("https://ex.com/_next/static/chunk"), "a build-output path"),
    (_res("https://ex.com/nocache", headers={}), "NO response headers at all"),
    (_res("https://ex.com/nostore", headers={"cache-control": "no-store"}),
     "no-store: an origin can say it and still sit behind a caching CDN"),
    (_res("https://ex.com/private", headers={"cache-control": "private, max-age=600"}),
     "private: same — the header states an intention, not the deployed behaviour"),
])
def test_a_page_the_old_value_gate_rejected_is_now_emitted(monkeypatch, result, why):
    pages, _, _ = _drive(monkeypatch, [result])
    assert [p["url"] for p in pages] == [result.url], why


def test_no_page_in_a_mixed_crawl_is_filtered_out(monkeypatch):
    """The same set as one crawl, to prove the gates are gone from the LOOP and not merely
    unreachable one page at a time. Under the old gate this yielded 2 of 7."""
    results = [_res("https://ex.com/keep"),
               _res("https://ex.com/app.a1b2c3.js"),
               _res("https://ex.com/gone", status=404),
               _res("https://ex.com/empty", html="", markdown=""),
               _res("https://ex.com/uncached", headers={}),
               _res("https://ex.com/nostore", headers={"cache-control": "no-store"}),
               _res("https://ex.com/edge", headers={"x-cache": "MISS"})]
    pages, _, _ = _drive(monkeypatch, results, {"max_pages": 20})
    assert [p["url"] for p in pages] == [r.url for r in results]


def test_an_emitted_record_carries_no_cache_fields(monkeypatch):
    """Belt and braces on the record shape, checked on the path that actually writes sub-units."""
    pages, _, _ = _drive(monkeypatch, [_res("https://ex.com/x", headers={
        "cache-control": "public, max-age=60", "vary": "Accept-Encoding", "age": "9"})])
    assert set(pages[0]) == _RECORD_KEYS
    assert not {"cache_control", "vary", "cached"} & set(pages[0])


def test_a_near_duplicate_request_shape_is_still_suppressed(monkeypatch):
    """The ONE surviving filter. /product/1001 and /product/1002 mask to the same skeleton, so
    the second is a page we have already written down."""
    pages, _, _ = _drive(monkeypatch, [_res("https://ex.com/product/1001"),
                                       _res("https://ex.com/product/1002"),
                                       _res("https://ex.com/product/98765"),
                                       _res("https://ex.com/about")])
    assert [p["url"] for p in pages] == ["https://ex.com/product/1001", "https://ex.com/about"]
    # …and the suppression is about the SEEN SET, never about the page: alone, /product/1002 is
    # emitted. Nothing in the crawler judges a target.
    alone, _, _ = _drive(monkeypatch, [_res("https://ex.com/product/1002")])
    assert [p["url"] for p in alone] == ["https://ex.com/product/1002"]


def test_the_radius_knob_moves_the_only_filter_there_is(monkeypatch):
    """The knob drives the ONE filter, end to end. Sibling routes sit at ~16 bits, so they
    survive the default 3 and only collapse at a radius that is documented as UNSAFE for
    production (it would also merge different methods and hosts — see
    test_no_safe_radius_collapses_sibling_leaves). Exercised here anyway because the point of
    the test is that the parameter is wired and moves the outcome."""
    siblings = [_res("https://ex.com/account/settings/email", req_headers={"accept": "*/*"}),
                _res("https://ex.com/account/settings/phone", req_headers={"accept": "*/*"})]
    assert len(_drive(monkeypatch, siblings)[0]) == 2
    assert len(_drive(monkeypatch, siblings, {"simhash_distance": 20})[0]) == 1


def test_a_generated_path_space_collapses_at_the_default_radius(monkeypatch):
    """The case the knob does NOT have to be widened for: object ids are masked before hashing,
    so /product/1001..1005 are one shape at distance 0 and collapse at the safe default."""
    gen = [_res(f"https://ex.com/product/{1000 + i}", req_headers={"accept": "*/*"})
           for i in range(1, 6)]
    assert len(_drive(monkeypatch, gen)[0]) == 1


# ---------------------------------------------------------------------------------
# Resumability: the seen set rides the frontier checkpoint, in the RESUMABLE tier only.

def test_the_seen_set_is_checkpointed_only_in_the_resumable_tier(monkeypatch):
    """The DEFAULT tier leaves unit_state unwired by design (it re-crawls the whole seed on a
    death, which is safe because re-emission is an idempotent overwrite); writing a checkpoint
    there would cost a state write per page for nothing."""
    results = [_res("https://ex.com/a"), _res("https://ex.com/b")]

    ckpt = _Ckpt()
    _drive(monkeypatch, results, {"resumable": False}, ckpt=ckpt)
    assert ckpt.writes == []

    ckpt = _Ckpt()
    _drive(monkeypatch, results, {"resumable": True}, ckpt=ckpt)
    assert ckpt.writes == ["seen", "seen"]
    assert len(ckpt.store["seen"]) == 2
    assert all(isinstance(x, str) for x in ckpt.store["seen"])   # JSON-safe, never a 64-bit int


def test_a_resumed_seed_does_not_re_yield_a_shape_it_already_emitted(monkeypatch):
    """The whole point of persisting the set — and the reason it is capped rather than dropped."""
    primed = C4.dump_seen([C4.request_simhash(
        {"method": "GET", "url": "https://ex.com/a", "headers": {}})])
    pages, _, _ = _drive(monkeypatch, [_res("https://ex.com/a"), _res("https://ex.com/b")],
                         {"resumable": True}, ckpt=_Ckpt({"seen": primed}))
    assert [p["url"] for p in pages] == ["https://ex.com/b"]
