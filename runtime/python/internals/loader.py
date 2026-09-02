"""Load an actor module by path — the shared loader the build/registration scripts (and
tests) use to read an actor's registry + identity. NOT a CLI: a Python actor is RUN by
`python3 actor.py` (actor.serve()), and DISPATCH is the orchestrator's job (pnpm). This only
*imports* an actor module so its `@actor.defn` / `@actor.*` decorators populate the
registry, then hands back the registry + actor.json identity.
"""

from __future__ import annotations

import importlib.util
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Optional

from internals.manifest import ActorManifest, derive_identity


@dataclass
class LoadedActor:
    registry: object
    actor_dir: Path
    manifest: Optional[ActorManifest]


def _resolve_actor_paths(path_str: str) -> tuple[Path, Path, str]:
    """Return (actor_dir, actor_file, actor_name)."""
    path = Path(path_str).resolve()
    if path.is_dir():
        for candidate in ("actor.py", "main.py", "__init__.py"):
            if (path / candidate).exists():
                return path, path / candidate, path.name
        raise SystemExit(f"no actor.py/main.py found in {path}")
    return path.parent, path, path.parent.name


def load_actor(path_str: str) -> LoadedActor:
    """Import the actor module at `path_str` so its decorators register, and return the
    populated registry + identity. Loading by file path (not package name) keeps the
    actor's own imports resolving to the real PyPI packages."""
    actor_dir, file, name = _resolve_actor_paths(path_str)

    spec = importlib.util.spec_from_file_location(f"kontra_actor_{name}", file)
    if spec is None or spec.loader is None:
        raise SystemExit(f"cannot import actor module at {file}")
    module = importlib.util.module_from_spec(spec)

    from actorkit import actor

    actor.actor_name = "actor"
    actor.actor_dir = None
    actor.manifest = None
    actor.load_fn = None
    actor.close_fn = None
    actor.healthcheck_fn = None
    actor.methods = {}
    actor.version = ""
    actor.input_type = None
    actor.output_type = None
    actor.params_type = None
    actor.actor_class = None

    spec.loader.exec_module(module)

    # Identity derivation (name default, empty-check, manifest, version) is the shared
    # rule — loader keeps only its distinct job (path resolve + import + the registry
    # reset above). actor_dir.name == the resolved `name` here, so behaviour is unchanged.
    derive_identity(actor, actor_dir, source=str(file))

    return LoadedActor(registry=actor, actor_dir=actor_dir, manifest=actor.manifest)


def read_jsonl(path: Path) -> list[object]:
    """Parse a JSONL file into a list of units (one JSON value per non-blank line)."""
    units: list[object] = []
    for line_no, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip():
            continue
        try:
            units.append(json.loads(line))
        except json.JSONDecodeError as e:
            raise SystemExit(f"invalid JSON on input line {line_no}: {e}") from e
    return units
