"""The traversal ported from crawl4ai, driven against a REAL site on localhost.

Every bound here is one a live crawl hits, and every one of them fails silently if it is wrong:
a depth that does not advance reads as "this host has one page", a simhash radius that does not
collapse a generated path space spends the whole page budget on `/product/1..N`, and a wall-clock
budget that never fires lets one origin hold a Machine for the length of the run.

    python3 -m pytest test_traversal.py -q
"""
import asyncio
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
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

# 30 generated product pages (ONE shape), 3 distinct routes, and one off-host link.
_INDEX = ("".join(f'<a href="/product/{i}">p{i}</a>' for i in range(1, 31))
          + '<a href="/settings/email">e</a><a href="/settings/phone">p</a>'
          + '<a href="/about">a</a><a href="https://elsewhere.invalid/x">off</a>')


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"<html><body>{_INDEX}</body></html>".encode()
        self.send_response(200)
        self.send_header("content-type", "text/html")
        # THE HEADERS THIS ACTOR EXISTS FOR. crawl4ai drops these by ADR 0015 determinism; if the
        # port lost them too, the whole reason for doing it in webcrawl is gone.
        self.send_header("access-control-allow-headers", "x-tenant-id, x-api-key")
        self.send_header("vary", "origin, x-region")
        self.send_header("set-cookie", "csrf=zzz; Path=/")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):  # keep pytest output readable
        pass


@pytest.fixture(scope="module")
def site():
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}/"
    srv.shutdown()


def _run(seed, **params):
    """Drive `crawl` with params bound the way a hosted run binds them.

    `param.get()` returns a lazy ParamRef outside a run, so the values have to be installed in
    the `_run_params` contextvar — the same mechanism the host uses — or every knob under test
    reads as its default and the test passes for the wrong reason.
    """
    async def go():
        inst = wc.WebCrawl()
        await inst.open_browser()
        try:
            ds = collecting_dataset()
            await inst.crawl(stub_batch([{"seed": seed, "program": "t"}], takes=wc.Seed), ds)
            return ds.records
        finally:
            await inst.close_browser()

    token = _lib._run_params.set({"nav_timeout_ms": 8000, "settle_ms": 100,
                                  "max_events": 600, **params})
    try:
        return asyncio.run(go())
    finally:
        _lib._run_params.reset(token)


def _navigated(events, site):
    """Document navigations only — the pages the walk actually visited."""
    return [e["url"] for e in events
            if e["kind"] == "response" and e.get("resource_type") == "document"]


@pytest.mark.e2e
def test_depth_zero_visits_only_the_seed(site):
    pages = _navigated(_run(site, depth=0, max_pages=20), site)
    assert len(pages) == 1, f"depth=0 followed links: {pages}"


@pytest.mark.e2e
def test_depth_one_follows_links_and_collapses_the_generated_space(site):
    events = _run(site, depth=1, max_pages=12, simhash_distance=3)
    pages = _navigated(events, site)
    assert len(pages) > 1, "depth=1 never left the seed"

    products = [u for u in pages if "/product/" in u]
    routes = {u.rsplit("/", 1)[0] for u in pages if "/product/" not in u}
    # 30 product URLs are ONE shape once object ids are masked, so the walk must not spend its
    # budget there — that is the entire point of fingerprinting in request space.
    assert len(products) <= 2, f"the generated path space was not collapsed: {products}"
    assert len(routes) >= 2, f"distinct routes were suppressed along with the ids: {pages}"


@pytest.mark.e2e
def test_the_walk_never_leaves_the_host(site):
    events = _run(site, depth=2, max_pages=15)
    off = [e["url"] for e in events if "elsewhere.invalid" in e["url"]]
    assert off == [], f"crawled off-host: {off} — a seed is one row of somebody's scope"


@pytest.mark.e2e
def test_response_headers_survive_the_port(site):
    events = _run(site, depth=0, max_pages=1)
    docs = [e for e in events if e["kind"] == "response" and e.get("resource_type") == "document"]
    assert docs, "no document response captured"
    headers = json.loads(docs[0]["headers"])
    for name in ("access-control-allow-headers", "vary", "set-cookie"):
        assert name in headers, (
            f"{name} is missing — this is the half a wordlist cannot know, and the reason the "
            f"traversal was ported into webcrawl instead of using crawl4ai")


@pytest.mark.e2e
def test_a_bound_that_bites_is_announced(site):
    events = _run(site, depth=3, max_pages=3)
    bounds = [e["error"] for e in events if e.get("error")]
    assert any("max_pages" in b for b in bounds), (
        f"the page cap stopped the walk and said nothing: {bounds} — a crawl that stopped early "
        f"and reads as complete is how a surface describes a fraction of a program")


def test_a_path_shape_gets_a_page_quota_not_a_single_sample():
    """`pages_per_path` is how many concrete pages ONE route shape may spend.

    Before it, `near_duplicate` skipped a matched shape outright — so `/product/1001` and
    `/product/1002`, which collapse to one shape once object ids are masked, meant the route was
    sampled exactly once and judged on whichever URL the walk reached first.

    The quota is counted against the REPRESENTATIVE the shape matched, which is why the frontier
    needs `nearest` and not the yes/no `near_duplicate`.
    """
    from dedupe import nearest, request_fingerprint

    distance, per_path = 3, 2
    seed_fp = request_fingerprint("GET", "https://x.test/")
    seen = [seed_fp]
    shape_pages = {seed_fp: 1}
    admitted = []

    # Four URLs of the SAME generated shape, plus one genuinely different route.
    links = [f"https://x.test/product/{1000 + i}" for i in range(4)]
    links.append("https://x.test/checkout/v1/pay")

    for link in links:
        fp = request_fingerprint("GET", link)
        rep = nearest(fp, seen, distance)
        if rep is not None:
            if shape_pages.get(rep, 0) >= per_path:
                continue
            shape_pages[rep] = shape_pages.get(rep, 0) + 1
        else:
            seen.append(fp)
            shape_pages[fp] = 1
        admitted.append(link)

    products = [u for u in admitted if "/product/" in u]
    assert len(products) <= per_path, (
        f"the shape spent {len(products)} pages against a quota of {per_path}: {products}"
    )
    assert len(products) >= 1, "the quota suppressed the shape entirely"
    assert "https://x.test/checkout/v1/pay" in admitted, (
        "a genuinely different route was charged against another shape's quota"
    )


def test_near_duplicate_stays_exactly_nearest_is_not_none():
    """Two callers must not disagree about what a duplicate is."""
    from dedupe import near_duplicate, nearest, request_fingerprint

    seen = [request_fingerprint("GET", "https://x.test/a")]
    for url in ("https://x.test/a", "https://x.test/totally/different/route"):
        fp = request_fingerprint("GET", url)
        assert near_duplicate(fp, seen, 3) == (nearest(fp, seen, 3) is not None)
