# Decisions

Every place where the specs (`spec/`) or the code depart from, correct, or settle something the design
documents left open (`docs/prd-v4.md`, `docs/architecture/01-*.html`, `02-*.html`, `docs/README.md`).
Numbered once, never renumbered; a reversed decision gets a new entry that says so.

Each entry: what the documents say, what was decided, why, and the evidence. **Needs the owner** marks
the ones that change a documented decision or a test's expected result; they are listed in the M0 PR
for sign-off. The evidence was gathered on 25 Oct 2026 against the pinned versions in
[`spec/relay.md`](../spec/relay.md) §0 and [`spec/tunnel.md`](../spec/tunnel.md) §0.

## Protocol facts the documents got slightly wrong

### D1. A2A 1.0 has eleven operations, not twelve

PRD §5 says "all 12 v1.0 operations". Spec v1.0.1 §3.1.1–3.1.11 and the proto service define eleven RPCs;
the twelfth is the agent-card GET, which is not an operation. The gate serves all eleven plus the card.
Also renamed to the 1.0 terms: `securityRequirements` (not `security`), `CreateTaskPushNotificationConfig`
(not `SetTaskPushNotificationConfig`), part `filename` (not `name`), `returnImmediately` (not `blocking`).

### D2. `Last-Event-ID` resume is the gate's own SSE, not a2a-go's — **accepted by the owner, 25 Oct**

PRD §5/§15 and note 1 §3: `SubscribeToTask` replays from `Last-Event-ID` (the event `seq`).
A2A 1.0.1 does not define `Last-Event-ID` at all, and a2a-go v2.7.0 writes a random UUID as every SSE id
and never reads the header (`internal/sse/sse.go:78`; probe P6). Decision: the gate writes its own SSE for
`SendStreamingMessage` and `SubscribeToTask` on both bindings, fed from `task_events`, with `id: <seq>`.
Resume with `Last-Event-ID: K` sends the snapshot **as of K** (folded from events 1..K, without an id),
then K+1..N, then the tail ([`relay.md`](../spec/relay.md) §9). That keeps the PRD's "snapshot then events
from seq+1" and also A2A's generation-order rule, which a current snapshot followed by older events would
break. A resubscribe without the header follows A2A: a terminal task is `UnsupportedOperationError` (TCK
`STREAM-SUB-003`, MUST), and the stream stays open through `input-required` (TCK `STREAM-SUB-002`), which
a2a-go's own writer does not do.

### D3. Caller-supplied context ids are kept, scoped per buyer

PRD §7: "The gate mints `task_id` and `context_id`". Spec §3.4.1 requires a server to preserve a
client-supplied `contextId` or reject it, never replace it (TCK `CORE-MULTI-002`). Decision: the gate mints
task ids always and context ids only when the caller gives none; contexts are keyed by `(tenant, service,
buyer, context_id)`, so two buyers' identical strings never meet ([`relay.md`](../spec/relay.md) §4).

## Relay on a2a-go v2.7.0

The PRD (§18, first risk) asked to decide early whether a2a-go's executor shape fights a relay. It does
not, fatally; it fights it in the places below, each with a local fix verified by probes P1–P14 (research
brief, `a2a-go-probes`). Kontra stays on `a2asrv.NewHandler` + `WithClusterMode` and implements
`taskstore.Store`, `eventqueue.Puller` and a Temporal-backed `workqueue.Queue` as the PRD's correction
paragraph says ([`relay.md`](../spec/relay.md) §15).

### D4. Cancel is decided before a2a-go's executor sees it

PRD §7: an agent that does not support cancel → the gate returns the error and the task continues.
a2a-go marks the task `failed` whenever `AgentExecutor.Cancel` returns an error (`a2asrv/agentexec.go:515`;
P2a), and a successful cancel does not stop the running execution (P2b). Decision: a call interceptor
handles `CancelTask`: it forwards upstream through `forward_cancel` and returns the upstream's answer
without entering a2a-go's cancel path ([`relay.md`](../spec/relay.md) §8).

### D5. An upstream `Message` reply becomes a completed task

In cluster mode a bare `Message` never reaches the caller and `SendMessage` hangs (P1). Decision: the
relay converts it to a terminal `completed` status whose message is the upstream's. The caller always gets
a task ([`relay.md`](../spec/relay.md) §6.3).

### D6. The work queue's payload stays out of Temporal, by reference

PRD §10: events never enter Temporal history. a2a-go's `workqueue.Payload` carries the whole request
**and every inbound header, `Authorization` and cookies included** (`a2asrv/ctxcodec.go:54`; P8). Starting
or signalling `TaskLifecycle` with it would put bearer tokens and message content into history. Decision:
`Write` stores a sanitized copy in Postgres (`work_items`: headers reduced to `A2A-Version` and
`A2A-Extensions`) and passes only its id ([`relay.md`](../spec/relay.md) §8.1).

### D7. a2a-go is pinned to v2.7.0 and re-probed before any bump

v2.7.0 is the first release that rejects requests without `A2A-Version`, negotiates
`application/a2a+json` and validates input modes, all of which change gate behaviour. The probes
(`examples/` will carry them as tests at M1) are re-run before moving past it. a2a-go needs Go ≥ 1.26.

Revision 3 (M1 planning): the probes P1–P14 are carried as contract tests next to the code that depends on them
(`internal/relay`), run by `make check`, not under `examples/`, which holds sellers, MCP servers and front proxies.

### D8. Every terminal outcome is an explicit event; retries survive a2a-go's error path

On an executor error a2a-go writes `failed` with no message, on an already-cancelled context; a store
that honours the context drops the write and the caller hangs (`internal/taskexec/execution_handler.go:43`;
P10b). Decision: the relay never relies on an error to fail a task — it writes explicit terminal events
with the upstream's message; store writes use `context.WithoutCancel`; and while a dispatch for the task is
registered, in every phase of the attempt, the store refuses any `failed` write the relay did not emit (a2a-go's
own `SetTaskFailed`), the dispatch returns its outcome from a per-process registry, and the workflow writes the
terminal event through `write_terminal`, so it can also start the next attempt or re-attach ([`relay.md`](../spec/relay.md) §6.3, §15).

### D9. Version and extension headers are normalized in front of a2a-go

a2a-go does not split a comma-separated `A2A-Extensions` (so a list fails required-extension checks),
ignores `X-A2A-Extensions`, never echoes activated URIs, and rejects `A2A-Version: 1.0.1`. Spec v1.0.1 says
patch versions must not affect negotiation and the header is comma-separated. Decision: middleware
normalizes both before a2a-go and echoes the activated URIs. The version is the `A2A-Version` header, else the
`A2A-Version` query parameter (spec §3.6.1, as a2a-go accepts); only when both are absent or empty is the request
0.3, refused with `VersionNotSupportedError` (§3.6.2; PRD §18: serve 1.0 only downstream)
([`relay.md`](../spec/relay.md) §2.4).

### D10. The card is also served at `GET {service URL}`

a2a-go's resolver fetches a non-root URL as the card itself, so a buyer resolving
`https://gate/agents/hello` got an empty card (P13), and `a2a send -a https://gate/agents/hello` failed.
Decision: `GET /agents/{name}` returns the card, as `/.well-known/agent-card.json` does
([`relay.md`](../spec/relay.md) §2.1).

### D11. Cards are rewritten as raw JSON

PRD §5: every seller-authored field kept. Any card that passes through `a2a.AgentCard` (including
`NewSignedCardProducer` and the compat parser) loses fields a2a-go does not know (P11). Decision: the gate
rewrites the card as raw JSON and signs it with its own JWS code over a vetted JCS library (a2a-go's
`a2acrypto.Signer` cannot add the protected-header parameter of D13) ([`relay.md`](../spec/relay.md) §3).

### D12. 0.2.x cards are normalized by the gate

PRD §5: pre-1.0 cards are normalized through a2a-go's compat layer. a2acompat handles 0.3 only; a 0.2 card
without `preferredTransport` yields no interfaces (P11), and 0.2.5 sellers serve their card at
`/.well-known/agent.json`, which a2a-go's resolver never tries. Decision: the gate fetches `agent.json` as a
fallback and converts a 0.2.x or 0.3 card to the 1.0 shape as raw JSON (D11); a2a-go's `a2av0` parser only reads
what dispatch needs, never the served card (`card_compat` keeps its expected result) ([`relay.md`](../spec/relay.md) §3).

### D13. The seller's signature moves to a bound upstream card — **accepted by the owner, 25 Oct**

PRD §5 and §15 `card_passthrough`: the served card keeps the seller's signature beside the gate's ("two
signatures"). A JWS over the upstream card cannot verify over the rewritten card (spec §8.4.3); a strict
verifier would then reject the whole card, and a lenient one (a2a-go accepts any one valid signature)
learns nothing from it. Decision: the served card carries one signature, the gate's, whose protected
header binds `kontra.upstream: {sha256, url}`; the upstream card is served verbatim, seller signature
intact, at `{svc}/.well-known/agent-card.upstream.json`. `card_passthrough` then expects one signature
plus a verifiable upstream card, with seller fields byte-identical under JCS. Revision 2 (review): the gate
verifies seller signatures only against keys the operator configured for the service (never a `jku` from a
card), says in the protected header whether it did (`verified`), signs with its own JWS code (a2a-go's signer
has a fixed protected header), and a node checks the gate's `jku` against the origin it dialed; card stripping
by an active gate is caught only by a node that pinned the seller's keys ([`relay.md`](../spec/relay.md) §3, §16).

### D14. No gate-side input-mode enforcement

a2a-go v2.7.0's card enforcer rejects parts whose media type the card does not list: it would refuse every
sealed part (the envelope type is not in a seller's modes) and every `text/plain` part to the README demo
seller, whose card declares `defaultInputModes: ["text"]`. The gate relays parts without interpreting them;
the upstream enforces its own modes ([`relay.md`](../spec/relay.md) §3).

### D15. Dispatch falls back from streaming to polling; the demo commands follow a2a-cli v0.3.1 — **accepted by the owner, 25 Oct** (README demo)

PRD §7: `SendStreamingMessage` when the card says `streaming: true`. The demo seller is a2a-cli v0.3.1's
exec mode, whose card always says `streaming: true` and which refuses streaming unless `--chunk` is given.
Also, the PRD's and notes' commands do not exist in that CLI: the subcommand is `server` (not `serve`),
`send` needs `-a <card URL>` or `-e <endpoint>`, and there is no `-H` (it is `--auth`). Decision: dispatch
tries streaming and, on `UnsupportedOperationError` before any event, uses `SendMessage` with
`returnImmediately: true` and polls `GetTask` (which is also what lets a cancel find an upstream task early).
The README demo uses the real syntax:

```sh
a2a server --exec 'echo Hello World' --port 9999
kontra connect --serve a2a:hello=http://localhost:9999
a2a send -a http://localhost:7777/agents/anon-7f3a/hello "hi"
```

`input_required_sticky` needs a seller that asks for input, which exec mode cannot do: `examples/sellers/booking`.

### D16. Push is one long activity per config, not one activity per event

PRD §7/§10: the workflow schedules `push_webhook(task_id, seq)` per event. The workflow would have to learn
of every event (one signal each: hundreds per streaming task in history), and a2a-go's own push is inline
on the relaying replica (C6). Decision: one `push_webhook(task_id, config_id)` activity per config tails the
log from its last delivered seq (kept in heartbeat details and `push_configs.delivered_seq`), delivers the
snapshot at each seq in order, may coalesce runs of artifact-only events, 10 s per HTTP attempt, 24 h
retry budget per seq ([`relay.md`](../spec/relay.md) §10.2). History stays one activity per config.

### D17. The gate's own SSRF-safe dialer

PRD §12 relies on a2a-go's SSRF-aware push sending. It does not block CGNAT (`100.64.0.0/10`) and honours
the proxy from the environment (`a2asrv/push/sender.go:120,134`). Decision: one dialer for push and direct
backends: resolve once, pin, refuse private/loopback/link-local/ULA/CGNAT/metadata, no redirects, no env
proxy ([`relay.md`](../spec/relay.md) §10.3).

### D18. The TCK pin, its expected failures, and what the TCK needs from the gate

The pinned TCK (`d4d6fae`, `1.0.0.alpha3` + #250) vendors spec v1.0.0, sends no credentials and no
`A2A-Extensions`, posts JSON-RPC to the interface URL plus a trailing slash, uses POST for REST
`:subscribe`, hard-fails missing card cache headers, and fails `200` SSE responses that carry an error.
Decision: the gate serves the trailing-slash route and both verbs, sends `Cache-Control`/`ETag`/
`Last-Modified` on cards, validates before writing SSE headers; the TCK target service opts in to the
`anonymous` principal and `push.allow_private` in its test config; the TCK seller is an a2a-go example
(`examples/sellers/tck`). The TCK has no expected-failures mechanism: a 35-line pytest plugin marks listed
node ids `xfail(strict=True)` (a listed test that passes fails the run). The seed list is the five TCK-bug
entries (snake_case push/historyLength, the inverted media-type assertion, the v1.0.0 409-vs-400 cancel
code), each with its upstream issue; re-pin when a2a-tck #232, #200 and #197 merge.

Revision 3 (M1 planning): the TCK registers plain-HTTP webhooks on loopback, so `push.allow_private` must also
permit `http://`, which D67 adds ([`relay.md`](../spec/relay.md) §10.3).

### D19. The MCP relay is raw JSON

go-sdk v1.8.0's types drop `Tool.execution` and reject `tasks/*`, so any message decoded and re-encoded
through it loses the task-support declaration. Decision: the MCP relay reads and writes raw JSON;
go-sdk is used at most for the gate's own `/mcp` tools and resource-server auth helpers
([`relay.md`](../spec/relay.md) §11).

### D20. Two new principal kinds in `allow:`

PRD §12 principals: `key:`, `org:`, `oidc:sub:`. Hiring through a node and the TCK need two more:
`node:<key_id>` (a node hiring without an API key) and `anonymous` (unauthenticated callers, opt-in per
service, never implied). Consequence, accepted: all anonymous callers of a team gate (and of one tenant on a
multi-tenant gate) share one buyer, so their tasks are readable and cancellable by anyone holding the task id;
`ListTasks`, `tasks/list` and push configs are refused for them ([`relay.md`](../spec/relay.md) §2.3).
Amended by D301 (`oidc:*`, every principal of the configured OIDC issuer).

## MCP

### D21. `quoted` shows as `working` to MCP clients; `-32042` only for clients that declared URL elicitation — **accepted by the owner, 25 Oct**

README ("Hiring from another agent"): `hire` answers with a URL-mode elicitation, the URL in the tool
result text for older clients. Claude Code cancels a task that requests input when it is not interactive
(its docs, "MCP tasks"), so an `input_required` approval would be declined by accident in headless runs.
Decision: `hire` answers `-32042` with the URL only to clients that declared `elicitation.url`, and retries
of the same `hire` return the existing task; to others it answers at once, the task `working` with the URL
in `statusMessage` and `_meta["sh.kontra/approve-url"]`. On A2A, `quoted` stays `input-required` with
`kontra.approve.url` as the README says ([`relay.md`](../spec/relay.md) §5.1, §12.3).

### D22. A durable MCP task's dispatch opens its own upstream session

Note 1 §5: the gate pins the client's session to a tunnel session. Upstream task stores are often scoped
to the session that created the task (`server-everything@1.0.0` builds a task store per session), so a
client's `DELETE`, idle expiry or re-initialize would destroy a gate-owned task's upstream half. Decision:
for `tasks: durable`, the dispatch activity initializes and owns an upstream session for the attempt,
relays the upstream's input requests through the log, and stores the upstream result as soon as the task
is terminal ([`relay.md`](../spec/relay.md) §11.5).

### D23. The 2024-11-05 HTTP+SSE transport is not carried in v4.0 — **accepted by the owner, 25 Oct**

Note 1 §6 lists "legacy HTTP+SSE for 2024-11 clients" in the v4 HTTP binding. It doubles the session code
for clients that have had Streamable HTTP for eighteen months, and 2026-07-28 deprecates it. Decision: not
served to clients nor spoken to upstreams in v4.0; a client's legacy probe gets `405`
([`relay.md`](../spec/relay.md) §11.11).

### D24. MCP 2026-07-28: both revisions on the same endpoints — **decided by the owner, 25 Oct**

The goal and the notes pin MCP 2025-11-25. Revision 2026-07-28 was released on 28 Jul 2026 and removes
`initialize`, `Mcp-Session-Id`, the `GET` stream, SSE resumability, the 2025-11-25 Tasks (moved to the
`io.modelcontextprotocol/tasks` extension, SEP-2663, not wire-compatible) and `-32042`. Claude Code — the
only stock client that does MCP tasks — does them only on 2026-07-28, and
`server-everything@2.0.0` / `server-filesystem@2.0.0` are dual-era. Under the pin, session affinity,
`mcp_sessions`, `mcp_session_cross_node` and `per_session` pricing work, but no shipping client uses the
durable task path. Proposed: serve both eras on the same endpoints (2026-07-28 explicitly allows a
dual-era server), with the tasks extension as the durable surface for modern clients (`tasks/get` carries
`inputRequests` and `result`; `tasks/update` becomes a workflow signal), the same `TaskLifecycle` and log
behind both, and `per_session` redefined for modern clients (per principal per service per period). The
specs stay on 2025-11-25 until the owner decides ([`relay.md`](../spec/relay.md) §11.12).

Decided by the owner (25 Oct): serve both revisions on the same endpoints, as proposed. The 2026-07-28 text
(the tasks extension as the durable surface, `tasks/update` as a workflow signal, `per_session` for modern
clients) is written into relay.md §11 before M3 builds the MCP relay. Until then §11 describes 2025-11-25.

### D25. The gate's `/mcp` is an OAuth resource server; the authorization server is the operator's

README: a stock client adds `gateway.kontra.sh/mcp` as a connector. claude.ai/Desktop connectors need
OAuth (static headers are a limited beta); MCP authorization makes the server an OAuth 2.1 resource server
with RFC 9728 metadata, and forbids passing tokens through. Decision: the gate implements the resource
server side (401 + `WWW-Authenticate` + protected-resource metadata, audience-checked JWTs from the
configured issuer) and keeps accepting `kk_` API keys; the authorization server (CIMD and DCR for Claude)
is the operator's — Cloud runs one, a team gate can point at its IdP ([`relay.md`](../spec/relay.md) §12.1).

## Hooks

### D26. Cloud fills the hooks by configuration, not by building another binary — **accepted by the owner, 25 Oct**

PRD §13: Cloud is "the same binary with two hook interfaces filled in" and the hooks are "interfaces the
Cloud build fills"; PRD §18: "no public Go API from kontra in v4"; Cloud's settlement is Python. A Go
`internal/` package cannot be imported by another module, so a Cloud Go build would need a public API, and
it would not reuse Cloud's existing Python Stripe code. Decision: each hook has a built-in implementation
(open source: allow lists, no-ops) and a remote one selected in `kontra.yaml`: **Admission** is an HTTPS
call to the operator's control plane (fails closed), **Lifecycle** is five activities by type name on the
operator's Temporal queue (Cloud: `kontra-settlement`, Python workers, as PRD §10 already says), and the
operator answers through the Updates `kontra.approval` and `kontra.annotate` and the signal `kontra.close`. The hooks gain what the README's pricing and quote flow need: an Admission `operation`
(`task`, `session`, `call`), a `quote` outcome that creates the task `quoted`, and metadata patches as hook
results — so every write to a task still goes through the gate's store. Service annotations carry the
operator's card extensions, `_meta` prices and search strings, uninterpreted. Metering is not a hook: the
gate writes `calls` rows (no price) that the operator reads. Revision 2 (review): Temporal authorizes by namespace,
not by queue or Update name, so any operator credential for the gate's namespace is equivalent to the gate's own
authority over every task; it is held only by the operator's workers and control-plane backend, as one trust
domain with the gate. Moving the remote hooks to HTTPS callbacks was considered and not taken: PRD §10 already
puts Cloud's settlement activities on `kontra-settlement` in that namespace ([`relay.md`](../spec/relay.md) §13,
§14.4).

### D27. MCP `_meta` keys are `sh.kontra/…` — **accepted by the owner, 25 Oct** (names in kontra-cloud#59 too)

README/#59: `_meta["kontra.price"]`, `_meta["kontra.escrow"]`. Legal (an unprefixed name), but MCP keeps the
unprefixed space for its own keys and asks third parties for a reverse-DNS prefix. Decision:
`sh.kontra/price`, `sh.kontra/escrow`, `sh.kontra/approve-url`, `sh.kontra/sealed` (the sealed capability
under `capabilities.experimental`, since 2025-11-25 has no `extensions`). A2A metadata keys stay
`kontra.*` as the PRD has them ([`ext-escrow.md`](../spec/ext-escrow.md) §6, [`ext-sealed.md`](../spec/ext-sealed.md) §1).

## Schema and tooling

### D28. One file per migration, applied with goose — **accepted by the owner, 25 Oct** (PRD §17 default)

PRD §17 default: golang-migrate; M0 deliverable: `migrations/0001_init.sql`. golang-migrate needs separate
`.up.sql`/`.down.sql` files (and silently skips any other name: a directory holding only `0001_init.sql`
applies nothing). Decision: `pressly/goose`'s single-file format (`-- +goose Up` / `-- +goose Down`),
embedded in `kontra-gate migrate`, which creates the schema `kontra` itself before goose runs and names
goose's table `kontra.goose_db_version` (goose creates its table before any migration, so the schema cannot
be created inside 0001 alone). Schema changes from PRD §10, all following from the notes and the
specs: `tenants` (anonymous ones expire); `agents` → `services(kind, …)` with `tools`, `tasks_mode`,
`annotations`, `listed`; `key_services` (a key hosts several services) and `enrollment_tokens`;
`gate_replicas`; `tasks.protocol`, `tasks.state` including `quoted`, `tasks.context_id` as text (D3);
`work_items` (D6); `push_configs` (A2A 1.0 allows several per task); `mcp_sessions`; `calls`
([`migrations/0001_init.sql`](../migrations/0001_init.sql)).

## Tunnel and sealing

### D29. The identity key file is an age identity file

PRD §17 default kept: one X25519 key for the handshake and for sealing. Settled: the file is age's own
identity-file format (`age -d -i` works on it); the first line is active, later lines decrypt-only during
rotation; `key_id = "k1_" + base32(SHA-256(recipient))[:26]`; per-job keys under
`~/.kontra/jobs/<gate host>/` ([`tunnel.md`](../spec/tunnel.md) §2).

### D30. Possession is proved with an HPKE export, not a signature — **accepted by the owner, 25 Oct**

PRD §6: "signing the gate's nonce (X25519 key converted for signing via the age/ed25519 derivation, or a
paired Ed25519 key)"; note 1's figure: `sig(nonce, identity key)`. X25519 cannot sign; age only converts
Ed25519 → X25519, not the other way; there is no maintained Go XEdDSA; a paired Ed25519 key would not prove
possession of the published X25519 recipient. Also, a proof over a bare nonce can be relayed by a gate the
node dialed to another gate where the same key is enrolled. Decision: the gate encapsulates to the node's
key with HPKE (RFC 9180, Go 1.26 `crypto/hpke`, export-only) over a transcript binding the origin the node
dialed, the TLS exporter when the gate terminates TLS, the node's hello and both nonces; the node returns
the exported secret ([`tunnel.md`](../spec/tunnel.md) §4.1). The PQ variant is the same code with
`MLKEM768X25519`.

### D31. What sealing protects is stated narrowly, and the header binds more — **accepted by the owner, 25 Oct**

PRD §12: "Dishonest or compromised gate: ciphertext end to end". The gate serves the card that carries the
recipients, and age does not authenticate senders, so an active gate can substitute its own recipient.
Also, the first message of a new task cannot carry the gate's task id (it does not exist yet), so the
PRD's header (`taskId`, `contextId`, `kids`, `mediaType`) cannot bind it. Decision: the header also binds
`dir`, `messageId`/`artifactId`, `part`, `chunk`, `kind`, and on requests `replyTo` and a reply key `rk`,
and replies carry an HMAC under `rk`; the hosting node replies only to the `replyTo` inside the sealed
request. The guarantee is stated as: confidentiality against an honest-but-curious or breached gate always,
against an active gate only with pinned recipients (`kontra connect` records, shows and can pin them).
Revision 2 (review) — **changes `sealed_replay`'s expected result**: requests also carry `iat` and a part count;
the hosting node keeps durable job state (every `replyTo` that opened a job, the job's `rk`, the upstream ids,
the forwarded `(messageId, part)` pairs), so a first message replayed into a new task is refused on that node
(`replayed`) and a follow-up into another job is refused (`wrong_job`). The stated residual: a gate can deliver a
replayed first message to another hosting node of the service within the node's `max_age` (default 25 h, which
must cover quote approval and the task timeout), and that node runs it once (the reply
still goes to the original buyer's key). Pins also cover whether the service is sealed and whether its upstream
card was signed, and a downgrade is always refused ([`ext-sealed.md`](../spec/ext-sealed.md) §3, §5, §7, §9).

### D32. `mcp_task_resume` keeps the "after the first upstream id, tunnel loss fails" rule

Note 1 §7: a replica killed during a task-augmented call; `tasks/result` still returns. If the killed
replica held the tunnel and the upstream task existed, the PRD's rule (and the goal's) fails the task.
Decision: the rule stands for MCP as for A2A; `mcp_task_resume` asserts that `tasks/result` returns on the
other replica — with the result when the killed replica only held the client's connection, with
`failed: tunnel_lost` when it held the tunnel — and never hangs ([`relay.md`](../spec/relay.md) §11.9, §16).
Open for the owner: front proxies, cloud load balancers and gate deploys cut WebSockets routinely, so
long tasks will meet a tunnel loss. Re-attaching a task when the **same key** reconnects within a grace
window would recover upstream tasks that are still running on the node without any double work; it would
change the rule, so it is not built.

Decided by the owner (25 Oct): the rule stands. Re-attach on reconnect is not built.

### D33. Liveness is the gate's and the node's own ping loop

PRD §6: ping every 20 s from the node, pong from the gate, three misses close. yamux's keepalive closes on
the first missed pong and gives the receiving side no hook, so a gate that only answers pings never learns
a node went silent. Decision: yamux keepalive off; both sides `Ping()` every 20 s and close after three
consecutive failures; `tunnels.last_ping_at` written every 60 s, not per ping ([`tunnel.md`](../spec/tunnel.md) §7, §11).

### D34. Hiring rides streams the node opens

Note 1 §1: the node calls out "over that single connection". Settled: the node serves `127.0.0.1:7777`
and forwards on streams it opens; the gate serves them with the same handler as network requests, with the
session's principal (the API key from the handshake, else `node:<key_id>` with the key's tenant, anonymous
sessions included; relay.md §2.3)
and the session's limits; the node rewrites card URLs to localhost after verifying the gate's signature
([`tunnel.md`](../spec/tunnel.md) §9).

### D35. Graceful drain is make-before-break over a control stream

yamux does not wake `Accept` on a remote GoAway. Decision: the first gate-opened stream is a control
stream; `drain` makes the node dial a new session before the old one closes, and the draining replica keeps
its per-replica Temporal worker polling until its dispatches end or `drain_timeout` passes, so rolling
deploys do not cut tasks that finish within `drain_timeout` (Kubernetes grace ≥ `drain_timeout` + 30 s)
([`tunnel.md`](../spec/tunnel.md) §13, [`relay.md`](../spec/relay.md) §14.3).

### D36. The Kubernetes example is not ingress-nginx

PRD §4 and note 2 list an ingress example. ingress-nginx was retired in March 2026, and an L7 proxy that
terminates TLS also removes the tunnel's channel binding. Decision: `examples/front/ingress` shows an L4
load balancer with TLS at the gate, and a Gateway API route with hour-long WebSocket timeouts.

## From the kontra-cloud survey

### D37. The gate's tables live in their own Postgres schema, `kontra`

PRD §10: Kontra's tables and Cloud's in one Postgres. Cloud already has an `api_keys` table (different
columns), and its Alembic test compares the whole default schema with its own metadata, so the gate's
tables would fail it. Decision: every gate table is in schema `kontra` (goose's version table too); Cloud
keeps its own. The schema targets Postgres 16 or later, because Cloud runs 16 (CloudNativePG); `make
check` applies it on 16 and 17. The API-key prefix check accepts an operator's key format, not only `kk_`
([`migrations/0001_init.sql`](../migrations/0001_init.sql)).

### D38. Activating the escrow extension acknowledges the quote; only a person approves it — **accepted by the owner, 25 Oct** (amends PRD §5/§9)

PRD §5/§9: activating the escrow URI on `SendMessage` is the buyer's consent to the quote. README (25 Oct,
later): an MCP or API-key session never approves a quote. Decision: the README rule, written into the
extension spec — a task from an API key or an MCP session is always `quoted` until a person accepts on the
operator's page, and (D45) every paid task, a person's included, is `quoted` first and held on acceptance
([`ext-escrow.md`](../spec/ext-escrow.md) §1, §2). Consequence for A2A buyer agents: a paid task always
waits for a person once.

### D39. A service's owner may see the tasks of its own services — **accepted by the owner, 25 Oct** (refines PRD §12)

PRD §10/§12: every query keyed by `(tenant, agent, buyer)`, no cross-buyer list. kontra-cloud#59's Tasks view
lists "served by my nodes", which is across buyers by construction. Decision: no protocol surface ever
lists across buyers; the operator's own API may show a tenant the tasks and calls of the services it owns
(metadata, never content) ([`relay.md`](../spec/relay.md) §13.5).

### D40. An operator may issue tenant-wide enrollment tokens — **accepted by the owner, 25 Oct** (weakens PRD §12)

PRD §12: an enrollment token is bound to one agent. kontra-cloud#59: "Add a node" mints one token bound to
the party, pasted with `kontra connect --enroll … --serve …`. Decision: a token is bound to one service, or,
when the operator issues it so, to the tenant, enrolling the key for the services the node declares
(single use and 15 minutes either way; `enrollment_tokens.scope` says which, never a bare null).
Revision 2 (review): a tenant token is minted with the list of service names it may bind and never adds a key to
a service that already has one (adding a replica, and so a sealing recipient, needs a service token or an API key
with an `enroll:` scope, and is announced to the service's other nodes).
`kontra-gate enroll <service>` on a team gate stays per service
([`tunnel.md`](../spec/tunnel.md) §5).

## From the Temporal research (server 1.32.1, Go SDK v1.50.0, measured)

### D41. Follow-ups, cancel, approval and dispute are Temporal Updates, not signals

Spec drafts and PRD §10 had signals. A signal cannot be refused: two follow-ups racing on two replicas would
both be accepted, and the second caller would receive the first turn's answer; a cancel could not return the
upstream's answer; a control plane could not learn whether its approval beat the quote expiry. Decision:
`kontra.message` (the first one via `UpdateWithStart`), `kontra.cancel`, `kontra.approval`, `kontra.annotate`
and `kontra.review_message` are Updates whose validators apply the relay's rules; refused Updates leave no
history. `close`, `input_response`, `dispatched` and `push` stay signals ([`relay.md`](../spec/relay.md) §14.1).

### D42. A direct backend is re-attached after a gate replica dies — **accepted by the owner, 25 Oct** (M1's gate)

PRD M1's gate: kill a gate replica mid-task, the task completes. The retry table would fail the task once
an upstream task exists. For a direct backend the upstream is untouched by a gate replica's death, so a new
dispatch of the **same** attempt follows the existing upstream task by id (no new `SendMessage`, so no
double work, and the "retries only before the first upstream id" rule still holds). Tunnel backends keep
`tunnel_lost`. `replica_kill` has a direct variant (M1: completes) and a tunnel variant (PRD §15: retried or
failed) ([`relay.md`](../spec/relay.md) §6.4, §16).

Revision 3 (M1 planning): re-attach is bounded. A handler or executor error that is not a lost upstream context
(a2a-go refusing a parallel active execution, a validation failure) is `upstream_error`, not a context loss. Resume
dispatches of one attempt back off, 1 s doubling to 30 s, and are capped at 10: when an 11th would be needed, the
task fails `upstream_error` with the message `re-attach limit reached`. The 30 s `GetTask` rule and the task
deadline still apply, but the deadline alone does not bound the loop: `task.timeout` has no upper limit, and a
resume every 30 s would fill a long task's history ([`relay.md`](../spec/relay.md) §6.4, §15).

### D43. The team-gate compose uses the supported Temporal images

Note 2's compose names `temporalio/auto-setup`, which is deprecated and frozen at 1.29.x (no 1.30+ tag).
Decision (M4): three long-running containers as note 2 says — the gate, `temporalio/server:1.32.1` (with a
mounted dynamic-config file, without which it does not start) and Postgres — plus two one-shot
`temporalio/admin-tools:1.32.1` jobs (schema, namespace); Temporal's port is not published. Postgres 16 for
the compose (Temporal tests 13–16; Cloud runs 16); the gate's schema runs on 16 and 17 (D37).

### D44. A cancel before any upstream task aborts the dispatch in process

Spec draft §8: "cancels the dispatch activity (Temporal activity cancellation)". Temporal delivers that
cancellation only in a heartbeat response, up to the heartbeat throttle later (measured 22 s late with the
draft's settings; never, for an activity that does not heartbeat). Decision: an `abort` activity on the
attempt's replica's control queue (`kontra-ctl-<replica_id>`, with its own worker slots) cancels the dispatch's
context through the replica's registry; the dispatch runs with
`WaitForCancellation`, so `canceled` is written only after it stopped; the gate's Temporal server enables
worker commands as a second path, and dispatch workers cap the heartbeat throttle at 10 s
([`relay.md`](../spec/relay.md) §8, §14.2).

### D45. Admission never moves money; every paid task is quoted first — **accepted by the owner, 25 Oct** (amends PRD §13)

PRD §13: Cloud's `Admit` "places the hold". Nothing makes the hook atomic with the task's creation: a gate
that stops between the answer and the task row leaves a charge with no task and no workflow to refund it.
Decision: Admission only decides; money moves on approval (the operator places the hold, then sends
`kontra.approval`, refunding if the Update is refused) and in the Lifecycle hooks. A person hiring from the
operator's page accepts in the same gesture ([`relay.md`](../spec/relay.md) §13.1–§13.2,
[`ext-escrow.md`](../spec/ext-escrow.md) §2).

### D46. Gate workers are unversioned; workflow changes are patched and replay-tested

PRD §13 keeps temporal-worker-controller. Worker Deployment Versioning allows at most 100 queues per
version, and per-replica queues exceed that within a few deploys; the gate pod is also the tunnel endpoint,
so it cannot be split into versioned worker deployments. Decision: gate workers unversioned,
`workflow.GetVersion` for behaviour changes, a replay test over recorded histories in CI, the hook
configuration snapshotted into each workflow's input, a cap of 1,000 turns per task. The controller remains
an option for Cloud's own Python workers ([`relay.md`](../spec/relay.md) §14.1, §14.4).

## Kill list (PRD §16)

### D47. At M0 the kill list closes; it does not delete — **accepted by the owner, 25 Oct**

PRD §16: execute at M0; "Delete" the Temporal Authorizer, per-seller namespaces and Nexus endpoints, `nexgen`
derivation, the Engine Protocol and the publish-gate scan; yank `kontra-sdk` on PyPI and archive the repo.
Evidence (kill-list inventory, 25 Oct): the Authorizer, the namespaces and Nexus, and contract derivation run
in production today (`api.kontra.run` healthy; production Temporal is `kontra-temporal:c7a4f83`, built from
kontra-sdk's `temporal/authorizer`); kontra-cloud imports kontra-sdk in 280 places, including the ledger
and Stripe code the PRD keeps and the seam ② route table every kontra-cloud#59 route must enter; PyPI has
six releases, not one; kontra-sdk #7–#10 and kontra-cloud #3, #6 were already closed as completed and are
deployed; the Engine Protocol exists only on unmerged PR branches. Decision: at M0, close the open issues
the PRD names (kontra-sdk #50–#60, kontra-cloud #44–#47) and the Linear items (KON-5–KON-8) with a comment
pointing at this PRD, and comment on the already-closed ones. Deleting production code, archiving
kontra-sdk and yanking PyPI move to M5, with the gate switch, after the owner decides where kontra-cloud's
imports of kontra-sdk go. The 17 open v3 PRs (kontra-sdk #61–#86) and the v3-only issues the list does not
name (kontra-sdk #40, #49, #63, #64, #66, #69–#72, #84; kontra-cloud #54, #58) are left for the owner,
listed in the M0 PR.

### D48. Image names and the Go module path — **decided by the owner, 25 Oct**

Note 2's compose uses `ghcr.io/kontra/kontra` and `ghcr.io/kontra/kontra-gate`. The GitHub account `kontra`
belongs to an unrelated person, so those names are a supply-chain hole for anyone who copies the file; and
`ghcr.io/medmahmoudi26/kontra` (the v1 CLI) and `ghcr.io/medmahmoudi26/kontra-gate` (the FastAPI gate) are
taken. Options: a new organization (for example `kontra-sh`, apparently unregistered) for both images, or
new names under `medmahmoudi26`. The Go module path is undecided too: `proxy.golang.org` caches a v1
pseudo-version of `github.com/medmahmoudi26/kontra`, and git tags `v4.x` would require the `/v4` suffix.
Nothing in this repository names an image or a module until the owner chooses.

Revision 3 (M1 planning): the M1 draft uses `github.com/medmahmoudi26/kontra` as a provisional module path, marked
as such in `go.mod`. Nothing is tagged or published until the owner decides; switching is
`go mod edit -module <path>` plus one rewrite of the imports.

Decided by the owner (25 Oct): the images are `ghcr.io/kontra-sh/kontra` and `ghcr.io/kontra-sh/kontra-gate`,
under a new GitHub organization `kontra-sh`. The Go module path is `kontra.sh/kontra`, served by a `go-import`
page on `kontra.sh` (the domain is the owner's since 25 Oct). M1 switches its `go.mod` and imports to it.

## From the Postgres research (pgx v5.11.0, goose v3.29.0, Postgres 16.15 and 17.11, probed)

### D49. Relayed content is stored as `json`, and `ListTasks` orders by status time

PRD §10: `snapshot jsonb`, `payload jsonb`. `jsonb` refuses a NUL character (`\u0000`), which a legal A2A
text part or MCP result can carry, so one such event would fail its insert and stall the task; it also
drops duplicate keys and reorders. Decision: `tasks.snapshot`, `task_events.payload` and
`work_items.payload` are `json` (no SQL reads inside them; states, ids and times are columns); operator
config stays `jsonb`. `ListTasks` must sort by status timestamp (spec §3.1.4, TCK `CORE-LIST-003`), so
`tasks.status_at` holds it and the caller indexes use it; no index names `updated_at`, so per-chunk
updates stay HOT-eligible. Also: the store refuses `PrevVersion == 0`, non-UUID ids are `TaskNotFound`
before any query, resend detection needs the same request hash (the TCK reuses message ids), and
`task_events` is append-only by trigger ([`migrations/0001_init.sql`](../migrations/0001_init.sql),
[`relay.md`](../spec/relay.md) §4, §5.2, §15).

Revision 3 (M1 planning): `services.card` is `json` too, like `snapshot` and `payload`. `jsonb` refuses U+0000
(`22P05`) and reorders keys, so neither the served bytes nor their ETag could come from it; the served card is
produced from `upstream_card` and kept as `json`, and `scripts/migration_tests.sql` checks that a card with U+0000
is accepted ([`migrations/0001_init.sql`](../migrations/0001_init.sql), [`relay.md`](../spec/relay.md) §3). Amended
by D193 (a card that has no JCS form is not installed).

## From the adversarial review of the M0 specs (25 Oct, 60 findings, all confirmed or partly confirmed)

The review ran four lenses (coverage, security, A2A/Temporal, MCP), each checked by a skeptic. Its fixes are in
the specs; the entries below are the ones that settle or change something the documents said.

### D50. An unknown or forbidden service gets one answer, on every route

PRD §5: gate-side refusals (unknown agent) are JSON-RPC errors before a task exists; PRD §12: allow lists should
not reveal which services exist. Decision: a service that does not exist, is of the other kind, is disabled, or
does not admit the principal gets HTTP `404` with a body fitting the route (JSON-RPC `-32601 unknown service`,
`google.rpc.Status NOT_FOUND`, or empty on card GETs); cards and the upstream card follow the same rule unless the
service admits `anonymous` or is listed ([`relay.md`](../spec/relay.md) §2.1).

### D51. The gate names no escrow key; Kontra's keys are the gate's and the operator's

PRD goal 5: nothing escrow-specific in open source but the extension spec. The drafts had the gate parse
`kontra.escrow.dispute`, strip escrow keys by name and build `sh.kontra/escrow`. Decision: generic mechanisms only.
The gate removes every `kontra.` key from callers (except sealing keys and the operator's `caller_metadata`) and
from services (and `sh.kontra/` keys at MCP protocol-level locations), so a seller cannot spoof an approve URL, an
escrow state, a price or sealing recipients; a message to a task in its review window is offered to the
operator's `OnReviewMessage` hook, which decides whether it is a dispute; the operator writes its MCP escrow
rendering itself (`mcpMeta`) ([`relay.md`](../spec/relay.md) §2.5, §7, §13).

### D52. The gate also owns `capabilities.streaming` and `capabilities.extendedAgentCard`

PRD §5 and note 1: exactly five card fields rewritten. The gate streams from its own log whatever the upstream
does, and does not serve the extended card (PRD §17 default), so it must also set those two fields; and the
security fields are omitted for a service that admits only `anonymous` ([`relay.md`](../spec/relay.md) §3).

### D53. No caller context id ever reaches an upstream

D3 keeps caller context ids verbatim and scopes them per buyer, but forwarding them upstream would merge two
buyers' identical strings in a stateful seller, which sees one client. Decision: the first task of a gate context
goes upstream without `contextId`; the upstream's own context id is reused only on the same session or direct
backend; caller `referenceTaskIds` are mapped to the caller's own upstream tasks or dropped; an upstream event
naming a foreign task id fails the attempt ([`relay.md`](../spec/relay.md) §4).

### D54. `auth-required` is not a turn boundary

PRD §7 treated `input-required` and `auth-required` alike. a2a-go ends an execution only on a terminal state or
`input-required`, and A2A §7.6.1 has agents keep streams open through `auth-required` (credentials arrive out of
band). Decision: the dispatch follows the upstream through `auth-required`; a caller message to an `auth-required`
task is refused; on MCP the state shows as `working` ([`relay.md`](../spec/relay.md) §5.1, §6.2, §7).

### D55. Push runs on its own queue, with limits

PRD §10: `push_webhook` on `kontra-lifecycle`. Each push config is a long activity (D16), so unbounded configs
on the lifecycle's queue would let one buyer starve every task's lifecycle. Decision: queue `kontra-push` with its
own slots; at most 10 configs per task and 256 per buyer; deliveries capped at 10 a second per destination host;
configs announced to the workflow by a `kontra.push` signal ([`relay.md`](../spec/relay.md) §10.1–§10.2).

### D56. Nodes fetch large parts through their own tunnel, never from a URL

PRD §7: parts over 256 KiB are served "as a FilePart with a presigned GET". A node that fetched any URL handed to
it in a part could be made to dial inside its own network, which PRD §12 forbids ("the sidecar forwards to one
configured target only"). Decision: callers get presigned URLs; nodes get `kontra-blob:<ref>` parts and fetch them
on a blob stream inside the tunnel ([`relay.md`](../spec/relay.md) §10.4, [`tunnel.md`](../spec/tunnel.md) §8.3).

### D57. An MCP cancel always cancels — **accepted by the owner, 25 Oct** (departs from PRD §7 for MCP)

PRD §7: if the agent does not support cancel, the error is returned and the task continues. MCP 2025-11-25
requires the receiver of `tasks/cancel` — the gate, for a gate-owned task — to move a non-terminal task to
`cancelled` before it responds. Decision: on MCP the gate forwards the cancel upstream when the upstream declared
it, waits at most 10 s, and writes `canceled` whatever the upstream answered. Consequence for Cloud: a cancelled
MCP task is refunded even if the upstream finished the work ([`relay.md`](../spec/relay.md) §11.6).

### D58. A `hire` task that asks a question shows as `input_required` only when the gate can deliver it

A2A `input-required` maps naturally to MCP `input_required`, but MCP then has the client block on `tasks/result`
for a request the gate can deliver only as a form elicitation. Decision: with form elicitation declared and a
text-only question, `input_required` and one elicitation; otherwise `working`, the question in `statusMessage`,
"answer with the reply tool", and `_meta["sh.kontra/input-required"] = true` ([`relay.md`](../spec/relay.md) §12.4).

### D59. Operator patches may cross Temporal history, capped and operator-authored only

PRD §10 and the owner's rule: events never enter Temporal history. Approvals, annotations and hook results carry
metadata patches. Decision: those patches (operator-authored only, keys under the operator's prefixes, at most 16
KiB each) may cross history; relayed events and caller or service content never do, and hook inputs carry only the
operator's own keys, built from Postgres by the activity. The one caller-authored value that crosses history is a
review-window message's `caller_metadata` keys (Cloud: the dispute reason), at most 16 KiB. Carrying patches by
reference was the alternative not taken
([`relay.md`](../spec/relay.md) §13.3, §14).

### D60. Two test expectations restated: `version_conflict` and `ssrf_webhook` — **accepted by the owner, 25 Oct**

PRD §15: `version_conflict` expects "one 409"; `ssrf_webhook` expects a redirecting URL refused at config time.
No route writes by version, so the 409 is the store's `ErrConcurrentModification`, tested at the store; and a
redirect is only visible at delivery, so a redirecting URL is accepted when set, its `3xx` is a failed delivery,
and the redirect target receives no connection ([`relay.md`](../spec/relay.md) §16).

### D61. Sealing recipients include enrolled keys that are not connected

PRD §8: `recipients[]` "one per connected tunnel key". A task sealed while one replica of a service is offline
could then never be served by it, and the recipient set would change with every reconnect. Decision: recipients
are the keys enrolled for the service, not revoked and not rotated; a rotated key leaves the list at once
([`tunnel.md`](../spec/tunnel.md) §12, [`ext-sealed.md`](../spec/ext-sealed.md) §1).

### D62. MCP sealing covers everything a sealed call carries

Note 1 §8: MCP sealing on `arguments` and `content`. A durable task's result arrives through `tasks/result`, and
its upstream's elicitation and sampling requests, the client's answers and its `statusMessage` are logged in
`task_events`; leaving them in plaintext would narrow PRD goal 3 ("the gate never sees plaintext when `sealed:
true`"). Decision: for a task a sealed call created, the hosting node seals the result, the input requests and the
status message to the job's reply key, and the hiring node seals its answers to the recipients
([`ext-sealed.md`](../spec/ext-sealed.md) §2–§3). Amended by D219 (a sealed plain call's input requests and
answers).

### D63. Tasks whose workflow never started end `lifecycle_unavailable` — **accepted by the owner, 25 Oct** (adds to PRD §7's reasons)

The work queue writes the task row before it starts `TaskLifecycle`; a crash, an unreachable Temporal or a timeout
in between would leave a task with no workflow, no deadline timer and no terminal event. Decision: a resend
re-issues the start with the original work item id as the Update id (idempotent), and a once-a-minute sweep fails
`submitted`/`quoted` tasks older than 2 minutes with no workflow as `failed: lifecycle_unavailable`, a reason added
to PRD §7's set ([`relay.md`](../spec/relay.md) §6.4, §14.5).

### D64. Escrow states gain `resolved`, receipts gain `chargeback` — **accepted by the owner, 25 Oct** (amends PRD §9)

PRD §9: `held`, `released`, `refunded`, `disputed`. Cloud already settles a dispute by a split between the parties
(its ledger's `resolved`) and handles card chargebacks after release. Decision: `kontra.escrow.status` may be
`resolved`, and a receipt's event may be `resolved` or `chargeback` ([`ext-escrow.md`](../spec/ext-escrow.md) §3).

### D65. The MCP relay also accepts 2025-06-18, and refuses batches — **accepted by the owner, 25 Oct** (the goal says 2025-11-25 only)

The goal pins MCP 2025-11-25. Clients and upstreams that negotiate 2025-06-18 (which also forbids batches) are
relayed with tasks off; 2025-03-26 (which requires batch support) is refused with `400`; an absent
`MCP-Protocol-Version` means the session's negotiated version; every POST body must be a single JSON object, so a
batch can never carry calls past per-tool policy, Admission and metering ([`relay.md`](../spec/relay.md) §0, §11.1).

## From M1 planning (revision 3, 25 Oct)

An M1 planning pass (a work plan and an adversarial audit of it) found places where the written design cannot be
built as written. M0 is not signed off yet, so they are fixed in the specs rather than left as departures in code.

### D66. Update validators read workflow state only; the work item is checked where it is used

relay.md §7 and §14.1 (revision 2): the `kontra.message` and `kontra.review_message` validators refused a work
item that was not this task's or was already consumed. A Temporal Update validator runs on the workflow task
thread: it must be read-only and cannot do I/O, so it cannot read `work_items`. Decision: validators check
workflow state only: §7's state table, no turn running, and a `workItemId` not already accepted in this run (kept
in workflow state). Work-item ownership moves out of the validator. The first dispatch of a turn loads the work
item by `(id, taskId)` and consumes it (`consumed_at`) in Postgres; a later dispatch of the same turn (a retry,
attempt `n + 1`, or a resume) may reload a consumed item it owns. `kontra.review_message` has no dispatch: its
handler's append loads the item by `(id, taskId)` and consumes it in the same transaction as the message append,
and an item that is missing or already consumed completes the Update with `accepted: false`. A refused
`kontra.message` or `kontra.review_message` deletes its work item. A caller-side timeout (the Update not accepted
within 10 s, `-32603` / `503`, or a `kontra.review_message` accepted but not completed within the 10 s) does not:
the workflow may still accept the Update, and the turn then runs. Postgres does not record which items a turn
accepted, so no sweep can tell an abandoned item from one a quoted task will consume after approval: an
unconsumed item stays until its task ends and retention deletes it with the task. A first dispatch whose item is
missing or belongs to another task fails the task `lifecycle_unavailable`; a review message's hook patch and
dispute flag apply only when its append commits ([`relay.md`](../spec/relay.md) §7, §14.1, §14.5, §15).

### D67. `allow_private` also permits `http://` — **accepted by the owner, 25 Oct** (relaxes an SSRF rule)

relay.md §10.3 (revision 2) refused `http://` everywhere, and `allow_private: true` (PRD §12: the per-backend
opt-in) skipped only the address check. Read that way, no plain-HTTP upstream or webhook could ever be used, yet the
M1 seller (a2a-cli v0.3.1 serves plain HTTP only), the e2e upstream and D18's TCK webhooks are plain HTTP on
loopback. Decision: `allow_private` (per backend, and `push.allow_private`) also permits `http://`; without it only
`https://` is allowed; redirects are never followed and the environment's proxy is ignored either way
([`relay.md`](../spec/relay.md) §10.3). Revision 2's rule also clashed with §13.1, which lets the Admission hook
URL be `http://` on a loopback host. The clash is now stated: §10.3 names that exception (and any other `hooks.*`
URL §13 allows), and every other operator URL follows §10.3 ([`relay.md`](../spec/relay.md) §10.3, §13.1). Amended
by D192 (the bucket's endpoint).

### D68. API key globs are written `key:kk_acme_*`

PRD §11's example `allow: ["key:acme-*"]` cannot match: a key's principal is `key:<prefix>`
([`relay.md`](../spec/relay.md) §2.3 describes key principals), and `api_keys.prefix` is `^[A-Za-z0-9_]{4,32}$`,
with no hyphen. Decision: keys the gate mints are `kk_<operator prefix>_<random>`, and globs are written
`key:kk_acme_*`. The split: the operator prefix is 1–16 characters `[a-z0-9]` and always present (when
`kontra-gate enroll --api-key <name>` is given no `--prefix`, it is the key's name lowercased, reduced to
`[a-z0-9]` and cut to 16 characters, or `k` if that leaves nothing), the random part 32 base62 characters, and the
stored prefix is everything before the random part plus the random part's first 8 characters, at most
3 + 16 + 1 + 8 = 28 characters, within the column's 32. The random part's other 24 characters (about 143 bits)
are never shown. Keys an operator writes itself (Cloud) keep any format the column accepts, as D37 allows.

### D69. A gate behind a TLS-terminating proxy, and where the client address comes from

PRD §11: the gate terminates TLS itself and refuses plaintext when `public_url` is not localhost. PRD §12 allows a
front proxy, `tck_behind_caddy` runs behind Caddy, and relay.md §17 and tunnel.md §4 already expect channel
binding `none` when a proxy terminates TLS, but no key let such a gate start. Behind any proxy, `rps_per_ip` would
also limit the proxy's one address and throttle every caller. Decision: `tls.terminated_by_proxy: true` lets a gate
listen in plaintext with an `https` `public_url`: HSTS is left to the proxy, the tunnel channel binding is `none`,
and a warning is logged; without it the gate still refuses plaintext off localhost. The client address comes from
`edge.client_ip: {from: remote | proxy_protocol | x_forwarded_for, trusted: [CIDR]}` (default `remote`):
`X-Forwarded-For` is honoured only from trusted CIDRs, taking the rightmost untrusted hop, and a PROXY protocol
header (v1 or v2) only from trusted CIDRs. Cloud's Traefik edge uses the PROXY protocol today (kontra-cloud
`docs/operations.md`: "proxy protocol from the private network", "Traefik believes nobody's X-Forwarded-For")
([`relay.md`](../spec/relay.md) §2.2, §17).

### D70. Built-in ACME is for one replica in v4.0 — **accepted by the owner, 25 Oct** (narrows PRD §12 and §14)

PRD §12 and §14 (M1): built-in TLS with ACME. autocert keeps certificates and challenge tokens in its cache.
Replicas that do not share a cache each order their own certificate (and meet Let's Encrypt's duplicate-certificate
limits), and an HTTP-01 or TLS-ALPN-01 challenge lands on a replica that does not hold its token; 0001 has no table
for a shared cache. Decision: v4.0's built-in ACME (autocert, HTTP-01 and TLS-ALPN-01) is for a single replica with
a directory cache. Several replicas use provided certificates, or a front proxy (D69), until a later migration adds
a shared certificate cache (`kontra.acme_cache`, issuance serialized by an advisory lock)
([`relay.md`](../spec/relay.md) §2.2).

### D71. A direct backend's URL is the base; the card never steers the dialer

relay.md §6.1–§6.2 (revision 2): the gate calls "the URL" through the SSRF dialer. Whether that URL is the A2A
endpoint or the base the card is fetched from was not said, and an a2a-go client built from the upstream card dials
the card's interface URL, which may name another host or port: a seller-controlled value would steer the gate's
dialer. Decision: the configured `url` is the base; the card is fetched relative to its path (`agent-card.json`,
then `agent.json`); the gate dials only the configured `scheme://host:port`, enforced in the backend's dialer,
whatever the card's interface URL says; only the binding, protocol version and path come from the card
([`relay.md`](../spec/relay.md) §3, §6.1–§6.2).

### D72. The push rate cap is shared out among replicas

relay.md §10.2 (revision 2) capped deliveries to one destination host at 10 a second across the gate, and did not
say how replicas share the cap. Decision: each replica enforces 10/N a second, where N is the number of live
replicas in `gate_replicas`, at least 1, refreshed every 10 s; so 10 a second is a gate-wide upper bound. N counts
draining replicas too: they keep polling `kontra-push` (D76), and leaving them out would make N 0 on a gate of one
replica and let the gate exceed the cap during a drain ([`relay.md`](../spec/relay.md) §10.2).

### D73. Connection caps

PRD §12 lists connection caps among the edge controls; no key and no default existed. Decision:
`limits.conns_per_ip` (default 256) and `limits.max_conns` (default 10,000), enforced by the listener.
Connections from a trusted proxy address (`edge.client_ip.trusted`, D69) are exempt from `conns_per_ip`, because
behind an `x_forwarded_for` proxy every connection carries the proxy's address; `max_conns` still applies to them.
With `proxy_protocol` each connection's client address is known (its PROXY header), and `conns_per_ip` applies to
it. Consequence: behind an `x_forwarded_for` proxy the gate caps no single client's connections, so a front proxy
holding many SSE streams is not refused at 256, and only `max_conns` bounds them; `rps_per_ip` still counts each
client by its forwarded address ([`relay.md`](../spec/relay.md) §2.2, §17).

### D74. The A2A resend hash

relay.md §15 and D49 detect a resend by "the same request hash", and `tasks.first_request` is "sha256 of that
request", but only `hire` tasks said what is hashed (§12.3). Decision: for A2A, SHA-256 of Go's `json.Marshal` of
the typed `a2a.SendMessageRequest` after the §2.5 key stripping: the whole request, including any inline push
config, so a resend that differs in anything is a new task. Go's encoder is deterministic for equal values, so no
JCS is needed. The encoding is tied to the pinned a2a-go structs: a gate upgrade inside the 24 h resend window can
turn a resend into a new task, which is accepted ([`relay.md`](../spec/relay.md) §15).

### D75. The snapshot fold past the terminal row

relay.md §9 (revision 2) folds rows `1..K` with a2a-go's `ApplyUpdate`. A terminal task's log still grows by an
accepted review-window message and by metadata rows (§5.1), and `ApplyUpdate` refuses any update to a terminal task
(`a2aevent/event.go`, `validateUpdate`), so a fold past the terminal row failed. Decision: the fold applies task,
status and artifact rows with a2a-go's update rules up to the terminal row; after it, message rows are appended to
history directly and metadata rows merged at the top level ([`relay.md`](../spec/relay.md) §9).

### D76. Drain order

relay.md §14.3 (revision 2): a draining replica's per-replica workers keep polling until its last dispatch ends, and
the replica is not selected for new attempts. A direct dispatch goes to whichever replica polls the shared
`kontra-direct` queue, so "keeps polling" contradicted "not selected". And the text named only the per-replica
workers: with `kontra-lifecycle` stopped, a gate of one replica (a team gate often is) could accept no Update (a
cancel) and record no dispatch result during a drain. Decision: a draining replica stops its `kontra-direct`
pollers at once (`WorkerStopTimeout` = `drain_timeout`; in-flight dispatches keep running and heartbeating), and
keeps `kontra-lifecycle`, `kontra-push`, `kontra-direct-ctl` and `kontra-ctl-<id>` polling until its last dispatch
has ended or `drain_timeout` passes, then stops them. `kontra-tunnel-<id>` keeps polling as before: §6.1 already
selects no new attempt on a draining replica ([`relay.md`](../spec/relay.md) §14.3).

### D77. A dispatch's writes are fenced

relay.md §6.4 retries before the first upstream id, and re-attaches after it, when a dispatch's heartbeat times out.
Temporal's timeout does not stop a dispatch whose process is still alive (a partition, a GC pause, `SIGSTOP`): it
learns only on its next heartbeat, and could meanwhile keep writing the log beside the retry or the re-attach.
Decision: the fence is a per-dispatch token, `attempts.dispatch_id`. Every dispatch (a new attempt, a follow-up turn
on the same attempt, a resume) writes a fresh `dispatch_id` on its attempt when it starts and holds it; every
append it makes commits only if, in the same transaction, attempt `n` is the task's latest attempt, has `ended_at`
null and has `dispatch_id` equal to the held value. A fenced write aborts that dispatch. The token is not
`replica_id`: a re-attach can land on the stalled dispatch's own replica (a GC pause or `SIGSTOP` on a gate of one
replica, which is then the only `kontra-direct` poller), so a newer dispatch must fence an older one on the same
replica too. `attempts.replica_id` stays, as where the dispatch runs; it routes `abort` and `forward_cancel`
([`relay.md`](../spec/relay.md) §4, §5.2, §6.4, §14.2).

`attempts.ended_at` and `outcome` are written only when the attempt ends (a terminal outcome, a failure that is
retried, `aborted`, `no_session`, …), never when a turn stops at `input-required` (`auth-required` ends no turn,
D54): follow-up turns run on the same attempt, and an `ended_at` written at the end of a turn would fence every
follow-up. The outcome `interrupted` is written when the task ends (timeout, cancel) while its attempt waits at
`input-required` ([`relay.md`](../spec/relay.md) §4, §7).

### D78. The workflow outlives its pushes

relay.md §10.2: `push_webhook` ends after delivering the terminal snapshot. If `TaskLifecycle` completed first (no
review window, as in open source), a pending `push_webhook` would learn on its next heartbeat that its workflow is
closed and stop, so a webhook that was down across the end of the task would never get the terminal snapshot.
Decision: `TaskLifecycle` does not complete while a `push_webhook` activity it started is pending; the activity
still ends at its own limits ([`relay.md`](../spec/relay.md) §10.2, §14.1).

### D79. Cancel when the dispatching replica is gone

relay.md §8 and §14.2 send `abort`, and a direct task's `forward_cancel` while a dispatch runs, to
`kontra-ctl-<replica>`. When that replica has died and its dispatch's heartbeat has not yet timed out, nothing polls
the queue, and both fail their 10 s schedule-to-start. And a cancel whose upstream answered `canceled` could return
before the dispatch had logged it, so a `GetTask` right after `CancelTask` could still show the task running.
Decision: `forward_cancel` for a direct task falls back to `kontra-direct-ctl` when `kontra-ctl-<replica>` does not
start it within 10 s; `abort` waits for the dispatch's heartbeat timeout, then the §8 cancel table applies;
`kontra.cancel` completes after the terminal `canceled` event is in the log, within its 60 s budget
([`relay.md`](../spec/relay.md) §8, §14.2). The wait for the heartbeat timeout applies to A2A `CancelTask` only.
An MCP `tasks/cancel` keeps relay.md §11.6's rule (at most 10 s, then `canceled`, D57), which is safe while an old
dispatch may still be alive: once `canceled` is logged the terminal rule refuses its later appends, and its fence
(D77) aborts it; `abort` still runs when it can ([`relay.md`](../spec/relay.md) §8, §11.6).

### D80. The named tests M1 runs

relay.md §16 says every named test is a test in `test/e2e`, and that `replica_kill`'s direct variant is M1's, but
not which tests M1 runs. Decision: M1 runs `relay_resume`, `replica_kill` (direct variant), `cancel_forwarded`
(direct, against an a2a-go seller that handles cancel: a2a-cli v0.3.1's exec mode can end `failed` instead of
`canceled` on a cancel), `push_delivery`, `ssrf_webhook`, `card_compat`, `cross_buyer` and `version_conflict`; the
tunnel variants and the rest come with their milestones ([`relay.md`](../spec/relay.md) §16).

## From the owner's review (25 Oct)

### D81. A follow-up records its delivery; a resume re-sends only an undelivered one — **decided by the owner, 25 Oct**

Found while building M1. A follow-up turn on a direct backend whose dispatch dies after taking the work item but
before the upstream reacts is resumed by a re-attach (D42), which sees the upstream still at `input-required` and
cannot tell whether the message arrived. Ending the turn there loses the caller's answer without anyone knowing.
Decision: the dispatch writes `work_items.delivered_at` when the upstream first answers the follow-up's
`SendMessage` or `SendStreamingMessage` (a `Task`, a `Message` or the first stream event), before it logs anything
from that answer. A resume of a follow-up turn whose item has no `delivered_at` sends it again, with the same
`messageId` and the upstream ids substituted, then continues as a re-attach; with `delivered_at` set it
re-attaches without sending. The one case left is an answer lost in transit after the upstream processed the
message: the upstream then sees the follow-up twice with the same `messageId`, which the owner accepted. A
task's first turn is not covered: a resume before its first upstream id is a retry (§6.4), and after it the
first message is never sent again. Tunnel backends are not covered either: after the first upstream id a lost
tunnel fails the task (D32) ([`relay.md`](../spec/relay.md) §6.4, §7).

## From building M1 (25 Oct)

Building M1 (work packages WP-01 to WP-09 and the review after them) met places where the approved specs (revision
3) say nothing, contradict themselves, or say what the code cannot or should not do. Each was resolved in the code
with the closest faithful behaviour; where that departs from the text, a `SPEC-CONFLICT` comment in the code says
so and names its entry here. The specs now say what the code does and cite the entry, except where an entry says
the spec is unchanged. **Needs the owner** marks the entries that change documented behaviour; the others settle a
silence or a contradiction.

### D82. Built-in ACME validates with TLS-ALPN-01 only — **accepted by the owner, 26 Oct** (narrows D70)

relay.md §2.2 and D70: built-in ACME is "autocert, HTTP-01 and TLS-ALPN-01". HTTP-01 is validated on port 80, and
no key (§17) opens a second, plaintext listener there: the gate's one public listener is its TLS listener, so an
HTTP-01 challenge cannot be answered as the spec is written. Decision: the gate validates with TLS-ALPN-01 only, on
its TLS listener; the CA connects to port 443, so `listen` is `:443` or forwarded from it. `ACME.HTTPHandler` is
the seam for HTTP-01 once a key for a port-80 listener exists ([`relay.md`](../spec/relay.md) §2.2;
`internal/edge/tls.go` `ACME`, `TestACMEIssuesIntoDirectoryCache`).

### D83. An IPv6 client is counted by its /64 — **accepted by the owner, 26 Oct** (amends D73)

relay.md §2.2 and D73 count `rps_per_ip` and `conns_per_ip` "by the client address". One end site (a home, a cloud
instance, a phone) commonly holds a whole IPv6 /64, so counting each address gives such a client a fresh token
bucket and a fresh connection quota for every address it picks, and `rps_per_ip` no longer bounds credential
spraying. Decision: an IPv6 client is counted by its /64 (a constant, not a key), an IPv4 client by its address;
the client address itself stays whole wherever it is recorded. Precedent: [`tunnel.md`](../spec/tunnel.md) §6.2
counts IPv6 by /48 for anonymous tenants. A key for another prefix length is the owner's call
([`relay.md`](../spec/relay.md) §2.2; `internal/edge/clientip.go` `countKey`, `TestIPv6CountedPerSlash64`).

### D84. Edge refusals before the body is read

relay.md §2.2: on JSON-RPC a refusal is also a JSON-RPC error object "when the request could be parsed". A
rate-limit refusal and a `413` happen before the body is read, so no request `id` is known, and the text does not
say whether the error body is then owed. Decision: every refusal on a JSON-RPC route carries a JSON-RPC error body
(`-32600`, `request too large` or `rate limited`), with the request's `id` when the body was read (the in-flight
cap) and `id: null` otherwise ([`relay.md`](../spec/relay.md) §2.2; `internal/edge/edge.go`, `TestJSONRPCErrorID`).

### D85. A slow request body is cut after 30 s

PRD §12 lists slow-loris protection among the edge controls. relay.md §2.2 has a header timeout but no bound on how
long a body may take to arrive, and the HTTP server's `ReadTimeout` cannot be that bound: `WriteTimeout` must stay
0 for SSE, and the edge bounds bodies itself. Decision: a body that has not arrived within 30 s (4 MiB at about 140
KiB/s) is refused `408` before the handler runs. It is a constant, not a configuration key
([`relay.md`](../spec/relay.md) §2.2; `internal/edge/edge.go` `DefaultBodyTimeout`, `TestSlowBodyIsCut`).

### D86. An HTTP status beside a JSON-RPC code is the HTTP+JSON binding's — **accepted by the owner, 26 Oct**

relay.md §2.4 ("`VersionNotSupportedError` (`-32009`, HTTP `400`)"), §7 and §14.1 ("`-32603` (HTTP `503`)") and §8
(`TaskNotCancelableError`, HTTP `400`) pair a JSON-RPC code with an HTTP status without saying which binding the
status belongs to. Sent on the JSON-RPC route, a `400` or `503` hides the error from A2A clients: a2a-go's JSON-RPC
client reads an error only from an HTTP `200` response and reports any other status as "unexpected HTTP status"
(`a2aclient/jsonrpc.go`), and a2a-go's own JSON-RPC server answers every error with `200`. Decision: on JSON-RPC
the error is the body of an HTTP `200`; the status applies on HTTP+JSON, as A2A §3.3.2's table maps codes to
statuses (`-32009` is `400`; an Update not accepted in time is `503`, `UNAVAILABLE`). The JSON-RPC statuses the
spec states explicitly stay: the unknown-service `404` (§2.1) and a refused credential's `401`. So the M1 plan's
"A2A-Version absent is `-32009` (HTTP `400`)" holds on HTTP+JSON only ([`relay.md`](../spec/relay.md) §2.4;
`internal/sse/errors.go` `WriteError`, `TestWriteError`, `TestVersions`, `TestUnavailableIs503`).

### D87. The credential is checked before the service

relay.md §2.1 gives an unknown or forbidden service one answer, `404` (D50), and §2.3 makes a revoked or expired
API key `401`; neither says which check comes first. Looking the service up first would make a bad key `404` on an
unknown service and `401` on an existing one, which tells a caller which services exist. Decision: the credential
is checked first, so a bad one is `401` on every A2A route, cards included, whether or not the service exists. A
request without a credential is the `anonymous` principal and gets the `404` unless the service admits `anonymous`;
a key without the `hire` scope is not admitted, so it gets the `404` too ([`relay.md`](../spec/relay.md) §2.1;
`internal/a2ahttp/a2ahttp.go`, `TestBadCredential`, `TestUnknownServiceIsOneAnswer`).

### D88. A card in both shapes is normalized like a 0.x card for its 0.x members — **accepted by the owner, 26 Oct**

relay.md §3: "A 1.0 card is used as is", and every seller-authored member but the gate's is kept. A seller built on
a2a-go v2 that also serves 0.3 clients (`a2av0`'s card producers) publishes one card with `supportedInterfaces` and
the 0.3 `url`, `preferredTransport`, `additionalInterfaces`, `protocolVersion`, `security` and
`supportsAuthenticatedExtendedCard`. Used as is, the served card names the seller's own, often internal, address
beside the gate's interfaces, so a 0.3 client dials the seller past the gate's `allow:` list and authentication,
against §3's "Never an upstream URL" and §2.4's 1.0-only downstream; and its `security` and
`supportsAuthenticatedExtendedCard` contradict the gate's `securityRequirements` and `extendedAgentCard: false`
(D52). Decision: those members are the 0.x spelling of fields §3 gives the gate, so a card that has them beside 1.0
members is normalized like a 0.x card for them: the interface members are dropped, and `security`,
`supportsAuthenticatedExtendedCard` and each skill's `security` are converted where the card has no 1.0 member of
its own and dropped otherwise. A pure 1.0 card is still used as is ([`relay.md`](../spec/relay.md) §3;
`internal/card/card.go` `Normalize`, `TestCombinedCardServesNoUpstreamURL`).

### D89. `card_fetched_at` counts from the gate's last good fetch

relay.md §3: a failed fetch keeps the last verified card, and the service becomes unavailable only when no fetch
has succeeded for 30 minutes. The card keeper persists only a changed card, so `services.card_fetched_at` was the
time the card last changed: a replica restarting while the seller was down seeded from that old time and served
`503` cards, although another replica had fetched the card a minute before. Decision: every successful fetch whose
bytes equal the stored `upstream_card` also moves `card_fetched_at` (guarded by backend and bytes), so the 30
minutes count from the gate's last good fetch on any replica ([`relay.md`](../spec/relay.md) §3;
`internal/gate/services.go` `touchCard`, `TestCardKeepersPersistAndSeed`).

### D90. `seller_keys` on a direct A2A service is refused until verification is built — **accepted by the owner, 26 Oct** (M1 only)

relay.md §3 and §17: with `seller_keys` configured, a card no key verifies makes the service unavailable (`503`
card routes, `service_unavailable` at the first attempt). M1 does not verify seller signatures (M3), and has no
unavailable state for a direct service, so ignoring the keys would serve the card unverified: failing open.
Decision: until verification is built, a configuration with `seller_keys` on a direct `a2a` service refuses to
start, as one with the remote hooks does, and a row the operator writes (§13.5) with the same is left out of the
registry: its routes answer as an unknown service and its tasks fail `service_unavailable`. On a tunnel or MCP
service, which M1 does not serve, the key is accepted with a warning. The spec is unchanged: this is M1's scope,
and the work that verifies signatures removes the refusal (`internal/config/validate.go`,
`internal/gate/services.go` `refusable`, `TestFailOpenKeysRefused`,
`TestRegistryRefusesWhatTheConfigurationRefuses`). Amended for tunnel services by D144: M2 serves them, and
refuses `seller_keys` on them too.

### D91. `sealed:` needs a tunnel backend

relay.md §17 and [`ext-sealed.md`](../spec/ext-sealed.md) §1 do not tie `sealed:` to a backend. But the recipients
are the identity keys enrolled for the service, and the hosting node unseals (ext-sealed §1, §4); a direct backend
has neither, so a sealed direct service could never be served as declared, and a build that does not seal would
relay it in plaintext. Decision: `sealed:` on a direct backend is refused at start, and a row the operator writes
with it is left out of the registry, as in D90 ([`relay.md`](../spec/relay.md) §17; `internal/config/validate.go`,
`internal/gate/services.go` `refusable`, `TestFailOpenKeysRefused`,
`TestRegistryRefusesWhatTheConfigurationRefuses`). Amended by D144: until sealing is built (M3), `sealed:` on a
tunnel service is refused as well.

### D92. "The same direct backend", without a column for it — **accepted by the owner, 26 Oct**

relay.md §4 reuses a recorded upstream context id only on "the same tunnel session or the same direct backend", and
maps `referenceTaskIds` only within the same backend. But 0001's `attempts` records no direct backend's identity,
only `backend = 'direct'`: after a SIGHUP or NOTIFY moves a service to another URL, seller A's context and task ids
would be sent to seller B. Decision: without a schema change, an attempt counts as on the service's current direct
backend only if it started after the service row last changed (`services.updated_at`, which every change of its
backend moves); otherwise the task carries none, as §4 says. This also drops context continuity after any other
change of the row, a changed card included. The proposal: a migration 0002 adding an `attempts` column with the
direct backend's identity (a hash of `services.backend`), matched exactly, and the column added to §4's list
([`relay.md`](../spec/relay.md) §4; `internal/store/attempts.go` `SameDirectBackend`,
`TestUpstreamIDsStayWithTheirBackend`).

### D93. a2a-go's synthesized failure is also told by its shape

relay.md §15 and D8: the store refuses a `failed` write the relay did not emit. Read literally, that refuses every
`failed` write the registry does not hold; if a2a-go ever stopped passing the yielded event pointer through
unchanged, it would refuse the relay's own failure, and the task would hang while the registry reports it logged.
Decision: while a dispatch is registered, the store refuses a `failed` status only when it has no status message
and the registry does not hold it. At the pinned a2a-go v2.7.0 this refuses exactly a2a-go's own `SetTaskFailed`,
which never carries a message; the relay's failures always do (§6.3) ([`relay.md`](../spec/relay.md) §15;
`internal/store/store.go` `synthesizedFailure`, `TestFailureGuard`, `TestClusterRefusesSynthesizedFailure`).

### D94. A work item records the task version it was stored at

D81's resend needs a follow-up's turn boundary: the version the task stood at when the follow-up's work item was
stored. 0001's `work_items` has no column for it, and adding one needs a migration the owner has not approved.
Decision: the version is stored inside `work_items.payload` under a key of Kontra's own (`kontraTaskVersion`) and
decoded in Go only, so D49's "no SQL reads inside" holds and 0001 is unchanged. A column in a later migration is
the owner's call ([`relay.md`](../spec/relay.md) §8.1; `internal/store/workitems.go`,
`TestFollowUpResumeTurnBoundary`).

### D95. Closed sessions of the gate's own `/mcp`

relay.md §5.4 deletes `mcp_sessions` closed for longer than "the retention", which is a service's
(`services.<name>.task.retention`); a session of the gate's own `/mcp` names no service. Decision: such sessions
keep the default task retention, 7 days; a service's sessions use its `retention_s` ([`relay.md`](../spec/relay.md)
§5.4; `internal/store/sweep.go` `DeleteClosedSessions`, `TestRetentionSweep`).

### D96. An upstream stream that ends before its turn does

relay.md §6.2–§6.4 do not say what a dispatch does when the upstream closes a `SendStreamingMessage` stream
cleanly, without an error, before a terminal or `input-required` event. Decision: the closest rule is the
re-attach's ("a subscription that ends without a terminal event falls back to polling", §6.4): once an upstream
task id exists, the dispatch follows the task by `GetTask` polling; before one, the attempt fails `upstream_error`
([`relay.md`](../spec/relay.md) §6.2; `internal/relay/executor.go` `stream`).

### D97. The 30 s `GetTask` rule applies to every `GetTask` loop — **accepted by the owner, 26 Oct**

relay.md §6.4 states "only `GetTask` failing for 30 s fails the task" for the re-attach, while its table fails a
task whose upstream id exists at the first refused or reset connection or `502/503/504` (`upstream_unreachable`)
and at the first `408/429` (`upstream_error`). Read literally, one failed poll of an hour-long task on the polling
path would fail it. Decision: every `GetTask` loop, the polling path's and the re-attach's, counts the calls that
do not reach the service (refused, reset, `502/503/504`, or no answer within 10 s) and throttled calls (`408/429`,
honouring `Retry-After` within the grace) toward 30 s, and when the 30 s run out the task fails with the table's
reason (`upstream_unreachable`; `upstream_error` for `408/429`). An answer that is an error is never retried: a
protocol error or an over-limit body fails the task `upstream_error` at once. The call that carries the message
(`SendMessage`, `SendStreamingMessage`) keeps the table's rule ([`relay.md`](../spec/relay.md) §6.4;
`internal/relay/executor.go` `getTask`, `TestGetTaskFailures`, `TestGetTaskNeverAnswered`).

### D98. A resent follow-up the upstream refuses — **accepted by the owner, 26 Oct** (refines D81)

relay.md §6.4's table fails a task at an upstream protocol error once its upstream id exists, while D81 resends an
undelivered follow-up and "then continues as a re-attach". An upstream that already took the follow-up refuses the
copy: a2a-go does while the first copy runs, and any seller does once the task it finished is terminal. Failing
then loses the result of a turn the seller is working on or has finished, which D81 exists to prevent; following an
upstream that never took the follow-up would instead wait at its `input-required` until the deadline. Decision: a
refused resend continues as a re-attach, and the task fails `upstream_error` (with the upstream's message) only if,
for 30 s, the upstream task neither leaves the projection's `input-required` nor shows the follow-up's `messageId`
in `GetTask` with `historyLength: 1`. A2A lets an agent keep no history, so an upstream that keeps none and takes
longer than 30 s to move is still failed ([`relay.md`](../spec/relay.md) §6.4; `internal/relay/executor.go`
`refusedResend`, `TestResentFollowUpRefused`, `TestResentFollowUpRefusedWhileUpstreamWorks`).

### D99. Retries used up by lost dispatches fail `upstream_unreachable`

relay.md §6.4 retries a lost dispatch of a direct backend (a heartbeat timeout, a lost context, or no replica
polling `kontra-direct` within the 15 s schedule-to-start) before the first upstream id, but names no failure
reason for when those retries run out. Decision: the task fails `upstream_unreachable` ("the service could not be
reached"), the closest reason in the closed set, and the dead attempt's outcome is the same; a tunnel backend's is
`tunnel_lost` ([`relay.md`](../spec/relay.md) §6.4; `internal/lifecycle/workflow.go` `lost`,
`TestScheduleToStartTimeoutStartsANewAttempt`).

### D100. A dispatch that never started after the first upstream id

relay.md §6.4's table does not name a dispatch that never started (its schedule-to-start passed) once the upstream
task id exists. Applying the re-attach row to it loses a follow-up: a resume skips the work item's consume,
re-attaches without sending, finds the upstream still at `input-required` and ends the turn, so the follow-up is in
the gate's history but the seller never received it. A new attempt is no fix: after the first upstream id there is
none. Decision: before the first upstream id, a new attempt, as for any lost dispatch (D99). After it, a direct
backend dispatches the same turn again on the same attempt, as it was (a follow-up's dispatch stays the turn's
first, which consumes the unconsumed item and sends it; a resume stays a resume), backing off 1 s doubling to 30 s
and counted with resumes against D42's cap of 10; at the cap the task fails `upstream_unreachable` ("no gate
replica took the turn's dispatch"), as in D99. A tunnel backend fails `tunnel_lost` ([`relay.md`](../spec/relay.md)
§6.4; `internal/lifecycle/workflow.go` `notStarted`, `TestFollowUpDispatchNotStartedIsSentAgain`,
`TestResumeNotStartedStaysAResume`, `TestFollowUpDispatchNeverStartedFails`).

### D101. Which dispatch may reload a consumed work item

D66 lets "a later dispatch of the same turn (a retry, attempt `n + 1`, or a resume)" reload a consumed work item it
owns, but a dispatch cannot otherwise tell a retry from a duplicate: a stale dispatch still alive after a resume
replaced it is fenced for its appends (D77), not for its send. Decision: a consumed item is reloaded only by a
resume, or by a retry of the task's first turn on a new attempt (`n > 1`; a follow-up is never retried on a new
attempt). A resume that finds its item unconsumed consumes it, and is then the dispatch that may send it (D81). Any
other dispatch that finds its item consumed refuses it rather than send the message upstream twice
([`relay.md`](../spec/relay.md) §14.1; `internal/relay/dispatcher.go` `workItem`,
`TestConsumedFollowUpNotSentAgain`).

### D102. What a dispatch returns

relay.md §14.2: `dispatch` "returns `{outcome}` only", and §6.4 honours `Retry-After` on a `408/429` retry, which
is the workflow's. With the outcome alone the workflow could neither wait out the upstream's `Retry-After` nor know
which reason to write when the retries of a retryable outcome run out. Decision: the result is `{outcome, reason,
retryAfterSeconds}`. `reason` is the failure the relay logged, for a terminal outcome, or the reason
`write_terminal` records when the retries run out, for a retryable one (`upstream_unreachable`, or `upstream_error`
for `408/429`); `retryAfterSeconds` is the upstream's `Retry-After` on a retryable `408/429`. A failure's text
never crosses history: the relay, or the dispatcher for a missing work item, writes the `failed` event itself under
the dispatch's fence, and ends the attempt whenever the dispatch ends it ([`relay.md`](../spec/relay.md) §14.2;
`internal/lifecycle/types.go` `DispatchResult`, `TestRetryHonoursRetryAfter`).

### D103. `limits.task_bytes` counts each event's whole encoding — **accepted by the owner, 26 Oct**

relay.md §6.6's table names `limits.task_bytes` "Parts stored per task", while its rule is that "nothing an
upstream sends is unbounded", and `task_bytes` is the only bound on what one task accumulates. Counting parts only
left the rest of every event unbounded: the metadata of a task, status, artifact, message or part, a part's
`filename` and `mediaType`, an artifact's name and description, and status events with no message at all, each
event up to 4 MiB (SSE) or 8 MiB (polled), every one logged and rewritten into the projection until the deadline.
Decision: each event counts its whole JSON encoding, the bytes the log stores for it, and a dispatch starts from
what the projection already holds; the 256 KiB part rule is unchanged. Two gaps stay for the owner: the budget
restarts from the projection at every dispatch, so the status rows of many follow-up turns are not all counted; and
every append rewrites the whole snapshot, which may want a cap on rows or on write volume
([`relay.md`](../spec/relay.md) §6.6; `internal/relay/executor.go` `overLimit`, `TestTaskBytesCountsWholeEvents`).
Amended by D194 (the task's bucket objects count too).

### D104. No bucket in M1: parts over 256 KiB are refused even with `blobs:` — **accepted by the owner, 26 Oct** (M1 only)

relay.md §10.4 stores a part over 256 KiB in the bucket `blobs:` names, and refuses it only on a gate without one.
M1 has no bucket code (the claim-check bucket is M3's). Decision: `blobs:` is validated and accepted with a warning
at start, and an M1 gate behaves as one without a bucket: a caller part over 256 KiB is `InvalidParams` (`part too
large: configure blobs`), and an upstream part over it fails the task `upstream_error`. The spec is unchanged: the
work that builds the bucket removes the warning ([`relay.md`](../spec/relay.md) §10.4;
`internal/config/validate.go` `unbuilt`, `internal/relay/interceptors.go`, `TestCallerLimits`,
`TestUpstreamPartLimit`, `TestUnbuiltWarnings`).

### D105. A follow-up to a turn end the workflow has not processed yet is not refused — **accepted by the owner, 26 Oct**

relay.md §7 and §14.1 refuse a follow-up "while the previous turn of the same task is still being dispatched", by a
validator that reads workflow state only (D66). But a caller learns that a turn ended from the log: the dispatch
logs `input-required`, a2a-go answers the caller at once, and the dispatch activity returns to the workflow a
moment later. A caller that answers at once meets a workflow that still counts the turn as running, and read
literally it is refused although the turn has ended (the relay test over the real stack hit this). Comparing
`tasks.version` cannot tell the two apart either, because operator metadata rows move it. Decision: the
`kontra.message` Update carries the turn end the follow-up answers (the seq of the last event that is not a
`metadata` row), and the validator tells a final refusal (another follow-up of that turn end was accepted first, or
the caller read no later turn end than the workflow processed: `UnsupportedOperationError` at once) from one by a
workflow that has not processed that turn end yet. Only the latter is sent again, backing off 25 ms doubling to 1
s, and only while the task was waiting for input when the work item was stored and its log still ends at that turn
end. One not accepted within the 10 s is `-32603` (HTTP+JSON `503`), and since no Update of it is pending, its work
item is deleted, not kept ([`relay.md`](../spec/relay.md) §7, §14.1; `internal/lifecycle/queue.go` `message`,
`TestMessageRefusalSaysWhetherTheWorkflowIsBehind`, `TestQueueRacesOverTemporal`).

### D106. A review-window message sent before the window is decided waits for it — **accepted by the owner, 26 Oct**

relay.md §14.1's `kontra.review_message` validator refuses "unless the window is open". A caller sends the message
once it reads `completed` from the log, which can be before the workflow learns that the turn ended and before
`OnTerminal` and `ReviewWindow` run (a remote hook can take seconds): read literally, a message sent into a window
about to open is refused. Decision: before `ReviewWindow` has answered, the validator accepts the message and its
handler waits for the window; a window that does not open answers `accepted: false`, which the caller sees exactly
as a refusal (the interceptor deletes the work item and answers `UnsupportedOperationError`). Once the window is
decided closed, the validator refuses as written ([`relay.md`](../spec/relay.md) §7, §14.1;
`internal/lifecycle/workflow.go` `validateReviewMessage`, `TestReviewMessageBeforeTheWorkflowLearnsTheEnd`).

### D107. `kontra.cancel` has a fourth outcome, `continues`

relay.md §14.1 names `kontra.cancel`'s outcomes `canceled | not_cancelable | unsupported`, and §8 gives the caller
"whatever the upstream answers, with gate ids": a `canceled` task, or the upstream's error while the task
continues. An upstream may answer `CancelTask` with a task that is not canceled and no error (A2A 1.0.1 §3.1.5:
"success is not guaranteed"), which none of the three describes: `canceled` would log `canceled` for a task the
seller keeps running (§8 writes it only for "the upstream's `canceled` status"), and `not_cancelable` or
`unsupported` would answer an error the upstream never sent. Decision: an answered task in `canceled` is
`canceled`; one in another terminal state is `not_cancelable`; one still running, or no task, is `continues`:
nothing is written, the task continues, and the caller gets the task as the gate stores it, with gate ids
([`relay.md`](../spec/relay.md) §8, §14.1; `internal/lifecycle/types.go` `CancelContinues`,
`TestCancelAnsweredWithARunningTaskContinues`).

### D108. The stream of a resend

relay.md §9 defines the `SendStreamingMessage` stream of a new task and of a follow-up, but not that of a resend: a
request without a `taskId` that the work queue answers with the task it repeats (§15). Read as a follow-up's, the
stream of a resend whose task is already terminal or at `input-required` would never close. Decision: a resend's
stream leads with the snapshot at the task's current version, without an `id:` line; if that snapshot already ends
a turn (terminal, `input-required` or `quoted`) the stream closes after it, and otherwise the later rows follow and
it closes at the first that ends a turn ([`relay.md`](../spec/relay.md) §9; `internal/sse/sse.go`,
`TestSendStreamingResend`).

### D109. The stream of a review-window message

relay.md §9 does not cover a `SendStreamingMessage` to a `completed` task with an open review window, which the
relay's interceptor answers itself (§7, §15): the work queue never runs, and there is no turn to stream. Decision:
the stream is the interceptor's answer as one task event without an `id:` line, and then it closes
([`relay.md`](../spec/relay.md) §9; `internal/sse/sse.go`, `TestSendStreamingAnswer`).

### D110. A review-window message's inline push config is dropped

relay.md §10.1 has the gate save an inline push config before the work item is written. A review-window message is
to a task that has ended: a config set on it would deliver nothing (D111), and keeping it in the stored request
would put the webhook URL, token and credentials into `work_items`, which 0001 documents as holding no credentials.
Decision: the inline config is removed from the stored request and not saved, and the message is not refused for
carrying one (a2a-go's client sends its default push config with every message) ([`relay.md`](../spec/relay.md)
§10.1; `internal/relay/interceptors.go`, `TestReviewMessageStoresNoPushCredentials`).

### D111. A push config set on an ended task delivers nothing

relay.md §10.1–§10.2 do not say what a config set on a task that has already ended receives. Delivering it the
terminal snapshot works only while the workflow is still open; once the workflow has closed nothing delivers the
config, which still counts toward the buyer's 256 delivering configs until retention. Decision: a config set on an
ended task starts at the terminal row: it is stored, listed and deletable, receives nothing, and does not count as
delivering. This matches a2a-go (a config receives the events that follow it) and A2A §3.1.7 (a config persists
until the task completes) ([`relay.md`](../spec/relay.md) §10.1; `internal/push/push.go`,
`TestConfigsOnEndedTasksAreDone`).

### D112. History names a push config by a digest of its id

relay.md §14 lets no caller-authored value but `caller_metadata` keys cross history, while §10.2, §14.1 and §14.2
put a push config's `configId` in `kontra.push` and in `push_webhook`'s input, and A2A lets the caller choose that
id (up to 128 characters). Decision: wherever history carries a config, it carries the config's ref, `pc_` and a
hex SHA-256 of the task id and the config id, in the `configId` field; `push_webhook` finds the config among the
task's (at most 10) by it. The ref is stable: a config deleted and set again with the same id has the same ref
([`relay.md`](../spec/relay.md) §10.2, §14.1, §14.2; `internal/lifecycle/pushref.go` `ConfigRef`, `TestConfigRef`,
`TestPushWebhookFindsItsConfigByRef`).

### D113. Operator metadata prefixes lie under `kontra.` — **accepted by the owner, 26 Oct**

relay.md §13.2 and [`schema/config.json`](../spec/schema/config.json) accept any non-empty
`operator_metadata_prefixes` entry. But §2.5 strips only `kontra.` keys from callers and services, and a2a-go
copies a service event's metadata into the task's: under another prefix (`acme.`), a seller could write keys that
`TaskRef.metadata` hands the Lifecycle hooks as operator-written, which §13.3 says it never holds. Decision: every
prefix lies under `kontra.` (as `caller_metadata` keys must, and as Cloud's `kontra.escrow.` does), so §2.5's
stripping vouches for every key under it; `kontra.` alone, which would cover the gate's own keys, is refused too.
The schema gets `caller_metadata`'s pattern ([`relay.md`](../spec/relay.md) §13.2; `internal/config/validate.go`,
`TestLoadErrors`).

### D114. A hook's `TaskRef` is in history as the hook's input

relay.md §13.3: the `TaskRef` is "built by the activity from Postgres (not carried in workflow history)". But a
hook is an activity the workflow schedules with `(TaskRef, args)` as its input, on the operator's queue for a
remote hook, so the `TaskRef` is in history whatever builds it. Decision: the workflow never keeps a `TaskRef` in
its state or input; a `task_ref` activity builds it from Postgres right before each hook call, and it holds only
ids, the state and the operator-written metadata (at most 16 KiB), which D59 lets cross history, and never caller
or service content, except the caller's `contextId` (D115) ([`relay.md`](../spec/relay.md) §13.3;
`internal/lifecycle/workflow.go` `taskRef`, `TestTaskRefCarriesOperatorKeysOnly`).

### D115. Two caller-chosen identifiers cross history — **accepted by the owner, 26 Oct** (amends D59)

relay.md §14 and D59: "the one caller-authored value that crosses history is a review-window message's
`caller_metadata` keys". Yet §13.3 and [`schema/hooks.json`](../spec/schema/hooks.json) put `contextId` in every
`TaskRef` (the gate context id, which is the caller's own, up to 256 characters, when it chose one), and §13.3 and
§14.1 put the caller's `messageId` in `OnReviewMessage`'s args and in `kontra.review_message`; a hook's input and
an Update's payload are in history. Replacing them (with a hash) would change the contract the operator's hooks are
written against. Decision: both are kept as the hook contract names them, and §14 lists them as the other
caller-authored values in history. The seller's own cancel error text, which also crossed history, no longer does:
`forward_cancel` returns the outcome and the upstream's JSON-RPC code only, and the caller gets the gate's text for
that code. The alternative for the owner: the hook contract carries a hash of the two
([`relay.md`](../spec/relay.md) §13.3, §14; `internal/hooks/hooks.go` `TaskRef`, `internal/lifecycle/types.go`
`UpstreamError`).

### D116. Who writes the `services` rows

relay.md §13.5: Cloud's control plane writes the `services` rows, and team gates load `services:` from
`kontra.yaml`. It does not say what a multi-tenant gate does with a `services:` block, nor what a file entry does
to a row the operator wrote. Decision: a multi-tenant gate with a `services:` block refuses to start, and a
`services:` entry naming a row the operator wrote (`source = 'db'`) is refused; nothing is written either way
([`relay.md`](../spec/relay.md) §13.5; `internal/gate/services.go` `Sync`, `TestSyncRefusesADBSourcedService`).

### D117. A service removed while its tasks run

relay.md does not say what removing a service from the file, or disabling it, does to its tasks in flight.
Decision: its card keeper stops and its routes answer as an unknown service at once; `prepare` gets an error, and a
later dispatch or `forward_cancel` of a task in flight fails the task `service_unavailable`
([`relay.md`](../spec/relay.md) §13.5; `internal/gate/services.go` `Backend`,
`TestSyncWritesTheBlockAndAuditsChanges`, `TestDeadPrepareBackendIsServiceUnavailable`).

### D118. The drain settles before it stops the other workers

relay.md §14.3 and D76 keep `kontra-lifecycle` and the control queues polling "until its last dispatch has ended",
then stop them. On a gate of one replica, stopping them the moment the last dispatch returns leaves no worker for
the workflow task that records its result, so a `kontra.cancel` waiting on it (D79) never completes. Decision: once
`kontra-direct` has stopped and the last dispatch has ended, the other workers keep polling for a 2 s settle, then
stop in D76's order. This fulfils the section's stated purpose, that dispatch results are recorded
([`relay.md`](../spec/relay.md) §14.3; `internal/lifecycle/workers.go` `Drain`,
`TestDrainSettlesThenStopsInTheDrainOrder`, `TestDrainServesCancelOnOneReplica`).

### D119. A draining replica keeps its public listener

relay.md §14.3 says a draining replica fails its readiness check, but not whether its public listener stops taking
requests; closing it at `SIGTERM` would stop callers from cancelling on a gate of one replica. Decision: the
listener keeps serving while the workers drain (`/healthz` answers `503`, which steers load balancers away) and
shuts down after them, giving open requests and SSE streams 5 s; callers resume elsewhere with `Last-Event-ID`
([`relay.md`](../spec/relay.md) §14.3; `internal/gate/gate.go` `drain`, `TestReplicaServesReloadsAndDrains`).

### D120. A stalled replica is taken over within two heartbeat rounds

relay.md §14.2 bounds the detection of a dead replica (within a minute, `replica_kill`) and says nothing of a
stalled one (`SIGSTOP`, a GC pause, a partition). A stopped process keeps its open `kontra-direct` long polls,
which the Temporal server still counts as pollers, so the first resume dispatch can be handed to the stalled
replica itself; it times out in turn, and only the second resume, once the server has ended those polls, is sure to
reach a live replica. Each stolen dispatch costs a heartbeat timeout and counts toward D42's cap of 10. Decision:
no further mechanism; D77's fence keeps the outcome right wherever the resume lands, and §14.2 states the bound: a
stalled replica's dispatch is taken over within two heartbeat rounds rather than one (measured 38–70 s;
`TestReplicaStallDirect` allows 90 s) ([`relay.md`](../spec/relay.md) §14.2; `test/e2e/replica_kill_test.go`).

### D121. a2a-go's finished-turn race

Not a conflict in the spec's text, but an a2a-go v2.7.0 behaviour the design does not cover (probe P15). If a turn
ends at `input-required` before a blocking `SendMessage`'s subscription reads the store, a2a-go answers the caller
"queue is closed" instead of the task; and when the work queue's `Write` switches a new task to Admission's
pre-minted id, a2a-go recreates the reader at the log's tail after `Write`, and the call waits forever. Decision:
when a2a-go answers "queue is closed", an interceptor answers the stored task (the one the request named, or for a
new task the pre-minted id); and the reader for a task the request itself created starts at the log's first row,
which is safe because every row of that task belongs to that turn. So §9's rule holds: a blocking `SendMessage`
returns on the first terminal, `input-required` or `auth-required` state ([`relay.md`](../spec/relay.md) §9, §15;
`internal/relay/interceptors.go` `finishedTurn`, `internal/relay/handlers.go` `newTaskFromStart`,
`TestP15FinishedTurnRace`, `TestFinishedTurnAnswersTheTask`).

### D122. `card_compat` runs against a stand-in for the 0.2.5 helloworld seller

relay.md §16: `card_compat` serves "a 0.2.5 helloworld upstream" as 1.0. The seller it names,
`examples/sellers/helloworld` (the Python a2a-samples seller), is still a skeleton, assigned to a later work
package. Decision: until it is built, `card_compat` runs against `slowagent`'s `Legacy025` stand-in: that seller's
0.2.5 card byte for byte at `/.well-known/agent.json`, a2a-go's 0.x JSON-RPC server, and a "Hello World" message
answer, as the Python seller gives. The expected result is unchanged ([`relay.md`](../spec/relay.md) §16;
`test/e2e/card_compat_test.go` `TestCardCompat`). M2 built the helloworld seller at a2a-samples `6603ba3` (a2a-sdk
1.1.0, a 1.0 card), not the 0.2.5 one, so this stands (D154).

## From building M2 (26 Oct)

Building M2 (the tunnel: work packages WP-01 to WP-08 and the reviews after them) met places where the approved
specs say nothing, contradict themselves, or say what the code cannot or should not do. The M2 plan's audit
confirmed ten of its eleven conflicts (C1–C7, C9–C11; C8, `anonymous_unavailable`, is M4's) and corrected some of
their proposals; the builders met more. Each was resolved in the code with the closest faithful behaviour, marked
`SPEC-CONFLICT` where that departs from the text. The owner said on 26 Oct "Don't ask me anything": the entries that
change documented behaviour, once marked as needing the owner, are **decided under the owner's standing instruction,
26 Oct**, each with the option it recommends, and say which. Where a proposal would add to what both binaries speak
on the wire (a handshake code, a `Kontra-Node` reason), the built behaviour is the option taken. The others settle a
silence or a contradiction. The specs now say what the code does and cite the entry
([`tunnel.md`](../spec/tunnel.md) revision 4, [`relay.md`](../spec/relay.md) revision 5), except where an entry says
the spec is unchanged.

### D123. A `--serve` target is served at its root — **decided under the owner's standing instruction, 26 Oct** (C1)

[`tunnel.md`](../spec/tunnel.md) §8.2 has the node prefix the target's base path to every request path, while the
gate calls the path of the upstream card's interface ([`relay.md`](../spec/relay.md) §6.2, §3, D71), which already
holds that base: `http://h:9/base` with interface `/base/rpc` would be called at `/base/base/rpc`. The gate cannot
strip a base it does not know without new wire protocol. Decision: M2 fails closed: `kontra connect` refuses a
`--serve` target whose path is not empty or `/` (`ErrBasePath`, naming this conflict); every example serves its
agent at the root. The node's proxy still refuses a path outside a non-root base (`400`), which only a unit test can
reach (`internal/node` `TestProxyConfines` builds a `Serve` directly), so `sidecar_not_a_proxy` checks confinement
under the root base instead: `/../../etc/passwd` reaches the target as `/etc/passwd`. Decided: the proposal. §8.2
now forwards a path already under the base as it is and prefixes any other, and says that `kontra connect` refuses a
target with a path until the node forwards by that rule; with the root base the two rules are one, so M2's refusal
stands and every example still serves its agent at the root ([`tunnel.md`](../spec/tunnel.md) §8.2;
`internal/node/serve.go`, `TestSidecarNotAProxy`). Amended by D210 (an MCP target names its endpoint).

### D124. A hello whose outcome was lost is probed; a confirmed token is never sent again — **decided under the owner's standing instruction, 26 Oct** (C2)

§5 makes enrollment tokens single use and §12 refuses a `rotate_from` naming an already rotated key
`enroll_invalid`; §4 does not retry `enroll_invalid`. A node whose welcome was lost after the gate committed
(the connection cut after the proof, the gate's own failure, D136, or the process stopped), or that restarts with
the same `--enroll` as a compose sidecar or a systemd unit does, would send a used token again and be refused for
good. Decision, the node's side within the spec's text: `~/.kontra/credentials` keeps, per gate, the sha256 of
every token a welcome confirmed (never sent again, whatever the command line) and of the tokens a hello in flight
carries. The next handshake after a lost outcome is a probe: the active key alone, no tokens, no `rotate_from`, no
API key. Its welcome means the change committed (and confirms the tokens only if it serves every declared
service); `unknown_key`, or `service_denied` for a known key, means it did not, and the full hello follows at once.
Tokens the gate refused `enroll_invalid`, and an API key it refused `auth_invalid`, are removed from the
credentials file. Residual: the node cannot tell which services a token binds, so a declared service whose binding
was revoked makes a committed probe read as not committed (its welcome leaves that service out); the full hello then
resends the used token, and its `enroll_invalid` stops the node for good. Decided: the node's probe as built, which
§2.4 now states. The alternatives that would close the residual, an idempotent admission at the gate or a welcome
that says what the hello enrolled, are not taken: each changes the gate's side of §5 and §12, while the probe needs
nothing of the gate ([`tunnel.md`](../spec/tunnel.md) §2.4, §4, §5, §12; `internal/node/run.go` `connect`,
`TestConsumedTokenIsNotSentAgain`, `TestLostWelcomeProbe`, `TestRealAdmissionLostWelcome`; end to end,
`TestTunnelLostWelcome` loses a first hello's welcome and `TestTunnelKill` restarts its node with its original
`--enroll`).

### D125. `rotate_from` at a gate that cannot carry the rotation — **decided under the owner's standing instruction, 26 Oct**

§12 has a rotated node connect with `rotate_from` naming the old key, which must be a key of this gate, neither
revoked (`revoked`) nor rotated (`enroll_invalid`), and §4 retries neither. But one key is enrolled separately at
each gate (§2.1), and the key file records which gates confirmed a rotation, not which hold the old key: a first
enrollment at a gate that never knew the old key, or a rotation after a leak followed by revoking the old key
(§12, §15), would be refused on every start. Decision: the node sends `rotate_from` to a gate that has not
settled the rotation, and when that gate refuses it `enroll_invalid` or `revoked`, sends the hello again at once
without it and does not name that key there again in this process; the hello's tokens or `enroll:` scope then
enroll the new key, and a gate that knows neither key answers `unknown_key`, which is retried. Any welcome of the
active key settles the rotation at its gate. Decided: the node's side as built, the proposal's first form in effect
(the node stops naming the old key to a gate that cannot carry the rotation), which §12 now states; the second form,
a gate that ignores such a `rotate_from`, is not taken ([`tunnel.md`](../spec/tunnel.md) §12;
`internal/node/run.go` `connect`, `TestRealAdmissionRotationAtANewGate`,
`TestRealAdmissionRotationAfterOldKeyRevoked`, `TestNodeRotateAfterLeak`).

### D126. The old key is pruned 24 h after the last gate that confirmed the rotation — **decided under the owner's standing instruction, 26 Oct**

§12 times the node's 24 h from the rotation's commit at the one gate it rotates at; §2.1 enrolls one key at each
gate the node dials, and gives the node no list of them. Decision: `nodekey.Prune` drops an old key only when at
least one gate has confirmed the rotation (`# rotated: <time> <gate>`) and every confirming gate's stamp is 24 h
old; a key no gate confirmed is never dropped. A gate the node has not redialed since is not waited for. Decided as
built, and §12 now says so; waiting for every enrolled gate, which needs that set from the credentials file, is not
taken ([`tunnel.md`](../spec/tunnel.md) §2.1, §12; `internal/nodekey/nodekey.go` `Prune`, `TestPrune`).

### D127. A new handshake of a rotated key inside its overlap is refused `revoked`

§12 keeps a rotated key valid for 24 h for the tasks already on its session, and says nothing of a new handshake
with it. Decision: such a hello is refused `revoked`: the old session keeps its tasks, but a new session of the key
could serve nothing new, since new attempts never select a rotated key ([`tunnel.md`](../spec/tunnel.md) §12;
`internal/enroll/admit.go` `refuseUnusable`, `TestAdmitRotation`).

### D128. Revoking a key does not follow its rotation — **decided under the owner's standing instruction, 26 Oct**

§15: revoking a stolen identity key ends its session and every future handshake. But a rotation needs only proof
of the old key, so a thief can rotate the stolen key to one of their own first. Following `rotated_to` on revoke
would also revoke the new key of the legitimate flow (rotate after a leak, then revoke the old key to end its
overlap early). Decision, as built and now in §12 and §15: revocation does not follow the chain;
`kontra-gate enroll --revoke` warns on stderr with the newest still-enrolled key of the chain and the command that
revokes it. Following the chain, by default or behind a flag, is not taken ([`tunnel.md`](../spec/tunnel.md) §12,
§15; `internal/enroll/revoke.go` `Successor`, `cmd/kontra-gate` `enrollRevoke`, `TestEnrollRevokeRotatedKey`).

### D129. A rotation copies every binding with its revocation — **decided under the owner's standing instruction, 26 Oct**

§12: the rotation "copies its active `key_services` rows". With only the active rows, a service whose binding was
revoked would be one the new key was never enrolled for, and the node's next handshake with its unchanged `--serve`
would be `service_denied`, final for every service. Decision, as built and now in §12: every row is copied with its
`revoked_at`; a revoked copy grants nothing but keeps §5's "left out" for the new key. A binding being per key, a
binding revocation that reaches a session of the rotated key in its overlap is that key's alone: the node stops
serving the service on that session and keeps declaring it with the active key (M2 review)
([`tunnel.md`](../spec/tunnel.md) §5, §12; `internal/enroll/rotate.go`, `TestAdmitRotationKeepsRevokedBindings`;
`internal/node/run.go` `bindingRevoked`, `TestRotatedKeyBindingRevocationKeepsTheNewKeysService`).

### D130. A revoked binding is enrolled again only by a one-time token — **decided under the owner's standing instruction, 26 Oct**

§5 leaves a revoked binding out of `welcome.services` but does not say what may enroll it again. The hello's
standing API key rides on every handshake as the hiring principal, so letting its `enroll:` scope do it would undo
the operator's revocation at the node's next connect. Decision, as built and now in §5: only a service or tenant
token in the hello re-enrolls a revoked binding, as the operator's explicit act ([`tunnel.md`](../spec/tunnel.md)
§5; `internal/enroll/admit.go`, `TestAdmitAPIKeyKeepsRevokedBinding`, `TestRevocationsReenrolledBinding`).

### D131. `kontra-gate enroll --revoke <key_id> --service <name>` revokes one binding — **decided under the owner's standing instruction, 26 Oct** (C9)

§12 defines a binding's revocation, and `key_scope` needs one, but names only the key's command, and a team gate has
no console. Decision, as built and now in §12:
`kontra-gate enroll -f kontra.yaml --revoke <key_id> --service <name>` revokes one binding
(the audit row and `NOTIFY kontra_revoke 'binding:…'` in one transaction); without `--service` it revokes the key.
It is operator CLI, not wire protocol ([`tunnel.md`](../spec/tunnel.md) §12; `internal/enroll/revoke.go`
`RevokeBinding`, `cmd/kontra-gate` `enrollRevoke`, `TestEnrollRevoke`, `TestKeyScope`).

### D132. What an enrollment authority may bind (C10)

§5 does not say what a token for a service that is not a tunnel service does, what an `enroll:` scope naming a
missing service does, or what becomes of a bound service the operator later moved to a direct backend or
disabled. Decision: minting a token for an unknown, disabled or non-tunnel service is an error and writes nothing;
an `enroll:` scope binds existing, enabled tunnel services only (a missing one is `service_denied`); only tenant
tokens create services. A bound service that is no longer an enabled tunnel service is left out of the served set,
like a revoked binding, not `service_denied`: the handshake is all-or-nothing and `service_denied` is final, so the
node would lose its other services (the audit's correction). A token that applies to no declared service is
`enroll_invalid` and, the transaction rolled back, not consumed; a bound service declared as another kind is
`kind_mismatch` ([`tunnel.md`](../spec/tunnel.md) §5; `internal/enroll/tokens.go`, `internal/enroll/admit.go`,
`TestMintServiceToken`, `TestAdmitRefusals`).

### D133. An unknown key that nothing in its hello enrolls is `unknown_key`

§4's step 6, read literally, makes an unknown key whose hello carries only an API key that cannot enroll
`service_denied`, which the node never retries; but the lost-welcome probe (D124) needs `unknown_key` to learn that
nothing committed. Decision: an unknown key that nothing in the hello enrolls is `unknown_key`, whatever else the
hello carries; `service_denied` stays for known keys and for hellos that enroll some declared services but not all
(`key_scope`'s step 2) ([`tunnel.md`](../spec/tunnel.md) §4, §5; `internal/enroll/admit.go`, `TestAdmitRefusals`,
`TestKeyScope`). Amended by D206 (a node that only hires).

### D134. A hello of the wrong shape takes its subject's code — **decided under the owner's standing instruction, 26 Oct**

§4's twelve codes are a closed list, with none for a malformed hello (a name outside 1–64 characters, a nonce that
is not 32 bytes, a field of the wrong type, a NUL) or for a binary message during the handshake. Decision: each
refusal takes the code of its subject: `bad_origin` for the origin, `bad_proof` for a key id or recipient that does
not match, `service_denied` for a bad service kind or name; a shape error with no listed subject, and a binary
message, are `unsupported_version`, which the node retries. Decided as built, and §4 now says so; the non-retryable
code proposed for a malformed handshake is not added: §4's list of codes stays closed as both binaries speak it
([`tunnel.md`](../spec/tunnel.md) §4; `internal/tunnel/handshake.go` `ParseHello`, `TestParseHelloRefusals`).

### D135. A connection that cannot be channel-bound is refused at the upgrade — **decided under the owner's standing instruction, 26 Oct**

§4 binds `tls-exporter` whenever the gate terminates TLS and has no code for a connection that cannot export (TLS
1.2 without Extended Master Secret), and the audit forbids falling back to `none` for one connection. Decision: the
gate refuses the upgrade itself, so no challenge is sent: `400` when the client's TLS has no exporter, `500` when
the gate binds `tls-exporter` on a listener without TLS. The node sees a failed dial and retries. Decided as built,
and §3 now says so; no §4 code is added for it, since the refusal comes before the handshake begins
([`tunnel.md`](../spec/tunnel.md) §3, §4, §4.1; `internal/tunnelgate/handler.go`,
`TestExporterUnavailableIsRefused`).

### D136. The gate's own failure during a handshake closes `1011` — **decided under the owner's standing instruction, 26 Oct**

§4's last message is welcome or error, and error's codes have none for the gate's own failure (Postgres
unreachable during the admission). Decision: the gate sends neither and closes with WebSocket `1011`; the node
retries it, and since the admission may have committed, probes first (D124). Decided as built, and §4 now says so;
no code is added for it, since the `1011` close already has the node retry ([`tunnel.md`](../spec/tunnel.md) §4;
`internal/tunnelgate/handler.go` `ServeHTTP`, `TestAdmitOutcomes`).

### D137. A node-opened stream is in flight until both halves end — **decided under the owner's standing instruction, 26 Oct**

§14 answers a node-opened stream over the cap (32) with `429` and keeps the session. M2 serves no hiring, so every
node-opened stream is refused at once, and a refusal frees nothing while the node keeps its half open (yamux
buffers up to a window per half-closed stream and has no reset). Decision: the gate counts a node-opened stream as
in flight from its accept until both halves have ended, and closes the session when one more than the cap would be.
M3's `429` needs the same count. Decided as built: §14 now says that a stream is in flight until both halves end, on
either side (with D157), and that M2, which serves no hiring, closes the session over the cap
([`tunnel.md`](../spec/tunnel.md) §7, §14; `internal/tunnelgate/handler.go` `acceptNodeStreams`,
`TestNodeOpenedStreamsAreCapped`, `TestNodeOpenedStreamsFreedWhenTheNodeCloses`).

### D138. Replicas tell each other of supersedes and enrollments: `NOTIFY kontra_tunnel` (C4)

§11 (a newer session of a key supersedes the older) and §5 (a service's other nodes are told of a new key) need one
replica to act on a session another replica holds; tunnel.md's `kontra_revoke` covers keys, bindings and API keys
only. Decision: a gate-internal channel, `kontra_tunnel`: `'session:<id>'` is sent in the superseding transaction,
and `'enrolled:<key_id>/<tenant_id>/<service>'` in the admission's own transaction, so the notice rides the
binding's commit and every replica, the admitting one included, sends `{"t": "enrolled"}`. The 60 s row check stays
the supersede's fallback. Registrations of one key serialize on a row lock on its `tunnel_keys` row. It is Postgres
between replicas, not tunnel wire protocol, and needs no migration ([`tunnel.md`](../spec/tunnel.md) §5, §11;
`internal/tunnelgate/registry.go`, `internal/enroll/enroll.go` `EnrolledPayload`, `TestSupersedeAcrossReplicas`,
`TestAddSerializesOneKey`, `TestEnrolledNotices`, `TestAdmitAnnouncesEnrollments`). Extended by D199 (the `mcp:` and
`tools:` payloads).

### D139. "Older" is the order of the handshakes

§11 closes "the older session". A session registers only after its card fetch (up to 10 s), so the order of the
registrations' commits can differ from the handshakes': a half-open session whose welcome was lost could register
after the node's live redial and close it. Decision: the order is the session ids' (UUIDv7, minted at the
challenge); a registration that finds a newer session of its key on a replica that is not draining inserts nothing
and closes. Within a replica the ids are monotonic; across replicas they follow the clocks, so a skew larger than
the time between two handshakes of one key can order them wrongly, and the wrongly refused session's node redials
([`tunnel.md`](../spec/tunnel.md) §11; `internal/tunnelgate/registry.go`, `TestOlderHandshakeDoesNotSupersedeNewer`;
end to end, `TestTunnelLostWelcome`: the half-open session registers 10 s after the redialed one and closes).

### D140. The end of a rotated key's overlap closes its session without `{"t": "revoked"}`

§12's Revoke sends `{"t": "revoked"}`, on which the node stops for good. A rotated key whose overlap ends also gets
`revoked_at`, but belongs to the Rotate bullet, and the node has moved to its new key. Decision: a key revoked
with reason `rotated` has its session closed without the message; every other key revocation (owner, operator,
expired, a row that is gone) sends it, then closes. On the node's side, `{"t": "revoked"}` stops the node only when
the session's key is the key file's active key: a session of a key rotated since, by this process or by another
`kontra connect` sharing the file, closes alone, and the active key's session is dialed at once (M2 review)
([`tunnel.md`](../spec/tunnel.md) §12, §13; `internal/tunnelgate/control.go`, `TestAllowsRefusals`,
`TestRevokedWithoutDatabase`; `internal/node/run.go` `keyRevoked`, `TestRevokedRotatedKeyBeforeTheMaintainTick`).

### D141. The drain tells the nodes only after `draining_at` commits

§13's `{"t": "drain"}` makes the node dial a new session at once, and §11 keeps two sessions of a key only while
one is on a draining replica: a node told before the replica's `gate_replicas.draining_at` commits would supersede
its own session there and cut its tasks. Decision: the drain message is sent once the mark has committed; the write
is retried every second, each attempt within 5 s (`DrainMarkTimeout`), and abandoned once the workers have drained,
in which case the nodes are not told and their sessions close with the replica ([`tunnel.md`](../spec/tunnel.md)
§11, §13; `internal/gate/gate.go` `markThenTellTunnels`, `TestDrainTellsTunnelsAfterDrainingAtCommits`;
`TestTunnelDrain` holds the draining replica's row across its SIGTERM, and fails if the node is told while
the write waits on it).

### D142. One card per tunnel service (C11)

[`relay.md`](../spec/relay.md) §3 keeps one upstream card per service, while several nodes, each with its own
target, may serve one tunnel service (§6.2, tunnel.md §11), and the dispatch reads its interface from that card
whichever session runs it. Decision: the nodes of one service are expected to serve one card; the last good fetch
through any session is the service's card, and a fetch whose bytes differ from the card served is logged as a
warning. §3's 30 minutes are read literally: a tunnel service none of whose nodes connected for 30 minutes is
unavailable, and becomes available again as soon as a session connects (its card is fetched before the session
becomes a candidate). §3 now says that a service's nodes serve the same card ([`relay.md`](../spec/relay.md) §3;
`internal/gate/services.go` `newTunnelKeeper`, `TestTunnelCardKeepers`).

### D143. A task placed before a tunnel service's first card is retried — **decided under the owner's standing instruction, 26 Oct**

§3 fetches a tunnel service's card when a session for it connects, and §6.4 fails a task whose card is unavailable
`service_unavailable`, with no retry. A node started before its agent (a sidecar's order) has a session whose card
fetch failed (`502 Kontra-Node: target_unreachable`); withholding the session would take down the node's other
services, the handshake being all-or-nothing. Decision (the audit's second option for its gap 3): the session
becomes a candidate anyway, its card is retried on the keeper's backoff (1 s doubling to 1 min), and a dispatch for
a tunnel service whose keeper has no usable card is retried like `no_session` (`relay.ErrCardPending`), failing
`service_unavailable` only once `task.retries` are spent. No usable card is none yet, or only one whose last good
fetch is older than the keeper's 30 minutes: a node back after a longer absence, its agent not up yet, is in the
same race (M2 review). That dispatch has the card fetched again at once (at
most once a second), never later than the fetch already due: a dispatch that only restarted the backoff left an
agent that came up after its node to a refetch up to a minute away, and the task failed meanwhile. Decided as built,
with the M2 review's extension to a card older than 30 minutes; §3 and §6.4 now say so
([`relay.md`](../spec/relay.md) §3, §6.4; `internal/gate/services.go` `tunnelConnected`, `runTunnelCard`,
`TestTunnelBackend`, `TestTunnelCardPendingFetchesAtOnce`, `TestTunnelExpiredCardIsPending`; end to end,
`TestTunnelCardPending`, whose task is sent as its node's agent comes up).

### D144. `sealed:` and `seller_keys` on a tunnel service are refused — **decided under the owner's standing instruction, 26 Oct** (amends D90, D91)

D90 and D91 accepted `seller_keys` and `sealed:` on tunnel services with a warning while M1 did not serve them. M2
serves tunnel services and neither seals nor verifies seller signatures (M3), so both would fail open. Decision:
either key on a tunnel service refuses to start, and a row with it is left out of the registry. Consequence: the
spec's team fixture, `spec/schema/examples/config.team.json` (`sealed` on `recon` and `fs`, both tunnel services),
no longer loads; `TestTeamFixtureLoads` asserts exactly those two refusals. Decided as built. The spec is unchanged:
this is M2's scope, as D90 was M1's, and the work that builds sealing and verification lifts both refusals
([`relay.md`](../spec/relay.md) §3, §17; `internal/config/validate.go`, `internal/gate/services.go` `refusable`).

### D145. No capacity skip in the session choice (C5)

[`relay.md`](../spec/relay.md) §6.1 and §14.2 skip replicas at their dispatch capacity, but 0001 records no
replica's dispatches in flight, `prepare` runs on any replica, and `attempts` has no index on `replica_id`.
Decision: M2 does not skip by capacity; load is bounded by each replica's `kontra-tunnel-<id>` worker
(`limits.dispatch_slots`) and by §6.4's schedule-to-start row. The round-robin among candidates is per replica.
A capacity-aware choice waits for a column written with the replica's heartbeat ([`relay.md`](../spec/relay.md)
§6.1, §14.2; `internal/tunnelgate/picker.go`, `TestPickerCandidates`).

### D146. `no_session` attempts count against `task.retries` — **decided under the owner's standing instruction, 26 Oct** (C6)

§6.4's `no_session` row ("retry after 1 s, 2 s, 4 s … capped at the deadline; failed: no_session when exhausted")
does not say what is exhausted. Decision: read with the section's opening sentence, `task.retries` bounds the new
attempts, and a `no_session` attempt is one: with the default 2, a task sent while its only node reconnects fails
`no_session` within about 3 s. Decided as built, and §6.4 now says so; the alternative, waiting for a node until the
deadline, is not taken ([`relay.md`](../spec/relay.md) §6.4; `TestNoSessionRetriesThenFails`, `TestKeyScope`).

### D147. A `full` answer after the first upstream id is retried for 30 s (C7)

§6.4's tunnel row retries only a poll refused `Kontra-Node: full` after the first upstream id; a follow-up's
`SendMessage` or a stream refused so would read as "any other: failed: `tunnel_lost`", although `full` proves the
node forwarded nothing. Decision: before the first id, `full` is a retryable `tunnel_lost` (another session); after
it, any exchange refused `full` is made again on the poll's backoff (1 s doubling to 5 s) for at most 30 s (D97's
grace), then fails `tunnel_lost`. `forward_cancel` retries a `full` answer for up to 20 s, after which the cancel
counts as not delivered ([`relay.md`](../spec/relay.md) §6.4, §8; `internal/relay/executor.go` `nodeFull`,
`TestTunnelNodeFullWhilePolling`).

### D148. The cancel of a tunnel task whose session is gone fails it `tunnel_lost` — **decided under the owner's standing instruction, 26 Oct** (C3)

§8's row "active or `input-required` | exists, session gone" answers "the task as it is after that (failed:
`tunnel_lost`)", assuming a dispatch that is failing. For a task at `input-required` no dispatch runs, and M1's
workflow answered a `forward_cancel` that failed, or did not start within 10 s, with "the task continues". D77 would
also end a waiting attempt `interrupted`. Decision (the audit's accepted proposal, as built and now in §8 and
§14.2): `forward_cancel` answers `tunnel_lost` when this replica does not hold the session, or the session no longer
serves the service, with no stream opened; for a tunnel attempt that, or a `forward_cancel` not started within 10 s
on `kontra-ctl-<session replica>`, makes the workflow write `failed: tunnel_lost` (the attempt's outcome
`tunnel_lost`, kept as it is), behind `GetVersion` `cancel-tunnel-lost`, and the caller gets that task. A node's own
answer to a cancel on a live session (`target_unreachable`, `not_hosted`, `kind_mismatch`) is §8's "session up"
row: not delivered, `TaskNotCancelableError`, and the task continues; §6.4's "after the first id: failed" covers the
dispatch's own exchanges only ([`relay.md`](../spec/relay.md) §8; `internal/lifecycle/workflow.go`,
`internal/relay/dispatcher.go` `ForwardCancel`, `TestCancelWhenTheTunnelSessionIsGone`,
`TestTunnelForwardCancelSessionUp`, `TestCancelForwardedTunnel`).

### D149. A dispatch whose session is gone

§6.4 fails a tunnel attempt whose session is lost `tunnel_lost`, retried before the first upstream id. Decision:
before the first id the dispatch returns `tunnel_lost` at once, opening no stream and logging nothing, and the
next attempt avoids that session; after it, the dispatch itself logs `failed: tunnel_lost` under its fence, still
opening no stream, so an attempt that waited at `input-required` ends `tunnel_lost`, not `interrupted`
([`relay.md`](../spec/relay.md) §6.4, §7; `internal/relay/executor.go`, `TestTunnelSessionMissing`,
`TestTunnelLostRetriesOnAnotherSession`, `TestInputRequiredSticky`).

### D150. The tunnel worker's drain

The M2 plan gave `kontra-tunnel-<id>` a `WorkerStopTimeout` of `drain_timeout`; §14.3 keeps the per-replica queues
polling until their last dispatch ends or `drain_timeout` passes, and then stops them with "a few seconds". With
`drain_timeout` as the stop timeout a drain could wait twice. Decision: the spec over the plan: the worker stops
within 5 s, after the drain has waited on a count of the tunnel dispatches in flight until it is zero or
`drain_timeout` has passed since the drain began; a dispatch cut at `drain_timeout` reports it. The worker polls
until it stops, so a follow-up can start during the settle: the drain waits for it and settles again, and once no
dispatch runs after a settle it closes the queue, so that a dispatch the worker still starts before its stop
returns `context_lost` at once, having sent nothing, instead of being cut 5 s later after it delivered the
follow-up (M2 review; [`relay.md`](../spec/relay.md) §14.3; `internal/lifecycle/workers.go`,
`TestDrainWaitsForTunnelDispatches`, `TestDrainWaitsForAFollowUpThatStartsDuringTheSettle`, `TestTunnelDrain`).

### D151. A `400` the node writes carries no `Kontra-Node` — **decided under the owner's standing instruction, 26 Oct**

§8.2's node-written answers carry `Kontra-Node` with a reason from a closed list (`not_hosted`, `kind_mismatch`,
`full`, `target_unreachable`, `sealed_rejected`), and the same section has the node answer `400` to `CONNECT`, an
`Upgrade`, `TRACE`, a path outside the base and an unreadable preamble, which no listed reason fits. A new value
would change the wire, and the gate reads unknown values as `tunnel_lost`, retried elsewhere. Decision: those `400`s
carry no `Kontra-Node`, so the gate takes them as the service's own `400` (`upstream_error`, not retried), which
suits requests no gate sends. Decided: the proposal's second form, the rule that a node-written `400` carries no
`Kontra-Node` and is final, which is what the node does and what §8.2 now says; no reason such as `refused` is added
to the closed list ([`tunnel.md`](../spec/tunnel.md) §8.2; `internal/node/proxy.go`, `TestProxyRefusesMethods`,
`TestSidecarNotAProxy`).

### D152. `kontra connect` reads `KONTRA_KEY` only — **decided under the owner's standing instruction, 26 Oct**

PRD §6 has `KONTRA_GATE`, `KONTRA_AGENT`, `KONTRA_TO` and `KONTRA_ENROLL` mirror the flags of `kontra tunnel`, which
note 1 replaced with `kontra connect`; tunnel.md, the authority, names only `KONTRA_KEY` (§2.1). Decision, as built:
the mirrors are dropped: `kontra connect` reads `KONTRA_KEY` and `HOME`; a container passes flags, and secrets as
`--enroll @file`. The spec is unchanged: §2.1 already names `KONTRA_KEY` alone ([`tunnel.md`](../spec/tunnel.md)
§2.1; `internal/node/node.go`).

### D153. Rotation needs a file lock

Rotate, MarkRotated and Prune rewrite the identity file, which another `kontra connect` may be rewriting (one
process per gate, one key file); `flock(2)` serializes them. Decision: on platforms without it (Windows, plain
Solaris) those three refuse with an error rather than rewrite the file unlocked; loading and creating a key work
everywhere. The specs name no node platforms ([`tunnel.md`](../spec/tunnel.md) §2.1, §12;
`internal/nodekey/lock_other.go`).

### D154. The named tests M2 runs

Like D80 for M1. [`tunnel.md`](../spec/tunnel.md) §16 and [`relay.md`](../spec/relay.md) §16 name the tunnel tests
without saying which are M2's. Decision: M2 runs `tunnel_kill` (with its before-the-first-upstream-id variant),
`input_required_sticky`, `cancel_forwarded` through the tunnel (a working task, a seller that talks after its cancel,
an `input-required` task, a seller unreachable behind a live node, and a node gone), `key_scope`,
`sidecar_not_a_proxy`, `replica_kill`'s tunnel variant (after and before the first upstream id), the
make-before-break drain, and the helloworld seller through the tunnel, plus the rotation after a leak at the binary
level, a welcome lost after the gate committed (D124, D139), a stream refused while the revocation view is stale, and
a task placed before a tunnel service's first card (D143). The harness puts `public_url` on a TCP front the test
routes to either replica, runs nodes as `kontra connect` processes, and uses two in-process doubles: a node that
ignores the control stream (`key_scope`: the gate never relies on the node honouring a revocation) and a gate with a
fixed admission (`sidecar_not_a_proxy`, no Postgres or Temporal). `sidecar_not_a_proxy` checks confinement under the
root base (D123), and reads the node's own sockets through `/proc/<pid>/fd`, since `/proc/<pid>/net/tcp` lists the
whole network namespace. The helloworld test runs the a2a-samples seller at `6603ba3` unmodified and is skipped,
saying why, only without `uv`, the pinned checkout (`$KONTRA_A2A_SAMPLES` or a clone) or a free `127.0.0.1:9999`;
with `KONTRA_REQUIRE_HELLOWORLD` set it fails instead, as do `run.sh`'s guards (examples/sellers/helloworld), and
CI's e2e and integration jobs set it, install `uv` and fetch the pinned checkout first, so that CI never passes
without running them (M2 review; `internal/ci` `TestHelloworldRunsInCI`).
Only the `6603ba3` seller is built (WP-07), so D122 and `card_compat`'s stand-in for the 0.2.5 seller stay as they
are. `anon_limits`, `local_rebinding`, `stdio_roots` and the MCP tests come with M3 and M4 (`test/e2e`).

### D155. The gate bounds a node's pings, and every write has a deadline — **decided under the owner's standing instruction, 26 Oct**

§7 sets yamux's knobs and says `AcceptBacklog` bounds what a hostile peer can pin, but yamux v0.1.2 answers each
ping on a goroutine of its own that waits up to `ConnectionWriteTimeout` (15 s) to queue its pong, and its send
loop writes with no deadline: a node that floods pings while it stops reading pinned a goroutine for every 12
bytes it sent, enough to take a replica's memory and every tenant's sessions with it. Decision: the gate reads
each session's frames through a token bucket of ping SYNs, 16 at once and one more every 500 ms (a node pings
once per `ping_s`, at least 1 s, §11, and waits for each pong), and closes the session over it; and every write
to the WebSocket, on both sides, has `ConnectionWriteTimeout`, so a peer that stops reading fails the send loop and
closes the session within 15 s. Decided: the proposal: §14 lists the ping budget among the per-session limits, and
§7 the write deadline ([`tunnel.md`](../spec/tunnel.md) §7, §11, §14; `internal/tunnel/guard.go`,
`internal/tunnel/mux.go`, `TestGatePingBudget`, `TestStalledPeerClosesTheSession`).

### D156. yamux's log lines are rate-limited per session

yamux v0.1.2 logs a line for every frame of a stream it does not hold, and the session's logger adds its
attributes, so one node could make the gate write about 25 bytes of log for each byte it sent. Decision: a
session's yamux lines pass 10 at once and one more a second; the first line over that says lines are being
suppressed, and the next line let through is preceded by the count suppressed. The frames themselves are
read and dropped as yamux always did ([`tunnel.md`](../spec/tunnel.md) §7; `internal/tunnel/guard.go`,
`TestYamuxLinesAreRateLimited`).

### D157. A gate-opened stream is counted until both halves end — **decided under the owner's standing instruction, 26 Oct**

D137 counts a node-opened stream until both halves end, but nothing bounded the streams the gate opens, closes,
and the node does not: yamux v0.1.2 keeps such a stream, and buffers up to a window (256 KiB) of what the node
still sends on it, for `StreamCloseTimeout` (5 min). A node that answers every exchange and never closes its half
pinned a window per stream, at the rate the gate opens streams to it (a poll every 5 s per task). Decision: once
the gate closes its half (or aborts the stream) it reads and discards what the node sends until the node's half
ends, its close timeout passes or the session ends, and counts the stream until then; a session with more than
256 such streams (`GateMaxHalfClosed`, four times the node's default in-flight cap) is closed. A node closes its
half when it has written its response, or at once when the gate's half closes, so only one that keeps them open
reaches the cap. Decided: the proposal, with D137's: §14 now says that a stream is in flight until both halves end,
on either side, and gives this cap ([`tunnel.md`](../spec/tunnel.md) §7, §14; `internal/tunnelgate/session.go`
`linger`, `TestHalfClosedStreamsAreCapped`).

## From M3 planning (26 Oct)

M3 builds sealing and MCP hosting (PRD §14's M3 row as the owner restated it on 26 Oct: envelope v1, `kontra seal`,
card re-signing with a JWKS, the claim-check bucket; `/mcp/{name}` with session affinity, `TaskLifecycle` for
task-augmented calls, per-tool policy and the stdio bridge). Planning it met the points below. The owner said on
26 Oct "Don't ask me anything": the entries marked **decided under the owner's standing instruction, 26 Oct**
settle open points with the recommended option and change the specs where they say so; the others record the
owner's own decisions (kontra-cloud#60) where they land. The 2026-07-28 evidence is the MCP specification at tag
`2026-07-28` (`5f5440bb`) and the tasks extension at `v0.2.2` (`5246bc3d`), pinned in [`relay.md`](../spec/relay.md) §0.

### D158. A request's era is read from its method and its `MCP-Protocol-Version` — **decided under the owner's standing instruction, 26 Oct** (carries out D24)

D24 has the gate serve 2025-11-25 and 2026-07-28 on the same endpoints and asked for the text before M3 builds the
relay; it left open how one endpoint tells the eras apart, and what happens when the upstream speaks only one.
2026-07-28 has a dual-era server select "from how the client opens" (an `initialize` is legacy, per-request `_meta`
is modern; Versioning, "Backward Compatibility"), requires `MCP-Protocol-Version` on every modern POST, equal to
`_meta`'s version (Streamable HTTP, "Protocol Version Header"), and has a dual-era client fall back to `initialize`
on a `400` whose body is not a recognized modern error. Decision ([`relay.md`](../spec/relay.md) §11.12):
`initialize` is legacy whatever its headers; `MCP-Protocol-Version: 2026-07-28` is modern, and its `Mcp-Session-Id`
and `Last-Event-ID` are ignored; a legacy or absent header needs a session, and without one gets a `-32000` body,
as legacy servers send: `400` with `-32000` `Bad Request: No valid session ID provided` (the text of
`server-everything@1.0.0`'s `GET` handler, `dist/transports/streamableHttp.js:143-151`; its `POST` without a
session goes to a new SDK transport, lines 87-111, which answers its own `-32000` body and is never closed, D176);
any other version is `-32022` listing the three the gate speaks. The gate never translates between eras: a modern
request goes upstream as it came, and a legacy-only upstream's own `400` reaches the client, which falls back. A
service is modern exactly when its upstream is, and no translation between server-to-client requests and multi
round-trip results is built. The gate validates `Mcp-Method` and `Mcp-Name` against the body (`-32020`) because it
processes the body; policy still reads the body only (research brief C15). Amended by D176 (the version checks) and
D177 (the service's era).

### D159. Modern clients get gate-owned tasks through the tasks extension; the gate decides, and answers in line when the task ends within 2 s — **decided under the owner's standing instruction, 26 Oct** (carries out D24)

D24 makes the `io.modelcontextprotocol/tasks` extension the durable surface for modern clients. The extension has no
per-request opt-in and 2026-07-28 removed `Tool.execution`: "the server is the sole decider" (ext-tasks
`tasks.md`, "Capability Negotiation"), and a `CreateTaskResult` must not be returned before the task is durable.
§11.5's step 0 (`execution.taskSupport`) therefore has nothing to read, and note 1's default ("never run the task
shape over a plain upstream call") must be kept. Two shapes were weighed. Adopting the upstream's task after the
fact would start work before Admission, so a quote (D45) could never precede it. Deciding up front keeps Admission
first. Decision: on a `durable` service whose upstream declared the extension in `server/discover`, every modern
`tools/call` that declares the extension becomes a gate-owned task (Admission `task`, a quote possible), and the
gate waits up to 2 s before answering: a task that ended is answered with its outcome in line, the client never
seeing a task; one that did not, or a quoted one (D21), is answered `CreateTaskResult` with `pollIntervalMs` 1000.
The dispatch speaks to the upstream in the era of the request that created the task (the modern dispatch polls
upstream `tasks/get`, sends `tasks/update`, and handles an upstream `InputRequiredResult` the same way), and the log
stays era-neutral: an input request is logged once per upstream key under the gate id `k.<taskId>.<seq>`, so either
era can read the task. `tasks/update` logs answers once each and signals `kontra.input_response`; `tasks/cancel`
sends `kontra.cancel` without awaiting it (D57 still writes `canceled` within 10 s); `tasks/result` and
`tasks/list` are `-32601`, as the extension requires. A terminal task is presented to a modern reader by its
terminal event: a stored `CallToolResult` is `completed` even with `isError: true` (the extension forbids `failed`
for it), a JSON-RPC error or a gate failure is `failed`. Consequence: every such call on such a service starts a
workflow; an operator who wants plain calls sets `tasks: off` ([`relay.md`](../spec/relay.md) §11.12). Amended
by D178 (a client that leaves cancels; the first upstream id of a multi round-trip call; what such a task keeps).

### D160. `per_session` for modern clients is a virtual session per principal and service — **decided under the owner's standing instruction, 26 Oct** (carries out D24)

D24 redefines `per_session` for modern clients as "per principal per service per period" without saying what the
gate records. `per_session` prices carry `idleTimeoutSeconds` ([`schema/price.json`](../spec/schema/price.json)),
and Admission has a `session` operation. Decision: the gate keeps one `mcp_sessions` row per service and principal
for modern requests, `protocol_version = '2026-07-28'`, keyed by the hash of a random id it never sends, with no
upstream or tunnel ids; it is opened by the principal's first modern request (after Admission `session`), closed
`idle` after `session_idle_s`, and named by its `calls` rows. It pins nothing ([`relay.md`](../spec/relay.md)
§11.12). Amended by D177: 0001's columns hold the row, but one open row per principal and service needs a unique
index (migration 0002), and the row opens only after the upstream answered a modern request.

### D161. What modern clients are not offered in v4.0 — **decided under the owner's standing instruction, 26 Oct**

Four parts of the modern surface have no faithful mapping yet. A `passthrough` task's later `tasks/get` would need
routing to the node holding the upstream task, with no session to pin it to; the gate removes the extension from
those services' requests and discovery, so their modern clients get plain calls. `notifications/tasks` would make
the gate write into a relayed `subscriptions/listen` stream (§11.7 forbids it); `taskIds` is removed and clients
poll. A stdio child speaks one era per process, and the bridge (tunnel.md §10) is built for 2025-11-25's
session-per-child; it answers a modern request with the legacy `400`, so dual-era clients fall back. Sealed services:
D162 ([`relay.md`](../spec/relay.md) §11.12, [`tunnel.md`](../spec/tunnel.md) §10).

### D162. Sealed MCP is legacy-era only in envelope v1 — **decided under the owner's standing instruction, 26 Oct**

[`ext-sealed.md`](../spec/ext-sealed.md) §3 binds a sealed MCP call to `session` (the hash of the gate
`Mcp-Session-Id`) and `messageId` (the JSON-RPC id, unique within that session), and §5 keeps per-session replay
state. 2026-07-28 has neither a session nor ids unique beyond a connection, and its `Mcp-Param-*` headers would
copy argument values outside the envelope. Decision: a sealed MCP service is served to 2025-11-25 and 2025-06-18
requests only; a modern request to it gets the legacy `400` (D158), so a dual-era client falls back and the
guarantee of D31 and D62 holds unchanged. The hiring node never sends `Mcp-Param-*` for a sealed call. A modern
binding (a per-call nonce in place of `session`) is an envelope v2 question.

### D163. A tunnel session held by another replica is reached through a peer hop — **decided under the owner's standing instruction, 26 Oct**

[`relay.md`](../spec/relay.md) §11.2 routes every later request of a pinned MCP session "to the same tunnel
session", but only the replica holding the socket can open a stream on it ([`tunnel.md`](../spec/tunnel.md) §11),
and A2A reaches it through Temporal (per-replica queues), which a plain MCP request must not use (no durability,
and events never in history). The spec named no mechanism, so on a gate of several replicas a client routed to a
replica other than its session's could not be served. Decision: each replica may run a peer listener
(`peer.listen`, advertised in a new `gate_replicas.peer_addr` column) that accepts one route: an MCP exchange for a
tunnel session it holds, carried with an assertion of the principal, the session, the service and the preamble's MCP
fields, a nonce and the time, under HMAC-SHA256 with a shared secret (`peer.secret_env`) over the assertion, method,
path and body hash; refused after 30 s or on a seen nonce. The holding replica only checks §6.1 and runs the
exchange; the receiving replica applies every other rule once. Without `peer.listen` only local sessions are
candidates, which is all one replica needs. The listener is a new internal surface: it never serves the public
routes, and on Kubernetes the gate's egress policy (443 only, kontra-cloud#60) must allow the peer port within the
namespace ([`relay.md`](../spec/relay.md) §11.2, §17). Amended by D179 (what the HMAC covers, `replica_id`, more
than one replica) and D183 (this repository's chart).

### D164. No local console — **decided by the owner, 26 Oct** (kontra-cloud#60)

Note 2 put a read-only console on `:7778`, served by `kontra connect`, and [`ext-sealed.md`](../spec/ext-sealed.md)
§7 showed recipient changes on it. The owner's final decisions on 26 Oct: no local console; the CLI is `connect`,
`login`, `seal` and `publish`. Decision: the node serves no port but `:7777`; a pin change is named on `kontra
connect`'s stderr and in its log and waits for `--accept-change <service>`; `local_rebinding` covers `:7777`
([`ext-sealed.md`](../spec/ext-sealed.md) §7, [`tunnel.md`](../spec/tunnel.md) §9, §16; `cmd/kontra` drops
`console`).

### D165. Sealing needs an A2A 1.0 upstream — **decided under the owner's standing instruction, 26 Oct**

The hosting node replaces sealed parts in whatever the gate forwards and seals every part of every reply
([`ext-sealed.md`](../spec/ext-sealed.md) §5), so it must parse each A2A binding and version the dispatch can
speak. The dispatch prefers 1.0 JSON-RPC, then 1.0 HTTP+JSON, then 0.3 (`internal/upstream` `pickInterface`); 0.3
parts have another shape (`kind`, `file.bytes`, `mimeType`) and predate extensions. Decision: a sealed service whose
upstream card offers no 1.0 interface is unavailable as a seller-key failure is (card routes `503`, tasks
`service_unavailable`, [`relay.md`](../spec/relay.md) §3), so the hosting and hiring nodes handle 1.0 JSON-RPC and
1.0 HTTP+JSON only, unary and SSE. The hiring side sees only 1.0 anyway: the gate serves 1.0 only.

### D166. The card-signing key is a PEM file; the first key signs, the rest stay published — **decided under the owner's standing instruction, 26 Oct**

PRD §8 and §11: `signing_key: file:./kontra.key`, "or Scaleway Key Manager in Cloud", rotated by `kid` "with
overlap"; [`relay.md`](../spec/relay.md) §3 signs ES256 and serves `/.well-known/jwks.json`; neither says what a
gate without a key does. Decision: `signing_key: file:<path>` names a PEM file of ECDSA P-256 private keys; the
first signs, every key is in the JWKS (`kid` = RFC 7638 thumbprint, `Cache-Control: max-age=300`), so a rotation is
"prepend the new key, deploy, drop the old one after the cache and the card refresh have passed". Every replica
reads the same file. Without the key a gate whose `public_url` is loopback signs with a key minted at start (one
replica, development) and logs it; any other gate serving A2A services refuses to start, since an unsigned card is
one `kontra connect` refuses to serve locally ([`tunnel.md`](../spec/tunnel.md) §9). A KMS reference is Cloud's
(M5). The JCS is `github.com/gowebpki/jcs` v1.0.2 behind `internal/jcs`; the JWS is the gate's own (D13), checked
against a2a-go's verifier (`card_passthrough`). `services.card_hash` is now filled. Amended by D183: the
repository's chart mounts the key.

### D167. The claim-check bucket is S3-compatible, its refs live in the parts and a `blobs` table, and its objects die with their task — **decided under the owner's standing instruction, 26 Oct** (lifts D104)

[`relay.md`](../spec/relay.md) §10.4 names `blobs:` and presigned GETs, and has the event row carry
`payload_ref`, but says nothing of the store's API, the object names, who deletes objects, or how a blob stream
finds a ref's task ([`tunnel.md`](../spec/tunnel.md) §8.3). One `payload_ref` column also cannot name the several
large parts one message or artifact may hold. Decision: `blobs: s3://<bucket>[/<prefix>][?endpoint=…&region=…]`
through the S3 API (`minio-go` v7; Scaleway Object Storage in Cloud, any S3-compatible store for a team gate),
credentials from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. A part over 256 KiB is stored as an object whose
key is `<prefix>/<tenant>/<task>/<ref>` (`ref` 128 random bits) and logged in the event's payload as a `url`
part `kontra-blob:<ref>`, which is a name, not a URL anyone dials; a new `blobs(ref, tenant_id, task_id, size,
created_at)` table (migration 0002, cascading with the task) maps each ref to its task; `payload_ref` stays
unused. Reads render the part per reader: a presigned GET valid 15 minutes for a caller, a push receiver or a
direct backend, `kontra-blob:<ref>` for a node. A blob stream's ref is checked against the session asking: a task
one of its attempts serves, or one its hiring principal's buyer owns. Retention deletes a task's objects (by
prefix) before its rows. The bucket carries A2A parts only: MCP has no parts, and its bodies stay under the edge's
body limit. Without `blobs:` the gate behaves as D104 says; with it, D104's refusal is lifted. Amended by D181
(`kontra-blob:` is reserved; the blob stream's framing).

### D168. The team-gate compose comes with M3, and the pentest runs on it — **decided by the owner, 26 Oct** (the M3 goal; amends D43's milestone and PRD §14's "VPS")

PRD §14 runs M3's pentest "against a VPS deployment with no proxy in front"; D43 placed the compose in M4. The
owner's M3 goal: "PRD §12 pentest list run on a compose team gate". Decision: M3 adds `compose.yaml` as D43
describes it (the gate built from this repository's `Dockerfile`, `temporalio/server:1.32.1` with its dynamic
config and the two `admin-tools` jobs, Postgres 16, no Temporal or Postgres port published), with the gate
terminating TLS itself from a provided certificate, as on a VPS with no proxy. The pentest drives it from the host:
nodes, sellers and attackers are host processes, as a team's machines would be. M4 runs it from a clean VPS.
Amended by D186 (what the pentest overlay publishes on this host).

### D169. "Escrow dispute after release" is tested at the hook boundary — **decided under the owner's standing instruction, 26 Oct**

PRD §12's pentest list ends with "escrow dispute after release". The open-source gate has no money code (PRD §13);
what it owns is the review window ([`relay.md`](../spec/relay.md) §13.3, D106): a review-window message is accepted
only while the window the Lifecycle hook opened is open. Decision: the pentest runs a test Lifecycle worker on its
own queue (`hooks.lifecycle.task_queue`) whose `ReviewWindow` is 5 s, and asserts that a dispute message sent after
the window closed is refused and reaches no hook. The money side of the item is Cloud's, tested in M5. Amended by
D186: the worker reaches Temporal through the pentest overlay, and M3 lifts the load-time refusal of
`hooks.lifecycle`.

### D170. What M3 leaves to M4 and M5 — **decided under the owner's standing instruction, 26 Oct**

The owner's plan (26 Oct) puts the gate's own `/mcp` with `hire` and URL elicitation, anonymous sessions, the pinned
TCK and the README demo in M4, and Cloud's JWTs as principals in M5. Decision: M3 does not build
[`relay.md`](../spec/relay.md) §12 (`/mcp` answers as an unknown route until M4), `kontra login`, `--public` and
anonymous sessions ([`tunnel.md`](../spec/tunnel.md) §6, `anon_limits`), nor the MCP OAuth resource server
(§12.1, `auth.mcp_oauth`), which comes with the OIDC principals in M5; until then `/mcp/{svc}` accepts API keys and
node principals, and `auth.mcp_oauth` keeps its start-time note. M3 builds direct `mcp: {url}` backends as well as
tunnel ones: the relay is the same, and direct backends never hop. D225 adds what the M3 decisions leave to build.

### D171. The named tests M3 runs — **decided under the owner's standing instruction, 26 Oct**

Like D80 and D154. The gate tests the owner named: `sealed_no_plaintext` (an A2A task, a plain MCP call and a
durable MCP call whose upstream asks one elicitation, each carrying a marker; the marker is absent from every table,
both replicas' logs at debug level, the Temporal histories and the bucket's objects), `sealed_replay`,
`sealed_unsealed_part`, `mcp_session_cross_node`, `mcp_tool_denied` and `mcp_task_resume` (both variants). With them:
`card_passthrough`, `mcp_batch_refused`, `mcp_meta_spoof`, `mcp_session_revoked`, `stdio_roots`, `local_rebinding`,
`mcp_modern_task`, `mcp_era_fallback` and `mcp_peer_hop`, and a sealed part over 256 KiB through the bucket. The
stock upstreams are `@modelcontextprotocol/server-everything@1.0.0` and `server-filesystem@1.0.0` (Node 22); both
2.0.0 releases need Node 24, so the modern-era tests run against an in-repository dual-era upstream double, and
against `server-everything@2.0.0` too, skipped saying why, when Node 24 is not installed. `anon_limits` and the TCK
tests are M4's. Amended by D172 (`mcp_case_smuggling` and `a2a_case_smuggling` join them) and D173 (`stdio_roots`
asserts the node's roots).

## From the M3 plan's audit (26 Oct)

An adversarial audit of the M3 plan, its spec text and its skeleton found five blockers (D172–D176) and smaller
problems in all three. The owner's standing instruction ("Don't ask me anything") covers them: the five blockers
were settled as recorded below, and the rest with the audit's proposals, changing the specs where the entries say
so. The builders' corrections to the plan follow from these entries.

### D172. JSON-RPC bodies are decoded with exact keys, and a repeated or case-variant key is `-32600` — **decided under the owner's standing instruction, 26 Oct**

The skeleton's `mcpwire.Message` decoded through `encoding/json` struct tags, which match keys case-insensitively
and keep the last of a repeated key. On go1.26.10, `{"method": "tools/call", "METHOD": "ping"}` read as `ping` and
`{"name": "echo", "NAME": "add"}` as `add`, while Node upstreams (`JSON.parse`) and go-sdk v1.8.0 (which turns case
folding off on purpose, `internal/json/json.go:32`) read `tools/call` and `echo`. The relay forwards the bytes it
received ([`relay.md`](../spec/relay.md) §11.2), so a denied tool would run with no `calls` row, and a seller
answering `{"isError": true, "ISERROR": false}` would be metered as a success. The A2A JSON-RPC route has two
readers as well: `internal/a2ahttp` reads the method (to route streams), `params.tenant` and the id, and a2a-go
decodes the body again. Decision: the gate decodes every JSON-RPC body it acts on with exact, case-sensitive keys,
and refuses a message in which a key repeats, or two keys are equal under Unicode case folding, at a level it reads
or rewrites. On MCP, from a client: the message, `params`, `params._meta`,
`params._meta["io.modelcontextprotocol/clientCapabilities"]` and its `extensions`, `params.capabilities`, the top
level of `params.arguments` and of `params.inputResponses`, `result` and `result._meta`. On MCP, from an upstream:
the message, `params` and `params._meta` (of requests and notifications), `result`, `result._meta`, each of
`result.tools` and its `_meta`, `result.capabilities` and its `experimental` and `extensions`, the task objects it
reads (`result.task`; `result.inputRequests`, each of its entries and their `params` and `params._meta`;
`result.result` and `result.error`), and `error`. On A2A JSON-RPC: the request, `params`, `params.metadata`,
`params.configuration`, `params.message`, `params.message.metadata`, each part and each part's `metadata`. (The
first text named only the levels that route and meter; the revision check of the M3 planning extended them to the
levels the gate reads or rewrites, §2.5's scrub locations included, so that `internal/strictjson`, `mcpwire` and
`a2ahttp` list exactly the spec's levels.) A client's message is refused with `-32600` and HTTP `400` before
policy, metering, Admission or dispatch; an upstream's is not relayed (`502` with `-32603`, the `calls` row
`is_error` true, a dispatch `upstream_error`). The hosting node and the node's MCP filters read the same way, since
the gate is not trusted to have done it. A2A HTTP+JSON is decoded by a2a-go alone and is not checked. Consequence:
a tool whose arguments have two names that differ only in case cannot be called through the gate
([`relay.md`](../spec/relay.md) §2.1, §2.4, §11.1; [`ext-sealed.md`](../spec/ext-sealed.md) §5;
[`tunnel.md`](../spec/tunnel.md) §10; `internal/strictjson`, `mcpwire.Parse`, `a2ahttp`'s `strictJSONRPC`;
`mcp_case_smuggling`, `a2a_case_smuggling`). Amended by D191 (a lone case variant is refused like a pair).

### D173. Roots never leave the node — **decided under the owner's standing instruction, 26 Oct**

[`tunnel.md`](../spec/tunnel.md) §10 routed a stdio server's `roots/list` to the remote client, and `stdio_roots`
asserted a first call "using the client's roots". `server-filesystem@1.0.0` replaces the directories it was started
with by the client's roots (`dist/server.js:590-643`), so a buyer, or the gate forging the answer, that sent a root
of `file:///` would read and write the node's whole filesystem: `~/.kontra/identity.key` and the job keys, and code
execution by writing `~/.bashrc` or `~/.ssh`. That breaks PRD §12 (the gate never executes code) and tunnel.md §15
(the node is not a proxy). Decision: for every MCP server a node hosts, stdio or `mcp:<name>=<url>`, the node
answers `roots/list` itself from roots its operator names (`kontra connect --roots <name>=<dir>`, repeatable with
one directory per flag, as in `--roots fs=/a --roots fs=/b`; none by default, which is an empty list), replaces the
client's `capabilities.roots` in the `initialize` it forwards with its own (present only when roots are
configured), and never relays `roots/list`, `notifications/roots/list_changed` or the client's roots in either
direction; on a 2026-07-28 exchange it removes the roots capability, roots answers and roots requests, and
configured roots are offered to legacy sessions only. A separate flag was chosen over a `,roots=` suffix on
`--serve` because a stdio command line or a URL may contain commas, and one directory per flag because a
directory's name may contain one too. `stdio_roots` now asserts that the server sees only the node's configured
roots, and `examples/mcp/fs-stdio` passes its allowed directory on its command line ([`relay.md`](../spec/relay.md)
§11.10; [`tunnel.md`](../spec/tunnel.md) §8.2, §10, §15, §16; `internal/mcproots`, `stdiobridge.Config.Roots`).
Amended by D212 (which client responses reach a hosted server).

### D174. The sealed envelope is kept exactly where ext-sealed §3 puts it — **decided under the owner's standing instruction, 26 Oct** (SPEC-CONFLICT)

[`relay.md`](../spec/relay.md) §2.5 removed every `sh.kontra/` key from the `params._meta` of upstream requests,
which is where [`ext-sealed.md`](../spec/ext-sealed.md) §3 puts the envelope of a sealed `elicitation/create` or
`sampling/createMessage`, so `sealed_no_plaintext` (c) could not pass; and it had no rule for MCP callers, whose
sealed `tools/call` and answers carry the envelope in `_meta` too. Both texts predate M3. Decision: §2.5 scrubs
`sh.kontra/` keys from MCP callers as well (`params._meta` of requests and notifications, `result._meta` of
responses), and on a service with `sealed` set, for 2025-11-25 and 2025-06-18 messages, keeps
`_meta["sh.kontra/sealed"]` in the `params._meta` of a caller's `tools/call` and of an upstream `elicitation/create`
or `sampling/createMessage`, and in the `result._meta` of the caller's answer to one, when its value is exactly
`{"v": 1, "envelope": <base64>}`, as the A2A rule keeps `kontra.sealed` on envelope parts. Any other `sh.kontra/`
key, and `sh.kontra/sealed` anywhere else or on another service, is removed. A sealed result's envelope is in
`content`, which §2.5 never reads (`mcpwire.ScrubParams` and `ScrubResult` take the direction, the method and the
service's sealed flag; `mcp_meta_spoof` gains the case of a service without `sealed`). Amended by D200 (the envelope
on every client response).

### D175. A sealed MCP input request is bound to the upstream's own request id — **decided under the owner's standing instruction, 26 Oct**

[`ext-sealed.md`](../spec/ext-sealed.md) §3 sealed an upstream's input request with `messageId` "the gate request
id the gate gives it" (`k.<taskId>.<seq>`), which the hosting node cannot know when it seals, and hosting rule 4,
written for `tools/call`, would then refuse the hiring node's answer (its `tool` and `messageId` match no preamble
field), or rule 5 would call it replayed. Rule 4's mismatches had no reason either, so `sealed_replay`'s MCP call
resent on another session had none to assert. Decision: the hosting node seals an input request with `messageId` =
the upstream server's JSON-RPC id for it, which it reads as it relays the request, and with the `session` and
`tool` of the call that created the task. The gate rewrites JSON-RPC ids for routing and never touches the
envelope. The hiring node checks `session`, `tool` and that the `messageId` is new in the task, keeps which gate
request id carried it, and seals the local client's answer with the same `messageId` and the job's `replyTo` and
`rk`. A new hosting rule 4b accepts an answer only for an input request the node sealed for the preamble's task
less than `max_age` ago, once (`wrong_message`, `replayed`, `wrong_job`), and rule 5 applies to requests only,
since a server's ids are another space than the client's. A mismatch of rule 4's `session`, `tool` or `messageId`
is `wrong_message`, already a §6 reason. In a job whose request was sealed, an answer to an input request the node
sealed must itself be sealed and pass rule 4b: an unsealed one is refused `bad_header` and nothing of it reaches
the service, so a gate cannot answer the server's question in plaintext for the buyer (the hosting node's half of
the hiring node's rule 4a; added by the revision check). A per-job input id minted by the node (the audit's other
option) was not taken: it needs a new header member and a preamble field for answer exchanges, while the upstream's
id already reaches the node on both legs ([`ext-sealed.md`](../spec/ext-sealed.md) §3, §5, §9; `sealing.Hosting`,
`sealing.Job`; `sealed_replay`).

### D176. A modern request's version is checked as 2026-07-28 says, and D158's evidence is corrected — **decided under the owner's standing instruction, 26 Oct**

[`relay.md`](../spec/relay.md) §11.12 answered a `_meta` `protocolVersion` that differs from
`MCP-Protocol-Version` with `-32602`. 2026-07-28 Streamable HTTP, "Protocol Version Header": the header MUST
match `_meta`'s version, and a mismatch MUST be rejected with `400` and `HeaderMismatch` (`-32020`); `-32602` is
for a missing required `_meta` field (basic, "Per-request protocol fields"). §11.12 also gave `-32021` no HTTP
status (basic requires `400`), and removed `notifications.taskIds` from a `subscriptions/listen` that does not
declare the tasks extension, which the extension (v0.2.2, "Task Status Notifications") answers with `-32021`.
D158 cited `server-everything@1.0.0` `dist/transports/streamableHttp.js:143-151` for "exactly what legacy servers
answer": that is its `GET` handler; its `POST` without a session goes to a new SDK transport (lines 87-111) that
answers its own `-32000` body and is never closed. Decision: a missing `protocolVersion` or `clientCapabilities` is
`400` with `-32602`; a `protocolVersion` other than the header is `400` with `-32020`; `-32021` is `400` with
`data.requiredCapabilities` naming the extension; a `subscriptions/listen` asking for `taskIds` without declaring
the extension is `400` with `-32021`, and any other has them removed. Rule 3's body stays: a `-32000` body is what
legacy servers send and is not a modern error, so D158's conclusion holds, and D177 stops forwarding modern probes
to upstreams known to be legacy-only ([`relay.md`](../spec/relay.md) §11.12; `mcpwire.CheckModern`).

### D177. The gate learns each service's era, and opens one virtual session per principal and service after a modern answer — **decided under the owner's standing instruction, 26 Oct** (SPEC-CONFLICT)

Four problems with D158 and D160 as written. A modern request to a legacy-only upstream was always forwarded: each
probe of `server-everything@1.0.0` leaks a server and its transport on the seller's node (D176), and it opened a
virtual session after Admission `session` for a service the client then uses through a legacy session.
`mcp_sessions` has no unique constraint for open modern rows, and Claude Code sends `server/discover` and
`tools/list` at once, so two first requests wrote two rows; D160's "no schema change" was wrong. The plan had the
gate refuse modern requests to stdio services, which it cannot tell apart (the hello declares kind `mcp` only,
[`tunnel.md`](../spec/tunnel.md) §10). And 2026-07-28 makes the era a property of the server that a client SHOULD
cache per origin (Versioning, "Backward Compatibility"), while one gate origin serves modern services beside
legacy-only, sealed and stdio ones. Decision: the gate keeps each service's era. A `server/discover` result makes a
service modern (its cached rewrite, or the answer to one the gate sends itself when it has none, which a modern
request finding the era unknown waits for). Only an explicit legacy answer to the gate's own `server/discover`
makes it legacy-only: rule 3's `-32000` body, a `404` or `405`, or a `200` or `400` whose body is not a 2026-07-28
response. A response to a client's request never does: a `400`, `404` or `405` answering a client's modern request
is relayed to that client and changes nothing for other principals, since otherwise one principal could make a
modern service legacy-only for every other with a request its upstream refuses (the revision check). The era is
kept 10 minutes like the tools list and dropped when a node of the service connects or disconnects; while a service
is legacy-only, a modern request is answered with rule 3's `400` at the gate, as for a sealed service. A virtual
session opens only once the upstream has answered a modern request with a modern result, after Admission `session`;
migration `0002_m3.sql` adds a partial unique index on `mcp_sessions (tenant_id, service, principal) nulls not
distinct where protocol_version = '2026-07-28' and closed_at is null`, and the insert is `on conflict do nothing`
followed by a read, so concurrent first requests share one row (Admission may still be asked twice by two replicas:
it decides and moves no money, §13.1). The gate refuses modern requests itself for sealed services only; a stdio
service is learned like any upstream, from the bridge's answer to the gate's probe. The per-origin cache stays a
residual, logged by a new `mcp_era_fallback` variant (a modern success on `/mcp/a`, then a legacy-only `/mcp/b` on
the same origin), and Claude Code runs against the compose gate before M4 ([`relay.md`](../spec/relay.md) §11.12,
§16).

### D178. A modern client that leaves cancels its task, an `InputRequiredResult` is the first upstream id, and a gate-owned task's retention is stated — **decided under the owner's standing instruction, 26 Oct**

Three gaps in D159. A client that closes its stream during the 2 s inline wait never learns the task's id, but the
task ran on; 2026-07-28 Streamable HTTP ("Cancellation") has the server treat a closed response stream as a
cancellation. An `InputRequiredResult` answering the dispatch's `tools/call` set no first upstream id (§11.12 named
only a `CreateTaskResult`'s `taskId`), so a retried attempt sent the original call again: the upstream ran it again
and asked again under new keys, and the first input requests stayed outstanding in `tasks/get`. And D159 recorded
only the cost of making every modern call on a durable service a task, not that its arguments and result are then
kept in `work_items` and `task_events` for the retention period, where a plain call keeps nothing. Decision: a
client that closes its stream before the inline answer or the `CreateTaskResult` makes the gate send
`kontra.cancel` (D57); the first `InputRequiredResult` counts as the first upstream task id (the attempt records
its first `inputRequests` key in `upstream_task_id`), so a tunnel lost after it fails the task `tunnel_lost`; and
§11.12 and the operator docs next to `tasks: off` state the retention. A `tasks: durable_tools` allowlist was
considered and not added: `tasks: off` is the switch in v4.0 ([`relay.md`](../spec/relay.md) §11.12, §16:
`mcp_modern_task`, `mcp_task_resume`).

### D179. The peer hop signs what the holder forwards, `mcp_sessions.replica_id` names the holder, and several replicas SHOULD set `peer.listen` — **decided under the owner's standing instruction, 26 Oct**

D163's HMAC covered the assertion, the method, the path and the body's hash, but not the headers the holding
replica sends upstream: the upstream `Mcp-Session-Id` it substitutes, `Last-Event-ID`, `MCP-Protocol-Version`,
`Mcp-Method`, `Mcp-Name` and `Mcp-Param-*`. On a plaintext peer link, anyone on the path could steer a vetted
request to another client's upstream session on the same tunnel. The tunnel reaper closed "the `mcp_sessions`
pinned to" a dead replica, but with the hop the replica that wrote a row (the one the client reached) may not hold
its tunnel, and nothing said which one `replica_id` names. Nothing said either that a gate of two replicas behind a
balancer and without `peer.listen` sends about half of its pinned requests to a `404` and a new `initialize`.
Decision: the assertion carries the upstream session id and the SHA-256 of the other forwarded headers in a
canonical form (names lowercased and sorted, one `name: value` line each), and the holder builds the upstream
request from signed data only and refuses a request whose headers do not match; the peer address is reachable only
from the replicas, or carries TLS; `mcp_sessions.replica_id` is the replica holding the tunnel session, NULL for a
direct backend, so killing a replica that held only a client's connection closes nothing (a reaper unit test); and
a gate of more than one replica SHOULD set `peer.listen` (§11.2, §17, the config schema). D183 covers this
repository's chart; Cloud's Helm values open the peer port within the namespace (D163)
([`relay.md`](../spec/relay.md) §11.2, §17; [`tunnel.md`](../spec/tunnel.md) §11; `peer.Assertion`,
`peer.ForwardedHeaders`; `mcp_peer_hop`).

### D180. A modern exchange's preamble carries no MCP session — **decided under the owner's standing instruction, 26 Oct**

[`tunnel.md`](../spec/tunnel.md) §8.1 defined `mcp_session` as the hash of the client's gate `Mcp-Session-Id`,
which a 2026-07-28 exchange does not have, so its value for modern requests and modern dispatches was undefined. A
virtual session's hash (D160) would hand nodes an identifier that binds nothing, and a first modern request has
none yet (D177). Decision: on an exchange for a modern request, and on every exchange of a task a modern request
created, `mcp_session` and `mcp_request_id` are absent, and a node must not rely on them there; sealed services,
which need them, are never sent a modern request (D162).

### D181. `kontra-blob:` is reserved, and a blob stream answers with one line — **decided under the owner's standing instruction, 26 Oct**

D167 renders every `kontra-blob:<ref>` part as a presigned GET and authorizes a blob stream for a task the session
serves, but nothing refused a `kontra-blob:` part that a caller or an upstream wrote: anyone who learned a ref (a
node serving several buyers, say) could put it into another task and receive a presigned URL or a blob stream for
another buyer's object. The plan also wrote the stream's framing twice, once without `too_large`. Decision: a `url`
part whose URL starts with `kontra-blob:` is refused from a caller (`InvalidRequest`) and from a service
(`upstream_error`); egress rendering and blob streams resolve a ref through `blobs`, and only for the task its row
names; a blob stream answers with one JSON line, `{"size": n}` and then exactly `n` bytes, or `{"error":
"not_found" | "forbidden" | "too_large"}`, and closes ([`relay.md`](../spec/relay.md) §10.4;
[`tunnel.md`](../spec/tunnel.md) §8.3; `blob.Fetch`, `tunnelgate.NodeStreams`).

### D182. A node-opened stream over the cap gets `429`, and a node that keeps refused streams open loses its session — **decided under the owner's standing instruction, 26 Oct** (settles D137's text)

[`tunnel.md`](../spec/tunnel.md) §14 answered a node-opened stream over the cap with `429` and kept the session.
yamux v0.1.2's `Stream` exports `Close`, `Read`, `Write` and the deadline setters only (`forceClose` and
`closeTimeout` are unexported), so there is no reset, and a refused stream frees nothing while the node holds its
half; D137 counted a stream from its accept until both halves end, and left §14's text open. Decision: §14 says
that a stream counts from its accept until both halves end; over the cap (32, 8 for an anonymous session) the gate
answers `429`, and a node that keeps more than the cap of refused streams open loses its session.

### D183. The repository's chart mounts the signing key, and runs more than one replica only with a peer port — **decided under the owner's standing instruction, 26 Oct**

`charts/kontra` mounted only `kontra.yaml` from its config Secret, so after D166 a chart deployment serving A2A
services at a public `public_url` refuses to start: it has no file for `signing_key`. It runs one replica and its
NetworkPolicy admits only the public and metrics ports, and no work package listed it. Decision: M3 WP-02 adds
`signingKey: {existingSecret, key}` to the chart's values, mounted read-only at `/etc/kontra/signing/` with mode
`0440`, which the pod's `fsGroup` makes readable and `jws.LoadKeySet` accepts, and the values document
`signing_key: file:/etc/kontra/signing/kontra.key`. M3 WP-04 adds `gate.peer: {enabled, port, secret:
{existingSecret, key}}` (the container port, the `peer.secret_env` variable from that Secret, and NetworkPolicy
ingress to the peer port from the release's gate pods with the matching egress) and `gate.replicas` (default 1),
which the chart refuses above 1 while `persistence.enabled` holds the one-replica ACME cache (D70) or without
`gate.peer.enabled` (D179).

### D184. A stdio child is stopped with its process group — **decided under the owner's standing instruction, 26 Oct**

[`tunnel.md`](../spec/tunnel.md) §10 closed the child's stdin, then sent `SIGTERM` after 5 s, to the child only.
The pinned commands start with `npx`, which runs `node` as its child, so killing the `npx` pid could leave `node`
running past `--max-sessions` and past the e2e leak check. Decision: the bridge starts each child in its own
process group, sends the group `SIGTERM` 5 s after closing stdin and `SIGKILL` 5 s later, and does the same on
idle, on a crash and on `Close`; WP-05's tests assert that no process of the group survives
(`stdiobridge.KillAfter`).

### D185. The e2e and pentest harnesses keep stock servers on loopback, with nothing to leak — **decided under the owner's standing instruction, 26 Oct**

`server-everything@1.0.0` listens on every interface (`dist/transports/listen.js`, `app.listen(port)`) with CORS
`*`, and ships `get-env` (all of `process.env`) and `gzip-file-as-resource` (fetches any URL while
`GZIP_ALLOWED_DOMAINS` is empty). Started with the harness's environment on this shared host, it would hand the
network `KONTRA_TEST_PG`'s credentials, the bucket's keys and `KONTRA_PEER_SECRET`. The bridge's scrubbed
environment also hides stdio children from `scripts/test_e2e.sh`, which finds leftovers by their `KONTRA_E2E_*`
variables. Decision: every server the harnesses start listens on `127.0.0.1` only (Go servers bind it; the npm
servers start with a `--require` preload from `test/e2e/testdata` that makes a port-only `listen` bind
`127.0.0.1`), with a minimal environment (`PATH`, `HOME`, `LANG` and the run's marker variables) and
`GZIP_ALLOWED_DOMAINS` set to a name that matches nothing; MCP services deny `get-env` and `gzip-file-as-resource`
unless a test needs them; stdio services get `--env KONTRA_E2E_RUN --env KONTRA_E2E_SESSION`, and the leak check
also matches children by process group. `examples/mcp/everything` tells operators the same.

### D186. The pentest runs on this host's Docker without touching its neighbours — **decided under the owner's standing instruction, 26 Oct**

Five things stopped the pentest from running as D168 and D169 describe it. The D169 test Lifecycle worker needs
Temporal's frontend, which `compose.yaml` does not publish; and the `hooks.lifecycle` key it needs is refused at
load until M5 (`internal/config/validate.go`), although the gate's half (the hooks snapshot, and the five
activities scheduled by type name on the configured queue, `lifecycle.WorkerConfig.RemoteHooks`) is built since
M1. Port 8333, SeaweedFS's S3 default, is held on the build host by another container. SigV4 presigning signs the
host, so a presigned GET works only through the endpoint the gate signed it for, which the gate's container and
the host's attackers must both reach. `go test`'s default 10-minute timeout is shorter than building the gate
image on 2 CPUs and running the targets, and would panic with the stack left up. And a key file bind-mounted
root-owned at `0440` is unreadable to the image's uid 65532. Decision: `compose.yaml` publishes no Temporal or
Postgres port; `compose.pentest.yaml` publishes Temporal's frontend on `127.0.0.1` and a free port for the test
worker, which runs on the host, and the D169 probe alone runs with `hooks.lifecycle` set (a second gate
configuration, applied by a restart); M3 lifts the load-time refusal of `hooks.lifecycle`, with an integration
test of a worker on its own queue, while `hooks.admission` stays refused until M5. The driver creates the run's
network first, reads its gateway address and publishes the bucket on that address and a free port, which is
`blobs`' `endpoint`. It passes `-timeout 90m` (`KONTRA_PENTEST_TIMEOUT` overrides it), builds the image once
(`make pentest-image`) before `go test`, and tears down in `t.Cleanup` and in the make target's trap. The run's
signing key and certificate are owned by uid 65532 before `up`, and the acceptance asserts that the gate starts
with them.

### D187. `kontra connect` prints key ids and thumbprints where the console showed them — **decided under the owner's standing instruction, 26 Oct**

[`ext-sealed.md`](../spec/ext-sealed.md) §7's threat table still said "The seller's key ids are shown in the
operator's console for out-of-band comparison", though D164 removed the local console and a team gate has none.
Decision: `kontra connect` prints the recipient key ids and the seller key thumbprints on stderr and in its log
when it first sees a service and on every change. Amended by D223 (a `kid`-only seller signature is verified with the
seller key set the gate names, and pinned unverified only when it names none).

### D188. Cloud's thread URL is `https://app.kontra.sh/#/t/<taskId>` — **decided by the owner, 26 Oct** (kontra-cloud#60)

[`relay.md`](../spec/relay.md) §12.3, the hooks fixture and the docs named `https://app.kontra.run/t/<taskId>`.
The owner's final decisions (kontra-cloud#60) put the app at `app.kontra.sh`, with hash routes. Decision: the
examples use `https://app.kontra.sh/#/t/<taskId>`. The URL stays the operator's, and the gate never builds it.

## From building M3 (27 Oct)

### D189. The named tests M3 runs, the refusals it lifted, and the spec conflicts met while building it

Like D154 for M2. **The tests** (`test/e2e`, `make test-e2e`; D171, D172): the six gate tests the owner named,
`sealed_no_plaintext` (`TestSealedNoPlaintext`: an A2A task with a 300 KiB part and a follow-up, a plain MCP call
and a durable MCP call whose one elicitation is answered, all carrying a random marker; the marker, raw,
JSON-escaped, hex, base64 or base64url, is in no table of schema `kontra`, neither replica's debug log, no node's
log, no Temporal history with its payloads decoded and no bucket object, with positive controls showing the scan
reads each source), `sealed_replay` (`kontra seal` against the gate: `replayed`, `wrong_job` and the MCP
`replayed`/`wrong_message`, each audited and never retried, the gate's own retry after a node-written `502`
accepted), `sealed_unsealed_part`, `mcp_session_cross_node`, `mcp_tool_denied` (with `mcp_batch_refused`) and
`mcp_task_resume` (both variants, and the modern ones: a replica kill, and an `InputRequiredResult` before the
tunnel dies); and with them `card_passthrough`, `mcp_batch_refused`, `mcp_meta_spoof`, `mcp_session_revoked`,
`mcp_case_smuggling`, `a2a_case_smuggling`, `stdio_roots`, `local_rebinding` and the hiring row (with
`auth_revoked`), `mcp_modern_task`, `mcp_era_fallback`, `mcp_peer_hop` and a sealed 2 MiB part through the bucket.
Each runs on two gate replicas behind the Front, with the client on one replica and the tunnel on the other
wherever the row allows. The harness writes one `signing_key` both replicas sign with (D166), runs a gofakes3
bucket on `127.0.0.1` whose credentials only the replicas get, gives each replica a peer listener
(`--peer-listen`, `KONTRA_PEER_SECRET`), logs the replicas at debug level (`kontra-gate serve --log-level`, added
for this), installs `server-everything@1.0.0` and `server-filesystem@1.0.0` once per run from the example locks
(an install that fails fails the test, unless `KONTRA_E2E_OFFLINE=1` skips it), and keeps D185: every server on
`127.0.0.1` (asserted through `ss -Hltnp` at the end of every test; the preload binds `127.0.0.1` for every `listen`
Node would bind on every interface, a backlog, no port, an empty or null host and an options object included, not
only a port-only one), the npm servers and the harness's own `npm ci` with a minimal environment and the run's
markers, stdio children given the run's markers with `--env`, and `scripts/test_e2e.sh` also killing every process
in the group of a leftover and every process whose command line names the run's directory or whose working
directory is under it. The modern-era tests use the
in-repository dual-era double; `server-everything@2.0.0` runs from a pinned lock (`test/e2e/testdata/everything-2`)
and is skipped, saying why, without Node 24 (this host has Node 22). The residuals of ext-sealed.md §7 (a replay to
another hosting node within `max_age`) and of relay.md §11.12 (a client that caches the era per origin) are
logged by `sealed_replay` and `mcp_era_fallback`, not asserted as passes.

**The refusals M3 lifted.** D90 (`seller_keys` on a direct service) and the `seller_keys` half of D144 (on a tunnel
service): seller signatures are verified (M3 WP-02, `internal/card` `SellerKeys`), and a card no configured key
verifies makes the service unavailable. D104: parts over 256 KiB go to the bucket (D167). The `sealed:` half of
D144: sealing is built (M3 WP-08), so a sealed tunnel service is served and its recipients published; D91 stands
(`sealed:` needs a tunnel backend). `hooks.admission` stays refused until M5 (C3, D186).

**Spec conflicts met while building M3**, each marked `SPEC-CONFLICT` where it is implemented; the closest faithful
behaviour was built, and each is decided in D191–D224 (28 Oct), which change the specs where they say so:

- Strict decoding (relay.md §11.1, D172): a lone case variant (`METHOD` without `method`, `NAME`, `ISERROR`, a
  Kelvin-sign `tasK`) of a member the gate reads is refused like a pair; the four `Task` members are checked only
  where the dispatch reads them (`mcpwire` `Parse`, `CheckTask`). §11.1 should name lone variants. The A2A route
  refuses ambiguous levels only, every reader there decoding as a2a-go does (`a2ahttp` strict.go).
- The bucket's endpoint is dialed as configured, `http://` only when written so, never through the SSRF dialer
  (§10.3, D67; `internal/blob` s3.go). A card that is not valid UTF-8 has no JCS form and is not installed (§3,
  D49; `card` keeper.go). `limits.task_bytes` counts each bucket object as well as each logged encoding (§6.6, D103;
  `relay` executor.go): §6.6's table and D103 should say so.
- Hiring (tunnel.md §4, §5, §9, §14; D133): an unknown key whose hello declares no service and carries a
  hire-scoped API key is admitted hire-only, and a known key with no binding only with one (`enroll` admit.go); a
  specific non-loopback `--listen` is an accepted `Host` (`hiring` CheckLocal); the hiring edge applies its limits
  with the session as the client (`gate` gate.go); a blob stream over the cap is closed unanswered
  (`tunnelgate` nodestreams.go); a hosting exchange whose `kontra-blob:` part cannot be fetched answers D151's
  final `400`, or `503 full` when a retry may succeed (`node` proxy.go); every node binds `--listen`, which the
  owner may want to turn off for nodes that only host (`node` run.go).
- The MCP relay (relay.md §11.1–§11.4, §6.6; D174): the holder answers a refused session `404` with
  `Kontra-Peer-Gone`; plain MCP maps a node's own answers (`not_hosted`/`kind_mismatch` close the row
  `tunnel_lost`, `full` is `503`), a `DELETE` whose upstream is gone is `200`, a `401` and an unknown session use
  `-32001`, and a call whose answer never arrived is metered `is_error` true (`mcprelay` exchange.go, legacy.go);
  only SSE events a client dispatches are relayed and metered, and a second answer ends the stream; a `tools/list`
  drain is bounded to 30 s (`policy.go`); on a sealed service an exact `{v, envelope}` is kept on every client
  response, since a response does not name its request (`legacy.go`); an absent `tools.allow` allows all and an
  empty one none (`gate` services.go); a `GET` or `DELETE` with 2026-07-28 and a session is `405` (`mcpwire`
  wire.go); a hosting node's `sealed_rejected` on a plain call reaches the client as a JSON-RPC error with `200`.
- Hosting MCP (tunnel.md §8.2, §10; D173): an MCP `--serve` target may name its endpoint path, and the gate's `/`
  goes to it (`node` serve.go, proxy.go); an unquoted shell operator in a stdio command line is a usage error
  (`stdiobridge` command.go); `Last-Event-ID` replays only a GET-stream event the queue holds, and a second `GET`
  replaces the first; a client response reaches a hosted server only as the answer to a request the node relayed
  and has not seen answered, and any result with `roots` is dropped (`mcproots` roots.go): §10 should be reworded.
- Durable MCP (relay.md §11.5–§11.8): a resumed `k.` stream ends without the `tasks/result` answer, which a client
  asks for again (`mcprelay` tasks.go); `forward_cancel` sends `tasks/cancel` where the upstream's declaration is
  not known (`dispatch.go`); an upstream that cancels its own task ends it `canceled` with no `kontra.canceled`
  (`legacy_dispatch.go`).
- 2026-07-28 (relay.md §11.12; D177, D178): an inconclusive era probe is not repeated for every waiting request
  (`mcprelay` era.go); a cached modern `tools/list` and a gate-owned task open the virtual session; an `Mcp-Name` on
  a method that has no name in its body is not forwarded; an `InputRequiredResult` is metered with `is_error` NULL;
  a re-attach that finds a multi round-trip first id and no logged round fails `upstream_error`, and a call sent
  again with its answers is never repeated after an exchange that may have reached the upstream (`modern.go`,
  `modern_dispatch.go`).
- Sealing (ext-sealed.md §3, §5, §6): a hiring node refuses a request to a service its pin says is sealed when the
  card cannot be read; the client's own sealed message in a Task's history is restored or left out, never refused;
  a first sight of an artifact may start at any chunk (`sealing` hiring.go); the hosting node lets no exchange reach
  a sealed job's upstream task unless it is that job's (`hosting.go`); a durable MCP task is kept in its session's
  job, and its input requests carry the gate task id (`sealing` mcp.go).
- `kontra seal` (§5.1, M3 WP-09): it sends an API key, since the card of a service that is neither listed nor open
  to anonymous is otherwise the unknown-service `404` (relay.md §2.1); only one given with `--api-key` (or `@file`),
  never one read from the environment, which is tied to no gate (the gate a `--service` names is untrusted, §7), and
  only on the service's card once it was `404` without it, then on its upstream card, never on the key set
  (`sealing` seal.go). And `--open` binds a job to the task its reply names, so that `--task` seals a follow-up with
  that job's key and `rk`, which rule 3 requires. §5.1 should say both.

Three defects the gate tests exposed were fixed where they were, each with a regression test: a follow-up whose
header names another task than the preamble's is `wrong_job` (rule 3), not `wrong_task` (`sealing` hosting.go);
`kontra seal --open` leaves out the client's own sealed message in a Task's history (only it: any other part no
job key opens is still an error); and the replica holding a tunnel flushes
an exchange's headers before its body, so an idle event stream answers through the peer hop (`mcprelay`
`RunExchange`, `TestPeerHopIdleGET`).

### D190. The M3 pentest's findings, and how its harness departs from D186

The PRD §12 list and M3's own surface ran against the compose team gate on this host, as D168, D169 and D186 say
([`pentest-m3.md`](pentest-m3.md) records every probe). **Four findings, fixed** with a regression test each:

- F-1: a start that failed after `gate.New` had built the replica panicked (its deferred cleanup read the named
  result every error return had set to nil). The cleanup holds the replica it built.
- F-2: the closed default route's empty `404` carried no HSTS. HSTS is set before the route is matched.
- F-3: the bucket check at start had no bound (minio-go's retries held an unreachable bucket's gate for minutes,
  never healthy). `blob.Open` gives up after 30 s.
- F-4: a hosting node's refusal of an answer to a sealed MCP input request (ext-sealed.md §5 rule 4b) was treated as
  an upstream's refusal: no audit row, and the task waited at `input_required` until its deadline. ext-sealed.md §6
  already says what the gate does with a node's refusal: an audit row, the task's failure, no retry. The durable
  dispatch now does it for an answer as for any other sealed part. No spec text changes.

**The harness's departures from D186.** The bucket's endpoint is its own address on the run's network, not the
network's gateway address with a published port: this host's firewall drops what a container sends to the host's own
addresses, so the gate's container could not reach a port published there, while the bucket's network address is
reachable from the gate and from the host alike, and nothing of the bucket is published. Images are built with the
docker driver's builder only (`BUILDX_BUILDER=default`), never with another project's buildx builder. The trap of
`make pentest` also removes the run's directory.

**What the run could not exercise on this host**, each covered elsewhere and named in the record: the push redirect leg
(it needs a globally reachable webhook, and D185 keeps every server on loopback), an oversized blob announcement to a
node (the e2e FakeGate serves no node-opened blob stream), and a node's own `replayed` refusal of an answer the gate
sent twice (the gate takes one answer per gate request id, relay.md §11.7). The dual-era client run D177 asks to record
before M4 was Claude Code 2.1.281's `claude mcp list` against a modern and a legacy-only service on the gate's one
origin: both connected, the modern one in the modern era. Amended by D266 (the second run, after the review rounds).

## From M3's spec conflicts and its review (28 Oct)

Building M3 met the spec conflicts D189 lists, and the review after its pentest (the eleven `M3 review:` commits of 28
Oct) met more; each is marked `SPEC-CONFLICT` where the code departs from the text, with the closest faithful
behaviour built. The owner's standing instruction ("Don't ask me anything", 26 Oct) covers them: each is **decided
under the owner's standing instruction, 28 Oct** below, with the option its builder or reviewer recommended, and each
code marker names its entry. Where that option is what the code does, the specs now say it and cite the entry
([`relay.md`](../spec/relay.md) revision 7, [`tunnel.md`](../spec/tunnel.md) revision 5,
[`ext-sealed.md`](../spec/ext-sealed.md) revision 3); where it was not built yet, the specs stated the built behaviour
and its residual, and D225 listed what was left to build, since built in M3 too. As in M2, a proposal that would only
add a value to what both binaries already speak (a blob refusal, a `Kontra-Node` reason) is not taken where the built
behaviour holds; the envelope's `service` member (D221) is taken, because it closes a residual nothing else closes.

### D191. A lone case variant of a member the gate reads is refused like a pair — **decided under the owner's standing instruction, 28 Oct** (amends D172)

D172 and [`relay.md`](../spec/relay.md) §11.1 refuse a level in which a key repeats or two keys are equal under
Unicode case folding. A lone variant is neither: `{"METHOD": "ping"}` with no `method`, a lone `NAME` or `ISERROR`,
`tasK` spelt with the Kelvin sign. The gate reads the member as absent, while a decoder that folds case (Go's
`encoding/json` into a struct, as mark3labs/mcp-go decodes) reads it as the member, so a denied tool could run, or a
failed call be metered a success. Decision, as built: at the levels §11.1 names, a key equal under case folding, but
not exactly, to a member the gate reads is refused like a pair, from a client (`400`, `-32600`) and from an upstream
(not relayed); the members are those `mcpwire` lists (`clientReads`, `upstreamReads`), where a member is added
before the gate reads it. The four members of a task object (`taskId`, `status`, `statusMessage`, `pollInterval`)
are checked only where the dispatch reads them (`mcpwire.CheckTask`): checked in every upstream result, they would
refuse ordinary results that carry a `Status`. The A2A JSON-RPC route refuses ambiguous levels only, since every
reader there (the gate's and a2a-go's) decodes with `encoding/json` alike (`a2ahttp` strict.go)
([`relay.md`](../spec/relay.md) §11.1; `mcpwire.Parse`, `mcp_case_smuggling`).

### D192. The bucket's endpoint is dialed as configured — **decided under the owner's standing instruction, 28 Oct** (amends D67, D167)

[`relay.md`](../spec/relay.md) §10.3 has every other operator URL follow the SSRF dialer (D67): `https://` and
globally reachable addresses only, with no `allow_private` for `blobs:`. That refuses every private team bucket
(MinIO or SeaweedFS beside the gate, the e2e harness's gofakes3 on `127.0.0.1`, the pentest's bucket on its compose
network, D190). Decision, as built: the bucket is the gate's own storage, like its Postgres and Temporal, and
nothing a caller or a seller writes chooses its address; its endpoint is dialed as configured (`http://` only when
the operator writes it), with the environment's proxy ignored and redirects never followed, as §10.3 says of every
connection ([`relay.md`](../spec/relay.md) §10.3; `internal/blob` s3.go `Open`).

### D193. A card that has no JCS form is not installed — **decided under the owner's standing instruction, 28 Oct** (amends D49)

§3 serves the upstream card byte for byte (D49) and signs the served card over its RFC 8785 JCS, with the upstream
card's JCS hash in the header (D166). JCS is defined for valid Unicode only, so a card that is not valid UTF-8, or
holds a lone surrogate, can be neither hashed nor signed; M1 served such bytes as they were. Decision, as built:
such a card is not installed (`ErrBadCard`, as for any card the gate cannot serve): the service has no card (`503`,
`service_unavailable`) until its seller publishes one, and nothing is ever served unsigned or altered
([`relay.md`](../spec/relay.md) §3; `card` keeper.go, `TestUnsignableCardIsNotServed`).

### D194. `limits.task_bytes` counts the task's bucket objects — **decided under the owner's standing instruction, 28 Oct** (amends D103)

D103 counts in `limits.task_bytes` each logged event's whole encoding, and the M3 plan counted a part moved to the
bucket as its logged `url` part only. That bounded a task's objects in number, not in bytes (`task_bytes` over a
`url` part's few hundred bytes, each object up to an SSE event's 4 MiB), against §6.6's rule that nothing an
upstream sends is unbounded and §10.4's "per-task totals are bounded". Decision, as built: `task_bytes` bounds
everything the gate stores for a task, each logged event's whole encoding plus each of its bucket objects
(`blobs.size`), the caller's included; a dispatch starts from the projection and the task's `blobs` rows, and an
upstream object that would cross the limit is never written: the task fails `upstream_error`
([`relay.md`](../spec/relay.md) §6.6; `relay` executor.go `storePart`).

### D195. A sealed service's errors keep their code only, at the node and at the gate — **decided under the owner's standing instruction, 28 Oct**

[`ext-sealed.md`](../spec/ext-sealed.md) §2 lists what is sealed (parts, MCP arguments and results) and says nothing
of errors, while §7 promises that the gate's database and logs hold ciphertext only; and
[`relay.md`](../spec/relay.md) §6.4 fails a task `upstream_error` "with the upstream message", while §11.8 stores an
MCP task's JSON-RPC error verbatim. A service's error routinely quotes the input it refused (a validation error
echoing a card number), so the gate logged and stored sealed content (M3 review security-1). The reviewer's options:
§2 says that a sealed job's errors carry their code only, or §3 defines an error envelope that the hiring node
opens. Decision: the first, as built. On a sealed job the hosting node relays an error (a JSON-RPC error, an
HTTP+JSON `google.rpc.Status`) with its code, and its status, and fixed text in place of its message and data; and
for a sealed service the gate logs and stores its own words, never the service's: an A2A failure's text names the
HTTP status at most, and a durable MCP task's stored error keeps its code. An error envelope, which would give the
buyer the text, is an envelope v2 question ([`ext-sealed.md`](../spec/ext-sealed.md) §2;
[`relay.md`](../spec/relay.md) §6.4, §11.8; `sealing` hosting.go `errorResponse`, mcp.go; `relay` executor.go
`failureText`; `mcprelay` legacy_dispatch.go `finishError`; `sealed_no_plaintext` (d),
`TestReviewSealedJobErrorsKeepTheirCodeOnly`, `TestReviewSealedFailureText`).

### D196. What the MCP relay answers where §11.2 and §11.4 are silent — **decided under the owner's standing instruction, 28 Oct**

The spec does not say how the replica holding a tunnel session reports, to the replica the client reached, a session
that §6.1 refuses; what a plain MCP request gets for a node-written answer; what a `DELETE` gets when the upstream
is gone; which JSON-RPC code a `401` or an unknown session carries; or how a call whose answer never arrived is
metered (M3 WP-04). Decision, as built: the holder answers `404` with `Kontra-Peer-Gone: tunnel_lost | revoked`
(`503` when it cannot read the key check), a gate-internal header like D138's channel, and the receiving replica
closes the row with that reason; a node's `not_hosted` or `kind_mismatch` on a pinned session closes it
`tunnel_lost` and is `404`, `full` is `503` with `Retry-After`, `sealed_rejected` is D202's, and any other node
answer is `502` with `-32603`; a `DELETE` always closes the row (`deleted`), and is `200` when the upstream's tunnel
is gone; the gate's `401` and its unknown-session `404` carry `-32001`, as the TypeScript SDK's servers answer an
unknown session; a call whose exchange was answered but whose JSON-RPC response never arrived (a cut stream, a
refused event, a client that left) is metered `is_error` true, and a call that got no answer at all (a §6.1 refusal,
an unreachable upstream or peer, a client that left first) has no `calls` row ([`relay.md`](../spec/relay.md) §11.2,
§11.4; `mcprelay` exchange.go, legacy.go).

### D197. The MCP relay reads what a client reads — **decided under the owner's standing instruction, 28 Oct**

§11.1 and §11.2 say what happens to an SSE event whose message fails strict decoding, and that events otherwise pass
unchanged. They say nothing of an event a client does not dispatch, a second response to one request, a leading byte
order mark, how a response is matched to its request, or an `isError` that is not a boolean, and each let the gate
meter, scrub or check other bytes than the client acts on (M3 WP-04; M3 review mcp-spec-1 to mcp-spec-3). Decision,
as built: the gate's SSE reader, and the node's URL filter, drop one leading UTF-8 byte order mark, as WHATWG SSE
and stock clients do; only events whose type is absent or `message` are read, metered and relayed, and others are
dropped; the response that answers the client's request is matched as the TypeScript SDK matches it (`Number(id)`:
`"7"`, `7.0` and `7e0` answer `7`; a string request id only its own string), it is what is metered, and the stream
ends before a second one; a top-level `isError` that is not a JSON boolean is a malformed upstream message (`502`
with `-32603` and `is_error` true; `upstream_error` in a dispatch) ([`relay.md`](../spec/relay.md) §11.1, §11.4;
`mcprelay` stream.go, exchange.go `relaySSE`; `mcpwire.IsErrorFlag`; `mcp_meta_spoof`, `mcp_case_smuggling`).

### D198. An empty `allow` allows no tool, and a `tools/list` drain has 30 s — **decided under the owner's standing instruction, 28 Oct**

Two gaps in §11.3 and §6.6. M1 stored a service's `tools` as `{"allow": [], …}` whenever the file named no `allow`,
so a deny-only policy could not be told from an explicit empty `allow`, which §11.3 does not define. And §6.6 bounds
a drain in pages and bytes only, while every request of the service on a replica waits for the one drain in flight,
so an upstream session that never answers held them all for as long as the client that started it waited (M3 WP-04).
Decision, as built: `allow` is stored only when the file names it; an absent `allow` allows every tool and an empty
one none (on a team gate's first start with M3, each configured MCP row with a `tools:` block is rewritten once,
with an `audit` row; a row Cloud wrote with `"allow": []` now denies every tool). A drain runs on the gate's own
context, detached from the client that started it, and has 30 s in all; past it the drain fails `upstream_error`
(`502` with `-32603`), and each waiter drains again through its own session ([`relay.md`](../spec/relay.md) §6.6,
§11.3; `gate` services.go `desiredOf`, `mcprelay` policy.go `DrainTimeout`).

### D199. Replicas tell each other of an MCP service's nodes and tools on `kontra_tunnel` — **decided under the owner's standing instruction, 28 Oct** (extends D138)

§11.12 drops a service's era "when a node of the service connects or disconnects", and §11.3 refreshes the drained
`tools/list` on the upstream's `notifications/tools/list_changed`. With the peer hop (D163) every replica serves
other replicas' nodes, and only the replica that saw the change acted on it: after a `list_changed` relayed on one
replica, a tool the upstream had just added was refused `Unknown tool` on the other for up to 10 minutes (M3 WP-07;
M3 review mcp-spec-5). Decision, as built: two more gate-internal payloads on D138's `NOTIFY kontra_tunnel`.
`'mcp:<tenant_id>/<service>/<replica_id>'` is sent when a node of an MCP service connects to or leaves a replica,
and every other replica drops the service's era (a `LISTEN` reconnect drops every era, for a lost notice).
`'tools:<tenant_id>/<service>'` is sent after an upstream's `tools/list_changed`, whether the relay or a durable
dispatch on its own session saw it, coalesced per service (at most one each 250 ms, sent at the window's end), and
every replica drops the service's lists, both eras. This is Postgres between replicas, not tunnel wire protocol, and
needs no migration ([`relay.md`](../spec/relay.md) §11.3, §11.12; [`tunnel.md`](../spec/tunnel.md) §11; `tunnelgate`
registry.go, `mcprelay` toolsnotify.go; `TestReviewToolsListChangedReachesEveryReplica`). The payload, the hooks and
the coalescing are unit-tested; a gate-level test of the round trip between two replicas is not written (D225).

### D200. On a sealed service the envelope is kept on every client response — **decided under the owner's standing instruction, 28 Oct** (amends D174)

D174 keeps `_meta["sh.kontra/sealed"]` "in the `result._meta` of the caller's response to" an `elicitation/create`
or `sampling/createMessage`. A JSON-RPC response does not name the request it answers, and on a pinned session that
request may have reached the client through another replica's stream, so the receiving replica cannot tell which
responses those are (M3 WP-04). Decision, as built: on a sealed service an exact `{"v": 1, "envelope": "<base64>"}`
is kept in the `result._meta` of every client response; every other `sh.kontra/` key is still removed, nothing
changes on unsealed services, and the hosting node, which sealed the request, still checks the answer
([`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4b) ([`relay.md`](../spec/relay.md) §2.5; `mcprelay` legacy.go
`forward`).

### D201. A `GET` or `DELETE` with `2026-07-28` is `405`, and a repeated version header is `-32022` — **decided under the owner's standing instruction, 28 Oct**

§11.1 makes a request with `MCP-Protocol-Version: 2026-07-28` modern, and §11.12 rule 5 makes `GET` and `DELETE`
legacy only; neither says what a `GET` or `DELETE` carrying `2026-07-28` and an `Mcp-Session-Id` gets, nor which
version a header sent twice names (M3 WP-04, WP-07). Decision, as built: such a `GET` or `DELETE` is modern, so its
session id is never read (rule 2), and a modern request has no `GET` stream and no session to delete: it is `405`,
as rule 5 answers one without a session and as a 2026-07-28 server answers a `GET`. `MCP-Protocol-Version` is read
once, as its field value: a header sent more than once is its lines joined with `, ` (RFC 9110 §5.3), which names no
version, so such a request other than an `initialize` (rule 1) is rule 4's `-32022` (a `GET` or `DELETE` without a
session `405`), never classified by one line while the upstream reads another ([`relay.md`](../spec/relay.md)
§11.12; `mcpwire` wire.go `Classify`).

### D202. A node's `sealed_rejected` reaches a plain MCP client as a JSON-RPC error with `200` — **decided under the owner's standing instruction, 28 Oct**

[`ext-sealed.md`](../spec/ext-sealed.md) §6's `400` is the hosting node's answer to the gate; nothing says what
status a plain MCP client gets when the gate relays it, and a Streamable HTTP client takes a `400` as a transport
failure and may not read its body (M3 WP-08). Decision, as built: the gate relays the node's error as a JSON-RPC
error response with HTTP `200`: `-32602`, `sealed part rejected[: <reason>]` (the reason only when it is one of
§6's), the request's id and the session header, as it answers any other failed call; the `calls` row has `is_error`
true, and an `audit` row `sealed.rejected` is written. A durable dispatch fails its task `sealed_rejected` with no
retry, as §6 says ([`ext-sealed.md`](../spec/ext-sealed.md) §6; [`relay.md`](../spec/relay.md) §11.4; `mcprelay`
exchange.go `relayResponse`).

### D203. Durable MCP where §11.6–§11.8 are silent — **decided under the owner's standing instruction, 28 Oct**

Three points (M3 WP-06). (a) A `GET` whose `Last-Event-ID` is `k.<taskId>.<n>` resumes a `tasks/result` stream,
whose last event is the answer to that `tasks/result` request; the event id names the task and the seq, not the
request, and no replica keeps per-stream state, so a resumed stream cannot write that answer with its request's id.
(b) §11.6's `forward_cancel` sends upstream `tasks/cancel` "only if the upstream declared `tasks.cancel`", which is
in the `InitializeResult` of the dispatch's own session, and `attempts` has no column for it. (c) An upstream that
cancels its own task fits neither §11.8 case 3, whose data names who cancelled, nor §11.5 step 6:
`server-everything@1.0.0` stores no result for a cancelled task and answers its `tasks/result` with an error.
Decision, as built: (a) the resumed stream carries every pending input request after `n`, then each new one, under
its gate id, and ends when the task is terminal; the client asks `tasks/result` again and gets the stored outcome at
once. (b) The replica running the dispatch keeps the declaration in memory and sends nothing to an upstream that did
not declare it; where it is not known (the dispatching process is gone, a `forward_cancel` on `kontra-direct-ctl`),
`tasks/cancel` is sent, and an upstream that does not serve it answers `-32601`, which changes nothing: the workflow
writes `canceled` whatever it answers (D57). (c) As §11.12 says of the modern dispatch ("`cancelled` is
`canceled`"), the task ends `canceled` with no `sh.kontra/canceled`: its `tasks/result` is `-32603` `task cancelled`
without `data`, and its `calls` row has `is_error` true ([`relay.md`](../spec/relay.md) §11.6, §11.7, §11.8;
`mcprelay` tasks.go `Resume`, dispatch.go `ForwardCancel`, legacy_dispatch.go `cancelledUpstream`).

### D204. The modern relay where §11.12 is silent — **decided under the owner's standing instruction, 28 Oct**

Four points (M3 WP-07). (a) "A modern request that finds the era unknown waits for that probe": read literally,
every modern request to a service whose probe keeps ending inconclusively would probe again and wait up to 10 s. (b)
The virtual session opens "once the upstream has answered that request with a modern result", but a `tools/list`
served from the modern cache and a `tools/call` that becomes a gate-owned task are answered without the upstream
answering them. (c) `Mcp-Name` is defined for `tools/call`, `prompts/get`, `resources/read` and the tasks methods
only; on any other method no body member checks it, and an intermediary behind the gate that routed on it would
route on a value nobody checked. (d) §11.4 writes one `calls` row when "the response completes" and says nothing of
an `InputRequiredResult`, which answers a multi round-trip call without its outcome before the call is sent again
with its answers, so `per_call` would be billed twice. Decision, as built: (a) after a probe that left the era
unknown, requests are forwarded at once for 5 s, and the next one after that probes again; (b) both open the virtual
session, the service being known modern from its upstream's own answers (a probe ending in the legacy `400` still
opens nothing); (c) an `Mcp-Name` on such a method is not forwarded; (d) the row of an `InputRequiredResult` has
`is_error` NULL, as a passthrough `CreateTaskResult`'s has (§11.5), and the call sent again has its own row
([`relay.md`](../spec/relay.md) §11.12; `mcprelay` era.go `probeBackoff`, modern.go `openVirtual`, `modernHeaders`,
`modernForward`).

### D205. The modern dispatch never sends again a call that may have run — **decided under the owner's standing instruction, 28 Oct**

§6.4 makes a failed exchange again for 30 s after the first upstream id and re-attaches a lost dispatch of a direct
backend (D42); §11.12 and D178 make the first `InputRequiredResult` the first upstream id and never send the call
afresh after it. Neither says whether the call sent again with its answers may be repeated after an exchange that
may have reached the upstream, nor what a re-attach does after a dispatch was lost between recording a multi
round-trip first id and logging that round's input requests (or after a round that listed only a `requestState`,
which no event keeps): there is no upstream task to follow and no round to answer (M3 WP-07). Decision, as built: a
call sent again with its answers is not repeated after an exchange that may have reached the upstream (the task
fails `upstream_unreachable`, or `tunnel_lost`); only a refusal given before the call ran (`full`, `408`, `429`) is
sent again at the next wake. Such a re-attach sends the upstream nothing and fails the task `upstream_error` at
once, rather than at its deadline. The choices §11.12 leaves open are recorded as built: a modern attempt keeps its
upstream task id in `attempts.upstream_session` (it has no session) and its first id in `upstream_task_id` (the task
id, the first `inputRequests` key, or `requestState` for a round that listed none); a legacy reader's JSON-RPC error
answering a modern input request is left out of `inputResponses` and `tasks/update`, so the upstream asks again; and
D159's inline wait answers `CreateTaskResult` before its 2 s when the task is `input_required`
([`relay.md`](../spec/relay.md) §11.12; `mcprelay` modern_dispatch.go `modernRun`).

### D206. A node that only hires is admitted on a hire-scoped API key — **decided under the owner's standing instruction, 28 Oct** (amends D133)

[`tunnel.md`](../spec/tunnel.md) §5 and D133 make an unknown key that nothing in its hello enrolls `unknown_key`
"whatever else the hello carries", while §4 allows a hello that declares no service ("a node that only hires"), §9
gives its streams the API key's principal, and a session needs a `tunnel_keys` row. §9 also gives a session without
`auth` the principal `node:<key_id>`, which `org:<tenant>` admits, so a key admitted once on a hire-scoped API key
would hire on after that API key's revocation, against §12 (M3 WP-03). Decision, as built: an unknown key whose
hello declares no service and carries an `auth` API key with the `hire` scope (and no token) is admitted hire-only:
a `tunnel_keys` row in the API key's tenant, no binding, and an `audit` row naming the API key; without such a key
it stays `unknown_key` (D133), so the lost-welcome probe, which carries no `auth`, still learns that nothing
committed. A known key with no binding at all is admitted only on a hello that carries such an API key (whose
principal the session hires as) or that enrolls it; otherwise it is `unknown_key`, retried, or `auth_invalid` for a
revoked or expired API key. A key with bindings, revoked ones included, keeps `node:<key_id>`, so a hosting node
whose API key was revoked hires as `node:<key_id>` once it reconnects without it, as §9 says. Hire-only admission
does not need `tunnel.enroll_with_api_key`, which governs the `enroll:` scopes that bind keys to services; with that
option's self-hosted default (`false`) a node could otherwise never hire through such a gate without hosting
([`tunnel.md`](../spec/tunnel.md) §5, §9; `enroll` admit.go).

### D207. The hiring endpoint: a specific `--listen` address, the session's edge limits, and the node's own user — **decided under the owner's standing instruction, 28 Oct**

Three points of [`tunnel.md`](../spec/tunnel.md) §9. Its `Host` check accepts only `127.0.0.1:<port>`,
`localhost:<port>` and `[::1]:<port>`, yet it allows `--listen` on a non-loopback address with `--local-token`,
whose clients reach the node by that address (M3 WP-03). It gives a hiring request "the API key's" edge limits, but
the edge counts in flight per presented credential and rates per client address, and a hiring request carries
neither: the gate keeps no API key secret after the handshake (M3 WP-03). And on a loopback `--listen` without
`--local-token`, every account on the machine could hire as the node's principal and read the sealed answers its
per-job keys open (M3 review security-7). Decision, as built: with a non-loopback `--listen` of one specific address
(never `0.0.0.0` or `::`), that address and port are an accepted `Host` too, the local token guarding them; the
edge's limits apply with the session as the client (`limits.body` and the body timeout as on the network,
`rps_per_ip` per session, and in flight the session's stream cap of 32, §14), not shared with the same key's
requests from the network; and on Linux the node serves only its own user, reading the owner of each client socket
from `/proc/net/tcp` and `/proc/net/tcp6`, as §9 says since the review. The reviewer's further options (a `0600`
Unix socket, a local token minted by default) are not taken: where the socket table does not exist, §9 tells the
operator of a shared machine to set `--local-token` ([`tunnel.md`](../spec/tunnel.md) §9; `hiring` hiring.go
`CheckLocal`, peer.go; `gate` gate.go; `TestHiringRefusesOtherLocalUsers`).

### D208. Every node serves `--listen` until `--listen off` turns it off — **decided under the owner's standing instruction, 28 Oct**

[`tunnel.md`](../spec/tunnel.md) §9 has every node serve `127.0.0.1:7777` and names no way to turn it off, so every
`kontra connect` binds it, hosting-only ones included: a second node on one host stops at start unless it is given
another `--listen` (this host's `kontra-node-hello` and `kontra-seller-hello` units would collide on an upgrade),
and a hosting node offers its hiring principal to local processes (M3 WP-03 review). Decision, the builder's
recommended option: `--listen off` turns the hiring endpoint off, for a node that only hosts; the default stays
`127.0.0.1:7777`, so a node that hosts and hires needs nothing new. Built (D225): `--listen off` binds no port; a
`--local-token` or an `--allow-origin` beside it, which guard `--listen`, and `--listen off` with no `--serve` (a node
that would neither host nor hire) are usage errors ([`tunnel.md`](../spec/tunnel.md) §9; `node` run.go, node.go;
`hiring` hiring.go `ListenOff`; `TestRunListenOff`, `TestNewChecksListen`).

### D209. A blob that cannot be fetched — **decided under the owner's standing instruction, 28 Oct**

§14 answers a node-opened stream over the cap with `429`, but a blob stream is no HTTP exchange and §8.3's error
line has no `busy`; §8.2 and §8.3 do not say what a hosting exchange answers when one of its `kontra-blob:` parts
cannot be fetched (M3 WP-03); and a blob stream copied its object with nothing bounding a stalled read, holding its
slot of the cap until the session ended (M3 review robustness-10). Decision, as built: a blob stream over the cap is
closed unanswered, which the node reads as a fetch that ended early (the node keeps its own streams under the cap,
so it meets this only while the gate still counts a stream the node has seen end); a hosting exchange whose fetch
would fail again (a refused ref, a bad answer, a malformed body, more bytes than announced) answers D151's final
`400` without `Kontra-Node`, and one whose fetch may succeed if asked again answers `503` `Kontra-Node: full`, which
the gate already retries; and opening an object in the bucket, and each read of it, must bring bytes within 30 s, or
the gate ends the blob stream. A `busy` blob refusal and a `Kontra-Node` reason of its own were proposed and are not
taken: each adds to what both binaries speak, while `full` and an unanswered close already lead to the retry
([`tunnel.md`](../spec/tunnel.md) §8.3, §14; `tunnelgate` nodestreams.go, blobs.go; `node` proxy.go `inlineBody`;
`TestReviewBlobStreamStallEnds`).

### D210. An MCP target names its endpoint, and the gate's `/` goes to it — **decided under the owner's standing instruction, 28 Oct** (amends D123)

D123 had `kontra connect` refuse a `--serve` target with a path until the node forwards by §8.2's rule.
`server-everything@1.0.0` serves `/mcp` only, so a root-only MCP target cannot host the pinned example; and the gate
sends every MCP exchange of a tunnel service to `/`, which §8.2's prefix rule turns into `<base>/`, a `404` from a
server whose one endpoint is an exact route (M3 WP-05). Decision, as built: the node forwards by §8.2's rule (a path
already under the base as it is, any other prefixed); an MCP target may name its endpoint path (clean, with no query
or fragment), and the gate's `/` goes to that endpoint exactly as `--serve` names it; an A2A target with a path is
still refused, as D123 decided ([`tunnel.md`](../spec/tunnel.md) §8.2; `node` serve.go `ParseServe`, proxy.go
`targetPath`).

### D211. The stdio bridge refuses shell syntax, and resumes only its `GET` stream — **decided under the owner's standing instruction, 28 Oct**

§10 splits the command line "with POSIX shell word rules" and runs it without a shell, but does not say what an
unquoted operator, expansion or comment becomes; it replays "the queue after" a `GET`'s `Last-Event-ID`, though
`POST` streams share the id sequence, so an id from one could skip `GET` messages never delivered; and it does not
say what a second `GET` does (M3 WP-05). Decision, as built: an unquoted `|`, `&`, `;`, `<`, `>`, `(`, `)`, `$` or
backtick, or a `#` that starts a word, is a usage error at start (passed on as arguments, they would run something
other than what the operator wrote; quoted, they pass); a `GET`'s `Last-Event-ID` replays the queue after it only
when it names an event of the `GET` stream (a `POST` stream's id replays nothing: the bridge resumes no `POST`
stream); and a `GET` that opens while another is open takes its place, the older one ending
([`tunnel.md`](../spec/tunnel.md) §10; `stdiobridge` command.go `SplitCommand`, bridge.go).

### D212. A client response reaches a hosted MCP server only as the answer to a request the node relayed — **decided under the owner's standing instruction, 28 Oct** (amends D173)

§10 dropped "any client response whose id is that of a request it answered". That cannot keep a client's roots from
a hosted server: a response that arrives before the node has seen the server's `roots/list` is not one, and server
ids are predictable (the TypeScript SDK's first is `0`). Through the real gate, `{"id": 0, "result": {"roots":
[{"uri": "file:///"}]}}` sent right behind `notifications/initialized` reached `server-filesystem@1.0.0` ahead of
the node's answer, which then listed `/` and read a file outside `--roots` (M3 WP-05 review). Decision, as built:
the node forwards a client response only when it answers a server-to-client request that the node relayed on that
session and has not seen answered; it drops any result carrying `roots` (any case variant) whatever its id, and
every response on a 2026-07-28 exchange, which relays no server request; each dropped one is `202`
([`tunnel.md`](../spec/tunnel.md) §10; `mcproots` roots.go `FromClient`; `stdio_roots`,
`TestMCPStdioFilesystemThroughTheGate`).

### D213. A principal will hold at most two MCP sessions of a service, and a full stdio bridge will stop its longest-idle child — **decided under the owner's standing instruction, 28 Oct**

[`tunnel.md`](../spec/tunnel.md) §10 caps a stdio service at 8 children (`--max-sessions`), `503` `full` beyond, and
[`relay.md`](../spec/relay.md) §11.2 caps no principal. Every principal admitted to the service, and every durable
task's own upstream session (D22), shares those children, so one principal holding eight idle sessions has every other
`initialize` answered `full` until `--session-idle` (1 h) and the gate's `session_idle_s` pass (M3 review
security-10). The bridge sees tunnel sessions, never principals. Decision: the reviewer's proposal, both halves. The
gate caps the open MCP sessions of one principal on one service (`services.<name>.max_sessions_per_principal`, default
2), an `initialize` beyond the cap closing that principal's longest-idle session with an upstream `DELETE`; and a
bridge that is full stops its longest-idle child, when that child has been idle for at least 5 minutes, rather than
answer `full`. Both are built (D225). At the gate (`services.max_sessions_per_principal`, in 0002, 1 to 10000): the
eviction runs once an upstream is chosen, before the upstream `initialize` (one refused before then closes nothing),
closing with reason `idle` the principal's sessions beyond the cap other than the new one, the oldest `last_seen_at`
first, each with a `DELETE` on its pinned target made before the `initialize` goes; it runs again once the new row is
written, for concurrent `initialize`s; 2026-07-28 virtual sessions are not counted. On the node: an open `GET` stream
counts as an exchange, so its session is never idle; a stopping child still counts against `--max-sessions` until its
group is gone, and an `initialize` that finds one waits for it (the gate's `DELETE` reaches the bridge first),
stopping no other child meanwhile, rather than answer `full` ([`relay.md`](../spec/relay.md) §11.2, §17;
[`tunnel.md`](../spec/tunnel.md) §10; `mcprelay` exchange.go `makeRoom`, sessions.go `Evict`; `stdiobridge` bridge.go
`reserve`; `TestSessionsPerPrincipal`, `TestSessionsPerPrincipalThroughTheHop`,
`TestFullBridgeStopsItsLongestIdleChild`, `TestSessionsPerPrincipalThroughTheGate`). Amended by D231: the gate's half
closes only a session idle for 5 minutes and none in use, and otherwise refuses the `initialize`; it leaves
`anonymous` and direct backends alone, and its default is 4.

### D214. A hiring node that cannot read a sealed service's card — **decided under the owner's standing instruction, 28 Oct**

[`ext-sealed.md`](../spec/ext-sealed.md) §5 has the hiring node seal for a service "whose verified card carries the
extension" once it has checked its pin, and says nothing of a card it cannot read at all (the gate unreachable, a
`503` card route, a card that does not verify) (M3 WP-08). Decision, as built: when the service's pin says it is
sealed, the request is refused `502`, and nothing is sent in plaintext to a service known to be sealed; with no pin,
or an unsealed one, it goes as it came, which is what a first sight would record anyway (trust on first use, §7),
and that unknown is kept 5 s only. One card read serves every request waiting on it and runs on its own bounded
context, so a request that leaves never ends it for the others (M3 review robustness-1); card routes themselves are
not sealed ([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` hiring.go `info`).

### D215. The hiring node reads a Task's history, and an artifact first seen in a snapshot — **decided under the owner's standing instruction, 28 Oct**

§5 rule 4 opens every sealed part the node receives for the job as `dir: "res"`, and has chunks arrive in order. But
a Task's history holds the client's own message as the gate stored it, sealed `dir: "req"` to the service's
recipients, which the per-job key cannot open; and a Task's artifact holds the chunks the gate merged (after an
`append: false`, only those since), so a node that first sees an artifact in a snapshot (a client that polls) cannot
tell that from a gate that dropped the first chunks (M3 WP-08). Decision, as built: the client's own sealed message
in a history is restored from what this node sealed for the job (its plaintext, kept in memory), or else left out of
what the local client gets, never refused; the first sight of an artifact in a Task may start at any chunk, its
chunks then consecutive, and every later chunk is next in order or a byte-identical repeat, anything else
`sealed_rejected` and dropped. After a restart in the middle of a job, an artifact is a first sight again, and the
client's own history parts are left out ([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` hiring.go
`openContainer`, `checkChunks`).

### D216. `chunk` counts every chunk a node sealed for an artifact — **decided under the owner's standing instruction, 28 Oct**

§3's `chunk` is "for artifact updates, the 0-based count of updates for that `artifactId` this node sealed; 0 for a
whole artifact". A whole artifact reuses the ciphertext of the parts the gate already holds (§5, so that polling
sees identical bytes), which carry the chunk they were sealed as, and its new parts cannot be chunk 0 again once a
chunk 0 was sealed: the hiring node refuses a `(chunk, part)` seen before with other bytes (rule 4) (M3 review
sealing-spec-7). Decision: the reviewer's amendment, which is what the node does: `chunk` is the 0-based count of
the chunks this node sealed for the `artifactId`, an update or the new parts of a whole artifact each counting as
one, so a whole artifact sealed first is chunk 0 and the updates that follow count on from it
([`ext-sealed.md`](../spec/ext-sealed.md) §3; `sealing` hosting.go `sealContainer`).

### D217. Only a sealed job's own exchanges reach its task, and a sealed job takes nothing unsealed — **decided under the owner's standing instruction, 28 Oct**

§5's rules bind the sealed parts of a request and say nothing of an exchange without one (a `GetTask`, a
`CancelTask`, a `SubscribeToTask`, an unsealed message) that names a sealed job's upstream task: its replies would
be sealed to whatever job the preamble names, or not at all, so a gate could read a sealed task's content on an
exchange of a job of its own (M3 WP-08). Nor do they say what a service with sealed jobs does with a request that
would carry a task's plaintext out of the node, or with an unsealed part inside a sealed job (M3 review security-3,
security-4). Decision, as built: an exchange reaches a sealed job's upstream task or context only if it is that
job's (its preamble names the job's gate task, or for MCP its session); a request naming another job's upstream task
is `wrong_task`, one naming another gate context's upstream context `wrong_context`, and a service reply naming a
sealed job's task on another exchange is refused before anything of it is forwarded. A service with sealed jobs, or
started with `--sealed-required`, refuses `bad_header` a push config (inline, by method or by route: it would have
the service post a sealed task's plaintext to a URL the gate chose), a JSON-RPC method that is none of A2A 1.0's
(0.3 methods included), and a body that is not one JSON object. In a job whose request was sealed, an unsealed part
beside sealed ones, an unsealed message and an unsealed `tools/call` on its session are `bad_header`, whatever
`--sealed-required` says, and the job's MCP tasks keep their `statusMessage` sealed on every exchange
([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` hosting.go).

### D218. A durable MCP task lives in its session's job, and its input requests and their answers name its gate task — **decided under the owner's standing instruction, 28 Oct**

§5 keys a hosting node's job "by the gate task id; for plain MCP calls, by the session hash", while rule 4 holds one
`replyTo` and `rk` per session and the hiring node keeps one per-job key per MCP session (§4). §3's MCP reply column
names `taskId` only for a durable task's result, and an input request is sealed "like a reply" with `session`,
`tool` and `messageId`; with those alone the hiring node cannot tell an input request replayed from another task of
the same session and tool (M3 WP-08). And the answer, sealed "like a request" with no `taskId`, binds `session`,
`tool` and `messageId`, while each durable task has its own upstream session whose ids start again, so a gate could
deliver the buyer's answer to one task's question as the answer to another's (M3 review security-2). Decision, as
built: a durable task is kept inside its session's job, with its gate task id and its input requests, so both keys
find it; a durable task's input request carries its gate `taskId` (a passthrough task's and a plain call's carry
none), which the hiring node checks under rule 4; an answer carries the `taskId` of the input request it answers
(absent when that had none), which the hosting node requires under rule 4b; and a `tools/call` still carries none.
Whether a response answers a sealed input request is read from the node's own index by upstream session and id,
whatever the preamble says (M3 review sealing-spec-3) ([`ext-sealed.md`](../spec/ext-sealed.md) §3, §5; `sealing`
mcp.go `sealInput`, `mcpRule1`).

### D219. A sealed plain call's input requests and answers are sealed — **decided under the owner's standing instruction, 28 Oct** (amends D62)

§2 and D62 seal the input requests of "a task the sealed call created", and say that the gate holds no plaintext of
a sealed MCP call, plain or durable. A plain call's server builds its input requests from the sealed arguments too
(`server-everything`'s `sampleLLM` sends the prompt), so they reached the gate in plaintext (M3 review security-6).
Decision: the reviewer's amendment, as built. A sealed plain call's `elicitation/create` and
`sampling/createMessage`, and the client's answers, are sealed as a task's are, the header naming the call's session
and tool, the server's id, and no `taskId`; the binding to the call is implicit (the call's own stream, and the
node's index by upstream session); the answers must come sealed and pass rule 4b; and the hiring node drops any
other input request on a sealed session. Upstream `notifications/message` and progress of a sealed call stay
unsealed, as §2 says of notifications ([`ext-sealed.md`](../spec/ext-sealed.md) §2, §3, §5; `sealing` mcp.go
`inputOf`).

### D220. A sealed job's artifact name and description are not relayed — **decided under the owner's standing instruction, 28 Oct**

§2 seals the content of every part and keeps "a part's `filename`" outside, with a warning; it says nothing of an
artifact's `name` and `description`, which a service may write from the sealed input (M3 review security-8).
Decision: the reviewer's first option, as built: on a sealed job the hosting node drops an artifact's `name` and
`description` (any case variant, which a lenient decoder takes for the member), and §2 says so beside its warning on
`filename`. Sealing them into the first part's body, the other option, would change the body's form, an envelope v2
question ([`ext-sealed.md`](../spec/ext-sealed.md) §2; `sealing` hosting.go `sealContainer`).

### D221. The envelope names no service: replay sets are node-wide, and the header gains a `service` member — **decided under the owner's standing instruction, 28 Oct**

§3's header binds the gate's task, context, session, tool and `messageId`, never the service, and §5 keeps the
replay sets per service, so a gate could deliver a buyer's sealed request to another service of the same node than
the one hired, which opens it and answers with a valid `auth`; the hiring node checks no service either (M3 review
security-5). Decision: the reviewer's proposal. Built: rule 2's `replyTo` set and rule 5's `(session, messageId)`
set are checked across every service of the node, so a request one service accepted is refused by every other
(`replayed`). And built since (D225): a `service` member in every request and reply header, the hired service as
the gate's URL path names it (`<name>`, or `<tenant>/<name>` on a multi-tenant gate), checked by the hosting node
against the preamble's `service` and by the hiring node against the service it hired. Envelope v1 is not released, so
the member joins v1 and both nodes, and `kontra seal`, change together; the recorded age CLI vector is made again
with it. The hosting node reads its services' paths from its `welcome` (each service's `url` and the `tenant`: only
the tenant is the gate's to say, the name being the node's own service), and a header naming another service, or
none, is `wrong_job`, the reason for a request that is another job's, in rule 1 so that it holds for a message, a
`tools/call` and an answer alike; no new §6 reason is added, which would change what both binaries speak. The hiring
node and `kontra seal --open` refuse a reply naming another service as they refuse any reply that fails rule 4. The
node-wide replay sets stay, so §7's row holds both ways ([`ext-sealed.md`](../spec/ext-sealed.md) §3, §5, §6, §7;
`sealed` envelope.go `Header.Service`; `sealing` hosting.go `checkService`, hiring.go `openContainer`, seal.go
`openParts`; `node` run.go `servicePaths`; `TestHostingChecksTheService`, `TestHiringChecksTheService`,
`TestReviewCrossServiceSplice`, `TestServicePaths`, `TestAgeInterop`, `TestSealedThroughTheGate`).

### D222. `kontra seal` takes `--api-key`, and `--open` binds a job for `--task` — **decided under the owner's standing instruction, 28 Oct**

§5.1 has `kontra seal` fetch the service's card with no word on a credential, but [`relay.md`](../spec/relay.md)
§2.1 answers a card `GET` of a service that is neither listed nor open to `anonymous` with `404` when no credential
admits the caller, so it could seal for listed services only (`examples/sealing`'s `allow: org:default` service
failed). §5.1 also mints a per-job key and `rk` for every message, `--task` included, while rule 3 requires a
follow-up to carry its job's `replyTo` and `rk` (`wrong_job` otherwise) (M3 WP-09). Decision, as built: `--api-key
<key>` (or `@<file>`) is sent as a bearer credential only to the `--service`'s origin, only to the service's card
once a `GET` of it without one was `404`, and then to its upstream card; never to the gate's key set, and never read
from the environment, which ties a key to no gate (the gate a `--service` names is untrusted, §7). `--open` binds a
job to the gate task its reply names (its key and state renamed to that id, as the hiring node names its jobs), and
`--task <id>` seals with that job's key, `rk` and `contextId`; a task with no job here is refused, with a hint to
`--open` one of its replies first. `--open` leaves out the client's own sealed message in a Task's history, as the
hiring node does (D215). The other option, the API key `kontra connect` keeps per gate in `~/.kontra/credentials`,
is not taken: `internal/sealing` cannot import `internal/node` ([`ext-sealed.md`](../spec/ext-sealed.md) §5.1;
`sealing` seal.go `SealCommand`, `keyedGet`; `sealed_replay`).

### D223. A seller signature that names its key by `kid` only is pinned unverified, and the gate will name the seller key set — **decided under the owner's standing instruction, 28 Oct** (amends D187)

§7 pins "the seller key thumbprints that verified its upstream card", and [`relay.md`](../spec/relay.md) §3 has a
verifier check the seller's signatures "against keys it trusts for the seller". `kontra connect` and `kontra seal`
pinned the `kid` of every signature as a thumbprint without verifying any, so a gate that replaced the card and its
signature with a forged one claiming the pinned `kid` left the pin unchanged (M3 review sealing-spec-4). The node is
given no seller key set: `seller_keys` is the gate's configuration. Built: a signature that carries its key (a `jwk`
in its protected header) is verified over the card's JCS and its key's RFC 7638 thumbprint pinned; one that names
its key by `kid` only is pinned as the unverified `kid:<kid>` and printed so, never as a thumbprint; a pin that
recorded a raw `kid` before reports a seller-key change once. Decision: the reviewer's first proposal. The gate's
`kontra.upstream` header parameter gains `jwks`, the URL on the gate's origin where it serves the seller key set it
verified with (`seller_keys`); the node fetches it through its tunnel, verifies `kid`-only signatures with it and pins
its keys' thumbprints, trusted on first use like every pin and compared out of band through the printed thumbprints
(D187). A `kontra connect --seller-keys` flag, the other option, would have every operator carry each seller's keys.
Built (D225): `jwks` is `{public_url}{svc}/.well-known/seller-jwks.json`, present only with `verified: true` (a
verifier that read exactly three members refuses the card, but v4 is not released and both sides change together);
the route serves the public keys (`kty`, `crv`, `x`, `y`, `kid`, `use`, `alg`, nothing private) of the set the
current card verified with, with the cards' admission and caching, `404` when the gate verified none and `503` while
the card is unavailable; the hiring node and `kontra seal` (its `--api-key` going to the set as to the cards, D222)
fetch it from the gate's origin only. A `kid`-only signature the named set does not verify is no seller key, as one
carrying a key that does not verify (D228); a named set that cannot be read leaves the card unreadable (D214's rules
then apply); with no set named, a `kid`-only signature is pinned unverified as before, and a pin that recorded
`kid:<kid>` reports a seller-key change once its gate names the set. Each replica installs its card with the set it
verified it with, so during a seller's key rotation a node may, for one refresh, read one replica's card and
another's set: the card then reads as unsigned, refused once and read again a minute later
([`ext-sealed.md`](../spec/ext-sealed.md) §7; [`relay.md`](../spec/relay.md) §2.1, §3; `jws` jws.go `Upstream.JWKS`;
`card` keeper.go, card.go `SellerJWKSHandler`; `sealing` seal.go `SellerKeySet`, `SellerKeyIDs`; `hiring` hiring.go
`verified`; `TestReviewSellerKeyIDsVerified`, `TestSellerKeySetNamedByTheGate`, `TestKeeperWithSellerKeys`,
`TestSellerKeySetThroughTheGate`, `TestCardPassthrough`).

### D224. A sealed MCP session's key outlives the gate's idle period, and the gate will state it — **decided under the owner's standing instruction, 28 Oct**

[`tunnel.md`](../spec/tunnel.md) §2.3 deletes an MCP session's per-job key "when the session closes and every task
created in it is done", but a client may leave without a `DELETE`, and the gate closes an idle session on its own
after the service's `session_idle_s` ([`relay.md`](../spec/relay.md) §11.2), which the node does not learn (M3
review robustness-3). Built: the hiring node deletes a session's key when the gate answers `404` for the session, or
when the session has had no exchange for a day (`sessionJobIdle`, beyond the gate's default `session_idle_s` of an
hour) and its tasks are done; a client of a gate that keeps sessions longer than a day must `initialize` again (the
node answers `404` for a session it no longer knows). Decision: the reviewer's first proposal: the gate states the
service's `session_idle_s` in the `initialize` result's `_meta["sh.kontra/session_idle_s"]`, and the hiring node
keeps an idle session's key that long; a flag on `kontra connect`, the other option, would have to match every
gate's setting. Built (D225): the gate writes the service's `session_idle_s` as a JSON integer into every legacy
`initialize` result's `_meta`, after the operator's `initialize_meta` and in place of any upstream's; the hiring node
keeps it with the session's job and deletes the key once the session has been idle that long and its tasks are done.
A value it does not take (not a whole number of seconds from 1 to 30 days, the node's own bound on a key a gate
asks it to keep), or none, counts as a day, as before ([`relay.md`](../spec/relay.md) §2.5, §11.2;
[`tunnel.md`](../spec/tunnel.md) §2.3; `mcprelay` legacy.go `initializeResult`; `sealing` hiring.go `sessionIdleOf`,
`sweepJobs`; `TestSessionKeyKeptForTheGatesSessionIdle`, `TestIdleSweep`, `TestSealedMCPThroughTheGate`).

### D225. What the M3 decisions leave to build — **decided under the owner's standing instruction, 28 Oct** (adds to D170)

Five options decided above were not built when the M3 decisions were recorded, and were to join D170's list for M4:
`kontra connect --listen off` (D208); the gate's cap of MCP sessions per principal and service,
`services.<name>.max_sessions_per_principal`, with the stdio bridge's eviction of a long-idle child (D213); the
envelope header's `service` member (D221); the seller key set the gate names in `kontra.upstream.jwks` (D223); and
`_meta["sh.kontra/session_idle_s"]` in the `initialize` result (D224). The review also left two smaller follow-ups:
the hosting node's sealing state took one node-wide lock, so that a commit's append and `fsync` and the job files'
writes happened under it (M3 review robustness-4), and D199's notice has no gate-level test across two replicas (M3
review mcp-spec-5). All five options are built in M3 now, before it merges, each with its spec text and its entry
above saying how: `--listen off` (`node` run.go; `TestRunListenOff`), the cap and the bridge's eviction (`mcprelay`
`makeRoom`, `stdiobridge` `reserve`, 0002's `services.max_sessions_per_principal`; `TestSessionsPerPrincipal`,
`TestFullBridgeStopsItsLongestIdleChild`, `TestSessionsPerPrincipalThroughTheGate`), the `service` member
(`sealing` `checkService`; `TestHostingChecksTheService`, `TestHiringChecksTheService`, `TestSealedThroughTheGate`),
the named seller key set (`card` `SellerJWKSHandler`, `sealing` `SellerKeySet`; `TestSellerKeySetNamedByTheGate`,
`TestSellerKeySetThroughTheGate`) and the stated `session_idle_s` (`mcprelay` `initializeResult`, `sealing`
`sessionIdleOf`; `TestSessionKeyKeptForTheGatesSessionIdle`, `TestSealedMCPThroughTheGate`); so is the review's
proposal for robustness-4: each hosted service's state has a lock of its own, and the node-wide replay checks of
D221 a lock of their own over an index of what each service recorded, filled at start, checked and reserved in one
step (`sealing` hosting.go `hostedService`, `claimNode`; `TestHostingServicesLockedApart`,
`TestHostingNodeWideReplyToIsClaimedOnce`, `TestHostingNodeWideMCPCallIsClaimedOnce`; an MCP call's claim names its
service as the A2A `replyTo`'s does, since a plain call has no gate task, M3 review D225-5). The specs no longer state those residuals. What remains for M4 of this
list is mcp-spec-5's gate-level two-replica test of D199's notice; D170's list is otherwise unchanged.

### D226. A sealed MCP job holds its upstream session, and its requests' ids — **decided under the owner's standing instruction, 28 Oct** (amends D217)

D217 lets an exchange reach a sealed job's upstream task or context only if it is that job's, "for MCP its
session", and the hosting node bound a job to the preamble's `mcp_session`. The upstream session it forwards on is
the request's `Mcp-Session-Id`, which the gate chooses as it does the preamble: with a preamble naming another gate
session, or none, and the header naming the buyer's upstream session, the gate could send an unsealed `tools/call`
into the session the buyer's sealed calls set up (a login, an opened document) and read its result, or a sealed one
of its own and open the result with its own key (M3 review round 2, sealing-fixes-2). And 6ba105f dropped a sealed
call's record whenever a later request reused its JSON-RPC id, before anything refused that request: a `ping` with
the id, then a `GET` with `Last-Event-ID`, had the server replay the sealed result into no record, and the node
relayed it in plaintext (sealing-fixes-1). Rule 4a and rule 4b also matched a response by the id's exact spelling,
where the TypeScript SDK takes `"8"` for `8` (`Number(id)`), so a gate could answer a sealed call in plaintext with a
string id (other-fixes-2). Decision, as built, the reviewers' first proposals: the upstream session a sealed call of
a job went on is indexed to the job in the service's state (in flight from the node's checks on, as the job's
binding is), and any message on it from an exchange of another session, or of none, is refused (`wrong_job` for a
sealed `tools/call`, `bad_header` otherwise), and nothing the server sends on it reaches such an exchange; a request
that reuses the id of a sealed request the node recorded on the session is `replayed` (MCP never reuses an id in a
session), and the record stays; both nodes match a response to a request as the gate does
(`mcpwire.SameID`, `AnswerKeys`); the gate's durable dispatch draws its own ids per run, so a re-attach on the same
upstream session (D42) reuses none. On a sealed job's exchange, a response that answers neither the exchange's
request nor one the node holds is not relayed, so that Sweep can drop an answered request's index entry once its
record is older than `--sealed-max-age` (the indexes no longer grow with a long-lived session, sealing-fixes-9). The
other proposal, scoping the reuse to the reusing exchange, would still relay a sealed result a server routes by id
to that exchange ([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` mcp.go `sessionOwner`, `recorded`,
`FromServer`; `TestReviewReusedIDAfterSealedCall`, `TestReview2SealedJobOwnsItsUpstreamSession`,
`TestReview2StringSpelledIDsAreSealed`, `TestReview2SweepPrunesAnsweredIndexEntries`).

### D227. A sealed A2A job is guarded before its first reply, and a request the service had is recorded — **decided under the owner's standing instruction, 28 Oct** (amends D217)

D217's guard held only once a job's first reply had named its upstream task. Before that, the gate could list the
service's tasks (`ListTasks` with `historyLength`) on any exchange, the job's own included, and read the buyer's
message in plaintext, holding the window open with a blocking send (`returnImmediately` is the gate's to set); and a
gate that ended the tunnel stream before the answer released the job, although the service had the message, and
could then open the gate task with a binding of its own (M3 review round 2, sealing-fixes-3). An unsealed first
message of another gate task naming the sealed job's upstream context passed, and its reply too, so an agent keeping
a conversation per context could repeat the buyer's (sealing-fixes-6). And a job went 7 days after its last exchange
whatever `--sealed-max-age` was, so with a longer one the first message resent under its own task found no pairs
(sealing-fixes-8). Decision, as built, the reviewers' proposals: a service with sealed state (or a job being opened,
or `--sealed-required`) refuses a task list `bad_header` (the gate never lists the upstream's tasks, relay.md §6); a
request of a gate task whose first sealed message the service has not answered is `wrong_task`; a reply on an
exchange of no sealed job that names an upstream task or context the node has not indexed waits while a job of the
service is being opened (at most 30 s, then `wrong_task`) and is read again; a request the node handed whole to the
service whose exchange ends before the answer is recorded as an accepted one (a node-written answer, or a non-2xx,
records nothing, as before); an unsealed message into any sealed job's upstream context is `bad_header`, and a reply
of an unsealed exchange naming one is refused; a job is kept for 7 days or `--sealed-max-age` and the clock skew,
whichever is longer. The residual: a first message whose exchange ended before its reply leaves its upstream task
unindexed, readable on another exchange by an id the gate already knows (not by a list). The other proposal for the
window, a provisional job other exchanges seal to, would write a job before the service accepted its request
([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` hosting.go `replyHeld`, `End`, `other`, `unsealedSend`,
`Sweep`; `TestReview2NoTaskListWhileAJobOpens`, `TestReview2ReplyHeldWhileAJobOpens`,
`TestReview2AbandonedFirstMessageIsRecorded`, `TestReview2UnsealedMessageIntoSealedContext`,
`TestReview2JobKeptForMaxAge`).

### D228. A card whose seller signatures verify nothing is unsigned, and a lost seller key needs acceptance — **decided under the owner's standing instruction, 28 Oct** (amends D187, D223)

D223 verifies a seller signature that carries its key, but a signature that did not verify was only dropped from the
seller keys, the card still counted as signed, and losing a pinned key (`removed_seller_key`) was accepted silently,
even under `--pin-recipients`: a gate could replace the upstream card, keep a signature that verifies nothing, and
the pin was rewritten with no seller key (M3 review round 2, sealing-fixes-5). Decision, as built, both halves of the
reviewer's proposal: a card is signed only when a seller signature verified, or names its key by `kid` only
(unverifiable, D223), so a pinned signed card replaced so is `unsigned`, always refused; and a removed seller key is
refused under `--pin-recipients`, and otherwise until `--accept-change <service>`, as a new one is
([`ext-sealed.md`](../spec/ext-sealed.md) §7; `sealing` seal.go `VerifyCard`, `hiring` hiring.go `verified`, `pins`
`needsAccept`; `TestReview2SellerSignatureThatVerifiesNothingIsUnsigned`, `TestAcceptedChanges`).

### D229. The hiring endpoint checks every connection's owner, and attributes only sockets a process holds — **decided under the owner's standing instruction, 28 Oct** (amends D207)

The same-user check of [`tunnel.md`](../spec/tunnel.md) §9 ran only for a connection from a loopback address, while
Linux accepts a connection to 127.0.0.1 from any of the machine's addresses (`curl --interface`); it read the uid of
the first matching socket table entry, which the kernel lists as 0 for a socket its owner closed (orphaned,
TIME_WAIT), so a request sent and closed at once reached a node running as root; and it read `/proc/net`, hidden by
`ProcSubset=pid`, letting every connection through with no word (M3 review round 2, other-fixes-1, -3, -4).
Decision, as built: every connection to a loopback `--listen` without `--local-token` is checked, whatever its source
address; only an entry with a non-zero inode (a socket a process holds) is attributed, and the overflow uid (a user a
user namespace does not map) attributes nothing; the tables are read from `/proc/self/net`, and on Linux a table
that cannot be read closes the connection, with a warning at start (as does a node running as the overflow uid).
Off Linux the check is still not made ([`tunnel.md`](../spec/tunnel.md) §9; `hiring` peer.go; `TestFindPeer`,
`TestPeerListenerAccept`, `TestPeerUIDTables`, `TestHiringRefusesOtherLocalUsers`).

### D230. A LISTEN reconnect drops the drained tool lists too — **decided under the owner's standing instruction, 28 Oct** (amends D199)

D199 has the replica that relays an upstream's `tools/list_changed` tell the others by `NOTIFY kontra_tunnel
'tools:…'`, and a LISTEN reconnect drop every era "for a lost notice"; a `tools:` notice lost while a replica's
connection was down was never made up for, and the replica kept the old list, and the `Unknown tool` decisions on
it, for up to the 10-minute refresh (M3 review round 2, other-fixes-5, records-6). Decision, as built: the reconnect
drops every cached list as well as every era ([`relay.md`](../spec/relay.md) §11.3; `mcprelay` era.go
`Reconnected`, `gate` gate.go; `TestToolsCacheReconnected`).

### D231. The session cap closes only an idle session, and leaves `anonymous` and direct backends alone — **decided under the owner's standing instruction, 28 Oct** (amends D213)

The review of D225's builds found D213's gate half too blunt (M3 review D225-1 to -4). Every unauthenticated caller is
the one principal `anonymous`, so one budget held every anonymous client of a service, and any anonymous `initialize`
closed another client's session, even one in use: one caller looping `initialize` ended every other anonymous
session. A hiring node's local clients, and every user of one API key, are one principal too, so at the default of 2
a third client closed another's session and they then closed each other's in turn; and the cap applied to direct
HTTP backends, which have no children to share. The longest-idle session was the oldest `last_seen_at`, written only
as a request starts and at most once a minute, so a session with a long `tools/call` or an open `GET` stream was
taken for idle and closed mid-call, its client losing the result. And a buyer's several keys, and its durable tasks'
own upstream sessions (D22), each escaped the cap. Decision, as built ([`relay.md`](../spec/relay.md) §2.3, §11.2,
§17; [`tunnel.md`](../spec/tunnel.md) §9, §10; `mcprelay` exchange.go `capped`, `makeRoom`, `openSession`,
sessions.go `Evict`, `OpenCapped`, inuse.go):

- **Only an idle session is closed for a new one.** An `initialize` beyond the cap closes as many of the principal's
  sessions as it needs among those whose `last_seen_at` is at least 5 minutes old (`EvictIdle`, the stdio bridge's
  own bound), the oldest first, with reason `idle` and the upstream `DELETE` of D213. When too few are that old it
  closes none and is refused `429` with `Retry-After: 60` and JSON-RPC error `-32600` `too many sessions`
  (`data.max_sessions_per_principal`), as the edge's in-flight caps are (§2.2): a refusal costs the new client a
  retry, where a close cost a working client its session.
- **A session in use is never idle.** The replica serving a request or a `GET` stream of a legacy session writes its
  `last_seen_at` at least every 90 s while one is open (a timer per session in use; the column is still written at
  most once a minute) and once more as the last ends when it has not for a minute, so a session in use on any replica is never
  5 minutes old; the replica's own sessions in use are also excluded by hash. The idle sweep reads the same column,
  so a session with a `GET` stream open is no longer closed under `session_idle_s` while the stream lasts (for a
  `session_idle_s` over 90 s), as the bridge never stops a child with one open (D213).
- **The check is atomic.** It runs in one transaction under an advisory lock of the (tenant, service, principal),
  before the upstream `initialize` (so a refused one reaches no upstream) and again as the new row is inserted (so
  concurrent `initialize`s of one principal leave at most the cap open); the one that finds no room the second time
  writes no row, is refused the same way, and its upstream session gets a `DELETE`.
- **`anonymous` is not capped.** Its callers are distinct clients the gate cannot tell apart: a shared budget lets any
  of them crowd out, or close, the others' sessions, and a separate one that refuses would let one caller hold every
  place until `session_idle_s`. A service open to `anonymous` shares its upstream's sessions with everyone; the
  bridge's own eviction of an idle child and the edge's limits are what bound them.
- **Direct backends are not capped.** A direct `mcp: {url}` backend is an HTTP server the operator runs, with no
  stdio children; the cap counts and closes only sessions pinned to a tunnel. The gate cannot tell a node's stdio
  server from its URL server, so a tunnel service is capped either way.
- **The default is 4**, half the bridge's default `--max-sessions`: one principal keeping its sessions in use leaves
  another room on a default bridge, and a hiring node (or a team key) serves four clients of a service at once, its
  idle ones making room for more. Every local client of a hiring node, and every user of one API key, shares the
  principal's cap, as [`tunnel.md`](../spec/tunnel.md) §9 now says.
- **The cap counts the credential.** It stays per principal, the identity a session is bound to: counting a
  `buyer_id` would make every key minted for one buyer share one budget, the sharing above, and a party that can mint
  keys can mint buyers too. A buyer's several keys each hold the cap. A durable task's upstream session (D22) is the
  dispatch's for the attempt and is not counted: a principal's durable tasks on a stdio service can hold its children
  (the bridge never sees them idle, the dispatch polls), bounded only by `limits.dispatch_slots`. A cap of a
  principal's durable tasks per service joins what D225 leaves to M4.

Tests: `TestSessionsPerPrincipal`, `TestSessionsCapLeavesAnonymousAlone`, `TestSessionsCapLeavesDirectBackendsAlone`,
`TestSessionsCapNeverClosesASessionInUse` (a held call and a `GET` stream through the peer hop, the new
`initialize`s on the other replica), `TestSessionsCapConcurrentInitializes`, `TestSessionsCapCountsTheCredential`,
`TestSessionsPerPrincipalThroughTheHop`, `TestSessionsPerPrincipalThroughTheGate`.

### D232. On a sealed session an input request is sealed or not relayed, on whichever exchange it comes — **decided under the owner's standing instruction, 28 Oct** (amends D219)

D219 sealed a plain call's `elicitation/create` and `sampling/createMessage` only when they came on the call's own
`POST` exchange. The stdio bridge routes every server request without a related task to the session's `GET` stream,
and a URL server resumed with `Last-Event-ID` replays one there: on that exchange the node had no call to attribute
it to, relayed it in plaintext (the canonical `server-everything` deployment puts the sealed prompt into it), and then
forwarded the gate's own unsealed answer, since the request was in no record (M3 review round 3, r3-hosting-1).
Decision, the reviewer's proposal: on an exchange of a session that holds a sealed job (or whose first sealed call is
with the server) such a request is sealed or not relayed, never relayed in plaintext. It is a task's when its related
task, or the exchange's gate task, names one; the exchange's own sealed call's; or else the session's one sealed plain
call the server has not answered, on that upstream session. A call in flight counts: a URL server that answers JSON
sends its headers only with its result, and its request about the call is its acceptance of the call, which the node
then records as `Accepted` would. With none, or more than one, the request goes nowhere, and its id is recorded on the
upstream session (swept after `--sealed-max-age`), so that any answer to it, sealed or not, is refused: no client saw
it. Every answer the gate could give for the buyer is so either checked by rule 4b or refused; a server's `ping`, the
only other request a client answers, carries nothing. The hiring node opens a plain call's input request on a `GET`
too, when its `tool` is one of the session's unanswered sealed calls' ([`ext-sealed.md`](../spec/ext-sealed.md) §2,
§3, §5; `sealing` mcp.go `soleCall`, `dropInput`, `answer`; hiring.go `openInput`; `TestReview3PlainCallInputOnGET`,
`TestReview3UnattributableInputIsNotRelayed`, `TestReview3InputWhileFirstCallInFlight`,
`TestReview3StdioPlainCallInputOnGET`, `TestReview3URLInputOnResumedGET`).

### D233. An MCP message the node handed whole is recorded when the gate ends its exchange — **decided under the owner's standing instruction, 28 Oct** (amends D227)

D227's rule, that a request the node handed whole to the service whose exchange the gate ended before the answer is
recorded as an accepted one, was built for A2A only: an MCP exchange was recorded when the server's response headers
came, and a server that answers JSON sends them with its result, so a gate that ended the stream after the server had
the buyer's sealed call released everything. It could then open the buyer's session's job with a sealed call of its
own and read the upstream session the buyer's call set up, or replay the buyer's call, which ran again (M3 review
round 3, r3-hosting-3). Decision, as the reviewer proposed: the MCP exchange marks itself handed once the transport has
read its forwarded body whole, and its end with no answer records it as `Accepted` does (the job's binding, `(session,
messageId)`, the upstream session's index, the call's record); one never handed is released, as before. One rule now
holds for both protocols ([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` mcp.go `End`, `Body`; `node` proxy.go;
`TestReview3MCPCutBeforeAnswerIsRecorded`).

### D234. The hosting node refuses a 2026-07-28 request to a sealed MCP service itself — **decided under the owner's standing instruction, 28 Oct** (amends D162)

D162 has the gate never serve a sealed MCP service to a 2026-07-28 request, and only the gate enforced it: a gate could
send the buyer's sealed legacy `tools/call` as a 2026-07-28 request, its modern `_meta` keys beside the envelope (which
binds only its own key), and the node opened it. A dual-era server then answered with what the modern shapes carry
outside `content` (a multi round-trip's `inputRequests` and `requestState`, built from the arguments; a
`CreateTaskResult` at the top of `result`, not the legacy `result.task` the node binds) in plaintext (M3 review round 3,
r3-hosting-2). Decision, the reviewer's proposal: the node reads the exchange's era itself and refuses `bad_header`,
before anything reaches the server, every 2026-07-28 request to an MCP service with sealed state or started with
`--sealed-required`, and any sealed `tools/call` on a modern exchange; and, in depth, a reply to a sealed call that
carries `resultType`, `inputRequests`, `requestState` or a top-level `taskId` is never relayed. The class closes
because no 2026-07-28 shape reaches a sealed service at all, whatever the gate checked
([`ext-sealed.md`](../spec/ext-sealed.md) §2, §5; `sealing` mcp.go `SetModern`, `modernResult`; `node` mcp.go;
`TestReview3SealedCallAsModernIsRefused`, `TestReview3ModernRefusedOnSealedService`).

### D235. The hosting node reads the route the service gets — **decided under the owner's standing instruction, 28 Oct** (amends D217, D227)

The sealing classified a request by its raw path, while the node forwards it cleaned (`path.Clean`, [`tunnel.md`](../spec/tunnel.md)
§8.2): `GET /tasks/x/..` read as a `GetTask` of `x` reached the service as `GET /tasks`, a2a-go's `ListTasks`, past
D227's refusal of a task list, and `…/tasks/{id}/./pushNotificationConfigs` reached it as a push config route, past
D217 (M3 review round 3, r3-hosting-4). Decision, the reviewer's first option: the sealing reads the path the node
forwards, cleaned as the node cleans it (a trailing slash kept); the node hands it the forwarded path itself. There
is one spelling of a route on the node, so no spelling can hide one ([`ext-sealed.md`](../spec/ext-sealed.md) §5;
`sealing` hosting.go `cleanRoute`; `node` proxy.go; `TestReview3DotSegmentRoutes`,
`TestReview3DotSegmentRoutesAreRefused`).

### D236. Services that reach one upstream keep one sealed state — **decided under the owner's standing instruction, 28 Oct** (amends D217, D226)

Every guard of D217 and D226 was kept per service name, while the node accepts two `--serve` names with one target,
and one service's base path may cover another's on one origin: an exchange of the other service reached the sealed
job's upstream task or session unguarded, and its replies passed unread (an unsealed `whoami` on a second MCP service
read the buyer's sealed login; a `GetTask` on a second A2A service read the buyer's task) (M3 review round 3,
r3-hosting-5). Serving one server under two names is a natural configuration (per-tool policy and pricing are per
service: a read-only and a full tier), and the e2e suites do it, so refusing it at start, the reviewer's first option,
was not taken. Decision, the second: the node groups the services whose targets share an upstream (one origin, any
loopback name of a loopback host as one, a default port written or not; the base path of one the other's or under
it, the root covering every path; a stdio service shares nothing) and the hosting sealing keeps each group's state
as one, under the group's first name: its locks, job files, indexes and the `guarded` test. A header's `service` and
`--sealed-required` stay per service. Every guard then holds across the services one upstream is reached by, since
there is one state to consult ([`tunnel.md`](../spec/tunnel.md) §8.2; [`ext-sealed.md`](../spec/ext-sealed.md) §5;
`node` serve.go `upstreamGroups`; `sealing` hosting.go `stateOf`; `TestReview3TwoServicesOneUpstreamMCP`,
`TestReview3TwoServicesOneUpstreamA2A`, `TestReview3UpstreamGroups`).

### D237. Every task a request names counts, a message's references included — **decided under the owner's standing instruction, 28 Oct** (amends D217)

The node read only a message's `taskId` and `contextId`, while A2A 1.0's `referenceTaskIds` name tasks an agent loads
into the message's context, history and artifacts in plaintext (a2a-go's `ReferencedTasksLoader`, a2a-python's referred
tasks): a message of any gate task referencing the buyer's sealed upstream task was forwarded, and its reply passed
unread, or sealed to the gate's key (M3 review round 3, r3-hosting-7). And `other()` checked the index only when the
exchange's job knew its own upstream task, so a job recorded by `End` reached another job's task (r3-hosting-9).
Decision, the reviewer's proposal: `referenceTaskIds` is read with exact keys on every send; an entry naming another
sealed job's upstream task is `wrong_task`, sealed message or not, and on a service with sealed jobs an unsealed
message with any reference is `bad_header` (the node cannot tell a task it has not indexed, D227, from another);
every task id a request names is checked against the index, whatever the job knows. D217's rule now reads every
member of A2A 1.0 that names a task ([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` hosting.go
`referenceTaskIDs`, `other`; `TestReview3ReferenceTaskIDs`, `TestReview3OtherChecksIndexWithoutUpstreamTask`).

### D238. A sealed MCP exchange's error that is not JSON keeps its status only — **decided under the owner's standing instruction, 28 Oct** (amends D195)

D195 relays a sealed job's error with its code and fixed text. A URL MCP server's non-2xx answer whose body is not JSON
(Go's `http.Error`, a framework's error page, a validation error quoting the refused argument) was not read, so it
reached the gate in plaintext; the A2A side replaces any non-2xx body on a sealed job (M3 review round 3,
r3-hosting-8). Decision, the reviewer's proposal: on an exchange that carries a sealed call or an answer to a sealed
input request, sits on a session with a sealed job, or is of a service started with `--sealed-required`, a non-2xx
answer that is neither JSON nor an event stream is replaced by the node's own JSON-RPC error with the request's id,
code `-32603` and D195's fixed text, its status kept ([`ext-sealed.md`](../spec/ext-sealed.md) §2; `sealing` mcp.go
`SealedReply`, `ErrorBody`; `node` mcp.go; `TestReview3MCPNon2xxTextErrorIsScrubbed`).

### D239. An MCP header names its role: `op` — **decided under the owner's standing instruction, 28 Oct** (amends D175, D218)

An MCP envelope's header did not say which of four roles it plays: a `tools/call`'s arguments, the client's answer to
an input request, a result, an input request. A plain call's answer has no `taskId` (D218), so its header had a
`tools/call` header's form, and an input request's had a result's; and `messageId` holds the client's ids and the
server's. A gate could send the buyer's sealed answer to elicitation `N` of tool `X` as a `tools/call` with id `N`, and
the node ran `X` again with the answer as its arguments; or deliver an input request whose server id equals a later
call's id as that call's result, which the hiring node opened as an empty success (M3 review round 3, r3-hiring-5).
Decision, the reviewer's first option: the header gains `op`, `"call"` | `"answer"` (dir `req`, the hiring node's) |
`"result"` | `"input"` (dir `res`, the hosting node's), on every MCP header (one with `session`) and no A2A header,
which `Canonical` enforces; it is authenticated with the rest of the header (the age file, and `auth` for replies).
The hosting node takes a `tools/call` only with `op: "call"` and an answer only with `"answer"` (`wrong_message`); the
hiring node a result only with `"result"`, whose body holds `content` or `structuredContent`, and an input request
only with `"input"`. Envelope v1 is not released, so the member joins v1, and both nodes change together, as with
D221's `service`. One member closes every splice between roles, where namespacing the server's ids in `messageId`,
the other option, would leave a client free to choose a request id that looks like one
([`ext-sealed.md`](../spec/ext-sealed.md) §3; `sealed` envelope.go `Header.Op`; `sealing` mcp.go, hiring.go;
`TestReview3RolesAreNotSpliced`, `TestInputRequestHeaders`).

### D240. MCP sessions and A2A services are kept apart on the hiring node — **decided under the owner's standing instruction, 28 Oct**

The hiring node kept A2A services by their path and MCP sessions by the gate's `Mcp-Session-Id` in one map, and the
gate chooses that id: answering an MCP `initialize` with `Mcp-Session-Id: /agents/<sealed service>` wrote the session's
"not sealed" over the A2A service's entry, and the service's next messages went unsealed for a minute, renewable at
will (M3 review round 3, r3-hiring-2). Decision, as proposed: the two are separate maps, and an `initialize` result whose
session id is not MCP's form (visible ASCII, 0x21 to 0x7E) or starts with `/` is refused. No id the gate chooses can
then stand for anything but an MCP session ([`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4c; `sealing` hiring.go
`sessions`, `sessionIDForm`; `TestReview3SessionIDCollidesWithA2APath`).

### D241. A sealed MCP session's job stays while one of its exchanges is open — **decided under the owner's standing instruction, 28 Oct** (amends D224)

Round 2 refused unread messages only on `GET`s opened after a session's job was gone. A stream already open when the
job went (a gate `404` on another exchange closes the session; the sweep runs with the `session_idle_s` the gate chose,
down to 1 s, and in-flight requests did not refresh it) then passed, unread, a plaintext result for a pending sealed
call; with an honest gate, a sealed call that outlasted `session_idle_s` across a sweep lost its key and its genuine
result was refused (M3 review round 3, r3-hiring-6). Decision, the reviewer's proposal: the node counts each session's
exchanges in flight (the hiring server ends one when its handler returns) and never drops a session's job, by the
sweep or a `404`, while one is open; the last to end drops a closed session's. And on a sealed session a result that
answers no request the node tracks is refused on a `GET`, and on any stream once the node no longer holds the session.
The job, and the records of its sealed requests, outlive every stream that could carry their answers
([`tunnel.md`](../spec/tunnel.md) §2.3; [`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4a, 4c; `sealing` hiring.go
`Job.Done`, `maybeEndSession`, `mcpMessage`; `TestReview3OpenGETAfterSessionJobDropped`,
`TestReview3SweepKeepsAnOpenCallsKey`).

### D242. A session entry with no job is swept when idle — **decided under the owner's standing instruction, 28 Oct**

Every MCP `initialize` added a session entry that went only with a `DELETE`, a gate `404` or the session's job; a
session that never made a sealed call, every session of an unsealed service included, has no job, and clients often
leave without `DELETE`, so a long-running `kontra connect` grew with each new session (M3 review round 3,
r3-hiring-10). Decision: an entry records when it was last used, and the sweep drops one with no job and no exchange
open once it has been idle for its bound (the gate's `session_idle_s`, else a day) ([`tunnel.md`](../spec/tunnel.md)
§2.3; `sealing` hiring.go `sweepJobs`; `TestReview3UnsealedSessionEntryIsSwept`).

### D243. An input request is opened once per id space, and one gate id carries one — **decided under the owner's standing instruction, 28 Oct** (amends D218, D219)

The hiring node recorded which sealed question a gate request id stands for by plain overwrite and never cleared it:
a gate that delivered question Q1 and then Q2 with the same id had the client's answer to Q1 sealed as Q2's (the
TypeScript SDK runs both handlers and answers each with the id), so "may I read notes.txt?" accepted became "may I
delete everything?" accepted (M3 review round 3, r3-hiring-4). And it counted "opened before" per call or task, while
an input request's `messageId` is the upstream session's: a plain call's question replayed on another plain call's
stream opened again (r3-hiring-8). Decision, the reviewers' proposals: an input request on a gate id the session holds
for an unanswered one is dropped; the entry goes once its answer is sealed, and a second answer to the id is refused
locally; "opened before" is kept apart from the pending answers, per durable task for an input request that names one
and per session for one with no `taskId` (plain calls and passthrough tasks share their session's upstream session);
and once an input request of a task named it, one of that task's that does not is dropped (a durable task's always
do, D218). Each question is then opened once and answered once, under the id it came with
([`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4; `sealing` hiring.go `openInput`, `sealAnswer`, `hireJob.Opened`,
`Answered`; `TestReview3InputRequestIDReuse`, `TestReview3InputRequestReplayedAcrossCalls`).

### D244. A `CreateTaskResult` answers only a call that asked for a task, and holds only the task — **decided under the owner's standing instruction, 28 Oct**

The hiring node passed unread any result of a sealed call that had a `task` object, a plain call's included: a gate
answered `{"task": …, "content": [forged]}`, and stock clients, which ignore `task`, read the forged `content` as the
tool's result; the forged task also kept the session's key forever (M3 review round 3, r3-hiring-3). Decision, the
reviewer's proposal: the node records whether a sealed call carried `params.task`, and takes a `CreateTaskResult` only
for such a call and only with nothing beside `task` and `_meta`; anything else is refused as an unsealed result is,
and only then is the task registered ([`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4a; `sealing` hiring.go
`openAnswer`; `TestReview3ForgedCreateTaskResult`).

### D245. One local client per sealed session, and a reused id is refused on the hiring node too — **decided under the owner's standing instruction, 28 Oct** (amends D226)

The hiring node accepted an `initialize` result naming a session id it already held, so a gate could put a second
local client into a sealed session's job; and it deleted a sealed call's record whenever the session sent any request
reusing the call's id, the deletion round 2 removed from the hosting node: two stock clients number their requests
alike, so client B's `tools/list` id 3 deleted client A's pending sealed call 3, and the gate's plaintext result for it
reached A on its `GET` (M3 review round 3, r3-hiring-7). Decision, as proposed: an `initialize` result whose session id
the node holds (an entry, or a job's file) is refused; a request that reuses a sealed request's id is refused locally,
as the hosting node refuses it (D226), and the record stays the sealed request's. The two nodes then apply one rule
to a session's ids ([`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4c; `sealing` hiring.go `mcpRequest`,
`initResult`; `TestReview3SessionFixation`, `TestReviewReusedIDAfterSealedCall`).

### D246. A client's error answer to a sealed input request goes with its code only — **decided under the owner's standing instruction, 28 Oct** (amends D175)

A JSON-RPC error answer to a sealed input request (a user who rejects a sampling request, MCP 2025-11-25's `-1`; a
client without a handler's `-32601`) could not be sent: the hiring node refused it locally, and the hosting node would
have refused it unsealed, so the service never learnt it and the call or task waited for its own timeout; §3 and §5 said
nothing of it (M3 review round 3, r3-hiring-9). Decision, the reviewer's second option, D195's rule for errors: the
hiring node sends the error with its code and fixed text in place of its message and data; the hosting node takes it,
once, on an exchange of the input request's session and task, as that request's answer, with its code and fixed text.
An error carries no answer, so sealing it is not needed: a gate can send one of its own to deny the buyer's input
request, as it can drop the request, never answer it, and §7 says so. Sealing it, the other option, would give the
envelope an error body for no confidentiality gained ([`ext-sealed.md`](../spec/ext-sealed.md) §2, §5, §7; `sealing`
hiring.go `sealAnswer`, mcp.go `declined`; `TestReview3ErrorAnswerToSealedInputRequest`).

### D247. A served card names the service's own path, and the hiring node seals by it — **decided under the owner's standing instruction, 28 Oct** (amends D207)

The hiring node seals by path, and the gate chose the path a local A2A client posts to: the card it serves kept the
gate's path in every `supportedInterfaces[].url`, so a signed card for a pinned sealed service could send the client's
messages to `/agents//x`, `{svc}/.well-known/a`, `/.well-known/a` or `/mcp`, none of which the node sealed (a card read
was any method on a `.well-known` path), or to another service's path with a fresh first-sight pin (M3 review round 3,
r3-hiring-1). Decision, the reviewer's proposal: the card served locally names `http://localhost:<port>{svc}`, the
service it was read for, in every interface; a card read is a `GET` (or `HEAD`) only, and only a `GET` card route is
unsealed; a request other than a `GET` to `/.well-known/…` or to an `/agents/…` path that names no service is refused
`404` locally. The client can then reach only the path the node seals for the pinned service
([`tunnel.md`](../spec/tunnel.md) §9; [`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 1; `hiring` cards.go
`RewriteCard`, hiring.go `Handler`; `sealing` hiring.go `Request`; `TestReview3InterfaceURLIsTheService`).

### D248. `--accept-change` accepts the change pending at a service's first check of the run — **decided under the owner's standing instruction, 28 Oct** (amends D228)

`--accept-change <service>` accepted every later needs-accept change of that service for the life of the process, so
a node left running with it (a systemd unit, say) recorded the service's later recipient substitutions and seller-key
swaps silently, where the pin is meant to refuse them until the operator accepts the change it saw (M3 review round 3,
r3-hiring-11). Decision, the reviewer's first option: the acceptance applies to the service's first check in the
process, the change the operator saw refused and restarted to accept; any later change is refused until the next
explicit acceptance. `kontra connect --help` and §7 say so; `--accept-change NAME=<key ids>`, the other option, would
have the operator copy key ids for a decision the restart already expresses ([`ext-sealed.md`](../spec/ext-sealed.md)
§7; `pins` pins.go `Check`; `TestAcceptChangeIsOnce`).

### D249. The session cap evicts after 2 minutes, and its `Retry-After` says when a retry finds room — **decided under the owner's standing instruction, 28 Oct** (amends D231)

D231 let an `initialize` beyond the cap close only sessions idle for 5 minutes, the stdio bridge's own bound. The
TypeScript SDK, and Claude Code built on it, end a session without a `DELETE`, so the fifth short-lived client of one
key within 5 minutes was refused `429`, and its `Retry-After: 60` led to another refusal: no retry inside a stock
client's reconnect window could succeed (M3 review round 3, r3-relay-1). Decision, the reviewer's first proposal: the
gate's bound (`EvictIdle`) is 2 minutes, just above what a session in use shows (its `last_seen_at` is written at least
every minute and a half while a request or stream of it is open, so a session in use is still never closed), and the
`Retry-After` is the time until enough of the principal's sessions not in use here will be that idle (2 minutes when
the sessions in use leave too few). A client abandoned without a `DELETE` then makes room within two minutes, and
the refusal says exactly when ([`relay.md`](../spec/relay.md) §11.2; `mcprelay` sessions.go `capped`,
`TooManySessionsError`; exchange.go `tooManySessions`; `TestReview3SerialShortLivedClients`).

### D250. A session the gate answers `404` for without a hop is closed, and a `DELETE` always closes it `deleted` — **decided under the owner's standing instruction, 28 Oct** (amends D196)

A multi-replica gate without `peer.listen` answers a request for a session another replica holds `404`, and §11.2 tells
the client to initialize again; the row stayed open and, touched by the request, looked freshly used, so the new
`initialize` counted it and after `cap` bounces the client was refused `429` holding no usable session (M3 review round
3, r3-relay-2). A `DELETE` that reached such a replica, or found the holder replica no longer live or the tunnel
revoked, was answered `404` and the row closed `tunnel_lost` or left open, against D196's "a `DELETE` always closes the
row (deleted)", and the TypeScript SDK's `terminateSession()` then throws (r3-relay-3). Decision, the reviewers'
proposals: the no-hop `404` closes the row `tunnel_lost`, so a session the client is told is gone is gone; and a
`DELETE` is dispatched before the target's error is acted on: it closes the row `deleted` and answers `200` when the
tunnel is gone or cannot be reached, `503` only for a key the gate could not check ([`relay.md`](../spec/relay.md)
§11.2; `mcprelay` legacy.go `pinned`, `upstreamFailed`; `TestReview3NoHopRowsDoNotCountAgainstTheCap`,
`TestReview3DeleteClosesTheRowDeleted`).

### D251. A gate's own sealed first message into a buyer's context is a residual — **decided under the owner's standing instruction, 28 Oct** (amends D227)

D227 refuses an unsealed message into a sealed job's upstream context, since an agent that keeps a conversation per
context would answer it from the buyer's sealed one. A gate can seal a first message of its own to the node's public
recipients, with its own `replyTo`, under a new gate task but the buyer's gate context and its recorded upstream
`contextId`: rule 2 accepts it, the agent answers from the buyer's conversation, and the answer is sealed to the gate's
key (M3 review round 3, r3-hosting-6). The node cannot tell it from the buyer's own next task in the context: each
A2A task has a fresh per-job key, and nothing in the header binds a context to its first job. Decision, the
reviewer's second option: §7 states it as a residual next to the unsealed case D227 refuses, and §7's claim that a
forged request "gains nothing it could not get as a buyer" names it. Binding a context to its first job (a MAC of the
`contextId` under the first job's `rk` in every later first message) would change the header and the hiring node's
key keeping for every context; it joins D170's list for a later envelope revision. A service that keeps no
conversation per context is not exposed ([`ext-sealed.md`](../spec/ext-sealed.md) §7).

### D252. A sealed session reaches only the tasks bound to its job, and MCP keeps the task list rule — **decided under the owner's standing instruction, 28 Oct** (amends D227, D233)

The hosting node bound a sealed task-augmented call's task to the job only as it read the server's `CreateTaskResult`.
A gate that ended the call's exchange first (on the stdio bridge, closing the `POST` is enough: the answer is then
dropped) left the task unbound, and on the buyer's own session, whose preamble and `Mcp-Session-Id` it chooses,
`tasks/result` relayed the task's result and `tasks/list` its `statusMessage` in plaintext; D227's residual ("the gate
cannot list it") did not hold, since `tasks/list` was not refused (M3 review round 4, r4-hosting-1). The reviewer
proposed binding an unbound task on first sight to the session's one unanswered task-augmented call. Decision, the
simpler rule that closes the class, refuse rather than track: every task of a sealed session is a sealed call's (an
unsealed call is refused there), so one the node has not bound is one whose `CreateTaskResult` it never read; on a
sealed session (a job, or one being opened, or an upstream session a sealed job holds) a `tasks/get`, `tasks/result` or
`tasks/cancel` naming it is `wrong_task`, `tasks/list` lists the job's own tasks only, and its status notification is
not relayed; on an MCP service with sealed state a `tasks/list` on an exchange of no sealed job is `bad_header`, as
D227 has it for A2A. No honest party loses anything: neither the client nor the gate's dispatch ever learned the
task's id. What remains is D227's residual: an upstream that does not bind tasks to the session could hand the task
to another session by an id the gate knows ([`ext-sealed.md`](../spec/ext-sealed.md) §2, §5; `sealing` mcp.go
`fromClient`, `FromServer`, `ownList`; `TestReview4UnboundTaskAfterCut`, `TestReview4UnboundTaskURL`,
`TestReview4UnboundTaskStdio`).

### D253. A server that mints no session id is one upstream session for the input requests the node seals — **decided under the owner's standing instruction, 28 Oct** (amends D175, D232)

The hosting node indexed a sealed input request by upstream session only when there was one, and looked an answer up
in the preamble's job when there was none: on a URL server that mints no `Mcp-Session-Id`, the gate's own plaintext
answer to the buyer's sealed sampling request, posted under a preamble naming no session or another one, was found to
answer nothing and was forwarded, and the server took it for the buyer's (M3 review round 4, r4-hosting-2). Decision,
the reviewer's proposal for input requests: the sessionless state is one upstream session, `""`, whose request ids are
the server's one id space; a sealed input request is indexed under it as under any session, and an answer is checked
against the index whatever the preamble names, so "answers must be sealed" holds there too. The client's own request
ids are not indexed so: they are not one id space across a sessionless server's clients (two clients both send id
`1`), and such a server answers a request only on its own stream, which carries the exchange's own record
([`ext-sealed.md`](../spec/ext-sealed.md) §5, §7; `sealing` mcp.go `sealInput`, `answer`;
`TestReview4SessionlessPlainAnswer`, `TestReview4SessionlessAnswerURL`).

### D254. What a sealed request records is written down before it is handed — **decided under the owner's standing instruction, 28 Oct** (amends D227, D233)

A sealed request's records (the job's binding, its `replyTo`, the MCP `(session, messageId)`, the upstream session's
owner, the record of the call whose answer the node seals) were held in memory until the service's headers came, or
the exchange ended: a node that stopped meanwhile (a crash, an OOM kill, a reboot) forgot them while the service had
the buyer's plaintext. After the restart the buyer's upstream session was nobody's, so the gate's unsealed call on it
read what the sealed call set up, and on A2A the service had no sealed state, so `ListTasks` listed the buyer's task
with its history and the first message resent ran again (M3 review round 4, r4-hosting-3). Decision, the reviewer's
first option: an exchange's records are data (`commitOp`), written to the service's `pending/<id>.json`, synced, before
the body is handed; acceptance (or End once handed) applies them and removes the file, the node's own answer removes
it unapplied, and the node applies, as accepted, every file left when it starts, since the service may have had the
request. One write covers every record a request makes, so no state of a request in flight is left in memory only; a
removal a crash lost records a request the node answered itself (the gate's retry is then a replay), never forgets one
the service had. The other option, a §7 residual, would have left the restart window open
([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` pending.go, mcp.go `FromClient`, hosting.go `Request`,
`NewHosting`; `TestReview4RestartA2AFirstMessageInFlight`, `TestReview4RestartMCPCallInFlight`,
`TestReview4RestartWhileFirstCallInFlight`).

### D255. A sealed job's upstream session stays held after the job is swept, until a `DELETE` of it is answered — **decided under the owner's standing instruction, 28 Oct** (amends D226)

D226 held the upstream session a sealed job's calls set up for that job only while the job's file lived: Sweep removed
the job 7 days after its last sealed exchange, and its session's index entry with it. The gate's own traffic on the
session kept nothing alive, and the gate could withhold the buyer's `DELETE`; a URL server keeps the session, the
buyer's login or opened document in it, until a `DELETE` or a restart, so a week later the gate's unsealed call on it
was forwarded and answered in plaintext (M3 review round 4, r4-hosting-4). Decision, the reviewer's second proposal:
Sweep moves a swept job's sessions to `held`, which guards them as a live job's do (an unsealed `tools/call` on one is
refused under any preamble, the job's own included), until a `DELETE` of the session is answered `2xx` or `404`, by
the server or the stdio bridge: only the server's end of the session ends what the job's calls set up there, and no
clock of the node's bounds it. Updating the job's last use on every exchange, the first proposal, would not hold
against a gate that sends nothing for a week ([`ext-sealed.md`](../spec/ext-sealed.md) §5; `sealing` hosting.go
`sweepService`, mcp.go `sessionOwner`, `SessionGone`; `node` mcp.go, proxy.go; `TestReview4SessionHeldAfterSweep`,
`TestReview4DeleteEndsTheHold`).

### D256. The gate's own unsealed methods on a sealed session are a residual — **decided under the owner's standing instruction, 28 Oct** (amends D217, D226)

D217 and D226 refuse the gate's own unsealed `tools/call` on a sealed session; every other method passes there under
the buyer's preamble, which the gate chooses: `resources/read`, `prompts/get`, `completion/complete`, and
`logging/setLevel`, which raises the level of a sealed call's unsealed `notifications/message`. A server that exposes
what a sealed call opened as a resource answers the gate in plaintext, and §7 stated no residual for it (M3 review
round 4, r4-hosting-5). Decision, the reviewer's first option: §2 says these methods are unsealed, so the buyer's own
use of them already reaches the gate in plaintext; §7 now states that the gate can send them itself on a sealed
session and read what the server exposes there. Refusing them under `--sealed-required`, the other option, would take
resources and prompts from a sealed service's buyers for a guarantee §2 never made; sealing them would change the
envelope and is left to a later revision (D170) ([`ext-sealed.md`](../spec/ext-sealed.md) §2, §7;
`TestReview4OtherMethodsOnASealedSessionAreAResidual`).

### D257. Services that reach the local host by any of its names are one upstream — **decided under the owner's standing instruction, 28 Oct** (amends D236)

D236 groups services whose targets reach one upstream, a loopback host by any of its names, but read only `localhost`
and loopback addresses so: `0.0.0.0` and `::` (what a server prints as its listen address, which Linux dials as the
local host) and `localhost.` were not grouped with `127.0.0.1`, and an unsealed call on one service read the buyer's
session on the other (M3 review round 4, r4-hosting-6). Decision, the reviewer's proposal: a target's host is read
without its trailing dot, and the unspecified addresses and names under `localhost` (RFC 6761) are the local host too.
Grouping too much only shares a guard; grouping too little opens one, so every name of the local host counts
([`ext-sealed.md`](../spec/ext-sealed.md) §5; `node` serve.go `upstreamOrigin`; `TestReview4UpstreamGroupAliases`).

### D258. The hiring node never relays a redirect — **decided under the owner's standing instruction, 28 Oct**

The hiring node passed the gate's `3xx` answers, `Location` included, to the local client. a2a-go's `http.Client` and
the Python MCP SDK (`follow_redirects=True`) follow a `307` or `308` with their own plaintext body, so a gate that
answered a sealed send or `tools/call` with `307 Location: https://<gate>/collect`, or a local path of a service with no
pin, read it in plaintext (M3 review round 4, r4-hiring-1). Decision, the reviewer's proposal: on every hiring path a
`3xx` is replaced by the node's own `502` (an A2A or MCP error body) with no `Location`, whatever the service's
sealing. The gate has no hiring route that redirects, so the rule costs nothing and needs no list of what a client
does with each status ([`tunnel.md`](../spec/tunnel.md) §9; [`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 1;
`hiring` hiring.go `modify`, `proxyError`; `TestReview4RedirectsAreNotRelayed`, `TestReview4RedirectPythonMCPClient`).

### D259. A served card holds nothing else a client builds an endpoint from — **decided under the owner's standing instruction, 28 Oct** (amends D247)

D247 rewrote only an interface's exact `url` under the exact `supportedInterfaces`. a2a-go's REST transport joins an
interface's `tenant` into the path (`../evil` reaches another service, `x` the service `hello/x`), and decodes a card
with `encoding/json`, which reads a lone `URL` or `SupportedInterfaces` as the member: the client posted to a path with
no pin, or to the gate's own URL, in plaintext (M3 review round 4, r4-hiring-2). Decision, the reviewer's proposal: a
served interface keeps `url` (rewritten), `protocolBinding` and `protocolVersion` only; the 0.3 members a compatibility
parser reads an endpoint from (`url`, `preferredTransport`, `additionalInterfaces`, any case) are left out; a card
with a lone case variant of a member a client reads there is not served. Keeping a list of what may stay, rather than
of what must go, closes the members A2A adds later too. A follow-up of a task whose job another service holds is
refused on the hiring node, since it would carry that job's `replyTo` and `rk` to the other service
([`tunnel.md`](../spec/tunnel.md) §9; [`ext-sealed.md`](../spec/ext-sealed.md) §5 rules 1 and 3; `hiring` cards.go
`RewriteCard`; `sealing` hiring.go `sealSend`; `TestReview4CardInterfaceEscapes`,
`TestReview4HiringFollowUpOfAnotherService`).

### D260. Only an `initialize` answer names a session, a session is its service's, and a response answers a request the node relayed — **decided under the owner's standing instruction, 28 Oct** (amends D240, D245)

The TypeScript SDK (1.32.2) takes the `Mcp-Session-Id` of every `POST` answer for its session, and the hiring node
read a request's sealing from its session id alone: a gate that named another session the node held on any answer
moved the client there, so its next `tools/call` to the sealed service went in plaintext on an unsealed session, or its
answer to a sealed sampling request went unsealed on another sealed session, where no input request of that id was
known (M3 review round 4, r4-hiring-3). Decision, all three of the reviewer's proposals: the node removes
`Mcp-Session-Id` from every answer but an `initialize`'s; a session entry records the service path it was initialized
for, and a request on it to another path, or on an unsealed session to a service whose pin says sealed, is `404` (the
client initializes again); on a sealed session a response of the local client goes only as the answer to a request
the node relayed on that session (an opened input request, sealed; a `ping` or `roots/list`, as it is), once. The last
rule closes the class for any way a client could be moved, since an answer is checked against what its own session
was asked ([`tunnel.md`](../spec/tunnel.md) §9; [`ext-sealed.md`](../spec/ext-sealed.md) §5 rules 4 and 4c; `hiring`
hiring.go `modify`; `sealing` hiring.go `mcpRequest`, `otherService`, `sealAnswer`;
`TestReview4HiringSessionOfAnotherService`, `TestReview4HiringResponseToNoRelayedRequest`,
`TestReview4TSClientStaysOnItsSession`, `TestReview4TSAnswerStaysOnItsSession`).

### D261. A task-augmented input request is opened without its task, and a sealed session relays no other server request — **decided under the owner's standing instruction, 28 Oct** (amends D219)

MCP 2025-11-25 lets a server augment `sampling/createMessage` and `elicitation/create` with a task (server-everything
2.0's `trigger-sampling-request-async`): a client with a task store answers with a `CreateTaskResult`, which the node
sealed as the answer, and the server then fetched the real answer with `tasks/result`, which the node relayed unread and
whose response it sent as it came, the LLM's completion of the sealed prompt in plaintext (M3 review round 4,
r4-hiring-4). Decision, the reviewer's first option and both guards: the node opens a sealed input request without its
`task`, so a stock client answers at once and the answer goes sealed (server-everything accepts a synchronous answer);
a task as the answer to a sealed input request is refused; and on a sealed session the node relays no server request
but `ping`, `roots/list` and the input requests it opens, so a server's `tasks/get` or `tasks/result` to the client
never reaches it and, by D260, no response to one is sent ([`ext-sealed.md`](../spec/ext-sealed.md) §2, §5 rule 4;
`sealing` hiring.go `openInput`, `sealAnswer`, `mcpMessage`; `TestReview4HiringTaskAugmentedInputRequest`,
`TestReview4TSClientTaskAugmentedSampling`).

### D262. A loopback `--listen` binds both loopback families — **decided under the owner's standing instruction, 28 Oct** (amends D229)

The node binds `127.0.0.1:7777` and sends its clients to `localhost:7777` (its cards, the README, ext-sealed §5),
which a stock Linux resolves to `::1` first: another local user could bind `[::1]:7777` while the node ran and receive
every local client's plaintext A2A messages and MCP arguments; D229's same-user check guards only the node's own
socket (M3 review round 4, r4-hiring-5). Decision, the reviewer's first proposal: a loopback `--listen` binds the port
on `127.0.0.1` and `[::1]` (`hiring.Listen`), and a port another process holds on either family stops the start; a
family the machine does not have is skipped, since no one can bind it either. This keeps every client that follows
`localhost` on the node, whatever its resolver prefers, where naming `127.0.0.1` in the cards would have left the
clients a user configured by hand. §9 states that before the node binds another user can, and that `--local-token`
is the remedy on a shared machine ([`tunnel.md`](../spec/tunnel.md) §9; `hiring` peer.go `Listen`; `node` run.go;
`TestReview4HiringBindsBothLoopbackFamilies`).

### D263. The rest of an `initialize` exchange that declared sealing is the sealed session's — **decided under the owner's standing instruction, 28 Oct** (amends D219)

On the `initialize` exchange the hiring node relayed every message after the result unread, a service pinned sealed
included: the gate's own `elicitation/create` there, right after a result that declares sealing, reached the local
client as the sealed service's question, and its answer went unsealed on the now sealed session (M3 review round 4,
r4-hiring-6). Decision, the reviewer's proposal: once the result recorded a sealed session, the rest of the exchange
is read as any exchange of that session (an input request there has no call it can belong to, and is dropped), and a
server request before the result is dropped whatever the sealing, since no session is known yet to check its answer
on ([`ext-sealed.md`](../spec/ext-sealed.md) §5 rule 4c; `sealing` hiring.go `mcpMessage`;
`TestReview4HiringInitializeExchange`, `TestReview4TSInitStreamElicitation`).

### D264. On a sealed job the hiring node hands no text only the gate can have written as the service's — **decided under the owner's standing instruction, 28 Oct** (amends D220, D244)

The hosting node writes `"sealed"` for a sealed task's `statusMessage` and drops an artifact's `name` and `description`
on a sealed job (§2, D220), so any other text there is the gate's; the hiring node handed it to the local client
unchecked, in a `CreateTaskResult`, a `tasks/get` or `tasks/list` answer and a `notifications/tasks/status`, and in an
A2A artifact (M3 review round 4, r4-hiring-7). Decision, the reviewer's proposal: on a sealed session every task's
`statusMessage` (any case variant) is handed as `"sealed"`, every task of a sealed session being a sealed call's (so no
list of the session's tasks is needed), and on a sealed A2A job an artifact's `name` and `description` (any case) are
dropped ([`ext-sealed.md`](../spec/ext-sealed.md) §2, §5 rule 4a; `sealing` hiring.go `sealStatusMessages`,
`openAnswer`, `openA2A`; `TestReview4HiringForgedStatusMessage`, `TestReview4HiringArtifactNameDropped`).

### D265. A `POST`'s stream is not resumable through the gate, and a `GET`'s carries no response — **decided under the owner's standing instruction, 28 Oct** (amends D196, D197)

The gate meters a `tools/call` on its `POST` exchange only, and writes `is_error` true when the stream ends before the
response (D196). The TypeScript SDK resumes any cut `POST` stream that carried an event id with a `GET`
`Last-Event-ID`, and a stock server with an event store (server-everything@1.0.0 sends a priming event on every `POST`
stream) replays the response there, which the gate relayed unmetered: an honest client on a network blip got its
result while the row said error, and a buyer that dropped every call's stream after its first event had every
successful call metered an error. A seller could skew it the other way with a response on the `GET` stream, which both
stock clients take (M3 review round 4, r4-relay-1). Decision, the reviewer's simpler alternative: the gate relays a
`POST`'s event stream without the upstream's event ids, so no stock client resumes it and the client sees the failure
the row records, and it ends a `GET`'s stream before a response, which MCP has a server send there only on such a
resume. The row is then always what the client reads; tracking pending rows across replicas and settling them from a
resumed `GET`, the first proposal, would keep resumability for plain calls at the cost of state in Postgres for every
cut call. Gate-written streams keep their `k.` ids (§11.7) ([`relay.md`](../spec/relay.md) §11.2, §11.4; `mcprelay`
exchange.go `relaySSE`, `relayJSON`, stream.go `dropID`; `TestReview4CallStreamIsNotResumable`,
`TestReview4GETCarriesNoResponse`).

## From M3's second pentest run (29 Oct)

### D266. The pentest ran again after the review rounds, under a project and an image of its own — **decided under the owner's standing instruction, 29 Oct** (amends D186, D190)

The review rounds (D191–D265) changed sealing, the hiring endpoint, the MCP session cap and the envelope after the
pentest of D190, which therefore no longer covered the branch, and some of its controls no longer held for the reason
they probe: the session cap (D231) refused buyer A's fifth `vaultmcp` session, and the hiring endpoint binds both
loopback families (D262). Another checkout (M4's) also runs its own pentest on this host's Docker, under D186's project
`m3pentest` and image `kontra-gate:m3`: a run under the same names would tear down the other's stack, or run the
other's gate. Decision: the harness probes what the rounds added (each named in [`pentest-m3.md`](pentest-m3.md):
D195, D197, D207, D208, D217, D218, D221, D223, D224, D226, D227, D228, D229, D231, D235, D236, D237, D239, D247, D249,
D252, D259, D262, D265), its controls are corrected (a client ends its MCP sessions; the listener check expects both
families), and the run is repeated on the committed tree. `KONTRA_PENTEST_PROJECT` and `KONTRA_PENTEST_IMAGE` name the
run's project (its network `<project>_net`) and the gate's image, D186's names by default, in the harness, the
`Makefile` and `compose.pentest.yaml`; the second run used `m3pentest2` and `kontra-gate:m3-pentest2`. The session
cap's eviction probe backdates its sessions' `last_seen_at` in the gate's database instead of waiting two minutes, the
only write a probe makes to the gate's state. The second run (`33f06a8`): 232 probes held, no finding, and two
observations recorded, D223's accepted residual (a forged `kid`-only seller signature, where a gate names no seller
key set, leaves an unverified `kid:` pin unchanged) and the dual-era client of D177 (`test/pentest`; `pentest-m3.md`).

## The merge (29 Oct)

### D267. M3 merges after four review rounds; the dishonest-gate review of sealing continues as follow-up work — **decided under the owner's standing instruction, 29 Oct**

Four adversarial review rounds of the branch confirmed 30, 19, 23 and 15 findings, every one fixed or recorded as a
residual (D191–D265), and each round's fixes carry regression tests that fail before them. The findings of the last two
rounds are in the dishonest-gate model alone (a gate that splices, replays, cuts or re-routes a sealed exchange to read
it), and each round still found new ones there: the review does not converge to an empty round on this surface in a
bounded time, while M4 and M5 wait on this merge. Decision: M3 merges on a bar it meets: M3's named tests
(`sealed_no_plaintext`, `sealed_replay`, `sealed_unsealed_part`, `mcp_session_cross_node`, `mcp_tool_denied`,
`mcp_task_resume`) and the full suites (`check-go`, `test-integration`, `test-e2e`) green on the final tree, the PRD §12
pentest rerun on it with no finding (D266), and every confirmed finding of the four rounds fixed or recorded. The
review of sealing against a dishonest gate continues after the merge on its own branch, rounds of review, verify and
fix until a round confirms no blocker or major, starting with a review of round 4's fixes (D252–D265); it is tracked in
an issue that lists the open residuals of [`ext-sealed.md`](../spec/ext-sealed.md) §7 (D227, D251, D252, D253, D255,
D256). Sealing stays opt-in, and §7 states what it does not yet withstand.

## From M4 planning (28 Oct)

M4 builds the gate's own `/mcp`, anonymous sessions, `kontra login`, the pinned TCK in CI, the compose team gate on a
VPS and the README demo against a staging gate. The owner's M4 goal (26 Oct): "the gate's own /mcp (search, describe,
hire -> URL elicitation whose URL is the task thread, reply, MCP Tasks); anonymous sessions; pinned TCK in CI with
expected-failures; compose.yaml team gate; README demo. Gate: tck_* green bar expected-failures, anon_limits, demo
from a clean laptop against staging, team gate up on a clean VPS." The owner's final decisions (kontra-cloud#60, 26
Oct) stand: the thread URL is `https://app.kontra.sh/#/t/<task>` (D188), there is no local console (D164),
`gateway.kontra.sh` is the gate and `app.kontra.sh` the console, `kontra.run` is development and staging, the gate
accepts Cloud's JWTs as OIDC principals, and the MCP authorization server lives in Cloud's control plane (M5). The
owner said "Don't ask me anything": the entries below are **decided under the owner's standing instruction, 28 Oct**.
They were built under provisional labels while M3's review added D entries on `main`, and were renumbered D268 to
D298, after M3's last (D267), in every file of the repository when M4 was merged onto M3's final tree (29 Oct).

### D268. What M4 builds, and what it leaves to M5 — **decided under the owner's standing instruction, 28 Oct** (amends D170, D225)

D170 left [`relay.md`](../spec/relay.md) §12, `kontra login`, `--public` and anonymous sessions to M4, and the MCP
OAuth resource server (§12.1) to M5 with the OIDC principals; the remote Admission hook has been refused at load since
M1 (C3, D186). Decision: M4 builds §12 in both MCP eras (D269 to D274), the remote Admission hook of §13.1 (D275),
the resource server of §12.1 (D276), [`tunnel.md`](../spec/tunnel.md) §6 with `--public`, expiry, tombstones and the
§6.2 limits (D277), `kontra login` and the conversion it leads to (D278), the TCK job (D279), the README demo
against a staging gate (D280) and the compose team gate on a VPS (D281). M5 keeps the authorization server, the
accounts and the page on which a person accepts a quote (Cloud's control plane), OIDC principals on the A2A routes
(`auth.oidc`, audience `kontra`) with Cloud's revocation feed, listings and `kontra publish`, anonymous sessions on
`gateway.kontra.sh` (production keeps its team configuration until M5 swaps the deployment), and packaged installs
(Homebrew, `curl | sh`). Why: every gate item of the goal can then be tested in this repository with in-repository
doubles (an Admission server, an authorization server), and the gate code M5 needs from this repository (the
resource server, the Admission client) is open-source code M5 would otherwise write here while its own work is in
kontra-cloud. The open-source gate still contains no money code: Admission decides and never moves money (D45).
Amended by D299 (what M3's review rounds, D225 and D231, left to M4: the two-replica test of D199's notice is built,
the cap of a principal's durable tasks is not).

### D269. The gate's own `/mcp` serves both eras, with the gate as the only server — **decided under the owner's standing instruction, 28 Oct** (carries out D24)

§12 said "same transport and session rules as §11.1–§11.2, with the gate as the server", written before D24 put
2026-07-28 on every MCP endpoint. `/mcp` has no upstream: nothing to pin, hop to or learn an era from. Decision
([`relay.md`](../spec/relay.md) §12, §12.5): §11.1's rules and §11.12's classification (rules 1–5) apply as on
`/mcp/{svc}`; a legacy session is an `mcp_sessions` row with `service` NULL and the principal's tenant (0001 already
allows it), served by any replica; the gate answers `server/discover` itself, advertising the tasks extension; a
modern `hire` that declares the extension is a task answered as §11.12 step 4 (in line within 2 s, else
`CreateTaskResult`), one that does not is a plain call; `-32042` is legacy only (D161). No virtual session is opened
and Admission's `session` operation is not consulted: `per_session` prices a service's sessions, and `/mcp` is not a
service; Admission is asked about each `hire` (operation `task`).

### D270. `/mcp` has no anonymous principal — **decided under the owner's standing instruction, 28 Oct**

[`relay.md`](../spec/relay.md) §2.3 grants the `anonymous` principal only on a service whose `allow:` contains
`anonymous`; `/mcp` is not a service, and its `tasks/list` would be refused to `anonymous` anyway (D20). Decision: a
request to `/mcp` without a credential is `401` (with §12.1's challenge when `auth.mcp_oauth` is configured). A laptop
on an anonymous session still reaches `/mcp` through its node, as the session's principal (`node:<key_id>`,
[`tunnel.md`](../spec/tunnel.md) §9).

### D271. Service refs, `search` and `describe` — **decided under the owner's standing instruction, 28 Oct**

§12.2 names `ref`, `description`, `available`, the `price` filter and a cursor without saying what they are.
Decision ([`relay.md`](../spec/relay.md) §12.2): a ref is the service's path segment (`<name>`, or `<tenant>/<name>` on
a multi-tenant gate, the pattern [`mcp-tools.json`](../spec/schema/mcp-tools.json) already has); `query` matches a
case-insensitive substring of the ref, the name or the description; `kind` and `price` filter by equality, `price` on
the `price_model` annotation (§13.4), `free` matching a service without one; results are ordered by ref, 50 a page,
and the cursor is an opaque keyset position that grants nothing, since every page is filtered for the principal. A
description is the served card's (A2A), else the operator's `search.description` annotation, else the upstream's
cached `serverInfo` (MCP). `describe` of an MCP service whose `tools/list` is not cached answers without `tools`
(`toolsCached: false`) and starts §11.3's background drain, at most one per service per 10 minutes, so no request
costs an upstream call, as §12.2 requires.

### D272. What `hire` accepts, what plain calls wait for, and which tasks `/mcp` shows — **decided under the owner's standing instruction, 28 Oct**

§12.2 has a plain `hire` answer `{taskId, state, approveUrl?}` at once, which leaves a client without MCP Tasks (most
stock clients) no way to read a result; it does not say which tasks `tasks/list` on `/mcp` lists, nor what `hire`
does with a sealed service, whose required sealing the gate cannot apply to MCP tool arguments. Decision
([`relay.md`](../spec/relay.md) §12.2): a plain `hire` and every `reply` wait up to 25 s (below the 60 s request
timeout of the MCP SDKs) for the task to end, ask a question or be quoted, and answer what the task then is, the
ended task rendered as `tasks/result` renders it; a quoted task is answered at once (§12.3); a task still running is
`{taskId, state}` with the A2A URL that follows it. A ref of an MCP service, or of a service whose sealing is
required, is a `CallToolResult` with `isError` naming the route to use (sealing tool arguments is envelope v2's) and
creates no task. Every task `hire` creates is marked (`tasks.hired`, migration 0003): the MCP Tasks of `/mcp` see only
those, of the principal's buyer, and `tasks/list` lists them across services; `tasks/cancel` is §11.6's (D57), the
upstream receiving §8's `CancelTask`. Amended by D289 (the structured result, and no `calls` rows).

### D273. `notifications/elicitation/complete` reaches each open stream of the session once — **decided under the owner's standing instruction, 28 Oct**

§12.3 sends the notification "on that session's `GET` stream, if one is open", but a session's stream may be on any
replica, may be reopened by the client after a dropped connection, and the client's retry window is 10 minutes.
Decision ([`relay.md`](../spec/relay.md) §12.3): each `GET` stream of the session that received the `-32042`, open
when the task leaves `quoted` or opened within the 10 minutes after, gets the notification once, written by the
replica holding the stream, which the task's events wake (§9); never another session's stream. `/mcp`'s `GET`
streams carry no event ids and are not resumed: a client that missed the notification retries `hire`, which §12.3
answers with the task's state. Amended by D288: one stream per session, never each stream.

### D274. A modern reader of a `hire` task that asks a question — **decided under the owner's standing instruction, 28 Oct** (amends D58)

§12.4 delivers the question as a form elicitation through `tasks/result`, which 2026-07-28 removed. The tasks
extension carries server requests in `inputRequests` and their answers in `tasks/update` (§11.12). Decision
([`relay.md`](../spec/relay.md) §12.4): a modern reader whose request declares form elicitation sees a text-only
question as `input_required` with one form `elicitation/create` in `inputRequests`, keyed by the gate id
`k.<taskId>.<seq>`; `tasks/update` with `accept` becomes the §7 follow-up once; anything else is `working` with the
reply-tool text, as for a legacy reader without the capability. The capability is the reader's, read per request.

### D275. The remote Admission hook is built in M4 — **decided under the owner's standing instruction, 28 Oct** (lifts the refusal kept by D186)

§12.3's URL elicitation exists only for a quoted task, and only an Admission that returns `quote` creates one
(§13.2); the open-source built-in always allows. With `hooks.admission` refused at load, no `-32042`, no retry rule
and no `notifications/elicitation/complete` could be exercised before M5. Decision: M4 builds
[`relay.md`](../spec/relay.md) §13.1's remote hook as written (HTTPS unless loopback, `Authorization: Bearer
<secret>`, fail closed with `-32603` `admission unavailable`, the key and size rules of §13.2, a `quote` refused
outside operation `task` on a gate-owned task) for A2A tasks, MCP operations and `hire`, and the e2e tests drive it
with an in-repository Admission server whose quotes carry `https://app.kontra.sh/#/t/<taskId>`. Cloud points it at its
control plane in M5. The approval itself stays the operator's `kontra.approval` Update (§13.2); the tests send it
through Temporal, as a control plane would.

### D276. The MCP OAuth resource server is built in M4, and how a token is verified — **decided under the owner's standing instruction, 28 Oct** (amends D170)

D170 put §12.1's resource server in M5 with the OIDC principals. It is code in this repository, testable against an
in-repository authorization server, and M5's control plane builds the authorization server it trusts
(kontra-cloud#60: authorization code with PKCE, RFC 8414, RFC 9728, DCR). §12.1 says what is checked, not how the key
set is found. Decision ([`relay.md`](../spec/relay.md) §12.1): M4 builds the resource server on `/mcp` and every
`/mcp/{svc}`; the issuer's RFC 8414 metadata (else its OpenID configuration) and its `jwks_uri` are fetched over
`https://` as §10.3's operator-configured endpoints (D284: no address check, since the issuer is the operator's and
never derived from a request and a team's IdP may be on a private network; no redirect followed, no proxy, bounded
bodies), and cached 10 minutes, an unknown `kid` refetching the key set at most once a minute; `iss`, `aud`
(the route's canonical URI), `exp` and `nbf` are checked with 60 s of skew; ES256, RS256 and EdDSA are accepted, never
`none` or HMAC; `scope` is not checked. The principal is `oidc:sub:<sub>` with no tenant and `via = mcp`. OIDC
principals on the A2A routes (`auth.oidc`) and Cloud's revocation feed stay in M5, on the same verifier.

### D277. Anonymous sessions: who opens them, how they end, and how the limits count — **decided under the owner's standing instruction, 28 Oct**

[`tunnel.md`](../spec/tunnel.md) §6 says what an anonymous session is; it does not say that its URLs need a
multi-tenant gate, which hellos open one, what happens to the key at expiry, or how §6.2's limits are counted and
configured. Read literally, expiry revokes the key (`revoked`, §12), so the same laptop could not start a new session
for the next 24 h, and "a 5th concurrent task is `429`" names no counter. Decision ([`tunnel.md`](../spec/tunnel.md)
§6, [`relay.md`](../spec/relay.md) §17): `tunnel.anonymous` needs `tenancy: multi` and takes either `true` or an object
of §6.2's limits (`ttl`, `tenants_per_network`, `services`, `tasks`, `hiring_streams`, `parts_bytes`,
`purge_after`), which tests compress and staging may tune. A hello with neither `enroll` nor `auth` opens one when its
key is unknown at the gate or belongs to an anonymous tenant; the same key before expiry gets its tenant back. The
gate sends `expiring` 10 minutes before the end; at the end it revokes the key's bindings, disables the tenant's
services (`404`; tasks in flight fail `service_unavailable`, D117), sends `revoked` and closes the session, but does
not revoke the key: its next hello opens a new tenant with a new slug. 24 h later the tenant's services, enrollments,
tasks, calls and bucket objects are deleted and the tenant row stays as a tombstone. Tenants per network count the
live anonymous tenants created from it; tasks as host count the non-terminal tasks of the tenant's services, as hirer
those whose principal is one of its keys, under a per-tenant lock at creation; over a cap A2A answers `429` as §2.2
does, MCP `429` with `-32600`, and `hire` an `isError` result. `public` names must be declared services. Services of an
anonymous tenant are never `listed` and never in `search`. Amended by D285 (only a hello carrying `anonymous: true`
opens one), D286 (what a key of an anonymous tenant may present), D287 (the limits per tenant and gate-wide,
the purge, the reserved slug) and D297 (at the end a dispatch in flight is aborted as a revoked binding's is: its
task fails `tunnel_lost` once an upstream task exists, else `service_unavailable` at its next dispatch).

### D278. `kontra login` gets an API key through the gate's authorization server, and a handshake with it converts the anonymous tenant — **decided under the owner's standing instruction, 28 Oct**

[`tunnel.md`](../spec/tunnel.md) §6.1: "`kontra login` turns the anonymous tenant into the person's account (Cloud's
flow); the services keep their names under the account's slug and stop expiring." Nothing says what the CLI speaks,
what it gets, or where the tenant changes; the owner's decisions put the authorization server in Cloud's control plane
(authorization code with PKCE, RFC 8414/9728, DCR) and keep the CLI to `connect`, `login`, `seal` and `publish`. An
OAuth access token for `/mcp` cannot be the tunnel's credential (its audience is `/mcp`), while every gate already
takes API keys in the handshake (§4 `auth`). Decision ([`tunnel.md`](../spec/tunnel.md) §6.3): `kontra login` finds the
authorization server through the gate's RFC 9728 document for `/mcp`, registers once per gate (RFC 7591), runs the
authorization code flow with PKCE S256 and a loopback redirect (RFC 8252) for `resource` = the gate's origin (RFC
8707), and accepts only an access token that is an API key of the gate (`kk_…`), which it keeps as the gate's
credential (§2.4). Cloud's authorization server issues such a key for the person's account, with `hire` and
`enroll:*`. The conversion happens at the gate, in the next handshake whose key belongs to a live anonymous tenant and
whose `auth` is that key: the declared services are created in the account under the same names (`allow:
[org:<account>]`), the key moves to the account with bindings to them, and the anonymous tenant expires at once. The
anonymous URLs then answer `404`: a converted service is a new service at a new URL, and its old tasks fail
`service_unavailable`. A gate without `auth.mcp_oauth` has no sign-in, and `kontra login` says to ask its operator for
an API key. A device flow for machines without a browser is later work.

### D279. The TCK runs anonymous, three ways, from a script; its expected failures name their cause — **decided under the owner's standing instruction, 28 Oct** (amends D18)

D18 has the TCK target opt in to the `anonymous` principal and `push.allow_private`, because the pinned TCK
(`d4d6fae`) sends no credential (a2a-tck#163); D20 refuses `ListTasks` and push configs to `anonymous`, so the TCK's
`CORE-LIST-*` and push tests fail against the gate by design, which D18's seed list of five did not foresee. The
baseline against a2a-go v2.7.0 (M0 research) was 26 failed, 141 passed, 95 skipped and 3 xfailed; the gate already
fixes its card-cache, subscribe and SSE-validation rows (M1). Decision: the TCK runs against a `tck` service with
`allow: [anonymous]`, on the seller `examples/sellers/tck` (a2a-go v2.7.0), three ways: the direct backend and the
tunnel backend bare (`tck_jsonrpc`, `tck_rest`), and the direct backend behind `examples/front/caddy`
(`tck_behind_caddy`), each binding a separate run with its own JUnit report. `test/tck/expected-failures.txt` lists
D18's five TCK bugs and the anonymous refusals, each with its reason and upstream issue; the strict-xfail plugin
(`test/tck/kontra_xfail.py`, from M0) fails a run on an unexpected pass and on an entry that names no collected test
of the binding run. The runs are `make tck VARIANT=…` (`scripts/tck.sh`: the gate, the seller and the node as host
processes, a Temporal development server from the pinned CLI, a fresh database), not `go test`; a blocking `tck`
workflow runs them on pull requests that change the gate and nightly. Re-pin when a2a-tck #200, #232 and #197 merge,
and drop the anonymous entries then by running the TCK with an API key.

### D280. The README demo, how a clean laptop installs the CLI, and the staging gate — **decided under the owner's standing instruction, 28 Oct** (amends note 2 §2 and the README's M4 target)

Note 2 §2 and the README's "Five minutes" show `brew install kontra`, `a2a serve`, `a2a send <url>`, a console on
`:7778` and a short slug `anon-7f3a`; there is no Homebrew tap or release yet, a2a-cli's subcommand is `server` and
its agent flag `-a` (D15), there is no local console (D164), and a slug is `anon-` and 16 hex digits (§6.1). The owner's
gate is "demo from a clean laptop against staging". Decision: the demo installs with `go install
kontra.sh/kontra/cmd/kontra@<version>` (Go 1.26 is its one prerequisite, Node 22 for the MCP lines), a2a-cli v0.3.1 by
`go install` as `examples/sellers/hello-exec` says, and runs `a2a server --exec`, `kontra connect --gate
https://gateway.kontra.run --serve a2a:hello=… --serve 'mcp:fs=stdio:…' --public hello`, `a2a send -a
http://localhost:7777/agents/anon-…/hello`, a stock MCP client on the node's `/mcp/anon-…/fs` and on the gate's own
`/mcp` (`hire`), and a call of the public URL without a credential. The staging gate is `gateway.kontra.run`, a
multi-tenant gate with `tunnel.anonymous` in the cluster's `kontra-staging` namespace; until M5 opens anonymous
sessions on `gateway.kontra.sh`, the demo names `--gate https://gateway.kontra.run`, and the README says so. The
README block between its `demo` markers is what the clean-laptop run executes, so the two cannot drift. Amended by
D298 (`@latest`, the slug placeholder, the run's two substitutions, the pinned MCP client).

### D281. The compose team gate gets an ACME variant, and runs on a throwaway VPS — **decided under the owner's standing instruction, 28 Oct** (completes D168)

D168 made the compose file terminate TLS from a provided certificate, as on a VPS with no proxy, and left the run
"from a clean VPS" to M4. A team bringing a gate up on a fresh VPS has no certificate yet; the gate's built-in ACME
(TLS-ALPN-01, D82) needs only port 443 and a writable cache. Decision: `compose.yaml` mounts a named volume for the
ACME cache and `deploy/compose/kontra.yaml` documents the ACME variant beside the provided certificate; the gate run
brings the file up on a new DigitalOcean droplet under a throwaway `kontra.run` name, connects a node and hires through
it from another machine, and destroys the droplet and the name afterwards (the run's record says how long it lived).
Completed by D298 (the healthcheck's server name, the image's `/var/lib/kontra-gate`, `kontra.vps.yaml`).

### D282. The named tests M4 runs, and the gate runs — **decided under the owner's standing instruction, 28 Oct**

Like D154 and D171. `test/e2e` gains `anon_limits` ([`tunnel.md`](../spec/tunnel.md) §16), `anon_lifecycle`,
`login_converts`, and [`relay.md`](../spec/relay.md) §16's `gate_mcp_search`, `gate_mcp_hire`, `gate_mcp_hire_quote`,
`gate_mcp_reply`, `gate_mcp_modern` and `gate_mcp_oauth`, on two replicas behind the Front wherever the row allows, with
an in-repository Admission server and authorization server, and anonymous limits compressed through
`tunnel.anonymous`. `tck_jsonrpc`, `tck_rest` and `tck_behind_caddy` are `make tck` (D279). The two runs the owner's
gate names are not tests in CI: the clean-laptop demo against the staging gate (`scripts/demo-clean-laptop.sh`, a
container image built from `golang` with nothing else) and the team gate on a clean VPS (`scripts/team-gate-vps.sh`);
both are to be recorded in `docs/gate-runs-m4.md` with what was run, what was seen and when everything was torn down
(written by WP-09 on 31 Oct: [`gate-runs-m4.md`](gate-runs-m4.md); the VPS run failed at `3b890f3` on a defect of
`compose.yaml` and passed at `f9f5e61` with its fix, D313; D306 lists what is left).

### D283. PRD §14's other M4 examples stay skeletons — **decided under the owner's standing instruction, 28 Oct**

PRD §14's M4 row also lists `kontra dev` with embedded Temporal (removed by note 2), the sellers
`examples/sellers/{temporal-harness,adk,claude-cli}` and the fronts `examples/front/{agentgateway,ingress}`. The owner's
M4 goal names none of them, and no gate item needs them. Decision: M4 builds `examples/sellers/tck` and
`examples/front/caddy`, which `tck_*` need (D279); the other three sellers and two fronts keep their skeleton
READMEs until a milestone needs them.

## From the M4 plan's audit (28 Oct)

The audit of the M4 plan (seven blockers, all accepted by the orchestrator) found that §10.3 still sent every operator
URL through the SSRF dialer, that the anonymous rules broke the lost-welcome probe, that `elicitation/complete` was
broadcast across streams, and several holes in the anonymous limits. These entries settle them before the code that
depends on them. Like D268 to D283 they are **decided under the owner's standing instruction, 28 Oct**.

### D284. The operator-configured endpoints, and what the gate accepts from the Admission hook — **decided under the owner's standing instruction, 28 Oct** (amends D67, D192; corrects D276)

[`relay.md`](../spec/relay.md) §10.3 (D67) sent every operator URL through the SSRF dialer, with one exception for
`http://` on loopback for `hooks.*` URLs. Yet a team's Admission hook, its IdP and its object store are usually on a
private network; D276 had the issuer "dialed as configured, like the Admission URL", which §10.3 contradicted, and
M3 dialed the bucket as configured under a `SPEC-CONFLICT` (D189), which D192 settled for the bucket alone (dialed as
configured, no proxy, no redirect). Decision ([`relay.md`](../spec/relay.md) §10.3):
`hooks.admission.url`, `auth.mcp_oauth.issuer` with its RFC 8414 or OpenID metadata and the `jwks_uri` that metadata
names, and the bucket's endpoint are **operator-configured endpoints**. They need `https://`, except `http://` on a
loopback host (connected to a loopback address only; the bucket keeps the scheme its URL is written with), follow no
redirect, ignore the environment's proxy and skip the private-address check. The hook's and the issuer's answers are
bounded (an Admission decision at 64 KiB, the metadata and the key set at 256 KiB each) and each of their requests
has a timeout (5 s; `hooks.admission.timeout` for Admission, at most 30 s: the MCP relay bounds the one `session`
decision a modern client's first requests share at 30 s, so a longer timeout would be cut there and nowhere else).
The bucket is not given these bounds: its requests carry parts as large as the storage limits allow, so it keeps its
S3 client's own dial, TLS-handshake and response-header timeouts and the context of the work that makes each request
(the corrections' "timeouts 5 s" for the whole class, read for the bucket, would cut large-part uploads; M4 WP-01's
review). Every other operator URL (`seller_keys` URLs) stays on the SSRF dialer. `internal/ssrf` builds the class
(`ssrf.NewOperatorClient`), and `hooks.RemoteAdmission` and the resource server (M4 WP-06) use it.

What the gate accepts from the hook ([`relay.md`](../spec/relay.md) §13.1): the answer is read as
[`schema/hooks.json`](../spec/schema/hooks.json)'s `decision` with exact keys (no repeated or case-variant key, no
unknown member, a `deny` with its `refusal`, a `quote` with its terms, a `refusal.code` that is an integer and not
`null`); a quote's `approveUrl` must be an absolute `https://` URL without userinfo, and its `expiresAt` an RFC 3339
`date-time` in the future, written with upper-case `T` and `Z` as every reader of it parses it (Go's `time.RFC3339`;
a comma fraction, a one-digit hour and an offset of `+24:00` or minute 60, which that parser alone also takes, are
refused). Anything else, like a timeout, a non-`2xx` (a `3xx` included) or maps that break §13.2's rules, is
`-32603` `admission unavailable` (`503`): it fails closed. The URL itself is passed through untouched (D188), so a
team gate configures its own approve page.

### D285. A hello asks for an anonymous session with `anonymous: true` — **decided under the owner's standing instruction, 28 Oct** (amends D277)

Under D277 a hello with neither `enroll` nor `auth` from an unknown key opened an anonymous tenant. That is also the
shape of [`tunnel.md`](../spec/tunnel.md) §2.4's lost-welcome probe, on which `internal/enroll` and `internal/node`
rely: on a gate with `tunnel.anonymous`, a probe after an enrollment that did not commit would open an anonymous
tenant, the node would record its unconfirmed tokens as consumed and never send them again, and a network slot and a
tombstone would be spent. Decision ([`tunnel.md`](../spec/tunnel.md) §4, §6): the hello gains an optional field
`anonymous: true`. `kontra connect` sends it only when the origin's credentials hold no API key and no pending or
consumed enrollment token and neither `--enroll` nor `--api-key` was given; a probe never sends it. The gate opens or
resumes an anonymous session only on such a hello; without it an unknown key stays `unknown_key`. A gate without
`tunnel.anonymous` answers it, from a key it does not know, `anonymous_unavailable`.

### D286. What a key of a live anonymous tenant may present, and how a key moves — **decided under the owner's standing instruction, 28 Oct** (amends D277, D278)

[`tunnel.md`](../spec/tunnel.md) §6.3 converts an anonymous key on an `auth` key with `enroll:*`; nothing said what
`enroll` tokens or a hire-only API key from such a key do, and `key_services`' foreign key `(key_id, tenant_id)` has no
`ON UPDATE`, so moving a key with its bindings in place fails. Decision ([`tunnel.md`](../spec/tunnel.md) §6.1):
`enroll` tokens from a key of a live anonymous tenant convert it into the tokens' tenant (bound as §5 says, the
anonymous tenant expired at once), as §6.3 does; a hire-only `auth` is admitted as the anonymous session with that key
as its hiring principal (§9), and nothing is converted. Every move of a key to another tenant (these conversions,
§6.3's, and a new anonymous tenant after expiry) is one transaction under the key's row lock that deletes the key's
old `key_services` rows before it updates `tunnel_keys.tenant_id`.

### D287. Anonymous tenants are bounded per tenant and gate-wide, and the purge leaves only the tombstone — **decided under the owner's standing instruction, 28 Oct** (amends D277)

D277 counted services per hello while undeclared services kept their rows, so reconnecting with new names grew a
tenant without bound; plain MCP calls to a `--public` service were bounded only by the node; nothing capped the live
anonymous tenants of the gate (an IPv6 /32 holds 65,536 /48 networks); the `anon-` slug could be taken by an account;
the purge kept the node keys and the gate's own `/mcp` sessions; [`tunnel.md`](../spec/tunnel.md) §12 still listed
anonymous expiry as a key revocation; and "parts over the total are refused" named no answer. Decision
([`tunnel.md`](../spec/tunnel.md) §6.1, §6.2, §12; [`relay.md`](../spec/relay.md) §13.5, §17):

- services are counted per tenant (those not disabled), and a resumed hello disables what it no longer declares;
- two new limits in `tunnel.anonymous`: `calls` (8, concurrent plain MCP calls to the tenant's services as host,
  over it `429` with `-32600`) and `max_tenants` (1000, live anonymous tenants gate-wide, over it
  `anonymous_unavailable`; the staging gate sets 200);
- a caller's message whose parts would pass `parts_bytes` is `InvalidParams` `anonymous storage limit` before any
  task row, and upstream bytes past it fail the task `upstream_error` (§6.6);
- migration 0003 adds the check `(kind = 'anonymous') = (slug ~ '^anon-[0-9a-f]{16}$')` on `tenants`, and a new slug
  that collides is drawn again;
- the purge deletes the keys still in the tenant (their bindings and tunnels go with them), the MCP sessions whose
  `tenant_id` is the tenant (`service` NULL included), its services with their calls (an exception to §13.5's "the
  gate never deletes calls"), enrollments, tasks and bucket objects, and keeps only the tenants row;
- §12's key revocation no longer names anonymous expiry: expiry revokes the bindings, not the key.

### D288. `notifications/elicitation/complete` goes on one stream of the session — **decided under the owner's standing instruction, 28 Oct** (amends D273)

D273 sent the notification on each open `GET` stream of the session. MCP 2025-11-25 transports, "Multiple
Connections": the server "MUST send each of its JSON-RPC messages on only one of the connected streams; that is, it
MUST NOT broadcast the same message across multiple streams." Decision ([`relay.md`](../spec/relay.md) §12.3):
migration 0003 adds `tasks.elicitation_notified_at`; a replica holding an open `GET` stream of the session claims the
notification with `UPDATE … SET elicitation_notified_at = now() WHERE id = $1 AND elicitation_notified_at IS NULL`
and writes it on that one stream only if its update won; a stream opened within the 10 minutes after the task left
`quoted` gets it only if nothing claimed it. A client that missed it retries `hire` (§12.3).

### D289. The structured result of `hire`, `reply` and `tasks/result`, and `/mcp`'s metering — **decided under the owner's standing instruction, 28 Oct** (amends D272)

§12.2 put data parts "in `structuredContent`" while D272 put `{taskId, state}` there beside the content: both cannot
be one object, and several data parts had no shape. Nor did anything say what `/mcp` meters, while `calls.service` is
`NOT NULL` with a foreign key to `services`. Decision ([`relay.md`](../spec/relay.md) §12.2): a plain `hire`, every
`reply` and `tasks/result` of a `hire` task carry `structuredContent` `{taskId, state, data?}`, `data` being the
values of the rendered data parts in order (message first, then each artifact's), present only when there are some.
`search`, `describe`, `hire`, `reply` and the MCP Tasks of `/mcp` write no `calls` rows: a `hire` is accounted as its
A2A task.

## From M4 WP-02's TCK runs (28 Oct)

The first measured runs of the pinned TCK through the gate (`make tck`, four variants, two bindings each) found one
gate defect and corrected the predicted expected failures. Like the entries above they are **decided under the
owner's standing instruction, 28 Oct**.

### D290. A blocking resend of a task at `input-required` or `quoted` is answered at once — **decided under the owner's standing instruction, 28 Oct** (completes D108)

D108 closes the stream of a resend whose task already ended a turn; nothing said the same of a blocking
`SendMessage`. The work queue answers a resend with its task, a2a-go subscribes at the log's tail, and a turn that
ended at `input-required` before that sends nothing more: the call waited until the task's next turn, its deadline or
the caller's timeout. The pinned TCK hit it in every run (its prerequisite tasks are resends, D291): a dozen tests per
binding ran into its 30 s read timeout. Decision ([`relay.md`](../spec/relay.md) §9): a resend whose task stands at
`input-required`, `quoted` or `auth-required` is answered at once with the task as it stands (`internal/relay`
`resendAnswer`, after Admission and the other checks, with the work queue's own lookup), the states on which D108's
stream of the same resend closes at once. A task reaches `input-required` or `auth-required` only by a turn its
workflow ran, so the start a resend re-issues (§14.5) is moot there; a `quoted` task's start may be the lost one, so
it is re-issued first, as the work queue does for a resend (`lifecycle.Queue.ReissueStart`, which the gate hands the
interceptors as `relay.StartReissuer`): a lost start gets its workflow, which waits for `kontra.approval` until the
quote expires, and nothing is approved by the resend (D38, D45). A resend of a terminal task is still answered by
a2a-go after the re-issue, and one of a `submitted` or `working` task, whose start may be the lost one, still goes
through the work queue and waits for the turn. Tests: `TestBlockingResendAtTurnEnd` (10 s deadline exceeded before,
under a second after) and `TestBlockingResendOfAQuotedTask` (a quoted task, and one whose start was lost: the resend
answered at once, the lost start's workflow re-issued and running once approved; 10 s deadline exceeded before), and
`TestRemoteAdmissionThroughTheGate` through a replica. Amended by M4 WP-02's review (28 Oct), which found the
`quoted` resend still waiting for the quote's end.

### D291. What the TCK measures through the gate, per variant — **decided under the owner's standing instruction, 28 Oct** (amends D279)

D279's expected failures were predicted; the audit showed that the plugin let a listed test skip and an unlisted test
xfail. Measured: (1) `CORE-SEND-003` passes through the gate (a2a-tck#202 asserts success, and the gate relays the
seller's `ContentTypeNotSupportedError` as a failed task, §6.4), and `DM-MSG-001` fails by D5; (2) the TCK's message ids
are per session (`tck_id`; #250 fixed follow-ups only), so its prerequisite tasks after the first are resends, answered
with the first task (§15; A2A §3.3.1 lets an agent detect duplicates by message id): once an early test completed or
canceled that task, the tests that need a fresh one skipped, eight or nine per binding (CORE-CANCEL-001, CORE-MULTI-005,
STREAM-SUB-001/002 and the history tests, MUSTs among them, and with an API key two of the three push deliveries); (3)
the JSON-RPC client's snake_case `task_id` on push configs, a2a-tck#242's defect not filed for push. Decision:
`test/tck/expected-failures.txt` is the measured list of the tests that do not pass, expected failures and expected
skips (`skip <node id>`), in sections per variant, each reason citing an issue or a decision; `kontra_xfail.py` fails a
binding whose outcomes differ from its entries in any way (an expected failure that passes, errors or skips; an unlisted
xfail; an expected skip that does not skip; an unlisted skip of a test of the run's binding, since the TCK skips
whenever a prerequisite request fails, so a regression would otherwise hide as a skip; an entry naming no test) and a
malformed file; a test's binding is its id's suffix, its directory, its function's name suffix (`_jsonrpc`, `_rest`) or
its class's name prefix (`TestJsonRpc`, `TestRest`), so the other bindings' tests, which skip "not configured", need no
entry. The harness gives each test fresh TCK ids (`test/tck/kontra_tck_ids.py`, an autouse fixture setting
`tck.requirements.base._SESSION` per test; no TCK file changed), so the tests of (2) run through the gate; they showed
CORE-HIST-002 failing on JSON-RPC by #242 and CORE-HIST-003 (a SHOULD) by a2a-go v2.7.0, which applies `historyLength`
to GetTask only (D7; the seller alone fails it too), both now entries. A fourth variant, `auth`, runs nightly behind a
loopback front that adds a tck API key, so ListTasks, push CRUD and push delivery run through the TCK. Each run uploads
JUnit and HTML reports per variant and binding, and the processes' logs. The seller is `examples/sellers/tck`, the front
`examples/front/caddy` (Caddy 2.11.7). Measured by WP-02's review on 28 Oct with fresh ids, each variant once per
binding: direct, tunnel and caddy 82 passed, 10 xfailed and 17 expected skips (jsonrpc), 76 passed, 9 xfailed and 16
expected skips (http_json); auth 90 passed, 5 xfailed and 14 expected skips (jsonrpc), 90 passed, 3 xfailed and 8
expected skips (http_json); the rest are the other bindings' tests. Amended by M4 WP-02's review (28 Oct): skips are
entries, and the ids are fresh per test (before, "skips are not entries", and the tests of (2) never ran).

## From M4 WP-03, anonymous sessions (28 Oct)

### D292. How the gate keeps anonymous sessions' limits and ends them — **decided under the owner's standing instruction, 28 Oct** (completes D277, D285 to D287)

[`tunnel.md`](../spec/tunnel.md) §6 says what is limited and refused; it does not say how a gate of several replicas
counts plain MCP calls in flight, what the 429s carry, how the parts total is kept across a tenant's tasks, or how the
replica holding a session learns that its tenant ended. Decision (`internal/anon`, `internal/enroll/anonymous.go`,
`internal/tunnelgate`; [`tunnel.md`](../spec/tunnel.md) §6.1, §6.2, [`relay.md`](../spec/relay.md) §3):

- **Calls, gate-wide.** A plain `tools/call` relayed to an anonymous tenant's service holds a row of `anon_calls`
  (migration 0003: `id`, `tenant_id`, `replica_id` referencing `gate_replicas` with `on delete cascade`, `started_at`)
  from before it is forwarded until its answer has been relayed, inserted under a lock on the tenant's row only while
  the tenant has fewer than `calls`. A replica that dies takes its rows with it when it is reaped (§11); the purge deletes
  a tenant's. A passthrough task-augmented call counts too: it holds the same relay while its answer comes back. A
  per-replica count would have allowed `calls` × replicas.
- **The 429s.** A task over `tasks` (A2A, as host or hirer) is `-32600` `rate limited` on JSON-RPC and HTTP 429 with
  `Retry-After: 5` on both bindings and on the streaming methods, as the edge answers its own rate limits on a JSON-RPC
  route (§2.2); on HTTP+JSON the body is a `google.rpc.Status` `RESOURCE_EXHAUSTED` with reason `INVALID_REQUEST`. An MCP
  task-augmented call over `tasks`, and a plain call over `calls`, are HTTP 429 with `-32600` and the same `Retry-After`.
  A caller's message or call over `parts_bytes` is `InvalidParams` (`-32602`) `anonymous storage limit`, HTTP 200 on
  JSON-RPC and MCP, before any task row.
- **The parts total.** A message to an anonymous tenant's service counts as its encoding against what the tenant's
  tasks store (each event's payload encoding and each bucket object, §6.6, D103, D194); a dispatch of one of its tasks counts
  the other tasks' bytes as they stood when it started, and its limit is `min(task_bytes + others, parts_bytes)`. So
  tasks dispatched together may store up to `tasks` × the room left between them (SPEC-CONFLICT in `anon.Caps.Budget`;
  an exact total would read every other task's bytes under a lock at every event).
- **The end.** The replica holding an anonymous session runs its clock: `expiring` 10 minutes before the end (at once
  when less is left), and at the end `{"t": "revoked"}` and the close; it then asks its expiry sweep to run. Every
  replica's sweep (every 5 s) ends the tenants past `expires_at` in one transaction each: `NOTIFY kontra_tunnel,
  'expired:<tenant_id>'` first (a gate-internal payload like M2's, which the holder of a session of that tenant applies
  at once), then the services disabled and the bindings revoked with their `kontra_services` and `kontra_revoke`
  notices, and an audit row; a session whose tenant ended never gets one `{"t": "revoked", "service"}` per binding. A
  conversion (D286) ends the anonymous tenant the same way.
- **The purge** runs every minute on every replica, on a goroutine of its own (the expiry on another), so a bucket that
  hangs holds neither the expiry nor the gate's other sweeps. A pass takes up to 100 due tenants and claims each with a
  session advisory lock (a tenant another replica is purging is skipped); it deletes the bucket objects of every task
  of the tenant by the task's prefix, whether or not the task has a `blobs` row (D167: an object stored for an event
  that was then refused, such as an upstream part past `parts_bytes`, has none), 8 tasks at once and each within a
  minute, then the rows in one transaction. It stops at the first bucket failure (that tenant keeps its rows) and after
  5 minutes; the next pass goes on (the M4 WP-03 review).
- **Lock order.** A transaction that takes a key and its anonymous tenant takes the key's row first, then the
  tenant's, before anything else: the admission locks the anonymous tenant of the hello's key, live or ended, right
  after the key. The sweeps take a tenant with `SKIP LOCKED`, and the purge takes the tenant's keys with `NOWAIT`,
  leaving to its next pass a tenant one of whose keys an admission holds. So a resume, a key's move and a conversion
  never deadlock with the sweeps (the M4 WP-03 review's 40P01s).
- **A gate that stopped opening them.** On a gate without `tunnel.anonymous` (turned off while anonymous tenants live),
  a hello from a key of a live anonymous tenant that asks for a session is `anonymous_unavailable`; any other is §5's,
  as a new key's: the probe (§2.4) is `unknown_key`, so the node sends its full hello; a hello §5 admits (enroll tokens,
  a hire-only `auth` key with no service) moves the key, and the anonymous tenant ends at once as D286's conversions
  end it. Answering every hello `anonymous_unavailable` kept a probing node probing forever (the M4 WP-03 review).
- **The card.** The served card of an anonymous tenant's service carries the gate-asserted extension
  `https://kontra.sh/ext/anonymous/v1` (`required: false`, `params.expiresAt`), written into the service's
  `annotations.card_extensions` when the session creates or enables it, so a card the gate signs says the service is
  an ephemeral anonymous session's (the M4 corrections' recommendation).
- **The node.** `kontra connect` exits 1 with "the anonymous session expired at <time>" when the gate ends its anonymous
  session, and also when such a session closes within a minute of its end without the message (a redial would open
  another session with new URLs). `expires_at` is the gate's time, and the node's clock may be off by more than that
  minute, so the node measures the end on its monotonic clock: from the welcome by its own wall clock, then from the
  gate's `expiring`, which the gate sends ten minutes before the end by its own clock (at once when less is left); a
  `{"t": "revoked"}` without a service after `expiring` is the end, and a close of an anonymous session is judged only
  once the control stream has been read (the end's `{"t": "revoked"}` comes just before the close, which the node may
  see first). A revocation of the key in the session's last ten minutes therefore reads as its end: the node stops
  either way, and its next hello learns `revoked` (the M4 WP-03 review).

Tests: `internal/anon` (`TestOpenCountsNetworks`, `TestOpenServicesPerTenant`, `TestCapsTask`, `TestCapsCall`,
`TestCapsStorage`, `TestPurgeLeavesTheTombstone`, `TestPurgeDeletesTheObjectsOfEveryTask`,
`TestPurgeNeverWaitsOnAnAdmissionOfItsKey`, `TestPurgePassStopsAtTheFirstBucketFailure`, `TestPurgePassEndsAtItsBudget`,
`TestPurgeClaimsATenant`, `TestNoTableHoldsTheClientAddress`), `internal/enroll` (`TestAdmitAnonymous*`,
`TestAdmitEnrollFromAnAnonymousKeyConverts`, `TestConversionAndExpiryNeverWaitOnEachOther`,
`TestEnrollFromAnEndedAnonymousKeyAndExpiryNeverWaitOnEachOther`), `internal/tunnelgate`
(`TestAnonymousSessionEnds`, `TestAnonymousTenantEndClosesItsSession`, `TestAnonymousHiringStreamsCap`),
`internal/mcprelay` (`TestPlainCallsAreCapped`), `internal/gate` (`TestAnonymousTenantLimitsThroughTheGate`,
`TestAnonymousPurgeDeletesObjectsWithoutRows`, `TestAnonymousSweepsRunApart`), `internal/node` and `cmd/kontra`
(`TestRealAdmissionProbeOnAnAnonymousGate`, `TestAnonymousExpiryWithASkewedClock`, `TestAnonymousEndEstimate`,
`TestConnectExitsWhenTheAnonymousSessionExpires`).

## From M4 WP-04, the gate's own `/mcp` (28 Oct)

Building [`relay.md`](../spec/relay.md) §12's legacy era settled what §12 left open. Like the entries above it is
**decided under the owner's standing instruction, 28 Oct**.

### D293. How `/mcp` hires, cancels and answers — **decided under the owner's standing instruction, 28 Oct** (completes D272, D288, D289)

§12.2 has `tasks/cancel` of a `hire` task follow §11.6 (canceled within 10 s whatever the upstream answers, D57), but a
`hire` task is an A2A task, whose `kontra.cancel` follows §8 (D79's wait, "the task continues"); and several answers of
the tools were not written down. Decision ([`relay.md`](../spec/relay.md) §12.2, §14.1):

- **The cancel's MCP mode.** `kontra.cancel` takes an optional `mode: "mcp"`; `/mcp`'s `tasks/cancel` of a `hire` task
  sends it, and TaskLifecycle then applies §11.6's rule to the A2A task: `abort` (no upstream task yet) or
  `forward_cancel` (which sends the upstream §8's `CancelTask`), waited for at most 8 s, then `canceled` (`client`). The
  change is behind `workflow.GetVersion("hire-mcp-cancel")`, read only for a cancel that asked for the mode, which no
  history recorded before it holds; those histories replay unchanged (D46: `TestReplayHistoriesBeforeTheCancelMode`), and
  the mode's own runs are recorded (`hire_mcp_cancel_*.json`). An A2A `CancelTask` of the same task keeps §8; a cancel
  that joins one in flight shares its result and so its mode (§8: concurrent cancels are coalesced).
- **A `hire` is an A2A request.** The task is created by the service's own a2a-go handler (`SendStreamingMessage` with
  the principal on the context, the first event taken): the interceptors (identity, §2.5's stripping, sealed and part
  checks, an anonymous tenant's storage, Admission with operation `task`, protocol `a2a` and the principal's `via`,
  `CheckQuote`, large parts) and the work queue's write are the A2A route's. The work queue marks it `hired`, keys it by
  §12.3's hash, and under that key's lock answers a retry with the earlier task; the retry is looked up before
  Admission too, so a retry never asks Admission again and never changes the task's state. A retry of a task still
  `quoted` re-issues its start (the service's work queue, as an A2A resend does, §14.5), so a task whose first `hire`
  committed its row, lost its start and answered `503` gets the workflow the person's approval reaches. §2.4's
  required extensions, which the A2A routes check in front of a2a-go, are checked by the hirer too (D294).
- **Tool execution errors.** Arguments that break a tool's input schema, the unknown-service answer (one for every
  ref that is malformed, unknown, disabled, of an ended tenant or not admitted), an MCP or required-sealed ref, a ref
  whose served card requires an extension (D294), a `reply` to a task that is quoted or takes no input, and an
  unknown task in `reply` are a `CallToolResult` with `isError` (MCP 2025-11-25 answers input validation errors so; an
  HTTP `404` on `/mcp` would end the client's session). A `message` part is one a2a-go reads back from what it writes:
  a `data` of JSON `null` is no content to it (`{"data":null}` is written and not read back), so a task holding one
  could never be loaded (`hire` answered `503`, the task stayed `submitted` past its deadline, counted against an
  anonymous host's tasks cap, and every later `tasks/list` of the buyer failed); `mcp-tools.json`'s `data` excludes
  `null`, and the gate checks each built part's round trip besides. Admission's refusal is the JSON-RPC error it names; `admission unavailable` and a lifecycle that does not
  accept in time are `503` with `-32603`; an anonymous tenant's caps `429` with `-32600` (D292).
- **Rendering.** A raw file part is an embedded `resource` (`uri` `kontra:task/<taskId>/<filename>`, else
  `part-<n>`, its media type, the bytes as `blob`); a URL file part is a `resource_link`. A plain answer's
  `structuredContent.state` is the A2A state (a `quoted` task's is `TASK_STATE_INPUT_REQUIRED`, its approve URL in the
  text and `_meta`). A plain wait whose request accepts `text/event-stream` is answered as one event of a stream kept
  alive every 15 s. A `rejected` task (MCP `failed`) carries `sh.kontra/failure: "rejected"` in its result's and its
  `tasks/get`'s `_meta`: §12.2 gives every failed, rejected or canceled `hire` task one of the two keys, and §11.8's
  reasons are the gate's own failures, none of which an agent's rejection is.
- **Sessions.** `initialize` negotiates `2025-11-25` or `2025-06-18` (the client's when it asks one of them, else
  `2025-11-25`) and asks no Admission; a `2025-06-18` session has no tasks (`tasks/*` `-32601`, `hire` without
  `execution`). The negotiated version decides that, never a request's header: a request whose `MCP-Protocol-Version`
  names the other version is `400` with `-32022` (`data.supported` the session's), as MCP's transport answers a version
  the session may not use. Any replica closes a `/mcp` session idle for an hour (§11.2's default `session_idle_s`).
- **Who may use `/mcp`.** A principal that may not call services (`auth.Principal.MayHire`: an API key with only
  `enroll:` scopes, which §2.3 gives node enrollment only) is `403` (`-32600`, the A2A identity interceptor's words)
  on every request, before parsing and before any session, in both eras. Keys of one account may share a `buyer_id`,
  and `/mcp`'s task surfaces are keyed by the buyer: without this, an enroll-only key listed, read and cancelled the
  hire key's tasks. `HireTasks` answers such a principal as having no tasks too, whatever calls it.
- **§12.4's answer.** A client response to `k.<taskId>.<seq>` is the follow-up only when it is `accept` with
  `content.reply` a string, from a session of the task's buyer that declared form elicitation, and `seq` is the event
  that asked the question outstanding now; anything else (a quoted task's gate id included) is `202` and dropped.
- **`elicitation/complete`.** An open `GET` stream reads its session's elicitations at open, every 2 s and on each
  wake of a quoted one; it claims and writes each that left `quoted` (D288).
- **Descriptions.** An MCP service's description falls back to its upstream's `serverInfo` title or name as the
  replica's last background drain (started by `describe`) read it.
- **Search's bound.** A page is filtered for the principal after the read, so one request reads at most 1000 services
  (five queries of 200); one that reaches the bound answers what it found, fewer than 50 services or none, with
  `nextCursor` at the last service read, which grants nothing. Without it a principal who sees few of many services
  made each request read the whole table, sorted by the computed ref (no index applies to `slug || '/' || name`).

### D294. A `hire` of a service whose card requires an extension is refused — **decided under the owner's standing instruction, 28 Oct** (completes D293; §2.4 on `/mcp`)

§2.4 refuses an A2A request missing a URI the served card marks `required: true` (`-32008`, before any task row), and
§13.4 enforces an operator's `card_extensions` entry "like any other". The check is HTTP middleware in front of
a2a-go, so a `hire`, which runs through the service's a2a-go handler (D293), skipped it: a task was created and
dispatched to a service that requires an extension no tool call can activate. Decision ([`relay.md`](../spec/relay.md)
§12.2): `hire` and `reply` read the service's required URIs as the A2A routes read them (the replica's served card,
`a2ahttp.Server.Declared`), and a service that has any is refused before anything else (no Admission, no task row,
nothing sent), as a `CallToolResult` with `isError` naming the URIs and the service's A2A URL, where a client
activates them in `A2A-Extensions`; the required-sealed answer is the precedent. A form answer to a question
(§12.4) goes through `reply`'s path and is dropped the same way.

Consequence, for the owner: Cloud's `per_task` services carry the escrow entry with `required: true`
([`ext-escrow.md`](../spec/ext-escrow.md) §1), so they cannot be hired through `/mcp`: §12.3's quote flow on `/mcp` then
serves a quoting Admission on services without a required extension (a team gate's, or Cloud's if its escrow entry is
not required). Proposal, not built: `hire` and `reply` take an optional `extensions: [uri]` argument that activates
URIs as `A2A-Extensions` does (ignored unless declared, passed to Admission and the upstream as the A2A route passes
them; activating escrow acknowledges the quote, D38, and a person still approves it); §12.4's form answers would then
need the task's activation kept for its follow-ups. Amended by D300: `hire` activates the operator's required entries
(the escrow entry among them) for the buyer, and refuses only the seller's own and the sealed extension's.

## From M4 WP-05, `/mcp` in the 2026-07-28 era (28 Oct)

### D295. How `/mcp` answers a 2026-07-28 client — **decided under the owner's standing instruction, 28 Oct** (completes D269, D274; one SPEC-CONFLICT with §11.12 step 4)

D269 and D274 have `/mcp` serve the modern era, but left five things unsaid that building it had to settle
([`relay.md`](../spec/relay.md) §12.5, §16 `gate_mcp_modern`):

- **What the gate's own modern results carry.** 2026-07-28 gives every result a `resultType` and every list and
  `server/discover` result `ttlMs` and `cacheScope` (CacheableResult). The tool results, the empty acknowledgements of
  `tasks/update` and `tasks/cancel`, and `tasks/get`'s DetailedTask carry `resultType: "complete"`, a
  `CreateTaskResult` `"task"`; `server/discover` and the modern `tools/list` carry `ttlMs` 600000 (§11.12's bound on a
  relayed list; the four tools change only with the gate's version) and `cacheScope: "private"`, and `server/discover`
  names the gate in `_meta["io.modelcontextprotocol/serverInfo"]` (a SHOULD) with `initialize`'s instructions.
- **Methods `/mcp` does not serve.** `ping` (removed by 2026-07-28), `tasks/result` and `tasks/list` (removed by the
  extension) and every method other than `server/discover`, `tools/list`, `tools/call` and the three `tasks/*` are `404`
  with `-32601` (2026-07-28 Streamable HTTP: a method the server does not implement). `subscriptions/listen` asking for
  `taskIds` without declaring the extension is `-32021` (the extension's MUST, D176); any other is `404` with
  `-32601`: `/mcp` has no notification to offer (its tools never change while it runs; `notifications/tasks` is not
  offered, D161), and `server/discover` declares no `listChanged`.
- **A failed `hire` task to a modern reader.** §12.2 makes every terminal `hire` task a `CallToolResult`, never a
  JSON-RPC error; the tasks extension forbids `failed` for a tool result with `isError` ("MUST NOT be used for
  non-JSON-RPC errors") and requires `error` with it. So a task that ended `failed` or `rejected` is `completed` to a
  modern reader, its `result` the `isError` `CallToolResult` with `sh.kontra/failure` in both `_meta`s, as §11.12's
  table already says for a stored `CallToolResult` whatever its `isError`; `canceled` is `cancelled`, without `result`.
  A legacy reader keeps §11.8's `failed`.
- **`tasks/update`.** Only the gate id of the question outstanding now, as the request's own `tasks/get` would show it
  (D274: input-required, a text-only question, form elicitation declared in that request), is read; its `accept` with
  a string `content.reply` is the §7 follow-up through the Hirer, the path `reply` and a legacy client's response take,
  so the workflow's one-follow-up-per-turn-end rule (§7, D105) makes a second answer a no-op. Everything else (a quoted
  task's keys, an earlier turn's, another task's, a malformed key, a decline, a cancel, an answer to a working or
  ended task) is the empty acknowledgement and forwards nothing (the corrections' rule: a quoted task is never
  approved by a client). So is an answer to the outstanding key that §7 refuses as no longer outstanding (another
  follow-up of the turn accepted first: a2a's UnsupportedOperation). Any other refusal is an error, never the
  acknowledgement, since the extension has clients track keys and never answer one twice, so an acknowledged answer
  that was not taken would be lost while the task waits: a lifecycle that did not accept it in time and the gate's own
  failure `503` with `-32603`, an anonymous limit `429`, another A2A refusal (the anonymous storage limit, D294's
  required extension) its JSON-RPC error, as `hire` and `reply` answer them. The key's seq is read in the statement that
  reads the task (`store.Log.LoadHiredAsked`), so `tasks/get` never pairs one question's text with a later question's
  key (the extension: a key MUST NOT name two distinct requests); `tasks/result`'s form elicitation reads them the same
  way.
- **A client that leaves.** 2026-07-28 says a server MUST treat a closed response stream as the cancellation of that
  request. A `hire` that created a task and whose client leaves before the answer is written (the 2 s wait of a
  task-augmented call, D178, or the 25 s plain wait) sends that task `kontra.cancel` in MCP mode, not awaited; the
  task's creation runs on a context the client cannot cancel (bounded to a minute), so a task created while the client
  left is learnt and cancelled rather than left running unseen. A task a retry found again (§12.3) is the earlier
  request's and is never cancelled; neither is a `reply`'s task, whose id its client knows. A plain wait is cut so that
  the answer is written within 50 s of the request's arrival (at least 1 s): Admission (30 s at most) and the start
  (`AcceptWait`) come before it, and without the cut a slow Admission would push the answer past the MCP SDKs' 60 s
  request timeout, turning a task that runs normally into a cancelled one.
- **The cancels nobody waits for** (a modern `tasks/cancel`, a client that left) wait only for the workflow's
  acceptance (`lifecycle.Client.RequestCancelMCP`), and a replica sends one per task at a time: none while one is being
  sent, none for 10 s (D57's bound) after one was accepted, so a client repeating `tasks/cancel` at the edge's rate
  cannot pile Updates onto the task's history, whose per-workflow Update limits would then refuse the task's own
  `kontra.message` and `kontra.approval`. The drain waits for them, and for the modern hires still being answered, at
  most `AcceptWait` + 1 s after the listener stopped and before the Temporal client closes. Measured on the pinned
  development server: an Update admitted before the client closes is still accepted later by another worker, so the
  wait's guarantee is that each cancel is accepted and in the history when the replica exits (and none is cut before
  Temporal admitted it); a gate of one replica has no worker left to accept it, waits out the bound, and the admitted
  Update waits for the next worker.

SPEC-CONFLICT, resolved by this entry: §11.12 step 4, which §12.5 applies to `hire`, cancels the task of any client
that leaves before "either answer", a quoted task's `CreateTaskResult` included. On `/mcp` that breaks §12.3's retry:
2026-07-28 makes a client re-issue a request whose stream broke, and a re-issued `hire` within 10 minutes of its quoted
task's end answers that task, so the client would be told "the task was canceled" for 10 minutes, and a person could
never approve the quote. A quoted task runs nothing until a person approves it, and ends by itself when its quote
expires. Decision: a quoted task is not cancelled when its client leaves; the re-issued `hire` answers it with its
approve URL (`TestGateMCPModernClientLeaves`). §11.12 step 4 stands for `/mcp/{svc}`, whose MCP tasks have no retry.

## From M4 WP-06, the MCP resource server and `kontra login` (28 Oct)

### D296. How the MCP routes answer OAuth clients, how `kontra login` defends itself, and what a conversion binds — **decided under the owner's standing instruction, 28 Oct** (completes D276, D278, D286; one SPEC-CONFLICT between §12.1 and §2.3)

Building [`relay.md`](../spec/relay.md) §12.1 and [`tunnel.md`](../spec/tunnel.md) §6.3 left choices the text did not
make. Decisions:

- **A request without a credential on `/mcp/{svc}` (SPEC-CONFLICT, §12.1 with §2.3 and tunnel.md §6.1).** §12.1 has
  every request without a credential answer `401` once `auth.mcp_oauth` is set; §2.3 gives such a request the
  `anonymous` principal on a service whose `allow:` contains `anonymous`, and §6.1's `--public` MCP services are
  "callable by anyone". Taken: a service that admits `anonymous` still serves it; every other request without a
  credential, to an existing service or not, gets the `401` challenge instead of §2.1's `404`, so an OAuth client can
  sign in and nothing says which services exist (D50). `/mcp` itself has no anonymous principal (D270).
- **The challenge.** Without a credential, exactly `Bearer resource_metadata="…", scope="mcp"`; a credential that was
  presented and refused adds `error="invalid_token"` (RFC 6750 §3.1); a name that is not well formed has no document
  and gets `Bearer scope="mcp"` (nothing a client sends is quoted into the header).
- **JWT or API key.** On the MCP routes, with a resource server, a bearer of a JWT's shape (three base64url segments,
  at most 16 KiB) is verified as the authorization server's token; any other is an API key, as on every route. A
  key the gate mints never has that shape, and an operator's own key (D37) with two dots between base64url segments is
  not one anyone writes. The A2A routes never ask the resource server: a JWT there stays `401` until M5.
- **An issuer the gate cannot read** is its own failure: `503` with `-32603`, never `401` (a client would discard a
  valid token). It is asked again at most every 5 s, and a refetch for an unknown `kid` that fails keeps the fresh set.
  The metadata is looked for at RFC 8414's well-known URI (inserted before an issuer's path, §3.1), then as §12.1
  writes it, then the OpenID configuration appended and inserted; a document naming another issuer is not used.
  Headers with `crit`, RSA keys under 2048 bits and keys that are not signing keys are refused or skipped.
- **`kontra login` against mix-up and leaks** (RFC 8414 §3.3, RFC 9207, RFC 9700; the M4 audit). The gate's RFC 9728
  document must be for `{origin}/mcp` (§3.3), compared as an origin (lower-case scheme and host, the default port
  elided, as `tunnel.md` §4 writes origins: the gate writes its `resource` from `public_url` as configured) and the
  path `/mcp`, with no userinfo, query or fragment; the metadata's `issuer` must be the `authorization_servers` entry,
  the endpoints `https://` (`http://` only on a loopback host, as `--gate` allows), PKCE `S256` offered; an answer's
  `state` must match and an `iss` must be the issuer (required when the metadata says it is sent); no request follows
  a redirect; the code and the verifier are never printed, even when the server echoes them. The environment's proxy
  is honoured, as `kontra connect` honours it. The DCR `client_id` is kept per origin with the issuer it was
  registered at, and forgotten when the token endpoint answers `invalid_client`, or when a login that reused it gets
  no answer before its bound or an interrupt: an authorization server that no longer knows a client must not redirect
  to it (RFC 6749 §4.1.2.1), so a reset client table would otherwise lock every person who logged in there out until
  they edited `~/.kontra/credentials` (the M4 WP-06 review). The CLI says it forgot it; a login merely left unfinished
  costs one registration more. No `--register` flag: the next login registers by itself. The loopback listener is
  closed as soon as an answer arrives, before the token request (§6.3 step 3).
- **`auth.mcp_oauth` needs a `public_url` without a path** (the M4 WP-06 review). The RFC 9728 documents are served at
  `{public_url}/.well-known/oauth-protected-resource/mcp…`, which is RFC 9728 §3.1's location (the well-known path
  inserted after the host) only when `public_url` has no path, and `kontra login` reads them at the gate's origin. The
  configuration is refused at load; a `public_url` in upper case or with its default port is accepted (the
  documents' `resource` keeps it as written, and `kontra login` compares origins).
- **The `kk_` rule is this revision's.** The access token must be an API key of the gate (`kk_` then letters, digits
  and `_`, at most 4096 bytes), checked in one function of `internal/login`; M5 may revise it with Cloud's
  authorization server (kontra-cloud#60 accepts Cloud's JWTs as principals).
- **What a conversion binds.** §6.3's conversion is §5's enrollment by the API key's `enroll:*` scope, with two
  differences: a declared service the tenant does not have is created there (`allow: [org:<tenant>]`) when the
  anonymous session serves it (an enabled service of the anonymous tenant of that name and kind, bound to the key:
  §6.1's "the services keep their names"), and the key moves (D286) with the anonymous tenant ended at once. Any
  other declared name the tenant lacks is §5's `service_denied` "no such service" (the M4 WP-06 review): an anonymous
  session costs no credential, so creating every declared name would let any `enroll:*` key create services in its
  tenant, which §5 leaves to tenant tokens and the operator (M2's C10), and past §6.2's `services` limit. So an
  existing service of the same kind is bound, not created again; one that is disabled or not a tunnel service is
  `service_denied`; another kind is `kind_mismatch`; nothing moves on a refusal; a service's other nodes are told of
  the new key (§5); a hello declaring no service converts the key hire-only when the API key may hire. A team tenant's
  `enroll:*` key converts into the team as an account's does, and a gate that stopped opening anonymous sessions still
  converts (§6.3 does not depend on `tunnel.anonymous`). Every conversion, by an API key or by enrollment tokens
  (D286), ends the anonymous tenant with reason `converted` and writes a `tunnel.anonymous_converted` audit row
  naming both tenants, the authority and the services bound.

## From M4 WP-07, the M4 end-to-end tests (28 Oct)

### D297. The named tests M4 runs, how the harness runs them, and the spec conflicts met — **decided under the owner's standing instruction, 28 Oct** (like D189 for M3; completes D282; one SPEC-CONFLICT in tunnel.md §6.1)

Like D189 for M3. **The tests** (`test/e2e`, `make test-e2e`; [`relay.md`](../spec/relay.md) §16,
[`tunnel.md`](../spec/tunnel.md) §16): `gate_mcp_search` (`TestGateMCPSearch`), `gate_mcp_hire` (`TestGateMCPHire`),
`gate_mcp_hire_quote` (`TestGateMCPHireQuote`), `gate_mcp_reply` (`TestGateMCPReply`), `gate_mcp_modern`
(`TestGateMCPModern`), `gate_mcp_oauth` (`TestGateMCPOAuth`), `anon_limits` (`TestAnonLimits`), `anon_lifecycle`
(`TestAnonLifecycle`) and `login_converts` (`TestLoginConverts`), each with one assertion per clause of its §16 row and
of the M4 corrections' WP-07 list (the quote-acceptance invariant item by item, each asserting the task still quoted and
the seller without a request; the `-32042` URL byte-equal to the hook's `approveUrl` and naming the pre-minted id; a
team gate's own approve URL; exactly one of a session's two `GET` streams notified and another session's none; a
malformed, past or unreachable Admission answer `-32603` with no task; `tasks/update` on a quoted task ignored; no
`calls` row and the `{taskId, state, data?}` shape; the per-tenant services cap across a reconnect, the `calls` cap,
`max_tenants`, both parts-over-total outcomes; the probe, a conversion by enrollment tokens, the purge of a key still in
its tenant, the widened address scan with its positive control, and the `anon-` slug refused for an account). Each runs
on two replicas behind the Front with the client on one replica and the tunnel on the other wherever the row allows;
D185's loopback check and `scripts/test_e2e.sh`'s leftover check hold for every one.

**Tenancy.** Single tenant (a `services:` block): the five `gate_mcp_*` tests other than search. Multi-tenant
(`tenancy: multi`): `gate_mcp_search`, `anon_limits`, `anon_lifecycle`, `login_converts`; their tenants, services and API
keys are rows the harness writes as Cloud would, each service followed by `NOTIFY kontra_services` (D116, D37), and
refs are `<tenant>/<name>`.

**The harness's M4 additions.** `tunnel.anonymous` compressed in the test's own configuration only (never `deploy/`):
`ttl` 60 s (`anon_limits`), 40 s (`anon_lifecycle`), 10 min (`login_converts`), `purge_after` 10 s, the caps 3/4/4/8/8,
`parts_bytes` 1 MiB, `max_tenants` 12 (`anon_limits`); the Front as a PROXY v2 forwarder (`edge.client_ip`
`proxy_protocol`, trusted `127.0.0.1/32`) naming the source the test sets, so nodes come from chosen /24s and /48s (its
header is checked against the gate's own edge parser, `TestFrontProxyHeaderIsTheClientAddress`); an Admission double on
loopback quoting `https://app.kontra.sh/#/t/<taskId>` or a team gate's own page, approvals sent as `kontra.approval`
through the harness's Temporal client; `internal/oidc/oidctest` over HTTPS as the issuer, trusted through
`SSL_CERT_FILE` by the replica processes and `kontra login`; peer listeners where an MCP request crosses replicas
(§11.2); a third replica without `auth.mcp_oauth` on the same database for `login_converts`' last clause.

**Where the tests differ from the plan's steps** (the rows hold; the means differ):

- **The exec sellers.** hello-exec through a2a-cli v0.3.1 is installed neither on this host nor in CI's e2e job, so an
  a2a-go seller in the test process, keyed on the message text, stands in for it (a slow exec working until cancelled,
  a failing exec, a question) and serves the TCK seller's artifact kinds. `examples/sellers/tck` and
  `examples/sellers/booking` run unmodified, as processes built from the checkout (`e2e.yml`'s paths name both).
- **The TCK seller's artifact kinds.** It keys every scenario on the message id, and `hire` mints that id (§12.3), so
  through `/mcp` it answers every message "Unhandled messageId prefix" completed: the fast hire uses it; the artifact
  kinds come from the in-test seller.
- **"The booking seller logs one task id."** booking logs nothing at its level; the test reads it from the gate:
  the task's attempts carry one `upstream_task_id`.
- **`max_tenants`.** The gate is filled with live anonymous tenant rows rather than nodes, and tested at its boundary:
  the network checks' tenants are ended first (their 60 s `ttl` would otherwise move the count while the gate fills),
  11 rows go in, a node then takes the 12th place, and the next node is `anonymous_unavailable`.
- **The probe.** An enrollment that did not commit is reproduced by the node's own state (its credentials hold the token
  unconfirmed, as after a connection lost after the proof): splitting the WebSocket frames at the Front to lose a proof
  would test the Front. The probe gets `unknown_key` on the anonymous gate, opens no tenant, and the token is sent again.

**The spec conflict met (SPEC-CONFLICT, marked in `test/e2e/anon_lifecycle_test.go`).** tunnel.md §6.1 and its §16
`anon_lifecycle` row said that at an anonymous session's end "tasks in flight fail `service_unavailable` at their next
dispatch". The end revokes the session's bindings and closes it, and §12 says a revoked binding's in-flight dispatches
"fail `tunnel_lost`, or are retried if no upstream task exists" (relay.md §6.4 fails a task `tunnel_lost` once an
upstream task exists). The gate does what §12 and §6.4 say: a working task fails `tunnel_lost`, and one whose dispatch
had no upstream task id yet is retried and fails `service_unavailable` at that next dispatch (its service disabled,
relay.md §13.5). Decision: the gate is right; §6.1 and the row now say so. The test asserts both.

**Measured** (28-29 Oct, this host, 2 CPUs at a load of 5 to 9 beside the M3 review): `make test-e2e ARGS='-run
"TestAnon|TestLoginConverts|TestGateMCP" -v'` passes the nine in 329 s (6 min 15 s with the builds), the longest
`TestAnonLifecycle` (77 s, the end and the purge), `TestAnonLimits` (66 s, a session's end) and `TestGateMCPOAuth`
(66 s, the key set's minute); the leftover check found nothing. Mutations checked red: the `elicitation/complete` claim
without its `is null` guard (`TestGateMCPHireQuote`: both of S1's streams notified), and a 4th tenant per network let in
(`TestAnonLimits`).

**Review fixes (29 Oct).** Seven assertions passed without testing their clause; each now has an input only the rule
under test decides. `gate_mcp_hire`: the buyer also sends an ordinary A2A `SendMessage` with the same API key, and that
task is neither in `tasks/list` nor answered by `tasks/get` (`-32602`). `gate_mcp_search`: the anonymous tenant's service
is `listed` and admits `org:default`, so only the anonymous-tenant rule hides it from search and describe.
`gate_mcp_hire_quote`: each A2A message to the quoted task, a plain one included, must be §7's
`UnsupportedOperationError` (`-32004`). `gate_mcp_modern`: the input-required task gets keys that are not the outstanding
one, and they are ignored. `anon_limits`: `max_tenants` is tested at its boundary (above). `anon_lifecycle`: the address
scan also runs while both sessions are live, with a plain call held in flight, and an operator's service and tenant
enrollment tokens make the purge's enrollments clause real. The harness's `hostOn` fails when it cannot read the node's
replica, where it skipped the check before. Each assertion was checked red against a mutation of the gate, in one run of a
mutated copy: `ListHired`, `LoadHired` and `LoadHiredAsked` without `hired`; search and describe without the
anonymous-tenant rule; the quoted refusal as `-32602`; `tasks/update` taking any `k.<task>.*` key; `all > max_tenants`;
the client's address appended to `tunnels.cli_version`; the purge without its `enrollment_tokens` delete. `hostOn` was
checked with a node redialling to replica A after `StartNode`: the old check passed, and the new one fails.

## From M4 WP-08, the gate-run kits (29 Oct)

M4's two gate runs that are not tests in CI, the README demo from a clean laptop against staging and the team gate on a
clean VPS (D280 to D282), are kits in this repository that M4 WP-09 runs after the merge. Building them settled what
the plan and its audit left open.

### D298. What the README demo runs, how the clean laptop runs it, and what the compose ACME variant needs — **decided under the owner's standing instruction, 29 Oct** (amends D280; completes D281, D282)

**The ACME variant's two defects (the plan's audit, gaps 17).** Built-in ACME (autocert) refuses a TLS handshake without
a server name, and compose's healthcheck dialled `https://127.0.0.1:8443/healthz` with none; and a new named volume
mounted on a path the image does not have is root's, while the gate runs as 65532. Decision: `kontra-gate health` sends
`--server-name`, else the host of `public_url` in the configuration `-f` names, else the URL's host, and still skips
verification on a loopback dial; compose's healthcheck passes `-f /etc/kontra/kontra.yaml`, which works for both
configurations (a provided certificate is served whatever the name). The image creates `/var/lib/kontra-gate`, empty
and owned by 65532:65532 (`COPY --chown` from the build stage, no `--chmod`, so the classic builder builds it too), and
Docker initialises a new named volume from it. `compose.yaml` mounts the volume `acme` there in both gate services;
`deploy/compose/kontra.vps.yaml` is `kontra.yaml` with `tls.acme` (cache `/var/lib/kontra-gate/acme`), `public_url`
`https://gate.example.com` to edit, and one tunnel service, `hello`; a test keeps the two in step. The container keeps
listening on 8443 (no privilege) and `KONTRA_PORT=443` publishes it where TLS-ALPN-01 is validated (D82).

**The README's block** (amends D280's `@<version>`, as the audit asks): `go install kontra.sh/kontra/cmd/kontra@latest`
(proxy.golang.org already resolves `kontra.sh/kontra` through the vanity import), a2a-cli v0.3.1 by `go install` renamed
to `a2a`, `export PATH="$PATH:$(go env GOPATH)/bin"` (a fresh Go install does not put it on PATH), a directory for the
filesystem server, `a2a server --exec`, `kontra connect --gate https://gateway.kontra.run … --public hello`, then the
clients naming the slug as the placeholder `anon-<…>` (the README says what it stands for): `a2a send -a
…/.well-known/agent-card.json`, `@modelcontextprotocol/inspector@2.13.0 --cli` on the node's `/mcp/anon-<…>/fs`
(`tools/list`) and on `/mcp` (`tools/call hire`), and `curl` of the public card. The inspector is pinned to 2.13.0, not
the 1.x line the plan had in mind: 1.x prints that it is deprecated and receives security fixes only, and 2.x's CLI keeps
`--cli <url> --transport http --method … --tool-name … --tool-arg k=v` and negotiates the 2025-11-25 era by default, which
the node's stdio bridge needs (D161). `test/demo` checks the block: the markers, the commands, the pins, the placeholder,
and no Homebrew, `:7778` console, `a2a serve`, short slug or production gate.

**The clean-laptop run** (`scripts/demo-clean-laptop.sh`; the plan's gate run B2 and the audit's gaps 12 and 13). The
laptop is `test/demo/Dockerfile`: `golang:1.26.10-bookworm` pinned by its index digest (the one this host pulled on 21 Oct;
the tag has moved since, and both stay pullable), Node 22.23.4 from nodejs.org's tarball with its sha256, a user `laptop`
with an empty home, GOPATH the default and off PATH; target `laptop-src` adds `git archive HEAD` at `/src` for the run
before the merge, `laptop` has none. `test/demo/laptop.sh` runs the block's commands one at a time in one shell, with
exactly two substitutions, both printed: (a) before the merge, the kontra install runs as `(cd /src && go install
./cmd/kontra)`; after it (`--published --expect <merge commit>`), as written, again a minute apart for up to 30 minutes
until `kontra version` names the commit; (b) `anon-<…>` is the slug kontra connect printed. A client command is tried
three times, 15 s apart, because the first `npx` of each package downloads it and the node starts server-filesystem
through `npx` on the first session; every attempt is in the transcript. The laptop then prints the slug and holds the
node connected while the host checks the public URL with no credential (the card, and a JSON-RPC `SendMessage` "hi"
answering Hello World; `a2a send` too when a2a is on the host's PATH, which the plan assumed and this host lacks), then
stops it: it copies a file named `stop` into the laptop, which stops its node and exits 0 (`docker stop` is the
fallback). A file and not only a signal, because on this host runc cannot signal a container's init ("unable to signal
init: permission denied"), so `docker stop` kills it after its timeout and a TERM trap never runs. The script prints its
teardown first and runs it in a trap. Options `--network`, `--ca-file` and `--resolve` exist for a rehearsal against a
local gate only.

**`kontra version` names the commit of a `go install`ed binary.** A module-mode build has no VCS stamp, so it printed
`dev`; the post-merge check could not read the commit. `internal/version` now reports the main module's version when
there is no stamp (a pseudo-version ends with the commit's 12-digit prefix).

**The VPS run** (`scripts/team-gate-vps.sh`; the plan's gate run C and the corrections' WP-09 pre-flight). The script
does the corrections' pre-flight itself (the DigitalOcean key's MD5 fingerprint against this host's key, the size, no
droplet tagged `kontra-m4-gaterun`, no record for the name), waits until 1.1.1.1 resolves the name (DNS over HTTPS)
before compose asks Let's Encrypt, publishes on 443, checks the ports with bash's `/dev/tcp` (no nc), and hires through
the gate with `curl` (A2A JSON-RPC and an MCP `search` on `/mcp`) from this host while a `kontra-demo` container is the
team member's node. Droplets are only ever listed by the run's tag, `--teardown-only` reaps by tag and by the record's
comment, and a droplet the operator names in `KONTRA_VPS_PROTECTED` is refused even if tagged (D306). The droplet's user data adds `docker-buildx` to
the plan's `docker.io` and `docker-compose-v2`: Ubuntu packages buildx separately, and without it Docker builds with its
deprecated legacy builder.

**Staging.** The manifests are private (kept outside this repository), named `gate-v4` and `gate-v4-*` with the
labels the corrections give, the signing key mounted at `/etc/kontra-signing` as in production; `config.staging.json`
names the same hosts and path.

**Tested here, nothing run against staging, DigitalOcean or Cloudflare** (O2). `make test-kits` (`KONTRA_KIT_TESTS=1`)
runs compose's ACME variant on this host (the gate writes its account key into a fresh volume as 65532; once the cache
holds a certificate for the name, compose's own healthcheck turns the gate healthy, and health without a server name
fails), checks the laptop image, and rehearses the whole demo run against a local multi-tenant gate with anonymous
sessions that answers for `gateway.kontra.run` on a docker network (`--network`, `--ca-file`, `--resolve`). Without
Docker, `test/demo` and `test/vps` check the README block, the scripts (bash -n, shellcheck, the prerequisites each
refuses without, the teardown each prints, `--dry-run` and `--teardown-only` against stubs of docker, doctl, ssh and
Cloudflare's API) and the user data's ASCII.

**Measured** (29 Oct, this host, 2 CPUs at a load of 3 to 19 beside other sessions): `make test-kits` passes, exit 0:
`TestACMEVariantWritesItsCache` 113 s (the gate wrote `acme/` 0700 and `acme_account+key` as 65532 in a fresh volume;
the attempt to reach Let's Encrypt was refused at 127.0.0.1; healthy once the cache held the name's certificate),
`TestDemoImage` 3 s, `TestDemoRehearsal` 265 s (the laptop ready in 162 to 443 s, every command passing at its first
attempt, the public card and `SendMessage` from the host, the stop file, one anonymous tenant created from a /24).
Checked red: the old healthcheck without `-f` (the gate never healthy, 4 minutes); a fresh volume on `kontra-gate:m3`
(root's, 65532 cannot write); README blocks with a short slug, `-a` without the card path and a `:7778` console; the
first rehearsal failing at its `docker stop` (exit 137, the runc refusal above).

**Review fixes** (WP-08's review, 29 Oct). The VPS script fails closed. The droplet and record listings fail loudly
when doctl or Cloudflare does not answer (a 429, an expired token). Every check treats that as "could not verify",
never as "nothing left", and the pre-flight refuses to start on it. DNS goes before the droplet, in the trap and in
`--teardown-only`: first the run's record by id, then every record with the run's comment, including one whose POST
answer was lost. A droplet is deleted only once that succeeded, so the name never points at an address DigitalOcean may
give to someone else, who could then serve it and get a certificate for it. The price is a droplet that keeps billing
until `--teardown-only` succeeds; the run says so ("KEPT"). The reap deletes only records with the run's comment. A
record named the name with another comment is reported and left, and the reap fails. `KONTRA_VPS_NAME` must be
`m4-teamgate<…>` in the zone (the review deleted staging's records with a mistyped one). No secret is on a command line:
curl reads the Cloudflare token on its stdin (`-H @-`) and the gate's key from a 0600 file in the run's 0700 directory,
and `a2a send` reads `A2ACLI_AUTH`. The payloads both kits `docker cp` for the image's non-root user get explicit modes
(directory 0755, files 0644). The build context keeps the archive's modes (`tar -p`), and the remote configure step sets
`umask 022`. So a hardened umask (027, 077) on this host or on the VPS breaks neither kit. Steps 4 and 5 also run on a
host one already has (`--host ADDR`: nothing created or deleted on DigitalOcean or Cloudflare, the gate stays up, a
configured host refused), and `make test-kits` rehearses them as written (`TestTeamGateRehearsal`). The stand-in VPS is
this host: an ssh that runs the remote commands here with `/opt/kontra` as the stack's directory; an `edge` forwarder
that answers for the name's port 443 on the project's network, as the published port does on the VPS; the name's
certificate in the ACME cache from the run's CA; and the rehearsal flags `--network`, `--ca-file` and `--connect-to`.
Only steps 1 to 3 (covered by stubs), the Let's Encrypt issuance and issuer, and the port probe remain the real VPS's.
Measured (29 Oct, this host, beside other sessions' runs): `make test-kits` exit 0. `TestTeamGateRehearsal` took 274 s:
compose up 18 s with the configured files, then the team member's node connected about 3 minutes later (it builds kontra
from `/src` and a2a-cli), then SendMessage answered Hello World and `search` listed `hello`, all under umask 077.
`TestDemoRehearsal` took 346 s, also under umask 077. Checked red against the scripts as they were: the fail-open reap
and pre-flight (exit 0, "nothing of a run is left"), the droplet deleted before its record, the staging records deleted
with `KONTRA_VPS_NAME=gateway.kontra.run`, the token in curl's argv, and the payload copied 0700/0600 under umask 077.

## The merge onto M3's final tree (29 Oct)

### D299. M4 merges onto M3's final tree, and what M3's review left to M4 — **decided under the owner's standing instruction, 29 Oct** (amends D268; carries out D225)

M4's branch was cut from M3's at `25faac5`, before M3's review rounds 2 to 4 (D191–D267); M3 then merged to `main` as
`98a9f25`, the tree of `m3-sealing-mcp` at `6a2dba2`. Merging that tree into M4 met eleven conflicting files, the
interfaces the rounds changed under M4's code, and two items M3's decisions leave to M4 that M4's plan predates: D225's
gate-level test of D199's notice across two replicas, and D231's cap of a principal's durable tasks per service.
Decision:

- **Both sides are kept.** M3's revisions of the specs stand ([`relay.md`](../spec/relay.md) revision 7,
  [`tunnel.md`](../spec/tunnel.md) revision 5) and M4's follow them (8 and 6). tunnel.md §5's `unknown_key` rule
  names both of its exceptions, a node that only hires (D206) and a node that asks for an anonymous session (D285);
  relay.md §10.3's operator-configured endpoints (D284) take in the bucket that D192 dials as configured. The MCP
  relay's dependencies carry M3's `ToolsChanged` beside M4's anonymous `Calls` and `Tenants`; the CLI keeps M3's
  `kontra seal` usage and help beside `kontra login` and `--public`; the README keeps M3's sealing, MCP and
  second-pentest text beside M4's demo and team-gate sections. M4's provisional labels are D268 to D298.
- **What the rounds changed under M4's code holds.** An anonymous tenant's `calls` slot (D287) is taken after
  Admission and before a call is metered, in both eras, where D196 and D265 leave metering. The sessions of `/mcp`
  itself (service `NULL`, no tunnel) are neither counted nor evicted by `max_sessions_per_principal` (D213, D231,
  D249), and have their own idle sweep. The streams `/mcp` writes keep D265's rule: a plain `hire`'s answer and a
  `GET` stream carry no event id, a `tasks/result` stream its `k.` ids (§11.7). A hiring node's `/mcp` is no service
  path, so its sealing of MCP sessions (D219, D260, D263) leaves it alone, and D260's removal of `Mcp-Session-Id` from
  every answer but an `initialize`'s takes nothing from a client of `/mcp`, whose answers name the session they
  answer on.
- **D225's test is built.** `TestToolsListChangedAcrossReplicas` (`internal/gate`) has replica B drain a direct
  service's `tools/list`, replica A relay the upstream's `notifications/tools/list_changed` on the `POST` stream of a
  call, and B relay a call of the tool the upstream added within 5 seconds, where B's drained list would answer it
  `Unknown tool` for up to 10 minutes ([`relay.md`](../spec/relay.md) §11.3).
- **D231's cap of a principal's durable tasks per service is not built in M4**, and is follow-up work beside D267's.
  The owner's M4 goal does not name it, and it needs a service setting with its schema, spec text in relay.md §11.2 and
  §11.5 and a refusal for a task-augmented call over it, which a merge does not decide alone. Until it is built, a
  principal's durable tasks on a service are bounded by `limits.dispatch_slots` gate-wide and, on an anonymous
  tenant's service, by `tunnel.anonymous.tasks` (D287).

## From M5's plan audit (31 Oct)

### D300. `hire` activates the operator's required extensions for the buyer, and refuses only those it cannot carry — **decided under the owner's standing instruction, 31 Oct** (amends D294; carries out [`ext-escrow.md`](../spec/ext-escrow.md) §1 on `/mcp`)

M5's plan audit (28 Oct) found that D294 refuses, before Admission, every `/mcp` `hire` of a service whose served card
marks an extension `required: true`. ext-escrow §1 makes Cloud's escrow entry `required: true`, so every Cloud
`per_task` listing would be refused through `/mcp`, and M5's gate that a stock MCP client's `hire` answers the task's
thread URL could not pass; M5's plan had worked around it by writing the entry `required: false`, against §1. Yet
`hire` is the escrow flow: the quote is shown through §12.3's URL elicitation, its URL is the thread on the operator's
page, and a person accepts it there, never an API key or an MCP session (D38). Decision ([`relay.md`](../spec/relay.md)
§12.2, §12.3 step 1, §13.4; ext-escrow §1):

- **The operator's required entries are activated for the buyer.** A required URI of the service's `card_extensions`
  annotation (§13.4) is the operator's: the operator asserts it on the card and its own hooks carry it (Admission's
  quote, its approve page), which every `hire` task goes through as an A2A task does. `hire` activates it on the
  buyer's behalf, and Admission receives it in `extensions` (§13.1) as it receives an A2A caller's activation; the
  task is then created, quoted or refused as Admission decides, which stays the place where price and policy are
  decided. The activation is not a service parameter: the work item does not keep it and the service never receives
  it, even when its own card declares the URI (an A2A caller's own activation of such a URI does reach it, §6.2),
  since the buyer activated nothing it could speak. A `reply` to such a task needs nothing more: a follow-up has no
  Admission.
- **What `hire` cannot carry is still refused.** A required URI of the seller's own card, one the operator's entries
  do not assert, is the seller's protocol, which a tool call cannot speak: refused as D294 says, before Admission,
  naming that URI and the service's A2A URL. So is the sealed extension's URI even as an operator's entry, beside
  D272's refusal of a service whose sealing is required: `hire` cannot seal. The gate still names no escrow URI: it
  activates whatever the operator's entries require (ext-escrow's "the open-source gate names no escrow key" holds),
  and M5 can write ext-escrow §1's entry as §1 writes it, `required: true`.
- **Why not D294's proposal.** An `extensions` argument needs a client that sends it; a stock MCP client sends only a
  tool's documented arguments. It remains a proposal for the seller's extensions.

Tested: `TestHireActivate` and `TestHireActivatesTheOperatorsExtensions` (`internal/relay`: Admission told of the
operator's URI, the seller receiving the hire and the reply without `A2A-Extensions` although its card declares the
URI; the seller's own URI and the sealed one refused, with no Admission, task row or message);
`TestGateMCPHireOperatorExtension` (`internal/gate`: a `per_task` service whose card carries ext-escrow §1's entry,
required, behind a quoting Admission server: the A2A route refuses a `SendMessage` that does not activate it, `hire`
answers `-32042` whose url is `https://app.kontra.sh/#/t/<task>`, the hook is asked once with the escrow URI in
`extensions`, the approved task reaches the seller without `A2A-Extensions`, and a seller-required and a
required-sealed service are still `isError` with no task and no Admission); `TestGateMCPReviewFixes` and the e2e
`TestGateMCPHire` (§16 `gate_mcp_hire`).

### D301. An `allow:` entry `oidc:*` admits every principal of the configured OIDC issuer — **decided under the owner's standing instruction, 31 Oct** (amends D20)

M5's audit: an MCP OAuth principal (`oidc:sub:<sub>`, no tenant, `via = mcp`, D276) is admitted only by an exact
`oidc:sub:` entry or by `anonymous`; `org:` and `key:` never match it. So a public Cloud listing could not admit every
signed-in user without admitting anonymous callers too (M5's plan wrote `[key:*, anonymous]`). Decision
([`relay.md`](../spec/relay.md) §2.3, §12.1, §12.2; `spec/schema/config.json`):

- **Grammar.** `oidc:*` is an entry of its own beside `oidc:sub:<subject>`. It admits every principal of kind `oidc`,
  whatever its subject. Such a principal exists only for a JWT the configured issuer signed (`auth.mcp_oauth` on the
  MCP routes, D276; `auth.oidc` on the A2A routes once M5 builds them), with `via` `mcp` or `oidc`; with both
  configured and naming two issuers, it admits the principals of both, as an `oidc:sub:` entry matches a subject of
  either. It never admits an API key, a node or the anonymous principal: a listing open to API keys too writes
  `[key:*, oidc:*]`. `oidc:sub:*` is refused at load: a subject is matched exactly, so it would admit only the subject
  `*`. An `oidc:*` entry on a gate without `auth.mcp_oauth` admits nobody, and the load says so in a warning.
- **Visibility.** `oidc:*` is a named entry, as `key:*` is: `search` shows a service it is on to every OIDC principal
  (§12.2); an `anonymous` entry still shows nothing.
- **Admission decides the rest.** The entry says who may reach the service; price and policy stay Admission's
  (§13.1), which receives the principal and its `via`.

Tested: `TestParseAllowEntry` and `TestAllowOIDCAnyWarnsWithoutAnIssuer` (`internal/config`), `TestAllowList`
(`internal/auth`), `TestAdmitsNamed` and `TestCatalogOIDCAny` (`internal/gatemcp`: search and hire as a signed-in user,
an API key and the anonymous principal), `TestGateMCPOAuth` (`internal/gate`: a token of the in-repository
authorization server finds and hires an `oidc:*` service on `/mcp` and is served on an `oidc:*` `/mcp/{svc}`; an API
key is not admitted by the entry).

### D302. §13.3's metric of the oldest pending hook activity is not built in M4 — **decided under the owner's standing instruction, 31 Oct**

[`relay.md`](../spec/relay.md) §13.3 says the gate exports the age of the oldest pending Lifecycle hook activity, so an
operator can alert on a hook that never completes (its activities are retried forever, a typo in a type name
included). M5's audit found that the metric does not exist. It is not cheap. The hook activities are scheduled by each
task's `TaskLifecycle` on the operator's queue, so whether one is pending, and since when, is in Temporal, per
workflow. The queue's backlog age (Temporal's `DescribeTaskQueue`) counts only activities no worker has picked up: an
activity a worker polls and fails, which a type name no worker registers is, waits out its retry backoff outside the
backlog, so the typo case would not show. Describing every open workflow for its pending activities costs one request
per running task on every scrape. Measuring it needs the workflow to record when each hook call began and ended, a
change to `TaskLifecycle`'s commands that goes behind `workflow.GetVersion` (§14.4), either a Temporal search
attribute the operator registers or a gate table, and spec text for the metric's name and labels. Decision: not built
in M4. It is follow-up work, beside D299's, for a public-repo issue, and §13.3 says the metric is not built yet. Until
it is, an operator alerts on the hook queue's backlog age and on its own hook worker's activity failures (M5's plan
alerts on `kontra-settlement`'s backlog age), and the gate's `kontra_gate_operator_patches_refused_total` counts the
patches it drops.

## From M4's review (31 Oct)

The review of the merged M4 branch (`69bd5f0`) confirmed twenty findings, two of them twice. D303 to D306 record how
each was settled.

### D303. `/mcp` serves what the replica's registry serves, and its waits, sessions, streams and parts hold as the other routes' do — **decided under the owner's standing instruction, 31 Oct** (amends D271, D293, D299; completes D272, D288)

The review found five places where the gate's own `/mcp` departed from the routes beside it. Decision:

- **What `/mcp` serves is the registry's.** The A2A route resolves every request, `GetTask`, `CancelTask` and a
  follow-up included, through the replica's in-memory registry: an enabled entry of kind `a2a` whose `allow:` admits
  the principal, else `404` "unknown service". `/mcp` read the services table instead, and its task surfaces were keyed
  by `buyer_id` only, so a key sharing the buyer of the key that hired (an operator-written key, or `MintRequest.BuyerID`)
  but refused by `allow:` could read, cancel and answer the task, and so could the hiring key after its `allow:` entry
  was removed; a reply to a task whose service was disabled was sent and failed the task `service_unavailable`.
  `search`, `describe` and `hire` now see exactly what the registry serves, with its `allow:` (`gatemcp.Served`, wired
  to `gate.Services.LookupByID`); a hire task is the reader's only while its service is so served and admits the
  reader (`reply` "unknown task", `tasks/*` `-32602`, left out of `tasks/list`, nothing sent or cancelled); and the
  hirer checks it again before a hire or a follow-up is sent (`relay.Hirer.Serves`, `relay.ErrNotServed`). A row the
  registry refused to build (an annotation it cannot read) or has not loaded yet is an unknown service to `/mcp`, as to
  the A2A route, instead of a hire that ran Admission without the operator's required extensions activated (D300) or
  the seller's checked (D294). The answer is the unknown service, not `503`: the registry cannot tell a row it has not
  loaded from one it refused, and the A2A route answers both the same. relay.md §12.2.
- **A row that cannot be read is left out, never a failed request.** A services row whose `allow:` no longer parses (a
  stored `oidc:sub:*`, a literal subject before D301 and refused since) made every `search` whose scan reached it `503`,
  for every tenant's buyers on a multi-tenant gate. `search` now logs it (once per row per 10 minutes) and goes on past
  it, and `describe` and `hire` answer it as an unknown service, as the registry leaves it out. No data step rewrites
  such rows: on a team gate config refuses them at load and the sync rewrites the row; on a multi-tenant gate the rows
  are the operator's, and the log names each.
- **A plain wait reads a state and the event it comes from together.** A reply's wait read the row and then, in a
  second statement, the seq of the task's last status event; a turn's terminal event committed between the two made the
  wait end on the row's earlier question, so `reply` answered "Which date?" for a task that had completed (6 of 150 in
  the reviewer's run) and the client's next reply was refused. The wait now reads both in one statement
  (`store.Log.LoadHiredAsked`), and ends at `input-required` only on a question asked after the reply's own message.
- **A session in use is not idle.** §11.2 keeps a session whose stream is open from the idle sweep; `/mcp` wrote
  `last_seen_at` only when a request started, so a stock client whose standalone `GET` stream stayed open for an hour
  had its session closed under it, and its next request was `404` (the TypeScript SDK does not re-initialize on `404`).
  The replica serving a `/mcp` session's `GET` stream or a blocking `tasks/result` now keeps its `last_seen_at` fresh
  (`mcprelay.Sessions.KeepSeen`, every `IdleWrite/2`). relay.md §12.
- **`tasks/result` streams carry no event ids** (amends D299's bullet that kept their `k.` ids). D299 kept them under
  §11.7, but `/mcp` never resumes a stream: its `GET` is always the session's notification stream, so a client whose
  `tasks/result` stream was cut and that reconnected with `Last-Event-ID`, as the TypeScript SDK does once it has seen
  an id, waited on a stream that never carries the answer. Without ids the client asks `tasks/result` again, which is
  what §11.7's resumed stream would have made it do too (D203). The `elicitation/create` keeps its JSON-RPC id
  `k.<taskId>.<seq>`, which a response names. relay.md §12.3.
- **A bucketed part reaches an `/mcp` client as a presigned GET, through a node too.** A request on a hiring stream
  keeps `kontra-blob:` refs, which the hiring node inlines for `/agents/…` answers only, so a `hire` result fetched
  through a node carried a `resource_link` to `kontra-blob:<ref>` that no client can fetch. `/mcp` now renders a hire
  task's parts for a caller whatever stream the request came on (`blob.ForCaller`): the node relays an MCP answer as
  it is, its local client is a caller, and the node dials nothing a part names (D56). The node does not inline them
  instead: a `resource_link` with a fetchable URL is what an MCP client expects for large content, where an embedded
  resource of up to 64 MiB would land whole in a model's context, and `hire` cannot be sealed (D272), so there is
  nothing for the node to open. relay.md §10.4, and the hiring node's comment.

Tests: `TestReplyAnswersTheTurnItStarted` (the terminal event committed between the wait's read and its decision),
`TestHireTaskSurfacesFollowTheRegistry`, `TestCatalogServesWhatTheRegistryServes`, `TestGateSessionInUseOutlivesTheIdleSweep`,
`TestGateTasksResultStreamHasNoEventIDs`, `TestGateMCPRendersBlobsThroughANode` (`internal/gatemcp`), and through a whole
replica in `TestGateMCPReviewFixes` (a key of the same buyer refused by `allow:`, the `allow:` entry removed, the
service disabled, a row the registry refused, a row whose `allow:` does not parse, and forty `hire` and `reply` turns
each answering its own).

### D304. What an anonymous tenant's `calls` limit counts, an anonymous key is not rotated, and the sweeps run without the setting — **decided under the owner's standing instruction, 31 Oct** (amends D287, D292; completes D286)

- **`calls` counts every relayed request and stream.** §6.2 gave `calls` the purpose of keeping a `--public` MCP
  service from being an unbounded relay, but its mechanism counted plain `tools/call` only: `resources/read`,
  `prompts/get`, `completion/complete`, any made-up method, an `initialize` and every `GET` or `subscriptions/listen`
  stream went through with no slot. Every request with an `id` and a method that the relay forwards to an anonymous
  tenant's service, and every `GET` or listen stream for as long as it is open, now takes one of the tenant's `calls`
  slots (`mcprelay.Handler.takeRelay`, the same gate-wide `anon_calls` rows), with `429` `-32600` and `Retry-After`
  over it; a notification, a client's response and what the gate answers itself (`tools/list` from its cache, durable
  `tasks/*`, a gate-written stream) take none. Streams share the limit with calls rather than having one of their own:
  a second limit would need a column of `anon_calls`, a key and its spec, for the same bound, and a stock client's
  standalone `GET` refused `429` is not fatal to it (the TypeScript SDK reports it and goes on). The anonymous
  principal still has no `max_sessions_per_principal` (relay.md §11.2): that is deliberate. tunnel.md §6.2.
- **A key of an anonymous tenant is not rotated.** `kontra connect --rotate` sends `rotate_from` and never
  `anonymous`, and the gate admitted the new key into the anonymous tenant through §5's rules, without the anonymous
  decision: no `welcome.anonymous`, no end clock on the session, the 32-stream hiring cap instead of `hiring_streams`,
  and no tenant counted. A chain of rotations held any number of sessions of one tenant at once until its end (the
  sweeper's end still closed them all). A `rotate_from` naming a key of an anonymous tenant, live or ended, is now
  `enroll_invalid`, and nothing is written; the node then sends its hello without `rotate_from`, which with no
  credential asks for an anonymous session of its own, counted by §6.2. Rotating inside the tenant was the
  alternative; an anonymous key has nothing a rotation protects that a new session does not give. tunnel.md §6.1, §12.
- **The sweeps run on every multi-tenant gate.** They were built only with `tunnel.anonymous`, so a gate that turned
  the setting off while anonymous tenants lived, typically against abuse, never ended them in the database nor purged
  their tasks, objects, sessions and keys. The expiry and the purge now run on every multi-tenant gate, with the
  block's values or §6.2's defaults; with no anonymous tenant they find nothing. tunnel.md §6.
- **What anonymous tenants can keep stored is documented, not changed.** The creation limits count live tenants and an
  ended tenant keeps its parts until its purge, so about (1 + `purge_after` / `ttl`) generations coexist: 13 with the
  defaults, about 2.4 GiB per /24 and 0.8 TiB gate-wide. Counting unpurged tenants toward `tenants_per_network` would
  give a network three anonymous sessions a day instead of three at a time, which the demo and the purpose of the
  setting do not survive; deleting parts at the end would take away the day an operator has to look at an abusive
  tenant before its purge. tunnel.md §6.2 states the bound and what lowers it (`purge_after`, `parts_bytes`, the
  creation limits); a gate-wide cap on anonymous stored bytes is follow-up work if Cloud's figures need one.

Tests: `TestEveryRelayedRequestIsCapped` (`internal/mcprelay`), `TestAnonymousKeyIsNotRotated` (`internal/enroll`),
`TestAnonymousSweepsRunWithoutAnonymousSessions` (`internal/gate`). Amended by D307 (a notification or response is
answered `202`, never relayed as a stream), D308 (streams have a limit of their own), D309 (the limits apply with the
setting off, and `enabled: false` keeps them), D310 (the stored-parts bound carries `tasks`) and D312 (an `initialize`
without an `id` is refused `400`, so every forwarded `initialize` takes a slot).

### D305. An authorization server issues a gate's API key only for that gate's origin — **decided under the owner's standing instruction, 31 Oct** (amends D278, D296)

`kontra login` takes the authorization server from the gate's own RFC 9728 document and stores the `kk_` key it gets
under that gate's origin, and `kontra connect` sends it there. Nothing bound the key to the gate: an API key has no
audience, so a hostile gate whose document named Cloud's authorization server would receive the person's Cloud key
once they signed in through it and connected. RFC 8707 makes the binding the authorization server's to keep.
Decision: tunnel.md §6.3 step 4 now says the authorization server MUST issue a gate API key only when `resource` is the
origin of the gate whose keys it issues, answer any other `invalid_target` (RFC 8707 §2), and name that origin on its
consent page; this is a requirement of M5's authorization server (kontra-cloud#60), to be carried into that contract
when it is written (this branch does not reach that repository). The in-repository doubles keep it
(`oidctest.ForResource`; `DBMinter` and the e2e authorization server mint only for their gate's origin), and
`kontra login` prints the authorization server's origin beside the gate's. It does not warn when the two are not the
same site: Cloud's own sign-in is on another host than its gate, and the CLI cannot know an operator's sites. Tests:
`TestLoginThroughAnotherGatesDocumentStoresNothing` (`internal/login`), and `login_converts` (another gate's document
naming the gate's authorization server: refused `invalid_target`, nothing stored).

### D306. The compose gate is M4's, published on both address families, its secrets ignored, the kits neutral, and the gate runs not done yet — **decided under the owner's standing instruction, 31 Oct** (amends D282, D298)

- **The compose image is `kontra-gate:m4`, built with `VERSION m4`.** It was still `kontra-gate:m3`, so every M4 compose
  gate reported `m3` (`kontra-gate version`, `gate_replicas.version`, `/mcp`'s `serverInfo`, the hook clients'
  User-Agent, the VPS run's record), and an M3 host that upgraded with `docker compose up -d` and no `--build` kept
  running its M3 image, whose embedded migrations never apply `0003_m4.sql`. The pentest's default image follows
  (`KONTRA_PENTEST_IMAGE`, `test/pentest`); its project stays `m3pentest` by default, which D266 already lets a run
  beside another checkout's override. The README's upgrade is `git pull && docker compose up -d --build --wait`.
  Amended by D313: only the `gate` service builds the image, which `gate-migrate` runs.
- **The gate is published on IPv4 and IPv6.** `kontra.vps.yaml` advises an AAAA record, but compose published on
  `0.0.0.0`, which Docker binds on IPv4 only, so IPv6 clients were refused. The default now names no host address
  (`${KONTRA_LISTEN_ADDR:+${KONTRA_LISTEN_ADDR:-}:}${KONTRA_PORT:-8443}:8443`), which Docker publishes on both families;
  `KONTRA_LISTEN_ADDR` still restricts it, and set but empty means the default, never `0.0.0.0`.
- **An operator's secrets stay out of git and of the build.** The README's VPS steps write `.env`, the operator's copy
  of the configuration and the card-signing key into the working tree; a root `.gitignore` now ignores them (and a
  provided certificate), keeping the shipped configurations tracked, and `.dockerignore` keeps `.env` and `deploy/` out
  of the build context.
- **The kits name no one's infrastructure.** `scripts/team-gate-vps.sh` defaulted to the owner's DigitalOcean key id
  and SSH key file and refused to delete one droplet by its name. `KONTRA_VPS_DO_KEY` and `KONTRA_VPS_SSH_KEY` are now
  required (exit 2 when unset), the droplets never deleted are those `KONTRA_VPS_PROTECTED` names (none by default),
  and the tests use neutral values; the public files no longer name a host's fleet or a private secrets path.
- **The gate runs are not done.** The README's demo is written for the staging gate, `gateway.kontra.run`, which is
  not deployed: it does not resolve, the clean-laptop and VPS runs (WP-09) have not been made on this head, and
  `docs/gate-runs-m4.md` does not exist. The README now says the staging gate is not up yet and points at the
  from-source gate meanwhile; `docs/README.md` and D282 say the record will be written. **M4 is not gated until
  WP-09 has deployed staging from this head, run `make test-kits`, the clean laptop in source mode and the VPS run
  (dry run, then a real droplet with Let's Encrypt), committed `gate-runs-m4.md`, and, after the merge, run
  `demo-clean-laptop.sh --published --expect <merge commit>`.** None of that is this review's to do: it touches
  staging, DigitalOcean and Cloudflare, which the orchestrator runs (O2). WP-09 ran them on 31 Oct and
  [`gate-runs-m4.md`](gate-runs-m4.md) records them, with each gate item's verdict and commit: staging deployed,
  `make test-kits`, the clean laptop in source mode, the TCK and `anon_limits` passed at `3b890f3`; the VPS run failed
  there on a defect of `compose.yaml` (`gate-migrate` and `gate` both build `kontra-gate:m4`, which races on a fresh
  Ubuntu 24.04's containerd image store), fixed by D313, and passed at `f9f5e61`. The `tck` workflow's first CI run on
  the pull request, staging's redeploy from the merge commit, the published laptop run and the anonymous tenant's
  purge are still to do.
- **Smaller corrections.** The demo's prerequisite is Node 22.19 or later (the pinned inspector and its undici need
  it); compose has three one-shot jobs; M4's decisions run from D268 under their headings. `demo-clean-laptop.sh`
  creates the laptop's log before its follower starts: under load the loop read it first and failed
  (`TestDemoPayloadModes` failed once in `make check` on this host for that reason).

Tests: `TestComposeTeamGate` (the image, its version and the publish default), `TestOperatorSecretsAreIgnored` and the
VPS kit's refusals without `KONTRA_VPS_DO_KEY` or `KONTRA_VPS_SSH_KEY` and its protected-droplet case (`test/vps`).

## From M4's second review (31 Oct)

The second review of the branch (`ea50cec`, after D303 to D306) confirmed six findings. D307 to D311 record how each
was settled; every one has a regression test that fails on `ea50cec`.

### D307. A client's notification or response is answered `202`, never relayed as a stream — **decided under the owner's standing instruction, 31 Oct** (amends D304)

D304 gave a notification and a client's response no `calls` slot because "they hold no relay open", but on the legacy
pinned path the gate forwarded them and relayed whatever the upstream answered: an event stream on a notification was
relayed until the upstream or the client closed it, with no slot and no timeout. The stock node passes
`text/event-stream` through for any exchange, so an anonymous tenant only had to make its own local server stream on
a notification to hold any number of relayed streams open for its `--public` service, bounded only by the edge's
per-address limits (an anonymous caller has no key for `inflight_per_key`). The reviewer relayed 144 events, 600 KB, in
3 s with the cap full. The 2026-07-28 path was not affected: it answers a notification `202` and a response `400`
before forwarding anything.

Decision: the gate answers a forwarded notification or response as the MCP transport says a server answers one, whatever
the upstream sent: `202` with no body when the upstream answered `2xx`, its error status bare otherwise (the 404 that
closes the session and the `401`/`403`/`407` → `502` rule as before), and it closes the upstream's body unread, which
ends the exchange (the tunnel stream, the peer hop, the direct backend's connection). Taking a slot for such a POST
when the upstream streams was the alternative; it would still relay messages a client does not expect there and
read a body the transport says is empty. The body of an error status is dropped too, which the transport allows (an
error response "MAY" carry one): reading it is a relay without a slot as well. relay.md §11.2, tunnel.md §6.2.
Test: `TestEveryRelayedRequestIsCapped` (an upstream that answers a notification and a client response with an
endless event stream while the cap is full: `202`, no body, the upstream's exchange ended). Amended by D312 (an
`initialize` without an `id` was neither: it is refused `400`, never forwarded).

### D308. MCP streams relayed to an anonymous tenant have a limit of their own, `streams` — **decided under the owner's standing instruction, 31 Oct** (amends D304, D287)

D304 made each `GET` or `subscriptions/listen` stream take one of the tenant's `calls` slots for as long as it is open,
and nothing ends an idle stream (keep-alives every 15 s, no write timeout on SSE, its session kept fresh while it is
open, D303). The TypeScript SDK's client (1.32.2) opens its standalone `GET` as soon as `notifications/initialized` is
accepted, and the stock server keeps it open until it is cancelled. So eight idle stock clients held the eight default
slots, and every `tools/call` and every new `initialize`, from any client, was `429` for as long as they stayed: an
anonymous `--public` service, which takes callers without a credential and has no `max_sessions_per_principal`, could be
shut off for its whole `ttl` at no cost. D304's rationale considered only a refused ninth stream, not open streams
starving requests.

Decision: streams are counted apart from requests, at most `streams` per tenant (default 8, a new `tunnel.anonymous`
key), gate-wide in the same `anon_calls` rows with a `stream` column (migration 0003, M4's own and not yet released).
Requests keep `calls`; a ninth stream is `429`, which a stock client reports and goes on from (D304). Reserving part of
`calls` for requests would leave the same starvation at a lower count, and bounding a stream's lifetime frees nothing
for long, since the SDK reconnects its `GET`. tunnel.md §6.2, relay.md §17. Tests: `TestIdleStreamsLeaveCallsTheirSlots`
(eight stock-shaped clients with their `GET` open still get `tools/call` served, and a ninth client's `initialize`; its
`GET` is `429`), `TestEveryRelayedRequestIsCapped` (a request is served while a stream holds its slot; a second stream
is refused), `TestCapsStreamsApartFromCalls` (`internal/anon`).

### D309. An anonymous tenant's limits apply on every multi-tenant gate, and `enabled: false` keeps them — **decided under the owner's standing instruction, 31 Oct** (amends D277, D292, D304)

D304 ran the sweeps on every multi-tenant gate, but the runtime limits (`anon.Caps`: `tasks`, `parts_bytes`, `calls`)
were still built only with `tunnel.anonymous` on, while the registry serves a live anonymous tenant's services until
its `expires_at` whatever the setting says. After an operator turned the setting off, typically against abuse, a live
tenant's `--public` service took any number of tasks and parts from unauthenticated callers until its end (the reviewer
had 11 of 11 accepted against the default `tasks` of 4), and a replica with it off took no `calls` slot for a session
another replica serves. And the setting off could not carry the operator's limits: `false` is §6.2's defaults, and the
object form always meant on, so an operator who kept `purge_after: 168h` for evidence had that data purged 24 h after
each end once the setting was off.

Decision: the caps are built on every multi-tenant gate, with the sweeps' limits (they act on anonymous slugs only, so
on a gate with no anonymous tenant they cost one indexed lookup for a task a node's key creates and one for an MCP
task's dispatch), and the object form takes an
`enabled` key (default `true`): `{enabled: false, purge_after: 168h, …}` opens no anonymous session and keeps those
limits for the tenants that live and for their purge, the others at §6.2's defaults. Recording each tenant's purge
deadline on its row was the alternative; it would take the operator's later change of `purge_after` away from tenants
already ended, and the gate keeps no other per-tenant copy of a limit. A purge reads `purge_after` when it runs, so on
a gate whose replicas disagree the shortest wins; tunnel.md §6 says so. A disabled block's limits are still checked
(`hiring_streams` at most 32) but not refused on a single-tenant gate. tunnel.md §6, relay.md §17,
`spec/schema/config.json`. Tests: `TestAnonymousLimitsHoldWithTheSettingOff` (`internal/gate`, through a replica:
`enabled: false, tasks: 2`, a third task `429`, a message past `parts_bytes` refused), the `enabled: false` case of
`TestAnonymousSweepsRunWithoutAnonymousSessions`, and `TestTunnelAnonymous` (`internal/config`).

### D310. What anonymous tenants can keep stored carries the `tasks` factor — **decided under the owner's standing instruction, 31 Oct** (amends D304)

D304's bound, about (1 + `purge_after` / `ttl`) × `tenants_per_network` × `parts_bytes`, left out D292's Budget
SPEC-CONFLICT: a dispatch counts the tenant's other tasks' bytes as they stood when it started, so its `tasks`
concurrent tasks started together may each store up to `parts_bytes`. The tenant controls its upstream and its callers,
so it reaches that. Decision: the conflict stands (re-reading the total at each externalized part is D292's open
proposal, for the spec's owner), and tunnel.md §6.2 states the bound it gives: (1 + `purge_after` / `ttl`) ×
`tenants_per_network` × `tasks` × `parts_bytes`, 13 × 3 × 4 × 64 MiB ≈ 9.75 GiB per /24 or /48 with the defaults, and
13 × 1000 × 4 × 64 MiB ≈ 3.2 TiB gate-wide, with `tasks` among what lowers it. `Caps.Budget`'s comment already said
"tasks × the room left"; its "64 MiB, the default task_bytes" now says `parts_bytes` (task_bytes is 256 MiB, and
the `min(…, parts_bytes)` caps each task at 64 MiB). Documentation only, so no regression test.

### D311. A seller's case variant of a member the gate owns refuses its card, and §2.4 reads the served card with exact keys — **decided under the owner's standing instruction, 31 Oct** (amends D52, D300; carries D172 to cards)

`card.Rewrite` set and removed members by their exact keys, while a2a-go's card parser and the gate's own §2.4 reader
(`a2ahttp.cardExtensions`, which backs the A2A route's required extensions and `Server.Declared`, hence hire's
activation, D300) decode with `encoding/json`, which matches names case-insensitively and keeps the last match. A seller
whose `capabilities` held `"Extensions"` after `"extensions"` had the operator's entries written at the first place and
its own read in their place, on a card the gate signed and a2a-go verified: an operator's optional entry made required
(hire activates it for every buyer, the route refuses callers without it), a required one (Cloud's escrow) hidden (the
route stops requiring it, hire stops activating it, a buyer's URI is dropped as undeclared before Admission), an escrow
entry's quote replaced, the gate's anonymous extension removed or forged. Money still moved only through the operator's
own quote on its own page, and the node's strict readers refuse such a card. The reviewer's third claim, a top-level
`"SupportedInterfaces"` or `"Capabilities"` shadow, does not reach a client: signing refuses a pair of top-level keys
equal under case folding (`jws.CardPayload`), so such a card was never installed. A lone top-level variant of a member
the gate removes (`"Signatures"`, `"URL"`, an only-anonymous service's `"SecuritySchemes"`) did pass signing.

Decision: `Rewrite` reads the normalized card with exact keys at the levels the gate acts on and refuses it
(`ErrBadCard`: not installed, no card until the seller publishes another, as for a card JCS refuses, D193) when its top
level, `capabilities` or a `capabilities.extensions` entry holds a repeated key, two keys equal under case folding, or a
lone case variant of a member the gate writes, removes or reads there: `supportedInterfaces`, `capabilities`,
`securitySchemes`, `securityRequirements`, `signatures` and the 0.x members at the top; `pushNotifications`,
`streaming`, `extendedAgentCard` and `extensions` in `capabilities`; `uri` and `required` in an entry. Dropping the
variants instead would serve a card the seller did not write in a way it did not mean, and the hosting node refuses them
already. `cardExtensions` reads `capabilities.extensions` and each entry with exact keys, as the node does. relay.md §3.
Tests: `TestCaseVariantsOfGateMembersAreRefused` (`internal/card`, through `Keeper.Seed` with the gate's signer: each
variant refused; the controls install and a2a-go reads exactly the operator's entry), `TestDeclaredReadsExactKeys`
(`internal/a2ahttp`: the variants declare nothing; an installed card declares what a2a-go reads of it).

## From M4's third review (31 Oct)

The third review of the branch (`3b890f3`, after D307 to D311) confirmed one finding, a minor one. D312 records how it
was settled; its regression tests fail on `3b890f3`.

### D312. An `initialize` without an `id` is refused `400`, never forwarded — **decided under the owner's standing instruction, 31 Oct** (amends D307, D304)

D304 gave every forwarded request with an `id` a `calls` slot and D307 answered every forwarded notification `202`, but
an `initialize` without an `id` was neither. `mcpwire.Parse` reads a method without an `id` as a notification, §11.12's
rule 1 sends every `initialize` to the legacy era, and the relay routed it to the session opening on its method alone,
where `takeRelay` takes no slot for a message without an `id` and nothing answered it `202`. So, with the `calls` cap
full, the gate forwarded it and relayed what the upstream answered: a `2xx` JSON body up to the 8 MiB `MaxBody`, with no
session; and an event stream that sends no response was held with no time limit, since the relay waits for a response
whose `id` matches the request's, which an absent `id` never does, and neither the edge nor the relay sets a deadline
on the exchange. A silent stream, or one of events typed other than `message` (dropped before they count toward
`MaxBody`), held the exchange, an edge connection and a tunnel stream for as long as the tenant's own server kept it
open. `message` events, comment-only ones included (they have no `event:` field, so they are messages), are held up to
`MaxBody` and end in `502`. The node passes such an `initialize` through (`mcproots` rewrites only
`capabilities.roots`). Only a tenant's own server could use it, a stock upstream answers at once, and nothing past
8 MiB per exchange reached a client: concurrency beyond `calls` and holds with no slot, not an unbounded relay, so
the review rated it minor. The reviewer reproduced it with the cap full: a 1 MiB JSON answer relayed with `200`, and a
silent stream and 10 MiB of typed events still open after 6 s with no slot taken. The gate's own `/mcp` had the same
gap without the relay: it opened a session for such an `initialize` and answered a result with `id` `null`.

Decision: an `initialize` is a request (MCP lifecycle; a request carries an `id`), so one without an `id` is §11.1's
invalid request, `400` with `-32600` `invalid request` and `id` `null`, refused in §11.12's rule 1
(`mcpwire.Classify`) on `/mcp/{svc}` and `/mcp` alike, before Admission, any slot or anything is forwarded. The stdio
bridge already refused one so. Taking a slot for it, or answering it `202` as D307 answers a notification, were the
alternatives. Both still send the upstream a message it cannot answer, and a `202` would tell the client nothing about
the session it asked for. relay.md §11.12, tunnel.md §6.2. Tests: `TestEveryRelayedRequestIsCapped`
(`internal/mcprelay`: with no slot free, an `initialize` without an `id`, without a session and on one, to an upstream
that answers it as the stock double does, with a 1 MiB JSON body, with a silent event stream and with an endless one:
`400` with that body each time, and the upstream receives none), `TestGateMCPReviewFixes` (`internal/gate`: on `/mcp`,
`400` and no session row), `TestClassify` (`internal/mcpwire`: whatever `MCP-Protocol-Version` says).

## From M4's gate runs (31 Oct)

The team gate on a clean VPS (gate run C, WP-09, at `3b890f3`) failed at `docker compose up -d --build --wait` on a
defect of `compose.yaml`. D313 records the fix, with which the run passed at `f9f5e61`; the gate runs' record,
`docs/gate-runs-m4.md`, holds both runs.

### D313. Only the `gate` service builds the gate's image, and `gate-migrate` runs it — **decided under the owner's standing instruction, 31 Oct** (amends D306; completes D281)

`compose.yaml`'s `x-gate` anchor carried `image: kontra-gate:m4` and `build`, and `gate-migrate` and `gate` both merge
it, so compose built one tag twice, in parallel. On the droplet the second export failed:
`#18 ERROR: image "docker.io/library/kontra-gate:m4": already exists`, `target gate: failed to solve`. A fresh Ubuntu
24.04 now installs `docker.io` 29.1.3 (containerd 2.2.1; compose 2.40.3 and buildx 0.30.1 from Ubuntu's packages, as
`test/vps/cloud-init.yaml` asks), whose fresh install uses the containerd image store (storage driver `overlayfs`,
`driver-type io.containerd.snapshotter.v1`); there the second export of a tag the first has just named fails. On the
classic `overlay2` store (this host: Docker 24.0.5, compose 2.24.6) retagging is idempotent and nothing showed. It is a
race: the shape at `3b890f3` failed 5 of 15 fresh tries in an `ubuntu:24.04` container with the droplet's packages.
The kits could not see it: they gave both services the kit's image and reset the build (`build: !reset null`) or came
up with `--no-build`, and the record kept only compose's last 20 lines, where the error happened to be. The droplet
lived 5 min 34 s and was torn down and checked; nothing past step 4 ran.

Decision: `build` moves from the anchor to the `gate` service, the one service that builds `kontra-gate:m4`;
`gate-migrate` keeps the image and runs what `gate` built. Compose builds the images a command needs before it creates
any container, so the image exists when `gate-migrate` starts (`gate` depends on `gate-migrate`, not the reverse, and
no `depends_on` could order a build). Before it builds, compose tries to pull `gate-migrate`'s image and, because a
service builds that image, ignores the failure (compose 2.24.6 prints `gate-migrate Pulling` and `gate-migrate Error`,
and goes on). Run alone, `gate-migrate` needs the image first: `docker compose build gate && docker compose run --rm
gate-migrate`, which the README's upgrade now says (before, `run` built it). A second tag for `gate-migrate`, built
from the same Dockerfile, was the alternative: two builds of one binary, and two images to keep in step.

The VPS run's record says which Docker it met and keeps what compose said. `scripts/team-gate-vps.sh` records, in
step 4 and so also with `--host`, the server's components (engine, containerd, runc), the storage driver and its
status, compose and buildx (step 2 recorded compose and buildx only, and only on a droplet). It keeps compose's whole
output: on success the images compose built (its `naming to … done` lines) and the last 20 lines; on failure every
line, then `docker compose ps -a` and each service's last 40 log lines. The rehearsal (`TestTeamGateRehearsal`) no
longer resets the gate's build: the script's `docker compose up --build` builds the gate from the shipped archive, as
on the VPS, under a tag of the run's (`kontra-gate:kitvps-<pid>`, never `kontra-gate:m4`, which another checkout may
hold), and the test requires one build of it in the record, `kontra-gate m4`, and `gate-migrate` exited 0 on the image
the gate runs. This host's `overlay2` store would retag a second build silently, so the builds are counted.

Verified before this change (the gate runs' evidence, 31 Oct): the one-build shape 0 failures in 15 fresh tries on
Docker 29.1.3 and compose 2.40.3, an upgrade (`up --build` over a running stack) and `build` then `up --no-build`;
this `compose.yaml` with the whole stack and a real gate build on the same Docker (1 CPU): `up -d --build --wait`
exit 0 in 366 s, one `naming to`, every service healthy or exited 0, `kontra-gate version` m4, `/healthz` 200, an API
key and an enrollment token minted. A named volume over the image's 65532-owned `/var/lib/kontra-gate` is 65532's and
writable on the containerd store too. Tests: `TestComposeTeamGate` (`test/pentest`, in `make check`: no service but
`gate` declares `build`, and `gate-migrate` runs the gate's image; fails on `7033e0a` with `service gate-migrate builds
"kontra-gate:m4" too`), `TestComposeConfigResolves` (the same, as `docker compose config` resolves the file with either
configuration; fails there with `the services that build [gate gate-migrate]`), `TestTeamGateHostMode` and
`TestTeamGateRecordsComposeFailure` (`test/vps`, against stubs: the versions, the storage driver and the built image in
the record; on a failure all 31 lines of compose's output and the stack's state, and no step 5; both fail on the
script as it was), `TestTeamGateRehearsal` (`make test-kits`). The VPS run was made again with this fix at `f9f5e61`
(31 Oct, droplet 614610093: Docker 29.1.3 on the containerd image store, compose 2.40.3) and passed: `up -d --build
--wait` exit 0 in 232 s with one build, `gate-migrate` and `gate` on the one image, the Let's Encrypt certificate, and
a hire and an MCP `search` through the gate from another machine ([`gate-runs-m4.md`](gate-runs-m4.md)).

## From M5's gate side (1 Nov)

Kontra Cloud builds M5 against this gate, and its record (kontra-cloud `docs/m5-decisions.md`) names what it needs
from this repository: decision 7's OIDC principals on the A2A routes with a tenant mapping and a revocation feed
(public-repo PR M5-G1; its C4 and D-M5-35). D314 to D316 settle them. Where Cloud's record proposed something else, the entry
says so and what Cloud changes.

### D314. The A2A routes take an issuer's JWTs as OIDC principals — **decided under the owner's standing instruction, 1 Nov** (carries out the owner's decision 7; completes D268, D276)

The owner's decision 7: the gate accepts Cloud's JWTs as OIDC principals, issuer = Cloud's control plane with its JWKS,
audience `kontra`, revocation checked against `/v1/auth/revocations`. `auth.oidc` (PRD §11, [`relay.md`](../spec/relay.md)
§2.3) was refused at load with a warning since M1, and D268 and D276 left it to M5 on §12.1's verifier. Decision
(§2.3, §17; `spec/schema/config.json`):

- **Which bearer.** With `auth.oidc`, a bearer of a JWT's shape (`auth.LooksLikeJWT`, as D296 reads one on the MCP
  routes) on an A2A route is verified as the issuer's token and never looked up as an API key; any other bearer is an
  API key. Without it nothing changes: a JWT is looked up as a key and is `401`.
- **How.** §12.1's verifier (D276, D284, D296): metadata and key set as operator-configured endpoints, the `kid`
  refetch at most once a minute, ES256, RS256 or EdDSA, `iss`, `exp`, `nbf` with 60 s of skew. `aud` must contain
  `auth.oidc.audience` (exactly, as one member). The principal is `oidc:sub:<sub>`, `buyer_id` UUIDv5(issuer ‖ sub)
  (the same buyer as the subject's MCP tokens, which Cloud's D-M5-11 pins), `via = oidc`, and the tenant D315 maps.
- **Audiences never cross.** A token for an MCP route's canonical URI is `401` on the A2A routes; an
  `auth.oidc.audience` token is `401` on the MCP routes (`AuthenticateMCP` never asks the A2A verifier). So Cloud's
  `aud kontra` tokens (its D-M5-39) and its MCP tokens (`aud` = the route) are two credentials.
- **One issuer, one verifier.** When `auth.oidc.issuer` equals `auth.mcp_oauth.issuer` (Cloud's case), the replica
  builds one `oidc.Issuer` for it: one key-set cache, one revocation feed (D316), one tenant claim (D315). Both blocks
  must then write the same `tenant_claim` and `revocations`, or the load fails: the two settings are the issuer's,
  true of every token it signs on every route, and a block that left the feed out would be a hole on its routes.
- **Failures.** A refused token is `401` (`Bearer`, as every A2A `401`); an issuer, feed or tenant the gate could not
  read is the gate's own failure, answered as a database failure is on these routes (`-32603` on JSON-RPC, `503` on a
  card), never `401`: a caller cannot tell a refused token from an outage of the operator's, and a client that
  retried a `401` with a new token would be misled.
- **Streams.** A token is checked when its request arrives. A stream it opened is not cut when it expires or is
  revoked, as an API key's is not; the next request is checked.

The `auth.oidc` warning (`Config.unbuilt`) and the `oidc:*` warning on a gate with `auth.oidc` alone go. Why reuse the
MCP verifier rather than a second one: the checks are §12.1's, which D276 already settled and M4 tested, and Cloud's
issuer is one population of tokens. What it does not do: no scope or `azp` check (Cloud mints `aud kontra` for one
purpose), no token introspection (the feed is the revocation channel), no per-service audience.

Tested: `TestAuthenticateA2A` (`internal/auth`: a JWT is the verifier's on A2A only, a key is a key, a refused token
`401`, a feed stale the gate's own error, a node's request its session's), `TestA2A` and `TestNewIssuerRefuses`
(`internal/oidc`: three algorithms, `aud` among others, an MCP audience, another audience, expired, a rotated key
fetched once), `TestOIDCIssuerPolicy` and the `TestLoadErrors` rows (`internal/config`), `TestGateA2AOIDC` (`internal/gate`,
a whole replica against `oidctest` with its own JWKS and feed: a task via `oidc` with the token's buyer, read back by
another token of the subject; audiences not crossing; a key rotated at the issuer accepted after one refetch).

### D315. An issuer's tenant claim maps its principals to a tenant by slug — **decided under the owner's standing instruction, 1 Nov** (completes D314; amends D276's "no tenant")

[`relay.md`](../spec/relay.md) §2.3 gave an OIDC principal "none, unless the operator maps it" and §12.1 left the
mapping to M5; Cloud's D-M5-35 asks for "a tenant claim mapping" without naming one. A principal's tenant is what
`org:<tenant>` entries admit (a private listing is `[org:<tenant>]` in Cloud's PR-3) and what Admission receives as
`principal.tenantId`. Decision (§2.3, §12.1, §17):

- **Which claim.** `tenant_claim`, on `auth.oidc` and on `auth.mcp_oauth`, names one top-level claim (a name of 1 to
  128 characters `[A-Za-z0-9_.:/#~+-]`, so a collision-resistant URI such as `https://kontra.sh/claims/tenant` or a
  private name; never `iss`, `sub`, `aud`, `exp`, `nbf`, `iat`, `jti` or `sid`). Absent: no claim is read and no OIDC
  principal has a tenant, as before. The gate does not fix the name: a team's IdP chooses its own claims, and the
  operator writes the name it emits.
- **Its value.** A string, the slug of a live tenant of this gate of kind `team` or `account` (not deleted, not
  expired): the slug is what `org:` entries, routes and Cloud's party handles name, and a team's IdP can know it where
  it cannot know a random UUIDv7. The principal then carries the tenant's id and slug.
- **A token without the claim** is a principal without a tenant: admitted by `oidc:sub:`, `oidc:*` and `anonymous`,
  never by `org:`. It is not refused, because a buyer with no tenant of its own (a signed-in person who hires) is the
  normal case.
- **A token with a claim the gate cannot honour** is `401`: a value that is not a slug (not a string, `null`, upper
  case), a slug of no live tenant, or of an anonymous tenant (an anonymous tenant is a session's node key, never a
  signed-in principal's; tunnel.md §6). The issuer asserted a membership that does not hold, so the token is refused
  rather than accepted with less: it fails closed, and the operator's bug shows.
- **Tenancy.** The rule is the same on both. On a multi-tenant gate the claim names one of the operator's tenants. On
  a team gate the only tenant is `default`, so a team's IdP writes `default` for its members, and `org:default`
  (which a handshake gives the services it creates, tunnel.md §5) admits them.
- **Freshness.** `tenant.Resolver` keeps an answer 5 s and never past the tenant's `expires_at`, so a deleted tenant
  stops mapping within 5 s, the bound an API key's tenant has (§2.3's revocation cache); an unknown slug is kept as
  long, so one token cannot cost a query per request. A database failure is the gate's own error, not a `401`.
- **The buyer does not change.** `buyer_id` is UUIDv5(issuer ‖ sub) whatever the tenant (D-M5-11).

What Cloud changes: its `aud kontra` tokens (D-M5-39: `sub` = party, `sid`) and its MCP tokens carry the party's gate
tenant slug in the claim its configuration names, when the party has a tenant; the two blocks of its gate
configuration write the same `tenant_claim`.

Tested: `TestIssuerTenantClaim` (`internal/oidc`: mapped, absent, unknown, malformed, a database failure, the setting
off), `TestResolver` (`internal/tenant`, Postgres: team, account, anonymous, deleted, expired, unknown, the 5 s and
`expires_at` bounds), `TestAllowList` (`internal/auth`: a mapped principal is an `org:` member and still an OIDC
principal), `TestGateA2AOIDC` (`org:default` on A2A and on `/mcp/{svc}`), `TestGateA2AOIDCMultiTenant` (`org:acme` on
a multi-tenant gate; an anonymous tenant's slug, an unknown slug and a case variant `401`).

### D316. An issuer's revocation feed: polled, required of its tokens, failing closed after 30 s — **decided under the owner's standing instruction, 1 Nov** (completes D314; carries out Cloud's C4 and D-M5-35)

A token the gate verifies by itself is good until it expires; decision 7 has Cloud's revocations reach it through
`GET /v1/auth/revocations`, which at kontra-cloud `c16e182` (`gate/auth/service.py` `recent_revocations`, the
`Revocations` model) answers `{"window_seconds": 1200, "sessions": [sha256(sid) hex, …]}`: every session revoked in
the last 20 minutes (at most 200,000), newest first, `Cache-Control: no-store`, public. Its tokens carry `sid` and
`iat`; access tokens live 15 minutes and MCP tokens 10 (its D-M5-38). Decision (§2.3, §10.3, §12.1, §17):

- **Configuration.** `revocations: {url, poll, max_stale}` on `auth.oidc` and `auth.mcp_oauth` (equal when they name
  one issuer, D314). `url` is an operator-configured endpoint (§10.3, D284: `https://`, or `http://` on a loopback
  host; no redirect, no proxy), read with a 10 s timeout and a 32 MiB bound (262,144 entries of 67 bytes fit with
  room; above Cloud's own 200,000). `poll` defaults to 5 s (1 s to 1 min), §2.3's bound for an API key's revocation;
  `max_stale` to 30 s (at least twice `poll`, at most 5 min).
- **The answer.** A JSON object with exact keys (`strictjson`), `window_seconds` an integer from 1 s to 30 days,
  `sessions` an array of 64-hex-digit digests; other members are ignored. One malformed entry, or any other shape, is
  a failed read: the gate cannot tell what it would miss. A failed read keeps the last list.
- **What a token then needs.** A string `sid` of at most 255 bytes, whose sha256 (of its UTF-8 bytes, hex) is not
  listed; an `iat` at most 60 s ahead; and a lifetime `exp − iat` of at most `window_seconds − 120 s` (1080 s under
  Cloud's window). Otherwise `401`. The lifetime bound is what makes the list sufficient: a session revoked at R is
  listed until R + W on the issuer's clock, its tokens were issued at `iat` ≤ R, and the gate accepts one until
  `exp` + 60 s on its own clock, within 60 s of the issuer's; so with `exp − iat` ≤ W − 120 s the session is listed
  for as long as any of its tokens is accepted. The gate therefore keeps no revocation of its own, and a replica that
  restarts knows every revocation that matters from its first read. A token without `sid` or `iat` cannot be checked,
  so it is refused rather than trusted.
- **Staleness fails closed.** Before the first read, and once no read has succeeded for `max_stale` (counted from when
  the last good read began), every token of the issuer is the gate's own failure (D314's: an internal error on the A2A
  routes, `503` on the MCP routes and cards), never accepted; a revoked token keeps its `401`. API keys, nodes and
  anonymous callers are unaffected. So a revocation reaches every replica within `max_stale`, normally within `poll`.
  30 s is the trade: an outage of the operator's control plane longer than that stops its signed-in callers (not its
  API keys) until it is back, which is the price of never accepting a token that may have been revoked; a longer
  bound lets a revoked token work longer. The operator can set it from 2·`poll` to 5 min.
- **Start.** Each replica polls each distinct feed from `Run`, and waits up to 5 s (`FeedWait`) for each one's first
  read before it serves, so the tokens of its first instants are not refused for want of a read; a feed still unread
  is logged and polled on.
- **Watching it.** Gauges `kontra_gate_oidc_revocations_age_seconds` (since the last good read began; `+Inf` before
  any) and `kontra_gate_oidc_revocations_fresh` (1 while under `max_stale`), labelled by `replica` and `issuer`; a
  revoked session's token refused is logged at info with the digest's first 4 bytes and the subject.

Conflict with Cloud's proposals: none in the feed's shape. Two requirements follow for Cloud: every token it mints for
the gate (the `aud kontra` kind and the MCP AS's) carries `sid` and `iat` and lives at most `window_seconds − 120 s`
(its 15 and 10 minutes do; `REVOCATION_WINDOW`'s comment already intends it), and its gate configuration writes the
same `revocations` in both blocks. Its `/v1/auth/revocations` stays public, as decision 7 has it: a digest discloses
nothing without the token.

Tested: `TestParseFeed`, `TestNewFeedRefuses`, `TestIssuerRevocationFeed` (`internal/oidc`: never read, `sid` and `iat`
required and bounded, the lifetime bound at 1080 s and 1081 s, a revocation refused on A2A and MCP audiences and
another session not, a failed read keeping its list, stale at 30 s and fresh at 29, a malformed answer, the window
shrinking), `TestFeedRun`, `TestA2A` (revoked; stale), `TestOIDCIssuerPolicy` and `TestLoadErrors` rows
(`internal/config`), `TestGateA2AOIDC` (a revoked `sid` `401` within the poll on A2A and `/mcp`, another session
served; the feed down: past `max_stale` `-32603` on A2A and `503` on `/mcp/fs` while an API key is served, the gauge
0, then back), `TestOperatorFixtureLoads` (`spec/schema/examples/config.operator.json`).

