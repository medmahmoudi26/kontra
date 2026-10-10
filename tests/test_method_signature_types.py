"""A METHOD'S SIGNATURE IS ITS CONTRACT (PRD D2).

`async def enrich(self, batch: Batch[Product], dataset: Dataset[Enriched])` says what the Method
takes and emits as plainly as `takes=Product, emits=Enriched` does, so `@actor.method` reads it as
one. `takes=`/`emits=` stay accepted, win when given, and a disagreement between the two is a
warning naming both.
"""

from __future__ import annotations

import warnings
from dataclasses import dataclass

import pytest

from kontra import Batch, Dataset, actor
from kontra.actor import ActorRegistry, signature_types


@dataclass
class Product:
    url: str


@dataclass
class Enriched:
    title: str


@dataclass
class Other:
    x: int


def _registry() -> ActorRegistry:
    return ActorRegistry()


def test_the_signature_declares_takes_and_emits():
    reg = _registry()

    @reg.method
    async def enrich(self, batch: Batch[Product], dataset: Dataset[Enriched]) -> None: ...

    m = reg.methods["enrich"]
    assert (m.takes, m.emits) == (Product, Enriched)


def test_explicit_arguments_still_work_alone():
    reg = _registry()

    @reg.method(takes=Product, emits=Enriched)
    async def enrich(self, batch, dataset): ...

    m = reg.methods["enrich"]
    assert (m.takes, m.emits) == (Product, Enriched)


def test_agreeing_declarations_are_quiet():
    reg = _registry()
    with warnings.catch_warnings():
        warnings.simplefilter("error")

        @reg.method(takes=Product, emits=Enriched)
        async def enrich(self, batch: Batch[Product], dataset: Dataset[Enriched]) -> None: ...


def test_a_disagreement_warns_and_the_argument_wins():
    reg = _registry()
    with pytest.warns(UserWarning, match=r"takes=Other but the signature says Product"):

        @reg.method(takes=Other)
        async def enrich(self, batch: Batch[Product], dataset: Dataset[Enriched]) -> None: ...

    m = reg.methods["enrich"]
    assert (m.takes, m.emits) == (Other, Enriched)


def test_bare_and_unannotated_parameters_declare_nothing():
    reg = _registry()

    @reg.method
    async def a(self, batch: Batch, dataset: Dataset) -> None: ...

    @reg.method
    async def b(self, batch, dataset): ...

    for name in ("a", "b"):
        m = reg.methods[name]
        assert (m.takes, m.emits) == (None, None)


def test_a_forward_reference_resolves_once_the_class_exists():
    # `from __future__ import annotations` is on in this module, so the hint is a string; the class
    # it names is defined AFTER the decorator runs, which is the shape that raises NameError.
    reg = _registry()

    @reg.method
    async def later(self, batch: Batch[DefinedLater], dataset: Dataset[Enriched]) -> None: ...

    m = reg.methods["later"]
    assert m.hints_pending and m.takes is None
    globals()["DefinedLater"] = type("DefinedLater", (), {})
    try:
        assert m.resolved().takes is globals()["DefinedLater"]
        assert m.emits is Enriched and not m.hints_pending
    finally:
        del globals()["DefinedLater"]


@dataclass
class Clashes:
    #: `node` is a provenance column the framework stamps on every output row.
    node: str


def test_an_inferred_emits_type_is_checked_for_reserved_fields():
    reg = _registry()
    with pytest.raises(TypeError, match="node"):

        @reg.method
        async def m(self, batch: Batch[Product], dataset: Dataset[Clashes]) -> None: ...


def test_signature_types_reads_positions_not_names():
    async def m(self, units: Batch[Product], out: Dataset[Enriched]) -> None: ...

    assert signature_types(m) == (Product, Enriched, False)


def test_the_catalog_advertises_inferred_schemas():
    from internals.catalog import operations_of

    reg = _registry()

    @reg.method
    async def enrich(self, batch: Batch[Product], dataset: Dataset[Enriched]) -> None: ...

    (op,) = operations_of(reg)
    assert op["input"]["properties"]["url"]["type"] == "string"
    assert op["output"]["properties"]["title"]["type"] == "string"


def test_batch_and_dataset_are_the_actor_side_types():
    import kontra.batch
    import kontra.catalog

    assert Batch is kontra.batch.Batch and Dataset is kontra.batch.Dataset
    # The caller's handle on a Method's output is a different thing and keeps its own module.
    assert Batch is not kontra.catalog.Batch
    # And a collector Dataset still works with a type parameter on it.
    d = Dataset[Enriched]()
    assert d.records == []


def test_a_forward_reference_to_a_reserved_field_fails_when_it_resolves():
    # From the review: a hint naming a class defined below the Method skipped the reserved-field
    # check, so a `node` column was advertised and then refused by the materializer (GitHub #22).
    reg = _registry()

    @reg.method
    async def later(self, batch: Batch[Product], dataset: Dataset[LateClash]) -> None: ...

    m = reg.methods["later"]
    assert m.hints_pending
    globals()["LateClash"] = dataclass(type("LateClash", (), {"__annotations__": {"node": str}}))
    try:
        with pytest.raises(TypeError, match="node"):
            m.resolved()
    finally:
        del globals()["LateClash"]


def test_a_disagreement_found_on_resolution_still_warns():
    reg = _registry()

    @reg.method(takes=dict)
    async def later(self, batch: Batch[LateIn], dataset: Dataset[Enriched]) -> None: ...

    globals()["LateIn"] = type("LateIn", (), {})
    try:
        with pytest.warns(UserWarning, match="takes=dict but the signature says LateIn"):
            reg.methods["later"].resolved()
        assert reg.methods["later"].takes is dict
    finally:
        del globals()["LateIn"]


def test_equal_generic_aliases_are_not_a_disagreement():
    reg = _registry()
    with warnings.catch_warnings():
        warnings.simplefilter("error")

        @reg.method(takes=list[str])
        async def m(self, batch: Batch[list[str]], dataset: Dataset[Enriched]) -> None: ...


def test_an_explicit_reserved_emits_is_refused_even_when_the_signature_is_pending():
    reg = _registry()
    with pytest.raises(TypeError, match="node"):

        @reg.method(emits=Clashes)
        async def m(self, batch: Batch[NotYetDefined], dataset) -> None: ...
