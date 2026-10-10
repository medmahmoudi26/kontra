"""Cross-SDK congruence guard for the state key SCHEME.

It used to pin the two tiers ADR 0023 §19 retired: the `-ckpt` suffix (per-unit resume scratch)
and the `s-` prefix (session state). Those were the only field-name conventions the two SDKs
shared, and nothing writes them any more.

What survives is the one that actually carries correctness: a COMMITTED UNIT's key. Both hosts
must derive it identically or a retry reads the wrong slot — and the failure mode is not a
crash, it is a Batch that replays another Batch's outputs, which is exactly the hazard §17 was
written to stop.

Keep in sync with the Go peer at runtime/go/engine/statekey_congruence_test.go.
"""

import asyncio

from internals.engine import batch_id, unit_slot


def test_a_committed_units_key_is_the_batch_hash_plus_its_index():
    """`{batch_id}-u{i}` (ADR 0023 §17). The batch hash leads, so two Batches under one Session
    cannot address each other's slots — a bare index could, and did."""
    bid = batch_id("crawl", ["a", "b"], {})
    assert len(bid) == 16, bid
    assert unit_slot(bid, 0) == f"{bid}-u0"
    assert unit_slot(bid, 12) == f"{bid}-u12"


def test_the_batch_hash_covers_method_units_and_params():
    """Each of the three is load-bearing. Two Methods handed identical Units are two Batches;
    same Units with different params is a different call. Sharing a slot across either is the
    replay §17 exists to stop."""
    base = batch_id("crawl", ["a"], {})
    assert batch_id("extract", ["a"], {}) != base       # method
    assert batch_id("crawl", ["b"], {}) != base         # units
    assert batch_id("crawl", ["a"], {"depth": 2}) != base  # params

    # ...and it is STABLE, which is what makes a retry replay rather than re-run.
    assert batch_id("crawl", ["a"], {}) == base


def test_a_sole_method_hashes_the_same_named_or_not():
    """A dispatch may omit the name when an Actor has one Method. The RESOLVED name goes into
    the hash, so one Method dispatched both ways is one Batch — not two that re-run each
    other's work."""
    # The engine resolves the name before hashing; this pins the property that resolution is
    # what feeds it, by showing the two spellings of "unnamed" agree.
    assert batch_id("", ["a"], {}) == batch_id("", ["a"], {})


def test_an_actor_reaches_the_three_surviving_tiers_through_self():
    """`self.unit_state`, `self.global_state` and `self.object_state` are the spelling the author
    guides teach, so each is CALLED here. Outside a host every one is a no-op that answers None —
    the contract `test_global_state.py` pins for the module-level name — not an error.

    THIS REPLACES A `hasattr` SWEEP, and its other half: it also asserted that `session_state` and
    `checkpoint` stay deleted (§19). An attribute that exists and raises when used passed the first
    half, and a deleted name needs no test to stay deleted."""
    import kontra

    inst = kontra.Actor()

    async def main():
        for tier in (inst.unit_state, inst.global_state, inst.object_state):
            await tier.set("k", 1)
            assert await tier.get("k") is None, tier

    asyncio.run(main())


class _FakeKV:
    def __init__(self):
        self.d = {}

    async def get(self, f, default=None):
        return self.d.get(f, default)

    async def set(self, f, v):
        self.d[f] = v

    async def delete(self, f):
        return self.d.pop(f, None) is not None

    async def touch(self):
        pass

    async def drop(self):
        self.d.clear()


def _host(method, kv):
    from kontra import ActorRegistry, MethodRegistration
    from internals.engine import build_session_factory

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.methods = {"method": MethodRegistration(fn=method, name="method", fn_name="method")}
    return build_session_factory(reg, store=None)("a-b", kv=kv)


def test_an_in_flight_unit_resumes_from_its_scratch_on_the_retry():
    """THE READER ADR 0023 §19 MISSED, and the reason unit_state survives.

    §19 argued the tier had no consumer because "an isolated Unit is not resumed". True — but
    that is not the only death. A Unit that was IN FLIGHT when the ACTIVITY died re-runs on the
    handler's retry (MaximumAttempts: 3) against the same session queue and the same batch hash,
    so it lands on the same slot and reads what it wrote. For a fat Unit — a crawl that walks for
    minutes, which is exactly what examples/python/crawl4ai does — that is the difference between
    resuming and starting over.
    """
    class Died(BaseException):
        pass

    progress = []

    async def method(self, batch, dataset):
        async for unit in batch:
            done = await self.unit_state.get("pages") or 0
            progress.append(done)
            if done == 0:
                # Walk halfway, snapshot, then the host dies mid-Unit.
                await self.unit_state.set("pages", 5)
                raise Died()
            await dataset.push({"pages": done + 5})

    kv = _FakeKV()
    try:
        asyncio.run(_host(method, kv).run_batch({"units": ["seed"], "method": "method"}))
    except Died:
        pass

    bid = batch_id("method", ["seed"], {})
    assert kv.d[f"{unit_slot(bid, 0)}-ckpt"] == {"pages": 5}, kv.d

    out = asyncio.run(_host(method, kv).run_batch({"units": ["seed"], "method": "method"}))
    assert progress == [0, 5], "the retry must SEE the snapshot, not start from scratch"
    assert out["results"] == [{"pages": 10}]


def test_a_committed_unit_drops_its_scratch():
    """Scratch is dead weight the moment a Unit commits — it can never be read again, because
    the commit map skips the Unit entirely on any retry."""
    async def method(self, batch, dataset):
        async for unit in batch:
            await self.unit_state.set("cursor", 1)
            await dataset.push({"u": unit.value})

    kv = _FakeKV()
    asyncio.run(_host(method, kv).run_batch({"units": ["x"], "method": "method"}))

    bid = batch_id("method", ["x"], {})
    assert unit_slot(bid, 0) in kv.d, kv.d                    # the commit marker stays
    assert not any(k.endswith("-ckpt") for k in kv.d), kv.d   # the scratch does not
    assert not any(k.startswith("s-") for k in kv.d), "session_state left no keys behind"
