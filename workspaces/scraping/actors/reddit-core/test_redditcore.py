"""redditcore tests — the parts that are wrong silently, with no browser and no network.

Everything here is transport-free, which is the point of the split: `_search` and
`_fetch_comments` used to be reachable only through a live Camoufox, so the paging rules and the
comment-tree assembly were never covered. A fake transport makes them ordinary functions.

    .venv/bin/python -m pytest examples/private/reddit-core/ -q
"""

import asyncio
import sys
from dataclasses import fields
from pathlib import Path

import pytest

sys.path[:0] = [str(Path(__file__).resolve().parents[3]), str(Path(__file__).resolve().parent)]

import kontra  # noqa: E402
import redditcore as rc  # noqa: E402

_lib = sys.modules[kontra.actor.__class__.__module__]


@pytest.fixture(autouse=True)
def _bound_params():
    """`param.get()` returns a lazy ParamRef outside a hosted run — binding an empty map is what
    a real turn does, and without it every default reads back as a ref."""
    token = _lib._run_params.set({})
    yield
    _lib._run_params.reset(token)


class Fake(rc.RedditCore):
    """A transport that returns canned documents. Everything above `_fetch_json` is real."""

    BASE = "https://x"
    JSON_SUFFIX = ".json"

    def __init__(self, responses):
        self.responses = list(responses)
        self.urls = []
        self.run_id = "r"
        self._compile_taxonomy()

    async def _fetch_json(self, url, attempts=None):
        self.urls.append(url)
        r = self.responses.pop(0) if self.responses else None
        if isinstance(r, Exception):
            raise r
        return r


def _listing(children):
    return {"data": {"children": [{"data": c} for c in children], "after": None}}


def _post(pid, title="moving off wordpress", body="we switch from wordpress to webflow", score=10):
    return {"id": pid, "title": title, "selftext": body, "author": "u", "score": score,
            "created_utc": 1700000000, "upvote_ratio": 0.9, "num_comments": 3,
            "link_flair_text": "f", "domain": "self.webdev", "permalink": f"/r/webdev/{pid}"}


# --- taxonomy -----------------------------------------------------------------------------


def test_words_accepts_a_string_or_a_list():
    assert rc._words("a, B ,c") == ["a", "b", "c"]
    assert rc._words(["A", " b "]) == ["a", "b"]
    assert rc._words("") == []


def test_an_ambiguous_word_alone_is_not_a_hit_in_a_general_sub():
    # "payload" in r/webdev is an HTTP payload far more often than the CMS.
    assert Fake([])._hits("the payload was malformed", is_platform=False) == []


def test_the_same_word_is_a_hit_with_cms_context_nearby():
    assert "payload" in Fake([])._hits("moving my blog to payload next week", is_platform=False)


def test_a_platform_sub_needs_no_context():
    assert "payload" in Fake([])._hits("the payload was malformed", is_platform=True)


def test_buckets_split_one_text_three_ways():
    plat, intent, topic = Fake([])._buckets_for(False)(
        "thinking of switch from webflow to a headless cms")
    assert plat == ["webflow"] and intent == ["switch from"]
    # Overlapping keywords both hit — `cms` matches inside `headless cms`. Prototype behaviour,
    # pinned deliberately: the taxonomy is a client-tunable param, and a change that started
    # dropping the broader term would quietly narrow every topic filter they have.
    assert topic == ["cms", "headless cms"]


def test_iso_of_never_captured_is_empty_not_the_epoch():
    assert rc._iso(None) == "" and rc._iso(0) == ""
    assert rc._iso(1700000000).endswith("Z")


# --- block detection (measured live, 2026-08-24) ----------------------------------------------


def test_the_real_block_page_is_detected():
    # THE ACTUAL PAGE, from a blocked DigitalOcean egress IP. 797 chars and the wording is
    # "network policy" — so the prototype's `len(txt) < 500` and its "network security" sentinel
    # BOTH sailed past it, and only the .json probe noticed.
    assert rc._is_block_page("whoa there, pardner!\n\nYour request has been blocked due to a "
                             "network policy.")


def test_the_older_wording_is_still_detected():
    assert rc._is_block_page("you have been blocked by network security")


def test_ordinary_content_is_not_a_block():
    assert not rc._is_block_page("a thread about migrating off wordpress")
    assert not rc._is_block_page("")


# --- search paging (was unreachable without a browser) ------------------------------------------


def test_a_platform_sub_lists_new_and_keeps_everything():
    f = Fake([_listing([_post("a", title="unrelated", body="nothing on topic")])])
    got = asyncio.run(f._search("webflow", is_platform=True))
    assert [p["post_id"] for p in got] == ["a"]
    assert "/r/webflow/new.json" in f.urls[0]


def test_a_general_sub_re_checks_the_hit_locally():
    # Reddit's search matches loosely. A post that came back for "payload" but carries no CMS
    # context must be dropped HERE, or r/webdev fills the dataset with HTTP payloads.
    f = Fake([_listing([_post("a", title="bad request", body="the payload was malformed")])]
             + [None] * 40)
    assert asyncio.run(f._search("webdev", is_platform=False)) == []


def test_low_scoring_posts_are_dropped():
    f = Fake([_listing([_post("a", score=1), _post("b", score=9)])])
    got = asyncio.run(f._search("webflow", is_platform=True))
    assert [p["post_id"] for p in got] == ["b"]        # min_post_score defaults to 2


def test_the_per_sub_cap_is_enforced():
    f = Fake([_listing([_post(str(i)) for i in range(20)])])
    assert len(asyncio.run(f._search("webflow", is_platform=True))) == 8   # max_posts_per_sub


def test_a_dead_search_skips_the_keyword_not_the_subreddit():
    # Losing one keyword is a coverage gap; losing the subreddit loses every other keyword too.
    f = Fake([RuntimeError("net")] + [_listing([_post("a")])] + [None] * 40)
    got = asyncio.run(f._search("webdev", is_platform=False))
    assert [p["post_id"] for p in got] == ["a"]


def test_a_block_during_search_propagates():
    f = Fake([rc.Blocked("reddit block")])
    with pytest.raises(rc.Blocked):
        asyncio.run(f._search("webflow", is_platform=True))


def test_post_row_matches_the_declared_post_schema():
    row = Fake([])._post_row(_post("x1"), "webdev", False)
    assert set(row) == {f.name for f in fields(rc.Post)}


# --- comment tree -------------------------------------------------------------------------------


def _c(cid, body, replies=None, **kw):
    d = {"kind": "t1", "data": {"id": cid, "name": f"t1_{cid}", "body": body, "author": "u",
                                "score": 5, "created_utc": 1700000000,
                                "permalink": f"/c/{cid}", **kw}}
    if replies:
        d["data"]["replies"] = {"data": {"children": replies}}
    return d


def test_the_comment_tree_nests_and_counts_every_node():
    tree = [{"data": {"children": []}},
            {"data": {"children": [_c("a", "a", [_c("b", "b", [_c("c", "c")])])]}}]
    f = Fake([tree])
    nodes, total = asyncio.run(f._fetch_comments("webdev", "p1"))
    assert total == 3                       # every node, not just top level
    assert len(nodes) == 1 and nodes[0]["replies"][0]["replies"][0]["body"] == "c"


def test_deleted_comments_are_dropped_not_emitted_as_markers():
    tree = [{"data": {"children": []}},
            {"data": {"children": [_c("a", "[deleted]"), _c("b", "real")]}}]
    nodes, total = asyncio.run(Fake([tree])._fetch_comments("webdev", "p1"))
    assert [n["body"] for n in nodes] == ["real"] and total == 1


def test_op_and_mod_comments_are_flagged():
    tree = [{"data": {"children": []}},
            {"data": {"children": [_c("a", "x", is_submitter=True),
                                   _c("b", "y", distinguished="moderator")]}}]
    nodes, _ = asyncio.run(Fake([tree])._fetch_comments("webdev", "p1"))
    assert nodes[0]["is_op"] and nodes[1]["is_mod"]


def test_reddits_own_order_is_preserved():
    # ?sort=top already ranks these. Re-sorting by raw score would compare a depth-3 reply with a
    # top-level comment as peers, which is how a sub-comment becomes "the thread's best".
    tree = [{"data": {"children": []}},
            {"data": {"children": [_c("a", "first", score=1), _c("b", "second", score=99)]}}]
    nodes, _ = asyncio.run(Fake([tree])._fetch_comments("webdev", "p1"))
    assert [n["body"] for n in nodes] == ["first", "second"]


# --- row building ---------------------------------------------------------------------------------


def _thread(**over):
    post = {"post_id": "abc123", "subreddit": "webdev", "title": "moving off wordpress",
            "text": "we switch from wordpress to webflow", "author": "someone",
            "created_utc": "2026-01-01T00:00:00Z", "score": 12, "upvote_ratio": 0.9,
            "num_comments": 40, "flair": "discussion", "domain": "self.webdev",
            "permalink": "https://reddit.com/r/webdev/x",
            "tier": "general", "platforms": "wordpress,webflow", "intent": "switch from",
            "topic": "", "run_id": "r"}
    post.update(over)
    return post


def _node(cid, body, replies=(), depth=0):
    kids = [dict(c, depth=depth + 1) for c in replies]
    return {"id": cid, "body": body, "author": "u", "created_utc": 1700000000,
            "permalink": "/r/webdev/c", "is_op": False, "is_mod": False, "score": 3,
            "depth": depth, "replies": kids}


def _rows(comments, **kw):
    kw.setdefault("total", 3)
    kw.setdefault("failed", False)
    return rc._rows_for_thread(_thread(), comments, run_id="r", tier="general",
                               buckets=Fake([])._buckets_for(False), **kw)


def test_comments_captured_counts_nested_replies_not_just_top_level():
    # The classic off-by-a-subtree. len(toplevel) says 1 here; the answer is 3.
    rows = _rows([_node("t1_a", "a", [_node("t1_b", "b", [_node("t1_c", "c")])])])
    assert rows[0]["comments_captured"] == 3 and len(rows) == 4


def test_depth_and_tree_path_describe_position():
    rows = _rows([_node("t1_a", "a", [_node("t1_b", "b")]), _node("t1_d", "d")])
    by = {r["doc_id"]: r for r in rows}
    assert by["t3_abc123"]["depth"] == 0 and by["t3_abc123"]["tree_path"] is None
    assert by["t1_a"]["depth"] == 1 and by["t1_a"]["tree_path"] == "0"
    assert by["t1_b"]["depth"] == 2 and by["t1_b"]["tree_path"] == "0.0"
    assert by["t1_d"]["tree_path"] == "1" and by["t1_b"]["parent_id"] == "t1_a"


def test_a_failed_fetch_is_distinguishable_from_an_empty_thread():
    # THE point of the three counts. Both have zero comments; only the flags say which is a hole.
    hole = rc._rows_for_thread(_thread(num_comments=40), [], total=0, failed=True, run_id="r",
                               tier="general", buckets=Fake([])._buckets_for(False))[0]
    empty = rc._rows_for_thread(_thread(num_comments=0), [], total=0, failed=False, run_id="r",
                                tier="general", buckets=Fake([])._buckets_for(False))[0]
    assert hole["fetch_failed"] and hole["num_comments"] == 40 and hole["comments_fetched"] == 0
    assert not empty["fetch_failed"] and empty["num_comments"] == 0


def test_post_only_fields_are_null_on_comment_rows_not_missing():
    comment = _rows([_node("t1_a", "a")])[1]
    for f in rc._POST_ONLY:
        assert f in comment, f"{f} missing entirely — a schema change, not a null"
        assert comment[f] is None, f


def test_a_deleted_body_becomes_empty_not_the_literal_marker():
    rows = rc._rows_for_thread(_thread(text="[deleted]"), [], total=0, failed=False, run_id="r",
                               tier="general", buckets=Fake([])._buckets_for(False))
    assert rows[0]["text"] == "" and rows[0]["word_count"] == 0


def test_every_document_row_carries_the_full_declared_shape():
    declared = {f.name for f in fields(rc.Document)}
    for row in _rows([_node("t1_a", "a")]):
        assert set(row) == declared, set(row) ^ declared


# --- terminal errors must not read as "nothing found" ---------------------------------------


def test_a_terminal_error_mid_search_propagates_instead_of_emptying_the_subreddit():
    """THE BUG THIS PINS, measured on `redditscrape-1787584680`.

    `_search` caught every non-Blocked exception and `break`-ed to the next keyword, on the
    reasoning that one bad keyword should not cost the subreddit. A NonRetryableError is not a
    bad keyword — it is the transport saying this call can never succeed. An OAuth actor whose
    credential Reddit refuses raises it on EVERY keyword, so a general subreddit re-minted a
    dead token once per keyword (202 auth POSTs from a 3-Unit Batch) and then reported zero
    posts. The Run went green with an empty Dataset and no failures: "found nothing" and
    "could not ask" had become the same row.
    """
    f = Fake([kontra.NonRetryableError("reddit refused the credentials (HTTP 401)")])
    with pytest.raises(kontra.NonRetryableError):
        asyncio.run(f._search("webdev", is_platform=False))

    # And it stopped at the FIRST keyword rather than retrying the dead credential once per
    # keyword — the request storm is the part that reaches Reddit.
    assert len(f.urls) == 1, f"asked {len(f.urls)} times with a credential known to be refused"


def test_an_ordinary_error_mid_search_still_only_costs_that_keyword():
    """The other half: the original behaviour must survive for what it was written for. One
    keyword that errors is skipped, the rest of the sweep runs, and the subreddit still emits."""
    f = Fake([RuntimeError("flaky"), _listing([_post("p1")])])
    posts = asyncio.run(f._search("webdev", is_platform=False))
    assert [p["post_id"] for p in posts] == ["p1"]


def test_a_terminal_error_fetching_comments_is_not_recorded_as_fetch_failed():
    """`fetch_failed=True` means a fetch was ATTEMPTED and lost — the distinction the three
    comment counts exist to preserve. A terminal error means we never got to ask, and writing
    the post row with `comments_fetched=0` beside it puts that on the wrong side of the line."""
    f = Fake([kontra.NonRetryableError("reddit refused the credentials (HTTP 401)")])
    with pytest.raises(kontra.NonRetryableError):
        asyncio.run(f._fetch_comments("webdev", "p1"))
