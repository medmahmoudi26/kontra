"""The Actor probe (ADR 0033) — one Method, unkeyed, and the counts that keep a drop visible.

Three properties are worth a test each, and only one of them is the happy path:

  • THE COUNT (§1). *How many Methods can one request name?* One. Two — in any spelling — is a
    topology, and a server that executes a topology is the interpreter ADR 0023 §12 deleted. The
    refusals are asserted here rather than trusted, because a request that quietly ran ONE call
    while naming two would hand back a result the caller reads as both.
  • THE MISSING KEY (§2). A keyed dispatch ATTACHES to the execution already holding that key and
    returns THAT batch's results, so an operator pressing Run twice in ten seconds would read
    their own first probe's rows as a second run's. Nothing downstream could tell those apart —
    so what is asserted is the wire: `idempotency_key` empty, on every dispatch.
  • DROPPED ≠ EMPTY (ADR 0028 §4). A Method that isolated every Unit and one that legitimately
    found nothing both return zero rows. The probe's result carries `isolated` and `done` beside
    `results`, and a probe UI drawing only `results` is the failure that let a 15,814-target run
    report `completed` in seven minutes having scanned almost nothing.

NO CLUSTER, AND THE SEAM IS AS DEEP AS IT GOES. What is substituted is the NEXUS CLIENT itself —
`workflow.create_nexus_client` — not `dispatch_ref` above it, which is what
`control/orchestrator/src/actorControl.test.ts` stubs to run the generated caller. The difference matters
for exactly one assertion in this file: `idempotency_key` is derived INSIDE `dispatch_ref`
(`idem = idempotency_key or self.key`), so stubbing that method would have tested the stub's idea
of an unkeyed dispatch rather than the SDK's. Everything above the client is the real code — the
real handle, the real `__getattr__` Method resolution, the real `entry_input`, the real
`(results, dropped)` tuple and the real Dataset writer.
"""

from __future__ import annotations

import asyncio
import datetime
import uuid
from pathlib import Path

import pytest
from temporalio import workflow as _wf

from actorkit import catalog, probe

ROOT = Path(__file__).resolve().parent.parent


# ---------------------------------------------------------------------------------------------
# The harness: the one wire seam, substituted
# ---------------------------------------------------------------------------------------------


class Wire:
    """Everything the probe put on the wire — so a test can COUNT the dispatches and read them.

    A LIST, NOT A FLAG. The count is the assertion in half these tests: "the probe called one
    Method" is not provable by a boolean that says a call happened.
    """

    def __init__(self) -> None:
        #: One entry per Nexus operation executed — the `EntryInput` the actor's op receives,
        #: with the endpoint it went to alongside.
        self.calls: list[dict] = []
        #: One entry per activity scheduled: the Dataset plane (publish, close, tag).
        self.activities: list[tuple[str, object]] = []
        #: What the handler would stamp on the returned ref's meta. A test sets these before
        #: driving the probe to say what came back.
        self.meta = {"n": "0", "isolated": "0", "done": "true", "machine": ""}

    def result_ref(self) -> dict:
        return {"sha256": "deadbeef", "size": 0, "meta": dict(self.meta)}


@pytest.fixture
def wire(monkeypatch):
    """Substitute the Nexus client, the workflow clock and the activity plane — and nothing else.

    THE CLIENT, NOT `dispatch_ref`. The key derivation, the run/node ids and the whole
    `entry_input` shape happen INSIDE `dispatch_ref`; a stub there would be asserting its own
    behaviour back at itself. Here the real method runs and hands its bytes to this fake client.
    """
    made = Wire()

    class _Client:
        def __init__(self, endpoint: str) -> None:
            self._endpoint = endpoint

        async def execute_operation(self, _op, entry, **_kw):
            made.calls.append({"endpoint": self._endpoint, **dict(entry)})
            return made.result_ref()

    async def _activity(name, arg, **_kw):
        made.activities.append((name, arg))
        if name == catalog.RESOLVE_BATCH_ACTIVITY:
            return {"ref": None}
        if name == catalog.PUBLISH_BATCH_ACTIVITY:
            return {"rows": int(made.meta["n"])}
        return {}

    class _Info:
        workflow_id = "actorprobe-1755000000"
        start_time = datetime.datetime.now(datetime.timezone.utc)

    monkeypatch.setattr(
        _wf, "create_nexus_client", lambda *, service, endpoint: _Client(endpoint)
    )
    monkeypatch.setattr(_wf, "execute_activity", _activity)
    monkeypatch.setattr(_wf, "info", lambda: _Info())
    # `dispatch_ref` mints a per-dispatch node id from the workflow's deterministic uuid source;
    # outside a workflow that raises, so it is seeded here with a counter that is stable per test.
    seq = iter(range(1, 1000))
    monkeypatch.setattr(_wf, "uuid4", lambda: uuid.UUID(int=next(seq)))
    return made


def run_probe(request: dict) -> dict:
    """Drive the workflow's `run` directly — the same call a worker makes off a workflow task."""
    return asyncio.run(probe.ActorProbe().run(request))


def ask(**over) -> dict:
    base = {
        "actor": "probe",
        "version": "0.1.0",
        "method": "head",
        "units": [{"url": "https://a.test"}, {"url": "https://b.test"}],
    }
    base.update(over)
    return base


# ---------------------------------------------------------------------------------------------
# §1 — the count
# ---------------------------------------------------------------------------------------------


def test_one_request_makes_exactly_one_method_call(wire):
    """The happy path, asserted as a COUNT rather than as a success."""
    out = run_probe(ask())

    assert len(wire.calls) == 1
    assert wire.calls[0]["method"] == "head"
    # The Actor and the version reach the wire as the ENDPOINT the operation was executed on —
    # `kontra-<name>-<version>`, which is what registration created (§4).
    assert wire.calls[0]["endpoint"] == catalog.endpoint_name("probe", "0.1.0")
    assert out["actor"] == "probe"
    assert out["version"] == "0.1.0"
    assert out["units"] == 2


@pytest.mark.parametrize(
    "request_,names",
    [
        # A second Method, spelled as a list — how one arrives when somebody is being helpful.
        (ask(method=["head", "tail"]), "ONE method"),
        # A second Method, spelled as a separator inside one string.
        (ask(method="head,tail"), "not one Method name"),
        (ask(method="head tail"), "not one Method name"),
        (ask(method="head|tail"), "not one Method name"),
        (ask(method="head->tail"), "not one Method name"),
        # A second Actor.
        (ask(actor=["probe", "beacon"]), "ONE actor"),
        (ask(version=["0.1.0", "0.2.0"]), "ONE version"),
        # A second Method under a field of its own — the shape a topology actually arrives in.
        (ask(then={"method": "tail"}), "'then'"),
        (ask(methods=["head", "tail"]), "'methods'"),
        (ask(next={"actor": "beacon", "method": "ask"}), "'next'"),
        (ask(nodes=[{"method": "head"}], edges=[]), "'edges', 'nodes'"),
        # The rest of §1's list: a branch, a loop, a retry policy, a schedule, a fan-out width.
        (ask(when="results > 0"), "'when'"),
        (ask(repeat=3), "'repeat'"),
        (ask(retry={"maximum_attempts": 5}), "'retry'"),
        (ask(schedule="0 * * * *"), "'schedule'"),
        (ask(shards=8), "'shards'"),
        # An output wired to another input.
        (ask(into={"actor": "beacon", "method": "ask"}), "'into'"),
    ],
)
def test_a_request_naming_more_than_one_call_is_refused(wire, request_, names):
    """A COUNT, and it holds however the second name is dressed.

    THE REFUSAL COMES BEFORE THE DISPATCH — asserted by the empty call list, not only by the
    exception. A probe that refused a topology *after* running its first Method would have run
    half of one, which is the outcome that reads as a success in every surface downstream.
    """
    with pytest.raises(probe.ProbeRefused) as refused:
        run_probe(request_)
    assert names in str(refused.value)
    assert wire.calls == []


def test_the_refusal_names_the_field_rather_than_dropping_it(wire):
    """Ignoring an unknown field is the worst of the three outcomes: one call runs, the caller
    reads two, and nothing on either side ever says so."""
    with pytest.raises(probe.ProbeRefused) as refused:
        run_probe(ask(then={"method": "tail"}))
    said = str(refused.value)
    assert "'then'" in said
    assert "one Actor, one version, one Method, one Batch" in said


def test_the_request_has_no_field_a_second_call_could_ride_in():
    """The structural half of §1, and the reason this is a decision rather than a validator: a
    shape that CAN express a topology eventually has its check relaxed for a good reason. This one
    has nowhere to put a second Method at all."""
    assert probe.PROBE_FIELDS == ("actor", "version", "method", "units", "dataset")
    assert set(probe.ProbeRequest.__dataclass_fields__) == set(probe.PROBE_FIELDS)


# ---------------------------------------------------------------------------------------------
# §2 — unkeyed
# ---------------------------------------------------------------------------------------------


def test_the_dispatch_carries_no_key(wire):
    """THE WIRE, not the intent. `idempotency_key` is what the handler derives the actor id from
    and what `backingWorkflowID` is named after; empty is what makes the id fall through to this
    probe's own run id, which is fresh per probe."""
    run_probe(ask())
    call = wire.calls[0]
    assert call["idempotency_key"] == ""
    # And no Session either: an anonymous, private dispatch is one load/run/close on the actor's
    # shared queue, which is what "two probes share nothing" means.
    assert call["session_id"] == ""


def test_a_key_is_refused_rather_than_quietly_accepted(wire):
    """Probing a keyed object's durable state is a real need and it is NOT satisfied by adding a
    key field — it needs the attach semantics on screen, and that is its own ADR."""
    with pytest.raises(probe.ProbeRefused) as refused:
        run_probe(ask(key="acme.com"))
    said = str(refused.value)
    assert "does not take a key" in said
    assert "ATTACHES" in said
    assert wire.calls == []


def test_two_probes_of_one_actor_get_two_backing_workflows(wire, monkeypatch):
    """The consequence §2 buys: two probes run in parallel and share nothing.

    `backingWorkflowID` is `actor-<name>-<idempotencyKey | runID-nodeID | uuid>`, so with no key
    the run id decides — and the orchestrator mints a fresh workflow id per probe. Simulated by
    driving the same request under two workflow ids, the way two presses of Run would.
    """
    seen = []
    for wfid in ("actorprobe-1755000000", "actorprobe-1755000009"):
        class _Info:
            workflow_id = wfid
            start_time = datetime.datetime.now(datetime.timezone.utc)

        monkeypatch.setattr(_wf, "info", lambda i=_Info: i())
        run_probe(ask())
        call = wire.calls[-1]
        # `dispatch_ref` defaults run_id to the workflow id and mints a fresh node id per dispatch.
        seen.append((call["run_id"], call["node_id"]))

    assert seen[0] != seen[1], "two probes ten seconds apart must not derive one backing workflow"


# ---------------------------------------------------------------------------------------------
# ADR 0028 §4 — a drop is not an empty result
# ---------------------------------------------------------------------------------------------


def test_dropped_units_are_reported_beside_the_results(wire):
    wire.meta.update({"n": "1", "isolated": "1", "done": "true"})
    out = run_probe(ask())
    assert out["results"] == 1
    assert out["isolated"] == 1
    assert out["done"] is True


def test_everything_dropped_does_not_read_as_nothing_found(wire):
    """The two shapes a probe must never render identically: 0 rows because the Method found
    nothing, and 0 rows because every Unit was isolated."""
    wire.meta.update({"n": "0", "isolated": "0", "done": "true"})
    found_nothing = run_probe(ask())

    wire.meta.update({"n": "0", "isolated": "2", "done": "false"})
    dropped_everything = run_probe(ask())

    assert found_nothing["results"] == dropped_everything["results"] == 0
    assert found_nothing["isolated"] == 0
    assert dropped_everything["isolated"] == 2
    assert found_nothing["done"] is True
    assert dropped_everything["done"] is False
    assert found_nothing != dropped_everything


def test_the_result_names_the_machine_when_the_handler_stamped_one(wire):
    wire.meta.update({"n": "2", "machine": "kf-actor-03"})
    assert run_probe(ask())["machine"] == "kf-actor-03"


def test_an_unrecorded_machine_stays_empty(wire):
    """Unrecorded is a real answer. An older actor host stamps nothing, and a plausible-looking
    default would be a claim about where this ran."""
    assert run_probe(ask())["machine"] == ""


# ---------------------------------------------------------------------------------------------
# §5 — an ordinary, untagged Dataset
# ---------------------------------------------------------------------------------------------


def test_the_results_publish_into_the_named_dataset(wire):
    wire.meta.update({"n": "2"})
    out = run_probe(ask(dataset="probe-probe-head-1755000000"))

    published = [a for a in wire.activities if a[0] == catalog.PUBLISH_BATCH_ACTIVITY]
    assert len(published) == 1
    assert published[0][1]["dataset"] == "probe-probe-head-1755000000"
    assert out["dataset"] == "probe-probe-head-1755000000"


def test_the_dataset_is_untagged_and_gets_no_probe_specific_lifetime(wire):
    """ADR 0033 §5: nothing new is built here and nothing new is ALLOWED here. Untagged means
    ADR 0029's sweep already disposes of it; a tag, a `probe` flag or a special sweep class would
    be a second lifetime for the same object."""
    run_probe(ask(dataset="probe-probe-head-1755000000"))
    names = [a[0] for a in wire.activities]
    assert catalog.TAG_DATASET_ACTIVITY not in names
    # The scope seals on a clean exit, which is the ordinary writer's contract and not a probe's.
    assert catalog.CLOSE_DATASET_ACTIVITY in names


def test_no_dataset_named_publishes_nothing(wire):
    """The counts are still the answer; the rows just stay a chainable Batch."""
    out = run_probe(ask())
    assert [a for a in wire.activities if a[0] == catalog.PUBLISH_BATCH_ACTIVITY] == []
    assert out["dataset"] == ""


def test_a_dataset_name_that_could_escape_a_name_is_refused(wire):
    with pytest.raises(probe.ProbeRefused):
        run_probe(ask(dataset="../../etc/passwd"))
    assert wire.calls == []


# ---------------------------------------------------------------------------------------------
# The Batch, and the Method name
# ---------------------------------------------------------------------------------------------


def test_the_batch_is_the_units_the_form_collected(wire):
    units = [{"url": "https://a.test", "depth": 2}, {"url": "https://b.test", "depth": 0}]
    run_probe(ask(units=units))
    assert wire.calls[0]["units"] == units


def test_a_bare_object_is_refused_rather_than_wrapped(wire):
    """A Batch is a list. Wrapping silently would teach the shape wrong, and `len(batch)` on a
    dict is its number of keys."""
    with pytest.raises(probe.ProbeRefused) as refused:
        run_probe(ask(units={"url": "https://a.test"}))
    assert "a Batch is a list of Units" in str(refused.value)


def test_an_empty_batch_is_a_batch(wire):
    out = run_probe(ask(units=[]))
    assert len(wire.calls) == 1
    assert out["units"] == 0


def test_a_method_name_python_cannot_spell_still_dispatches(wire):
    """`core.Registry.AddMethod` takes any non-empty string, so `dns-facts` is a real catalogued
    Method — and `handle.dns-facts` is a SyntaxError. `getattr` reaches it through the same
    callable-handle path, so the tuple return is identical."""
    run_probe(ask(method="dns-facts"))
    assert wire.calls[0]["method"] == "dns-facts"


def test_a_method_name_that_collides_with_the_handle_is_refused_not_misdispatched(wire):
    """`handle.dispatch_ref(units)` would dispatch with an EMPTY method name and fail at the
    actor's registry with a sentence about a name nobody wrote. The generated caller has the same
    limitation, so the probe names it here instead of at the far end of a Nexus call."""
    with pytest.raises(probe.ProbeRefused) as refused:
        run_probe(ask(method="dispatch_ref"))
    assert "dispatch_ref" in str(refused.value)
    assert wire.calls == []


def test_a_dunder_or_private_name_is_not_a_method(wire):
    with pytest.raises(probe.ProbeRefused):
        run_probe(ask(method="_dispatch_one"))
    assert wire.calls == []


# ---------------------------------------------------------------------------------------------
# §3 — one queue, one workflow type, and the orchestrator's copy of both
# ---------------------------------------------------------------------------------------------


def test_the_probe_endpoint_is_the_actors_registered_one(wire):
    """§4: the probe dispatches through `kontra.actor:run` on `kontra-<name>-<version>`, exactly as
    a caller's workflow does — not onto the Actor's queue directly."""
    run_probe(ask())
    assert wire.calls[0]["endpoint"] == catalog.endpoint_name("probe", "0.1.0")


def test_the_queue_and_the_workflow_type_match_the_orchestrators_copy():
    """The cross-language pin, in the spirit of `test_queue_congruence.py`. Both names are written
    independently in `control/orchestrator/src/queues.ts`; the orchestrator refuses a start onto a queue
    nobody polls, but under a drift that refusal names a queue that IS being polled — under the
    other spelling."""
    queues = (ROOT / "control" / "orchestrator" / "src" / "queues.ts").read_text()
    assert f"export const PROBE_QUEUE = '{probe.PROBE_QUEUE}'" in queues
    assert f"export const PROBE_WORKFLOW = '{probe.PROBE_WORKFLOW}'" in queues


def test_the_probe_worker_serves_exactly_one_workflow_type():
    """A kontra queue serving ARBITRARY caller workflows is the execution queue ADR 0023 §12
    deleted, rebuilt under a new name. One type — and a second on this queue is a reviewable
    event, which is what this assertion makes it."""
    served: list[list] = []
    original = catalog.serve
    try:
        catalog.serve = lambda workflows, **kw: served.append((list(workflows), kw))  # type: ignore[assignment]
        probe.serve()
    finally:
        catalog.serve = original  # type: ignore[assignment]

    assert len(served) == 1
    workflows, kw = served[0]
    assert workflows == [probe.ActorProbe]
    assert kw["task_queue"] == probe.PROBE_QUEUE


# ---------------------------------------------------------------------------------------------
# The refusal has to REACH somebody
# ---------------------------------------------------------------------------------------------


def test_a_refusal_fails_the_workflow_rather_than_its_task():
    """The half of a refusal that is easy to get wrong and impossible to see.

    An uncaught PLAIN exception fails the WORKFLOW TASK in Python's SDK and retries it forever, so
    a request naming two Methods would not be refused at all: the Run would sit at `running` with
    nothing moving and no sentence anywhere — which is exactly the failure the refusal exists to
    prevent, produced by the refusal itself. `sdk/python/actorkit/hitl.py:AskExpired` carries the same
    note for the same reason.

    NON-RETRYABLE, because a request does not become a probe by being tried again.
    """
    from temporalio.exceptions import ApplicationError

    refusal = probe.ProbeRefused("two Methods is a topology")
    assert isinstance(refusal, ApplicationError)
    assert refusal.non_retryable is True
    assert refusal.type == "ProbeRefused"


def test_a_method_named_like_an_INSTANCE_attribute_is_refused_too(wire):
    """The half a class-only guard misses.

    `session` and `dispatch_ref` live on `ActorHandle`; `name`, `version`, `endpoint` and `key` are
    set in `__init__`, so `hasattr(type(handle), "name")` is False. Unguarded, `handle.name(units)`
    is `'probe'(units)` — `'str' object is not callable`, raised from inside a workflow, about a
    Method the catalog is showing on the page beside it.
    """
    for colliding in ("name", "version", "endpoint", "key"):
        with pytest.raises(probe.ProbeRefused) as refused:
            run_probe(ask(method=colliding))
        assert colliding in str(refused.value)
    assert wire.calls == []
