"""Call deployed kontra code FROM your own Temporal workflow — the client half of the SDK.

Everything else in kontra is the CALLEE: `@actor.method` + `actor.serve()` make a process that
serves work. This module is the other side, and since v2 removed the graph interpreter it is the
ONLY side that calls one. You write a plain Temporal workflow — yours, on your laptop, in your
own task queue — and orchestrate deployed Actors from it with ordinary Python control flow:

    from kontra import catalog
    from temporalio import workflow

    subfinder = catalog.actor("subfinder", "0.1.0")
    probe     = catalog.actor("probe", "0.1.0")

    @workflow.defn
    class Recon:
        @workflow.run
        async def run(self, apexes: list[str]) -> dict:
            subs, _    = await subfinder.enumerate(apexes, params={"sources": "all"})
            live, lost = await probe.head(subs)
            return {"subs": len(subs), "live": len(live), "lost": len(lost)}

THE HANDLE IS CALLABLE, AND A METHOD CALL RETURNS TWO THINGS (ADR 0028 §4). A Method name is an
attribute — `subfinder.enumerate(batch)` — and awaiting it hands back `(results, dropped)`. You
cannot reach `results` without naming `dropped`, so a caller who does not care has to write
`results, _ = ...`, which is a choice a reviewer sees rather than a silence. `dropped` is falsy
when nothing was lost, `len(dropped)` costs no fetch (the count rides on the ref's meta), and
`await dropped.rows()` gets the Units back for a retry. This is what replaced raise-by-default:
nothing fails a Batch on the framework's judgement (ADR 0023 §14), and after the tuple a caller
who ignores the drops did so on purpose rather than by forgetting to check.

THE CALLER NAMES WHERE OUTPUT GOES (ADR 0028 §2). A Method call takes an optional SECOND positional
argument — an open `dataset(name).writer()` or a bare `dataset(name)` handle — and the CALLER
publishes each returned Batch into that named Dataset once the call returns, at the granularity you
call at (per chunk when you re-page, per whole call otherwise), so `await out.publish(...)` is no
longer a line in your loop and each published Batch carries the Machine that produced it. The actor
never learns the name: its `dataset` parameter is the same unnamed sink whether or not you name a
destination (`entry_input` carries no dataset field), so publishing is the caller's — the `results`
ref reaches the lake by the same actor→blob→ref→publish path a hand-written `out.publish(results)`
took, one publish per Batch, not one per push. Rows are visible mid-run at the `units/run={run_id}`
blob level `kontra monitor --query` reads, not as Dataset rows until the call returns. Omit the
argument and the results stay an unnamed, chainable Batch that materializes nothing — the one-word
choice that keeps an intermediate fan-out cheap:

    verdicts, dropped = await ns.ask(pairs, lame)       # named Dataset  — queryable mid-run
    pairs,    dropped = await ns.delegation(batch)      # unnamed Batch  — chains

AND IT CAN BE ITERATED AS RESULTS LAND (ADR 0023 §8). The value a Method call returns is BOTH
awaitable and async-iterable — one object, two ways to take the answer. Awaited, it hands back the
whole `(results, dropped)`; iterated, it yields `(results, dropped)` per bounded chunk as each
lands, for a caller that wants to act before the Batch finishes (branch on an early finding, feed a
second Actor, stop early). Breaking the loop leaves the remaining chunks undispatched — nothing is
cancelled — and the chunk width is bounded so a large Batch cannot blow up workflow history:

    async for verdicts, dropped in ns.ask(pairs):       # a chunk at a time
        if hit(await verdicts.rows()):
            break                                       # the rest never dispatches

ONE DEPLOYED KIND (ADR 0023 §9). There is no `activities()` handle beside `actor()`: a function
with no loaded resource is an Actor with one Method and no `@actor.load`, so it is called the
way everything else is.

A CALL IS ONE BATCH; A SESSION IS A SCOPE. Calling a Method loads the actor, runs your Batch and
closes it. When you want the loaded thing to STAY loaded across several calls, open a scope —
the Actor is activated for exactly its length, and every call inside it reaches that one process
and that one instance (ADR 0023 §4):

    crawler = catalog.actor("crawler", "0.1.0")

    async with crawler["acme.com"] as browser:
        pages, _ = await browser.crawl(seeds)       # loads the browser
        await browser.extract(pages)                # same browser, same self.*

The spelling is the SAME with or without the scope and returns the same `(results, dropped)`
either way — `async with` stops being the price of admission and goes back to meaning only what
it is for, keeping `self.*` alive across several calls. Keys are optional: bare `crawler` is a
private anonymous Session. Losing the host RAISES out of the `async with` rather than silently
re-activating — see `Session`, which is where the whole deal is written down.

WHY THIS IS NOT `actor.serve()`. That boots a Temporal activity worker and blocks forever —
inside `@workflow.defn` it is three separate determinism/sandbox violations, and it points the
opposite way: it starts the CALLEE. The verb for "make a deployed actor do a batch" is a Method
call, and it is a Nexus call. (It was spelled `actor.run()` until ADR 0021, which is exactly the
confusion this module exists to end: `run` reads like the caller's verb.)

WHAT A CALL ACTUALLY IS: Nexus service `kontra.actor`, operation `run`, `EntryInput` in, a
claim-check `BareRef` out. Nexus is language-neutral, so a Python workflow and the actor's Go
handler are two halves of one call the same way the Go handler and a Python actor already were.
Nothing new is deployed for this to work — the endpoint an actor registers on boot is the
endpoint you call. (Until v2 the orchestrator's `interpreter.ts` made the identical call; it was
deleted with the graph, and these bytes are what it left behind.)

WHAT A CALLER'S RUN DOES NOT GET, stated once so it cannot be discovered the hard way:

  • No run row. The orchestrator mints runs; you are not going through it. Per-unit blobs still
    land under `units/run={run_id}/…` — so `kontra monitor --query` and DuckDB-over-S3 read
    your output normally — but `kontra runs <id>` has no status row to show.
  • No named dataset unless you publish one. ADR 0023 §1 makes materialization caller-invokable
    rather than automatic: `dataset(name).writer()` is what puts a name where `kontra explore`
    can see it, and a call on its own publishes nothing.
  • No failure policy. Isolation comes back as `dropped` and it is YOUR branch that decides —
    there is no merge/mapping layer above you to absorb it.

That last one is the reason a Method call returns `(results, dropped)` rather than a bare list: a
node that dropped every unit and a node that legitimately found nothing must not render
identically, which is exactly how a 15,814-target run once reported `completed` in seven minutes.

THE ARROW, AND THE ONE EXCEPTION IN THIS FILE. `sdk/` may not import `runtime/` — the runtime
imports the author's vocabulary, never the reverse, and tests/test_sdk_arrow.py fails on a
module-scope `internals` import anywhere under sdk/. `serve_workflows()` is the exception, for the
same reason `actor.serve()` is: it IS the handoff, the line where an author stops writing workflow
code and gives the process to the workflow host. It is a DEFERRED import inside the function
(`from internals.temporal.wfhost import serve_workflows`), so `from kontra import catalog` — what
a workflow module does, inside the Temporal sandbox — still reaches no runtime module.
"""

from __future__ import annotations

import asyncio
import functools
from collections import abc
from dataclasses import dataclass, replace
from datetime import timedelta
from typing import Any, AsyncIterator, Callable, Iterable, Mapping, Sequence

# The Nexus service every actor serves — ADR 0001, one operation, one way in. The typed contract
# is `kontra.contract.KontraActorService` (imported lazily, since it needs nexusrpc); these
# names are re-exported here because they are the cross-language literals, and because the
# congruence tests read them from one place.
SERVICE_NAME = "kontra.actor"

# The one operation on it. Named by the generated const on the Go side (RunWorkflowOperationName).
RUN_OPERATION = "run"

# The handler's blob-plane activities, registered on the SHARED queue of every deployed actor.
# We call fetch_blob to dereference a result — no new deployment needed, it is already there.
FETCH_BLOB_ACTIVITY = "kontra.fetch_blob"

# The two activities that bracket a Session (ADR 0023 §4). Served by the ACTOR's own process
# (internals/temporal/host.py), not by the handler, and scheduled by name from here — so these
# strings are a two-writer contract with no registration step to catch a rename.
OPEN_SESSION_ACTIVITY = "OpenSession"
CLOSE_SESSION_ACTIVITY = "CloseSession"


# ---------------------------------------------------------------------------------------------
# Identity. These four derivations are the cross-language contract, and there is no shared code
# by design. A drift has NO loud failure mode: the call goes to a queue or endpoint nobody serves
# and the workflow simply waits.
#
# WHAT HOLDS THEM TO ONE ANSWER IS shared/conformance/queues.json, which every language executes
# (tests/test_queue_congruence.py is this module's arm). Deliberately not a comment counting the
# peers or naming their files: the three comments that did — here, in sdk/go/catalog and in
# control/orchestrator/src/panels/pollers.ts — gave three different counts, and one of the files named here
# had been renamed out of the tree.
# ---------------------------------------------------------------------------------------------


def shared_queue(name: str, version: str = "") -> str:
    """The actor's own task queue — where its Go handler serves the workflow and the blob
    activities. `{name}-{version}`, or `{name}-shared` when a version is absent (the DIY path).

    NOT SANITISED. Temporal accepts a space and a non-ASCII rune in a queue name, and
    shared/conformance/queues.json carries both as rows: a derivation that cleaned this up would route
    to a queue nobody polls. `endpoint_name` below is the one that sanitises.
    """
    return f"{name}-{version}" if version else f"{name}-shared"


def sessions_queue(name: str, version: str = "") -> str:
    """Where the actor PROCESS polls for RunBatch/Close (ADR 0018). You do not schedule onto this
    directly — the handler's workflow does — but it is here because the caller has to be able to
    read the address it is dispatching against."""
    return shared_queue(name, version) + "-sessions"


def session_queue(name: str, version: str = "", session_id: str = "") -> str:
    """Where ONE live Session is addressed: `{shared}-s-{sessionId}` (ADR 0023 §6).

    Not to be confused with `sessions_queue` one line up — the plural is the actor's standing
    queue, polled by every worker of that version, and it is where an OPEN lands. This one comes
    into existence when a worker activates a Session and is polled by that worker alone, which is
    what pins every call in the scope to one process. The queue name IS the address: no placement
    directory, no lease, no idle policy, and an orphaned queue is a ScheduleToStart timeout rather
    than a silent re-activation somewhere else.

    Derived independently by the handler (from its own task queue and the dispatch's session id)
    and by the actor host, under the decoupling rule; shared/conformance/queues.json §session holds the
    five derivations to one answer.

    NO ID, NO QUEUE. Raising is Python's idiom for the refusal the Go sides express by returning
    the empty string: `{shared}-s-` is a real queue every Session of this actor would share, so
    the pinning would be gone with nothing failing.
    """
    if not session_id:
        raise ValueError("a per-session queue needs a session id — see ADR 0023 §6")
    return f"{shared_queue(name, version)}-s-{session_id}"


def endpoint_name(name: str, version: str = "") -> str:
    """The Nexus endpoint the actor's worker creates on boot: `kontra-{name}-{version}` with
    every non-alphanumeric collapsed to '-', doubles collapsed, ends stripped
    (echo 0.1.0 -> "kontra-echo-0-1-0").

    THE ONE DERIVATION IN THIS MODULE THAT SANITISES, because the cluster enforces
    ^[a-zA-Z][a-zA-Z0-9-]*[a-zA-Z0-9]$ on an endpoint name and enforces nothing on a queue name.
    shared/conformance/queues.json runs the same inputs through both rules for exactly that reason.
    """
    raw = f"kontra-{name}-{version}"
    safe = "".join(c if (c.isascii() and (c.isalnum() or c == "-")) else "-" for c in raw)
    while "--" in safe:
        safe = safe.replace("--", "-")
    return safe.strip("-")


def entry_input(
    units: Iterable[Any],
    *,
    run_id: str,
    node_id: str,
    params: Mapping[str, Any] | None = None,
    idempotency_key: str = "",
    expected_digest: str = "",
    method: str = "",
    session_id: str = "",
    input_ref: Mapping[str, Any] | None = None,
) -> dict:
    """Build the `EntryInput` the actor's Nexus op takes — the one wire shape in this module.

    Kept a plain function (and not inlined into dispatch) because it IS the contract: the field
    set is pinned against `kontra/v1/entry_pb2` by tests/test_workflows_client.py, the same way
    the Go and TS peers are pinned against the same proto. `return_ref` is always true — the
    result comes back as a claim-check ref and is dereferenced on demand, which is what keeps a
    50 MB batch result out of your workflow's history.

    Unset optionals are OMITTED rather than sent as null — the shape the deleted interpreter.ts
    set, kept because the Go handler still decodes against it and a null would not match.

    `method` names which Method of the actor to call and `session_id` which Session the call
    belongs to (ADR 0023 §5, §6). Both are plain strings where "" means unset, and both are
    sent even when empty — the same way the other ids are, so one caller's bytes match the
    other's. An actor with a single Method accepts an unnamed dispatch; one with several
    refuses it rather than taking declaration order.
    """
    entry: dict[str, Any] = {
        "units": list(units),
        "return_ref": True,
        "run_id": run_id,
        "idempotency_key": idempotency_key,
        "node_id": node_id,
        "expected_digest": expected_digest,
        "method": method,
        "session_id": session_id,
    }
    if params:
        entry["params"] = dict(params)
    if input_ref:
        # `units` stays PRESENT AND EMPTY, never dropped: the handler keys the ref path off
        # `len(units) == 0 && in.InputRef != nil` (runtime/handler/workflow.go), so removing the key
        # would not select it — it would just send an id-less batch of nothing.
        entry["input_ref"] = dict(input_ref)
    return entry


# ---------------------------------------------------------------------------------------------
# What the log says about a dispatch.
#
# A Method call's own name travels INSIDE the dispatch (`entry_input`'s `method`), and a payload
# on this deployment may be a claim-check ref (ADR 0007) — so a surface that wanted to say which
# Method a run called would pay a blob GET per dispatch, which for a 623-unit sweep is the
# unbounded read the whole log discipline exists to prevent. It rides as a Temporal user-metadata
# Summary instead: METADATA on the scheduling event, read straight off the event with nothing
# fetched, and rendered by Temporal's own UI on the bar label — so kontra's transcript and
# Temporal's own surface say the same sentence rather than diverging.
#
# THE BACKING WORKFLOW ID IS NOT THE PLACE FOR IT and deliberately stays as it is
# (handler/nexus.go): it is a label nothing parses, and a label that names the wrong thing is
# worse than no label at all.
#
# The reader is `methodOf` in control/orchestrator/src/transcript.ts, which takes the FIRST field of the
# line as the Method name when it has the shape of one. Like the identity block above this is a
# two-writer contract with no registration step to catch a drift — and like it, the failure mode
# was chosen: a line the reader does not recognise leaves the transcript with NO Method name,
# never with a wrong one, and every older run in the archive lands in exactly that case.
# ---------------------------------------------------------------------------------------------

#: What a Summary joins its fields with, and what the reader splits on — the same separator the
#: reduced log already puts between the `key=value` pairs of an event's own detail line.
SUMMARY_SEP = " · "

#: How many UTF-8 BYTES one Summary may take, in total.
#:
#: NOTHING VALIDATES THIS — not the SDK, which takes any string, and not the server's reply. An
#: overrun is therefore never an error; it is a line that gets cut somewhere downstream, by a
#: server limit or by a UI that elides, with nothing said about it anywhere.
#:
#: WHICH IS WHY IT IS BUILT TO FIT, and that is the whole discipline of the function below.
#: Composing the line first and trimming it afterwards loses whichever field happened to be last,
#: silently, on exactly the dispatches worth reading — a long Actor name, an operator's URL as a
#: key. Spending the budget in priority order costs one pass and cannot surprise anyone.
SUMMARY_BUDGET = 200

#: The most of the budget one Method name may take. Generous for an identifier — `crawl`,
#: `extract_links` — and a ceiling, so that whatever a Method is called the Actor still fits.
_METHOD_CAP = 64

#: …and the most one `actor@version` may take, for the same reason in the other direction.
_ACTOR_CAP = 80

#: Below this there is no room to say anything TRUE about a key, so it is left out altogether
#: rather than rendered as a pair of brackets with an ellipsis inside them.
_KEY_MIN = 8

_ELLIPSIS = "…"


def _utf8(text: str) -> int:
    """How many bytes this string costs. The budget is in bytes; `len()` counts characters, and
    the two differ on exactly the keys an operator is most likely to type."""
    return len(text.encode("utf-8"))


def _fit(text: str, room: int) -> str:
    """`text`, or as much of it as fits in `room` UTF-8 BYTES with an ellipsis marking the cut.

    CUT ON A CHARACTER BOUNDARY. Slicing the encoded form can land inside a multi-byte character,
    so the kept prefix is decoded with `ignore` — which drops exactly the partial trailing bytes
    and nothing else, because the input came from a valid `str` and has nothing else invalid in
    it to drop.

    An empty answer means "there was no room to say this", and every caller leaves the field out
    entirely when it gets one. A lone `…` is never returned: a field that says only that it was
    cut is a label pretending to be a name.
    """
    if room <= 0:
        return ""
    raw = text.encode("utf-8")
    if len(raw) <= room:
        return text
    mark = _utf8(_ELLIPSIS)
    kept = raw[: room - mark].decode("utf-8", "ignore") if room > mark else ""
    return kept + _ELLIPSIS if kept else ""


def dispatch_summary(
    actor: str,
    version: str = "",
    method: str = "",
    key: str = "",
    units: int | None = None,
    *,
    budget: int = SUMMARY_BUDGET,
) -> str:
    """The one line a dispatch puts on its own scheduling event.

        crawl · crawler@0.1.0[acme.com] · 12 units

    THE METHOD IS PAID FIRST, because it is the one fact here that nothing else in the log
    carries. The Actor and its version are recoverable from the Nexus endpoint and exactly from
    the shared task queue; the unit count is at least plausible from elsewhere; the Method is in
    the payload and nowhere else. So it goes at the front of the line, which is also where a bar
    label in Temporal's UI is read from.

    THE COUNT IS RESERVED BEFORE THE KEY, and that ordering is the point of the budget. The key
    is the one field whose length nobody in this repo controls — `crawler["https://…"]` is a
    legal binding — and an operator's URL must not be what pushes the unit count off the end.

    A dispatch that names no Method — a single-Method Actor accepts an unnamed one — simply
    starts with the Actor instead. The reader's shape test rejects `crawler@0.1.0` as a Method
    name, so that dispatch reads as having no Method rather than as calling one named `crawler`.
    """
    fields: list[str] = []
    left = budget
    sep = _utf8(SUMMARY_SEP)

    # Reserved, not appended: it goes on the end, but its room is taken out of the budget here so
    # that everything discretionary below is spent against what is genuinely left over.
    tail = f"{units} units" if units is not None else ""
    if tail and _utf8(tail) + sep <= left:
        left -= _utf8(tail) + sep
    else:
        tail = ""

    if method:
        fitted = _fit(method, min(_METHOD_CAP, left))
        if fitted:
            fields.append(fitted)
            left -= _utf8(fitted)

    # THE `@` IS ALWAYS THERE, EVEN WITH NO VERSION, and it is not decoration: it is the whole
    # reason the reader can tell a Method name from an Actor name by looking at one field. A
    # version-less Actor (the DIY path, `echo-shared`) would otherwise render as a bare `echo` —
    # and on a dispatch that named no Method that is the FIRST field, which the reader would then
    # read as a Method called `echo`. `echo@` says the true thing instead: an Actor, no version.
    who = _fit(
        f"{actor}@{version}",
        min(_ACTOR_CAP, left - (sep if fields else 0)),
    )
    if who:
        left -= _utf8(who) + (sep if fields else 0)
        # The brackets cost two of the bytes the key is being given, so they come out of its room
        # rather than out of the budget after the fact.
        if key and left - 2 >= _KEY_MIN:
            fitted = _fit(key, left - 2)
            who += f"[{fitted}]"
            left -= _utf8(fitted) + 2
        fields.append(who)

    if tail:
        fields.append(tail)
    return SUMMARY_SEP.join(fields)


# ---------------------------------------------------------------------------------------------
# Results — a Method call returns (results, dropped)
# ---------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class Batch:
    """A set of Units that lives in the object store, addressed by a ref — the currency of v2.

        A Dataset yields Batches. A Method call returns `(results, dropped)`, both Batches.

    That `results` is a Batch is what lets one Method's output feed the next without either side
    holding a row:

        async with kontra.actor("crawl4ai") as crawler, kontra.actor("extract") as ex:
            async for batch in catalog.dataset("targets").batches(200, order_by="host"):
                found, _ = await crawler.crawl(batch)   # Batch in, Batch out
                await ex.extract(found)                 # chained, nothing materialized

    WHAT IT CARRIES, and why each field is here rather than one fetch away: `n`, `isolated` and
    `machine` come off the ref's meta, which the handler stamps, so `len(batch)`, "did anything
    get dropped" and "which Machine ran this" are answerable INLINE. That is what lets the
    `dropped` half of the tuple (ADR 0028 §4) report a drop count with zero fetches — the failure
    rows are fetched only when there are some and somebody asks with `await dropped.rows()`.

    IT IS DELIBERATELY NOT ITERABLE. `entry_input` does `list(units)`, so an iterable Batch
    reaching a Method call would be silently flattened into inline units — the exact regression
    this type exists to prevent, and it would fail as a size problem in production rather than
    a type error in a test. Use `await batch.rows()` when you genuinely want the records.

    Distinct from `kontra.batch.Batch`, which is the CALLEE's view — the thing a Method's
    author loops over. Same word, opposite ends of the call; an author writing both halves
    imports both.
    """

    #: The claim-check ref addressing this Batch's units — a bare list, so it is directly
    #: usable as the next dispatch's `input_ref`.
    ref: Mapping[str, Any]
    #: How many records the Batch holds, from the ref's meta. No fetch.
    n: int = 0
    #: How many Units the producing Method permanently dropped. No fetch.
    isolated: int = 0
    #: False when the producer returned before covering its input.
    done: bool = True
    #: Where the failure rows live, when there are any — a CAS sha, fetched on demand.
    failures_sha: str = ""
    #: Which actor produced it: `rows()` / `failures()` dereference on that actor's shared queue.
    actor: str = ""
    version: str = ""
    #: WHICH MACHINE ran the Method that produced it — the actor host's own hostname, stamped
    #: into the meta by the handler and read here with NO fetch, like every other field on this
    #: class. Empty means unrecorded, and unrecorded is a real answer: a Batch paged out of a
    #: Dataset was produced by the lake, not by a Machine, and an actor host older than this
    #: contract reports nothing rather than a plausible-looking default. `publish` writes it as
    #: the row's Machine, which is how "which Machine produced this verdict" becomes a query.
    machine: str = ""

    def __len__(self) -> int:
        return self.n

    def __bool__(self) -> bool:
        # Explicit, because __len__ alone would make a fully-isolated Batch falsy and a caller
        # could read that as "nothing to do" rather than "everything was dropped".
        return bool(self.n) or bool(self.isolated)

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        drops = f", {self.isolated} isolated" if self.isolated else ""
        return f"<Batch {self.n} units{drops} from {self.actor or '?'}>"

    @classmethod
    def from_ref(cls, ref: Mapping[str, Any], *, actor: str = "", version: str = "") -> "Batch":
        """Build one from what a dispatch returned. Meta values are strings on the wire."""
        meta = dict((ref or {}).get("meta") or {})

        def _int(key: str) -> int:
            try:
                return int(meta.get(key, "") or 0)
            except (TypeError, ValueError):
                return 0

        return cls(
            ref=dict(ref or {}),
            n=_int("n"),
            isolated=_int("isolated"),
            done=meta.get("done", "true") != "false",
            failures_sha=str(meta.get("failures") or ""),
            actor=actor,
            version=version,
            # Absent on a Dataset page (nothing ran a Method) and on a ref an older actor host
            # produced. Both mean unrecorded, and both must stay empty rather than borrow a
            # neighbouring value — telling "nothing wrote this" from "this is the value" is the
            # whole point of carrying it.
            machine=str(meta.get("machine") or ""),
        )

    async def batches(
        self, size: int, *, timeout: timedelta = timedelta(minutes=5)
    ) -> AsyncIterator["Batch"]:
        """Re-page THIS Batch into Batches of at most `size` — the peer of
        `dataset(...).batches()`, and the answer to "how big is what the last Method emitted?".

            resolved, _ = await dns.addrs(page)       # 200 units in, 10_000 emitted
            async for chunk in resolved.batches(200): # back to a sane width
                await probe.head(chunk)

        A caller sizes its INPUT pages, but a Method's fan-out is the author's business: a 1→50
        Method turns a 200-unit page into a 10,000-unit result. Handing that to the next Actor
        is one activity on one worker with one oversized blob — past the measured 200/1000
        guard, with no parallelism and no isolation boundary between 10,000 units.

        Skip it when you do not need it: `len(batch)` is free, so
        `if len(resolved) > 500: ...` costs nothing and a 1:1 Method needs no re-paging at all.
        """
        if size <= 0:
            raise ValueError(f"batch size must be positive, got {size}")
        if not self.n:
            return
        if self.n <= size:
            # ALREADY FITS — yield self and schedule nothing. This is what lets a caller write
            # `async for chunk in resolved.batches(size)` unconditionally instead of guarding it
            # with `if len(resolved) > size`, which is a branch every caller would otherwise
            # copy. A 1:1 Method therefore pays nothing for the loop.
            yield self
            return

        from temporalio import workflow

        out = await workflow.execute_activity(
            SPLIT_BATCH_ACTIVITY,
            {"sha256": (self.ref or {}).get("sha256", ""), "size": size},
            task_queue=DATASET_QUEUE,
            start_to_close_timeout=timeout,
        )
        for ref in out.get("refs") or []:
            # The split refs are minted by the orchestrator, so their meta names no Machine —
            # but the ROWS inside them are still the ones this Batch's Machine produced, and
            # re-paging must not lose that. Carried the same way `actor` and `version` are, and
            # for the same reason: slicing a result does not change who produced it.
            chunk = Batch.from_ref(ref, actor=self.actor, version=self.version)
            yield replace(chunk, machine=self.machine)

    async def rows(self, *, timeout: timedelta = timedelta(minutes=5)) -> list:
        """Materialize this Batch's records INTO YOUR WORKFLOW. Explicit because it is the one
        thing the ref exists to avoid — reach for it at the end of a pipeline, on something
        small, not between two Methods.

        RECORDS, NOT REFS, and it did not used to be. An actor host with an object store commits
        each emitted record to its own blob, so fetching the envelope alone hands back a list of
        `{"$ref": …}` entries — which is the right thing to pass to the next Method (see
        `_resolved_ref`) and exactly the wrong thing to call "this Batch's records". A caller who
        asked to materialize and got refs has no way to tell that from an actor that emitted them
        literally, and the first place it shows up is a workflow's own return value: two verdicts,
        both `{"$ref": …}`, in a run that says it completed.

        ONE EXTRA ACTIVITY, AND ONLY WHERE THERE IS SOMETHING TO RESOLVE. `_resolved_ref` is a
        no-op for a Dataset page (no `actor`) and for an actor deployed without an object store,
        so the cost lands exactly on the case that needs it. `rows()` is already the documented
        end-of-pipeline call, which is where paying it is right.
        """
        return await self._fetch(await _resolved_ref(self), timeout=timeout)

    async def failures(self, *, timeout: timedelta = timedelta(minutes=5)) -> list:
        """The Units the producing Method permanently dropped. Empty without a fetch when
        `isolated` is 0, which is the common case."""
        if not self.isolated or not self.failures_sha:
            return []
        return await self._fetch(
            {"sha256": self.failures_sha, "size": 0, "meta": {}}, timeout=timeout)

    async def _fetch(self, ref: Mapping[str, Any], *, timeout: timedelta) -> list:
        from temporalio import workflow

        payload = await workflow.execute_activity(
            FETCH_BLOB_ACTIVITY,
            dict(ref),
            task_queue=shared_queue(self.actor, self.version),
            start_to_close_timeout=timeout,
        )
        if isinstance(payload, list):
            return payload
        # An envelope-kind ref, i.e. one minted before the split.
        if isinstance(payload, abc.Mapping):
            return list(payload.get("results") or [])
        return [] if payload is None else [payload]


class Dropped:
    """The second half of `(results, dropped)` — the Units a Method call permanently dropped
    (ADR 0028 §4).

    It exists to be the value a caller CANNOT skip past: destructuring the tuple forces the drops
    into a name, so a caller who does not care writes `results, _ = ...` on purpose rather than
    never learning there were any. This is what replaced raise-by-default — nothing fails a Batch
    on the framework's judgement (ADR 0023 §14), so a run that dropped every Unit and a run that
    found nothing must be told apart by the caller, and they are: `len(dropped)` is the count.

    `bool(dropped)` and `len(dropped)` cost NO FETCH — the isolation count rides on the producing
    Batch's ref meta, the same way `len(results)` does. `await dropped.rows()` is the one call
    that pays a fetch, and it exists for exactly one reason: to hand the lost Units back for a
    retry (see `examples/python/workflows/sweep.py`).
    """

    def __init__(self, batch: "Batch") -> None:
        #: The producing Batch. Its `isolated` count and `failures_sha` are the whole of what a
        #: Dropped is — a view over the drop side of the same ref, not a second payload.
        self._batch = batch

    def __len__(self) -> int:
        return self._batch.isolated

    def __bool__(self) -> bool:
        # A drop count of zero is the common case and must read as falsy, so `if dropped:` is the
        # branch a caller writes when something was lost — and an empty `results` beside a truthy
        # `dropped` is precisely how "dropped everything" stops looking like "found nothing".
        return bool(self._batch.isolated)

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        return f"<Dropped {self._batch.isolated} unit(s) from {self._batch.actor or '?'}>"

    async def rows(self, *, timeout: timedelta = timedelta(minutes=5)) -> list:
        """The dropped Units themselves, fetched on demand — for handing back to a retry. Empty
        without a fetch when nothing was dropped."""
        return await self._batch.failures(timeout=timeout)


#: The chunk width a Method call iterates at (ADR 0028 §5's re-paging, turned inward for the
#: iterating form of ADR 0023 §8). BOUNDED, and the bound is the point: `async for` yields one
#: `(results, dropped)` per chunk, and each yield is a workflow-visible event — a Nexus dispatch,
#: plus a publish when a Dataset is named. At per-record granularity a 40,000-unit Batch would
#: build 40,000 events and rebuild the history blow-up that once capped a run near 5,100 units
#: (memory: batchSize:1 caps ~5,100). Fixed at `SAFE_PAGE_MAX` (200 — the measured safe per-node
#: width the pager already refuses above), so the SAME 40,000-unit Batch iterates in
#: ceil(40000/200) = 200 chunks, i.e. ~200 dispatch events, not 40,000. Measured budget: ~4-8
#: history events per chunk (schedule/started/completed for the dispatch, plus the publish's
#: three when a Dataset is named), so the ceiling on a Method call's iteration cost is
#: ceil(n / 200) × ~8 events, independent of how many records the actor pushes inside a chunk.
#: Not a per-call knob: a smaller width is finer results at more events, which is the trade this
#: bound exists to take off the caller.
def _iter_chunk_size() -> int:
    return SAFE_PAGE_MAX


async def _iter_chunks(units: "Iterable[Any] | Batch") -> AsyncIterator[Any]:
    """Split a Method call's input into bounded chunks for the iterating form (ADR 0023 §8).

    A `Batch` re-pages through `Batch.batches` (the `splitBatch` activity), so a fan-out result
    is sliced by ref without a row entering the workflow; a plain list is sliced in place. Either
    way the chunk width is `_iter_chunk_size()`, which is what bounds the history cost — see its
    comment. An already-small input yields exactly one chunk, so the iterating and awaiting forms
    dispatch the same single Batch and their totals are trivially identical there.
    """
    size = _iter_chunk_size()
    if isinstance(units, Batch):
        async for sub in units.batches(size):
            yield sub
        return
    seq = list(units)
    for i in range(0, len(seq), size):
        yield seq[i:i + size]


class MethodCall:
    """What a Method call returns — awaitable AND async-iterable, one object, two ways to take the
    answer (ADR 0023 §8, ADR 0028 §4). The Actor does not know or care which the caller wrote:

        verdicts, dropped = await ns.ask(chunk)             # once, at the end
        async for verdicts, dropped in ns.ask(chunk):       # as the actor's chunks land
            ...

    AWAITED, it dispatches the whole Batch as one Method call and hands back `(results, dropped)`.
    ITERATED, it dispatches the Batch in bounded chunks and yields `(results, dropped)` per chunk,
    for a caller that wants to ACT on results before the whole Batch finishes — branch on an early
    finding, feed a second Actor, stop early. The totals reconcile: the chunks partition the same
    input, so `sum(len(results) for results, _ in ns.ask(b))` equals `len((await ns.ask(b))[0])`.

    BREAKING OUT OF THE LOOP is defined, not discovered (ADR 0023 §8). Each chunk is a COMPLETE
    Method call that finished before it was yielded, so a `break` simply stops dispatching the
    remaining chunks: nothing is cancelled, no Actor is left mid-Batch, and every chunk already
    yielded is committed and (if a Dataset was named) published. The blast radius of stopping
    early is exactly the input you did not reach.

    SINGLE USE. A MethodCall dispatches work, so consuming it twice — awaiting after iterating, or
    iterating twice — would silently re-run the whole Batch. It refuses instead, naming both
    forms, which is the defined answer to "a caller that iterates and then also awaits".

    `dataset` is the caller's output destination (ADR 0028 §2); when named, every chunk (or the
    whole Batch, when awaited) publishes into it, so the iterating form streams to a named Dataset
    at chunk granularity while the caller also acts on each chunk.
    """

    __slots__ = ("_caller", "_method", "_units", "_dataset", "_options", "_consumed")

    def __init__(
        self,
        caller: Any,
        method: str,
        units: "Iterable[Any] | Batch",
        dataset: Any = None,
        **options: Any,
    ) -> None:
        self._caller = caller
        self._method = method
        self._units = units
        self._dataset = dataset
        self._options = options
        self._consumed = False

    def _claim(self, how: str) -> None:
        if self._consumed:
            raise RuntimeError(
                f"this {self._method}() call was already consumed; a Method call runs work and "
                f"cannot be {how} twice — await it OR iterate it, once"
            )
        self._consumed = True

    async def _dispatch(self, units: "Iterable[Any] | Batch") -> "Batch":
        batch = await self._caller._dispatch_one(self._method, units, **self._options)
        if self._dataset is not None:
            await _publish_to(self._dataset, batch)
        return batch

    def __await__(self):
        self._claim("awaited")
        return self._whole().__await__()

    async def _whole(self) -> tuple["Batch", "Dropped"]:
        batch = await self._dispatch(self._units)
        return batch, Dropped(batch)

    def __aiter__(self) -> "AsyncIterator[tuple[Batch, Dropped]]":
        self._claim("iterated")
        return self._each()

    async def _each(self) -> "AsyncIterator[tuple[Batch, Dropped]]":
        async for sub in _iter_chunks(self._units):
            batch = await self._dispatch(sub)
            yield batch, Dropped(batch)


# ---------------------------------------------------------------------------------------------
# Handles
# ---------------------------------------------------------------------------------------------


class ActorHandle:
    """A deployed actor, addressed by (name, version). Cheap and stateless — build it at module
    scope next to your workflow class; nothing happens until you call a Method.

    A METHOD IS AN ATTRIBUTE, AND CALLING IT RETURNS `(results, dropped)` (ADR 0028 §4).
    `ns.delegation(batch)` reads like the Actor's own API rather than a string dispatch, and it
    works whether or not a scope is open — outside `async with` it is one load/run/close on the
    actor's shared queue; inside one it pins to the scope's process and instance. Either way it
    returns the same tuple, so `async with` is no longer the price of admission to a chainable
    result (the hole that forced `Batch.from_ref(await ns.dispatch_ref(...))` on callers).
    """

    def __init__(
        self, name: str, version: str = "", *, endpoint: str | None = None, key: str = ""
    ) -> None:
        self.name = name
        self.version = version
        #: Overridable for a cross-namespace endpoint registered under another name.
        self.endpoint = endpoint or endpoint_name(name, version)
        #: The virtual-object key, bound by `handle[key]`. Empty = un-keyed (a fresh instance
        #: per dispatch). Rides the wire as `idempotency_key`; see __getitem__.
        self.key = key
        #: The scope opened by `async with handle`, if any. See __aenter__.
        self._scope: "Session | None" = None

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        keyed = f"[{self.key!r}]" if self.key else ""
        return f"<kontra actor {self.name}@{self.version or 'shared'}{keyed} via {self.endpoint}>"

    def __getitem__(self, key: str) -> "ActorHandle":
        """Bind a KEY — `crawler["acme.com"]` — making this actor a virtual object.

        The key becomes the dispatch's `idempotency_key`, which is what the handler derives the
        actor id from (`runtime/handler/workflow.go`) and what the backing workflow is named after
        (`handler/nexus.go:backingWorkflowID`). Three things follow, and all three are the point:

          • **One at a time per key.** The backing workflow id is unique server-side, so two
            batches for one key cannot run concurrently — the property Restate calls an
            exclusive handler and Orleans calls single activation. Different keys still run
            fully in parallel, capped only by KONTRA_MAX_PARALLEL_SESSIONS.
          • **`self.object_state` is scoped to the key** (ADR 0022) and outlives the batch, so
            `crawler["acme.com"]`'s dedupe set is still there on the next dispatch, next week.
          • **A concurrent second dispatch ATTACHES rather than queues.** It joins the running
            execution and returns THAT batch's results — it does not run your units. This is
            correct for a retry (the field is called idempotency_key for a reason) and wrong if
            you meant "queue behind it": for that, await the first dispatch before the second.

        Keys are the caller's strings and are namespaced per actor, so `beacon["acme.com"]` and
        `crawler["acme.com"]` share nothing.
        """
        if not isinstance(key, str):
            raise TypeError(f"actor key must be a str, got {type(key).__name__}")
        k = key.strip()
        if not k:
            raise ValueError("actor key must be a non-empty string")
        if any(c in k for c in "\x00\r\n\t"):
            raise ValueError(f"actor key {key!r} contains a control character")
        if len(k) > 400:
            # It becomes part of a Temporal workflow id, which has a server-side size limit; a
            # cap here fails at the call site instead of deep inside a Nexus start.
            raise ValueError(f"actor key is {len(k)} chars; cap is 400")
        return ActorHandle(self.name, self.version, endpoint=self.endpoint, key=k)

    def session(
        self,
        *,
        open_timeout: timedelta = timedelta(minutes=5),
        close_timeout: timedelta = timedelta(minutes=1),
    ) -> "Session":
        """A Session on this actor, not yet opened — `async with crawler.session() as browser:`.

        `async with crawler` (or `async with crawler["acme.com"]`) is the short form and is what
        you normally write; this one exists for the case that form cannot serve, which is two
        anonymous scopes open at once on ONE module-scope handle — the handle would not know
        which scope is exiting. A keyed handle is already a fresh object per `handle[key]`.
        """
        return Session(self, open_timeout=open_timeout, close_timeout=close_timeout)

    async def __aenter__(self) -> "Session":
        """Open a Session on this actor: `async with crawler["acme.com"] as browser:` (§4).

        Refuses a second concurrent scope on one handle rather than guessing at exit time, since
        `__aexit__` takes no argument saying which Session it is ending. `.session()` is the
        answer when you want two.
        """
        if self._scope is not None:
            raise RuntimeError(
                f"{self.name} already has a scope open on this handle; `async with` cannot tell "
                "two apart at exit — open each with actor.session() (or bind a key)"
            )
        self._scope = self.session()
        try:
            return await self._scope.__aenter__()
        except BaseException:
            self._scope = None
            raise

    async def __aexit__(self, *exc: Any) -> bool:
        scope, self._scope = self._scope, None
        if scope is None:  # pragma: no cover - only reachable by calling __aexit__ by hand
            return False
        return await scope.__aexit__(*exc)

    def __getattr__(self, method: str) -> Any:
        """A Method of this Actor is an attribute here — `ns.delegation(batch)` (ADR 0028 §4).

        Calling it builds a `MethodCall`, the object that is both awaitable and async-iterable
        (ADR 0023 §8), so `await ns.delegation(b)` and `async for … in ns.delegation(b)` are two
        readings of the same call. `functools.partial(MethodCall, self, method)` binds the caller
        and the Method name; the call site supplies `units`, the optional output `dataset`, and
        the keyword options.

        Reached only for names this class does not define, so `session`, `dispatch_ref`, `key`
        and the dunders keep their meaning. A leading underscore is refused rather than treated
        as a Method name, so an attribute probe (pickle, copy, `hasattr` on a dunder) does not
        turn into a dispatch.
        """
        if method.startswith("_"):
            raise AttributeError(method)
        return functools.partial(MethodCall, self, method)

    async def _dispatch_one(
        self, method: str, units: Iterable[Any] | "Batch", **options: Any
    ) -> "Batch":
        """One Method dispatch over one Batch, returning the results Batch — the routing half a
        Method call shares whether it is awaited whole or iterated in chunks (ADR 0028 §4).

        Inside `async with` the call routes through the open scope, so it lands on the Session's
        own queue and reaches the one pinned process (ADR 0023 §6); outside one it is a plain
        dispatch on the actor's shared queue — a single load/run/close, which is what an Activity
        is. The route is the only thing that differs, which is why the caller never branches on
        whether a scope happened to be open.

        An unknown Method name is not intercepted here — it rides the wire as `method=` and the
        actor's registry refuses it (`resolve_method`), surfacing as the awaited call raising.
        Both routes it can take (the shared queue, or the pinned Session's) are actually polled,
        so a typo fails loudly rather than sitting forever on a queue nobody serves.
        """
        scope = self._scope
        if scope is not None:
            return await scope._dispatch_batch(method, units, **options)
        ref = await self.dispatch_ref(units, method=method, **options)
        return Batch.from_ref(ref, actor=self.name, version=self.version)

    async def dispatch_ref(
        self,
        units: Iterable[Any],
        *,
        params: Mapping[str, Any] | None = None,
        run_id: str | None = None,
        node_id: str | None = None,
        idempotency_key: str = "",
        expected_digest: str = "",
        method: str = "",
        session_id: str = "",
        schedule_to_close_timeout: timedelta | None = None,
    ) -> dict:
        """Dispatch and return the raw claim-check ref WITHOUT fetching it.

        For when the payload is large and you only need to hand it on — refs are the currency
        (ADR 0007), and a ref you never dereference costs nothing to carry.

        Note the asymmetry before you try to chain one straight back in: a result ref addresses
        an ENVELOPE (`{done, results, failures, opens}`), while an input ref must address a bare
        list of units. The actor's handler decodes an input ref into `[]any` and an envelope will
        not fit that shape. Fetch, take `.results`, and pass those.
        """
        from temporalio import workflow

        info = workflow.info()
        # run_id MUST be non-empty: the handler refuses an id-less run rather than letting two
        # dispatches share one actor instance and read each other's committed state. The
        # workflow id (not the run id) is the right default — it survives continue-as-new, so
        # every attempt of one logical run writes to one `units/run=…` partition.
        rid = run_id or info.workflow_id
        # A distinct node id per dispatch, or two dispatches of the same actor in one workflow
        # would derive the same actor id AND the same backing workflow id, and the second would
        # attach to the first instead of running. `workflow.uuid4` is the deterministic,
        # replay-stable source; pin `node_id` yourself when you WANT that attach (a retry that
        # must resume the same actor instance).
        nid = node_id or f"{self.name}-{workflow.uuid4().hex[:8]}"
        # A key bound by `handle["acme.com"]` IS the idempotency key — it outranks the generated
        # node id in the handler's actor-id derivation, which is what makes the dispatch land on
        # the same virtual object every time. An explicit argument still wins over the binding.
        idem = idempotency_key or self.key

        # A Batch is handed over BY REF and never materialized: this is the seam where one
        # Method's output becomes the next one's input without a row entering this workflow
        # (ADR 0007). Type-tested rather than duck-typed, because the failure mode of getting
        # it wrong is a 50 MB batch silently inlined into history.
        inbound = units
        ref_in: Mapping[str, Any] | None = None
        if isinstance(units, Batch):
            ref_in, inbound = await _resolved_ref(units), []

        entry = entry_input(
            inbound,
            params=params,
            run_id=rid,
            node_id=nid,
            idempotency_key=idem,
            expected_digest=expected_digest,
            method=method,
            # Which Session this call belongs to (ADR 0023 §6). The handler reads it to address
            # the Session's own queue; empty means no scope was opened, and the batch runs on the
            # actor's shared queue with load and close around it — which is what an Activity is.
            session_id=session_id,
            input_ref=ref_in,
        )

        # The shared service definition, not a pair of strings — Temporal's own pattern for
        # calling across a boundary, and the reason the operation name is declared once instead
        # of typed at every call site. Imported here rather than at module scope so
        # `import kontra` stays free of a Temporal dependency.
        from kontra.contract import KontraActorService

        client = workflow.create_nexus_client(
            service=KontraActorService, endpoint=self.endpoint
        )
        ref = await client.execute_operation(
            KontraActorService.run,
            entry,
            schedule_to_close_timeout=schedule_to_close_timeout,
            # METADATA, NOT PAYLOAD — the Method name is in `entry` too, but `entry` is a payload
            # and may be a claim-check ref, so this is the copy a reader can afford. See
            # `dispatch_summary`.
            #
            # The count comes from the Batch when the units rode in by ref — `entry['units']`
            # is empty on that path, so reading it would label every chained dispatch
            # "0 units" in the Temporal UI.
            summary=dispatch_summary(
                self.name,
                self.version,
                method,
                self.key,
                len(units) if isinstance(units, Batch) else len(entry["units"]),
            ),
        )
        return dict(ref) if ref else {}


class Session:
    """One activated Actor, for exactly as long as the scope (ADR 0023 §4, §6, §7).

        crawler = catalog.actor("crawler", "0.1.0")

        async with crawler["acme.com"] as browser:
            pages, _ = await browser.crawl(seeds)
            await browser.extract(pages)

    Inside the scope the Actor is LOADED and pinned: `browser.crawl` and `browser.extract` reach
    the same process and the same instance, so whatever `@actor.load` opened — a browser, a
    connection pool, a compiled ruleset — is there for both, and `self.*` carries between them.
    Two Method calls are two dispatches, not one long one; the scope is what makes them one
    Session.

    HOW THE PINNING WORKS, because it explains every timeout you can meet here. Opening puts one
    activity on the actor's shared queue, where every worker of that version polls — Temporal's
    dispatch IS the placement decision. The host that takes it starts a second worker on a queue
    named after this Session alone, and nothing else ever polls that queue. So the queue name is
    the address: no directory, no lease, no idle policy.

    LOSING THE HOST FAILS THE SCOPE (§7). The Session's queue is then orphaned and the next call
    sits in it until ScheduleToStart fires, which surfaces as an activity timeout out of the
    `async with`. That is the deal being struck: while a Session lives `self.*` is coherent, and
    when it cannot be, you get an exception rather than a silent re-activation with an empty
    instance. Catch it, reopen, and re-dispatch the Batch you were on — the blast radius is one
    Batch, because the cursor is yours and the commit map is keyed by the Batch's content (§17).

    KEYS ARE OPTIONAL (§10). `crawler["acme.com"]` claims a shared identity: the durable
    `object_state` of that key, still there next week (ADR 0022). Bare `crawler` is a private
    anonymous Session that shares nothing — which is what two independent scans of one host
    need, and why keying is never a tax on an ordinary dispatch.

    The Actor is loaded on the FIRST Method call, not at open, so `@actor.load` sees that call's
    params. A scope that opens and dispatches nothing costs one worker and closes empty.
    """

    def __init__(
        self,
        handle: ActorHandle,
        *,
        open_timeout: timedelta = timedelta(minutes=5),
        close_timeout: timedelta = timedelta(minutes=1),
    ) -> None:
        self.handle = handle
        self.open_timeout = open_timeout
        self.close_timeout = close_timeout
        self._id = ""

    @property
    def session_id(self) -> str:
        """The Session's id — empty until the scope is open. It names the queue, so it is also
        how a run in the Temporal UI is traced to the process that served it."""
        return self._id

    @property
    def key(self) -> str:
        """The key this scope claimed, or "" for an anonymous Session."""
        return self.handle.key

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        return f"<kontra session {self.handle.name}@{self.handle.version} {self._id or 'unopened'}>"

    async def __aenter__(self) -> "Session":
        await self._open()
        return self

    async def __aexit__(self, *exc: Any) -> bool:
        await self._close()
        return False  # never swallow the body's exception — losing the host must be seen

    async def _open(self) -> None:
        from temporalio import workflow

        if self._id:
            raise RuntimeError(f"session {self._id} is already open")
        # Deterministic and replay-stable, and short because it becomes part of a task queue
        # name. Minted by the CALLER because the caller owns the scope's lifetime.
        self._id = workflow.uuid4().hex[:12]
        await workflow.execute_activity(
            OPEN_SESSION_ACTIVITY,
            {"session_id": self._id, "key": self.key},
            task_queue=sessions_queue(self.handle.name, self.handle.version),
            # Per attempt. The open only spawns a worker, so it is quick or it is wrong.
            start_to_close_timeout=timedelta(seconds=30),
            # The whole open, retries included. A host at its live-Session cap REFUSES, which is
            # retryable on purpose — the task goes back to the shared queue and another host can
            # take it — so this is the bound on waiting for a busy fleet, and it fails the scope
            # rather than blocking a Run forever.
            schedule_to_close_timeout=self.open_timeout,
            summary=f"open {self.handle.name}@{self.handle.version}"
                    f"{f'[{self.key}]' if self.key else ''} {self._id}",
        )

    async def _close(self) -> None:
        """End the Session. Best-effort by design, and bounded by ScheduleToStart.

        The close runs ON the Session's own queue, because the resource lives in that one
        process. So if the host died, nothing polls that queue and the close cannot land: it is
        bounded by `close_timeout` and swallowed, because there is no resource left to leak and
        raising here would mask the failure the scope is already carrying.
        """
        from temporalio import workflow

        sid, self._id = self._id, ""
        if not sid:
            return
        try:
            # SHIELDED, because the one exit path that would otherwise skip the close is a
            # CANCELLED Run — and that is exactly when the resource is still open on a fleet
            # Machine with nobody left to notice. A cancelled workflow cancels its pending
            # tasks, so an unshielded activity started here never leaves the caller.
            await asyncio.shield(
                workflow.execute_activity(
                    CLOSE_SESSION_ACTIVITY,
                    {"session_id": sid, "key": self.key},
                    task_queue=session_queue(self.handle.name, self.handle.version, sid),
                    start_to_close_timeout=timedelta(seconds=30),
                    # The orphaned-queue bound: an unreachable host must not hold a scope open.
                    schedule_to_start_timeout=self.close_timeout,
                    retry_policy=_close_retry(),
                    summary=f"close {self.handle.name}@{self.handle.version} {sid}",
                )
            )
        except Exception as e:
            # A close that could not be delivered is a host that is already gone.
            workflow.logger.warning(
                "session close did not land", extra={"session": sid, "error": str(e)}
            )

    async def call(
        self,
        method: str,
        units: Iterable[Any] | Batch,
        dataset: Any = None,
        **options: Any,
    ) -> tuple[Batch, "Dropped"]:
        """Run one Method of this Session over one Batch and return `(results, dropped)`.

            found, _ = await crawler.crawl(batch)
            await extractor.extract(found)          # no rows in this workflow, ever

        `results` is a Batch, so it feeds the next Method directly; `units` may be a list of
        values OR a Batch another Method returned, and a Batch travels by ref and is never
        materialized here.

        `dataset` is the caller's output destination (ADR 0028 §2), the same second positional as
        on a bare handle — an open `DatasetWriter` or a `DatasetHandle`. Given one, the call
        publishes the results into it; omitted, they stay a chainable Batch.

        `dropped` is the drops (ADR 0028 §4). Nothing fails a Batch on the framework's judgement
        (§14), so the caller is the one who decides — and destructuring the tuple is what makes
        that a decision rather than a silence. `len(dropped)` costs NOTHING: `isolated` rides on
        the ref's meta, so a clean batch performs no fetch at all; `await dropped.rows()` and
        `await results.rows()` are the explicit, opt-in materializations.

        Everything `dispatch_ref` takes is forwarded, so params, an expected digest and the
        timeout are all reachable; `method` and `session_id` are this scope's to fill.
        """
        batch = await self._dispatch_batch(method, units, **options)
        if dataset is not None:
            await _publish_to(dataset, batch)
        return batch, Dropped(batch)

    async def _dispatch_one(
        self, method: str, units: Iterable[Any] | Batch, **options: Any
    ) -> Batch:
        """The routing half of a scoped Method call, named the same as `ActorHandle._dispatch_one`
        so a `MethodCall` reaches either the same way (ADR 0028 §4)."""
        return await self._dispatch_batch(method, units, **options)

    async def _dispatch_batch(
        self, method: str, units: Iterable[Any] | Batch, **options: Any
    ) -> Batch:
        """The Batch behind a scoped call — the pinning half, reached by `Session._dispatch_one`
        so a `MethodCall` made through the handle inside `async with` routes identically."""
        if not self._id:
            raise RuntimeError(
                f"{self.handle.name}.{method}() outside its scope — use `async with` (ADR 0023 §4)"
            )
        ref = await self.handle.dispatch_ref(
            units, method=method, session_id=self.session_id, **options
        )
        return Batch.from_ref(ref, actor=self.handle.name, version=self.handle.version)

    def __getattr__(self, method: str) -> Any:
        """`browser.crawl(units)` — an Actor's Methods are its attributes here, and calling one
        builds the same awaitable-and-async-iterable `MethodCall` a bare handle does (ADR 0023 §8,
        ADR 0028 §4), routed through this open scope so every chunk lands on the pinned Session.

        Only reached for names this class does not define, which is what keeps `call`, `key` and
        the dunders from being read as Method names.
        """
        if method.startswith("_"):
            raise AttributeError(method)
        return functools.partial(MethodCall, self, method)


def _close_retry():
    """A bounded retry for the close. Unbounded would hold a finished scope open against a host
    that is not coming back; one attempt would drop the resource on a single blip."""
    from temporalio.common import RetryPolicy

    return RetryPolicy(maximum_attempts=3)



async def _resolved_ref(batch: "Batch") -> Mapping[str, Any]:
    """The ref to hand the next Method — dereferenced when this Batch is another Method's output.

    WHY A CHAINED DISPATCH NEEDS THIS. An actor host with an object store configured commits each
    emitted record to its own blob and returns a list of `{"$ref": …}` entries (ADR 0007's blob
    plane — it is why a 10,000-unit result costs a history nothing). Nothing downstream
    dereferences them: `runtime/handler/workflow.go` fetches the ref and passes the list to `RunBatch`
    verbatim, so the next Method's `unit.Str("domain")` reads a `$ref` object and returns "".

    MEASURED, on four Machines: a 400-domain sweep returned `{"pairs": 623, "checked": 0,
    "dropped": 623}` — every unit of the second Method isolated as malformed, a `completed` run,
    and an empty Dataset. It cannot happen locally, because a host with no object store emits
    inline and the chain works.

    ONLY FOR A METHOD'S OUTPUT. `Batch.actor` is set by `from_ref(..., actor=)` — the Method call
    stamps it — and is empty on a Dataset page, whose units are already records, so the first
    dispatch of every loop pays nothing and only a chain pays one activity.

    The activity returns `None` when there was nothing to resolve, and this returns the original
    ref in that case: an actor deployed without an object store is a legal deployment, and its
    batches must keep working unchanged.
    """
    if not batch.actor or not (batch.ref or {}).get("sha256"):
        return batch.ref

    from temporalio import workflow

    out = await workflow.execute_activity(
        RESOLVE_BATCH_ACTIVITY,
        {"sha256": batch.ref["sha256"]},
        task_queue=DATASET_QUEUE,
        start_to_close_timeout=timedelta(minutes=10),
    )
    resolved = (out or {}).get("ref")
    return resolved if resolved else batch.ref


# ---------------------------------------------------------------------------------------------
# Datasets — the read side of "a Dataset yields Batches"
# ---------------------------------------------------------------------------------------------

#: Where the pager runs. The materializer host serves it on its own queue, so a caller's page
#: read never queues behind a long decode. Mirrors control/orchestrator/src/queues.ts.
DATASET_QUEUE = "kontra-datasets"
PAGE_DATASET_ACTIVITY = "pageDataset"
SPLIT_BATCH_ACTIVITY = "splitBatch"
RESOLVE_BATCH_ACTIVITY = "resolveBatch"
PUBLISH_BATCH_ACTIVITY = "publishBatch"
CLOSE_DATASET_ACTIVITY = "closeDataset"
DATASET_STATE_ACTIVITY = "datasetState"
#: Records a temporary Dataset's owning Run at open, before any Batch lands (temp-datasets slice
#: 01). The ONLY temp-specific write — publish and close go through the durable path unchanged.
OPEN_TEMP_ACTIVITY = "openTempDataset"
#: Promote rows out of one Dataset into a durable one (temp-datasets slice 02). Distinct from
#: publishBatch so the promoted rows keep the PRODUCING Run's provenance, not the promoter's.
PROMOTE_DATASET_ACTIVITY = "promoteDataset"
#: Write the author's tag onto this Run's Dataset RECORD (ADR 0029 §4) — the durable authority the
#: retention sweeper reads. An in-workflow `catalog.dataset(name, tag=…)` runs this FIRST, then
#: mirrors the tag to the `KontraTag` search attribute (see `_tag_run`).
TAG_DATASET_ACTIVITY = "tagDataset"

#: The Temporal search attribute an in-workflow tag MIRRORS to (ADR 0029 §4). Registered by the
#: orchestrator (`control/orchestrator/src/visibility.ts`). It is a PROJECTION over the live window, never
#: read as truth — the Dataset record is the authority.
KONTRA_TAG_ATTRIBUTE = "KontraTag"


async def _publish_batch(
    name: str, version: str, batch: "Batch", timeout: timedelta
) -> int:
    """Append one Batch's rows to the named Dataset and return how many landed — the one write
    to the lake, shared by `DatasetWriter.publish` and `DatasetHandle.publish` so the open-writer
    and bare-handle destinations a Method call accepts (ADR 0028 §2) go through byte-identical
    provenance handling.

    THE BATCH'S PROVENANCE, NEVER A SUBSTITUTE. `machine` and `version` come off the Batch, which
    is the only thing here that knows who produced the rows; an explicitly-named `version` wins
    (that partition was asked for on purpose) but an absent one stays absent rather than becoming
    the `'0'` that once read back as a version nobody deployed (see the publish activity). This
    is why a multi-Machine run names every Machine that took part: each dispatched Batch carries
    its own producer, and each publish forwards that Batch's own Machine.
    """
    if not isinstance(batch, Batch):
        raise TypeError(f"publish takes a Batch, got {type(batch).__name__}")
    if not batch.n:
        return 0

    from temporalio import workflow

    info = workflow.info()
    out = await workflow.execute_activity(
        PUBLISH_BATCH_ACTIVITY,
        {
            "dataset": name,
            "sha256": (batch.ref or {}).get("sha256", ""),
            "runId": info.workflow_id,
            "runStartedAt": int(info.start_time.timestamp() * 1000),
            "version": version or batch.version,
            "machine": batch.machine,
        },
        task_queue=DATASET_QUEUE,
        start_to_close_timeout=timeout,
    )
    return int(out.get("rows") or 0)


async def _publish_to(destination: Any, batch: "Batch") -> None:
    """Redirect a Method call's output to the caller's Dataset — the second argument of
    `ns.ask(chunk, dest)` (ADR 0028 §2), accepting either an open `DatasetWriter` or a bare
    `DatasetHandle` so a single call needs no scope. The caller still owns sealing: the actor
    is one of possibly several producers and has no idea when the Dataset is finished, so
    publishing marks it `open` and nothing here seals it.

    Type-checked rather than duck-typed because the near-miss is passing a Batch as the
    destination (a chain meant for the FIRST argument), and silently treating that as "no
    destination" would drop the rows a caller asked to name.
    """
    if isinstance(destination, (DatasetWriter, DatasetHandle)):
        await destination.publish(batch)
        return
    raise TypeError(
        f"a Method call's output destination must be a dataset writer or handle, got "
        f"{type(destination).__name__} — chain a Batch by passing it as the FIRST argument"
    )


async def _tag_run(tag: str) -> None:
    """Apply the author's tag to the CURRENT Run's Dataset — the in-workflow half of ADR 0029 §4,
    "this kind of run always matters" as a parameter on publication rather than a Temporal call.

    THE RECORD IS WRITTEN BEFORE TEMPORAL IS TOUCHED, and the order is the whole design (§4). The
    `tagDataset` activity lands the durable, authoritative record FIRST; only then is the tag
    MIRRORED to the `KontraTag` search attribute. Reversed — an `upsert_search_attributes` that
    happened to also be a tag — a mirror that failed would leave the record, which is what the
    retention sweeper reads (§5), saying untagged, and the Dataset the author asked to keep would be
    collected.

    THE MIRROR IS ONE-WAY AND IT FREEZES AT EXECUTION CLOSE. `upsert_search_attributes` is a
    workflow-internal command; there is NO API to write a search attribute on a closed execution
    (`temporal workflow update-options` is versioning-only, `temporal batch` is
    cancel/terminate/signal/reset — ADR 0029 finding 6). So a tag applied by the operator after this
    Run closes never reaches `KontraTag`, and the two disagree permanently. That is a FROZEN INDEX,
    not drift: do NOT write a reconciliation job — there is no window to reconcile into. The record
    is the truth; `KontraTag` is only a fast query path over the live window.

    Best-effort on the mirror, deliberately (the same posture the handler's own upsert takes): the
    record is already durable, so a mirror hiccup is logged and swallowed rather than failing the
    publish. A failure to write the RECORD, by contrast, propagates — the authoritative half must
    not be silently lost.
    """
    from temporalio import workflow

    info = workflow.info()
    # RECORD FIRST — the authoritative Dataset record (issue 02's store), keyed by this Run.
    await workflow.execute_activity(
        TAG_DATASET_ACTIVITY,
        {"runId": info.workflow_id, "tag": tag},
        task_queue=DATASET_QUEUE,
        start_to_close_timeout=timedelta(minutes=5),
    )
    # MIRROR SECOND — one-way, freezes at close, never read as truth. Best-effort.
    try:
        workflow.upsert_search_attributes({KONTRA_TAG_ATTRIBUTE: [tag]})
    except Exception as e:  # pragma: no cover - a mirror hiccup must not fail a written record
        workflow.logger.warning(
            "KontraTag mirror failed; the Dataset record is written and authoritative",
            extra={"tag": tag, "error": str(e)},
        )


def _source_name(source: Any) -> str:
    """The bare name of a Dataset a promotion reads FROM — a temporary one, normally, but any
    named Dataset works. Type-checked rather than duck-typed so the near-miss (passing a Batch,
    which has no name) fails at the call site instead of building SQL against `""`.

    An unopened temp is refused by name: `TempDataset.__aenter__` mints the name from the Run, so
    a temp promoted before its `async with` has none — that is a caller ordering bug, not an empty
    promotion, and it says so.
    """
    if isinstance(source, (DatasetWriter, DatasetHandle)):
        if not source.name:
            raise ValueError(
                "cannot promote from an unopened temporary Dataset — open it with `async with` "
                "before `insert_from`, so it has a name derived from its Run"
            )
        return source.name
    raise TypeError(
        f"insert_from reads from a dataset writer or handle, got {type(source).__name__}"
    )


class DatasetWriter:
    """An open Dataset you append Batches to — the write half of ADR 0023 §1 and §11.

        async with catalog.dataset("crawled").writer() as out:
            async for batch in targets.batches(200, order_by="host"):
                await out.publish(await crawler.crawl(batch))

    THREE STATES, and the vocabulary is the contract (§11): `open` while anything may still be
    appended, `sealed` when a caller declares it complete, `abandoned` when a caller gives up on
    it and says so.

    THE SCOPE SEALS, and Go's `defer w.Close()` is its exact peer — so the common path needs no
    verb you must remember. A clean exit seals; an exception on the way out marks it `abandoned`,
    which is a caller that caught its own failure. A CRASH reaches neither, so the Dataset stays
    `open`, and that is the point: a reader can tell "the producer died" from "there was nothing
    to find", which a short-but-sealed Dataset cannot express, and which is the same failure
    shape as a node that dropped every unit and still reported `completed`.

    `await out.seal()` is the explicit form, for a producer that finishes writing well before its
    scope ends — a workflow that goes on to do other work should not leave readers waiting on an
    `open` Dataset it is done with. Sealing is idempotent, and the scope's own exit will not
    re-seal after it.
    """

    def __init__(
        self, name: str, *, version: str = "", timeout: timedelta, tag: str = ""
    ) -> None:
        self.name = name
        self.version = version
        self._timeout = timeout
        self.rows = 0
        self._closed = False
        #: The author's tag (ADR 0029 §4), declared on the destination via `dataset(name, tag=…)`
        #: and applied ONCE — the first publish writes the record and mirrors to `KontraTag`, and
        #: `_tagged` keeps a streaming publish from re-doing it per chunk. Empty means untagged.
        self._tag = tag
        self._tagged = False

    async def __aenter__(self) -> "DatasetWriter":
        return self

    async def __aexit__(self, exc_type, exc, tb) -> bool:
        # `abandoned` on the way out of a failure: the caller caught its own error and is saying
        # so. Distinct from `open`, which is what a CRASH leaves behind — nobody ran this.
        await self._close("sealed" if exc_type is None else "abandoned")
        return False

    async def seal(self) -> None:
        """Declare this Dataset complete NOW, without waiting for the scope to end."""
        await self._close("sealed")

    async def abandon(self) -> None:
        """Give up on this Dataset explicitly — a decision, as opposed to the `open` a crash
        leaves behind."""
        await self._close("abandoned")

    async def _close(self, state: str) -> None:
        # Idempotent, so an explicit seal inside the scope is not undone or duplicated by the
        # exit — and so an abandon() followed by a clean exit does not silently reopen as sealed.
        if self._closed:
            return
        self._closed = True

        from temporalio import workflow

        await workflow.execute_activity(
            CLOSE_DATASET_ACTIVITY,
            {"dataset": self.name, "state": state},
            task_queue=DATASET_QUEUE,
            start_to_close_timeout=self._timeout,
        )

    async def publish(self, batch: "Batch") -> int:
        """Append one Batch. Returns the rows it added.

        The Batch's ref IS the manifest — the materializer already resolves `$ref` entries — so
        a Method's output needs no reshaping to become queryable rows, and nothing passes
        through this workflow.

        THE BATCH'S PROVENANCE IS FORWARDED, NOT THIS WRITER'S. A row's Machine and Actor
        version are facts about the Method call that produced it, and this writer knows neither
        — it used to send `self.version or "0"` and no Machine at all, which is why a
        four-Machine `nscheck` run landed 1,246 rows carrying one node and one version between
        them. An explicitly-versioned writer still wins (a caller who wrote
        `dataset(name, version=…)` asked for that partition on purpose); what it no longer does
        is substitute a placeholder for a vacancy.

        Since ADR 0028 §2 this is reached FROM the Method call itself — `ns.ask(chunk, out)`
        hands the writer as the second argument and the call publishes for you, so
        `await out.publish(...)` is no longer a line in the caller's loop. It stays public
        because a caller that holds an unnamed Batch and decides after the fact still needs it.

        THE AUTHOR'S TAG, IF ANY, IS APPLIED ONCE HERE (ADR 0029 §4). A destination named with
        `dataset(name, tag=…)` writes the durable Dataset record and mirrors to `KontraTag` on the
        first publish; `_apply_tag_once` keeps a streaming, per-chunk publish from re-paying it.
        """
        await self._apply_tag_once()
        added = await _publish_batch(self.name, self.version, batch, self._timeout)
        self.rows += added
        return added

    async def _apply_tag_once(self) -> None:
        """Write the record and mirror the tag, at most once for this writer. The flag is set only
        AFTER the record write succeeds, so a record-write failure (the authoritative half) leaves
        the writer untagged and re-raises rather than being silently swallowed; a mirror failure is
        handled inside `_tag_run` and never reaches here."""
        if not self._tag or self._tagged:
            return
        await _tag_run(self._tag)
        self._tagged = True


#: The longest a Run id may be inside a temporary Dataset's storage name. A real id is
#: `<type>-<unixseconds>` (~24 chars); this bounds the pathological one an explicit `--id` can mint,
#: so no temp can produce an object key a store refuses. Uniqueness never depended on the Run part —
#: the uuid suffix carries it — so a truncated id costs legibility and nothing else.
_SLUG_MAX = 64


def _slug(run_id: str) -> str:
    """A Run id as ONE path segment and ONE SQL identifier.

    `control/orchestrator/src/data/parquet.ts:safeName` maps everything outside `[A-Za-z0-9_.-]` to `_`
    before a name becomes a table or a key, and `datasetStateKey`/`datasetOwnerKey` do it again for
    the object store. Doing it HERE too is not redundancy: without it the name a caller reads in
    `kontra dataset ls` (this one) and the name the lake stores (the sanitized one) would be two
    different strings for one Dataset, which is exactly the class of drift a run-derived name exists
    to remove.

    A THREE-WRITER DERIVATION, PINNED BY ``shared/conformance/slug.json``. This, Go's ``tempSlug``, and
    ``safeName``. This docstring used to end "``tests/test_temp_dataset.py`` pins the pair"; there
    is no such file and there never was, so this side was asserted by nothing while the Go peer was
    asserted against a value hand-copied out of it. The corpus also records the three places
    ``safeName`` is NOT the same rule — it never truncates, its empty fallback is ``unnamed``, and
    it prefixes ``a`` to a leading digit — and why none of them is live.
    """
    out = "".join(c if (c.isascii() and (c.isalnum() or c in "_.-")) else "_" for c in run_id)
    return out[:_SLUG_MAX] or "run"


class TempDataset(DatasetWriter):
    """A temporary Dataset: a Method's output stages here before it is anybody's answer
    (temp-datasets slice 01, on ADR 0028).

        async with catalog.dataset.temp() as tmp:
            async for chunk in pairs.batches(size):
                verdicts, dropped = await ns.ask(chunk, tmp)     # streams into tmp, live
            # tmp is queryable RIGHT NOW, mid-run, and owned by this Run.
            await catalog.dataset("lame").insert_from(tmp, where="NOT ok")   # slice 02

    THREE THINGS ARE TRUE OF IT, and each is the point:

      • IT MATERIALIZES AS THE RUN PROGRESSES. A temp is NOT a new write path — it is a
        `DatasetWriter` against a differently-named Dataset, so the same per-chunk publish a
        durable Dataset uses puts rows in the lake while the Run is still going, and
        `kontra dataset query` sees them mid-run. Nothing accumulates to be flushed at seal.

      • IT IS OWNED BY ITS RUN. The owning workflow's id is recorded on the Dataset at open (the
        `openTempDataset` activity, before any Batch lands), which is what makes "whose is this,
        is anyone still using it, can it go" answerable — every lifecycle decision downstream
        reads it. A temp with no recorded owner is the bug.

      • ITS NAME IS THE FRAMEWORK'S, NOT THE CALLER'S. Derived from the owning Run at open, so the
        caller invents no unique name and — the load-bearing part — the ACTOR never needs one.
        ADR 0028's invariant is that a Method's `dataset` parameter is indistinguishable whether
        the destination is temporary or durable: the author writes `await dataset.push(x)` either
        way and cannot tell. Publishing is caller-side (the actor pushes to an unnamed sink), so a
        temp is just another destination the caller hands to a Method call — `_publish_to` accepts
        it because it IS a `DatasetWriter`, and the actor is untouched. That invariant is exactly
        why the name is the framework's to derive and not the caller's to invent.

    The `open` / `sealed` / `abandoned` lifecycle (ADR 0023 §11) is a durable Dataset's, unchanged:
    inherited straight from `DatasetWriter`, the scope seals on a clean exit, marks `abandoned` on
    an exception a caller caught, and a CRASH reaches neither so a half-filled temp stays `open` —
    "the producer died", not "there was nothing to find".
    """

    def __init__(self, *, timeout: timedelta) -> None:
        # The name is minted at __aenter__, not here: it is derived from the owning Run, and a temp
        # built at module scope has no Run yet to be owned by. Empty until then.
        super().__init__("", timeout=timeout)
        #: The owning Run's id, recorded at open. Empty before __aenter__.
        self.owner = ""

    async def __aenter__(self) -> "TempDataset":
        from temporalio import workflow

        info = workflow.info()
        # The workflow id IS the Run (ADR 0023 §12) — it survives continue-as-new, so a temp is
        # attributed to one logical Run rather than to one attempt of it.
        self.owner = info.workflow_id
        # THE NAME SAYS WHOSE IT IS. `tmp_4e9b1b23` named nothing a human could follow — the owning
        # Run was on the line above and went nowhere, so a temp in `kontra dataset ls` was eight hex
        # characters an operator had to open something else to attribute. The storage name now
        # CARRIES the Run, and keeps every property the uuid form had:
        #
        #   • REPLAY-STABLE — `workflow_id` is fixed for the Run and `workflow.uuid4()` is seeded
        #     from it (the same source Session ids come from), so a replay mints the same name and
        #     never orphans the rows a previous attempt wrote.
        #   • UNIQUE PER TEMP WITHIN A RUN — a Run may open several, so the suffix stays. It is what
        #     tells two temps of one Run apart; the Run part is what tells them from another Run's.
        #     No caller-held counter, which could not survive a re-entered scope anyway.
        #   • VISIBLY TEMPORARY — the `tmp_` prefix is untouched, so it still never reads like
        #     `lame` in a flat list. (The listing's authority on temp-ness remains the owner marker,
        #     never this prefix.)
        #   • BACKWARD-COMPATIBLE, with nothing to migrate — a temp written under the older
        #     `tmp_<uuid8>` spelling is read by exactly the same code, because NOTHING derives
        #     temp-ness or ownership from the name: the listing, the delete route and the retention
        #     sweep all read the `_owner.json` marker (`control/orchestrator/src/data/datasets.ts`), and the
        #     name is storage only. Old temps keep their names, keep their rows, keep listing as
        #     temporary, and now ALSO render a run-grain name — from that same marker.
        #   • PATH-SAFE — the name becomes a DuckLake table name and an object-store key segment,
        #     and a Run id is only `<type>-<unixseconds>` by DEFAULT: `--id` accepts anything
        #     Temporal accepts, including slashes and colons. `_slug` is what keeps a caller's id
        #     from reaching a path, and it is bounded so a pathological id cannot make a key no
        #     store will take.
        self.name = f"tmp_{_slug(self.owner)}_{workflow.uuid4().hex[:8]}"
        await workflow.execute_activity(
            OPEN_TEMP_ACTIVITY,
            {"dataset": self.name, "owner": self.owner},
            task_queue=DATASET_QUEUE,
            start_to_close_timeout=self._timeout,
        )
        return self


#: Above this many units per page, warn; above the hard cap, refuse. Measured, not guessed —
#: the same 200/1000 thresholds `kontra dispatch --batch-size` has guarded since a cascading
#: unit took a whole node with it and the run still reported `completed`. A page IS a batch, so
#: it inherits them; the safe working range is ~20-40 units per node.
SAFE_PAGE_MAX = 200
HARD_PAGE_MAX = 1000


class DatasetHandle:
    """A named Dataset. Cheap and stateless to page — nothing is read until you page it.

    Holds ONE piece of write-side state: the author's `tag` and a `_tagged` guard, so a handle
    used as a publish destination (`ns.ask(chunk, dataset("lame", tag=…))`) tags its Run's Dataset
    once rather than per chunk (ADR 0029 §4). A read-only handle leaves both untouched.
    """

    def __init__(self, name: str, *, version: str = "", dt: str = "", tag: str = "") -> None:
        self.name = name
        self.version = version
        self.dt = dt
        #: The author's tag (ADR 0029 §4), applied once on the first publish. Empty = untagged.
        self.tag = tag
        self._tagged = False

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        scope = f"@{self.version}" if self.version else ""
        return f"<kontra dataset {self.name}{scope}>"

    def writer(self, *, timeout: timedelta = timedelta(minutes=30)) -> DatasetWriter:
        """Open this Dataset for writing — the peer of Go's `w := ds.Writer(); defer w.Close()`.

            async with catalog.dataset("crawled").writer() as out:
                await out.publish(await crawler.crawl(batch))

        The scope seals on a clean exit, so the common path needs no closing verb; `out.seal()`
        is there for a producer that finishes early. See DatasetWriter. The handle's `tag` (if any)
        rides onto the writer, so the writer's first publish applies it.
        """
        return DatasetWriter(self.name, version=self.version, timeout=timeout, tag=self.tag)

    async def publish(self, batch: "Batch", *, timeout: timedelta = timedelta(minutes=30)) -> int:
        """Append one Batch's rows to this Dataset WITHOUT an open writer scope — the bare-handle
        destination a Method call accepts (ADR 0028 §2), so `ns.ask(chunk, catalog.dataset("lame"))`
        needs no `async with`. It marks the Dataset `open` and never seals it: sealing is the
        caller's, because the actor is one of possibly several producers (see `_publish_to`).

        The provenance is the Batch's, exactly as `DatasetWriter.publish` — the two share
        `_publish_batch`, so a row's Machine and Actor version do not depend on which destination
        form the caller reached for. An author's `tag` (ADR 0029 §4) is applied once here too."""
        await self._apply_tag_once()
        return await _publish_batch(self.name, self.version, batch, timeout)

    async def _apply_tag_once(self) -> None:
        """Write the record and mirror the tag, at most once for this handle — the record write
        settles before `_tagged` flips, so its failure re-raises rather than passing silently. See
        `DatasetWriter._apply_tag_once`; both keep the guard beside the state it protects."""
        if not self.tag or self._tagged:
            return
        await _tag_run(self.tag)
        self._tagged = True

    async def seal(self, *, timeout: timedelta = timedelta(minutes=5)) -> None:
        """Seal this Dataset STANDALONE, for a producer whose loop is not a `writer()` scope —
        a run that publishes from several places, or one finishing a Dataset an earlier run
        left `open`."""
        await self._close("sealed", timeout)

    async def abandon(self, *, timeout: timedelta = timedelta(minutes=5)) -> None:
        """Give up on this Dataset explicitly. Distinct from leaving it `open`, which is what a
        CRASH leaves: this says somebody decided, that says nobody finished."""
        await self._close("abandoned", timeout)

    async def _close(self, state: str, timeout: timedelta) -> None:
        from temporalio import workflow

        await workflow.execute_activity(
            CLOSE_DATASET_ACTIVITY,
            {"dataset": self.name, "state": state},
            task_queue=DATASET_QUEUE,
            start_to_close_timeout=timeout,
        )

    async def state(self, *, timeout: timedelta = timedelta(minutes=5)) -> str | None:
        """`open` / `sealed` / `abandoned`, or None if nothing ever wrote this Dataset.

        Read it before trusting a Dataset you did not produce: `open` means nobody has declared
        it complete, and paging it will read a partial answer as a whole one.
        """
        from temporalio import workflow

        out = await workflow.execute_activity(
            DATASET_STATE_ACTIVITY,
            {"dataset": self.name},
            task_queue=DATASET_QUEUE,
            start_to_close_timeout=timeout,
        )
        return out.get("state")

    async def insert_from(
        self,
        source: Any,
        *,
        where: str = "",
        query: str = "",
        timeout: timedelta = timedelta(minutes=30),
    ) -> int:
        """PROMOTE rows out of a temporary (or any) Dataset into this durable one — the act the
        whole temp-datasets feature exists for (slice 02, on ADR 0028). Returns the rows promoted.

            async with catalog.dataset.temp() as tmp:
                async for chunk in pairs.batches(size):
                    await ns.ask(chunk, tmp)            # PRODUCE — stage into the temp, live
                await catalog.dataset("lame").insert_from(tmp, where="NOT ok")   # ACCEPT

        Producing rows and accepting rows are TWO acts with a gap between them, and the gap is where
        triage fits — a workflow can park on a signal for as long as review takes, because the temp
        outlives the fleet that filled it. This is the accepting side, and it must never quietly
        re-couple: it is a separate call, not a flag on the producing one.

        THE `where=` / `query=` RULE IS THE PAGER'S. `query` is full SQL over the source by its
        bare name — "the rows the query returns ARE the rows promoted", the same rule `batches`
        and the CLI's `--query` follow — and `where` is the shorthand for the common case. Passing
        both raises rather than silently taking one. Because the source is framework-named, a
        `query` references it by `source.name`, which is known once the temp is open:
        `query=f'SELECT * FROM "{tmp.name}" WHERE verdict = ...'`.

        PROVENANCE SURVIVES. The default and the `where` shorthand are `SELECT *`, so a promoted
        row keeps the `node`, `version` and `run_id` of the **Run** that PRODUCED it — promotion
        does not stamp this workflow over the producing one, which is the bug that once landed 1,246
        rows all reading `node='w'`. This is why promotion is `promoteDataset`, not `publishBatch`.

        NOT IDEMPOTENT, by design: a second identical promotion appends the same rows again. A
        filtered INSERT…SELECT has no content address and the lake stamps no row id to dedup on, and
        refusing a re-promote would break promoting different subsets over time. Promotion is a
        deliberate act you run once; running it twice doubles.

        The destination argument on a Method call (`ns.ask(chunk, tmp)`, ADR 0028 §2) SURVIVES
        alongside this and is not replaced by it: that names where output STAGES, this names what
        gets ACCEPTED. They are the two acts this feature separates — the same word twice would be
        the rot; two acts at two times are not.
        """
        if where and query:
            raise ValueError("pass `query` OR `where`, not both — `where` is shorthand for it")
        src_name = _source_name(source)

        from temporalio import workflow

        quoted = src_name.replace('"', '""')
        sql = query or (f'SELECT * FROM "{quoted}"' + (f" WHERE {where}" if where else ""))
        out = await workflow.execute_activity(
            PROMOTE_DATASET_ACTIVITY,
            {"target": self.name, "source": src_name, "sql": sql},
            task_queue=DATASET_QUEUE,
            start_to_close_timeout=timeout,
        )
        return int(out.get("rows") or 0)

    async def batches(
        self,
        size: int,
        *,
        order_by: str,
        where: str = "",
        query: str = "",
        force_size: bool = False,
        start: int = 0,
        timeout: timedelta = timedelta(minutes=5),
    ) -> AsyncIterator[Batch]:
        """Page this Dataset into Batches — the caller's loop, one page at a time.

            async for batch in catalog.dataset("subs").batches(200, order_by="host"):
                await scope.crawl(batch)

        Each yielded Batch is a REF. No row enters your workflow, so a 40k-unit dataset costs
        your history a handful of ~110-byte refs rather than the payload (ADR 0007).

        `order_by` IS REQUIRED, and it is a correctness requirement rather than a nicety: a
        materialized dataset stamps no row id, so LIMIT/OFFSET over it has no defined row order
        and two pages may overlap or skip units with nothing raising.

        `query` is full SQL over the dataset by its bare name — "the rows the query returns ARE
        the units", the same rule the CLI's `--query` follows. `where` is the shorthand for the
        common case. Passing both is an error rather than a silent precedence.

        Iteration stops on the first SHORT page, which is a fact rather than a guess: the pager
        reads one row past the page to decide. An EMPTY FIRST page raises — a dataset that is
        gone, misspelled or filtered to nothing is a mistake, not an empty sweep — while an
        empty later page is simply the end.
        """
        if size <= 0:
            raise ValueError(f"page size must be positive, got {size}")
        if size > HARD_PAGE_MAX and not force_size:
            raise ValueError(
                f"page size {size} exceeds the safe maximum of {HARD_PAGE_MAX} units. One "
                f"cascading unit can take a whole node with it and the run still reports "
                f"`completed`. Use ~{SAFE_PAGE_MAX // 5}, or pass force_size=True if you have "
                f"a reason"
            )
        if query and where:
            raise ValueError("pass `query` OR `where`, not both — `where` is shorthand for it")

        from temporalio import workflow

        if size > SAFE_PAGE_MAX and workflow.in_workflow():
            # Guarded on in_workflow(): `workflow.logger` raises outside the workflow event loop,
            # and a line whose whole job is to warn must not itself be the failure.
            workflow.logger.warning(
                "dataset page size is above the safe range (~20-40/node)",
                extra={"dataset": self.name, "size": size},
            )

        quoted = self.name.replace('"', '""')
        sql = query or (
            f'SELECT * FROM "{quoted}"' + (f" WHERE {where}" if where else "")
        )
        scope = {"name": self.name}
        if self.version:
            scope["version"] = self.version
        if self.dt:
            scope["dt"] = self.dt

        offset = max(int(start), 0)
        first = True
        while True:
            page = await workflow.execute_activity(
                PAGE_DATASET_ACTIVITY,
                {
                    "name": self.name,
                    "sql": sql,
                    "orderBy": order_by,
                    "limit": size,
                    "offset": offset,
                    "scope": scope,
                },
                task_queue=DATASET_QUEUE,
                start_to_close_timeout=timeout,
            )
            n = int(page.get("n") or 0)
            if first and n == 0:
                raise ValueError(
                    f"dataset {self.name!r} returned no rows on its first page — check the "
                    f"name, the version/dt scope, and the filter before treating this as an "
                    f"empty sweep"
                )
            first = False
            if n:
                yield Batch.from_ref(page.get("ref") or {})
            if page.get("done", True):
                return
            offset += n


# ---------------------------------------------------------------------------------------------
# The author-facing constructors + the worker
# ---------------------------------------------------------------------------------------------


def actor(name: str, version: str = "", *, endpoint: str | None = None) -> ActorHandle:
    """A handle on a deployed actor. Build it at module scope; dispatch from your workflow."""
    return ActorHandle(name, version, endpoint=endpoint)


def _dataset(name: str, *, version: str = "", dt: str = "", tag: str = "") -> "DatasetHandle":
    """A named Dataset, by the bare name `kontra dataset list` shows.

        async for batch in catalog.dataset("subs").batches(200, order_by="host"):
            await scope.crawl(batch)

        async for batch in catalog.dataset["subs"].batches(200, order_by="host"):
            await scope.crawl(batch)      # the same call, spelled as a lookup

    `version` / `dt` prune an OUTPUT dataset's partitions, exactly as the CLI's flags do; they
    are meaningless on a standalone list.

    `tag` is AUTHORED POLICY (ADR 0029 §4) — "this kind of run always matters", e.g. a scheduled
    sweep that keeps its own output. Naming the destination with a tag makes the first publish into
    it write the Run's Dataset record and mirror to `KontraTag`. It is the author's peer of the
    operator's `kontra dataset tag`; both write the same set-valued record, which is the authority.
    """
    return DatasetHandle(name, version=version, dt=dt, tag=tag)


class _Datasets:
    """The `catalog.dataset` surface: CALL it or SUBSCRIPT it, and `.temp()` hangs off both.

        catalog.dataset("lame")     # the call form — takes version / dt / tag too
        catalog.dataset["lame"]     # the subscript form — names the Dataset, nothing else
        catalog.dataset.temp()      # a temporary Dataset, framework-named

    ONE IMPLEMENTATION, THE OTHER DELEGATING. `__getitem__` is literally `self(name)`, so the two
    spellings cannot drift into two behaviours — the hazard that makes a second way to say one
    thing worth refusing. The subscript is a peer of the call, not a replacement: the call keeps
    every keyword, and only the call can express a partition scope or an authored tag.

    WHY A SUBSCRIPT AT ALL. `catalog.dataset["lame"]` is the same shape as `crawler["acme.com"]`
    (`ActorHandle.__getitem__`), and the repo already reads that as "address the one named `…`".
    A Dataset name is an address in exactly the same sense — the bare name `kontra dataset ls`
    prints — so the two surfaces are spelled the same way. Nothing is created by either form: a
    handle is cheap and stateless, and nothing is read until it is paged.

    `catalog.dataset` was a plain function until this, with `.temp` attached to it, and a function
    cannot be subscripted — hence a small object, holding the same three members and nothing more.
    """

    __slots__ = ()

    def __call__(
        self, name: str, *, version: str = "", dt: str = "", tag: str = ""
    ) -> "DatasetHandle":
        return _dataset(name, version=version, dt=dt, tag=tag)

    def __getitem__(self, name: str) -> "DatasetHandle":
        """`catalog.dataset["lame"]` — the call form, spelled as a lookup. See {@link _Datasets}."""
        if isinstance(name, tuple):
            # `dataset["lame", "0.1.0"]` reads like a partition scope and would silently be a
            # tuple key. Refuse at the call site and name the form that DOES take a version.
            raise TypeError(
                "a Dataset is addressed by ONE name: use catalog.dataset(name, version=…, dt=…) "
                "for a partition scope"
            )
        if not isinstance(name, str):
            raise TypeError(f"dataset name must be a str, got {type(name).__name__}")
        return self(name)

    @property
    def temp(self):  # noqa: ANN201 - the callable itself, not its result
        """`catalog.dataset.temp()` — a temporary Dataset. See {@link _temp_dataset}."""
        return _temp_dataset

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        return "<kontra catalog.dataset: dataset(name) | dataset[name] | dataset.temp()>"


#: The author-facing surface. Both spellings and `.temp` come from ONE object, so
#: `catalog.dataset("lame")` and `catalog.dataset["lame"]` are the same call.
dataset = _Datasets()


def _temp_dataset(*, timeout: timedelta = timedelta(minutes=30)) -> "TempDataset":
    """Open a temporary Dataset owned by the current Run — see `TempDataset`.

        async with catalog.dataset.temp() as tmp:
            verdicts, dropped = await ns.ask(chunk, tmp)

    Spelled as a method ON `dataset` rather than a free function because a temp IS a dataset the
    caller did not name: `catalog.dataset.temp()` reads as "a dataset, the temporary kind" and
    keeps the one word (`dataset`) the model already has. The framework names it — from the Run —
    and the caller does not, which is what keeps the actor from ever learning a name (ADR 0028).
    """
    return TempDataset(timeout=timeout)


# `catalog.dataset.temp()` is the settled surface (temp-datasets PRD); `_Datasets.temp` above is
# where it now hangs, since `catalog.dataset` had to become an object for `dataset[name]` to be
# spellable at all — a function cannot be subscripted.


def serve(
    workflows: Sequence[type],
    *,
    task_queue: str = "",
    activities: Sequence[Any] = (),
    address: str = "",
    namespace: str = "",
    passthrough_modules: Sequence[str] = (),
    max_concurrent_activities: int | None = None,
) -> None:
    """Run YOUR workflows on THIS machine — the local half of the deal.

        if __name__ == "__main__":
            catalog.serve([Recon], task_queue="recon")

    Wired the one way that matters: the client carries the claim-check codec, so an actor result
    over 128 KiB decodes instead of dying on `Unknown payload encoding binary/claim-check-v1`.
    That failure only appears once a batch is big enough, which means a demo passes and a real
    run does not — so it is not left to the caller to remember.

    `activities=` registers activities in THIS process, on the same queue as your workflows:
    local glue (a DB write, a Slack post) that has no business being deployed.

    `task_queue` falls back to $KONTRA_WORKFLOW_QUEUE, which `kontra workflow serve <folder>` sets
    to the queue DERIVED from the folder's content — so the file is served on the right queue
    without editing it and without a typed queue (GitHub #15).

    AND IT REGISTERS WHAT IT IS SERVING, one descriptor per `@workflow.defn` class: the type a
    caller starts, the schemas derived from your run signature, and your class docstring's first
    paragraph (`$KONTRA_ORCHESTRATOR_URL`, best-effort — a catalog nobody can reach never stops a
    worker serving). Writing the docstring on the CLASS rather than only at the top of the file is
    what puts words on the Workflows page: a module docstring describes the file, and a file can
    hold two workflows.
    """
    from internals.temporal.wfhost import serve_workflows

    serve_workflows(
        workflows,
        task_queue=task_queue,
        activities=activities,
        address=address,
        namespace=namespace,
        passthrough_modules=passthrough_modules,
        max_concurrent_activities=max_concurrent_activities,
    )


__all__ = [
    "actor",
    "serve",
    "entry_input",
    "ActorHandle",
    "Session",
    "Batch",
    "Dropped",
    "MethodCall",
    "TempDataset",
    "shared_queue",
    "sessions_queue",
    "session_queue",
    "endpoint_name",
    "SERVICE_NAME",
    "RUN_OPERATION",
    "FETCH_BLOB_ACTIVITY",
    "OPEN_SESSION_ACTIVITY",
    "CLOSE_SESSION_ACTIVITY",
]
