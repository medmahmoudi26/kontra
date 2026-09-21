"""reddit — the BROWSER transport: one Camoufox per session, reading old.reddit.com's .json.

TWO ACTORS, ONE CORE. Everything about Reddit that is not transport — the taxonomy, the search
paging, the comment tree, the "load more" expansion, the row builders and both Method bodies —
lives in `redditcore.py`, which is a symlink shared with the `redditapi` actor. This file is the
browser half and nothing else: how a session is opened, how one document is fetched, and how the
session proves it is not blocked.

WHICH ONE TO RUN — `redditapi`, on a fleet. This transport needs no Reddit credentials and no
commercial agreement, and it reads exactly what a logged-out human sees, which makes it the right
tool from a machine whose address Reddit will serve. A DigitalOcean Droplet is not that machine:
measured 2026-08-24 across three brand-new Droplets in sfo3, nyc3 and ams3, every anonymous data
path — old/www/api .json and the RSS feed alike — answers 403 with the same 1522-byte block page.
A fresh IP per crawl does not change it; that was the measurement, not a guess.

There is nothing this file can do about that, because what it lacks is the thing the block is
keyed on: a token. Run it from an address Reddit serves, or run `redditapi` instead.

WHY THE SESSION IS ONE BROWSER, SEQUENTIALLY. webcrawl runs `parallel_seeds` BrowserContexts at
once. This must not. The whole anti-block design is one warmed page on one host issuing paced,
same-origin fetches — that is what makes Reddit serve JSON at all. Concurrency breaks the pacing,
and a second context is a second, colder identity behind the same IP.

Run it:  kontra serve --actor examples/private/reddit-scrapper
"""

import asyncio
import json
import os
from typing import Optional

from kontra import SessionLost, actor, param
from redditcore import (
    Blocked, Document, Post, RedditCore, Target, _is_block_page,
    _AMBIGUOUS, _CONTEXT_WORDS, _INTENT_KEYWORDS, _KEYWORDS, _PLATFORM_KEYWORDS, _TOPIC_KEYWORDS,
)

# Warm up on the SAME host we scrape — cf_clearance cookies are host-scoped, so warming
# www.reddit.com and then requesting old.reddit.com was relying on luck.
WARMUP_URL = "https://old.reddit.com/"

# Where deploy.sh installs the browser, on both Targets. Mirrors webcrawl's _SYSTEM_BROWSERS.
_SYSTEM_CACHE = "/opt/kontra-cache"

# Camoufox generates a fresh fingerprint per launch, so one launch = one identity. That is exactly
# the granularity @actor.load gives us: a SessionLost reload IS a new identity, which is why
# Blocked maps to it and to nothing else.
CAMOUFOX_OPTS = {
    "headless": True,
    "persistent_context": True,
    "user_data_dir": "./camoufox_profile",
    "block_images": True,       # we only read JSON
    "block_webrtc": True,       # nothing here needs it, and it is one more identity signal
    "humanize": False,          # nothing to click — cursor sim is pure latency
    "os": "windows",
    "geoip": True,
    "i_know_what_im_doing": True,
}


from dataclasses import dataclass  # noqa: E402  (below the constants, for reading order)


@dataclass
class ScrapeParams:
    """The prototype's tuning block, per-run instead of per-edit. Every default is its measured
    value. The taxonomy is comma-separated so it travels as a scalar and a campaign can be
    re-aimed without touching the image."""

    # discover
    time_filter: str = "year"          # hour|day|week|month|year|all
    min_post_score: int = 2
    max_posts_per_sub: int = 8
    search_pages: int = 3

    # harvest
    top_fraction: float = 1.0
    comment_limit: int = 500
    expand_more: bool = True
    max_more_requests: int = 5

    # pacing. ~10 req/min is the documented unauthenticated ceiling; at 3.0 we measured Reddit's
    # edge dropping connections outright deep into a run.
    page_pause: float = 5.0
    fetch_attempts: int = 3
    search_attempts: int = 5
    fail_cooldown: float = 60.0
    fails_before_cooldown: int = 2

    # taxonomy
    keywords: str = _KEYWORDS
    ambiguous: str = _AMBIGUOUS
    platform_keywords: str = _PLATFORM_KEYWORDS
    intent_keywords: str = _INTENT_KEYWORDS
    topic_keywords: str = _TOPIC_KEYWORDS
    context_words: str = _CONTEXT_WORDS
    context_window: int = 120

    # browser + egress
    headless: bool = True


@actor.defn
class Reddit(RedditCore):
    params = ScrapeParams

    BASE = "https://old.reddit.com"
    JSON_SUFFIX = ".json"              # the public host serves JSON at .json paths

    # ---- session ---------------------------------------------------------------------------

    @actor.load
    async def open_browser(self):
        """ONE Camoufox for the whole session, warmed once.

        The warm-up is part of load, not of the first unit, because a browser that cannot reach a
        `.json` endpoint is a dead session, not a bad subreddit — and finding that out here costs
        one probe instead of a subreddit's worth of doomed requests.
        """
        # WHERE deploy.sh PUT THE BROWSER. Camoufox resolves its install dir through platformdirs
        # ($XDG_CACHE_HOME, else $HOME/.cache) and takes no env var of its own, so a build user
        # and a run user with different HOMEs disagree about where Firefox is — webcrawl pins
        # PLAYWRIGHT_BROWSERS_PATH for exactly this.
        if not os.environ.get("XDG_CACHE_HOME") and os.path.isdir(_SYSTEM_CACHE):
            os.environ["XDG_CACHE_HOME"] = _SYSTEM_CACHE

        from camoufox.async_api import AsyncCamoufox      # the actor's own dep, imported late

        opts = dict(CAMOUFOX_OPTS)
        opts["headless"] = bool(param.get("headless", True))

        self._cm = AsyncCamoufox(**opts)
        self.browser = await self._cm.__aenter__()
        pages = getattr(self.browser, "pages", [])        # persistent_context may own one
        self.page = pages[0] if pages else await self.browser.new_page()
        self._fail_streak = 0
        self._compile_taxonomy()
        await self._warmup()

    @actor.healthcheck
    async def browser_alive(self):
        """Probed after any Method failure and on the beat. A dead Camoufox takes the warmed
        session with it, and a cold browser's first `.json` request is blocked every time."""
        page = getattr(self, "page", None)
        if page is None or page.is_closed():
            raise RuntimeError("page gone")
        return {"url": page.url, "fail_streak": self._fail_streak}

    @actor.close
    async def close_browser(self):
        cm = getattr(self, "_cm", None)
        if cm is not None:
            try:
                await cm.__aexit__(None, None, None)
            except Exception:
                pass                                       # a close that fails is still closed

    # ---- methods (bodies in redditcore) ------------------------------------------------------

    @actor.method(takes=Target, emits=Post)
    async def discover(self, batch, dataset):
        """Search one subreddit, emit the posts worth harvesting."""
        await self._run_discover(batch, dataset)

    @actor.method(takes=Post, emits=Document)
    async def harvest(self, batch, dataset):
        """Fetch one thread, emit the post row and every comment row."""
        await self._run_harvest(batch, dataset)

    # ---- transport ----------------------------------------------------------------------------

    async def _host_page(self):
        """Park the browser on a real Reddit HTML page and keep it there. Everything is fetched
        FROM this page rather than by navigating to it — the page is the origin the requests come
        from, so it has to be loaded and on the same host."""
        if self.page.url.startswith(WARMUP_URL):
            return
        await self.page.goto(WARMUP_URL, wait_until="load", timeout=60000)
        await asyncio.sleep(4)

    async def _warmup(self):
        """Load Reddit normally, then prove a `.json` request actually works.

        The HTML check alone is not a canary: a block can refuse every `.json` endpoint while
        ordinary browsing still succeeds — measured, and it made the old warm-up hand out a green
        light seconds before the run died.
        """
        await self._host_page()
        txt = await self.page.evaluate("() => document.body.innerText")
        title = ((await self.page.title()) or "").strip().lower()
        # The TITLE is the reliable tell — the body wording has changed once already and the
        # block page is comfortably longer than the 500-char floor.
        if _is_block_page(txt) or title == "blocked" or len(txt) < 500:
            raise SessionLost(
                f"homepage blocked (title={title!r}, {len(txt)} chars) — not burning requests")
        try:
            probe = await self._fetch_json("https://old.reddit.com/r/test/new.json?limit=1")
        except Blocked as e:
            # A block DURING warm-up is still a spent identity, and @actor.load raising a bare
            # Blocked would isolate rather than relaunch.
            raise SessionLost(f"warm-up probe blocked: {e}") from e
        if probe is None:
            raise SessionLost("homepage loads but .json does not parse")

    async def _fetch_json(self, url: str, attempts: Optional[int] = None):
        """Fetch a `.json` endpoint from inside a loaded Reddit page.

        NOT page.goto(). Measured against a live block, same browser and session:

            goto .json, cold                        BLOCKED
            goto homepage, then goto .json          BLOCKED
            goto the sub's html, then goto .json    BLOCKED
            sub's html, then in-page fetch()        200, real JSON
            .rss instead of .json                   BLOCKED
            www.reddit.com instead of old.          BLOCKED

        Navigating the main document to a JSON file announces itself with Sec-Fetch-Dest:
        document / Mode: navigate / Site: none — a combination no human produces. A fetch() from
        an already-loaded page sends Dest: empty, Site: same-origin and a Referer, which is what
        Reddit's own front-end does. Same URL, same browser, same cookies; only this one works.
        """
        tries = int(attempts if attempts is not None else param.get("fetch_attempts", 3))
        body = None
        for attempt in range(tries):
            try:
                await self._host_page()               # must be ON reddit to fetch from it
                res = await self.page.evaluate(
                    """async (u) => {
                        const r = await fetch(u, {credentials: 'include'});
                        return {status: r.status, body: await r.text()};
                    }""", url)
                body = res["body"]
                if res["status"] == 403 or _is_block_page(body):
                    raise Blocked(f"reddit block (HTTP {res['status']})")
                break
            except Blocked:
                raise
            except Exception:
                if attempt == tries - 1:
                    raise
                # A connection reset is not a block, and one blip must not cost a subreddit.
                await asyncio.sleep(20 * (attempt + 1))

        await asyncio.sleep(float(param.get("page_pause", 5.0)))
        try:
            return json.loads(body)
        except (json.JSONDecodeError, TypeError):
            return None


if __name__ == "__main__":
    actor.serve()
