"""redditapi (OAuth transport) — only what is transport-specific.

Shared logic is tested once in examples/private/reddit-core/test_redditcore.py. This file covers
the OAuth half: credential validation, token refresh, rate-limit accounting, and how a refusal is
classified.

    .venv/bin/python -m pytest examples/private/reddit-api/ -q
"""

import asyncio
import sys
import time
from pathlib import Path

import pytest

sys.path[:0] = [str(Path(__file__).resolve().parents[3]), str(Path(__file__).resolve().parent)]

import kontra  # noqa: E402

# ONE PROCESS PER EXAMPLE DIRECTORY — see the twin comment in the browser actor's tests
# and the rationale recorded in pyproject.toml's [tool.pytest.ini_options].
import actor as ra  # noqa: E402

METHODS = dict(kontra.actor.methods)
ACTOR_CLASS = kontra.actor.actor_class

_lib = sys.modules[kontra.actor.__class__.__module__]


@pytest.fixture(autouse=True)
def _bound_params():
    token = _lib._run_params.set({"page_pause": 0.0})
    yield
    _lib._run_params.reset(token)


class FakeResponse:
    def __init__(self, status=200, payload=None, text="", headers=None):
        self.status_code, self._payload = status, payload
        self.text = text
        self.headers = headers or {}

    def json(self):
        if self._payload is None:
            raise ValueError("not json")
        return self._payload

    def raise_for_status(self):
        if self.status_code >= 400:
            raise RuntimeError(f"HTTP {self.status_code}")


class FakeClient:
    def __init__(self, responses):
        self.responses = list(responses)
        self.headers = {}
        self.gets = []

    async def get(self, url):
        self.gets.append(url)
        r = self.responses.pop(0) if self.responses else FakeResponse(200, {})
        if isinstance(r, Exception):
            raise r
        return r


def _inst(client=None, expires_ahead=3600):
    i = ra.RedditApi()
    i._client = client or FakeClient([])
    i._fail_streak = 0
    i._rl_remaining = None
    i._rl_reset = 0.0
    i._token_expires = time.time() + expires_ahead
    i.run_id = "r"
    i._compile_taxonomy()
    return i


# --- registration ---------------------------------------------------------------------------


def test_it_registers_the_same_two_operations_as_the_browser_actor():
    # The two transports must be drop-in for each other — the workflow names an actor and calls
    # these by name, so a divergence is a broken swap.
    assert set(METHODS) == {"discover", "harvest"}
    assert METHODS["discover"].takes is ra.Target and METHODS["discover"].emits is ra.Post
    assert METHODS["harvest"].takes is ra.Post and METHODS["harvest"].emits is ra.Document


def test_it_targets_the_oauth_host_with_no_json_suffix():
    # oauth.reddit.com serves JSON at bare paths; appending .json there 404s every call.
    assert ACTOR_CLASS.BASE == "https://oauth.reddit.com"
    assert ACTOR_CLASS.JSON_SUFFIX == ""


# --- credentials -------------------------------------------------------------------------------


def test_missing_credentials_fail_terminally_not_as_a_lost_session():
    # SessionLost would reload and mint the SAME missing credential, forever. This has to be
    # terminal or a fleet spends its whole batch rediscovering an empty config.
    _lib._run_params.set({})
    with pytest.raises(kontra.NonRetryableError):
        asyncio.run(ra.RedditApi.__dict__["open_client"](ra.RedditApi()))


def test_an_empty_user_agent_is_refused_before_any_request():
    # A generic or empty UA is a documented cause of Reddit blocks, so this is refused locally
    # rather than discovered as a 403 mid-batch.
    _lib._run_params.set({"client_id": "a", "client_secret": "b", "user_agent": ""})
    with pytest.raises(kontra.NonRetryableError) as e:
        asyncio.run(ra.RedditApi.__dict__["open_client"](ra.RedditApi()))
    assert "user_agent" in str(e.value)


# --- refusal classification -----------------------------------------------------------------------


def test_a_403_is_blocked():
    with pytest.raises(ra.Blocked):
        asyncio.run(_inst(FakeClient([FakeResponse(403)]))._fetch_json("https://x/y"))


def test_a_429_is_blocked_too():
    # Rate-limited is not "one bad unit": marching on earns more 429s. Blocked -> SessionLost
    # makes the batch back off through a reload instead.
    with pytest.raises(ra.Blocked):
        asyncio.run(_inst(FakeClient([FakeResponse(429)]))._fetch_json("https://x/y"))


def test_a_200_carrying_the_block_page_is_blocked():
    body = "whoa there, pardner! blocked due to a network policy"
    with pytest.raises(ra.Blocked):
        asyncio.run(_inst(FakeClient([FakeResponse(200, None, body)]))._fetch_json("https://x/y"))


def test_a_non_json_body_is_none_not_an_exception():
    got = asyncio.run(_inst(FakeClient([FakeResponse(200, None, "<html>")]))._fetch_json("https://x/y"))
    assert got is None


def test_a_good_response_returns_parsed_json():
    got = asyncio.run(_inst(FakeClient([FakeResponse(200, {"ok": True})]))._fetch_json("https://x/y"))
    assert got == {"ok": True}


# --- rate limiting ----------------------------------------------------------------------------------


def test_the_remaining_allowance_is_read_off_every_response():
    # Reddit reports this on every call. Reading it is what lets the actor pace against the real
    # number rather than a guessed sleep — and it is what the healthcheck surfaces.
    i = _inst(FakeClient([FakeResponse(200, {"ok": 1},
                                       headers={"x-ratelimit-remaining": "42.0",
                                                "x-ratelimit-reset": "30"})]))
    asyncio.run(i._fetch_json("https://x/y"))
    assert i._rl_remaining == 42


def test_a_nearly_spent_allowance_sleeps_until_the_window_resets():
    # Earning a 429 is strictly worse than waiting: the reset is the same either way, and the
    # 429 also counts against you.
    i = _inst()
    i._rl_remaining, i._rl_reset = 1, time.time() + 0.05
    t0 = time.monotonic()
    asyncio.run(i._respect_ratelimit())
    assert time.monotonic() - t0 >= 0.05
    assert i._rl_remaining is None            # cleared, so the next response re-reads it


def test_a_healthy_allowance_does_not_sleep():
    i = _inst()
    i._rl_remaining, i._rl_reset = 500, time.time() + 600
    t0 = time.monotonic()
    asyncio.run(i._respect_ratelimit())
    assert time.monotonic() - t0 < 0.05
