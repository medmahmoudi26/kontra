# Query Surface — datasets

A **dataset** is anything queryable, and it is the currency in **both directions**: the lists you load *in*, and what an actor produced *out*. One noun covers both — `kontra dataset`.

Queries run on the **orchestrator's** already-attached connection by default (CLI and web workbench alike), which is what makes them ~50 ms instead of ~1.3 s. That is a change from "controller query RAM is zero": the controller now executes operator SQL, bounded by a 192 MB `memory_limit` it may not spill past and a read-only, filesystem-less sandbox (§3a). `--local` puts execution back on your workstation, and `kontra explore` never left it.

```sh
kontra dataset list                                             # standalone (loaded) + output (produced)
kontra dataset query bbscope --sql "SELECT host FROM bbscope LIMIT 20"
kontra dataset tag  wf-nscheck-0.1.0--2026-08-19T14-32-07Z--2faa3d --add keep
```

Two questions this page keeps apart, because different places answer them:

- **Did the run finish?** — execution status, authority Temporal.
- **Is the output queryable?** — materialization status, authority an application-owned ledger.

## 1. Two roots on the object store

`DATA_PATH` is the **bucket root**, so DuckLake's own `<schema>/<table>/` layout *is* the object layout: the top-level directory is the schema name.

| On the store | DuckLake schema | What it is |
|---|---|---|
| `standalone/<name>/…parquet` | `standalone` | a list **you** loaded — scope, seeds, targets. An **INPUT**. |
| `output/<actor>/version=<v>/dt=<YYYY-MM-DDTHH-MM-SS>/…parquet` | `output` | what an actor **produced**. A **RESULT**. |

Nothing derives a path by string-building, and there is no `datasets/` middle segment whose only job was to hold hashed table names (`control/orchestrator/src/data/parquet.ts`).

Two more prefixes share the bucket and are **not** datasets — they are the internal data plane, and no query surface addresses them: `cas/` (the claim-check store, reclaimed by mark-and-sweep only) and `units/` (the raw per-unit blobs the materializer decodes into the tables above, aged out by the 90-day sweep). See [[Data-Plane]] §1–2.

## 2. Identity — actor + version + dt, never a UUID

**One table per actor, named after the actor**, in schema `output`, partitioned by `(version, dt)`.

- `dt` is the **dispatch time to the second** (`2026-08-03T17-50-50`), taken from the server-minted `run_started_at` — not from when materialization happened, which drifts whenever decoding lags. Hour precision was not enough: two dispatches of one actor in an hour is the normal case.
- A dispatch sharded across `n1..nK` is **many graph nodes and ONE dataset**. Node ids stay visible for diagnosis; they never name anything.
- Gone with the old layout: `ds_<sha1(actor,version,node)>` table names, the `_kontra_dataset` registry that existed only to translate them back, and per-node views. Actor names are contract-validated identifiers, so the name itself is a safe *and legible* table name.

Identity also travels **in the rows**, so a bare parquet file still says where it came from:

| Column | Type | Meaning |
|---|---|---|
| `version` | `VARCHAR` | partition column — the actor version |
| `dt` | `VARCHAR` | partition column — the dispatch, to the second |
| `node` | `VARCHAR` | the graph node (shard) that produced the row |
| `run_id` | `VARCHAR` | the run |
| `run_started_at` | `TIMESTAMPTZ` | server-minted once per run, preserved across retries |

…plus the actor's own output object, typed — nested output stays nested as `STRUCT`/`LIST`. Schema is inferred per batch and the table **evolves**: a batch carrying a field the table lacks gets an explicit `ADD COLUMN` before the insert (`INSERT … BY NAME` tolerates reordering and absence but rejects unknown columns), so heterogeneous actor output neither loses a field nor fails the node.

**No backfill.** `materialization_schema_version` still keys the ledger record (`(run_id, actor, version, node, schema_version)`) but no longer names a table. Pointer-layout output written by the pre-ADR-0017 writer — two columns, `run_id` + `$ref`, where `SELECT host` could not be written at all — lives in hashed tables under the old `datasets/` root and is not listed or resolved by anything on this page. Re-dispatch if you need typed output for an old run.

## 2a. A Dataset's NAME, and how long it lives

[ADR 0029](../adr/0029-live-datasets-name-tag-retention.md). The table identity above is what SQL
addresses; this is what an operator reads on a listing row, and it is **run-grain** — one **Run**
writing two actor tables has one name, not two.

```
wf-<workflow>-<version>--<dtPartition(runStartedAt)>Z--<runFragment(runId)>
wf-nscheck-0.1.0--2026-08-19T14-32-07Z--2faa3d
```

- **Derived, never stored.** One function renders it and every surface — the API, the Datasets page,
  `kontra dataset list` — calls that one function. There is no name→path registry; the deleted
  `_kontra_dataset` table is not coming back.
- **From the caller's manifest, not the Actor's.** A materialization record carries the *Actor*'s
  name, so a workflow `foo` dispatching an actor `bar` would otherwise be named `wf-bar-…`. Both
  start paths snapshot the caller's `name`/`version` against the `runId` at start (ADR 0025's
  pattern). Datasets that predate that store fall back to the Actor — drop every row and every
  Dataset still lists and still renders a name.
- **The trailing fragment is a digest of the run id, not its prefix.** Run ids are
  `<type>-<unixseconds>`, so `runId[:6]` carried no run entropy at all — every nscheck run rendered
  `--nschec`. The full `runId` is on the row beside the name, and is the key `tag`/`rename` address.
- **UTC**, via `dtPartition` — two controllers exist, and a local-time name would denote two
  different instants depending on which box wrote it. The UI renders local.

### Lifetime

| Dataset | How long it lives |
|---|---|
| **standalone** (operator-loaded) | forever — operator-owned, `kontra dataset delete <name>` |
| **output, tagged** | kept |
| **output, untagged** | expires on a TTL **this repo owns** (default 24h), clocked from **last write** |
| **temporary** | until its owning Run deletes it explicitly — never swept, whatever its age or tag |

The TTL runs from last write rather than creation, because a run that appends for thirty hours would
otherwise have its earliest chunks collected while it is still writing to them. Collection is a
**sweep with a grace window**, not a delete at exactly T+TTL: tagging is frequently retroactive — an
operator tags at hour 23 because that is when the run turned out to be interesting — so the sweep
must not race the tag. The sweeper reads the **Dataset record**, never the Temporal search
attribute; `KontraTag` is a one-way projection of the record, not a second authority.

A tag on a **temporary** Dataset does not extend its life (a temp has no clock), does not make it
durable, and does not block the explicit delete verb. What it *does* keep is the same Run's
**durable** output — the record is Run-grain — which is the sensible reading of "keep this": the
staging table goes when triage is done, the answer stays. This is why the delete confirmation names
the tags.

> **Wired at boot; it previews until you say otherwise.** `orchestrator-infra` registers the hourly
> Schedule (`kontra-dataset-retention`) on every start, idempotently — a restart does not duplicate
> it or undo a `kontra schedule pause`. Until `KONTRA_RETENTION_COLLECT=1` is set on the worker
> holding the lake, every firing is a **dry run**: it reports and deletes nothing. Steer it with
> `kontra schedule`, and read what it would collect at `GET /api/datasets/retention/preview`.
>
> The Datasets listing shows the reading per row — an `expires` column beside `tags`, saying `would
> be collected`, `would be collected in 7h`, or which safeguard keeps it — so retention is visible
> before it bites. Retention is decided per **Run**, including for a partition several Runs wrote:
> each contributor is weighed against its own record, so one Run's tag keeps that Run's rows and
> nobody else's.

## 3. `kontra dataset` — the one noun

```sh
kontra dataset list
kontra dataset query <name> [--sql "…"] [--version V] [--dt PREFIX] [--export FILE] [--local]
kontra dataset create <file.csv|.jsonl|.parquet> <name>    # replace wholesale
kontra dataset anew   <file.csv|.jsonl|.parquet> <name>    # append only rows not already present
kontra dataset delete <name>                               # standalone only (see below)
kontra dataset tag    <name> --add TAG [--remove TAG]      # KEEP this output past its TTL
kontra dataset rename <name> (--to NEWNAME | --reset)      # override the derived run-grain name
```

`tag` and `rename` address a **Dataset record**, which is keyed by **runId** — so where several
dispatches share a name, narrow with `--version` / `--dt`, or skip name resolution entirely with
`--run <runId>`. A tag is a **set** (`--add`/`--remove`, repeatable); a rename is a single
authoritative choice, so `--to` replaces and `--reset` restores the derived name.

**These two answer to `KONTRA_RUN_TOKEN`**, the same credential as `serve`/`start`/`cancel` — they
are writes about a **Run**, keyed by its id. That is not bookkeeping: because a tag is what the
retention sweep reads to decide keep-or-collect, *removing* one is the delete button for a Dataset,
one sweep tick later. Like the rest of the Run surface the routes are **open when no token is set**
(`kontra doctor` and the page both say so); setting the token closes them.

`ds` is short for `dataset`; `db` and `explore` stay as aliases so muscle memory keeps working (`kontra db list` shows the standalone side only).

**Nothing needs to be exported first.** The catalog DSN is discovered from the checkout (`docker-compose.yml` + `.env`) or, from any other directory, from the running orchestrator container; the bearer token resolves the same way. `--catalog` still overrides.

**`query` runs on the orchestrator by default**, because it already holds an open connection. Locally, every invocation pays the same fixed cost *twice* — ~410 ms to `INSTALL`/`LOAD` three extensions plus ~95 ms to `ATTACH` the catalog, once to resolve which schema the name lives in and once to run the query — about **1.3 s before any work happens**. Over the API the same query is **~50 ms wall clock**, and the semantics are identical because it is the same name resolution on an already-open connection.

`--local` runs DuckDB here instead. Use it when the stack is down, when you passed `--catalog`/`--data-path` for a lake that is not this stack's (both imply `--local`), or when a query legitimately needs more memory than the server-side sandbox allows. An unreachable orchestrator falls back automatically; a *rejection* from the orchestrator does not — silently re-running locally after a 401 would turn a refusal into a result. `--local` needs `duckdb` on `PATH` (or `~/.duckdb/cli/latest/duckdb`).

- **`list`** — one table, both kinds: `kind, name, version, dt`. Catalog metadata only; no data file is opened.
- **`query <name>`** — resolves the **bare name** (standalone first, then output) to a DuckDB view of the same name, so your SQL references it as a plain table. A name that matches nothing is an **error**, never an empty result — a typo must not read as "found nothing".
- **`anew`** is `anew(1)` semantics: insert only rows absent from the table and report how many were new, so re-running a scope export is idempotent and tells you what changed.
- **`create` / `anew` / `delete`** work on the **standalone** side only — they write and drop `lake.standalone.<name>`. Actor output is not deleted from here; **untagged** output expires on a TTL kontra owns ([[Data-Plane]] — Retention), and a **temporary** Dataset is deleted explicitly by its owning Run. Watch the asymmetry: `delete` on an output name drops nothing and still prints `deleted dataset …`.
- **`tag` / `rename`** work on the **output** side only — they mutate the durable Dataset *record*, never the data files. A tag is what stops the retention sweep collecting a Dataset; see below.
- **`--version` / `--dt`** fold into the view's `WHERE`. Because those *are* the partition columns, they **prune partitions** instead of filtering rows.

`--dt` is a **prefix**:

| `--dt` | Selects |
|---|---|
| `2026-08-03` | that whole day |
| `2026-08-03T17` | that hour |
| `2026-08-03T17-50-50` | exactly one dispatch |

`--version`/`--dt` apply to **output** datasets only. A standalone list has neither column, so using them there is refused with a message that says so, rather than erroring deep inside DuckDB.

```sh
kontra dataset query crawl4ai --dt 2026-08-03T17 \
  --sql "SELECT status_code, count(*) FROM crawl4ai GROUP BY 1 ORDER BY 2 DESC"

# --export writes the FULL result — the row cap applies to the screen, not the file.
# The format comes from the extension: .csv, .parquet, .json (array), .jsonl/.ndjson.
kontra dataset query crawl4ai --dt 2026-08-03 --export findings.parquet \
  --sql "SELECT * FROM crawl4ai WHERE status_code = 200"
```

## 3a. The web workbench

http://localhost:8088 → **Query**. Schema tree on the left, SQL editor with autocomplete over the real catalog, results grid, and Export to CSV / Parquet / JSON / JSONL. ⌘/Ctrl-↵ runs. Datasets are addressed by the same bare names, so a query drafted there runs verbatim in `kontra dataset query` and pastes into `dispatch --query`.

It runs the **same engine** as the CLI's default path — `POST /api/datasets/query` — on a connection deliberately built to be bad at everything except reading datasets:

| Control | Effect |
|---|---|
| `READ_ONLY` attach | `DROP` / `DELETE` / `INSERT` / `CREATE` against the lake are refused |
| `disabled_filesystems=LocalFileSystem,HTTPFileSystem` | no local file read or write, no `INSTALL … FROM` a URL, and no arbitrary outbound fetch — `s3://` is a separate filesystem and keeps working |
| `lock_configuration=true` | the submitted SQL cannot re-enable any of the above, `memory_limit` included |
| wrapped as `SELECT * FROM (…)` | a non-`SELECT` statement is a parse error before the connection sees it |
| its own DuckDB instance | the hardening is instance-global and must not reach the connection the rest of the server uses |

Blocking `HTTPFileSystem` is specifically the SSRF defence, and it was added because the hole was real: httpfs must be loaded (S3 is built on it), and while it is, `read_csv('http://169.254.169.254/…')` is a working outbound GET from the API container — measured returning HTTP 404, i.e. the request left the box.

There is **no spilling**, by construction: with LocalFileSystem disabled DuckDB cannot spill to a temp directory, so `memory_limit` (`KONTRA_QUERY_MEMORY_LIMIT`, default 192 MB) becomes a real ceiling and a runaway join errors instead of growing until the cgroup OOM-kills the API. A query that hits it says so and points at `--local`.

The routes are bearer-gated (`KONTRA_EXPLORE_TOKEN`, falling back to `KONTRA_STATE_TOKEN`) and **fail closed** — with no token configured they serve nothing. Note what gating a *browser* page means: the token is baked into the bundle at build time, so it is only as private as the page serving it. This surface belongs on the private network, not behind a public port.

## 4. A dataset is the INPUT too — shape it here, page it there

The headline workflow. Load a list, shape it with SQL until the rows *are* the units you want, then
hand that **same query** to `batches()` in your workflow:

```sh
kontra dataset create bbscope.parquet bbscope

kontra dataset query bbscope \
  --sql "SELECT host AS url, 5 AS max_pages FROM bbscope WHERE platform = 'hackerone'"
```

```python
async for batch in catalog.dataset("bbscope").batches(
    200,
    order_by="url",
    query="SELECT host AS url, 5 AS max_pages FROM bbscope WHERE platform = 'hackerone'",
):
    found, dropped = await crawl4ai.crawl(batch, out)
```

**The rows the query returns ARE the units the actor receives** — the query you proved on the CLI is
the query that pages, over the same view.

- `where=` is the shorthand for the common `WHERE` case; `query=` is full SQL over the dataset by its
  bare name. Passing **both is an error**, not a silent precedence.
- `order_by` is **required**, and it is correctness rather than ceremony: a materialized dataset
  stamps no row id, so `LIMIT`/`OFFSET` over it has no defined order and two pages may overlap or
  skip units with nothing raising.
- An **empty first page raises** — a dataset that is gone, misspelled or filtered to nothing is a
  mistake, not an empty sweep. An empty *later* page is simply the end, and iteration stops on the
  first short page, which the pager knows by reading one row past.
- Page size is capped: **200** soft, **1000** hard (`force_size=True` to override the soft one). One
  cascading unit can take a whole worker with it and the run still reports `completed`.

Chaining is the same call — one Method's output **is** the next one's input, as a ref:

```python
resolved, _ = await dns.addrs(batch)
async for chunk in resolved.batches(200):     # re-page: fan-out is the author's business
    checked, _ = await probe.head(chunk, out)
```

## 5. `kontra explore` — one dispatch, no catalog credential on the box

```sh
kontra explore crawl4ai                          # newest dispatch: views + a local DuckDB shell
kontra explore crawl4ai@1.0.0 --dt 2026-08-03T17
kontra explore crawl4ai --sql "SELECT ..."       # one-shot, non-interactive
kontra explore --list                            # every actor/version/dt that has output
kontra explore crawl4ai --ttl 300                # shorter presigned URLs (server clamps to [60, 900])
kontra explore crawl4ai --ui                     # the DuckDB web UI (fetches assets from ui.duckdb.org)
kontra explore crawl4ai --print-init             # just print the generated init script
```

An **actor name is the address**. A raw run id still works (a leading `orch-` is stripped) and scripts can keep using one, but nobody has to carry it. When several dispatches match a selector the CLI says so and prints the `--dt` for each, rather than silently picking one — that is how an operator ends up reading yesterday's data.

Needs `KONTRA_EXPLORE_TOKEN` (falling back to `KONTRA_STATE_TOKEN`, the same order the server uses). The endpoint mints presigned URLs, so it **fails closed**.

What happens (`cli/explore.go` + `control/orchestrator/src/data/explore.ts`):

- `GET /api/datasets/runs?actor=…&version=…&dt=…` resolves the name to a dispatch. `GET /api/runs/:runId/explore` then returns per-actor state, **column schemas**, and presigned GET URLs for **that dispatch's `version=…/dt=…` files only**. Default lifetime 900 s.
- The CLI writes the init script into a private `0600` workspace, removes it on exit, and passes it as `-init <file>` — never in argv, because `/proc/<pid>/cmdline` is world-readable and the URLs in it are the credential. Leftovers from a crashed session are swept on the next start.
- **One view per ACTOR** (`crawl4ai`), named after the actor — never a node id, never a physical table name.
- Plus **`kontra_datasets`** — one row per actor the dispatch *recorded*, files or not: `actor, version, dt, state, rows, error, view`.

```sql
SELECT actor, version, dt, state, "rows", error FROM kontra_datasets ORDER BY actor;
SELECT * FROM crawl4ai LIMIT 500;
SELECT status_code, count(*) FROM crawl4ai GROUP BY 1 ORDER BY 2 DESC LIMIT 50;
```

DuckDB **exits** if any statement in an init file fails (an expired URL, a deleted object), so the CLI prints that table itself *before* DuckDB starts — it survives a workspace that fails to open. The fix for an expiry is to re-run `kontra explore`, not to cache.

Exploring during `finalizing` is fine: the complete datasets are readable now, and the CLI prints the materialization counts and tells you to re-run once it settles.

## 6. `failed` is not an empty result

The distinction this whole surface exists to preserve:

| What you see | What it means |
|---|---|
| `state=complete`, `rows=0` | the actor ran and **found nothing**. A successful empty result. |
| `state=failed` | that output is **MISSING**, not empty. A query over the rest under-reports and will not say so. |
| `state=pending` / `running` | not written yet — re-run once the dispatch settles |

A dataset's state is the **worst** of the nodes folded into it: one failed shard makes the dataset `failed`, because a healthy-looking count over four of five shards is exactly the defect ADR 0017 exists to remove. `kontra explore` prints `!! N dataset(s) FAILED to materialize — that output is MISSING, not empty` and gives a fileless dataset a row with its error text. There is no `partial` state.

Materialization never fails the run — analytics stay off the execution critical path (ADR 0005). It is no longer *silent*, which is the change: an exhausted decode records `failed` and surfaces as `output_failed`.

## 7. Lifecycle — the projection over both dimensions

```sh
curl -s localhost:8088/api/runs/<run-id>/lifecycle | jq
```

Returns `execution` (the raw `RunStatus`), `materialization` (counts + `rows`/`bytes`), `materializationNodes` (the per-node records), and `lifecycle` — a **projection**, never a `RunStatus` member:

| `lifecycle` | Means |
|---|---|
| `executing` | the actor graph has not reached a terminal outcome yet |
| `finalizing` | execution is done; typed output is still being written |
| `completed` | execution succeeded **and** every node's typed output is queryable |
| `output_failed` | the run has no trustworthy queryable output |

Order matters: a `failed` materialization outranks an outstanding one, and a run whose *execution* failed or was cancelled projects `output_failed` even when the records it did produce committed. The raw execution status always rides alongside, so you can still see *which* dimension broke — that pairing is the point.

Poll `lifecycle` when you care that the **output** is ready. `GET /api/runs/:id/output` reads the same projection and **409s while `finalizing`**, rather than naming datasets you cannot yet query.

## 8. Exact dispatch is the default; `--catalog` is the exception

**A presigned URL is object-level authorization.** Whoever holds it reads that object, whole; it carries no notion of a row filter. A `WHERE` clause in generated SQL is defence in depth and **not** access control — if one data file held two dispatches, a URL scoped to one would hand over the other as well.

Three properties make the exact-dispatch scope real:

1. **Physical exclusivity** — output is partitioned by `(version, dt)` and `dt` is the dispatch time to the second, so every parquet file lives under exactly one `version=…/dt=…/` directory, and only files under the requested one are presigned. Compaction re-checks that after every pass and **never merges across dispatches** (`control/orchestrator/src/data/maintenance.ts`).
2. **Short expiry is the boundary** — the URL, not the file, is the secret. Deleting the init script is hygiene; the 15-minute lifetime is the guarantee.
3. **No durable credential leaves the server** — the workstation gets URLs, never object-store keys and never the catalog connstring.

Three tiers of reach, in order of how much credential they want:

| Command | Reach | Credential on the workstation |
|---|---|---|
| `kontra explore <actor>` | one dispatch | the explore token only — presigned URLs, nothing durable |
| `kontra dataset list` / `query` | the catalog's table list; one named dataset at a time, any dispatch of it | `KONTRA_DUCKLAKE_CATALOG` + `KONTRA_S3_*`; the `ATTACH` is **not** read-only — point it at a read-only role |
| `kontra explore --catalog` | every dispatch in the lake | separately provisioned read-only creds; refuses to start without them |

```sh
export KONTRA_CATALOG_PG='postgres://<read-only role>@host/db'   # NOT the orchestrator's writer DSN
export KONTRA_S3_ENDPOINT=… KONTRA_S3_ACCESS_KEY=… KONTRA_S3_SECRET_KEY=… KONTRA_S3_BUCKET=… KONTRA_S3_REGION=…
kontra explore --catalog
```

The `ATTACH` is `READ_ONLY`, extensions are pinned (no autoinstall, no autoload, no community repo), and you land on a `kontra_datasets` view built from catalog metadata alone — no data file is opened and no hash has to be decoded:

```sql
SELECT actor, version, dt, "rows" FROM kontra_datasets ORDER BY dt DESC LIMIT 500;
SELECT * FROM lake.output."crawl4ai" WHERE version = '1.0.0' AND dt = '2026-08-03T17-50-50' LIMIT 500;
```

## 9. Which surface answers which question

| Question | Surface |
|---|---|
| **What did it find?** | `kontra dataset query <name>` · `kontra explore <actor>` — DuckDB on your workstation ([[Data-Plane]] §4) |
| **Is it moving?** | The Dashboard's **Monitor** and **Datasets** surfaces — http://localhost:8088 — over the bounded summaries below |
| **Is the output queryable yet?** | `GET /api/runs/:id/lifecycle` |
| **What is streaming right now, before materialization?** | `kontra monitor --run-id <id> --query "SELECT * FROM run LIMIT 20"` — one view over the run's raw `units/` blobs |
| **Why did that node fail / where is the history?** | Temporal Web UI — http://localhost:8233 |
| **Is the stack up, are there pollers?** | `kontra doctor` · `kontra infra status` · `kontra workers list` |

**The "is it moving?" surface is not the findings explorer.** Grafana used to hold this row, and the reason it no longer does is the same reason it was always bounded here: a data-frame model with no struct type either flattens nested actor output (losing shape) or serializes it to a string (losing queryability), and pointing a panel at the parquet means an object-store scan per refresh. The Dashboard reads the summary routes instead — four scalar columns from a relational table:

| Endpoint | Returns |
|---|---|
| `GET /api/summaries/health` | fleet-wide `{pending, running, complete, failed, stale, refreshedAt}` |
| `GET /api/summaries/runs/:runId` | one run's summary row (both dimensions + totals) |
| `GET /api/summaries/hourly?hours=N` | output volume by actor and hour (`N` clamped to `[1, 720]`) |

`stale` is the number that matters: non-terminal records past the threshold, i.e. materialization nothing is going to finish. Without it a worker that died mid-decode leaves runs reading `finalizing` forever with no panel naming the cause.

The summary refresh is deliberately **best-effort** — it feeds dashboards, not correctness; the status ledger stays the authority, and a refresh failure is logged rather than failing the decode. `run_id` is a *column* in these tables, never a metric label (one series per run is unbounded cardinality).

## 10. Configuration

| Env var | Purpose | Default |
|---|---|---|
| `KONTRA_EXPLORE_TOKEN` | gates `GET /api/runs/:id/explore`; set it on **both** the orchestrator and the workstation | falls back to `KONTRA_STATE_TOKEN` |
| `KONTRA_STATE_TOKEN` | the fallback for the above, and the token for `/api/state/*` | _(unset ⇒ both endpoints 503)_ |
| `KONTRA_DUCKLAKE_CATALOG` | DuckLake catalog DSN — what `kontra dataset` attaches; `postgres:…` shares one catalog across processes | `orchestrator-datasets.ducklake` |
| `KONTRA_DUCKLAKE_DATA_PATH` | lake `DATA_PATH` (the bucket root) | `s3://<bucket>/`, else `./` |
| `KONTRA_S3_ENDPOINT` (or `KONTRA_S3_PUBLIC_ENDPOINT`) | the store `kontra dataset` reads and writes parquet through | `http://localhost:8333` |
| `KONTRA_S3_BUCKET` / `KONTRA_S3_ACCESS_KEY` / `KONTRA_S3_SECRET_KEY` | bucket + the credential pair DuckLake writes need | `kontra` / `kontra` / `kontra` |
| `KONTRA_CATALOG_PG` | read-only catalog DSN for `kontra explore --catalog` | _(required for that mode)_ |
| `KONTRA_MATERIALIZATION_DB` | the materialization status ledger + summary tables (`postgres://…` ⇒ schema `kontra`) | `KONTRA_ORCHESTRATOR_DB`, else `orchestrator.db` (SQLite) |

`kontra dataset`/`db` rewrite a bare `host=safedeps-postgres` in `KONTRA_DUCKLAKE_CATALOG` to `host=localhost`, because that name is a compose-network name the host cannot resolve; `--catalog` overrides it verbatim. `--data-path` overrides the lake root per invocation — it must match what the catalog recorded or DuckLake refuses to attach.

Both token-gated endpoints **fail closed**: with no token configured they return `503` and serve nothing. Presigning without authentication would make the per-dispatch scoping claim false by construction. The rest of the API is unauthenticated — that predates this work and is recorded, not fixed, here.
