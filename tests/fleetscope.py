"""A fleet scope driven with every Temporal call recorded, and nothing real behind it.

BOTH DOORS, ONE INSTRUMENT. `run()` drives `fleet.up(...)` and `hold()` drives
`fleet.hold(...)` + `place()` (ADR 0037), through the same fakes and the same recording. That is
not tidiness: "`up()` is sugar over `hold` + `place`" is a claim about what the two SCHEDULE, and
two harnesses would let the two doors be compared by two different instruments — which is how a
claim like that stays true in a comment and false in the code.

WHY A HARNESS RATHER THAN A MONKEYPATCH PER TEST. `kontra.fleet`'s scope now makes FOUR kinds of
Temporal call — an activity to resolve the Bundle, an activity to HOLD a **Lease**, a child workflow
to converge, and an activity to DROP — and the questions worth asking are all about which of them
happened, in what order, and with what. Five tests each re-patching five names is five chances to
patch four of them, and the fifth failure is silent: `workflow.info()` outside a workflow raises,
which reads as the test being wrong rather than as the code being untested.

It is deliberately NOT a Temporal test environment. What it pins is the SHAPE of the scope — what it
schedules and what it does not — and the shape is a property of this file's code, not of a server.
The server-side half of the same contract is `control/orchestrator/src/workflows/lease.test.ts`, which runs
against a real Temporal, and `control/orchestrator/src/activities/lease.test.ts`, which runs the real activities.

THE ONE THING IT MUST NOT DO IS SUCCEED QUIETLY WHEN NOTHING RAN. `run()` asserts that the scope
actually entered and exited, so a harness that patched the wrong name fails here instead of leaving
every assertion built on it looking at an empty list.
"""

from __future__ import annotations

import asyncio
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable

from kontra import fleet


@dataclass
class Call:
    """One thing the scope asked Temporal to do."""

    kind: str  # "activity" | "child"
    name: str
    arg: Any
    kwargs: dict[str, Any] = field(default_factory=dict)


class FleetScope:
    """Run one `async with fleet.up(...)` against fakes, and remember everything it did.

    `leases` is what the **Lease** workflow reports back on the hold — 1 means this **Run** is alone on the
    **Fleet**, more means it is shared, and `kontra.fleet` branches on exactly that to decide
    whether its own failure may tear the **Fleet** down.
    """

    def __init__(
        self,
        *,
        leases: int = 1,
        converge_fails: str = "",
        hold_fails: str = "",
        place_fails: str = "",
        resolve_fails: str = "",
        machines: int = 0,
        resolve_controller: str = "10.124.0.2",
    ) -> None:
        self.leases = leases
        self.converge_fails = converge_fails
        self.hold_fails = hold_fails
        #: Fail only a converge that CARRIES A PLACEMENT. `converge_fails` cannot express that: a
        #: hold/place scope makes two `op=up` converges and the whole point of the second one is
        #: that the Machines from the first are already standing when it goes wrong.
        self.place_fails = place_fails
        #: Fail `resolveBundle`. THE ADR'S OWN EXAMPLE of a `place()` that fails after a successful
        #: `hold()` — "no room, image will not pull, digest unsigned" — and the one `fleet.up()`
        #: structurally cannot reach, because it resolves before a Machine exists.
        self.sessions_fail: str = ""
        self.sessions_refused: list = []
        self.machines_named: list = ["kf-nscheck-01", "kf-nscheck-02"]
        self.resolve_fails = resolve_fails
        #: How many Machines a converge reports. Non-zero so the standing-Machine count in
        #: `PlacementFailed` is a number under test rather than a constant 0.
        self.machines = machines
        #: What `resolveBundle` answers for `controller`. EMPTY IS A REAL ANSWER and the reason this
        #: is a knob: a control plane that cannot resolve its own address returns one, and the SDK's
        #: refusal for that case was unreachable while the fake always supplied a good one — a
        #: mutation deleting the whole check survived the suite.
        self.resolve_controller = resolve_controller
        self.calls: list[Call] = []
        self.logs: list[tuple[str, str]] = []
        self.entered = False
        self.exited = False
        #: The Fleet the scope handed the body, kept so a test can read what it BELIEVES is placed
        #: after the scope has closed. `None` when the scope never opened.
        self.fleet: Any = None
        #: The workflow id the fake `workflow.info()` reports — the holder of every Lease taken here.
        self.run_id = "recon-run-1"
        #: The deterministic nonce the fake `workflow.uuid4()` returns.
        #:
        #: SPELLED AS HEX AND NOT AS AN INT, because the SDK takes `hex[:8]` and a `UUID(int=small)`
        #: is zero-padded at the FRONT: every small integer produces the nonce `00000000`, so two
        #: scopes with different fixtures would look identical and the uniqueness test would fail
        #: for a reason that has nothing to do with the code. (Found by writing that test.)
        self.nonce = uuid.UUID("a1b2c3d4-0000-4000-8000-000000000000")

    # --- what the scope did -------------------------------------------------------------------

    @property
    def lease(self) -> str:
        """The **Lease** id this scope held, derived the same way the SDK derives it."""
        held = self.activities_named(fleet.HOLD_LEASE_ACTIVITY)
        return str(held[-1]["lease"]) if held else ""

    def activities_named(self, name: str) -> list[dict[str, Any]]:
        return [c.arg for c in self.calls if c.kind == "activity" and c.name == name]

    def activity(self, name: str) -> dict[str, Any]:
        got = self.activities_named(name)
        assert got, f"the scope never called the {name!r} activity; it made {self.summary()}"
        return got[-1]

    def children(self, wf_type: str, **match: Any) -> list[dict[str, Any]]:
        out = [c.arg for c in self.calls if c.kind == "child" and c.name == wf_type]
        for key, want in match.items():
            out = [a for a in out if a.get(key) == want]
        return out

    def child(self, wf_type: str, **match: Any) -> dict[str, Any]:
        got = self.children(wf_type, **match)
        assert got, f"the scope never started a {wf_type!r} child; it made {self.summary()}"
        return got[-1]

    def order(self) -> list[str]:
        """Just the names, in the order they happened. Ordering IS a property here: holding after
        converging leaves a window in which another Run's exit destroys the Machines this scope
        just paid four minutes for."""
        return [c.name for c in self.calls]

    def summary(self) -> str:
        return f"{len(self.calls)} call(s): {self.order()}"

    # --- driving it ---------------------------------------------------------------------------

    def hold(
        self,
        provider: Any = None,
        *,
        tag: str = "dns",
        body: Callable[[Any], Any] | None = None,
        expect_error: type[BaseException] | None = None,
        **hold_kwargs: Any,
    ) -> "FleetScope":
        """The OTHER door (ADR 0037): `async with fleet.hold(tag=…, machines=…) as f:`.

        Same fakes, same recording, so the two doors are compared by the same instrument — which is
        the only way "up() is sugar over hold+place" can be checked rather than asserted.
        """
        return self._drive(
            lambda: fleet.hold(provider, tag=tag, **hold_kwargs), body, expect_error
        )

    def run(
        self,
        provider: Any = None,
        *,
        actor: str = "nscheck",
        version: str = "0.1.0",
        body: Callable[[Any], Any] | None = None,
        expect_error: type[BaseException] | None = None,
        **up_kwargs: Any,
    ) -> "FleetScope":
        return self._drive(
            lambda: fleet.up(provider, actor=actor, version=version, **up_kwargs),
            body,
            expect_error,
        )

    def _drive(
        self,
        make: Callable[[], Any],
        body: Callable[[Any], Any] | None,
        expect_error: type[BaseException] | None,
    ) -> "FleetScope":
        from temporalio import workflow as wf

        async def execute_activity(name, arg=None, **kw):
            self.calls.append(Call("activity", name, arg, kw))
            if name == fleet.RESOLVE_BUNDLE_ACTIVITY:
                if self.resolve_fails:
                    raise RuntimeError(self.resolve_fails)
                # EXACTLY `ResolvedBundle` (`control/orchestrator/src/activities/fleet.ts`), all five fields.
                # The SDK spreads this straight into the stack args, so a fake missing a field is a
                # fake that cannot see a placement key going missing — which is the whole class of
                # bug `shared/conformance/placement.json` exists for. `test_placement_conformance.py`
                # checks this dict against the corpus's `from_resolver` list, so it cannot drift.
                return {
                    "actorName": (arg or {}).get("actor", "nscheck"),
                    "actorVersion": (arg or {}).get("version", "0.1.0"),
                    "actorEngine": "py",
                    "bundleUrl": "http://10.124.0.2:5000/b.tgz",
                    "bundleSha": "a" * 64,
                    "controller": self.resolve_controller,
                }
            if name == fleet.HOLD_LEASE_ACTIVITY:
                if self.hold_fails:
                    raise RuntimeError(self.hold_fails)
                return {
                    "workflowId": "kontra-lease/" + str(arg["stackFqn"]),
                    "lease": arg["lease"],
                    "leases": self.leases,
                    "expiresAt": 1_756_569_600_000,
                }
            if name == fleet.CONVERGE_SESSIONS_ACTIVITY:
                # The session converge is an OBSERVABILITY affordance, so this fake can be told to
                # fail and the scope must survive it — see `test_fleet_sessions.py`.
                if self.sessions_fail:
                    raise RuntimeError(self.sessions_fail)
                return {
                    "converged": [m for m in sorted(self.machines_named)],
                    "refused": list(self.sessions_refused),
                }
            if name == fleet.DROP_LEASE_ACTIVITY:
                return {"workflowId": "kontra-lease/" + str(arg["stackFqn"]), "delivered": True}
            return {}

        async def execute_child_workflow(name, arg=None, **kw):
            self.calls.append(Call("child", name, arg, kw))
            args = ((arg or {}).get("args") or {})
            if (arg or {}).get("op") == "up":
                if self.converge_fails:
                    raise RuntimeError(self.converge_fails)
                if self.place_fails and args.get("bundleUrl"):
                    raise RuntimeError(self.place_fails)
            inventory = {
                f"kf-{args.get('tag', 'x')}-{i + 1:02d}": {
                    "name": f"kf-{args.get('tag', 'x')}-{i + 1:02d}",
                    "host": f"10.0.0.{i + 1}",
                    "publicIp": f"1.1.1.{i + 1}",
                }
                for i in range(self.machines)
            }
            return {"outputs": {"inventory": inventory}}

        harness = self

        class _Log:
            """The lines the scope actually WRITES, rendered.

            IT USED TO RECORD THE FORMAT STRING. `workflow.logger` takes lazy `%`-arguments, like
            every `logging` call, and this recorded `str(msg)` — so a test asserting on a substring
            that falls AFTER the first `%s` was asserting on text no log line ever contains. The two
            existing assertions here happen to match text before the first placeholder, which is why
            nothing had noticed; the third one written did not, and that is how this was found.

            A FAILED INTERPOLATION IS RECORDED RATHER THAN RAISED, because `logging` does the same:
            a mismatched argument count must not turn a working scope into a failing one, and a test
            reading the unrendered template will fail on its own terms.
            """

            @staticmethod
            def _line(msg, a):
                text = str(msg)
                if not a:
                    return text
                try:
                    return text % a
                except (TypeError, ValueError):  # pragma: no cover - the assertion is the report
                    return text

            def info(self, msg, *a, **k):
                harness.logs.append(("info", self._line(msg, a)))

            def warning(self, msg, *a, **k):
                harness.logs.append(("warning", self._line(msg, a)))

            def error(self, msg, *a, **k):
                harness.logs.append(("error", self._line(msg, a)))

        class _Info:
            workflow_id = self.run_id

        saved = (wf.execute_activity, wf.execute_child_workflow, wf.logger, wf.info, wf.uuid4)
        wf.execute_activity = execute_activity
        wf.execute_child_workflow = execute_child_workflow
        wf.logger = _Log()
        wf.info = lambda: _Info()
        wf.uuid4 = lambda: self.nonce
        try:

            async def drive():
                async with make() as f:
                    self.entered = True
                    self.fleet = f
                    if body is not None:
                        res = body(f)
                        if asyncio.iscoroutine(res):
                            await res
                self.exited = True

            if expect_error is not None:
                try:
                    asyncio.run(drive())
                except expect_error:
                    pass
                else:  # pragma: no cover - the assertion IS the point
                    raise AssertionError(f"expected {expect_error.__name__} and the scope succeeded")
            else:
                asyncio.run(drive())
        finally:
            (wf.execute_activity, wf.execute_child_workflow, wf.logger, wf.info, wf.uuid4) = saved

        # THE GUARD ON THE HARNESS. A patch that missed a name, or a scope that returned without
        # doing anything, would leave every assertion downstream looking at an empty list and
        # passing. `expect_error` runs are exempt from `exited` and nothing else.
        assert self.calls, "the scope made no Temporal calls at all — the harness patched nothing"
        if expect_error is None:
            assert self.entered and self.exited, "the scope did not open and close"
        return self
