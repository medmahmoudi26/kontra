"""Caller workflows that open a Session on the `session-probe` actor (ADR 0023 §4, §6, §7).

Kept out of the test module because the workflow sandbox RE-IMPORTS the module a workflow class
lives in: this one must therefore be free of side effects, where the test module reaches a live
cluster at import to decide whether to skip.

WHAT STANDS IN FOR WHAT. The scope itself — open, close, the ids, the queues — is the real
caller SDK. The Method call is not: a real one is a Nexus dispatch that the Go handler turns
into a `RunBatch` on the Session's queue, and running the handler is a second language and a
second binary. So `_whoami` schedules that same activity onto that same queue directly. The
handler's half of the derivation is pinned by handler/runbatch_test.go; what these prove is the
other half — that the queue leads to ONE process holding ONE loaded instance, and that when it
does not, the wait ends in a timeout rather than never.
"""

from __future__ import annotations

import asyncio
from datetime import timedelta

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from kontra import catalog

ACTOR = "session-probe"
VERSION = "0.1.0"

# Short on purpose: these bound how long a test waits for a queue nobody polls.
PICKUP = timedelta(seconds=10)
CLOSE = timedelta(seconds=5)


async def _whoami(session_id: str, i: int, key: str = "") -> dict:
    """One Method call inside an open scope — the handler's leg, written out.

    `actor_id` is the handler's derivation (runtime/handler/workflow.go): the key if the scope claimed
    one, else the Session id. Pinned on the Go side by runbatch_test.go and on the host side by
    tests/test_queue_congruence.py; written here because this stands in for that leg.
    """
    envelope = await workflow.execute_activity(
        "RunBatch",
        {
            "actor_id": key or session_id,
            "units": [i],
            "method": "whoami",
            "run_id": workflow.info().workflow_id,
            "node_id": f"n{i}",
        },
        task_queue=catalog.session_queue(ACTOR, VERSION, session_id),
        start_to_close_timeout=timedelta(seconds=60),
        schedule_to_start_timeout=PICKUP,
    )
    return (envelope.get("results") or [{}])[0]


@workflow.defn
class TwoCallsInOneScope:
    """Two Method calls in one scope. They must meet the same process AND the same instance.

    `key` is optional exactly as it is for a caller (ADR 0023 §10): "" opens a private anonymous
    Session, a key claims a shared identity whose `object_state` outlives the scope.
    """

    @workflow.run
    async def run(self, key: str = "") -> dict:
        crawler = catalog.actor(ACTOR, VERSION)
        handle = crawler[key] if key else crawler
        async with handle.session(close_timeout=CLOSE) as browser:
            first = await _whoami(browser.session_id, 1, key)
            second = await _whoami(browser.session_id, 2, key)
            return {"session": browser.session_id, "calls": [first, second]}


@workflow.defn
class ConcurrentScopes:
    """Several scopes at once, so pinning cannot be an artefact of there being one host."""

    @workflow.run
    async def run(self, n: int) -> list:
        crawler = catalog.actor(ACTOR, VERSION)

        async def one(i: int) -> dict:
            # `.session()` rather than `async with crawler`, because one handle cannot tell two
            # concurrent scopes apart at exit.
            async with crawler.session(close_timeout=CLOSE) as browser:
                return await _whoami(browser.session_id, i)

        return list(await asyncio.gather(*(one(i) for i in range(n))))


@workflow.defn
class LosingTheHost:
    """Open a scope, report which process took it, and call again once the test has killed it.

    Nothing here catches: the point is that the second call RAISES out of the `async with` (§7)
    rather than hanging or landing somewhere else with an empty instance, and that the scope's
    close does not wedge the workflow when its host is gone.
    """

    def __init__(self) -> None:
        self._pid = 0
        self._killed = False

    @workflow.query
    def pid(self) -> int:
        return self._pid

    @workflow.signal
    def host_is_gone(self) -> None:
        self._killed = True

    @workflow.run
    async def run(self) -> dict:
        crawler = catalog.actor(ACTOR, VERSION)
        async with crawler.session(close_timeout=CLOSE) as browser:
            first = await _whoami(browser.session_id, 1)
            self._pid = int(first.get("pid") or 0)
            await workflow.wait_condition(lambda: self._killed)
            second = await _whoami(browser.session_id, 2)
            return {"unreachable": second}


@workflow.defn
class CallAnOrphanedQueue:
    """Dispatch onto the queue of a Session that is already closed."""

    @workflow.run
    async def run(self, session_id: str) -> str:
        try:
            await _whoami(session_id, 99)
        except Exception as e:
            return f"{type(e).__name__}: {e}"
        return "landed"
