"""The Checkpoint an actor carries in its Temporal activity heartbeat.

WHY THIS EXISTS. The record of which Units had committed lived in Redis, in a hash under a 24 h TTL
that `maxmemory-policy volatile-lru` evicted FIRST under memory pressure (ADR 0059). Losing it is
not a slowdown: a retry reads an absent commit map and re-runs finished work, or reads a
partly-evicted one and skips work that never ran. Nothing raises either way.

A heartbeat's details live in the activity's own history. Temporal hands them back to the next
attempt (`activity.info().heartbeat_details`), so the commit map travels with the retry instead of
being looked up in a cache that may have dropped it.

`done` IS A RANGE SET, AND THAT IS A SIZE DECISION. Heartbeat details are a Temporal payload with a
size limit, so a per-unit list of 10,000 integers is a batch-size ceiling wearing a different hat.
One contiguous run is one pair.

CANONICAL FORM IS PART OF THE CONTRACT: inclusive `[lo, hi]` pairs, sorted ascending, merged so no
two ranges touch or overlap. The Go peer (`runtime/go/checkpoint`) and the orchestrator
(`control/orchestrator/src/checkpoint.ts`) encode the same way, and
`shared/conformance/checkpoint.json` is what holds all three to it — two implementations that agree
on membership and disagree on encoding produce different bytes for the same facts, and this
repository has already shipped that bug once (see `shared/conformance/README.md`).
"""

from __future__ import annotations

#: The only version this reader understands. A checkpoint that does not say `1` is discarded whole.
VERSION = 1


class RangeSet:
    """A set of unit indices, stored as merged inclusive ranges.

    Mutable and append-oriented, because that is how the engine uses it: units commit one at a
    time, in no guaranteed order, and the set is re-encoded into every heartbeat.
    """

    __slots__ = ("_r",)

    def __init__(self, ranges=None):
        # Normalised on the way in, so a RangeSet is canonical at every moment rather than only
        # after an explicit call somebody can forget.
        self._r: list[list[int]] = _merge([[int(a), int(b)] for a, b in (ranges or [])])

    def add(self, i: int) -> None:
        """Record one index. Re-adding a member is a no-op — a retry re-commits what it re-ran."""
        self._r = _merge(self._r + [[int(i), int(i)]])

    def __contains__(self, i: int) -> bool:
        # Linear rather than bisecting: a batch has a handful of ranges, not thousands, and the
        # bisect version of this was the one place an off-by-one could hide silently.
        return any(lo <= i <= hi for lo, hi in self._r)

    def __len__(self) -> int:
        """How many indices the set holds — NOT how many ranges."""
        return sum(hi - lo + 1 for lo, hi in self._r)

    def ranges(self) -> list[list[int]]:
        """The canonical encoding, as it goes into a heartbeat."""
        return [[lo, hi] for lo, hi in self._r]

    def __repr__(self) -> str:
        return f"RangeSet({self._r!r})"


def _merge(pairs: list[list[int]]) -> list[list[int]]:
    """Sort and coalesce. ADJACENCY COUNTS: [0,2] and [3,5] touch, so they become [0,5].

    Without that, `add` one index at a time would produce one range per index and the compaction
    this type exists for would never happen.
    """
    if not pairs:
        return []
    out: list[list[int]] = []
    for lo, hi in sorted(pairs):
        if out and lo <= out[-1][1] + 1:
            if hi > out[-1][1]:
                out[-1][1] = hi
        else:
            out.append([lo, hi])
    return out


class Checkpoint:
    """How far one Batch got, as the heartbeat carries it.

    `batch_id` IS A GUARD AND NOT A LABEL. Unit indices are positions WITHIN ONE BATCH, so applying
    a checkpoint from a different batch would skip units by index in a batch that never ran them.
    `resume_from` discards a mismatch outright rather than taking the parts that look plausible.
    """

    __slots__ = ("v", "batch_id", "done", "failed", "manifest_ref")

    def __init__(self, batch_id="", done=None, failed=None, manifest_ref=""):
        self.v = VERSION
        self.batch_id = batch_id
        self.done = done if isinstance(done, RangeSet) else RangeSet(done)
        # A set, not a RangeSet: isolated units are few and scattered by nature, so ranges would
        # cost more than they save and would imply a contiguity that does not exist.
        self.failed = set(int(i) for i in (failed or []))
        self.manifest_ref = manifest_ref

    def commit(self, i: int) -> None:
        self.done.add(i)

    def isolate(self, i: int) -> None:
        self.failed.add(int(i))

    def to_details(self) -> dict:
        """The heartbeat payload. FIELD NAMES ARE THE CONTRACT — see the Go and TS peers."""
        return {
            "v": self.v,
            "batch_id": self.batch_id,
            "done": self.done.ranges(),
            "failed": sorted(self.failed),
            "manifest_ref": self.manifest_ref,
        }

    @classmethod
    def from_details(cls, d) -> "Checkpoint | None":
        """Decode one heartbeat payload, or `None` if it may not be trusted.

        REFUSED RATHER THAN PARTIALLY READ. A version this code does not know may have moved a
        field's meaning, and a reader that ignores what it does not recognise resumes from a
        checkpoint it only half understood. `None` means "start from the beginning", which is
        always safe: the work is idempotent by position.
        """
        if not isinstance(d, dict):
            return None
        if d.get("v") != VERSION:
            return None
        return cls(
            batch_id=d.get("batch_id") or "",
            done=d.get("done") or [],
            failed=d.get("failed") or [],
            manifest_ref=d.get("manifest_ref") or "",
        )


def accepted(details, batch_id: str) -> "Checkpoint | None":
    """The checkpoint in `details` if a resume of `batch_id` may act on it, else `None`.

    ONE RULE, TWO READERS. `resume_from` below answers "which units are left" and the engine also
    needs "which finished, and where are their outputs" (`manifest_ref`). Both come through here, so
    the engine cannot fold back a checkpoint that `resume_from` — and the corpus — would discard.

    AN EMPTY `batch_id` MATCHES NOTHING, including another empty one. An unidentified checkpoint is
    not evidence about any particular batch, and treating two blanks as equal is how a checkpoint
    from an unrelated dispatch gets applied.
    """
    ck = Checkpoint.from_details(details)
    if ck is None or not batch_id or ck.batch_id != batch_id:
        return None
    return ck


def resume_from(details, batch_id: str, units: int) -> list[int]:
    """Which unit indices still need running, given what a previous attempt reported.

    `details` is whatever Temporal handed back (`None` on a first attempt). The result is every
    index in `[0, units)` that is neither committed nor isolated — or all of them, when `accepted`
    refuses the checkpoint.
    """
    todo = list(range(max(int(units), 0)))
    ck = accepted(details, batch_id)
    if ck is None:
        return todo
    return [i for i in todo if i not in ck.done and i not in ck.failed]
