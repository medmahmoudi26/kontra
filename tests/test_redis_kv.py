"""RedisEtagKV against a REAL Redis.

`tests/test_global_state.py` already proves GlobalStore's CAS retry logic against a fake KV.
What a fake cannot prove is that the *adapter* is genuinely atomic — that is a property of the
Lua script running server-side, and a fake that simply does what it is told would pass while
the real thing lost updates. So these run against the live Redis and skip when there is none.

The concurrency test is the one that matters: it is the lost-update race the whole EtagKV seam
exists to prevent.

RUN THESE. They skip without a reachable Redis, and compose does NOT publish it on localhost —
point them at the store explicitly:

    KONTRA_REDIS_HOST=10.124.0.2:6379 .venv/bin/python -m pytest tests/test_redis_kv.py

They were also marked `@pytest.mark.asyncio` without pytest-asyncio installed, which meant that
even WITH a Redis the async bodies never executed — the marker was unknown and pytest declined
to run them. Two independent ways to look green while testing nothing, on the adapter that owns
every atomic in the tier. Hence `@sync` below: no plugin, no way to silently no-op.
"""

from __future__ import annotations

import asyncio
import functools
import os
import sys
import uuid

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "runtime", "python"))

from internals.globalstore import GlobalStore  # noqa: E402
from internals.redis_kv import RedisEtagKV  # noqa: E402

REDIS_HOST = os.environ.get("KONTRA_REDIS_HOST", "localhost:6379")


def _factory():
    import redis.asyncio as aioredis

    host, _, port = REDIS_HOST.partition(":")
    return aioredis.Redis(host=host or "localhost", port=int(port or 6379), db=0)


async def _reachable() -> bool:
    try:
        c = _factory()
        await c.ping()
        await c.aclose()
        return True
    except Exception:
        return False


pytestmark = pytest.mark.skipif(
    not asyncio.run(_reachable()),
    reason=f"no Redis at {REDIS_HOST} (set KONTRA_REDIS_HOST to the controller's)",
)


def sync(fn):
    """Run an async test body on its own loop. The repo has no pytest-asyncio, and a bare
    `async def test_` is silently not-run rather than failed."""

    @functools.wraps(fn)
    def wrapper(*a, **kw):
        async def body():
            try:
                return await fn(*a, **kw)
            finally:
                # The adapter opens its client lazily on first use, inside THIS loop. Close it
                # here too: a fixture teardown would run on a fresh loop and fail, and leaving
                # it open surfaces as an unraisable "Event loop is closed" blamed on whichever
                # test happens to run next.
                for arg in (*a, *kw.values()):
                    client = getattr(arg, "_c", None)
                    if client is not None:
                        await client.aclose()

        return asyncio.run(body())

    return wrapper


@pytest.fixture
def kv():
    return RedisEtagKV(_factory)


@pytest.fixture
def key():
    return f"kontra-test:{uuid.uuid4()}"


@sync
async def test_absent_key_reads_as_none_with_empty_etag(kv, key):
    data, etag = await kv.get(key)
    assert data is None
    assert etag == ""


@sync
async def test_create_then_read_back(kv, key):
    assert await kv.try_save(key, b"first", "") is True
    data, etag = await kv.get(key)
    assert data == b"first"
    assert etag != "", "a stored key must expose a non-empty etag or CAS can never target it"


@sync
async def test_create_is_first_write_wins(kv, key):
    """Two creators race; exactly one wins. Without this a second actor silently overwrites a
    freshly created dedupe set."""
    assert await kv.try_save(key, b"a", "") is True
    assert await kv.try_save(key, b"b", "") is False
    data, _ = await kv.get(key)
    assert data == b"a"


@sync
async def test_stale_etag_is_rejected(kv, key):
    await kv.try_save(key, b"v1", "")
    _, etag1 = await kv.get(key)
    assert await kv.try_save(key, b"v2", etag1) is True

    # etag1 is now stale — a writer holding it must lose, not clobber v2.
    assert await kv.try_save(key, b"v3", etag1) is False
    data, _ = await kv.get(key)
    assert data == b"v2"


@sync
async def test_unconditional_put_invalidates_a_held_etag(kv, key):
    """`set()` is last-write-wins, but it must still bump the version — otherwise a CAS holder
    that missed the set would overwrite it while believing nothing had changed."""
    await kv.try_save(key, b"v1", "")
    _, etag = await kv.get(key)

    await kv.put(key, b"clobbered")

    assert await kv.try_save(key, b"stale-cas", etag) is False
    data, _ = await kv.get(key)
    assert data == b"clobbered"


# GlobalStore retries a contended CAS `_CAS_RETRIES` (16) times before giving up. Concurrency
# at or below that budget must be lossless; above it the contract is a LOUD failure, never a
# silent one. Both halves are pinned below.
WITHIN_BUDGET = 12


@sync
async def test_concurrent_increments_lose_nothing(kv, key):
    """The real test. Concurrent incr() through the CAS loop must land on exactly N — a lost
    update shows up as a smaller number, which is the failure mode a fake KV cannot catch."""
    gs = GlobalStore(kv, f"race-{uuid.uuid4()}")
    await asyncio.gather(*(gs.incr("counter") for _ in range(WITHIN_BUDGET)))
    assert await gs.get("counter") == WITHIN_BUDGET


@sync
async def test_concurrent_set_adds_keep_every_member(kv, key):
    """add_to_set is the dedupe flagship: concurrent adds must all survive, and a re-add must
    report not-new."""
    gs = GlobalStore(kv, f"race-{uuid.uuid4()}")
    members = [f"m{i}" for i in range(WITHIN_BUDGET)]
    results = await asyncio.gather(*(gs.add_to_set("seen", m) for m in members))

    assert all(results), "every distinct member is a NEW add"
    stored = await gs.get("seen")
    assert sorted(stored) == sorted(members)

    assert await gs.add_to_set("seen", "m0") is False, "a re-add must report not-new"


@sync
async def test_extreme_contention_fails_loud_rather_than_losing_writes(kv, key):
    """Beyond the retry budget the op RAISES. That is the design (`_CAS_RETRIES`), and it is the
    property that matters: a hot key degrades into a visible error, never into silently dropped
    members of a dedupe set.

    Worth knowing operationally — ~16 concurrent writers to ONE global_state key is the ceiling,
    which is a real consideration for a shared dedupe set across a wide fleet. The bound is
    `_CAS_RETRIES` in globalstore.py, not anything the store imposes.
    """
    gs = GlobalStore(kv, f"race-{uuid.uuid4()}")
    with pytest.raises(RuntimeError, match="contention exhausted"):
        await asyncio.gather(*(gs.incr("hot") for _ in range(64)))
