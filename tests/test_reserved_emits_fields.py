"""An `emits=` type may not declare a framework column — GitHub #22, refused at import.

WHY AT IMPORT AND NOT AT INSERT. The collision is invisible at author time (no schema check),
invisible at Method time (the push succeeds), and surfaces as `Binder Error: Duplicate column name
"node" in INSERT` inside a retrying materializer activity — attempt 8, run still reading RUNNING,
nothing in the actor's log or the workflow's. The only way to see it was
`temporal workflow describe | jq .pendingActivities`.

`node` is the one that actually happens: it is the obvious name for "which machine produced this
row", which is exactly what the framework is also recording.

THIS FILE ALSO PINS THE TWO COPIES OF THE SET TOGETHER. The list lives in Python (for this check)
and in TypeScript (`data/parquet.ts:RESERVED_OUTPUT_COLUMNS`, where the SELECT is), and nothing but
a test can notice the orchestrator growing a sixth column.
"""

import re
from dataclasses import dataclass
from pathlib import Path

import pytest

from kontra.actor import RESERVED_OUTPUT_FIELDS, ActorRegistry, _reserved_emits_fields


def _actor() -> ActorRegistry:
    # A fresh registry per test: `methods` is per-instance, so two tests declaring the same
    # dispatch name would otherwise collide on the duplicate-name guard rather than the one
    # under test.
    return ActorRegistry()


def test_the_reserved_set_is_what_the_orchestrator_stamps():
    """The two copies, held together. If `parquet.ts` adds a column and this does not, an author
    gets the eight-retry binder error again with no warning at import."""
    ts = Path(__file__).resolve().parents[1] / "control/orchestrator/src/data/parquet.ts"
    src = ts.read_text()
    block = re.search(
        r"RESERVED_OUTPUT_COLUMNS:\s*readonly string\[\]\s*=\s*\[(.*?)\]", src, re.S
    )
    assert block, "RESERVED_OUTPUT_COLUMNS not found in parquet.ts"
    names = tuple(re.findall(r"'([^']+)'", block.group(1)))
    assert names == RESERVED_OUTPUT_FIELDS


def test_a_dataclass_declaring_node_is_refused_at_decoration():
    """The literal repro from the issue."""

    @dataclass
    class Beat:
        label: str
        slept: float
        node: str  # <- collides

    a = _actor()
    with pytest.raises(TypeError) as err:

        @a.method(emits=Beat)
        async def tick(self, batch, dataset):  # pragma: no cover - never registered
            ...

    msg = str(err.value)
    assert "'node'" in msg
    # The message must answer the question the binder error could not: which names are taken.
    for name in RESERVED_OUTPUT_FIELDS:
        assert name in msg


def test_every_reserved_name_is_refused_not_just_node():
    """`node` is the one that happens; the rule is the set."""
    for reserved in RESERVED_OUTPUT_FIELDS:
        ns = {"__annotations__": {"payload": str, reserved: str}}
        emits = type("Row", (), ns)
        a = _actor()
        with pytest.raises(TypeError, match=re.escape(repr(reserved))):

            @a.method(emits=emits)
            async def go(self, batch, dataset):  # pragma: no cover
                ...


def test_an_ordinary_emits_type_still_registers():
    """Non-vacuous: the check must not refuse the normal case."""

    @dataclass
    class Page:
        url: str
        status: int
        worker: str  # the workaround the canary took — not reserved

    a = _actor()

    @a.method(emits=Page)
    async def crawl(self, batch, dataset):  # pragma: no cover
        ...

    assert "crawl" in a.methods


def test_no_emits_type_still_registers():
    """`emits=` is optional and always was."""
    a = _actor()

    @a.method()
    async def bare(self, batch, dataset):  # pragma: no cover
        ...

    assert "bare" in a.methods


def test_a_type_it_cannot_read_is_left_alone():
    """QUIET ON WHAT IT CANNOT PARSE. Refusing to serve because the helper could not introspect a
    type would turn a diagnostic into an outage — and the materializer still catches the collision.
    """
    assert _reserved_emits_fields(None) == []
    assert _reserved_emits_fields(dict) == []
    assert _reserved_emits_fields(int) == []


def test_it_reports_every_colliding_field_not_only_the_first():
    """An author who used two reserved names should have to read the error once."""

    @dataclass
    class Bad:
        node: str
        run_id: str

    a = _actor()
    with pytest.raises(TypeError) as err:

        @a.method(emits=Bad)
        async def two(self, batch, dataset):  # pragma: no cover
            ...

    assert "'node'" in str(err.value)
    assert "'run_id'" in str(err.value)
