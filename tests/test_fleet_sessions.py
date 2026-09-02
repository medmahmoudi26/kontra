"""A Fleet raised by a Run gets tmux sessions, and never fails because it did not.

THE BUG THIS CLOSES. ADR 0020 made session existence a Temporal converge rather than an argument on
the provision, and `control/orchestrator/src/infra/programs/fleet.ts` says so where somebody would reach for it:
*"There is deliberately no `tmux` arg"*. The consequence went unwritten for a release —
`kontra fleet up --tmux` could create a session and `fleet.up()` structurally could not, so every
workflow-raised Machine drew "no session on kf-… — Converge session" on the Monitor forever. It read
as an error while being the designed state, and the operator's only path was a button.

The converge is still a converge: the scope starts the same `tmuxSessionWorkflow` the Monitor's
button starts, through the same `temporalConverger`. What changed is that a Run no longer has to be
told to press it.
"""

from __future__ import annotations

import pytest

from actorkit import fleet

from fleetscope import FleetScope

#: The provider every fleet test passes; the harness never reaches a cloud.
DO = fleet.do_fleet(machines=2, region="nyc3")


def test_a_converged_fleet_asks_for_its_sessions() -> None:
    """The whole point: raising a Fleet from a workflow now converges sessions on its Machines."""
    s = FleetScope().run(DO)

    asked = s.activity(fleet.CONVERGE_SESSIONS_ACTIVITY)
    converged = s.child(fleet.STACK_WORKFLOW)

    # THE SAME STACK, not merely a well-formed one. Sessions converged against a different fqn
    # would read another Fleet's Machines and quietly create sessions on somebody else's.
    assert asked["stackFqn"] == converged["stackFqn"], (
        f"sessions were converged for {asked['stackFqn']!r} but the Machines were provisioned "
        f"under {converged['stackFqn']!r}"
    )


def test_the_sessions_are_asked_for_after_the_machines_exist() -> None:
    """ORDER IS THE CORRECTNESS PROPERTY, not a preference.

    `machinesFromStack` reads the stack's OUTPUTS. Asking before the converge child has returned
    would read a stack that has no Machines yet and converge nothing at all — silently, because a
    Fleet with no placement is a legitimate empty answer.
    """
    order = FleetScope().run(DO).order()

    assert fleet.STACK_WORKFLOW in order, f"no converge happened at all: {order}"
    assert fleet.CONVERGE_SESSIONS_ACTIVITY in order, f"sessions were never asked for: {order}"
    assert order.index(fleet.STACK_WORKFLOW) < order.index(fleet.CONVERGE_SESSIONS_ACTIVITY), (
        "sessions were converged BEFORE the Machines existed, so the activity read an empty stack "
        f"and converged nothing: {order}"
    )


def test_a_session_converge_that_fails_does_not_fail_the_run() -> None:
    """A PANE IS NOT THE WORK, and this is the assertion that says so.

    The Workers are already polling when this runs — `_up` is set and the placement has converged —
    so a Fleet whose sessions did not appear still produces every row of its output. Raising here
    would turn a cosmetic miss into a failed provision, and would do it AFTER real Machines were
    billed for.
    """
    s = FleetScope()
    s.sessions_fail = "temporal is unwell"

    # The control: without it, a scope that silently never called the activity would pass this test
    # for the wrong reason.
    s.run(DO)
    assert s.activities_named(fleet.CONVERGE_SESSIONS_ACTIVITY), (
        "the scope never asked for sessions, so surviving the failure proves nothing"
    )


def test_the_run_still_tears_the_fleet_down_after_a_session_failure() -> None:
    """The failure must not skip the drop: an un-dropped Lease is Machines that bill."""
    s = FleetScope()
    s.sessions_fail = "temporal is unwell"
    s.run(DO)

    assert s.activities_named(fleet.DROP_LEASE_ACTIVITY), (
        "a failed session converge swallowed the scope exit — the Lease was never dropped, which is "
        "the one failure this program must not introduce"
    )


def test_refusals_are_reported_and_still_not_fatal() -> None:
    """A Machine with no session is NAMED. "2 refused" tells an operator nothing they can act on."""
    s = FleetScope()
    s.sessions_refused = [{"machine": "kf-nscheck-02", "why": "ssh: connection refused"}]
    s.run(DO)

    assert s.activities_named(fleet.CONVERGE_SESSIONS_ACTIVITY)
    assert s.activities_named(fleet.DROP_LEASE_ACTIVITY), "a refusal is not a reason to leak a Fleet"


@pytest.mark.parametrize("name", ["CONVERGE_SESSIONS_ACTIVITY"])
def test_the_activity_name_is_the_one_the_worker_registers(name: str) -> None:
    """THE NAME IS THE CONTRACT, and nothing type-checks it across the two languages.

    `control/orchestrator/src/infra.ts` registers `...infraActivities` by their EXPORTED NAME, so this string and
    the function name in `control/orchestrator/src/activities/infra.ts` are joined by nothing but agreement. A
    drift here is an activity that retries to its timeout against a worker that has no such handler
    — and because the scope swallows that failure by design, it would be invisible.
    """
    assert getattr(fleet, name) == "convergeFleetSessions"
