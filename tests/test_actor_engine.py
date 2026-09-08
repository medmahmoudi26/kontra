"""The actor engine: identity-keyed commits, input-order results, replay-skip, failure
isolation, healthcheck->reload, poison-unit isolation, and the three state tiers.

Every one of these tests describes the execution ENGINE, not the runtime that invokes it —
which is why the ADR 0018 runtime swap was a transformation of this file rather than a rewrite.
Exactly one test went: the time-boxed turn loop, replaced below by the invariant that took its
place.
"""

import asyncio

import pytest

from kontra import ActorRegistry, MethodRegistration
from internals.engine import batch_id, build_session_factory, unit_slot


class FakeKV:
    """An in-memory ActorStateKV. One dict standing in for the one Redis hash — which is what
    made the real thing simple enough to fake in six lines (`internals/statekv.py`)."""

    def __init__(self):
        self.d = {}

    async def get(self, field, default=None):
        return self.d.get(field, default)

    async def set(self, field, value):
        self.d[field] = value

    async def delete(self, field):
        return self.d.pop(field, None) is not None

    async def touch(self):
        pass

    async def drop(self):
        self.d.clear()


class FakeUnitStore:
    """In-memory unitstore double: same put_subunit contract, keys -> bytes.

    It delegates to the REAL `blob_key`, so the hive layout and the `run_date` the handler
    stamps are both exercised here. An earlier version invented its own flat
    `units/{run}/{node}/u{i}/{sha}.json` — which is why Python silently dropped `run_date`
    for as long as it did while Go carried it.
    """

    def __init__(self):
        self.blobs = {}

    def put_subunit(self, run_id, node_id, i, record, run_date=""):
        import hashlib
        import json as _json

        from internals.unitstore import blob_key

        data = _json.dumps([record], sort_keys=True).encode()
        sha = hashlib.sha256(data).hexdigest()
        key = blob_key(run_date, "t", run_id, node_id, i, sha)
        self.blobs[key] = data
        return {"$ref": {"key": key, "size": len(data), "sha256": sha}}

    def get_subunit(self, key):
        """The ingest half — same unwrap-the-1-element-array contract as the real store."""
        import json as _json

        rec = _json.loads(self.blobs[key])
        return rec[0] if isinstance(rec, list) else rec


def unit_blobs(store, run, unit):
    """Keys this run wrote for one unit, matched on the hive segments rather than the whole
    key — `dt=` is today's date unless the payload carried a run_date, and pinning it here
    would make these tests fail at midnight."""
    return [
        k for k in store.blobs
        if k.startswith(f"units/run={run}/") and f"/unit={unit:05d}/" in k
    ]


def make_host(method, load=None, healthcheck=None, store=None):
    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.methods = {method.__name__: MethodRegistration(
        fn=method, name=method.__name__, fn_name=method.__name__)}
    reg.load_fn = load
    reg.healthcheck_fn = healthcheck
    # store=None => inline commits (the no-S3 mode). The factory is the whole construction
    # path now: there is no runtime ctor to bypass and no activation callback to fire.
    return build_session_factory(reg, store=store)("run1-node1", kv=FakeKV())


def test_a_method_receives_the_whole_batch_and_pushes_to_the_dataset():
    """The v2 author surface (ADR 0023 §2, ADR 0028 §2): one call carries the Batch and its
    output Dataset, the author owns the loop, and a record reaches the run by being pushed — it
    names no Unit, the framework attributes it to the one the iterator is on."""
    seen = []

    async def method(self, batch, dataset):
        async for unit in batch:
            seen.append(unit.value)
            await dataset.push({"u": unit.value})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "b"]}))
    assert seen == ["a", "b"]          # ONE call over the whole Batch, not one call per Unit
    assert out["done"] is True
    assert out["results"] == [{"u": "a"}, {"u": "b"}]
    assert out["failures"] == []


def test_concurrent_pushes_land_in_push_order_not_input_order():
    """ADR 0028 §1: push names no Unit, so concurrency no longer buys input-ordered output. An
    author who takes the whole Batch and pushes from spawned tasks gets records in the order they
    were pushed (here: completion order). The set is complete; the order is the author's to make
    deterministic by putting a key IN the record, not the framework's to reconstruct from a Unit
    link that no longer exists."""
    async def method(self, batch, dataset):
        async def one(unit):
            await asyncio.sleep((5 - unit.value) * 0.01)  # later units finish first
            await dataset.push({"u": unit.value}, key=f"u-{unit.value}")  # no current Unit -> keyed

        await asyncio.gather(*(one(u) for u in batch.units))

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": [0, 1, 2, 3, 4]}))
    assert out["done"] is True
    # Push order == completion order, which the sleeps made the reverse of input order.
    assert out["results"] == [{"u": i} for i in reversed(range(5))]
    assert out["failures"] == []


def test_a_unit_pushes_as_many_records_as_it_likes():
    """1 -> N is ordinary: push is a call, so the count is the author's business and every
    record commits with the Unit the iterator is on (ADR 0028 §1)."""

    async def method(self, batch, dataset):
        async for unit in batch:
            for page in range(unit.value):
                await dataset.push({"seed": unit.value, "page": page})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": [2, 1]}))
    assert out["results"] == [{"seed": 2, "page": 0}, {"seed": 2, "page": 1},
                              {"seed": 1, "page": 0}]


def test_a_committed_unit_is_keyed_by_the_batch_hash_plus_its_index():
    """ADR 0023 §17. The key scheme is durable state, so it is asserted directly: a bare index
    is what let a second Batch read the first's slots."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})

    host = make_host(method)
    asyncio.run(host.run_batch({"units": ["a", "b"], "method": "method"}))

    bid = batch_id("method", ["a", "b"], {})
    assert host._kv.d[unit_slot(bid, 0)] == {"out": [{"u": "a"}]}
    assert host._kv.d[unit_slot(bid, 1)] == {"out": [{"u": "b"}]}


def test_retry_replays_committed_units_by_identity_not_position():
    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    host = make_host(method)
    # A prior attempt of THIS batch committed units 1 and 3. Same units, same Method, same
    # params -> the same content hash, which is what makes a retry find them at all.
    bid = batch_id("method", ["a", "b", "c", "d"], {})
    host._kv.d[unit_slot(bid, 1)] = {"out": [{"u": "one-committed"}]}
    host._kv.d[unit_slot(bid, 3)] = {"out": [{"u": "three-committed"}]}
    out = asyncio.run(host.run_batch({"units": ["a", "b", "c", "d"], "method": "method"}))
    assert sorted(ran) == ["a", "c"]  # committed units never re-run
    assert out["results"] == [{"u": "a"}, {"u": "one-committed"}, {"u": "c"}, {"u": "three-committed"}]


def test_a_crash_mid_batch_resumes_at_the_first_uncommitted_unit():
    """The whole point of the commit map. A host death is not a unit failure — nothing catches
    it — so the batch stops where it stood; the retry re-runs the Unit that was in flight and
    NOT the ones already committed."""

    class HostStruck(BaseException):
        pass

    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            if unit.value == "c" and ran.count("c") == 1:
                raise HostStruck()
            await dataset.push({"u": unit.value})

    host = make_host(method)
    units = ["a", "b", "c", "d"]
    with pytest.raises(HostStruck):
        asyncio.run(host.run_batch({"units": units}))

    out = asyncio.run(host.run_batch({"units": units}))     # = the Temporal activity retry
    assert ran == ["a", "b", "c", "c", "d"]                 # only the in-flight unit re-ran
    assert out["done"] is True
    assert out["results"] == [{"u": u} for u in units]


def test_an_author_who_stops_early_commits_what_they_finished():
    """The loop is the author's, so leaving it is theirs to decide (ADR 0023 §14): the Units
    reached are committed and the rest are simply not done — never committed empty, which would
    make a skipped Unit indistinguishable from one that produced nothing."""
    ran = []

    async def method(self, batch, dataset):
        handled = 0
        async for unit in batch:
            if handled == 2:
                break
            ran.append(unit.value)
            handled += 1
            await dataset.push({"u": unit.value})

    host = make_host(method)
    units = ["a", "b", "c", "d"]
    out = asyncio.run(host.run_batch({"units": units}))
    assert out["results"] == [{"u": "a"}, {"u": "b"}]
    assert out["done"] is False                     # two Units were never attempted

    out2 = asyncio.run(host.run_batch({"units": units}))   # the same Batch again
    assert ran == ["a", "b", "c", "d"]              # only the unfinished Units re-ran
    assert out2["done"] is True


def test_push_from_a_concurrent_task_carries_its_own_provenance():
    """ADR 0028 §1: provenance is IN the record, not in a Unit link. The author takes the whole
    Batch, finishes the Units in reverse order, and each record still says which seed it came from
    because the author put `u` in it — which is the point of decoupling, since under concurrency
    there is no iterator position to attribute by. The batch commits when the Method returns."""

    async def method(self, batch, dataset):
        async def one(unit):
            await asyncio.sleep(0.01 * (3 - unit.index))    # finish in reverse order
            # No current Unit under batch.units -> each push names its own out-of-loop key.
            await dataset.push({"u": unit.value, "first": True}, key=f"{unit.value}-1")
            await dataset.push({"u": unit.value, "second": True}, key=f"{unit.value}-2")

        await asyncio.gather(*(one(u) for u in batch.units))

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "b", "c"]}))
    assert out["done"] is True
    # Push order (completion order: c, then b, then a), each pair kept together by the task.
    assert out["results"] == [
        {"u": "c", "first": True}, {"u": "c", "second": True},
        {"u": "b", "first": True}, {"u": "b", "second": True},
        {"u": "a", "first": True}, {"u": "a", "second": True},
    ]
    # Every seed's two records are present regardless of order — nothing was lost to the tail.
    assert {r["u"] for r in out["results"]} == {"a", "b", "c"}


def test_a_raise_with_no_unit_to_blame_propagates_and_commits_nothing():
    """An author who takes the whole Batch to run it themselves owns its errors too. There is no
    position to attribute the raise to, so the framework does not guess: it commits nothing —
    the author's tasks may still be running — and the retry re-runs the Batch."""

    async def method(self, batch, dataset):
        units = batch.units
        await dataset.push({"u": units[0].value}, key="first")  # no current Unit -> keyed
        raise RuntimeError("my own concurrency, my own error")

    host = make_host(method)
    with pytest.raises(RuntimeError, match="my own concurrency"):
        asyncio.run(host.run_batch({"units": ["a", "b"]}))

    bid = batch_id("method", ["a", "b"], {})
    assert unit_slot(bid, 0) not in host._kv.d      # in-flight work is never committed
    assert unit_slot(bid, 1) not in host._kv.d


def test_live_failure_isolates_unit_and_batch_completes():
    """The naive loop — no try/except anywhere — does the right thing: the raise is attributed
    to the Unit the iterator was on and the Method is re-invoked with the remainder."""

    async def method(self, batch, dataset):
        async for unit in batch:
            if unit.value == "boom":
                raise ValueError("bad unit")
            await dataset.push({"u": unit.value})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "boom", "c"]}))
    assert out["done"] is True
    assert out["results"] == [{"u": "a"}, {"u": "c"}]
    assert len(out["failures"]) == 1
    f = out["failures"][0]
    assert f["unit"] == "boom" and f["error"]["type"] == "ValueError" and f["category"] == "exhausted"
    # the failure commit replays verbatim on retry
    out2 = asyncio.run(host.run_batch({"units": ["a", "boom", "c"]}))
    assert out2["failures"] == out["failures"]


def test_the_method_is_re_invoked_with_only_the_remaining_units():
    """ADR 0023 §13. The raise is attributed to the Unit the iterator was on, and the Method is
    entered AGAIN with what is left. Everything pushed before the raise is already committed,
    so re-entry does not re-push it — which is what makes it cheap and equivalent."""
    entries = []                       # one list per entry into the Method

    async def method(self, batch, dataset):
        seen = []
        entries.append(seen)
        async for unit in batch:
            seen.append(unit.value)
            if unit.value == "boom":
                raise ValueError("bad unit")
            await dataset.push({"u": unit.value})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "boom", "c", "d"]}))

    assert entries == [["a", "boom"], ["c", "d"]]   # the second entry saw ONLY the remainder
    assert out["results"] == [{"u": "a"}, {"u": "c"}, {"u": "d"}]  # 'a' pushed once, not twice
    assert [f["unit"] for f in out["failures"]] == ["boom"]


def test_author_locals_reset_across_re_entry_while_self_persists():
    """The sharpest thing an author must know and would never guess (§13): a Method is entered
    several times per Batch, so what a LOCAL accumulated is gone at the re-entry and what is on
    `self` is not."""

    async def load(self):
        self.on_self = []

    async def method(self, batch, dataset):
        local = []
        async for unit in batch:
            local.append(unit.value)
            self.on_self.append(unit.value)
            if unit.value == "boom":
                raise ValueError("bad unit")
            await dataset.push({"local": list(local), "on_self": list(self.on_self)})

    host = make_host(method, load=load)
    out = asyncio.run(host.run_batch({"units": ["a", "boom", "c"]}))
    assert out["results"] == [
        {"local": ["a"], "on_self": ["a"]},
        {"local": ["c"], "on_self": ["a", "boom", "c"]},   # the local reset; `self.*` did not
    ]


def test_a_dead_resource_ends_the_session_rather_than_reloading_it():
    """ADR 0023 §20: `@actor.healthcheck` says END ME, not RELOAD ME.

    Reloading in place rebuilds `self.*` under a running author — the silent reset §7 rejected
    for host loss. So the scope ends, and it stays ended: a second call cannot re-activate it.
    """
    from kontra.retry import SessionLost

    opens = []

    async def load(self):
        opens.append(1)
        self.alive = True

    async def method(self, batch, dataset):
        async for unit in batch:
            if unit.value == "die":
                self.alive = False
                raise RuntimeError("engine crashed")
            await dataset.push({"u": unit.value})

    async def hc(self):
        if not self.alive:
            raise RuntimeError("dead")
        return True

    host = make_host(method, load=load, healthcheck=hc)
    units = ["a", "die", "c"]
    with pytest.raises(SessionLost):
        asyncio.run(host.run_batch({"units": units}))

    # The Session is over. Calling again raises instead of quietly opening a second resource.
    with pytest.raises(SessionLost):
        asyncio.run(host.run_batch({"units": units}))
    assert opens == [1], "@actor.load must not run twice — that is the reload §20 deletes"


def test_a_resource_that_dies_on_the_beat_ends_the_session_too(monkeypatch):
    """`@actor.healthcheck` runs on the heartbeat as well as after a failure, and THAT is the
    worst case rather than the mildest: an author whose loop swallows its own errors would
    otherwise hand back a Batch that committed nothing and reads as finished. A dead resource
    ends the Session either way (ADR 0023 §20)."""
    import internals.engine as engine
    from kontra.retry import SessionLost

    monkeypatch.setattr(engine, "_BEAT_S", 0.01)   # the beat interval, not a behaviour knob
    alive = [True]

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})
            alive[0] = False               # the resource dies with nothing raised
            await asyncio.sleep(0.05)      # long enough for one beat

    async def hc(self):
        if not alive[0]:
            raise RuntimeError("resource gone")
        return True

    host = make_host(method, healthcheck=hc)
    with pytest.raises(SessionLost):
        asyncio.run(host.run_batch({"units": ["a", "b"]}))

    # What committed before the death is still committed — the scope ended, it was not undone.
    assert host._kv.d[unit_slot(batch_id("method", ["a", "b"], {}), 0)] == {"out": [{"u": "a"}]}


def reopen(method, load=None, healthcheck=None, store=None, key="acme.com"):
    """A caller's scope against ONE key, openable more than once over one durable state.

    This is what §7's recovery is: the scope raises, the caller opens a NEW one against the same
    key, and the state that key owns is still there. Returns `(open_scope, kv)`.
    """
    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.methods = {method.__name__: MethodRegistration(
        fn=method, name=method.__name__, fn_name=method.__name__)}
    reg.load_fn = load
    reg.healthcheck_fn = healthcheck
    factory = build_session_factory(reg, store=store)
    kv = FakeKV()                       # the key's durable state, outliving any one scope
    return (lambda: factory(key, kv=kv)), kv


def test_a_reopened_scope_resumes_from_the_commit_map():
    """ADR 0023 §7 + §17. Losing the resource costs at most the Batch in flight: the caller
    opens a new scope against the same key and the Units that committed before the death do not
    run again, because a content hash does not know its scope died."""
    from kontra.retry import SessionLost

    ran, died = [], []

    async def load(self):
        self.alive = True

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            if unit.value == "die" and not died:
                died.append(1)
                self.alive = False
                raise RuntimeError("engine crashed")
            await dataset.push({"u": unit.value})

    async def hc(self):
        if not self.alive:
            raise RuntimeError("dead")
        return True

    scope, _ = reopen(method, load=load, healthcheck=hc)
    units = ["a", "die", "c"]
    with pytest.raises(SessionLost):
        asyncio.run(scope().run_batch({"units": units}))

    out = asyncio.run(scope().run_batch({"units": units}))   # the caller reopens
    assert out["opens"] == 1                  # a NEW resource in a NEW scope, not a rebuilt one
    assert out["done"] is True
    assert out["results"] == [{"u": "a"}, {"u": "die"}, {"u": "c"}]
    assert ran == ["a", "die", "die", "c"], "only the Unit that died re-ran"


def test_a_unit_that_kills_scope_after_scope_is_isolated_and_skipped():
    """ADR 0023 §21. §20 and §17 compose into a tight loop — end the scope, reopen, resume at
    the poison Unit, die again — so the framework counts the scopes a Unit has killed and drops
    it. The counter is durable and hangs off the Unit's commit slot, which is what lets it
    survive the scope it just killed."""
    from kontra.retry import SessionLost

    async def load(self):
        self.alive = True

    async def method(self, batch, dataset):
        async for unit in batch:
            if unit.value == "poison":
                self.alive = False
                raise RuntimeError("kills the engine every time")
            await dataset.push({"u": unit.value})

    async def hc(self):
        if not self.alive:
            raise RuntimeError("dead")
        return True

    scope, _ = reopen(method, load=load, healthcheck=hc)
    units = ["a", "poison", "c"]
    with pytest.raises(SessionLost):          # first scope: the poison unit kills it
        asyncio.run(scope().run_batch({"units": units}))

    out = asyncio.run(scope().run_batch({"units": units}))   # second: isolated, batch completes
    assert out["done"] is True
    assert out["results"] == [{"u": "a"}, {"u": "c"}]
    assert len(out["failures"]) == 1 and out["failures"][0]["unit"] == "poison"


def test_unit_state_resumes_mid_unit_and_clears_on_commit():
    """A fat unit (multi-page crawl) snapshots its resume scratch per page; a death mid-unit
    resumes from the snapshot — pages before it are NOT re-fetched — and the slot dies with
    the commit. (unit_state is the ADR-0015 name for the per-unit resume scratch.)"""
    from kontra import unit_state
    from kontra.retry import SessionLost

    fetched = []  # every page fetch across all attempts — replay shows up here
    died = []

    async def method(self, batch, dataset):
        async for unit in batch:
            state = await unit_state.get("progress") or {"next_page": 0, "pages": []}
            for page in range(state["next_page"], 4):
                if unit.value == "big" and page == 2 and not died:
                    died.append(1)
                    raise SessionLost("host struck mid-crawl")
                fetched.append((unit.value, page))
                state["pages"].append(f"{unit.value}-p{page}")
                state["next_page"] = page + 1
                await unit_state.set("progress", state)
            await dataset.push({"unit": unit.value, "pages": state["pages"]})

    scope, kv = reopen(method)
    slot = unit_slot(batch_id("method", ["big"], {}), 0)
    with pytest.raises(SessionLost):
        asyncio.run(scope().run_batch({"units": ["big"]}))
    # the unit's keys live in one blob beside its commit slot; the snapshot survived the death
    assert kv.d[f"{slot}-ckpt"]["progress"]["next_page"] == 2

    out = asyncio.run(scope().run_batch({"units": ["big"]}))  # the caller reopens the scope
    assert out["done"] is True
    assert out["results"] == [{"unit": "big", "pages": ["big-p0", "big-p1", "big-p2", "big-p3"]}]
    assert fetched == [("big", 0), ("big", 1), ("big", 2), ("big", 3)]  # no page fetched twice
    assert f"{slot}-ckpt" not in kv.d  # slot cleared on commit
    assert slot in kv.d


def test_each_unit_gets_its_own_unit_state_slot():
    """One Unit's resume scratch is never another's: the slot follows the Unit the iterator
    handed out, so a key written under one Unit reads back as nothing under the next."""
    from kontra import unit_state

    async def method(self, batch, dataset):
        async for unit in batch:
            seen = await unit_state.get("mine")     # the PREVIOUS unit wrote this key
            await unit_state.set("mine", unit.value)
            await dataset.push({"unit": unit.value, "saw": seen})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "b", "c", "d"]}))
    assert all(r["saw"] is None for r in out["results"]), out["results"]


def test_unit_state_multiple_keys_share_one_blob_delete_and_clear():
    """Keyed unit_state (ADR 0015): several keys on one unit share ONE u{i}-ckpt blob; delete
    drops just its key; clear-on-commit drops the WHOLE blob (every key)."""
    from kontra import unit_state
    from kontra.retry import SessionLost

    died = []

    async def method(self, batch, dataset):
        async for unit in batch:
            await unit_state.set("frontier", {"pending": [unit.value]})
            await unit_state.set("cursor", 7)
            await unit_state.set("scratch", "tmp")
            await unit_state.delete("scratch")   # delete one of three; the other two remain
            if not died:
                died.append(1)
                raise SessionLost("die mid-unit, before commit")
            await dataset.push({"frontier": await unit_state.get("frontier"),
                             "cursor": await unit_state.get("cursor"),
                             "scratch": await unit_state.get("scratch")})

    scope, kv = reopen(method)
    ckpt = f"{unit_slot(batch_id('method', ['x'], {}), 0)}-ckpt"
    with pytest.raises(SessionLost):
        asyncio.run(scope().run_batch({"units": ["x"]}))
    # both surviving keys coexist in the ONE blob; the deleted key is gone
    assert kv.d[ckpt] == {"frontier": {"pending": ["x"]}, "cursor": 7}

    out = asyncio.run(scope().run_batch({"units": ["x"]}))  # reopened: all keys resume from the blob
    r = out["results"][0]
    assert r["frontier"] == {"pending": ["x"]} and r["cursor"] == 7 and r["scratch"] is None
    assert ckpt not in kv.d  # clear-on-commit dropped the whole blob (all keys)










def test_push_writes_one_durable_blob_per_record():
    """Each pushed record is its own blob under the unit's prefix
    (units/{run}/{node}/u{i}/{sha}.json), written AS it is pushed — so a downstream streaming
    cursor consumes records before the unit finishes. The unit's commit holds the refs."""
    store = FakeUnitStore()

    async def method(self, batch, dataset):
        async for unit in batch:
            for page in range(unit.value["pages"]):
                await dataset.push({"url": f"{unit.value['seed']}/p{page}"})

    host = make_host(method, store=store)
    units = [{"seed": "a", "pages": 3}]
    out = asyncio.run(host.run_batch({"units": units, "run_id": "r", "node_id": "n"}))
    assert out["done"] is True
    subkeys = unit_blobs(store, "r", 0)
    assert len(subkeys) == 3, sorted(store.blobs)         # one blob per pushed record
    assert all(k.endswith(".json") for k in subkeys)
    assert len(out["results"]) == 3                        # results = the refs
    assert all("$ref" in r for r in out["results"])
    slot = unit_slot(batch_id("method", units, {}), 0)
    assert host._kv.d[slot]["out"] == out["results"]       # the commit holds the refs


def test_push_without_a_store_collects_records_inline():
    """No object store (dev/test): push still works — records are collected inline into
    results and are durable when the unit commits."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"n": 1})
            await dataset.push({"n": 2})

    host = make_host(method, store=None)  # inline mode
    out = asyncio.run(host.run_batch({"units": ["x"], "run_id": "r", "node_id": "n"}))
    assert out["done"] is True
    assert out["results"] == [{"n": 1}, {"n": 2}]


def test_a_resumed_unit_repushes_by_content_sha():
    """A death mid-unit leaves the unit UNCOMMITTED (so it re-runs); on the retry the Method
    resumes from unit_state and the pushed records land under the same unit prefix — the S3
    prefix (what a streaming cursor reads) ends up holding every record, keyed by content sha."""
    from kontra.retry import SessionLost

    store = FakeUnitStore()
    died = []

    async def method(self, batch, dataset):
        async for unit in batch:
            state = await self.unit_state.get("cursor") or {"next": 0}
            for page in range(state["next"], 3):
                if page == 2 and not died:
                    died.append(1)
                    raise SessionLost("struck mid-unit")
                await dataset.push({"url": f"p{page}"})
                state["next"] = page + 1
                await self.unit_state.set("cursor", state)

    scope, kv = reopen(method, store=store)
    slot = unit_slot(batch_id("method", [{}], {}), 0)
    with pytest.raises(SessionLost):
        asyncio.run(scope().run_batch({"units": [{}], "run_id": "r", "node_id": "n"}))
    assert slot not in kv.d  # died mid-unit -> unit uncommitted
    assert len(unit_blobs(store, "r", 0)) == 2  # p0, p1 durable

    out = asyncio.run(scope().run_batch({"units": [{}], "run_id": "r", "node_id": "n"}))  # reopened
    assert out["done"] is True
    # the cursor's view (the full prefix) now holds all three records by content sha
    assert len(unit_blobs(store, "r", 0)) == 3


def test_ref_units_are_resolved_on_ingest_so_one_method_can_eat_another_s_output():
    """ADR 0007 at the seam it was previously violated. A Method's output is a list of
    `{"$ref": …}` entries into the hive blob plane; handing that list to the next Method must
    show its author the RECORD, not the ref dict.

    The ACTOR resolves, never the caller: the caller has no S3 credentials by design, the actor
    has them because it wrote these blobs.
    """
    seen = []

    async def method(self, batch, dataset):
        async for unit in batch:
            seen.append(unit.value)
            await dataset.push({"got": unit.value})

    store = FakeUnitStore()

    # Stage 1 writes two records and returns refs to them.
    first = asyncio.run(make_host(method, store=store).run_batch(
        {"units": ["a", "b"], "run_id": "r", "node_id": "n1"}))
    refs = first["results"]
    assert all("$ref" in r for r in refs), refs

    # Stage 2 is handed those refs verbatim — the shape a chained dispatch produces.
    seen.clear()
    second = asyncio.run(make_host(method, store=store).run_batch(
        {"units": refs, "run_id": "r", "node_id": "n2"}))

    assert seen == [{"got": "a"}, {"got": "b"}]          # records, not ref dicts
    assert second["done"] is True
    assert second["failures"] == []


def test_a_ref_unit_without_an_object_store_fails_loud():
    """Silently handing the author a ref dict is how a batch produces plausible nonsense."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push(unit.value)

    host = make_host(method, store=None)          # no KONTRA_S3_ENDPOINT
    ref = {"$ref": {"key": "units/run=r/dt=x/actor=t/shard=n/unit=00000/deadbeef.json",
                    "size": 3, "sha256": "deadbeef"}}
    with pytest.raises(Exception) as ei:
        asyncio.run(host.run_batch({"units": [ref]}))
    assert "no object store" in str(ei.value)


def test_non_ref_units_pass_through_the_resolver_untouched():
    """The resolver must be uniform across inline records, the no-Method passthrough and a
    Dataset page of plain values — only `{"$ref": …}` is intercepted."""
    seen = []

    async def method(self, batch, dataset):
        async for unit in batch:
            seen.append(unit.value)
            await dataset.push(unit.value)

    store = FakeUnitStore()
    mixed = ["plain", {"nested": {"$ref": "not-a-ref-entry"}}, {"$ref": None}, 42]
    out = asyncio.run(make_host(method, store=store).run_batch(
        {"units": mixed, "run_id": "r", "node_id": "n"}))
    assert seen == mixed
    assert out["done"] is True


def test_run_date_comes_from_the_handler_not_the_worker():
    """The `dt=` partition is the RUN's date, stamped by the handler from the workflow's start
    time (runtime/handler/workflow.go:104) — never this worker's clock.

    Go has asserted this since it shipped (TestRunDateComesFromTheWorkflowNotTheWorker); Python
    dropped the field on the floor until 2026-08-14, so a Python run crossing midnight split its
    blobs across two partitions while a Go run did not. A reader filtering `WHERE dt = ...` sees
    that as data loss, and nothing raises.
    """

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch(
        {"units": ["a"], "run_id": "r", "node_id": "n", "run_date": "1999-12-31"}))

    assert out["done"] is True
    key = out["results"][0]["$ref"]["key"]
    assert "/dt=1999-12-31/" in key, key
    assert list(store.blobs) == [key]


def test_unit_store_commits_refs_not_payloads():
    """With a store configured, Redis (the state manager) holds only small refs; the
    payload lives in the blob store; the response carries the refs; a retry replays the
    refs without touching S3."""
    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"u": unit.value, "big": "x" * 1000})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    units = ["a", "b"]
    out = asyncio.run(host.run_batch({"units": units, "run_id": "r9", "node_id": "n1"}))
    assert out["done"] is True

    # response + commits are refs, blobs hold the payloads under the unit's prefix
    bid = batch_id("method", units, {})
    for i, entry in enumerate(out["results"]):
        ref = entry["$ref"]
        assert ref["key"].startswith("units/run=r9/") and f"/unit={i:05d}/" in ref["key"]
        committed = host._kv.d[unit_slot(bid, i)]["out"]
        assert committed == [entry]
        assert len(str(committed)) < 300  # the 1000-char payload is NOT in the state store
    import json as _json
    blob0 = _json.loads(store.blobs[out["results"][0]["$ref"]["key"]])
    assert blob0 == [{"u": "a", "big": "x" * 1000}]

    # retry: committed refs replay verbatim, the Method does not re-run, no new blob writes
    n_blobs = len(store.blobs)
    out2 = asyncio.run(host.run_batch({"units": units, "run_id": "r9", "node_id": "n1"}))
    assert out2["results"] == out["results"]
    assert ran == ["a", "b"] and len(store.blobs) == n_blobs


def test_no_store_keeps_inline_commits():
    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})

    host = make_host(method, store=None)
    out = asyncio.run(host.run_batch({"units": ["a"]}))
    assert out["results"] == [{"u": "a"}]
    slot = unit_slot(batch_id("method", ["a"], {}), 0)
    assert host._kv.d[slot] == {"out": [{"u": "a"}]}  # payload inline, as before


def test_a_batch_finishes_in_one_call():
    """The turn loop is gone (ADR 0018).

    It existed because the handler drove the actor through a sidecar under a StartToClose
    guillotine, so a long batch had to hand control back before the clock ran out. The actor is
    a Temporal activity worker now and heartbeats for itself, so a batch runs to completion in
    ONE call — and `done` is always true, because a partial return has no second call to
    resume it. A regression here reloads the author's resource mid-batch.
    """
    opens = []

    async def load(self):
        opens.append(1)

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})

    host = make_host(method, load=load)
    out = asyncio.run(host.run_batch({"units": ["a", "b", "c"]}))

    assert out["done"] is True
    assert out["results"] == [{"u": "a"}, {"u": "b"}, {"u": "c"}]
    assert len(opens) == 1  # one @actor.load for the whole batch


def test_heartbeat_speaks_the_orchestrator_s_field_names():
    """The heartbeat payload is a cross-language wire contract, and a silent one.

    `control/orchestrator/src/heartbeat.ts` decodes {node, done, total, isolated} off the pending
    activity and every field is optional with a 0 default — so a renamed field does not error,
    it reports 0/0 forever, which renders exactly like a node that has done nothing. This drifted
    once already: the engine emitted {committed, total} after ADR 0018 and live progress went
    blank while the runs themselves were fine.
    """
    beats = []

    async def method(self, batch, dataset):
        async for unit in batch:
            if unit.value == "poison":
                raise RuntimeError("nope")
            await dataset.push({"u": unit.value})

    host = make_host(method)
    host._heartbeat = beats.append
    host._node_id = "n7"

    out = asyncio.run(host.run_batch({"units": ["a", "poison", "c"], "node_id": "n7"}))

    assert out["done"] is True
    assert beats, "a batch that committed units must have beaten at least once"
    for b in beats:
        assert set(b) == {"node", "done", "total", "isolated"}, b
        assert b["node"] == "n7"
        assert b["total"] == 3

    # The LAST beat is the one an operator sees when the batch ends: 2 committed, 1 dropped.
    # A node that isolates everything must never look identical to one that found nothing.
    assert beats[-1]["done"] == 2
    assert beats[-1]["isolated"] == 1


def test_a_second_batch_on_a_reused_actor_id_does_not_replay_the_first():
    """The failure this guards is silent and total. The commit map is keyed by unit INDEX and
    lives in the actor-id-scoped hash; before keying existed every dispatch minted a fresh id,
    so the map could only ever describe THIS batch. `handle["acme.com"]` points many batches at
    one id — and without an owner check the second dispatch's unit 0 reads the first's `u0`,
    returns it, and never runs the author. A full, plausible, wrong result."""
    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"unit": unit.value})

    host = make_host(method)
    first = asyncio.run(host.run_batch({"units": ["a"], "node_id": "n1"}))
    second = asyncio.run(host.run_batch({"units": ["b"], "node_id": "n2"}))

    assert ran == ["a", "b"]                              # the second batch actually RAN
    assert first["results"][0] == {"unit": "a"}
    assert second["results"][0] == {"unit": "b"}          # not "a" replayed under a new label


def test_a_retry_of_the_SAME_batch_still_replays_its_commits():
    """The other half of the guard: same owner (same run/node) is a RETRY, and replay-not-rerun
    is the property that makes a mid-batch death cheap (ADR 0012). Clearing on every batch
    would have been the easy fix and would have broken exactly this."""
    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            if len(ran) == 2:                             # die after unit "a" committed
                raise RuntimeError("host struck")
            await dataset.push({"unit": unit.value})

    host = make_host(method)
    asyncio.run(host.run_batch({"units": ["a", "b"], "node_id": "n1"}))
    out = asyncio.run(host.run_batch({"units": ["a", "b"], "node_id": "n1"}))  # the retry

    assert out["results"][0] == {"unit": "a"}
    assert ran.count("a") == 1, "the committed unit must NOT have re-run on the retry"




def test_object_state_is_keyed_by_the_actor_id_and_outlives_the_batch():
    """object_state (ADR 0022 tier 4) is what makes a keyed dispatch a virtual object: written
    in one batch, still there in the NEXT batch of the same actor id — the lifetime `self.*`
    does not have, since a Session's memory dies with its scope."""
    from kontra import object_state

    from test_global_state import FakeEtagKV

    async def method(self, batch, dataset):
        async for unit in batch:
            await object_state.add_to_set("seen", unit.value)
            await dataset.push({"unit": unit.value,
                             "seen": sorted(await object_state.get("seen"))})

    host = make_host(method)
    kv = FakeEtagKV()
    host._etag_kv_shared = kv          # inject before the tier is first touched

    first = asyncio.run(host.run_batch({"units": ["first"], "node_id": "n1"}))
    second = asyncio.run(host.run_batch({"units": ["second"], "node_id": "n2"}))

    assert first["results"][0]["seen"] == ["first"]
    # A new batch: fresh `self.*`, a new commit map — and the key's state still reads back.
    assert second["results"][0]["seen"] == ["first", "second"]
    # Namespaced by actor NAME + actor ID, so it is the KEY's state, not the actor's.
    assert "kontra-object:t:run1-node1:seen" in kv.store


def test_the_two_cross_session_tiers_do_not_share_a_namespace():
    """Same class, same store, same author-facing ops — only the prefix separates them. If the
    host bound one store to both contextvars, every key-scoped write would leak to every key."""
    from kontra import global_state, object_state

    from test_global_state import FakeEtagKV

    async def method(self, batch, dataset):
        async for unit in batch:
            await global_state.set("cursor", "name-scoped")
            await object_state.set("cursor", "key-scoped")
            await dataset.push({"g": await global_state.get("cursor"),
                             "o": await object_state.get("cursor")})

    host = make_host(method)
    kv = FakeEtagKV()
    host._etag_kv_shared = kv

    out = asyncio.run(host.run_batch({"units": ["x"]}))
    assert out["results"][0] == {"g": "name-scoped", "o": "key-scoped"}
    assert "kontra-global:t:cursor" in kv.store
    assert "kontra-object:t:run1-node1:cursor" in kv.store


def test_both_tiers_share_one_store_client():
    """Two tiers must not mean two connections per session — same store, different prefixes —
    and an actor that touches neither must still open nothing."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push(unit.value)

    host = make_host(method)
    assert host._etag_kv_shared is None                 # nothing opened at construction
    host._etag_kv_shared = object()
    assert host._global_store()._kv is host._object_store()._kv


def test_a_failed_load_leaves_no_session_behind_for_the_next_attempt_to_reuse():
    """THE BUG THIS PINS, measured on `redditscrape-1787584680`.

    `_open` assigned `self._inst` BEFORE running `@actor.load`, so a load that raised left a
    fully-constructed but UNLOADED instance on the Session. `run_batch` opens only when
    `self._inst is None`, so the next attempt of the same Batch skipped `_open` entirely and ran
    the Method against a resource whose load had never completed.

    Live consequence: an OAuth actor whose `@actor.load` raised on a refused credential ran all
    three of its Units anyway. Each Unit re-minted the dead token once per search keyword — 202
    auth POSTs from one 3-Unit Batch — and every Unit "succeeded" emitting nothing, so the Run
    reported `completed` with an empty Dataset and an empty `failures` list.
    """
    opens, ran = [], []

    async def load(self):
        opens.append(1)
        raise RuntimeError("credentials refused")

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    host = make_host(method, load=load)

    # Attempt 1: the load raises, and it reaches the caller rather than being swallowed.
    with pytest.raises(RuntimeError):
        asyncio.run(host.run_batch({"units": ["a"]}))
    assert ran == [], "the Method ran despite a load that never completed"

    # Attempt 2 — what Temporal does when the activity is retried. It must try to LOAD again and
    # fail again, not silently proceed on the corpse of attempt 1.
    with pytest.raises(RuntimeError):
        asyncio.run(host.run_batch({"units": ["a"]}))
    assert opens == [1, 1], "the retry skipped @actor.load and ran on an unloaded resource"
    assert ran == [], "the Method ran against a resource whose load had raised"


def test_a_failed_load_still_gets_its_close_so_a_half_open_resource_is_released():
    """`@actor.load` that acquires two things and dies on the second must still get its
    `@actor.close`: the first thing is open, and only the author knows how to release it. The
    close runs BEFORE the instance is cleared, because it is the instance's own hook."""
    closed = []

    async def load(self):
        self.handle = "open"
        raise RuntimeError("second acquire failed")

    async def close(self):
        closed.append(getattr(self, "handle", None))

    async def method(self, batch, dataset):
        pass

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.methods = {"method": MethodRegistration(fn=method, name="method", fn_name="method")}
    reg.load_fn = load
    reg.close_fn = close
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    with pytest.raises(RuntimeError):
        asyncio.run(host.run_batch({"units": ["a"]}))
    assert closed == ["open"], "a half-open resource was dropped without its @actor.close"
