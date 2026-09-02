"""`hold` / `place` / `ready`, and what splitting capacity from placement costs (ADR 0037).

WHAT THIS SLICE ADDS THAT CAN GO WRONG SILENTLY. `fleet.up()` sent ONE desired state and sent it
once. Two doors send two, at two different times, onto a **Fleet** that several **Runs** may hold —
and Pulumi's desired state is TOTAL, so every one of the new failures is a converge that succeeds
while removing something nobody asked it to remove:

  * a `place()` that forgot `machines` DESTROYS the Fleet. `coerceFleetArgs` reads an absent
    `machines` as `Number(undefined ?? 0)`, the program builds zero Droplets, and Pulumi deletes the
    ones that are there. The converge reports success.
  * a `hold()` on a **Fleet** somebody else has placed on omits their `bundleUrl`, which deletes
    their `command.remote.Command`, which runs `MACHINE_TEARDOWN` and stops their **Worker**. Their
    `ready()` has already passed; their next Batch waits on a queue nobody polls.
  * a second `place()` that RE-RESOLVES the Artifact swaps the sha under **Machines** that are
    already working, because a version is a mutable registry tag and somebody may have deployed in
    between. Half a **Run**'s output then came from code the other half did not use.
  * a `place()` for a SECOND Artifact that sent only its own placement deletes the first one's
    install command, which stops that **Worker** — the same silence, reached from the verb that
    exists to be safe. Every converge carries every placement, and the tests below read the first
    Actor back out of the second converge rather than trusting the comment that says so.
  * `spread=` misread hands a caller one egress address where they needed four, which is the
    `subfinder` rate-limit rule, or four where they asked for one.

And the one ADR 0037 names outright: **`place()` can fail after `hold()` succeeded**, so there is a
window in which **Machines** are up with nothing on them. `up()` cannot reach it and hold/place can,
which is the price of the split. `test_the_window_up_cannot_reach` measures exactly that difference.

The server-side halves are `control/orchestrator/src/workflows/lease.test.ts` and `control/orchestrator/src/infra/fleet.test.ts`;
the wire between the two writers of a desired state is `shared/conformance/placement.json`.
"""

from __future__ import annotations

import pytest

from actorkit import fleet
from fleetscope import FleetScope

DO = fleet.do_fleet(machines=2, region="nyc3")


def _placing(actor="nscheck", version="0.1.0", **kw):
    """A scope body that places one Artifact. `place()` is a coroutine, so the harness awaits it."""

    async def body(f):
        await f.place(actor, version, **kw)

    return body


def _child_args(scope: FleetScope) -> list[dict]:
    return [c.arg["args"] for c in scope.calls if c.kind == "child"]


# ---------------------------------------------------------------------------------------------
# ORDER AND SHAPE
# ---------------------------------------------------------------------------------------------


def test_hold_takes_the_lease_before_a_single_machine_exists():
    """The same ordering `up()` has, and for the same reason: converging before claiming leaves a
    window in which another Run's exit takes the last Lease and destroys the Machines this scope is
    in the middle of paying for."""
    scope = FleetScope().hold(DO, tag="dns")
    order = scope.order()
    assert order.index(fleet.HOLD_LEASE_ACTIVITY) < order.index("stackWorkflow"), order
    assert order[-1] == fleet.DROP_LEASE_ACTIVITY, order


def test_hold_converges_machines_and_places_nothing():
    """CAPACITY IS THE WHOLE OF IT. A hold that resolved an Artifact would have to be told which
    one, which is the argument this door exists not to take."""
    scope = FleetScope().hold(DO, tag="dns")
    assert scope.activities_named(fleet.RESOLVE_BUNDLE_ACTIVITY) == [], scope.summary()
    args = scope.child("stackWorkflow", op="up")["args"]
    assert args["machines"] == 2 and args["tag"] == "dns"
    for placement_key in ("bundleUrl", "bundleSha", "actorName", "actorVersion", "maxSessions"):
        assert placement_key not in args, placement_key


def test_place_converges_the_artifact_onto_machines_that_are_already_standing():
    """The second converge, and the whole shape of the slice in one assertion list."""
    scope = FleetScope(machines=2).hold(DO, tag="dns", body=_placing(sessions=8))
    assert scope.order() == [
        fleet.HOLD_LEASE_ACTIVITY,
        "stackWorkflow",  # capacity
        fleet.RESOLVE_BUNDLE_ACTIVITY,  # which Artifact
        "stackWorkflow",  # placement
        # SESSIONS FOLLOW THE PLACEMENT AND ONLY THE PLACEMENT. `machinesFromStack` names only
        # Machines carrying one, so converging after the CAPACITY stackWorkflow above would ask
        # about a stack with nothing on it — and a bare `hold()` therefore skips it entirely,
        # which is why this list has one session converge and not two.
        fleet.CONVERGE_SESSIONS_ACTIVITY,
        fleet.DROP_LEASE_ACTIVITY,
    ], scope.summary()
    placed = _child_args(scope)[-1]
    assert placed["bundleSha"] == "a" * 64
    assert placed["maxSessions"] == 8
    assert placed["controller"] == "10.124.0.2"


def test_every_converge_carries_the_machine_count():
    """THE ONE THAT DESTROYS A FLEET. Pulumi's desired state is total and the orchestrator coerces
    an absent `machines` to 0 (`coerceFleetArgs`), so a `place()` that sent only the placement would
    converge a zero-Droplet fleet — deleting every Machine it was placing onto, and reporting
    success. Both converges, not just the first."""
    scope = FleetScope(machines=2).hold(DO, tag="dns", body=_placing(sessions=8))
    everything = _child_args(scope)
    assert len(everything) == 2, everything
    for args in everything:
        assert args.get("machines") == 2, args
        assert args.get("tag") == "dns", args


def test_the_fleet_is_named_after_the_tag_when_it_is_capacity():
    """ADR 0037: a **Fleet** *"is not named after what it runs"*. The tag is already the
    DigitalOcean tag, the inventory group and the `kf-<tag>-NN` machine name, so `kontra-fleet/dns`
    is the stack whose Machines are `kf-dns-01` — and two Runs asking for `dns` capacity collide on
    purpose, which is what a Lease is for."""
    f = fleet.hold(DO, tag="dns")
    assert f.name == "dns"
    assert f.fqn == "kontra-fleet/dns"
    assert fleet.hold(DO, tag="crawl").fqn != f.fqn


def test_a_held_fleet_reports_nothing_placed_until_something_is():
    f = fleet.hold(DO, tag="dns")
    assert f.placements == {}
    assert f.actor == "" and f.version == "" and f.queue == "" and f.bundle_sha == ""
    assert "nothing placed" in repr(f)


# ---------------------------------------------------------------------------------------------
# THE SUGAR. `up()` is `hold` + a STAGED `place`, and the two claims worth proving are that it
# sends the same desired state and that it still costs one converge.
# ---------------------------------------------------------------------------------------------


def test_up_sends_the_same_desired_state_as_hold_plus_place():
    """The desugaring, checked rather than asserted in a comment. Same tag, same count, same
    Artifact, same density — the ONLY difference is the stack it is sent to, because the two doors
    name a Fleet differently on purpose (see `Fleet.fqn`)."""
    sugared = FleetScope(machines=2).run(DO, machines=None, sessions=8, tag="nscheck")
    spelled = FleetScope(machines=2).hold(DO, tag="nscheck", body=_placing(sessions=8))

    assert _child_args(sugared)[-1] == _child_args(spelled)[-1]
    assert sugared.child("stackWorkflow")["stackFqn"] == "kontra-fleet/nscheck-0.1.0"
    assert spelled.child("stackWorkflow")["stackFqn"] == "kontra-fleet/nscheck"


def test_up_still_costs_exactly_one_converge():
    """THE REASON THE PLACEMENT IS STAGED RATHER THAN PLACED. Desugaring `up()` into a literal
    `hold()` followed by a literal `place()` would send two converges where one used to go — a
    second child workflow, a second Pulumi run and a second wait, in the history of every workflow
    already written against this door."""
    sugared = FleetScope(machines=2).run(DO, machines=None, sessions=8)
    spelled = FleetScope(machines=2).hold(DO, tag="nscheck", body=_placing(sessions=8))
    assert len(_child_args(sugared)) == 1, sugared.summary()
    assert len(_child_args(spelled)) == 2, spelled.summary()
    # And the Artifact is resolved BEFORE the Machines exist, which is the next test's whole point.
    assert sugared.order().index(fleet.RESOLVE_BUNDLE_ACTIVITY) < sugared.order().index("stackWorkflow")


def test_the_window_up_cannot_reach():
    """ADR 0037's stated cost, MEASURED as the difference between the two doors.

    An Artifact that will not resolve — a version never published, an unsigned digest, a registry
    that refuses — costs `up()` one activity and NO Machines, because it resolves before the first
    Droplet exists. Through `hold`/`place` the same typo costs a whole provision: the Machines are
    up, they are billing, and the scope's own exit destroys them having run nothing.
    """
    sugared = FleetScope(resolve_fails="no such tag nscheck:0.1.1").run(DO, expect_error=RuntimeError)
    assert sugared.children("stackWorkflow") == [], "up() must not provision before it resolves"

    spelled = FleetScope(machines=2, resolve_fails="no such tag nscheck:0.1.1").hold(
        DO, tag="dns", body=_placing(version="0.1.1"), expect_error=fleet.PlacementFailed
    )
    assert len(spelled.children("stackWorkflow", op="up")) == 1, "the Machines were already up"
    # …and the Lease is still let go, so the Machines are collected rather than left billing.
    assert spelled.activities_named(fleet.DROP_LEASE_ACTIVITY), spelled.summary()


def test_a_failed_place_names_the_machines_it_left_standing():
    """The count IS the cost, so it is in the message. A caller reading `resolveBundle failed`
    cannot tell whether it cost an activity or four Droplets and four minutes."""
    with pytest.raises(fleet.PlacementFailed) as e:
        FleetScope(machines=2, place_fails="droplet limit reached").hold(
            DO, tag="dns", body=_placing(), expect_error=None
        )
    assert e.value.machines == 2
    assert e.value.fleet == "kontra-fleet/dns"
    assert e.value.ref == "nscheck@0.1.0"
    msg = str(e.value)
    assert "droplet limit reached" in msg, "the underlying failure must survive the wrapping"
    assert "2 Machine(s) are already up" in msg
    assert "fleet.up()" in msg, "name the door that cannot reach this state"


def test_a_place_that_failed_is_not_in_the_desired_state():
    """A rollback, and it is not tidiness: `ready()` would otherwise wait out its whole timeout on
    a placement that was never converged, and the exit's window warning would not fire."""
    scope = FleetScope(machines=2, place_fails="droplet limit reached")

    async def body(f):
        with pytest.raises(fleet.PlacementFailed):
            await f.place("nscheck", "0.1.0")
        assert f.placements == {}
        with pytest.raises(ValueError, match="nothing is placed"):
            await f.ready()

    scope.hold(DO, tag="dns", body=body)
    assert any("nothing placed" in m for _, m in scope.logs), scope.logs


def test_a_scope_that_placed_nothing_says_so_on_the_way_out():
    """Capacity bought and not used is money, so it is a line in the log. Not a raise: holding a
    Fleet open for a co-tenant is a legitimate thing to do."""
    scope = FleetScope(machines=2).hold(DO, tag="dns")
    warnings = [m for lvl, m in scope.logs if lvl == "warning"]
    assert any("nothing placed" in m for m in warnings), scope.logs


def test_up_never_logs_the_window():
    """THE CONTROL. Its placement is staged before the scope opens, so a `fleet.up()` scope that
    reached its exit necessarily placed something — an implementation that warned here would warn
    on every existing workflow in this repo."""
    scope = FleetScope(machines=2).run(DO)
    assert not any("nothing placed" in m for _, m in scope.logs), scope.logs


# ---------------------------------------------------------------------------------------------
# PLACE AGAIN IS THE SCALE OPERATION. There is no `scale()` (ADR 0037).
# ---------------------------------------------------------------------------------------------


def test_placing_the_same_actor_again_re_converges_the_new_density():
    """MID-RUN SCALING WITH NO NEW VERB. `maxSessions` is in the remote command's TRIGGERS, so this
    converge actually re-runs the install and rewrites `worker.env` — a converge that changed only
    the density and was skipped would report success and leave the old cap in place."""

    seen: list[fleet.Placement] = []

    async def body(f):
        seen.append(await f.place("nscheck", "0.1.0", sessions=8))
        seen.append(await f.place("nscheck", "0.1.0", sessions=16))

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    densities = [a.get("maxSessions") for a in _child_args(scope)]
    assert densities == [None, 8, 16], densities
    assert scope.fleet.sessions == 16
    # AND THE CALL HANDS BACK WHAT IT CONVERGED. Worth returning and worth logging: `p.ref` is the
    # `<actor>@<version>` an operator types, and the second call's answer is the one that is true.
    assert [p.sessions for p in seen] == [8, 16]
    assert seen[-1].ref == "nscheck@0.1.0"


def test_a_scale_does_not_re_resolve_the_artifact():
    """THE SILENT ONE. A version is a MUTABLE registry tag, so resolving it twice in one scope may
    legitimately answer with two different shas — somebody deployed in between. Re-resolving on a
    density change would swap the code under Machines that are already running work, mid-Run, with
    nothing reporting it."""

    async def body(f):
        await f.place("nscheck", "0.1.0", sessions=8)
        await f.place("nscheck", "0.1.0", sessions=16)

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    assert len(scope.activities_named(fleet.RESOLVE_BUNDLE_ACTIVITY)) == 1, scope.summary()
    shas = [a["bundleSha"] for a in _child_args(scope) if "bundleSha" in a]
    assert len(shas) == 2 and len(set(shas)) == 1, shas


# ---------------------------------------------------------------------------------------------
# REFUSALS. Each one is a thing that would otherwise succeed and remove something.
# ---------------------------------------------------------------------------------------------


# ---------------------------------------------------------------------------------------------
# PACKING (ADR 0037's other half). A second `place()` IS the request, and the whole desired state
# is what keeps the first one alive.
# ---------------------------------------------------------------------------------------------


def test_the_adrs_own_snippet_runs():
    """ADR 0037 §"What a caller writes" — `place("nscheck", …)` then `place("subfinder", …,
    spread=True)` on ONE four-Machine Fleet. It could not run before this slice: a fleet stack
    carried one `actorName` and the second call was a refusal.

    Written as the ADR writes it, because a snippet in an accepted record that the code refuses is
    a record that is wrong, and this is the assertion that says which one moved."""

    async def body(f):
        await f.place("nscheck", "0.1.0", sessions=8)
        await f.place("subfinder", "0.2.0", spread=True)

    scope = FleetScope(machines=4).hold(
        fleet.do_fleet(machines=4, region="nyc3"), tag="dns", body=body
    )
    assert sorted(scope.fleet.placements) == ["nscheck", "subfinder"]
    placed = _child_args(scope)[-1]["placements"]
    assert [p["actorName"] for p in placed] == ["nscheck", "subfinder"]
    # nscheck named no count, so it takes every Machine; subfinder's spread=True says the same thing
    # out loud and crosses as the NUMBER 4 — there is no boolean on this wire.
    assert placed[0].get("workers") is None
    assert placed[1]["workers"] == 4


def test_the_second_converge_still_carries_the_first_placement():
    """THE ONE THAT WOULD OTHERWISE STOP A WORKER AND REPORT SUCCESS.

    Pulumi's desired state is TOTAL. A converge naming only the Artifact being placed is a request
    to DELETE the other one's `command.remote.Command`, which runs its teardown and stops its
    Worker — no error, on either side, and the run that finds out is the one dispatching into a
    queue nobody polls. So this reads the FIRST Actor back out of the SECOND converge."""

    async def body(f):
        await f.place("nscheck", "0.1.0", sessions=8)
        await f.place("subfinder", "0.2.0")

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    everything = _child_args(scope)
    assert len(everything) == 3, scope.summary()  # capacity, nscheck, subfinder
    second = everything[-1]["placements"]
    assert [p["actorName"] for p in second] == ["nscheck", "subfinder"]
    # …and the first placement's DENSITY survived too, not just its name. A converge that carried
    # the Artifact but dropped `maxSessions` would re-run nscheck's install with the host default
    # and reset a cap the run is depending on.
    assert second[0]["maxSessions"] == 8
    assert second[0]["bundleSha"] == "a" * 64


def test_a_third_place_of_the_first_actor_still_carries_the_second():
    """SCALING ONE OF TWO. The scale operation is `place()` again, and on a packed Fleet its
    converge has to carry the co-tenant as well — otherwise raising nscheck's density would delete
    subfinder, which is the same silent stop reached from the verb that exists to be safe."""

    async def body(f):
        await f.place("nscheck", "0.1.0", sessions=8)
        await f.place("subfinder", "0.2.0")
        await f.place("nscheck", "0.1.0", sessions=16)

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    last = _child_args(scope)[-1]["placements"]
    assert [p["actorName"] for p in last] == ["nscheck", "subfinder"]
    assert last[0]["maxSessions"] == 16
    assert "maxSessions" not in last[1], "subfinder never asked for a cap"


def test_a_packed_converge_sends_no_single_placement_scalars():
    """There is no honest scalar answer for two Artifacts. A first-one-wins `actorName` riding
    beside the array would be a second desired state disagreeing with the true one — and the
    reader prefers the array, so the scalars would be a lie nothing consumes and everything
    reads."""

    async def body(f):
        await f.place("nscheck", "0.1.0")
        await f.place("subfinder", "0.2.0")

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    packed = _child_args(scope)[-1]
    for scalar in ("actorName", "actorVersion", "bundleUrl", "bundleSha", "maxSessions"):
        assert scalar not in packed, scalar
    assert packed["machines"] == 2 and packed["tag"] == "dns"


def test_one_placement_still_sends_what_it_always_sent():
    """THE CONTROL for the test above, and it is about every stack this repo has ever converged.
    A Fleet holding one Artifact sends the scalars as well as the array, so Pulumi sees no change
    nobody asked for and `kontra fleet deploy`'s spelling keeps working."""
    scope = FleetScope(machines=2).hold(DO, tag="dns", body=_placing(sessions=8))
    placed = _child_args(scope)[-1]
    assert placed["actorName"] == "nscheck" and placed["maxSessions"] == 8
    assert [p["actorName"] for p in placed["placements"]] == ["nscheck"]


def test_a_packed_fleet_refuses_to_answer_as_if_it_placed_one_thing():
    """`f.actor`, `f.queue` and `f.bundle_sha` name ONE placement, and they answered
    `next(iter(...))` when a Fleet could only have one. Returning the first now would be a lie a
    caller cannot see — a dispatch loop built on `f.queue` would watch the wrong queue — so they
    raise and name the plural forms."""

    async def body(f):
        await f.place("nscheck", "0.1.0")
        await f.place("subfinder", "0.2.0")
        for read in (lambda: f.actor, lambda: f.queue, lambda: f.bundle_sha, lambda: f.sessions):
            with pytest.raises(ValueError, match="places nscheck@0.1.0, subfinder@0.2.0"):
                read()
        # …and the plural forms answer.
        assert sorted(f.placements) == ["nscheck", "subfinder"]
        assert f.queue_for(f.placements["subfinder"]).startswith("subfinder-0.2.0")

    FleetScope(machines=2).hold(DO, tag="dns", body=body)


def test_packing_is_said_out_loud_when_it_happens():
    """Two Workers on a Machine share its egress address, and the run that discovers that from a
    rate-limited source three hours later has no line to search for. This is that line."""

    async def body(f):
        await f.place("nscheck", "0.1.0")
        await f.place("subfinder", "0.2.0")

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    assert any("packs 2 Artifacts" in m for _, m in scope.logs), scope.logs
    assert any("share its egress address" in m for _, m in scope.logs), scope.logs


def test_a_fleet_with_one_placement_does_not_claim_to_pack():
    """THE CONTROL. An implementation that logged the packing line unconditionally would put it in
    the history of every existing workflow in this repo."""
    scope = FleetScope(machines=2).hold(DO, tag="dns", body=_placing())
    assert not any("packs" in m for _, m in scope.logs), scope.logs


# ---------------------------------------------------------------------------------------------
# `workers=` AND `spread=`. One is a count; the other says something about the count and refuses
# what it cannot mean.
# ---------------------------------------------------------------------------------------------


def test_spread_true_crosses_as_the_machine_count_and_not_as_a_flag():
    """`shared/conformance/placement.json:never_boolean` — `coerceFleetArgs` narrows to strings and
    numbers, so a boolean `spread` would be DROPPED before the program saw it and one Worker per
    Machine would quietly become whatever the default was. That is how `--tmux` rode a release. So
    the flag resolves to a number on this side, and this is the assertion that says so."""
    scope = FleetScope(machines=4).hold(
        fleet.do_fleet(machines=4, region="nyc3"), tag="dns", body=_placing(spread=True)
    )
    placed = _child_args(scope)[-1]["placements"][0]
    assert placed["workers"] == 4
    assert not isinstance(placed["workers"], bool)
    assert scope.fleet.placements["nscheck"].spread is True


def test_workers_lands_the_placement_on_that_many_machines():
    scope = FleetScope(machines=4).hold(
        fleet.do_fleet(machines=4, region="nyc3"), tag="dns", body=_placing(workers=2)
    )
    assert _child_args(scope)[-1]["placements"][0]["workers"] == 2
    assert scope.fleet.placements["nscheck"].workers == 2


def test_a_placement_that_named_no_count_sends_none():
    """ABSENT MEANS EVERY MACHINE, which is what a Fleet has always done — so the wire of a
    placement nobody said anything about is byte-identical to the pre-packing one. A writer that
    always sent `workers` would put a new key in every existing caller's converge."""
    scope = FleetScope(machines=2).hold(DO, tag="dns", body=_placing())
    assert "workers" not in _child_args(scope)[-1]["placements"][0]


@pytest.mark.parametrize("bad", [0, -2, True, 1.5, "2"])
def test_workers_refuses_a_count_that_is_not_one(bad):
    import asyncio

    f = fleet.hold(DO, tag="dns")
    f._lease = "run#nonce"
    with pytest.raises(ValueError, match="workers"):
        asyncio.run(f.place("nscheck", "0.1.0", workers=bad))


def test_more_workers_than_machines_is_refused_naming_the_density_knob():
    """A placement puts at most ONE Worker on a Machine — two of one `<actor>@<version>` there
    would share a `KONTRA_WORKER` label, a unit name and a queue, and `cli/warden.go:reconcile`
    already refuses the duplicate out loud. Truncating would leave a caller believing in four
    Workers that were never built, permanently, because the converge succeeds."""
    import asyncio

    f = fleet.hold(DO, tag="dns")  # two Machines
    f._lease = "run#nonce"
    with pytest.raises(ValueError, match="workers=8") as e:
        asyncio.run(f.place("nscheck", "0.1.0", workers=8))
    assert "sessions=" in str(e.value), "name the argument that DOES mean more concurrency"


def test_spread_true_beside_workers_is_refused():
    """Two ways to say one number, and both resolutions are silent: honouring `workers=2` hands a
    caller two source addresses where they asked for one per Machine, and honouring `spread`
    ignores an argument they typed."""
    import asyncio

    f = fleet.hold(DO, tag="dns")
    f._lease = "run#nonce"
    with pytest.raises(ValueError, match="two ways to say one number"):
        asyncio.run(f.place("nscheck", "0.1.0", spread=True, workers=2))


def test_spread_false_alone_is_refused_and_beside_workers_is_not():
    """`spread=False` is the explicit spelling of "this placement does not need every Machine",
    which is `workers=`. On its own it asks for nothing and changes nothing, and accepting an
    argument that does nothing is how `--tmux` rode a whole release."""

    async def body(f):
        with pytest.raises(ValueError, match="asks for nothing"):
            await f.place("nscheck", "0.1.0", spread=False)
        assert f.placements == {}, "a refused placement is not a placement"
        await f.place("nscheck", "0.1.0", spread=False, workers=1)

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    assert scope.fleet.placements["nscheck"].workers == 1
    assert len(_child_args(scope)) == 2, "only the honoured placement converged"


# ---------------------------------------------------------------------------------------------
# WHERE A PLACEMENT CALLS HOME. Three sources, one per placement, and the failure when there are
# none — which was unreachable through this harness until a mutation walked through it.
# ---------------------------------------------------------------------------------------------


def test_a_placement_may_name_its_own_controller():
    """`controller=` on `place()` beats the one resolved with the Artifact, and it is PER PLACEMENT
    since packing: two Artifacts on one Fleet may legitimately call home to two control planes, so
    a converge-level answer would be one address for a question that now has N."""

    async def body(f):
        await f.place("nscheck", "0.1.0", controller="10.9.9.9")
        await f.place("subfinder", "0.2.0")

    scope = FleetScope(machines=2).hold(DO, tag="dns", body=body)
    entries = _child_args(scope)[-1]["placements"]
    assert entries[0]["controller"] == "10.9.9.9"
    assert entries[1]["controller"] == "10.124.0.2", "the other one keeps the resolver's answer"


def test_the_fleets_controller_reaches_a_placement_that_named_none():
    """`hold(controller=…)` is the **Fleet**'s answer, and it fills in for a placement that did not
    give one — checked with a resolver that supplies none, or it would be the resolver's value
    being asserted rather than the fleet's."""
    scope = FleetScope(machines=2, resolve_controller="").hold(
        DO, tag="dns", controller="10.7.7.7", body=_placing()
    )
    assert _child_args(scope)[-1]["placements"][0]["controller"] == "10.7.7.7"


def test_a_placement_with_no_controller_anywhere_is_refused_here():
    """NAMED AT THE CALL SITE RATHER THAN A MINUTE INTO A CONVERGE. Without this the stack program
    fails with `controller="" is not safe to place on a Machine` — true, unactionable from this
    side, and arriving after the Machines are up.

    The resolver answering with an empty controller is a real state (a control plane that cannot
    resolve its own address) and it was UNREACHABLE through this harness until now: the fake always
    supplied a good one, so a mutation deleting this whole check passed the suite."""

    async def body(f):
        with pytest.raises(fleet.PlacementFailed, match="no Controller address for nscheck@0.1.0"):
            await f.place("nscheck", "0.1.0")

    scope = FleetScope(machines=2, resolve_controller="").hold(DO, tag="dns", body=body)
    # AND IT REFUSED BEFORE CONVERGING, so no Machines were re-placed on the strength of it.
    assert len(_child_args(scope)) == 1, scope.summary()


def test_spread_on_a_fleet_with_no_machines_is_refused():
    """`spread=True` asks for one Worker per Machine and there is no Machine to give it an address
    of its own. Refused here rather than converging a placement with `workers=0`, which the program
    reads as a count that is not one and rejects a minute later."""
    import asyncio

    f = fleet.hold(fleet.do_fleet(machines=0), tag="dns")
    f._lease = "run#nonce"
    with pytest.raises(ValueError, match="no Machine"):
        asyncio.run(f.place("nscheck", "0.1.0", spread=True))


def test_place_outside_the_scope_is_refused():
    """Converging Machines while holding no Lease leaves capacity nothing collects but the **Lease** workflow's
    clock — the exact leak slice 09 exists to close, reached from the new door."""
    import asyncio

    f = fleet.hold(DO, tag="dns")
    with pytest.raises(RuntimeError, match="holds no Lease"):
        asyncio.run(f.place("nscheck", "0.1.0"))


def test_place_on_a_shared_fleet_is_refused():
    """MY DESIRED STATE IS THE WHOLE FLEET'S. A placement converge carries everything, so mine
    deletes the co-tenant's `command.remote.Command` — which runs MACHINE_TEARDOWN and stops their
    Worker while their Run is mid-dispatch. Nothing raises on either side."""

    async def body(f):
        with pytest.raises(RuntimeError, match="held by 2 Runs"):
            await f.place("nscheck", "0.1.0")

    scope = FleetScope(leases=2, machines=2).hold(DO, tag="dns", body=body)
    assert scope.children("stackWorkflow") == [], scope.summary()


def test_a_later_holder_converges_nothing():
    """THE OTHER HALF OF THE SAME SENTENCE. A machines-only converge omits the co-tenant's
    placement, and in a declarative desired state omitting it REMOVES it. So a hold onto capacity
    somebody is already holding takes what is there — one activity, no Pulumi, no four minutes."""
    scope = FleetScope(leases=2).hold(DO, tag="dns")
    assert scope.children("stackWorkflow") == [], scope.summary()
    assert scope.activities_named(fleet.HOLD_LEASE_ACTIVITY), scope.summary()
    assert scope.activities_named(fleet.DROP_LEASE_ACTIVITY), scope.summary()
    assert any("converging nothing" in m for _, m in scope.logs), scope.logs


def test_the_first_holder_does_converge():
    """THE CONTROL for the test above. An implementation that simply never converged from `hold()`
    would pass it, and would give every caller a Fleet with no Machines in it."""
    scope = FleetScope(leases=1).hold(DO, tag="dns")
    assert scope.children("stackWorkflow", op="up") != [], scope.summary()


def test_up_converges_on_a_shared_fleet_exactly_as_it_did_before():
    """The skip above keys on "this converge carries no placement", NOT on "the Fleet is shared" —
    and the difference is every existing caller. `up()` sends a complete desired state, so two Runs
    on one `<actor>-<version>` Fleet converge the same thing, which is what they did before 0037."""
    scope = FleetScope(leases=2).run(DO)
    assert scope.child("stackWorkflow", op="up")["args"]["bundleSha"] == "a" * 64


def test_machines_none_is_refused_naming_the_fleet_it_would_destroy():
    """ADR 0037 gives `machines=None` the meaning "take what already exists", and this slice cannot
    honour it: what that needs is a read of the CURRENT count, and there is none a workflow can
    reach. The naive version is worse than missing — an absent `machines` coerces to 0 server-side,
    so the converge that "left the count alone" deletes every Droplet and reports success."""
    with pytest.raises(ValueError, match="machines= is required") as e:
        fleet.hold(tag="dns")
    assert "destroys the Fleet" in str(e.value)


@pytest.mark.parametrize("bad", ["9dns", "DNS", "d", "d" * 17, "dns_x", ""])
def test_hold_refuses_a_tag_that_would_fail_minutes_later(bad):
    """The tag is validated on this side too, not instead: it becomes a DigitalOcean tag, an
    inventory group, every machine name — and since 0037 the Fleet's own name."""
    with pytest.raises(ValueError, match="invalid"):
        fleet.hold(DO, tag=bad)


@pytest.mark.parametrize("kwargs", [{"sessions": 0}, {"sessions": -2}, {"sessions": True}])
def test_place_refuses_a_density_that_is_not_one(kwargs):
    import asyncio

    f = fleet.hold(DO, tag="dns")
    f._lease = "run#nonce"  # the scope's own guard is tested separately
    with pytest.raises(ValueError, match="sessions"):
        asyncio.run(f.place("nscheck", "0.1.0", **kwargs))


@pytest.mark.parametrize("args", [("", "0.1.0"), ("nscheck", ""), ("", "")])
def test_place_requires_both_halves_of_an_artifact(args):
    import asyncio

    f = fleet.hold(DO, tag="dns")
    f._lease = "run#nonce"
    with pytest.raises(ValueError, match="one published Artifact"):
        asyncio.run(f.place(*args))
