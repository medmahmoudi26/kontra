"""hunt — attack the surface `surface` mapped, two ways, and own the leads.

THE QUESTION: does anything in front of these hosts disagree with anything behind them, about
where a request ENDS or about which bytes mean NEWLINE?

This is the second of the two workflows. It sends every hostile byte in the system and it crawls
nothing: the surface is an INPUT. That separation is what lets the payload corpus grow and be
re-swept against the same crawl at zero browser cost, and what stops a re-scan from re-crawling.

    ┌ surface (the other workflow) ┐        ┌ hunt (this file) ──────────────────────┐
    │ injection_points   where     │───────▶│  A screen / B sweep   SMUGGLING        │
    │ exchanges_<prog>   what      │        │  C sweep              SPLITTING        │
    └──────────────────────────────┘        │    → observations → desync_leads       │
                                            └────────────────────────────────────────┘

TWO BUGS, TWO METHODS, ONE ORACLE DISCIPLINE. They share a socket primitive and a per-host pacer
and nothing else, which is why they are two Methods and not one with a flag:

    split     HTTP REQUEST SPLITTING. A codepoint whose low byte is 0x0A survives a lossy
              narrowing and becomes a real LF, so the request line comes apart and the attacker
              writes the next header. Input is a crawled EXCHANGE, because the response half
              names injection points no wordlist knows. Oracle: a MATCHED CONTROL — the same
              encoding one value higher — because against a plain baseline every host that
              echoes its own URL looks vulnerable.

    smuggle   HTTP REQUEST SMUGGLING. Two parsers disagree about where the body ends, so the
              front-end forwards bytes the back-end reads as the start of somebody else's
              request. Input is a TARGET, because the payload is a whole rendered request.
              Oracle: the SOCKET — normal, attack, normal, all written before any is read.

`fold` and `framing` still appear below in prose. They name the MECHANISM — a codepoint that
folds a line, a disagreement about message framing. `split` and `smuggle` name the BUG, and the
bug is what goes in the `axis` column, because that is the word the rest of the field uses.

THIS WORKFLOW OWNS `desync_leads`. A lead is not an observation: observations are every probe,
kept forever, most of them boring. A lead is a probe that earned a human's attention, and it
carries THE ORIGINAL REQUEST AND THE TRIGGERED REQUEST IN SEPARATE FIELDS — because the finding
IS the difference between two requests, and asking a reader to reconstruct the normal one from a
vector id and a point id is asking them to take the analyzer's word for it.

Run it:

    kontra build --actor kontra-actors/go/desync
    kontra workflow serve python/hunt
    kontra workflow start python/hunt --wait \\
        --input '{"program": "8x8", "machines": 4, "rate_ms": 200}'
"""
from temporalio import workflow
from typing_extensions import TypedDict

import asyncio
from dataclasses import dataclass
from datetime import timedelta

from kontra import catalog, fleet


async def _log_drops(dropped, *, phase: str) -> None:
    """Surface what a Method call DROPPED — a workflow's choice, not the framework's.

    Nothing logs a dropped Unit by default: isolation comes back as the second half of the tuple
    (`obs, dropped = await d.smuggle(...)`) and it is THIS branch that decides to say so. Each line
    is the ACTOR'S OWN error message, verbatim — `PerUnitFailure.error.message`, the message the Unit
    actually failed with — never a sentence composed here. The record it happened on rides in the
    line's structured fields (`host`/`endpoint`/`url`/`unit_id`/`point`/`class`/`category`), so the
    run's log rail can show WHICH Unit and WHY without the message having to carry either.

    `bool(dropped)` and the fetch are free when nothing was lost (`isolated == 0`, the common case),
    so this costs a blob fetch only on the runs that actually dropped something.
    """
    if not dropped:
        return
    for f in await dropped.rows():
        err = f.get("error") or {}
        unit = f.get("unit") or {}
        fields = {"phase": phase, "category": f.get("category", "")}
        for k in ("host", "endpoint", "url", "unit_id", "point", "class"):
            if isinstance(unit, dict) and unit.get(k) is not None:
                fields[k] = unit[k]
        workflow.logger.error(err.get("message") or "unit isolated with no error message", extra=fields)

# 1.2.0 IS THE VERSION THAT CAN BE BELIEVED, and the version is how a reader tells its rows from
# the ones that could not be.
#
# (1.1.0 exists in the registry and never ran. It carried the shape changes below but not the
# oracle change, and `kontra deploy` correctly refused to republish that tag with different code
# — a digest is a promise about bytes, not a mutable pointer. Bumping was cheaper than arguing
# with the rule that exists to stop exactly this.)
#
# THE ORACLE CHANGED. 1.0.0 had no matched control on this axis — the file said so in as many
# words — so `canary_reflected` fired whenever a server read the smuggled body as a request,
# whether or not the obfuscation had anything to do with it. A server that simply pipelines does
# that to everyone. On 2026-09-18 that produced 40 `is_proof` leads and two reports, and both were
# retracted: a plain `Content-Length: 0` fired on paypalobjects.com 3 times in 6 while the
# obfuscated attacks fired 1 and 2.
#
# It re-runs every firing technique with a well-formed Content-Length — same body, same canary,
# same endpoint, gadget removed — and withdraws the claim when that reproduces the behaviour. Two
# new signal names carry the outcome (`control_survived`, `control_also_fired`) and `is_proof`
# below now requires the first.
#
# THE ROW SHAPE CHANGED WITH IT:
#
#   + endpoint          which path the probe went to. Absent before, which is how an entire
#                       campaign went to `/` without its own output being able to say so.
#   + control_request,  the third request and what it answered. Empty on a row that fired
#     control_post      nothing, because no control is run where there is no claim to test.
#   + elided,           what the row dropped, and how many bytes. A `resp_raw` that is empty
#     resp_raw_elided   because nothing arrived and one that is empty because it was not kept
#                       are different facts.
#   - sent_raw          a base64 twin of `triggered_request_b64`, byte-identical on 820 of the
#                       first campaign's 833 rows.
#
# A query written against 1.0.0 rows that selects `sent_raw` should fail loudly here rather than
# read an empty column as an empty request — and more importantly, a 1.0.0 row's `is_proof` and a
# 1.2.0 row's are not the same claim. The version is what lets a reader see which they have.
DESYNC = ("desync", "1.2.1")

# The Dataset this workflow OWNS. Not program-suffixed: a lead's program is a column, and the
# question "show me every confirmed desync we have ever had" should not be a UNION over one table
# per program. The suffix belongs on things scoped to one authorisation (scope, exchanges), not on
# the output every reader wants to see whole.
LEADS = "desync_leads"


@dataclass(frozen=True)
class Phase:
    """Everything a phase of this run needs, resolved ONCE in `run()`.

    ── WHY THIS EXISTS (github #20) ────────────────────────────────────────────────────────────

    `_sweep` took ten positional parameters and `_split` six of the same ones, so adding one knob
    meant editing three signatures and every call site, and the argument ORDER was memorised
    rather than read. The helpers are not activities and must not become them — a workflow body is
    already durable, and collapsing the page loop into one activity would mean a crash re-running
    the sweep from page 1, re-sending attack traffic to hosts already probed.

    ── FROZEN, AND THAT IS THE LOAD-BEARING PART ───────────────────────────────────────────────

    A workflow body must produce the same sequence of commands on replay. A mutable bag threaded
    through three phases is the easiest way to lose that: one `p.size = 4` inside a retry path and
    the replay diverges from the history it is replaying. `frozen=True` makes that a `TypeError`
    at the moment it is written rather than a non-determinism error days later.
    """

    req: "HuntInput"
    program: str
    scope: object          # catalog.Dataset — untyped here to keep the sandbox import surface small
    scope_name: str
    observations: object   # catalog.Dataset
    size: int
    lanes: int
    params: dict
    call_opts: dict
    erratic_sql: str


class HuntInput(TypedDict, total=False):
    program: str       # selects scope_<program> / exchanges_<program>; stamped on every row
    machines: int      # fleet width. SCALE.
    sessions: int      # live Sessions per Machine. DENSITY.
    size: int          # units per Batch
    rate_ms: int       # per-HOST minimum gap between connections
    backoff: bool      # let a host widen its own gap on 429/503/reset (default on)
    backoff_max_ms: int  # ceiling on that widening
    tier: int          # fold: vector tier ceiling. 1 quick, 2 standard, 3 full.
    max_points: int    # fold: injection points probed per exchange
    concurrency: int   # units in flight inside one Worker
    timeout_ms: int
    call_minutes: int
    heartbeat_seconds: int  # liveness bound per dispatch; see call_opts

    # ONE SWITCH PER PHASE. A/B/C are three independent decisions, so leaving any of them
    # unswitchable makes a whole axis unreachable on its own.
    skip_screen: bool  # phase A off — do not re-run the smuggling screen over the scope
    skip_sweep: bool   # phase B off — screen only, no per-class smuggling sweep
    skip_split: bool   # phase C off — smuggling axis only, no splitting sweep
    skip_corpus: bool  # do not publish the payload corpus
    skip_leads: bool   # scan only; do not promote

    into: str          # write observations HERE instead of `observations`
    leads: str         # write leads HERE instead of `desync_leads`
    techniques: str    # write the corpus HERE instead of `techniques`
    corpus_shards: int # how many Units the corpus is split across (payload size, not speed)
    # ONE SCOPE DATASET FOR MANY PROGRAMS. `scope_<program>` is the default and stays the
    # default — it is right when a program is a project. It is wrong at campaign scale: the
    # paying HackerOne surface is 460 programs, and 460 near-identical Datasets whose only
    # difference is a value already sitting in the `program` COLUMN is a catalog nobody can
    # read. Every query here already filters `WHERE program = '<program>'`, so the NAME is the
    # only thing that had to become a parameter.
    scope: str           # read scope HERE instead of `scope_<program>`

    # WHERE THE SMUGGLING AXIS KNOCKS. Without these it knocked on `/` and only `/`, for every
    # host, for the whole first campaign — see `_targets`.
    paths_from: str      # dataset holding the crawl's paths (default `injection_points`)
    paths_per_host: int  # crawled paths screened per host, on top of `/` (default 5)
    bounty_only: bool    # screen only scope rows marked bounty-eligible

    only: str          # fold: ILIKE pattern on the exchange url — aim the axis at a target
    chunk: int         # pages per Session. Bounds how long one Machine is pinned.
    lanes: int         # concurrent dispatch lanes. >1 can outrun a single materializer slot.
    attach: bool       # drive Workers ALREADY polling; hold no fleet of your own


def _num(req, key, default):
    """`req.get(k) or default` is WRONG for a number here: `machines=0` — the way you ask a run to
    provision nothing — is falsy, so `or 4` reads it as a request for four and provisions them."""
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class Hunt:
    @workflow.run
    async def run(self, req: HuntInput) -> dict:
        program = req.get("program") or "8x8"
        size = _num(req, "size", 100)
        lanes = max(1, _num(req, "lanes", 1))

        scope_name = req.get("scope") or f"scope_{program}"
        scope = catalog.dataset(scope_name)
        observations = catalog.dataset(req.get("into") or "observations")

        params = {
            "program": program,
            "rate_ms": _num(req, "rate_ms", 100),
            "tier": _num(req, "tier", 1),
            # 40 IS THE ACTOR'S OWN DEFAULT, and 12 was starving the thing this axis is for.
            # `inject.Enumerate` ranks response-derived points first and the cap keeps a prefix,
            # so at 12 a page with any query string spent every remaining slot on path and query
            # and probed ZERO request headers — measured, and now pinned by
            # inject.TestTheCapKeepsHeadersOnAQueryHeavyPage.
            "max_points": _num(req, "max_points", 40),
            "concurrency": _num(req, "concurrency", 30),
            "timeout_ms": _num(req, "timeout_ms", 4000),
            # OFF IS SOMETHING YOU HAVE TO ASK FOR. `rate_ms` alone says what we are willing to
            # send and nothing about what the origin can take, so at 100ms a host answering 429
            # would otherwise keep being hit at 100ms — the fastest route to a blocked source
            # address, and to a page of `erratic` rows about a host that was merely throttled.
            "backoff": bool(req.get("backoff", True)),
            "backoff_max_ms": _num(req, "backoff_max_ms", 30000),
        }
        # 20 MINUTES WAS TOO SHORT FOR THE PHASE THAT DOES THE WORK, and the way it failed was the
        # expensive way: the screen finished in 43 seconds, the sweep then ran for 21 minutes and
        # Temporal killed the activity at the ceiling — `nexus operation completed unsuccessfully /
        # operation timed out` — AFTER every probe had been sent. The bytes went out; the Run was
        # marked failed and its Dataset left `open`.
        #
        # The screen is bounded by the host count; the SWEEP is bounded by the corpus, which grows.
        # So the default has to fit the phase that scales, not the one that does not.
        # THE LIVENESS BOUND HAS TO FIT THE UNIT, and for this workflow it does not by default.
        # The actor beats ONCE PER COMMITTED UNIT and a sweep unit is one HOST against its whole
        # class: `CL.0` is 3,630 techniques, the oracle writes normal/attack/normal, and `rate_ms`
        # paces per host — 10,890 requests, ~18 minutes at 100ms and ~45 at 250ms. Against the
        # handler's 2-minute default the first beat arrives long after Temporal has killed the
        # attempt, and the run dies with `activity Heartbeat timeout` while the worker is sending
        # probes perfectly. MEASURED: hunt-1789742802, 75 minutes, zero rows, no failures recorded
        # against the Nexus operation at all.
        #
        # `heartbeatFor` floors this at the 2-minute default and caps it at one hour, so a caller
        # cannot weaken liveness below production — it can only tell the truth about how long its
        # own units legitimately take.
        call_opts = {"schedule_to_close_timeout":
                     timedelta(minutes=_num(req, "call_minutes", 90)),
                     "debug_heartbeat_seconds": _num(req, "heartbeat_seconds", 1800)}

        # A host that would not reproduce its own baseline was REFUSED, not scanned, so it is not
        # rescanned. `state()` is the probe rather than a query because it answers for a Dataset
        # that may not exist yet and costs no scan.
        erratic_sql = ""
        if await observations.state() is not None:
            erratic_sql = (f'SELECT DISTINCT host FROM "{observations.name}" '
                           f"WHERE program = '{program}' AND erratic")

        phase = Phase(req=req, program=program, scope=scope, scope_name=scope_name,
                      observations=observations, size=size, lanes=lanes, params=params,
                      call_opts=call_opts, erratic_sql=erratic_sql)

        if req.get("attach"):
            workflow.logger.info(f"attached to whoever is serving desync@{DESYNC[1]} — no Lease held")
            screened, swept = await self._sweep(phase)
        else:
            # `actor=` AND `version=` ARE REQUIRED, and `up` PLACES them itself — it is sugar
            # over `hold` + `place` and costs ONE converge rather than two. This called
            # `fleet.up(tag=…, machines=…)` and then placed separately, which is the older
            # spelling: it raised `TypeError: up() missing 2 required keyword-only arguments`
            # before provisioning anything, so the fleet path of this workflow could not run at
            # all. The `attach` path above was unaffected, which is why it went unnoticed.
            async with fleet.up(actor=DESYNC[0], version=DESYNC[1], tag="desync",
                                machines=_num(req, "machines", 4),
                                sessions=_num(req, "sessions", 4)) as f:
                await f.ready()
                # WHAT THIS RUN IS SPENDING, SAID OUT LOUD, TWICE. A Fleet is the only part of a
                # Run that bills by wall clock, so a Run that never names its rate is one whose
                # cost arrives on an invoice. The rate goes out before the work starts (so it can
                # still be cancelled) and the total after the Fleet is released (so it is real
                # rather than estimated).
                workflow.logger.info(f"{len(f.inventory)} machine(s) polling desync@{DESYNC[1]} — "
                     f"{f.cost_words()}")
                held = workflow.now()
                try:
                    screened, swept = await self._sweep(phase)
                finally:
                    # IN `finally`, because a cancelled or failed Run held the Machines just the
                    # same and is exactly the Run whose cost somebody will ask about.
                    workflow.logger.info("fleet released — "
                         + f.cost_words((workflow.now() - held).total_seconds()))

        promoted = 0
        if not req.get("skip_leads"):
            promoted = await self._promote(phase)
        return {"program": program, "screened": screened, "swept": swept, "promoted": promoted}

    # ------------------------------------------------------------------ the targets

    # The nine columns a `desync.Target` reads. Spelled once: every target query below has to
    # emit exactly these or the actor silently takes its zero value for the ones it misses —
    # which is how `endpoint` came to be empty everywhere without anything raising.
    _TARGET_COLS = ("program, host, port, scheme, host_header, sni, header_block, class")

    # THE PATH THE PROGRAM ITSELF SCOPED, when there is one.
    #
    # This was the literal `'/'`, and that threw away real path data sitting in the scope row. A
    # `url`-kind target is scoped AS A URL — HackerOne's asset for 1Password is
    # `https://events.1password.com/api/`, not the host — so `endpoint` arrives as `/api/` and
    # probing `/` tests something the program did not publish while leaving what it did publish
    # untouched. 799 of the 7,943 paying rows are `url` kind.
    #
    # `nullif` because the column is '' (not NULL) on every row `classify` resolved to a bare
    # host, and `coalesce` alone would hand the actor an empty request-target.
    _SCOPED_ENDPOINT = "coalesce(nullif(endpoint, ''), '/') AS endpoint"

    def _root_targets(self, scope_name, where):
        """Every scoped host at `/`. What this phase did before there were paths, kept as the
        announced fallback rather than deleted — a run without a crawl should still screen."""
        return (f"SELECT {self._TARGET_COLS}, {self._SCOPED_ENDPOINT} "
                f'FROM "{scope_name}" WHERE {where}')

    def _targets(self, program, scope_name, where, paths, per_host):
        """Every scoped host at `/`, PLUS the crawl's best `per_host` paths on that host.

        ROOT IS ALWAYS INCLUDED, and not as a courtesy: it is the one endpoint guaranteed to
        exist, it is what every prior observation was taken at, and dropping it would make this
        campaign incomparable with the last one.

        ── HOW A PATH EARNS ITS SLOT ────────────────────────────────────────────────────────

        `per_host` is small because the cost is multiplicative — a host at 8 paths is 8 baselines
        and 8x the techniques — so the ranking is doing real work, and it ranks by what makes a
        path likely to sit in front of a DISAGREEMENT rather than by what makes it interesting to
        read:

          a live backend   a 2xx path is served by something; a 404 is served by the edge, and
                           the edge is the half of the pair we already have.
          depth            every path segment is another routing decision — a rewrite, a proxy
                           pass, a service mesh hop. `/api/v1/session` crosses more parsers than
                           `/about`, and desync lives exactly where two of them disagree.
          seen often       a path the crawl reached repeatedly from different pages is real
                           routing rather than one dead link.

        The `bounty`/wildcard filtering is already in `where`; this only decides WHERE on an
        authorised host to knock.
        """
        return f"""
            WITH scoped AS (
                SELECT {self._TARGET_COLS}, {self._SCOPED_ENDPOINT}
                FROM "{scope_name}" WHERE {where}
            ),
            ranked AS (
                SELECT host, point_id AS endpoint,
                       row_number() OVER (
                           PARTITION BY host
                           ORDER BY (status_code BETWEEN 200 AND 299) DESC,
                                    length(point_id) - length(replace(point_id, '/', '')) DESC,
                                    seen_count DESC,
                                    point_id
                       ) AS rn
                FROM "{paths}"
                WHERE program = '{program}' AND point_kind = 'path'
                  AND point_id <> '/' AND point_id LIKE '/%'
            )
            SELECT {self._TARGET_COLS}, endpoint FROM scoped
            -- `UNION`, NOT `UNION ALL`. A crawled path can be the same string as the one the
            -- program scoped — `/api/` is exactly the kind of asset that is both — and two
            -- identical Targets are two baselines and two full technique runs against the same
            -- door, landing on one dedup key so the second is not even recorded.
            UNION
            -- QUALIFIED WITH `s.`, because `ranked` also has a `host` and an unqualified list
            -- here is a Binder Error on the one column both sides carry.
            SELECT {', '.join('s.' + c.strip() for c in self._TARGET_COLS.split(','))},
                   r.endpoint
            FROM scoped s JOIN ranked r ON r.host = s.host
            WHERE r.rn <= {per_host}
        """

    # ------------------------------------------------------------------ the scan

    async def _sweep(self, p: Phase):
        # UNPACKED, NOT REWRITTEN THROUGHOUT. The defect #20 names is the SIGNATURE — ten
        # positional arguments threaded through three helpers, where adding a knob meant editing
        # three of them and the order was memorised rather than read. That is fixed above: one
        # parameter, and a new field is one line here.
        #
        # Rewriting 155 lines of a live sweep to `p.program` everywhere would be a much larger
        # diff over working code for a readability gain inside a body that already reads fine, and
        # this workflow is mid-campaign. The names below are READ-ONLY views of a frozen object,
        # so nothing can be mutated back into the bag.
        req, program, scope, scope_name = p.req, p.program, p.scope, p.scope_name
        observations, size, lanes = p.observations, p.size, p.lanes
        params, call_opts, erratic_sql = p.params, p.call_opts, p.erratic_sql
        techniques = catalog.dataset(req.get("techniques") or "techniques")
        if not req.get("skip_corpus"):
            # SHARDED, BECAUSE ONE PAYLOAD CANNOT HOLD THE CORPUS. Dispatching a single
            # `{"emit": "all"}` Unit returned every row in one message: 4,416,752 bytes against
            # Temporal's 4,194,304-byte gRPC limit, so the activity failed ResourceExhausted and
            # retried forever at 0% CPU — indistinguishable from a hang, and the reason
            # `techniques` sat at 7 rows while the corpus was 12,330.
            #
            # 32 shards keeps each message a few hundred KB with room for the corpus to grow.
            # They go one Batch at a time rather than all at once: the shards are the whole
            # corpus, and firing them concurrently just moves the same bytes through the single
            # materializer slot faster than it can take them.
            n = max(1, _num(req, "corpus_shards", 32))
            total = 0
            async with catalog.actor(*DESYNC) as d0:
                for i in range(n):
                    rows, _ = await d0.corpus([{"shard": i, "of": n}], techniques, **call_opts)
                    total += len(rows)
                    # SHARDS ARE SLOW AND THERE ARE 32 OF THEM — measured at ~15s each, so a
                    # corpus publish is eight minutes during which this workflow said nothing at
                    # all. That is the single longest unexplained wait in a run, and it happens
                    # before any probe goes out, so an operator watching a new campaign sees it
                    # first and has no way to tell it from a hang.
                    workflow.logger.info(f"corpus shard {i + 1}/{n}: {len(rows)} technique(s), {total} so far")
            workflow.logger.info(f"corpus published: {total} technique(s) in {n} shard(s) "
                 f"at desync@{DESYNC[1]}")
        else:
            workflow.logger.info("corpus publish skipped — the scan reads techniques from memory")

        # ── PHASE A · screen every host with the tier-1 set ────────────────────────
        screened = 0
        if req.get("skip_screen"):
            workflow.logger.warning("screen skipped — the framing axis is not being re-run", extra={"incomplete": True, "axis": "framing", "phase": "screen"})
        else:
            # A WILDCARD IS NOT A HOST. `scope_<program>` carries the authorisation list, and
            # that includes `*.wavecell.com` (subfinder's input) and `vcc-*.8x8.com` (a glob that
            # expands to nothing anybody published). Both have an empty `seed` and neither is
            # probeable; without this filter they arrive as Targets and burn a baseline each.
            where = (f"program = '{program}' AND kind NOT IN ('wildcard', 'unexpandable')" +
                     (" AND bounty" if req.get("bounty_only") else "") +
                     (f" AND host NOT IN ({erratic_sql})" if erratic_sql else ""))

            # ── WHERE THE PATHS COME FROM ────────────────────────────────────────────────
            #
            # `scope_<program>.endpoint` IS BLANK ON EVERY ROW and always has been — bbscope
            # emits authorisation, not paths — so this phase used to hand the actor a Target
            # with no endpoint, `orDefault(h.Endpoint, "/")` chose root, and the sweep query
            # below then said `'/' AS endpoint` in as many words. The whole smuggling axis was
            # a root scanner that nothing in its own output could reveal as one.
            #
            # The paths are the CRAWL's, from `surface`. A path invented from a wordlist is a
            # path that 404s at the edge and never reaches the backend whose disagreement this
            # axis is looking for; a path the crawl actually reached routes somewhere real.
            #
            # THE FALLBACK IS ANNOUNCED, NOT SILENT. A hunt run before its crawl still screens
            # every host at root — that is useful and it is what used to happen — but it must
            # not read afterwards as though the paths were covered and found clean.
            paths = req.get("paths_from") or "injection_points"
            per_host = _num(req, "paths_per_host", 5)
            pages = []
            try:
                async for batch in scope.batches(
                        size, order_by="host, endpoint",
                        query=self._targets(program, scope_name, where, paths, per_host)):
                    pages.append(batch)
                workflow.logger.info(f"targets: root + up to {per_host} crawled path(s) per host, from {paths}")
            except Exception as exc:  # noqa: BLE001 - the reason is the announcement
                pages = []
                workflow.logger.warning(f"no usable path inventory ({paths}: {exc!r}) — screening ROOT ONLY. "
                        f"Every non-root path in {program} is UNSCANNED, not clean. Run the "
                        f"`surface` workflow to build it.",
                        extra={"incomplete": True, "axis": "endpoints", "phase": "screen", "paths_from": paths})
                async for batch in scope.batches(size, order_by="host",
                                                 query=self._root_targets(scope_name, where)):
                    pages.append(batch)
            workflow.logger.info(f"screening {len(pages)} page(s) across {lanes} lane(s)")

            # PROGRESS IS REPORTED PER PAGE, NOT PER PHASE.
            #
            # This loop used to emit one line before it started and one after it finished, and
            # between them a screen over thousands of targets said nothing for as long as it
            # took. A run that is working and a run that is wedged produce the same output —
            # which is precisely the reading `surface-1789866474` got wrong for eleven minutes.
            #
            # `note` per page is cheap (it is a workflow log line, not an activity) and it gives
            # the one number that matters while waiting: how far through, and how much it has
            # found so far.
            async def lane(i):
                n = 0
                mine = pages[i::lanes]
                async with catalog.actor(*DESYNC) as d:
                    for j, page in enumerate(mine, 1):
                        obs, dropped = await d.smuggle(page, observations,
                                               params={**params, "phase": "screen"}, **call_opts)
                        await _log_drops(dropped, phase="screen")
                        n += len(obs)
                        workflow.logger.info(f"screen lane {i + 1}/{lanes}: page {j}/{len(mine)} "
                             f"(+{len(obs)} observation(s), {n} this lane)")
                return n

            screened = sum(await asyncio.gather(*(lane(i) for i in range(lanes))))
            workflow.logger.info(f"screen complete: {screened} observation(s)")

        # ── PHASE B · sweep only what moved, only in the class it moved on ─────────
        swept = 0
        if not req.get("skip_sweep"):
            reactive = f"""
                -- `header_block` IS CARRIED, NOT BLANKED. It is the researcher marker both
                -- programs require on every request, and it arrives on the scope row. Phase A
                -- reads scope directly so it gets it for free; this phase reads OBSERVATIONS,
                -- which is why the column had to be re-selected — and why spelling it '' here
                -- would have made the sweep, the noisiest phase, the one that went out
                -- unattributed.
                --
                -- `o.endpoint`, NOT `'/'`. This line used to read `'/' AS endpoint` — a literal,
                -- which meant the sweep re-probed ROOT no matter which path had reacted in the
                -- screen. Combined with `scope_*.endpoint` being blank on every row (bbscope
                -- emits authorisation, not paths), it made the entire smuggling axis a root-only
                -- scanner: 40 hosts, 833 observations, 225 leads, every byte sent to `/`.
                --
                -- It is in the GROUP BY for the same reason it is in the actor's dedup key: what
                -- reacted is a (host, ENDPOINT, class) triple. Two paths on one host routinely
                -- sit behind different backends, and that disagreement is frequently the whole
                -- reason one desyncs and the other does not — grouping them together sweeps one
                -- and silently reports it as both.
                SELECT o.program, o.host, o.port, o.scheme, o.class,
                       '' AS host_header, '' AS sni, o.endpoint AS endpoint,
                       coalesce(max(s.header_block), '') AS header_block
                FROM "{observations.name}" o
                LEFT JOIN "{scope_name}" s
                       ON s.program = o.program AND s.host = o.host
                WHERE o.program = '{program}' AND o.phase = 'screen'
                  AND o.signal_count > 0 AND NOT o.erratic
                GROUP BY o.program, o.host, o.port, o.scheme, o.class, o.endpoint
            """
            pages_done = 0
            async with catalog.actor(*DESYNC) as d:
                async for pair in observations.batches(size, order_by="host", query=reactive):
                    obs, dropped = await d.smuggle(pair, observations,
                                           params={**params, "phase": "sweep"}, **call_opts)
                    await _log_drops(dropped, phase="sweep")
                    swept += len(obs)
                    pages_done += 1
                    # THE SWEEP IS THE LONGEST PHASE IN THE SYSTEM — one reacting host is 3,625
                    # techniques — so it is the one that most needs to say where it is. The page
                    # count is not known in advance here (the query streams), so this reports
                    # what it has done rather than a fraction.
                    workflow.logger.info(f"sweep: page {pages_done}, +{len(obs)} observation(s), {swept} total")
            workflow.logger.info(f"sweep complete: {swept} observation(s)")

        # ── PHASE C · the FOLD axis ───────────────────────────────────────────────
        if not req.get("skip_split"):
            swept += await self._split(p)
        return screened, swept

    async def _split(self, p: Phase):
        req, program, observations = p.req, p.program, p.observations
        size, params, call_opts = p.size, p.params, p.call_opts
        exchanges = catalog.dataset(f"exchanges_{program}")

        # `state()` IS A LIFECYCLE PROBE, NOT AN EXISTENCE PROBE. It reads one object,
        # `datasets/<name>/_state.json`, so it says whether some Run declared this Dataset
        # complete — nothing about rows. A Dataset loaded outside a Run has every row and no state
        # object, and reading None as "no such dataset" silently skipped this entire axis for the
        # whole first campaign. It informs; it does not gate.
        state = await exchanges.state()
        if state is None:
            workflow.logger.warning(f"exchanges_{program} has no lifecycle record — written outside a Run. "
                    f"Paging it anyway; that is not the same as empty.", extra={"incomplete": True, "axis": "exchanges", "phase": "sweep"})
        elif state == "open":
            workflow.logger.warning(f"exchanges_{program} is still open — this reads a partial crawl as whole",
                    extra={"incomplete": True, "axis": "exchanges", "phase": "sweep"})

        where = f"program = '{program}'"
        if req.get("only"):
            pat = str(req["only"]).replace("'", "''")   # SQL-quoted: this reaches a WHERE clause
            where += f" AND url ILIKE '{pat}'"

        pages = []
        async for batch in exchanges.batches(size, order_by="url", where=where):
            pages.append(batch)

        chunk = max(1, _num(req, "chunk", 16))
        workflow.logger.info(f"splitting axis: {len(pages)} page(s), a fresh Session every {chunk}")

        # A SESSION PER CHUNK, and the BATCH as the unit of failure. Both were learned the same
        # way: one Session held across a whole crawl pins the axis to ONE Machine and dies partway,
        # and a try/except around the whole loop turned that into "the axis found nothing" — a
        # partial scan reported as a complete one, which is the failure this engine's whole
        # erratic/voided/clean discipline exists to prevent.
        folded, lost, streak = 0, 0, 0
        for start in range(0, len(pages), chunk):
            if streak >= 3:
                break
            async with catalog.actor(*DESYNC) as d:
                for batch in pages[start:start + chunk]:
                    try:
                        obs, dropped = await d.split(batch, observations,
                                              params={**params, "phase": "split"}, **call_opts)
                        await _log_drops(dropped, phase="split")
                    except asyncio.CancelledError:
                        raise   # an operator's cancel is not a phase that quietly completed
                    except Exception as exc:  # noqa: BLE001 - the reason is the payload
                        lost += 1
                        streak += 1
                        workflow.logger.warning(f"split batch voided ({lost} so far): {exc!r}", extra={"incomplete": True, "axis": "url-order", "phase": "split", "voided": lost})
                        if streak >= 3:
                            workflow.logger.warning(f"splitting axis ABANDONED after {folded} observation(s): "
                                    f"{streak} consecutive failures. Everything past this "
                                    f"point in url order is UNSCANNED, not clean.",
                                extra={"incomplete": True, "axis": "url-order", "phase": "split", "scanned": folded})
                            break
                        continue
                    streak = 0
                    folded += len(obs)

        if lost:
            workflow.logger.warning(f"splitting axis complete with gaps: {folded} observation(s), "
                    f"{lost} batch(es) voided — that ground was not covered.",
                        extra={"incomplete": True, "axis": "url-order", "phase": "split", "voided": lost, "scanned": folded})
        else:
            workflow.logger.info(f"splitting axis complete: {folded} observation(s)")
        return folded

    # ------------------------------------------------------------------ the leads

    async def _promote(self, p: Phase):
        req, program, observations = p.req, p.program, p.observations
        """Promote signalling observations into `desync_leads`, THIS workflow's output.

        THE TWO REQUESTS ARE TWO COLUMNS. A desync lead is a claim about a difference between what
        a normal client sends and what triggered the signal, so both are on the row, rendered as
        resendable HTTP rather than base64 — a PoC nobody can read off the row is a PoC nobody
        reproduces. `control_request` is the third, because on the splitting axis the matched control is
        what turns "this looks odd" into "this is the injection".

        `is_proof` is computed here rather than left to every reader. Two signals are PROOF —
        `authority_injected` and `canary_reflected` — because each means a value this scanner
        minted came back where no innocent path puts it. Everything else is evidence with innocent
        explanations left, and a triage queue that cannot tell them apart is a triage queue that
        reads 400s all day.
        """
        leads = catalog.dataset(req.get("leads") or LEADS)
        source = observations.name

        # `insert_from` is NOT idempotent (catalog.py:1752): re-promoting with a wider WHERE
        # re-appends every lead already there. The anti-join is doing real work.
        # THE SPLITTING AXIS OWNS THREE OF THESE COLUMNS, and a smuggle-only Run never writes them.
        # `point` is where the payload went in and `control_request`/`_b64` are the matched control
        # — all three are produced by phase C. Selecting them unconditionally made `_promote` fail
        # with
        #
        #     Binder Error: Values list "o" does not have a column named "point"
        #
        # on every Run started with `skip_split`, which is every Run in a smuggle-only campaign. The
        # observations were written, the signals were found, and the promote that turns them into
        # leads could not run — so the whole point of the Run was lost at the last step.
        #
        # NULL RATHER THAN OMITTED, because `desync_leads` has one shape whichever axis produced the
        # row, and a reader diffing two leads should see an empty control, not a missing field.
        split_ran = not req.get("skip_split")
        point = "o.point" if split_ran else "CAST(NULL AS VARCHAR) AS point"
        control = "o.control_request" if split_ran else "CAST(NULL AS VARCHAR) AS control_request"
        control_b64 = ("o.control_request_b64" if split_ran
                       else "CAST(NULL AS VARCHAR) AS control_request_b64")

        promote = f"""
            SELECT
                o.ts, o.program, o.host, o.port, o.scheme, o.axis, o.class,
                -- WHICH PATH. A lead that names only a host is a lead a triager has to re-derive
                -- the target of by decoding `triggered_request_b64` — and for the whole first
                -- campaign the answer was always `/`, so nobody noticed it was missing.
                o.endpoint,
                o.technique, o.gadget, {point}, o.tier,
                -- THE THREE REQUESTS, EACH IN ITS OWN FIELD, EACH TWICE.
                -- The plain columns are for reading in the grid — they escape a bare LF rather
                -- than normalising it, so the bug stays visible but the text is NOT resendable.
                -- The _b64 columns are the wire bytes: `base64 -d` and paste into Caido's replay.
                -- A desync payload is made of exactly the bytes a clipboard destroys, so the
                -- readable form would arrive with its bug normalised out and reproduce nothing.
                o.original_request,      o.original_request_b64,
                o.triggered_request,     o.triggered_request_b64,
                {control}, {control_b64},
                o.canary,
                o.signals, o.signal_count,
                -- PROOF NOW REQUIRES THE CONTROL, and that is the single most important change
                -- in this file.
                --
                -- It used to be `authority_injected OR canary_reflected` — a canary this scanner
                -- minted came back, so there is no innocent path for those bytes. The reasoning
                -- was right about the BYTES and wrong about the CLAIM: the bytes came back
                -- because the server read the smuggled body as a request, and the claim was that
                -- the GADGET made it. A server that pipelines — reads any body as the next
                -- request, Content-Length or not — reflects the canary for a reason that has
                -- nothing to do with the obfuscation and is not a vulnerability.
                --
                -- That is not hypothetical. It is what these 40 `is_proof` rows turned out to be
                -- on 2026-09-18, and both reports built on them were retracted: on
                -- paypalobjects.com a plain `Content-Length: 0` fired 3 times in 6 while the
                -- obfuscated attacks fired 1 and 2.
                --
                -- `control_survived` means the actor re-ran the identical request with a
                -- well-formed Content-Length and the behaviour did NOT reproduce. Requiring it
                -- means `is_proof` can no longer be true of a row nobody controlled — a row from
                -- desync@1.0.0, or one whose control connection failed — which is exactly the
                -- population that produced the retraction.
                (list_contains(o.signals, 'authority_injected')
                    OR list_contains(o.signals, 'canary_reflected'))
                  AND list_contains(o.signals, 'control_survived')     AS is_proof,
                o.run_id, o.node,
                -- WHICH ORACLE MADE THIS CLAIM. `observations.version` is the ACTOR version, and
                -- after 1.2.0 that is not a provenance detail — it is the difference between a
                -- lead whose `is_proof` was tested against a matched control and one whose was
                -- not. The 225 leads already in this table came from 1.0.0 and were retracted;
                -- without this column a reader cannot tell them from the ones that were not.
                o.version AS actor_version,
                'new'  AS status,
                ''     AS assignee,
                ''     AS notes
            FROM "{source}" o
            WHERE o.program = '{program}'
              AND NOT o.erratic
              AND NOT o.voided
              AND o.signal_count > 0
        """
        n = await leads.insert_from(observations, query=promote)
        workflow.logger.info(f"{n} lead(s) promoted into {leads.name}")
        return n


if __name__ == "__main__":
    catalog.serve([Hunt])
