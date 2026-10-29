# Kontra design documents

Three documents, written in order. The later one wins where they disagree. The PRD is the base;
the two architecture notes correct it after the prototypes of 21–24 October 2026.

| Document | Date | What it settles |
|---|---|---|
| [`prd-v4.md`](prd-v4.md) | 21 Oct | Kontra as a durable A2A gateway on a2a-go v2 and Temporal. Relay semantics (ids, attempts, stickiness, retries, cancel), sealing envelope, the three extensions, data model, security model, OSS vs Cloud, milestones M0–M5, acceptance tests, kill list for v3. |
| [`architecture/01-tunnel-and-protocols.html`](architecture/01-tunnel-and-protocols.html) | 24 Oct | **One node type**: no buyer sidecar and no seller sidecar, one `kontra connect` that hosts and hires. **Membership is which gate you dial.** One identity key per node, enrolled per gate, plus per-job keys. **MCP servers tunnel next to A2A agents**: `/mcp/{name}`, session affinity for plain calls, the same `TaskLifecycle` for task-augmented calls (MCP Tasks, 2025-11-25). Per-tool allow lists. No Kontra wire protocol; standard bindings only. `services:` replaces `agents:`. |
| [`architecture/02-deployment-topology.html`](architecture/02-deployment-topology.html) | 24 Oct | **Only the CLI runs on a user's machine.** Gates are clusters: `gateway.kontra.sh` (Cloud) or a team's own (Helm or a three-container compose). `kontra dev` is deleted; anonymous ephemeral sessions on the public gate are how people try it. One Go module, two binaries: `kontra` and `kontra-gate`. |

## Specifications (M0, 25 Oct; amended by each milestone)

The specifications in [`../spec/`](../spec/) are written from the three documents above and are later
than all of them: [`tunnel.md`](../spec/tunnel.md), [`relay.md`](../spec/relay.md),
[`ext-escrow.md`](../spec/ext-escrow.md), [`ext-sealed.md`](../spec/ext-sealed.md),
[`ext-input-schema.md`](../spec/ext-input-schema.md), and the schema, one file per milestone
([`0001_init.sql`](../migrations/0001_init.sql), [`0002_m3.sql`](../migrations/0002_m3.sql),
[`0003_m4.sql`](../migrations/0003_m4.sql)). Where a spec differs from a document, the
spec wins once the owner has signed it, and the difference is a numbered entry in
[`decisions.md`](decisions.md) with its reason and evidence. Each milestone's decisions are under its own heading
there (M4's, D268 to D298, were numbered after M3's review had merged).

## Section map: what the notes change in the PRD

| PRD section | Note 1 (tunnel and protocols) | Note 2 (deployment) |
|---|---|---|
| §3 Roles | Buyer/Seller are hats a node wears per request; add **Service** (A2A agent or MCP server behind a node) | — |
| §4 Architecture | Topology figure replaced | "One binary" → one module, two binaries |
| §6 Tunnel | `--to` → repeatable `--serve kind:name=target`; stdio bridge; `tunnel.key` → `identity.key` | — |
| §8 Sealing | Key table collapses to identity key + per-job key; MCP scope (`arguments`, `content`), both-ends-nodes condition | — |
| §9 Extensions | MCP mirrors: `_meta["kontra.escrow"]`, sealed as a server capability | — |
| §10 Data model | `agents` → `services(kind, …)`; `mcp_sessions`; `tasks.protocol` | Embedded-Temporal paragraph and localhost-only rule removed; anonymous tenants with expiry |
| §11 CLI | `kontra tunnel` + `kontra proxy` → `kontra connect`; `--console` on it | Two tables: `kontra` (connect, console, login, seal, publish) and `kontra-gate` (serve, enroll, migrate); `kontra dev` deleted |
| §12 Security | Tests `mcp_session_cross_node`, `mcp_tool_denied`, `mcp_task_resume` | Anonymous-session abuse controls, test `anon_limits` |
| §13 OSS vs Cloud | MCP hosting in both; listings gain `kind`; escrow only on task-shaped MCP calls | Cloud gains anonymous and free tiers; OSS gains Helm/compose team gate |
| §14 Milestones | — | M4 → anonymous sessions + team-gate compose (built: D268; the compose came with M3, D168) |
| §15 Acceptance | README demo gains a stdio MCP server through the gate | Demo loses its gate terminal; add "team gate from compose on a clean VPS" (the demo runs against the staging gate `gateway.kontra.run` until M5, with go install and a2a-cli's real syntax: D280, D298) |

## The compose team gate, the M3 pentest and the M4 gate runs

A team gate on one host is [`../compose.yaml`](../compose.yaml) (D43, D168): the gate built from the repository's
`Dockerfile`, terminating TLS itself, `temporalio/server` 1.32.1 with its two `admin-tools` jobs, and Postgres 16, with
no Temporal or Postgres port published. [`../deploy/compose/kontra.yaml`](../deploy/compose/kontra.yaml) is its
configuration with a provided certificate and says what to edit; [`../deploy/compose/kontra.vps.yaml`](../deploy/compose/kontra.vps.yaml)
is the same gate with built-in ACME, for a fresh VPS (D281, D298). PRD §12's pentest list and M3's own surface were
run against it on 27 Oct and, after the M3 review rounds, again on 29 Oct (`make pentest`, D186, D266):
[`pentest-m3.md`](pentest-m3.md) records every probe of the second run, the findings and their fixes.

M4's two gate runs are kits in this repository, run by hand (D282, D298): `scripts/demo-clean-laptop.sh` runs the
README's demo block, as written, in a fresh container against the staging gate, and `scripts/team-gate-vps.sh` brings
the ACME variant up on a throwaway DigitalOcean droplet, hires through it from another machine and destroys it (DNS
before the droplet; a listing that fails is never "nothing left"). Each prints its teardown before it starts; `test/demo`
and `test/vps` test both (`make test-kits` with Docker rehearses each against a local gate), and the runs are recorded
in `gate-runs-m4.md` beside this file.

## Hiring from another agent (decided 25 Oct)

The gate serves its own MCP endpoint, `https://gateway.kontra.sh/mcp`, as a first-party remote MCP
server: `search`, `describe`, `hire` (task-augmented), `reply`, plus MCP Tasks. An MCP client's
session is the agent's and can never approve money. `hire` creates the task in a pre-hold state
`quoted`, posts the quote card into the task's thread, and answers with a URL-mode elicitation
(MCP 2025-11-25; the URL in the tool result text for older clients) whose URL is the thread itself,
`app.kontra.sh/#/t/<taskId>` (D188). The person accepts the quote there; the hold follows. Decline or quote
expiry cancels with nothing held. Thread SSE and the client's `tasks/get` / `tasks/result` read the
same `task_events`, so the task can be followed and answered from either side. A2A-native buyer
agents get the same thread URL in `input-required` metadata (`kontra.approve.url`).

PRD deltas: §7 gains the `quoted` state before `submitted`; §9 names `kontra.approve.url`; §11's
gate table gains the `/mcp` endpoint; §12 adds that an MCP or API-key session never approves a
quote. Full spec: kontra-cloud#59. Built in M4 as [`relay.md`](../spec/relay.md) §12 in both MCP eras (D269 to D274,
D288, D289, D293 to D295); the page on which a person accepts a quote is Cloud's, M5.

## Pricing (decided 25 Oct, in the Cloud repository's console issue; recorded here for the extension specs)

Three price shapes on a listing, matching the three request shapes the gate distinguishes:

- `per_task` — A2A tasks and task-augmented MCP calls. Quote held at admission, released after the
  review window, refunded on dispute or failure. The escrow extension, unchanged.
- `per_call` — plain MCP `tools/call`, gate-metered on non-error results (tool name, bytes,
  duration and `isError` are outside the sealed content). Prices per tool, attached to
  `tools/list` as `_meta["kontra.price"]`. Invoiced monthly through Stripe Billing; Kontra holds no
  balance.
- `per_session` — per `Mcp-Session-Id`, metered and invoiced the same way.

Open: whether the gate should fabricate task-augmented calls over a plain upstream so every tool
call is durable (default: no, mirror the upstream); session affinity on tunnel loss (default: 404,
client re-initializes); gRPC through the tunnel (deferred).
