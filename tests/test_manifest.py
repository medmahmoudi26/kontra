"""actor.json is identity-only now (name/version); I/O schemas come from the typed
@actor.defn class (see test_schema_validation.py). This covers identity loading + the
param-resolution helpers, which take a schema DICT (derived from the params type)."""

from pathlib import Path

import pytest

from internals.manifest import ManifestError, apply_param_defaults, load_manifest, resolve_params


@pytest.fixture
def echo_dir():
    return Path(__file__).resolve().parent / "fixtures" / "echo"


def test_load_identity(echo_dir):
    m = load_manifest(echo_dir, actor_name="echo")
    assert m is not None
    assert (m.name, m.version) == ("echo", "0.1.0")


def test_missing_manifest_returns_none(tmp_path):
    assert load_manifest(tmp_path) is None


def test_malformed_manifest_json(tmp_path):
    (tmp_path / "actor.json").write_text("{not json")
    with pytest.raises(ManifestError, match="invalid JSON"):
        load_manifest(tmp_path)


def test_unsupported_schema_version(tmp_path):
    (tmp_path / "actor.json").write_text('{"schemaVersion":"kontra.actor.v9","name":"x","version":"1"}')
    with pytest.raises(ManifestError, match="unsupported schemaVersion"):
        load_manifest(tmp_path)


def test_name_mismatch_fails(tmp_path):
    (tmp_path / "actor.json").write_text('{"schemaVersion":"kontra.actor.v1","name":"wrong","version":"1"}')
    with pytest.raises(ManifestError, match="does not match"):
        load_manifest(tmp_path, actor_name="right")


# --- param resolution (schema is a dict derived from the params type) ---

PARAMS_SCHEMA = {
    "type": "object",
    "additionalProperties": False,
    "properties": {"headless": {"type": "boolean", "default": False}},
}


def test_apply_param_defaults_fills_unset_keys():
    schema = {"properties": {"headless": {"default": False}, "region": {"default": "us"}}}
    assert apply_param_defaults({"headless": True}, schema) == {"headless": True, "region": "us"}


def test_resolve_params_applies_default_then_validates():
    assert resolve_params({}, PARAMS_SCHEMA) == {"headless": False}
    assert resolve_params({"headless": True}, PARAMS_SCHEMA) == {"headless": True}


def test_resolve_params_rejects_unknown_key():
    with pytest.raises(ManifestError, match="params"):
        resolve_params({"headles": True}, PARAMS_SCHEMA)


def test_resolve_params_rejects_wrong_type():
    with pytest.raises(ManifestError, match="params"):
        resolve_params({"headless": "yes"}, PARAMS_SCHEMA)


def test_resolve_params_passthrough_when_no_schema():
    assert resolve_params({"anything": 1}, None) == {"anything": 1}
