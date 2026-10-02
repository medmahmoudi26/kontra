"""webcrawl — one browser per session, one browser context per unit.

The shape this actor exists to demonstrate:

    @actor.load   opens ONE Chromium for the whole session
    @actor.method loops the Batch itself, opening ONE BrowserContext per unit and running
                  `parallel_seeds` of them AT THE SAME TIME
    @actor.close  closes the browser

A context — not a browser — per unit is the point. A browser costs ~120 MB and several
hundred ms to launch; a context costs a few MB and is instant, and it is a real isolation
boundary: its own cookie jar, its own storage, its own cache. So `parallel_seeds` seeds
crawl concurrently in one process without leaking session state between the programs we are
pointed at, which for scope-separated bug-bounty targets is a correctness property and not
just hygiene.

It also means a session RELOAD (see @actor.healthcheck) rebuilds one browser rather than N,
and the units that were in flight replay into fresh contexts.

WHAT IT PUSHES
--------------
One object per HTTP request and one object per HTTP response — `kind` discriminates, and `tx`
joins them back together. They are the SAME output type on purpose: a Method pushes records of
one declared type (ADR 0015), and a request and its response are two observations of one
transaction, not two datasets. Join on `tx` to pair them; filter on `kind` to get either side:

    SELECT * FROM webcrawl WHERE kind = 'response' AND status >= 500
    SELECT req.url, res.status FROM webcrawl req
      JOIN webcrawl res USING (tx) WHERE req.kind='request' AND res.kind='response'

Records are pushed as they happen: `await dataset.push(x)` makes each one durable AS IT IS
PUSHED, so events land while the page is still loading rather than in one lump when the unit
finishes. That is why the capture below feeds a queue the loop drains, instead of the obvious
`collect into a list, push at the end` — buffering the whole page would give up the push-time
durability. Which seed a record came from is IN the record (`seed`, `program`), not in the Unit
it was pushed under (ADR 0028 §1) — seeds run concurrently, so a page cannot be attributed by
the framework's position anyway.

Drive it from a caller's workflow — the paging and the query are yours:

    async for batch in catalog.dataset("scope_paid").batches(
        100, order_by="seed", where="kind='domain'"
    ):
        found, dropped = await crawl.crawl(batch, out)
"""

import asyncio
import hashlib
import json
import os
import time
from dataclasses import dataclass, field

from kontra import actor, param

from urllib.parse import urlparse

from dedupe import nearest, request_fingerprint

# Where deploy.sh puts Chromium. See @actor.load.
_SYSTEM_BROWSERS = "/opt/ms-playwright"

# Chromium in a container: no /dev/shm worth the name, no sandbox available as root, and
# nothing on screen to composite. Same list the crawl4ai actor arrived at.
_CHROME_LEAN_ARGS = (
    "--no-sandbox",
    "--disable-dev-shm-usage",
    "--disable-gpu",
    "--disable-background-networking",
    "--disable-extensions",
    "--no-first-run",
    "--no-default-browser-check",
)

# Blocked when `block_media` is on. These are the resource types that dominate bytes and
# contribute least to an HTTP inventory.
# The frontier and the seen-set are both CAPPED. A page that links a thousand distinct routes
# would otherwise make one seed's in-memory state unbounded, and `near_duplicate` is a linear
# scan — an uncapped `seen` turns the walk quadratic on exactly the sites that need it most.
# Evicting only costs re-visiting a long-forgotten shape, which overwrites its own key.
_FRONTIER_CAP = 2000
_SEEN_CAP = 2000


def _host_of(url: str) -> str:
    """Lowercased hostname, port stripped. Empty for anything unparseable, which never matches
    a real host and therefore excludes the link rather than admitting it."""
    try:
        return (urlparse(url).hostname or "").lower()
    except ValueError:
        return ""


_HEAVY = {"image", "media", "font"}

# Header values are unbounded (a Set-Cookie or CSP can run to kilobytes) and this actor writes
# every one of them to a column store. Truncation is visible in the data rather than implicit.
_HEADER_CAP = 2048


@dataclass
class Seed:
    """One unit: a URL to load. `program` rides along so the output is attributable back to the
    bug-bounty program the seed came from without a second join against scope."""

    seed: str
    program: str = ""


@dataclass
class HttpEvent:
    """One HTTP request OR one HTTP response. `kind` says which; `tx` joins the pair."""

    seed: str
    program: str
    tx: str
    kind: str            # "request" | "response"
    url: str
    method: str
    resource_type: str   # document, script, xhr, fetch, stylesheet, ...
    status: int          # -1 on a request (and on a response we never received)
    mime_type: str
    headers: str         # JSON object; flattened because header SETS are not a fixed schema
    body_sha256: str     # "" unless capture_bodies
    body_bytes: int      # -1 when unknown
    from_cache: bool
    ts_ms: int           # epoch ms at capture
    error: str           # non-empty only on a failed request or a failed navigation


@dataclass
class CrawlParams:
    # The knob that makes contexts concurrent — this actor's own, not the framework's: the
    # author owns the loop (ADR 0023 §18). 6 is a starting point for a 2 vCPU / 4 GB machine;
    # each context is cheap but the page it drives is not.
    parallel_seeds: int = 6
    nav_timeout_ms: int = 20000
    # How long to keep capturing after the navigation settles. A single-page app issues most
    # of its XHRs after `load`, so stopping at `load` misses the interesting half of the
    # inventory — which is the half a bug-bounty crawl is actually for.
    settle_ms: int = 2000
    wait_until: str = "load"        # commit | domcontentloaded | load | networkidle
    max_events: int = 400           # per-seed cap; the stream stops, the unit still commits
    capture_bodies: bool = False    # read response bodies (slower, much larger output)
    block_media: bool = True        # drop image/media/font — bytes without inventory value
    ignore_https_errors: bool = True  # scope hosts routinely have broken chains
    user_agent: str = ""
    viewport_width: int = 1280
    viewport_height: int = 800

    # ---- traversal, ported from crawl4ai (see _crawl_seed) --------------------------------
    depth: int = 0                  # links followed from the seed; 0 = the seed alone
    max_pages: int = 1              # pages visited per seed
    max_seconds_per_host: int = 0   # wall-clock budget per seed; 0 = unbounded
    # Hamming radius over the 64-bit request fingerprint. MEASURED, and near-maximal already:
    # a sibling route is ~16 bits apart, a different METHOD ~11 and a different HOST ~15, so any
    # radius wide enough to merge two sibling pages also merges GET with POST. Do not raise it.
    simhash_distance: int = 3
    # How many concrete pages ONE path shape may contribute. `simhash_distance` decides what
    # counts as the same path; this decides how deeply to sample it. 1 is the old behaviour — a
    # near-duplicate was skipped outright, so every route was judged on whichever URL the walk
    # happened to reach first. 2 costs one extra page per route and catches the common case of a
    # template that renders differently for two ids.
    pages_per_path: int = 2
    # Headers added to EVERY request the browser makes, including subresources. Separate from
    # `user_agent` because a marker is not always a UA.
    extra_headers: dict = field(default_factory=dict)


def _field(value, name: str, default: str = "") -> str:
    """Read one field off a Unit's value, whatever shape the host handed us.

    `@actor.method(takes=Seed, ...)` makes the runtime COERCE each unit into `Seed` before the
    Method sees it, so `unit.value` is a dataclass in production and `.get()` raises
    `AttributeError: 'Seed' object has no attribute 'get'` on the very first seed.

    That is not a new bug — this method has read `.get()` since 0.1.0 — and it is the reason
    `http_events_<program>` has never existed and `exchanges_<program>` had to be loaded by hand
    outside any Run. It survived because the tests build batches with `stub_batch([{...}])`,
    which yields plain dicts: the suite supplied the one input shape production never sends.
    `test_webcrawl.py` now passes `takes=Seed` so the stub coerces the way the host does.

    Both shapes are accepted rather than just the dataclass, because a Method can also be
    dispatched with `takes` unset, and then dicts are exactly what arrives.
    """
    if isinstance(value, dict):
        return value.get(name, default)
    return getattr(value, name, default)


def _headers_json(h: dict) -> str:
    out = {}
    for k, v in (h or {}).items():
        s = str(v)
        out[str(k).lower()] = s if len(s) <= _HEADER_CAP else s[:_HEADER_CAP] + "…[truncated]"
    return json.dumps(out, separators=(",", ":"), sort_keys=True)


def _int_header(h: dict, name: str) -> int:
    try:
        return int((h or {}).get(name, ""))
    except (TypeError, ValueError):
        return -1


@actor.defn
class WebCrawl:
    input = Seed
    output = HttpEvent
    params = CrawlParams

    @actor.load
    async def open_browser(self):
        """ONE Chromium for the whole session. Re-runs on a reload, handing the session a
        fresh browser; the units that were in flight replay into fresh contexts."""
        # deploy.sh installs Chromium to a SYSTEM path, not $HOME/.cache, so that the build
        # user and the run user cannot disagree about where the browser is. Point the driver
        # at it before it starts, unless the operator has said otherwise.
        if not os.environ.get("PLAYWRIGHT_BROWSERS_PATH") and os.path.isdir(_SYSTEM_BROWSERS):
            os.environ["PLAYWRIGHT_BROWSERS_PATH"] = _SYSTEM_BROWSERS

        from playwright.async_api import async_playwright

        self._pw = await async_playwright().start()
        self.browser = await self._pw.chromium.launch(
            headless=True, args=list(_CHROME_LEAN_ARGS)
        )
        # Serial per-session counter for tx ids, PREFIXED WITH A PER-LOAD NONCE.
        #
        # The counter alone was not unique and the join that depends on it fanned out. `@actor.load`
        # re-runs — on a session reload, and once per Worker — so `_seq` restarts at 0 each time
        # and the same `00000056` is minted again. MEASURED on surface-1789866474: 799 request
        # events carried 282 distinct `tx`, three seeds shared `00000056`, and `_exchanges` —
        # which joins request to response on `tx` alone — turned 799 requests into 2,349
        # "exchanges". `injection_points` took the same damage through the same join: inflated
        # `seen_count`, and a `status_code` picked by `max()` from whichever unrelated response
        # happened to collide.
        #
        # The original note said "unique WITHIN a unit is all the join needs, and a counter avoids
        # paying for a uuid per HTTP event". The first half was the wrong requirement — the join
        # is over a whole DATASET, not a unit — and the second is preserved here: this pays for
        # ONE random value per load, not one per event, and the counter still does the per-event
        # work.
        self._seq = 0
        self._nonce = os.urandom(4).hex()

    @actor.healthcheck
    async def browser_alive(self):
        """LIVENESS ONLY. A crashed Chromium takes every in-flight context with it, so this is
        the difference between reloading the session and isolating one bad URL.

        IT RETURNS NOTHING NOW, and that is the point of the split. It used to return
        `{"contexts": N}` — and because the runtime read a liveness probe's return value as the
        progress feed, that became the entire signal an operator got from a crawl: how many
        browser tabs were open, never which program or which page.
        """
        browser = getattr(self, "browser", None)
        if browser is None or not browser.is_connected():
            raise RuntimeError("browser gone")

    @actor.progress
    async def where_i_am(self):
        """WHAT THIS CRAWLER IS ON, on the same beat, and unable to end the Session.

        A raise here is swallowed by the engine, which is the whole difference from the hook
        above: reading `self.*` to report where you are must never kill a Session that is
        working fine. `_at` is set in `_visit` BEFORE navigating, because navigation is the slow
        part and a stall is exactly when a reader needs the URL named.
        """
        browser = getattr(self, "browser", None)
        return {"program": getattr(self, "_program", ""),
                "at": getattr(self, "_at", ""),
                "contexts": len(browser.contexts) if browser is not None else 0}

    @actor.close
    async def close_browser(self):
        browser = getattr(self, "browser", None)
        if browser is not None:
            await browser.close()
        pw = getattr(self, "_pw", None)
        if pw is not None:
            await pw.stop()

    @actor.method(takes=Seed, emits=HttpEvent)
    async def crawl(self, batch, dataset):
        """The author's loop. Seeds run CONCURRENTLY — one BrowserContext each, `parallel_seeds`
        of them at a time. `push` names no Unit, so a page captured on one seed's task carries its
        own `seed`/`program` provenance in the record rather than relying on the framework's
        position, which under concurrency there is none of (ADR 0028 §1)."""
        limit = asyncio.Semaphore(max(1, int(param.get("parallel_seeds", 6))))

        async def one(unit):
            async with limit:
                await self._crawl_seed(unit, dataset)

        await asyncio.gather(*(one(u) for u in batch.units))
    async def _crawl_seed(self, unit, dataset):
        """One seed -> one BrowserContext -> a DFS walk -> a stream of HttpEvents (1 -> N).

        ── WHY THE TRAVERSAL LIVES HERE AND NOT IN crawl4ai ────────────────────────────────────

        `crawl4ai` already had depth, and it cannot carry this surface: its `PageOut` is
        content-deterministic by ADR 0015 and keeps "only STABLE response fields — status and
        content-type", so it emits NO RESPONSE HEADERS. Those headers are the point — the
        `Access-Control-Allow-Headers` / `Vary` / `Set-Cookie` an application answers with are
        what `surface._points()` calls "the half a wordlist cannot know", and what
        `exchanges_<prog>` hands the desync split axis. So the traversal came to the headers.

        ── THREE BOUNDS, BECAUSE THEY FAIL DIFFERENTLY ─────────────────────────────────────────

        `max_pages` bounds breadth, `depth` bounds distance, and `max_seconds_per_host` bounds
        WALL CLOCK — which neither of the others does. A host serving a thousand fast pages
        satisfies every per-navigation timeout and still holds its Worker for as long as it
        likes; that is the bound that stops one origin from owning a Machine.

        Each is announced when it bites. A crawl that stopped early and reads as a complete one
        is how a surface silently describes a fraction of a program.
        """
        seed = str(_field(unit.value, "seed") or _field(unit.value, "url") or "").strip()
        program = str(_field(unit.value, "program") or "")
        if not seed.startswith(("http://", "https://")):
            # Not a navigable URL. Push ONE event carrying the reason rather than dropping it:
            # a scope row that cannot be crawled is a finding about the scope. Seeds run under
            # `batch.units`, so there is no current Unit — each push names an out-of-loop key
            # (ADR 0028). `{seed}#{n}` is unique per seed (one seed = one Unit) and stable.
            await dataset.push(self._event(seed, program, "request", seed,
                                           error="not an http(s) url"), key=f"{seed}#0")
            return

        # IMPORTED HERE, BELOW THE VALIDATION, and the order is the point. It used to be the first
        # line of this method, which made rejecting `com.stubhubandroid` — a pure string check that
        # never opens a browser — depend on Playwright being installed. Every developer has it, so
        # the test passed everywhere and failed only in CI, where it is deliberately absent (it is
        # an actor's own dependency, not an SDK extra). Validating before importing is also just
        # right: a seed that cannot be crawled should cost nothing to refuse.
        from playwright.async_api import Error as PWError

        ctx_kw = {
            "ignore_https_errors": bool(param.get("ignore_https_errors", True)),
            "viewport": {
                "width": int(param.get("viewport_width", 1280)),
                "height": int(param.get("viewport_height", 800)),
            },
        }
        ua = str(param.get("user_agent", "") or "")
        if ua:
            ctx_kw["user_agent"] = ua
        extra = param.get("extra_headers") or {}
        if extra:
            ctx_kw["extra_http_headers"] = {str(k): str(v) for k, v in dict(extra).items()}

        context = await self.browser.new_context(**ctx_kw)
        queue: asyncio.Queue = asyncio.Queue()
        pending_bodies: dict = {}
        capture_bodies = bool(param.get("capture_bodies", False))

        cap = max(1, int(param.get("max_events", 400)))
        depth_max = max(0, int(param.get("depth", 0)))
        pages_cap = max(1, int(param.get("max_pages", 1)))
        budget = float(param.get("max_seconds_per_host", 0) or 0)
        distance = max(0, int(param.get("simhash_distance", 3)))
        per_path = max(1, int(param.get("pages_per_path", 2)))

        try:
            if bool(param.get("block_media", True)):
                async def _route(route, request):
                    if request.resource_type in _HEAVY:
                        await route.abort()
                    else:
                        await route.continue_()

                await context.route("**/*", _route)

            page = await context.new_page()
            # WIRED ONCE, FOR EVERY NAVIGATION. The handlers hang off the page, not off a
            # navigation, so re-using one page across the walk keeps capture continuous and
            # costs one context for the whole seed instead of one per page.
            self._wire(page, queue, pending_bodies, seed, program, capture_bodies)

            loop = asyncio.get_running_loop()
            deadline = (loop.time() + budget) if budget > 0 else None
            host = _host_of(seed)

            frontier = [(seed, 0)]
            # The seed's own shape is seeded into `seen` so a self-link cannot re-fetch it.
            seed_fp = request_fingerprint("GET", seed)
            seen = [seed_fp]
            # HOW MANY PAGES EACH PATH SHAPE HAS CONTRIBUTED. The seed is already one of its own.
            #
            # `simhash_distance` decides what counts as the SAME path — `/product/1001` and
            # `/product/1002` collapse to one shape once object ids are masked — and this decides
            # how many concrete pages that shape may spend. At 1 (the old behaviour, a bare skip)
            # a route was sampled exactly once, so a template that renders differently for two
            # ids was judged on whichever the crawler happened to reach first.
            shape_pages = {seed_fp: 1}
            emitted, pages, stopped = 0, 0, ""

            while frontier and pages < pages_cap and emitted < cap:
                if deadline is not None and loop.time() >= deadline:
                    stopped = f"max_seconds_per_host {budget:g}s reached after {pages} page(s)"
                    break
                url, d = frontier.pop()
                emitted = await self._visit(page, url, seed, program, dataset, queue,
                                            pending_bodies, capture_bodies, emitted, cap)
                pages += 1

                if d >= depth_max or emitted >= cap:
                    continue
                for link in await self._links(page, host):
                    if len(frontier) >= _FRONTIER_CAP:
                        break
                    fp = request_fingerprint("GET", link)
                    # NEAR-duplicate, not exact: a generated path space (/product/1001,
                    # /product/1002 …) collapses to one shape at distance 0 once object ids are
                    # masked, so the walk spends its page budget on distinct ROUTES.
                    #
                    # `nearest` rather than `near_duplicate`, because a shape now has a QUOTA and
                    # a quota has to be counted against a stable representative. A matched shape
                    # is admitted until it has spent `pages_per_path`, then skipped exactly as
                    # before.
                    rep = nearest(fp, seen, distance)
                    if rep is not None:
                        if shape_pages.get(rep, 0) >= per_path:
                            continue
                        shape_pages[rep] = shape_pages.get(rep, 0) + 1
                    elif len(seen) < _SEEN_CAP:
                        seen.append(fp)
                        shape_pages[fp] = 1
                    # A NEW SHAPE PAST _SEEN_CAP IS STILL CRAWLED, untracked — the cap bounds the
                    # linear scan, not the crawl, and dropping the link there would silently make
                    # a big host's tail unreachable.
                    frontier.append((link, d + 1))

            if not stopped and pages >= pages_cap and frontier:
                stopped = f"max_pages {pages_cap} reached with {len(frontier)} link(s) unvisited"
            elif not stopped and emitted >= cap:
                stopped = f"max_events {cap} reached after {pages} page(s)"
            if stopped:
                # A BOUND THAT IS ANNOUNCED, on the row rather than in a log nobody reads back.
                await dataset.push(self._event(seed, program, "request", seed, error=stopped),
                                   key=f"{seed}#bound")
        finally:
            # Closing the context is what makes a context-per-unit affordable — leak it and a
            # long session accumulates one per unit until Chromium's memory does the accounting
            # for us. `finally` because a cancelled or failed unit must not leak either.
            try:
                await context.close()
            except PWError:
                pass

    async def _visit(self, page, url, seed, program, dataset, queue, pending_bodies,
                     capture_bodies, emitted, cap):
        """Navigate to one URL and drain what it produced. Returns the new `emitted` count.

        The key stays `{seed}#{n}` with `n` monotonic ACROSS the whole walk, not per page: the
        sub-unit id has to be unique within the Unit, and the Unit is the seed.
        """
        # WHERE THIS CRAWLER IS, for the beat. Set BEFORE navigation rather than after, because
        # navigation is the slow part and a stall is exactly when a reader needs the URL named —
        # the same reasoning as `prog.last` on the smuggling axis.
        #
        # It is the one fact the healthcheck could never report: `browser_alive` returned
        # `{"contexts": N}`, so every beat said how many tabs were open and none of them said
        # which program or which page, which is what an operator watching a 454-program campaign
        # is actually asking.
        self._at, self._program = url, program
        nav = asyncio.create_task(self._navigate(page, url, program, queue))

        # Drain-while-navigating. `wait_for` rather than a plain `await queue.get()` so the
        # loop notices navigation finishing even when no further events arrive; the timeout
        # is a poll interval, not a deadline.
        while emitted < cap and (not nav.done() or not queue.empty()):
            try:
                ev = await asyncio.wait_for(queue.get(), timeout=0.25)
            except asyncio.TimeoutError:
                continue
            await dataset.push(await self._finish(ev, pending_bodies, capture_bodies),
                               key=f"{seed}#{emitted}")
            emitted += 1

        if not nav.done():
            nav.cancel()
        await asyncio.gather(nav, return_exceptions=True)

        while emitted < cap and not queue.empty():
            await dataset.push(await self._finish(queue.get_nowait(), pending_bodies,
                                                  capture_bodies), key=f"{seed}#{emitted}")
            emitted += 1
        return emitted

    async def _links(self, page, host):
        """Same-host hrefs on the current page, in document order.

        SAME HOST, NOT SAME SITE, and that is a scope decision rather than a crawl one. A seed
        is one row of somebody's published scope; `blog.example.com` linked from
        `app.example.com` is only in scope if THAT row exists too, and it will be crawled as its
        own seed when it does. Widening here would put the crawler on hosts nobody authorised.
        """
        try:
            hrefs = await page.eval_on_selector_all(
                "a[href]", "els => els.map(e => e.href)")
        except Exception:  # noqa: BLE001 — a page that cannot be queried yields no links
            return []
        out, seen = [], set()
        for h in hrefs or []:
            h = str(h or "").split("#", 1)[0]
            if not h.startswith(("http://", "https://")) or _host_of(h) != host:
                continue
            if h not in seen:
                seen.add(h)
                out.append(h)
        return out


    # ---- capture --------------------------------------------------------------------

    def _wire(self, page, queue, pending_bodies, seed, program, capture_bodies):
        """Attach the capture handlers. They are SYNC callbacks that only enqueue: anything
        awaited in here runs inside Playwright's own dispatch and stalls the page."""

        def on_request(request):
            tx = self._tx()
            # The tx id rides on the Request object because that is the ONE thing the response
            # and the failure events both hold a reference to. If a Playwright version ever
            # refuses the attribute, both sides still emit — they just stop joining, which
            # degrades the pairing rather than the inventory.
            try:
                request._kontra_tx = tx
            except AttributeError:
                pass
            queue.put_nowait(
                self._event(
                    seed, program, "request", request.url,
                    tx=tx,
                    method=request.method,
                    resource_type=request.resource_type,
                    headers=_headers_json(request.headers),
                    body_bytes=len(request.post_data_buffer or b""),
                )
            )

        def on_response(response):
            tx = getattr(response.request, "_kontra_tx", "") or self._tx()
            ev = self._event(
                seed, program, "response", response.url,
                tx=tx,
                method=response.request.method,
                resource_type=response.request.resource_type,
                status=response.status,
                headers=_headers_json(response.headers),
                mime_type=(response.headers or {}).get("content-type", ""),
                body_bytes=_int_header(response.headers, "content-length"),
                from_cache=bool(response.from_service_worker),
            )
            # PARKED ALWAYS, not only for bodies. `response.headers` is the SYNC accessor and it
            # omits `Set-Cookie` — Playwright exposes that one only through the async
            # `all_headers()`, because it is the one header that legitimately repeats. Nothing
            # said so: the event carried a plausible header set with a hole in it, and
            # `surface._points()` names `set-cookie` as one of its three POINT_RATIONALE headers
            # ("a value the application reads back"), so that injection point has never once been
            # produced. Awaiting here would stall Playwright's dispatch, so the handle is parked
            # and resolved at yield time — the mechanism bodies already used.
            pending_bodies[id(ev)] = response
            queue.put_nowait(ev)

        def on_failed(request):
            queue.put_nowait(
                self._event(
                    seed, program, "response", request.url,
                    tx=getattr(request, "_kontra_tx", "") or self._tx(),
                    method=request.method,
                    resource_type=request.resource_type,
                    headers="{}",
                    error=(request.failure or "request failed"),
                )
            )

        page.on("request", on_request)
        page.on("response", on_response)
        page.on("requestfailed", on_failed)

    async def _navigate(self, page, seed, program, queue):
        """Load the seed and then keep capturing for `settle_ms`. A navigation TIMEOUT is not
        a failed unit: a page that never fires `load` has usually already issued the requests
        we came for, so we record the timeout as an event and keep whatever we captured."""
        from playwright.async_api import Error as PWError

        try:
            await page.goto(
                seed,
                wait_until=str(param.get("wait_until", "load")),
                timeout=int(param.get("nav_timeout_ms", 20000)),
            )
        except PWError as e:
            queue.put_nowait(
                self._event(seed, program, "response", seed,
                            error=f"{type(e).__name__}: {str(e).splitlines()[0][:300]}")
            )
        try:
            await page.wait_for_timeout(int(param.get("settle_ms", 2000)))
        except PWError:
            pass

    async def _finish(self, ev, pending_bodies, capture_bodies):
        """Resolve anything that had to wait for the context: the full header set, then bodies.

        Both are awaits that cannot happen in the capture callback without stalling Playwright's
        dispatch, and both are still inside the `try` that owns the context — a response handle
        outlives neither.
        """
        response = pending_bodies.pop(id(ev), None)
        if response is None:
            return ev

        try:
            # `all_headers()` is the only accessor that includes Set-Cookie. It is also the one
            # that lowercases and joins repeats, which is what `_headers_json` wants anyway.
            ev["headers"] = _headers_json(await response.all_headers())
        except Exception:  # noqa: BLE001
            # Keep the sync snapshot already on the event: an incomplete header set beats none,
            # and a context closing under us is an ordinary end-of-unit race.
            pass

        if not capture_bodies:
            return ev
        try:
            body = await response.body()
        except Exception:
            return ev  # 204/redirect/aborted — no body is a normal outcome, not an error
        ev["body_sha256"] = hashlib.sha256(body).hexdigest()
        ev["body_bytes"] = len(body)
        return ev

    # ---- helpers --------------------------------------------------------------------

    def _tx(self) -> str:
        """A transaction id that is unique across loads, sessions and Workers — see `_nonce`.

        `getattr` on the nonce rather than `self._nonce` directly, because `_event` calls this as
        a fallback (`tx or self._tx()`) and a Method reached without `@actor.load` having run —
        which is what every unit test does — would otherwise raise AttributeError on the id
        rather than on the browser it actually lacks.
        """
        self._seq = getattr(self, "_seq", 0) + 1
        return f"{getattr(self, '_nonce', '00000000')}{self._seq:08x}"

    def _event(self, seed, program, kind, url, *, tx="", method="", resource_type="",
               status=-1, mime_type="", headers="{}", body_sha256="", body_bytes=-1,
               from_cache=False, error="") -> dict:
        return {
            "seed": seed,
            "program": program,
            "tx": tx or self._tx(),
            "kind": kind,
            "url": url,
            "method": method,
            "resource_type": resource_type,
            "status": status,
            "mime_type": mime_type,
            "headers": headers,
            "body_sha256": body_sha256,
            "body_bytes": body_bytes,
            "from_cache": from_cache,
            "ts_ms": int(time.time() * 1000),
            "error": error,
        }


if __name__ == "__main__":
    actor.serve()
