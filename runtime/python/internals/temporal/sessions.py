"""The live Sessions one host process holds — a Worker apiece (ADR 0023 §6).

A **Session** is an explicit scope the caller's workflow owns, and it is bound by its own task
queue: `{actor}-{version}-s-{sessionId}`, polled by the one Worker that activated it. That queue
name is the whole addressing scheme. There is no placement directory, no lease and no idle
policy, and the properties that buys are the point:

  • every call inside the scope lands on the process holding the loaded resource, because no
    other process polls that queue;
  • losing the host orphans the queue, so calls sit until ScheduleToStart fires and the scope
    RAISES (§7) instead of silently re-activating somewhere with an empty `self.*`;
  • the scope ends when the caller says so, and the Worker goes with it.

Temporal's own Worker Sessions are exactly this feature and are Go/Java only, which is why it
is built here rather than configured.

WHY THE WORKER IS INJECTED. `spawn(queue) -> worker` is the seam, so this module is testable
with no cluster and the two traps below are pinned by tests rather than by care.

TRAP 1 — THE CLOSE MUST NOT AWAIT ITS OWN WORKER'S SHUTDOWN. The close activity runs ON the
Session's queue, so it is an in-flight activity of the very Worker it tears down, and a graceful
shutdown waits for in-flight activities to finish. Awaiting inline is a deadlock that presents
as a hung close, not as an error. `close()` therefore detaches the teardown and returns.

TRAP 2 — `max_concurrent_activities` IS NOT THE LIVE-SESSION CAP. It was, while a live Session
meant an activity slot held for the length of a batch. The open activity returns as soon as it
has spawned the Session's Worker, freeing its slot immediately, so a 2-slot worker will happily
hold six live Sessions. The cap lives here, counted in live Sessions.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any, Callable, Dict, Set

log = logging.getLogger("kontra.sessions")


class SessionCapReached(RuntimeError):
    """This host is already holding as many Sessions as it offers.

    Deliberately an ordinary retryable error: the open is a Temporal activity, so a refusal
    becomes back-pressure — the task returns to the actor's shared queue and any host with a free
    slot takes it. A non-retryable failure here would fail a caller's scope for a fleet that is
    merely busy.
    """


class SessionWorkers:
    """The Sessions this process holds, one Worker each. Not thread-safe; one event loop."""

    def __init__(self, spawn: Callable[[str], Any], *, max_live: int) -> None:
        self._spawn = spawn
        self.max_live = max_live
        self._live: Dict[str, Any] = {}
        self._runs: Dict[str, asyncio.Task] = {}
        # Detached teardowns, held only so the event loop does not garbage-collect a task that
        # nothing awaits (asyncio keeps weak references).
        self._teardowns: Set[asyncio.Task] = set()

    def __len__(self) -> int:
        return len(self._live)

    def __contains__(self, session_id: str) -> bool:
        return session_id in self._live

    async def open(self, session_id: str, queue: str) -> bool:
        """Activate a Session on `queue`. True if this call spawned its Worker.

        IDEMPOTENT, because a Temporal retry of the open must not build a second Worker on one
        Session's queue — two pollers would split the scope's calls across two instances of the
        actor, which is the pinning silently gone on a path that only happens under retry.
        """
        if session_id in self._live:
            return False
        if len(self._live) >= self.max_live:
            raise SessionCapReached(
                f"host holds {len(self._live)}/{self.max_live} live Sessions; "
                f"cannot open {session_id} (KONTRA_MAX_PARALLEL_SESSIONS)"
            )
        worker = self._spawn(queue)
        run = asyncio.ensure_future(worker.run())
        # Give the poller a turn to fail: a Worker that dies on boot would otherwise leave the
        # open reporting success while its queue has no poller, which the caller meets as a hang
        # rather than as an error. Failing the open lets the retry land on another host.
        await asyncio.sleep(0)
        if run.done():
            run.result()  # raises whatever killed it
            raise RuntimeError(f"session worker for {session_id} exited before serving {queue}")
        self._live[session_id] = worker
        self._runs[session_id] = run
        log.info("[sessions] opened %s on %s (%d live)", session_id, queue, len(self._live))
        return True

    async def close(self, session_id: str) -> bool:
        """End a Session and free its slot. True if this process held it.

        Returns as soon as the teardown is under way — see TRAP 1. Not an error for a Session
        this process never held: the close is best-effort and retried, and a scope that is
        already over must not fail because its host is gone.
        """
        worker = self._live.pop(session_id, None)
        run = self._runs.pop(session_id, None)
        if worker is None:
            return False
        task = asyncio.ensure_future(self._teardown(session_id, worker, run))
        self._teardowns.add(task)
        task.add_done_callback(self._teardowns.discard)
        return True

    async def _teardown(self, session_id: str, worker: Any, run: asyncio.Task | None) -> None:
        try:
            await worker.shutdown()
            if run is not None:
                await run
        except Exception as e:  # pragma: no cover - a torn-down worker must not raise anywhere
            log.warning("[sessions] teardown of %s: %s", session_id, e)
        log.info("[sessions] closed %s (%d live)", session_id, len(self._live))
