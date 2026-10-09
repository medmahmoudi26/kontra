"""A Session is an explicit scope bound by its own task queue (ADR 0023 §4, §6, §7).

The host half. What is tested here is the machinery that makes the queue name an address: one
Worker per live Session, spawned by the open and torn down by the close, with the process's cap
counted in live Sessions rather than in activity slots.

The Temporal Worker is injected, so these run with no cluster. The live-cluster proof — two
Method calls landing on one process while several hosts poll, and an orphaned queue timing out
— is `test_session_pinning_live.py`, which skips when no server answers.
"""

from __future__ import annotations

import asyncio

import pytest

from internals.temporal.sessions import SessionCapReached, SessionWorkers


@pytest.fixture(autouse=True)
def a_fresh_process():
    """The host's activation tables are per-PROCESS state (they are what "one live instance per
    id" means), so a test that left one behind would be read by the next as a live Session."""
    from internals.temporal import host

    for table in (host._SESSIONS, host._LOCKS, host._SESSION_ACTORS):
        table.clear()
    yield
    for table in (host._SESSIONS, host._LOCKS, host._SESSION_ACTORS):
        table.clear()


class FakeWorker:
    """A stand-in for temporalio's Worker with the two methods the host drives it by."""

    def __init__(self, queue: str) -> None:
        self.queue = queue
        self.running = False
        self.shutdown_called = False
        self._stopped = asyncio.Event()
        #: Held closed so a test can assert the close path does not wait on it.
        self.may_shutdown = asyncio.Event()
        self.may_shutdown.set()

    async def run(self) -> None:
        self.running = True
        await self._stopped.wait()

    async def shutdown(self) -> None:
        self.shutdown_called = True
        await self.may_shutdown.wait()
        self.running = False
        self._stopped.set()


def workers(max_live: int = 4):
    built: list[FakeWorker] = []

    def spawn(queue: str) -> FakeWorker:
        built.append(FakeWorker(queue))
        return built[-1]

    return SessionWorkers(spawn, max_live=max_live), built


def test_opening_a_session_spawns_one_worker_on_that_sessions_queue():
    live, built = workers()

    async def scenario():
        assert await live.open("s1", "crawler-0.1.0-s-s1") is True

    asyncio.run(scenario())
    assert [w.queue for w in built] == ["crawler-0.1.0-s-s1"]
    assert len(live) == 1


def test_reopening_the_same_session_does_not_build_a_second_worker():
    """The open is a Temporal activity, so it retries. Two Workers polling one Session's queue
    would split its calls across two instances of the actor — the pinning gone, silently, on a
    path that only happens under retry."""
    live, built = workers()

    async def scenario():
        assert await live.open("s1", "q-s-s1") is True
        assert await live.open("s1", "q-s-s1") is False

    asyncio.run(scenario())
    assert len(built) == 1
    assert len(live) == 1


def test_the_cap_counts_live_sessions_not_activity_slots():
    """`max_concurrent_activities` stopped being this cap the moment the open activity returned
    as soon as it had spawned the Session's worker: it frees its slot immediately, so a 2-slot
    worker will happily hold six live Sessions. The bound has to be a count of live Sessions or
    it is not a bound at all."""
    live, _ = workers(max_live=2)

    async def scenario():
        await live.open("s1", "q-s-s1")
        await live.open("s2", "q-s-s2")
        with pytest.raises(SessionCapReached):
            await live.open("s3", "q-s-s3")

    asyncio.run(scenario())
    assert len(live) == 2


def test_closing_a_session_frees_its_slot():
    live, _ = workers(max_live=1)

    async def scenario():
        await live.open("s1", "q-s-s1")
        await live.close("s1")
        assert len(live) == 0
        assert await live.open("s2", "q-s-s2") is True

    asyncio.run(scenario())


def test_the_close_does_not_wait_for_its_own_workers_shutdown():
    """THE deadlock. The close runs ON the Session's queue, so it is an in-flight activity of
    the very worker it is tearing down — and a graceful shutdown waits for in-flight activities.
    Awaiting it inline hangs the close forever, and a hang is not an error: the scope just never
    exits. Detached, the close returns and the worker drains it."""
    live, built = workers()

    async def scenario():
        await live.open("s1", "q-s-s1")
        built[0].may_shutdown.clear()  # this worker will never finish shutting down

        await asyncio.wait_for(live.close("s1"), timeout=1)

        assert len(live) == 0
        await asyncio.sleep(0)  # let the detached teardown reach its first await
        assert built[0].shutdown_called is True
        assert built[0].running is True, "the shutdown was awaited inline"

    asyncio.run(scenario())


def test_closing_a_session_this_process_never_held_is_not_an_error():
    """The close is best-effort and retried, and the host that answers it may not be the host
    that opened the scope. A raise here would fail a scope that is already over."""
    live, _ = workers()

    async def scenario():
        assert await live.close("never-opened") is False

    asyncio.run(scenario())


class FakeSession:
    """An engine Session double: the two calls the host makes on one."""

    def __init__(self, actor_id: str, closed: list) -> None:
        self.actor_id, self._closed = actor_id, closed

    async def run_batch(self, payload: dict, resume=None) -> dict:
        return {"done": True, "results": list(payload.get("units") or []), "failures": []}

    async def close(self) -> dict:
        self._closed.append(self.actor_id)
        return {"closed": True}


def host_activities(live, closed=None):
    """The host's four activities, with the Worker and the engine's Session factory injected."""
    from kontra import ActorRegistry
    from internals.temporal import host

    reg = ActorRegistry()
    reg.actor_name, reg.version = "crawler", "0.1.0"
    fns = host.build_activities(
        reg,
        sessions=live,
        session_factory=(lambda aid, **kw: FakeSession(aid, closed)) if closed is not None else None,
    )
    return {f.__name__: f for f in fns}


def test_the_open_activity_spawns_the_session_worker_on_the_derived_queue():
    """The queue name is the address (ADR 0023 §6), so the host must derive the SAME string the
    caller closes on and the handler dispatches onto — there is no registration step that would
    catch a mismatch, only a scope whose calls sit in a queue nobody polls."""
    live, built = workers()
    acts = host_activities(live)

    out = asyncio.run(acts["open_session"]({"session_id": "3f9a", "key": ""}))

    assert [w.queue for w in built] == ["crawler-0.1.0-s-3f9a"]
    assert out["queue"] == "crawler-0.1.0-s-3f9a"


def test_the_open_reports_the_process_that_took_it():
    """Which host answered the open is the one fact a caller cannot derive: the queue chose it.
    It rides back so a run can be traced to a process, and so pinning is observable at all."""
    import os

    live, _ = workers()
    acts = host_activities(live)

    out = asyncio.run(acts["open_session"]({"session_id": "3f9a"}))
    assert out["pid"] == os.getpid()


def test_an_open_with_no_session_id_is_refused():
    """There is no default. A blank id derives a queue every Session of this actor would share,
    which is the pinning gone with nothing failing."""
    live, _ = workers()
    acts = host_activities(live)

    with pytest.raises(ValueError):
        asyncio.run(acts["open_session"]({"session_id": ""}))


def test_closing_the_scope_ends_the_actor_instance_and_the_worker():
    """`async with` is the actor's lifetime: whatever it holds in memory lives exactly this long
    (CONTEXT: Session). So the close runs the author's @actor.close and takes the Worker with it
    — a Session that ended but left its resource open leaks a browser onto a fleet Machine."""
    closed = []
    live, built = workers()
    acts = host_activities(live, closed)

    async def scenario():
        await acts["open_session"]({"session_id": "3f9a"})
        await acts["run_batch"]({"actor_id": "3f9a", "units": [1]})
        out = await acts["close_session"]({"session_id": "3f9a"})
        assert out["closed"] is True
        await asyncio.sleep(0)

    asyncio.run(scenario())
    assert closed == ["3f9a"], "the author's @actor.close did not run"
    assert built[0].shutdown_called is True
    assert len(live) == 0


def test_the_close_leaves_a_second_scope_on_the_same_key_alone():
    """Two scopes on one key share this process's instance, because a key IS the identity (ADR
    0022). Ending one must not close the resource the other is still calling — that would be a
    Session losing its `self.*` with nothing raised, which is the one thing §7 forbids."""
    closed = []
    live, _ = workers()
    acts = host_activities(live, closed)

    async def scenario():
        await acts["open_session"]({"session_id": "s1", "key": "acme.com"})
        await acts["open_session"]({"session_id": "s2", "key": "acme.com"})
        await acts["run_batch"]({"actor_id": "acme.com", "units": [1]})
        await acts["close_session"]({"session_id": "s1", "key": "acme.com"})
        assert closed == [], "closed an instance a live Session is still holding"
        await acts["close_session"]({"session_id": "s2", "key": "acme.com"})

    asyncio.run(scenario())
    assert closed == ["acme.com"]


class DeadResourceSession:
    """A Session whose resource dies on the first call and stays dead — what the engine does
    under ADR 0023 §20."""

    def __init__(self, actor_id, built):
        from kontra.retry import SessionLost

        self.actor_id = actor_id
        self._lost = SessionLost
        self._ended = False
        built.append(actor_id)

    async def run_batch(self, payload: dict, resume=None) -> dict:
        if self._ended:
            raise self._lost(f"session {self.actor_id} ended when its resource died")
        self._ended = True
        raise self._lost("resource died mid-batch")

    async def close(self) -> dict:
        return {"closed": True}


def test_a_dead_session_is_not_rebuilt_by_the_next_call():
    """ADR 0023 §20 at the host boundary. The engine ends a Session when its resource dies, and
    the host must not answer the next call by CONSTRUCTING a fresh one — that is the reload
    under another name: a new resource welded under a scope whose `self.*` is gone."""
    from temporalio.exceptions import ApplicationError
    from kontra import ActorRegistry
    from internals.temporal import host

    built: list[str] = []
    live, _ = workers()
    reg = ActorRegistry()
    reg.actor_name, reg.version = "crawler", "0.1.0"
    acts = {f.__name__: f for f in host.build_activities(
        reg, sessions=live,
        session_factory=lambda aid, **kw: DeadResourceSession(aid, built))}

    async def scenario():
        await acts["open_session"]({"session_id": "3f9a"})
        for _ in range(2):
            with pytest.raises(ApplicationError):
                await acts["run_batch"]({"actor_id": "3f9a", "units": [1]})

    asyncio.run(scenario())
    assert built == ["3f9a"], "the host built a second Session for a scope that had ended"


def test_a_dead_resource_is_not_retried_into_a_re_activation():
    """The retry is the other way a reload sneaks back: a Temporal activity retry lands on the
    same Session queue and asks again. Nothing about a dead resource improves by asking twice,
    so the failure is raised NON-RETRYABLE and the caller's scope is what recovers (§7)."""
    from temporalio.exceptions import ApplicationError
    from kontra import ActorRegistry
    from internals.temporal import host

    live, _ = workers()
    reg = ActorRegistry()
    reg.actor_name, reg.version = "crawler", "0.1.0"
    acts = {f.__name__: f for f in host.build_activities(
        reg, sessions=live,
        session_factory=lambda aid, **kw: DeadResourceSession(aid, []))}

    async def scenario():
        await acts["open_session"]({"session_id": "3f9a"})
        with pytest.raises(ApplicationError) as raised:
            await acts["run_batch"]({"actor_id": "3f9a", "units": [1]})
        assert raised.value.non_retryable is True
        assert raised.value.type == "SessionLost"

    asyncio.run(scenario())


class TerminalLoadSession:
    """A Session whose `@actor.load` raises NonRetryableError — a refused credential, a missing
    required param: the errors an author declares terminal because no retry can fix them."""

    def __init__(self, actor_id, attempts):
        from kontra.retry import NonRetryableError

        self.actor_id = actor_id
        self._terminal = NonRetryableError
        self._attempts = attempts

    async def run_batch(self, payload: dict, resume=None) -> dict:
        self._attempts.append(1)
        raise self._terminal("reddit refused the credentials (HTTP 401)")


def test_a_terminal_error_out_of_the_load_is_not_retried_by_the_activity():
    """THE BUG THIS PINS, measured on `redditscrape-1787584680`.

    `_classify` already honours NonRetryableError for a raise inside a Method body — the Unit is
    isolated, never retried. But a raise from `@actor.load` escapes `run_batch` before any Unit
    runs, and this boundary caught only SessionLost. Everything else fell through as an ordinary
    exception and got Temporal's DEFAULT activity retry policy, which is unlimited attempts — so
    the one error the author declared un-retryable became the one that retried hardest.

    An OAuth actor opened against a credential Reddit refuses re-attempted the mint until Reddit
    answered 429: 202 auth POSTs in forty seconds from a single 3-Unit Batch. Against a real
    account that is indistinguishable from credential stuffing, and it is the account holder who
    pays for it.
    """
    from temporalio.exceptions import ApplicationError
    from kontra import ActorRegistry
    from internals.temporal import host

    attempts: list[int] = []
    live, _ = workers()
    reg = ActorRegistry()
    reg.actor_name, reg.version = "crawler", "0.1.0"
    acts = {f.__name__: f for f in host.build_activities(
        reg, sessions=live,
        session_factory=lambda aid, **kw: TerminalLoadSession(aid, attempts))}

    async def scenario():
        await acts["open_session"]({"session_id": "3f9a"})
        with pytest.raises(ApplicationError) as raised:
            await acts["run_batch"]({"actor_id": "3f9a", "units": [1]})
        # non_retryable is what actually stops the storm: Temporal will not schedule attempt 2.
        assert raised.value.non_retryable is True
        assert raised.value.type == "NonRetryableError"
        assert "reddit refused" in str(raised.value), "the cause must survive the boundary"

    asyncio.run(scenario())
    assert attempts == [1]


# ---------------------------------------------------------------------------------------------
# The caller's side of the scope. What can be checked without a cluster is the wiring: which
# queue each leg is addressed to, and that the two activity names the caller schedules are the
# two the host registers. The behaviour — pinning, the close completing, an orphaned queue timing
# out — is `test_session_pinning_live.py`.
# ---------------------------------------------------------------------------------------------


def test_the_caller_and_the_host_agree_on_the_two_activity_names():
    """The caller schedules these BY NAME onto a queue it derived. Nothing registers a contract:
    a rename on one side is a task nobody can execute, which the scope meets as a timeout."""
    from temporalio.activity import _Definition

    from kontra import catalog

    live, _ = workers()
    registered = {_Definition.must_from_callable(f).name for f in host_activities(live).values()}
    assert catalog.OPEN_SESSION_ACTIVITY in registered
    assert catalog.CLOSE_SESSION_ACTIVITY in registered


def test_a_scope_is_opened_on_the_actors_shared_queue_and_closed_on_its_own():
    """Two different queues on purpose (ADR 0023 §6). The open goes where every worker of the
    actor polls, because Temporal's dispatch IS the placement decision; the close goes to the one
    queue the host that answered is polling, because only that process holds the resource."""
    import inspect

    from kontra import catalog

    opening = inspect.getsource(catalog.Session._open)
    closing = inspect.getsource(catalog.Session._close)
    assert "sessions_queue(" in opening and "session_queue(" not in opening
    assert "session_queue(" in closing


def test_a_method_call_in_a_scope_carries_the_session_id():
    """The id is what binds the call to the activated Actor: the handler reads it to address the
    Session's queue. A call that drops it lands on the shared queue and runs against a fresh
    instance — a Session that silently stopped being one."""
    import inspect

    from kontra import catalog

    src = inspect.getsource(catalog.Session._dispatch_batch)
    assert "session_id=self.session_id" in src
    assert "session_id" in inspect.signature(catalog.ActorHandle.dispatch_ref).parameters
    assert "session_id=session_id" in inspect.getsource(catalog.ActorHandle.dispatch_ref)


def test_a_method_is_named_by_attribute_on_the_open_session():
    """`browser.crawl(batch)` — the Method name is the attribute, so the call site reads like the
    Actor's own API rather than like a string dispatch. Calling it builds a `MethodCall` bound to
    this scope and that Method (ADR 0023 §8), the object that is both awaitable and iterable."""
    from kontra import catalog

    scope = catalog.actor("crawler", "0.1.0").session()
    scope._id = "3f9a"
    built = scope.crawl
    assert built.func is catalog.MethodCall and built.args == (scope, "crawl")
    call = built([1, 2])
    assert isinstance(call, catalog.MethodCall)


def test_the_scope_refuses_to_dispatch_before_it_is_opened():
    """Outside `async with` there is no Session id, so the call would go to the shared queue and
    run against a fresh instance. Loud, because the wrong version of this is silent."""
    from kontra import catalog

    scope = catalog.actor("crawler", "0.1.0").session()
    with pytest.raises(RuntimeError):
        asyncio.run(scope.call("crawl", [1]))


def test_a_bare_handle_and_a_keyed_one_both_open_a_scope():
    """Keys are optional (ADR 0023 §10): a bare handle is a private anonymous Session, a key is a
    claim on a shared identity. Mandatory keys would mean two independent scans of one host
    silently receiving each other's results."""
    from kontra import catalog

    crawler = catalog.actor("crawler", "0.1.0")
    assert crawler.session().key == ""
    assert crawler["acme.com"].session().key == "acme.com"


def test_one_handle_will_not_hold_two_anonymous_scopes_at_once():
    """`async with handle` has to pair its exit with an entry, and a module-scope handle entered
    twice concurrently cannot tell which scope is exiting. Refused by name rather than closing
    the wrong Session, and `.session()` is the answer the error gives."""
    from kontra import catalog

    crawler = catalog.actor("crawler", "0.1.0")

    async def scenario():
        crawler._scope = crawler.session()  # stand in for a scope already open
        with pytest.raises(RuntimeError, match="session()"):
            await crawler.__aenter__()

    asyncio.run(scenario())


def test_a_worker_that_dies_on_boot_fails_the_open():
    """The open returning success while its worker is already dead leaves the caller dispatching
    into a queue nobody polls, which reads as a hang until ScheduleToStart. Fail the open instead
    — it is retried, and the retry can land on another host."""

    class DeadWorker(FakeWorker):
        async def run(self):
            raise RuntimeError("no such namespace")

    live = SessionWorkers(lambda q: DeadWorker(q), max_live=4)

    async def scenario():
        with pytest.raises(RuntimeError):
            await live.open("s1", "q-s-s1")

    asyncio.run(scenario())
    assert len(live) == 0
