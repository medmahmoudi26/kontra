"""`progress` — the declared fact, and the fields it actually puts on the record (issue 06).

WHAT THIS IS PROTECTING. The console's log rail filters on `incomplete` independently of level, and
it has been doing so against a schema no code defined — 68 hand-built logger calls across six
workflows, spelling `incomplete`/`axis`/`phase`/`program` by hand. These tests hold the declaration
to those names, because renaming one of them here silently unhooks a filter over there.

THE ASSERTIONS ARE ON `record.__dict__`, NOT ON THE MESSAGE. The sentence is for a person; the
fields are what `runtime/python/internals/logs.py` turns into indexed VictoriaLogs keys, and they
are the half that makes this a fact rather than a line. A test that only checked the rendered string
would pass with `extra=` dropped entirely — which is a mistake this module made once already.
"""
from __future__ import annotations

import logging

import pytest

from kontra.facts import progress


class Capture(logging.Handler):
    """Every record, kept whole — `__dict__` and all."""

    def __init__(self) -> None:
        super().__init__()
        self.records: list[logging.LogRecord] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.records.append(record)


@pytest.fixture()
def caught() -> Capture:
    log = logging.getLogger("kontra.progress")
    handler = Capture()
    log.addHandler(handler)
    log.setLevel(logging.INFO)
    # Outside a workflow the module logger is what `progress` resolves to, and `propagate` left on
    # would also hand these to pytest's root capture — harmless, but it makes a failure noisier
    # than the thing being asserted.
    previous, log.propagate = log.propagate, False
    yield handler
    log.propagate = previous
    log.removeHandler(handler)


def fields(record: logging.LogRecord) -> dict:
    """Just what `progress` put there — the ~18 stdlib attributes are not the subject."""
    standard = set(logging.LogRecord("", 0, "", 0, "", None, None).__dict__)
    return {k: v for k, v in record.__dict__.items() if k not in standard and not k.startswith("_")}


def test_emits_the_declared_fields_as_structured_extras(caught: Capture) -> None:
    progress("crawl", "pages", done=12, total=380, program="acme")

    assert len(caught.records) == 1
    assert fields(caught.records[0]) == {
        "phase": "crawl",
        "axis": "pages",
        "done": 12,
        "total": 380,
        "program": "acme",
    }


def test_reads_as_a_sentence_too(caught: Capture) -> None:
    """The rail is a list of lines before it is a set of fields."""
    progress("crawl", "pages", done=12, total=380, program="acme")
    assert caught.records[0].getMessage() == "[acme] crawl/pages: 12/380"


def test_the_denominator_can_arrive_before_the_work(caught: Capture) -> None:
    """`total` with no `done` is a complete call — it is how a reader who opens the rail at second
    zero learns how big the thing is. Said as a quantity, because "none done yet" and "this is how
    big it is" are different claims and only the second is being made."""
    progress("crawl", "pages", total=380, program="acme")

    assert fields(caught.records[0]) == {"phase": "crawl", "axis": "pages", "total": 380, "program": "acme"}
    assert caught.records[0].getMessage() == "[acme] crawl/pages: 380 to do"


def test_incomplete_raises_the_level_and_sets_the_field_the_rail_filters_on(caught: Capture) -> None:
    progress("crawl", "pages", done=9, total=380, incomplete=True, program="acme", detail="seed_limit reached")

    record = caught.records[0]
    assert record.levelno == logging.WARNING
    assert fields(record)["incomplete"] is True
    assert "INCOMPLETE" in record.getMessage()
    assert "seed_limit reached" in record.getMessage()


def test_incomplete_is_absent_rather_than_false_on_an_ordinary_fact(caught: Capture) -> None:
    """The rail asks whether the key is SET. Writing `false` on every line would mean the filter
    had to exclude it instead, and would triple the cardinality of the one field it reads."""
    progress("crawl", "pages", done=1, total=2, program="acme")
    assert "incomplete" not in fields(caught.records[0])


def test_author_fields_ride_along(caught: Capture) -> None:
    """`lane` and `seed_limit` are two of the names the hand-built lines already carried."""
    progress("crawl", "pages", done=1, total=2, program="acme", lane=3, seed_limit=500)

    got = fields(caught.records[0])
    assert got["lane"] == 3
    assert got["seed_limit"] == 500


def test_python_itself_refuses_a_shadowed_declared_field() -> None:
    """MEASURED, and it is why there is no guard for this in the module: every declared name is a
    real parameter, so a `**` collision is a TypeError before `progress` runs. An earlier version
    filtered these out by hand, in a branch that could never be reached."""
    with pytest.raises(TypeError, match="multiple values"):
        progress("crawl", "pages", total=2, program="acme", **{"total": 999})


def test_a_non_scalar_author_field_is_dropped(caught: Capture) -> None:
    """The formatter emits scalars only, so a dict built here would be dropped there — a field
    that silently never arrives is worse than one that was never offered."""
    progress("crawl", "pages", done=1, program="acme", hosts=["a", "b"], lane=2)

    got = fields(caught.records[0])
    assert "hosts" not in got
    assert got["lane"] == 2


@pytest.mark.parametrize(
    "bad",
    [
        {"done": "twelve"},
        {"done": None, "total": object()},
        {"done": -1},
        {"done": True},  # `int(True)` is 1, which would render as real progress
    ],
)
def test_a_value_that_is_not_a_count_is_dropped_not_rendered(caught: Capture, bad: dict) -> None:
    progress("crawl", "pages", program="acme", **bad)

    got = fields(caught.records[0])
    assert "done" not in got or isinstance(got["done"], int)
    assert got.get("done") is not True


def test_never_raises_whatever_it_is_given(caught: Capture) -> None:
    """A progress fact is an aside. A call that gets its own arguments wrong must not be the thing
    that fails a Run that is otherwise working."""
    progress(None, None, done=object(), total=object(), program=object())  # type: ignore[arg-type]
    progress("", "", program="acme")


def test_it_is_reachable_from_the_package_and_is_the_same_function() -> None:
    """A surface that names something it will not provide is worse than one that never mentioned
    it — the mistake `speak` made in `_VERBS` and `__all__`."""
    import kontra

    assert kontra.progress is progress
    assert "progress" in kontra.__all__
    assert "progress" in dir(kontra)
