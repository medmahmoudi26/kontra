"""Many Methods per Actor, named by the dispatch (ADR 0023 §5, §9).

An Actor used to have exactly one Method, so chaining a transformation meant deploying a
second actor and an edge between them. These tests describe the registry that replaces
that: any number of named Methods, resolved per dispatch, sharing one loaded Session.
"""

import asyncio
from dataclasses import dataclass

import pytest

from kontra import ActorRegistry, MethodRegistration
from internals.engine import build_session_factory
from test_actor_engine import FakeKV


def registry_with(*fns, name="t"):
    reg = ActorRegistry()
    reg.actor_name = name
    for f in fns:
        reg.method(f)
    return reg


def test_two_methods_register_under_their_function_names_in_order():
    async def crawl(self, unit): ...

    async def extract(self, unit): ...

    reg = registry_with(crawl, extract)
    assert list(reg.methods) == ["crawl", "extract"]
    assert reg.methods["crawl"].fn is crawl


def test_a_named_dispatch_picks_that_method():
    async def crawl(self, unit): ...

    async def extract(self, unit): ...

    reg = registry_with(crawl, extract)
    assert reg.resolve_method("extract").fn is extract


def test_the_sole_method_needs_no_name():
    async def crawl(self, unit): ...

    assert registry_with(crawl).resolve_method().fn is crawl


def test_an_unnamed_dispatch_against_several_methods_raises_rather_than_guessing():
    # Declaration order is not a tiebreak: a caller who forgot the name would otherwise
    # silently get whichever the author happened to write first.
    async def crawl(self, unit): ...

    async def extract(self, unit): ...

    reg = registry_with(crawl, extract)
    with pytest.raises(TypeError, match="must name one"):
        reg.resolve_method()


def test_an_unknown_method_name_says_what_does_exist():
    async def crawl(self, unit): ...

    reg = registry_with(crawl)
    with pytest.raises(TypeError, match="have: crawl"):
        reg.resolve_method("craw")


def test_two_methods_cannot_claim_one_dispatch_name():
    reg = ActorRegistry()

    @reg.method
    async def crawl(self, unit): ...

    with pytest.raises(TypeError, match="declared twice"):

        @reg.method(name="crawl")
        async def crawl_v2(self, unit): ...


def test_name_overrides_the_dispatch_name_not_the_function():
    reg = ActorRegistry()

    @reg.method(name="crawl")
    async def _crawl_impl(self, unit): ...

    assert list(reg.methods) == ["crawl"]
    assert reg.methods["crawl"].fn_name == "_crawl_impl"


def test_a_generator_cannot_be_a_method():
    """ADR 0028 §3: push is a call on the dataset, so a Method is an ordinary async function. A
    generator would be built and never iterated — a Batch that silently pushes nothing — so it is
    rejected where the author can still see it, at import."""
    reg = ActorRegistry()

    with pytest.raises(TypeError, match="not a generator|is a generator"):

        @reg.method
        async def crawl(self, batch, dataset):
            yield {"never": "emitted"}


def test_a_load_only_actor_resolves_to_no_method():
    assert ActorRegistry().resolve_method() is None


def test_two_methods_share_one_loaded_session():
    """The point of §9: `extract` sees the resource `load` opened and what `crawl` left on
    `self`. Two Methods of one Actor are one process and one instance — which is why an
    Activity is just an Actor with one Method and no load/close."""

    async def load(self):
        self.resource = "browser"
        self.seen = []

    async def crawl(self, batch, dataset):
        async for unit in batch:
            self.seen.append(unit.value)
            await dataset.push({"crawled": unit.value, "with": self.resource})

    async def extract(self, batch, dataset):
        # Sees the resource from load AND the accumulation from the previous Method call.
        async for unit in batch:
            await dataset.push({"extracted": unit.value, "after": list(self.seen),
                             "with": self.resource})

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.load_fn = load
    for f in (crawl, extract):
        reg.method(f)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    async def drive():
        a = await host.run_batch(
            {"units": ["a"], "method": "crawl", "run_id": "r1", "node_id": "n1"})
        b = await host.run_batch(
            {"units": ["b"], "method": "extract", "run_id": "r2", "node_id": "n2"})
        return a, b

    first, second = asyncio.run(drive())
    assert first["results"] == [{"crawled": "a", "with": "browser"}]
    assert second["results"] == [{"extracted": "b", "after": ["a"], "with": "browser"}]


def test_a_second_method_under_one_batch_owner_does_not_replay_the_first():
    """The hazard §17 exists to close.

    Two Methods dispatched under the SAME run/node — which is what a caller's `async with`
    scope does once it holds a Session across calls — used to collide in the commit map at
    `u0`. The second Method never ran and the caller got the first Method's output: full,
    plausible and wrong, the shape 0022 §3 was written to stop. Keying a committed Unit by
    the BATCH's content hash plus its index is what separates them.
    """

    async def crawl(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"crawled": unit.value})

    async def extract(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"extracted": unit.value})

    reg = ActorRegistry()
    reg.actor_name = "t"
    for f in (crawl, extract):
        reg.method(f)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    async def drive():
        await host.run_batch({"units": ["a"], "method": "crawl"})
        return await host.run_batch({"units": ["b"], "method": "extract"})

    assert asyncio.run(drive())["results"] == [{"extracted": "b"}]


def test_two_methods_handed_the_SAME_units_are_two_batches():
    """The sharper half of the same hazard: identical Units under one owner. Nothing in the
    payload differs except which Method was named, so a hash over the units alone would still
    collide and `extract` would replay `crawl`'s output."""

    async def crawl(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"crawled": unit.value})

    async def extract(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"extracted": unit.value})

    reg = ActorRegistry()
    reg.actor_name = "t"
    for f in (crawl, extract):
        reg.method(f)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    async def drive():
        first = await host.run_batch({"units": ["a"], "method": "crawl"})
        return first, await host.run_batch({"units": ["a"], "method": "extract"})

    first, second = asyncio.run(drive())
    assert first["results"] == [{"crawled": "a"}]
    assert second["results"] == [{"extracted": "a"}]


def test_the_dispatched_name_selects_which_method_runs():
    calls = []

    async def crawl(self, batch, dataset):
        async for unit in batch:
            calls.append(("crawl", unit.value))

    async def extract(self, batch, dataset):
        async for unit in batch:
            calls.append(("extract", unit.value))

    reg = ActorRegistry()
    reg.actor_name = "t"
    for f in (crawl, extract):
        reg.method(f)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    asyncio.run(host.run_batch({"units": ["x"], "method": "extract"}))
    assert calls == [("extract", "x")]


def test_a_registration_carries_both_its_dispatch_name_and_its_function_name():
    reg = ActorRegistry()

    @reg.method(name="fetch")
    async def http_get(self, unit): ...

    m = reg.methods["fetch"]
    assert isinstance(m, MethodRegistration)
    assert (m.name, m.fn_name) == ("fetch", "http_get")


# ---------------------------------------------------------------------------------------------
# Typed I/O — what `takes=` and `emits=` buy the AUTHOR, not just the catalog
# ---------------------------------------------------------------------------------------------


@dataclass
class Target:
    host: str
    port: int = 443


@dataclass
class Probe:
    host: str
    open: bool


def test_a_declared_method_gets_a_typed_unit_and_emits_a_typed_record():
    """The ergonomic half of ADR 0023 §9. Declaring the signature used to buy only a catalog
    entry; the author still wrote `unit.value["host"]` and `{**unit.value, ...}`. Now the
    declaration is what the BODY reads and writes."""
    seen = []

    async def probe(self, batch, dataset):
        async for unit in batch:
            seen.append(unit.value)                       # a Target, not a dict
            await dataset.push(Probe(host=unit.value.host, open=unit.value.port == 443))

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.method(takes=Target, emits=Probe)(probe)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    out = asyncio.run(host.run_batch(
        {"units": [{"host": "a.example"}, {"host": "b.example", "port": 80}],
         "method": "probe"}))

    assert [type(v) for v in seen] == [Target, Target]
    assert seen[0].host == "a.example" and seen[0].port == 443   # the default applied
    # ...and the record went to the wire as plain JSON, because the blob plane hashes it.
    assert out["results"] == [
        {"host": "a.example", "open": True},
        {"host": "b.example", "open": False},
    ]


def test_an_undeclared_method_still_sees_the_plain_dict():
    """Declaring types is optional and stays optional — the dict path is not deprecated by
    this, it is just no longer the only one."""
    seen = []

    async def raw(self, batch, dataset):
        async for unit in batch:
            seen.append(unit.value)
            await dataset.push({"got": unit.value["host"]})

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.method(raw)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())
    out = asyncio.run(host.run_batch({"units": [{"host": "a"}], "method": "raw"}))

    assert seen == [{"host": "a"}]
    assert out["results"] == [{"got": "a"}]


def test_a_unit_that_does_not_fit_the_declared_type_isolates_itself():
    """WHY the coercion is lazy. A malformed record must fail its own Unit at the iterator
    boundary (ADR 0023 §13), not the whole Batch — an eager pass over the Batch would lose
    every good Unit because one payload was wrong."""
    ran = []

    async def probe(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value.host)
            await dataset.push(Probe(host=unit.value.host, open=True))

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.method(takes=Target, emits=Probe)(probe)
    host = build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())

    out = asyncio.run(host.run_batch(
        {"units": [{"host": "good"}, {"nope": 1}, {"host": "also-good"}], "method": "probe"}))

    assert ran == ["good", "also-good"], "the good Units still ran"
    assert len(out["failures"]) == 1
    assert out["failures"][0]["unit"] == {"nope": 1}
    assert [r["host"] for r in out["results"]] == ["good", "also-good"]
