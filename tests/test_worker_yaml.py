"""worker.yaml: Temporal Worker tuning beside the actor, under the SDK's own names (PRD D2)."""

from __future__ import annotations

from datetime import timedelta
from pathlib import Path

import pytest

from internals.workeryaml import WorkerYamlError, load, parse, tunable_options


def test_the_prd_example_with_the_python_sdks_own_names():
    got = parse(
        """
        # worker.yaml — Temporal WorkerOptions, Temporal's names, nothing else.
        max_concurrent_activities: 2
        max_concurrent_activity_task_polls: 2
        max_cached_workflows: 0
        graceful_shutdown_timeout: 30s
        """
    )
    assert got == {
        "max_concurrent_activities": 2,
        "max_concurrent_activity_task_polls": 2,
        "max_cached_workflows": 0,
        "graceful_shutdown_timeout": timedelta(seconds=30),
    }


def test_a_name_the_sdk_does_not_have_is_refused_with_the_nearest_real_one():
    # The PRD's own draft spelled it `..._pollers`; the Python SDK calls it `..._polls`.
    with pytest.raises(WorkerYamlError, match=r"did you mean max_concurrent_activity_task_polls"):
        parse("max_concurrent_activity_task_pollers: 2")


@pytest.mark.parametrize("key", ["task_queue", "identity", "build_id"])
def test_addressing_and_identity_are_not_knobs(key):
    with pytest.raises(WorkerYamlError, match=f"{key} cannot be set here"):
        parse(f"{key}: something")


@pytest.mark.parametrize("key", ["client", "activities", "workflows", "interceptors", "workflow_runner"])
def test_wiring_is_not_tuning(key):
    with pytest.raises(WorkerYamlError, match="not a tunable"):
        parse(f"{key}: x")


@pytest.mark.parametrize(
    "text, expect",
    [
        ("graceful_shutdown_timeout: 500ms", timedelta(milliseconds=500)),
        ("graceful_shutdown_timeout: 2m", timedelta(minutes=2)),
        ("graceful_shutdown_timeout: 1h", timedelta(hours=1)),
        ("graceful_shutdown_timeout: 45", timedelta(seconds=45)),
    ],
)
def test_durations(text, expect):
    assert parse(text)["graceful_shutdown_timeout"] == expect


@pytest.mark.parametrize(
    "text, message",
    [
        ("max_concurrent_activities: lots", "a whole number"),
        ("max_concurrent_activities: true", "a whole number"),
        ("debug_mode: 1", "true or false"),
        ("graceful_shutdown_timeout: soon", "a duration"),
        ("- max_concurrent_activities", "must be a map"),
    ],
)
def test_a_wrong_type_names_the_key_and_what_it_must_be(text, message):
    with pytest.raises(WorkerYamlError, match=message):
        parse(text)


def test_empty_and_absent_mean_nothing_to_apply(tmp_path: Path):
    assert parse("") == {}
    assert parse("# only a comment\n") == {}
    assert load(tmp_path) == {}
    assert load(None) == {}


def test_load_reads_the_file_beside_the_actor_and_names_it_in_errors(tmp_path: Path):
    (tmp_path / "worker.yaml").write_text("max_concurrent_activities: 3\n")
    assert load(tmp_path) == {"max_concurrent_activities": 3}
    (tmp_path / "worker.yaml").write_text("task_queue: elsewhere\n")
    with pytest.raises(WorkerYamlError, match=str(tmp_path / "worker.yaml")):
        load(tmp_path)


def test_the_tunable_set_follows_the_installed_sdk():
    opts = tunable_options()
    # A few that exist in every supported temporalio, with the kinds the coercion relies on.
    assert opts["max_concurrent_activities"] == "int"
    assert opts["graceful_shutdown_timeout"] == "timedelta"
    assert opts["debug_mode"] == "bool"
    assert "task_queue" not in opts and "client" not in opts


class _Registry:
    def __init__(self, actor_dir: Path) -> None:
        self.actor_name = "enrich"
        self.version = "0.3.0"
        self.actor_dir = actor_dir


def _serve(monkeypatch, registry, **kw):
    """Run `host.serve_async` with everything that would leave the process faked out, and return
    the keyword arguments the actor's Worker was built with (or the exception, before a connect)."""
    import asyncio

    from internals import metrics
    from internals.temporal import connect as kconnect
    from internals.temporal import host

    seen: dict = {"connected": False}

    async def fake_connect(queue, **_):
        seen["connected"] = True
        return object()

    class _Worker:
        async def run(self):
            return None

    def fake_actor_worker(client, reg, *, task_queue, **options):
        seen["task_queue"] = task_queue
        seen["options"] = options
        return _Worker()

    monkeypatch.setattr(kconnect, "connect", fake_connect)
    monkeypatch.setattr(kconnect, "actor_worker", fake_actor_worker)
    monkeypatch.setattr(metrics, "serve", lambda *a, **k: None)
    monkeypatch.setattr(host, "publish_catalog", lambda *a, **k: None)
    monkeypatch.setattr(host, "_install_blob_reader", lambda *a, **k: None)
    asyncio.run(host.serve_async(registry, address="x:1", namespace="ns", **kw))
    return seen


def test_serve_applies_worker_yaml_over_the_hosts_default(monkeypatch, tmp_path: Path):
    (tmp_path / "worker.yaml").write_text("max_concurrent_activities: 2\ngraceful_shutdown_timeout: 30s\n")
    seen = _serve(monkeypatch, _Registry(tmp_path))
    assert seen["options"]["max_concurrent_activities"] == 2
    assert seen["options"]["graceful_shutdown_timeout"] == timedelta(seconds=30)
    # The queue is still the derived one: worker.yaml cannot move it.
    assert seen["task_queue"] == "enrich-0.3.0-sessions"


def test_serve_without_worker_yaml_keeps_the_hosts_default(monkeypatch, tmp_path: Path):
    seen = _serve(monkeypatch, _Registry(tmp_path))
    assert seen["options"]["max_concurrent_activities"] >= 4


def test_a_bad_worker_yaml_fails_before_anything_connects(monkeypatch, tmp_path: Path):
    (tmp_path / "worker.yaml").write_text("task_queue: elsewhere\n")
    with pytest.raises(WorkerYamlError):
        _serve(monkeypatch, _Registry(tmp_path))


def test_a_workflow_folder_declares_its_worker_options_too(tmp_path: Path):
    import importlib.util
    import sys

    from internals.temporal.wfhost import declared_worker_options

    def workflow_in(folder: Path, cls: str):
        folder.mkdir()
        src = folder / "workflow.py"
        src.write_text(
            "from temporalio import workflow\n"
            f"@workflow.defn\nclass {cls}:\n    @workflow.run\n    async def run(self) -> None: ...\n"
        )
        spec = importlib.util.spec_from_file_location(f"wf_{cls}", src)
        mod = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = mod
        spec.loader.exec_module(mod)
        return getattr(mod, cls)

    a = workflow_in(tmp_path / "a", "A")
    b = workflow_in(tmp_path / "b", "B")
    assert declared_worker_options([a]) == {}
    (tmp_path / "a" / "worker.yaml").write_text("max_cached_workflows: 10\n")
    assert declared_worker_options([a]) == {"max_cached_workflows": 10}
    # One folder declaring and one silent: the declaration applies.
    assert declared_worker_options([a, b]) == {"max_cached_workflows": 10}
    # Two folders disagreeing cannot share one worker.
    (tmp_path / "b" / "worker.yaml").write_text("max_cached_workflows: 20\n")
    with pytest.raises(WorkerYamlError, match="different worker.yaml"):
        declared_worker_options([a, b])
