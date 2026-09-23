"""Tests for the crawl4ai deep-crawl actor (the two-tier streaming design, ADR 0015).

Layers:
  - pure: valid_url + _page_out determinism (no deps).
  - tiers: the Method's tier wiring + streaming, driven directly against a FAKE crawl4ai and a
    fake streaming crawler (no crawl4ai package, no browser) — DEFAULT leaves unit_state
    unwired; RESUMABLE wires crawl4ai's native frontier state to self.unit_state.
  - isolation: concurrent Units each crawl in their OWN BrowserContext and release it — proved
    twice, against the fake seam (no browser) and against a REAL browser (cookies set while
    crawling seed A must never reach seed B, and the context count returns to baseline).
  - e2e: a REAL crawl4ai browser deep-crawls a real (local, in-process) site — the isolation
    pair and the engine-loop test both need only crawl4ai + Chromium:

    .venv/bin/python -m pytest examples/python/crawl4ai/test_crawl4ai.py
    .venv/bin/python -m pytest examples/python/crawl4ai/test_crawl4ai.py -m 'not e2e'
"""

from __future__ import annotations

import asyncio
import contextlib
import importlib.util
import json
import socket
import sys
import threading
import types
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

ACTOR_PY = Path(__file__).resolve().parent / "actor.py"


def _load():
    """Reset the registry singleton and (re)load the actor; returns (registry, module)."""
    from kontra import actor

    actor.actor_name = "crawl4ai"
    actor.version = ""
    actor.load_fn = None
    actor.close_fn = None
    actor.healthcheck_fn = None
    actor.methods = {}
    actor.input_type = actor.output_type = actor.params_type = None
    actor.actor_class = None

    spec = importlib.util.spec_from_file_location("c4_actor", ACTOR_PY)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    actor.actor_name = "crawl4ai"
    return actor, mod


# ---------------------------------------------------------------------------------
# Pure unit tests — no crawl4ai, no browser.

def test_valid_url():
    _, m = _load()
    assert m.valid_url("https://x.com/")
    assert m.valid_url("http://x.com/")
    assert not m.valid_url("not-a-url")
    assert not m.valid_url("")


def test_page_out_is_deterministic_and_drops_volatile_headers():
    """The sub-unit id is the record's sha, so _page_out must be content-deterministic: same
    page -> same bytes, and NO volatile headers (Date) that would remint the id every fetch."""
    _, m = _load()

    class R:
        url = "http://x/p"
        success = True
        status_code = 200
        markdown = "# hi"
        metadata = {"depth": 1}
        response_headers = {"content-type": "text/html", "date": "Mon, 01 Jan 2020 00:00:00 GMT"}
        links = {"internal": [{"href": "/a"}, {"href": "/b"}]}
        error_message = None

    a = m._page_out(R(), "http://x", discovery=False)
    b = m._page_out(R(), "http://x", discovery=False)
    assert a == b                                              # stable across calls
    assert a["response"] == {"status": 200, "content_type": "text/html"}  # only stable fields
    assert "date" not in json.dumps(a).lower()                # volatile header excluded
    assert a["depth"] == 1 and a["links"] == 2 and a["markdown"] == "# hi"
    # discovery mode drops the body
    d = m._page_out(R(), "http://x", discovery=True)
    assert d["markdown"] == ""


# ---------------------------------------------------------------------------------
# Tier wiring + streaming — a FAKE crawl4ai (captures the strategy kwargs) and a fake
# streaming crawler. Runs anywhere: no crawl4ai package, no browser, no actor host (only
# `simhash`, which actor.py imports at module level).

def _fake_crawl4ai(monkeypatch, captured):
    c4 = types.ModuleType("crawl4ai")
    c4.CacheMode = types.SimpleNamespace(BYPASS="bypass")

    class CrawlerRunConfig:
        session_id = None      # a real CrawlerRunConfig always has one (None = no session)

        def __init__(self, **kw):
            self.kw = kw

    c4.CrawlerRunConfig = CrawlerRunConfig
    dc = types.ModuleType("crawl4ai.deep_crawling")

    class DFSDeepCrawlStrategy:
        def __init__(self, **kw):
            captured["strategy"] = kw

    dc.DFSDeepCrawlStrategy = DFSDeepCrawlStrategy
    monkeypatch.setitem(sys.modules, "crawl4ai", c4)
    monkeypatch.setitem(sys.modules, "crawl4ai.deep_crawling", dc)


class _StreamResult:
    """One crawled page.

    Give these WORD paths (`/alpha`), never object-id paths (`/1`). Dedupe fingerprints the
    request SKELETON, and `skeleton_string` masks object-id segments on purpose — so `/1`, `/2`
    and `/3` are one shape, and a fake built from them makes a streaming test assert the dedupe
    instead. The body plays no part in the fingerprint, so varying it would not have helped.
    """

    def __init__(self, url, depth=0):
        self.url = url
        self.success = True
        self.status_code = 200
        self.markdown = f"# {url}"
        self.metadata = {"depth": depth}
        self.response_headers = {"content-type": "text/html", "date": "VOLATILE"}
        self.links = {"internal": [{"href": "/x"}]}
        self.error_message = None


class _StreamCrawler:
    """A fake crawler whose arun() streams a fixed list of results (ignores the strategy)."""

    ready = True

    def __init__(self, results):
        self._results = results

    async def start(self):
        pass

    async def close(self):
        pass

    async def arun(self, url, config):
        results = self._results

        async def _gen():
            for r in results:
                yield r

        return _gen()


async def _always_resolves(_url):
    """`_resolves` does a real getaddrinfo, and these seeds (`http://a`) are fakes that will never
    resolve — the Method would drop every one of them before reaching the crawler. Dropping an
    unresolvable host is deliberate (a dead host holds its BrowserContext for a whole navigation
    timeout) but it is not what these tests are about, and a unit test must not depend on DNS."""
    return True


def _drive(m, reg, unit, params, crawler, emit_durable=True):
    from kontra.actor import _run_params

    m._resolves = _always_resolves
    inst = reg.actor_class()
    inst.params = params
    inst.crawler = crawler
    inst.emit_durable = emit_durable  # the host sets this from the unit store; default durable here

    async def go():
        _run_params.set(params)  # the host sets this per run; set it directly for the unit test
        # ADR 0028: the caller passes the Batch first and the Dataset second, so a Method body is
        # driven with a stub of each and read back off `.records` — no host, no object store.
        from kontra.testing import collecting_dataset, stub_batch

        ds = collecting_dataset()
        await inst.crawl(stub_batch([unit]), ds)
        return ds.records

    return asyncio.run(go())


def test_default_tier_streams_pages_without_unit_state(monkeypatch):
    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)
    reg, m = _load()
    pages = _drive(
        m, reg, {"url": "http://a"},
        {"resumable": False, "depth": 1, "max_pages": 10, "discovery_mode": False},
        _StreamCrawler([_StreamResult("http://a/alpha"), _StreamResult("http://a/beta")]),
    )
    assert [p["url"] for p in pages] == ["http://a/alpha", "http://a/beta"]  # one push per page
    assert all(p["markdown"].startswith("#") for p in pages)
    # DEFAULT: no resume/frontier wiring
    assert captured["strategy"]["resume_state"] is None
    assert captured["strategy"]["on_state_change"] is None


def test_resumable_tier_wires_native_frontier_to_unit_state(monkeypatch):
    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)
    reg, m = _load()
    pages = _drive(
        m, reg, {"url": "http://a"},
        {"resumable": True, "depth": 2, "max_pages": 5},
        _StreamCrawler([_StreamResult("http://a/alpha", depth=1)]),
    )
    assert [p["url"] for p in pages] == ["http://a/alpha"]
    # RESUMABLE: crawl4ai's native frontier state is wired to self.unit_state's "frontier" key.
    # resume_state is unit_state.get("frontier") (None outside the host); on_state_change is a
    # callback that routes each native state snapshot to unit_state.set("frontier", ...).
    assert captured["strategy"]["resume_state"] is None
    oc = captured["strategy"]["on_state_change"]
    assert oc is not None and callable(oc)  # wired (contrast the DEFAULT tier: None)
    coro = oc({"visited": []})              # crawl4ai awaits this per URL; here it's the no-op set
    assert asyncio.iscoroutine(coro)
    coro.close()                            # don't leave it un-awaited


def test_resumable_falls_back_to_atomic_when_emit_not_durable(monkeypatch):
    """RESUMABLE (native frontier wired to unit_state) is a silent page-loss trap when yields are
    NOT durable (inline no-S3 mode): a resume would skip visited URLs whose pages were never
    persisted. So with emit_durable=False, even resumable=True must fall back to atomic — the
    native frontier state is NOT wired."""
    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)
    reg, m = _load()
    pages = _drive(
        m, reg, {"url": "http://a"},
        {"resumable": True, "depth": 2, "max_pages": 5},
        _StreamCrawler([_StreamResult("http://a/alpha")]),
        emit_durable=False,  # inline mode -> no durable per-page emission
    )
    assert [p["url"] for p in pages] == ["http://a/alpha"]
    # gated off: no frontier resume wiring despite resumable=True
    assert captured["strategy"]["resume_state"] is None
    assert captured["strategy"]["on_state_change"] is None


def test_invalid_url_yields_a_bad_record(monkeypatch):
    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)
    reg, m = _load()
    pages = _drive(m, reg, {"url": "not-a-url"}, {"resumable": False}, _StreamCrawler([]))
    assert len(pages) == 1
    assert pages[0]["error"] == "invalid url" and pages[0]["url"] == "not-a-url"
    assert "strategy" not in captured  # bailed before constructing a strategy / touching crawl4ai


# ---------------------------------------------------------------------------------
# Per-Unit BrowserContext isolation, against the FAKE seam — no crawl4ai, no browser, and
# deterministic: a barrier holds both Units in flight at once, so "they shared a context" is a
# fact, not a race. This is the contract the real-browser pair below re-proves end to end.

class _FakeContext:
    """Stands in for a Playwright BrowserContext."""

    def __init__(self, n):
        self.n = n
        self.pages = []
        self.closed = False
        self.set_up = False

    async def new_page(self):
        self.pages.append(f"page-{self.n}-{len(self.pages)}")
        return self.pages[-1]

    async def close(self):
        self.closed = True


class _FakeBrowserManager:
    """The crawl4ai seam the actor routes. Its NATIVE get_page is the bug in miniature: every
    identical config resolves to ONE cached context, so concurrent Units would share it."""

    def __init__(self):
        self.config = types.SimpleNamespace(use_persistent_context=False)
        self.created: list[_FakeContext] = []
        self.native_context = None      # the shared one crawl4ai would have handed out

    async def create_browser_context(self, cfg):
        self.created.append(_FakeContext(len(self.created)))
        return self.created[-1]

    async def setup_context(self, ctx, cfg, is_default=False):
        ctx.set_up = True

    async def get_page(self, crawlerRunConfig):
        if self.native_context is None:
            self.native_context = await self.create_browser_context(crawlerRunConfig)
        return await self.native_context.new_page(), self.native_context


class _PageStreamCrawler(_StreamCrawler):
    """A fake crawler that allocates a page through the browser manager before each result —
    the way crawl4ai's strategy does — and records (seed, context) so the test can see which
    context each seed crawled in."""

    def __init__(self, results, manager, gate=None):
        super().__init__(results)
        self.crawler_strategy = types.SimpleNamespace(browser_manager=manager)
        self._gate = gate
        self.seen: list[tuple[str, _FakeContext]] = []

    async def arun(self, url, config):
        results, gate, seen = self._results, self._gate, self.seen
        manager = self.crawler_strategy.browser_manager

        async def _gen():
            for n, r in enumerate(results):
                _, ctx = await manager.get_page(config)     # the routed call
                seen.append((url, ctx))
                if gate is not None and n == 0:
                    await gate.wait()      # both seeds are now mid-crawl, together
                yield r

        return _gen()


def _drive_concurrent(reg, crawler, seeds, params, m=None):
    """Run several seeds through ONE actor instance CONCURRENTLY — one asyncio task per seed,
    like the host's sliding window (internals/window.py does ensure_future per unit), which
    is what makes the actor's per-Unit context slot task-local."""
    from kontra.actor import _run_params

    if m is not None:
        m._resolves = _always_resolves   # see _always_resolves: no DNS in a unit test
    inst = reg.actor_class()
    inst.params = params
    inst.crawler = crawler
    inst.emit_durable = True

    async def go():
        _run_params.set(params)
        inst._route_pages_to_unit_contexts()          # @actor.load does this after start()

        async def one(seed):
            from kontra.testing import collecting_dataset, stub_batch

            ds = collecting_dataset()  # one Dataset per concurrent seed, as a caller would
            await inst.crawl(stub_batch([{"url": seed}]), ds)
            return ds.records

        return await asyncio.gather(*(asyncio.create_task(one(s)) for s in seeds))

    return asyncio.run(go())


def test_concurrent_units_do_not_share_a_context_and_release_it(monkeypatch):
    """The bug: crawl4ai caches contexts by config signature, so every concurrent Unit gets the
    SAME BrowserContext (one cookie jar for unrelated seeds). Each Unit must get its OWN, use it
    for all of that seed's pages, and CLOSE it when the seed ends."""
    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)
    reg, m = _load()
    manager = _FakeBrowserManager()
    gate = asyncio.Barrier(2)             # both seeds in flight at once — no lucky serialization
    crawler = _PageStreamCrawler(
        [_StreamResult("/alpha"), _StreamResult("/beta"), _StreamResult("/gamma")], manager, gate)

    pages = _drive_concurrent(reg, crawler, ["http://a", "http://b"], {"resumable": False}, m)
    assert [len(p) for p in pages] == [3, 3]          # both seeds streamed all their pages

    by_seed: dict[str, set] = {}
    for seed, ctx in crawler.seen:
        by_seed.setdefault(seed, set()).add(id(ctx))
    # ISOLATION: one context per seed, and the two seeds' contexts are DIFFERENT objects.
    assert [len(v) for v in by_seed.values()] == [1, 1], by_seed
    a, b = (next(iter(v)) for v in by_seed.values())
    assert a != b, "concurrent Units shared one BrowserContext"
    assert manager.native_context is None             # never fell back to crawl4ai's shared cache
    assert all(c.set_up for c in manager.created)     # built like crawl4ai builds them
    # NO LEAK: exactly one context per seed, every one closed when its Unit ended.
    assert len(manager.created) == 2
    assert all(c.closed for c in manager.created)
    assert [len(c.pages) for c in manager.created] == [3, 3]   # a seed's pages ride ITS context


def test_page_calls_outside_a_unit_keep_crawl4ais_own_resolution(monkeypatch):
    """The routing is scoped to an in-flight Unit: crawl4ai's own internal fetches (and any
    session_id call, whose kill_session semantics are crawl4ai's) fall through untouched."""
    captured: dict = {}
    _fake_crawl4ai(monkeypatch, captured)
    reg, m = _load()
    manager = _FakeBrowserManager()
    crawler = _PageStreamCrawler([], manager)
    inst = reg.actor_class()
    inst.crawler = crawler
    inst._route_pages_to_unit_contexts()

    from crawl4ai import CrawlerRunConfig

    async def go():
        return await manager.get_page(CrawlerRunConfig())

    _, ctx = asyncio.run(go())
    assert ctx is manager.native_context is not None   # crawl4ai's native path, not a Unit's


# ---------------------------------------------------------------------------------
# REAL — a real crawl4ai browser deep-crawling a real (local, in-process) site through a real host
# loop (fake state store, no Temporal). With no object store the streaming host collects the
# yielded pages inline, so results is the list of page records.
#
# Driven through the real engine. This helper once imported a host module that had been deleted,
# behind an `importorskip` guard that meant the ImportError was never reached — so the test
# silently skipped instead of failing. No guard now: it runs or it breaks.

def _run_on_host(reg, seeds, params):
    from internals.engine import build_session_factory

    class FakeKV:
        """In-memory ActorStateKV: one dict for the one Redis hash."""

        def __init__(self):
            self.d = {}

        async def get(self, field, default=None):
            return self.d.get(field, default)

        async def set(self, field, value):
            self.d[field] = value

        async def delete(self, field):
            return self.d.pop(field, None) is not None

        async def touch(self):
            pass

        async def drop(self):
            self.d.clear()

    session = build_session_factory(reg, store=None)("test-crawl4ai", kv=FakeKV())

    async def go():
        out = await session.run_batch({"units": seeds, "params": params})
        await session.close()
        return out

    return asyncio.run(go())


def _page(title: str, body: str, links: str) -> bytes:
    return (f"<html><head><title>{title}</title></head><body><article><h1>{title}</h1>"
            f"<p>{body}</p><nav>{links}</nav></article></body></html>").encode()


_SITE = {
    "/": _page("Kontra Test Site",
               "Kontra is a durable actor runtime built on Temporal. This root page "
               "links out to the two sections of the documentation so the crawler can walk them.",
               "<a href='/a'>Section A</a> <a href='/b'>Section B</a> <a href='/missing'>dead link</a>"),
    "/a": _page("Section A — Actors",
                "An actor loads a shared resource once, then processes each unit of work in "
                "arun. This section explains the lifecycle in more detail on the home page.",
                "<a href='/'>back home</a>"),
    "/b": _page("Section B — Dispatch",
                "The orchestrator dispatches a batch of units to an actor over Nexus, and the "
                "handler owns the workflow and the actor serves RunBatch on its own queue.",
                ""),
}


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        body = _SITE.get(self.path)
        self.send_response(200 if body is not None else 404)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(body or b"<html><body>not found</body></html>")

    def log_message(self, *a):
        pass


@contextlib.contextmanager
def _local_site():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    srv = ThreadingHTTPServer(("127.0.0.1", port), _Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        yield f"http://127.0.0.1:{port}"
    finally:
        srv.shutdown()


@pytest.mark.e2e
def test_real_crawl4ai_deep_crawls_a_local_site():
    pytest.importorskip("crawl4ai")     # + Chromium; both live in the actor image, not the SDK
    reg, m = _load()
    with _local_site() as base:
        result = _run_on_host(reg, [{"url": base + "/", "max_pages": 12}],
                              {"depth": 1, "concurrent_crawls": 2, "headless": True})

    assert result["done"] is True
    assert result["failures"] == []                       # real crawl, no unit sunk
    by_url = {r["url"].rstrip("/"): r for r in result["results"]}
    assert base in by_url and base + "/a" in by_url and base + "/b" in by_url  # BFS reached all 3

    root = by_url[base]
    assert root["status"] == 200                          # real HTTP status
    assert root["request"]["method"] == "GET"
    assert "Kontra Test Site" in root["markdown"]         # real HTML -> markdown by crawl4ai
    assert root["links"] >= 2                              # real link extraction
    assert all(by_url[u]["error"] is None for u in (base, base + "/a", base + "/b"))
    dead = by_url.get(base + "/missing")
    assert dead is not None and dead["error"] is not None and dead["markdown"] == ""


# ---------------------------------------------------------------------------------
# REAL BROWSER, per-Unit isolation — the same contract the fake seam pins above, proved end to
# end against a real Chromium. No host: the seeds are driven straight onto the actor, one
# asyncio task each, exactly like the host's window (internals/window.py).
#
# The site puts BOTH seeds on ONE origin (so a shared cookie jar would be observable), gives
# each seed its own cookie, and ECHOES back the cookies the browser sent. A barrier holds the
# two seed roots until both have been served, so both cookies exist before either seed fetches a
# child page: sharing one BrowserContext MUST show seed a's cookie on seed b's pages.

def _cookie_site_page(seed: str, name: str, cookies: str, links: str) -> bytes:
    return (f"<html><head><title>{seed} {name}</title></head><body><article>"
            f"<h1>Seed {seed} page {name}</h1>"
            f"<p>COOKIES-SEEN [{cookies or 'none'}] on this page of the kontra isolation site, "
            f"which exists only to echo the cookie jar the browser used for this request.</p>"
            f"<nav>{links}</nav></article></body></html>").encode()


class _CookieHandler(BaseHTTPRequestHandler):
    """/{seed}/ links to /{seed}/p1 + /{seed}/p2; every response sets kontra_{seed}=1 for the
    WHOLE origin and echoes the Cookie header it received."""

    barrier = None          # set by the test: releases both seed roots together

    def do_GET(self):  # noqa: N802
        parts = [p for p in self.path.split("/") if p]
        seed = parts[0] if parts else "root"
        name = parts[1] if len(parts) > 1 else "root"
        if name == "root" and _CookieHandler.barrier is not None:
            try:
                _CookieHandler.barrier.wait(timeout=30)     # both seeds' cookies, then children
            except threading.BrokenBarrierError:
                pass
        links = "" if name != "root" else (
            f"<a href='/{seed}/p1'>page one</a> <a href='/{seed}/p2'>page two</a>")
        body = _cookie_site_page(seed, name, self.headers.get("Cookie", ""), links)
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Set-Cookie", f"kontra_{seed}=1; Path=/")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


@contextlib.contextmanager
def _cookie_site(barrier=None):
    _CookieHandler.barrier = barrier
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    srv = ThreadingHTTPServer(("127.0.0.1", port), _CookieHandler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        yield f"http://127.0.0.1:{port}"
    finally:
        srv.shutdown()
        _CookieHandler.barrier = None


def _run_waves(reg, waves, params):
    """Load the actor for real (one browser), run each wave of seeds CONCURRENTLY (one task per
    seed), and report what the browser actually did: the contexts crawl4ai used (via its own
    on_page_context_created hook) and the live context count after every wave."""
    from kontra.actor import _run_params

    inst = reg.actor_class()
    inst.params = params
    inst.emit_durable = True
    seen = []          # every BrowserContext crawl4ai served a page from

    async def go():
        _run_params.set(params)
        await reg.load_fn(inst)
        manager = inst.crawler.crawler_strategy.browser_manager
        inst.crawler.crawler_strategy.set_hook(
            "on_page_context_created",
            lambda page, context=None, **kw: (seen.append(context), page)[1])
        out = {"baseline": len(manager.browser.contexts), "live": [], "waves": []}
        try:
            for wave in waves:
                async def one(seed):
                    from kontra.testing import collecting_dataset, stub_batch

                    ds = collecting_dataset()
                    await inst.crawl(
                        stub_batch([{"url": seed, "max_pages": params["max_pages"]}]), ds)
                    return ds.records

                out["waves"].append(await asyncio.gather(
                    *(asyncio.create_task(one(s)) for s in wave)))
                out["live"].append(len(manager.browser.contexts))
            out["cached"] = len(getattr(manager, "contexts_by_config", {}))
            out["sessions"] = len(getattr(manager, "sessions", {}))
            out["contexts"] = [id(c) for c in seen]
            return out
        finally:
            await reg.close_fn(inst)

    return asyncio.run(go())


# Each seed is a 3-page site (root + 2 children at depth 1). max_pages is crawl4ai's own cap
# and it stops one short of it, so 4 buys the whole 3-page seed.
_SEED_PAGES = 3
_REAL_PARAMS = {"depth": 1, "max_pages": _SEED_PAGES + 1, "headless": True,
                "discovery_mode": False, "resumable": False}


@pytest.mark.e2e
def test_real_concurrent_units_do_not_share_a_browser_context():
    """ISOLATION: two seeds crawled at once get two DIFFERENT BrowserContexts, and the cookie
    seed a sets while crawling is never sent on seed b's requests."""
    pytest.importorskip("crawl4ai")     # + Chromium; both live in the actor image, not the SDK
    reg, m = _load()
    with _cookie_site(threading.Barrier(2)) as base:
        out = _run_waves(reg, [[base + "/a/", base + "/b/"]], _REAL_PARAMS)

    a_pages, b_pages = out["waves"][0]
    assert len(a_pages) == len(b_pages) == _SEED_PAGES       # real crawl: root + 2 children
    assert all(p["error"] is None and p["status"] == 200 for p in a_pages + b_pages)

    # STRUCTURAL: crawl4ai served every page from exactly one context per in-flight Unit.
    assert len(set(out["contexts"])) == 2, "concurrent Units shared a BrowserContext"

    # SEMANTIC: no cross-seed cookie. Not vacuous — each seed DOES see its own cookie, so the
    # echo works and the jars are simply separate.
    a_md, b_md = " ".join(p["markdown"] for p in a_pages), " ".join(p["markdown"] for p in b_pages)
    assert "kontra_a=1" in a_md and "kontra_b=1" in b_md     # each seed's own jar carries its own
    assert "kontra_b" not in a_md, "seed b's cookie leaked into seed a's crawl"
    assert "kontra_a" not in b_md, "seed a's cookie leaked into seed b's crawl"


@pytest.mark.e2e
def test_real_batch_does_not_leak_browser_contexts():
    """NO LEAK: a batch of 6 seeds over 3 windows of 2 returns to the baseline context count
    after EVERY window — the live contexts are window-wide, never batch-wide — and the browser
    is still usable when it owns no contexts at all."""
    pytest.importorskip("crawl4ai")
    reg, m = _load()
    with _cookie_site() as base:
        seeds = [base + f"/s{i}/" for i in range(6)]
        out = _run_waves(reg, [seeds[0:2], seeds[2:4], seeds[4:6]], _REAL_PARAMS)

    assert [len(w) for w in out["waves"]] == [2, 2, 2]
    assert all(len(pages) == _SEED_PAGES for wave in out["waves"] for pages in wave)  # all 18
    # the LAST wave still crawled fine -> the browser survived every context being reclaimed
    assert all(p["error"] is None for p in out["waves"][-1][0])
    # every wave released its contexts: back to baseline after each, never 2 -> 4 -> 6
    assert out["live"] == [out["baseline"]] * 3, out["live"]
    assert len(set(out["contexts"])) == 6            # one context per seed, not one for the batch
    assert out["cached"] == 0 and out["sessions"] == 0   # crawl4ai's own caches never grew
