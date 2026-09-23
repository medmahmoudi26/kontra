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
from dataclasses import dataclass

from kontra import actor, param

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
        # Serial per-session counter for tx ids. Unique WITHIN a unit is all the join needs,
        # and a counter avoids paying for a uuid per HTTP event.
        self._seq = 0

    @actor.healthcheck
    async def browser_alive(self):
        """Probed after any Method failure and on the beat. A crashed Chromium takes every
        in-flight context with it, so this is the difference between reloading the session and
        isolating one bad URL."""
        browser = getattr(self, "browser", None)
        if browser is None or not browser.is_connected():
            raise RuntimeError("browser gone")
        return {"contexts": len(browser.contexts)}

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
        """One seed -> one BrowserContext -> a stream of HttpEvents (1 -> N)."""
        seed = str(unit.value.get("seed") or unit.value.get("url") or "").strip()
        program = str(unit.value.get("program") or "")
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

        context = await self.browser.new_context(**ctx_kw)
        queue: asyncio.Queue = asyncio.Queue()
        pending_bodies: dict = {}
        capture_bodies = bool(param.get("capture_bodies", False))

        try:
            if bool(param.get("block_media", True)):
                async def _route(route, request):
                    if request.resource_type in _HEAVY:
                        await route.abort()
                    else:
                        await route.continue_()

                await context.route("**/*", _route)

            page = await context.new_page()
            self._wire(page, queue, pending_bodies, seed, program, capture_bodies)

            nav = asyncio.create_task(self._navigate(page, seed, program, queue))
            emitted = 0
            cap = max(1, int(param.get("max_events", 400)))

            # Drain-while-navigating. `wait_for` rather than a plain `await queue.get()` so the
            # loop notices navigation finishing even when no further events arrive; the timeout
            # is a poll interval, not a deadline.
            while emitted < cap and (not nav.done() or not queue.empty()):
                try:
                    ev = await asyncio.wait_for(queue.get(), timeout=0.25)
                except asyncio.TimeoutError:
                    continue
                # No current Unit under batch.units -> key each event by seed and its index.
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
        finally:
            # Closing the context is what makes a context-per-unit affordable — leak it and a
            # long session accumulates one per unit until Chromium's memory does the accounting
            # for us. `finally` because a cancelled or failed unit must not leak either.
            try:
                await context.close()
            except PWError:
                pass

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
            if capture_bodies:
                # The body has to be read while the context is still open, but reading it here
                # would block dispatch. Park the handle and read it at yield time — still
                # inside the `try` that owns the context.
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
        """Resolve anything that had to wait for the context: response bodies."""
        if not capture_bodies:
            return ev
        response = pending_bodies.pop(id(ev), None)
        if response is None:
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
        self._seq += 1
        return f"{self._seq:08x}"

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
