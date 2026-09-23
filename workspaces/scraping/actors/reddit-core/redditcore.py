"""redditcore — everything about Reddit that is not TRANSPORT.

TWO ACTORS SHARE THIS FILE. `reddit` drives a Camoufox browser and reads `old.reddit.com/...json`;
`redditapi` holds an OAuth bearer token and reads `oauth.reddit.com/...`. They differ in exactly
three things — a base URL, whether paths carry a `.json` suffix, and how one document is fetched —
and in nothing else. The taxonomy, the search paging, the comment tree, the "load more" expansion
and the row builders are identical, so they live here once.

HOW IT IS SHARED. `redditcore.py` is a SYMLINK in each actor directory pointing at this file.
`cli/bundle.go:addTree` reads every entry with `os.ReadFile` and writes it as a regular file, so
each Bundle ships its own real copy while the checkout keeps one editable original. The
alternative — two copies — is two copies that drift, and the thing that would drift is the
parsing both Datasets are built from.

WHAT A TRANSPORT MUST SUPPLY (see RedditCore):
    BASE          e.g. "https://oauth.reddit.com"
    JSON_SUFFIX   ".json" for the public host, "" for the OAuth host
    _fetch_json(url, attempts=None) -> parsed JSON | None, raising Blocked when refused

Schemas are the client prototype's own column names, unchanged, so both Datasets and the
spreadsheet share a vocabulary.
"""

import asyncio
import re
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from typing import Optional

from kontra import NonRetryableError, SessionLost, param

_DELETED = ("[deleted]", "[removed]")

# The client's taxonomy. Params override every one of these without a redeploy — it is the thing
# most likely to change between campaigns.
_KEYWORDS = ("hubspot,framer,webflow,wordpress,ghost,contentful,payload,cms,"
             "content management system,headless cms,blogging platform,site builder,"
             "migrate off,switch from,alternative to")
_AMBIGUOUS = "ghost,framer,payload,hubspot"
_PLATFORM_KEYWORDS = "hubspot,framer,webflow,wordpress,ghost,contentful,payload"
_INTENT_KEYWORDS = "migrate off,switch from,alternative to"
_TOPIC_KEYWORDS = "cms,content management system,headless cms,blogging platform,site builder"
_CONTEXT_WORDS = "cms,website,blog,site,hosting,headless,editor,publish,theme,plugin"

# Reddit serves its OWN block page, not Cloudflare's, and the wording is not the one the prototype
# measured: "blocked due to a network POLICY", not "network SECURITY". Measured 2026-08-24 from a
# DigitalOcean egress IP — 797 chars, <title>Blocked</title>, so BOTH of the prototype's homepage
# heuristics (the sentinel string and `len(txt) < 500`) sailed past it and only the .json probe
# caught the block. A list because the wording has already changed once.
_BLOCK_MARKERS = (
    "blocked due to a network policy",
    "blocked by network security",
    "whoa there, pardner",
)


def _is_block_page(text) -> bool:
    """True when a body is Reddit's block page. Checked on EVERY response, not just non-200s:
    the block arrives as a 200 about as often as a 403."""
    low = (text or "").lower()
    return any(m in low for m in _BLOCK_MARKERS)


class Blocked(Exception):
    """Reddit refused us. Never retried in place — for the browser transport the identity is
    spent and only a relaunch mints a new fingerprint; for the OAuth transport it means the
    token or the app is refused, which retrying cannot fix either."""


# --- types --------------------------------------------------------------------------------------


@dataclass
class Target:
    """One unit for `discover`: a subreddit to sweep.

    `tier` is the ambiguity switch, not a label. In a platform sub the whole feed is on topic, so
    we list `new` and keep everything; in a general sub we search each keyword and re-check the
    hit locally, because Reddit's search matches loosely. It also decides whether an AMBIGUOUS
    word ("ghost", "payload") counts alone or needs CMS context nearby.
    """

    subreddit: str
    tier: str = "general"        # platform | general


@dataclass
class Post:
    """POST_COLUMNS — one per kept post. `discover` emits it, `harvest` takes it.

    The three fetch-outcome fields of the prototype's posts sheet are NOT here:
    `comments_fetched`, `comments_captured` and `fetch_failed` are things only a harvest can know,
    and a 0 written at discover time would resurrect exactly the ambiguity the prototype's
    three-count design exists to kill. They live on the Document post row.
    """

    post_id: str
    subreddit: str
    tier: str
    author: str
    created_utc: str             # ISO 8601 UTC
    score: int
    upvote_ratio: Optional[float]
    num_comments: Optional[int]  # Reddit's OWN count — the independent number
    platforms: str               # comma-joined; the xlsx shows these joined too
    intent: str
    topic: str
    flair: str
    domain: str
    permalink: str
    run_id: str
    title: str
    text: str


@dataclass
class Document:
    """DOC_COLUMNS, plus the post-only extras that ride on the post row.

    ONE type for posts and comments, discriminated by `type`. They are two observations of one
    thread, not two datasets, and the prototype already treats them this way: its posts sheet is
    literally `[d for d in docs if d["type"] == "post"]`, a view over the jsonl rather than a
    second file.

        SELECT * FROM reddit_docs WHERE type = 'post' AND fetch_failed
        SELECT * FROM reddit_docs WHERE type = 'comment' AND is_op AND word_count > 20
    """

    doc_id: str                  # posts t3_xxx, comments t1_xxx — Reddit's permanent id
    parent_id: Optional[str]     # blank on posts
    tree_path: Optional[str]     # 0.1.0 = 1st comment, 2nd reply, 1st reply
    type: str                    # post | comment
    depth: int                   # 0 = post, 1 = top-level comment
    subreddit: str
    tier: str
    post_id: str
    post_title: str              # repeated on comment rows so each row reads alone
    author: str
    is_op: bool
    is_mod: bool
    created_utc: str
    score: int
    platforms: str
    intent: str
    topic: str
    word_count: int
    permalink: str
    run_id: str
    text: str

    # post-only; NULL on comment rows
    upvote_ratio: Optional[float] = None
    num_comments: Optional[int] = None
    comments_fetched: Optional[int] = None
    comments_captured: Optional[int] = None
    fetch_failed: Optional[bool] = None
    flair: Optional[str] = None
    domain: Optional[str] = None
    title: Optional[str] = None


# The post-only columns, named once. A comment row must still CARRY them (as None) rather than
# omit them: a key absent from some records is a schema difference, not a null, and the column
# store resolves it a run too late.
_POST_ONLY = ("upvote_ratio", "num_comments", "comments_fetched", "comments_captured",
              "fetch_failed", "flair", "domain", "title")


# --- pure helpers -------------------------------------------------------------------------------


def _words(raw) -> list:
    """A comma-separated param -> a list. Accepts a real list too, so a caller sending JSON gets
    the same behaviour as one sending a string."""
    if isinstance(raw, (list, tuple)):
        return [str(w).strip().lower() for w in raw if str(w).strip()]
    return [w.strip().lower() for w in str(raw or "").split(",") if w.strip()]


def _iso(ts) -> str:
    """Epoch seconds -> ISO 8601 UTC. "" when never captured — a column store wants ONE
    absent-value convention, and None and "" both reaching it is how a date column becomes a
    string column."""
    if not ts:
        return ""
    return datetime.fromtimestamp(ts, timezone.utc).isoformat().replace("+00:00", "Z")


def _keyword_hits(text: str, is_platform: bool, kws, ambiguous, context_re, window: int) -> list:
    """Which keywords appear, honouring the ambiguity rule.

    An AMBIGUOUS word outside its own platform sub needs CMS vocabulary within `window`
    characters. "ghost" is a common English word; "hubspot" is ambiguous differently — a product
    whose CRM is far better known than its CMS, so in business subs it means the CRM every time.
    """
    if not text:
        return []
    low, found = text.lower(), []
    for kw in kws:
        for m in re.finditer(r"\b" + re.escape(kw) + r"\b", low):
            if kw in ambiguous and not is_platform:
                lo, hi = max(0, m.start() - window), m.end() + window
                if not context_re.search(low[lo:hi]):
                    continue                      # bare mention, no CMS context
            found.append(kw)
            break
    return found


def _join(hits) -> str:
    """Hits -> one cell. Comma-joined because that is what the client's spreadsheet shows and what
    a `LIKE '%webflow%'` filter expects; `string_split(platforms, ',')` recovers the list in
    DuckDB when an exact match is wanted."""
    return ",".join(hits)


def _rows_for_thread(post: dict, comments: list, *, total: int, failed: bool,
                     run_id: str, tier: str, buckets) -> list:
    """One harvested post -> the flat Document rows. Ported from the prototype's
    `rows_for_record`, minus the nested twin (a caller wanting threads joins on `post_id`).

    Matching is recomputed here rather than carried from discover, so every row is judged by the
    same, final taxonomy even if a param changed between the two Methods.
    """
    post_id = post["post_id"]
    doc_id = f"t3_{post_id}"
    title = post.get("title") or ""
    body = post.get("text") or ""
    if body in _DELETED:
        body = ""

    plat, intent, topic = buckets(f"{title}\n{body}")
    common = {
        "subreddit": post["subreddit"], "tier": tier, "post_id": post_id,
        "post_title": title, "run_id": run_id,
    }

    post_row = {
        "doc_id": doc_id, "parent_id": None, "tree_path": None,
        "type": "post", "depth": 0, **common,
        "author": post.get("author") or "", "is_op": True, "is_mod": False,
        "created_utc": post.get("created_utc") or "",
        "score": int(post.get("score") or 0),
        "platforms": _join(plat), "intent": _join(intent), "topic": _join(topic),
        "word_count": len(body.split()),
        "permalink": post.get("permalink") or "",
        "text": body,
        # Three counts, because one cannot tell the story: Reddit says 40 / fetched 0 = failure;
        # Reddit says 0 = an empty thread. Without the independent number those two were the same
        # row forever.
        "upvote_ratio": post.get("upvote_ratio"),
        "num_comments": post.get("num_comments"),
        "comments_fetched": total,
        "comments_captured": 0,                  # filled below, once the tree is walked
        "fetch_failed": failed,
        "flair": post.get("flair") or "",
        "domain": post.get("domain") or "",
        "title": title,
    }
    rows = [post_row]

    def emit(nodes, parent_id, path=()):
        for i, c in enumerate(nodes):
            here = path + (i,)
            tree_path = ".".join(str(n) for n in here)
            # Reddit's own permanent comment id. The positional path rides along as its own field
            # rather than as the id, because position shifts the moment an upstream comment is
            # deleted — it describes where a comment sits, it cannot identify it across runs.
            cid = c.get("id") or f"{doc_id}.{tree_path}"
            cplat, cintent, ctopic = buckets(c["body"])
            rows.append({
                "doc_id": cid, "parent_id": parent_id, "tree_path": tree_path,
                "type": "comment",
                # Positional, like tree_path beside it — the node carries its own `depth` for
                # expand_more's re-attachment, and two sources for one fact can disagree.
                "depth": len(here), **common,      # depth 0 is the post
                "author": c.get("author") or "",
                "is_op": bool(c.get("is_op")), "is_mod": bool(c.get("is_mod")),
                "created_utc": _iso(c.get("created_utc")),
                "score": int(c.get("score") or 0),
                "platforms": _join(cplat), "intent": _join(cintent), "topic": _join(ctopic),
                "word_count": len(c["body"].split()),
                "permalink": ("https://reddit.com" + c["permalink"]) if c.get("permalink") else "",
                "text": c["body"],
                **{k: None for k in _POST_ONLY},
            })
            emit(c["replies"], cid, here)

    emit(comments, doc_id)
    post_row["comments_captured"] = len(rows) - 1
    return rows


# --- the transport-free half of the actor ---------------------------------------------------------


class RedditCore:
    """Search, comment-tree and taxonomy logic, over whatever `_fetch_json` a transport provides.

    A mixin rather than a base class with abstract methods: the two actors are `@actor.defn`
    classes with their own lifecycle hooks, and this only contributes the parts that are the same.
    """

    BASE = "https://old.reddit.com"
    JSON_SUFFIX = ".json"

    # Class default so the attribute always exists. A load hook resets it — a reload is a new
    # identity and a new streak — but a Method must never depend on load having assigned it, or
    # the failure is an AttributeError from inside the error path.
    _fail_streak = 0

    # ---- taxonomy ------------------------------------------------------------------------

    def _compile_taxonomy(self):
        """Read the params once per session and compile the regex once, not per post."""
        self._kws = _words(param.get("keywords", _KEYWORDS))
        self._ambiguous = set(_words(param.get("ambiguous", _AMBIGUOUS)))
        self._platform_kw = set(_words(param.get("platform_keywords", _PLATFORM_KEYWORDS)))
        self._intent_kw = set(_words(param.get("intent_keywords", _INTENT_KEYWORDS)))
        self._topic_kw = set(_words(param.get("topic_keywords", _TOPIC_KEYWORDS)))
        self._window = int(param.get("context_window", 120))
        ctx = _words(param.get("context_words", _CONTEXT_WORDS))
        self._context_re = re.compile(r"\b(" + "|".join(re.escape(w) for w in ctx) + r")\b")

    def _hits(self, text: str, is_platform: bool) -> list:
        return _keyword_hits(text, is_platform, self._kws, self._ambiguous,
                             self._context_re, self._window)

    def _buckets_for(self, is_platform: bool):
        """A closure over the tier, so the row builders stay free of actor state.

        The scraper keeps one flat keyword list because matching does not care what kind of word
        it is. Analysis does: "webflow" names a product, "cms" a topic, "switch from" an
        intention — three columns you can pivot against each other.
        """
        def buckets(text: str):
            hits = self._hits(text, is_platform)
            return ([h for h in hits if h in self._platform_kw],
                    [h for h in hits if h in self._intent_kw],
                    [h for h in hits if h in self._topic_kw])
        return buckets

    # ---- pacing --------------------------------------------------------------------------

    async def _cool_off(self):
        """Back off once a rate-limit window is evident, then let the batch continue.

        These windows run for minutes — far longer than the per-request ladder inside one fetch —
        so marching straight on donates more posts to the same window. The streak lives on `self`
        because it is a property of the SESSION, not of a unit: locals reset between Batch
        entries and this must not (ADR 0023 §13).
        """
        self._fail_streak += 1
        if self._fail_streak >= int(param.get("fails_before_cooldown", 2)):
            await asyncio.sleep(float(param.get("fail_cooldown", 60.0)))
            self._fail_streak = 0

    # ---- reddit --------------------------------------------------------------------------

    async def _search(self, sub: str, is_platform: bool) -> list:
        """Posts for one subreddit. Platform subs list `new`; general subs search each keyword.

        A general sub's hits are re-checked locally because Reddit's search matches loosely —
        without that, "payload" in r/webdev pulls in HTTP payloads.
        """
        cap = int(param.get("max_posts_per_sub", 8))
        min_score = int(param.get("min_post_score", 2))
        max_pages = int(param.get("search_pages", 3))
        time_filter = str(param.get("time_filter", "year"))
        attempts = int(param.get("search_attempts", 5))
        sfx = self.JSON_SUFFIX

        posts, seen = {}, set()
        queries = [None] if is_platform else self._kws

        for q in queries:
            if len(posts) >= cap:
                break
            after, pages = None, 0
            while pages < max_pages and len(posts) < cap:
                if q is None:
                    url = f"{self.BASE}/r/{sub}/new{sfx}?limit=100"
                else:
                    url = (f"{self.BASE}/r/{sub}/search{sfx}"
                           f"?q={q.replace(' ', '%20')}&restrict_sr=1&sort=new"
                           f"&t={time_filter}&limit=100")
                if after:
                    url += f"&after={after}"

                try:
                    # Try harder than a comment fetch does. Losing a comment fetch costs one
                    # thread; losing a SEARCH means every post matching only this keyword is
                    # never discovered, and nothing downstream can tell they are missing.
                    data = await self._fetch_json(url, attempts=attempts)
                except Blocked:
                    raise
                except NonRetryableError:
                    # A TERMINAL ERROR IS NOT AN EMPTY KEYWORD. `break` here reads a refused
                    # credential as "this subreddit has nothing", and since every general sub
                    # searches every keyword, one dead token was re-minted once per keyword:
                    # 202 auth POSTs from a 3-Unit Batch, and a Run that reported `completed`
                    # with an empty Dataset and no failures. The only thing distinguishing
                    # "found nothing" from "could not ask" is that this raise propagates.
                    raise
                except Exception:
                    break                              # skip THIS keyword, not the subreddit
                if not data or "data" not in data:
                    break

                for c in data["data"].get("children", []):
                    p = c.get("data") or {}
                    pid = p.get("id")
                    if not pid or pid in seen or (p.get("score") or 0) < min_score:
                        continue
                    seen.add(pid)
                    hits = self._hits(f"{p.get('title','')}\n{p.get('selftext','')}", is_platform)
                    if not is_platform and not hits:
                        continue                       # search matched loosely; enforce locally
                    posts[pid] = self._post_row(p, sub, is_platform)

                after = data["data"].get("after")
                pages += 1
                if not after:
                    break

        return list(posts.values())[:cap]

    def _post_row(self, p: dict, sub: str, is_platform: bool) -> dict:
        """Reddit's ~100-field payload -> the Post schema. Only the declared fields are kept,
        deliberately: adding a column later means scraping again, and that is the trade the
        prototype already made."""
        title = p.get("title") or ""
        body = p.get("selftext") or ""
        if body in _DELETED:
            body = ""
        plat, intent, topic = self._buckets_for(is_platform)(f"{title}\n{body}")
        return {
            "post_id": p.get("id") or "",
            "subreddit": sub,
            "tier": "platform" if is_platform else "general",
            "author": p.get("author") or "",
            "created_utc": _iso(p.get("created_utc")),
            "score": int(p.get("score") or 0),
            "upvote_ratio": p.get("upvote_ratio"),
            "num_comments": p.get("num_comments"),
            "platforms": _join(plat), "intent": _join(intent), "topic": _join(topic),
            "flair": p.get("link_flair_text") or "",
            "domain": p.get("domain") or "",
            "permalink": "https://reddit.com" + (p.get("permalink") or ""),
            "run_id": "",                              # stamped by discover, from self.run_id
            "title": title,
            "text": body,
        }

    async def _fetch_comments(self, sub: str, post_id: str):
        """Top-level comments in Reddit's own `top` order, replies kept nested.

        Reddit already ranks these via ?sort=top — we must NOT re-sort. Flattening the tree and
        sorting by raw score compares a depth-3 reply against a top-level comment as if they were
        peers, which is how a nested sub-comment ends up presented as the thread's best.
        """
        limit = int(param.get("comment_limit", 500))
        url = (f"{self.BASE}/r/{sub}/comments/{post_id}{self.JSON_SUFFIX}"
               f"?sort=top&limit={limit}")
        data = await self._fetch_json(url)
        if not data or len(data) < 2:
            return [], 0

        total = 0
        index = {}          # fullname -> node, so expanded comments can be re-attached
        pending = []        # ids hidden behind "load more comments" stubs

        def node_from(d, depth):
            nonlocal total
            total += 1
            n = {
                "id": d.get("name") or (f"t1_{d['id']}" if d.get("id") else None),
                "body": d.get("body") or "",
                "author": d.get("author"),
                "created_utc": d.get("created_utc"),
                "permalink": d.get("permalink"),
                # Reddit flags the submitter's own replies — the highest-signal rows in any
                # thread, since that is the person with the problem answering questions.
                "is_op": bool(d.get("is_submitter")),
                # Mod boilerplate scores well and says nothing. Flag it so it can be dropped.
                "is_mod": bool(d.get("stickied")) or d.get("distinguished") == "moderator",
                "score": d.get("score", 0),
                "depth": depth,
                "replies": [],
            }
            if n["id"]:
                index[n["id"]] = n
            return n

        def build(children, depth=0):
            nodes = []
            for c in children:
                d = c.get("data", {})
                # A "more" child is Reddit's "load more comments" link: the ids of comments it
                # chose not to inline. Collect them rather than drop them — on a big thread this
                # is most of the conversation.
                if c.get("kind") == "more":
                    pending.extend(d.get("children") or [])
                    continue
                if c.get("kind") != "t1" or d.get("body") in _DELETED or not d.get("body"):
                    continue
                n = node_from(d, depth)
                replies = d.get("replies")
                if isinstance(replies, dict):
                    n["replies"] = build(replies["data"]["children"], depth + 1)
                nodes.append(n)
            return nodes

        toplevel = build(data[1]["data"]["children"])      # already in Reddit's order

        if bool(param.get("expand_more", True)) and pending:
            await self._expand_more(post_id, toplevel, index, pending, node_from)

        keep = max(1, round(len(toplevel) * float(param.get("top_fraction", 1.0))))
        return toplevel[:keep], total

    async def _expand_more(self, post_id, toplevel, index, pending, node_from):
        """Pull in the comments hidden behind "load more comments" links.

        Reddit inlines only part of a large tree; the rest arrives only if you ask by id.
        Measured: at limit=500 a 72-comment thread has no stubs at all, while a 1,044-comment one
        hid 480 ids behind 89 of them — 450 comments without this, 802 with.

        Note what this does NOT explain: a thread reporting 72 and serving 39 has no stubs — that
        gap is comments since deleted, which Reddit drops entirely and will never serve.

        /api/morechildren returns a FLAT list with parent_id on each item, so everything is
        re-attached by id. Items can arrive before their parent, so unattached ones are held back
        and retried until a pass attaches nothing new.
        """
        link_id = f"t3_{post_id}"
        orphans, requests = [], 0
        budget = int(param.get("max_more_requests", 5))

        while pending and requests < budget:
            batch, pending = pending[:100], pending[100:]        # Reddit's per-call cap
            url = (f"{self.BASE}/api/morechildren{self.JSON_SUFFIX}?api_type=json"
                   f"&link_id={link_id}&sort=top&children={','.join(batch)}")
            try:
                data = await self._fetch_json(url)
            except Blocked:
                raise
            except NonRetryableError:
                raise                    # terminal: not "this thread has no more comments"
            except Exception:
                # Losing an expansion costs depth on one thread, not the thread itself.
                break
            requests += 1

            for t in (data or {}).get("json", {}).get("data", {}).get("things", []):
                d = t.get("data", {})
                if t.get("kind") == "more":
                    pending.extend(d.get("children") or [])
                elif t.get("kind") == "t1" and d.get("body") not in _DELETED and d.get("body"):
                    orphans.append(d)

            # Attach whatever now has a known parent, repeatedly: a comment and its own replies
            # can both be in the same batch, in any order.
            while True:
                still = []
                for d in orphans:
                    parent = d.get("parent_id")
                    if parent == link_id:
                        toplevel.append(node_from(d, 0))
                    elif parent in index:
                        host = index[parent]
                        host["replies"].append(node_from(d, host["depth"] + 1))
                    else:
                        still.append(d)
                if len(still) == len(orphans):
                    break                                # no progress — the rest are stranded
                orphans = still

    # ---- the two Method bodies -------------------------------------------------------------
    #
    # The bodies live here; each actor declares the Methods themselves, because @actor.method
    # registers into a registry keyed by name and a decorator on a shared mixin would register
    # once for whichever class imported it first.

    async def _run_discover(self, batch, dataset):
        """One subreddit -> the posts worth harvesting (1 -> N).

        Sequential. For the browser transport that is mandatory (one warmed page, paced
        fetches); for OAuth it keeps the two actors behaviourally identical and stays inside
        Reddit's per-client rate limit, which is the real ceiling either way.
        """
        async for unit in batch:
            # `takes=Target` coerces the payload, so this is an object, not a dict — that is what
            # the declaration buys (internals/batch.py:Unit.value).
            target = unit.value
            sub = target.subreddit.strip().lstrip("r/").strip("/")
            tier = target.tier or "general"
            if not sub:
                continue                                   # nothing to search; commit empty
            try:
                found = await self._search(sub, tier == "platform")
            except Blocked as e:
                # Reloading is the only recovery that helps: the browser transport gets a new
                # fingerprint, the OAuth transport a fresh token. Isolating this subreddit would
                # march the batch through every remaining one against the same refusal.
                raise SessionLost(f"blocked during search: {e}") from e
            for post in found:
                await dataset.push({**post, "tier": tier, "run_id": self.run_id})

    async def _run_harvest(self, batch, dataset):
        """One post -> its post row and every comment row (1 -> N).

        A post whose comment fetch fails still emits its post row, with `fetch_failed` true and
        `comments_fetched` 0. That is the prototype's rule and the whole reason the three counts
        exist: a silent 0 makes a hole indistinguishable from an empty thread, forever.
        """
        async for unit in batch:
            post = unit.value                              # a Post, coerced by `takes=`
            sub, post_id = post.subreddit, post.post_id
            tier = post.tier or "general"
            if not sub or not post_id:
                continue

            comments, total, failed = [], 0, False
            try:
                comments, total = await self._fetch_comments(sub, post_id)
                self._fail_streak = 0
            except Blocked as e:
                raise SessionLost(f"reddit block — {e}") from e
            except NonRetryableError:
                # `fetch_failed=True` is for a fetch that was ATTEMPTED and lost. A terminal
                # error means we could not ask at all, and writing the post row with
                # `comments_fetched=0` beside it would put the one thing this schema's three
                # counts exist to distinguish on the wrong side of the line.
                raise
            except Exception:
                failed = True
                await self._cool_off()

            buckets = self._buckets_for(tier == "platform")
            # asdict at the boundary: the row builder stays dict-shaped so it can be tested
            # without constructing a Post, and so it keeps producing the flat output record.
            rows = _rows_for_thread(asdict(post), comments, total=total, failed=failed,
                                    run_id=self.run_id, tier=tier, buckets=buckets)
            for row in rows:
                await dataset.push(row)
