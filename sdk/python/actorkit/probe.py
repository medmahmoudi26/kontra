"""The Actor probe — kontra's own one-shot workflow, which calls exactly ONE Method (ADR 0033).

    (actor, version, method, units, dataset?)  →  one Method call  →  an untagged Dataset

An operator on the Actors page fills in a Batch and presses Run. Something has to schedule the
Nexus operation, and a Nexus operation is scheduled by a WORKFLOW command in every SDK we have
(ADR 0033 finding 1: `workflow.create_nexus_client` here, `workflow.NewNexusClient` in Go, and
`createNexusServiceClient` exported only from `@temporalio/workflow`). There is no activity-context
spelling anywhere, so "a dispatch activity" is not a cheaper design — it is not a design. A workflow
must exist; the only question was whose, and making the operator host a process to answer a question
about somebody else's Actor is the errand this replaces.

THE LINE, AND THE TEST THAT KEEPS IT THERE (ADR 0033 §1). The probe takes one Actor, one version,
one Method, one Batch. It refuses a second of any of them BY HAVING NOWHERE TO PUT THEM: the
request is five named fields, `ProbeRequest.of` rejects every key that is not one of them, and
`method` is a single bounded string rather than anything a second name could ride in. The reviewer's
test is a COUNT — *how many Methods can one request name?* One is a probe; two, in any spelling,
is a topology, and a server that executes a topology is the interpreter ADR 0023 §12 deleted. This
is deliberately NOT a validator over a shape that could express two: a check gets relaxed for a good
reason one day and the interpreter comes back with no commit that reads like it.

IT DISPATCHES UNKEYED, AND THAT IS A CORRECTNESS PROPERTY (ADR 0033 §2). `crawler["acme.com"]` is a
claim on a shared virtual object, and a concurrent dispatch on a held key ATTACHES to the running
execution and returns ITS results (`ActorHandle.__getitem__`). A probe is the one caller most likely
to be fired twice in ten seconds by an impatient human, and `backingWorkflowID` carries no Method to
keep two of them apart — so the second press would silently read the first probe's rows as its own.
This module therefore builds a BARE handle, never a keyed one, and never passes `idempotency_key`;
the fresh workflow id the orchestrator mints per probe is what makes the backing workflow id fall
through to a distinct one. `tests/test_actor_probe.py` asserts the empty key on the wire, because
nothing downstream could tell an attached probe from a fresh one.

IT GOES THROUGH THE PRODUCTION NEXUS PATH, not a shortcut to the Actor's queue (ADR 0033 §4). The
direct route is simpler and loses the backing workflow: the ref rehydration, the actor-id derivation
and its loud failure, the `KontraRunId`/`KontraActor` upsert without which the run is invisible,
the retry-plus-heartbeat pairing that makes resume-from-committed real, `Close` on every exit path,
and the CAS store that keeps rows out of history. Two of those fail SILENTLY when reproduced wrong.
And the reason that outlives all of them: what you debug should be what runs.

IT RUNS THE SDK'S CALLER HALF RATHER THAN A COPY OF IT — `catalog.actor(name, version).<method>(
batch, out)`, the same three lines the generated caller shown beside the Run button contains. That
is what makes the artefact honest instead of merely illustrative.

Served by ONE process, on ONE queue, serving exactly ONE workflow type (ADR 0033 §3):

    python3 -m actorkit.probe            # or: docker compose up -d orchestrator-probe

A kontra queue that served ARBITRARY caller workflows would be the general execution queue ADR 0023
§12 deleted, rebuilt under a new name. A second workflow type on this queue is a reviewable event.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass
from typing import Any, Mapping

# Temporal AT MODULE SCOPE, which `lib/actor.py` and `lib/catalog.py` deliberately avoid — the same
# exemption `lib/hitl.py` takes, and for the same two reasons. This module is only ever imported by
# the probe worker and by a test, never by `import actorkit`; and `ProbeRefused` must subclass
# `ApplicationError` at class-definition time (see its own docstring).
from temporalio import workflow
from temporalio.exceptions import ApplicationError

from actorkit import catalog

#: The kontra-owned task queue the probe worker polls. ONE workflow type lives here — see the
#: module docstring. `KONTRA_PROBE_QUEUE` moves both halves at once (the orchestrator reads the
#: same variable), for a second control plane sharing one cluster.
#:
#: THE DEFAULT IS A CROSS-LANGUAGE LITERAL, derived independently here and in
#: `backend/src/queues.ts:PROBE_QUEUE`, so `tests/test_actor_probe.py` pins the pair the way
#: `test_queue_congruence.py` pins the others. A drift is not silent here — the orchestrator
#: refuses a start when nothing polls the queue it named — but the refusal would name a queue that
#: IS being polled, under the other spelling, which is the confusing half of the same failure.
PROBE_QUEUE = os.environ.get("KONTRA_PROBE_QUEUE") or "kontra-probe"

#: The workflow type the orchestrator starts. The @workflow.defn name, spelled once here and once
#: in `queues.ts`; same pin, and a start naming a type this worker does not serve sits on the queue
#: until its own timeout rather than failing.
PROBE_WORKFLOW = "ActorProbe"

#: What a probe's Run is CALLED, when it stamps its own identity (ADR 0033 §5). The orchestrator
#: writes this through `PUT /api/runs/:runId/workflow` right after the start, so a probe's output
#: Dataset reads as a probe's rather than falling back to the Actor-grain name.
PROBE_IDENTITY = "kontra-probe"

#: The five fields a probe request has, and there is no sixth. Everything the ADR lists as refused
#: — a second Method, a second Actor, an output wired to another input, a branch, a condition, a
#: loop, a retry policy, a schedule, a fan-out width — is refused by not being in this tuple.
PROBE_FIELDS = ("actor", "version", "method", "units", "dataset")

#: A Method name, bounded the way the orchestrator's route bounds it. The Go SDK's
#: `core.Registry.AddMethod` takes any non-empty string, so `dns-facts` is a real catalogued
#: Method and a dash must be legal — what is refused is everything a SECOND name could ride in:
#: a comma, a pipe, an arrow, whitespace, a newline. That is what makes "one Method, in any
#: spelling" a property of the parse rather than of somebody's care.
_METHOD_RE = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$")

#: A Dataset name, same bound. The probe writes an ORDINARY untagged Dataset (ADR 0033 §5), so
#: this is the ordinary name rule and not a probe-specific one.
_DATASET_RE = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$")


class ProbeRefused(ApplicationError):
    """A request that is not a probe — and the sentence saying which line it crossed.

    AN `ApplicationError`, NOT A `ValueError`, AND THAT IS THE WHOLE OF WHY IT IS A CLASS HERE. An
    uncaught plain exception fails the WORKFLOW TASK in Python's SDK and retries it forever, so a
    probe request naming two Methods would not be refused at all — it would sit at `running` with
    nothing moving and no sentence anywhere, which is precisely the failure this refusal exists to
    prevent, produced by the refusal itself. `lib/hitl.py:AskExpired` is the same exemption for the
    same reason.

    NON-RETRYABLE, because a request does not become a probe by being tried again.

    Its own type because the orchestrator answers it 400 while a dispatch failure is a run that
    started and failed, and the two must not read alike: one is a request to fix, the other is a
    finding about the Actor. Raised at PARSE time, before anything is dispatched, so a refused
    request costs no Nexus call and leaves no half-run behind.
    """

    def __init__(self, message: str) -> None:
        super().__init__(message, type="ProbeRefused", non_retryable=True)


@dataclass(frozen=True)
class ProbeRequest:
    """One Actor, one version, one Method, one Batch, and where the results go.

    FROZEN, and the whole shape is here: there is no `then`, no `next`, no second `method`, no
    edge and no node list, so a topology cannot be expressed at all — which is the difference
    between a decision and a validator (ADR 0033 §1's last rejected alternative).
    """

    actor: str
    version: str
    method: str
    units: list
    #: Where the results publish. Empty means the results stay a chainable Batch and nothing is
    #: materialized — the same reading `callerFor` gives an omitted Dataset. The orchestrator
    #: always names one, so this is empty only for a caller that asked for counts alone.
    dataset: str = ""

    @classmethod
    def of(cls, request: Any) -> "ProbeRequest":
        """Parse a request, or refuse it by name.

        AN UNKNOWN KEY IS A REFUSAL, NOT A SHRUG, and that is the load-bearing half. Ignoring
        `{"method": "head", "then": {"method": "tail"}}` would run one call and hand back a result
        the caller reads as two, which is the worst of the three available outcomes — worse than
        running both, because nothing on either side ever says so. Refusing names the field.
        """
        if not isinstance(request, Mapping):
            raise ProbeRefused(
                f"a probe takes one request object with {', '.join(PROBE_FIELDS)} — got "
                f"{type(request).__name__}"
            )
        extra = [k for k in request if k not in PROBE_FIELDS]
        if extra:
            raise ProbeRefused(_extra_field_refusal(sorted(extra)))

        actor = _one_string(request.get("actor"), "actor")
        if actor == "":
            raise ProbeRefused("a probe names one Actor; this request names none")
        # A version may legitimately be empty (the DIY `<name>-shared` queue), so it is bounded
        # but not required — unlike the actor and the Method, which a probe cannot do without.
        version = _one_string(request.get("version"), "version")
        method = _one_string(request.get("method"), "method")
        if not _METHOD_RE.match(method):
            raise ProbeRefused(
                f"{method!r} is not one Method name. A probe calls exactly one Method (ADR 0033 "
                "§1); two Methods in one request — however they are spelled — is a topology, and "
                "the way to run two is to run two probes."
            )
        units = request.get("units", [])
        if units is None:
            units = []
        # A LIST, because a Batch is a list of Units. A bare dict silently wrapped would teach the
        # shape wrong on the surface whose whole job is teaching it, and `len(batch)` on a dict is
        # its number of KEYS.
        if not isinstance(units, (list, tuple)):
            raise ProbeRefused(
                f"a Batch is a list of Units; got {type(units).__name__} — wrap the Unit in a list"
            )
        dataset = _one_string(request.get("dataset"), "dataset")
        if dataset and not _DATASET_RE.match(dataset):
            raise ProbeRefused(f"{dataset!r} is not a Dataset name")
        return cls(actor=actor, version=version, method=method, units=list(units), dataset=dataset)


def _one_string(value: Any, field: str) -> str:
    """One string, or the refusal that says a list of them is a topology.

    THE LIST FORM IS THE INTERESTING FAILURE. `{"method": ["head", "tail"]}` is exactly how a
    second Method arrives when somebody is being helpful, and `str(["head","tail"])` would have
    dispatched to a Method called `['head', 'tail']` and failed at the actor's registry — a
    confusing sentence about a name nobody wrote, minutes later, instead of a refusal here.
    """
    if value is None:
        return ""
    if isinstance(value, (list, tuple, set, Mapping)):
        raise ProbeRefused(
            f"a probe names ONE {field}, not {len(value)} — one Actor, one version, one Method, "
            "one Batch (ADR 0033 §1). Run a second probe for the second."
        )
    if not isinstance(value, str):
        raise ProbeRefused(f"{field} must be a string, got {type(value).__name__}")
    return value.strip()


def _extra_field_refusal(extra: list) -> str:
    """Name the field, and say what it would have made this."""
    if "key" in extra:
        # NOT an oversight and not a field to add later. A keyed dispatch ATTACHES to a running
        # execution (ADR 0033 §2); a probe fired twice in ten seconds would read its own first
        # probe's rows as a second run's, and nothing downstream could tell those apart. Probing a
        # keyed object's durable state is a real need with its own ADR ahead of it.
        return (
            "a probe does not take a key. A keyed dispatch ATTACHES to the execution already "
            "holding that key and returns THAT batch's results (ADR 0033 §2), so two probes ten "
            "seconds apart would silently be one — the probe dispatches unkeyed, into a private "
            "anonymous Session."
        )
    return (
        f"a probe request has no {'field' if len(extra) == 1 else 'fields'} "
        f"{', '.join(repr(k) for k in extra)}. It takes {', '.join(PROBE_FIELDS)} and nothing "
        "else: one Actor, one version, one Method, one Batch (ADR 0033 §1). A second Method, a "
        "branch, a loop, a retry policy or a fan-out width would make this a topology, and a "
        "server that executes a topology is the interpreter ADR 0023 §12 removed."
    )


@workflow.defn(name=PROBE_WORKFLOW)
class ActorProbe:
    """Call one Method over one Batch, publish what came back, and return.

    This is what the Actors page's Run button starts. It composes nothing, decides no failure
    policy, owns no cursor and shards nothing — it is one dispatch a human asked for by hand, of
    exactly the kind they would otherwise have written into a file and served themselves.
    """

    @workflow.run
    async def run(self, request: Any) -> dict:
        ask = ProbeRequest.of(request)

        # A BARE HANDLE. No `[key]`, no `idempotency_key`, no Session scope — see §2 in the module
        # docstring. `workflow.info().workflow_id` becomes the dispatch's run id inside
        # `dispatch_ref`, and the orchestrator mints a fresh one per probe, so two probes of one
        # Actor get two backing workflows, two actor instances and two sets of `self.*`.
        handle = catalog.actor(ask.actor, ask.version)
        target = _method_of(handle, ask.method)

        # THE SAME TWO SPELLINGS THE GENERATED CALLER SHOWS, because it is the same code (ADR 0033
        # §3). Named: an open Dataset writer handed over as the SECOND positional argument (ADR
        # 0028 §2), sealed on a clean exit. Omitted: the results stay a chainable Batch.
        if ask.dataset:
            async with catalog.dataset(ask.dataset).writer() as out:
                results, dropped = await target(ask.units, out)
        else:
            results, dropped = await target(ask.units)

        return probe_result(ask, results, dropped)


def _method_of(handle: Any, method: str) -> Any:
    """`handle.head` / `getattr(handle, "dns-facts")` — the callable-handle path, guarded.

    `ActorHandle.__getattr__` is reached only for names ORDINARY LOOKUP does not find, so a Method
    genuinely called `session`, `key` or `dispatch_ref` would resolve to the handle's own member
    and be CALLED — `dispatch_ref(units)` would dispatch with an empty method name and reach the
    actor's registry as a different error entirely, and `handle.name(units)` would raise
    `'str' object is not callable`. Rare, and worth a sentence rather than a riddle: the same
    limitation lives in every generated caller, and the answer to it is the same one (write the
    dispatch by hand against `dispatch_ref`), so it is named here rather than discovered at the far
    end of a Nexus call.

    BOTH HALVES OF ORDINARY LOOKUP ARE CHECKED, and only checking one is the easy mistake.
    `session` and `dispatch_ref` are on the CLASS; `name`, `version`, `endpoint` and `key` are
    INSTANCE attributes set in `__init__`, so `hasattr(type(handle), "name")` is False and a Method
    called `name` would sail past a class-only guard into `'probe'(units)`. `hasattr(handle, …)`
    is not the answer either — it triggers `__getattr__`, which answers True for every legitimate
    Method name and would refuse all of them.
    """
    if method in vars(handle) or hasattr(type(handle), method):
        raise ProbeRefused(
            f"{method!r} is also the name of something on the caller's handle, so the SDK's "
            f"`handle.{method}(batch)` spelling cannot reach the Method (this is true of the "
            "generated caller too). Dispatch it by hand with `handle.dispatch_ref(units, "
            f"method={method!r})`."
        )
    return getattr(handle, method)


def probe_result(ask: ProbeRequest, results: Any, dropped: Any) -> dict:
    """What one probe answers with — and DROPPED IS NOT OPTIONAL IN IT.

    `(results, dropped)` is undestructurable-around in the SDK (ADR 0028 §4) precisely so a caller
    who does not care has to say so; a probe that reported only `results` would rebuild the failure
    mode that let a 15,814-target run report `completed` in seven minutes having scanned almost
    nothing. So `isolated` is always here, beside `results`, and `done` says whether the Method
    covered its input at all — which is how "everything was dropped" stops reading as "nothing was
    found".

    Both counts are FREE: they ride the returned Batch's ref meta, so this fetches nothing.
    """
    return {
        "actor": ask.actor,
        "version": ask.version,
        "method": ask.method,
        # What was ASKED FOR, so an empty result has a denominator on screen.
        "units": len(ask.units),
        "results": len(results),
        "isolated": len(dropped),
        "done": bool(getattr(results, "done", True)),
        # Which Machine ran the Method — stamped by the handler, empty when unrecorded, and
        # unrecorded is a real answer rather than a value to invent.
        "machine": str(getattr(results, "machine", "") or ""),
        "dataset": ask.dataset,
    }


def serve() -> None:
    """Run the probe worker: one process, one queue, one workflow type (ADR 0033 §3).

        python3 -m actorkit.probe

    `catalog.serve` is what wires the claim-check codec, the sandbox passthrough and the logging —
    the three things a hand-rolled worker gets wrong once each. It also pushes a descriptor to the
    catalog, which is right: a probe worker that is not running is the difference between a Run and
    a request that sits on a queue nobody polls, and the orchestrator refuses the start rather than
    hanging (`backend/src/probe.ts`).
    """
    catalog.serve([ActorProbe], task_queue=PROBE_QUEUE)


if __name__ == "__main__":  # pragma: no cover - the worker entry point
    serve()
