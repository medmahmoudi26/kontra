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
is accepted ([`migrations/0001_init.sql`](../migrations/0001_init.sql), [`relay.md`](../spec/relay.md) §3).

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
([`ext-sealed.md`](../spec/ext-sealed.md) §2–§3).

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
URL §13 allows), and every other operator URL follows §10.3 ([`relay.md`](../spec/relay.md) §10.3, §13.1).

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
`SPEC-CONFLICT` where that departs from the text. The specs' text is unchanged by these entries: the ones marked
**needs the owner** change documented behaviour and wait for the owner's decision before the specs follow; the
others settle a silence or a contradiction.

### D123. A `--serve` target is served at its root — **needs the owner** (C1)

[`tunnel.md`](../spec/tunnel.md) §8.2 has the node prefix the target's base path to every request path, while the
gate calls the path of the upstream card's interface ([`relay.md`](../spec/relay.md) §6.2, §3, D71), which already
holds that base: `http://h:9/base` with interface `/base/rpc` would be called at `/base/base/rpc`. The gate cannot
strip a base it does not know without new wire protocol. Decision: M2 fails closed: `kontra connect` refuses a
`--serve` target whose path is not empty or `/` (`ErrBasePath`, naming this conflict); every example serves its
agent at the root. The node's proxy still refuses a path outside a non-root base (`400`), which only a unit test can
reach (`internal/node` `TestProxyConfines` builds a `Serve` directly), so `sidecar_not_a_proxy` checks confinement
under the root base instead: `/../../etc/passwd` reaches the target as `/etc/passwd`. Proposal: §8.2 forwards a path
already under the base as it is and prefixes any other (`internal/node/serve.go`, `TestSidecarNotAProxy`).

### D124. A hello whose outcome was lost is probed; a confirmed token is never sent again — **needs the owner** (C2)

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
was revoked makes a committed probe read as not committed (its welcome leaves that service out); the full hello
then resends the used token, and its `enroll_invalid` stops the node for good. The owner's alternative, which would
close it, is an idempotent admission at the gate, or a welcome that says what the hello enrolled
([`tunnel.md`](../spec/tunnel.md) §4, §5, §12; `internal/node/run.go` `connect`,
`TestConsumedTokenIsNotSentAgain`, `TestLostWelcomeProbe`, `TestRealAdmissionLostWelcome`; end to end,
`TestTunnelLostWelcome` loses a first hello's welcome and `TestTunnelKill` restarts its node with its original
`--enroll`).

### D125. `rotate_from` at a gate that cannot carry the rotation — **needs the owner**

§12 has a rotated node connect with `rotate_from` naming the old key, which must be a key of this gate, neither
revoked (`revoked`) nor rotated (`enroll_invalid`), and §4 retries neither. But one key is enrolled separately at
each gate (§2.1), and the key file records which gates confirmed a rotation, not which hold the old key: a first
enrollment at a gate that never knew the old key, or a rotation after a leak followed by revoking the old key
(§12, §15), would be refused on every start. Decision: the node sends `rotate_from` to a gate that has not
settled the rotation, and when that gate refuses it `enroll_invalid` or `revoked`, sends the hello again at once
without it and does not name that key there again in this process; the hello's tokens or `enroll:` scope then
enroll the new key, and a gate that knows neither key answers `unknown_key`, which is retried. Any welcome of
the active key settles the rotation at its gate. Proposal: §12 says the node sends no `rotate_from` to a gate
that does not hold the old key, or the gate ignores it there ([`tunnel.md`](../spec/tunnel.md) §12;
`internal/node/run.go` `connect`, `TestRealAdmissionRotationAtANewGate`,
`TestRealAdmissionRotationAfterOldKeyRevoked`, `TestNodeRotateAfterLeak`).

### D126. The old key is pruned 24 h after the last gate that confirmed the rotation — **needs the owner**

§12 times the node's 24 h from the rotation's commit at the one gate it rotates at; §2.1 enrolls one key at each
gate the node dials, and gives the node no list of them. Decision: `nodekey.Prune` drops an old key only when at
least one gate has confirmed the rotation (`# rotated: <time> <gate>`) and every confirming gate's stamp is 24 h
old; a key no gate confirmed is never dropped. A gate the node has not redialed since is not waited for; waiting
for every enrolled gate needs that set, from the credentials file, and is the owner's call
([`tunnel.md`](../spec/tunnel.md) §2.1, §12; `internal/nodekey/nodekey.go` `Prune`, `TestPrune`).

### D127. A new handshake of a rotated key inside its overlap is refused `revoked`

§12 keeps a rotated key valid for 24 h for the tasks already on its session, and says nothing of a new handshake
with it. Decision: such a hello is refused `revoked`: the old session keeps its tasks, but a new session of the key
could serve nothing new, since new attempts never select a rotated key ([`tunnel.md`](../spec/tunnel.md) §12;
`internal/enroll/admit.go` `refuseUnusable`, `TestAdmitRotation`).

### D128. Revoking a key does not follow its rotation — **needs the owner**

§15: revoking a stolen identity key ends its session and every future handshake. But a rotation needs only proof
of the old key, so a thief can rotate the stolen key to one of their own first. Following `rotated_to` on revoke
would also revoke the new key of the legitimate flow (rotate after a leak, then revoke the old key to end its
overlap early). Decision: revocation does not follow the chain; `kontra-gate enroll --revoke` warns on stderr with
the newest still-enrolled key of the chain and the command that revokes it ([`tunnel.md`](../spec/tunnel.md) §12,
§15; `internal/enroll/revoke.go` `Successor`, `cmd/kontra-gate` `enrollRevoke`, `TestEnrollRevokeRotatedKey`).

### D129. A rotation copies every binding with its revocation — **needs the owner**

§12: the rotation "copies its active `key_services` rows". With only the active rows, a service whose binding was
revoked would be one the new key was never enrolled for, and the node's next handshake with its unchanged
`--serve` would be `service_denied`, final for every service. Decision: every row is copied with its `revoked_at`;
a revoked copy grants nothing but keeps §5's "left out" for the new key ([`tunnel.md`](../spec/tunnel.md) §5, §12;
`internal/enroll/rotate.go`, `TestAdmitRotationKeepsRevokedBindings`).

### D130. A revoked binding is enrolled again only by a one-time token — **needs the owner**

§5 leaves a revoked binding out of `welcome.services` but does not say what may enroll it again. The hello's
standing API key rides on every handshake as the hiring principal, so letting its `enroll:` scope do it would undo
the operator's revocation at the node's next connect. Decision: only a service or tenant token in the hello
re-enrolls a revoked binding, as the operator's explicit act ([`tunnel.md`](../spec/tunnel.md) §5;
`internal/enroll/admit.go`, `TestAdmitAPIKeyKeepsRevokedBinding`, `TestRevocationsReenrolledBinding`).

### D131. `kontra-gate enroll --revoke <key_id> --service <name>` revokes one binding — **needs the owner** (C9)

§12 defines a binding's revocation, and `key_scope` needs one, but names only the key's command, and a team gate has
no console. Decision: `kontra-gate enroll -f kontra.yaml --revoke <key_id> --service <name>` revokes one binding
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
`TestKeyScope`).

### D134. A hello of the wrong shape takes its subject's code — **needs the owner**

§4's twelve codes are a closed list, with none for a malformed hello (a name outside 1–64 characters, a nonce that
is not 32 bytes, a field of the wrong type, a NUL) or for a binary message during the handshake. Decision: each
refusal takes the code of its subject: `bad_origin` for the origin, `bad_proof` for a key id or recipient that does
not match, `service_denied` for a bad service kind or name; a shape error with no listed subject, and a binary
message, are `unsupported_version`, which the node retries. Proposal: a non-retryable code for a malformed
handshake ([`tunnel.md`](../spec/tunnel.md) §4; `internal/tunnel/handshake.go` `ParseHello`,
`TestParseHelloRefusals`).

### D135. A connection that cannot be channel-bound is refused at the upgrade — **needs the owner**

§4 binds `tls-exporter` whenever the gate terminates TLS and has no code for a connection that cannot export (TLS
1.2 without Extended Master Secret), and the audit forbids falling back to `none` for one connection. Decision: the
gate refuses the upgrade itself, so no challenge is sent: `400` when the client's TLS has no exporter, `500` when
the gate binds `tls-exporter` on a listener without TLS. The node sees a failed dial and retries. Proposal: a §4
code for it ([`tunnel.md`](../spec/tunnel.md) §4, §4.1; `internal/tunnelgate/handler.go`,
`TestExporterUnavailableIsRefused`).

### D136. The gate's own failure during a handshake closes `1011` — **needs the owner**

§4's last message is welcome or error, and error's codes have none for the gate's own failure (Postgres
unreachable during the admission). Decision: the gate sends neither and closes with WebSocket `1011`; the node
retries it, and since the admission may have committed, probes first (D124). Proposal: a retryable code for it
([`tunnel.md`](../spec/tunnel.md) §4; `internal/tunnelgate/handler.go` `ServeHTTP`, `TestAdmitOutcomes`).

### D137. A node-opened stream is in flight until both halves end — **needs the owner**

§14 answers a node-opened stream over the cap (32) with `429` and keeps the session. M2 serves no hiring, so every
node-opened stream is refused at once, and a refusal frees nothing while the node keeps its half open (yamux
buffers up to a window per half-closed stream and has no reset). Decision: the gate counts a node-opened stream as
in flight from its accept until both halves have ended, and closes the session when one more than the cap would
be. M3's `429` needs the same count; §14 may need to say that in flight lasts until both halves end
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
`TestAddSerializesOneKey`, `TestEnrolledNotices`, `TestAdmitAnnouncesEnrollments`).

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
expired, a row that is gone) sends it, then closes ([`tunnel.md`](../spec/tunnel.md) §12, §13;
`internal/tunnelgate/control.go`, `TestAllowsRefusals`, `TestRevokedWithoutDatabase`).

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
becomes a candidate). Proposal: §3 says a service's nodes serve the same card ([`relay.md`](../spec/relay.md) §3;
`internal/gate/services.go` `newTunnelKeeper`, `TestTunnelCardKeepers`).

### D143. A task placed before a tunnel service's first card is retried — **needs the owner**

§3 fetches a tunnel service's card when a session for it connects, and §6.4 fails a task whose card is unavailable
`service_unavailable`, with no retry. A node started before its agent (a sidecar's order) has a session whose card
fetch failed (`502 Kontra-Node: target_unreachable`); withholding the session would take down the node's other
services, the handshake being all-or-nothing. Decision (the audit's second option for its gap 3): the session
becomes a candidate anyway, its card is retried on the keeper's backoff (1 s doubling to 1 min), and a dispatch for
a tunnel service whose keeper has no card yet is retried like `no_session` (`relay.ErrCardPending`), failing
`service_unavailable` only once `task.retries` are spent. That dispatch has the card fetched again at once (at
most once a second), never later than the fetch already due: a dispatch that only restarted the backoff left an
agent that came up after its node to a refetch up to a minute away, and the task failed meanwhile
([`relay.md`](../spec/relay.md) §3, §6.4; `internal/gate/services.go` `tunnelConnected`, `runTunnelCard`,
`TestTunnelBackend`, `TestTunnelCardPendingFetchesAtOnce`; end to end, `TestTunnelCardPending`, whose task is sent
as its node's agent comes up).

### D144. `sealed:` and `seller_keys` on a tunnel service are refused — **needs the owner** (amends D90, D91)

D90 and D91 accepted `seller_keys` and `sealed:` on tunnel services with a warning while M1 did not serve them. M2
serves tunnel services and neither seals nor verifies seller signatures (M3), so both would fail open. Decision:
either key on a tunnel service refuses to start, and a row with it is left out of the registry. Consequence: the
spec's team fixture, `spec/schema/examples/config.team.json` (`sealed` on `recon` and `fs`, both tunnel services), no
longer loads; `TestTeamFixtureLoads` asserts exactly those two refusals. The work that builds sealing and
verification lifts them ([`relay.md`](../spec/relay.md) §3, §17; `internal/config/validate.go`,
`internal/gate/services.go` `refusable`).

### D145. No capacity skip in the session choice (C5)

[`relay.md`](../spec/relay.md) §6.1 and §14.2 skip replicas at their dispatch capacity, but 0001 records no
replica's dispatches in flight, `prepare` runs on any replica, and `attempts` has no index on `replica_id`.
Decision: M2 does not skip by capacity; load is bounded by each replica's `kontra-tunnel-<id>` worker
(`limits.dispatch_slots`) and by §6.4's schedule-to-start row. The round-robin among candidates is per replica.
A capacity-aware choice waits for a column written with the replica's heartbeat ([`relay.md`](../spec/relay.md)
§6.1, §14.2; `internal/tunnelgate/picker.go`, `TestPickerCandidates`).

### D146. `no_session` attempts count against `task.retries` — **needs the owner** (C6)

§6.4's `no_session` row ("retry after 1 s, 2 s, 4 s … capped at the deadline; failed: no_session when exhausted")
does not say what is exhausted. Decision: read with the section's opening sentence, `task.retries` bounds the new
attempts, and a `no_session` attempt is one: with the default 2, a task sent while its only node reconnects fails
`no_session` within about 3 s. The alternative, waiting for a node until the deadline, changes §6.4's text
([`relay.md`](../spec/relay.md) §6.4; `TestNoSessionRetriesThenFails`, `TestKeyScope`).

### D147. A `full` answer after the first upstream id is retried for 30 s (C7)

§6.4's tunnel row retries only a poll refused `Kontra-Node: full` after the first upstream id; a follow-up's
`SendMessage` or a stream refused so would read as "any other: failed: `tunnel_lost`", although `full` proves the
node forwarded nothing. Decision: before the first id, `full` is a retryable `tunnel_lost` (another session); after
it, any exchange refused `full` is made again on the poll's backoff (1 s doubling to 5 s) for at most 30 s (D97's
grace), then fails `tunnel_lost`. `forward_cancel` retries a `full` answer for up to 20 s, after which the cancel
counts as not delivered ([`relay.md`](../spec/relay.md) §6.4, §8; `internal/relay/executor.go` `nodeFull`,
`TestTunnelNodeFullWhilePolling`).

### D148. The cancel of a tunnel task whose session is gone fails it `tunnel_lost` — **needs the owner** (C3)

§8's row "active or `input-required` | exists, session gone" answers "the task as it is after that (failed:
`tunnel_lost`)", assuming a dispatch that is failing. For a task at `input-required` no dispatch runs, and M1's
workflow answered a `forward_cancel` that failed, or did not start within 10 s, with "the task continues". D77
would also end a waiting attempt `interrupted`. Decision (the audit's accepted proposal): `forward_cancel` answers
`tunnel_lost` when this replica does not hold the session, or the session no longer serves the service, with no
stream opened; for a tunnel attempt that, or a `forward_cancel` not started within 10 s on
`kontra-ctl-<session replica>`, makes the workflow write `failed: tunnel_lost` (the attempt's outcome
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
`drain_timeout` has passed since the drain began; a dispatch cut at `drain_timeout` reports it
([`relay.md`](../spec/relay.md) §14.3; `internal/lifecycle/workers.go`, `TestDrainWaitsForTunnelDispatches`,
`TestTunnelDrain`).

### D151. A `400` the node writes carries no `Kontra-Node` — **needs the owner**

§8.2's node-written answers carry `Kontra-Node` with a reason from a closed list (`not_hosted`, `kind_mismatch`,
`full`, `target_unreachable`, `sealed_rejected`), and the same section has the node answer `400` to `CONNECT`, an
`Upgrade`, `TRACE`, a path outside the base and an unreadable preamble, which no listed reason fits. A new value
would change the wire, and the gate reads unknown values as `tunnel_lost`, retried elsewhere. Decision: those `400`s
carry no `Kontra-Node`, so the gate takes them as the service's own `400` (`upstream_error`, not retried), which
suits requests no gate sends. Proposal: a reason such as `refused`, or the rule that a node-written `400` carries
none and is final ([`tunnel.md`](../spec/tunnel.md) §8.2; `internal/node/proxy.go`, `TestProxyRefusesMethods`,
`TestSidecarNotAProxy`).

### D152. `kontra connect` reads `KONTRA_KEY` only — **needs the owner**

PRD §6 has `KONTRA_GATE`, `KONTRA_AGENT`, `KONTRA_TO` and `KONTRA_ENROLL` mirror the flags of `kontra tunnel`,
which note 1 replaced with `kontra connect`; tunnel.md, the authority, names only `KONTRA_KEY` (§2.1). Decision: the
mirrors are dropped: `kontra connect` reads `KONTRA_KEY` and `HOME`; a container passes flags, and secrets as
`--enroll @file` ([`tunnel.md`](../spec/tunnel.md) §2.1; `internal/node/node.go`).

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
saying why, only without `uv`, the pinned checkout (`$KONTRA_A2A_SAMPLES` or a clone) or a free `127.0.0.1:9999`.
Only the `6603ba3` seller is built (WP-07), so D122 and `card_compat`'s stand-in for the 0.2.5 seller stay as they
are. `anon_limits`, `local_rebinding`, `stdio_roots` and the MCP tests come with M3 and M4 (`test/e2e`).
