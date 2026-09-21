"""reddit (browser transport) — only what is transport-specific.

The taxonomy, search paging, comment tree and row builders are tested once, in
examples/private/reddit-core/test_redditcore.py. This file covers the Camoufox half: the warm-up
canary and how a refusal is classified.

    .venv/bin/python -m pytest examples/private/reddit-scrapper/ -q
"""

import asyncio
import sys
from pathlib import Path

import pytest

sys.path[:0] = [str(Path(__file__).resolve().parents[3]), str(Path(__file__).resolve().parent)]

import kontra  # noqa: E402

# ONE PROCESS PER EXAMPLE DIRECTORY. The actor registry is a process-global singleton —
# one actor per worker is the production shape — so importing two example actors into one
# pytest session raises `@actor.method name 'discover' declared twice`. pyproject.toml
# records that decision and `make test-examples` gives each directory its own process.
# Run this file with its own directory, never with the sibling actor's.
import actor as rd  # noqa: E402

METHODS = dict(kontra.actor.methods)
ACTOR_CLASS = kontra.actor.actor_class

_lib = sys.modules[kontra.actor.__class__.__module__]


@pytest.fixture(autouse=True)
def _bound_params():
    # page_pause is real pacing against Reddit, not behaviour under test — leaving it at its
    # 5s default put 10 seconds of sleep into this file.
    token = _lib._run_params.set({"page_pause": 0.0})
    yield
    _lib._run_params.reset(token)


class FakePage:
    """The three things _warmup and _fetch_json touch on a Playwright page."""

    def __init__(self, text, title, fetch=None):
        self._text, self._title, self._fetch = text, title, fetch
        self.url = "https://old.reddit.com/"

    async def goto(self, *a, **k):
        return None

    async def title(self):
        return self._title

    async def evaluate(self, script, arg=None):
        if "document.body.innerText" in script:
            return self._text
        return self._fetch


def _inst(page):
    i = rd.Reddit()
    i.page = page
    i._compile_taxonomy()
    return i


# --- registration ---------------------------------------------------------------------------


def test_it_registers_the_same_two_operations_as_the_api_actor():
    # The two transports must be drop-in for each other: the workflow names an actor and calls
    # `discover`/`harvest` by name, so a divergence here is a broken swap, not a new feature.
    assert set(METHODS) == {"discover", "harvest"}
    assert METHODS["discover"].takes is rd.Target and METHODS["discover"].emits is rd.Post
    assert METHODS["harvest"].takes is rd.Post and METHODS["harvest"].emits is rd.Document
    assert ACTOR_CLASS.BASE == "https://old.reddit.com"
    assert ACTOR_CLASS.JSON_SUFFIX == ".json"


# --- the warm-up canary -----------------------------------------------------------------------


def test_the_real_block_page_is_caught_at_the_homepage():
    # MEASURED: 797 chars, <title>Blocked</title>, "network policy". Neither of the prototype's
    # two heuristics fires — the title is what catches it, before any request is burned.
    page = FakePage("whoa there, pardner!\n\nYour request has been blocked due to a network "
                    "policy." + "x" * 700, "Blocked")
    with pytest.raises(kontra.SessionLost) as e:
        asyncio.run(_inst(page)._warmup())
    assert "blocked" in str(e.value).lower()


def test_a_suspiciously_short_homepage_is_also_refused():
    with pytest.raises(kontra.SessionLost):
        asyncio.run(_inst(FakePage("tiny", "reddit"))._warmup())


def test_a_healthy_homepage_with_a_blocked_json_endpoint_still_fails():
    # The whole reason the .json probe exists: a block can refuse every .json endpoint while
    # ordinary browsing succeeds, and the HTML check alone hands out a green light.
    page = FakePage("normal reddit content " * 60, "reddit",
                    fetch={"status": 403, "body": "nope"})
    with pytest.raises(kontra.SessionLost) as e:
        asyncio.run(_inst(page)._warmup())
    assert "probe blocked" in str(e.value)


def test_a_healthy_session_warms_up_clean():
    page = FakePage("normal reddit content " * 60, "reddit",
                    fetch={"status": 200, "body": '{"data": {"children": []}}'})
    asyncio.run(_inst(page)._warmup())          # no raise


# --- refusal classification ---------------------------------------------------------------------


def test_a_403_is_blocked_not_a_transport_error():
    page = FakePage("x", "reddit", fetch={"status": 403, "body": "denied"})
    with pytest.raises(rd.Blocked):
        asyncio.run(_inst(page)._fetch_json("https://old.reddit.com/x.json"))


def test_a_200_carrying_the_block_page_is_also_blocked():
    # The block arrives as a 200 about as often as a 403; status alone is not the signal.
    page = FakePage("x", "reddit",
                    fetch={"status": 200, "body": "whoa there, pardner! blocked due to a "
                                                  "network policy"})
    with pytest.raises(rd.Blocked):
        asyncio.run(_inst(page)._fetch_json("https://old.reddit.com/x.json"))


def test_a_non_json_body_is_none_not_an_exception():
    # An HTML interstitial that is not a block is a bad response, not a dead session — the caller
    # treats None as "no data from this page" and moves on.
    page = FakePage("x", "reddit", fetch={"status": 200, "body": "<html>maintenance</html>"})
    assert asyncio.run(_inst(page)._fetch_json("https://old.reddit.com/x.json")) is None
