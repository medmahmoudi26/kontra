"""Actor manifest (identity + resources) + JSON Schema validation helpers.

`actor.json` carries identity — `{schemaVersion, name, version}` — and, optionally, what the
actor needs to run: `resources: {cpus, memory}` (PRD §6), or `needs` (ADR 0040), its alias.
The actor's I/O schemas are DERIVED from the typed `input`/`output`/`params` on the
`@actor.defn` class (see kontra.schema), not from ActorInput.json/ActorOutput.json files. The
validators here take a schema DICT (whatever `schema_of(...)` produced), so they're
agnostic to where the schema came from.
"""

from __future__ import annotations

import json
import math
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional

from kontra.version import CONTRACT_VERSION

MANIFEST_FILENAME = "actor.json"
# Wire string MUST stay byte-identical: "kontra.actor.v1".
SCHEMA_VERSION = f"kontra.actor.{CONTRACT_VERSION}"


class ManifestError(Exception):
    """Raised when a manifest cannot be loaded or validated."""


@dataclass(frozen=True)
class Resources:
    """What an actor says it needs to run, normalised to Kubernetes' units.

    `None` is UNSTATED, never a value: zero CPUs and an empty memory are refused on the way in,
    so a consumer filling a default can tell "not said" from "said nothing"."""

    cpus: Optional[float] = None
    memory: Optional[str] = None

    def as_dict(self) -> dict[str, Any]:
        """The corpus's normalised shape: only the keys that were stated."""
        return {k: v for k, v in (("cpus", self.cpus), ("memory", self.memory)) if v is not None}


@dataclass(frozen=True)
class ActorManifest:
    """Actor identity from actor.json, and its resources when it states them. Schemas are NOT
    here — they come from the typed @actor.defn class (kontra.schema.schema_of)."""

    schema_version: str
    name: str
    version: str
    actor_dir: Path
    resources: Optional[Resources] = None


# THE UNIT TABLE, and it is shared/conformance/resources.json's, not this module's. The examples
# were written in Docker's units, where `256m` is 256 MiB; Kubernetes reads a lowercase `m` as
# MILLI, so the same string handed on verbatim is a 256-millibyte limit no container can start
# under. Docker's lowercase binary suffixes are rewritten to Kubernetes' binary ones; Kubernetes'
# own pass through; anything else is refused. `cli/actorresources.go` is the other reader.
_MEMORY = re.compile(r"([0-9]+)(\.[0-9]+)?([A-Za-z]*)")
_MEMORY_SUFFIX = {
    # Docker's binary suffixes, lowercase.
    "b": "", "k": "Ki", "m": "Mi", "g": "Gi", "t": "Ti",
    # Kubernetes' own, untouched.
    "": "", "Ki": "Ki", "Mi": "Mi", "Gi": "Gi", "Ti": "Ti", "Pi": "Pi", "Ei": "Ei",
    "M": "M", "G": "G", "T": "T", "P": "P", "E": "E",
}


def _normalise_memory(field: str, value: Any) -> str:
    if not isinstance(value, str):
        raise ManifestError(
            f'{field!r}.memory must be a string like "1Gi" or "512m", got {json.dumps(value)}'
        )
    m = _MEMORY.fullmatch(value)
    if m is None:
        raise ManifestError(
            f"{field!r}.memory: {value!r} is not a memory quantity — write a whole number with "
            'Ki/Mi/Gi (or Docker\'s k/m/g), e.g. "1Gi" or "512m"'
        )
    whole, frac, suffix = m.group(1), m.group(2) or "", m.group(3)
    if suffix not in _MEMORY_SUFFIX:
        raise ManifestError(
            f"{field!r}.memory: {value!r}: the unit {suffix!r} is in neither Kubernetes' table "
            "(Ki Mi Gi Ti Pi Ei, M G T P E) nor Docker's (b k m g t)"
        )
    if not (whole + frac.lstrip(".")).strip("0"):
        raise ManifestError(f"{field!r}.memory: {value!r} is no memory at all")
    unit = _MEMORY_SUFFIX[suffix]
    if frac and not unit:
        raise ManifestError(f"{field!r}.memory: {value!r} is a fraction of a byte")
    return whole + frac + unit


def resources_of(raw: dict[str, Any]) -> Optional[Resources]:
    """Read `resources` — or `needs`, its alias — from a parsed actor.json.

    BOTH SPELLINGS AT ONCE IS REFUSED rather than resolved by precedence: an author who added
    `resources` and forgot `needs` has two answers, and picking one silently is how the other turns
    out to be the one they edited. Every refusal names the key the author actually wrote."""
    stated = {k: raw.get(k) for k in ("resources", "needs") if raw.get(k) is not None}
    if len(stated) == 2:
        raise ManifestError(
            'both "resources" and "needs" are stated, and they are one field under two names. '
            'Keep "resources" and delete "needs"'
        )
    if not stated:
        return None
    ((field, block),) = stated.items()
    if not isinstance(block, dict):
        raise ManifestError(
            f'{field!r} must be an object like {{"cpus": 1, "memory": "1Gi"}}, got {json.dumps(block)}'
        )
    unknown = sorted(set(block) - {"cpus", "memory"})
    if unknown:
        raise ManifestError(
            f"{field!r} has unknown key(s) {', '.join(unknown)}: it takes \"cpus\" and \"memory\" and "
            "nothing else, and a key that is ignored is an actor run on the default with nothing said"
        )
    cpus = block.get("cpus")
    # `bool` FIRST, because `isinstance(True, int)` is true in Python: `"cpus": true` would otherwise
    # be one CPU here and a refusal in the Go reader. The corpus carries that row for this line.
    #
    # `isfinite` because Python's json module, unlike Go's, parses the non-JSON `NaN` and `Infinity`
    # — and `NaN <= 0` is False, so without it a NaN would sail through as a CPU count.
    if "cpus" in block and (
        isinstance(cpus, bool)
        or not isinstance(cpus, (int, float))
        or not math.isfinite(cpus)
        or cpus <= 0
    ):
        raise ManifestError(
            f"{field!r}.cpus must be a number above zero (1, or 0.5), got {json.dumps(cpus)}"
        )
    memory = _normalise_memory(field, block["memory"]) if "memory" in block else None
    if cpus is None and memory is None:
        return None
    return Resources(cpus=cpus, memory=memory)


def load_manifest(actor_dir: Path, *, actor_name: str | None = None) -> Optional[ActorManifest]:
    """Load identity from actor.json, or None if absent (a DIY actor needs no manifest).
    `operations`/schema refs, if present from the pre-types layout, are ignored."""
    manifest_path = actor_dir / MANIFEST_FILENAME
    if not manifest_path.exists():
        return None

    try:
        raw = json.loads(manifest_path.read_text())
    except json.JSONDecodeError as e:
        raise ManifestError(f"invalid JSON in {MANIFEST_FILENAME}: {e}") from e

    schema_version = raw.get("schemaVersion")
    if schema_version != SCHEMA_VERSION:
        raise ManifestError(
            f"unsupported schemaVersion {schema_version!r}, expected {SCHEMA_VERSION!r}"
        )

    name = raw.get("name")
    if not name:
        raise ManifestError(f"{MANIFEST_FILENAME} missing required field 'name'")

    version = raw.get("version")
    if not version:
        raise ManifestError(f"{MANIFEST_FILENAME} missing required field 'version'")

    if actor_name is not None and name != actor_name:
        raise ManifestError(
            f"manifest name {name!r} does not match actor directory name {actor_name!r}"
        )

    try:
        resources = resources_of(raw)
    except ManifestError as e:
        raise ManifestError(f"{MANIFEST_FILENAME}: {e}") from e

    return ActorManifest(
        schema_version=schema_version,
        name=name,
        version=version,
        actor_dir=actor_dir.resolve(),
        resources=resources,
    )


def catalog_key(name: str, version: str) -> str:
    """The catalog/digest primary key: `{name}@{version}`, or just `{name}` when
    unversioned (back-compat). ONE owner so the catalog POST (actor_build), the worker
    digest self-register (serve), and any other caller agree byte-for-byte instead of
    re-minting the f-string (arch review #04)."""
    return f"{name}@{version}" if version else name


def derive_identity(registry: Any, actor_dir: Path, *, source: str) -> None:
    """Apply the content-pinned (name, version) identity rules (ADR 0004/0011) to a
    registry from its directory: default the name to the dir name, require a non-empty
    lifecycle, load the manifest, set the version. The ONE definition both load paths
    use — the import path (loader.load_actor) and the `python3 actor.py` path
    (serve.resolve_identity) — so a new rule can't land on one and miss the other (arch
    review #05). Deliberately does NOT touch import hygiene (the ~12-field registry reset
    is load_actor's distinct concern). Raises SystemExit on an empty module or a
    bad/mismatched manifest. `source` is the file path, for the empty-module message."""
    registry.actor_dir = actor_dir
    if registry.actor_name == "actor":
        registry.actor_name = actor_dir.name
    if registry.load_fn is None and not registry.methods:
        raise SystemExit(f"{source} defined no @actor.load / @actor.method")
    try:
        registry.manifest = load_manifest(actor_dir, actor_name=registry.actor_name)
    except ManifestError as e:
        raise SystemExit(str(e)) from e
    registry.version = registry.manifest.version if registry.manifest else ""


def validate_unit(unit: Any, schema: dict[str, Any], *, context: str) -> None:
    """Validate a single unit against a JSON Schema. Raises ManifestError on failure."""
    # Imported HERE, not at module scope, and the reason is a dependency boundary rather than
    # startup cost: this module owns two things — actor IDENTITY (ActorManifest, catalog_key) and
    # SCHEMA VALIDATION. Only the second needs jsonschema, and jsonschema is a build-time
    # dependency a worker's runtime does not install. A module-level import made
    # identity unreachable from the runtime, which is why the worker's catalog self-registration
    # could not be written at all: importing internals.catalog crashed with ModuleNotFoundError
    # before it reached a single line of its own code.
    from jsonschema import Draft202012Validator

    validator = Draft202012Validator(schema)
    errors = sorted(validator.iter_errors(unit), key=lambda e: list(e.path))
    if errors:
        err = errors[0]
        path = ".".join(str(p) for p in err.path) if err.path else "(root)"
        raise ManifestError(f"{context}: {path}: {err.message}")


def validate_units(units: list[Any], schema: Optional[dict[str, Any]], *, context: str) -> None:
    """Validate each unit against `schema`. No-op when `schema` is None (undeclared I/O)."""
    if schema is None:
        return
    for i, unit in enumerate(units, 1):
        validate_unit(unit, schema, context=f"{context} {i}")


def apply_param_defaults(params: dict[str, Any], schema: dict[str, Any]) -> dict[str, Any]:
    """Fill in declared top-level `default`s for keys the caller didn't supply, so the
    documented default is the single source of truth. Shallow — params are flat config."""
    merged = dict(params)
    for key, spec in (schema.get("properties") or {}).items():
        if key not in merged and isinstance(spec, dict) and "default" in spec:
            merged[key] = spec["default"]
    return merged


def resolve_params(params: dict[str, Any], schema: Optional[dict[str, Any]]) -> dict[str, Any]:
    """Apply declared defaults then validate run-wide params against the params schema,
    returning the merged dict. No schema -> pass through free-form. Raises ManifestError
    on a violation, so a bad --param fails before any workflow starts."""
    if schema is None:
        return params
    merged = apply_param_defaults(params, schema)
    validate_unit(merged, schema, context="params")
    return merged
