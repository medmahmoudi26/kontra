"""`worker.yaml` — the actor's Temporal Worker tuning, declared beside `actor.py` (PRD D2).

    # worker.yaml — temporalio.worker.Worker options, under the SDK's own names.
    max_concurrent_activities: 2
    max_concurrent_activity_task_polls: 2
    graceful_shutdown_timeout: 30s

TEMPORAL'S NAMES, NOT KONTRA'S. Every key is a keyword argument of the installed
`temporalio.worker.Worker`, spelled the way that SDK spells it, and checked against its signature
at boot — so a Temporal upgrade that adds an option makes it available here with no kontra release,
and a key the SDK does not have is refused with the nearest real name rather than ignored. kontra
adds no aliases and no defaults of its own beyond the ones `actor.serve()` already applied.

ONLY TUNING. A key is accepted when the SDK types it as a number, a flag or a duration. Everything
else on `Worker` is wiring — the client, the activities, the interceptors, the runner — which the
host owns, and a few are refused by name because setting them would silently break an actor:

  • `task_queue`: derived from the actor's name and version (`catalog.shared_queue`). A worker
    polling any other queue is a healthy idle worker nothing ever schedules onto.
  • `identity`, `build_id`: the identity is the Worker name every log line and the console carry.

REFUSED BEFORE CONNECTING. A typo is a boot failure naming the file and the key, not a Worker that
started without the setting somebody believed was applied.
"""

from __future__ import annotations

import difflib
import inspect
import re
from datetime import timedelta
from pathlib import Path
from typing import Any, Dict, Optional

WORKER_FILE = "worker.yaml"

#: Keys that would break the actor's addressing or identity, with the reason given back to the
#: author. Other non-tuning keys are refused by type, with a generic reason.
RESERVED = {
    "task_queue": "the task queue is derived from the actor's name and version and is not a knob",
    "identity": "the Worker identity is the name the logs and the console carry; it is not a knob",
    "build_id": "versioning is the actor's version in actor.json, not a Worker option here",
}


class WorkerYamlError(ValueError):
    """`worker.yaml` names something the Worker cannot take."""


_SCALAR = re.compile(r"^(int|float|bool|timedelta)(\s*\|\s*None)?$")


def tunable_options() -> Dict[str, str]:
    """Every `Worker` keyword the installed SDK types as a scalar: name -> int|float|bool|timedelta."""
    from temporalio.worker import Worker

    out: Dict[str, str] = {}
    for name, p in inspect.signature(Worker.__init__).parameters.items():
        if name == "self" or name in RESERVED:
            continue
        ann = p.annotation if isinstance(p.annotation, str) else getattr(p.annotation, "__name__", str(p.annotation))
        m = _SCALAR.match(str(ann).replace("Optional[", "").rstrip("]").strip())
        if m:
            out[name] = m.group(1)
    return out


_DURATION = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*(ms|s|m|h)?\s*$")


def _duration(value: Any) -> timedelta:
    if isinstance(value, bool):
        raise ValueError("a duration, not a flag")
    if isinstance(value, (int, float)):
        return timedelta(seconds=value)
    m = _DURATION.match(str(value))
    if not m:
        raise ValueError("a duration like 30s, 500ms, 2m or 1h")
    n, unit = float(m.group(1)), m.group(2) or "s"
    return timedelta(milliseconds=n) if unit == "ms" else timedelta(seconds=n * {"s": 1, "m": 60, "h": 3600}[unit])


def _coerce(kind: str, value: Any) -> Any:
    if kind == "timedelta":
        return _duration(value)
    if kind == "bool":
        if not isinstance(value, bool):
            raise ValueError("true or false")
        return value
    if kind == "int":
        if isinstance(value, bool) or not isinstance(value, int):
            raise ValueError("a whole number")
        return value
    if kind == "float":
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise ValueError("a number")
        return float(value)
    raise ValueError(kind)


def parse(text: str, source: str = WORKER_FILE) -> Dict[str, Any]:
    """The Worker keyword arguments `text` declares, or WorkerYamlError naming what is wrong."""
    try:
        import yaml
    except ImportError as err:  # pragma: no cover - the [actor] extra carries it
        raise WorkerYamlError(f"{source}: reading it needs PyYAML (the kontra-sdk[actor] extra)") from err
    try:
        data = yaml.safe_load(text)
    except yaml.YAMLError as err:
        raise WorkerYamlError(f"{source}: not valid YAML: {err}") from err
    if data is None:
        return {}
    if not isinstance(data, dict):
        raise WorkerYamlError(f"{source}: must be a map of Worker options, got {type(data).__name__}")
    options = tunable_options()
    out: Dict[str, Any] = {}
    for key, value in data.items():
        key = str(key)
        if key in RESERVED:
            raise WorkerYamlError(f"{source}: {key} cannot be set here: {RESERVED[key]}")
        kind = options.get(key)
        if kind is None:
            near = difflib.get_close_matches(key, list(options), n=1)
            hint = f" — did you mean {near[0]}?" if near else ""
            raise WorkerYamlError(
                f"{source}: {key!r} is not a tunable temporalio Worker option{hint} "
                "(keys are the Python SDK's own Worker keyword names)"
            )
        try:
            out[key] = _coerce(kind, value)
        except ValueError as err:
            raise WorkerYamlError(f"{source}: {key} must be {err}, got {value!r}") from err
    return out


def load(actor_dir: Optional[Path]) -> Dict[str, Any]:
    """The options in `actor_dir/worker.yaml`, or {} when there is no such file."""
    if actor_dir is None:
        return {}
    path = Path(actor_dir) / WORKER_FILE
    if not path.is_file():
        return {}
    return parse(path.read_text(encoding="utf-8"), source=str(path))
