"""surface — turn a program's scope into its HTTP ATTACK SURFACE.

THE QUESTION: for every host in this program, what can be injected into, and where?

This is the first of the two workflows and it sends no attack payload at all. It drives a real
browser over the scope, records every request and response the page made, and reduces that traffic
into one queryable statement of surface per target. `hunt` then attacks what this found.

    ┌ surface (this file) ─────────────┐        ┌ hunt (the other workflow) ───────────┐
    │ scope_<prog>                     │        │ injection_points  ── where to inject │
    │   → webcrawl.crawl               │        │ exchanges_<prog>  ── what to mutate  │
    │   → http_events_<prog>  raw      │──────▶ │ techniques        ── what to send    │
    │   → injection_points    surface  │        │   → desync_leads                     │
    │   → exchanges_<prog>    replayable│       └──────────────────────────────────────┘
    └──────────────────────────────────┘

WHY THE SPLIT IS REAL AND NOT COSMETIC. Crawling and attacking have nothing in common
operationally: a crawl is browser-bound, runs once per program per cycle, needs 4 GB and a
Chromium, and is completely safe to re-run. An attack sweep is socket-bound, runs many times
against the same surface as the corpus grows, needs almost no memory, and puts hostile bytes on
someone else's origin. Fusing them meant re-crawling to re-scan and made the crawl's cost a toll
on every scan — and it is why `exchanges_8x8` ended up loaded BY HAND, outside any Run, which is
what left it with no lifecycle record and got the entire splitting axis silently skipped.

THREE OUTPUTS, THREE DIFFERENT JOBS — which is why they are three Datasets and not one:

    http_events_<prog>   THE RAW TRAFFIC, one row per request or response, `tx` joining the pair.
                         Kept because reduction is lossy and the questions change; a new injection
                         point discovered next month is a re-query, not a re-crawl.

    injection_points     THE SURFACE, one row per INJECTION POINT per host — every path the
                         crawl reached and every header either side sent. This is what `hunt`
                         reads to decide where to put a payload. Program is a COLUMN here, not a
                         name suffix, so "does anything anywhere read this header" is one query.

    exchanges_<prog>     THE REPLAYABLE PAIRS, request + response together, which is the fold
                         axis's input Unit shape. Program-suffixed because a fold sweep is always
                         scoped to one program's authorisation.

Run it:

    kontra workflow serve python/surface
    kontra workflow start python/surface --wait --input '{"program": "8x8", "machines": 2}'
"""
from temporalio import workflow
from typing_extensions import TypedDict

from dataclasses import dataclass
from datetime import timedelta

from kontra import KontraFlow, catalog, fleet, progress

# 0.2.0 CARRIES THE TRAVERSAL. 0.1.0 visits exactly the seed, so `depth` had nowhere to go —
# and it also predates the Set-Cookie fix, which means its `injection_points` can never contain
# the `set-cookie` rationale this workflow declares below. It now lives in THIS workspace: a
# Worker serves code from the current workspace only (ADR 0049), and `surface` is a bugbounty
# workflow, so a crawler sitting in `scraping/` was not reachable from here at all.
WEBCRAWL = ("webcrawl", "0.2.3")

# Headers a RESPONSE names that tell you something is read on the way in. Carried as SQL so the
# rationale lands on the row and a consumer needs no lookup table — the same reason `injection_points`
# has `point_rationale` at all.
#
# These are not "interesting headers". Each one is the application ADMITTING something:
#   access-control-allow-headers  the app declares which request headers it will read
#   vary                          the CACHE keys on these — the difference between poisoning one
#                                 response and poisoning everyone's
#   set-cookie                    a value the app reads back on the next request
POINT_RATIONALE = {
    "access-control-allow-headers": "the application declares it reads these request headers",
    "vary": "the cache keys on this — one response vs everyone's",
    "set-cookie": "a value the application reads back",
}


@dataclass(frozen=True)
class Phase:
    """Everything a phase of this crawl needs, resolved ONCE in `run()` — the peer of `hunt`'s.

    See github #20 for why this is a parameter object and NOT an activity: a workflow body is
    already durable, and collapsing the page loop into a single activity would mean a crash
    re-crawling from page 1 rather than resuming where it stopped.

    Frozen so a phase cannot mutate its own inputs — the one way this refactor could introduce a
    replay divergence, closed by construction rather than by review.
    """

    req: "SurfaceInput"
    program: str
    scope: object          # catalog.Dataset
    scope_name: str
    events: object         # catalog.Dataset
    size: int
    params: dict
    call_opts: dict


class SurfaceInput(TypedDict, total=False):
    program: str       # selects scope_<program>, and is stamped on every output row
    # ONE SCOPE DATASET FOR MANY PROGRAMS — the same knob `hunt` has, for the same reason.
    # A campaign across the paying HackerOne surface is 463 programs, and a Dataset per
    # program whose only distinguishing content is its `program` column is a catalog nobody
    # can read. The seed query already filters `program = '<program>'`; the NAME is all that
    # had to become a parameter.
    scope: str         # read scope HERE instead of `scope_<program>`
    machines: int      # fleet width. A browser per Machine is memory-bound, so this is small.
    sessions: int      # live Sessions per Machine
    size: int          # seeds per Batch
    seed_limit: int    # cap the scope read; 0 is uncapped
    parallel_seeds: int   # BrowserContexts in flight per Worker
    nav_timeout_ms: int
    settle_ms: int     # keep capturing this long after load — SPAs issue XHRs after it
    depth: int         # how far the crawl follows links from a seed
    max_pages: int     # pages yielded per seed (default 10 = 5 paths x 2 pages)
    pages_per_path: int  # concrete pages one path SHAPE may spend (default 2)
    max_seconds_per_host: int  # wall-clock budget per seed, so one slow host cannot hold a Worker
    simhash_distance: int      # near-duplicate radius over the request fingerprint
    max_events: int    # per-seed cap
    skip_points: bool  # do not rebuild injection_points (raw capture only)
    # WHERE THE SURFACE LANDS. `injection_points` is SHARED across programs (program is a
    # column, not a suffix) and `_points` APPENDS, so a table that once took bad rows cannot
    # be corrected in place — and a durable Dataset is not deletable by name, it ages out.
    # Naming the destination is how a campaign starts clean without waiting for retention.
    points: str        # write injection_points HERE instead
    attach: bool       # drive Workers already polling; hold no fleet of your own


def _num(req, key, default):
    """`req.get(k) or default` is WRONG for a number: `machines=0` — the way you ask a run to
    provision nothing — is falsy, so `or 2` reads it as a request for two and provisions them."""
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class Surface(KontraFlow):
    @workflow.run
    async def run(self, req: SurfaceInput) -> dict:
        program = req.get("program") or "8x8"
        size = _num(req, "size", 50)

        scope_name = req.get("scope") or f"scope_{program}"
        scope = catalog.dataset(scope_name)
        events = catalog.dataset(f"http_events_{program}")

        params = {
            "parallel_seeds": _num(req, "parallel_seeds", 6),
            "nav_timeout_ms": _num(req, "nav_timeout_ms", 20000),
            "settle_ms": _num(req, "settle_ms", 2000),
            "max_events": _num(req, "max_events", 400),
            "block_media": True,      # bytes without inventory value
            "capture_bodies": False,  # the surface is headers and paths; bodies are the next tier
            "depth": _num(req, "depth", 3),
            "max_pages": _num(req, "max_pages", 10),
            # PAGES PER PATH SHAPE, not per host. With `paths_per_host` 5 in `hunt`, a
            # host needs ~5 distinct routes and 2 samples of each — so `max_pages` 10 is
            # the product, not an independent dial. Raising one without the other either
            # starves the path inventory or spends the budget re-sampling one route.
            "pages_per_path": _num(req, "pages_per_path", 2),
            # A BUDGET, NOT A TIMEOUT. `nav_timeout_ms` bounds ONE navigation; a seed that serves
            # a thousand fast pages hits neither it nor `max_events` and holds its Worker for as
            # long as it likes. This bounds the seed.
            "max_seconds_per_host": _num(req, "max_seconds_per_host", 300),
            # MEASURED, and already near the ceiling: a sibling route is ~16 bits away, a
            # different METHOD ~11 and a different HOST ~15, so anything much above this merges
            # GET with POST and merges two hosts. Exposed to be LOWERED, not raised.
            "simhash_distance": _num(req, "simhash_distance", 3),
        }
        call_opts = {"schedule_to_close_timeout":
                     timedelta(minutes=_num(req, "call_minutes", 30))}

        phase = Phase(req=req, program=program, scope=scope, scope_name=scope_name,
                      events=events, size=size, params=params, call_opts=call_opts)

        if req.get("attach"):
            workflow.logger.info(
                f"attached to whoever is serving webcrawl@{WEBCRAWL[1]} — no Lease held")
            crawled = await self._sweep(phase)
        else:
            # `actor=` AND `version=` ARE REQUIRED, and `up` PLACES them itself — it is sugar
            # over `hold` + `place` and costs ONE converge rather than two. This called
            # `fleet.up(tag=…, machines=…)` and then placed separately, which is the older
            # spelling: it raised `TypeError: up() missing 2 required keyword-only arguments`
            # before provisioning anything, so the fleet path of this workflow could not run at
            # all. The `attach` path above was unaffected, which is why it went unnoticed.
            async with fleet.up(actor=WEBCRAWL[0], version=WEBCRAWL[1], tag="webcrawl",
                                machines=_num(req, "machines", 2),
                                sessions=_num(req, "sessions", 2)) as f:
                await f.ready()
                # See the same block in `hunt`: the rate before the work, the total after it, and
                # the total in `finally` so a cancelled crawl still reports what it cost.
                workflow.logger.info(
                    f"{len(f.inventory)} machine(s) polling; crawling {program} — "
                    f"{f.cost_words()}")
                held = workflow.now()
                try:
                    crawled = await self._sweep(phase)
                finally:
                    workflow.logger.info("fleet released — "
                                         + f.cost_words((workflow.now() - held).total_seconds()))

        # THE FLEET IS GONE before either reduction runs. Both are pure SQL over a durable
        # Dataset, so they cost no browser and no network and can be re-run next month over a
        # crawl that already happened — which is the whole reason the raw events are kept.
        point_rows = (0 if req.get("skip_points")
                      else await self._points(program, events, scope_name,
                                              req.get("points")))
        exch_rows = await self._exchanges(program, events)

        return {"program": program, "events": crawled,
                "injection_points": point_rows, "exchanges": exch_rows}

    async def _sweep(self, p: Phase):
        # Unpacked rather than rewritten throughout — see the same note in `hunt._sweep`. The
        # defect #20 names is the signature, and these are read-only views of a frozen object.
        req, program, scope, scope_name = p.req, p.program, p.scope, p.scope_name
        events, size, params, call_opts = p.events, p.size, p.params, p.call_opts
        """Page the scope into seeds and crawl them."""
        limit = _num(req, "seed_limit", 0)
        seen = 0
        pages = []
        # ONLY ROWS THAT CARRY A URL. `scope_<program>` holds the whole authorisation list,
        # wildcards and unexpandable globs included, and those have an empty `seed` by
        # construction — `webcrawl._crawl_seed` would answer each with one "not an http(s) url"
        # event, filling `http_events_<program>` with rows about rows.
        where = f"program = '{program}' AND seed <> ''"
        async for batch in scope.batches(size, order_by="host", where=where):
            pages.append(batch)
            seen += 1
            if limit and seen * size >= limit:
                # A BOUND THAT IS ANNOUNCED. A capped crawl that reads as a complete one is how
                # the surface silently describes a fraction of the program.
                workflow.logger.warning(
                    f"seed_limit {limit} reached — the crawl is PARTIAL by request",
                    extra={"incomplete": True, "axis": "seeds", "phase": "crawl",
                           "seed_limit": limit})
                break

        workflow.logger.info(f"crawling {len(pages)} page(s) of {scope_name} for {program}")
        # THE SHAPE A PANE READS. `program` and `at` are what turn "done 3/8" — which describes
        # the machine — into "visa, on aw.visa.com" — which describes the work. `total` is
        # published once up front so a subscriber joining late can draw a bar without waiting for
        # a second event to infer the denominator.
        progress(phase="crawl", program=program, done=0, total=len(pages), found=0)
        crawled = 0
        # A fresh Session per chunk, for the reason the fold axis learned the hard way: a Session
        # pins to ONE Worker, so one Session held across a whole crawl is a single Machine doing
        # all the work and a single point of failure for the entire run.
        chunk = max(1, _num(req, "chunk", 8))
        voided, done = 0, 0
        for start in range(0, len(pages), chunk):
            async with catalog.actor(*WEBCRAWL) as c:
                for page in pages[start:start + chunk]:
                    try:
                        rows, _ = await c.crawl(page, events, params=params, **call_opts)
                    except Exception as exc:  # noqa: BLE001 - the reason is the payload
                        voided += 1
                        workflow.logger.warning(
                            f"crawl batch voided: {exc!r} — that scope page has no surface",
                            extra={"incomplete": True, "axis": "scope-pages", "phase": "crawl"})
                        # A VOID IS AN EVENT A WATCHER NEEDS. Left unpublished, a page that threw
                        # and a page still running look identical in the pane — which is the same
                        # ambiguity that let a fully-voided run read as COMPLETED.
                        progress(phase="crawl", program=program,
                                 at=f"page {done + voided} voided", done=done,
                                 total=len(pages), found=crawled)
                        continue
                    crawled += len(rows)
                    done += 1
                    # `at` IS LEFT TO THE ACTOR. A Batch is a claim-check ref, so naming the
                    # host it holds would mean an async fetch per page from inside the workflow —
                    # a network round trip bought purely to label a progress line. The actor
                    # already knows the host it has open; it publishes `at` onto the same topic.
                    progress(phase="crawl", program=program, done=done,
                             total=len(pages), found=crawled)
                    # PER PAGE, because a crawl is browser-bound and slow, and between the
                    # "crawling N page(s)" line and the final count it otherwise says nothing
                    # for however long Chromium takes. `surface-1789866474` spent eleven
                    # minutes in exactly that silence and ended with zero events.
                    workflow.logger.info(
                        f"crawl: page {done}/{len(pages)}, +{len(rows)} event(s), "
                        f"{crawled} total")

        # EVERY BATCH VOIDED IS NOT A CRAWL THAT FOUND NOTHING. It is a crawl that did not happen,
        # and the two must not return the same thing.
        #
        # MEASURED, on surface-1789864210: the deployed webcrawl was a version behind and raised
        # `AttributeError: 'Seed' object has no attribute 'get'` on the first seed of every batch.
        # Temporal retried ten times, both Nexus operations failed, and this loop caught each one,
        # emitted a `partial`, and carried on — so the workflow returned
        # `{"events": 0, "exchanges": 0, "injection_points": 0}` with status COMPLETED, after
        # eleven minutes. A reader checking whether the crawl ran sees COMPLETED. A reader
        # checking the numbers concludes 8x8 has no web surface.
        #
        # SOME voiding is survivable and stays a `partial` — one unreachable host should not lose
        # the other twelve. ALL of it is a failed run, and saying so is the only way the next
        # phase (which reads what this wrote) does not describe an empty lake as a clean program.
        if voided and voided == len(pages):
            raise RuntimeError(
                f"crawl produced nothing: all {voided} batch(es) voided. This is NOT 'no surface' "
                f"— nothing was crawled at all. The partial events above carry the reason; the "
                f"usual one is a deployed actor behind the workspace, so check "
                f"`kontra workers list` against webcrawl@{WEBCRAWL[1]}.")

        if voided:
            workflow.logger.warning(
                f"crawl complete WITH GAPS: {crawled} event(s), {voided} of {len(pages)} "
                f"batch(es) voided — that ground was not covered.",
                extra={"incomplete": True, "axis": "scope-pages", "phase": "crawl",
                       "voided": voided})
        else:
            workflow.logger.info(
                f"crawl complete: {crawled} http event(s) into http_events_{program}")
        return crawled

    async def _points(self, program, events, scope_name, into=None):
        """Reduce raw traffic into `injection_points` — ONE ROW PER INJECTION POINT PER HOST.

        The grain is the point, not the request, because that is the question the exploit
        workflow asks: "give me everywhere I can put a payload on this host". A row per request
        would make that a GROUP BY at every call site.

        THREE KINDS OF POINT, in one table with a `point_kind` discriminator rather than three
        tables, because a consumer wants them together and ranked — a path and a header are both
        just somewhere bytes go.
        """
        points = catalog.dataset(into or "injection_points")

        # SQL-QUOTED, because these values are prose and prose has apostrophes. "the cache keys on
        # this — one response vs everyone's" terminated the string literal and the whole reduction
        # failed to parse. Escaping at the interpolation is the fix; the alternative — banning
        # apostrophes from the rationale — makes the schema's readability hostage to its plumbing.
        def _lit(s):
            return "'" + str(s).replace("'", "''") + "'"

        rationale = "\n".join(
            f"WHEN lower(name) = {_lit(k)} THEN {_lit(v)}" for k, v in POINT_RATIONALE.items())
        point_names = ",".join(_lit(k) for k in POINT_RATIONALE)

        # The host/path split happens once in the `ev` CTE and is reused by both branches, so
        # the reduction in one place instead of half here and half in a Python loop nobody reruns.
        sql = f"""
        WITH ev AS (
            -- `url_extract_host` / `url_extract_path` are NOT core DuckDB — they come from an
            -- extension the controller does not load, and the reduction died on
            -- "Scalar Function with name url_extract_host does not exist". Regex is core, works
            -- on every build, and keeps this query runnable in a plain `duckdb :memory:` when
            -- somebody wants to test it without the lake.
            SELECT *,
                   regexp_extract(url, '^https?://([^/:?#]+)', 1)          AS _host,
                   coalesce(nullif(regexp_extract(url, '^https?://[^/]*([^?#]*)', 1), ''), '/')
                                                                          AS _path
            FROM "http_events_{program}" WHERE url LIKE 'http%'
        ),
        -- PATHS: every distinct path the crawl actually reached on this host. These are the
        -- fold axis's targets — a codepoint goes INTO the path, so the path has to be real.
        paths AS (
            -- THE STATUS COMES FROM THE RESPONSE, joined on `tx`. It used to be `max(status)`
            -- over the REQUEST rows, and a request event has no status — `_event` defaults it to
            -- -1 — so every path row said `status_code = -1`. Nothing failed; the column was
            -- simply a constant, which is worse, because `hunt._targets` ranks candidate paths
            -- by whether they answered 2xx and that clause silently matched nothing.
            --
            -- LEFT JOIN, so a request whose response never arrived still yields a path row, with
            -- a NULL status that says so. A path the crawl asked for and never got an answer to
            -- is surface; it is just surface we know less about.
            SELECT '{program}' AS program,
                   q._host               AS host,
                   'path'                AS point_kind,
                   q._path               AS point_id,
                   ''                    AS name,
                   ''                    AS value,
                   any_value(q.method)   AS method,
                   max(r.status)         AS status_code,
                   count(*)              AS seen_count,
                   -- IN SCOPE, OR IT IS NOT AN INJECTION POINT.
                   --
                   -- A browser crawl records every host the PAGE touched, not every host the
                   -- program authorised: `fonts.googleapis.com`, `gstatic`, analytics and CDNs
                   -- all arrive as `request` events and reduce into rows stamped with THIS
                   -- program. Measured on nba-public: 21 of 409 hosts in this table are not in
                   -- the program's scope at all.
                   --
                   -- `hunt._targets` joins these to scope and so cannot target them — verified,
                   -- 569 targets and 0 third-party. But this column is named `is_injection_point`
                   -- and a future reader taking it at its word would send hostile bytes to
                   -- somebody nobody authorised. The column has to tell the truth on its own.
                   (s.host IS NOT NULL)  AS is_injection_point,
                   CASE WHEN s.host IS NULL
                        THEN 'OUT OF SCOPE for this program — the crawl loaded it as a '
                             || 'subresource. Inventory only; do not probe.'
                        ELSE 'the crawl reached this path; it routes to a real backend' END
                                         AS point_rationale
            FROM ev q
            LEFT JOIN ev r ON r.tx = q.tx AND r.kind = 'response'
            -- SCHEMA-QUALIFIED, because this query runs in the PROMOTE connection, not the
            -- workbench one. `pageDataset` calls `refreshViews`, which creates a bare-named view
            -- per dataset in `main`; `promoteDataset` does not, so an unqualified name resolves
            -- only for the insert's own source dataset. MEASURED: this reduction retried 48 times
            -- with
            --
            --   Catalog Error: Table with name scope_h1paid does not exist!
            --   Did you mean "lake.standalone.scope_h1paid"?
            --
            -- and DuckDB's suggestion is the fix. `standalone` is where a Dataset loaded by
            -- `kontra dataset create` lives — which is what a scope list always is, since scope
            -- comes from bbscope rather than from a Run. A scope dataset PRODUCED by a Run would
            -- be in `output` instead and this would need to follow it.
            LEFT JOIN (SELECT DISTINCT host FROM lake.standalone."{scope_name}"
                        WHERE program = '{program}') s
                   ON s.host = q._host
            WHERE q.kind = 'request'
            GROUP BY 1, 2, 3, 4, s.host
        ),
        -- HEADERS: request headers are what a client controls. Response headers are the
        -- application TELLING you what it reads, which is the half a wordlist cannot know.
        hdr AS (
            SELECT '{program}' AS program,
                   _host AS host,
                   CASE kind WHEN 'request' THEN 'request-header'
                             ELSE 'response-header' END AS point_kind,
                   lower(unnest(json_keys(headers)))    AS name,
                   _path                AS path,
                   method, status, kind
            FROM ev
        ),
        headers AS (
            SELECT program, host, point_kind,
                   name        AS point_id,
                   name,
                   ''          AS value,
                   any_value(method) AS method,
                   max(status)       AS status_code,
                   count(*)          AS seen_count,
                   -- A REQUEST header is controllable by definition. A RESPONSE header is a
                   -- point only when the application admits it reads it.
                   (point_kind = 'request-header'
                    OR lower(name) IN ({point_names}))
                        AS is_injection_point,
                   CASE {rationale}
                        WHEN point_kind = 'request-header'
                            THEN 'a header the client controls on the way in'
                        ELSE 'seen on the wire; no evidence the application reads it' END
                        AS point_rationale
            FROM hdr
            GROUP BY 1, 2, 3, 4, 5, 6
        )
        SELECT * FROM paths
        UNION ALL BY NAME
        SELECT * FROM headers
        """
        n = await points.insert_from(events, query=sql)
        workflow.logger.info(f"{points.name}: {n} injection point(s) across {program}")
        return n

    async def _exchanges(self, program, events):
        """Re-pair request and response into the fold axis's input Unit.

        `tx` is what makes this possible and it is why the crawler emits events rather than pairs:
        a browser sees a request and its response at different times, and forcing the pairing at
        capture time would drop every request whose response never arrived — which is exactly the
        set a desync hunt cares about.
        """
        exch = catalog.dataset(f"exchanges_{program}")
        sql = f"""
        SELECT '{program}' AS program,
               q.tx        AS id,
               q.url       AS url,
               {{'raw': '', 'method': q.method, 'target': regexp_extract(q.url, '^https?://[^/]*([^?#]*)', 1),
                 'proto': 'HTTP/1.1', 'headers': q.headers, 'body': ''}} AS request,
               {{'raw': '', 'proto': 'HTTP/1.1', 'status': coalesce(r.status, -1),
                 'headers': coalesce(r.headers, '{{}}'), 'body': ''}}    AS response,
               ''          AS notes
        FROM "http_events_{program}" q
        LEFT JOIN "http_events_{program}" r
               ON r.tx = q.tx AND r.kind = 'response'
        WHERE q.kind = 'request' AND q.url LIKE 'http%'
        """
        n = await exch.insert_from(events, query=sql)
        workflow.logger.info(f"exchanges_{program}: {n} replayable exchange(s)")
        return n


if __name__ == "__main__":
    catalog.serve([Surface])
