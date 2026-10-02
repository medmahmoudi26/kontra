"""demo — one URL in, desync findings out.

    ┌──────────────────────────────────────────────────────────────────────────────────────┐
    │  url  ──▶  webcrawl.crawl   ──▶  demo_http_events    one row per request OR response │
    │                   │                                                                  │
    │                   └── SQL fold on `tx` ──▶  demo_exchanges    request+response pairs  │
    │                                                   │                                  │
    │             desync.smuggle  ◀── targets ──────────┤                                  │
    │             desync.split    ◀── exchanges ────────┘                                  │
    │                   │                                                                  │
    │                   └──▶  demo_observations  ──  WHERE ──▶  demo_findings              │
    └──────────────────────────────────────────────────────────────────────────────────────┘

WHY A URL AND NOT A PROGRAM NAME. The `hunt` workflow next door takes a `program` and reads a
`scope_<program>` Dataset somebody loaded first — correct at campaign scale, and a wall for anyone
who wants to see the thing work once. This workflow's entire input is a URL, so the shortest path
from a fresh install to a finding is: press Run.

WHAT IT PROBES, AND WHAT IT DELIBERATELY DOES NOT. A crawl of one page pulls in whatever that page
loads — CDNs, analytics, consent widgets, payment iframes. Those hosts belong to other people and
are almost never in anybody's authorisation. So the probe scope is the SEED HOST ONLY unless a
caller widens it on purpose with `also_probe`; everything else the crawl discovers is recorded as
inventory and never receives a single attack request. See `_in_scope`.

Both axes write ONE Dataset (`demo_observations`) because an observation is an observation — the
axis is a column. Classification happens afterwards in SQL, which is what makes a detection
improvement cost a query rather than another pass over somebody else's network.
"""

from datetime import timedelta

from pydantic import Field
from temporalio import workflow
from temporalio.exceptions import ApplicationError
from typing_extensions import Annotated, TypedDict

from kontra import catalog, fleet, progress
from kontra.fleet import docker_fleet, do_fleet

#: The two Actors this run places. Pinned, so a run resolves against something that was built.
WEBCRAWL = ("webcrawl", "0.2.3")
DESYNC = ("desync", "1.3.3")

#: The URL a run probes when nothing is passed. A real host, on a program that authorises this
#: testing, because a demo whose default target is `example.com` proves the plumbing and nothing
#: about the technique.
DEFAULT_URL = "https://voapi.8x8.com"

#: The four Datasets. NAMES ARE FIXED rather than suffixed per target: the host is a column on
#: every row (`program`), and "show me every finding this demo has ever produced" should be a
#: SELECT rather than a UNION over one table per URL somebody once typed.
EVENTS = "demo_http_events"
EXCHANGES = "demo_exchanges"
OBSERVATIONS = "demo_observations"
FINDINGS = "demo_findings"


class DemoInput(TypedDict, total=False):
    """Every field carries a default and a description, which is what makes the console's launch
    form a form rather than a column of empty boxes — `catalog.workflow_descriptor` derives the
    shape with `TypeAdapter(tp).json_schema()`, and a `#` comment does not exist at runtime.
    """

    url: Annotated[str, Field(
        default=DEFAULT_URL,
        description="The one URL this run starts from. Its host is the only host that receives "
                    "attack traffic — everything else the crawl discovers is inventory, not a "
                    "target. Scheme optional; https is assumed.")]
    also_probe: Annotated[list[str], Field(
        default=[],
        description="Extra hostnames you are authorised to probe, on top of the seed's host. "
                    "Anything the crawl finds that is not in this list or the seed host is "
                    "recorded and never attacked.")]
    depth: Annotated[int, Field(
        default=1,
        description="Links followed from the seed. 0 crawls the seed page alone — which is "
                    "already a real surface, because one page issues requests to every endpoint "
                    "its scripts touch.")]
    max_pages: Annotated[int, Field(
        default=8,
        description="Pages visited, total. The crawl is the slow phase; this is the knob that "
                    "decides how long the run takes.")]
    machines: Annotated[int, Field(
        default=1,
        description="Machines in the Fleet. SCALE. Both Actors land on every Machine, so one is "
                    "a complete run and more is the same run, wider.")]
    sessions: Annotated[int, Field(
        default=2,
        description="Live Sessions per Machine. DENSITY, where `machines` is scale.")]
    provider: Annotated[str, Field(
        default="docker",
        description="Where the Machines land. \"docker\" is a real Fleet of Warden containers on "
                    "the local network and needs no credential. \"cloud\" is DigitalOcean, and "
                    "needs KONTRA_CONTROLLER to be an address a Droplet can reach.")]
    tier: Annotated[int, Field(
        default=1,
        description="How much of the payload corpus to send. 1 quick, 2 standard, 3 full. The "
                    "cost is multiplicative — tier 3 on a reacting host is thousands of "
                    "requests — so the default is the one you can run against a live site.")]
    paths_per_host: Annotated[int, Field(
        default=5,
        description="Crawled paths probed per host, on top of `/`. Ranked by what sits in front "
                    "of a disagreement: a live 2xx backend, then path depth, then how often the "
                    "crawl reached it.")]
    rate_ms: Annotated[int, Field(
        default=150,
        description="Minimum gap between connections TO ONE HOST. This is the politeness knob; "
                    "lower it only against something you own.")]
    skip_smuggle: Annotated[bool, Field(
        default=False,
        description="Turn off the CL.0 / request-smuggling axis.")]
    skip_split: Annotated[bool, Field(
        default=False,
        description="Turn off the request-splitting axis, which is the one that reads crawled "
                    "requests and injects into their own headers.")]


# ─────────────────────────────────────────────────────────────────── pure helpers, no imports

def _split_url(raw: str) -> tuple[str, str, int, str]:
    """`https://voapi.8x8.com/api?x=1` -> `("https", "voapi.8x8.com", 443, "/api")`.

    HAND-ROLLED RATHER THAN `urllib.parse`, because this runs in a Temporal workflow sandbox where
    the import surface is deliberately small and every line of a workflow body has to replay to the
    same answer. String slicing does; it also lets the empty/garbage cases below be explicit rather
    than inherited from a parser written for a different job.
    """
    s = (raw or "").strip()
    scheme = "https"
    if "://" in s:
        head, _, s = s.partition("://")
        scheme = head.lower() or "https"
    s = s.split("#", 1)[0]
    authority, slash, rest = s.partition("/")
    path = ((slash + rest).split("?", 1)[0]) or "/"
    if "@" in authority:                      # strip userinfo; it is not part of the authority
        authority = authority.rsplit("@", 1)[1]
    port = 443 if scheme == "https" else 80
    host = authority
    if host.startswith("["):                  # IPv6 literal: the colons inside are not a port
        host, _, tail = host.partition("]")
        host += "]"
        if tail.startswith(":") and tail[1:].isdigit():
            port = int(tail[1:])
    elif ":" in host:
        host, _, p = host.partition(":")
        if p.isdigit():
            port = int(p)
    return scheme, host.lower(), port, path


def _sql_str(value: str) -> str:
    """One single-quoted SQL literal. Every value this workflow interpolates into a WHERE clause
    comes from the launch form, so it is quoted HERE rather than trusted at each of the six call
    sites."""
    return "'" + str(value).replace("'", "''") + "'"


def _tag() -> str:
    """This Run's Fleet tag — short, unique per Run, and the same on every replay.

    A placement is the Fleet's WHOLE desired state, so two Runs sharing a tag means the second
    one's `place()` is refused. A constant tag would make "press Run twice" a broken state rather
    than a second run.

    `TAG_RE` is `^[a-z][a-z0-9-]{1,15}$`, so the workflow id cannot be passed through: it is too
    long and may legally carry `/`, `_` and uppercase. The last eleven characters are the id's
    timestamp, which is the part that distinguishes one Run from another. Derived from the id
    alone, so a replay computes the same string and a retry rejoins the Fleet it held before.
    """
    wid = workflow.info().workflow_id.lower()
    tail = "".join(c if (c.isascii() and (c.isalnum() or c == "-")) else "-" for c in wid)[-11:]
    return f"d{tail}"[:16]


def _provider(name: str, machines: int):
    """Which Fleet, as an object rather than a flag.

    An unrecognised value degrades to `docker` and SAYS SO rather than raising: a bare `raise` in
    workflow code is a workflow-TASK failure, which the SDK retries forever, so the run wedges with
    nothing to act on. A demo must not wedge on a typo in its own form.
    """
    if name == "cloud":
        return do_fleet(machines=machines)
    if name != "docker":
        workflow.logger.warning("demo: provider %r is not \"docker\" or \"cloud\" — using docker",
                                name)
    return docker_fleet(machines=machines)


def _num(req, key, default):
    """`req.get(k) or default` is wrong for a number: `machines=0` — the way you ask a run to
    provision nothing — is falsy, so `or 1` reads it as a request for one."""
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class Demo:
    """Crawl one URL, then probe what it found for HTTP request smuggling and request splitting.

    Give it a URL and it provisions a Fleet, crawls the page, folds the captured traffic into
    replayable request/response pairs, runs both desync axes against the seed host, and promotes
    the observations that carry proof into `demo_findings`. Everything it learns stays queryable
    after the run: four Datasets, one row per fact.
    """

    @workflow.run
    async def run(self, req: DemoInput | None = None) -> dict:
        # `req` MUST DEFAULT. `kontra workflow start` with no `--input` passes no argument at all,
        # and a required parameter makes that a workflow-task failure rather than a run.
        req = req or {}

        raw = str(req.get("url") or DEFAULT_URL)
        scheme, host, port, path = _split_url(raw)
        if not host or "." not in host.strip("[]"):
            # NON-RETRYABLE, so a typo FAILS the run with a readable reason instead of wedging the
            # workflow task in a retry loop nobody can read.
            raise ApplicationError(
                f"{raw!r} has no usable hostname — pass something like https://voapi.8x8.com",
                type="BadURL", non_retryable=True)

        seed = f"{scheme}://{host}:{port}{path}" if port not in (80, 443) else f"{scheme}://{host}{path}"
        # THE PROBE SCOPE, decided once and used by every phase below. The seed host is always in
        # it; everything else is opt-in, because the crawl WILL discover hosts that belong to other
        # people and this workflow sends attack traffic.
        extra = [str(h).strip().lower() for h in (req.get("also_probe") or []) if str(h).strip()]
        in_scope = [host] + [h for h in extra if h != host]
        scope_sql = ", ".join(_sql_str(h) for h in in_scope)

        events = catalog.dataset(EVENTS)
        exchanges = catalog.dataset(EXCHANGES)
        observations = catalog.dataset(OBSERVATIONS)

        machines = _num(req, "machines", 1)
        sessions = _num(req, "sessions", 2)
        tier = _num(req, "tier", 1)

        # The desync Actor's dials. Spelled once and shared by both axes, because the two axes
        # differ in what they SEND, not in how politely they send it.
        params = {
            "program": host,
            "tier": tier,
            "rate_ms": _num(req, "rate_ms", 150),
            "max_points": _num(req, "max_points", 40),
            "max_seconds": _num(req, "max_seconds", 600),
            "concurrency": _num(req, "concurrency", 20),
            "timeout_ms": _num(req, "timeout_ms", 4000),
            "backoff": bool(req.get("backoff", True)),
            "backoff_max_ms": _num(req, "backoff_max_ms", 30000),
        }
        # THE LIVENESS BOUND HAS TO FIT THE UNIT. The Actor beats once per committed Unit, and one
        # Unit here is a whole host against a class of payloads — minutes, not seconds. Against the
        # handler's 2-minute default the first beat arrives after Temporal has already killed the
        # attempt, and the run dies with `activity Heartbeat timeout` while the worker is sending
        # probes perfectly.
        call_opts = {
            "schedule_to_close_timeout": timedelta(minutes=_num(req, "call_minutes", 45)),
            "debug_heartbeat_seconds": _num(req, "heartbeat_seconds", 900),
        }

        workflow.logger.info(
            "demo: %s — crawling up to %d page(s), probing %s",
            seed, _num(req, "max_pages", 8), ", ".join(in_scope))
        # The denominator before anything happens, as a FACT rather than only a sentence: the
        # console reads `done`/`total` as numbers off the log record's `extra=` instead of parsing
        # them back out of prose.
        progress("demo", "phases", done=0, total=4, target=host)

        crawled = folded = screened = split = promoted = 0
        machines_up, cost_hourly, held_seconds = None, None, None

        async with fleet.hold(_provider(str(req.get("provider") or "docker"), machines),
                              tag=_tag()) as f:
            if f.shared:
                workflow.logger.warning(
                    "fleet %s was already held by another run — this scope converges nothing, so "
                    "machine and cost figures are UNAVAILABLE, not zero.", f.fqn,
                    extra={"incomplete": True, "axis": "fleet"})
            # BOTH ACTORS ON THE SAME FLEET. `place` is idempotent desired state per Artifact, not
            # a replace, so two calls put two Workers on every Machine — which is what lets one
            # Fleet serve a crawl and a scan without paying for two.
            await f.place(WEBCRAWL[0], WEBCRAWL[1], sessions=sessions)
            await f.place(DESYNC[0], DESYNC[1], sessions=sessions)
            # NOT OPTIONAL, and with no actor named it waits for BOTH placements. `place` returns
            # while the container is still starting; a Batch dispatched into that gap waits on a
            # queue nobody serves, which is indistinguishable from a hung run.
            await f.ready()
            machines_up, cost_hourly = len(f.inventory), f.cost_hourly()
            held = workflow.now()
            workflow.logger.info("%d machine(s) polling webcrawl@%s and desync@%s — %s",
                                 machines_up, WEBCRAWL[1], DESYNC[1], f.cost_words())
            try:
                # ── 1 · CRAWL ────────────────────────────────────────────────────────────────
                crawled = await self._crawl(req, events, seed, host)
                progress("demo", "phases", done=1, total=4, target=host, events=crawled)
                if await events.state() is None:
                    # NOTHING WAS EVER CAPTURED, so every phase below would read a Dataset that
                    # does not exist. Fail the RUN — cleanly and non-retryably, so it goes red with
                    # a readable reason — rather than letting a `batches` ValueError wedge the
                    # workflow task in a retry loop nobody can act on.
                    raise ApplicationError(
                        f"the crawl captured nothing from {seed} and {EVENTS} does not exist — "
                        f"check the URL is reachable from the Fleet before reading this as a "
                        f"clean scan",
                        type="NothingCrawled", non_retryable=True)

                # ── 2 · FOLD ─────────────────────────────────────────────────────────────────
                folded = await self._fold(events, exchanges, host)
                progress("demo", "phases", done=2, total=4, target=host, exchanges=folded)

                # ── 3 · PROBE ────────────────────────────────────────────────────────────────
                if not req.get("skip_smuggle"):
                    screened = await self._smuggle(
                        req, exchanges, observations, host, scope_sql, params, call_opts)
                if not req.get("skip_split"):
                    split = await self._split(
                        req, exchanges, observations, host, scope_sql, params, call_opts)
                progress("demo", "phases", done=3, total=4, target=host,
                         observations=screened + split)
            finally:
                # IN `finally`, because a cancelled or failed Run held the Machines just the same
                # and is exactly the Run whose cost somebody will ask about.
                held_seconds = (workflow.now() - held).total_seconds()
                workflow.logger.info("fleet released — %s", f.cost_words(held_seconds))

        # ── 4 · PROMOTE ──────────────────────────────────────────────────────────────────────
        # OUTSIDE THE FLEET SCOPE on purpose: this is SQL over data already on disk, so it does not
        # need a Machine and must not keep one billing while it runs.
        promoted = await self._promote(observations, host)
        progress("demo", "phases", done=4, total=4, target=host, findings=promoted)

        # SEAL, so these Datasets are not exempt from retention for ever. `open` is what a crash
        # leaves; saying so explicitly is a different claim.
        for ds in (events, exchanges, observations):
            try:
                await ds.seal()
            except Exception as err:  # noqa: BLE001 - a Dataset that will not close is not a failed run
                workflow.logger.warning("could not seal %s: %s", ds.name, err)

        workflow.logger.info(
            "demo complete on %s — %d event(s), %d exchange(s), %d observation(s), %d finding(s)",
            host, crawled, folded, screened + split, promoted)
        # THE NUMBERS COME BACK rather than only being logged: a log line dies with the run's rail,
        # a returned field is a named card on the run page and is what a caller can add up.
        return {
            "url": seed,
            "target": host,
            "probed": in_scope,
            "events": crawled,
            "exchanges": folded,
            "observations": screened + split,
            "smuggle_observations": screened,
            "split_observations": split,
            "findings": promoted,
            "machines": machines_up,
            "cost_hourly_usd": None if cost_hourly is None else round(cost_hourly, 4),
            "fleet_held_seconds": None if held_seconds is None else round(held_seconds, 1),
        }

    # ────────────────────────────────────────────────────────────────────────── 1 · the crawl

    async def _crawl(self, req, events, seed: str, host: str) -> int:
        """One seed in, every request and response the page made out.

        One page is already a real surface: a modern page issues requests to every endpoint its
        scripts touch, and those requests carry the headers this host actually reads — which is
        what no static wordlist knows.
        """
        params = {
            "depth": _num(req, "depth", 1),
            "max_pages": _num(req, "max_pages", 8),
            "parallel_seeds": 1,          # one seed; concurrency here would buy nothing
            "settle_ms": _num(req, "settle_ms", 2500),
            "nav_timeout_ms": _num(req, "nav_timeout_ms", 25000),
            "block_media": True,          # images and fonts are bytes without inventory value
            "ignore_https_errors": True,  # real scope hosts routinely have broken chains
        }
        async with catalog.actor(*WEBCRAWL) as c:
            try:
                rows, dropped = await c.crawl(
                    [{"seed": seed, "program": host}], events,
                    params=params,
                    schedule_to_close_timeout=timedelta(minutes=_num(req, "crawl_minutes", 20)),
                    debug_heartbeat_seconds=600,
                )
            except Exception as exc:  # noqa: BLE001 - the reason is the payload
                # A CRAWL THAT FAILS IS NOT A RUN THAT FAILS, as long as it says so. The probe
                # phases read Datasets, so a previous run's exchanges are still probeable — and a
                # workflow that dies here would destroy the Fleet it just paid to provision.
                workflow.logger.warning(
                    "crawl of %s failed: %r — continuing against whatever is already in %s",
                    seed, exc, EVENTS,
                    extra={"incomplete": True, "axis": "crawl", "phase": "crawl"})
                return 0
        if dropped:
            workflow.logger.warning("crawl dropped %d unit(s)", len(dropped),
                                    extra={"incomplete": True, "axis": "crawl"})
        workflow.logger.info("crawl: %d event(s) from %s", len(rows), seed)
        return len(rows)

    # ───────────────────────────────────────────────────────────────────────── 2 · the fold

    async def _fold(self, events, exchanges, host: str) -> int:
        """Re-pair request and response into the Unit the splitting axis takes.

        `tx` is what makes this possible, and it is why the crawler emits events rather than pairs:
        a browser sees a request and its response at different times, and pairing at capture time
        would drop every request whose response never arrived — exactly the set a desync hunt cares
        about.
        """
        # HEADERS ARE A LIST OF {name, value}, NOT THE CRAWL'S JSON OBJECT. The crawler stores
        # `headers` as a VARCHAR holding a JSON map, which is right for mining header NAMES in SQL.
        # The Actor reads this into Go, where `unit.Message.Headers` is `[]unit.Header` — and an
        # object is not an array:
        #
        #   bad input unit: json: cannot unmarshal string into Go struct field
        #   Message.request.headers of type []unit.Header
        #
        # which fails EVERY unit of every batch. A response that never arrived has no headers, and
        # an empty LIST is the honest spelling of that.
        hdrs = ("[{{'name': k, 'value': json_extract_string({col}, '$.\"' || k || '\"')}} "
                "for k in json_keys(coalesce({col}, '{{}}'))]")
        prog = _sql_str(host)
        sql = f"""
        SELECT {prog}      AS program,
               q.tx        AS id,
               q.url       AS url,
               {{'raw': '', 'method': q.method,
                 'target': regexp_extract(q.url, '^https?://[^/]*([^?#]*)', 1),
                 'proto': 'HTTP/1.1', 'headers': {hdrs.format(col='q.headers')}, 'body': ''}}
                           AS request,
               {{'raw': '', 'proto': 'HTTP/1.1', 'status': coalesce(r.status, -1),
                 'headers': {hdrs.format(col='r.headers')}, 'body': ''}}
                           AS response,
               ''          AS notes
        FROM "{EVENTS}" q
        LEFT JOIN "{EVENTS}" r ON r.tx = q.tx AND r.kind = 'response' AND r.program = {prog}
        WHERE q.kind = 'request' AND q.program = {prog} AND q.url LIKE 'http%'
        """
        n = await exchanges.insert_from(events, query=sql)
        workflow.logger.info("fold: %d replayable exchange(s) in %s", n, EXCHANGES)
        return n

    # ──────────────────────────────────────────────────────────── 3a · the smuggling axis

    #: The nine fields a `desync.Target` reads. A query that omits one hands the Actor Go's zero
    #: value for it with nothing raising — which is how `endpoint` once came to be empty everywhere.
    _TARGETS = """
        WITH seen AS (
            SELECT regexp_extract(url, '^https?://([^/:?#]+)', 1)                      AS host,
                   lower(regexp_extract(url, '^(https?)://', 1))                       AS scheme,
                   coalesce(nullif(regexp_extract(url, '^https?://[^/]*([^?#]*)', 1), ''), '/')
                                                                                       AS endpoint,
                   max(response.status)                                                AS status,
                   count(*)                                                            AS seen_count
            FROM "{table}"
            WHERE program = {prog}
            GROUP BY 1, 2, 3
        ),
        scoped AS (SELECT * FROM seen WHERE host IN ({scope})),
        ranked AS (
            SELECT *, row_number() OVER (
                PARTITION BY host
                -- HOW A PATH EARNS ITS SLOT, and it ranks by what sits in front of a
                -- DISAGREEMENT rather than by what is interesting to read: a 2xx is served by a
                -- live backend where a 404 is served by the edge (the half of the pair we
                -- already have); every path segment is another routing decision; and a path the
                -- crawl reached repeatedly is real routing rather than one dead link.
                ORDER BY (status BETWEEN 200 AND 299) DESC,
                         length(endpoint) - length(replace(endpoint, '/', '')) DESC,
                         seen_count DESC, endpoint
            ) AS rn
            FROM scoped WHERE endpoint <> '/' AND endpoint LIKE '/%'
        )
        -- ROOT IS ALWAYS INCLUDED, and not as a courtesy: it is the one endpoint guaranteed to
        -- exist and the one every prior observation was taken at. UNION, not UNION ALL — a
        -- crawled path can be the same string as the root and two identical Targets are two
        -- baselines against the same door.
        SELECT {prog} AS program, host, scheme,
               CASE WHEN scheme = 'http' THEN 80 ELSE 443 END AS port,
               '' AS host_header, '' AS sni, '' AS header_block, '' AS class,
               '/' AS endpoint
        FROM scoped
        UNION
        SELECT {prog} AS program, host, scheme,
               CASE WHEN scheme = 'http' THEN 80 ELSE 443 END AS port,
               '' AS host_header, '' AS sni, '' AS header_block, '' AS class,
               endpoint
        FROM ranked WHERE rn <= {per_host}
    """

    async def _smuggle(self, req, exchanges, observations, host, scope_sql, params, call_opts):
        """Send requests whose body length the front-end and back-end disagree about.

        The Unit is a TARGET — a door to knock on — because this axis is about what the two
        parsers in front of a host do with a length, which is a property of the host and the path
        rather than of any particular crawled request.
        """
        sql = self._TARGETS.format(table=EXCHANGES, prog=_sql_str(host), scope=scope_sql,
                                   per_host=_num(req, "paths_per_host", 5))
        # COUNTED BEFORE IT IS PAGED, because `batches` RAISES on an empty first page — "a dataset
        # that is gone, misspelled or filtered to nothing is a mistake, not an empty sweep" — and a
        # ValueError raised in workflow code is a workflow-TASK failure the SDK retries forever.
        # A crawl that found no in-scope door is a real outcome of a real run; it must report, not
        # wedge.
        if await self._count(exchanges, f"SELECT count(*) AS n FROM ({sql})") == 0:
            workflow.logger.warning(
                "smuggling axis: the crawl found no in-scope endpoint on %s, so nothing was "
                "probed — that is UNSCANNED ground, not a clean host.", host,
                extra={"incomplete": True, "axis": "smuggle"})
            return 0
        n = 0
        async with catalog.actor(*DESYNC) as d:
            async for batch in exchanges.batches(_num(req, "size", 50),
                                                 order_by="host, endpoint", query=sql):
                obs, dropped = await d.smuggle(batch, observations,
                                               params={**params, "phase": "screen"}, **call_opts)
                if dropped:
                    workflow.logger.warning("smuggle dropped %d unit(s)", len(dropped),
                                            extra={"incomplete": True, "axis": "smuggle"})
                n += len(obs)
                workflow.logger.info("smuggle: +%d observation(s), %d so far", len(obs), n)
        workflow.logger.info("smuggling axis complete: %d observation(s)", n)
        return n

    # ───────────────────────────────────────────────────────────── 3b · the splitting axis

    async def _split(self, req, exchanges, observations, host, scope_sql, params, call_opts):
        """Inject a codepoint whose low byte is 0x0A into a request the crawl really made.

        The Unit is an EXCHANGE, not a target, and that is the whole difference between the axes:
        this one injects into the header block the browser actually sent, so the injection points
        are the headers this host reads rather than the ones a wordlist guesses.
        """
        where = (f"program = {_sql_str(host)} AND "
                 f"regexp_extract(url, '^https?://([^/:?#]+)', 1) IN ({scope_sql})")
        # The same guard as the smuggling axis, for the same reason: an empty first page raises,
        # and a raise here wedges the workflow task rather than failing the run.
        if await self._count(
                exchanges, f'SELECT count(*) AS n FROM "{EXCHANGES}" WHERE {where}') == 0:
            workflow.logger.warning(
                "splitting axis: no in-scope exchange was captured for %s, so nothing was "
                "probed — that is UNSCANNED ground, not a clean host.", host,
                extra={"incomplete": True, "axis": "split"})
            return 0
        n, voided, streak = 0, 0, 0
        async with catalog.actor(*DESYNC) as d:
            async for batch in exchanges.batches(_num(req, "size", 50),
                                                 order_by="url", where=where):
                # THE BATCH IS THE UNIT OF FAILURE. A try/except around the whole loop turns one
                # bad page into "the axis found nothing" — a partial scan reported as a complete
                # one, which is the failure this engine's erratic/voided discipline exists to stop.
                try:
                    obs, dropped = await d.split(batch, observations,
                                                 params={**params, "phase": "split"}, **call_opts)
                except Exception as exc:  # noqa: BLE001 - the reason is the payload
                    voided += 1
                    streak += 1
                    workflow.logger.warning(
                        "split batch voided (%d so far): %r", voided, exc,
                        extra={"incomplete": True, "axis": "split", "voided": voided})
                    if streak >= 3:
                        workflow.logger.warning(
                            "splitting axis ABANDONED after %d observation(s): 3 consecutive "
                            "failures. Everything past this point in url order is UNSCANNED, "
                            "not clean.", n,
                            extra={"incomplete": True, "axis": "split", "scanned": n})
                        break
                    continue
                streak = 0
                if dropped:
                    workflow.logger.warning("split dropped %d unit(s)", len(dropped),
                                            extra={"incomplete": True, "axis": "split"})
                n += len(obs)
                workflow.logger.info("split: +%d observation(s), %d so far", len(obs), n)
        if voided:
            workflow.logger.warning(
                "splitting axis complete WITH GAPS: %d observation(s), %d batch(es) voided — that "
                "ground was not covered.", n, voided,
                extra={"incomplete": True, "axis": "split", "voided": voided})
        else:
            workflow.logger.info("splitting axis complete: %d observation(s)", n)
        return n

    # ───────────────────────────────────────────────────────────────────── 4 · the promotion

    async def _promote(self, observations, host: str) -> int:
        """Promote signalling observations into `demo_findings` — this workflow's output.

        `is_proof` is computed HERE rather than left to every reader, and it takes three signals,
        not two. `authority_injected` and `canary_reflected` each mean a value this scanner minted
        came back where no innocent path puts it — but a server that pipelines reflects a canary
        for a reason that has nothing to do with the gadget and is not a vulnerability.
        `control_survived` means the Actor re-ran the identical request well-formed and the
        behaviour did NOT reproduce. Without it, `is_proof` can be true of a row nobody controlled.

        Both requests are columns, rendered as resendable HTTP and as base64 of the wire bytes: a
        desync payload is made of exactly the bytes a clipboard destroys, so the readable form
        would arrive with its bug normalised out and reproduce nothing.
        """
        findings = catalog.dataset(FINDINGS)
        prog = _sql_str(host)
        # `insert_from` is NOT idempotent — re-promoting re-appends every row already there — so
        # the run_id anti-join is doing real work on a Dataset this demo appends to for ever.
        promote = f"""
            SELECT o.ts, o.program AS target, o.host, o.port, o.scheme, o.axis, o.class,
                   o.endpoint, o.technique, o.gadget, o.tier,
                   o.original_request,  o.original_request_b64,
                   o.triggered_request, o.triggered_request_b64,
                   o.canary, o.signals, o.signal_count,
                   (list_contains(o.signals, 'authority_injected')
                       OR list_contains(o.signals, 'canary_reflected'))
                     AND list_contains(o.signals, 'control_survived')   AS is_proof,
                   o.run_id, o.version AS actor_version,
                   'new' AS status
            FROM "{OBSERVATIONS}" o
            WHERE o.program = {prog}
              AND o.run_id = {_sql_str(self._run_id())}
              AND NOT o.erratic AND NOT o.voided
              AND o.signal_count > 0
        """
        try:
            n = await findings.insert_from(observations, query=promote)
        except Exception as exc:  # noqa: BLE001 - the reason is the payload
            # The scan already happened and its rows are in `demo_observations`. A promote that
            # cannot run is a lost SELECT, not a lost run, and must not throw away the bytes.
            workflow.logger.warning(
                "promote failed: %r — the observations are in %s and can be promoted by query",
                exc, OBSERVATIONS,
                extra={"incomplete": True, "axis": "promote"})
            return 0
        workflow.logger.info("promote: %d finding(s) into %s", n, FINDINGS)
        return n

    def _run_id(self) -> str:
        """This Run's id, as the Actor stamps it on every observation it emits."""
        return workflow.info().workflow_id

    async def _count(self, ds, sql: str) -> int:
        """One aggregate, one number — and a Dataset that is not there yet is 0, not a failed Run.

        `batches` is the only read the SDK gives a workflow, so a scalar is a one-page read of a
        one-row query. `order_by` is required by that API for a real reason and is harmless on a
        single aggregate row. A `count(*)` always returns a row, so this read never trips the
        empty-first-page rule it exists to protect the callers from.

        `via=DESYNC` IS LOAD-BEARING. A Dataset page is a ref, and turning one back into rows is
        `kontra.fetch_blob` — an activity only an ACTOR HOST registers. With no actor named, the
        page schedules onto the literal `"-shared"` queue, which nothing polls: the workflow waits
        there for ever with no error anywhere. That is why both callers sit inside the fleet scope.
        """
        try:
            async for batch in ds.batches(1, order_by="n", query=sql, via=DESYNC):
                rows = await batch.rows()
                return int(rows[0]["n"]) if rows else 0
        except Exception as exc:  # noqa: BLE001 - a missing dataset is not a failed run
            workflow.logger.warning(
                "could not count %s (%r) — treating as empty, which may understate this run",
                ds.name, exc, extra={"incomplete": True, "axis": "accounting"})
        return 0


if __name__ == "__main__":
    catalog.serve([Demo])
