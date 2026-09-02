"""Derive a JSON Schema (Draft 2020-12) from a typed actor I/O definition.

An actor declares its input/output/params as dataclasses (or pydantic models) on the
`@actor.defn` class; this is the ONE place those types become JSON Schema, via pydantic.
The generated schema feeds the orchestrator catalog (which gates it — ADR 0027), the UI ports,
and runtime validation — replacing hand-written ActorInput.json/ActorOutput.json.

Kept out of the workflow sandbox path on purpose: this imports pydantic and is only ever
called off the hot path (build/registration/dispatch), never inside a workflow.
"""

from __future__ import annotations

from typing import Any, Optional

from pydantic import TypeAdapter


def coerce(tp: Optional[type], value: Any) -> Any:
    """Build an instance of `tp` from a plain JSON value — what makes `unit.value.host` work.

    `None` for `tp` (a Method that declared no `takes`) passes the value through untouched, so
    an undeclared Method still sees the raw dict it always did.

    Raises when the value does not fit. That is deliberate and it is WHY this is called lazily
    from `Unit.value` rather than eagerly over the Batch: at the iterator boundary the raise is
    attributed to the Unit that carried the bad payload and isolates it (ADR 0023 §13), where an
    eager pass would fail the whole Batch because one record was malformed.
    """
    if tp is None:
        return value
    return TypeAdapter(tp).validate_python(value)


def to_jsonable(value: Any) -> Any:
    """Reduce a pushed record to plain JSON — what makes `dataset.push(Page(...))` work.

    A dataclass or pydantic instance becomes its dict; anything already JSON-shaped is returned
    as-is, so the dict path an author may still prefer costs nothing and keeps working.

    The blob plane hashes `json.dumps(..., sort_keys=True)` to content-address a record, so what
    lands here must be deterministic — the same reason ADR 0015 asks authors for
    content-deterministic records.
    """
    if value is None or isinstance(value, (str, int, float, bool, list, dict)):
        return value
    return TypeAdapter(type(value)).dump_python(value, mode="json")


def schema_of(tp: Optional[type]) -> Optional[dict[str, Any]]:
    """JSON Schema for a dataclass/pydantic type, or None when `tp` is None (the actor
    declared no type for that slot — e.g. a DIY actor with no schema)."""
    if tp is None:
        return None
    return TypeAdapter(tp).json_schema()
