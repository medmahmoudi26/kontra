"""`worker.yaml` — the actor's Temporal Worker tuning, declared beside `actor.py` (PRD D2).

    # worker.yaml — temporalio.worker.Worker options, under the SDK's own names.
    max_concurrent_activities: 2
    max_concurrent_activity_task_polls: 2
    graceful_shutdown_timeout: 30s

TEMPORAL'S NAMES, NOT KONTRA'S. Every key is a keyword argument of the installed
`temporalio.worker.Worker`, spelled the way that SDK spells it, and checked against its signature
at boot; a key the SDK does not have is refused with the nearest real name rather than ignored.
kontra adds no aliases and no defaults of its own beyond the ones `actor.serve()` already applied.

ONLY TUNING, BY NAME. The accepted keys are {@link TUNING} — the options that bound concurrency,
polling, throttling and shutdown — intersected with the installed SDK. Typing was not enough to
decide it: `no_remote_activities` is a bool, and setting it makes the actor's worker poll NO
activity tasks, a healthy idle worker nothing ever schedules onto; `use_worker_versioning` opts the
worker into a routing mode kontra does not run. Wiring and identity are refused by name with the
reason (`task_queue`, `identity`, `build_id`); anything else the SDK has is refused as "not a tuning
option", so a newer SDK's option arrives here only by being added to the table on purpose.

IN RANGE, AND REFUSED BEFORE CONNECTING. A count must be a whole number of at least 1 (0 where the
SDK means "none"), a rate a finite positive number, a duration finite and not negative, and a key
may appear once. A value the Worker would refuse — or worse, accept — is a boot failure naming the
file and the key, not a Worker that started without the setting its author believed in.

WHAT IT REACHES. The actor's shared worker (OpenSession, CloseSession, unscoped RunBatch) and a
workflow host. A Session's own worker has fixed slots and drain (`host.py`), so an actor's
`max_concurrent_activities` bounds Session OPENS, not the Methods a Session runs.
"""

from __future__ import annotations

import difflib
import inspect
import math
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


#: The tuning keys, with what each must be. `count1`: a whole number >= 1. `count0`: >= 0 (0 means
#: none, where the SDK reads it so). `rate`: a finite number > 0. `ratio`: a finite number in (0, 1].
#: `duration`: finite and >= 0. `flag`: true or false.
TUNING: Dict[str, str] = {
    "max_cached_workflows": "count0",
    "max_concurrent_workflow_tasks": "count1",
    "max_concurrent_activities": "count1",
    "max_concurrent_local_activities": "count1",
    "max_concurrent_nexus_tasks": "count1",
    "max_concurrent_workflow_task_polls": "count1",
    "max_concurrent_activity_task_polls": "count1",
    "nonsticky_to_sticky_poll_ratio": "ratio",
    "max_activities_per_second": "rate",
    "max_task_queue_activities_per_second": "rate",
    "max_eager_activity_reservations_per_workflow_task": "count0",
    "disable_eager_activity_execution": "flag",
    "sticky_queue_schedule_to_start_timeout": "duration",
    "max_heartbeat_throttle_interval": "duration",
    "default_heartbeat_throttle_interval": "duration",
    "graceful_shutdown_timeout": "duration",
}


def tunable_options() -> Dict[str, str]:
    """{@link TUNING}, limited to what the installed `temporalio.worker.Worker` actually takes."""
    from temporalio.worker import Worker

    have = set(inspect.signature(Worker.__init__).parameters)
    return {k: v for k, v in TUNING.items() if k in have}


_DURATION = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*(ms|s|m|h)?\s*$")


def _number(value: Any) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError("a number")
    if not math.isfinite(value):
        raise ValueError("a finite number")
    return float(value)


def _duration(value: Any) -> timedelta:
    if isinstance(value, bool):
        raise ValueError("a duration, not a flag")
    if isinstance(value, (int, float)):
        seconds = _number(value)
    else:
        m = _DURATION.match(str(value))
        if not m:
            raise ValueError("a duration like 30s, 500ms, 2m or 1h")
        n, unit = float(m.group(1)), m.group(2) or "s"
        seconds = n / 1000 if unit == "ms" else n * {"s": 1, "m": 60, "h": 3600}[unit]
    if seconds < 0:
        raise ValueError("a duration that is not negative")
    try:
        return timedelta(seconds=seconds)
    except OverflowError as err:
        raise ValueError("a duration a timedelta can hold") from err


def _coerce(kind: str, value: Any) -> Any:
    if kind == "duration":
        return _duration(value)
    if kind == "flag":
        if not isinstance(value, bool):
            raise ValueError("true or false")
        return value
    if kind in ("count0", "count1"):
        if isinstance(value, bool) or not isinstance(value, int):
            raise ValueError("a whole number")
        floor = 1 if kind == "count1" else 0
        if value < floor or value > 1_000_000:
            raise ValueError(f"a whole number between {floor} and 1000000")
        return value
    if kind == "rate":
        v = _number(value)
        if v <= 0:
            raise ValueError("a number above 0")
        return v
    if kind == "ratio":
        v = _number(value)
        if not 0 < v <= 1:
            raise ValueError("a number above 0 and at most 1")
        return v
    raise ValueError(kind)


def _unique_keys_loader():
    """A SafeLoader that refuses a duplicated key instead of keeping the last one silently."""
    import yaml

    class Loader(yaml.SafeLoader):
        pass

    def construct_mapping(loader, node, deep=False):
        seen = set()
        for key_node, _ in node.value:
            key = loader.construct_object(key_node, deep=deep)
            if key in seen:
                raise WorkerYamlError(f"{key} appears twice; a key may be set once")
            seen.add(key)
        return yaml.SafeLoader.construct_mapping(loader, node, deep=deep)

    Loader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping)
    return Loader


def parse(text: str, source: str = WORKER_FILE) -> Dict[str, Any]:
    """The Worker keyword arguments `text` declares, or WorkerYamlError naming what is wrong."""
    try:
        import yaml
    except ImportError as err:  # pragma: no cover - the [actor] extra carries it
        raise WorkerYamlError(f"{source}: reading it needs PyYAML (the kontra-sdk[actor] extra)") from err
    try:
        data = yaml.load(text, Loader=_unique_keys_loader())  # noqa: S506 — a SafeLoader subclass
    except WorkerYamlError as err:
        raise WorkerYamlError(f"{source}: {err}") from err
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
                f"{source}: {key!r} is not a tuning option kontra accepts{hint} "
                "(keys are the Python SDK's own Worker keyword names; see TUNING in workeryaml.py)"
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
