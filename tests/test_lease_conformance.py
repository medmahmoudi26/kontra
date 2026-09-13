"""THE PYTHON ARM of shared/conformance/lease.json.

This side is the lease-id grammar's WRITER. `control/orchestrator/src/lease.ts` reads it back — that is what
answers "who is holding this Fleet" on the one screen an operator looks at when a Fleet will not
die — and the two are joined by nothing but a matching separator character.

THE FAILURE HAS NO LOUD MODE. A **Lease** held under one spelling and dropped under another is a
**Lease** that is never dropped: the hold succeeds, the drop succeeds against an id the **Lease** workflow is
not holding, and the **Lease** workflow keeps a claim whose holder nothing can find. Nothing raises anywhere.
The clock bounds the damage to one TTL, which is exactly why the clock exists and is not a reason to
let the spellings drift.

It also pins the two ACTIVITY NAMES, which are written independently here and on the orchestrator's
side. An activity scheduled under a name nobody registered does not error — the task sits on the
queue until ScheduleToStart fires, which presents as a Run hanging at the first line of its fleet
scope with nothing to read.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

from kontra import fleet

ROOT = Path(__file__).resolve().parent.parent
CORPUS = json.loads((ROOT / "shared" / "conformance" / "lease.json").read_text(encoding="utf-8"))
BACKEND = ROOT / "control" / "orchestrator" / "src"


def _names() -> dict[str, dict]:
    return {c["name"]: c for c in CORPUS["names"]["cases"]}


def test_the_corpus_still_carries_the_inputs_that_break_a_naive_implementation():
    """THE GUARD ON THIS FILE. Every test below loops over the corpus, and a loop over an empty list
    reports success for having found nothing — the shape `shared/conformance/README.md` step 3 exists to
    prevent, and the shape half the guards it replaced actually had."""
    cases = CORPUS["lease_id"]["cases"]
    assert cases, "the corpus has no lease_id cases; every assertion below is vacuous"
    assert _names(), "the corpus names nothing"

    sep = _names()["separator"]["value"]
    # A holder that itself contains the separator — without this row a first-separator split passes.
    assert any(sep in c["holder"] for c in cases if c["holder"])
    # An UNATTRIBUTED lease, which is a real state and the one a drifted `holder` key would hide.
    assert any(c["holder"] == "" for c in cases)
    # A parse-only id with no separator at all.
    assert any(c.get("builds") is False for c in cases)


def test_the_writer_builds_every_id_the_corpus_pins():
    built = 0
    for case in CORPUS["lease_id"]["cases"]:
        if case.get("builds") is False:
            continue
        got = fleet.lease_id(case["holder"], case["nonce"])
        assert got == case["lease"], f"{case['why']}\n  built {got!r}, corpus says {case['lease']!r}"
        built += 1
    assert built > 0, "no buildable cases ran"


def test_the_separator_is_the_corpus_separator():
    """One character, in three languages. A rename here would produce ids the TypeScript reader
    splits differently and the Go reader prints whole."""
    assert fleet.LEASE_SEPARATOR == _names()["separator"]["value"]
    # …and it is the character the builder actually uses, not merely a constant beside it.
    assert fleet.lease_id("a", "b") == f"a{fleet.LEASE_SEPARATOR}b"


def test_the_activity_names_match_the_corpus():
    assert fleet.HOLD_LEASE_ACTIVITY == _names()["hold_activity"]["value"]
    assert fleet.DROP_LEASE_ACTIVITY == _names()["drop_activity"]["value"]


def test_the_activity_names_are_functions_the_orchestrator_actually_EXPORTS():
    """Temporal registers an activity under its EXPORTED FUNCTION NAME, so a name that matches a
    constant and not a function is a task scheduled onto a queue whose worker has never heard of it.

    This is a source read rather than a corpus comparison, and it is the honest exception ADR 0035
    rule two allows: what is being asserted is that a SYMBOL exists, which is not a value a corpus
    can hold. `tests/test_fleet_client.py` makes the same read for the two older fleet activities.
    """
    src = (BACKEND / "activities" / "lease.ts").read_text(encoding="utf-8")
    for name in (fleet.HOLD_LEASE_ACTIVITY, fleet.DROP_LEASE_ACTIVITY):
        assert re.search(rf"export async function {re.escape(name)}\b", src), (
            f"{name} is scheduled by this SDK and is not an exported function in activities/lease.ts"
        )


def test_the_lease_set_wire_holds_both_kinds_of_holder():
    """The golden this file does not itself decode, checked for the property the OTHER two arms
    depend on: one attributed Lease and one unattributed. A golden of only attributed Leases passes
    the day somebody adopts a Fleet; one of only unattributed Leases passes against a `holder` key
    that decodes nothing at all."""
    leases = CORPUS["lease_set_wire"]["golden"]["body"]["leases"]
    assert len(leases) == 2
    assert sum(1 for l in leases if l["holder"] == "") == 1
    assert sum(1 for l in leases if l["holder"] != "") == 1
    # And every golden id round-trips through THIS side's builder, so the two sections cannot drift
    # apart while each stays internally consistent.
    for l in leases:
        nonce = l["lease"].rsplit(fleet.LEASE_SEPARATOR, 1)[-1]
        assert fleet.lease_id(l["holder"], nonce) == l["lease"]
