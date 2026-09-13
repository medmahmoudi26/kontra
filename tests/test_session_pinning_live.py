"""A Session is pinned to one process, and losing that process fails the scope — live.

The unit tests in `test_session_scope.py` prove the machinery with the Worker injected. What
they cannot prove is the property the whole design rests on: that a queue named after a Session
really does lead to the one process that activated it, while OTHER processes poll the same
actor. That is a claim about Temporal's routing, and only Temporal can answer it.

So this starts TWO real actor hosts as subprocesses, opens real scopes from a real caller
workflow, and asks the actor which pid answered. Then it kills the host holding a scope and
requires the next call inside that scope to RAISE (ADR 0023 §7) rather than hang or quietly
re-activate somewhere with an empty instance.

RUN THESE. They skip without a Temporal server, and the actor's state store is Redis, which
compose does not publish on localhost:

    KONTRA_REDIS_HOST=10.124.0.2:6379 .venv/bin/python -m pytest tests/test_session_pinning_live.py

Hosts are killed by explicit pid on the way out; a pattern kill would match the test runner's
own command line.
"""

from __future__ import annotations

import asyncio
import functools
import os
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "tests" / "fixtures"))

import scope_probe  # noqa: E402  (the workflows, in their own side-effect-free module)

ADDRESS = os.environ.get("KONTRA_ADDRESS", "localhost:7233")
NAMESPACE = os.environ.get("KONTRA_NAMESPACE", "default")
REDIS_HOST = os.environ.get("KONTRA_REDIS_HOST", "localhost:6379")
PROBE_DIR = ROOT / "tests" / "fixtures" / "session_probe"
HOSTS = 2


async def _missing() -> str:
    """What is not reachable, named — a skip whose reason is "something" costs an hour."""
    from temporalio.client import Client

    try:
        await asyncio.wait_for(Client.connect(ADDRESS, namespace=NAMESPACE), timeout=5)
    except Exception:
        return f"no Temporal server at {ADDRESS} (set KONTRA_ADDRESS)"
    try:
        import redis.asyncio as aioredis

        host, _, port = REDIS_HOST.partition(":")
        c = aioredis.Redis(host=host or "localhost", port=int(port or 6379), db=0)
        await c.ping()
        await c.aclose()
    except Exception:
        return f"no Redis at {REDIS_HOST} (the actor's state store; set KONTRA_REDIS_HOST)"
    return ""


_UNAVAILABLE = asyncio.run(_missing())
pytestmark = pytest.mark.skipif(bool(_UNAVAILABLE), reason=_UNAVAILABLE or "live")


def sync(fn):
    """Run an async test body on its own loop — the repo has no pytest-asyncio, and a bare
    `async def test_` is silently not-run rather than failed."""

    @functools.wraps(fn)
    def wrapper(*a, **kw):
        return asyncio.run(fn(*a, **kw))

    return wrapper


def _chain(e: BaseException) -> str:
    """Every message in the cause chain. Temporal wraps an activity timeout three deep, and the
    outermost line says only that the workflow failed."""
    parts, seen = [], set()
    while e is not None and id(e) not in seen:
        seen.add(id(e))
        parts.append(f"{type(e).__name__}: {e}")
        e = e.__cause__
    return " | ".join(parts)


# ---------------------------------------------------------------------------------------------
# Two real actor hosts
# ---------------------------------------------------------------------------------------------


class Host:
    """One `actor.serve()` process, started and stopped by pid."""

    def __init__(self) -> None:
        env = dict(os.environ)
        env["KONTRA_ADDRESS"] = ADDRESS
        env["KONTRA_NAMESPACE"] = NAMESPACE
        env["KONTRA_REDIS_HOST"] = REDIS_HOST
        env["KONTRA_METRICS_ADDR"] = "off"        # two hosts cannot both bind 9110
        env["KONTRA_MAX_PARALLEL_SESSIONS"] = "4"
        env.pop("KONTRA_ORCHESTRATOR_URL", None)  # do not publish a test actor to the catalog
        env["PYTHONPATH"] = os.pathsep.join(
            [str(ROOT / "sdk" / "python"), str(ROOT / "runtime" / "python"),
             str(ROOT / "sdk" / "python" / "_gen")]
        )
        self.lines: list[str] = []
        self.proc = subprocess.Popen(
            [sys.executable, str(PROBE_DIR / "actor.py")],
            env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )
        threading.Thread(target=self._drain, daemon=True).start()

    def _drain(self) -> None:
        for line in self.proc.stdout:  # type: ignore[union-attr]
            self.lines.append(line.rstrip())

    @property
    def pid(self) -> int:
        return self.proc.pid

    def log(self) -> str:
        return "\n".join(self.lines)

    def wait_until_polling(self, timeout: float = 60) -> None:
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if any("-sessions @" in line for line in self.lines):
                return
            if self.proc.poll() is not None:
                raise RuntimeError(f"host {self.pid} exited:\n{self.log()}")
            time.sleep(0.1)
        raise TimeoutError(f"host {self.pid} never started polling:\n{self.log()}")

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=15)
            except subprocess.TimeoutExpired:  # pragma: no cover - a wedged host
                self.proc.kill()
                self.proc.wait(timeout=10)


@pytest.fixture
def hosts():
    """Per test, because one of them kills a host: a module-scoped fleet would make these tests
    order-dependent, which is how a live suite starts passing for the wrong reason."""
    live = [Host() for _ in range(HOSTS)]
    try:
        for h in live:
            h.wait_until_polling()
        yield live
    finally:
        for h in live:
            h.stop()


async def _caller(*workflow_types):
    """A worker for the caller's own workflows, wired the way `catalog.serve()` wires one —
    same codec, same sandbox passthrough, so this exercises the path an author actually runs."""
    from temporalio.client import Client
    from temporalio.worker import Worker
    from temporalio.worker.workflow_sandbox import SandboxedWorkflowRunner, SandboxRestrictions

    from internals import casstore

    client = await Client.connect(
        ADDRESS, namespace=NAMESPACE, data_converter=casstore.data_converter()
    )
    queue = f"scope-probe-{uuid.uuid4().hex[:8]}"
    worker = Worker(
        client,
        task_queue=queue,
        workflows=list(workflow_types),
        workflow_runner=SandboxedWorkflowRunner(
            restrictions=SandboxRestrictions.default.with_passthrough_modules("kontra", "actorkit")
        ),
    )
    return client, worker, queue


# ---------------------------------------------------------------------------------------------


@sync
async def test_two_method_calls_in_one_scope_land_on_one_process(hosts):
    """The pinning claim, end to end. Two hosts poll the actor's shared queue, so the open could
    have gone to either — but every call inside the scope must reach the one that took it, and
    must reach the SAME loaded instance: `calls` counts 1 then 2 because `self.*` survived
    between two separate dispatches, which is the whole of what a Session is."""
    client, worker, queue = await _caller(scope_probe.TwoCallsInOneScope)
    async with worker:
        out = await client.execute_workflow(
            scope_probe.TwoCallsInOneScope.run,
            id=f"scope-two-{uuid.uuid4().hex[:8]}",
            task_queue=queue,
        )

    first, second = out["calls"]
    assert first["pid"] == second["pid"], "the scope's calls hit two processes"
    assert first["pid"] in {h.pid for h in hosts}
    assert first["instance"] == second["instance"], "two instances in one Session"
    assert [first["calls"], second["calls"]] == [1, 2], "self.* did not survive the call"


@sync
async def test_a_keyed_scope_activates_the_key(hosts):
    """Keys are optional and a key is a claim on a shared identity (ADR 0023 §10, ADR 0022). The
    same scope, opened on `crawler["acme.com"]`, must activate the KEY's instance — that is what
    makes the dedupe set of acme.com still there next week, where an anonymous Session's state
    begins empty and dies with it."""
    client, worker, queue = await _caller(scope_probe.TwoCallsInOneScope)
    async with worker:
        out = await client.execute_workflow(
            scope_probe.TwoCallsInOneScope.run,
            "acme.com",
            id=f"scope-keyed-{uuid.uuid4().hex[:8]}",
            task_queue=queue,
        )

    first, second = out["calls"]
    assert first["actor"] == "acme.com", "the scope did not activate its key"
    assert first["pid"] == second["pid"] and first["instance"] == second["instance"]
    assert out["session"] not in first["actor"], "the key must outrank the Session id"


@sync
async def test_a_closed_scope_leaves_no_worker_on_its_queue(hosts):
    """The close must actually tear the Session's worker down. If it did not, a queue would be
    left polled for every scope ever opened — and the deadlock this path is written to avoid
    presents as exactly this: a close that returned, apparently fine, having done nothing."""
    client, worker, queue = await _caller(
        scope_probe.TwoCallsInOneScope, scope_probe.CallAnOrphanedQueue
    )
    async with worker:
        out = await client.execute_workflow(
            scope_probe.TwoCallsInOneScope.run,
            id=f"scope-closed-{uuid.uuid4().hex[:8]}",
            task_queue=queue,
        )
        after = await client.execute_workflow(
            scope_probe.CallAnOrphanedQueue.run,
            out["session"],
            id=f"scope-orphan-{uuid.uuid4().hex[:8]}",
            task_queue=queue,
        )

    assert after != "landed", "the Session's worker outlived its scope"
    assert "timed out" in after.lower() or "timeout" in after.lower(), after


@sync
async def test_several_scopes_at_once_are_each_pinned(hosts):
    """Four scopes, two hosts. Each scope's call is pinned to whichever host answered its open,
    and the fleet is visibly more than one process — otherwise "pinned" is unfalsifiable."""
    client, worker, queue = await _caller(scope_probe.ConcurrentScopes)
    async with worker:
        calls = await client.execute_workflow(
            scope_probe.ConcurrentScopes.run,
            4,
            id=f"scope-many-{uuid.uuid4().hex[:8]}",
            task_queue=queue,
        )

    assert {c["pid"] for c in calls} <= {h.pid for h in hosts}
    assert all(c["calls"] == 1 for c in calls), "a scope met an instance another scope had used"


@sync
async def test_losing_the_host_raises_out_of_the_scope(hosts):
    """ADR 0023 §7, the decision the whole model rests on. The alternative — re-activating
    somewhere else — would keep the caller's logic running against an instance whose `self.*`
    silently reset, which is the failure shape that reports `completed` with nothing in it.

    Also proves the close cannot wedge a Run: `__aexit__` runs with the host gone, and the
    workflow still reaches a terminal state rather than waiting on a queue nobody polls.
    """
    from temporalio.client import WorkflowFailureError

    client, worker, queue = await _caller(scope_probe.LosingTheHost)
    async with worker:
        handle = await client.start_workflow(
            scope_probe.LosingTheHost.run,
            id=f"scope-lost-{uuid.uuid4().hex[:8]}",
            task_queue=queue,
        )
        pid = 0
        for _ in range(300):
            pid = await handle.query(scope_probe.LosingTheHost.pid)
            if pid:
                break
            await asyncio.sleep(0.2)
        assert pid in {h.pid for h in hosts}, "the scope never reported a host"

        next(h for h in hosts if h.pid == pid).stop()  # the queue now has no poller at all
        await handle.signal(scope_probe.LosingTheHost.host_is_gone)

        with pytest.raises(WorkflowFailureError) as exc:
            await asyncio.wait_for(handle.result(), timeout=180)

    assert "timed out" in _chain(exc.value).lower(), _chain(exc.value)
