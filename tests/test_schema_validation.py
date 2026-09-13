"""Per-actor I/O schemas are DERIVED from the typed input/output on the @actor.defn class
(pydantic), not hand-written JSON Schema. Good units pass; bad units are rejected."""

from pathlib import Path

import pytest

from internals.loader import load_actor
from internals.manifest import ManifestError, validate_units
from kontra.schema import schema_of

# A small, self-contained fixture actor (not an example) so schema derivation is tested
# without depending on the examples/ tree.
_FIXTURE_ECHO = Path(__file__).resolve().parent / "fixtures" / "echo"


def _echo_registry():
    return load_actor(str(_FIXTURE_ECHO)).registry


def test_input_schema_derived_from_type():
    schema = schema_of(_echo_registry().input_type)
    assert "msg" in schema["properties"]
    assert schema["required"] == ["msg"]


def test_valid_input_passes():
    reg = _echo_registry()
    validate_units([{"id": 1, "msg": "hi"}], schema_of(reg.input_type), context="input line")


def test_invalid_input_rejected():
    reg = _echo_registry()
    with pytest.raises(ManifestError, match="input line 1"):
        validate_units([{"id": 1}], schema_of(reg.input_type), context="input line")  # missing msg


def test_valid_output_passes():
    reg = _echo_registry()
    units = [{"id": 1, "msg": "hi", "shout": "HI", "by": "echoed"}]
    validate_units(units, schema_of(reg.output_type), context="output unit")


def test_invalid_output_rejected():
    reg = _echo_registry()
    with pytest.raises(ManifestError, match="output unit 1"):
        validate_units([{"msg": "hi", "shout": "HI"}], schema_of(reg.output_type), context="output unit")


def test_undeclared_schema_is_noop():
    validate_units([{"anything": 1}], None, context="x")  # no declared type -> no validation
