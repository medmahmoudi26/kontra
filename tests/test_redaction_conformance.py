"""The PYTHON ARM of the redaction contract (shared/conformance/redaction.json).

THE ONE PARITY SURFACE WHERE BEING WRONG IS A CREDENTIAL. Everything else these two SDKs must agree
about is a key, a name or a hash — getting it wrong loses work. Getting redaction wrong writes a
token into workflow history in the clear, where the claim-check codec leaves anything under 128 KiB
inline and readable by anyone who can open the run.

The three rules are byte-identical between the SDKs today; I diffed them. So this is not a bug
report — it is the guard for the thing `narrate.py`'s own header already named and nothing acted
on: "a word only one of them knows is redacted in one surface and not in the other."

THE TWO KINDS STAY SEPARATE, and both implementations argue why in their own headers. A whole-key
match over an ask's fields is not the same job as a word-in-a-sentence match over prose, and
merging them would either redact `password policy` in a narration or miss `db_password` in a form.
What this corpus asserts is that the two LANGUAGES agree, not that the two RULES should.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from kontra.hitl import redact as redact_value
from kontra.redaction import redact as redact_sentence
from kontra.redaction import redact_http

FIXTURE = Path(__file__).resolve().parents[1] / "shared" / "conformance" / "redaction.json"
DOC = json.loads(FIXTURE.read_text(encoding="utf-8"))


def test_the_corpus_still_covers_the_cases_that_matter() -> None:
    """A corpus that shrank to its easy half would pass every case below.

    The negative cases are the ones worth naming: a rule that redacts everything passes every
    positive case in this file and is useless, so `passwordless` and an ordinary sentence are
    asserted present rather than assumed.
    """
    assert len(DOC["sentences"]) >= 8 and len(DOC["values"]) >= 6
    # A SECTION THIS FILE DOES NOT INDEX IS SILENTLY UNEXERCISED. `http_headers` arrived with the
    # report renderer, and nothing here would have failed if it had been added and never driven —
    # which is the same hole the Go arm had, for the same reason: both arms name their sections.
    assert len(DOC["http_headers"]) >= 12
    blob = json.dumps(DOC, ensure_ascii=False)
    for needed in ("passwordless", "bearer", "basic", "db_password", "PASSWORD", "Set-Cookie", "hunter2"):
        assert needed.lower() in blob.lower(), f"the corpus no longer exercises {needed}"


@pytest.mark.parametrize("case", DOC["sentences"], ids=[c["why"][:44] for c in DOC["sentences"]])
def test_a_sentence_is_redacted_as_the_corpus_says(case: dict) -> None:
    assert redact_sentence(case["input"]) == case["expect"]


@pytest.mark.parametrize("case", DOC["values"], ids=[c["why"][:44] for c in DOC["values"]])
def test_a_value_is_redacted_as_the_corpus_says(case: dict) -> None:
    assert redact_value(case["input"]) == case["expect"]


def test_the_two_replacement_strings_are_the_ones_the_corpus_recorded() -> None:
    """The marker itself is contract. A side that changed its wording would pass every case above
    only if the corpus were regenerated from it, which is exactly the drift this file exists to
    stop, so the strings are pinned separately from the cases that contain them."""
    from kontra.hitl import REDACTED as VALUE_REDACTED
    from kontra.redaction import REDACTED as SENTENCE_REDACTED

    assert SENTENCE_REDACTED == DOC["sentence_redacted"]
    assert VALUE_REDACTED == DOC["value_redacted"]


@pytest.mark.parametrize(
    "case", DOC["http_headers"], ids=[c["why"][:44] for c in DOC["http_headers"]]
)
def test_an_http_message_is_redacted_as_the_corpus_says(case: dict) -> None:
    """The third rule: an HTTP request or response, for a report's code blocks.

    IT EXISTS BECAUSE THE SENTENCE RULE LEAKS ON A HEADER. `SECRET_ASSIGNMENT_RE` captures `(\\S+)`,
    one run of non-whitespace, so `Cookie: a=1; b=2` became `Cookie: [redacted] b=2` — measured, in
    both languages. A line that reads as redacted while half the credential is still on it is the
    worst failure shape available, and widening the sentence rule instead would have changed prose
    redaction everywhere, where Go's `narrate.Summary` is a live caller.

    NO PYTHON CALLER EXERCISES THIS IN PRODUCTION, the same way no Python caller exercises
    `redact_sentence`: the report renderer is TypeScript. The corpus is what holds the three
    languages to each other, and an implementation that exists only in the language that needs it
    cannot be held to anything.
    """
    assert redact_http(case["input"]) == case["expect"]


def test_http_redaction_is_idempotent_over_the_whole_section() -> None:
    """A stored snapshot can be re-rendered, so applying the rule to its own output must change
    nothing. The marker carries brackets that no credential pattern here accepts, which is what
    makes that true rather than lucky."""
    for case in DOC["http_headers"]:
        once = redact_http(case["input"])
        assert redact_http(once) == once, f"not idempotent: {case['why'][:60]}"
