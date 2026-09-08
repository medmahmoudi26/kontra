"""Unit-testing a Method body — no host, no queue, no object store (ADR 0028).

A Method is `async def m(self, batch, dataset)`, and every parameter but `self` is something the
caller hands over — which is exactly what makes the body testable in isolation. `stub_batch([...])`
is the input the caller passes first; `collecting_dataset()` is the destination it passes second,
here backed by a list rather than the lake; `.records` is what the body pushed.

    from kontra.testing import stub_batch, collecting_dataset

    ds = collecting_dataset()
    await ask(inst, stub_batch([{"host": "a"}], takes=Pair), ds)
    assert ds.records == [{"host": "a", "ok": True}]

Before ADR 0028 an author had to reach into each `unit.out` and stitch the pieces together, or
stand up the engine; substituting the Dataset is the one-liner that replaces both.
"""

from __future__ import annotations

from typing import Any, Iterable, Optional

from kontra.batch import Batch, Dataset


class _NullSink:
    """The three-call sink a Batch drives, every call a no-op: a hostless Batch commits nowhere.
    Output never reaches `record` — `dataset.push` collects on the Dataset, not through the sink —
    so `enter`/`commit` only advance the iterator and `record` is here for completeness."""

    def enter(self, unit: Any) -> None:  # noqa: D401
        ...

    async def record(self, unit: Any, rec: Any) -> Any:
        return rec

    async def commit(self, unit: Any) -> None:
        ...


def stub_batch(values: Iterable[Any], *, takes: Optional[type] = None) -> Batch:
    """A Batch over plain `values` (each coerced to `takes` when given), committing nowhere — the
    input half of a Method-body test with no host behind it."""
    return Batch(_NullSink(), list(enumerate(values)), takes)


def collecting_dataset() -> Dataset:
    """A Dataset that collects instead of persisting — read `.records` for what the body pushed,
    in push order. The output half of a hostless Method-body test."""
    return Dataset()
