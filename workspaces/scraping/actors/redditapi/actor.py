"""redditapi — the OAUTH transport: a bearer token and plain HTTPS, no browser.

TWO ACTORS, ONE CORE. Everything about Reddit that is not transport lives in `redditcore.py`,
a symlink shared with the `reddit` (Camoufox) actor. This file is the OAuth half: how a token is
minted, how one document is fetched, and how the token proves itself before any work starts.

WHY THIS ONE EXISTS, AND WHAT WAS MEASURED. From a DigitalOcean Droplet — three brand-new ones,
sfo3/nyc3/ams3, 2026-08-24 — every Reddit DATA path answers 403 with the same 1522-byte "Blocked"
page: oauth.reddit.com, www/api/old .json, even the RSS feed. Exactly one path is exempt:

    POST www.reddit.com/api/v1/access_token  ->  401 on bad credentials, i.e. REACHABLE

Reddit is not banning the address space; it is banning data traffic that does not carry a token it
accepts, and leaving open the endpoint you need in order to obtain one. Those probes all carried
`Authorization: bearer FAKE`. Whether a VALID token clears the edge is the one thing that could not
be tested without a credential — and it is precisely what this actor supplies. That is the bet this
file makes, and the reason it, not the browser, is what the workflow now defaults to.

Rotating the Droplet does NOT substitute for the token: a fresh IP per crawl was measured above and
is blocked identically. A fresh IP is still worth having — it is what `fleet.up` gives for free, and
it keeps one campaign's reputation off the next one's — but it is not the lever.

It is also dramatically lighter than the browser: no Firefox, no 150 MB browser fetch, no warm-up,
no per-request 5s pacing — the ceiling becomes Reddit's rate limit rather than a browser's.

WHAT IT COSTS YOU. Credentials, and a decision. Reddit's free tier is 100 queries/minute per
OAuth client and is scoped to NON-COMMERCIAL use; client work needs a commercial agreement with
Reddit. That is a contractual question, not a technical one, and it is the reason both transports
exist rather than this one replacing the other.

CREDENTIALS. Create a "script" app at https://www.reddit.com/prefs/apps, then pass:

    client_id / client_secret   the app's own credentials
    username / password         the Reddit account the script acts as
    user_agent                  REQUIRED and must be descriptive; a generic or empty one is
                                itself a documented cause of blocks

They are Method params rather than image env, so a campaign can change identity without a rebuild
and the credential is a property of the Run — which is where the audit trail already is.

Run it:  kontra serve --actor examples/private/reddit-api
"""

import asyncio
import base64
import time
from dataclasses import dataclass
from typing import Optional

from kontra import NonRetryableError, SessionLost, actor, param
from redditcore import (
    Blocked, Document, Post, RedditCore, Target, _is_block_page,
    _AMBIGUOUS, _CONTEXT_WORDS, _INTENT_KEYWORDS, _KEYWORDS, _PLATFORM_KEYWORDS, _TOPIC_KEYWORDS,
)

TOKEN_URL = "https://www.reddit.com/api/v1/access_token"

# Reddit hands back its remaining allowance on every response. Reading it is what lets this actor
# pace itself against the real limit instead of a guessed sleep — the browser transport cannot,
# because a page.evaluate() fetch does not surface response headers.
_RL_REMAINING = "x-ratelimit-remaining"
_RL_RESET = "x-ratelimit-reset"


@dataclass
class ApiParams:
    """Same tuning block as the browser transport where it means the same thing, minus everything
    that was about driving a browser."""

    # credentials
    client_id: str = ""
    client_secret: str = ""
    username: str = ""
    password: str = ""
    # Reddit asks for "platform:app-id:version (by /u/name)". A generic UA is a documented cause
    # of blocks, so there is no sensible default and an empty one is refused at load.
    user_agent: str = ""

    # discover
    time_filter: str = "year"
    min_post_score: int = 2
    max_posts_per_sub: int = 8
    search_pages: int = 3

    # harvest
    top_fraction: float = 1.0
    comment_limit: int = 500
    expand_more: bool = True
    max_more_requests: int = 5

    # pacing. 0.7s ~= 85 req/min, just inside the documented 100/min per OAuth client. The
    # browser transport needs 5.0 because it is pretending to be a person; this one is a declared
    # client and may go as fast as the limit it is told about.
    page_pause: float = 0.7
    fetch_attempts: int = 3
    search_attempts: int = 5
    fail_cooldown: float = 60.0
    fails_before_cooldown: int = 2
    # Below this many remaining calls, sleep until the window resets rather than earn a 429.
    ratelimit_floor: int = 5

    # taxonomy
    keywords: str = _KEYWORDS
    ambiguous: str = _AMBIGUOUS
    platform_keywords: str = _PLATFORM_KEYWORDS
    intent_keywords: str = _INTENT_KEYWORDS
    topic_keywords: str = _TOPIC_KEYWORDS
    context_words: str = _CONTEXT_WORDS
    context_window: int = 120



@actor.defn
class RedditApi(RedditCore):
    params = ApiParams

    BASE = "https://oauth.reddit.com"
    JSON_SUFFIX = ""                   # the OAuth host serves JSON at bare paths

    # ---- session ---------------------------------------------------------------------------

    @actor.load
    async def open_client(self):
        """Mint a bearer token and prove it, once per session.

        The probe is the same idea as the browser transport's warm-up and for the same reason: a
        token that cannot read a subreddit is a dead session, not a bad unit, and finding out here
        costs one call instead of a batch of doomed ones.
        """
        import httpx                                   # the actor's own dep, imported late

        cid = str(param.get("client_id", "") or "")
        secret = str(param.get("client_secret", "") or "")
        ua = str(param.get("user_agent", "") or "")
        if not cid or not secret:
            # NonRetryableError, not SessionLost: a reload mints the same missing credential.
            raise NonRetryableError("client_id and client_secret are required for redditapi")
        if not ua:
            raise NonRetryableError(
                "user_agent is required — Reddit blocks generic and empty ones")

        self._client = httpx.AsyncClient(
            timeout=httpx.Timeout(30.0), headers={"User-Agent": ua})
        self._fail_streak = 0
        self._rl_remaining = None
        self._rl_reset = 0.0
        self._compile_taxonomy()
        await self._mint_token()

        # Prove it. A 401 here means the credentials are wrong, which no retry fixes.
        probe = await self._fetch_json(f"{self.BASE}/r/test/new?limit=1")
        if probe is None:
            raise SessionLost("token minted but /r/test/new did not parse")

    async def _mint_token(self):
        """client_credentials would give an APP-only token, which cannot read some endpoints;
        `password` grant is what a Reddit "script" app is for and is what the account's own rate
        limit attaches to."""
        import httpx

        basic = base64.b64encode(
            f"{param.get('client_id','')}:{param.get('client_secret','')}".encode()).decode()
        try:
            res = await self._client.post(
                TOKEN_URL,
                data={"grant_type": "password",
                      "username": str(param.get("username", "") or ""),
                      "password": str(param.get("password", "") or "")},
                headers={"Authorization": f"Basic {basic}"},
            )
        except httpx.HTTPError as e:
            raise SessionLost(f"token endpoint unreachable: {e}") from e

        if res.status_code in (401, 403):
            raise NonRetryableError(
                f"reddit refused the credentials (HTTP {res.status_code}) — check the app type "
                f"is 'script' and the account owns it")
        if res.status_code != 200:
            raise SessionLost(f"token mint failed (HTTP {res.status_code})")

        tok = res.json().get("access_token")
        if not tok:
            raise SessionLost("token response carried no access_token")
        self._client.headers["Authorization"] = f"bearer {tok}"
        # Reddit's tokens last an hour; refresh early rather than discover expiry mid-batch.
        self._token_expires = time.time() + int(res.json().get("expires_in", 3600)) - 300

    @actor.healthcheck
    async def token_alive(self):
        """Probed after any Method failure and on the beat. Reports the REAL allowance, so a
        session throttled by Reddit is visible in the Dashboard rather than merely slow."""
        if getattr(self, "_client", None) is None:
            raise RuntimeError("no http client")
        return {"rl_remaining": self._rl_remaining, "fail_streak": self._fail_streak}

    @actor.close
    async def close_client(self):
        client = getattr(self, "_client", None)
        if client is not None:
            try:
                await client.aclose()
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

    async def _respect_ratelimit(self):
        """Sleep until the window resets when the allowance is nearly spent.

        Reddit reports the remaining calls on every response, so this paces against the real
        number instead of a guess. Earning a 429 is worse than waiting for it: the reset is the
        same either way and the 429 also counts.
        """
        floor = int(param.get("ratelimit_floor", 5))
        if self._rl_remaining is not None and self._rl_remaining <= floor:
            wait = max(0.0, self._rl_reset - time.time())
            if wait > 0:
                await asyncio.sleep(min(wait + 1.0, 120.0))
            self._rl_remaining = None

    async def _fetch_json(self, url: str, attempts: Optional[int] = None):
        """One authenticated GET. Same contract as the browser transport's: parsed JSON, None if
        the body is not JSON, Blocked when Reddit refuses us.

        Deliberately does NOT import httpx: the client is injected on `self`, and the only thing
        the library was wanted for here was raising a retry signal. Keeping it out means this
        method — the one with all the classification logic in it — is testable against any
        object with a `.get`.
        """
        tries = int(attempts if attempts is not None else param.get("fetch_attempts", 3))

        # A batch can outlive an hour-long token; refresh before the call rather than let a 401
        # cost a subreddit.
        if time.time() >= getattr(self, "_token_expires", 0):
            await self._mint_token()

        res = None
        for attempt in range(tries):
            try:
                await self._respect_ratelimit()
                res = await self._client.get(url)

                rem = res.headers.get(_RL_REMAINING)
                if rem is not None:
                    try:
                        self._rl_remaining = int(float(rem))
                        self._rl_reset = time.time() + float(res.headers.get(_RL_RESET, 0) or 0)
                    except ValueError:
                        pass

                if res.status_code == 401:
                    # Expired or revoked mid-batch. One in-place refresh, then let the ladder run.
                    await self._mint_token()
                    raise RuntimeError("401 — token refreshed, retrying")
                if res.status_code in (403, 429) or _is_block_page(res.text):
                    raise Blocked(f"reddit refused (HTTP {res.status_code})")
                res.raise_for_status()
                break
            except Blocked:
                raise
            except Exception:
                if attempt == tries - 1:
                    raise
                await asyncio.sleep(2 * (attempt + 1))

        # Far shorter than the browser transport's 5s: a declared OAuth client is allowed to go
        # at the rate Reddit tells it, and _respect_ratelimit is what actually enforces the cap.
        await asyncio.sleep(float(param.get("page_pause", 0.7)))
        try:
            return res.json()
        except ValueError:
            return None


if __name__ == "__main__":
    actor.serve()
