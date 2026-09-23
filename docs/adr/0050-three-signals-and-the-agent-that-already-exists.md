# 50. Three signals, and the agent that already exists

Date: 2026-09-17

## Status

Proposed.

Supersedes nothing yet. It puts a dated end on the read path of
[ADR 0020](0020-dashboard-read-only-terminals-screen-attach.md): Terminals stay until the run page
can answer what a Terminal answers, and are removed from the commercial build.

## Context

An operator asked for a logs section on the workflow run page, and asked whether Temporal's
`workflow.logger` is a better primitive than kontra's `speak`. Answering it turned up a larger
fact.

**There is no observability backend at all.** `machine.ts` installs `vmagent` on every **Machine**,
writes its unit, its scrape config and its remote-write target — and nothing listens. There is no
VictoriaMetrics container, nothing in `docker-compose.yml`, nothing on `:8428`. Measured on
`kf-webcrawl-01` during the 8x8 campaign, every 60 seconds, for the life of the **Fleet**:

    couldn't send a block with size 575 bytes to "1:secret-url":
      Post "http://10.124.0.2:8428/api/v1/write": dial tcp4 10.124.0.2:8428: connect: connection refused

So the metrics half is a shipped agent with nowhere to send, and the logs half does not exist. The
only way to see inside a running **Machine** is a **Terminal** — `tmux attach -r` over SSH,
constructed by the infra worker, which therefore holds a key that reaches every **Machine**.

That is survivable for one operator on their own boxes. It is not a product:

- **A tenant cannot be given a shell on infrastructure**, and the read path's enforcement is that
  the SSH command line is built server-side. The boundary is a string.
- **A Terminal is a pixel stream.** It cannot be searched, filtered by **Run**, or read after the
  fact.
- **Scrollback dies with the Machine.** A **Fleet** Machine is destroyed when the last **Lease**
  drops. On 2026-09-17 the only evidence that a Worker was grinding an abandoned sweep — still
  sending traffic at a third party for a **Run** that had already failed — was
  `journalctl -u kontra-actor-desync` over SSH to `kf-desync-01`. That Machine no longer exists and
  that evidence is unrecoverable. The same Run's `speak` sentences are still readable.

## Decision

**1. Three signals, three homes, and they are not interchangeable.**

| signal | question it answers | where it lives | who writes it |
|---|---|---|---|
| **history** (structural) | what did this **Run** DO — which activities, timers, children, in what order? | Temporal, by construction | the engine |
| **logs** (lines) | what did the process actually do, and why did it die? | VictoriaLogs on the Controller | the author |
| **metrics** (series) | how loaded is this **Machine**? | VictoriaMetrics on the Controller | the agent already installed |

Conflating them is the mistake this ADR exists to prevent, and §2 is where it was nearly made:
history is the engine's own record and costs nothing extra, so putting AUTHORED sentences there
made them expensive (~5 events and ~1s each) and capped (200 per Run) for no property a log line
lacks. A logs pipeline that charged history per line would hit the continue-as-new wall the cursor
work already fought. **Logs are not narration at higher volume; narration was logs in the wrong
place.**

**2. `speak` is REMOVED, and `workflow.logger` replaces it — with one condition.**

*Amended 2026-09-17, before acceptance.* This section first kept both, on the argument that
history survives without a second service. That argument loses to a better one.

Measured across `hunt` and `surface`, 26 narrated sentences: roughly two thirds are progress
(`crawl complete: N http event(s)`, `screening N page(s)`, `fleet released`) which nobody would
defend as history events. The remaining third are a different thing — the workflow stating that its
own result is INCOMPLETE:

    seed_limit 200 reached — the crawl is PARTIAL by request
    exchanges_8x8 has no lifecycle record — written outside a Run … not the same as empty
    exchanges_8x8 is still open — this reads a partial crawl as whole
    splitting axis ABANDONED … everything past this point in url order is UNSCANNED, not clean

As narration those are buried in one Run's history, findable only by someone already reading that
Run. As log records they are **queryable and alertable across every Run** — "show me every Run last
month that abandoned an axis or read a partial crawl as whole" is impossible today and is one
LogsQL filter once logs exist. That is a monitoring capability, not a debugging convenience, and it
is worth more than the redundancy `speak` was being kept for.

**THE CONDITION, AND IT IS THE WHOLE RISK.** A completeness claim that becomes `log.info` has gone
from *in the run record* to *one line among thousands*, which is strictly worse than today. So the
replacement is not "call the logger instead":

- a claim about the RESULT's completeness is emitted at **WARN or above**, and carries a structured
  field (`incomplete=true`, with the axis or phase it is about), so it is findable without knowing
  which Run to look at;
- progress is INFO and nobody has to think about it;
- the authoring rule that replaces the old "narrate a phase, never a Unit" is **"log freely; raise
  the level when the RESULT is not what a reader would assume."**

What is lost, stated plainly: the Run record stops being self-describing without VictoriaLogs, and
Temporal's own UI stops showing labelled phases. Both are real. Neither is product-critical once
the console's run page is the surface of record, and the second was only ever a developer
convenience.

`workflow.logger` carries the workflow context (`workflow_id`, `run_id`) as metadata and is
suppressed during replay, so the labels needed for correlation come for free — and the sandbox
forbids I/O, not logging: the host's stdout becomes a journal entry and step 3 ships journals.

**2a. What `speak`'s removal takes with it.**

The 200-sentence cap (`MAX_SENTENCES`), the narration budget, the refusal turn that exists to say
the account is truncated, and the ~5 history events plus ~1s per sentence all go with it. On a
`hunt` run that is 17 sentences — 85 events and about 17 seconds of pure narration latency — spent
to say things a log line says for nothing.

**The run page does not empty out.** `readRunTurns` reduces the whole reduced log into nine arms;
narration is one of them. Activity, timer and child-workflow turns still build the spine, and the
authored sentences move underneath it as correlated log lines.

**3. Logs leave a Machine the way metrics already do — `vlagent` beside `vmagent`.**

`machine.ts` already encodes the constraint in a comment: *"Workers accept NO inbound connections,
so nothing can scrape them from the Controller — the agent has to scrape 127.0.0.1 and push
outward."* Logs have the identical shape, and VictoriaMetrics ships the identical agent for them.

`vlagent` reads **journald natively**, plus files by glob and OTLP over HTTP, and **buffers to disk
when the remote is unreachable** (`-remoteWrite.tmpDataPath`, bounded by
`-remoteWrite.maxDiskUsagePerURL`). That buffer is the point, not a detail: the Machine whose last
words matter most is the one that is failing or about to be destroyed.

VictoriaLogs over Loki on measurement, not taste — a published 500 GB/7-day comparison reports ~94%
lower query latency, ~40% less storage and under half the CPU and RAM. It also ingests journald,
syslog, OTLP, Loki and Elasticsearch protocols, so choosing it does not close a door.

**4. The emit format is OpenTelemetry, so the store stays the operator's choice.**

Actors and hosts emit OTLP; `vlagent` and VictoriaLogs both speak it. kontra ships VictoriaLogs as
the default backend, and an operator who already runs Datadog, Sentry or SigNoz re-points one
endpoint. This is what makes the pillar commercial rather than a house style.

For Temporal specifically, `OpenTelemetryPlugin` (`temporalio.contrib.opentelemetry`) propagates
trace context across Client, Workflow and Activity boundaries with a **replay-safe** tracer
provider, so a `run_id` on a log line and a span in a trace are the same identity.

**5. Sentry is not the log store here, and the reason is the appliance.**

Sentry Logs is good at the thing it is for — structured logs joined to exceptions by trace id,
with grouping and release health. But self-hosting it requires Postgres, Redis, **Kafka and
ClickHouse**. kontra's install is `curl | docker compose up`; adding a Kafka and a ClickHouse to
show a log line is not a trade worth making. The SaaS alternative meters per event, against a
product whose unit of work is millions of probes.

Because step 4 emits OTLP, an operator who wants Sentry points it there. That is the right shape:
kontra does not depend on it, and does not prevent it.

**6. VictoriaLogs is never exposed; the Controller proxies it.**

VictoriaLogs has **no authentication** — its multi-tenancy is an `AccountID`/`ProjectID` request
header, which is an authorisation decision made by whoever sets the header. So the console never
talks to it. The orchestrator, which already gates every route behind a session
([ADR 0045](0045-the-console-signs-in.md)), proxies `/select/logsql/query`, `/tail` and `/hits`
and **stamps the tenant header itself**. A tenant choosing their own AccountID would be a
cross-tenant read.

**7. Terminals stay until the run page reaches parity, then go.**

A **Terminal** still answers one question nothing else will for a while: what is this Machine doing
*right now*, including things no kontra process wrote. It is kept, and it is dated: when the run
page can answer what a Terminal answers, ADR 0020's read path is superseded and `KONTRA_SSH_KEY`
leaves it. The commercial build ships without tmux.

## Consequences

**Two containers join the appliance** — VictoriaMetrics and VictoriaLogs — and one agent joins each
Machine. Both are single Go binaries with no external dependencies, which is why they are
affordable here and Sentry's stack is not.

**The vmagent that has been failing silently since it was written starts working**, which is a
behaviour change disguised as a bug fix: fleet CPU and memory become visible during a campaign for
the first time.

**A log line is only as useful as its `run_id`.** The actor host already holds `run_id`, `node_id`
and `actor_id` and already POSTs them (`engine.py:144` → `/api/runs/:id/progress`), so the labels
exist; what is new is stamping them onto records rather than only onto progress.

**Retention becomes two policies, not one.** Datasets are swept by the retention schedule; logs are
bounded by VictoriaLogs' own retention. They will disagree, and the run page must not imply a log
line exists for every Run it can list.

**Every `speak` call site has to be re-levelled, not mechanically replaced.** A `sed` from
`await speak(...)` to `logger.info(...)` would silently demote every completeness claim in the
codebase to noise — the exact failure this ADR's condition exists to prevent. The migration is
per-call-site and the question at each one is "would a reader be wrong about the result if they
missed this?"

## What this does not decide

Whether the console should render metrics at all, or leave them to Grafana pointed at the same
VictoriaMetrics. Whether logs should also land in the lake as a **Dataset** so they can be JOINed
against scan output — attractive, and a second ingest path with a second retention policy, so it
needs its own case. And whether a tenant's logs should be a separate VictoriaLogs tenant or a
separate instance, which is a question about the isolation boundary, not about logging.
