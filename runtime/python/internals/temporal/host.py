"""The actor host: a Temporal activity worker (ADR 0018, ADR 0023).

An actor process polls `{actor}-{version}-sessions` directly. It serves no HTTP and has no port.
The handler still owns the workflow — Temporal splits workflow and activity across languages by
design, which is what lets a Python actor and a Go handler be two halves of one run.

TWO PLANES, TWO QUEUES. On the actor's SHARED sessions queue every worker of this version polls,
and that is where an `OpenSession` lands: whichever host takes it activates the Session. That
host then spawns a second worker on the Session's OWN queue — `{actor}-{version}-s-{sessionId}`,
polled by nobody else — and every call inside the scope goes there (ADR 0023 §6). The queue name
is the address: no placement directory, no lease, no idle policy. An unscoped dispatch (no
Session id) still runs on the shared queue, which is what an Activity is in v2 — an Actor with
one Method and no scope around it.

SINGLE ACTIVATION IS A PER-PROCESS CONCERN. What serialises work for one actor id is Temporal:
one backing workflow per id, blocking on its activity. So a dict plus a lock is the whole of it,
and a cluster-wide registry would neither be needed nor help:

    _SESSIONS: {actor_id -> Session}       one live instance per id, in this process
    _LOCKS:    {actor_id -> asyncio.Lock}  one batch at a time for that id

HEARTBEATS ARE THE LIVENESS CHECK. The actor beats for itself, once per unit outcome, so a
wedged unit simply stops beating and `HeartbeatTimeout` catches it. Anything that beats on the
actor's behalf — a keepalive ticker on the calling side — cannot tell a working actor from a
wedged one, which leaves `StartToCloseTimeout` as the only bound and makes it a guillotine.
"""

from __future__ import annotations

import asyncio
import logging
import os
import socket
from datetime import timedelta
from typing import Any, Dict

from kontra.retry import NonRetryableError, SessionLost

from internals.catalog import publish_catalog
from internals.engine import build_session_factory
from internals.temporal.sessions import SessionWorkers

log = logging.getLogger("kontra.host")

# ONE IMPLEMENTATION FOR BOTH PANES. The workflow host wrote this first (see its docstring for the
# failure it fixes); importing it rather than repeating it is what keeps an actor's pane and a
# workflow's pane behaving the same way, which is the property that makes either of them worth
# looking at.
from internals.temporal.wfhost import _configure_logging  # noqa: E402
from internals.temporal.tlsconfig import connect_tls

# Per-process activation. Keyed by actor id, which is the run/node the handler derives — so two
# nodes of one run get two instances (two browsers), and a retry of the SAME node reuses the
# instance if this process still holds it. Inside a scope the actor id is the Session's (or its
# key's), so every call in the scope meets the same instance.
_SESSIONS: Dict[str, Any] = {}
_LOCKS: Dict[str, asyncio.Lock] = {}

# Which actor id each live Session in THIS process activated. Written by OpenSession and read by
# CloseSession, so ending one scope cannot close an instance another live scope is still holding
# — two scopes on one key share the instance, because a key IS the identity (ADR 0022).
_SESSION_ACTORS: Dict[str, str] = {}

# How long a Session's worker is given to finish what it is running before its tasks are
# cancelled. It must outlast the close activity itself, which is IN FLIGHT on that worker when
# the teardown starts: at the SDK default of zero the close would be cancelled a beat before it
# reported completion, and the caller's scope would exit on a cancellation instead of a close.
# Twice the 30s the caller allows that activity (`Session._close`), so the author's @actor.close
# is bounded by its own timeout rather than by this one.
_SESSION_DRAIN = timedelta(seconds=60)

# Activity slots on a Session's own worker. Small on purpose: only that Session's calls arrive
# there, the engine serialises batches per actor id anyway, and the one thing that must never
# queue behind a running batch is the close that ends the scope.
_SESSION_SLOTS = 4


#: THE MACHINE THIS HOST RUNS ON — the host portion of the Temporal worker identity, which is
#: exactly what the fleet's poller listing shows per Machine (`11@kf-dns-01@nscheck-0.1.0` is
#: `pid@host@queue`). The pid is deliberately dropped: a restarted Worker is the same Machine,
#: and keeping it would make one Droplet read as a new Machine after every restart.
#:
#: Snapshotted at import because a hostname does not change under a live process, and this is
#: read once per Batch. Neither SDK sets a custom Temporal identity, so the default derivation —
#: `{pid}@{hostname}` in Python, `{pid}@{hostname}@{queue}` in Go — makes this string and the
#: poller listing's host portion the same value on both halves of a Worker.
#:
#: On the fleet it is the Droplet's name (`kf-dns-01`): a Worker is a pair of systemd units ON
#: the Machine, not a container, so there is no container id standing between the two
#: (control/orchestrator/src/infra/programs/machine.ts).
MACHINE = socket.gethostname()


def _lock(actor_id: str) -> asyncio.Lock:
    lk = _LOCKS.get(actor_id)
    if lk is None:
        lk = _LOCKS[actor_id] = asyncio.Lock()
    return lk


def task_queue(name: str, version: str) -> str:
    """The sessions queue, byte-identical to the handler's derivation.

    `{name}-{version}-sessions`, or `{name}-shared-sessions` when a version is absent. The
    handler builds the same string from its workflow's own task queue (it cannot read env —
    workflow code must stay deterministic), so a mismatch here means an actor that registers,
    polls nothing, and looks idle.

    shared/conformance/queues.json §sessions is what holds the five derivations of this to one answer;
    tests/test_queue_congruence.py is this module's arm.
    """
    base = f"{name}-{version}" if version else f"{name}-shared"
    return f"{base}-sessions"


def session_task_queue(name: str, version: str, session_id: str) -> str:
    """ONE live Session's queue: `{name}-{version}-s-{sessionId}` (ADR 0023 §6).

    The singular of `task_queue` above and a different plane: that one is polled by every worker
    of this actor version and is where an open lands; this one is polled by the single worker
    that answered the open, which is what pins the scope's calls to one process.

    Derived independently in four languages with no shared code — here, the caller
    (`kontra.catalog.session_queue`, which closes on it), the Go actor host, and the handler,
    which dispatches onto it from its own task queue. A drift has no loud failure mode, so
    shared/conformance/queues.json §session is what holds them to one answer.

    NO ID, NO QUEUE: `{shared}-s-` is a real queue every Session of this actor would share.
    """
    if not session_id:
        raise ValueError("a per-session queue needs a session id — see ADR 0023 §6")
    base = f"{name}-{version}" if version else f"{name}-shared"
    return f"{base}-s-{session_id}"


def session_actor_id(session_id: str, key: str = "") -> str:
    """Which actor instance a Session activates: its key if it claimed one, else the Session id.

    Keys are optional (ADR 0023 §10). A key is a claim on a shared identity — it carries durable
    `object_state` across scopes and across Runs (ADR 0022) — while a bare handle is a private
    anonymous Session whose state begins empty and dies with it. Congruent with the actor id the
    handler derives for a scoped dispatch (`runtime/handler/workflow.go`); the two are written
    independently, and tests/test_queue_congruence.py pins that pair directly — it is a rule
    about ONE dispatch's identity, not a name two processes have to spell the same, so it is not
    in shared/conformance/queues.json.
    """
    return key or session_id


def build_activities(registry, *, sessions: SessionWorkers | None = None, session_factory=None):
    """Return the activities this actor serves, closed over its registry.

    Both keyword arguments are the host's seams, and both exist so the Session machinery is
    testable with no cluster: `sessions` is the live-Session table (which spawns Temporal
    workers), `session_factory` is the engine's `(actor_id, kv, heartbeat) -> Session`.
    """
    from temporalio import activity
    from temporalio.exceptions import ApplicationError

    make_session = session_factory or build_session_factory(registry)
    live = sessions if sessions is not None else live_sessions(registry)
    version = getattr(registry, "version", "") or ""

    def _session(actor_id: str):
        s = _SESSIONS.get(actor_id)
        if s is None:
            # The heartbeat is bound per session rather than per call because the engine holds
            # it for the life of the batch; `activity.heartbeat` is context-local and resolves
            # against whichever activity execution is running when it fires.
            s = _SESSIONS[actor_id] = make_session(actor_id, heartbeat=activity.heartbeat)
        return s

    @activity.defn(name="RunBatch")
    async def run_batch(payload: dict) -> dict:
        actor_id = str(payload.get("actor_id") or payload.get("actorID") or "")
        if not actor_id:
            raise ValueError("RunBatch payload carries no actor_id")
        # One batch at a time per actor id. Two concurrent batches would share the instance the
        # author opened in @actor.load — one browser, two batches writing the same commit map.
        async with _lock(actor_id):
            session = _session(actor_id)
            try:
                out = await session.run_batch(payload)
                # PROVENANCE TRAVELS WITH THE BATCH. The handler copies this onto the result
                # ref's meta, the caller's `Batch` reads it from there without a fetch, and
                # `publish` writes it as the row's Machine. It has to be stamped HERE, in the
                # process that actually ran the Method: the handler workflow polls a queue every
                # Machine's handler polls, so its own hostname would name a Machine at random.
                #
                # Placed in the host and not in the engine because it is a fact about WHERE this
                # process runs, not about the Units — the engine has no business knowing.
                out["machine"] = MACHINE
                return out
            except SessionLost as e:
                # The resource died, so the Session is over (ADR 0023 §20) — and it is KEPT, not
                # dropped. Dropping it would let the next call build a fresh one and load a
                # second resource under a scope whose `self.*` is gone, which is the reload this
                # rule deletes. Non-retryable for the same reason: nothing about a dead resource
                # improves by asking the same Session again. The caller's scope is what recovers,
                # by reopening and resuming from the cursor it holds (§7).
                raise ApplicationError(str(e), type="SessionLost", non_retryable=True) from e
            except NonRetryableError as e:
                # TERMINAL MEANS TERMINAL, INCLUDING OUT OF @actor.load. `_classify` already
                # honours this for a raise inside a Method body — the Unit is isolated, not
                # retried — but a raise from the LOAD escapes `run_batch` before any Unit runs,
                # and an uncaught exception here gets Temporal's DEFAULT retry policy: unlimited
                # attempts. So the one error the author declared un-retryable became the one that
                # retried hardest.
                #
                # What that costs is not abstract. `redditscrape-1787584680` opened an OAuth
                # session against a credential Reddit refuses; the 401 is a NonRetryableError by
                # construction, and it was re-attempted until Reddit answered 429 — 202 auth
                # POSTs in forty seconds from a single 3-Unit Batch. Against a real account that
                # is indistinguishable from credential stuffing.
                #
                # The Session is NOT ended here, unlike SessionLost: a terminal error says this
                # CALL cannot succeed, not that the resource died.
                raise ApplicationError(
                    str(e), type="NonRetryableError", non_retryable=True) from e

    @activity.defn(name="Close")
    async def close(payload: dict) -> dict:
        actor_id = str(payload.get("actor_id") or payload.get("actorID") or "")
        await _close_instance(actor_id)
        return {"closed": True}

    @activity.defn(name="OpenSession")
    async def open_session(payload: dict) -> dict:
        """Activate a Session on this host and start polling its queue (ADR 0023 §4, §6).

        Runs on the actor's SHARED sessions queue, so Temporal's own dispatch picks the host —
        that choice is the placement, and the answer rides back as `pid` because nothing else
        can tell a caller which process took the scope.

        Idempotent: a retry of this activity must not build a second worker on one Session's
        queue. Refusing at the cap is deliberately retryable, so a busy fleet applies
        back-pressure to the open instead of failing a scope.
        """
        session_id = str(payload.get("session_id") or payload.get("sessionId") or "")
        if not session_id:
            raise ValueError("OpenSession payload carries no session_id")
        key = str(payload.get("key") or "")
        queue = session_task_queue(registry.actor_name, version, session_id)
        opened = await live.open(session_id, queue)
        _SESSION_ACTORS[session_id] = session_actor_id(session_id, key)
        return {
            "session_id": session_id,
            "actor_id": _SESSION_ACTORS[session_id],
            "queue": queue,
            "pid": os.getpid(),
            "opened": opened,
        }

    @activity.defn(name="CloseSession")
    async def close_session(payload: dict) -> dict:
        """End a Session: the author's @actor.close, then the Session's worker (ADR 0023 §4).

        THIS RUNS ON THE SESSION'S OWN QUEUE, so it is an in-flight activity of the worker it is
        tearing down — see `internals/temporal/sessions.py`, TRAP 1. The teardown is detached
        there; awaiting it here would hang the close, and a hung close is a scope that never
        exits rather than an error anybody sees.
        """
        session_id = str(payload.get("session_id") or payload.get("sessionId") or "")
        if not session_id:
            raise ValueError("CloseSession payload carries no session_id")
        actor_id = _SESSION_ACTORS.pop(
            session_id, session_actor_id(session_id, str(payload.get("key") or "")))
        held = await live.close(session_id)
        # Only when no OTHER live scope in this process shares the instance: two scopes on one
        # key are two Sessions of one identity, and closing the resource under the survivor
        # would reset its `self.*` with nothing raised — the failure shape §7 exists to forbid.
        if actor_id not in _SESSION_ACTORS.values():
            await _close_instance(actor_id)
        return {"closed": True, "held": held, "session_id": session_id, "pid": os.getpid()}

    return [run_batch, close, open_session, close_session]


async def _close_instance(actor_id: str) -> None:
    """Run @actor.close on this process's instance for `actor_id`, if it holds one."""
    async with _lock(actor_id):
        session = _SESSIONS.pop(actor_id, None)
        if session is not None:
            await session.close()
        _LOCKS.pop(actor_id, None)


# The live Sessions this process holds. One per process, built on first use because the worker
# that serves a Session is spawned from inside an activity (that is where a client exists).
_LIVE: SessionWorkers | None = None


def max_parallel_sessions() -> int:
    """How many Sessions one host offers at once — the cap that used to be
    `max_concurrent_activities` and could not stay there (see sessions.py, TRAP 2)."""
    return int(os.environ.get("KONTRA_MAX_PARALLEL_SESSIONS", "4"))


def live_sessions(registry) -> SessionWorkers:
    global _LIVE
    if _LIVE is None:
        _LIVE = SessionWorkers(_spawn_session_worker(registry), max_live=max_parallel_sessions())
    return _LIVE


def _spawn_session_worker(registry):
    """Build the `spawn(queue) -> Worker` that activates a Session on this process.

    The client is the one the OPEN is running on (`activity.client()`), so the Session's worker
    inherits the claim-check codec and the address without any of it being passed around.
    """
    def spawn(queue: str):
        from temporalio import activity
        from temporalio.worker import Worker

        return Worker(
            activity.client(),
            task_queue=queue,
            activities=build_activities(registry, sessions=live_sessions(registry)),
            max_concurrent_activities=_SESSION_SLOTS,
            graceful_shutdown_timeout=_SESSION_DRAIN,
        )

    return spawn


def _install_blob_reader() -> None:
    """Give `kontra.File.read()` a way to fetch — the runtime half of an SDK-declared type.

    THE ARROW IS RUNTIME → SDK, WHICH IS WHY THIS IS HERE AND NOT THERE. `kontra.blobs` declares
    `File` and `Folder` and holds a `None` reader; anything that linked an S3 client into the author
    surface would be importable only by a caller that has one, and `tests/test_sdk_arrow.py` fails
    the build over it — statically AND by importing every SDK module with `internals` made
    unimportable. So the side that is allowed to have the store registers the fetch.

    IT IS THE SAME STORE THE CODEC USES, deliberately: `casstore.from_env()` and
    `codec.cas_key` are what offload a claim-check payload and what the orchestrator's upload route
    content-addresses into. One derivation of `cas/<sha[:2]>/<sha>`, under one prefix, for the whole
    system — which is what makes an uploaded file dereferenceable with nothing new to configure.

    NO STORE IS NOT AN ERROR HERE. A local run with `KONTRA_S3_ENDPOINT` unset is legitimate and the
    codec is passthrough under exactly the same condition; an actor that never takes a File never
    notices, and one that does gets `File.read()`'s own sentence rather than a failure at startup
    about a feature it may not use.
    """
    from internals import casstore
    from internals.codec import cas_key

    store = casstore.from_env()
    if store is None:
        return

    async def read(sha: str) -> bytes:
        # BLOCKING boto3 ON A THREAD. A Method body runs on this worker's event loop, and a
        # synchronous get of a 30 MB object there stalls every other Unit in flight — including the
        # heartbeats, which is how a healthy worker gets its activity timed out.
        return await asyncio.to_thread(store.get, cas_key(sha))

    from kontra import blobs

    blobs.set_blob_reader(read)


async def serve_async(registry, *, address: str = "", namespace: str = "") -> None:
    # LOGGING FIRST, and for the reason `wfhost._configure_logging` records at length: Python emits
    # nothing until a handler exists, so an actor author's `logging.getLogger(__name__).info(...)`
    # — and the SDK's own warnings about activity failures and retries — were discarded. `print()`
    # reached the pane and `logging` did not, which is a distinction nobody writing a Method should
    # have to know. This is the same call the workflow host makes, so both panes behave alike.
    _configure_logging()

    from temporalio.client import Client
    from temporalio.worker import Worker

    from internals import casstore, metrics

    address = address or os.environ.get("KONTRA_ADDRESS", "localhost:7233")
    namespace = namespace or os.environ.get("KONTRA_NAMESPACE", "default")
    version = getattr(registry, "version", "") or ""
    queue = task_queue(registry.actor_name, version)

    # The claim-check codec is NOT optional here. The handler's client encodes any payload over
    # 128 KiB into a `binary/claim-check-v1` ref, and this worker executes that activity — so
    # without the matching codec every over-threshold batch dies on "Unknown payload encoding",
    # retried to exhaustion. It passes through when KONTRA_S3_ENDPOINT is unset, exactly as the
    # handler's does, so a local no-S3 run is unaffected.
    # TLS from the environment, in one place for every client in this repository — see
    # `internals/temporal/tlsconfig.py`. `False` when nothing is configured, which is what the
    # SDK means by no TLS and what this call passed before.
    tls = connect_tls()
    client = await Client.connect(
        address, namespace=namespace, data_converter=casstore.data_converter(), tls=tls
    )
    metrics.serve(registry.actor_name, version)  # /metrics on its own port
    publish_catalog(registry)                    # self-register so the actor is dispatchable
    _install_blob_reader()                       # what `kontra.File.read()` calls

    # `max_concurrent_activities` bounds what runs on THIS queue at once, and it is no longer the
    # live-Session cap — an OpenSession frees its slot the moment the Session's worker is up, so
    # this number would bound opens in flight, not Sessions alive. The cap that means what it
    # says is KONTRA_MAX_PARALLEL_SESSIONS, counted in live Sessions by `SessionWorkers`.
    worker = Worker(
        client,
        task_queue=queue,
        activities=build_activities(registry, sessions=live_sessions(registry)),
        max_concurrent_activities=max(4, max_parallel_sessions()),
    )
    log.info("[host] %s@%s serving on %s (%s), %d live Sessions max",
             registry.actor_name, version, queue, address, max_parallel_sessions())
    print(f"[host] {registry.actor_name}@{version} -> {queue} @ {address}", flush=True)
    await worker.run()


def serve(registry, **kw) -> None:
    """THE way `actor.serve()` boots a Python actor."""
    asyncio.run(serve_async(registry, **kw))
