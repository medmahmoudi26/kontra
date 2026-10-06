"""The batch engine — one Batch in, results out, durable per unit (ADR 0018, ADR 0023).

This is the correctness core of an actor run and it is runtime-agnostic on purpose:
commit-by-identity, per-unit isolation, the Session that ends when its resource dies, and
durable-at-push records all live here, and none of them know what invoked them. ADR 0018
swapped the runtime under it (a sidecar-hosted actor -> a Temporal activity) without touching a
line of this logic — the argument for the swap being cheap, so it had better stay true.

    load  ->  method(batch)  ->  close
              ^ the AUTHOR loops; the framework commits each Unit as the loop moves past it

WHAT THE HOST SUPPLIES. A session is constructed with an actor id and a state store, and that
is the whole seam. Activation — "one live instance per actor id" — is the host's job; it was
the runtime's, and it is a dict plus a lock in the Temporal worker.

ONE ACTIVITY PER BATCH. The multi-turn protocol is gone (ADR 0018 §3). It existed only because
the handler could not observe the actor: a turn was time-boxed so a wedged host would return
rather than be killed by StartToClose. Now the actor heartbeats for itself, so a wedged unit
simply stops heartbeating and HeartbeatTimeout is a true liveness check. A resource therefore
loads exactly ONCE per batch by construction.

Execution is AT-LEAST-ONCE with exactly-once result recording: Method bodies must tolerate
replay, and the commit map makes a retry skip what already finished.
"""

from __future__ import annotations

import asyncio
import dataclasses
import hashlib
import importlib
import inspect
import json
import logging
import os
import urllib.request


from kontra import param
from kontra.retry import NonRetryableError, SessionLost

from kontra.batch import Batch, Dataset
from internals.checkpoint import Checkpoint as _Checkpoint
from internals import logs, workerid
from internals.globalstore import GlobalStore, object_prefix
from internals.redis_kv import redis_kv_from_env
from internals.statekv import STATE_TTL_S, ActorStateKV, state_kv
from internals.unitstore import from_env as unitstore_from_env, unit_ref
from internals import metrics

# the SAME module instance `param` uses, so setting _run_params actually feeds param.get()
_lib = importlib.import_module(type(param).__module__)  # kontra.actor
_run_params = _lib._run_params
_ckpt_io = _lib._ckpt_io
_global_io = _lib._global_io
_object_io = _lib._object_io
KontraBase = _lib.Actor


# `session_state` retired with ADR 0023 §19 and took `_SessionStore` (the `s-{key}` keys plus
# the `s-index` TTL list) with it: every path that could read it runs where `self.*` already
# works. `unit_state` was retired in the same breath and PUT BACK — see the amendment. Its
# store is below.


class _UnitCkpt:
    """The per-unit unit_state store: `self.unit_state` in a Method body resolves here via the
    task-local contextvar. ALL of a unit's keys live in ONE blob (a dict) at `{slot}-ckpt`, so
    clear-on-commit drops the whole unit's scratch by deleting that single key. The slot is the
    unit's commit key, so scratch is scoped exactly like the commit it belongs to and two
    Batches of one Session never share it (ADR 0023 §17)."""

    __slots__ = ("_a", "_slot")

    def __init__(self, a, slot):
        self._a, self._slot = a, f"{slot}-ckpt"

    async def get(self, key):
        blob = await self._a._get(self._slot) or {}
        return blob.get(key)

    async def set(self, key, value):
        blob = await self._a._get(self._slot) or {}
        blob[key] = value
        await self._a._set(self._slot, blob)

    async def delete(self, key):
        blob = await self._a._get(self._slot) or {}
        if key in blob:
            del blob[key]
            await self._a._set(self._slot, blob)


log = logging.getLogger("kontra.engine")

# A unit that has killed this many SCOPES is ISOLATED as a failure and skipped (ADR 0023 §21).
# Framework-internal machinery beside the commit map, not an author knob and not a failure
# budget: it bounds one pathological unit, and §14 still holds — nothing fails a Batch on the
# framework's judgement.
_MAX_UNIT_KILLS = 2
# Seconds between @actor.healthcheck beats — a visibility interval, not a behaviour knob.
_BEAT_S = 2
# Concurrent blob reads when resolving `{"$ref": …}` units on ingest. Bounded because each is
# a thread doing a synchronous S3 GET, and an unbounded gather over a 1000-unit Batch would
# open 1000 of them at once.
_INGEST_CONCURRENCY = 16
# Committed unit keys self-expire this long after the run so Redis doesn't grow unbounded
# (one key set per run_id, never otherwise deleted). Comfortably past the handler's retry
# budget (MaxAttempts x StartToClose) and a same-day recover_run. The store owns the TTL now
# (one EXPIRE per write on the actor's hash) rather than a per-key set_state_ttl.
_STATE_TTL_S = STATE_TTL_S


def batch_id(method: str, units: list, params: dict) -> str:
    """The Batch's CONTENT HASH — the first half of a committed unit's key (ADR 0023 §17).

    Stable across a retry by construction, distinct across Batches, and it survives a reopened
    scope because a hash does not know its scope died. A sequence number would restart at zero
    on exactly the recovery path v2 makes routine.

    The Method's name is part of the content: two Methods handed the same units are two
    Batches, and letting them share slots is precisely the replay §17 exists to stop. Params
    are in it for the same reason — same units, different params is a different call. What
    follows from hashing rather than counting: an identical call, made twice on purpose, IS a
    retry and replays.
    """
    # `ensure_ascii=False` IS PART OF THE CONTRACT, not a formatting preference. Left at its
    # default, `json.dumps` escapes every non-ASCII character to `\uXXXX` while Go's encoder emits
    # it raw — so the two SDKs hashed the same Batch to different keys the moment a Unit contained
    # an accent. Go had the mirror-image bug, escaping `<`, `>` and `&`, which a URL query string
    # reaches every time. Both are off now and both sides emit raw UTF-8. See the Go peer's header
    # for the measured pairs and for why a plain-ASCII Batch's key is unchanged.
    payload = json.dumps([method, units, params], sort_keys=True, default=str,
                         ensure_ascii=False, separators=(",", ":")).encode()
    return hashlib.sha256(payload).hexdigest()[:16]


def unit_slot(bid: str, i: int) -> str:
    """Where unit `i` of batch `bid` commits, and the prefix its scratch hangs off."""
    return f"{bid}-u{i}"


async def _maybe_await(v):
    return await v if inspect.isawaitable(v) else v


# ── THE WORKFLOW-STREAM PUBLISHER WAS HERE, AND IT IS GONE ─────────────────────────────────────
#
# Six functions: `_stream_for` (a `WorkflowStreamClient` for the run), `_method_topic`
# (`<actor>/<method>`), `_stream_body` (the author's fields plus the engine's routing keys),
# `_worker_label`, `_publisher_for` and the `publish` closure `kontra.stream()` wrote through.
#
# A Temporal Workflow Stream lives in the WORKFLOW'S MEMORY and dies with the workflow, so every
# record published through this was unreadable the moment the run closed — and a run that takes
# under a minute is over before a browser has loaded and signed in. The console pane that consumed
# it is gone; this is the producer.
#
# `_worker_label` WENT WITH IT AND IS NOT MISSED: it answered "which Worker is speaking" for a
# stream record, and the log records the Method now emits already carry the same string, put there
# by `logs.bind_run` for every line rather than only for the ones that called a verb.
#
# WHAT SURVIVES, BELOW: `_as_mapping`, because the healthcheck beat still flattens an author's map,
# and `_post_progress`, which is a DIFFERENT mechanism — an HTTP POST to the orchestrator's live
# beat store (`GET /api/runs/{runId}/progress`), read by the CLI, durable for the life of the
# process rather than the workflow.


def _as_mapping(value):
    """An author's record as a plain dict — dataclass, pydantic model or mapping alike.

    Not `json.dumps`: the payload converter wants a structure, and a dataclass that reached it
    unconverted would arrive as a repr string that no schema describes.
    """
    if value is None:
        return {}
    if isinstance(value, dict):
        return dict(value)
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return dataclasses.asdict(value)
    dump = getattr(value, "model_dump", None) or getattr(value, "dict", None)
    if callable(dump):
        try:
            return dict(dump())
        except Exception:  # noqa: BLE001 - fall through to __dict__
            pass
    d = getattr(value, "__dict__", None)
    if d is not None:
        return dict(d)
    if hasattr(value, "_asdict"):      # NamedTuple: its fields, not a positional tuple
        return dict(value._asdict())
    # A str, int, list or tuple has NO mapping to flatten, and returning `{}` here shipped a
    # record carrying only the framework's own `node`/`actor` — the author's value gone with
    # nothing raised and nothing logged. `await stream(f"fetched {url}")` is a natural first
    # thing to try given the verb's name. Nest it instead, which is the same rule the Go host
    # already follows for a non-map beat.
    return {"value": value}


def _post_progress(orch, run_id, node_id, actor_id, progress, total):
    """Best-effort POST of one @actor.healthcheck beat to the orchestrator's live-progress
    store (GET /api/runs/{runId}/progress). Runs off the event loop (to_thread); a failure
    is swallowed — progress is a VISIBILITY aid and must never perturb the run."""
    body = json.dumps({
        "node": node_id, "actor": actor_id, "progress": progress, "total": total,
    }).encode()
    req = urllib.request.Request(
        f"{orch}/api/runs/{run_id}/progress", data=body,
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        urllib.request.urlopen(req, timeout=2).close()
    except Exception:
        pass


def build_session_factory(registry, *, store="env"):
    """Return `(actor_id, kv=None, heartbeat=None) -> Session` for `registry`.

    A factory rather than a class so the closures over the author's functions are resolved once
    at boot instead of per activation.
    """
    load_fn, close_fn, hc_fn = registry.load_fn, registry.close_fn, registry.healthcheck_fn
    cls = registry.actor_class or KontraBase
    # Per-unit blob store (None -> inline commits, the no-S3 dev/test mode). "env" defers
    # to KONTRA_S3_*; tests inject a fake or None explicitly.
    unit_store = unitstore_from_env() if store == "env" else store

    class Session:
        """One live actor instance, keyed by actor id. The host holds at most one per id."""

        def __init__(self, actor_id: str, kv=None, heartbeat=None):
            self._actor_id = actor_id
            # Tier 1 + 2 state. Same keys, same Redis, same 24h TTL as the store this replaced —
            # ADR 0018 is a client swap here, not a state-model change.
            self._kv: ActorStateKV = kv if kv is not None else state_kv(actor_id)
            # Called once per committed unit. This is what turns HeartbeatTimeout into a real
            # liveness check: a wedged unit stops beating, rather than a keepalive ticker beating
            # on its behalf while the work is stuck.
            self._heartbeat = heartbeat
            self._inst = None
            self._opens = 0
            # Set once the resource died under this Session (§20). A Session ends exactly once
            # and never comes back: the caller opens a new scope, it does not get a new resource
            # silently welded under the old one.
            self._ended = False
            # Set by the heartbeat's probe when the resource dies with nothing raised.
            self._resource_dead = False
            # Set per batch; used for sub-unit blob keys. `_run_date` is the RUN's date as the
            # handler stamped it, NOT this worker's clock — see run_batch.
            self._run_id = self._node_id = self._run_date = ""
            self._global = None                # lazily-built GlobalStore (name-scoped, tier 3)
            self._object = None                # lazily-built GlobalStore (key-scoped, tier 4)
            self._etag_kv_shared = None        # ONE Redis client behind both of them
            self._fail_slots = {}
            # Which Method this batch is running. Resolved per batch, not at boot: an actor
            # declares many and the dispatch names one (ADR 0023 §9).
            self._method = None
            # Per-batch: the content hash every commit key hangs off, the committed outputs by
            # unit index, and how many units the batch carries (for the heartbeat).
            self._bid = ""
            self._slots: dict[int, list] = {}
            self._total = 0

        def _etag_kv(self):
            """The one ETag KV behind BOTH cross-session tiers. Opened on the first call, so an
            actor that touches neither tier opens no extra connection — and an actor that
            touches both opens ONE, not two."""
            if self._etag_kv_shared is None:
                self._etag_kv_shared = redis_kv_from_env()
            return self._etag_kv_shared

        def _global_store(self):
            """The actor's cross-session, actor-NAME-scoped global_state store, built once."""
            if self._global is None:
                self._global = GlobalStore(self._etag_kv(), registry.actor_name)
            return self._global

        def _object_store(self):
            """The actor's cross-session, actor-KEY-scoped object_state store (ADR 0022). Same
            ops as global_state under a namespace carrying the actor id, so two keys of one
            actor never read each other's state — which is the whole of virtual-object scoping.
            Un-keyed dispatches get a fresh run/node id and therefore a private, empty tier."""
            if self._object is None:
                self._object = GlobalStore(
                    self._etag_kv(),
                    prefix=object_prefix(registry.actor_name, self._actor_id),
                )
            return self._object

        async def _open(self, params: dict):
            inst = cls()
            inst.params = params
            inst.run_id = self._actor_id
            # Always 0 since ADR 0023 §20: a Session opens exactly once and a dead resource ends
            # it rather than rebuilding it. The field outlives the mechanism because its Go peer
            # (Session.Rebuilds) does; both go together.
            inst.rebuilds = self._opens
            inst.emit_durable = unit_store is not None  # records durable at emit-time? (S3 vs inline)
            _run_params.set(params)
            self._inst = inst
            self._opens += 1
            if load_fn is not None:
                try:
                    await _maybe_await(load_fn(inst))     # @actor.load
                except BaseException:
                    # A FAILED LOAD MUST LEAVE NO SESSION BEHIND. `self._inst` is assigned above
                    # so the author's own `@actor.close` can release whatever the failed load had
                    # already acquired — but if it stays assigned after the raise, the next
                    # attempt of this Batch sees `self._inst is not None`, SKIPS `_open`
                    # entirely, and runs the Method against a resource that was never loaded.
                    #
                    # Measured on `redditscrape-1787584680`: an OAuth actor whose load raised on
                    # a refused credential ran all three Units anyway, each re-minting a token
                    # per keyword — 202 auth requests from one 3-Unit Batch — and returned
                    # `{done: true, results: [], failures: []}`. A green Run, no output, and a
                    # request storm against the credential's own rate limit.
                    #
                    # Close first (it reads `self._inst`), THEN clear, so the next attempt opens
                    # a genuinely new instance or fails again loudly.
                    #
                    # COUNTED BEFORE THE CLEANUP, because the fact is already true here and
                    # `_close` runs an author's `@actor.close`, which is arbitrary code that may
                    # take as long as it likes. A counter incremented after it would lose the
                    # failure of every load whose close hangs — which is not a rare case, it is
                    # the shape of a load that failed because the resource is unreachable.
                    metrics.count_load(ok=False)
                    await self._close()
                    self._inst = None
                    raise
            # A load that returned. `count_load` moves the denominator on both paths, so a
            # `@actor.load`-less actor counts nothing at all and the Warden reports "cannot
            # tell" rather than a ratio over zero attempts.
            metrics.count_load(ok=True)
            log.info("[%s] loaded resource (open #%d)", self._actor_id, self._opens)

        async def _close(self):
            if close_fn is not None and self._inst is not None:
                try:
                    await _maybe_await(close_fn(self._inst))   # @actor.close
                except Exception:
                    pass

        async def _on_deactivate(self):
            await self._close()                           # backstop; primary close is handler-driven

        async def _end(self):
            """End this Session for good (ADR 0023 §20). Runs the author's close so the dead
            resource's handles go, then marks the scope over — `_ended` is what makes the
            promise have no third case: `self.*` survives, or the next call raises."""
            await self._close()
            self._inst = None
            self._ended = True

        # Tier 1 + 2 state.
        async def _get(self, k, default=None):
            return await self._kv.get(k, default)

        async def _set(self, k, v):
            await self._kv.set(k, v)                      # each commit lands -> survives host death

        async def _renew_session_ttls(self):
            """Slide the whole session's TTL forward so an active-but-idle actor's durable state
            never expires under it. ONE call now: the state is a single hash and EXPIRE applies
            to all of it. The store this replaced had no TTL touch, so it had to re-write every
            indexed key here. Best-effort — a store hiccup never fails a run, because the
            write-time TTL is the backstop."""
            try:
                await self._kv.touch()
            except Exception:
                pass

        def _slot(self, i):
            """Unit i's commit key in THIS batch (ADR 0023 §17), and the prefix its scratch
            hangs off."""
            return unit_slot(self._bid, i)

        async def _clear_ckpt(self, slot):
            # A committed unit never re-runs, so its resume scratch is dead weight — drop it
            # now; the state TTL is the backstop if this fails.
            try:
                await self._kv.delete(f"{slot}-ckpt")
            except Exception:
                pass

        async def _fail(self, unit, e, category):
            # Isolate one unit as a durable failure. The record matches the wire contract
            # (PerUnitFailure {unit, error:{type,message}, category}); the SAME structured
            # error is stored in the commit so the skip-replay path re-emits it verbatim.
            err = {"type": type(e).__name__, "message": str(e)}
            slot = self._slot(unit.index)
            await self._set(slot, {"out": [], "error": err, "category": category})
            await self._clear_ckpt(slot)
            metrics.count_isolated(category)  # the run just lost this unit; make it observable
            self._fail_slots[unit.index] = {"unit": unit.raw, "error": err,
                                            "category": category}
            self._beat(len(self._slots), self._total)

        async def _end_or_isolate(self, unit, e):
            """A unit signalled resource death — it raised SessionLost, or it errored and
            `@actor.healthcheck` reports the resource gone. The Session ENDS (ADR 0023 §20):
            reloading in place is what silently reset `self.*` under a running author, which is
            the failure §7 already rejected for host loss.

            Unless this unit has done it before. A DURABLE per-unit counter — the poison counter
            of §21, keyed off the unit's commit slot so it survives the scope that died — records
            how many scopes this unit has killed; at _MAX_UNIT_KILLS it is isolated as a failure
            and skipped instead. Without that, §20 and §17 compose into a tight loop: end the
            scope, reopen, resume at the poison unit, die again.
            """
            key = f"{self._slot(unit.index)}-kills"
            n = int(await self._get(key, 0) or 0) + 1
            if n >= _MAX_UNIT_KILLS:
                await self._fail(unit, e, "exhausted")
                return
            await self._set(key, n)
            raise SessionLost(str(e)) from e

        async def _probe(self):
            """(dead, progress) — from TWO hooks now, with opposite failure semantics.

            `@actor.healthcheck` decides LIVENESS: a raise or `False` ends the Session.
            `@actor.progress` describes the WORK: a raise is swallowed, because reporting where
            you are must never kill a Session that is working fine.

            They used to be one function, and the shipped actors show what that cost. `webcrawl`'s
            probe is written to answer "reload or isolate?", so it returned `{"contexts": 2}` —
            two browser tabs — and that was the entire progress signal an operator got for a
            454-program campaign.

            A healthcheck that still returns a value keeps working: its map is merged UNDER the
            progress hook's, so an actor that has not been split yet loses nothing, and one that
            has cannot have its `at` overwritten by a stale liveness field.
            """
            if hc_fn is None:
                return (False, None)
            try:
                res = await _maybe_await(hc_fn(self._inst))
            except Exception:
                return (True, None)
            if res is False:
                return (True, None)
            return (False, None if isinstance(res, bool) else res)

        async def close(self) -> dict:
            """@actor.close, invoked when the caller's scope exits (host-agnostic: the Go host
            does the same). A closed Session is over for the same reason a dead one is — it
            ends once and does not come back, and the next scope is a new Session."""
            await self._close()
            self._inst = None
            self._ended = True
            return {"closed": True}

        async def _resolve_refs(self, todo):
            """Turn `{"$ref": …}` entries in `todo` into the records they address.

            This is what makes "a Method takes a Batch and returns a Batch" real: a Method's
            output is a list of refs into the hive blob plane, and handing that list to the
            next Method would otherwise show its author a ref dict as `unit.value`. The ACTOR
            resolves, not the caller — it already holds the S3 credentials because it wrote
            these blobs, while the caller deliberately holds none.

            Non-ref entries pass through untouched, so this is a no-op for inline records, the
            no-Method passthrough, and Dataset pages of plain values.
            """
            hits = [(pos, ref) for pos, (_i, u) in enumerate(todo)
                    if (ref := unit_ref(u)) is not None]
            if not hits:
                return todo
            if unit_store is None:
                # Loud, and named. Handing the author a ref dict would look like the actor's
                # own bug, and silently passing it through is how a whole batch produces
                # plausible nonsense.
                raise NonRetryableError(
                    f"{len(hits)} unit(s) arrived as blob refs but this actor has no object "
                    "store configured (KONTRA_S3_ENDPOINT unset) — it cannot read them")

            sem = asyncio.Semaphore(_INGEST_CONCURRENCY)

            async def one(pos, ref):
                async with sem:
                    rec = await asyncio.to_thread(unit_store.get_subunit, ref["key"])
                return pos, rec

            resolved = dict(await asyncio.gather(*(one(p, r) for p, r in hits)))
            return [(i, resolved.get(pos, u)) for pos, (i, u) in enumerate(todo)]

        # ---- the Batch sink (actorkit/batch.py calls these three) ----------------------

        def enter(self, unit):
            """A Unit was handed to the author. Bind its resume scratch task-locally, so
            `self.unit_state` in the body resolves to THIS unit's slot."""
            _ckpt_io.set(_UnitCkpt(self, self._slot(unit.index)))

        async def record(self, unit, rec):
            """One `await dataset.push(x)` (ADR 0028): make the record durable NOW, attributed to
            the Unit the iterator is on, and return what stands in for it in that Unit's commit.
            With an object store configured that is a blob keyed by content sha under the unit's
            prefix (units/{run}/{node}/u{i}/{sha}.json), so a downstream cursor sees records
            before the unit finishes and a re-run overwrites idempotently. Without one the record
            rides inline and is durable when the unit commits — which is what `self.emit_durable`
            tells the author."""
            if unit_store is None:
                return rec
            return await asyncio.to_thread(
                unit_store.put_subunit, self._run_id, self._node_id, unit.index, rec,
                self._run_date)

        async def commit(self, unit):
            """The Unit is finished: write its durable done-marker, drop its scratch, and beat.
            This is the marker a retry reads to skip it, so nothing after this line may re-run
            the unit."""
            slot = self._slot(unit.index)
            await self._set(slot, {"out": unit.out})
            await self._clear_ckpt(slot)
            self._slots[unit.index] = unit.out
            self._beat(len(self._slots), self._total)

        # ---- the author's loop -----------------------------------------------------------

        async def _classify(self, unit, e):
            """Record one raise against the Unit the iterator was on (ADR 0023 §13)."""
            if isinstance(e, SessionLost):                 # the author says the resource is gone
                await self._end_or_isolate(unit, e)
            elif isinstance(e, NonRetryableError):         # terminal: isolate without probing
                await self._fail(unit, e, "terminal")
            elif (await self._probe())[0]:                 # dead resource -> the Session ends
                await self._end_or_isolate(unit, e)
            else:                                          # live resource -> isolate the unit
                await self._fail(unit, e, "exhausted")

        async def _run_method(self, batch, dataset):
            """Hand the whole Batch and its output Dataset to the author's Method (ADR 0023 §2,
            ADR 0028 §2). The author loops and pushes to `dataset`.

            A raise is attributed to the Unit the iterator was on, recorded, and the Method
            RE-INVOKED with the remainder — cheap and equivalent, because everything pushed
            before the raise is already committed. So a Method is entered several times per
            Batch: author locals reset between entries, `self.*` does not.

            A raise with no Unit to blame (the author held the Batch to run it concurrently)
            propagates: there is no position to attribute by, and guessing one would pin the
            failure on whichever Unit happened to be pulled last.

            A push that failed to persist is systemic, not one bad Unit (ADR 0028 §3): it fails
            the whole call rather than isolating the Unit it struck, because isolating would
            commit that Unit empty and a retry would skip it — losing the record for good.
            """
            fn = self._method.fn
            while True:
                # A push made with no current Unit rides the Batch tail, keyed by the author's
                # explicit key (ADR 0028). The tail is RETAINED across the isolation re-invokes
                # below — not rebuilt per entry — and reconciled first-write-wins by that key: a
                # re-push of a key already written is dropped before its durable write, so a
                # self-guarded push kept from entry 1 survives, a content-varying one keeps its
                # first value, and a push that appears only on the re-invoke carries a fresh key and
                # is kept. Identity is TOLD by the key, never inferred from control-flow position —
                # the three position/content inference attempts each lost or duplicated a push.
                try:
                    await fn(self._inst, batch, dataset)
                except Exception as e:
                    if batch.push_error is not None:
                        raise batch.push_error
                    unit = await batch.blame()
                    if unit is None:
                        raise
                    await self._classify(unit, e)
                else:
                    await batch.settle()
                    return
                if not batch.pending:
                    return

        async def run_batch(self, payload: dict) -> dict:
            if self._ended:
                # The resource died under this Session (ADR 0023 §20). Answering the call would
                # mean loading a second resource inside a scope whose `self.*` is gone — the
                # silent reset the rule exists to delete.
                #
                # BEFORE count_batch: a refused call did not run a Batch, and counting it would
                # inflate the denominator of the sick-worker ratio with the very calls a sick
                # worker refuses.
                raise SessionLost(
                    f"session {self._actor_id} ended when its resource died; open a new scope")
            # The DENOMINATOR of the sick-worker signature. `count_reload` is meaningless without
            # it: reloads alone cannot distinguish a worker that reloaded twice in a thousand
            # batches from one that reloaded twice in two. It was defined and never called, so the
            # fleet dashboard's ratio had a permanently-zero denominator on every host.
            metrics.count_batch()
            units = payload.get("units", []) or []
            params = payload.get("params", {}) or {}
            # Run lineage for the per-record blob keys (units/{run}/{node}/u{i}/{sha}); the
            # handler stamps both from EntryInput. Absent on old handlers -> keyed under run/node.
            run_id = payload.get("run_id", "") or ""
            node_id = payload.get("node_id", "") or ""
            # The RUN's date (YYYY-MM-DD), stamped by the handler from the workflow's start time
            # so every worker on one run agrees on the `dt=` partition. Dropping it — as this did
            # until 2026-08-14 — silently keys blobs by the WORKER's clock, which splits a run
            # across two partitions at midnight and reads as data loss. Go has always sent and
            # honoured it (runtime/handler/workflow.go:104, runtime/go/engine/engine.go:148).
            run_date = payload.get("run_date", "") or ""
            # Named by the dispatch, or the sole Method when the actor has only one. Resolving
            # here (not at boot) is what lets one loaded session serve several Methods.
            self._method = registry.resolve_method(str(payload.get("method") or ""))
            # for the per-record blob keys
            self._run_id, self._node_id, self._run_date = run_id, node_id, run_date
            # AND ONTO EVERY LOG LINE THIS BATCH WRITES (ADR 0050 §1, kontra#16). The identity is
            # already here — it is the same lineage the blob keys hang off — so correlating a line to
            # a Run is a bind, not a new plumbing path. A CONTEXTVAR because one host runs many
            # Sessions on one loop: a module global would label every line with whichever Unit bound
            # last, which is the lie one layer up that `vmagent.env` exists to prevent.
            #
            # Absent values are dropped by `bind_run`, so a Worker polling with no Run writes records
            # WITHOUT the label rather than records with an empty one.
            #
            # `actor` IS THE NAME AND `actor_id` IS THE SESSION, WHICH IS NOT WHAT THIS SENT.
            # It bound `actor=self._actor_id` — a session id like `c5eaf2b6a275` — into the field
            # the console's logs rail renders and filters as the actor NAME
            # (`core/src/run/logs.ts`, whose fixture reads `actor: 'desync'`). Two consequences,
            # and the second is worse than the first: a reader filtering `actor:desync` matched
            # nothing, and `actor` is one of the four STREAM fields the shipper declares — so a
            # session id there mints a new log stream per Session, which is unbounded cardinality
            # in the index for a value nobody groups by.
            #
            # This is the same confusion the streaming work fixed one layer over, in the other
            # direction: there the topic carried the name and the record had to carry the session,
            # here the label carries the name and the session needed its own key.
            logs.bind_run(
                run_id=run_id,
                node_id=node_id,
                actor=os.environ.get("KONTRA_ACTOR_NAME", ""),
                actor_id=self._actor_id,
                actor_version=os.environ.get("KONTRA_ACTOR_VERSION", ""),
            )
            # Every commit key of this batch hangs off its content hash. The RESOLVED name goes
            # in, not the wire field: a sole Method dispatched once by name and once without is
            # one Method, and must hash to one Batch.
            self._bid = batch_id(self._method.name if self._method else "", units, params)
            _run_params.set(params)
            if self._inst is None:
                # The FIRST call of this Session, and the only one that can open a resource: a
                # Session that lost its resource is ended, not re-opened (§20, guarded above).
                await self._open(params)
            else:
                self._inst.params = params

            # Slide the TTL of the actor's live state forward.
            await self._renew_session_ttls()
            # Bind the two cross-session tiers (the shared Redis client opens lazily on first
            # use of either): global_state by actor NAME, object_state by actor KEY.
            _global_io.set(self._global_store())
            _object_io.set(self._object_store())

            # A new OWNER (run/node) means a new session on this actor id, so its per-session
            # state goes. The commit map no longer needs this guard — §17's content hash already
            # keeps two batches out of each other's slots, which is what makes a SECOND Method
            # call under one owner safe — but a previous session's scratch is scoped to that
            # session and would otherwise read as this batch's own.
            #
            # Cleaning up in close() would be simpler and is not enough — the handler's Close is
            # best-effort (bounded retry, error discarded in runtime/handler/workflow.go), so a batch
            # whose close never landed would poison the next one. A guard here cannot be skipped.
            #
            # Tiers 3/4 live in another store and are untouched — object_state surviving this is
            # the whole point of keying.
            owner = f"{run_id}/{node_id}"
            prev_owner = await self._get("batch-owner")
            if prev_owner is not None and prev_owner != owner:
                log.info("[%s] batch %s supersedes %s: clearing per-session state",
                         self._actor_id, owner, prev_owner)
                await self._kv.drop()
            if prev_owner != owner:
                await self._set("batch-owner", owner)  # after the drop, which clears the hash

            # Input-order assembly (identity-keyed): slots[i] holds unit i's outputs,
            # _fail_slots[i] its failure record — never completion order.
            slots = self._slots = {}
            self._fail_slots = {}
            self._total = len(units)

            # Replay commits from a PRIOR ATTEMPT of this batch; what's left is this attempt's
            # work. The commit map is why a retry after a mid-batch death is not a re-run, and
            # keying it by the batch's content hash is why a SECOND batch under this owner
            # cannot read the first's slots (ADR 0023 §17).
            todo = []
            for i, unit in enumerate(units):
                prev = await self._get(self._slot(i))
                if prev is None:
                    todo.append((i, unit))
                elif prev.get("error"):
                    self._fail_slots[i] = {"unit": unit, "error": prev["error"],
                                           "category": prev.get("category", "exhausted")}
                else:
                    slots[i] = prev["out"]

            # Resolve `{"$ref": …}` units into the records they address, so one Method can be
            # handed another's output without the payload ever passing through the caller
            # (ADR 0007). Only `todo` is resolved: a unit already committed is never re-run, so
            # fetching its input would be a pure cost.
            #
            # Everything that is not ref-shaped passes through untouched, which is what makes
            # this uniform across the three input shapes — inline records (no store
            # configured), the no-Method passthrough, and a Dataset page of plain values.
            if todo:
                todo = await self._resolve_refs(todo)

            self._resource_dead = False
            # The output Dataset the Method pushes to (ADR 0028 §2). Bound to the Batch so a push
            # commits against the Unit the iterator is on; the caller does not yet name it (that is
            # slice 07), so it is the unnamed, chainable kind. `tail` collects pushes made with no
            # current Unit and is folded into results below.
            batch = Batch(self, todo, getattr(self._method, "takes", None))
            beat = asyncio.create_task(self._progress_beat(run_id, node_id, len(units)))
            # A PER-BATCH WORKFLOW-STREAM PUBLISHER WAS BOUND HERE, and `kontra.stream()` wrote
            # through it. Both are gone: the stream died with the workflow, so nothing could read
            # it a minute after the run. A Method narrates with its own logger, which
            # `logs.bind_run` has already stamped with the run, the Worker and the Temporal
            # context — so the same facts reach the run page's rail and stay there.
            try:
                if self._method is None:
                    # No @actor.method (load-only actor): identity passthrough, one turn.
                    for i, unit in todo:
                        slots[i] = [unit]
                else:
                    await self._run_method(batch, Dataset(batch))
                if self._resource_dead:
                    # The beat saw the resource die. Whatever committed stays committed; the
                    # Session still ends, because the alternative is returning a Batch that
                    # looks finished and was run against a corpse.
                    raise SessionLost("@actor.healthcheck reported the resource dead")
            except SessionLost:
                # The resource is gone, so the SESSION is over (ADR 0023 §20). Tear it down here
                # and remember: the caller's scope raises, and it reopens if it wants to — which
                # resumes from the commit map, because a content hash does not know its scope
                # died. What must not happen is a quiet reopen under the author's feet, so the
                # ended Session refuses every later call rather than rebuilding `self.*`.
                log.warning("[%s] @actor.healthcheck: resource dead -> the Session ends",
                            self._actor_id)
                # Counted on the same EVENT as Go's countReload(), which is "the resource was
                # declared dead" — that event survived §20 even though what follows it changed
                # from a reload to the end of the Session. Dropping the call would re-open the
                # hole this metric was added to close: a python actor reporting zero however sick
                # it got, against a denominator that now finally exists.
                metrics.count_reload()
                await self._end()
                raise
            finally:
                beat.cancel()

            done = len(slots) + len(self._fail_slots) == len(units)
            # Per-Unit outputs in input order, then the tail: records pushed with no current Unit
            # (before/after the loop, or a spawned task under `batch.units`) belong to no Unit's
            # commit, so they append after the assembled Units, in push order (ADR 0028 §1).
            tail = batch.tail if self._method is not None else []
            return {
                "done": done,
                "results": [x for i in sorted(slots) for x in slots[i]] + list(tail),
                "failures": [self._fail_slots[i] for i in sorted(self._fail_slots)],
                "opens": self._opens,
            }

        def _beat(self, committed, total):
            """Tell the runtime we are alive AND how far along. Best-effort: a heartbeat that
            raises must not fail a unit that already committed.

            THE FIELD NAMES ARE A CONTRACT, not a local choice. The orchestrator decodes this
            payload off the pending activity (`control/orchestrator/src/heartbeat.ts`, HeartbeatDetail)
            and it is what `kontra monitor` renders as live per-node progress. Every field there
            is optional and defaults to 0, so a wrong name does not error — it reports 0/0
            forever, which reads exactly like a node that has done nothing.

            `isolated` rides along for the same reason it exists at all: a node that dropped
            every unit and a node that legitimately found nothing otherwise render identically.
            """
            if self._heartbeat is None:
                return
            try:
                self._heartbeat({
                    "node": self._node_id,
                    "done": committed,
                    "total": total,
                    "isolated": len(self._fail_slots),
                    # THE CHECKPOINT RIDES ALONG, AND IT IS THE DURABLE HALF.
                    #
                    # The four counters above are a progress display: they say how many, never
                    # WHICH, so nothing can resume from them. The checkpoint says which, in
                    # `shared/conformance/checkpoint.json`'s canonical encoding, and heartbeat
                    # details live in the activity's own history — so this is the one copy of the
                    # commit map that a cache cannot lose (ADR 0059).
                    #
                    # `batch_id` is the batch's CONTENT HASH, which is what makes the checkpoint
                    # safe to act on: a stale one from another batch is discarded by the reader
                    # rather than applied by index to units it never saw.
                    #
                    # IT DOES NOT YET REPLACE THE REDIS COMMIT MAP, and the reason is `commit`:
                    # that map holds each Unit's OUTPUT, not merely a done-marker, and an output
                    # does not fit in a heartbeat payload. Replacing it needs the outputs addressed
                    # by a manifest in the unit store first — which is what `manifest_ref` is for,
                    # and why it is in the contract before it is in use.
                    "checkpoint": self._checkpoint().to_details(),
                })
            except Exception:
                pass

        def _checkpoint(self):
            """This batch's progress, in the cross-SDK encoding.

            Built from `self._slots` and `self._fail_slots` rather than maintained alongside them:
            two structures tracking one fact drift, and the drift would be a checkpoint that
            disagrees with the commits it describes.
            """
            ck = _Checkpoint(batch_id=self._bid)
            for i in self._slots:
                ck.commit(i)
            for i in self._fail_slots:
                ck.isolate(i)
            return ck

        async def _progress_beat(self, run_id="", node_id="", total=0):
            # Beat @actor.healthcheck output to the orchestrator so the CLI/UI can stream live
            # progress. Best-effort visibility — a POST failure NEVER touches the run.
            orch = os.environ.get("KONTRA_ORCHESTRATOR_URL", "").rstrip("/")
            # THE BEAT ALSO WENT ONTO THE RUN'S WORKFLOW STREAM, and that half is gone. What is
            # left is `_post_progress` — an HTTP POST to the orchestrator's live beat store, which
            # outlives the workflow and which `GET /api/runs/{runId}/progress` and the CLI read.
            try:
                while True:
                    await asyncio.sleep(_BEAT_S)
                    dead, progress = await self._probe()
                    if dead:
                        # The resource died with nothing raised — the author's loop may never
                        # notice. Record it and stop beating; run_batch ends the Session when the
                        # Method returns (ADR 0023 §20). Ending it from here would mean
                        # cancelling the author mid-push, which costs more than the units the
                        # Batch has left.
                        self._resource_dead = True
                        return
                    if progress is not None:
                        log.info("[%s] progress: %s", self._actor_id, progress)
                        if orch and run_id:
                            await asyncio.to_thread(
                                _post_progress, orch, run_id, node_id, self._actor_id, progress, total)
            except asyncio.CancelledError:
                pass
            finally:
                pass

    return Session
