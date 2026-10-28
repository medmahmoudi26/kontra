# Relay

Status: **approved by the owner** (25 Oct 2026, revision 3 after M1 planning; revision 2 was the adversarial
review). Revision 4 brings the text in line with the M1 code (D82–D122), accepted by the owner on 26 Oct. Revision 5
brings it in line with the M2 code (D141–D143, D145–D150, D154), decided under the owner's standing instruction on
26 Oct. Revision 6 (26 Oct, before M3 builds the MCP relay) writes the 2026-07-28 revision into §11.12 as D24
requires, with the peer hop of §11.2 and §10.4's blob refs (D158–D163, D167), and was amended the same day after the
M3 plan's audit (D172–D188). Revision 7 (28 Oct) includes the M3 decisions: it brings the text in line with the M3
code and its review (D191–D205, D213, D223, D224, D230), decided under the owner's standing instruction.
Normative. The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

This document specifies how the gate relays A2A tasks and MCP requests between a caller and a service:
principals, the served card, identifiers and contexts, the task state machine and the event log, dispatch,
retries, `input-required`, cancel, timeouts, streaming and resume, push, large parts, the MCP session and
task mappings, the gate's own MCP endpoint, the hook points an operator fills, and the `TaskLifecycle`
workflow. The tunnel that carries dispatch to a node is [`tunnel.md`](tunnel.md); the three extensions
are [`ext-escrow.md`](ext-escrow.md), [`ext-sealed.md`](ext-sealed.md) and
[`ext-input-schema.md`](ext-input-schema.md). Departures from the PRD and the architecture notes are
numbered in [`../docs/decisions.md`](../docs/decisions.md) and cited here as **D*n***.

## 0. Pins

| What | Pin | Why it is pinned |
|---|---|---|
| A2A specification | v1.0.1, `3303592588e388e62e0f69f701af531d2f4e3991` (proto `specification/a2a.proto` at that tag) | The normative reference for the caller-facing protocol. Wire version string `"1.0"`. |
| a2a-go | `github.com/a2aproject/a2a-go/v2` v2.7.0, `b82362f9ff91d5d8ce3774f429cdc771d41840c6` | Server, client, `taskstore`, `eventqueue`, `workqueue`. Its behaviour on that commit is what §15 works around; re-run the probes before any bump (D7). |
| a2a-go 0.3 compat | `github.com/a2aproject/a2a-go` v0.3.15 (transitive, `a2acompat/a2av0`) | Upstream agents that still speak 0.3. |
| MCP specification | `2025-11-25`, tag `38c84e9f93ad191d9eb26d92b945d17bd0efcaf3` (clients and upstreams may negotiate `2025-06-18`; both forbid JSON-RPC batches), and `2026-07-28`, tag `5f5440bb26a62e2cf3440b92da5a667efa03b267` | The MCP revisions the gate speaks, both on the same endpoints (§11.12, D24). MCP Tasks are experimental in 2025-11-25 (§11.5). |
| MCP Tasks extension | `io.modelcontextprotocol/tasks`, `modelcontextprotocol/ext-tasks` `v0.2.2`, `5246bc3d0253c1c4b09e682f690b7e8b97362500` (`specification/2026-07-28/tasks.md`; later commits only correct two examples' `resultType`) | The durable surface for 2026-07-28 clients (§11.12). Not wire-compatible with the 2025-11-25 Tasks. |
| Temporal | server 1.32.1, Go SDK `go.temporal.io/sdk` v1.50.0 | `TaskLifecycle` (§14); the timeouts below were measured on them. |
| A2A TCK | `a2aproject/a2a-tck` `d4d6fae7887f88ef607fb461c13e3b0e1802f19b` (tag `1.0.0.alpha3` + #250) | The conformance bar; expected failures listed with reasons (D18). |

## 1. Terms

| Term | Meaning |
|---|---|
| **Gate** | The `kontra-gate` process. A2A server and MCP server toward callers; A2A client and MCP client toward services. Several replicas share one Postgres and one Temporal namespace. |
| **Node** | A machine running `kontra connect`. Hosts services over the tunnel, and hires through the gate on its localhost. |
| **Service** | One A2A agent (`kind: a2a`) or one MCP server (`kind: mcp`) behind the gate, reached through a **backend**: `tunnel` (a node) or `a2a`/`mcp` `{url}` (direct, the operator's own network). |
| **Tenant** | The owner of services and keys. A team gate has one; Cloud has one per account and one per anonymous session. |
| **Principal** | Who a request is from, after authentication (§2.3). Every principal has a tenant (possibly none) and a stable `buyer_id`. "Buyer" and "seller" are hats per request, not identities. |
| **Gate task** | The task the caller sees. Its id, context id, state, history, artifacts and metadata are the gate's. |
| **Attempt** | One placement of a gate task on one upstream: `attempts(task_id, n, …)`. A gate task has 1..`1 + task.retries` attempts. Its first dispatch, each follow-up turn and each re-attach are **dispatches** on it (§6.4, §7). |
| **Upstream task** | The task the service created for an attempt, with the service's own ids. Upstream ids never reach a caller. |
| **Session** | One tunnel connection from one node (`tunnels.session_id`). Owned by exactly one gate replica. |
| **seq** | The position of an event in a gate task's log. `seq` equals the task's version after that event (§5). |
| **Operator** | Whoever runs the gate and configures its hooks (Kontra for Cloud; a team for its own gate). |

## 2. Gate surface

### 2.1 Routes

The gate's public listener serves these routes and nothing else (closed default route, PRD §12):

| Route | What |
|---|---|
| `GET  {svc}` and `GET {svc}/.well-known/agent-card.json` | The served A2A card (§3). The first form exists because a2a-go's resolver fetches a non-root URL as the card itself (D10). |
| `GET {svc}/.well-known/agent-card.upstream.json` | The upstream card, verbatim (§3). |
| `GET {svc}/.well-known/seller-jwks.json` | The public JWK Set of the seller keys the gate verified the upstream card with, which `kontra.upstream.jwks` names (§3, D223); `404` when the gate verified none, `503` when the card is unavailable. |
| `POST {svc}` and `POST {svc}/` | A2A JSON-RPC 2.0, decoded strictly (§2.4). The trailing-slash form is what the TCK posts (D18). |
| `{svc}/message:send`, `{svc}/message:stream`, `{svc}/tasks…`, `{svc}/extendedAgentCard` | A2A HTTP+JSON, every route of spec v1.0.1 §5.3 / `a2a.proto`. `:subscribe` accepts both `GET` and `POST` (spec prose says POST, the proto says GET; the TCK uses POST). The gate's path already names the service, so its interfaces carry no A2A `tenant`: the `/{tenant}/` additional bindings are not served, and a JSON-RPC request with a non-empty `tenant` is answered as an unknown service. |
| `POST\|GET\|DELETE /mcp/{svc}` | MCP Streamable HTTP for a hosted MCP server (§11). |
| `POST\|GET\|DELETE /mcp` | The gate's own MCP server (§12). |
| `GET /v1/tunnel` | WebSocket upgrade for nodes ([`tunnel.md`](tunnel.md)). |
| `GET /.well-known/jwks.json` | The gate's card-signing public keys. |
| `GET /.well-known/oauth-protected-resource/mcp[/{svc}]` | RFC 9728 metadata for the MCP endpoints, when the operator configures an authorization server (§12.1). |
| `GET /healthz` | `200` when this replica's Postgres and Temporal connections are up and it is not draining; `503` otherwise. |

`{svc}` is `/agents/{name}` on a single-tenant gate and `/agents/{tenant}/{name}` on a multi-tenant gate
(Cloud). `name` and `tenant` match `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`. The MCP routes use the same
names: `/mcp/{name}` or `/mcp/{tenant}/{name}`.

**Unknown service.** A path naming no service, a service of the other `kind`, a disabled service, or a
service whose `allow:` does not admit the principal (§2.3) gets the same answer: HTTP `404`, with a body
that fits the route (a JSON-RPC error `{"code": -32601, "message": "unknown service"}` on JSON-RPC and
MCP POSTs, `google.rpc.Status` `NOT_FOUND` on HTTP+JSON, empty on card GETs). This holds for every route
of the service, cards and the upstream card included, unless the service's `allow:` contains `anonymous`
or the service is `listed` (§12.2), whose cards are public. `allow:` lists therefore never reveal which
services exist (D50). The credential is checked before the service is looked up, so a bad one is `401` on every
route whether or not the service exists (D87).

Metrics live on a separate listener (`:9090/metrics` by default), never on the public one.

### 2.2 Edge: limits, TLS and the client address

Applied before authentication, per replica. Defaults are the PRD's (§11 `limits:`): body 4 MiB,
`inflight_per_key` 32, `rps_per_ip` 50, header timeout 10 s, idle 60 s. A request over a limit is refused
before any task row exists: `413` for the body, `429` with `Retry-After` for rates and in-flight caps.
On JSON-RPC routes the error is also a JSON-RPC error object (`-32600`, `message: "request too large"` or
`"rate limited"`), with the request's `id` when its body was read and `null` otherwise (D84). A body that has
not arrived within 30 s is refused `408` before the handler runs (D85). SSE and long-poll responses do not count
against the idle timeout; they send a comment line (`: keep-alive`) every 15 s.

**Connection caps** (PRD §12) are enforced by the listener: `limits.conns_per_ip` (default 256) per client
address and `limits.max_conns` (default 10,000) in all (D73). Connections from a trusted proxy address
(`edge.client_ip.trusted`, below) are exempt from `conns_per_ip`, because behind an `x_forwarded_for` proxy every
connection carries the proxy's address; `max_conns` still applies to them. With `proxy_protocol` each
connection's client address is known (its PROXY header), and `conns_per_ip` applies to that address.

**TLS.** The gate terminates TLS itself, with ACME or a provided certificate, and refuses plaintext unless
`public_url` is a localhost origin (PRD §11). Built-in ACME (autocert, validated with TLS-ALPN-01 on the TLS
listener, so `listen` is `:443` or forwarded from it; HTTP-01 waits for a key that opens a port-80 listener, D82)
is for a single replica with a directory cache: replicas that do not share a cache each order a certificate, and a challenge can
land on a replica that does not hold its token. Several replicas use provided certificates, or a front proxy,
until a later migration adds a shared certificate cache, `kontra.acme_cache`, with issuance serialized by an
advisory lock (D70).

**Behind a TLS-terminating front proxy**, `tls.terminated_by_proxy: true` lets the gate listen in plaintext with
an `https` `public_url`. It then leaves HSTS to the proxy, its tunnel channel binding is `none`
([`tunnel.md`](tunnel.md) §4), and it logs a warning at start. Without the key, the gate still refuses plaintext
off localhost (D69).

**Client address.** `rps_per_ip` and `conns_per_ip` count by the client address (an IPv6 address by its /64,
D83), which `edge.client_ip: {from, trusted}` names (D69):

- `remote` (the default): the connection's remote address.
- `x_forwarded_for`: honoured only on a connection from a `trusted` CIDR, where the client address is the
  rightmost `X-Forwarded-For` hop that is not in `trusted`; on any other connection, the remote address.
- `proxy_protocol`: a PROXY protocol header, v1 or v2, honoured only on a connection from a `trusted` CIDR; its
  source address is the client address.

Behind a proxy, `remote` is the proxy's one address, and `rps_per_ip` would throttle every caller together.

### 2.3 Authentication and principals

| Credential | Principal | Tenant | `buyer_id` | `via` |
|---|---|---|---|---|
| `Authorization: Bearer <api key>` (stored as sha256, PRD §12) | `key:<prefix>`, the prefix unique per gate (whoever mints a key mints again on a collision) | the key's | the key's `buyer_id` (several keys may share one) | `api_key` |
| `Authorization: Bearer <JWT>` from the configured OIDC issuer (A2A routes; audience `auth.oidc.audience`) | `oidc:sub:<sub>` | none, unless the operator maps it | UUIDv5(issuer ‖ sub) | `oidc` |
| `Authorization: Bearer <JWT>` on `/mcp` or `/mcp/{svc}` from the configured MCP authorization server, audience = that route's canonical URI (§12.1) | `oidc:sub:<sub>` | as above | as above | `mcp` |
| A request on a stream the node opened ([`tunnel.md`](tunnel.md) §9) | the API key from the handshake's `auth` when present, else `node:<key_id>` | the key's | the API key's, else UUIDv5(gate namespace ‖ key_id) | `api_key`, else `node` |
| Nothing, on a service whose `allow:` contains `anonymous` | `anonymous` | none | team gate: one fixed `buyer_id` for all anonymous callers; multi-tenant gate: UUIDv5(service tenant ‖ "anonymous") | `anonymous` |

`allow:` entries (deny by default, PRD §12): `key:<prefix glob>`, `org:<tenant>` (every principal whose
tenant is that tenant: its API keys and its nodes' keys), `oidc:sub:<subject>`, `node:<key_id>`,
`anonymous`. `anonymous` is never implied; an `anonymous` entry admits every principal, authenticated or not,
and an authenticated caller keeps its own principal, `buyer_id` and `via`. A service the gate creates during a
handshake gets `allow: [org:<its tenant>]` ([`tunnel.md`](tunnel.md) §5–§6), so its owner's own nodes can call it.

**Anonymous callers share a buyer.** All of them on a team gate, and all of one tenant's on a
multi-tenant gate, are one `buyer_id`. So for the `anonymous` principal `ListTasks` and MCP `tasks/list`
are refused (`UnsupportedOperationError` / `-32601`), push configs are refused, and a task is readable
and cancellable by whoever holds its id: ids are not secrets (a UUIDv7 discloses its creation time and
has 62 random bits), so this is a property of opting in to `anonymous`, recorded as such (D20). The TCK
target is the case for it (D18). An MCP service's `max_sessions_per_principal` does not apply to `anonymous`: one
budget for callers the gate cannot tell apart would let any of them crowd out, or close, the others' sessions
(§11.2, D231).

**API key scopes.** `hire` (the default: call services), `enroll:<service>` and `enroll:*` (enroll node
keys, [`tunnel.md`](tunnel.md) §5). A key's scopes never grant an operator action.

**API key format.** A key the gate mints is always `kk_<operator prefix>_<random>`. The operator prefix is
1–16 characters `[a-z0-9]`, chosen when the key is minted; when `kontra-gate enroll --api-key <name>` is given no
`--prefix`, the prefix is the key's name lowercased, reduced to `[a-z0-9]` and cut to 16 characters, or `k` if
that leaves nothing. The random part is 32 base62 characters. The stored prefix (`api_keys.prefix`, so the
principal `key:<prefix>`) is the key up to the random part plus the random part's first 8 characters: at most
3 + 16 + 1 + 8 = 28 characters, within the column's 32. The other 24 characters are never shown. A glob names the
keys of one operator prefix as `key:kk_acme_*` (PRD §11's `key:acme-*` cannot match: a prefix has no `-`, D68).
Keys an operator writes itself (Cloud) may use any prefix the column accepts (D37).

**Revocation is checked on every request.** A replica keeps a revocation cache of API keys, node keys and
key–service bindings, refreshed by `NOTIFY kontra_revoke` and re-read from Postgres (what was revoked since the
last read) at least every 5 s and whenever its `LISTEN` connection reconnects; NOTIFY only shortens the delay. A
revoked or expired API key is `401` on its next request, including every later stream a node opens with it
([`tunnel.md`](tunnel.md) §12); the node's hosting is unaffected.

### 2.4 Protocol version and extensions (A2A)

Applied by HTTP middleware in front of a2a-go's transports, and by the gate's own SSE writer (D9):

- **`A2A-Version`.** The version is the `A2A-Version` header, else the `A2A-Version` query parameter (spec
  §3.6.1; a2a-go accepts it too). `1.0` and `1.0.<patch>` are accepted and normalized to `1.0` (patch
  numbers MUST NOT affect negotiation). Both absent or empty means 0.3 (spec §3.6.2), refused with
  `VersionNotSupportedError` (`-32009`, HTTP `400`), whose message names the header to send. The gate
  serves 1.0 only downstream (PRD §18).
- **`A2A-Extensions`.** Accepted as one comma-separated header, several headers, or both; split, trimmed
  and deduplicated. `X-A2A-Extensions` (0.3) is accepted as an alias. URIs the served card does not
  declare are ignored. A request missing a URI the card marks `required: true` is refused with
  `ExtensionSupportRequiredError` (`-32008`) before any task row exists. The response carries the
  activated URIs in one comma-separated `A2A-Extensions` header.
- **Strict decoding of JSON-RPC (D172).** The gate reads a JSON-RPC body itself (the method that routes a stream,
  `params.tenant`, the `id`) before a2a-go decodes it again, so the two must never see different members. The body
  is decoded with exact, case-sensitive keys first. A request in which a key repeats, or two keys are equal under
  Unicode case folding, at a level the gate or a2a-go reads (the request object, `params`, `params.metadata`,
  `params.configuration`, `params.message`, `params.message.metadata`, each of `params.message.parts`, and each
  part's `metadata`) is refused with `InvalidRequestError` (`-32600`, HTTP `400`) after the credential and service
  checks of §2.1 and before `params.tenant` is read, the version and extension checks, Admission and any task row.
  HTTP+JSON bodies are decoded by a2a-go alone, and its decoded values are all the gate reads and forwards, so they
  are not checked.

Where this spec gives an HTTP status beside a JSON-RPC error code (here, §7, §8, §14.1), the status is the
HTTP+JSON binding's, as A2A §3.3.2 maps them. On JSON-RPC the error is the body of an HTTP `200`, the only
response a2a-go's JSON-RPC client reads an error from; the exceptions are the unknown-service `404` (§2.1), a
refused credential's `401` (D86) and a body that strict decoding refuses (`400`, D172).

### 2.5 Kontra's own keys are the gate's

Kontra's metadata keys (A2A `kontra.*`) and `_meta` keys (MCP `sh.kontra/*`) state facts only the gate or
the operator may assert: approve URLs, escrow state, failure reasons, prices, sealing recipients. So:

- **From callers**, the gate removes every `kontra.`-prefixed key from message, part and request metadata
  before anything is logged or forwarded, except `kontra.sealed` and `kontra.sealed.replyTo` (sealing,
  [`ext-sealed.md`](ext-sealed.md)) and the keys the operator lists in `caller_metadata` (Cloud lists
  `kontra.escrow.dispute`, which its hook reads, §13.3). On MCP it removes every `sh.kontra/`-prefixed key from
  the `params._meta` of requests and notifications and the `result._meta` of responses, except the sealed
  envelope below.
- **From services**, the gate removes every `kontra.`-prefixed key from the metadata of every upstream `Task`,
  status update, artifact update, `Message`, `Artifact` and `Part`, except `kontra.sealed` on a part whose
  `mediaType` is the sealed envelope type. On MCP it removes every `sh.kontra/`-prefixed key from protocol-level
  locations only: a result's `_meta`, `params._meta` of upstream notifications and requests, each `tools[]._meta`
  of `tools/list`, the `InitializeResult`'s and the `server/discover` result's `_meta`, task objects' `_meta` (and
  `inputRequests` entries' `params._meta`, §11.12), `capabilities.experimental["sh.kontra/sealed"]` and every
  `sh.kontra/` key of `capabilities.extensions`, except the sealed envelope below. It never descends into
  `content`, `structuredContent` or `arguments`, which may be sealed.
- **The sealed envelope (D174).** On a service with `sealed` set, and on 2025-11-25 and 2025-06-18 messages only
  (D162), `_meta["sh.kontra/sealed"]` is kept exactly where [`ext-sealed.md`](ext-sealed.md) §3 puts it: in the
  `params._meta` of a caller's `tools/call`, in the `params._meta` of an upstream `elicitation/create` or
  `sampling/createMessage`, and in the `result._meta` of every response from the caller (a JSON-RPC response does
  not name the request it answers, which may have reached the client through another replica's stream; the hosting
  node checks the answer, [`ext-sealed.md`](ext-sealed.md) §5 rule 4b, D200), and there only when its value is
  exactly `{"v": 1, "envelope": "<base64>"}`. Every other `sh.kontra/` key in those objects, and
  `sh.kontra/sealed` anywhere else or on any other service, is removed as above.
- Then it writes its own (sealed capability, approve URL, failure reason, an `initialize` result's
  `sh.kontra/session_idle_s`, §11.2) and the operator's (§13.4). An operator key replaces a same-named key; it is
  never merged with it, except `sh.kontra/session_idle_s`, which is the gate's and replaces the operator's (D224).

The gate itself names no escrow key: the escrow keys belong to the operator and its extension spec (D51).

## 3. The served A2A card

The gate fetches the service's card through the backend (`GET /.well-known/agent-card.json`, then
`/.well-known/agent.json` for 0.2.x agents; for a direct backend, relative to the path of its `url`, §6.2) when
a session for the service connects or the direct backend is configured, and again every 10 minutes while
serving, at most 256 KiB (§6.6). The upstream card is kept verbatim (`services.upstream_card`); the served card
is produced from it as raw JSON and kept as `json` (`services.card`), never `jsonb`, which refuses U+0000 and
reorders keys (D49). The gate never round-trips a card through a typed struct, because that drops fields it
does not know (D11).

**A tunnel service** has one card: its nodes are expected to serve the same one, and the last good fetch through
any of its sessions is the service's card (D142). A session whose card fetch fails (its node started before its
agent) still becomes a candidate; its card is fetched again with backoff, 1 s doubling to 1 min, and at once when
a task is dispatched without it. A tunnel service none of whose nodes has connected for 30 minutes is unavailable
until a session connects, and a task dispatched while the service has no card fetched in the last 30 minutes is
retried like `no_session` (§6.4, D143).

**Normalization.** A 1.0 card is used as is, unless it also carries 0.x members (`url`, `preferredTransport`,
`additionalInterfaces`, `protocolVersion`, `security`, `supportsAuthenticatedExtendedCard`, a skill's
`security`): for those it is normalized like a pre-1.0 card, the interface members dropped and the rest
converted where the card has no 1.0 member of its own, dropped otherwise (D88). A pre-1.0 card is converted to the 1.0 shape as raw JSON:
top-level `url`, `preferredTransport` (default `JSONRPC`, which is what a 0.2.x card without it means) and
`additionalInterfaces` become `supportedInterfaces` (replaced below anyway), the 0.x field names are
renamed per the spec's migration table, and every field the gate does not know is kept (D11, D12).
a2a-go's `a2av0` parser is used only to read what the gate needs to speak to the upstream (its protocol
version, binding, the path of its URL and capabilities; never a host or port, §6.2), never to produce the served
card.

**Rewrite.** Every seller-authored field is kept byte-for-byte except these, which the gate sets (PRD §5's
five fields with the 1.0 names, plus two it must also own, D52):

| Field | Gate sets it to |
|---|---|
| `supportedInterfaces` | `[{url: {public_url}{svc}, protocolBinding: "JSONRPC", protocolVersion: "1.0"}, {url: {public_url}{svc}, protocolBinding: "HTTP+JSON", protocolVersion: "1.0"}]`. Never an upstream URL. |
| `securitySchemes`, `securityRequirements` | `{"bearer": {"httpAuthSecurityScheme": {"scheme": "Bearer"}}}` and `[{"schemes": {"bearer": {"list": []}}}]`; plus an `openIdConnect` scheme when the operator configures an IdP. Omitted when the service's `allow:` is exactly `[anonymous]`. |
| `capabilities.pushNotifications` | `true`: the gate implements push itself (§10). |
| `capabilities.streaming` | `true`: the gate streams from its own log whatever the upstream does. |
| `capabilities.extendedAgentCard` | `false` (PRD §17, default). `GetExtendedAgentCard` answers `UnsupportedOperationError`. |
| `capabilities.extensions` | The seller's list, except that every entry whose URI the gate or the operator asserts — `https://kontra.sh/ext/sealed/v1` for a sealed service, and every URI in the operator's `card_extensions` (§13.4) — replaces a seller entry with the same URI. A seller's `input-schema/v1` entry passes unchanged. |
| `signatures` | Exactly one: the gate's JWS (below). |

**The gate's signature.** ES256 over RFC 8785 JCS of the served card without `signatures`; protected header `{alg:
"ES256", kid, typ: "JOSE", jku: {public_url}/.well-known/jwks.json, "kontra.upstream": {sha256, url, verified,
jwks}}`, where `sha256` is the hex SHA-256 over the JCS of the upstream card, `url` is
`{public_url}{svc}/.well-known/agent-card.upstream.json`, `verified` says whether the gate verified the seller's
signature (below), and `jwks`, present only when it did, is `{public_url}{svc}/.well-known/seller-jwks.json`, where
the gate serves the public keys of the `seller_keys` it verified with (D223). `kontra.upstream` is never listed in
`crit`, so stock verifiers (a2a-go's included) accept the card. The gate signs with its own JWS code over a vetted JCS
library, because a2a-go's signer cannot add a header parameter (D13). JCS is defined for valid Unicode only: an
upstream card that is not valid UTF-8, or holds a lone surrogate, is not installed, and the service has no card until
its seller publishes one; it is never served unsigned or altered (D193).

**The seller's signature (D13).** A signature over the upstream card does not verify over the rewritten
card (spec §8.4.3), so it is not copied into `signatures`; the upstream card is served verbatim instead.

- The gate verifies seller signatures **only against keys the operator configured for the service**
  (`services.<name>.seller_keys`: an inline JWKS, or an `https` URL fetched with the §10.3 dialer). It
  never fetches a `jku` taken from a card. With keys configured, a card none of whose signatures verifies
  makes the service **unavailable** (PRD §5): its card routes answer `503` with an empty body, and a new task is
  still created and fails `service_unavailable` at its first attempt (§6.4), so the caller gets a task carrying
  the reason. A failed fetch keeps the last verified card; the service becomes unavailable only when no fetch has
  succeeded for 30 minutes, on any replica (D89). Without keys, the gate does not verify and says so: `verified: false`. MCP services
  have no card and are never unavailable this way.
- A verifier (`kontra connect` does this) checks the gate's signature with a `jku` equal to the origin it
  dialed, fetches the upstream card, checks the hash, and checks the seller's signatures against keys it
  trusts for the seller. A gate that strips the seller's signatures cannot hide it from a verifier that
  pinned the seller's keys ([`ext-sealed.md`](ext-sealed.md) §7). `kontra connect` holds no seller key set of its
  own: it verifies a signature that carries its key (`jwk`) and pins that key's thumbprint, and verifies one that
  names its key by `kid` only with the key set `kontra.upstream.jwks` names, fetched from the gate's origin (through
  its tunnel), and pins that key's thumbprint; with no `jwks` named, such a signature is pinned unverified (D223).

Caching: the card responses carry `Cache-Control: max-age=60`, an `ETag` (the hash of the served body) and
`Last-Modified` (the TCK hard-fails without them).

The gate does not enable a2a-go's input-mode enforcer. The relay does not interpret parts; the upstream
enforces its own modes (D14).

## 4. Identifiers, contexts and attempts

- **Task ids** are minted by the gate, UUIDv7. A caller can never choose one (spec §3.4.1). A message naming
  an unknown `taskId`, a task the principal may not see, or anything that does not parse as a UUID (checked
  before any query) is `TaskNotFoundError` (`-32001`; MCP `-32602`).
- **Context ids.** A `contextId` supplied by the caller on a message without `taskId` is kept verbatim (spec
  §3.4.1: preserve or reject, never replace). Contexts are scoped by `(tenant_id, service, buyer_id,
  context_id)`: two buyers using the same string never share a context. Without one, the gate mints a
  UUIDv7.
- **Upstream contexts.** The gate never sends a caller-supplied or gate-minted context id upstream. The
  first task of a gate context is sent without `contextId`; the context id the upstream answers with is
  recorded on the attempt. A later task in the same gate context carries that recorded id only when it is
  dispatched to the same tunnel session or the same direct backend; otherwise it carries none. So two
  buyers' identical strings never meet at a stateful seller either (D3, D53).
- **Referenced tasks.** A caller's `referenceTaskIds` are replaced by the upstream ids of the caller's own
  tasks in the same gate context and backend, and dropped otherwise. In both bullets, an attempt is on the same
  direct backend only if it started after the service row last changed (`services.updated_at`): 0001 records no
  direct backend's identity (D92).
- **Message ids** are passed upstream unchanged (the TCK keys seller behaviour on them).
- **Attempts.** Each attempt is a row `attempts(task_id, n, backend, tunnel_session, key_id, replica_id,
  dispatch_id, upstream_task_id, upstream_context_id, upstream_session, started_at, first_upstream_id_at,
  ended_at, outcome)`. `n` starts at 1. Every dispatch on the attempt (§14.2) writes `replica_id` (where it runs,
  which routes `abort` and `forward_cancel`) and a fresh `dispatch_id` (its fence, §6.4) before any upstream
  call, for both backends. `ended_at` and `outcome` are written only when the attempt ends (a terminal outcome, a
  failure that is retried, `aborted`, `no_session`, …), never when a turn stops at `input-required` (nor at
  `auth-required`, which ends no turn, D54): follow-up turns run on the same attempt (§7). The outcome
  `interrupted` is written when the task ends (timeout, cancel) while its attempt waits at `input-required`
  (D77). The **first upstream task id** of an attempt is the first task id the upstream returns for it (a `Task`,
  or any status or artifact event). Its arrival sets `first_upstream_id_at` before the event that carried it is
  logged.
- **Rewriting.** Before an upstream event is logged, every upstream task id and context id in it is replaced
  with the gate's, at every depth (`task.id`, `task.contextId`, `status.message.taskId`, `history[*].taskId`,
  `history[*].contextId`). An upstream event whose task id differs from the attempt's first upstream task id
  fails the attempt `upstream_error`; it is never rewritten into the gate task. Artifact ids are task-scoped
  and pass unchanged. In the other direction, a follow-up's `taskId`/`contextId` are replaced with the sticky
  attempt's upstream ids (§7).

## 5. States, the projection and the event log

### 5.1 States

`tasks.state` holds one vocabulary for both protocols:

| `tasks.state` | A2A wire (`TaskState`) | MCP wire (`Task.status`) | Class |
|---|---|---|---|
| `quoted` | `TASK_STATE_INPUT_REQUIRED` with `metadata["kontra.approve.url"]` (README, 25 Oct) | `working`, with the approve URL in `statusMessage` and `_meta["sh.kontra/approve-url"]` (D21) | pre-hold; only an Admission that returns a quote creates it (§13.2) |
| `submitted` | `TASK_STATE_SUBMITTED` | `working` | active |
| `working` | `TASK_STATE_WORKING` | `working` | active |
| `auth-required` | `TASK_STATE_AUTH_REQUIRED` | `working` (statusMessage says what the agent waits for) | active: not a turn boundary (D54) |
| `input-required` | `TASK_STATE_INPUT_REQUIRED` | `input_required`, or `working` (§12.4) | interrupted: a turn boundary |
| `completed` | `TASK_STATE_COMPLETED` | `completed` | terminal |
| `failed` | `TASK_STATE_FAILED` | `failed` | terminal |
| `canceled` | `TASK_STATE_CANCELED` | `cancelled` | terminal |
| `rejected` | `TASK_STATE_REJECTED` | `failed` | terminal |

`tasks.protocol` (`a2a` | `mcp`) is the protocol dispatch speaks upstream. A task is rendered in the
vocabulary of the endpoint that reads it: an A2A route, an MCP route, or the gate's own `/mcp` (a task
created by `hire` is an A2A task followed over MCP, §12).

**After a terminal event** a task's state, status and artifacts never change again; its history grows only
by an accepted review-window message (§7), and its metadata only by `metadata` rows (operator patches: hook
results, annotations). Every other write to a terminal task —
status, artifact, result, input — is refused by the store; that is the rule that drops late upstream events
after a cancel (§8).

### 5.2 One transaction per event

The gate's Postgres store implements a2a-go's `taskstore.Store` (PRD §10, as corrected). Every change to a
task is one transaction:

1. ```sql
   UPDATE tasks SET snapshot = $task, state = $state, status_at = $status_timestamp, version = version + 1,
          updated_at = now(),
          submitted_at    = CASE WHEN $state = 'submitted' AND submitted_at IS NULL THEN now() ELSE submitted_at END,
          deadline        = CASE WHEN $state = 'submitted' AND deadline IS NULL THEN now() + $timeout ELSE deadline END,
          terminal_at     = CASE WHEN $state IN (<terminal>) AND terminal_at IS NULL THEN now() ELSE terminal_at END,
          retention_until = CASE WHEN $state IN (<terminal>) AND retention_until IS NULL THEN now() + $retention ELSE retention_until END
    WHERE id = $id AND version = $prev
      AND (state NOT IN (<terminal>) OR ($kind IN ('metadata', 'message') AND $state = state AND $after_terminal_ok))
   RETURNING version, deadline
   ```
   Zero rows (the version moved, or the write is refused by the terminal rule) is
   `taskstore.ErrConcurrentModification`. a2a-go then reloads the task, and a terminal stored task ends the
   execution, which is how a cancel that won on one replica stops a relay on another. A stale version is an
   error, **never a merge** (PRD §12). The store also refuses `PrevVersion == 0` as a conflict: a2a-go's
   in-memory store treats 0 as "skip the check", and the gate's store never writes unchecked. `status_at` is
   the logged `status.timestamp` truncated to microseconds, which `ListTasks` orders by (spec §3.1.4); no
   index names `updated_at`, so artifact-only updates stay cheap (D49). `$after_terminal_ok` is set only by
   the two writers allowed after a terminal event: `apply_metadata` and the accepted review-window message
   (§5.1). `deadline` is written only here, at the transition into `submitted`, and returned to whoever wrote it
   (the workflow sets its timeout timer to that stored value, §14.1).
2. `INSERT INTO task_events (task_id, seq, kind, payload, payload_ref) VALUES ($id, <the returned version>, …)`.
3. `SELECT pg_notify('kontra_task_events', '<task_id>:<seq>')`.

An append made by a dispatch also checks its attempt in the same transaction: the task's latest, not ended, and
still holding that dispatch's `dispatch_id` (the fence of §6.4, D77).

`Create` is the same with `INSERT INTO tasks … version = 1` and `seq = 1`, kind `task`. Writes run on a
context detached from the request's cancellation (`context.WithoutCancel`, D8), so a terminal event is never
lost because the request that caused it ended.

`task_events.kind` is one of `task` (the creation snapshot), `status`, `artifact`, `message` (a caller
message appended to history: a follow-up or a review-window message), `metadata` (a metadata-only change),
and for MCP tasks `input_request`, `input_response` and `result` (§11.5). `payload` is the A2A
`StreamResponse` for that event with gate ids (for `metadata`, `{"metadata": {…}}`), or the MCP equivalent.
`snapshot` and `payload` are `json`, not `jsonb`, so a NUL character or a duplicate key in relayed content is
kept, not refused or rewritten (D49). `task_events` is append-only (a trigger refuses updates; rows go only
at retention, §5.4).

One `LISTEN kontra_task_events` connection per replica wakes in-process subscribers; the notify payload is a
hint, the table is the truth. Every NOTIFY-ing commit in a Postgres cluster is serialized by Postgres; M1
measures that ceiling on the target database, and runs of artifact-append chunks may be coalesced into one
event if it binds.

### 5.3 What is logged and what is not

The event log holds what a caller may read: gate ids, states, messages, artifacts, metadata. It never holds
credentials or request headers. The work queue's payload is not in Temporal: it is stored by reference
(§8.1, D6). Push configs' credentials are in `push_configs.config`, which no operator read includes (§13.5).

### 5.4 Retention

Every terminal task, with its `task_events`, `attempts`, `work_items`, `push_configs` and bucket objects, is
deleted at `retention_until` (`services.<name>.task.retention`, default 7 days after the terminal event; an
operator's review window and dispute period must fit inside it). A sweep on any replica does it every 10
minutes. `mcp_sessions` closed for longer than the retention go the same way (a session of the gate's own
`/mcp`, which names no service, after the default 7 days, D95); `calls` rows are the
operator's to delete (§13.5). Parts sealed to a key are only as safe as that key ([`tunnel.md`](tunnel.md)
§15): retention is what bounds the exposure of a stolen key.

## 6. Dispatch

### 6.1 Session choice and the enrollment check

A new task's first attempt picks a backend:

- **Direct** (`a2a: {url}` / `mcp: {url}`): the configured URL, through the gate's SSRF-safe dialer (§10.3),
  which connects only to that URL's `scheme://host:port` (§6.2, D71).
- **Tunnel**: a connected session of a node whose key is enrolled for this service. The candidates are

  ```sql
  SELECT t.session_id, t.replica_id FROM tunnels t
    JOIN tunnel_keys k   ON k.key_id = t.key_id AND k.revoked_at IS NULL AND k.revoked_reason IS DISTINCT FROM 'rotated'
    JOIN key_services ks ON ks.key_id = t.key_id AND ks.service = $service AND ks.revoked_at IS NULL
    JOIN gate_replicas r ON r.replica_id = t.replica_id AND r.last_seen_at > now() - interval '30 seconds' AND r.draining_at IS NULL
   WHERE t.tenant_id = $tenant AND $service = ANY (t.services)
  ```

  Among them, round-robin, per replica; no replica is skipped for its load, which its `kontra-tunnel-<id>` worker
  bounds (§14.2, D145). For a new task in a known context, the session of the context's previous task is preferred
  while it is connected (best effort, PRD §7). No candidate: the attempt fails `no_session` before any upstream
  task exists, which is retryable (§6.4).

**The service comes from the key, never from the request.** The candidate set is computed from the
enrollment tables only. And **before opening any stream to a node** — a dispatch, a follow-up, a poll, an
`abort`, a `forward_cancel`, a card fetch, every request on a pinned MCP session — the replica holding the
session checks its local registry, kept current by the revocation cache (§2.3): the session is open, its key
is neither revoked nor past a rotation's overlap, and the service is still in the session's served set. If
not, the request is handled as a lost session and no stream is opened (`key_scope`): A2A `tunnel_lost`; MCP, the
pinned row is closed `revoked` when the key or binding is revoked or the key is past its rotation overlap,
`tunnel_lost` when the tunnel session is gone, and the client gets `404`. The stream preamble names the service
([`tunnel.md`](tunnel.md) §8); the node refuses one naming a service it does not host.

### 6.2 The upstream call

The dispatch activity runs on the replica that owns the chosen session (queue `kontra-tunnel-<replica>`) or
on any replica for direct backends (queue `kontra-direct`), and makes the upstream call with a2a-go's client
over an `http.Client` whose dialer opens a tunnel stream (or the SSRF-safe dialer). Rules:

- **Endpoint (direct backends).** An `a2a: {url}` backend's `url` is a base, not the A2A endpoint: its card is
  fetched relative to the URL's path (§3). The gate calls the configured `scheme://host:port` with the path,
  binding and protocol version of the upstream card's interface, whatever host or port the card's interface URL
  names. The backend's dialer refuses any other `host:port`, so a seller-controlled card never steers it (D71).
- **Headers sent upstream:** `A2A-Version` for the upstream's protocol version; `A2A-Extensions` with the
  activated URIs that the upstream card declares; `Content-Type`/`Accept` of the binding. Never the caller's
  `Authorization`, cookies or other headers, and never a gate credential.
- **Streaming first.** If the upstream card says `capabilities.streaming: true`, the activity calls
  `SendStreamingMessage`. If that is refused with `UnsupportedOperationError` (or the 0.3 equivalent) before
  any event, it falls back in the same attempt to the polling path (`a2a server --exec` advertises streaming
  and refuses it without `--chunk`, D15). A stream the upstream ends cleanly before a terminal or
  `input-required` event is followed by polling once an upstream task id exists, and fails `upstream_error`
  before one (D96).
- **Polling path.** `SendMessage` with `configuration.returnImmediately: true`, so an upstream task id exists
  early and a cancel has something to cancel; then `GetTask` with `historyLength: 0` every 1 s, backing off
  to 5 s after a minute without change. An upstream that ignores `returnImmediately` and blocks is waited on;
  its task id then arrives with the result.
- **Through interrupted states.** The dispatch follows the upstream through `auth-required` (which is not a
  turn boundary: the agent waits for credentials that arrive out of band, A2A §7.6.1) and ends its turn only
  on a terminal state or `input-required` (D54).
- **Heartbeat.** The activity heartbeats every 5 s; heartbeat timeout 30 s (§14.2).

### 6.3 Translating upstream events

| Upstream returns | The gate logs |
|---|---|
| A `Task` (first time in this attempt) | Its status as a `status` event and each artifact as an `artifact` event, ids rewritten (§4) and Kontra's keys removed (§2.5). Never a second `Task` event: the gate task already exists (it was created at admission). |
| A `Task` (later, from polling, or the first one after a re-attach, §6.4) | The difference from the gate's projection: a `status` event if the status changed, an `artifact` event per new or grown artifact (`a2aevent.Recover` semantics). Nothing already in the log is logged twice. |
| `statusUpdate` | `status`, ids rewritten. |
| `artifactUpdate` | `artifact`, ids rewritten. A first chunk with `append: true` for an artifact the gate has not seen is logged with `append: false`; an empty artifact is dropped. |
| A `Message` (no task) | A terminal `status` event: `completed`, with the upstream message (ids rewritten, `taskId` set to the gate task) as `status.message` (D5). The caller always gets a task. |
| A JSON-RPC/HTTP error **before** the first upstream task id | Depends on the error: see §6.4. |
| A JSON-RPC/HTTP error **after** it | A terminal `status` event `failed` with a status message carrying the upstream error text. |
| `rejected` / `failed` status | Logged as is; terminal; no retry. An upstream terminal status without a message gets a gate-written agent message naming the upstream state, so every relayed failure carries a message. |

Every terminal outcome is an explicit event written by the relay or, when no relay can run, by the workflow
(`write_terminal`, §14.2). The relay never relies on an executor error to fail a task, and the store refuses
the failures a2a-go synthesizes on its own error paths (§15, D8).

### 6.4 Retries

`task.retries` (default 2) bounds **new attempts**, each a fresh upstream `SendMessage` with the caller's
original message. A retry is only ever made **before the first upstream task id** of the current attempt
exists. After it, the agent's state is the agent's (PRD §7): a lost tunnel fails the task, and a direct
backend is re-attached to, never sent the message again. Retries are the workflow's, never Temporal's:
Temporal retries an activity on its original queue, which for a dead replica is a queue no one polls (§14.2).

| Failure | No upstream task id yet | Upstream task id exists |
|---|---|---|
| No candidate session (`no_session`) | retry after 1 s, 2 s, 4 s … (capped at the deadline), each retry a new attempt counted against `task.retries`; `failed: no_session` when they are spent (D146) | — |
| Tunnel stream could not be opened, session closed or revoked, or replica lost (`tunnel_lost`) | retry on another session if one exists, else the same rules as `no_session` | `failed: tunnel_lost`, no retry |
| A response the node wrote itself (`Kontra-Node` other than `sealed_rejected`, [`tunnel.md`](tunnel.md) §8.2) | retry on another session, as `tunnel_lost` | `full`: the exchange (a poll, a follow-up, a stream) is made again with the poll's backoff for at most 30 s, then `failed: tunnel_lost` (D147); any other: `failed: tunnel_lost` |
| Dispatch heartbeat timeout, **tunnel** backend (the replica holding the socket died; ≤ 30 s) | retry | `failed: tunnel_lost`, no retry |
| Dispatch heartbeat timeout or lost context, **direct** backend (a gate replica died; the upstream is untouched) | retry; `failed: upstream_unreachable` when exhausted (D99) | **re-attach** (below; D42) |
| Dispatch not started within its schedule-to-start (no replica polled its queue) | as a lost dispatch, above | direct: the same turn's dispatch again on the same attempt, as it was (a follow-up stays the turn's first dispatch, a resume a resume), backing off and capped with re-attaches, `failed: upstream_unreachable` at the cap; tunnel: `failed: tunnel_lost` (D100) |
| Connection refused / reset / TLS error to a direct URL, HTTP 502/503/504 from upstream | retry | `failed: upstream_unreachable`, no retry |
| Upstream protocol error (JSON-RPC error, HTTP 4xx other than 408/429, malformed body) | `failed: upstream_error` with the upstream message, no retry | `failed: upstream_error` with the upstream message |
| HTTP 408 / 429 | retry, honouring `Retry-After` | `failed: upstream_error`, no retry |
| An upstream limit exceeded (§6.6) | `failed: upstream_error` | `failed: upstream_error` |
| Card unavailable (signature fails, fetch fails) | `failed: service_unavailable`, no retry; a tunnel service with no card fetched in the last 30 minutes: retried like `no_session`, `failed: service_unavailable` when spent (§3, D143) | — |
| A hosting node refused a sealed part ([`ext-sealed.md`](ext-sealed.md) §6) | `failed: sealed_rejected`, no retry | same |
| `task.timeout` reached | `failed: timeout` | `forward_cancel` (best effort), then `failed: timeout` |

**Re-attach (direct backends only).** A new `dispatch` of the same attempt `n` on `kontra-direct` first calls
`GetTask` on the recorded upstream id and logs the difference from the gate's projection (§6.3). If the
upstream task is terminal, the attempt ends there. Otherwise it follows it with `SubscribeToTask` when the
upstream card streams, else by polling. `UnsupportedOperationError` from `SubscribeToTask` (a task that just
became terminal) and a subscription that ends without a terminal event both fall back to polling; only
`GetTask` failing for 30 s fails the task, `upstream_unreachable`. The 30 s rule applies to every `GetTask` loop,
the polling path's too: a call refused, reset, answered `502/503/504` or `408/429`, or not answered within 10 s
counts toward it, and when it runs out the task fails with the table's reason (`upstream_error` for `408/429`); an
answer that is an error fails the task at once. The call that carries the message keeps the table's rule (D97).

**Follow-up delivery (D81).** A follow-up turn's dispatch writes `work_items.delivered_at` when the upstream
first answers its `SendMessage` or `SendStreamingMessage` (a `Task`, a `Message` or the first stream event),
before it logs anything from that answer. A resume of a follow-up turn whose work item has no `delivered_at`
sends the follow-up again, with the same `messageId` and the upstream ids substituted, then continues as a
re-attach; with `delivered_at` set it re-attaches without sending. An answer lost in transit after the upstream
processed the message is the one case where the upstream sees the follow-up twice. An upstream that refuses the
resent copy is followed as a re-attach all the same; the task fails `upstream_error` only if, for 30 s, the
upstream task neither leaves the projection's `input-required` nor shows the follow-up's `messageId` in its
history (D98). A task's first turn is never
re-sent this way (before its first upstream id a resume is a retry, above).

Re-attach is bounded (D42). An error from a2a-go's handler or the relay executor that is not a lost upstream
context (a2a-go refusing a parallel active execution, a validation failure) is `upstream_error`, never a lost
context. Successive resume dispatches of one attempt back off, 1 s doubling to 30 s, and are capped at 10: when an
11th would be needed, the task fails `upstream_error` with the message `re-attach limit reached`. The 30 s
`GetTask` rule and the task deadline still apply.

**Fencing.** The fence is a per-dispatch token. Every dispatch (a new attempt, a follow-up turn on the same
attempt, a resume) writes a fresh `attempts.dispatch_id` on its attempt when it starts, and holds it. Every append
it makes commits only if, in the same transaction, attempt `n` is the task's latest attempt, has `ended_at` null
and has `dispatch_id` equal to the held value. A newer dispatch therefore fences an older one that Temporal timed
out but whose process is still alive (a partition, a GC pause, `SIGSTOP`), on another replica or on the same one
(a gate of one replica). A fenced write aborts that dispatch (D77). `attempts.replica_id` still records where the
dispatch runs, which routes `abort` and `forward_cancel` (§14.2).

`failed: <reason>` means the terminal status message is an agent-role message with one text part saying what
happened, and `metadata["kontra.failure"] = "<reason>"` (MCP: §11.8). The reasons are a closed set:
`no_session`, `tunnel_lost`, `upstream_unreachable`, `upstream_error`, `service_unavailable`, `timeout`,
`sealed_rejected`, `lifecycle_unavailable` (§14.5). For a sealed service the text is the gate's own, never the
upstream's message, which may quote the sealed input it refused (D195). A declined or expired quote is not a
failure: it ends `canceled` with `metadata["kontra.canceled"] = "declined"` or `"quote_expired"` (§13.2).
Retries never change the gate task id, and a caller sees no event for an attempt that left no upstream task.

### 6.5 Timeouts

`task.timeout` (default 1 h; Cloud listings: `taskTimeoutSeconds`) is **wall clock from submission**,
regardless of state, including `input-required` and `auth-required` (PRD §7). Submission is the moment the
task enters `submitted`; a `quoted` task's clock starts when it is accepted. `tasks.deadline` is fixed then
(§5.2) and never moved. When it passes, the lifecycle sends `forward_cancel` upstream (if an upstream task
exists and the session is up) and writes `failed: timeout` without waiting for the upstream's answer.

### 6.6 Upstream limits

Nothing an upstream sends is unbounded, because a replica that holds one tenant's tunnel also holds others'.
Each of these fails the exchange `upstream_error` (`service_unavailable` for a card), and every upstream body
is read through a byte limit:

| What | Limit |
|---|---|
| A non-streaming upstream body | 8 MiB |
| One SSE event | 4 MiB |
| A card | 256 KiB |
| `tools/list` | 100 pages or 4 MiB in all, and 30 s for the whole drain, which runs detached from the client that started it (D198) |
| Bytes stored per task (each logged event's whole encoding, and each of the task's objects in the bucket, D103, D194) | `limits.task_bytes`, default 256 MiB (an anonymous tenant: 64 MiB in all, [`tunnel.md`](tunnel.md) §6.2) |

Runs of artifact-append chunks may be coalesced into one event before they are logged; the bytes are the
same. For a sealed service, events are never coalesced, and the polling path logs a changed artifact whole
(`append: false`, every part, the hosting node's ciphertext unchanged), so the hiring node's checks hold
([`ext-sealed.md`](ext-sealed.md) §5).

## 7. Follow-ups: `input-required`, and messages after the end

| Caller sends | Task state | Gate does |
|---|---|---|
| A message with the task's `taskId` | `input-required` | Appends it to history (`message` event), then dispatches it on the **same attempt's session** to the **same upstream task**, with the upstream `taskId`/`contextId` substituted. |
| same | the sticky session is gone, revoked or rotated past its overlap | `failed: tunnel_lost`. No retry: the upstream task's state is unrecoverable from outside. |
| same | `submitted`, `working` or `auth-required` | `UnsupportedOperationError` (`-32004`): a running task takes no input, and `auth-required` waits for credentials out of band (D54). Not forwarded. |
| same | `quoted` | `UnsupportedOperationError`: a quote is accepted on the operator's page, not in the protocol (§13.2). |
| same | `completed`, with a review window open (the operator's `ReviewWindow` hook, §13.3) | The interceptor stores the sanitized message as a work item and sends the `kontra.review_message` Update **first**; inside it the operator's `OnReviewMessage` hook decides (Cloud: a dispute when the metadata carries `kontra.escrow.dispute`). Accepted: the message is appended (`message` event) and the caller gets the task. Refused, `accepted: false` (the hook declined, or the work item is missing or already consumed, §14.1), or the workflow already closed: nothing is logged (the hook's patch and dispute flag apply only when the append commits), the work item is deleted, and the caller gets `UnsupportedOperationError`. Not accepted within 10 s, or accepted but not completed within the 10 s in all: `-32603` (HTTP `503`), and the work item is kept; an accepted handler may still append the message, which the caller sees on its next read (D66). Never forwarded upstream. The gate itself names no escrow key (D51). |
| same | terminal otherwise | `UnsupportedOperationError` (spec §3.4.1). |
| A message with a `contextId` and no `taskId` | — | A new gate task in that context (§4); dispatched to the context's previous session when it is connected, otherwise per §6.1, with the upstream context of §4. |
| A message whose `taskId` and `contextId` disagree | — | `InvalidParams` (`-32602`). |

A follow-up while the previous turn of the same task is still being dispatched is refused with
`UnsupportedOperationError`; turns are strictly sequential. A follow-up that answers a turn end the workflow has
not processed yet (the dispatch logs `input-required` a moment before it returns) is not refused: the work queue
sends it again, backing off, within the 10 s, and if it is still not accepted then, its work item is deleted, as
no Update of it is pending (D105). These refusals are synchronous: every follow-up is
a Temporal **Update** of the task's workflow whose validator applies this table, so two racing follow-ups
cannot both be accepted (§14.1, D41). The validator reads workflow state only; the turn's first dispatch loads and
consumes the work item (§14.1, D66). A refused follow-up's work item is deleted. A follow-up not accepted within
10 s gets `-32603` (HTTP `503`) and its work item is kept: the workflow may still accept it, and the turn then
runs. A follow-up turn runs on the same attempt, which stays open while the task waits at `input-required` (§4).

## 8. Cancel

`CancelTask` (and MCP `tasks/cancel`, §11.6) is handled by the gate before a2a-go's executor sees it (D4):
a2a-go's own cancel path marks the task `failed` when the executor's `Cancel` errors, which would contradict
the "task continues" outcome below. It is the `kontra.cancel` Update (§14.1), awaited up to 60 s. A cancel
that wins completes the Update only once the terminal `canceled` event is in the log, within those 60 s, so a
`GetTask` right after `CancelTask` agrees with it (D79).

| Task | Upstream task | Gate does | A2A caller gets |
|---|---|---|---|
| terminal | — | nothing | `TaskNotCancelableError` (`-32002`, HTTP `400`) |
| `quoted` | — | `canceled`, `kontra.canceled = "client"`; nothing was held | the `canceled` task |
| active | none yet | aborts the running dispatch **in process**: `abort(task, attempt)` on the attempt's replica's control queue cancels the dispatch's context through the replica's registry (Temporal's own activity cancellation is requested too, as a backstop, D44). The workflow writes `canceled` only once the dispatch has stopped; if the dispatch had recorded a first upstream task id meanwhile, it sends `forward_cancel` first. The attempt is recorded `aborted` | the `canceled` task |
| active or `input-required` | exists, session up | `forward_cancel(task, attempt)` sends `CancelTask` upstream **inside the tunnel the node opened** | whatever the upstream answers, with gate ids: a `canceled` task (the upstream's `canceled` status is logged as the terminal event; for an `input-required` task, where no dispatch runs, the workflow writes it with `write_terminal`), or the upstream's error (`TaskNotCancelableError`, `UnsupportedOperationError`) while **the task continues**; an upstream task still running (no error, not `canceled`) is answered with the task as the gate stores it, and the task continues (D107); an answer the node writes itself (`Kontra-Node`) is not delivered either: `TaskNotCancelableError`, and the task continues (D148) |
| active or `input-required` | exists, session gone | nothing to forward: `forward_cancel` answers `tunnel_lost`, opening no stream, when its replica does not hold the session or the session no longer serves the service, and for a tunnel attempt that, or a `forward_cancel` not started within 10 s, makes the workflow write `failed: tunnel_lost`; a running dispatch is failing `tunnel_lost` anyway (D148) | the task as it is after that (`failed: tunnel_lost`) |

**The dispatching replica is gone.** `abort`, and a direct task's `forward_cancel` while a dispatch runs, go
to `kontra-ctl-<replica>` (§14.2). When that replica has died and the dispatch's heartbeat has not yet timed out,
nothing polls that queue. A `forward_cancel` not started within 10 s (schedule-to-start) falls back to
`kontra-direct-ctl`; after an `abort` not started within 10 s, the workflow waits for the dispatch's heartbeat
timeout, then applies the table above (D79). That wait is for A2A `CancelTask` only: an MCP `tasks/cancel` keeps
§11.6's rule (at most 10 s, then `canceled`, D57). This is safe while the old dispatch may still be alive: once
`canceled` is logged, the terminal rule refuses its later appends (after it, only `metadata` rows and a
review-window message are written, §5.1), and its fence aborts it (D77); `abort` still runs when it can.

Concurrent cancels are coalesced: while one is in flight, another awaits the same result; one after it
completed starts anew. If the Update does not complete within 60 s the caller gets the stored task as it is.

A cancel that wins writes `canceled` as the last event; any upstream event that arrives later is refused by
the store's terminal rule (`cancel_forwarded`: no `completed` after `canceled`). Cancel never races money: the
gate writes `canceled` first and only then calls the operator's `OnTerminal` (§13.3). MCP's own cancel
semantics differ (§11.6).

### 8.1 How the queue carries work

a2a-go's `workqueue.Payload` carries the full request **and every inbound HTTP header, `Authorization` and
cookies included**. Kontra's Temporal-backed queue therefore never puts the payload in Temporal: its `Write`
stores a sanitized copy in Postgres (`work_items`: the message, the call context minus every header except
`A2A-Version` and `A2A-Extensions`, the authenticated principal, and the task version it was stored at, D94) and
starts or updates the workflow with the
row id only (D6). Detail in §15.

## 9. Streaming to the caller and resume

The gate writes its own SSE for `SendStreamingMessage` and `SubscribeToTask` on both bindings, fed from the
event log, never from the upstream connection (D2). a2a-go's SSE writer emits random event ids and never
reads `Last-Event-ID`, which is why the gate cannot use it here.

- **What a stream carries.** A2A streams carry only `task`, `status` and `artifact` rows: a task stream is a
  `Task` followed by status and artifact updates (spec §3.1.2), and a `Message` in a stream would mean the
  message-only pattern. `message`, `metadata` and the MCP kinds are never sent on A2A streams; their effect
  shows in the next snapshot (GetTask, the resume snapshot, push).
- **Framing.** One SSE event per carried row: `id: <seq>` then one line `data: <JSON>` (the JSON-RPC envelope
  `{"jsonrpc":"2.0","id":…,"result":<StreamResponse>}` on JSON-RPC, the bare `StreamResponse` on HTTP+JSON).
  `data` is always a single line. A `: keep-alive` comment every 15 s. Ids are seqs: strictly increasing, not
  contiguous (the rows a stream does not carry are skipped).
- **Errors before the stream.** Unknown task, terminal task without resume, version or extension errors are
  returned as ordinary JSON-RPC/HTTP errors **before** SSE headers are written (the TCK fails a `200` stream
  that carries an error).
- **`SendStreamingMessage`, new task.** Admits and creates the task (as `SendMessage` does), then streams the
  creation snapshot (`id: 1`) and every later carried row. It closes after the first row whose state is
  terminal, `input-required` or `quoted`.
- **`SendStreamingMessage`, follow-up** (to an `input-required` task). The first event is the task snapshot
  at the version V current when the `kontra.message` Update was accepted, sent without an `id:` line; then
  rows with seq > V. The leading snapshot never closes the stream; it closes after the first row with
  seq > V whose state is terminal, `input-required` or `quoted`.
- **`SendStreamingMessage`, resend** (no `taskId`, answered with the task it repeats, §15). The snapshot at the
  task's current version without an `id:` line; the stream closes after it if it already ends a turn, and
  otherwise after the first later row that does (D108).
- **`SendStreamingMessage` to a `completed` task with an open review window.** The interceptor's answer (§7) as
  one task event without an `id:` line; then the stream closes (D109).
- **`SubscribeToTask` without `Last-Event-ID`.** Terminal task: `UnsupportedOperationError` (spec §3.1.6; TCK
  `STREAM-SUB-003`). Otherwise the current snapshot as a `Task` event **without** an `id:` line, then every
  carried row with seq greater than the snapshot's version. The stream stays open through `input-required`
  and `auth-required` and closes after the terminal event (TCK `STREAM-SUB-002`).
- **Resume: `SubscribeToTask` with `Last-Event-ID: K`** (header, on either binding; `K` a seq the gate issued
  for this task). The gate sends the task snapshot **as of seq K** — folded from rows `1..K`: up to the terminal
  row, a2a-go's `ApplyUpdate` for task, status, artifact and message rows; after it, message rows appended to
  `history` directly, because `ApplyUpdate` refuses any update to a terminal task (D75); metadata rows always as
  a top-level merge into `metadata` — as a `Task` event without an `id:` line, then every carried row with
  seq > K, then the live tail.
  Every carried row after K is sent exactly once, in seq order, and the generation order of spec §3.5.2 is
  kept, because the snapshot precedes everything it does not contain. On a terminal task with K below its
  terminal seq the stream ends after the terminal row; with K at or past it, it is
  `UnsupportedOperationError` (nothing is missing). A `K` that is not a seq of this task is `InvalidParams`.
  This is a Kontra behaviour on top of A2A, which does not define `Last-Event-ID` (D2).
- **Several subscribers** each read the log independently: a dropped stream never affects another stream or
  the task.

Non-streaming `SendMessage` returns on the first terminal, `input-required` or `auth-required` state (A2A
§3.1.1), or at once with `returnImmediately: true` (the `submitted` snapshot), as a2a-go does; a blocking call
is bounded only by the task's deadline and the caller's own connection (§15).

## 10. Push, SSRF and large parts

### 10.1 Configs

`Create/Get/List/DeleteTaskPushNotificationConfig` and the inline `configuration.taskPushNotificationConfig`
are stored per gate task in `push_configs`. The gate (not a2a-go's executor) saves an inline config, before
the work item is written, so a refused URL is a synchronous `InvalidParams` and nothing is stored. A config's
URL is checked when it is set (§10.3); a redirecting URL is accepted, and its redirect is never followed
(§10.2). Save is idempotent: a config without an id is keyed by (task, URL, token hash), so a2a-go's re-save
on each turn neither duplicates rows nor starts a second delivery. At most 10 configs per task and 256
delivering configs per buyer; beyond that, `InvalidParams`. The `anonymous` principal cannot set push
configs (§2.3). The upstream's own push configs are never used: the gate is the agent's only client. A
review-window message's inline config is dropped, neither saved nor stored with its work item, and does not
refuse the message (D110). A config set on a task that has already ended starts at its terminal row: it is
stored, listed and deletable, receives nothing, and does not count as delivering (D111).

### 10.2 Delivery

Push is delivered by the lifecycle, never by the replica that relays events (D16). After a `Save` that
inserted a row, or a `Delete`, commits, the store sends the signal `kontra.push {configId, deleted}` to the
task's workflow; the workflow runs one `push_webhook(task_id, config_id)` activity per live config, on its own
queue `kontra-push` (its own worker slots, so pushes never starve the lifecycle's activities, D55), and lists
`push_configs` at start and after each dispatch to recover a lost signal. In history, `configId` (here and in
§14.1–§14.2) is the config's ref, `pc_` and a SHA-256 of the task id and the config id, never the id itself,
which a caller may choose (D112). The activity tails the event log
from the last seq it delivered and, for each event, delivers the **task snapshot at that seq** as
`{"task": …}`, in seq order. Runs of consecutive `artifact` events may be coalesced into the snapshot at the
last seq of the run; every event that changes the status, and the terminal event, is delivered.

- One HTTP `POST` per delivery, 10 s timeout, `Content-Type: application/a2a+json`, the config's
  `authentication` as `Authorization`, the config's `token` as both `A2A-Notification-Token` and
  `X-A2A-Notification-Token`. Redirects are not followed: a `3xx` is a failed delivery and its target
  receives no connection.
- Any `2xx` is delivered. Anything else is retried with exponential backoff (1 s doubling to 5 min) for at most
  24 h from the first failure of that seq, then the config is marked `failed` and delivery stops for it; the
  task is unaffected. Deliveries to one destination host are capped at 10 a second across the gate: each replica
  allows 10/N a second, where N is the number of live replicas in `gate_replicas` ([`tunnel.md`](tunnel.md)
  §11), draining ones included (they keep polling `kontra-push`, D76), and at least 1; N is refreshed every
  10 s. So 10 a second is a gate-wide upper bound (D72).
- The activity ends after delivering the terminal snapshot, or when its config is deleted. History stays one
  activity per config, whatever the number of events: events never enter Temporal history. `TaskLifecycle`
  does not complete while a `push_webhook` it started is pending, so the workflow's end never cuts off the
  terminal push; the activity still ends at the limits above (D78).
- Delivery is at least once: `push_configs.delivered_seq` is written after every delivery (it only moves
  forward), and a restarted activity resumes from the larger of it and its heartbeat details. A receiver may
  see a snapshot twice; the seq order never goes backwards.

### 10.3 SSRF rules (push URLs, direct backends, operator URLs)

One dialer for every connection the gate opens to a URL it was given. It resolves the host, checks **every**
address the name resolves to, pins the one it uses for the connection, and refuses every address that is not
globally reachable per the IANA IPv4 and IPv6 special-purpose address registries (RFC 6890 and successors):
loopback, RFC 1918, link-local, ULA, CGNAT `100.64.0.0/10`, `0.0.0.0/8`, `198.18.0.0/15`, `240.0.0.0/4`,
broadcast, multicast, documentation ranges, and the cloud metadata addresses. It first unmaps IPv4-mapped
IPv6 addresses and extracts the embedded IPv4 address of NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`), 6to4 and
Teredo addresses, and checks that too. Redirects are never followed and the proxy from the environment is
always ignored. Only `https://` is allowed, except where `allow_private: true` covers the backend or config (per
backend; for push, `push.allow_private` in the gate config, meant for tests): it skips the address check and
also permits `http://` (D67), and never relaxes the redirect or proxy rule. The one other exception is
§13.1's: the Admission hook URL (and any other `hooks.*` URL §13 allows) may be `http://` on a loopback host.
Every other operator URL follows this section (D67), except the bucket's endpoint (`blobs:`, §10.4): the bucket
is the gate's own storage, like its Postgres, so its endpoint is dialed as configured (`http://` only when the
operator writes it), with the environment's proxy ignored and redirects never followed (D192). a2a-go's push
sender lacks several of these rules, so the gate uses its own dialer (D17).

### 10.4 Parts and size

Parts up to 256 KiB (measured on the decoded bytes, before base64; for a sealed part, its ciphertext) are
stored inline in `task_events.payload`. Larger ones go to the bucket (`blobs:` in `kontra.yaml`), and the event's
payload carries each as a `url` part `kontra-blob:<ref>`, a name that is never dialed; the `blobs` table maps each
ref to its task, since one `payload_ref` column cannot name the several large parts of one message (D167). They
are delivered:

- **to callers** (stock A2A clients, push receivers, and direct backends) as a `url` part with a presigned GET
  valid 15 minutes;
- **to nodes**, as a `url` part whose URL is `kontra-blob:<ref>`: the node fetches it through its own tunnel
  ([`tunnel.md`](tunnel.md) §8.3), never from a URL taken from a part (D56).

A gate without a bucket refuses a request with a part over 256 KiB (`InvalidParams`, message `part too large:
configure blobs`), and an upstream part over the limit fails the task `upstream_error`. Sealed or not, the
gate stores bytes it does not interpret; per-task totals are bounded (§6.6).

**`kontra-blob:` is the gate's (D181).** A `url` part whose URL starts with `kontra-blob:` is refused from a
caller (`InvalidRequest`) and from a service (the task fails `upstream_error`), so every such part in the log is
one the gate wrote. A ref is resolved only through the `blobs` table and rendered only for the task its row names:
a part naming another task's ref is never turned into a presigned URL, and a blob stream's ref must belong to a
task the asking session serves or its hiring principal's buyer owns ([`tunnel.md`](tunnel.md) §8.3).

## 11. MCP relay: `/mcp/{svc}`

The gate serves each `kind: mcp` service as a Streamable HTTP MCP endpoint, protocol revision **2025-11-25**
(§0), and **2026-07-28** on the same endpoint (§11.12). §11.1–§11.11 describe the 2025-11-25 ("legacy") era;
§11.12 says what changes for a 2026-07-28 ("modern") request. Two request shapes, two primitives (note 1 §5):

| Shape | What the gate adds | Durability |
|---|---|---|
| Plain requests (`initialize`, `tools/list`, `tools/call`, `resources/*`, `prompts/*`, notifications, responses) | Auth, `allow:`, per-tool policy, limits, metering, session affinity | None: there is no task to make durable. A pinned session dies with its tunnel. |
| Task-augmented `tools/call` (`params.task`), when the service's `tasks:` mode is `durable` | All of the above, plus the gate owns the task: `TaskLifecycle`, event log, `tasks/*` from Postgres | As for A2A (§5–§10) |

The relay works on raw JSON, decoded with exact keys (§11.1). It never decodes and re-encodes a message
through a typed SDK, because go-sdk v1.8.0 drops `Tool.execution` and rejects `tasks/*` (D19). Unknown fields
and unknown methods pass through unchanged, except Kontra's own keys (§2.5).

### 11.1 Transport rules

- **One message per POST.** Every POST body on `/mcp/{svc}` and `/mcp` must be a single JSON object. An
  array, or any other JSON value, is refused before any forwarding with HTTP `400` and an id-less JSON-RPC
  error `-32600` (`batch not supported`), whatever `MCP-Protocol-Version` says: a batch would carry calls
  past per-tool policy, Admission and metering, and both revisions the gate speaks forbid it
  (`mcp_batch_refused`).
- **Strict decoding (D172).** The gate decodes every message it acts on, from the client and from the upstream,
  with exact, case-sensitive keys. A decoder that matches keys case-insensitively or keeps the last of a repeated
  key (Go's `encoding/json` into a struct does both) would read another method, tool name or `isError` than the
  upstream or the client reads from the same bytes, and the relay forwards the bytes it received. So it checks each
  level named next, and a level in which a key repeats, or two keys are equal under Unicode case folding, fails. A
  client message that fails in the message object, `params`, `params._meta`,
  `params._meta["io.modelcontextprotocol/clientCapabilities"]` or its `extensions`, `params.capabilities` (an
  `initialize`), the top level of `params.arguments` or of `params.inputResponses`, `result` or `result._meta` is
  refused with HTTP `400` and `-32600` (`invalid request`), with the `id` when it was read unambiguously and `null`
  otherwise, before policy, metering, Admission or forwarding. An upstream message that fails in the message
  object, `params` or `params._meta` (of a request or a notification), `result`, `result._meta`, any of
  `result.tools` or its `_meta` (`tools/list`), `result.capabilities` or its `experimental` or `extensions`
  (`initialize`, `server/discover`), a task object the gate reads, or `error`, is not relayed: a JSON response
  becomes HTTP `502` with `-32603` `upstream error`, an SSE stream ends before that event, the `calls` row has
  `is_error` true, and a dispatch fails `upstream_error` (`mcp_case_smuggling`). The task objects are a 2025-11-25
  `CreateTaskResult`'s `result.task` and, in a 2026-07-28 task or `InputRequiredResult` (which is `result` itself),
  `result.inputRequests`, any of its entries, their `params` and `params._meta`, and a terminal task's
  `result.result` and `result.error`. A lone case variant of a member the gate reads at one of these levels
  (`METHOD` without `method`, a lone `NAME` or `ISERROR`, `tasK` with a Kelvin sign) fails the same way, since a
  decoder that folds case reads it as the member; a task object's members are checked where the gate reads them
  (D191).
- **What a client reads (D197).** The gate reads, meters, scrubs and relays an upstream's answer as a client
  reads it: one leading UTF-8 byte order mark of an event stream is dropped; only events whose type is absent or
  `message` are read and relayed, and others are dropped; the response to the client's request is matched as the
  TypeScript SDK matches ids (`Number(id)`, so `"7"`, `7.0` and `7e0` answer `7`; a string id only itself), and
  the stream ends before a second response to it; a top-level `isError` that is not a JSON boolean makes the
  message malformed, as above.
- **Versions.** `MCP-Protocol-Version` `2025-11-25` and `2025-06-18` are accepted on a session; an absent header
  means the session's negotiated version; `2026-07-28` makes the request a modern one (§11.12); anything else is
  `400` with `-32022` (§11.12). If an upstream's `initialize` negotiates another
  version, the gate answers the client `-32602` naming the supported versions and sends the upstream a
  best-effort `DELETE`. On `2025-06-18` sessions the gate advertises no tasks (`tasks: off` behaviour).
- **Request headers.** `Accept`, `Content-Type` and `MCP-Protocol-Version` are forwarded. The client's
  `Authorization` is validated at the gate and **never** forwarded (MCP authorization: a server MUST NOT pass
  tokens through). An `Origin` header, when present, must be listed in `mcp.allowed_origins`, else `403`. On
  the node, `Host` is set to the target and `Origin` is dropped ([`tunnel.md`](tunnel.md) §8).
- **Response headers.** The gate drops every upstream response header except `Content-Type` and
  `Cache-Control`, and replaces `Mcp-Session-Id` with its own. An upstream `401`, `403` or `407` becomes HTTP
  `502` with JSON-RPC error `-32603` `upstream refused`: the only `WWW-Authenticate` a client ever sees is the
  gate's, so an upstream cannot steer a client's authorization discovery or set cookies on the gate's origin.

### 11.2 Sessions and affinity

- **`initialize`** is forwarded to a chosen upstream (§6.1 session choice). The gate records the upstream's
  `Mcp-Session-Id` (if any), mints its own (128 random bits, base64url, 22 characters) and returns that to the
  client. The pair is a row in `mcp_sessions(client_sid_hash, tenant_id, service, buyer_id, principal, via,
  upstream_sid, tunnel_session, replica_id, protocol_version, client_capabilities, created_at, last_seen_at,
  closed_at, close_reason)`, whose `replica_id` is the replica holding the tunnel session (NULL for a direct
  backend), not the one the client reached (D179). The client id is stored as its sha256 and bound to the
  principal (MCP security best practices). The operator's `initialize_meta` annotation is merged into the
  result's `_meta` (§13.4).
- **Every later request** carrying `Mcp-Session-Id` is routed to the same tunnel session after the enrollment
  check of §6.1, with the upstream's id substituted. Requests and responses pass unchanged otherwise: JSON-RPC
  ids, SSE event ids and `retry:` fields are the upstream's (they are scoped to the upstream session). A
  client's `GET` stream and its `Last-Event-ID` resume go to the same pinned session, except gate-written
  streams (§11.7).
- **404.** A client id that is unknown, closed, bound to another principal, or presented on another service's
  path is `404` (`mcp_session_cross_node`). A pinned tunnel session that is gone, or whose key or binding was
  revoked, closes the row (`tunnel_lost` or `revoked`) and answers `404`, as the transport requires; the client
  re-initializes and lands on any connected replica of the service (note 1 §8, default). An upstream `404` on
  a pinned session whose tunnel is up closes the row `upstream_closed`. A node's `not_hosted` or `kind_mismatch`
  on a pinned session closes it `tunnel_lost` and is `404`; its `full` is `503`, its `sealed_rejected` is §11.4's,
  and any other answer it writes is `502` with `-32603`. The gate's unknown-session `404` and its `401` carry `-32001`, as MCP servers answer an
  unknown session (D196).
- **`DELETE`** with the session id is forwarded upstream and closes the row; when the upstream's tunnel is gone
  the gate still closes the row and answers `200` (D196). A session idle for the service's
  `session_idle_s` (default 3600) is closed by the gate (`idle`; upstream `DELETE` best effort).
  `last_seen_at` is written at most once a minute. The `initialize` result states that bound: the gate sets
  `_meta["sh.kontra/session_idle_s"]` to the service's `session_idle_s` (a JSON integer of seconds), after the
  operator's `initialize_meta` and in place of any upstream's (§2.5), and a hiring node keeps a sealed session's key
  that long idle ([`tunnel.md`](tunnel.md) §2.3, D224).
- **A cap per principal (D213, D231).** A principal other than `anonymous` holds at most
  `services.<name>.max_sessions_per_principal` (default 4) open sessions of a service whose backend is a tunnel;
  2026-07-28 virtual sessions are not counted, and a direct backend is not capped (an HTTP server the operator runs
  has no stdio children to share). The cap counts the principal, the credential a session is bound to: every client
  of one API key shares it, and so do a hiring node's local clients, whose principal is the node session's
  ([`tunnel.md`](tunnel.md) §9); a buyer's several keys each hold it, and a durable task's own upstream session
  (D22) is not counted. `anonymous` is not capped: its callers are distinct clients the gate cannot tell apart, and
  one budget for all of them would let any caller crowd out, or close, the others' sessions; a service open to
  `anonymous` shares its upstream's sessions with everyone. An `initialize` beyond the cap closes as many of that
  principal's sessions as it needs, never another principal's, and only sessions whose `last_seen_at` is at least
  5 minutes old, the oldest first (`idle`), with an upstream `DELETE` the gate makes on each session's pinned target
  (the peer hop for a tunnel another replica holds) before it forwards the `initialize`, once it has chosen an
  upstream: a stdio service's children (`--max-sessions`, [`tunnel.md`](tunnel.md) §10) are shared by every
  principal admitted to it, and the closed session's child is what the new one gets. When too few are that old, it
  closes none and is refused `429` with `Retry-After` and JSON-RPC error `-32600` `too many sessions`
  (`data.max_sessions_per_principal`), as the edge's in-flight caps are (§2.2). A session is in use while a request
  or a `GET` stream of it is open on any replica: the replica serving it writes its `last_seen_at` at least every
  minute and a half meanwhile, and once more as the last ends, so a session in use is never 5 minutes old, and is
  never closed for a new one (nor by the idle sweep while a stream lasts, for a `session_idle_s` over 90 s). The
  check runs under one lock per principal and service, before the upstream `initialize` and again as the new row is
  written, so concurrent `initialize`s of one principal leave at most the cap open; one that finds no room then is
  refused the same way, and its upstream session gets a `DELETE`. A closed session answers `404`. A full stdio
  bridge also stops its own longest-idle child, idle for at least 5 minutes, for a principal that holds no session
  there.
- **The peer hop (D163).** Only the replica holding a tunnel session can open a stream on it
  ([`tunnel.md`](tunnel.md) §11), and a plain MCP request has no Temporal activity to carry it there. A request
  for a tunnel session held by another replica (a pinned session's later request, or a session chosen for an
  `initialize` or a modern request, §11.12) is forwarded to that replica's **peer listener** (`peer.listen`, at
  the address it advertises in `gate_replicas.peer_addr`): one HTTP/1.1 exchange carrying the method, path, body
  and headers the receiving replica would send upstream, never the client's `Authorization`, plus `Kontra-Peer`:
  an assertion of the principal, the tunnel session, the service, the preamble's MCP fields
  ([`tunnel.md`](tunnel.md) §8.1), the upstream's `Mcp-Session-Id`, the SHA-256 of the other forwarded headers
  (`Accept`, `Content-Type`, `MCP-Protocol-Version`, `Last-Event-ID`, `Mcp-Method`, `Mcp-Name` and every
  `Mcp-Param-*`, names lowercased and sorted, one `name: value` line each), a nonce and the time, with an
  HMAC-SHA256 under the shared secret (`peer.secret_env`) over the assertion, the method, the path and the
  body's SHA-256. An assertion older than 30 s, or whose nonce the replica has seen, is refused, and so is a
  request whose headers do not hash to the assertion's. The holding replica builds the upstream request from
  signed data only: the upstream session id from the assertion, and only the headers it hashed. It runs §6.1's
  check on its own registry (a session it refuses is answered `404` with the gate-internal `Kontra-Peer-Gone:
  tunnel_lost | revoked`, and the receiving replica closes the row with that reason, D196), then the exchange on
  its own tunnel, and streams the response back unchanged; every other rule of §11 (authentication, `allow:`,
  policy, the upstream id substitution, metering, rewrites) is applied once, by the receiving replica (D179).
  The peer listener serves that one route, answers anything else `404`, and is never the public listener. Its
  address must be reachable only from the gate's replicas (a private network, a NetworkPolicy), or carry TLS: the
  HMAC authenticates a request and does not hide it.
  Session choice prefers this replica's sessions. Without `peer.listen`, only this replica's sessions are
  candidates and a pinned session held elsewhere is `404` (its row stays open until it is idle): a gate of one
  replica needs no peer listener, and a gate of more than one SHOULD set it, since otherwise every later request
  that its balancer sends to another replica than the session's (about half of them with two) ends the session
  with a `404` and a new `initialize`. A direct `mcp: {url}` backend is reached from any replica and never
  hops.

### 11.3 Policy per tool

`tools: { allow: [glob…], deny: [glob…] }` on the service narrows which tools a principal may call; globs use
`*` only; deny wins; an absent `allow` allows all, and an empty one none (D198). It is applied on the **body's**
`params.name`, never on a header.

- **`tools/list`.** The gate drains every upstream page (following `nextCursor`, within the limits of §6.6),
  drops denied tools, removes Kontra's keys (§2.5), applies the operator's `tool_meta` annotation to each
  remaining tool's `_meta` (§13.4, e.g. Cloud's `sh.kontra/price`), and answers with one page and no cursor.
  It never forwards a cursor it did not mint, so denied tools never shape page boundaries. The drained list is
  cached per service and refreshed on upstream `notifications/tools/list_changed` (which is forwarded; the
  replica that sees it tells the others with `NOTIFY kontra_tunnel, 'tools:<tenant_id>/<service>'`, D199) and
  every 10 minutes. A replica whose `LISTEN` connection comes back drops every cached list, since such a notice may
  have been lost while it was down (D230).
- **`tools/call` of a denied or unknown tool** is answered by the gate, before dispatch, with `-32602` and
  message `Unknown tool: <name>`, exactly as for a tool that does not exist (`mcp_tool_denied`).

### 11.4 Plain calls and metering

A plain `tools/call` is forwarded on the pinned session. When the response completes, the gate writes one
`calls` row: `(id, tenant_id, service, tool, buyer_id, principal, mcp_session, task_id NULL, bytes_in,
bytes_out, duration_ms, is_error, at)`. `is_error` is true for a JSON-RPC error or a result whose
**top-level** `isError` is true. That and the tool name, byte counts and duration are all the gate reads; it
never parses `arguments`, `content` or `structuredContent`, which may be sealed
([`ext-sealed.md`](ext-sealed.md)). The table holds no price. What a call costs is the operator's business
(Cloud reads `calls` and prices it from its listings, §13.5).

A call whose exchange was answered but whose JSON-RPC response never arrived (a cut stream, a refused event, a
client that left) is metered `is_error` true; a call that got no answer at all (a §6.1 refusal, an unreachable
upstream or peer, a client that left first) has no row (D196). A hosting node's refusal of a sealed call
([`ext-sealed.md`](ext-sealed.md) §6) reaches the client as a JSON-RPC error with HTTP `200` (`-32602`, `sealed
part rejected: <reason>`, the request's id), metered `is_error` true and audited (D202).

### 11.5 Task-augmented calls

The service's `tasks:` key decides (note 1 §6 operator config):

| `tasks:` | Advertised to the client | A client `params.task` |
|---|---|---|
| `durable` (default) | `capabilities.tasks` as the upstream declares it; the gate adds `list` and `cancel` | the gate owns the task (below) |
| `passthrough` | as the upstream declares it | forwarded on the pinned session; the upstream owns the task; the gate relays `tasks/*` on that session like any plain request. The `calls` row is written when the `CreateTaskResult` returns, with `task_id NULL` and `is_error NULL`: the outcome is not observed, so an operator never bills `per_call` on such rows, and a priced task needs `durable` (§13.1) |
| `off` | `capabilities.tasks` removed; every tool's `execution` removed | `-32601` |

The gate never advertises tasks the upstream does not support, and never runs the task shape over a plain
upstream call (note 1 §8 and README "Open", default kept).

**`durable`:**

0. The gate resolves `params.name` against the service's cached `tools/list` (§11.3). Unknown or denied:
   `-32602` `Unknown tool: <name>`. `execution.taskSupport` absent or `forbidden`: `-32601`. Either way there
   is no Admission and no task.
1. Admission (§13.1) with operation `task`. Refused: the JSON-RPC error. Quote: the task is created `quoted`
   (§13.2).
2. The gate creates the gate task (`protocol = mcp`; the request, the client session's hash and the client's
   JSON-RPC request id stored by reference in `work_items`), starts `TaskLifecycle`, and answers at once with
   `CreateTaskResult`: `{"task": {"taskId": <gate task id>, "status": "working", "createdAt",
   "lastUpdatedAt", "ttl", "pollInterval": 1000}}`.
3. The dispatch activity, on the owning replica, opens **its own upstream session** (an `initialize` of its
   own, declaring the `elicitation` and `sampling` capabilities the client declared) and records it in the
   attempt. It does not ride the client's session: upstream task stores are often scoped to the session that
   created the task, and the client's session can end at any time (D22). Every exchange of the dispatch
   carries the client session's hash and the client's request id in its preamble ([`tunnel.md`](tunnel.md)
   §8.1), and the upstream `tools/call` reuses the client's JSON-RPC id (the dispatch's own other requests use
   ids that cannot collide with it), so a sealed call's binding holds ([`ext-sealed.md`](ext-sealed.md) §3).
4. It sends the task-augmented `tools/call` upstream with `task: {ttl: <milliseconds until the deadline>}`
   (the upstream may shorten it) and records the upstream `taskId` (the **first upstream task id**, §4).
   Then it polls upstream `tasks/get` at the upstream's `pollInterval` (bounded to 0.5–30 s), logging each
   status change as a `status` event, and holds a `GET` on its own upstream session. If upstream `tasks/get`
   or `tasks/result` answers `-32602` after the first upstream task id (the upstream deleted it), the task
   fails `upstream_error`, `upstream task expired`.
5. **`input_required`.** The activity calls upstream `tasks/result` on its session, which delivers the
   upstream's requests (`elicitation/create`, `sampling/createMessage`) carrying
   `_meta["io.modelcontextprotocol/related-task"]`; requests that arrive on its `GET` are treated alike. Each
   is logged as an `input_request` event, with the related-task id rewritten to the gate's and the request id
   rewritten to a gate id (§11.7). The client receives it on its own `tasks/result` stream. The client's
   response is logged as an `input_response` event only if none exists yet for that gate request id (checked in
   the same transaction, under the task's version; a loser is `202` and dropped). The running dispatch activity,
   which holds the upstream session and tails the task's log, writes it upstream under the original request id;
   the `kontra.input_response` signal only updates workflow state.
6. **Terminal.** As soon as the upstream task is terminal, the activity fetches the upstream `tasks/result`,
   logs it (ids rewritten) as the terminal event, writes one `calls` row with `task_id` set, and closes its
   upstream session.

### 11.6 The client side of a gate-owned MCP task

The client side reads only Postgres:

| Client method | Gate answers |
|---|---|
| `tasks/get {taskId}` | the projection: `{taskId, status, statusMessage?, createdAt, lastUpdatedAt, ttl, pollInterval}`, plus the operator's `mcpMeta` (§13.3) |
| `tasks/result {taskId}` | blocks (on `LISTEN`) until terminal, delivering any pending `input_request` events on its own SSE response stream first; then the stored result (§11.8), with `_meta["io.modelcontextprotocol/related-task"] = {taskId}` |
| `tasks/list {cursor?}` | the principal's own MCP tasks for this service, newest first, gate-minted cursors; refused for `anonymous` (§2.3) |
| `tasks/cancel {taskId}` | on a terminal task, `-32602`. Otherwise the `kontra.cancel` Update runs `abort` (no upstream task yet) or a `forward_cancel` that sends upstream `tasks/cancel` only if the upstream declared `tasks.cancel` (as the replica running the dispatch knows it; where that is not known, it is sent, and an upstream that does not serve it answers `-32601`, which changes nothing, D203); it waits at most 10 s, then writes `canceled` whatever the upstream answered, and returns the task with `status: "cancelled"`. MCP requires the receiver to move the task to `cancelled` before it responds; unlike A2A, an MCP task never "continues" after a cancel (D57). Later upstream events are refused by the store's terminal rule. This rule stands when the dispatching replica is gone: D79's wait for the heartbeat timeout is for A2A `CancelTask` only, and an old dispatch that may still be alive cannot write once `canceled` is logged (§5.1) and is aborted by its fence (D77); `abort` still runs when it can (§8) |

Tasks are bound to the principal's `buyer_id`, as A2A tasks are: a `tasks/*` call naming a task of another
buyer is `-32602`, as for an unknown id. The `io.modelcontextprotocol/related-task` key is rewritten to the
gate's task id in both directions; upstream task ids never reach the client. `ttl` is reported as
`retention_until − created_at` once the task is terminal, and as the deadline plus the retention (an upper
bound) before.

### 11.7 Gate-written messages and streams

The gate never writes into a relayed upstream stream. Messages it originates for gate-owned tasks (input
requests, and the task status if it sends any) go only on streams it serves itself: the response stream of
the client's `tasks/result`. On `/mcp/{svc}` it does not send `notifications/tasks/status` (optional in the
spec).

- Gate-served SSE events carry `id: k.<taskId>.<seq>`. A `GET` whose `Last-Event-ID` starts with `k.` is
  served by any replica from `task_events`, for the same principal only, and never forwarded; any other
  `Last-Event-ID` goes to the pinned session. A resumed `tasks/result` stream carries the task's pending input
  requests after that seq and each new one, and ends when the task is terminal, without the answer to the
  `tasks/result` it resumed, which the event id does not name: the client asks `tasks/result` again and gets the
  stored outcome at once (D203).
- Gate-minted server-to-client request ids are the strings `k.<taskId>.<seq>`. A client response carrying
  such an id is consumed by the gate, accepted once and only from a session of the task's buyer (an unknown,
  already answered or foreign one is `202` and dropped); every other response is forwarded.

### 11.8 Gate outcomes on MCP

For tasks of `/mcp/{svc}` (`protocol = mcp`), a `tasks/result` on a terminal task returns exactly what the
task-augmented call returned, as MCP requires (`hire` tasks: §12.2):

1. The upstream's own result, or the upstream's JSON-RPC error answering the task-augmented `tools/call` or
   `tasks/result`, stored as the terminal `result` and returned verbatim (code, message, data; ids rewritten),
   except that a sealed service's error is stored with its code only (D195).
   For a JSON-RPC error the task ends `failed` (`upstream_error`) and `tasks/get` carries
   `_meta["sh.kontra/failure"] = "upstream_error"`.
2. A failure the gate wrote (`no_session`, `tunnel_lost`, `timeout`, `upstream_unreachable`,
   `service_unavailable`, `sealed_rejected`, `lifecycle_unavailable`, and `upstream_error` when no upstream
   JSON-RPC error answered the call): `tasks/get` shows `status: "failed"`,
   `statusMessage` = the reason text and `_meta["sh.kontra/failure"] = "<reason>"`; `tasks/result` returns
   `{"code": -32603, "message": <reason text>, "data": {"sh.kontra/failure": "<reason>"}}`.
3. `canceled` (by the client, or a declined or expired quote): `tasks/result` returns `{"code": -32603,
   "message": "task cancelled", "data": {"sh.kontra/canceled": "client" | "declined" | "quote_expired"}}`. A task
   the upstream cancelled itself ends `canceled` too, and its `tasks/result` is `-32603` `task cancelled` without
   `data` (D203).

In cases 2 and 3 the task's `calls` row has `is_error = true`. A 2025-11-25 upstream reports `failed` for a
result with `isError: true`; the gate logs the result and ends the task `failed`. Cloud's settlement keys on
the gate state and on the result's top-level `isError`, never on content.

### 11.9 MCP events in the log

MCP task events use the same `task_events` rows with MCP payloads: `task` (creation), `status` (`{status,
statusMessage}` in the shared vocabulary of §5.1), `input_request` and `input_response` (the JSON-RPC
message, ids rewritten), and the terminal `result` (§11.8). Retries, timeouts and cancel follow §6.4–§8, with
§11.6's cancel. The first upstream task id is the `taskId` in the upstream `CreateTaskResult`; after it,
`tunnel_lost` fails the task (`mcp_task_resume`: the caller's `tasks/result` still returns, with that
failure, on whichever gate replica it asks).

### 11.10 MCP servers on a node: stdio and roots

`--serve mcp:fs=stdio:<cmd>` is bridged on the node, never at the gate. The gate sees a Streamable HTTP target
like any other ([`tunnel.md`](tunnel.md) §10).

For every MCP server a node hosts, stdio or `mcp:<name>=<url>`, the node answers the server's `roots/list` itself
from the roots its operator configures (`--roots`, none by default), and never relays `roots/list`,
`notifications/roots/list_changed` or the client's `roots` capability between the server and the remote client:
a client's roots never reach a hosted server (D173). The gate forwards those messages like any other; the node
drops them.

### 11.11 What is not carried in v4

The 2024-11-05 HTTP+SSE transport is not served to clients and not spoken to upstreams in v4.0 (D23).
Upstreams that only speak it can be put behind a bridge on the node; the gate answers a client's legacy `GET`
probe with `405`.

### 11.12 The 2026-07-28 revision

MCP 2026-07-28 removes `initialize`, `Mcp-Session-Id`, the `GET` stream, SSE resumability, the 2025-11-25 Tasks
and `-32042`. Every request carries its version and the client's capabilities in `_meta`, and tasks are the
`io.modelcontextprotocol/tasks` extension (§0). A dual-era server "MAY serve both eras concurrently on the same
endpoint" (2026-07-28, Versioning). The gate does (D24): legacy requests get §11.1–§11.11 unchanged, modern
requests get this section, and the same `TaskLifecycle` and log stand behind both.

**Classification (D158).** After §11.1's one-message rule, its strict decoding and its `Origin` check, and before
anything is forwarded:

1. A POST whose `method` is `initialize` is legacy and opens a session (§11.2), whatever its headers say.
2. A POST with `MCP-Protocol-Version: 2026-07-28` is **modern**. Its `Mcp-Session-Id` and `Last-Event-ID` are
   ignored: never looked up, never forwarded, and no session id is minted for it.
3. A POST whose header is `2025-11-25`, `2025-06-18` or absent is legacy and needs a session (§11.2). Without
   `Mcp-Session-Id` it is `400` with the body `{"jsonrpc": "2.0", "id": null, "error": {"code": -32000,
   "message": "Bad Request: No valid session ID provided"}}`: what a legacy server answers, and not a modern
   error, so a dual-era client falls back to `initialize` (2026-07-28, Streamable HTTP, "Backward
   Compatibility").
4. Any other header value is `400` with `-32022` (`UnsupportedProtocolVersionError`, `data: {"supported":
   ["2026-07-28", "2025-11-25", "2025-06-18"], "requested": <the header>}`). The header is read once, as its
   field value: one sent more than once is its lines joined with `, ` (RFC 9110 §5.3), which names no version
   (D201).
5. `GET` and `DELETE` are legacy only: without `Mcp-Session-Id`, or with `MCP-Protocol-Version: 2026-07-28`
   (whose session id rule 2 never reads), they are `405` (D201).

**Checks on a modern request.** `params._meta["io.modelcontextprotocol/protocolVersion"]` and
`params._meta["io.modelcontextprotocol/clientCapabilities"]` must be present, else `400` with `-32602`; a
`protocolVersion` that differs from the header is `400` with `-32020` (`HeaderMismatch`, 2026-07-28 Streamable
HTTP, "Protocol Version Header"; D176). `Mcp-Method` must equal `method`, and `Mcp-Name` must equal `params.name`
(`tools/call`, `prompts/get`), `params.uri` (`resources/read`) or `params.taskId` (`tasks/get`, `tasks/update`,
`tasks/cancel`), after decoding its Base64 form; a missing, malformed or different header is `400` with `-32020`
(`HeaderMismatch`), before policy. Policy and metering read the body only (§11.3, §11.4). A modern client sends no
JSON-RPC responses: one is `400` with `-32600`. A modern notification is `202` and is not forwarded (2026-07-28
defines none on Streamable HTTP; a client cancels by closing its response stream, and the gate then closes its
upstream stream).

**Where a modern request goes.** Nothing is pinned. Each request goes to a session chosen as for an `initialize`
(§6.1; this replica's sessions first, else the peer hop of §11.2) or to the direct backend, with §6.1's check before
every stream. The gate forwards `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name` and the `Mcp-Param-*` headers with
the body, changed only by §2.5 and the rewrites below; an `Mcp-Name` on a method that names nothing in its body has
nothing to check it against, and is not forwarded (D204). It never translates between eras: an upstream that does
not speak 2026-07-28 answers with its own `400`, `404` or `405`, which the gate relays to that client with its
status and body (headers as §11.1), and a dual-era client falls back to `initialize` on that endpoint. The gate
learns no era from it (below). A service is therefore modern exactly when its upstream is. A multi round-trip
request's `requestState` passes untouched; its retry may reach another node of the service, as behind any load
balancer.

**The service's era (D177).** The gate keeps, per service, whether its upstream speaks 2026-07-28, and learns that a
service is legacy-only from its own probe only. A `server/discover` result from the upstream makes the service
modern: the one cached by the rewrite below, or, when there is none, the answer to a `server/discover` the gate
sends itself (no session headers, one per service at a time), as it drains `tools/list` itself (§11.3). A modern
request that finds the era unknown waits for that probe; after a probe that left it unknown, requests are forwarded
at once for 5 s, and the next one after that probes again (D204). An explicit legacy answer to the probe makes the
service legacy-only: a `400` with rule 3's `-32000` body (no valid session), a `404` or `405`, or a `200` or `400`
whose body is not a 2026-07-28 response (a `server/discover` result, or an error with code `-32020`, `-32021` or
`-32022`). Any other outcome (a modern error, another status, a node-written answer, a timeout) leaves the era
unknown, and the waiting request is forwarded. The gate never learns an era from the answer to a client's request: a
`400`, `404` or `405` that an upstream returns to a client's modern request is relayed to that client and changes
nothing for other principals, so no client can make a modern service legacy-only for everyone with a request its
upstream refuses. The era is kept like the tools list (10 minutes) and dropped when a node of the service connects
or disconnects, on every replica (`NOTIFY kontra_tunnel, 'mcp:<tenant_id>/<service>/<replica_id>'`, D199). While a
service is legacy-only, the gate answers every modern request to it as rule 3 says, without forwarding it: a legacy
upstream such as `server-everything@1.0.0` hands each POST without a session to a new transport that it never
closes, so every forwarded request would cost the seller's node a server, where the probe costs one per period.
Sealed services are answered the same way without asking (D162); stdio services are learned like any other, from the
bridge's answer to the probe (rule 3's body, [`tunnel.md`](tunnel.md) §10).

**Rewrites.**

- `server/discover` is forwarded. In its result, `supportedVersions` becomes the upstream's versions that the gate
  speaks plus `"2025-11-25"` and `"2025-06-18"`; `capabilities.extensions["io.modelcontextprotocol/tasks"]` is
  kept only for a `durable` service and removed otherwise; §2.5 applies; the operator's `initialize_meta`
  annotation is merged into `_meta` (§13.4); `cacheScope` is `"private"`. The upstream's capabilities are cached
  per service, refreshed like `tools/list`.
- `tools/list` is §11.3's (every page drained, filtered, annotated, one page, no cursor), with `ttlMs` the
  upstream's but at most 600000 and `cacheScope: "private"`, since the list is per principal. The modern and the
  legacy list are cached separately.
- `subscriptions/listen` that asks for `notifications.taskIds` without declaring the extension is `400` with
  `-32021` (`data.requiredCapabilities` naming the extension), as the extension requires (D176). Any other is
  forwarded with `notifications.taskIds` removed: gate-owned tasks are polled, and the gate writes nothing into a
  relayed stream (§11.7). The upstream's acknowledgement then names no task.
- `params.task`, a 2025-11-25 field, is removed. `tasks/result` and `tasks/list` are `404` with `-32601`: the
  extension removes them.
- The client's per-request capabilities are forwarded, except `extensions["io.modelcontextprotocol/tasks"]`,
  which is removed from every request the gate does not turn into a gate-owned task, so an upstream never
  answers with a task the gate does not own.

**Gate-owned tasks (D159).** The extension has no per-request opt-in and no `execution.taskSupport`: the server
decides. For `tasks: durable` the gate decides as follows; `passthrough` and `off` give modern clients plain calls
only (D161).

1. A `tools/call` whose request declares the extension, on a `durable` service whose upstream declared it in its
   cached `server/discover`, becomes a gate-owned task. Any other is a plain call (§11.4).
2. An unknown or denied tool is `-32602`, as in §11.3. Then Admission (§13.1) with operation `task`; a `quote`
   creates the task `quoted` (§13.2).
3. The gate creates the gate task (`protocol = mcp`; the work item stores the request with its `_meta`, which
   names the era) and starts `TaskLifecycle`, as in §11.5 steps 1–2.
4. Unless the task is `quoted`, the gate waits up to 2 s for it to end. If it ends, the gate answers the call
   with the outcome §11.8 stores (the `CallToolResult`, with `resultType: "complete"`, or the JSON-RPC error),
   and the client never sees a task. Otherwise it answers `CreateTaskResult`: `{"resultType": "task", taskId,
   status, statusMessage?, createdAt, lastUpdatedAt, ttlMs, "pollIntervalMs": 1000}`; a `quoted` task is
   `working`, with the approve URL in `statusMessage` and in `_meta["sh.kontra/approve-url"]` (D21). Either
   answer is sent only after the task row is committed, so a `tasks/get` for it resolves. A client that closes
   its response stream before either answer is sent has cancelled the request (2026-07-28 Streamable HTTP,
   "Cancellation") and never learned the task's id: the gate sends the task `kontra.cancel` (§11.6, D57, D178).

Every such call is a gate task, so its request and its result are kept in `work_items` and `task_events` until
the task's retention ends (§5.4), as an A2A task's are, while a plain call keeps nothing. That holds for every
modern client of a `durable` service whose upstream declares the extension; an operator who wants plain calls
there sets `tasks: off` (D178).

**Dispatch in the request's era.** The dispatch activity speaks to the upstream in the era of the request that
created the task, so a task is advertised only where the upstream supports one in that era; the legacy dispatch
is §11.5's. The modern dispatch sends stateless requests on the attempt's session (sticky after the first
upstream task id, §6.4), each with the stored client `_meta`. A `CallToolResult` answering its `tools/call` is
the terminal result. A `CreateTaskResult`'s `taskId` is the first upstream task id (§4): the activity polls
upstream `tasks/get` (`Mcp-Name` = that id) at the upstream's `pollIntervalMs`, bounded to 0.5–30 s, logging each
status change. Each new key of the upstream's `inputRequests` is logged as one `input_request` event, whose gate
id is `k.<taskId>.<seq>`; the upstream key is kept in the event and never shown. An `InputRequiredResult`
answering the dispatch's `tools/call` (multi round-trip requests) is logged alike, and once every request it
listed is answered the call is sent again with a new JSON-RPC id, the answers as `inputResponses` and the
upstream's `requestState`. The first `InputRequiredResult` counts as the first upstream task id (§4; the
attempt records its first `inputRequests` key in its place), so the call is never sent afresh after it: a tunnel
lost later fails the task `tunnel_lost`, as for an upstream task (D178). Answers to a task's requests go upstream
as `tasks/update` under the upstream keys. A terminal upstream status ends the gate task: `completed` logs its
`result`, `failed` its `error`, and `cancelled` is `canceled`. A call sent again with its answers is never
repeated after an exchange that may have reached the upstream (the task fails `upstream_unreachable`, or
`tunnel_lost`); only a refusal given before it ran (`full`, `408`, `429`) is sent again. A re-attach that finds a
multi round-trip first id and no logged round to answer (or a round that listed only a `requestState`) sends the
upstream nothing and fails the task `upstream_error` (D205).

A plain call answered by an `InputRequiredResult` has no outcome yet: its `calls` row has `is_error` NULL, as a
passthrough `CreateTaskResult`'s has (§11.5), and the call sent again with its answers has its own row (D204).

**The client side.** It reads only Postgres, on any replica. A request naming a task must declare the extension,
else `400` with `-32021` (`MissingRequiredClientCapability`, `data.requiredCapabilities` naming the extension,
D176); a task of another buyer, or an unknown one, is `-32602`, as in §11.6.

| Method | Gate answers |
|---|---|
| `tasks/get {taskId}` | `resultType: "complete"` and the task: `taskId`, `status`, `statusMessage?`, `createdAt`, `lastUpdatedAt`, `ttlMs` (§11.6's `ttl`), `pollIntervalMs: 1000`, the operator's `mcpMeta` in `_meta`. `quoted`, `submitted` and `working` are `working`; `input-required` is `input_required`, with `inputRequests` = every `input_request` event without an `input_response`, keyed by gate id. A terminal task is presented by its terminal event, not its gate state: a stored `CallToolResult`, whatever its `isError`, is `completed` with `result`; a stored JSON-RPC error or a failure the gate wrote is `failed` with `error` in §11.8's forms (`sh.kontra/failure` also in `_meta`); `canceled` is `cancelled` |
| `tasks/update {taskId, inputResponses}` | Each key naming an outstanding input request of the task is logged once as an `input_response` event, under §11.5 step 5's rule, and `kontra.input_response {seq}` is signalled; other keys are ignored. Empty result, `resultType: "complete"` |
| `tasks/cancel {taskId}` | On a task that is not terminal, `kontra.cancel` is sent (§11.6, D57) and not awaited; on a terminal task, nothing. Empty result, `resultType: "complete"` |

Its buyer may read a task from either era, and each era presents the same log: a legacy reader gets input
requests as server-to-client requests whose JSON-RPC id is the gate id (§11.7), a modern reader gets them in
`inputRequests`; for a result with `isError: true`, `failed` or `completed` follows §11.8 for a legacy reader
and the table above for a modern one.

**`per_session` (D160).** A modern client has no session, so the gate keeps one **virtual session** per service and
principal: an `mcp_sessions` row with `protocol_version = '2026-07-28'`, a `client_sid_hash` over a random id the
gate never sends, and no upstream or tunnel ids. Admission with operation `session` (§13.1) runs before a
principal's first modern request to a modern service is forwarded, and the row is opened once the upstream has
answered that request with a modern result (D177), or once the request was answered from the modern `tools/list`
cache or became a gate-owned task, both of which follow the upstream's own modern answers (D204); a probe that ends
in the legacy `400` opens nothing. A unique index allows one open virtual session per principal and service: of two
first requests in flight at once, the one whose insert loses reads the winner's row and names it. It is closed
`idle` after the service's `session_idle_s` without a request, and its `last_seen_at` is written at most once a
minute. It pins nothing. It is the unit that `per_session` prices (`idleTimeoutSeconds` is the service's
`session_idle_s`), that Admission's `session` operation decides, and the `mcp_session` of its `calls` rows.

**Not offered to modern clients in v4.0 (D161, D162).** Tasks of `passthrough` services; `notifications/tasks`;
resumable streams; `-32042`, never emitted on a modern request; **sealed** services, to which every modern
request is answered as rule 3 above, because envelope v1 binds a 2025-11-25 session
([`ext-sealed.md`](ext-sealed.md) §2–§3), so a dual-era client falls back; and **stdio** services, whose bridge
on the node is legacy only ([`tunnel.md`](tunnel.md) §10) and answers a modern request the same way.

**One origin, several eras (D177).** 2026-07-28 (Versioning, "Backward Compatibility") makes the era a property
of the server, which a client SHOULD cache per origin. The gate's origin serves modern services beside
legacy-only, sealed and stdio ones, so a client that learned "modern" from one service and does not probe again
fails on another with the legacy `400`. This is a residual of serving every service from one origin; clients
that probe per endpoint work (`mcp_era_fallback`).

## 12. The gate's own MCP endpoint: `/mcp`

The gate serves `{public_url}/mcp` as a first-party MCP server (README, "Hiring from another agent"). It is
not a tunnelled service. Same transport and session rules as §11.1–§11.2, with the gate as the server.

### 12.1 Authentication

- An API key (`Authorization: Bearer <key>`) is accepted, as on every route.
- When `auth.mcp_oauth.issuer` is configured, the gate is an OAuth 2.1 resource server for `/mcp` **and** for
  every `/mcp/{svc}`. A request without a credential is `401` with
  `WWW-Authenticate: Bearer resource_metadata="{public_url}/.well-known/oauth-protected-resource/mcp[/{svc}]", scope="mcp"`;
  that RFC 9728 document's `resource` is the route's canonical URI (`{public_url}/mcp` or
  `{public_url}/mcp/{svc}`), it names the configured authorization server, and it is served for every
  well-formed name, existing or not. A JWT is accepted only if its `aud` contains the route's canonical URI
  (RFC 8707); a wrong-audience, invalid or expired token is `401`. The PRD's `auth.oidc.audience` applies to
  A2A routes only. The authorization server is the operator's (Cloud runs one; a team gate can point at its
  IdP) and is not part of the gate (D25).
- A principal that arrived through `/mcp` with OAuth has `via = mcp`. The gate passes `via` to the hooks. An
  MCP session, like an API key, can never approve anything that moves money: that is the hooks' invariant
  (Cloud), and the gate gives them what they need to keep it.

### 12.2 Tools

All four tools declare `annotations.readOnlyHint` and `annotations.destructiveHint`. Their input schemas are
JSON Schema 2020-12 objects, published in `spec/schema/mcp-tools.json`.

| Tool | Input | Result |
|---|---|---|
| `search` | `{query?: string, kind?: "a2a"\|"mcp", price?: string, cursor?: string}` | `structuredContent: {services: [{ref, kind, name, description, available, price?}], nextCursor?}`, plus the same as a text block. `available` = at least one connected session or a direct backend. `price` is the operator's annotation, verbatim. Only services whose `allow:` admits the principal by an entry other than `anonymous`, plus services the operator marks `listed`; services of an anonymous tenant never appear. |
| `describe` | `{service: string}` | Only for a service `search` would return to the principal, else the unknown-service error. The served card (A2A) or the cached, filtered, annotated `tools/list` (MCP; never an upstream call per request), `sealed`, `available`, and the URLs to call it directly (`{public_url}/agents/…` or `/mcp/…`). |
| `hire` | `{service: string, message: string \| {parts: Part[]}, contextId?: string}` | Task-augmented (`execution.taskSupport: "optional"`): a `CreateTaskResult`. Plain: a `CallToolResult` with `{taskId, state, approveUrl?}` (§12.3). Only `kind: a2a` services: MCP services are called directly at `/mcp/{svc}`. |
| `reply` | `{taskId: string, message: string \| {parts: Part[]}}` | Sends a follow-up to a task in `input-required` (§7). Answers the task's new state. |
| (MCP Tasks) | `tasks/get`, `tasks/result`, `tasks/list`, `tasks/cancel` | §11.6, for tasks created by `hire`. `tasks/result` returns the task's final message and artifacts as MCP content: text parts as `text`, data parts as `text` (JSON) and in `structuredContent`, file parts as `resource` blocks. For `hire` tasks this replaces §11.8: every terminal state answers a `CallToolResult`, never a JSON-RPC error; one that ended `failed`, `rejected` or `canceled` has `isError: true`, the final status message as text, and `sh.kontra/failure` or `sh.kontra/canceled` in its `_meta` (and in `tasks/get`'s) as in §11.8. |

`hire` creates an ordinary gate task (`protocol = a2a`, dispatched to the A2A service exactly as §6). The
thread in the Cloud console, the A2A `SubscribeToTask` stream and the MCP `tasks/*` all read the same log, so
a task can be followed and answered from any of them, in seq order.

### 12.3 `hire` and approval

1. Admission (§13.1) with operation `task` and `via`. An allow decision: the task starts `submitted` and
   `hire` answers as above.
2. A quote decision (§13.2): the task is created `quoted`. Then:
   - **The client declared `elicitation.url`:** `hire` answers with the JSON-RPC error `-32042`
     (`URLElicitationRequiredError`), `data.elicitations = [{mode: "url", elicitationId, url: <approveUrl>,
     message}]`. The gate stores `(elicitationId, task, mcp_session)` for the session that received the
     error. When the task leaves `quoted` for any reason (accepted, declined, expired), the gate sends
     `notifications/elicitation/complete {elicitationId}` on that session's `GET` stream, if one is open, and
     never on another session.
   - **Otherwise:** `hire` answers at once (task-augmented: `CreateTaskResult` with `status: "working"` and
     the URL in `statusMessage`; plain: a text block with the URL and the task id). No input is requested from
     the client, so a client that cancels input-requiring tasks when it is not interactive does not decline
     the quote by accident (D21).
   - **Retries.** For a `hire` task, `first_message_id` is the `messageId` the gate mints for the A2A message it
     builds, and `first_request` = SHA-256(utf8(service ref) ‖ 0x00 ‖ JCS(arguments)), which replaces §15's
     request hash for these tasks. A `hire` that matches an existing task on `(tenant, service, buyer_id,
     principal, first_request)` while it is `quoted`, or within 10 minutes after it left `quoted` (the client's
     retry after a URL elicitation completes), answers that task in its current state: still `quoted`, the same
     `-32042` with the same `elicitationId` to a session that declared `elicitation.url` and otherwise the answer
     above; accepted, the result of §12.2; declined or expired, a `CallToolResult` with `isError: true` saying so
     (task-augmented: the `cancelled` task). Any other `hire` is a new task.
3. The URL is the operator's (Cloud: the thread, `https://app.kontra.sh/#/t/<taskId>`, D188). It carries no
   credential, and the page that serves it must check that the person who opens it owns the principal that
   called `hire` (MCP elicitation, URL mode, safety rules). Accept: the hold, then `submitted`. Decline or
   quote expiry: `canceled`, nothing held, nothing created at the payment provider.

### 12.4 A `hire` task that asks a question

For a `hire` task, `statusMessage` is always the text parts of the A2A `status.message`.

- **`input-required`, the client declared `elicitation` (form), and the status message has only text
  parts:** `tasks/get` shows `input_required`, and `tasks/result` delivers one `elicitation/create {mode:
  "form", message: <that text>, requestedSchema: {type: "object", properties: {reply: {type: "string"}},
  required: ["reply"]}}` carrying the related-task `_meta`. `accept` becomes the §7 follow-up exactly as
  `reply` would; `decline` and `cancel` leave the task waiting.
- **Every other case** (`input-required` without that capability or with non-text parts): `tasks/get` shows
  `working`, with `statusMessage` = the agent's text and ` (answer with the reply tool, taskId <id>)`, and
  `_meta["sh.kontra/input-required"] = true`. The task is not `input_required` on MCP there, because MCP would
  have the client block on `tasks/result` for a request the gate has no way to deliver (D58).

## 13. Hook points

The open-source gate contains no money code (PRD §13). It has three hook points: **Admission** (may this
request create a task, a session or a call, and on what terms), **Lifecycle** (what happens around a task's
end, including a review window) and **annotations** (operator data the gate shows without interpreting). Each
has a built-in implementation, which is what open source ships, and a **remote** one, which is how an
operator's control plane (Kontra Cloud) fills it **without changing or rebuilding the binary** (D26). Go
`internal/` packages cannot be imported by another module, and Cloud's settlement code is Python, so "the same
binary with two hooks filled in" (PRD §13) is done by configuration, not by compilation. The payloads are JSON
with the camelCase keys of [`schema/hooks.json`](schema/hooks.json); durations are integer seconds.

`allow:` lists, per-tool policy, Kontra's reserved keys (§2.5) and edge limits are always enforced by the gate
itself, before any hook.

### 13.1 Admission

`Admit(AdmitRequest) → Decision`, called before any task, MCP session or call exists:

- `AdmitRequest = {operation: "task" | "session" | "call", taskId (pre-minted, for "task"), service, principal
  {kind, id, buyerId, tenantId, via}, protocol, extensions, contextId, message {messageId, metadata, parts
  [{mediaType, size, sealed}]} (never content), tool}`.
- `Decision = {outcome: "allow" | "deny" | "quote", refusal {code, message}, metadata, mcpMeta, quote
  {approveUrl, expiresAt}}`. `metadata` keys must be under an operator prefix (§13.2) and `mcpMeta` keys under
  `sh.kontra/`; both at most 16 KiB.

Rules:

- **Built in (open source):** `allow` for every request that passed the `allow:` list.
- **Remote:** `hooks.admission: {url, secret_env, timeout: 5s, operations: [task]}`. The gate `POST`s the
  `AdmitRequest` to `url` with `Authorization: Bearer <secret>` and expects a `Decision`. `url` must be
  `https://` unless its host is loopback. A timeout, a non-`2xx` or an unparsable answer is a deny with
  `-32603` and message `admission unavailable` (HTTP `503`): admission **fails closed**. `operations` says which
  of `task`, `session` (MCP `initialize`) and `call` (plain MCP `tools/call`) consult the hook.
- **A `quote` is valid only for operation `task` on a gate-owned task** (an A2A task, or an MCP task with
  `tasks: durable`). For `call`, `session`, or a passthrough task-augmented call it is treated as a deny,
  `-32603` `priced tasks need tasks: durable`.
- **Admission decides; it never moves money.** Nothing makes the hook atomic with the task's creation: the
  gate can stop between the answer and the task row. Money moves only where a workflow exists, on approval
  (§13.2) or in the Lifecycle hooks (§13.3). An operator that records anything under the pre-minted `taskId`
  treats an id that does not appear in `kontra.tasks` within 10 minutes as abandoned (D45).
- The URL is the operator's own control plane, configured by the operator. It is never derived from a request,
  and it is not a node: the gate still never dials a node.

### 13.2 Quotes and approval

A `quote` decision creates the task `quoted`, with the decision's `metadata` and `mcpMeta` plus
`kontra.approve.url` (A2A) / `sh.kontra/approve-url` (MCP); nothing is dispatched. `TaskLifecycle` then waits
for one of:

- the Update **`kontra.approval {accepted: true, metadata, mcpMeta}`**: the metadata is applied (one `metadata`
  event), the task moves to `submitted` (its `task.timeout` clock starts, §5.2) and dispatch begins;
- `kontra.approval {accepted: false}`: `canceled`, `metadata["kontra.canceled"] = "declined"`;
- the quote's `expiresAt`: `canceled`, `metadata["kontra.canceled"] = "quote_expired"`.

`kontra.approval` is an Update, so the control plane learns synchronously whether its approval beat the
expiry; the validator refuses it once the task has left `quoted`, and refuses metadata keys outside the
operator's prefixes (`operator_metadata_prefixes`, default empty; Cloud sets `["kontra.escrow."]`; each lies
under `kontra.` and is longer than it, so §2.5's stripping vouches for its keys, D113) and
`kontra.keyRetainUntil`, or a map over 16 KiB. The same key and size rules apply to Admission decisions,
`kontra.annotate` and Lifecycle patches. A control
plane that places a hold before approving refunds it when its approval is refused. A caller's message to a
`quoted` task is refused (§7): approval does not happen in the protocol; it happens on the operator's page, by
a person.

### 13.3 Lifecycle

Five activities of `TaskLifecycle`, each `(TaskRef, args) → result`, all JSON:

| Activity | Args | Result | When |
|---|---|---|---|
| `kontra.lifecycle.OnDispatched` | `{attempt}` | `null` | on the `kontra.dispatched {n}` signal the dispatch sends when it records an attempt's first upstream task id |
| `kontra.lifecycle.OnTerminal` | `{}` | `Patch` | once, after the terminal event is in the log |
| `kontra.lifecycle.ReviewWindow` | `{}` | `{open, seconds}` | after `OnTerminal`; `seconds` ≤ 604800 |
| `kontra.lifecycle.OnReviewMessage` | `{workItemId, messageId, metadata}`, where `metadata` holds only the message's `caller_metadata` keys (§2.5), at most 16 KiB | `{accept, dispute, patch}` | inside each `kontra.review_message` Update while the window is open (§7) |
| `kontra.lifecycle.OnReviewClosed` | `{disputed}` | `Patch` | when the window ends, or at once after a message the hook marked `dispute` |

`TaskRef = {taskId, tenantId, service, kind, protocol, buyerId, principal, via, contextId, state, metadata}`,
built from Postgres by a `task_ref` activity right before each hook call (never kept in workflow state; as the
hook's input it is in history, D114, and so is its `contextId`, D115), whose `metadata` holds only the
operator-written keys (§13.2 prefixes, at most 16 KiB), never caller or service metadata. `Patch = {metadata,
mcpMeta, holdOpenSeconds}`, each map at most 16 KiB; a patch over the limit or with a key outside the rules of
§13.2 fails its hook activity non-retryably and is dropped (a metric and an `audit` row). A patch is applied as
one `metadata` event; `mcpMeta` is
rendered verbatim into the `_meta` of `CreateTaskResult`, `tasks/get` and `tasks/result` for that task (Cloud
writes `sh.kontra/escrow` there itself; the gate never builds it, D51). A node keeps a hired job's key until
the task is terminal and the time in the task's `kontra.keyRetainUntil` metadata (RFC 3339 UTC; the operator may
always write this key), if any, has passed ([`tunnel.md`](tunnel.md) §2.3).

- **Built in:** no-op activities with the same type names on `kontra-lifecycle`; `ReviewWindow` answers
  `{open: false}`, so **in open source `OnTerminal` is the last hook that fires**.
- **Remote:** `hooks.lifecycle: {task_queue: kontra-settlement}`. The five activities are scheduled by type
  name on that queue; any Temporal SDK can serve them (Cloud's are Python). Retry: unlimited, 1 s doubling to
  5 min: a hook that is down delays settlement and never skips it. The gate exports the age of the oldest
  pending hook activity as a metric to alert on (a typo in a type name is retried forever, by design). The
  hook configuration in force when a task started is snapshotted into its workflow input.
- **`kontra.annotate {metadata, mcpMeta}`** (an Update, with the §13.2 rules) may be sent by the operator at any
  time while the workflow runs; **`kontra.close`** ends the hold that `holdOpenSeconds` started after
  `OnReviewClosed`; the workflow then completes as §14.1 says, once no `push_webhook` it started is pending (D78).
- **Ordering.** Patches and annotations that arrive while a dispatch, `abort` or `forward_cancel` is in flight
  are held in workflow state and applied when it returns: the lifecycle never writes a task row while an a2a-go
  execution holds it (a version bump under a running execution would abort it).

Cancel never races money: the gate writes `canceled` first and only then calls `OnTerminal`. The hook learns of
a terminal state only after it is in the log.

### 13.4 Annotations

Operator data the gate attaches without interpreting:

| Where | Key | Shows as |
|---|---|---|
| `services.annotations` | `card_extensions` | Appended to the served card's `capabilities.extensions` (§3), e.g. Cloud's `https://kontra.sh/ext/escrow/v1` entry with its `params`. A `required: true` entry is enforced like any other (§2.4). |
| | `tool_meta` | `{"<tool name>" \| "*": {<sh.kontra/… keys>}}`, applied to `tools/list` entries' `_meta` (§11.3), e.g. `sh.kontra/price`. |
| | `initialize_meta` | `{<sh.kontra/… keys>}`, applied to the `initialize` result's `_meta` (§11.2), e.g. a `per_session` price. |
| | `search` | The `price` and `price_model` strings `search` returns and filters on, verbatim (§12.2). |
| `services` | `listed` | Whether `search` shows the service to principals outside its tenant, and whether its cards are public (§2.1). |
| gate config | `caller_metadata` | `kontra.` keys callers may write (§2.5), e.g. Cloud's `kontra.escrow.dispute`. |
| gate config | `operator_metadata_prefixes` | The metadata prefixes Admission, Updates and patches may write, besides `kontra.keyRetainUntil` (§13.2). |

### 13.5 What an operator reads

The gate's tables, in the Postgres schema `kontra` (D37), are the operator's to read: `tasks`, `task_events`
(metadata only, never content for a sealed service), `calls`, `mcp_sessions`, `tunnels`. Never
`push_configs.config` (callers' webhook credentials) or `work_items.payload` (request content). `calls` rows are
the operator's to delete; the gate never deletes them. Cloud's own
tables live beside them in their own schema and are migrated by Cloud (PRD §10).

No protocol surface lists tasks across buyers: A2A `ListTasks`, MCP `tasks/list` and every other caller read
are scoped to the caller's own `buyer_id` (PRD §12). The **owner of a service** is different: the operator's
own API may show a tenant the tasks and calls of the services it owns (who called, when, state, metadata;
never content), which is what "served by my nodes" needs (D39). That is the operator's read of the gate's
tables, not a gate route.

In Cloud the services, enrollments and keys are written by the control plane into the gate's `services`,
`enrollment_tokens` and `api_keys` tables (as sha256, with prefix, `buyer_id` and scopes). A change to
`services` is followed by `NOTIFY kontra_services, '<tenant>/<name>'`, which every replica applies without
restart. Team gates load `services:` from `kontra.yaml` at start and on SIGHUP. A multi-tenant gate with a
`services:` block refuses to start, and a `services:` entry naming a row the operator wrote is refused (D116). A
service removed or disabled answers as an unknown service at once, and its tasks in flight fail
`service_unavailable` at their next dispatch or `forward_cancel` (D117).

## 14. `TaskLifecycle`

Postgres is the record and the log; Temporal is the clock and the durable dispatcher. **Events never enter
Temporal history**: the workflow's inputs, updates, signals and activity results carry ids, states, small
outcomes and operator patches of at most 16 KiB; relayed events and caller or service content never do. The
caller-authored values that cross history are a review-window message's `caller_metadata` keys, at most 16
KiB (D59), and the two identifiers the hook contract names: the caller's `contextId` in every `TaskRef` (§13.3)
and a review-window message's `messageId` (D115).

### 14.1 Workflow, updates and signals

One workflow type, `TaskLifecycle`, id `task-<task_id>`, in the gate's own namespace (`temporal.namespace`,
default `kontra`), on queue `kontra-lifecycle`. Its start input is `{taskId, timeoutSeconds, retries, hooks}`:
everything it branches on comes from its input or from activity results, never from configuration read inside
the workflow, so a config reload never changes a running task. `tasks.deadline` is written only by the store, at the
transition into `submitted` (§5.2); the approval handler's `submitted` write returns it, `prepare` returns it for a
task created `submitted`, and the workflow sets its timeout timer to that stored value.

Interactions that must be refused synchronously are **Updates** with validators (D41); fire-and-forget ones
are signals:

| Name | Kind | Payload → result | Sent by | Validator / effect |
|---|---|---|---|---|
| `kontra.message` | Update; the first one starts the workflow (`UpdateWithStart`, conflict policy use-existing, reuse policy reject-duplicate) | `{workItemId}` → `{turn}`; `UpdateID = workItemId` | the work queue's `Write` (§15) | §7's table, on workflow state only: refused unless the task is `input-required` (or new), no turn is running, and the `workItemId` has not been accepted in this run (below, D66); a follow-up to a turn end the workflow has not processed yet is told apart from a final refusal, and sent again (§7, D105) |
| `kontra.cancel` | Update, awaited up to 60 s | `{}` → `{outcome: canceled \| not_cancelable \| unsupported \| continues, upstreamError?}` (D107) | the cancel interceptor (§8) and MCP `tasks/cancel` (§11.6) | refused only for a terminal task; concurrent cancels share one result |
| `kontra.approval` | Update | `{accepted, metadata, mcpMeta}` → `{state}` | the operator's control plane (§13.2) | refused unless `quoted`; metadata keys and size per §13.2 |
| `kontra.review_message` | Update, `UpdateID = workItemId` | `{workItemId, messageId, metadata}` → `{accepted, seq?}` | the review-window interceptor (§7) | validator: refused unless the window is open and the `workItemId` has not been accepted in this run; before `ReviewWindow` has answered, accepted, and the handler waits for the window, answering `accepted: false` if it does not open (D106). Handler: runs `OnReviewMessage` (start-to-close 8 s, no retry; a failure or timeout is `accept: false`); on accept it appends the message from the work item (`message` event, after-terminal write), applies the patch and returns the `seq`. The append loads the work item by `(id, taskId)` and consumes it in the same transaction; an item that is missing or already consumed completes the Update with `accepted: false` (D66) |
| `kontra.dispatched` | signal | `{n}` | the dispatch activity, on an attempt's first upstream task id | runs `OnDispatched` |
| `kontra.push` | signal | `{configId, deleted}` | the push config store (§10.2) | starts or ends a `push_webhook` |
| `kontra.annotate` | Update | `{metadata, mcpMeta}` → `{}` | the operator's control plane (§13.3) | the §13.2 key and size rules |
| `kontra.input_response` | signal | `{seq}` | the MCP relay (§11.5); updates workflow state only | — |
| `kontra.close` | signal | `{}` | the operator's control plane (§13.3) | — |

**Validators and work items.** A validator runs on the workflow task thread, so it is read-only and does no I/O:
it checks workflow state only (§7's state table, no turn running, and a `workItemId` not already accepted in this
run, which the workflow keeps in its state). Work-item ownership is checked where the item is used. The first
dispatch of a turn loads the work item by `(id, taskId)` and consumes it (`consumed_at`) in Postgres; a later
dispatch of the same turn (a retry, attempt `n + 1`, or a resume) may reload a consumed item it owns, and any
other dispatch that finds its item consumed refuses it rather than send it twice (D101).
`kontra.review_message` has no dispatch: its handler's append loads and consumes the item (above). A refused
`kontra.message` or `kontra.review_message` deletes its work item. A caller-side timeout (the Update not accepted
within 10 s, below) does not: the workflow may still accept the Update, and the turn then runs. An unconsumed
work item stays until its task ends, and retention deletes it with the task (§5.4, D66). A first dispatch whose
work item is missing or belongs to another task fails the task `failed: lifecycle_unavailable` (D63, D66).

An Update needs a `kontra-lifecycle` worker to accept it: the gate waits 10 s for acceptance and maps a timeout
to `-32603` / HTTP `503`. Once accepted, `kontra.cancel` is awaited up to 60 s, past which the caller gets the
stored task as it is (§8); every other Update is awaited up to 10 s in all. Sequence: (`quoted`: wait for approval or expiry) → attempt loop →
terminal → `OnTerminal` → (review window) → the end, once no `push_webhook` it started is pending (§10.2,
D78). `task.timeout` is a workflow timer at the deadline, never Temporal's own execution timeout (which would end
the run without writing `failed: timeout` or calling `OnTerminal`). A task takes at most 1,000 turns (each is
about 11 history events; Temporal allows 2,000 Updates per run); the 1,001st fails `upstream_error` with that
reason. v4 needs no continue-as-new.

### 14.2 Activities

| Activity | Queue | Options | Does |
|---|---|---|---|
| `prepare(taskId, n)` | `kontra-lifecycle` | start-to-close 10 s; retried, max interval 30 s | Picks the backend and session (§6.1, D145); writes the attempt row; returns `{queue, sessionId}` |
| `dispatch(taskId, n, workItemId, resume)` | `kontra-tunnel-<replica_id>` or `kontra-direct` | schedule-to-start 15 s; heartbeat timeout 30 s, beats every 5 s; start-to-close = time to deadline + 30 s; `MaximumAttempts: 1`; `WaitForCancellation` | Writes `attempts.replica_id` and a fresh `attempts.dispatch_id` first, as every dispatch does (a new attempt, a follow-up turn, a resume), which fences every earlier dispatch of the task; every append it makes is fenced (§6.4, D77). Loads the work item by `(id, taskId)`, consuming it on the turn's first dispatch (§14.1, D66), and runs a2a-go's handler for it (§15) with the relay executor; `resume` re-attaches (§6.4). Discards the handler's result and returns `{outcome, reason, retryAfterSeconds}` only: the failure reason, never its text, and an upstream's `Retry-After` (D102) |
| `abort(taskId, n)` | `kontra-ctl-<attempts.replica_id>` | schedule-to-start 10 s, start-to-close 10 s | Cancels the running dispatch through the replica's local registry (§8). If no replica is recorded yet, the workflow instead cancels the scheduled dispatch through Temporal, which completes at once for an activity that has not started. Not started within 10 s (the replica is gone): for A2A `CancelTask` the workflow waits for the dispatch's heartbeat timeout, then applies §8's table; an MCP `tasks/cancel` keeps §11.6's 10 s (§8, D79) |
| `forward_cancel(taskId, n)` | tunnel: `kontra-ctl-<the session's replica>`; direct: `kontra-ctl-<attempts.replica_id>` while a dispatch runs, falling back to `kontra-direct-ctl` when it is not started within 10 s (§8, D79); else the shared `kontra-direct-ctl` every replica's control worker polls | schedule-to-start 10 s, start-to-close 30 s, `MaximumAttempts: 1` | Sends `CancelTask` (or MCP `tasks/cancel`) upstream inside the tunnel or to the direct URL; returns the upstream's answer. Through the tunnel it answers `tunnel_lost`, opening no stream, when its replica does not hold the session (§8, D148), and sends a cancel the node answers `full` again for up to 20 s (D147) |
| `attempt_state(taskId, n)` | `kontra-lifecycle` | 10 s, retried | Reads `first_upstream_id_at` and `replica_id` from Postgres: the retry decision is taken on what Postgres recorded, never on heartbeat details, which a crash loses; the workflow reads it before scheduling `abort` or `forward_cancel` |
| `list_push_configs(taskId)` | `kontra-lifecycle` | 10 s, retried | Returns the task's live push config ids (§10.2) |
| `write_terminal(taskId, state, reason)` | `kontra-lifecycle` | 10 s, retried | Writes the terminal event through the store when no handler can: retries exhausted, owner replica dead, timeout, the cancel of an `input-required` task |
| `apply_metadata(taskId, patch)` | `kontra-lifecycle` | 10 s, retried | One `metadata` event; the patch crosses history inline, at most 16 KiB (D59) |
| `push_webhook(taskId, configId)` | `kontra-push` | heartbeat 60 s; start-to-close = deadline + 24 h; retried | §10.2 |
| hooks (§13.3) | built in: `kontra-lifecycle`; remote: the operator's queue | unlimited retry, 1 s to 5 min | §13.3 |

**Per-replica queues** are what let several gate replicas and several tunnels coexist: a session's socket lives
on one replica, and only that replica polls `kontra-tunnel-<replica_id>` and `kontra-ctl-<replica_id>` (a
`replica_id` is unique per process start). When a replica dies its queues have no poller: a new `dispatch` fails
its schedule-to-start (15 s, never retried by Temporal), a running one its heartbeat timeout (30 s after the last
heartbeat that reached the server; measured 18–25 s after a SIGKILL), so a dead replica surfaces within a minute
(`replica_kill`) and the workflow consults `attempt_state` and the retry table (§6.4). A stalled replica (`SIGSTOP`,
a GC pause) keeps its open long polls, so a resume dispatch can be handed to it and time out in turn: its dispatch
is taken over within two heartbeat rounds rather than one (D120). Activities on a
per-replica queue never rely on Temporal's own retries, which go back to the same queue.

Worker settings: dispatch workers (`kontra-tunnel-<id>`, `kontra-direct`) are activity-only, with an explicit
`MaxConcurrentActivityExecutionSize` (`limits.dispatch_slots`, default 10,000) so load never looks like a dead
replica, and `MaxHeartbeatThrottleInterval` 10 s; the control worker (`kontra-ctl-<id>`) has its own slots, so
`abort` and `forward_cancel` are never queued behind dispatches; `kontra-push` has `limits.push_slots` (default
2,000). A dispatch that finds its session missing from the replica's registry fails `tunnel_lost` at once, opening
no stream; after the first upstream task id it logs `failed: tunnel_lost` itself, under its fence (D149). The
gate's Temporal server runs with `frontend.WorkerCommandsEnabled` and `system.enableCancelActivityWorkerCommand`
on, so Temporal's own activity cancellation reaches a worker without waiting for a heartbeat; `abort` does not
depend on it.

### 14.3 Draining

On `SIGTERM` a replica marks itself draining (`gate_replicas.draining_at`), fails its readiness check, stops
accepting tunnels, tells its connected nodes to reconnect elsewhere once that mark has committed
([`tunnel.md`](tunnel.md) §13, D141), and stops being selected for new attempts: §6.1 skips it for tunnel
sessions, and it stops its `kontra-direct` pollers at once, with `WorkerStopTimeout` = `drain_timeout`, so the
dispatches already running keep running and heartbeating. It **keeps polling** `kontra-lifecycle`, `kontra-push`,
`kontra-direct-ctl` and its per-replica queues (`kontra-tunnel-<id>`, `kontra-ctl-<id>`) until its last dispatch
has ended or `drain_timeout` (default 10 min) passes, so that, on a gate of one replica too, Updates such as a
cancel are still accepted, dispatch results are recorded, and `abort` and `forward_cancel` for its tasks run. Only
then, after a 2 s settle in which the last dispatch's result is recorded (D118), does it stop them
(`WorkerStopTimeout` a few seconds) and exit (D76). A tunnel dispatch that starts during the settle (a follow-up)
is waited for, within the same `drain_timeout`, and the settle runs again; once none has started, the replica
closes `kontra-tunnel-<id>`, so that a dispatch its worker still starts returns at once having sent nothing, and
the worker stops within 5 s (D150). Its public listener keeps serving during the drain (`/healthz`
answers `503`) and closes after the workers stop, giving open streams 5 s; their callers resume elsewhere with
`Last-Event-ID` (D119). Kubernetes must give it `terminationGracePeriodSeconds` ≥ `drain_timeout` + 30 s. A task
still dispatching on the drained replica when `drain_timeout` ends is a lost session like any other (§6.4):
rolling deploys do not cut tasks that finish within `drain_timeout`.

### 14.4 Versioning and access

- Gate workers are unversioned. Every change to `TaskLifecycle`'s behaviour goes behind `workflow.GetVersion`,
  and CI replays recorded histories (`worker.NewWorkflowReplayer`). Worker Deployment Versioning does not fit
  per-replica queues (at most 100 queues per deployment version, D46).
- The namespace is the gate's own: no node or caller ever reaches Temporal. Its only other clients are the
  operator's (control-plane Updates and signals, remote hook workers). Temporal authorizes by namespace, not by
  queue or Update name, so **any credential for the gate's namespace is equivalent to the gate's own authority
  over every task**: it is held only by the operator's workers and control-plane backend, never by a tier that
  serves browsers, and they are deployed as one trust domain with the gate. A self-hosted frontend has no
  authorizer, so its port is never published (compose keeps 7233 on the internal network), and an operator's
  workers off the host use mTLS or Temporal Cloud API keys.

### 14.5 Orphans

The work queue writes the task row before it starts the workflow (§15). A resend that matches an existing task
re-issues `UpdateWithStart` with the original work item id as `UpdateID` (idempotent), so a task whose start
was lost gets its workflow. A sweep on any replica, once a minute, ends tasks in `submitted` or `quoted` older
than 2 minutes whose workflow does not exist with `failed: lifecycle_unavailable`, written through the store.
Work items are not swept: an Update not accepted within 10 s keeps its work item, because the workflow may still
accept it, and an unconsumed item is deleted with its task (§5.4, §14.1, D66).

## 15. How the gate sits on a2a-go (informative)

This section records the implementation plan the M0 probes support. It is not normative; §2–§14 are.

| a2a-go piece | Kontra's |
|---|---|
| `taskstore.Store` | Postgres store (§5.2). Owner scoping from `CallContext.User` (the principal's `buyer_id`); `List` scoped to `(tenant, service, buyer)`, ordered by `status_at`, with a2a-go's page-token shape; versions start at 1; `PrevVersion == 0` refused; writes on `context.WithoutCancel`. A per-process registry of running dispatches records every terminal event the relay executor yields for `(task, n)`; while a dispatch is registered, a `failed` write without a status message that the registry does not hold as relay-emitted (a2a-go's own `SetTaskFailed`, on any of its error paths, which never carries one; the relay's failures always do, D93) is refused, and the dispatch returns its outcome from the registry (`retryable`, `tunnel_lost`, `interrupted`, or `context_lost`, which for a direct backend triggers the re-attach of §6.4; a handler or executor error that is not a lost upstream context is `upstream_error`, §6.4); the workflow writes any terminal event through `write_terminal` (D8). |
| `eventqueue.Puller` | Postgres puller: created at the log's tail (`max(seq)` when the reader is created, because a2a-go always passes a nil first cursor), long-polls on `LISTEN`, skips `message` rows with `role = ROLE_USER` and `metadata` rows (a2a-go's reader stops at the first final event and would otherwise return the caller's own message), returns `eventqueue.Message{Event, TaskVersion: seq}`. Behind `eventqueue.NewPullQueueManager` with `PollInterval` 100 ms and `InactivityTimeout` 0 (the puller already polls), so a blocking `SendMessage` on a quiet task keeps waiting. |
| `workqueue.Queue` | `workqueue.NewPushQueue(kontraWrite)`. `kontraWrite` for a new task pre-creates the gate task (gate ids, `submitted` or `quoted`), stores the sanitized payload in `work_items`, calls `UpdateWithStart` (`kontra.message`) on `TaskLifecycle`, and sets `payload.TaskID`; a resend (same principal, service and `messageId`, the same request hash (below), the first task younger than 24 h) is answered with the existing task and re-issues the start (§14.5); a reused `messageId` with a different request is a new task (the TCK reuses message ids). For a follow-up it stores the work item and sends the `kontra.message` Update; a refusal becomes `UnsupportedOperationError` and deletes the work item, and an Update not accepted within 10 s becomes `-32603` (HTTP `503`) and keeps it (§14.1, D66). The returned `HandlerFn` is what the `dispatch` activity calls, with a heartbeater attached (`workqueue.AttachHeartbeater`, 5 s); the activity's context reaches the executor, so `abort` stops it. The handler's concurrency limiter is off; the Temporal worker's activity slots bound concurrency. |
| `AgentExecutor` | The relay executor (§6.2–§6.4): never yields a `Task` (the gate task exists), never yields a bare `Message`, rewrites ids deeply, strips Kontra's keys, emits explicit terminal events. |
| `a2asrv.NewHandler(…, WithClusterMode(…))` | One handler per service, built on first use and cached, with `WithPushNotifications(pgPushConfigStore, noopSender)` (push is the lifecycle's, §10) and call interceptors for authentication, admission, the follow-up state checks of §7, the quoted guard, cancel (§8), inline push configs (§10.1), and review-window messages, which the interceptor stores as a work item, sends as the `kontra.review_message` Update and answers entirely itself as an early response (a2a-go refuses messages to terminal tasks, and must never see them). A blocking `SendMessage` that a2a-go answers "queue is closed" (a turn that ended before its subscription read the store) gets the stored task (D121). |
| transports | `NewJSONRPCHandler` and `NewRESTHandler` behind the middleware of §2.2–§2.5, except `SendStreamingMessage` and `SubscribeToTask`, which the gate's own SSE writer serves (§9). |
| cards | Raw-JSON card producer (`AgentCardJSONProducer`); the gate's own JWS signer (§3), since a2a-go's `a2acrypto.Signer` has a fixed protected header. |
| client | `a2aclient` with `WithJSONRPCTransport`/`WithRESTTransport` over an `http.Client` that dials tunnel streams, no client timeout (deadlines from the context), bodies read through byte limits (§6.6), HTTP/1.1 only; `a2av0` transports for 0.3 upstreams. |

**Resend hash.** For an A2A task, `tasks.first_request` is SHA-256 of Go's `json.Marshal` of the typed
`a2a.SendMessageRequest` after the key stripping of §2.5: the whole request, any inline push config included, so
a resend that differs in anything is a new task. The encoding is tied to the pinned a2a-go structs: a gate
upgrade inside the 24 h resend window can turn a resend into a new task, which is accepted (D74). `hire` tasks
hash otherwise (§12.3).

## 16. Named tests

Each is a test in `test/e2e`; the PRD §15 matrix, note 1 §7 and note 2 §4 define the first ones, and the review
of these specs added the rest.

M1 runs `relay_resume`, `replica_kill` (direct variant), `cancel_forwarded` (direct, against an a2a-go seller that
handles cancel: a2a-cli v0.3.1's exec mode can end `failed` instead of `canceled` on a cancel), `push_delivery`,
`ssrf_webhook`, `card_compat`, `cross_buyer` and `version_conflict`. The tunnel variants and the other tests come
with their milestones (D80). M2 runs the tunnel variants of `replica_kill` (after and before the first upstream
task id) and `cancel_forwarded`, `input_required_sticky` and `key_scope`, with [`tunnel.md`](tunnel.md) §16's
`tunnel_kill` and `sidecar_not_a_proxy`, and the helloworld seller through the tunnel (D154).

| Test | Expected |
|---|---|
| `relay_resume` | §9: resubscribe with `Last-Event-ID: K` → the snapshot as of K, then every carried row after K once, in order |
| `replica_kill` | §6.4, §14.2. Two variants: **direct backend** (M1's gate): two replicas, kill the one running the dispatch mid-task; the task **completes** by re-attaching, and the caller's SSE resumes with no gap or duplicate. **Tunnel backend** (PRD §15): kill the replica holding the tunnel; the dispatch times out within 60 s; the task is retried (no upstream task yet) or fails `tunnel_lost`; the `tunnels` row is gone ([`tunnel.md`](tunnel.md) §11) |
| `input_required_sticky` | §7 |
| `cancel_forwarded` | §8, including the cancel of an `input-required` task: the upstream receives `CancelTask`; the task is `canceled`; no `completed` after |
| `push_delivery` | §10.2: webhook down for 2 min, then back: every status change and the terminal snapshot delivered, in seq order; a config created after the task started is delivered too |
| `ssrf_webhook` | §10.3: `169.254.169.254`, `10.0.0.1`, `localhost`, `[::ffff:10.0.0.1]`, `64:ff9b::a00:1` and a name resolving to a private address are refused when set, with no connection; a redirecting URL is accepted, its `3xx` is a failed delivery and the redirect target receives no connection (D60) |
| `card_passthrough` | §3: one signature (the gate's), verifiable by a2a-go's verifier; its protected header binds the upstream card by hash and says whether the seller's signature was verified against the configured keys; the upstream card is served verbatim with the seller's signature; seller fields byte-identical under JCS (D13) |
| `card_compat` | §3: a 0.2.5 helloworld upstream served as 1.0 (M1: a stand-in serving that seller's card, D122) |
| `cross_buyer` | §2.3, §4: buyer B's `GetTask` of buyer A's task is `TaskNotFound`; two buyers sending the same `contextId` reach two distinct upstream contexts |
| `key_scope` | §6.1: a session enrolled for `hello` never receives a stream for another service, and a revoked binding receives none from the next request on |
| `version_conflict` | §5.2: two `Store.Update`s with the same `PrevVersion` from two replicas: exactly one commits, the other gets `ErrConcurrentModification`; seqs 1..N have no gap. (The PRD's "409" is that store error; no route writes by version, D60.) |
| `mcp_session_cross_node` | §11.2 |
| `mcp_session_revoked` | §6.1, §11.2: revoke one binding while a session is pinned, with the node ignoring the control message: the next request is `404` and opens no stream |
| `mcp_tool_denied` | §11.3 |
| `mcp_batch_refused` | §11.1: an array of two `tools/call`, one to a denied tool, is `400` with no upstream traffic |
| `mcp_case_smuggling` | §11.1: a `tools/call` carrying `"method"` and `"METHOD"`, `"name": "echo"` (denied) and `"NAME": "add"`, a repeated `name`, or a repeated or case-variant key in `params._meta` or at the top level of `arguments`, is `400` with `-32600` and reaches no upstream, Admission or `calls` row; an upstream result `{"isError": true, "ISERROR": false}` is not relayed (`502`, `-32603`) and its `calls` row has `is_error` true |
| `a2a_case_smuggling` | §2.4: a JSON-RPC `SendMessage` carrying `"method"` and `"METHOD"`, `"tenant"` and `"TENANT"` in `params`, or a part with two `mediaType` keys, is `-32600` with HTTP `400`, before any task row |
| `mcp_meta_spoof` | §2.5: an upstream's `sh.kontra/price`, `sh.kontra/escrow`, `sh.kontra/approve-url` and `capabilities.experimental["sh.kontra/sealed"]` never reach a client; an A2A upstream's `kontra.approve.url` and `kontra.escrow.*` never reach a caller; `_meta["sh.kontra/sealed"]` on an `elicitation/create` of a service without `sealed` is removed, and on a sealed service only an exact `{v, envelope}` reaches the client |
| `mcp_task_resume` | §11.9: a replica killed during a task-augmented call; `tasks/result` still returns on the other replica, with the task's result if the killed replica held only the client's connection, with the §11.8 form of `failed: tunnel_lost` if it held the tunnel and the upstream task already existed. It never hangs. Modern variant (§11.12, D178): the tunnel killed after the upstream answered the dispatch's `tools/call` with an `InputRequiredResult` fails the task `tunnel_lost`, which `tasks/get` shows on either replica, and the upstream never receives that `tools/call` again |
| `mcp_modern_task` | §11.12: a 2026-07-28 client's `tools/call` on a `durable` service whose upstream makes a task that asks one question: `CreateTaskResult` with a gate id; `tasks/get` is `input_required` with one gate-keyed request; `tasks/update` answers it, and a second answer to the same key is ignored; `tasks/get` then shows `completed` with the result; `tasks/result` is `-32601`; another buyer's `tasks/get` is `-32602`; the answers are the same from either replica. A request whose `_meta` `protocolVersion` differs from `MCP-Protocol-Version` is `400` with `-32020` and reaches no upstream (D176). A client that closes its response stream during the 2 s wait, before the inline answer or the `CreateTaskResult` is written, is sent neither, and the task ends `canceled` within 10 s, the upstream seeing the cancel, or the abort before its first upstream id (D178) |
| `mcp_era_fallback` | §11.12: a dual-era client's first modern request to a legacy-only service, to a sealed service and to a stdio service gets the legacy `400`, and its call succeeds after `initialize`; a legacy-only upstream receives the gate's own `server/discover` and no modern request of a client (three sent within a minute), and a sealed service is never probed; a `404` that a dual-era upstream returns to one principal's modern request reaches that client, and another principal's next modern request to the service is still forwarded and answered modern (D177); against a dual-era upstream the client stays modern, pins nothing, and two concurrent first requests write one virtual session row; a client that succeeded modern on `/mcp/a` then gets the legacy `400` from a legacy-only `/mcp/b` on the same origin (the residual of §11.12, logged, not asserted as a pass) |
| `mcp_peer_hop` | §11.2: with the client routed to one replica and the node's tunnel on the other, `initialize`, a pinned `tools/call` and its SSE stream complete through the peer hop; a `Kontra-Peer` assertion that is forged, older than 30 s or replayed, a request whose upstream session id or forwarded headers were changed after signing, and any other path on the peer listener, are refused and open no stream; killing the replica that held only the client's connection leaves the session open |
| `tck_jsonrpc`, `tck_rest`, `tck_behind_caddy` | §2, §3, §9 against the pinned TCK, expected failures in `test/tck/expected-failures.txt` |
| tunnel and sealing tests | [`tunnel.md`](tunnel.md) §16, [`ext-sealed.md`](ext-sealed.md) §9 |

## 17. Configuration keys these specs add

`kontra.yaml` as in PRD §11 and note 1 §6 (`services:` replaces `agents:`), plus:

| Key | Default | Section |
|---|---|---|
| `services.<name>.kind` | — (required) | §1 |
| `services.<name>.tools.allow`, `.deny` | allow all | §11.3 |
| `services.<name>.tasks` | `durable` | §11.5 |
| `services.<name>.sealed` | `false`; `true` or `{required: bool}` (`required` defaults to `true`); tunnel backends only (D91) | [`ext-sealed.md`](ext-sealed.md) §1 |
| `services.<name>.seller_keys` | none (seller signatures not verified) | §3 |
| `services.<name>.session_idle_s` | `3600` | §11.2 |
| `services.<name>.max_sessions_per_principal` | `4` (mcp only, tunnel backends, not `anonymous`; 1 to 10000) | §11.2 |
| `services.<name>.task.timeout`, `.retries`, `.retention` | `1h`, `2`, `7d` | §5.4, §6.4, §6.5 |
| `services.<name>.listed` | `false` | §12.2 |
| `tenancy` | `single` | §2.1 |
| `tls.terminated_by_proxy` | `false` | §2.2 |
| `edge.client_ip.from`, `.trusted` | `remote`, `[]` | §2.2 |
| `limits.conns_per_ip`, `limits.max_conns` | `256`, `10000` | §2.2 |
| `caller_metadata` | `[]` | §2.5 |
| `operator_metadata_prefixes` | `[]` (Cloud: `["kontra.escrow."]`) | §13.2 |
| `limits.dispatch_slots`, `limits.push_slots`, `limits.task_bytes` | `10000`, `2000`, `256MiB` | §6.6, §14.2 |
| `push.allow_private` | `false` | §10.3 |
| `blobs` | none (parts over 256 KiB refused) | §10.4 |
| `mcp.allowed_origins` | `[]` | §11.1 |
| `peer.listen` | none: only this replica's tunnel sessions serve MCP requests; a gate of more than one replica SHOULD set it | §11.2 (D163, D179) |
| `peer.advertise` | the `peer.listen` address, or, when it names no host, the address of the interface that holds the default route | §11.2 |
| `peer.secret_env` | — (required with `peer.listen`): the environment variable holding the shared secret, at least 32 bytes | §11.2 |
| `auth.mcp_oauth.issuer` | off | §12.1 (audiences are derived per route) |
| `hooks.admission.url`, `.secret_env`, `.timeout`, `.operations` | built in | §13.1 |
| `hooks.lifecycle.task_queue` | built in | §13.3 |
| `drain_timeout` | `10m` | §14.3 |
| `tunnel.channel_binding` | `tls-exporter` when the gate terminates TLS, else `none` | [`tunnel.md`](tunnel.md) §4 |
| `tunnel.anonymous` | `false` | [`tunnel.md`](tunnel.md) §6 |
| `tunnel.enroll_with_api_key` | `false` | [`tunnel.md`](tunnel.md) §5 |

The config's JSON Schema (`kontra.sh/schema/config/v1`, PRD §11) is published with the first gate code (M1).
