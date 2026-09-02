"""The Method name as user metadata: the one line a dispatch puts on its own scheduling event.

WHY THIS IS NOT IN THE PAYLOAD TESTS NEXT DOOR. `entry_input` already carries `method` on the
wire, and that copy is the one the Actor resolves against — but it is a PAYLOAD, and on this
deployment a payload may be a claim-check ref (ADR 0007). A surface that wanted to say which
Method a run called would pay a blob GET per dispatch. So the same name is written a second time
as a Temporal user-metadata Summary: metadata, ~200 bytes, on the scheduling event, where both
kontra's transcript (`control/orchestrator/src/transcript.ts`) and Temporal's own UI read it for free.

THE FAILURE MODE THIS FILE EXISTS FOR IS SILENT. Past the byte cap the line is cut with nothing
said about it, so a Summary composed first and trimmed afterwards loses whichever field happened
to be last — on exactly the dispatches worth reading, and without an error anywhere. Every test
below is about spending the budget instead of overrunning it.

The reader's half is `control/orchestrator/src/transcript.test.ts`, which pins the same shape from the
other side; there is no shared code between them by design, so both must be pinned.
"""

from __future__ import annotations

from pathlib import Path

from actorkit import catalog
from actorkit.catalog import SUMMARY_BUDGET, dispatch_summary

ROOT = Path(__file__).resolve().parent.parent


def utf8(text: str) -> int:
    """The budget's unit. `len()` counts characters, and the two differ on exactly the keys an
    operator is most likely to type."""
    return len(text.encode("utf-8"))


# ---------------------------------------------------------------------------------------------
# What it says
# ---------------------------------------------------------------------------------------------


def test_a_dispatch_names_its_actor_version_and_method():
    """The three facts a bar label has to carry. Only one of them — the Method — is unrecoverable
    from anywhere else in the log, which is why it goes first."""
    line = dispatch_summary("crawler", "0.1.0", "crawl", units=12)
    assert line == "crawl · crawler@0.1.0 · 12 units"


def test_a_keyed_dispatch_says_which_identity_it_claimed():
    """`crawler["acme.com"]` is a different virtual object from `crawler["evil.com"]` (ADR 0023
    §10), and two dispatches that differ only in their key are otherwise identical on screen."""
    assert dispatch_summary("crawler", "0.1.0", "crawl", "acme.com", 12) == (
        "crawl · crawler@0.1.0[acme.com] · 12 units"
    )


def test_a_dispatch_that_named_no_method_never_looks_like_one_that_did():
    """THE CROSS-LANGUAGE INVARIANT, and the reason the `@` is emitted even with no version.

    A single-Method Actor accepts an unnamed dispatch, and the reader takes the FIRST field as the
    Method name when it has the shape of one. A version-less Actor (the DIY path, `echo-shared`)
    would render as a bare `echo` in that first position, and the transcript would confidently
    report a Method called `echo`. `echo@` cannot be read as a Method name by anyone.
    """
    assert dispatch_summary("echo", "", "", units=5) == "echo@ · 5 units"
    assert dispatch_summary("crawler", "0.1.0", "", units=5) == "crawler@0.1.0 · 5 units"
    # Every first field of a Method-less line carries the one character a Method name never can.
    for line in (dispatch_summary("echo", "", "", units=5), dispatch_summary("echo", "0.1.0")):
        assert "@" in line.split(catalog.SUMMARY_SEP)[0]


def test_a_version_less_actor_still_carries_its_at_sign_when_a_method_is_named():
    assert dispatch_summary("echo", "", "say", units=5) == "say · echo@ · 5 units"


# ---------------------------------------------------------------------------------------------
# The budget — built to fit, never trimmed
# ---------------------------------------------------------------------------------------------


def test_an_ordinary_dispatch_is_nowhere_near_the_cap():
    """The cap is a guard, not a shape. If a normal line were close to it, every comment about
    priority below would be describing the common case rather than the pathological one."""
    assert utf8(dispatch_summary("crawler", "0.1.0", "crawl", "acme.com", 12)) < 64


def test_nothing_any_caller_can_write_puts_the_line_over_the_cap():
    """Four fields, four different authors: the Actor's name and version come from a manifest, the
    Method from a Python attribute, the key from whatever an operator bound, and the count from
    the Batch. Each of them is long here, and all of them at once."""
    for actor, version, method, key, units in [
        ("a" * 300, "0.1.0", "m" * 300, "k" * 3000, 1234567890),
        ("crawler", "0.1.0-rc.1+build.9999", "crawl", "https://" + "x" * 4000, 0),
        ("日本語のアクター" * 20, "0.1.0", "検索" * 40, "キー" * 500, 999999),
        ("a" * 300, "", "", "", None),
    ]:
        line = dispatch_summary(actor, version, method, key, units)
        assert utf8(line) <= SUMMARY_BUDGET, (actor[:8], utf8(line))


def test_a_long_actor_and_a_long_method_both_still_read():
    """BUILT TO FIT, and this is the test that tells the two approaches apart. A line composed
    first and cut to 200 bytes afterwards would end mid-word with the unit count gone; this one
    shortens the fields that are too long, marks each cut, and still ends in a whole count."""
    line = dispatch_summary("a-very-long-actor-name-" * 6, "0.1.0", "an_extremely_long_method_name" * 4, units=37)
    assert utf8(line) <= SUMMARY_BUDGET
    fields = line.split(catalog.SUMMARY_SEP)
    assert len(fields) == 3
    assert fields[0].startswith("an_extremely_long_method_name")
    assert fields[1].startswith("a-very-long-actor-name-")
    assert fields[2] == "37 units"
    # A field that was shortened SAYS it was shortened. A silent cut is the failure this file is
    # about, and it looks exactly like a legitimately short name.
    assert fields[0].endswith("…") and fields[1].endswith("…")


def test_the_unit_count_survives_a_key_nobody_in_this_repo_controls():
    """`crawler["https://…"]` is a legal binding, so the key is the one field whose length is an
    operator's to choose. Its room is what is LEFT OVER — reserving the count first is the only
    reason a 4 KB key cannot push it off the end and report a sweep with no size."""
    line = dispatch_summary("crawler", "0.1.0", "crawl", "https://" + "x" * 4000, 623)
    assert utf8(line) <= SUMMARY_BUDGET
    assert line.startswith("crawl · crawler@0.1.0[https://xxx")
    assert line.endswith("…] · 623 units")


def test_the_key_always_has_room_at_the_real_budget():
    """WHAT THE TWO CAPS BUY. A Method may take 64 bytes and an `actor@version` 80, so however
    long all three are the key still has room to say something — which is what keeps the ceilings
    from being arbitrary numbers. The worst case is every field at its cap at once."""
    line = dispatch_summary("a" * 300, "0.1.0" + "9" * 300, "m" * 300, "acme.com", 10**30)
    assert utf8(line) <= SUMMARY_BUDGET
    assert "[acme.com]" in line


def test_a_key_with_no_room_left_is_dropped_rather_than_rendered_as_brackets():
    """`[…]` says nothing true about which identity was claimed, and costs the bytes of saying it.

    Unreachable at the real budget, by the test above — so it is exercised at a smaller one, which
    is what makes the guard a guard rather than a comment about one.
    """
    line = dispatch_summary("crawler", "0.1.0", "crawl", "acme.com", 12, budget=40)
    assert utf8(line) <= 40
    assert "[" not in line
    assert line == "crawl · crawler@0.1.0 · 12 units"


def test_a_shortened_field_is_cut_on_a_character_boundary():
    """The budget is in BYTES and a name may not be ASCII, so slicing the encoded form can land
    inside a character. What comes out must still be a string anyone can print — a mojibake byte
    in a bar label is the kind of thing nobody reports and everybody stops trusting."""
    line = dispatch_summary("クローラー", "0.1.0", "検索", "日本語のキー" * 100, 12)
    assert utf8(line) <= SUMMARY_BUDGET
    # Round-trips: no partial character survived the cut.
    assert line.encode("utf-8").decode("utf-8") == line
    assert line.startswith("検索 · クローラー@0.1.0[日本語のキー")


def test_a_budget_too_small_for_a_field_leaves_it_out_instead_of_lying():
    """Not a production case — the budget is a constant — but it is the boundary the arithmetic
    is easiest to get wrong at, and the answer must never be a lone ellipsis pretending to be a
    name."""
    for budget in range(0, 40):
        line = dispatch_summary("crawler", "0.1.0", "crawl", "acme.com", 12, budget=budget)
        assert utf8(line) <= budget
        assert line.strip(" ·") != "…"


# ---------------------------------------------------------------------------------------------
# The call site, and the id it did not touch
# ---------------------------------------------------------------------------------------------


def test_the_dispatch_puts_the_method_on_the_summary_it_sends():
    """A builder nothing calls with the Method would pass every test above and ship the gap. Read
    from the source because `dispatch_ref` needs a workflow context and this file, like its
    neighbour, is infrastructure-free."""
    import inspect

    src = inspect.getsource(catalog.ActorHandle.dispatch_ref)
    assert "summary=dispatch_summary(" in src
    # The Method, the Actor, its version and the key — the argument ORDER is the contract, and the
    # count is the one argument computed here rather than read off the handle.
    assert "self.name," in src and "self.version," in src and "method," in src and "self.key," in src


def test_the_backing_workflow_id_is_not_where_the_method_went():
    """THE DECISION THIS SLICE DID NOT REVERSE (handler/nexus.go). The id is a label nothing
    parses, and a label that names the wrong thing is worse than no label at all — a dispatch that
    attaches to an existing actor instance would carry the first caller's Method name forever. The
    Summary exists precisely so the id did not have to change, so a change to it here is a change
    to the wrong thing.
    """
    nexus_go = (ROOT / "handler" / "nexus.go").read_text()
    assert 'return "actor-" + os.Getenv("KONTRA_ACTOR_NAME") + "-" + base' in nexus_go
    # Nothing about a Method reaches the id derivation.
    body = nexus_go.split("func backingWorkflowID(")[1].split("\nfunc ")[0]
    assert "method" not in body.lower()


def test_the_reader_on_the_other_side_pins_the_same_shape():
    """A TWO-WRITER CONTRACT WITH NO REGISTRATION STEP, like the queue and endpoint derivations.
    Nothing fails loudly on a drift — the transcript would simply stop naming Methods — so the
    separator and the reader's own guard are pinned from this side too."""
    reader = (ROOT / "shared" / "core" / "src" / "transcript.ts").read_text()
    assert f"const SUMMARY_SEP = '{catalog.SUMMARY_SEP}';" in reader
    # The guard is what keeps a line this reader does not recognise from becoming a wrong name.
    assert "const METHOD_NAME = " in reader
    assert "export function methodOf(" in reader
