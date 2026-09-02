"""The Lease a fleet scope holds (ADR 0037) — what it schedules, in what order, and what it stops.

WHAT CHANGED AND WHY IT IS DANGEROUS. Scope exit used to start a `stackWorkflow destroy` outright.
That is correct while exactly one **Run** owns a **Fleet**, and it is a way to delete another
**Run**'s **Machines** the moment a **Fleet** is shared — with no error anywhere, because the run
that lost its capacity sees a queue nobody polls and reports as slow. So the exit now DROPS A CLAIM
and the teardown moved to a **Lease** workflow that only fires at zero claims.

Every failure this file guards against is silent:

  * holding AFTER converging — a window in which another Run's exit destroys what this one just
    provisioned, and the symptom is a `ready()` that never opens on a fleet that came up perfectly;
  * a converge that fails without dropping — a Lease nobody holds and nothing collects until its
    clock runs out;
  * a shared Fleet's saga leg still armed — my failed `up` tearing down your Machines;
  * an adopted Fleet dropping its own Lease — the handoff destroying the thing it was handing off.

The server-side halves are `backend/src/workflows/lease.test.ts` (the **Lease** workflow against a real
Temporal) and `backend/src/activities/lease.test.ts` (the real activities). This file pins the
CALLER's half: the shape of the scope.
"""

from __future__ import annotations

import uuid
from datetime import timedelta

import pytest

from actorkit import fleet
from fleetscope import FleetScope

DO = fleet.do_fleet(machines=2, region="nyc3")


# ---------------------------------------------------------------------------------------------
# ORDER
# ---------------------------------------------------------------------------------------------


def test_the_lease_is_taken_before_a_single_machine_exists():
    """HOLD FIRST, CONVERGE SECOND. The other order leaves a window in which another Run's exit
    takes the last Lease and destroys the Machines this scope is in the middle of paying for."""
    scope = FleetScope().run(DO)
    order = scope.order()
    assert fleet.HOLD_LEASE_ACTIVITY in order, order
    assert "stackWorkflow" in order, order
    assert order.index(fleet.HOLD_LEASE_ACTIVITY) < order.index("stackWorkflow"), order
    # …and the drop is last of all.
    assert order[-1] == fleet.DROP_LEASE_ACTIVITY, order


def test_the_scope_starts_no_destroy_of_its_own():
    """The whole slice in one assertion. A destroy started here takes the Machines of every other
    Run holding this Fleet; the **Lease** workflow is the only thing that knows whether this was the last one."""
    scope = FleetScope().run(DO)
    assert scope.children("stackWorkflow", op="destroy") == []
    assert scope.children("stackWorkflow", op="up") != []


def test_the_lease_names_the_run_that_holds_it():
    """The holder is in the id, so the first question about a Fleet that will not die — who is
    holding it — is answerable from the **Lease** workflow alone."""
    scope = FleetScope().run(DO)
    held = scope.activity(fleet.HOLD_LEASE_ACTIVITY)
    assert held["holder"] == scope.run_id
    assert held["lease"] == fleet.lease_id(scope.run_id, scope.nonce.hex[:8])
    assert held["stackFqn"] == "kontra-fleet/nscheck-0.1.0"
    # AND THE DROP USES THE SAME STRING. A Lease held under one spelling and dropped under another
    # is one that is never dropped — this is that assertion, on this side of the boundary.
    assert scope.activity(fleet.DROP_LEASE_ACTIVITY)["lease"] == held["lease"]


def test_the_lease_id_is_unique_per_scope_not_per_run():
    """One Run may open two scopes on one Fleet — a retry, a nested `async with`. If both claimed
    the bare run id, the inner scope's exit would drop the outer scope's claim and destroy Machines
    the outer scope is still using."""
    a, b = FleetScope(), FleetScope()
    b.nonce = uuid.UUID("deadbeef-0000-4000-8000-000000000000")
    a.run(DO)
    b.run(DO)
    assert a.lease != b.lease
    assert a.lease.startswith(a.run_id) and b.lease.startswith(b.run_id)


# ---------------------------------------------------------------------------------------------
# A CONVERGE THAT DOES NOT FINISH
# ---------------------------------------------------------------------------------------------


def test_a_failed_converge_still_lets_go():
    """`__aexit__` never runs when `__aenter__` raises, so this is the only place that can drop —
    and a Lease nobody drops is a Fleet nothing collects until its clock runs out."""
    scope = FleetScope(converge_fails="creating kf-dns-03: droplet limit reached")
    scope.run(DO, expect_error=RuntimeError)
    assert scope.activities_named(fleet.DROP_LEASE_ACTIVITY), scope.summary()
    assert scope.activity(fleet.DROP_LEASE_ACTIVITY)["lease"] == scope.lease


def test_a_failed_hold_provisions_nothing():
    """THE CONTROL for the test above. If the hold fails there is no claim to drop and, far more
    importantly, no Machines to leave behind: nothing may be converged on a Fleet this scope was
    not able to claim."""
    scope = FleetScope(hold_fails="the lease **Lease** workflow is tearing the Fleet down")
    scope.run(DO, expect_error=RuntimeError)
    assert scope.children("stackWorkflow") == [], scope.summary()
    assert scope.activities_named(fleet.DROP_LEASE_ACTIVITY) == [], scope.summary()


# ---------------------------------------------------------------------------------------------
# THE SAGA LEG IS MINE ONLY WHILE THE FLEET IS
# ---------------------------------------------------------------------------------------------


def test_a_run_alone_on_its_fleet_keeps_the_saga_leg():
    """Unchanged behaviour for every caller in this repo: a cancelled provision that leaves Machines
    running is the worst outcome available, so the stack tears down what it built."""
    scope = FleetScope(leases=1).run(DO)
    assert scope.child("stackWorkflow", op="up")["compensateOnCancel"] is True


def test_a_run_sharing_a_fleet_gives_it_up():
    """AND THIS IS THE NEW FAILURE THE LEASE WOULD OTHERWISE INTRODUCE. `compensateOnCancel` tears
    down the WHOLE STACK, so a second Run's failed `up` would delete the first Run's Machines —
    inside `stackWorkflow`, where no Lease is consulted. Off when the **Lease** workflow reported a co-tenant;
    the dropped Lease collects the Fleet instead, at zero holders, which is the only condition that
    has ever been safe."""
    scope = FleetScope(leases=2).run(DO)
    assert scope.child("stackWorkflow", op="up")["compensateOnCancel"] is False
    # And the scope knows why, in a word a caller can read.
    assert scope.entered


# ---------------------------------------------------------------------------------------------
# ADOPTION
# ---------------------------------------------------------------------------------------------


def test_an_adopted_fleet_holds_a_lease_that_nobody_can_renew():
    """`destroy_on_exit=False` hands a Fleet to the next Run.

    IT STILL TAKES A LEASE, and that is not ceremony: a Fleet with no Lease at all would be
    destroyed the moment a CONCURRENT holder dropped theirs, which is the adoption failing in the
    one case it exists for.

    THE HOLDER IS EMPTY, and that is not an oversight either. Naming this Run would end the Fleet
    the moment this Run does — the **Lease** workflow would find the holder closed at the first deadline and
    tear it down, which is the opposite of adopting it. An unattributed Lease is never renewed and
    never asked about: it lives exactly one TTL, which is the handoff window.
    """
    scope = FleetScope().run(DO, destroy_on_exit=False)
    held = scope.activity(fleet.HOLD_LEASE_ACTIVITY)
    assert held["holder"] == ""
    assert held["lease"] == fleet.lease_id("", scope.nonce.hex[:8])
    # AND IT DOES NOT DROP. Dropping would destroy the Fleet this scope deliberately left standing.
    assert scope.activities_named(fleet.DROP_LEASE_ACTIVITY) == [], scope.summary()
    assert any("adopted" in msg for _, msg in scope.logs), scope.logs


def test_the_default_scope_does_drop():
    """THE CONTROL. Without it, an implementation that never dropped anything would pass the test
    above — and would leak every Fleet in the repo for a TTL apiece."""
    scope = FleetScope().run(DO, destroy_on_exit=True)
    assert scope.activities_named(fleet.DROP_LEASE_ACTIVITY), scope.summary()


# ---------------------------------------------------------------------------------------------
# THE CLOCK
# ---------------------------------------------------------------------------------------------


def test_an_unstated_ttl_sends_nothing_and_gets_the_control_planes_default():
    """An empty knob is not a value. Baking an hour in here would put a number in every caller's
    history that no operator could move — the same arrangement `credential` has, for the same
    reason."""
    scope = FleetScope().run(DO)
    assert "ttlMs" not in scope.activity(fleet.HOLD_LEASE_ACTIVITY)


def test_a_stated_ttl_crosses_as_milliseconds():
    """The unit is the failure. Seconds where milliseconds are expected is a Lease that expires a
    thousand times too soon — mid-run, taking the Machines with it — and nothing raises."""
    scope = FleetScope().run(DO, lease_ttl=timedelta(minutes=90))
    assert scope.activity(fleet.HOLD_LEASE_ACTIVITY)["ttlMs"] == 90 * 60 * 1000


@pytest.mark.parametrize("bad", [timedelta(0), timedelta(seconds=-1)])
def test_a_ttl_that_has_already_expired_is_refused_here(bad):
    """A zero or negative TTL is a Lease that lapsed before it was taken: the Fleet would be
    destroyed at the first deadline check, mid-run, by the mechanism meant to protect it."""
    with pytest.raises(ValueError, match="lease_ttl"):
        fleet.up(DO, actor="nscheck", version="0.1.0", lease_ttl=bad)


# ---------------------------------------------------------------------------------------------
# THE ROUTING LITERALS
# ---------------------------------------------------------------------------------------------


def test_the_lease_activities_ride_the_caller_queue():
    """NOT the infra queue. That worker serves one activity at a time behind sixty-minute converges,
    so a hold posted there would block a Run at the first line of its scope for an hour."""
    scope = FleetScope().run(DO)
    for call in scope.calls:
        if call.kind == "activity" and call.name in (
            fleet.HOLD_LEASE_ACTIVITY,
            fleet.DROP_LEASE_ACTIVITY,
        ):
            assert call.kwargs.get("task_queue") == fleet.CALLER_QUEUE, call


def test_the_hold_is_retried_and_the_retry_is_bounded():
    """A hold that arrives while the **Lease** workflow is tearing the Fleet down is refused on purpose, and the
    retry is what opens a fresh **Lease** workflow once it has closed — so a hold with no retry policy hangs on
    a race that resolves in seconds. An UNBOUNDED policy is the opposite failure: a Fleet that can
    never be held becomes a Run that waits for ever with nothing to read."""
    scope = FleetScope().run(DO)
    for call in scope.calls:
        if call.kind == "activity" and call.name == fleet.HOLD_LEASE_ACTIVITY:
            policy = call.kwargs.get("retry_policy")
            assert policy is not None, "the hold has no retry policy"
            assert policy.maximum_attempts and policy.maximum_attempts > 1
            assert policy.maximum_attempts < 100, "an unbounded hold hangs the Run"
