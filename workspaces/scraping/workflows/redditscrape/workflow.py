"""redditscrape — one Machine, both Methods, one Dataset, as a durable program.

THE QUESTION: which Reddit threads talk about moving between CMS platforms, and what does the
conversation under them actually say? The client's prototype answered it as a script that owned
its own resume logic; this answers it as a Run, so the Machine it crawls from is inside the same
scope as the crawl and cannot outlive it.

    ┌ the control machine ──────────┐        ┌ the fleet — created by this file ─────┐
    │ RedditScrape (this file)      │──ref───┤ redditapi@0.1.0 (Python actor) ×1     │
    │  up → page subreddits         │        │   discover  subreddit → posts         │
    │     → discover(batch)         │──ref───┤   harvest   post → post + comment rows│
    │     → harvest(chunk, tmp)     │        └───────────────────────────────────────┘
    │  → insert_from(tmp, into)     │         (the Machine is gone before this line)
    └───────────────────────────────┘

ONE MACHINE, AND ONE SESSION ON IT — `sessions` is density, and for both transports it must stay
1. Reddit's free OAuth tier is 100 queries/minute PER CLIENT, not per connection, so a second
session on the same credential does not double throughput; it halves each session's share and
races the ratelimit floor both of them are reading. (For the browser transport it is worse still:
each session is its own Camoufox behind the same Machine IP.) This is the one actor in the repo
where the concurrency knob is a footgun rather than a dial, which is why it is written down here
and defaulted rather than left to the caller.

TWO WIDTHS, AS ALWAYS. `discover` fans out — one subreddit emits up to `max_posts_per_sub`
posts — so a page of subreddits comes back much larger and is re-paged before `harvest` sees it.
That re-page is what keeps a batch inside the per-call guard and gives each thread its own
isolation boundary.

THE FAILURES ARE OUTPUT TOO. `harvest` emits the post row even when the comment fetch fails,
flagged `fetch_failed` with Reddit's own `num_comments` beside a `comments_fetched` of 0. A
dropped Unit would be absent from the Dataset, and for this run that is the difference between
"this thread has no comments" and "we never got this thread's comments" — the distinction the
client's three-count design exists to preserve.

PRODUCE INTO A TEMP, ACCEPT OUT OF IT. Rows stage into a temporary Dataset while the Machine is
alive and are promoted after it is destroyed, so the promoted rows keep the Machine, actor
version and Run that produced them.

    kontra build --actor examples/private/reddit-api             # publish the Artifact ONCE
    kontra workflow register .kontra/workflows/redditscrape
    kontra workflow start .kontra/workflows/redditscrape --wait --input '{
      "dataset": "subreddits", "into": "reddit_docs", "machines": 1,
      "client_id": "…", "client_secret": "…",
      "username": "…", "password": "…",
      "user_agent": "kontra-redditscrape/0.1 (by /u/<your-account>)"}'

    kontra db query "SELECT type, count(*) FROM reddit_docs GROUP BY 1"
    kontra db query "SELECT subreddit, count(*) FROM reddit_docs WHERE type='post' GROUP BY 1"

A FRESH IP PER CRAWL IS WHAT THE FLEET ALREADY IS. `fleet.up` provisions this run's Droplets
inside the run's own scope and destroys them at scope exit, so every crawl leaves from an address
that did not exist before it started and does not survive it. Nothing extra is needed to get that,
and nothing here is shared between runs — which is the whole reason the Machine lives inside the
`async with` rather than beside it.

WHAT THAT DOES AND DOES NOT BUY, MEASURED 2026-08-24. It does not defeat Reddit's block, and the
measurement is unambiguous — three brand-new Droplets, three regions, minutes old:

    host / path                          sfo3   nyc3   ams3
    www.reddit.com/api/v1/access_token   401    401    401     <- reachable, mints tokens
    oauth.reddit.com/<anything>          403    403    403     <- 1522-byte "Blocked" page
    www.reddit.com/r/<sub>/new.json      403    403    403
    api.reddit.com, old.reddit.com,      403    403    403
      and /r/<sub>/new/.rss

So rotating the address is not the lever: every data path is blocked from DigitalOcean and only
the AUTH path is exempt. That exemption is the interesting part — Reddit is not banning the
address space, it is banning traffic to data hosts that does not carry a token it accepts, while
deliberately leaving open the one endpoint you need in order to get such a token. The probes above
all carried `Authorization: bearer FAKE`, which an edge can reject without asking the API. Whether
a VALID token passes is the one variable that could not be tested here, because no Reddit
credential exists on this control plane — and it is exactly the variable `redditapi` supplies.

That is why this workflow defaults to `redditapi` and why `client_id`/`client_secret`/`username`/
`password`/`user_agent` are inputs on the form. With them, a run is one command. Without them
`@actor.load` raises NonRetryableError naming the missing field, before a single Droplet-minute is
spent crawling — and the run still promotes and still reports, rather than hanging.
"""
from temporalio import workflow
from temporalio.exceptions import ApplicationError
from typing_extensions import TypedDict

from kontra import catalog, fleet


class RedditScrapeInput(TypedDict, total=False):
    """The knobs this run reads — a DESCRIPTION of the body, so the Workflows page can generate
    a form from it rather than drawing `dict` as "declares no fields"."""

    actor: str          # WHICH TRANSPORT: "redditapi" (OAuth) | "reddit" (Camoufox)
                        # Both expose the same discover/harvest signature, so this is the only
                        # line that changes between them.    (default "redditapi")
    dataset: str        # Dataset of {subreddit, tier} to page over   (default "subreddits")
    into: str           # Dataset the rows promote into               (default "reddit_docs")
    machines: int       # Droplets, FRESH per run; 0 means zero, not 1 (default 1)
    sessions: int       # Workers per Machine — LEAVE AT 1, see above (default 1)
    size: int           # page size for both fan-out widths           (default 20)
    max_posts_per_sub: int   # per-subreddit cap                      (default 8)
    time_filter: str    # hour|day|week|month|year|all                (default "year")
    # "redditapi" credentials. A script app at https://www.reddit.com/prefs/apps.
    client_id: str
    client_secret: str
    username: str
    password: str
    user_agent: str     # REQUIRED by redditapi; a generic one is a documented block cause


def _why(e: BaseException) -> str:
    """The one line of an exception worth putting in a Dataset-shaped result. Nexus wraps a
    Method failure several layers deep, so the outermost message is usually the least useful —
    walk to the innermost cause, which is where SessionLost actually says what happened."""
    cur = e
    while getattr(cur, "__cause__", None) is not None:
        cur = cur.__cause__
    return f"{type(cur).__name__}: {str(cur).splitlines()[0][:200]}"


def _terminal(e: BaseException) -> bool:
    """True when the failure is one the actor declared UN-RETRYABLE — a refused credential, a
    missing required param.

    The distinction matters because of what the alternative costs. A terminal error repeats on
    every Batch by definition: `redditscrape-1787585053` recorded the same "reddit refused the
    credentials (HTTP 401)" seven times, once per page of subreddits, which is seven failed
    authentications against the client's own Reddit account for a credential already known to be
    refused. A transient failure is worth continuing past; this is not, and the account holder is
    the one who pays for pretending otherwise.

    Matched on the ApplicationError's `type`, which is what the actor host stamps
    (`internals/temporal/host.py`), walking the cause chain because Nexus wraps it several deep.
    """
    cur: BaseException | None = e
    while cur is not None:
        if getattr(cur, "type", None) == "NonRetryableError":
            return True
        cur = getattr(cur, "__cause__", None)
    return False


def _num(req: dict, key: str, default: int) -> int:
    """An integer knob where ABSENT means the default and ZERO means zero.

    `int(req.get("machines") or 1)` would read `machines: 0` — the way you ask for no Droplets —
    as a request for one, and provision it. Wrong in the direction that bills.
    """
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class RedditScrape:
    @workflow.run
    async def run(self, req: RedditScrapeInput) -> dict:
        size = _num(req, "size", 20)
        out_name = req.get("into") or "reddit_docs"
        # The two transports are drop-in for each other: same Methods, same takes/emits, same
        # Dataset shape. Choosing between them is a name, which is exactly what a Fleet is
        # already keyed on (<actor>-<version>), so the two never share a stack by accident.
        actor_name = req.get("actor") or "redditapi"
        subs = catalog.dataset(req.get("dataset") or "subreddits")

        # Handed to every Method call on this actor. The credential rides HERE rather than in the
        # image so a run can change identity without a rebuild — and so it is a property of
        # the Run, which is where the audit trail already is.
        params = {
            "max_posts_per_sub": _num(req, "max_posts_per_sub", 8),
            "time_filter": req.get("time_filter") or "year",
            # Ignored by the browser transport, required by the OAuth one. Passed unconditionally
            # so switching `actor` is genuinely a one-word change in the request.
            "client_id": req.get("client_id") or "",
            "client_secret": req.get("client_secret") or "",
            "username": req.get("username") or "",
            "password": req.get("password") or "",
            "user_agent": req.get("user_agent") or "",
        }

        posts = harvested = dropped = 0
        # Which leg gave up, and "" when the run simply finished. It rides out in the result
        # because a Dataset that stops short of the input has to say so — 4 subreddits of 19 is
        # a different fact from "19 subreddits, 4 had anything".
        stopped = ""
        # WHAT WENT WRONG, AS DATA. A refusal used to escape straight out of the scope: the run
        # failed, `insert_from` below never ran, and every row already harvested was thrown away
        # with it. One blocked subreddit costing the other eighteen is not a policy anyone chose.
        failures: list = []

        async with catalog.dataset.temp() as tmp:
            async with fleet.hold(tag="reddit", machines=_num(req, "machines", 1)) as f:
                # `spread=True` IS THE WHOLE POINT FOR THIS ACTOR, and it is the one place in this
                # repo where it is not decoration. The header above says a fresh IP per crawl is
                # what the Fleet already is — that was an invisible invariant while a Machine held
                # exactly one Worker, and packing (ADR 0037) took it away. `spread=True` asks for
                # it back: one Worker of THIS placement per Machine, so no two crawls share a
                # source address. It does NOT reserve the Machine — a co-placed Worker would still
                # share the address — which is why this Fleet has its own tag.
                await f.place(
                    actor_name, "0.1.0",
                    sessions=_num(req, "sessions", 1),
                    spread=True,
                )
                # POLLERS, not Pulumi. `place` returns while systemd is still starting, and a
                # Batch dispatched into that gap waits on a queue nobody serves.
                await f.ready()

                async with catalog.actor(actor_name, "0.1.0") as rd:
                    # `order_by` is required: a materialized dataset stamps no row id, so paging
                    # without one may overlap or skip subreddits and nothing would raise.
                    async for batch in subs.batches(size, order_by="subreddit"):
                        try:
                            found, drops = await rd.discover(batch, params=params)
                        except Exception as e:                      # noqa: BLE001
                            # Not CancelledError — that is a BaseException and still propagates,
                            # so a cancelled Run still tears the fleet down.
                            failures.append(f"discover: {_why(e)}")
                            if _terminal(e):
                                stopped = "discover"
                                break
                            continue
                        dropped += len(drops)
                        posts += len(found)

                        # Re-page: this caller does not get to predict the fan-out. Unconditional,
                        # because a Batch that already fits yields itself and schedules nothing.
                        async for chunk in found.batches(size):
                            try:
                                rows, drops = await rd.harvest(chunk, tmp, params=params)
                            except Exception as e:                  # noqa: BLE001
                                failures.append(f"harvest: {_why(e)}")
                                if _terminal(e):
                                    stopped = "harvest"
                                    break
                                continue
                            dropped += len(drops)
                            harvested += len(rows)
                        if stopped:
                            break

            # The Machine is destroyed; `tmp` outlived it. Promote everything — unlike nscheck
            # there is no `where`, because for this run the failures ARE the record: a post row
            # with fetch_failed set is the only evidence that a thread's comments are missing.
            #
            # UNCONDITIONAL, and that is the point of the try/except above. Whatever was gathered
            # before things went wrong becomes the Dataset; a run that half-worked keeps its half.
            promoted = await catalog.dataset(out_name).insert_from(tmp)

        result = {"into": out_name, "actor": actor_name, "machines": len(f.inventory),
                  "bundle": f.bundle_sha[:12], "posts": posts, "documents": harvested,
                  "promoted": promoted, "dropped": dropped, "stopped_early": stopped,
                  "failures": failures[:20]}

        # A run that gathered NOTHING and hit failures is a failed run, and has to say so — a
        # green run beside an empty Dataset is the "completed with no output" trap this codebase
        # keeps finding. It fails HERE, after the promotion, so the Dataset and any partial rows
        # still exist to be looked at.
        if failures and harvested == 0:
            raise ApplicationError(
                f"harvested nothing from {len(failures)} failed call(s): {failures[0]}",
                result, type="NoOutput", non_retryable=True)
        return result


if __name__ == "__main__":
    catalog.serve([RedditScrape])
