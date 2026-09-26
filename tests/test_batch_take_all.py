"""`take_all()` and the mix it now refuses — GitHub #23.

THE BUG WAS THAT NOTHING FAILED. `batch.units` reads like a length and is a mutation: it hands out
every Unit. A Method that measured it before looping got an empty `async for`, ran no author code,
committed, and returned SUCCESSFULLY. Measured on `canary-1789931619` — `20/20 unit(s), 0 beat(s)`,
COMPLETED in 17 seconds where the work takes 80, and the only symptom was an output dataset that
stayed empty, which is indistinguishable from a filter that matched nothing.

The two legitimate shapes — loop, or take-and-gather — have to keep working untouched, because the
same expression is idiomatic in one Method and a silent no-op in the other; that is the whole trap,
and a fix that broke the good half would be worse than the bug.

SYNC TESTS DRIVING `asyncio.run`, which is this suite's convention (`test_output_dataset.py`,
`test_actor_engine.py`) — there is no pytest-asyncio in the project venv, and a file that needed one
would pass on a developer's host python and fail in CI.
"""

import asyncio

import pytest

from kontra.batch import BatchAlreadyTaken
from kontra.testing import stub_batch


async def _drain(batch) -> list:
    return [unit.value async for unit in batch]


def test_plain_loop_still_works():
    """The ordinary shape. Nothing about this changed, and an exhausted loop is not an error."""
    assert asyncio.run(_drain(stub_batch(["a", "b", "c"]))) == ["a", "b", "c"]


def test_take_all_still_hands_out_everything():
    """The concurrent shape — `asyncio.gather(*(one(u) for u in batch.take_all()))`. Untouched."""
    batch = stub_batch(["a", "b", "c"])
    units = batch.take_all()
    assert [u.value for u in units] == ["a", "b", "c"]
    assert batch.pending == 0


def test_units_is_still_the_same_call():
    """The deprecated alias keeps working: `webcrawl` and the engine tests call it, and breaking a
    working Method to fix a naming mistake is the wrong trade."""
    assert [u.value for u in stub_batch(["a", "b"]).units] == ["a", "b"]


def test_take_then_loop_RAISES_instead_of_running_zero_times():
    """THE REGRESSION. This exact shape used to complete successfully having done nothing."""

    async def go():
        batch = stub_batch(["a", "b", "c"])
        of = len(batch.take_all())
        assert of == 3
        async for _unit in batch:
            pytest.fail("the loop body must not run — the Units were already taken")

    with pytest.raises(BatchAlreadyTaken) as err:
        asyncio.run(go())
    # The message has to name both halves, because the fix is to pick one.
    assert "take_all()" in str(err.value)
    assert "async for" in str(err.value)


def test_the_units_alias_is_refused_the_same_way():
    """`len(batch.units)` before a loop is the literal line from the issue."""

    async def go():
        batch = stub_batch(["a", "b", "c"])
        _of = len(batch.units)
        async for _unit in batch:
            pytest.fail("the loop body must not run")

    with pytest.raises(BatchAlreadyTaken):
        asyncio.run(go())


def test_an_empty_batch_is_not_an_error():
    """Nothing to take and nothing to iterate is an ordinary empty Batch, NOT a misuse. A check
    keyed on `_pending` alone could not tell this from the case above, which is why there is a flag."""
    assert asyncio.run(_drain(stub_batch([]))) == []


def test_take_all_on_an_empty_batch_then_looping_is_NOT_refused():
    """Taking nothing is not taking. An author who calls `take_all()` on an empty Batch and then
    loops has not made the mistake — there were no Units to lose.

    NO RAISE: the flag is never set, so the loop finds the same nothing it would have found anyway.
    Raising here would fail a Method that is correct on every input except the empty one, which is
    a worse bug than the one this guard catches and only ever reproduces on the input nobody tests
    with.
    """
    batch = stub_batch([])
    assert batch.take_all() == []
    assert asyncio.run(_drain(batch)) == []


def test_pending_is_the_non_destructive_way_to_ask_the_size():
    """The workaround the issue names, made first-class: read the size WITHOUT taking the Batch."""
    batch = stub_batch(["a", "b", "c"])
    assert batch.pending == 3
    # ...and the loop still gets everything, which is the entire point of it being non-destructive.
    assert asyncio.run(_drain(batch)) == ["a", "b", "c"]
