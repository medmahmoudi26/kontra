"""global_state (ADR 0015 tier 3): the no-op-outside-host surface plus the atomic ops' key
behaviors and concurrency invariants, driven against a fake ETag KV — no store needed, so this
runs in the main venv. The whole point of the atomics is that concurrent sessions never lose
updates, which is exactly what these tests pin."""

import asyncio

from kontra import global_state
from internals.globalstore import GlobalStore


def test_global_state_is_noop_outside_host():
    async def main():
        assert await global_state.get("k") is None
        await global_state.set("k", 1)                                # no-op
        assert await global_state.add_to_set("s", "a") is False
        assert await global_state.incr("c") == 0
        assert await global_state.compare_and_set("k", None, 1) is False
        assert await global_state.get("k") is None

    asyncio.run(main())


class FakeEtagKV:
    """In-memory ETag store: key -> (data, version). try_save succeeds only if the caller's etag
    still matches the current version; get yields control (await sleep(0)) so gather()'d ops
    interleave BETWEEN get and try_save, forcing the CAS-retry path a real store would hit."""

    def __init__(self):
        self.store: dict = {}

    async def get(self, key):
        await asyncio.sleep(0)
        if key not in self.store:
            return (None, "")
        data, ver = self.store[key]
        return (data, str(ver))

    async def put(self, key, data):
        _, ver = self.store.get(key, (None, 0))
        self.store[key] = (data, ver + 1)

    async def try_save(self, key, data, etag):
        cur = self.store.get(key)
        cur_etag = str(cur[1]) if cur else ""
        if etag != cur_etag:
            return False                                  # a concurrent writer won
        self.store[key] = (data, (cur[1] if cur else 0) + 1)
        return True


def _store():
    return GlobalStore(FakeEtagKV(), "myactor")


def test_get_set_and_actor_name_scoping():
    async def main():
        kv = FakeEtagKV()
        gs = GlobalStore(kv, "myactor")
        assert await gs.get("k") is None
        await gs.set("k", {"v": 1})
        assert await gs.get("k") == {"v": 1}
        assert "kontra-global:myactor:k" in kv.store       # keys are actor-NAME scoped

    asyncio.run(main())


def test_add_to_set_dedupes_and_reports_newness():
    async def main():
        gs = _store()
        assert await gs.add_to_set("seen", "a") is True    # newly added
        assert await gs.add_to_set("seen", "a") is False   # already present -> no-op
        assert await gs.add_to_set("seen", "b") is True
        assert sorted(await gs.get("seen")) == ["a", "b"]

    asyncio.run(main())


def test_concurrent_incr_loses_no_updates():
    async def main():
        gs = _store()
        await asyncio.gather(*[gs.incr("c") for _ in range(10)])
        assert await gs.get("c") == 10     # every increment landed (CAS retried under contention)

    asyncio.run(main())


def test_concurrent_add_to_set_loses_no_members():
    async def main():
        gs = _store()
        await asyncio.gather(*[gs.add_to_set("s", i) for i in range(10)])
        assert sorted(await gs.get("s")) == list(range(10))   # no member dropped by a race

    asyncio.run(main())


def test_compare_and_set():
    async def main():
        gs = _store()
        assert await gs.compare_and_set("k", None, "first") is True    # None == absent
        assert await gs.compare_and_set("k", "wrong", "x") is False    # value mismatch
        assert await gs.compare_and_set("k", "first", "second") is True
        assert await gs.get("k") == "second"

    asyncio.run(main())
