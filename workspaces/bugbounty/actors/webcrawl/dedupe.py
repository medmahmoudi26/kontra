"""Near-duplicate suppression in REQUEST space, for the crawl frontier.

── THIS IS A SECOND COPY, AND THAT IS A DELIBERATE, PINNED CHOICE ──────────────────────────────

Every function below is lifted verbatim from `crawl4ai`'s actor (which took it from the v1 proxy
addon at legacy/workers/proxy/addon/fingerprint.py). It is copied rather than imported because
the two actors ship as separate images in separate workspaces and share no package — and it is
COPIED rather than re-derived because `simhash64`'s own docstring records why re-deriving is the
wrong move: a subtly wrong similarity function "does not raise, it returns plausible small
numbers and reads as a clean result", and a hand-rolled Go sibling was blind to an injected
script tag on any page over ~200 words for a whole live sweep before anyone noticed.

`test_dedupe.py` pins this copy to the SAME golden fixtures crawl4ai's tests use. If the two ever
disagree, that test fails here rather than a crawl quietly deduping differently in each actor.

── WHY REQUEST SPACE, AND WHY THE RADIUS IS SMALL ──────────────────────────────────────────────

The fingerprint is over `method host path?param-names [header-names] body`, not over the page
body: two pages can render differently and still be one thing to probe, and two byte-identical
pages behind different endpoints are two things to probe.

`simhash_distance` defaults to 3 and MUST stay near there. Measured over these strings with
object ids masked: a sibling route is ~16 bits, a different METHOD ~11, a different HOST ~15. So
any radius wide enough to merge two sibling pages ALSO merges GET with POST and merges two hosts.
There is no safe radius above ~10.
"""
from __future__ import annotations

import re
from urllib.parse import parse_qsl, quote, urlparse

from simhash import Simhash as _Simhash


def normalize_host(raw_host: str) -> str:
    """Lowercase; strip a trailing port. Matches v1.extract_domain."""
    host = (raw_host or "").strip().lower()
    if ":" in host and not host.startswith("["):
        host = host.rsplit(":", 1)[0]
    return host


def _canonical_parts(method: str, url: str, header_names=(), body: str = "") -> tuple:
    parsed = urlparse(url)
    names = sorted({quote(n, safe="")
                    for n, _ in parse_qsl(parsed.query, keep_blank_values=True)})
    hdrs = sorted({str(h).strip().lower() for h in (header_names or ())})
    return (str(method or "GET").upper(), normalize_host(parsed.hostname or ""),
            parsed.path or "/", names, hdrs, body)


def _assemble(method: str, host: str, path: str, names, hdrs, body: str) -> str:
    if names:
        path = f"{path}?{'&'.join(names)}"
    return f"{method} {host} {path} {hdrs} {body}"


def canonical_string(method: str, url: str, header_names=(), body: str = "") -> str:
    """`method host path?names [header names] body` — the exact v1 layout, including the Python
    list repr of the header names. Kept byte-for-byte so the string this actor hashes is the
    same one the v1 proxy addon builds for the same request."""
    return _assemble(*_canonical_parts(method, url, header_names, body))


# A path segment that is an OBJECT ID rather than a route. Masking these before hashing is what
# makes a small Hamming radius mean anything: measured over these canonical strings, a raw
# simhash puts /product/1001 and /product/1002 ~9 bits apart and two unrelated endpoints ~13
# apart, so no radius separates them — a 64-bit simhash simply does not concentrate over a
# ~50-character string. With ids masked the same pair is 0 bits apart and everything structural
# (a different route 11+, host 15, method 13, header set 20) stays far outside the radius.
#
# WHOLE segments only, and only three unambiguous shapes — a run of digits, a hex blob that
# contains a digit (`9f3a2b1c`; the digit requirement keeps `/decade` and `/facade` intact), and
# a uuid. Deliberately NOT masked: partially-numeric slugs (`story-77`) and versions (`v2`,
# `v10`). The errors are not symmetric — masking too much MERGES two distinct endpoints and the
# second one is never probed, while masking too little only costs one extra unit — so the
# leftover variability is left to the radius knob instead.
_ID_SEGMENT_RE = re.compile(
    r"^(?:\d+|(?=[0-9a-f]*\d)[0-9a-f]{6,}|[0-9a-f]{8}-[0-9a-f-]{4,})$", re.IGNORECASE)


def mask_object_ids(path: str) -> str:
    return "/".join("#" if _ID_SEGMENT_RE.match(seg) else seg for seg in path.split("/"))


def skeleton_string(method: str, url: str, header_names=(), body: str = "") -> str:
    """The canonical string with object-id path segments masked — the string actually hashed."""
    method, host, path, names, hdrs, body = _canonical_parts(method, url, header_names, body)
    return _assemble(method, host, mask_object_ids(path), names, hdrs, body)


# Character n-grams, not words: what is left to collapse after masking differs by a few
# characters INSIDE a path segment (/settings/email vs /settings/phone), and a word tokenizer
# splitting on `/` would make those two whole tokens with nothing in common. Overlapping 4-grams
# share every window that does not straddle the change, so such a pair lands ~6 bits apart —
# inside a radius of 6-8, outside the default of 3. Above ~10 the radius starts eating genuinely
# distinct endpoints (a different method is 13 bits, a different host 15), so do not go there.
_MASK64 = (1 << 64) - 1


def simhash64(text: str) -> int:
    """64-bit Charikar simhash, from the `simhash` package rather than hand-rolled.

    This was a local implementation until measurement showed why that is a bad idea for a
    similarity function: a subtly wrong one does not raise, it returns plausible small numbers
    and reads as a clean result. The Go sibling's hand-rolled version turned out to be blind to
    an injected script tag on any page over ~200 words, and nothing caught it for a whole live
    sweep. A library that many people run is the right dependency here.

    Empty text is pinned to 0. The library fingerprints the empty string to a non-zero constant,
    which would make every empty request shape a near-duplicate of every other and silently
    suppress them as a group."""
    if not text:
        return 0
    return int(_Simhash(text).value)


def hamming_distance(a: int, b: int) -> int:
    return int(_Simhash(int(a)).distance(_Simhash(int(b))))


def nearest(fingerprint: int, seen, distance: int):
    """The first seen fingerprint within `distance`, or None.

    WHICH shape matched, not merely whether one did. `near_duplicate` answers the yes/no question
    and is all a caller needs to SKIP a duplicate; a caller that wants to allow N pages per path
    shape has to be able to count them, and it can only count against a stable representative.

    Linear scan — the seen set is capped at _SEEN_CAP, so an index (banded LSH) would cost more
    code than the few hundred XORs it saves.
    """
    for other in seen:
        if hamming_distance(fingerprint, other) <= distance:
            return other
    return None


def near_duplicate(fingerprint: int, seen, distance: int) -> bool:
    """Whether anything within `distance` has been seen. Kept as the yes/no form because it is
    what the tests and any skip-only caller read; it is `nearest(...) is not None` and must stay
    exactly that, or two callers disagree about what a duplicate is."""
    return nearest(fingerprint, seen, distance) is not None


def request_fingerprint(method: str, url: str, header_names=()) -> int:
    """The fingerprint of a URL the frontier is considering, before it is fetched.

    crawl4ai fingerprints a page it ALREADY has, so it can use the browser's own captured request
    and its real header names. The frontier has only a link, so header names are empty here —
    which is consistent for every candidate and therefore still separates routes, hosts and
    methods exactly as measured.
    """
    return simhash64(skeleton_string(method or "GET", url, header_names))
