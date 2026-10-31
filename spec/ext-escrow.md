# Escrow extension `https://kontra.sh/ext/escrow/v1`

Status: **draft** (M0, revision 2). Public specification; implemented only by Kontra Cloud. The open-source
gate contains no escrow code and names no escrow key: it passes the card entry through from the operator's
annotations, enforces `required: true` like any extension (a `hire` of its own `/mcp` activates the entry for the
buyer, as it does every required entry of the operator's, [`relay.md`](relay.md) §12.2), stores the task metadata the
operator's hooks write, renders the operator's `mcpMeta` verbatim, and offers review-window messages to the operator's
`OnReviewMessage` hook ([`relay.md`](relay.md) §2.5, §7, §13).

Escrow is the price shape `per_task` (README, "Pricing"): a fixed quote held once a person accepts it, released
to the seller after a review window, refunded on dispute or failure. The two metered shapes,
`per_call` and `per_session`, are not escrow; their advertisement format is in §6 because they share
the price object.

## 1. Card entry

In the served card's `capabilities.extensions`:

```json
{
  "uri": "https://kontra.sh/ext/escrow/v1",
  "description": "Fixed quote held in escrow; released after a review window unless disputed.",
  "required": true,
  "params": {
    "quote": { "amount": "15.00", "currency": "EUR" },
    "reviewWindowSeconds": 86400,
    "taskTimeoutSeconds": 7200,
    "rails": ["stripe"],
    "terms": "https://kontra.run/terms/escrow/v1"
  }
}
```

| Param | Type | Rule |
|---|---|---|
| `quote.amount` | decimal string | `^(0|[1-9][0-9]*)(\.[0-9]{1,8})?$`, > 0 |
| `quote.currency` | string | ISO 4217 alphabetic code |
| `reviewWindowSeconds` | integer | 0 ≤ n ≤ 604800 (7 days, PRD §18) |
| `taskTimeoutSeconds` | integer | 1 ≤ n; the gate's `task.timeout` for this service |
| `rails` | array of strings | v4: `["stripe"]`. The slot reserves room for other rails (PRD §17). |
| `terms` | URL | The terms the buyer consents to |

JSON Schema: [`schema/ext-escrow.params.json`](schema/ext-escrow.params.json).

`required: true`: a `SendMessage` that does not activate the URI is refused with
`ExtensionSupportRequiredError` before any task exists. Activating the URI acknowledges the quote shown in
the card the caller fetched (PRD §5). It is not, by itself, approval to pay: **an API key or an MCP session
never approves a quote** (README, 25 Oct, "§12 adds"), so a task from such a principal is always `quoted`
until a person accepts it on the operator's page (D38).

A `hire` of the gate's own `/mcp` sends no `A2A-Extensions`: the gate activates the entry on the buyer's behalf, because
`hire` is this flow. Admission receives the URI in `extensions` and quotes; the quote is shown to the client as the
approve URL (URL elicitation, or the URL in the answer: [`relay.md`](relay.md) §12.3), the thread on the operator's
page, where a person accepts it. The service is not told of the activation (D300).

## 2. Admission and the hold

The operator's Admission hook ([`relay.md`](relay.md) §13.1) decides for each new task; it never moves
money, because nothing makes it atomic with the task's creation. So every paid task starts `quoted`:

1. Admission answers `quote` with an approve URL. The task is `quoted`: on A2A its state is
   `TASK_STATE_INPUT_REQUIRED` with `metadata["kontra.approve.url"]`; on MCP it is `working` with the URL
   in `statusMessage` ([`relay.md`](relay.md) §5.1, §12.3). Nothing is dispatched.
2. A person accepts on that page (Cloud: a confirmed session; a card checkout first when one is needed).
   The operator places the hold, then sends the `kontra.approval` Update with the metadata of §3
   (`kontra.escrow.status = "held"`). If the Update is refused because the quote expired or the task was
   cancelled meanwhile, the operator refunds the hold it just placed.
3. Decline or expiry cancels the task with nothing held.

A person hiring from the operator's own page accepts in the same gesture, so for them the quote is
accepted at once; an API key or an MCP session never accepts (§1).

The quote a task was admitted with is fixed for its life (`kontra.escrow.quote`); a later price change
does not touch tasks in flight.

## 3. Task metadata

Written only by the operator, through Admission decisions, `kontra.approval` and `kontra.annotate` Updates and
signals, and Lifecycle patches ([`relay.md`](relay.md) §13); never by a caller or a service, whose `kontra.` keys
the gate removes ([`relay.md`](relay.md) §2.5). The operator's gate config sets `operator_metadata_prefixes:
["kontra.escrow."]` and `caller_metadata: ["kontra.escrow.dispute"]`.

| Key | Value |
|---|---|
| `kontra.escrow.status` | `held` \| `released` \| `refunded` \| `disputed` \| `resolved` (a dispute settled by a split between the parties) |
| `kontra.escrow.quote` | The quote object at admission, so the task stays self-describing after a price change |
| `kontra.escrow.holdId` | Opaque hold reference |
| `kontra.escrow.reviewEndsAt` | RFC 3339 UTC; set when the task completes |
| `kontra.escrow.receipts` | Append-only array of `{event, rail, ref, at}`: `event` ∈ `held`, `released`, `refunded`, `disputed`, `resolved`, `chargeback`; `ref` the rail's object id; `at` RFC 3339 UTC. Appended by `metadata` events; the final status message is never rewritten. |
| `kontra.approve.url` | While `quoted`: the page where the person accepts the quote |
| `kontra.keyRetainUntil` | RFC 3339 UTC; set when the task completes to `reviewEndsAt`, so a hiring node keeps the job's key while a dispute may still need the output ([`relay.md`](relay.md) §13.3, [`tunnel.md`](tunnel.md) §2.3). Not under the escrow prefix: the gate admits it for every operator. |

JSON Schema: [`schema/ext-escrow.metadata.json`](schema/ext-escrow.metadata.json) (the operator's keys).

Amounts are decimal strings; durations are integer seconds; times are RFC 3339 UTC.

## 4. Lifecycle

| Task | Escrow |
|---|---|
| `quoted`, accepted | `held` (the hold is placed by the accept) |
| `quoted`, declined or expired | no hold ever existed; no escrow metadata but the quote |
| terminal `failed`, `rejected` or `canceled` | `refunded` |
| `completed` | `held`; `reviewEndsAt` set to completion + `reviewWindowSeconds` |
| review window passes without a dispute | `released` |
| dispute inside the window (§5) | `disputed`; the release timer stops; the operator decides: `refunded`, `released`, or `resolved` (a split), each with a receipt |

A refund happens only after a terminal event is in the task's log (the gate writes the terminal state
before it calls the hook), so cancel never races money.

## 5. Dispute

Buyer-written, A2A only: a message to the task, inside the review window, with
`metadata["kontra.escrow.dispute"] = {"reason": "<text, ≤ 2000 chars>"}`. The gate offers it to the operator's
`OnReviewMessage` hook through the `kontra.review_message` Update ([`relay.md`](relay.md) §7, §13.3); the hook
answers `dispute: true` and a patch setting the status to `disputed`, the gate appends the message to the task's
history and never forwards it to the service, and the release timer stops. A message the hook does not accept,
one outside the window, or one on a task without escrow is refused like any message to a terminal task
(`UnsupportedOperationError`), and nothing is logged.

JSON Schema for the message metadata: [`schema/ext-escrow.dispute.json`](schema/ext-escrow.dispute.json).

MCP has no in-protocol dispute: a task hired through MCP is disputed on the operator's page.

## 6. Metered prices (`per_call`, `per_session`)

Not escrow: nothing is held. The gate meters ([`relay.md`](relay.md) §11.4) and the operator invoices
(Cloud: monthly, through Stripe Billing; Kontra holds no balance). The price is advertised so a client
can show it before calling:

- **`per_call`:** on each entry of `tools/list`, `_meta["sh.kontra/price"]`:
  ```json
  {"model": "per_call", "amount": "0.002", "currency": "EUR", "freePerMonth": 100, "capPerMonth": "10.00"}
  ```
  Written by the operator's `tool_meta` annotation ([`relay.md`](relay.md) §13.4). A call is billable when its
  `calls` row has `is_error = false` (not a JSON-RPC error, not a top-level `isError: true`); rows with
  `is_error` null (passthrough task-augmented calls, [`relay.md`](relay.md) §11.5) are never billable.
- **`per_session`:** on the `initialize` result, `_meta["sh.kontra/price"]`, written by the operator's
  `initialize_meta` annotation:
  ```json
  {"model": "per_session", "amount": "0.50", "currency": "EUR", "idleTimeoutSeconds": 900}
  ```
  One billable session is one `mcp_sessions` row, from `created_at` to `closed_at`; `idleTimeoutSeconds` must
  equal the service's `session_idle_s`, which closes it ([`relay.md`](relay.md) §11.2).
- **`per_task` on MCP:** requires the service's `tasks: durable` (a priced passthrough call is refused,
  [`relay.md`](relay.md) §13.1). The operator renders its escrow state on `CreateTaskResult`, `tasks/get` and
  `tasks/result` through `mcpMeta` as `_meta["sh.kontra/escrow"] = {status, quote, holdId, reviewEndsAt,
  receipts}` (the fields of §3 without the `kontra.escrow.` prefix); the gate never builds it.

What a metered call costs depends on facts the service controls: the top-level `isError` it returns
decides whether a call is billable. A service that reports failures as successes bills its callers for
them; `capPerMonth` bounds that exposure before an invoice, and an operator can stop a listing whose error
rate does not match what callers report. The gate cannot do better without reading content it must not
read.

`freePerMonth` and `capPerMonth` are optional. Key names follow MCP's `_meta` rule (a reverse-DNS prefix
for third parties, D27). JSON Schema: [`schema/price.json`](schema/price.json).

## 7. AP2

The quote object is shaped to embed in an AP2 `CartMandate` later; a `PaymentMandate` a client provides in
message metadata (`ap2.PaymentMandate`) stays in that message, in the task's history, as evidence. Neither is
required in v4.

## 8. Named tests (Cloud)

| Test | Expected |
|---|---|
| `escrow_e2e` | Stripe test mode: quote → accept → hold (`held`); `completed`; review timer; `released`; the transfer is visible in the Connect dashboard; `kontra.escrow.receipts` complete |
| `escrow_dispute` | A dispute message inside the window: `disputed`, the timer stops, the operator's decision (refund, release or split) recorded with a receipt; a dispute after release is refused with nothing logged |
