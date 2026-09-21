"""crawl4ai — DFS deep crawl that records every request shape it reaches, once.

This actor crawls and saves. That is the whole job. It has no opinion about caching, and it
does not decide what is worth probing — deciding whether a target is cacheable, and whether it
is poisonable, belongs entirely to the CACHEBUSTER, which proves it behaviourally.

ONE thing suppresses a record:

  DEDUPE  — a 64-bit SIMHASH over the canonical REQUEST string (`canonical_string`, ported from
            the v1 proxy addon, with object-id path segments masked first — see
            `_ID_SEGMENT_RE`). A page is skipped only when its fingerprint is within
            `simhash_distance` bits of one ALREADY EMITTED on this seed. That is a statement
            about what we have already written down, never a judgement about the target.

            It hashes the REQUEST, not the response. Two pages with wildly different content are
            still one endpoint to a header-reflection probe if the request that fetched them has
            the same shape; conversely the same endpoint serving different content on every hit
            must still only be recorded once. Response-side hashing gets both backwards.

            This is also what EXACT dedupe cannot do: a target minting a distinct path per
            object — /product/1001, /product/1002, … — has a distinct canonical string for every
            one of them but is ONE endpoint. Masking ids then measuring hamming distance
            collapses the family; an md5 of the same string does not.

  CAP     — `max_pages` bounds pages EMITTED, not pages crawled. Traversal keeps walking
            (max_pages * _TRAVERSAL_FANOUT) so dedupe has candidates to collapse.

Nothing else is filtered. Earlier versions gated on cache evidence and on static-asset paths;
both are gone. A `Cache-Control` header states an intention, not the deployed behaviour — an
origin can say `no-store` and still sit behind a caching CDN — so gating on it drops precisely
the targets where the interesting mismatch lives. Headers can lie; only behaviour is evidence.

Traversal is crawl4ai's native DFS, not BFS: DFS reaches DEEP distinct path shapes —
/account/orders/{id}/invoice — far sooner than a BFS that must first exhaust every sibling link
in the nav bar, and distinct shapes are exactly what survives dedupe.

Two durability tiers, chosen by `resumable`, unchanged from the base actor:

  DEFAULT (resumable=false) — ATOMIC per seed. No unit_state. A browser/host death re-crawls the
    WHOLE seed; because every page is keyed by its content sha, re-emitting is an idempotent
    overwrite, so nothing duplicates.
  RESUMABLE (resumable=true) — crawl4ai's native FRONTIER-ONLY state (no page content) is wired
    to `self.unit_state`, so a death resumes the walk instead of restarting. The seen-fingerprint
    set rides the same slot (see `_SEEN_CAP`) so a resumed seed does not re-yield a shape it
    already emitted.

Run it (a Python actor is run by Python — the process becomes a Temporal activity worker):
    kontra serve --actor examples/python/crawl4ai
"""

import asyncio
import contextlib
import contextvars
import hashlib
import os
import re
import socket
from dataclasses import dataclass, field
from urllib.parse import parse_qsl, quote, urlparse

from simhash import Simhash as _Simhash

from kontra import SessionLost, actor, param

# crawl4ai surfaces a lost browser two ways: a raised exception, OR a returned CrawlResult
# with success=False whose error_message mentions the disconnect. Both -> SessionLost (rebuild).
_BROWSER_DEAD = ("closed", "disconnect", "crashed", "target page", "browser has been closed")

# Chromium flags for a headless crawl on a SMALL box. Every one of these turns off a subsystem
# a crawl never uses — no GPU on a droplet, no crash reporting, no sync/translate/extensions,
# no background timers. Two matter most on a 2 GB host:
#   --disable-dev-shm-usage   containers get a tiny /dev/shm; without this chromium dies under
#                             tab pressure with an opaque renderer crash
#   --js-flags=--max-old-space-size  bounds each renderer's V8 heap instead of letting one
#                             runaway page consume the box
# DELIBERATELY ABSENT: anything JS-visible. No --disable-web-security, no image/JS blocking, no
# --disable-features touching client hints or UA — those change what a target sees and would
# undo the stealth posture. These flags are about resource use, not fingerprint.
_CHROME_LEAN_ARGS = (
    "--disable-gpu",
    "--disable-dev-shm-usage",
    "--disable-software-rasterizer",
    "--disable-extensions",
    "--disable-component-extensions-with-background-pages",
    "--disable-default-apps",
    "--disable-background-networking",
    "--disable-background-timer-throttling",
    "--disable-backgrounding-occluded-windows",
    "--disable-renderer-backgrounding",
    "--disable-breakpad",
    "--disable-crash-reporter",
    "--disable-domain-reliability",
    "--disable-client-side-phishing-detection",
    "--disable-hang-monitor",
    "--disable-prompt-on-repost",
    "--disable-sync",
    "--metrics-recording-only",
    "--mute-audio",
    "--no-first-run",
    "--no-default-browser-check",
    "--js-flags=--max-old-space-size=384",
)

# ONE browser per session — but ONE BrowserContext PER SEED.
#
# crawl4ai caches contexts by a CONFIG SIGNATURE (BrowserManager._make_config_signature) and
# hands every caller with a matching signature the same one, adding only a page. Every seed here
# builds the SAME CrawlerRunConfig, so out of the box every seed crawls in ONE
# context: one cookie jar, one storage, one HTTP cache shared by unrelated targets — state leaks
# across seeds and the seeds contend on a single context.
#
# No config field can split them: 0.8+/0.9 hash a WHITELIST of context-affecting fields only
# (proxy_config, locale, timezone_id, geolocation, override_navigator, simulate_user, magic) and
# every one of those changes what we look like on the wire, which is not an acceptable side
# effect of an isolation fix. A per-Unit `session_id` is worse — it pins ONE page for the whole
# seed, so the seed's own traversal batch would drive every URL through a single tab (and on
# 0.7.x kill_session would close the SHARED context out from under the other seeds).
#
# So we don't fight the cache, we bypass it: the seed binds its own context in a contextvar and
# crawl4ai's page allocation is routed to it. The context is built with crawl4ai's OWN
# create_browser_context/setup_context off the SAME crawlerRunConfig, so it is configured
# identically — same fingerprint, same init scripts, only a different identity — and it is
# CLOSED when the seed ends, so a batch never accumulates contexts. Contextvar, not `self.*`:
# crawl4ai's per-URL tasks inherit the calling task's context, so the routing reaches them
# without a parameter.
_UNIT_CTX: contextvars.ContextVar = contextvars.ContextVar("kontra_crawl4ai_ctx", default=None)


class _ArunContext:
    """The in-flight Unit's OWN BrowserContext: created on its first page, closed with the Unit.
    Created LAZILY so a seed that never fetches (an invalid url) costs nothing."""

    def __init__(self):
        self.context = None
        self.closed = False
        self._lock = asyncio.Lock()   # the seed's own traversal batch asks for pages concurrently

    async def page(self, manager, cfg):
        async with self._lock:
            if self.context is None:
                # crawl4ai's own builders off the same config -> a context configured exactly
                # like the one it would have cached, with a different identity.
                self.context = await manager.create_browser_context(cfg)
                await manager.setup_context(self.context, cfg)
        page = await self.context.new_page()
        stealth = getattr(manager, "_apply_stealth_to_page", None)
        if stealth is not None:
            await stealth(page)          # no-op unless the BrowserConfig turns stealth on
        return page, self.context

    async def close(self):
        ctx, self.context, self.closed = self.context, None, True
        if ctx is not None:
            try:
                await ctx.close()
            except Exception:            # noqa: BLE001 — a dead browser already took it with it
                pass


def valid_url(u: str) -> bool:
    return isinstance(u, str) and (u.startswith("http://") or u.startswith("https://"))


def _bad(url) -> dict:
    """A junk seed can't be fixed by retrying: record it, never fail the unit."""
    return {"seed": str(url), "url": str(url), "depth": 0, "status": 0,
            "request": {}, "response": {}, "markdown": "", "links": 0,
            "requests": [],
            "error": "invalid url"}


async def _resolves(url: str) -> bool:
    """Does the seed's host resolve? A dead host still costs a full navigation timeout, and it
    holds this seed's BrowserContext for the whole wait — on
    a large seed list that is the difference between a crawl and a stall. getaddrinfo is
    blocking, so it runs off the event loop."""
    host = urlparse(url).hostname
    if not host:
        return False
    try:
        await asyncio.to_thread(socket.getaddrinfo, host, None)
        return True
    except OSError:
        return False


def _lower_headers(headers) -> dict:
    """Header lookup is case-insensitive on the wire; playwright and crawl4ai disagree on case
    depending on the HTTP version, so normalize once instead of guessing at each call site."""
    return {str(k).strip().lower(): str(v) for k, v in (headers or {}).items()}


# ---------------------------------------------------------------------------------
# There is NO value gate. Every request the crawl reaches is a candidate.
#
# This actor crawls and records; it holds no opinion about caching. A previous version gated
# pages on cache evidence (`Cache-Control` permitting a shared cache, or an `Age`/`X-Cache`
# stamp) and on staticness. Both are removed, deliberately:
#
#   - Cache headers are not trustworthy input. An origin can serve `Cache-Control: no-store` and
#     still sit behind a CDN that caches; a header states an intention, not the deployed
#     behaviour. Gating on them silently drops exactly the targets where the interesting
#     mismatch between stated and real policy lives. Deciding what is cacheable is the
#     CACHEBUSTER's job, and it proves it behaviourally rather than reading a header.
#   - Static-asset rejection guessed from the path. A hashed bundle usually cannot reflect a
#     header, but "usually" is not a reason to make the page invisible to the data plane.
#
# The ONLY thing that suppresses a record is request-shape near-duplication (below), and that is
# a statement about what we have ALREADY emitted, never about the target.

# ---------------------------------------------------------------------------------
# DEDUPE — 64-bit simhash over the CANONICAL REQUEST STRING, which is ported verbatim from the
# v1 proxy addon (legacy/workers/proxy/addon/fingerprint.py). Three design choices in it are
# load-bearing:
#
#   HOST     is the host of the URL WE FETCHED (port-stripped, lowercased), never a client-sent
#            Host header — a forged Host collided distinct subdomains of one SPA build in v1 and
#            silently dropped their traffic.
#   QUERY    keeps param NAMES (sorted), drops VALUES — /api?x=1 and /api?x=2 are one shape to a
#            probe, /api?x= and /api?y= are two.
#   HEADERS  are NAMES only, values excluded, so the same request dedupes across session tokens.
#
# The simhash is over that STRING, not over the page body: the dedupe is in REQUEST space. Two
# pages can render differently and still be the same thing to probe (same endpoint, same param
# names, same header set), and two byte-identical pages behind different endpoints are two
# things to probe. Body-space dedupe would answer the wrong question.
#
# An earlier variant hashed that string verbatim with md5; this masks object-id path segments first
# (`skeleton_string`) and then compares with a Hamming radius. Both steps are needed — see
# `_ID_SEGMENT_RE` for why the radius alone cannot do it.
def normalize_host(raw_host: str) -> str:
    """Lowercase; strip a trailing port. Matches v1.extract_domain."""
    host = (raw_host or "").strip().lower()
    if ":" in host and not host.startswith("["):
        host = host.rsplit(":", 1)[0]
    return host


def _canonical_parts(method: str, url: str, header_names=(), body: str = "") -> tuple:
    parsed = urlparse(url)
    names = sorted({quote(n, safe="")
                    for n, _ in parse_qsl(parsed.query, keep_blank_values=True)})
    hdrs = sorted({str(h).strip().lower() for h in (header_names or ())})
    return (str(method or "GET").upper(), normalize_host(parsed.hostname or ""),
            parsed.path or "/", names, hdrs, body)


def _assemble(method: str, host: str, path: str, names, hdrs, body: str) -> str:
    if names:
        path = f"{path}?{'&'.join(names)}"
    return f"{method} {host} {path} {hdrs} {body}"


def canonical_string(method: str, url: str, header_names=(), body: str = "") -> str:
    """`method host path?names [header names] body` — the exact v1 layout, including the Python
    list repr of the header names. Kept byte-for-byte so the string this actor hashes is the
    same one the v1 proxy addon builds for the same request."""
    return _assemble(*_canonical_parts(method, url, header_names, body))


# A path segment that is an OBJECT ID rather than a route. Masking these before hashing is what
# makes a small Hamming radius mean anything: measured over these canonical strings, a raw
# simhash puts /product/1001 and /product/1002 ~9 bits apart and two unrelated endpoints ~13
# apart, so no radius separates them — a 64-bit simhash simply does not concentrate over a
# ~50-character string. With ids masked the same pair is 0 bits apart and everything structural
# (a different route 11+, host 15, method 13, header set 20) stays far outside the radius.
#
# WHOLE segments only, and only three unambiguous shapes — a run of digits, a hex blob that
# contains a digit (`9f3a2b1c`; the digit requirement keeps `/decade` and `/facade` intact), and
# a uuid. Deliberately NOT masked: partially-numeric slugs (`story-77`) and versions (`v2`,
# `v10`). The errors are not symmetric — masking too much MERGES two distinct endpoints and the
# second one is never probed, while masking too little only costs one extra unit — so the
# leftover variability is left to the radius knob instead.
_ID_SEGMENT_RE = re.compile(
    r"^(?:\d+|(?=[0-9a-f]*\d)[0-9a-f]{6,}|[0-9a-f]{8}-[0-9a-f-]{4,})$", re.IGNORECASE)


def mask_object_ids(path: str) -> str:
    return "/".join("#" if _ID_SEGMENT_RE.match(seg) else seg for seg in path.split("/"))


def skeleton_string(method: str, url: str, header_names=(), body: str = "") -> str:
    """The canonical string with object-id path segments masked — the string actually hashed."""
    method, host, path, names, hdrs, body = _canonical_parts(method, url, header_names, body)
    return _assemble(method, host, mask_object_ids(path), names, hdrs, body)


# Character n-grams, not words: what is left to collapse after masking differs by a few
# characters INSIDE a path segment (/settings/email vs /settings/phone), and a word tokenizer
# splitting on `/` would make those two whole tokens with nothing in common. Overlapping 4-grams
# share every window that does not straddle the change, so such a pair lands ~6 bits apart —
# inside a radius of 6-8, outside the default of 3. Above ~10 the radius starts eating genuinely
# distinct endpoints (a different method is 13 bits, a different host 15), so do not go there.
_MASK64 = (1 << 64) - 1


def simhash64(text: str) -> int:
    """64-bit Charikar simhash, from the `simhash` package rather than hand-rolled.

    This was a local implementation until measurement showed why that is a bad idea for a
    similarity function: a subtly wrong one does not raise, it returns plausible small numbers
    and reads as a clean result. The Go sibling's hand-rolled version turned out to be blind to
    an injected script tag on any page over ~200 words, and nothing caught it for a whole live
    sweep. A library that many people run is the right dependency here.

    Empty text is pinned to 0. The library fingerprints the empty string to a non-zero constant,
    which would make every empty request shape a near-duplicate of every other and silently
    suppress them as a group."""
    if not text:
        return 0
    return int(_Simhash(text).value)


def hamming_distance(a: int, b: int) -> int:
    return int(_Simhash(int(a)).distance(_Simhash(int(b))))


def near_duplicate(fingerprint: int, seen, distance: int) -> bool:
    """Linear scan — the seen set is capped at _SEEN_CAP, so an index (banded LSH) would cost
    more code than the few hundred XORs it saves."""
    return any(hamming_distance(fingerprint, other) <= distance for other in seen)


def fingerprint_request(record: dict) -> dict:
    """The request whose canonical shape IDENTIFIES this page. Prefer the browser's own captured
    request for the page url — it carries the real header names, which are part of the canonical
    string — and fall back to a synthesized GET when capture is off (`capture_requests=false`)."""
    url = str(record.get("url") or "")
    for req in record.get("requests") or []:
        if req.get("url") == url:
            return req
    return {"method": "GET", "url": url, "headers": {}}


def request_simhash(req: dict) -> int:
    return simhash64(skeleton_string(req.get("method") or "GET", str(req.get("url") or ""),
                                     (req.get("headers") or {}).keys()))


# The seen-fingerprint set is a RESUME CHECKPOINT, so it is capped. An unbounded set that grows
# with every yielded page is exactly the shape that blew Temporal's history limit elsewhere in
# this repo: the state is written on every emit and every version of it is retained. The cap
# bounds both the checkpoint and the live set, so a fresh crawl and a resumed one dedupe over
# the same window; the only cost of eviction is re-yielding a long-evicted shape, which is an
# idempotent overwrite (the sub-unit id is the record sha). It also bounds `near_duplicate`,
# which is a linear scan.
_SEEN_CAP = 512


def dump_seen(seen) -> list:
    """Fingerprints leave as 16-char HEX, never as JSON numbers: a 64-bit int does not survive a
    round trip through a JSON parser with 53-bit floats (the orchestrator is TypeScript), and a
    silently rounded fingerprint would quietly stop matching."""
    return [f"{int(fp) & _MASK64:016x}" for fp in seen]


def load_seen(raw) -> list:
    out = []
    for item in (list(raw) if raw else [])[-_SEEN_CAP:]:
        try:
            out.append(int(str(item), 16) & _MASK64)
        except ValueError:            # a hand-edited or truncated checkpoint entry
            continue
    return out


def remember(seen: list, fingerprint: int) -> list:
    """Record a fingerprint, evicting the oldest once the cap is reached."""
    seen.append(int(fingerprint) & _MASK64)
    del seen[:-_SEEN_CAP]
    return seen


# ---------------------------------------------------------------------------------

# Request headers dropped before a captured request is emitted. The sub-unit id is the sha of
# the WHOLE record (unitstore.put_subunit), so anything that varies between two crawls of the
# same page gives it a new id and breaks idempotent re-emission (ADR 0015).
_VOLATILE_REQ_HEADERS = frozenset({
    "cookie", "authorization", "date", "if-none-match", "if-modified-since",
    "x-request-id", "x-client-data", "traceparent", "tracestate", "sec-fetch-user",
})

# Resource types worth carrying downstream. Images/fonts/media/stylesheets are the bulk of a
# page's requests and the least interesting for cache poisoning — dropping them keeps the
# record small and its sha far more stable.
_CAPTURE_TYPES = frozenset({"document", "xhr", "fetch", "script", "manifest", "other"})


def _capture_requests(result, seed: str, limit: int) -> list:
    """Normalize the browser's captured requests into a DETERMINISTIC, IN-SCOPE list.

    This is the actual prize of the crawl for a cache-poisoning pass: the XHR/fetch endpoints a
    link-crawler never reaches. Four rules make it safe to emit:

      SCOPE  — same host as the seed only. Third-party requests (ads, analytics, CDNs) are both
               the dominant source of sha variance AND outside the engagement's authorization;
               emitting them would point the next actor at hosts we were never cleared to probe.
      FILTER — only the resource types above.
      STRIP  — volatile headers removed (see _VOLATILE_REQ_HEADERS); no timestamps ever.
      ORDER  — deduped by (method, url) and sorted, so two crawls of the same page produce
               byte-identical JSON and therefore the same sub-unit id.
    """
    if limit <= 0:
        return []
    raw = getattr(result, "network_requests", None) or []
    seed_host = (urlparse(seed).hostname or "").lower()
    seen, out = set(), []
    for ev in raw:
        if not isinstance(ev, dict) or ev.get("event_type") != "request":
            continue
        if str(ev.get("resource_type") or "other").lower() not in _CAPTURE_TYPES:
            continue
        url = str(ev.get("url") or "")
        if not valid_url(url) or (urlparse(url).hostname or "").lower() != seed_host:
            continue
        method = str(ev.get("method") or "GET").upper()
        if (method, url) in seen:
            continue
        seen.add((method, url))
        hdrs = ev.get("headers") or {}
        out.append({
            "method": method,
            "url": url,
            "headers": {str(k).lower(): str(v) for k, v in sorted(hdrs.items())
                        if str(k).lower() not in _VOLATILE_REQ_HEADERS},
        })
    out.sort(key=lambda r: (r["method"], r["url"]))
    return out[:limit]


@dataclass
class CrawlSeed:
    url: str
    max_pages: int = 10   # per-seed YIELD cap (see CrawlParams.max_pages)


@dataclass
class PageOut:
    seed: str
    url: str
    depth: int
    status: int
    request: dict          # {method, url}
    response: dict         # {status, content_type} — STABLE fields only (see _page_out)
    markdown: str          # empty in discovery_mode
    links: int             # count of internal links discovered on the page
    # In-scope requests the BROWSER made while rendering this page — [{method, url, headers}],
    # same-host only, normalized (see _capture_requests). The XHR/fetch surface a link-crawler
    # never sees, and what a downstream cache-poisoning pass actually probes.
    requests: list = field(default_factory=list)
    error: str | None = None


@dataclass
class CrawlParams:
    discovery_mode: bool = False   # map links fast (no markdown) vs. full content
    depth: int = 2                 # DFS max depth
    headless: bool = True
    max_pages: int = 10            # pages YIELDED per seed (traversal runs _TRAVERSAL_FANOUT x)
    resumable: bool = False        # False = atomic bounded (DEFAULT); True = frontier-resumable
    capture_requests: bool = True  # emit the browser's in-scope requests (see _capture_requests)
    capture_limit: int = 50        # max captured requests per page (bounds the record size)
    # Hamming radius over the 64-bit fingerprint. MEASURED against the library implementation
    # with object ids masked first (do not re-derive these by feel — masking already does most
    # of the work, so the interesting rungs are not where intuition puts them):
    #
    #   same shape, different object id .... 0 bits   <- masking collapses it at ANY radius
    #   uuid path vs numeric id ............ 0 bits
    #   different METHOD .................. 11 bits
    #   different HOST .................... 15 bits
    #   sibling route (/email vs /phone) .. 16 bits
    #   different route ................... 21 bits
    #
    # Note the ordering, which is the whole reason for the low default: collapsing sibling routes
    # would need a radius of 16, but a different METHOD sits at 11 and a different HOST at 15 —
    # so any radius wide enough to merge two sibling pages ALSO merges GET with POST, and merges
    # two different hosts. There is no safe radius above ~10. The default stays deliberately far
    # below it: generated path spaces collapse for free at distance 0, and everything else is
    # worth its own unit.
    simhash_distance: int = 3


# How much wider than the yield cap the traversal is allowed to run. Near-duplicate collapse
# rejects a large share of a modern site's pages (one shape covers a whole generated path space),
# so a traversal cap equal to the yield cap would return a handful of pages and stop; 20x gives
# candidates to actually fill the cap while still bounding a runaway crawl.
_TRAVERSAL_FANOUT = 20


def _header(headers, name: str) -> str:
    return _lower_headers(headers).get(name, "")



def _page_out(result, seed: str, discovery: bool, capture_limit: int = 0) -> dict:
    """Map one crawl4ai CrawlResult to a PageOut record.

    The record must be CONTENT-DETERMINISTIC: the streaming sub-unit id is its sha, so a
    re-crawl (DEFAULT tier) or a resume must reproduce the SAME bytes for the same page. So we
    take only STABLE response fields — status and content-type, which are properties of the
    endpoint — and NEVER volatile ones like Date / Set-Cookie / Age, which would give the same
    page a new id on every fetch (ADR 0015)."""
    depth = int((getattr(result, "metadata", None) or {}).get("depth", 0) or 0)
    md = ""
    if getattr(result, "success", False) and not discovery:
        md = getattr(result, "markdown", "") or ""
        md = getattr(md, "raw_markdown", md)   # MarkdownGenerationResult -> str
    status = getattr(result, "status_code", 0) or 0
    headers = getattr(result, "response_headers", {}) or {}
    links = getattr(result, "links", {}) or {}
    internal = links.get("internal", []) if isinstance(links, dict) else []
    err = (None if getattr(result, "success", False)
           else str(getattr(result, "error_message", "") or "crawl failed"))
    return {"seed": seed, "url": result.url, "depth": depth, "status": status,
            "request": {"method": "GET", "url": result.url},
            "response": {"status": status, "content_type": _header(headers, "content-type")},
            "markdown": str(md),
            "links": sum(1 for link in internal if link.get("href")),
            "requests": _capture_requests(result, seed, capture_limit),
            "error": err}


@actor.defn
class Crawler:
    input = CrawlSeed
    output = PageOut
    params = CrawlParams

    @actor.load
    async def open_browser(self):
        """Open ONE browser for the whole session. Re-runs on a SessionLost rebuild, handing
        the session a fresh browser; a RESUMABLE crawl then resumes from its frontier."""
        from crawl4ai import AsyncWebCrawler, BrowserConfig

        headless = bool(param.get("headless", True))
        # A container / CI host has no X server, so a HEADED browser can't launch — chromium
        # dies with "Missing X server or $DISPLAY" and takes the whole session down. Force
        # headless when there's no DISPLAY so a `headless=false` slip can't crash the crawl.
        if not headless and not os.environ.get("DISPLAY"):
            print("[crawl4ai] headless=false but no DISPLAY — forcing headless", flush=True)
            headless = True
        self.crawler = AsyncWebCrawler(config=BrowserConfig(headless=headless,
                                                           extra_args=list(_CHROME_LEAN_ARGS)))
        await self.crawler.start()
        self._route_pages_to_unit_contexts()

    def _route_pages_to_unit_contexts(self):
        """Point crawl4ai's page allocation at the IN-FLIGHT ARUN's own context (see the note at
        the top of the file). Calls with no Unit bound (crawl4ai's internal fetches) or with an
        explicit session_id fall through to crawl4ai's native resolution, so session /
        kill_session semantics are untouched. Re-installed on every load, so a SessionLost
        rebuild routes the fresh browser too."""
        strategy = getattr(self.crawler, "crawler_strategy", None)
        manager = getattr(strategy, "browser_manager", None)
        if manager is None or not hasattr(manager, "create_browser_context"):
            return            # not the Playwright strategy -> no contexts to isolate
        if getattr(getattr(manager, "config", None), "use_persistent_context", False):
            return            # a persistent profile IS one context by design
        native = manager.get_page

        async def get_page(crawlerRunConfig):
            unit_ctx = _UNIT_CTX.get()
            if unit_ctx is None or unit_ctx.closed or crawlerRunConfig.session_id:
                return await native(crawlerRunConfig)
            return await unit_ctx.page(manager, crawlerRunConfig)

        manager.get_page = get_page

    @actor.healthcheck
    async def browser_alive(self):
        """The framework probes this after any Method failure (and on the beat): a dead browser
        -> raise -> reload; a live one -> report the browser is up."""
        crawler = getattr(self, "crawler", None)
        if crawler is None or not getattr(crawler, "ready", True):
            raise SessionLost("browser gone")
        return True

    @actor.close
    async def close_browser(self):
        crawler = getattr(self, "crawler", None)
        if crawler is not None:
            await crawler.close()

    # One seed per unit; PUSHES every page that is not a near-duplicate (1 -> N). Each pushed
    # page is a durable record keyed by its content sha.
    @actor.method(takes=CrawlSeed, emits=PageOut)
    async def crawl(self, batch, dataset):
        # The author's loop, and it is SEQUENTIAL on purpose: this crawler resumes a seed from
        # `self.unit_state`, which is the Unit the loop is on, and pushes under it. Running seeds
        # on their own tasks (as webcrawl does) would hand them all one slot. Concurrency inside a
        # seed is crawl4ai's own — its DFS walks many pages at once on this seed's BrowserContext.
        async for unit in batch:
            await self._crawl_seed(unit, dataset)

    async def _crawl_seed(self, unit, dataset):
        from crawl4ai import CacheMode, CrawlerRunConfig
        from crawl4ai.deep_crawling import DFSDeepCrawlStrategy

        url = (unit.value.get("url") or "").strip()
        if not valid_url(url):
            await dataset.push(_bad(url))
            return
        # DROP a seed whose host does not resolve. Emitting a record for it would only push a
        # dead URL to the next actor (which would reject it), so the seed is dropped from the
        # data plane entirely and accounted for in the LOG instead — worker logs are durable
        # when KONTRA_LOG_DIR is mounted, so a dropped seed is still auditable after the run.
        if not await _resolves(url):
            print(f"[crawl4ai] dropped seed (host does not resolve): {url}", flush=True)
            return

        discovery = bool(param.get("discovery_mode", False))
        capture_limit = int(param.get("capture_limit", 50)) if bool(param.get("capture_requests", True)) else 0
        distance = max(0, int(param.get("simhash_distance", 3)))
        # RESUMABLE (native frontier wired to unit_state) is safe ONLY when emitted pages are
        # durable at emit-time — an object store is configured (self.emit_durable). In the inline
        # no-S3 mode, fall back to atomic re-crawl (DEFAULT tier), or a resume would skip visited
        # URLs whose pages were never persisted and silently drop them.
        resumable = bool(param.get("resumable", False)) and self.emit_durable

        # max_pages is an EMIT cap, not a traversal cap: the strategy keeps walking so the
        # dedupe gate has candidates, and WE stop the stream once the cap is filled.
        yield_cap = max(1, int(unit.value.get("max_pages", param.get("max_pages", 10))))
        seen = load_seen(await self.unit_state.get("seen"))

        # crawl4ai's NATIVE DFS. RESUMABLE wires its frontier-only state to unit_state (resume
        # across a death); DEFAULT leaves it unwired (re-crawl the whole seed on a death — safe
        # because pages are keyed by content sha, so re-emission overwrites idempotently).
        strategy = DFSDeepCrawlStrategy(
            max_depth=int(param.get("depth", 2)),
            max_pages=yield_cap * _TRAVERSAL_FANOUT,
            resume_state=(await self.unit_state.get("frontier")) if resumable else None,
            on_state_change=(lambda s: self.unit_state.set("frontier", s)) if resumable else None,
        )
        cfg_kw = dict(deep_crawl_strategy=strategy, cache_mode=CacheMode.BYPASS,
                      stream=True, verbose=False)
        if capture_limit:
            cfg_kw["capture_network_requests"] = True
        try:
            cfg = CrawlerRunConfig(**cfg_kw)
        except TypeError:
            # Older crawl4ai without capture_network_requests — crawl anyway, emit no requests.
            print("[crawl4ai] installed crawl4ai lacks capture_network_requests",
                  flush=True)
            capture_limit = 0
            cfg_kw.pop("capture_network_requests", None)
            cfg = CrawlerRunConfig(**cfg_kw)
        # THIS seed's own BrowserContext for the whole crawl (see the note at the top of the
        # file). Bound task-locally and never reset: every seed binds its own on entry, so a
        # stale slot can never be observed.
        unit_ctx = _ArunContext()
        _UNIT_CTX.set(unit_ctx)
        stream, crawled, yielded = None, 0, 0
        try:
            stream = await self.crawler.arun(url, config=cfg)
            async for result in stream:
                crawled += 1
                record = _page_out(result, url, discovery, capture_limit)
                # The ONE reason a crawled page is not emitted: its request shape is within
                # `distance` bits of a shape already emitted on this seed. Nothing else is
                # filtered — no status gate, no cache gate, no static-asset guess.
                fingerprint = request_simhash(fingerprint_request(record))
                if near_duplicate(fingerprint, seen, distance):
                    continue
                remember(seen, fingerprint)
                if resumable:
                    # Persisted alongside the frontier and only in the RESUMABLE tier: the
                    # DEFAULT tier leaves unit_state unwired by design and re-derives the set on
                    # a re-crawl, which is safe because a re-push is an idempotent overwrite.
                    await self.unit_state.set("seen", dump_seen(seen))
                await dataset.push(record)
                yielded += 1
                if yielded >= yield_cap:
                    break
        except SessionLost:
            raise
        except Exception as e:  # noqa: BLE001
            if any(k in str(e).lower() for k in _BROWSER_DEAD):
                raise SessionLost(f"browser lost crawling {url}: {e}") from e
            raise  # live resource -> the framework isolates this seed as a per-unit failure
        finally:
            # Close the stream EXPLICITLY on the cap break: crawl4ai keeps traversing until its
            # generator is closed, so relying on GC would let a finished seed keep fetching.
            if stream is not None and hasattr(stream, "aclose"):
                with contextlib.suppress(Exception):
                    await stream.aclose()
            # Release the seed's context the moment its crawl ends — a 100-seed batch must never
            # accumulate 100 contexts.
            await unit_ctx.close()
            # The dedupe rate is the number to watch when tuning simhash_distance, and it is
            # invisible in the data plane (suppressed pages leave no record) — so log it.
            print(f"[crawl4ai] {url}: yielded {yielded} of {crawled} crawled", flush=True)


if __name__ == "__main__":
    actor.serve()
