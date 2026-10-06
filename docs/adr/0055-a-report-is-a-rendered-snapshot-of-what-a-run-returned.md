# 55. A report is a rendered snapshot of what a Run returned

Date: 2026-10-06

## Status

**Accepted**, in full: the rendering engine, the redaction rule, the store, the run-end lifecycle, the
HTTP surface, the CLI developer loop and the console page.

## Context

A Run finishes and leaves behind a workflow's return value, a status, some timings, and — when it
touched a target — rows in object storage. Today a person reads that by opening the Run page and
looking at raw output. Nothing says what the Run *meant*, and nothing survives in a form anybody can
hand to somebody else.

The three things that have to be true at once are what make this hard:

1. **The template is not reviewed.** It lives in a workflow folder in a workspace, written by whoever
   wrote the workflow, and it executes on the orchestrator — the process holding the object-store
   credentials and the Temporal client.
2. **The data is hostile.** This is an offensive-security product. A workflow's return value contains
   bytes that came off somebody else's infrastructure, and a value that can write Markdown can move a
   table border, open a code fence, or end the cell it was supposed to sit in. "The report said what
   the Run found" becomes "the report said what the target wanted it to say".
3. **The render happens after the Fleet is gone.** A report is rendered when a Run reaches a terminal
   state, which is after every `fleet.hold()` scope has exited. Anything the renderer needs from a
   serving actor is not there.

## Decision

**The workflow computes; the report presents.** A template never queries. A Run puts something in
its report by returning it. There is no SDK verb for any of this.

### 1. Liquid, and the alternatives that were rejected

**LiquidJS**, pinned to an exact `10.30.0`. Shopify's language was designed for templates written by
people the host does not trust: loops, conditions and filters, with no expression evaluation and no
code execution.

Rejected, with reasons, because each was proposed and each is worse here:

- **Handlebars / Mustache** escape for HTML, not for Markdown — the wrong grammar, so every value
  would need escaping again anyway — and Handlebars has a prototype-pollution history.
- **Nunjucks / Jinja** have had sandbox escapes. A template is not reviewed; a sandbox escape is a
  shell on the orchestrator.
- **MDX** executes JavaScript. It is the opposite of this requirement.
- **Evidence.dev** puts SQL in the report, which makes the template a query and the author a database
  client. §1's "the workflow computes everything" exists to prevent exactly that.
- **A hand-rolled resolver** was the tempting one: `{{ a.b }}` is twenty lines. It is also a new
  grammar with no users, no fuzzing and no prior art, in the one place where being boring is worth
  most.

The Markdown parse is `unified` + `remark-parse` + `remark-gfm`, and it runs **after** Liquid.

**A note on packaging, because it nearly bit:** the remark stack is ESM-only and this package is
CommonJS. It works because `require(esm)` is default from Node 22.12 and `engines.node` here is
already `>=22.13.0` — the floor that makes it safe was in place before this change needed it. The
orchestrator image is `node:22-bookworm-slim`, which resolves above that floor. Verified by running
`require('unified')` from CommonJS rather than by reasoning about it.

### 2. Escape maximally, then parse, then drop — in that order

Every `{{ }}` output goes through `markdownEscape` (`report/escape.ts`), registered as LiquidJS's
`outputEscape`. Template text does not. **That is the whole model: template text is structure, a hole
is data.** `{{ "**bold**" }}` renders as literal asterisks even though the author typed them, because
the author had a way to write bold and chose a hole instead.

The pipeline is then: **escape → parse to mdast → drop what is not allowed → store the TREE.**

Reversing any two steps breaks it. Sanitising the string before parsing is regex-against-a-grammar,
which loses. Storing HTML means something downstream has to re-parse it. Because the *tree* is
stored, the console renders from structure and has no `innerHTML` anywhere — there is no HTML
upstream of it to render.

Escaping is context-free and therefore ugly at the intermediate stage: `kontra\-fleet`, `12\,480`.
That costs nothing, because nobody reads that stage. Both the Markdown export and the console are
generated from the tree, where a serialiser re-escapes only what its own position needs. Escaping
lightly to keep the intermediate readable is how a table breakout reaches the tree.

Two allowlists carry the rest of the model, and **both have their completeness asserted by a test**:

- **Filters.** 10.30.0 ships 88. Each is either allowed (string, number, date, array basics) or
  refused with a stated reason. Upgrading liquidjs fails a test rather than silently granting a
  template a new capability.
- **Node types.** A snapshot may hold 18 mdast types. Anything else is converted to its text or
  dropped. This is also what gives the console's `ReportView` a finite job.

### 3. What the measurements changed

§4.1 of the specification gives a configuration block. Running it against the pinned version rather
than reading it changed five things, and each is a test now.

| What the spec says | What 10.30.0 does |
|---|---|
| `fs: undefined` | **Throws in the constructor.** `normalize()` reads `options.fs.dirname` before it consults `relativeReference`. The filesystem is closed with a stub that refuses, plus `relativeReference: false`, plus `include`/`render`/`layout` overridden to say so in words. |
| `renderLimit: 2_000` | **Fails legitimate reports.** A 20,000-row GFM table through the real escaper measures 1,053 ms, and failed outright when run alongside other tests on a loaded box — which is a materializer's normal condition. Raised to 10,000 ms. |
| `memoryLimit: 10_000_000` bounds memory | It counts **cumulative characters allocated**, not bytes retained. An `append` loop is quadratic in its iteration count (trips at ~1,400 appends of a 10-character literal); a 20,000-row table is flat and passes under 100,000. The value stands, for a reason the spec does not state. |
| test 5: the 10⁸ loop stops at `renderLimit` | It stops at **`memoryLimit`, in 2 ms** — the range itself allocates. `renderLimit`'s real case is work that is slow without allocating much, which is the 50,000-row table at 2,199 ms. |
| "unregister or override the `raw` filter" | Both work. Deletion is used, because a deleted filter is `ParseError: undefined filter: raw` at **parse** time, which `serve` lint and `report preview` catch without rendering. |

Two further measurements with no line in the spec at all:

- **`| sample` is random** — 10 distinct outputs over 40 renders of one input. It is refused, because
  acceptance tests 12 and 13 require a re-render to reproduce a version, and one random filter makes
  every snapshot irreproducible.
- **`{% capture %}` double-escapes.** `{% capture c %}{{ y }}{% endcapture %}{{ c }}` escapes `y`
  twice. `{% assign %}` does not. Authors will meet this; the `serve` lint will warn on it.

And one correction to the §2.4 contract, which does not mention it: **`jsTruthy: false` is Liquid
truthiness, so `''` and `0` are TRUTHY.** `{% if result.sample_request %}` takes the true branch on an
empty string.

`renderLimit` is enforced by the library **periodically, not continuously** — a 300 ms budget was
measured overshooting to 5,022 ms on a tight loop. So the limit bounds a render loosely, and the
render runs in a worker thread whose own wall-clock deadline is the tight bound. That also satisfies
test 5's "another route answers in under 100 ms during the render", which an in-process render cannot:
Liquid's render does not yield to the event loop between iterations, so a 5-second render in the API
process blocks every other route for 5 seconds — and `POST /report/preview` is in the API process.

### 4. A ref is resolved from object storage, never from an actor

`{% code "http", value %}` accepts a string, `{b64}` exact bytes, or `{ref}` — a claim check.

Dereferencing a claim check **from a workflow** dispatches `kontra.fetch_blob` to
`<actor>-<version>`, an activity only an actor host registers. With the Fleet gone there is no queue
to ask and the read **hangs rather than erroring**: measured on a live Run that finished its work,
sealed every partition, and then sat in `running` with the activity reading `stalled` for one hour
and forty minutes before it was cancelled, costing a Droplet two hours of idle billing. A report
renders after every Fleet scope has exited, so that is this feature's default case.

So the renderer **reads the object store directly, in the orchestrator role, with the orchestrator's
own credentials**. No activity, no task queue, no actor — nothing to be absent. The hang is
structurally impossible rather than bounded. A deadline is kept anyway, because that protects against
a missing actor and not against a slow store.

**An unresolved ref is a first-class output, not an error.** The block renders a marker naming the ref
and why it was not read, and the version still stores as `ok`. A report that states what it could not
read is worth more than no report — and the inverse mistake, a block that renders empty, is
indistinguishable from a ref that legitimately held nothing. That confusion has already cost this
project a live Run in a different place, where an empty first page and a broken query raised the same
error.

Rendering is also deliberately **outside** any lease scope. Doing it inside, before the Fleet exits,
would resolve refs for free — and would extend every Run's lease by the render's duration, which is
the failure above with a different first cause.

### 5. Redaction, and the escaping that surrounds it

`lang == "http"` always redacts; `redact: true` redacts anything; the redacted bytes are what the
snapshot holds, what the console shows and what both exports contain. The originals go to
`report_secrets`, reachable only through an audited route behind a scope beyond `console`.

Redaction runs over a **latin-1 view** of the bytes, not a UTF-8 decode. Latin-1 is a total bijection
on the 256 byte values, so every byte the rules do not touch survives exactly; a lossy UTF-8 decode
replaces each invalid sequence with U+FFFD and the original bytes are gone for good. The rules are
ASCII patterns — header names, base64, hex — so they lose nothing by not seeing multi-byte characters
as characters. The visible text is a separate UTF-8 decode of the redacted bytes, so a report in
another script still reads correctly.

The corpus and the three-language parity are the next revision's subject.

### 6. The context contract

What a template can see, and nothing else — no environment, no secrets, no orchestrator internals.

| Name | Content |
|---|---|
| `run` | `id`, `workflow_id`, `status` (`completed`, `failed`, `cancelled`, `terminated`, `timed_out`), `started_at`, `ended_at`, `duration_s`, `error` (`{type, message}` or null) |
| `workflow` | `name`, `version` / source digest, `workspace` |
| `input` | the Run's start input, decoded |
| `result` | the workflow's return value, decoded, refs left as refs; **`null` when the Run did not complete** |
| `report` | `rendered_at`, `template_hash`, `version` |

The default report — for a folder with no `report.md` — is rendered through this same engine with one
extra key, `default`, holding tables that TypeScript has already flattened. Liquid has no recursion
and `{{ result }}` is refused outright, so a template cannot walk an unknown shape; the walk belongs
in code where it can be tested. That key exists only for the default template, which is what lets the
table above be published as the whole truth.

## Consequences

- A report is **immutable and versioned**. Re-rendering produces a new version; no version is edited.
  Every ingredient of a render is therefore required to be deterministic, which is why one random
  filter was a correctness bug and not a style preference.
- The console gains a renderer with a finite node vocabulary and no HTML path.
- A template author gets errors at parse time for a refused filter and at render time for a refused
  tag, both before any Run is waited on.
- **The default report is not optional.** Most Runs will never have a `report.md`, so if "no template"
  meant "no report" the feature would read as broken rather than unused.
- The 5 MiB snapshot cap is measured on the **serialised JSON**, blocks base64 included: a 1 MiB block
  costs 1.37 MiB of snapshot, and a cap applied to the Markdown would let four of them through.

### 7. The four tables are NOT in the orchestrator's own store, and the reason is a process boundary

§7.1 says to use `db/repo.ts`. That store's header records a decision this would reverse — it holds no
run records at all — and reversing it would be arguable on its own. What is not arguable is where the
three participants run.

| | |
|---|---|
| `repo.ts` is | `node:sqlite` over a FILE, on the `orchestrator-db` volume, which `docker-compose.yml:703` mounts on the **kontra-api service only** |
| a report is written | where a finished Run is noticed |
| a report is read | by the API |
| a report is deleted | inside the retention activity on the dataset queue, which the **materializer** role polls (`roles.ts:91`) |

Those are one container today only because compose runs `api,materializer` together
(`docker-compose.yml:671`). Split the roles — the entire point of having roles — and a `repo.ts`-backed
report silently finds no file, or creates an empty one at the relative default `orchestrator.db` and
reports nothing wrong. So the tables join `data/sql.ts`, Postgres when the writer and reader are
different hosts and SQLite otherwise, beside `runWorkflows`, `summaries`, `datasetRecords` and the
materialization ledger. §7.1 offers that as the escape hatch for oversized blobs; the real reason is
the boundary.

**The cost, stated rather than discovered later.** §7.1 also says a deleted Run's four tables are
cleaned "in the same transaction". **That family has no transaction primitive at all** — `SqlDriver`
is `exec`/`run`/`all`/`close`, and the only `tx()` in the orchestrator's TypeScript is `Repo.tx()`, over
the handle this deliberately does not use. Even on a single-host SQLite install the two families are
two `DatabaseSync` handles over one file, which cannot share a transaction and will take `SQLITE_BUSY`
from each other.

So `purgeRun` deletes in a STATED ORDER and the order is the mitigation: **`report_secret` first.** A
purge that dies half-way has removed the only rows in this system holding an unredacted credential;
what survives is a redacted snapshot and some feedback, which the next sweep takes. The reverse order
would leave unredacted bytes behind a version row that no longer exists. For the same reason the report
arm is **first** of the five in `collectRun`, whose arms are sequential bare awaits with no rollback.

### 8. The render hook attaches where run-end detection actually lives, which is not where §4.5 says

§4.5 names `data/sealFinishedDatasets.ts` and the materializer role. Neither notices anything:
`sealFinishedDatasets.ts` is a pure decision function whose only importer is its own test, and
`materializer.ts` is a Temporal Worker registering activities on `kontra-datasets`, hosting no
workflows.

What notices is `startHistoryArchiver` in `historyArchive.ts` — a `setInterval` in the **api** role
running one Temporal visibility query over closed runs. The report sweep is that pattern copied
deliberately: the counters, the `onNote`/`onError` split, the single-flight guard, the capped-page
warning. It was built for the same problem ("a Dataset outlives the Run; the Run's story does not").

§4.5's actual requirements are met: not in a request path, not inside a workflow. The RENDER goes to a
worker thread, so the API's event loop is not held by it — which is also the only way acceptance test
5's "another route answers in under 100 ms during the render" can be true, since Liquid does not yield
between iterations.

**The worker's honest gap.** The worker entry is a `.js` file that exists in a built image and does not
exist when the orchestrator runs from TypeScript sources, as vitest does. The host falls back to an
in-process render and SAYS SO through `onNote`. The in-process path is still bounded by `renderLimit`,
so the fallback is slower to protect rather than unprotected — but the worker path is exercised by the
built artifact and by nothing in the unit suite, and test 5 is therefore a property of the image.

### 9. §4.6's fallback cannot be implemented as written

Pinning happens at Run start, in `startRun`, beside the identity stamp `stampRunWorkflow` — ADR 0025's
pattern, and the folder is only known there. A folder with no `report.md` pins the default BY NAME,
which distinguishes "this Run had no template" from "this Run was never pinned".

§4.6 says that if the start path cannot pin, the renderer should "fall back to capturing at run end".
It cannot: **nothing maps a run id to a folder.** `runWorkflows` records a manifest name and version,
Temporal holds a workflow type, and the id is `<type>-<unixseconds>`. So the fallback is the default
report plus a warning in the snapshot naming why — which is the better form of §4.6's intent anyway,
since reading today's `report.md` for a Run that started last week produces a report nobody can
reproduce, the exact thing pinning exists to prevent.

**Which runs are affected:** one started through `POST /api/runs` is pinned. One started by
`kontra workflow start` is not — that command dials Temporal directly (`cli/workflow.go`) rather than
going through the route. Closing it means the CLI POSTing the template alongside the identity it
already records over `--api`, which is the CLI change's business.

### 10. The default template is named by its own digest, not by the release

§4.6 asks for `default@<kontra version>`. A package version does not change when the template's text
does, so two different default reports would both be `default@0.1.0` and neither would be reproducible
from its own name — which defeats acceptance test 12. The default is therefore
`default@<12 hex of sha256>`, with an explicit version still accepted for an install that wants its
release in the name.

## Consequences of the storage choice

- A report **outlives the Run's Parquet** under normal retention and dies with the Run when it is
  collected, which is what §7.1 asks for — by a sweep arm rather than by a cascade, because the
  database that would cascade is not the one holding the rows.
- An install whose report store has never been opened must not fail a retention sweep for the absence
  of a table it would create on first use, so the arm is optional and collection-only, like `summaries`.
- `render_key` is a sha256, not the canonical JSON it digests. The first version of it returned the JSON
  — which would have put a decoded multi-megabyte result into a UNIQUE-indexed column, and both
  backends would have accepted it.

### 11. `report:reveal` is a browser lock, not a capability boundary — and the spec reads the other way

§7.2 puts the unredacted bytes behind "a scope beyond `console`". Measured in `auth.ts`, a scope cannot
carry that weight:

```ts
const live = sessions.look(bearerOf(authorization));
if (live) {
  if (live.scopes.includes(scope)) return null;
  return { code: 403, … };
}
// the service-token compare happens here, and never consults `scope`
```

A caller holding the token is unaffected by any scope. So the reveal route is built the way ADR 0054
built `infra`:

- **`STATE_TOKEN_VARS` is the authority** — one variable, no fallback, fail-closed, so an install with
  no token configured answers 503 and serves nothing.
- **`REPORT_REVEAL_SCOPE` keeps browsers out**, and no mint path grants it, so no sign-in can produce a
  session that reveals.

Saying "the scope gates it" would read as a guarantee it does not give. If a browser must ever reveal,
that needs per-user authorisation data, which exists nowhere today: a scope on `ConsoleUser`, the CLI's
`AuthUser`, and a `kontra user` flag to set it.

**And the audit is not transactional.** `audit()` never throws — a full volume loses the line and the
action still happens (`audit.ts`). "No reveal without a record" is therefore not a property this gives.
Both outcomes are recorded and the refusal is audited before the gate, which is the most that mechanism
supports.

### 12. The generated spec can publish a gate that does not exist, and the report surface would have hit it

`openapi.ts`'s `GATES` table is a longest-prefix match over the raw route URL, not derived from the
handler. Without an entry, every `/api/runs/:runId/report*` path would inherit `/api/runs`'s
`runToken` — the wrong token, published as fact, over routes that check `EXPLORE_TOKEN_VARS`. And
`openapi.test.ts` would not catch it: it verifies that paths documented as OPEN are open, never that a
documented gate is the real one.

So the four `GATES` entries are part of this change, and the eleven `POSTURE` entries in
`auth/apiGate.ts` are the half that is actually enforced — `apiSurface.test.ts` boots the real server
and proves each `gated` route refuses an anonymous caller, in both directions. That assertion is what
makes a posture claim worth reading.

`EXPLORE_TOKEN_VARS` is the list for the report and the thread, by content: its own header says the
surface it guards "routinely contains targets and sometimes secrets", and a report is a rendering of
exactly that.

### 13. Preview rebuilds the context; it does not read a stored one

§6.2 says preview "uses the stored run context". The snapshot stores the rendered TREE, not the inputs
that produced it, and storing those would be a second copy of every Run's input and result — which is
what the claim-check codec exists to avoid. So preview rebuilds the context from the same reads the
sweep uses, through the same `contextForRun`. Two assemblers would mean previewing a change to a
document the preview is not showing.

A Run whose metadata has aged out of Temporal therefore gets **409 `state: 'gone'`** rather than a
preview against an invented context. A report already rendered for it stays readable.

**`current_template` answers 501.** §4.6 defines re-rendering from today's `report.md` as a separate
action, and §9 above is why it cannot be done: nothing maps a run id to a folder. A pinned re-render
dressed as `current_template` would look like success and produce the wrong document, so the route says
what is missing and names `POST /report/preview`, which takes a template text directly.

**Preview is the one rate-limited route here,** because it is the one that spends unbounded CPU on
request. There was no rate limiter in this codebase to copy — `routes/rowStream.ts` caps concurrency,
not rate — so it is a sliding window per socket address with an injectable cap. With `trustProxy` off
that address is the proxy behind a proxy, which makes it a brake on accidental load rather than a
per-user quota; `rowStream.ts` says the same of itself and so does this.

### 14. The lint is a second Liquid implementation, so it is deliberately narrow

`kontra workflow serve` refuses a `report.md` that cannot work, because a report renders only when a
run ENDS — a typo is otherwise discovered after however long the work took. `github.com/osteele/liquid`
parses; LiquidJS renders; they are different implementations of one language and do not agree about
everything. Measured:

| | osteele/liquid v1.9.2 | LiquidJS 10.30.0 |
|---|---|---|
| `{{ x \| nosuchfilter }}` | parses | `ParseError` under `strictFilters` |
| `{% code "http", v %}` | undefined tag unless registered | the engine's own tag |
| `{{ result.summary }` | literal text | literal text |

So the lint checks only what both agree on: a syntax error, and a context root outside §2.4. It does
not check filters, and it does not check `result.<field>` against the workflow's return type — §6.1
asks for the third and it needs `schemadump.py` extended to dump a return annotation, which is its own
change. **The refusal says so**, because an author who saw a lint pass would otherwise assume
`{{ result.missing_field }}` had been checked.

`KONTRA_REPORT_LINT=off` exists because the renderer is the authority: if the two implementations ever
disagree about a template that renders correctly, an author must be able to proceed.

§6.1 asks for line AND column; `liquid.SourceError` carries a line and no column, and inventing one
would be worse than saying so.

### 15. The report is a PAGE at `/runs/<id>/report`, not the run detail's default tab

§9.1 asks for a tab, defaulting to the report when one exists. The run detail is one 1,718-line
component with **no page-level tab strip** — the two `role="tablist"` strips in that repo are both
in-component and in-memory, neither reflected in the URL — so there is nothing to add a tab to, and
restructuring that component is a blast radius this feature should not take on days before a demo. The
design prototype also shows a standalone page.

So `/runs/<id>/report` is an address that round-trips, which is the property that actually matters: an
operator can paste it. The console has no router; `parseAddress` previously REFUSED every second
segment under a run, and that refusal is now narrowed by exactly one word rather than opened to a
wildcard — a typo'd tab is still `null`.

Honouring §9.1 properly means giving the run detail a real tab strip, which is worth doing on its own
and not as a rider on this.

### 16. No syntax highlighting, because the markers are the half that carries meaning

§9.2 asks for Shiki tokens with markers added after highlighting. There is no highlighter in the
console and adding one brings a grammar bundle for a cosmetic gain, so a code block renders monospace
text with the markers and no colour:

| byte | shown as |
|---|---|
| `\r` | `␍` **before** the break it caused, so CRLF and bare LF are told apart |
| `\t` | `→` |
| other C0, DEL, and every byte ≥ 0x80 | `\xHH` |

Those are evidence. "The response ended its headers with a bare LF" is a finding, and a renderer that
normalised it would destroy the finding while appearing to show it. Highlighting is not evidence.

The bytes are decoded **latin-1, not UTF-8**, for the reason the redaction rule uses latin-1: a UTF-8
decode replaces each invalid sequence with U+FFFD and the original byte is gone, so `\xff` could not be
shown as `\xff`. The cost is that a multi-byte character renders as its bytes; the hex view is where
those are read, and losing a byte entirely is the worse failure for evidence.

### 17. The prototype's palette is a departure, and the console's tokens win

The design prototype is dark (`#0c0f14` ground, `#11161e` cards, `#d97757` accent, IBM Plex). The
console's own tokens are a different dark (`--bg: #0b0f14`, `--panel: #111820`, `--accent: #4aa3ff`,
system fonts). The page takes the prototype's LAYOUT and information architecture — breadcrumb, status
pill, version selector, export actions, report card, thread with avatar, author, relative time and a
`via token` label — and the console's colours, because a report page that looked like a different
product would be the one page in the console that did.

### 18. One bug that only a browser could find

`truncationNote` printed MiB unconditionally, so a 171-byte block truncated out of a 4.1 MiB object
read **"showing 0.0 MiB of 4.1 MiB"** — a measurement that says nothing about the thing it measures.
Every unit test passed. It was found by looking at the rendered page, which is the argument for driving
a UI in a browser rather than trusting a typecheck.

## What this revision does not decide

Honouring §9.1's tab strip; the `result.<field>` type check in `serve`; a `list_feedback` that can
answer for a whole workflow; and whether a browser should ever be able to reveal unredacted bytes,
which needs per-user authorisation data that exists nowhere today.
