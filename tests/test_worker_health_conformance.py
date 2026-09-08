"""The PYTHON HOST's arm of shared/conformance/workerhealth.json — one of two WRITERS of the two series
the **Warden** divides to decide a **Worker** is sick but not dead.

WHY A CORPUS FOR TWO COUNTER NAMES. The names are spelled independently here and in
`runtime/go/engine/metrics.go`, and read a third time on a **Machine** by `cli/warden/sickworker.go`. A
drift between them has no loud failure and no red test on either side: each host renders a
perfectly valid exposition and passes its own assertions, and the only symptom is a Warden dividing
by an absent series on every Worker of one language — which surfaces as a `loads` chip stuck on
`unknown`, indistinguishable from an actor host that was never instrumented. That is ADR 0035 rule
two, and this repo has already paid for it once: the Batch content hash was pinned by a hand-copied
golden on the one input class where Go and Python could not differ, and the two SDKs hashed the same
Batch to different keys for months.

THE COUNTERS THIS FILE IS ABOUT ARE NOT THE ONES BESIDE THEM, and the difference is the whole
point. `kontra_batches_total` has existed since the incident with **no caller in either host**, so
the ratio the fleet dashboard plots divides by a permanent zero; `count_reload()` has no caller in
THIS host at all, and the crawler that caused the incident is a Python actor. Both are green in
their own tests. So the last test here drives the ENGINE and never `count_load` directly, because
that is the only assertion that can tell a wired counter from those two.
"""
from __future__ import annotations

import asyncio
import json
from pathlib import Path

import pytest

from internals import metrics

FIXTURE = Path(__file__).resolve().parents[1] / "shared" / "conformance" / "workerhealth.json"
DOC = json.loads(FIXTURE.read_text(encoding="utf-8"))


@pytest.fixture(autouse=True)
def _reset_counters():
    """The module's counters are process-global, which is right for a metrics endpoint and wrong
    for a test that runs after another one."""
    metrics._loads = 0.0          # noqa: SLF001 - the module has no reset and does not need one in production
    metrics._load_failures = 0.0  # noqa: SLF001
    yield


def test_the_corpus_still_names_both_counters() -> None:
    """A corpus that lost half its contract passes every renderer, including one that emits
    nothing."""
    assert DOC["metrics"]["loads"] and DOC["metrics"]["failures"]
    assert DOC["metrics"]["loads"] != DOC["metrics"]["failures"]
    assert set(DOC["metrics"]["labels"]) == {"actor", "version"}


def test_the_python_host_renders_the_series_the_corpus_names() -> None:
    metrics.count_load(ok=True)
    metrics.count_load(ok=False)
    metrics.count_load(ok=False)
    body = metrics.render("webcrawl", "0.2.0")

    loads = DOC["metrics"]["loads"]
    failures = DOC["metrics"]["failures"]
    assert f'{loads}{{actor="webcrawl",version="0.2.0"}} 3' in body
    assert f'{failures}{{actor="webcrawl",version="0.2.0"}} 2' in body
    for label in DOC["metrics"]["labels"]:
        assert f'{label}="' in body


def test_both_counters_are_emitted_at_zero() -> None:
    """A Worker that has loaded nothing must still emit both series.

    metrics.py already argues this for `kontra_isolated_units_total` — an absent series renders
    identically to no data — and the consequence here is sharper than a dashboard reading oddly.
    A Warden that finds no `..._loads_total` reports `cannot tell`, so without an explicit zero a
    Worker that has genuinely attempted no loads is indistinguishable from an actor host too old
    to carry the counter. Both are `cannot tell`, but only one of them clears when work arrives.
    """
    body = metrics.render("webcrawl", "0.2.0")
    assert f'{DOC["metrics"]["loads"]}{{actor="webcrawl",version="0.2.0"}} 0' in body
    assert f'{DOC["metrics"]["failures"]}{{actor="webcrawl",version="0.2.0"}} 0' in body


def test_a_failed_load_advances_both_counters() -> None:
    """THE DENOMINATOR CANNOT BE HALF-WIRED, which is why `count_load` takes a flag rather than
    there being two functions. `count_batch()` is the cautionary case: it exists, it renders, and
    nothing calls it — so the expression the dashboard plots divides by zero forever."""
    metrics.count_load(ok=False)
    assert (metrics._loads, metrics._load_failures) == (1.0, 1.0)  # noqa: SLF001


def test_a_load_that_returned_is_not_a_failure() -> None:
    metrics.count_load(ok=True)
    assert (metrics._loads, metrics._load_failures) == (1.0, 0.0)  # noqa: SLF001


def test_the_engine_is_what_advances_the_load_counters() -> None:
    """THE COUNTER IS WIRED, which is the one thing its three neighbours are not.

    Drives `_open` through the real session factory and then reads the counters. Calling
    `count_load` here would prove exactly what `metrics_test.go`'s
    `TestReloadCounterCarriesTheSickWorkerSignature` proves about `countReload`/`countBatch`,
    which is nothing about a running Fleet.
    """
    loaded: list[str] = []

    async def load(self):  # noqa: ANN001, ANN202
        loaded.append("ok")

    async def method(self, batch, dataset):  # noqa: ANN001, ANN202
        async for unit in batch:
            await dataset.push({"seen": unit.value})

    host = _make_host(method, load=load)
    asyncio.run(host.run_batch({"units": ["x"]}))

    # The control, first: if the load never ran, everything below is vacuous.
    assert loaded == ["ok"]
    assert metrics._loads == 1.0, "the engine opened the resource and the denominator did not move"  # noqa: SLF001
    assert metrics._load_failures == 0.0  # noqa: SLF001


def test_a_load_that_raises_is_counted_by_the_engine_as_a_failed_load() -> None:
    """The round-3 shape at its smallest: the load raised, and the numerator moved WITH the
    denominator. Both, because a numerator that outran its denominator would produce a ratio
    above 1 and a `sick` verdict on a Worker that never attempted a load."""

    async def load(self):  # noqa: ANN001, ANN202
        raise RuntimeError("the resource is unreachable")

    async def method(self, batch, dataset):  # noqa: ANN001, ANN202
        async for unit in batch:
            await dataset.push({"seen": unit.value})

    host = _make_host(method, load=load)
    with pytest.raises(RuntimeError):
        asyncio.run(host.run_batch({"units": ["x"]}))

    assert metrics._loads == 1.0  # noqa: SLF001
    assert metrics._load_failures == 1.0  # noqa: SLF001


def _make_host(method, load=None):  # noqa: ANN001, ANN202
    """The engine harness `tests/test_actor_engine.py` uses, kept to one construction path.

    Duplicated rather than imported because importing a private fixture out of another test
    module makes two files fail for one reason, and this one is about the counters."""
    from kontra import ActorRegistry, MethodRegistration  # noqa: PLC0415
    from internals.engine import build_session_factory  # noqa: PLC0415

    class FakeKV:
        def __init__(self):
            self.d = {}

        async def get(self, field, default=None):  # noqa: ANN001, ANN202
            return self.d.get(field, default)

        async def set(self, field, value):  # noqa: ANN001, ANN202
            self.d[field] = value

        async def delete(self, field):  # noqa: ANN001, ANN202
            return self.d.pop(field, None) is not None

        async def touch(self):
            pass

        async def drop(self):
            self.d.clear()

    reg = ActorRegistry()
    reg.actor_name = "t"
    reg.methods = {method.__name__: MethodRegistration(
        fn=method, name=method.__name__, fn_name=method.__name__)}
    reg.load_fn = load
    return build_session_factory(reg, store=None)("run1-node1", kv=FakeKV())
