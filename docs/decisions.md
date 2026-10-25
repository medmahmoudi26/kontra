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
