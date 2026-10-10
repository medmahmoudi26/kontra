"""The shipped `nscheck` workflow RUNS against the SDK it imports — fleet scope and all.

THIS REPLACES A SPELLING CHECK. `test_documented_kwargs.py` read the README, the wiki and these
workflow files with a regex, pulled out every `fleet.hold(…)`/`f.place(…)` keyword, and compared the
spellings with `inspect.signature`. Over prose that is a docs lint, and it is gone with the rest of
the wording tests (PRD §8). Over THIS file it was guarding a behaviour — "the code a person copies
calls the SDK with keywords the SDK takes" — and a behaviour is checked by running it: a keyword
`fleet.hold` stopped taking is a `TypeError` here, on the line that passes it, with no pattern in
between that could stop matching.

WHAT RUNS FOR REAL AND WHAT DOES NOT.

  * `kontra.fleet` is the real module. `FleetScope.program()` answers its Temporal calls (Lease,
    resolve, converge, pollers) and records them, exactly as it does for `test_fleet_hold_place.py`,
    so `hold` → `place` → `ready` → scope exit all execute.
  * `kontra.catalog` is replaced in the workflow's namespace, because its half of the run is
    dispatching Methods to an Actor and paging Parquet, and faking that wire here would be a second
    copy of `test_workflows_client.py`. But every catalog call is BOUND AGAINST THE REAL SIGNATURE
    before it is answered, so the fake refuses exactly what the SDK refuses — `inspect.signature`
    raises the same `TypeError` the call would.

THE FIXTURE IS A COPY (see `test_workflow_catalog.py`'s header for why it lives here at all). When
the SDK's fleet surface changes — PRD D3 renames it to `fleet.hold(size=…, nodes=…)` and
`place(…, replicas=…)` — this fails, and the fixture moves with it. That failure is the point.
"""

from __future__ import annotations

import importlib.util
import inspect
import sys
from pathlib import Path
from typing import Any

from kontra import catalog, fleet
from fleetscope import FleetScope

NSCHECK = Path(__file__).resolve().parent.parent / "testdata" / "workflows" / "nscheck" / "workflow.py"


def _bind(real: Any, *args: Any, **kwargs: Any) -> None:
    """Refuse what `real` refuses: the `TypeError` a call with these arguments would raise."""
    inspect.signature(real).bind(*args, **kwargs)


class _Rows:
    """A Batch's two answers a workflow reads without a fetch: its length, and a re-page."""

    def __init__(self, n: int) -> None:
        self.n = n

    def __len__(self) -> int:
        return self.n

    def batches(self, size: int, **kw: Any):
        _bind(catalog.Batch.batches, self, size, **kw)
        return self._pages()

    async def _pages(self):
        if self.n:
            yield self


class _Dataset:
    def __init__(self, record: list[str], name: str, rows: int = 0) -> None:
        self.record, self.name, self.rows = record, name, rows

    def batches(self, size: int, **kw: Any):
        _bind(catalog.DatasetHandle.batches, self, size, **kw)
        self.record.append(f"page {self.name}")
        return _Rows(self.rows)._pages()

    async def insert_from(self, source: Any, **kw: Any) -> int:
        _bind(catalog.DatasetHandle.insert_from, self, source, **kw)
        self.record.append(f"promote {source.name} -> {self.name}")
        return 1


class _Actor:
    """An Actor handle whose Methods answer at once. A Method's signature is `(units, dataset=None,
    **options)` on the real `MethodCall`, so that is what each call is bound against."""

    def __init__(self, record: list[str], name: str) -> None:
        self.record, self.name = record, name

    async def __aenter__(self) -> "_Actor":
        return self

    async def __aexit__(self, *exc: Any) -> bool:
        return False

    def __getattr__(self, method: str):
        if method.startswith("_"):
            raise AttributeError(method)

        async def call(*args: Any, **kw: Any):
            _bind(catalog.MethodCall, self, method, *args, **kw)
            self.record.append(f"{self.name}.{method}")
            return _Rows(2), _Rows(0)

        return call


class _Catalog:
    """`kontra.catalog`, as far as one run of nscheck reaches into it."""

    def __init__(self, rows: int) -> None:
        self.record: list[str] = []
        self.rows = rows
        outer = self

        class _Datasets:
            def __call__(self, name: str, **kw: Any) -> _Dataset:
                _bind(catalog.dataset, name, **kw)
                return _Dataset(outer.record, name, rows=outer.rows)

            def temp(self, **kw: Any):
                _bind(catalog.dataset.temp, **kw)
                tmp = _Dataset(outer.record, "tmp")

                class _Scope:
                    async def __aenter__(self):
                        return tmp

                    async def __aexit__(self, *exc: Any) -> bool:
                        return False

                return _Scope()

        self.dataset = _Datasets()

    def actor(self, name: str, *args: Any, **kw: Any) -> _Actor:
        _bind(catalog.actor, name, *args, **kw)
        return _Actor(self.record, name)


def _load_nscheck(name: str, path: Path = NSCHECK):
    """By path, under a name of its own — every workflow folder holds a `workflow.py`."""
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def test_the_shipped_nscheck_workflow_runs_its_fleet_scope_end_to_end(monkeypatch):
    module = _load_nscheck("shipped_runs_nscheck")
    fake = _Catalog(rows=3)
    monkeypatch.setattr(module, "catalog", fake)
    # The workflow's `fleet` IS the SDK's. Asserted rather than assumed: a fixture that grew its own
    # import alias, or a test that patched it, would leave this file running a fake end to end.
    assert module.fleet is fleet

    scope = FleetScope(machines=4)
    out = scope.program(lambda: module.NsCheck().run({}))

    # THE WHOLE BODY RAN: a page, both Methods, the promotion after the scope closed.
    assert fake.record == [
        "page domains",
        "nscheck.delegation",
        "nscheck.ask",
        "promote tmp -> lame",
    ], fake.record
    assert out == {
        "into": "lame", "machines": 4, "bundle": "a" * 12,
        "pairs": 2, "checked": 2, "promoted": 1, "dropped": 0,
    }

    # …AND THE FLEET SCOPE DID WHAT ITS ARGUMENTS SAY. The Lease is taken on the tag's Fleet, the
    # placement converge carries the fixture's own density, `ready()` asked the pollers question,
    # and the Lease is dropped on the way out.
    order = scope.order()
    assert order[0] == fleet.HOLD_LEASE_ACTIVITY, order
    assert fleet.QUEUE_POLLERS_ACTIVITY in order, order
    assert order[-1] == fleet.DROP_LEASE_ACTIVITY, order
    assert scope.activity(fleet.HOLD_LEASE_ACTIVITY)["stackFqn"].endswith("/dns")
    converges = [c.arg["args"] for c in scope.calls if c.kind == "child"]
    assert converges and all(a["machines"] == 4 for a in converges), converges
    placed = [a for a in converges if a.get("bundleUrl")]
    assert placed, f"no converge carried a placement: {scope.summary()}"
    assert placed[-1]["placements"][0]["maxSessions"] == 8, placed[-1]


def test_a_keyword_the_sdk_does_not_take_fails_the_run(monkeypatch, tmp_path):
    """THE GUARD ON THE GUARD. The property this file holds the fixture to is "a keyword nobody
    takes is a TypeError" — so the same fixture, with one keyword respelled, must fail here through
    the real `Fleet.place`, or the test above would pass against any SDK."""
    source = NSCHECK.read_text(encoding="utf-8")
    assert source.count("sessions=_num(") == 1, "the fixture no longer spells place()'s density this way"
    misspelled = tmp_path / "workflow.py"
    misspelled.write_text(source.replace("sessions=_num(", "density=_num("), encoding="utf-8")

    module = _load_nscheck("shipped_runs_nscheck_misspelled", misspelled)
    monkeypatch.setattr(module, "catalog", _Catalog(rows=3))
    try:
        FleetScope(machines=4).program(lambda: module.NsCheck().run({}))
    except TypeError as e:
        assert "density" in str(e), e
    else:  # pragma: no cover - the assertion IS the point
        raise AssertionError("f.place(density=…) was accepted; the run above proves nothing")


def test_the_catalog_fake_refuses_what_the_sdk_refuses():
    """The same guard for the faked half: the fake binds against the real signature, so a
    keyword the real `DatasetHandle.batches` refuses is refused here too."""
    fake = _Catalog(rows=3)
    try:
        fake.dataset("domains").batches(100, order="domain")  # `order_by=` is the real name
    except TypeError as e:
        assert "order" in str(e), e
    else:  # pragma: no cover - the assertion IS the point
        raise AssertionError("the catalog fake accepted a keyword the SDK refuses")
