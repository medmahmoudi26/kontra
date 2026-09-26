"""The Batch an author loops over, and the Dataset a Method pushes to (ADR 0023 §2, §3; ADR 0028).

    @actor.method(takes=Target, emits=Page)
    async def crawl(self, batch, dataset):
        async for unit in batch:
            await dataset.push(fetch(unit.value))

Two save points are tracked here and ADR 0028 §1 is the reason they are kept apart:

  INPUT POSITION says a Unit is FINISHED. The iterator is framework-owned, so asking for the next
  Unit is what tells the framework the last one is done and can be committed. An author who takes
  the whole Batch instead (`batch.units`, to run the Units concurrently) has no position to commit
  by, so nothing commits until the Method returns — the honest cost of owning the concurrency, and
  the only thing it costs.

  OUTPUT is a Dataset, not a Unit. `await dataset.push(x)` names no Unit; provenance the author
  cares about goes INSIDE the record (ADR 0028 §1). The framework needs the record→Unit link only
  for resume ("everything pushed so far is durable, and I am past unit 7"), which the input cursor
  already answers — so the link is gone from the author's surface.

A push made INSIDE the loop is committed against the Unit the iterator is currently handing out:
`batch.current`. That is byte-for-byte what the retired per-**Unit** emit did, only the author no
longer spells the Unit, and the per-Unit commit map (ADR 0023 §17) makes it exactly-once with no
key and no new burden — the overwhelmingly common path, untouched.

AN OUT-OF-LOOP PUSH IS IDENTIFIED BY AN EXPLICIT KEY, NOT BY ITS POSITION. A push made while no
Unit is current — before the loop, after it drains, or from a task spawned under `batch.units` —
belongs to no Unit's commit, so it rides the Batch TAIL and folds into the call's results at Method
exit. The engine re-invokes the body from the top with the remaining Units after isolating one (ADR
0023 §13), and `self.*` survives that in-memory re-invoke while author locals reset — so the same
out-of-loop push RE-RUNS, with the same or different bytes, and the framework must reconcile the
re-runs. It CANNOT infer whether the author "re-pushed a changed version of the same record" or
"deliberately pushed a different record": both are the same control-flow position with different
bytes. Three mechanisms that inferred identity from content or position each traded one failure for
another (truncate-and-re-execute LOST a self-guarded push; accumulate-and-content-dedup DUPLICATED
a content-varying push; (group, ordinal) first-wins DUPLICATED *and* LOST on a prefix shift). So
identity is TOLD, the principle ADR 0023 §18 settled for this class of problem (Restate's
`ctx.run(key, …)`, Lambda's `batchItemFailures` by `itemIdentifier`): name the durable thing.

    await dataset.push(summary, key="batch-summary")

Each keyed tail push is reconciled BY THAT KEY, FIRST-WRITE-WINS across re-invokes: a key already
written in an earlier entry is SKIPPED BEFORE the durable write, so results and the store agree with
no orphan blob to clean; a key new to this call is written and folded. Position stops mattering —
a push that appears only on the re-invoke (a `warn` under `if self._bad:`) carries a key nobody has
written yet and is kept, while the `data` push that precedes it carries a key already written and is
skipped, both surviving exactly once regardless of the prefix shift. A self-guarded push that ran
only on entry 1 keeps its slot; a content-varying push keeps its first value.

AN OUT-OF-LOOP PUSH WITH NO KEY RAISES, at the call site, naming the fix. There is no fourth guess:
a generated key would be inferred from position again and wrong the same way. The raise converts
today's silent duplication/loss into a loud error the author hits the first time they run the
Method. A within-entry duplicate (two concurrent tasks under `batch.units` pushing under distinct
keys the author chose) is kept — the author named two things. A genuine host death reloads a fresh
instance (`self.*` gone, `_tail_slots` empty) and re-executes from scratch; a keyed push lands on
the same blob key (a pure function of its key, ADR 0015), so a content-deterministic re-push is an
idempotent overwrite, and the per-Unit commit map skips the Units already finished (§17).

Pure sequencing, no host import: the durable half is three calls on the sink the host passes
in — `enter(unit)` when a Unit is handed out, `record(unit, x)` per push, `commit(unit)` when a
Unit is finished.
"""

from __future__ import annotations

import hashlib
from typing import Any, Dict, List, Optional

# Distinguishes "not yet coerced" from a legitimately None payload.
_UNSET = object()

# Blob-shard base for a tail record (one pushed with no current Unit). Kept far above any real
# Unit index so the two never share a shard prefix in the blob layout — output is offset-addressed,
# not Unit-addressed (ADR 0028 §1).
_TAIL_BASE = 1_000_000
# The synthetic-index span a tail key hashes into, above _TAIL_BASE. Wide enough that two distinct
# keys colliding on one synthetic index is negligible; even then their records differ in content
# sha and so land on different blobs.
_TAIL_SPAN = 1_000_000_000


class MissingPushKey(Exception):
    """Raised by `dataset.push(record)` when there is no current Unit and no explicit key. An
    out-of-loop push belongs to no Unit's commit, so the framework reconciles it across an
    isolation re-invoke by a key the author supplies (ADR 0028) — it cannot infer identity from
    control-flow position. Loud at the call site the first time the Method runs, in place of the
    silent duplication/loss the three inference attempts each produced."""


class BatchAlreadyTaken(Exception):
    """Raised when a Method empties its Batch with `take_all()` (or the `units` alias) and then
    iterates it with `async for`. Both are supported ways to consume a Batch; doing both runs the
    loop body ZERO times and reports success, which is the silent failure GitHub #23 records —
    `20/20 unit(s)`, COMPLETED in 17s where the work takes 80, and an empty dataset as the only
    symptom. An author error, named where it is made, like {@link MissingPushKey}."""


def _tail_index(key: str) -> int:
    """The synthetic Unit index a tail record commits under — a pure function of its KEY, so the
    SAME out-of-loop push lands on the SAME blob key across an isolation re-invoke or a
    genuine-death retry. That is what makes a content-deterministic re-push an idempotent overwrite
    instead of an orphan (ADR 0028 §1, ADR 0015). Distinct keys land on distinct indices, so two
    keys carrying identical bytes never collide onto one blob."""
    h = int.from_bytes(hashlib.sha256(key.encode()).digest()[:6], "big")
    return _TAIL_BASE + h % _TAIL_SPAN


class _TailUnit:
    """A stand-in the tail hands the sink so a push with no current Unit is still made durable NOW
    (a concurrent crawler streams as it goes). It carries only an offset-derived `index` — the
    sink keys the blob off it and touches nothing else."""

    __slots__ = ("index",)

    def __init__(self, index: int) -> None:
        self.index = index


class Unit:
    """One indivisible piece of work — the grain of retry and of commit (ADR 0023)."""

    __slots__ = ("index", "_out", "_raw", "_takes", "_typed")

    def __init__(self, index: int, value: Any, takes: Any = None) -> None:
        self.index = index      # position in the Batch; the index half of the commit key
        self._out: List[Any] = []
        self._raw = value       # the payload as it arrived, plain JSON
        self._takes = takes     # the Method's declared input type, or None
        self._typed = _UNSET

    @property
    def value(self) -> Any:
        """The author's payload — an instance of the Method's declared `takes` type when it
        declared one, and the plain JSON value when it did not.

        `@actor.method(takes=Target)` is what turns `unit.value["host"]` into `unit.value.host`,
        which is the difference between an actor that reads like code and one that reads like a
        dict lookup. Undeclared Methods are unaffected and keep the dict.

        COERCED HERE, LAZILY, and that placement is the design: the author touches `unit.value`
        inside the loop, so a payload that does not fit the declared type raises at the iterator
        boundary and isolates THAT Unit (ADR 0023 §13). Coercing the whole Batch up front would
        fail every Unit because one record was malformed.
        """
        if self._takes is None:
            return self._raw
        if self._typed is _UNSET:
            from kontra.schema import coerce

            self._typed = coerce(self._takes, self._raw)
        return self._typed

    @property
    def raw(self) -> Any:
        """The payload exactly as it arrived, before any coercion.

        This is what a FAILURE record carries, and it has to be: a Unit that failed *because* it
        did not fit the declared type has no coerced form, so recording `value` there would
        re-raise the very error being recorded. It is also the more useful answer — a caller
        re-dispatching a drop needs the wire payload, not a Python object it cannot send.
        """
        return self._raw

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        return f"<Unit {self.index} {self._raw!r}>"

    @property
    def out(self) -> List[Any]:
        """What was pushed while this Unit was current, as committed (refs, or the records
        themselves when no object store is configured). Framework-owned: the host reads it to
        write the commit. The author never names this Unit — the framework attributes a push to
        whichever Unit the iterator is handing out (ADR 0028 §1)."""
        return self._out


class Dataset:
    """Where a Method pushes its output (ADR 0028 §2). The caller's third parameter; from inside
    a Method the destination is indistinguishable whether the caller named it or not, which is the
    asymmetry ADR 0028 exists to remove.

        async def ask(self, batch, dataset):
            async for unit in batch:
                await dataset.push(Verdict(...))            # in-loop: keyed by the current Unit
            await dataset.push(summary, key="batch-summary")  # out-of-loop: needs an explicit key

    `push` names nothing and RETURNS nothing: a write failure surfaces at the next checkpoint or
    at Method exit, the Kafka-producer model where you check the flush and not every send (ADR
    0028 §3). Pass an instance of the Method's declared `emits` type or a plain dict — both reduce
    to JSON here. A push made with no current Unit REQUIRES `key=` and raises `MissingPushKey`
    without one (ADR 0028): the framework reconciles it by that key across an isolation re-invoke,
    never by its position.

    Constructed with no backend it is a plain COLLECTOR, which is the whole testability argument
    (ADR 0028 §rejected/bare-callable): a Method body is unit-tested by handing it a `Dataset()`
    and reading `.records`, with no host, no queue and no object store. The framework hands the
    real one, backed by the Batch, so the same body streams to the lake in production.
    """

    __slots__ = ("_batch", "_collected")

    def __init__(self, batch: "Optional[Batch]" = None) -> None:
        # None -> a plain collector (the unit-test path): records land in `.records`. The
        # framework passes the Batch, which routes each push to the Unit the iterator is on.
        self._batch = batch
        self._collected: List[Any] = []

    async def push(self, record: Any, key: Optional[str] = None) -> None:
        from kontra.schema import to_jsonable

        rec = to_jsonable(record)
        if self._batch is None:
            # A collector has no loop and no re-invoke, so key carries no meaning here — the
            # record is simply collected in push order for the unit-test read side.
            self._collected.append(rec)
            return
        await self._batch._push(rec, key)

    @property
    def records(self) -> List[Any]:
        """The records pushed to a collector Dataset, in push order — the unit-test read side."""
        return list(self._collected)

    def __len__(self) -> int:
        return len(self._collected)

    def __iter__(self):
        return iter(self._collected)


class Batch:
    """The Units handed to one Method call, as the iterator the author loops over."""

    __slots__ = (
        "_sink", "_pending", "_held", "current", "_takes", "_tail_slots", "_push_err", "_taken_all",
    )

    def __init__(self, sink: Any, items, takes: Any = None) -> None:
        self._sink = sink
        # The Method's declared input type, handed to every Unit this Batch hands out.
        self._takes = takes
        # (index, value) pairs not yet handed out, popped from the end -> input order.
        self._pending = list(reversed(list(items)))
        self._held: dict[int, Unit] = {}   # handed out, not yet committed
        self.current: Optional[Unit] = None
        # Records pushed while no Unit was current (before/after the loop, or a spawned task under
        # `batch.units`). They belong to no Unit's commit (ADR 0023 §17), so they ride the Batch and
        # fold into results at Method exit — made durable at push time like any other record. Keyed
        # by the AUTHOR'S explicit key, FIRST-WRITE-WINS across an isolation re-invoke (where `self.*`
        # persists): a re-push finds its key filled and is dropped BEFORE the write, so a self-guarded
        # push that ran only on entry 1 survives, a content-varying one keeps its first value with no
        # orphan blob, and a push that appears only on the re-invoke carries a fresh key and is kept.
        # Insertion order is fold order. NOT reset across entries — that retention is the mechanism.
        self._tail_slots: Dict[str, Any] = {}
        # The first push that failed to persist. Held, not raised, so `push` stays fire-and-forget
        # (ADR 0028 §3); surfaced at the next checkpoint (__anext__) and at Method exit (settle),
        # before the Unit it struck can commit — else a retry would skip that Unit and its record
        # would be lost for good (ADR 0028 §consequence 5). A MissingPushKey is NOT held here: it is
        # an author error, raised at the call site, not a store failure to surface later.
        self._push_err: Optional[BaseException] = None
        # Set by `take_all()` (and its `units` alias). Its ONLY job is to let `__anext__` tell
        # "this Batch is finished" from "somebody emptied this Batch and is now looping over it" —
        # two states that are identical in `_pending` and mean opposite things. See `__anext__`.
        self._taken_all = False

    @property
    def pending(self) -> int:
        """Units not yet handed to the author — the remainder a re-invoked Method receives."""
        return len(self._pending)

    @property
    def tail(self) -> List[Any]:
        """Records pushed with no current Unit, as committed (refs, or the records themselves in the
        no-store mode) — folded into the call's results after the per-Unit outputs, in first-write
        order (the order each key was first written across the whole call). Framework-owned."""
        return list(self._tail_slots.values())

    @property
    def push_error(self) -> Optional[BaseException]:
        """The first failed push, or None. The engine re-raises it as a whole-call failure: a
        broken store is systemic, not one bad Unit, and isolating a Unit whose write failed would
        commit it empty and lose the record on retry."""
        return self._push_err

    @property
    def units(self) -> List[Unit]:
        """DEPRECATED SPELLING of {@link take_all}. It reads like a length and it EMPTIES the Batch.

        Kept because `webcrawl` and others call it, and because breaking a working Method to fix a
        naming mistake is the wrong trade. New code should say `take_all()`, which cannot be
        mistaken for a property: `len(batch.take_all())` looks like the destructive thing it is,
        and `len(batch.units)` looks like a question.

        Reading this and then writing `async for unit in batch` is now an ERROR rather than a silent
        empty loop — see {@link take_all} and `__anext__`.
        """
        return self.take_all()

    def take_all(self) -> List[Unit]:
        """TAKE every remaining Unit at once, for an author who runs them concurrently themselves.

        THE NAME IS A VERB BECAUSE THE CALL IS A MUTATION. Spelled `units`, this looked like a
        property you could measure, and `of = len(batch.units)` before an `async for` handed out the
        whole Batch and left the loop nothing to iterate — the Method then completed, committed and
        returned successfully having run no author code at all. Measured on `canary-1789931619`:
        `20/20 unit(s), 0 beat(s)`, COMPLETED in 17s where the work takes 80, and the only symptom
        was an empty output dataset, which reads exactly like a filter that matched nothing.

        Taking the Batch this way gives up per-Unit commit: with no position there is nothing to
        commit by, so the whole Batch commits when the Method returns. Pushes made from the
        spawned tasks have no current Unit, so each REQUIRES an explicit `key=` (ADR 0028) — under
        real parallelism their arrival order is non-deterministic, so a positional identity was
        never knowable — and folds into results by that key at Method exit.

        MIXING THIS WITH `async for` IS REFUSED, not silently honoured. See `__anext__`.
        """
        taken = [self._hand_out() for _ in range(len(self._pending))]
        # SET ONLY WHEN SOMETHING WAS ACTUALLY TAKEN. Taking nothing is not taking: on an empty
        # Batch a following `async for` would have run zero times regardless, so flagging it would
        # turn a Method that is correct on every input into one that crashes on the empty one —
        # the worst shape of intermittent failure, and a strictly worse bug than the one this
        # guard exists to catch.
        if taken:
            self._taken_all = True
        return taken

    async def _push(self, rec: Any, key: Optional[str] = None) -> None:
        """One `await dataset.push(x)`, already reduced to JSON. Make it durable NOW, attributed
        to the Unit the iterator is handing out (ADR 0028 §1: durability unchanged) — or, when no
        Unit is current, to the Batch tail under the author's explicit key. A store failure is
        HELD, not raised, so push stays fire-and-forget (surfaced at the next checkpoint)."""
        unit = self.current
        if unit is None and key is None:
            # An out-of-loop push must name its durable identity. Raised at the call site (before
            # the try below, so it is not swallowed as a store failure), it is an author error the
            # framework refuses rather than a fourth guess at identity (ADR 0028).
            raise MissingPushKey(
                "push() with no current Unit needs an explicit key: "
                'await dataset.push(record, key="batch-summary"). A push made outside the loop '
                "belongs to no Unit's commit, so the framework reconciles it across an isolation "
                "re-invoke by that key (ADR 0028) — it cannot infer identity from where you are "
                "in control flow.")
        try:
            if unit is not None:
                unit._out.append(await self._sink.record(unit, rec))
                return
            # No current Unit: the tail, keyed by the author's explicit key. FIRST-WRITE-WINS: a key
            # already written in a prior entry is retained and this re-push is dropped BEFORE the
            # durable write — so a self-guarded push kept from entry 1 is not overwritten, and a
            # content-varying push writes no second blob (results and store agree, no orphan). A key
            # the author has not used yet is written and folded, whichever entry it first appears on.
            if key in self._tail_slots:
                return
            self._tail_slots[key] = await self._sink.record(_TailUnit(_tail_index(key)), rec)
        except Exception as e:  # noqa: BLE001 - surfaced at the checkpoint, not here
            if self._push_err is None:
                self._push_err = e

    def _raise_if_push_failed(self) -> None:
        if self._push_err is not None:
            raise self._push_err

    def __aiter__(self) -> "Batch":
        return self

    async def __anext__(self) -> Unit:
        # A failed push surfaces HERE, before the Unit it struck commits: a committed Unit is
        # skipped on retry, so committing one whose write failed would lose the record silently.
        self._raise_if_push_failed()
        # Asking for the next Unit IS the signal that the last one finished.
        done, self.current = self.current, None
        if done is not None:
            await self._commit(done)
        if not self._pending:
            # EMPTY BECAUSE IT IS FINISHED, OR EMPTY BECAUSE SOMEBODY TOOK IT? `_pending` cannot
            # tell those apart and they mean opposite things, so the flag does.
            #
            # A Method that called `take_all()` (or read the `units` alias) and then wrote
            # `async for unit in batch` used to get a clean `StopAsyncIteration` on the first
            # step: the loop body never ran, the Batch committed, and the Method returned
            # SUCCESSFULLY having executed no author code. Nothing failed anywhere — the only
            # trace was an output dataset that stayed empty, which is indistinguishable from a
            # filter that matched nothing. It cost about an hour to find (GitHub #23).
            #
            # This is a programming error, and naming it at the moment it is made is the same
            # posture `MissingPushKey` already takes for the other half of this API. It is raised
            # only when the loop is entered AFTER a take — a `take_all()` with no loop is the
            # supported concurrent pattern and is untouched, and an ordinary exhausted loop never
            # sets the flag.
            if self._taken_all and self.current is None:
                raise BatchAlreadyTaken(
                    "this Batch was emptied by take_all() (or its `units` alias) and then iterated "
                    "with `async for`, which would run the loop body zero times and report success. "
                    "Use ONE of them: `async for unit in batch` to take Units one at a time with "
                    "per-Unit commit, or `take_all()` to take them all and run them concurrently "
                    "yourself. If you wanted the size, read `batch.pending` BEFORE taking, or "
                    "`len(...)` the list take_all() returned."
                )
            raise StopAsyncIteration
        self.current = self._hand_out()
        return self.current

    async def settle(self) -> None:
        """Commit every Unit the author FINISHED but did not iterate past — the Units held for
        concurrent work, and the tail of a loop that broke early.

        The Unit the loop was still sitting on is NOT one of them: the author left the loop while
        holding it, so nothing says its work is done. It stays uncommitted and re-runs, which is
        the safe direction — a Unit committed too early is work that silently never happens.
        """
        # Method exit is the last checkpoint: a push that failed after the final commit (or from a
        # concurrent task) has nowhere later to surface, so it must surface before anything held
        # commits and the call reports success.
        self._raise_if_push_failed()
        cur, self.current = self.current, None
        if cur is not None:
            self._held.pop(cur.index, None)
        for unit in list(self._held.values()):
            await self._commit(unit)

    async def blame(self) -> Optional[Unit]:
        """The Unit the iterator was on when the Method body raised (ADR 0023 §13), so the host
        can record it as a failure. Every other Unit the author took is committed on the way out:
        what they pushed is already durable, and re-running them would be a second execution of
        finished work.

        `None` when the author held the Batch rather than iterating it — there is no position to
        blame, and inventing one would attribute a raise to whichever Unit was pulled last.
        Nothing is committed in that case either: the author's own tasks may still be running,
        and a Unit committed while its work is in flight is a Unit the retry will skip.
        """
        unit = self.current
        if unit is None:
            return None
        await self.settle()
        return unit

    def _hand_out(self) -> Unit:
        # Both the loop (__anext__) and `batch.units` route through here. The iterator's position
        # is set by __anext__ alone (`self.current`); `batch.units` hands out without a position, so
        # a push from a spawned task has no current Unit and must carry a key.
        index, value = self._pending.pop()
        unit = self._held[index] = Unit(index, value, self._takes)
        self._sink.enter(unit)
        return unit

    async def _commit(self, unit: Unit) -> None:
        if self._held.pop(unit.index, None) is not None:
            await self._sink.commit(unit)
