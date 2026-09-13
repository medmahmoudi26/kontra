"""object_state (ADR 0022 tier 4): the KEY-scoped cross-session tier.

The operations are global_state's — same class, same atomics, already covered by
test_global_state.py. What is new and what these pin is the NAMESPACE, because every way this
tier can be wrong is a way two things quietly share state they should not: two keys of one
actor, two actors handed the same caller string, or the two tiers colliding on one state key.
None of those raise. They just return someone else's data.
"""

import asyncio

from kontra import global_state, object_state
from internals.globalstore import GlobalStore, global_prefix, object_prefix

from test_global_state import FakeEtagKV


def test_object_state_is_noop_outside_host():
    """Same rule as every other tier: an actor body must stay runnable as a plain unit test,
    with no store and no host binding the contextvar."""

    async def main():
        assert await object_state.get("k") is None
        await object_state.set("k", 1)                                # no-op
        assert await object_state.add_to_set("s", "a") is False
        assert await object_state.incr("c") == 0
        assert await object_state.compare_and_set("k", None, 1) is False
        assert await object_state.get("k") is None

    asyncio.run(main())


def test_the_two_tiers_are_bound_independently():
    """They share a class; they must not share a contextvar, or binding the host's name-scoped
    store would silently answer key-scoped reads."""
    assert object_state is not global_state
    assert object_state._io is not global_state._io


def test_two_keys_of_one_actor_share_nothing():
    async def main():
        kv = FakeEtagKV()
        acme = GlobalStore(kv, prefix=object_prefix("crawler", "acme.com"))
        other = GlobalStore(kv, prefix=object_prefix("crawler", "other.com"))

        assert await acme.add_to_set("seen", "/a") is True
        assert await other.add_to_set("seen", "/a") is True   # newly added HERE too
        assert await other.get("seen") == ["/a"]
        assert await acme.get("seen") == ["/a"]

    asyncio.run(main())


def test_the_same_key_under_two_actors_shares_nothing():
    """The key is the CALLER's string, so a collision across actors is expected, not exotic —
    `beacon["acme.com"]` and `crawler["acme.com"]` are different objects."""

    async def main():
        kv = FakeEtagKV()
        crawler = GlobalStore(kv, prefix=object_prefix("crawler", "acme.com"))
        beacon = GlobalStore(kv, prefix=object_prefix("beacon", "acme.com"))

        await crawler.set("cursor", 10)
        assert await beacon.get("cursor") is None

    asyncio.run(main())


def test_the_tiers_do_not_collide_on_one_state_key():
    async def main():
        kv = FakeEtagKV()
        keyed = GlobalStore(kv, prefix=object_prefix("crawler", "acme.com"))
        named = GlobalStore(kv, "crawler")

        await keyed.set("budget", 1)
        await named.set("budget", 999)
        assert await keyed.get("budget") == 1

    asyncio.run(main())


def test_the_key_tier_survives_a_version_bump():
    """Same reasoning as tier 3: a frontier or a dedupe set that reset on every deploy would be
    worse than useless, so version is deliberately out of the namespace."""
    assert object_prefix("crawler", "acme.com") == "kontra-object:crawler:acme.com:"
    assert "0.1.0" not in object_prefix("crawler", "acme.com")
    assert global_prefix("crawler") == "kontra-global:crawler:"


def test_an_unkeyed_actor_still_gets_a_private_namespace():
    """With no idempotency_key the handler falls back to run-node, so the tier is present but
    private per dispatch — reads return None rather than another run's state."""

    async def main():
        kv = FakeEtagKV()
        a = GlobalStore(kv, prefix=object_prefix("crawler", "run1-node1"))
        b = GlobalStore(kv, prefix=object_prefix("crawler", "run2-node1"))
        await a.set("k", 1)
        assert await b.get("k") is None

    asyncio.run(main())
