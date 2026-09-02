"""THE PYTHON ARM of `shared/conformance/placement.json`.

This side is a WRITER. `actorkit.fleet` builds a **Fleet**'s desired state out of `hold()`,
`place()` and `up()`; `cli/fleet.go` builds the same thing out of `kontra fleet up|deploy`; and
`backend/src/infra/stacks.ts:coerceFleetArgs` is the only reader. Nothing joins the three but
matching string literals, and the reader DISCARDS WITHOUT A WORD anything it does not recognise.

WHY A KEY SET AND NOT A GOLDEN. The two writers cannot produce byte-identical desired states in a
unit test: Python's placement fields come from `resolveBundle` over the network and Go's from a
built Artifact on disk, so the values differ by construction while the CONTRACT — which keys are
present — is exactly what has to match. `lease.json`'s `lease_set_wire` section pins a golden because
one side genuinely computes the bytes; this one pins containment because two sides genuinely
compute different bytes for the same shape.

THE ONE THAT DESTROYS A FLEET is `machines`. Pulumi's desired state is total: an absent `machines`
is `Number(undefined ?? 0)` = 0 server-side, the program builds zero Droplets, and the converge
DELETES every Machine in the **Fleet** and reports success. So `required_always` is checked on the
placement converge as well as on the capacity one, which is the assertion that would have caught a
`place()` written as a patch.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from actorkit import fleet
from fleetscope import FleetScope

CORPUS = json.loads(
    (Path(__file__).resolve().parent.parent / "shared" / "conformance" / "placement.json").read_text("utf-8")
)

DO = fleet.do_fleet(machines=2, region="nyc3")


def _placing(**kw):
    async def body(f):
        await f.place("nscheck", "0.1.0", **kw)

    return body


def _converges(scope: FleetScope) -> list[dict]:
    return [c.arg["args"] for c in scope.calls if c.kind == "child"]


async def _packing(f):
    """ADR 0037's own snippet: two Artifacts on one **Fleet**, sharing its Machines' addresses."""
    await f.place("nscheck", "0.1.0")
    await f.place("subfinder", "0.2.0")


#: `case name` -> the args that case's converge produced, built the way `actorkit.fleet` builds it.
def _cases() -> dict[str, dict]:
    hold_only = _converges(FleetScope().hold(DO, tag="dns"))
    placed = _converges(FleetScope(machines=2).hold(DO, tag="dns", body=_placing()))
    dense = _converges(FleetScope(machines=2).hold(DO, tag="dns", body=_placing(sessions=8)))
    packed = _converges(FleetScope(machines=2).hold(DO, tag="dns", body=_packing))
    spread = _converges(FleetScope(machines=2).hold(DO, tag="dns", body=_placing(spread=True)))
    return {
        "machines_only": hold_only[-1],
        "placed": placed[-1],
        "placed_with_density": dense[-1],
        "packed": packed[-1],
        "spread": spread[-1],
    }


def test_the_corpus_still_has_the_cases_this_arm_exists_for():
    """A corpus that silently shrank to nothing passes everything (shared/conformance/README.md §3)."""
    names = [c["name"] for c in CORPUS["writer_cases"]]
    assert names == ["machines_only", "placed", "placed_with_density", "packed", "spread"], names
    assert "machines" in CORPUS["keys"]["sections"]["required_always"]["keys"]
    assert CORPUS["keys"]["sections"]["from_resolver"]["keys"], "the placement key list is empty"
    assert CORPUS["placement_keys"]["required"] == ["actorName", "bundleUrl"]


def _entries(args: dict) -> list[dict]:
    """Every placement one converge DESCRIBES, in whichever spelling its writer used.

    IT FOLDS, BECAUSE THE READER FOLDS. There are two legal spellings for ONE placement — the
    `placements` array and the pre-packing scalars `kontra fleet deploy` still sends — and
    `programs/fleet.ts:placementsOf` turns the second into the first on arrival. `cli/…_test.go`
    holds the same fold on the other writer's side, which is the only way the two arms are counting
    the same thing.
    """
    if "placements" in args:
        return list(args["placements"])
    if not args.get("bundleUrl"):
        return []
    return [{k: args[k] for k in CORPUS["placement_keys"]["optional"] + ["actorName", "bundleUrl"] if k in args}]


@pytest.mark.parametrize("case", CORPUS["writer_cases"], ids=lambda c: c["name"])
def test_this_writer_sends_the_number_of_placements_the_case_names(case):
    """THE ARM THAT ONLY PACKING NEEDED. `placements` carries the whole desired state now, so the
    failure worth pinning is a COUNT: a converge naming one of two Artifacts deletes the other,
    runs its teardown and stops its Worker, and Pulumi reports success either way."""
    args = _cases()[case["name"]]
    want = case.get("placements")
    entries = _entries(args)
    if want is None:
        assert entries == [], f"{case['name']} places nothing and must describe no placement"
        return
    assert len(entries) == want, entries


@pytest.mark.parametrize("case", CORPUS["writer_cases"], ids=lambda c: c["name"])
def test_this_writer_produces_the_keys_the_case_names(case):
    args = _cases()[case["name"]]
    missing = [k for k in case["keys"] if k not in args]
    assert not missing, f"{case['name']} is missing {missing}: {case['why']}\n  sent {sorted(args)}"
    present = [k for k in case["forbidden"] if k in args]
    assert not present, f"{case['name']} sent {present}, which this converge must not carry"


@pytest.mark.parametrize("case", CORPUS["writer_cases"], ids=lambda c: c["name"])
def test_every_converge_carries_the_keys_whose_absence_is_a_teardown(case):
    """`required_always`, on the PLACEMENT converge as much as on the capacity one. A `place()`
    written as a patch — placement keys only — deletes every Droplet it was placing onto."""
    args = _cases()[case["name"]]
    for key in CORPUS["keys"]["sections"]["required_always"]["keys"]:
        why = CORPUS["keys"]["sections"]["required_always"]["why"]
        assert key in args, f"{case['name']} omits {key!r}: {why}"


def test_this_writer_sends_no_key_the_reader_would_drop():
    """Every key must be in the corpus's list or it never reaches the program — no error, no
    warning, and the caller's converge reports success without it."""
    known = set(CORPUS["keys"]["sections"]["not_in_fleet_args"]["keys"])
    for section in ("required_always", "provider", "from_resolver", "density", "packing"):
        known |= set(CORPUS["keys"]["sections"][section]["keys"])
    for name, args in _cases().items():
        unknown = sorted(set(args) - known)
        assert not unknown, f"{name} sends {unknown}, which coerceFleetArgs drops silently"


def test_this_writer_sends_no_key_inside_a_placement_the_reader_would_drop():
    """THE SAME SENTENCE ONE LEVEL DOWN, and it needs its own arm because `coercePlacement` is a
    second narrowing with a second key list. A key the entry-level reader does not know is dropped
    exactly as silently as one the converge-level reader does not know."""
    known = set(CORPUS["placement_keys"]["required"]) | set(CORPUS["placement_keys"]["optional"])
    seen = 0
    for name, args in _cases().items():
        for entry in args.get("placements") or []:
            seen += 1
            unknown = sorted(set(entry) - known)
            assert not unknown, f"{name} sends {unknown} inside a placement, which is dropped"
            for required in CORPUS["placement_keys"]["required"]:
                assert required in entry, f"{name}: an entry without {required} is dropped ENTIRELY"
            for forbidden in CORPUS["placement_keys"]["not_here"]:
                assert forbidden not in entry, f"{name} puts {forbidden} inside a placement"
    assert seen >= 4, "no placement entry was examined; this sweep proves nothing"


def test_this_writer_sends_no_booleans():
    """The rule with a corpse behind it. `--tmux` rode as a boolean for a release and was narrowed
    away before the program saw it, so a Machine could deploy 'successfully' and never be viewable —
    and the NUMBER keys are worse than dropped, because `Number(True)` is 1 and nothing refuses a
    one-Machine fleet or a one-Machine placement.

    IT SWEEPS INSIDE THE PLACEMENTS TOO, because that is where the temptation is: `spread=` is a
    boolean at the call site and the obvious implementation forwards it. It resolves to the number
    `workers` instead, and this is what stops the obvious implementation coming back."""
    seen = 0
    for name, args in _cases().items():
        bools = sorted(k for k, v in args.items() if isinstance(v, bool))
        assert not bools, f"{name} sends {bools} as booleans: {CORPUS['types']['rules']['never_boolean']}"
        for entry in args.get("placements") or []:
            seen += 1
            inner = sorted(k for k, v in entry.items() if isinstance(v, bool))
            assert not inner, f"{name} sends {inner} as booleans inside a placement"
    assert seen >= 4, "no placement entry was examined; this sweep proves nothing"


def test_the_spread_case_crosses_as_the_machine_count():
    """`spread=True` is not on this wire and must not be. What crosses is `workers`, a NUMBER equal
    to the Fleet's machine count — so the corpus pins the VALUE, because a writer that dropped the
    key entirely would place on every Machine anyway and pass any assertion about the key alone."""
    case = next(c for c in CORPUS["writer_cases"] if c["name"] == "spread")
    entry = _cases()["spread"]["placements"][0]
    for key, want in case["placement_values"].items():
        assert entry.get(key) == want, f"spread sent {key}={entry.get(key)!r}, corpus says {want!r}"


def test_the_fake_resolver_is_exactly_the_resolved_bundle_the_sdk_forwards():
    """THE ARM'S OWN VACUITY GUARD, and it is the one that matters most here.

    `actorkit.fleet` spreads `resolveBundle`'s answer straight into the stack args, so what this
    file measures as "the placement keys" is really "the keys the fake returned". A fake short of a
    field would make `test_this_writer_produces_the_keys_the_case_names` pass while proving nothing
    about the SDK — the shape of green test this repo has already paid for. So the fake's key set is
    checked against the corpus's `from_resolver` list, which the TypeScript arm checks against
    `ResolvedBundle` itself.
    """
    placed = _cases()["placed"]
    for key in CORPUS["keys"]["sections"]["from_resolver"]["keys"]:
        assert key in placed, f"the harness's resolveBundle does not return {key!r}"


def test_the_scale_converge_carries_the_density_and_the_capacity_one_does_not():
    """`maxSessions` is the key the reader's string loop would have dropped, and it is also the one
    a second `place()` changes — so a writer that omitted it would make ADR 0037's scale operation a
    no-op reported as success."""
    cases = _cases()
    assert cases["placed_with_density"]["maxSessions"] == 8
    assert "maxSessions" not in cases["machines_only"]
    assert "maxSessions" not in cases["placed"], "unset density is ABSENT, not 0 — 0 is refused"
