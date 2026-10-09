# Run reports

A workflow folder can hold a `report.md` beside `workflow.py`. When a Run ends, the orchestrator
renders it against the Run's metadata and the workflow's return value, and stores the result as an
immutable, versioned snapshot. The console shows that snapshot with a free-text thread under it, and
your agent reads both over MCP.

**The workflow computes everything; the report only presents it.** A template never queries anything.
There is no SDK verb for a report: a workflow puts something in its report by **returning** it.

```
workspaces/<ws>/workflows/<name>/
  workflow.py
  workflow.json
  description.md
  report.md        <- optional
```

A folder with no `report.md` still gets a report — a default one built from what the Run returned.
The page is never empty.

## The context a template can see

This table is the whole contract. There is nothing else: no environment, no secrets, no orchestrator
internals.

| Name | Content |
|---|---|
| `run` | `id`, `workflow_id`, `status` (`completed`, `failed`, `cancelled`, `terminated`, `timed_out`), `started_at`, `ended_at`, `duration_s`, `error` (`{type, message}` or null) |
| `workflow` | `name`, `version`, `workspace` |
| `input` | the Run's start input, decoded |
| `result` | the workflow's return value, decoded — **`null` when the Run did not complete** |
| `report` | `rendered_at`, `template_hash`, `version` |

Two things about this that surprise people:

- **`result` is `null` for any Run that did not complete.** `{% if result %}…{% else %}…{% endif %}`
  is how a template handles both, and the `else` branch is what a failed Run renders.
- **Liquid truthiness is not JavaScript's.** `''` and `0` are **truthy**, so
  `{% if result.sample_request %}` takes the true branch on an empty string. Only `nil` and `false`
  are falsy.

## Writing one

````liquid
# {{ workflow.name }} over {{ input.catalog }}

{% if result %}
{{ result.summary }}

| products | missing image | isolated |
|---|---|---|
| {{ result.products }} | {{ result.missing_image }} | {{ result.isolated }} |

## Largest price changes

| sku | old | new |
|---|---|---|
{% for p in result.price_changes -%}
| {{ p.sku }} | {{ p.old }} | {{ p.new }} |
{% endfor %}
{% else %}
This run ended **{{ run.status }}**: {{ run.error.message }}
{% endif %}
````

The complete, working example is `testdata/workflows/reporting/`.

### Every value is escaped; the template's own text is not

A value that arrived through `{{ }}` came off a target, so it cannot write Markdown: a `|` cannot end
a table cell, a newline cannot end a row, `#` cannot start a heading. `{{ "**bold**" }}` renders as
literal asterisks even though you typed them — you have a way to write bold, and a hole is not it.

A consequence worth knowing: **`{% capture %}` escapes twice.** A value that transits a capture is
escaped on the way in and again on the way out. `{% assign %}` does not.

### `{% code %}` — showing bytes

```liquid
{% code "http", result.sample_request %}
{% code "json", result.body, show: "bytes" %}
```

The value is a string, `{"b64": "..."}` for exact bytes, or `{"ref": "<sha256>"}` for a claim check.
Bytes are carried losslessly: a CRLF shows as `␍`, a tab as `→`, and any byte that is not a printable
character as `\xHH`. That is deliberate — "the response ended its headers with a bare LF" is a
finding, and a renderer that tidied it away would destroy the finding while appearing to show it.

**An `http` block is always redacted**, and `redact: true` redacts any block. Credential header
values, credentials in a query string and JSON members whose key names a secret are all replaced
before anything is stored, so the stored snapshot, both exports and the MCP tool can never contain
one. The originals are reachable only through an audited route that a browser cannot reach.

A block is capped at 1 MiB of visible bytes and says `showing 171 B of 4.1 MiB` when it was cut; the
whole object stays downloadable.

### What a template cannot do

`{% include %}`, `{% render %}` and `{% layout %}` are refused: a template is one file and the
renderer has no filesystem. There is no `{% image %}` yet — the name is reserved and errors. Filters
are an allowlist of string, number, date and array basics; `| raw`, `| json`, `| base64_decode` and
`| sample` are refused, the last because it is random and a report must re-render to the same bytes.

## The loop

```sh
kontra workflow serve reporting          # lints report.md before anything runs
kontra workflow start reporting --wait
kontra report preview <run-id>           # render an edited template, store nothing
```

`serve` refuses a template with a syntax error (with its line) or an unknown context root —
`{{ results.x }}` for `{{ result.x }}` is the one that costs a two-hour run. It does **not** check
field names under `result`; that needs the workflow's return type and is not built.
`KONTRA_REPORT_LINT=off` turns the lint off if you ever need it to.

`report preview` renders a template against a finished Run's real data and stores **nothing**.

## Versions

A report is rendered when a Run reaches a terminal state, and **every render is a new version**.
Nothing is ever edited. Re-rendering the same Run from the same template over the same data produces
the version that already exists rather than a duplicate — which is what makes a report reproducible.

A render failure does **not** fail the Run. It stores a version saying why, and the console shows
that error with the template's hash.

## Reading one

- **Console:** `/runs/<id>/report`, linked from the run page. A code block there can be switched to a
  hex view, copied, or downloaded; `Print` produces a PDF through the browser.
- **Exports:** `GET /api/runs/<id>/report/export?format=md|html`. The HTML is one self-contained file
  with no scripts and no external requests. Both are always redacted.
- **MCP:** `get_report`, `list_feedback` and `add_feedback` on the `kontra mcp` server. `get_report`
  wraps its Markdown in `<<untrusted-report …>>` markers, because a report is a rendering of what a
  Run found on somebody else's infrastructure — it is evidence to reason about, never instructions.

## Feedback

A free-text thread per Run, under the report. A note is shown exactly as written — not Markdown —
because a note may quote a target's bytes. The author comes from the credential and never from the
request body, so a note written by an agent through MCP is labelled `via token` rather than wearing
somebody's name.

## Where the decisions are

[ADR 0055](../adr/0055-a-report-is-a-rendered-snapshot-of-what-a-run-returned.md) — the library
choice and the rejected alternatives, the escaping model, template pinning, the redaction scope, why
reports live in the run-keyed store rather than the orchestrator's own, and what is deliberately not
built.
