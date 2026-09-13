"""The fleet scope: the strings it routes on, and the gate it will not open early.

Two kinds of thing are tested here, for the same reason `test_workflows_client.py` exists: both
of `kontra.fleet`'s failure modes are silent.

A DRIFTED LITERAL does not raise. `INFRA_QUEUE`, `stackWorkflow`, `kontra-fleet` and the two
activity names are each written independently on this side and on the orchestrator's — the
decoupling rule — and a mismatch means a child workflow or an activity scheduled onto a queue
nobody polls. The run simply waits. So the tests below read the TypeScript peer and compare,
rather than trusting a comment to stay true.

THE READINESS GATE does not raise either, when it is wrong. `ready()` exists to stop a run
dispatching into a fleet whose Workers are not up yet, and the way that goes wrong is opening on
evidence it does not have: a failed DescribeTaskQueue reports zero pollers, and zero-because-we-
could-not-ask must never be read as zero-because-nothing-is-polling — nor, in the other
direction, may it end the wait.
"""

import asyncio
import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from kontra import fleet
from fleetscope import FleetScope

ROOT = Path(__file__).resolve().parent.parent
BACKEND = ROOT / "control" / "orchestrator" / "src"
#: The shared kernel (ADR 0041). Declarations both the orchestrator and the console read live here.
CORE = ROOT / "shared" / "core" / "src"


def _read(rel: str) -> str:
    return (BACKEND / rel).read_text(encoding="utf-8")


# ---------------------------------------------------------------------------------------------
# Identity. Every literal below reaches a queue, a workflow type or a stack project on the other
# side of a language boundary; none of them fails loudly when it is wrong.
# ---------------------------------------------------------------------------------------------


def test_the_infra_queue_matches_the_orchestrator():
    """`control/orchestrator/src/infra.ts` — the queue `stackWorkflow` is served on. A drift here is a
    child workflow that starts and is never picked up."""
    # THE DEFINITION MOVED, THE PROPERTY DID NOT. `infra.ts` used to inline the env read; since the
    # roles were merged into one process it calls `queues.ts:infraQueue()`, which is where the
    # default now lives. This test follows the definition rather than the file it used to be in —
    # a cross-language pin that greps the wrong file fails loudly, which is how this was caught,
    # but a pin that greps NOTHING would pass silently and is the outcome to avoid.
    src = _read("queues.ts")
    m = re.search(r"export const INFRA_QUEUE\s*=\s*'([^']+)'", src)
    assert m, "could not find INFRA_QUEUE in queues.ts"
    assert fleet.INFRA_QUEUE == m.group(1)


def test_the_stack_workflow_type_matches_the_orchestrator():
    """The workflow TYPE NAME, which is what Temporal dispatches on — not the file it lives in."""
    assert f"export async function {fleet.STACK_WORKFLOW}(" in _read("workflows/stack.ts")


def test_the_fleet_project_matches_the_infra_dispatch_table():
    """`infra/stacks.ts` refuses a stack outside its known projects, so an fqn built from a
    different literal is a 500 at converge time rather than a wrong fleet."""
    m = re.search(r"FLEET_PROJECT\s*=\s*'([^']+)'", _read("infra/stacks.ts"))
    assert m and fleet.FLEET_PROJECT == m.group(1)


def test_the_caller_queue_matches_the_queue_the_activities_are_registered_on():
    """The two fleet activities ride the SAME queue the caller SDK already pages Datasets on
    (see activities/fleet.ts for why they are not on the infra queue)."""
    m = re.search(r"DATASET_QUEUE\s*=\s*'([^']+)'", _read("queues.ts"))
    assert m and fleet.CALLER_QUEUE == m.group(1)

    from kontra import catalog

    # And it is literally the same constant the Dataset half uses — if these two ever diverge,
    # one of the two halves of a caller's workflow is talking to a queue nobody serves.
    assert fleet.CALLER_QUEUE == catalog.DATASET_QUEUE


def test_the_resolved_bundle_carries_the_controller_it_resolved():
    """`_converge` spreads `resolveBundle`'s result straight into the stack args, so a placement
    field missing from `ResolvedBundle` is a placement field missing from the converge.

    `controller` was missing, and it is the one the stack program refuses without: a caller that
    did not pass `controller=` provisioned Machines and failed at placement with
    `controller="" is not safe to place on a Machine`, a minute in, having already resolved a
    perfectly good Controller on the other side to build `bundleUrl` from. The same value must
    serve both — a Machine that fetches its Artifact from one Controller and registers its Worker
    with another provisions cleanly and never polls.
    """
    src = _read("activities/fleet.ts")
    body = re.search(r"export interface ResolvedBundle \{(.*?)\n\}", src, re.S)
    assert body, "ResolvedBundle is not declared where this test expects it"
    assert re.search(r"^\s*controller:\s*string;", body.group(1), re.M), (
        "ResolvedBundle does not declare `controller`; fleet.up() will fail at placement"
    )


def test_the_activity_names_are_the_exported_function_names():
    """Temporal dispatches an activity by NAME, and these are scheduled by string from here
    because the caller SDK cannot import the orchestrator's types."""
    src = _read("activities/fleet.ts")
    for name in (fleet.RESOLVE_BUNDLE_ACTIVITY, fleet.QUEUE_POLLERS_ACTIVITY):
        assert f"export async function {name}(" in src, name


def test_the_tag_pattern_matches_the_fleet_program():
    """A tag becomes a DigitalOcean tag, an inventory group and part of every machine name. The
    pattern is validated on both sides so a caller learns it is malformed in its own workflow,
    minutes before Pulumi would have said so."""
    m = re.search(r"TAG_RE\s*=\s*/([^/]+)/", _read("infra/programs/fleet.ts"))
    assert m, "could not find TAG_RE in the fleet program"
    assert fleet.TAG_RE.pattern == m.group(1)


def test_maxsessions_reaches_worker_env():
    """The gap this API forced closed. `sessions=` is decoration unless the placement writes
    KONTRA_MAX_PARALLEL_SESSIONS — the variable both hosts actually read — and it must be in the
    command's TRIGGERS too, or a converge that changes only the density is skipped and reported
    as success."""
    assert "KONTRA_MAX_PARALLEL_SESSIONS=${a.maxSessions}" in _read("infra/programs/machine.ts")
    assert "String(p.maxSessions ?? '')" in _read("infra/programs/fleet.ts")


# ---------------------------------------------------------------------------------------------
# The scope's shape. Nothing here provisions anything: `up()` is pure until it is entered.
# ---------------------------------------------------------------------------------------------


def test_up_provisions_nothing_until_the_scope_is_entered():
    f = fleet.up(tag="dns", machines=4, actor="nscheck", version="0.1.0")
    assert f.inventory == {} and f.bundle_sha == ""
    assert "not converged" in repr(f)


def test_the_fleet_is_named_after_what_it_places():
    """`<actor>-<version>`, derived, with no argument to invent and none to keep in agreement
    between the run that creates a fleet and the command that destroys it.

    The stack used to carry a caller-invented name defaulting to the role beside it — so two
    scopes collided when they happened to share a role, and never collided when they should have.
    Two runs both wanting `nscheck@0.1.0` Machines want the SAME Machines."""
    f = fleet.up(actor="nscheck", version="0.1.0", machines=1)
    assert f.name == "nscheck-0.1.0"
    assert f.fqn == "kontra-fleet/nscheck-0.1.0"
    # The tag does not enter the name — two fleets of one Artifact under different labels would
    # otherwise be two stacks fighting over the same Machines.
    assert fleet.up(actor="nscheck", version="0.1.0", machines=1, tag="dns").fqn == f.fqn
    # A different VERSION is a different fleet, because it is a different Artifact.
    assert fleet.up(actor="nscheck", version="0.2.0", machines=1).fqn != f.fqn


def test_the_tag_defaults_to_the_actor():
    """Almost every fleet wants its Machines labelled after what runs on them, and an argument
    whose only sensible value is derivable is an argument nobody should have to pass."""
    assert fleet.up(actor="nscheck", version="0.1.0", machines=1).tag == "nscheck"
    assert fleet.up(actor="nscheck", version="0.1.0", machines=1, tag="dns").tag == "dns"


def test_the_subscript_names_the_TAG_and_is_the_same_call_as_up():
    """`fleet["scanners"](...)` is `fleet.up(..., tag="scanners")` — one implementation, the other
    delegating, so the two spellings cannot come to mean two things.

    THE SUBSCRIPT NAMES THE TAG, NOT THE STACK. A fleet's name is `<actor>-<version>`, derived from
    what it places; letting a caller name it would restore the deleted form, which collided in both
    directions and was deleted. The tag is the one string about a fleet a caller owns — it becomes
    the DigitalOcean tag, the inventory group and the `kf-<tag>-NN` machine name."""
    subscripted = fleet["scanners"](actor="nscheck", version="0.1.0", machines=4)
    called = fleet.up(actor="nscheck", version="0.1.0", machines=4, tag="scanners")
    assert subscripted.__dict__ == called.__dict__

    # The STACK is untouched by the subscript: two labels of one Artifact are ONE fleet.
    assert subscripted.fqn == fleet.up(actor="nscheck", version="0.1.0", machines=4).fqn
    assert subscripted.tag == "scanners"


def test_the_subscript_form_keeps_every_refusal_the_call_form_has():
    """Validation stays in `up` — there is no second place for it to drift out of. A malformed tag
    still fails at the caller's own workflow rather than minutes into a Pulumi converge."""
    with pytest.raises(ValueError, match="invalid"):
        fleet["SCANNERS"](actor="nscheck", version="0.1.0", machines=1)
    with pytest.raises(ValueError, match="actor and version are required"):
        fleet["scanners"](actor="", version="", machines=1)
    with pytest.raises(TypeError, match="must be a str"):
        fleet[4]


def test_the_watched_queue_is_the_actors_shared_queue():
    """Not the sessions queue. The handler polls the shared one, and the handler is what
    `kontra workers list` counts."""
    from kontra import catalog

    f = fleet.up(tag="dns", machines=1, actor="nscheck", version="0.1.0")
    assert f.queue == catalog.shared_queue("nscheck", "0.1.0") == "nscheck-0.1.0"


@pytest.mark.parametrize(
    "kwargs, because",
    [
        ({"tag": "9dns"}, "must start with a letter"),
        ({"tag": "DNS"}, "lowercase only"),
        ({"tag": "d"}, "at least two characters"),
        ({"tag": "d" * 17}, "at most sixteen"),
        ({"tag": "dns_x"}, "no underscores — it becomes a tag"),
        ({"machines": -1}, "a negative fleet"),
        ({"machines": 1.5}, "not an integer"),
        ({"actor": ""}, "a fleet places one published Artifact"),
        ({"version": ""}, "a fleet places one published Artifact"),
        ({"sessions": 0}, "density cannot be zero"),
        ({"sessions": -2}, "density cannot be negative"),
    ],
)
def test_up_refuses_arguments_that_would_fail_minutes_later(kwargs, because):
    base = {"tag": "dns", "machines": 1, "actor": "nscheck", "version": "0.1.0"}
    with pytest.raises(ValueError):
        fleet.up(**{**base, **kwargs})
    assert because  # documents the case


def test_inventory_hosts_reads_private_addresses_in_machine_order():
    inv = {
        "kf-dns-02": {"name": "kf-dns-02", "host": "10.0.0.2", "publicIp": "1.1.1.2"},
        "kf-dns-01": {"name": "kf-dns-01", "host": "10.0.0.1", "publicIp": "1.1.1.1"},
    }
    assert fleet.inventory_hosts(inv) == ["10.0.0.1", "10.0.0.2"]


# ---------------------------------------------------------------------------------------------
# The readiness gate. Driven against a fake Temporal surface: `ready()` is ordinary async code
# over three workflow primitives, and the logic worth testing is which answers open the gate.
# ---------------------------------------------------------------------------------------------


class FakeClock:
    """Deterministic workflow time. Advances only when the loop sleeps, so a test's poll count is
    exactly the number of describes it expects."""

    def __init__(self) -> None:
        self.t = datetime(2026, 1, 1, tzinfo=timezone.utc)

    def now(self) -> datetime:
        return self.t

    async def sleep(self, d) -> None:
        self.t += d if isinstance(d, timedelta) else timedelta(seconds=float(d))


@pytest.fixture
def gate(monkeypatch):
    """Patch the three workflow primitives `ready()` uses and record every describe."""
    from temporalio import workflow as wf

    clock = FakeClock()
    calls: list = []

    def drive(answers):
        """What `queuePollers` returns.

        A LIST is one answer per call, the last repeating forever — the sequence a single-placement
        gate reads as "it was 1, then 3, then 4".

        A DICT is keyed by ACTOR and is stable across polls, which is the only shape that can
        express "this placement is ready and that one is not". A list cannot: with two placements
        each round consumes two entries, so the repeating tail gives BOTH queues the same answer and
        the straggler stops being a straggler on the second poll. That is not a fixture detail —
        it silently made the "names the straggler" assertion assert the wrong queue.
        """
        seq = list(answers) if not isinstance(answers, dict) else None

        async def execute_activity(name, arg=None, **kw):
            calls.append((name, arg, kw))
            if seq is None:
                return answers[(arg or {}).get("actor")]
            return seq[min(len(calls) - 1, len(seq) - 1)]

        monkeypatch.setattr(wf, "execute_activity", execute_activity)
        monkeypatch.setattr(wf, "now", clock.now)
        monkeypatch.setattr(wf, "sleep", clock.sleep)
        return calls

    drive.calls = calls
    return drive


def _fleet(machines=4):
    return fleet.up(tag="dns", machines=machines, actor="nscheck", version="0.1.0")


def ready(f, **kw):
    """Drive `ready()` to completion. `asyncio.run` rather than a pytest-asyncio marker: the
    package's dev extra is `pytest` alone, and one plugin's worth of dependency is not worth a
    coroutine three lines long."""
    return asyncio.run(f.ready(**kw))


def test_ready_opens_when_every_machine_is_polling(gate):
    calls = gate([{"pollers": 4}])
    assert ready(_fleet()) == 4
    assert len(calls) == 1, "a satisfied gate must not poll again"
    assert calls[0][0] == fleet.QUEUE_POLLERS_ACTIVITY
    assert calls[0][1] == {"actor": "nscheck", "version": "0.1.0"}
    assert calls[0][2]["task_queue"] == fleet.CALLER_QUEUE


def test_ready_waits_for_the_stragglers(gate):
    calls = gate([{"pollers": 1}, {"pollers": 3}, {"pollers": 4}])
    assert ready(_fleet(), poll=timedelta(seconds=5)) == 4
    assert len(calls) == 3


def test_a_failed_describe_never_opens_the_gate(gate):
    """THE POINT OF THE WHOLE FILE. A describe that failed reports zero pollers. Reading that as
    'nothing is polling' would be a guess; reading it as 'the fleet is ready' would be a
    catastrophe. It is neither — it is unknown, so the wait continues."""
    gate([{"pollers": 0, "error": "connection refused"}])
    with pytest.raises(fleet.FleetNotReady) as e:
        ready(_fleet(), timeout=timedelta(seconds=30), poll=timedelta(seconds=10))
    assert e.value.error == "connection refused"
    assert "connection refused" in str(e.value)


def test_an_impossible_poller_count_from_a_failed_describe_is_still_ignored(gate):
    """A describe can fail AND report a count — `panels/pollers.ts` drops a half-fold rather than
    returning a smaller one, but this side must not depend on that. An error field wins."""
    gate([{"pollers": 99, "error": "half the queue types errored"}])
    with pytest.raises(fleet.FleetNotReady) as e:
        ready(_fleet(), timeout=timedelta(seconds=1))
    assert e.value.got == 0, "a count from a failed describe is not a measurement"


def test_ready_times_out_saying_what_it_actually_saw(gate):
    """`0/4 polling` and `the describe failed` are different diagnoses with different fixes, and
    the exception has to carry which one happened."""
    gate([{"pollers": 1}])
    with pytest.raises(fleet.FleetNotReady) as e:
        ready(_fleet(), timeout=timedelta(seconds=20), poll=timedelta(seconds=10))
    assert (e.value.want, e.value.got, e.value.error) == (4, 1, "")
    assert "1/4 polling" in str(e.value)
    assert "nscheck-0.1.0" in str(e.value), "name the queue an operator has to go look at"


def test_at_least_starts_work_on_a_partial_fleet(gate):
    """There is no rolling health gate in the provision itself (ADR 0019), so 'three of four is
    enough' has to be expressible here."""
    calls = gate([{"pollers": 3}])
    assert ready(_fleet(), at_least=3) == 3
    assert len(calls) == 1


def test_a_zero_machine_fleet_is_ready_without_asking(gate):
    calls = gate([{"pollers": 0}])
    assert ready(fleet.up(tag="dns", machines=0, actor="a", version="1")) == 0
    assert calls == [], "nothing to wait for means nothing to ask"


def test_ready_can_name_the_one_placement_it_waits_for(gate):
    """`ready(actor)` waits for one placement and `ready()` for all (ADR 0037). With one placement
    the two are the same wait, which is exactly why the ARGUMENT has to be checked: a `ready()` that
    ignored its first positional would be indistinguishable from a correct one here."""
    calls = gate([{"pollers": 4}])
    assert ready(_fleet(), actor="nscheck") == 4
    assert calls[0][1] == {"actor": "nscheck", "version": "0.1.0"}


def test_ready_of_an_actor_this_fleet_does_not_place_names_what_it_does(gate):
    """Waiting on the wrong queue does not fail, it TIMES OUT — ten minutes of a run that looks
    slow, on a fleet that was ready the whole time. So the typo is refused before the first poll."""
    gate([{"pollers": 4}])
    with pytest.raises(ValueError, match="nscheck@0.1.0"):
        ready(_fleet(), actor="subfinder")
    assert gate.calls == [], "a refusal must not have asked Temporal anything"


def test_ready_refuses_a_fleet_with_nothing_placed_on_it(gate):
    """THE HOLD-WITHOUT-PLACE WINDOW, met from the other side. There is no queue to watch until a
    `place()` has made one, so this could only return 0 or wait out its whole timeout on a fleet
    that will never have a poller."""
    gate([{"pollers": 4}])
    held = fleet.hold(fleet.do_fleet(machines=4), tag="dns")
    with pytest.raises(ValueError, match="nothing is placed"):
        ready(held)
    assert gate.calls == []


def _two_placements():
    """A Fleet with two placements, staged the way `fleet.up()` stages its one.

    `place()` CAN BUILD THIS STATE SINCE PACKING (ADR 0037's other half, slice 11) — it is what two
    `f.place()` calls leave behind — and it is still staged directly here, for the reason `up()`
    stages rather than places: this file is about `ready()`, and reaching the state through a
    converge would put two child workflows and a resolver in the way of every assertion below.
    `tests/test_fleet_hold_place.py` is where the state is reached the real way.

    IT IS WRITTEN THIS WAY RATHER THAN NOT AT ALL because `ready()`'s aggregation is real logic that
    one placement cannot exercise: with one, "wait for all" and "wait for any" and "smallest" and
    "largest" are the same four answers. Two mutations survived the whole suite on exactly that —
    and a test asserting the aggregate was covered would have been the claim, not the cover.
    """
    f = fleet.hold(fleet.do_fleet(machines=4), tag="dns")
    f._placements["nscheck"] = fleet.Placement("nscheck", "0.1.0")
    f._placements["subfinder"] = fleet.Placement("subfinder", "0.2.0")
    return f


def test_ready_over_several_placements_answers_with_the_smallest_count(gate):
    """THE NUMBER A DISPATCH LOOP CAN RELY ON. `ready()` returns how many Machines are ready for ALL
    of the work, so with five polling one queue and four polling the other the answer is four —
    reporting five would tell a caller it has capacity that one of its Actors does not have."""
    calls = gate({"nscheck": {"pollers": 5}, "subfinder": {"pollers": 4}})
    assert ready(_two_placements()) == 4
    assert len(calls) == 2, "one describe per placement"
    assert [c[1]["actor"] for c in calls] == ["nscheck", "subfinder"]


def test_ready_over_several_placements_does_not_open_on_one_of_them(gate):
    """`ready()` with no argument waits for EVERY placement. Opening when any one is polling is the
    silent version of the failure the whole gate exists to prevent: the run dispatches into the
    second Actor's queue, which nobody is serving, and reports as slow."""
    gate({"nscheck": {"pollers": 4}, "subfinder": {"pollers": 1}})
    with pytest.raises(fleet.FleetNotReady) as e:
        ready(_two_placements(), timeout=timedelta(seconds=20), poll=timedelta(seconds=10))
    # AND IT NAMES THE STRAGGLER, not the healthy one — a message about the ready queue would send
    # somebody to look at the Machines that are working.
    assert e.value.got == 1
    assert "subfinder-0.2.0" in str(e.value), str(e.value)


def test_ready_waits_for_a_short_placements_own_worker_count(gate):
    """`workers=` MADE THE OLD DEFAULT WRONG, and wrong in the direction that hangs.

    `ready()` gated on the **Fleet**'s machine count. A placement with `workers=2` on a four-Machine
    Fleet has two pollers and always will, so that gate would wait out its whole timeout and then
    raise `FleetNotReady` about a Fleet that was working perfectly — the run reporting as broken
    because the number it compared against was about the wrong thing.
    """
    f = fleet.hold(fleet.do_fleet(machines=4), tag="dns")
    f._placements["subfinder"] = fleet.Placement("subfinder", "0.2.0", workers=2)
    gate([{"pollers": 2}])
    assert ready(f) == 2


def test_ready_holds_every_placement_to_its_own_number(gate):
    """A packed Fleet may carry one Artifact on every Machine beside another on two of them. One
    target for both is either a gate that never opens or one that opens early — and opening early is
    a dispatch into a queue with too few pollers, which reports as slow rather than as an error."""
    f = fleet.hold(fleet.do_fleet(machines=4), tag="dns")
    f._placements["nscheck"] = fleet.Placement("nscheck", "0.1.0")           # every Machine: 4
    f._placements["subfinder"] = fleet.Placement("subfinder", "0.2.0", workers=2)

    gate({"nscheck": {"pollers": 4}, "subfinder": {"pollers": 2}})
    assert ready(f) == 2, "both are satisfied; the answer is the smallest count seen"

    # …and the SHORT one being short is still a failure, measured against its own two rather than
    # against the Fleet's four.
    gate({"nscheck": {"pollers": 4}, "subfinder": {"pollers": 1}})
    with pytest.raises(fleet.FleetNotReady) as e:
        ready(f, timeout=timedelta(seconds=20), poll=timedelta(seconds=10))
    assert e.value.want == 2 and e.value.got == 1
    assert "subfinder-0.2.0" in str(e.value), str(e.value)


def test_the_long_placement_being_short_is_not_hidden_by_the_short_one(gate):
    """THE CASE THAT SEPARATES "EACH AGAINST ITS OWN" FROM "ALL AGAINST THE SMALLEST", and nothing
    else does.

    Four Machines: `nscheck` wants all four and has THREE; `subfinder` wants two and has two. Every
    count in sight is at least two, so a gate that compared both to the smallest target would open —
    and the run would dispatch `nscheck` work onto three pollers believing it had four. Three
    separate mutations survived the whole suite on this one shape: the comparison, the choice of
    which placement to name, and the number in the message.
    """
    f = fleet.hold(fleet.do_fleet(machines=4), tag="dns")
    f._placements["nscheck"] = fleet.Placement("nscheck", "0.1.0")           # wants 4
    f._placements["subfinder"] = fleet.Placement("subfinder", "0.2.0", workers=2)  # wants 2
    gate({"nscheck": {"pollers": 3}, "subfinder": {"pollers": 2}})

    with pytest.raises(fleet.FleetNotReady) as e:
        ready(f, timeout=timedelta(seconds=20), poll=timedelta(seconds=10))
    # THE STRAGGLER IS THE ONE FURTHEST FROM ITS OWN TARGET, not the one with the fewest pollers.
    # `subfinder` has two and `nscheck` has three, so a message chosen by raw count would send
    # somebody to the queue that is doing exactly what it was asked to.
    assert "nscheck-0.1.0" in str(e.value), str(e.value)
    assert e.value.got == 3
    # …and the number it was held to is ITS OWN four, not the Fleet's smallest target.
    assert e.value.want == 4


def test_at_least_overrides_every_placements_own_number(gate):
    """One `at_least` for the whole call, because it says how much capacity the CALLER needs — a
    statement about the run, not about an Artifact. A per-placement override would need a spelling
    nobody has asked for, and the partial-fleet case it exists for is "three of four is enough"."""
    f = fleet.hold(fleet.do_fleet(machines=4), tag="dns")
    f._placements["nscheck"] = fleet.Placement("nscheck", "0.1.0")
    f._placements["subfinder"] = fleet.Placement("subfinder", "0.2.0", workers=2)
    gate({"nscheck": {"pollers": 1}, "subfinder": {"pollers": 1}})
    assert ready(f, at_least=1) == 1


def test_a_zero_machine_hold_is_ready_before_it_asks_about_a_placement(gate):
    """ORDERING INSIDE `ready()`, and it is deliberate. A caller who asked for zero Machines — or
    for `at_least=0` — is ready by their own definition, and must not be refused for a placement
    they do not need. This is the assertion that keeps that check first."""
    gate([{"pollers": 0}])
    assert ready(fleet.hold(fleet.do_fleet(machines=0), tag="dns")) == 0
    assert ready(fleet.hold(fleet.do_fleet(machines=4), tag="dns"), at_least=0) == 0
    assert gate.calls == []


# ---------------------------------------------------------------------------------------------
# WHERE A FLEET LANDS AND WHAT IT SPENDS, EXPRESSED IN CODE (ADR 0034 §3, §4).
#
# Two failures live here and neither raises on its own. A region/VPC pairing that half-arrives
# HANGS the run rather than failing it — measured on a fresh nyc1 controller, whose first fleet
# landed in sfo3 where nothing could reach it. And a credential that reaches the wire as a VALUE
# rather than a NAME does not fail at all: it succeeds, and rides inline in workflow history in the
# clear for the namespace's whole retention, because the payload codec is a claim-check with a
# 128 KiB threshold and a cloud token is about seventy bytes.
# ---------------------------------------------------------------------------------------------


def test_the_credential_name_pattern_matches_the_secret_store():
    """`shared/core/src/secrets.ts:SECRET_NAME_RE`, character for character. The alphabet is bounded on
    both sides so `DO_TOKEN` and `do-token` cannot become two different secrets — the mistake
    somebody makes at 3am with a production credential.

    THE AUTHORITY MOVED, AND READING ITS SOURCE IS STILL THE ONLY MECHANISM HERE. It was
    `control/orchestrator/src/secrets/store.ts`; ADR 0041 moved the declaration to `@kontra/core` because the
    console had a second copy of it. THIS side cannot be fixed the same way — the SDK is Python and
    cannot import a TypeScript constant — so this really is a contract with two writers, and it is
    pinned by reading the one that decides. The console's copy is gone; this one is not a copy, it
    is another language.
    """
    m = re.search(
        r"SECRET_NAME_RE\s*=\s*/([^/]+)/", (CORE / "secrets.ts").read_text(encoding="utf-8")
    )
    assert m, "could not find SECRET_NAME_RE in shared/core/src/secrets.ts"
    assert fleet.SECRET_NAME_RE.pattern == m.group(1)


def test_the_credential_field_is_spelled_the_same_on_both_sides():
    """The name travels as `args.credential`. A drift here is not an error: the orchestrator falls
    back to its default credential name, which either works by luck or fails naming a secret the
    caller never asked for."""
    src = _read("infra/credential.ts")
    assert "coerceCredential(args?.credential" in src
    assert '"credential"' in _read("infra/stacks.ts") or "credential" in _read("infra/stacks.ts")
    assert fleet.do_fleet(machines=1, credential="do-prod").args()["credential"] == "do-prod"


def test_a_vpc_without_its_region_is_unrepresentable():
    """THE MEASURED OUTAGE, made impossible to express. A DigitalOcean VPC is REGIONAL, so region
    and VPC are one fact; the old API exposed `region=` and no VPC knob, so the one combination a
    caller could reach was a NEW region with the OLD VPC. DigitalOcean rejects that outright, and
    the run does not fail — it hangs on `ready()`, because Machines that cannot reach the
    controller's Temporal look exactly like Machines still booting."""
    with pytest.raises(ValueError, match="REGIONAL"):
        fleet.do_fleet(machines=1, vpc="482bd33f-2f05-4541-9b82-1a62022b06a0")
    # Named together, it is a fact rather than half of one.
    do = fleet.do_fleet(machines=1, region="nyc3", vpc="9b8c-uuid")
    assert do.args()["region"] == "nyc3" and do.args()["vpcUuid"] == "9b8c-uuid"
    # A region alone means that region's DEFAULT VPC, which is what somebody who set one meant —
    # so `vpcUuid` is ABSENT rather than blank. Pulumi is declarative and an empty string is a
    # request for a VPC called "".
    assert "vpcUuid" not in fleet.do_fleet(machines=1, region="nyc3").args()


def test_the_provider_config_sends_names_and_numbers_and_nothing_else():
    """Every unset knob absent, and the credential a NAME. There is no field on this object a
    token could be put in, which is the design rather than a convention."""
    do = fleet.do_fleet(
        machines=4,
        credential="do-prod",
        region="nyc3",
        vpc="9b8c-uuid",
        size="s-2vcpu-4gb",
        image="ubuntu-24-04-x64",
        ssh_key_ids=("44333901", 39835074),
    )
    assert do.args() == {
        "machines": 4,
        "credential": "do-prod",
        "region": "nyc3",
        "vpcUuid": "9b8c-uuid",
        "size": "s-2vcpu-4gb",
        "image": "ubuntu-24-04-x64",
        "sshKeyIds": ["44333901", "39835074"],
    }
    # Nothing set but the count: the controller's own defaults apply, and an empty credential means
    # "whatever THIS control plane calls its cloud credential" rather than a name baked into every
    # caller's history that a controller using `do-prod` could not honour.
    assert fleet.do_fleet(machines=1).args() == {"machines": 1}


def test_a_credential_that_is_a_value_rather_than_a_name_is_refused_here():
    """THE PASTE THAT CANNOT BE UNDONE, refused before it crosses.

    The alphabet is NOT the guard here, and finding that out is the point. A DigitalOcean token is
    `dop_v1_` followed by lowercase hex — every character of which a secret name allows — so
    `credential="dop_v1_…"` is a perfectly well-formed name and would sail into workflow history in
    the clear, where no rotation takes it back. The prefix check is what catches it, and it is a
    heuristic that cannot false-positive: nobody names a secret `dop_v1_…`.

    The alphabet still catches everything else a name cannot be."""
    for token in ["dop_v1_0123456789abcdef", "doo_v1_deadbeef", "dor_v1_cafe"]:
        with pytest.raises(ValueError, match="NAME of a secret") as e:
            fleet.do_fleet(machines=1, credential=token)
        # An error is a read path — a log line, an exception message, a failure event. It must not
        # repeat the thing it is refusing.
        assert token not in str(e.value)
    for bad in ["DO_TOKEN", "do prod", "-do-prod", "x" * 65, "do/prod"]:
        with pytest.raises(ValueError, match="not a secret name"):
            fleet.do_fleet(machines=1, credential=bad)


def test_the_provider_object_and_the_bare_knobs_are_the_same_call():
    """One implementation, the other delegating — the same bargain the `fleet[...]` subscript makes,
    so the two spellings cannot come to mean two things."""
    spelled = fleet.up(
        fleet.do_fleet(machines=4, region="nyc3", credential="do-prod"),
        actor="nscheck",
        version="0.1.0",
    )
    bare = fleet.up(
        actor="nscheck", version="0.1.0", machines=4, region="nyc3", credential="do-prod"
    )
    assert spelled.__dict__ == bare.__dict__
    assert spelled.machines == 4 and spelled.credential == "do-prod"
    # And the subscript still binds only the TAG, over either spelling.
    assert fleet["scanners"](fleet.do_fleet(machines=1), actor="nscheck", version="0.1.0").tag == "scanners"


def test_configuring_a_fleet_twice_is_a_refusal_rather_than_a_merge():
    """`fleet.up(do_fleet(region="nyc3"), region="sfo3")` has no right answer, and the wrong one
    puts Machines in a region that cannot reach the Controller — which does not fail, it hangs."""
    with pytest.raises(ValueError, match="once"):
        fleet.up(fleet.do_fleet(machines=1), actor="nscheck", version="0.1.0", region="sfo3")
    with pytest.raises(ValueError, match="once"):
        fleet.up(fleet.do_fleet(machines=1), actor="nscheck", version="0.1.0", machines=2)
    with pytest.raises(ValueError, match="once"):
        fleet.up(fleet.do_fleet(machines=1), actor="nscheck", version="0.1.0", credential="do-prod")


def test_the_credential_name_reaches_the_converge_and_the_teardown():
    """BOTH HALVES, and the teardown is the one that matters most.

    `destroy` deliberately carries no desired state — sending args is the one way to destroy the
    wrong thing — but WHICH credential to make the teardown's provider calls with is not desired
    state. Without it a fleet brought up under `do-prod` would be torn down against the control
    plane's default name, fail to resolve, and leave Droplets billing and reaching hostile
    infrastructure. That is strictly worse than a failed provision.

    WHERE THE TEARDOWN'S CREDENTIAL GOES NOW (ADR 0037): onto the HOLD, not onto a destroy this
    scope starts. The scope no longer destroys anything — it drops a **Lease**, and the **Lease** workflow tears
    the **Fleet** down when the last one goes. The **Lease** workflow outlives this **Run**, so the last holder
    out is very often not the one that provisioned: the credential has to be in the **Lease** workflow's hands
    before it is needed, which is what carrying it on the hold achieves.
    """
    scope = FleetScope().run(fleet.do_fleet(machines=2, region="nyc3", credential="do-prod"))

    up_args = scope.child("stackWorkflow")["args"]
    assert up_args["credential"] == "do-prod"
    assert up_args["region"] == "nyc3" and up_args["machines"] == 2
    # NOT the token. There is no way to put one here, and this is the assertion that says so.
    assert "value" not in up_args and "token" not in up_args

    hold = scope.activity(fleet.HOLD_LEASE_ACTIVITY)
    # The credential NAME, and nothing else about the cloud account.
    assert hold["credential"] == "do-prod"
    assert "value" not in hold and "token" not in hold

    # AND NO DESTROY FROM THIS SCOPE AT ALL. A destroy started here would take the Machines of every
    # other Run holding this Fleet — which is the failure this whole slice exists to prevent, so its
    # absence is asserted rather than assumed.
    assert scope.children("stackWorkflow", op="destroy") == []
    assert scope.activity(fleet.DROP_LEASE_ACTIVITY)["lease"] == scope.lease


def test_a_fleet_that_named_no_credential_sends_none_and_gets_the_controllers_default():
    """An empty name is not a name. Sending `credential: ""` would ask the orchestrator to resolve
    a secret called "", which is a refusal rather than a fallback."""
    f = fleet.up(actor="nscheck", version="0.1.0", machines=1)
    assert f.credential == ""
    assert "credential" not in f.provider.args()


def test_scale_is_still_required_when_no_provider_object_is_given():
    """`machines=` was a required keyword before the provider object existed and it stays required
    in effect. A zero-machine fleet built by omission opens the scope, returns an empty inventory
    and hangs the first `ready()` on Machines nobody asked for."""
    with pytest.raises(ValueError, match="machines= is required"):
        fleet.up(actor="nscheck", version="0.1.0")
    # Explicitly zero is still legal — `kontra fleet down` converges a stack to none.
    assert fleet.up(actor="nscheck", version="0.1.0", machines=0).machines == 0


def test_a_provider_object_of_the_wrong_type_is_named_as_such():
    with pytest.raises(TypeError, match="must be a DigitalOcean"):
        fleet.up({"machines": 4}, actor="nscheck", version="0.1.0")
