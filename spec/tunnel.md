# Tunnel

Status: **approved by the owner** (25 Oct 2026, revision 3 after M1 planning). Revision 4 brings the text in line
with the M2 code (D123–D157), decided under the owner's standing instruction on 26 Oct. Revision 5 (26–28 Oct)
includes the M3 decisions: those of the M3 planning and its audit (D161, D162, D164, D172, D173, D179–D182, D184),
and those that bring the text in line with the M3 code and its review (D199, D206–D213, D224, D229), decided under
the owner's standing instruction. Revision 6 (28 Oct, M4 planning) settles anonymous sessions and `kontra login` (§6,
D277, D278), and was amended the same day after the M4 plan's audit: the hello's `anonymous` field, what a key of
an anonymous tenant may present, and the anonymous limits counted per tenant and gate-wide (§4, §6, §12; D285 to
D287). It was amended on 31 Oct after M4's review: what the `calls` limit counts, an anonymous key not rotated, the
sweeps of a gate that stopped opening anonymous sessions, what anonymous tenants can keep stored (§6, §12; D304), and
the resource an authorization server issues a key for (§6.3; D305). Normative. RFC 2119 key words.

The tunnel is how a node (`kontra connect`) and a gate (`kontra-gate`) talk: one outbound connection from the
node, over which the gate sends HTTP requests to services on the node's localhost (hosting) and the node sends
HTTP requests to the gate (hiring). It is a reverse HTTP proxy over a multiplexed WebSocket. It carries
ordinary HTTP/1.1 exchanges and does not care whether their bytes are A2A JSON-RPC, A2A HTTP+JSON or MCP
Streamable HTTP.

**It is not a protocol a service or a client speaks.** Both ends are Kontra's own binaries. A service behind a
node is a plain A2A agent or MCP server on localhost; a client is a stock A2A or MCP client pointed at
`localhost:7777` or at the gate. Nothing in this document is visible to either. How requests are relayed is
[`relay.md`](relay.md); what is sealed is [`ext-sealed.md`](ext-sealed.md). Departures from the PRD and the
notes are cited as **D*n*** ([`../docs/decisions.md`](../docs/decisions.md)).

## 0. Pins

| What | Pin |
|---|---|
| Multiplexer | `github.com/hashicorp/yamux` v0.1.2 |
| WebSocket | `github.com/coder/websocket` v1.8.15 (`websocket.NetConn`, binary messages) |
| Identity and sealing | `filippo.io/age` v1.3.2 (X25519 identities and recipients) |
| Handshake proof | Go 1.26 standard library `crypto/hpke` (RFC 9180): DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, export-only |
| Go | 1.26 or later (`crypto/hpke`; a2a-go v2.7.0 also needs it) |

## 1. Roles

| | Node (`kontra connect`) | Gate (`kontra-gate serve`) |
|---|---|---|
| Opens the TCP/TLS/WebSocket connection | **always** | **never**: the gate never initiates a connection to a node (PRD §12) |
| yamux role | `yamux.Client` | `yamux.Server` |
| Opens streams | for **hiring** (§9) and blob fetches (§8.3) | for **hosting** (§8): each inbound request to a service on the node |
| Holds keys | its identity key and per-job keys (private halves) | public keys only |

## 2. Identity and credentials

### 2.1 The identity key

On first run `kontra connect` creates `~/.kontra/identity.key` (file mode `0600`, directory `~/.kontra` mode
`0700`; `--key` or `KONTRA_KEY` overrides the path, e.g. a mounted secret in a container). It is an age identity
file, the same format `age-keygen` writes:

```
# created: 2026-10-25T10:00:00Z
# public key: age1…
# kontra key id: k1_…
AGE-SECRET-KEY-1…
```

- The **first** `AGE-SECRET-KEY-1` line is the active key: it proves possession in the handshake (§4) and is
  published as the sealing recipient for every service the node hosts (D29: one key for both, PRD §17 default).
- Later lines are decrypt-only (rotation, §12). `age -d -i ~/.kontra/identity.key` opens a sealed part for
  debugging.
- The CLI refuses to start if the file is readable by group or others.
- One identity key per node, enrolled separately at each gate the node dials (note 1 §2).

### 2.2 Key id

`key_id = "k1_" + lowercase(base32_nopad(SHA-256(utf8(recipient))))[0:26]`, where `recipient` is the canonical
`age1…` string: 29 characters, 130 bits. The gate always stores and checks the **full public key**; it never
authenticates anything by `key_id` alone. The prefix versions the derivation (a post-quantum `age1pq1…`
recipient would use the same rule).

### 2.3 Per-job keys

When the node hires a sealed service, it mints an X25519 key for that job (an A2A task, an MCP task, or an MCP
session for plain calls) and keeps it at `~/.kontra/jobs/<gate host>/<job id>.key` (0600). It is what replies
are sealed to ([`ext-sealed.md`](ext-sealed.md)). It is deleted when the job is terminal and, if the task's
metadata carries the operator's `kontra.keyRetainUntil` (a review window, [`relay.md`](relay.md) §13.3), that
time has passed; an MCP session's key when the session closes and every task created in it is done. The node
learns that the gate closed a session from a `404` for it, and deletes the key of a session that has had no
exchange for the service's `session_idle_s` once its tasks are done: the gate closes an idle session on its own, and
states that bound in the `initialize` result's `_meta["sh.kontra/session_idle_s"]` ([`relay.md`](relay.md) §11.2,
D224). The node keeps it with the job; a value that is not a whole number of seconds from 1 to 30 days, or none,
counts as a day. A session the node no longer knows (a gate that keeps sessions longer than 30 days, every sealed
session after the node restarts) is answered `404` by the node, on a `GET` as on a `POST`, without forwarding
anything, and the client initializes again. While an exchange of a session is open (a `GET` stream, a call in
flight) its key stays, whatever the sweep or a `404` on another exchange says, and goes once the last ends (D241);
a session the client left without a `DELETE` and in which no sealed call was made is forgotten once idle that long
too (D242).

### 2.4 Credentials

API keys (`kontra login`, `--api-key`) and enrollment tokens (`--enroll`) are kept per gate origin in
`~/.kontra/credentials` (0600) and sent only to the origin they were issued for: a node never sends a credential
issued by one gate to another. `--api-key` and `--enroll` accept a value, `@file` or `-` (stdin), so a secret
need not appear in a process list. The file also keeps, per gate, the SHA-256 of every token a `welcome`
confirmed, which the node never sends again, and of the tokens a hello in flight carries. A handshake whose
outcome the node did not receive (the connection cut after the proof, a `1011` close, the process stopped) is
followed by a probe: a hello with the active key alone, without `enroll`, `rotate_from`, `auth` or `anonymous`
(so a gate with anonymous sessions still answers it `unknown_key`, D285). Its `welcome`
means the change committed; `unknown_key`, or `service_denied` for a known key, means it did not, and the full
hello follows at once. Tokens refused `enroll_invalid` and an API key refused `auth_invalid` are removed from the
file (D124).

## 3. Connection

`GET {gate}/v1/tunnel` with `Upgrade: websocket`, `Sec-WebSocket-Protocol: kontra-tunnel.v1` and
`User-Agent: kontra/<version> (<os>/<arch>)`. No credential travels in a header: credentials go in the
handshake, inside TLS, so they never reach a proxy's access log.

- `{gate}` is the gate's `public_url` (`--gate`, default `https://gateway.kontra.sh`). It must be `https://`,
  except `http://localhost`, `http://127.0.0.1` and `http://[::1]` (contributors' gates).
- The gate refuses an upgrade without the subprotocol (`400`), and the node closes a connection on which the
  gate did not select it. A gate that binds `tls-exporter` (§4) also refuses an upgrade it cannot channel-bind:
  `400` when the client's TLS has no exporter (TLS 1.2 without Extended Master Secret), `500` when its own
  listener has no TLS. The node retries it as a failed dial (D135).
- The WebSocket carries binary messages only after the handshake; compression is off.

## 4. Handshake

Four WebSocket **text** messages, before yamux starts. Each is a JSON object of at most 16 KiB; both sides set
that read limit for the handshake. The whole handshake must complete within 10 s of the upgrade, or the gate
closes the connection.

**M1, node → gate (`hello`):**

```json
{"t": "hello", "v": 1,
 "origin": "https://gate.team.internal",
 "kid": "k1_abcdefghijklmnopqrstuvwxyz",
 "recipient": "age1…",
 "name": "build-box-3",
 "nonce": "<base64url, 32 random bytes>",
 "services": [{"kind": "a2a", "name": "hello"}, {"kind": "mcp", "name": "fs"}],
 "enroll": ["<one-time token>", "…"],
 "auth": "<api key>",
 "rotate_from": {"kid": "k1_…", "recipient": "age1…"},
 "public": ["hello"],
 "anonymous": true,
 "agent": "kontra/4.0.0 (linux/amd64)"}
```

| Field | Rule |
|---|---|
| `origin` | The origin the node **dialed** (`scheme://host[:port]`, lowercase, default port elided). The gate requires it to equal the origin of its `public_url`. |
| `kid`, `recipient` | The active key. `kid` must equal the derivation of `recipient` (§2.2). |
| `name` | The node's name, shown in the operator's view of nodes: `--name`, default the hostname; 1–64 characters. |
| `services` | What the node wants to host on this connection: `kind` and `name` (the name alone; the tenant comes from the key). May be empty (a node that only hires). |
| `enroll` | Optional. Enrollment tokens (§5). |
| `auth` | Optional. An API key: the principal for hiring (§9), and with an `enroll:` scope also the authority to enroll (§5) and to convert an anonymous key (§6.3). |
| `rotate_from` | Optional. The key this one replaces (§12). |
| `public` | Optional, anonymous sessions only: services that accept unauthenticated callers (§6.1). Each must be one of `services` (else `service_denied`); on any other session it is ignored. |
| `anonymous` | Optional, `true` only: the node asks for an anonymous session (§6, D285). `kontra connect` sends it only when it holds no API key and no pending or consumed enrollment token for this origin and was given neither `--enroll` nor `--api-key`; a probe (§2.4) never sends it. Without it an unknown key is `unknown_key`; a gate without `tunnel.anonymous` answers it, from a key it does not know, `anonymous_unavailable`. |
| `agent` | CLI version and platform, shown in the operator's view of nodes. |

**M2, gate → node (`challenge`):**

```json
{"t": "challenge", "v": 1, "session": "<UUIDv7>", "nonce": "<base64url, 32 bytes>",
 "cb": "tls-exporter", "enc": "<base64url, HPKE encapsulated key>", "enc_old": "<… for rotate_from>"}
```

`cb` is `tls-exporter` when the gate itself terminates TLS for this connection (the default deployment, PRD
§12), and `none` when a front proxy terminates it (`tunnel.channel_binding: none` in `kontra.yaml`). `enc_old`
is present only when M1 carried `rotate_from`.

**M3, node → gate (`proof`):** `{"t": "proof", "v": 1, "proof": "<base64url, 32 bytes>", "proof_old": "<…>"}`

**M4, gate → node:** `welcome` or `error`.

```json
{"t": "welcome", "v": 1, "session": "<UUIDv7>", "replica": "<replica id>", "ping_s": 20,
 "tenant": "acme",
 "services": [{"kind": "a2a", "name": "hello", "url": "https://gate.team.internal/agents/hello"}],
 "anonymous": {"expires_at": "2026-10-25T12:00:00Z"}}
```

`services` lists what this session will receive requests for: the declared services the key is enrolled for
(after this handshake's enrollments). `anonymous` is present only for an anonymous session (§6).

```json
{"t": "error", "v": 1, "code": "bad_proof", "message": "…"}
```

`code` is one of `unsupported_version`, `bad_origin`, `bad_proof`, `unknown_key`, `revoked`, `enroll_invalid`,
`service_denied`, `kind_mismatch`, `anonymous_unavailable`, `rate_limited`, `draining`, `auth_invalid`. The gate
then closes with WebSocket status 4000 + the code's index in that list. The node does not retry `bad_origin`,
`bad_proof`, `revoked`, `enroll_invalid`, `service_denied`, `kind_mismatch` or `auth_invalid` (an `auth` API key
that is revoked, expired or unknown) without a change by its operator; it retries the others with backoff (§13). A
hello of the wrong shape (a field of the wrong type, a `name` outside 1–64 characters, a `nonce` that is not 32
bytes, a NUL) is refused with the code of its subject: `bad_origin` for the origin, `bad_proof` for a `kid` or
`recipient` that does not match, `service_denied` for a service's kind or name; one with no listed subject, and a
binary message during the handshake, is `unsupported_version` (D134). A gate that fails itself during the
handshake (Postgres unreachable during the admission) sends neither `welcome` nor `error` and closes with
WebSocket status `1011`, which the node retries, probing first (§2.4, D136).

### 4.1 The proof

The node proves it holds the private key of `recipient` without signing anything (an X25519 key cannot sign,
D30). The gate encapsulates to the node's public key with HPKE in export-only mode; the node decapsulates and
returns an exported secret.

```
lp(x) = uint16_be(len(x)) || x
info  = SHA-256( "kontra.sh/tunnel/v1" || 0x00
              || lp(origin)                     // from M1, as the node dialed it
              || lp(cb) || lp(cb_value)         // cb_value = TLS exporter "EXPORTER-Channel-Binding", empty context, 32 bytes (RFC 9266); empty when cb = none
              || lp(SHA-256(M1 bytes as sent))  // commits kid, recipient, services, enroll, auth, rotate_from, nonce
              || lp(session) || lp(gate nonce) )

gate:  enc, ctx = HPKE.SetupBaseS(pk = recipient, info)       // DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, export-only
       want     = ctx.Export("kontra.sh/tunnel/pop/v1", 32)
node:  ctx      = HPKE.SetupBaseR(enc, sk = identity, info)   // a low-order enc is an error
       proof    = ctx.Export("kontra.sh/tunnel/pop/v1", 32)
gate:  accept iff ConstantTimeCompare(proof, want)
```

With `rotate_from`, `proof_old` is computed the same way for the old key (`enc_old` to the old recipient, the
same `info`, export label `kontra.sh/tunnel/pop-old/v1`), proving the node holds both.

What this gives:

- **No replay**: `enc`, the session id and both nonces are fresh per connection.
- **No relay to another gate**: the node binds the origin *it* dialed. A gate that relays another gate's
  challenge to a node gets a proof for its own origin, which the other gate rejects.
- **No relay through a TLS-terminating middlebox** when `cb = tls-exporter`, because each TLS leg has its own
  exporter. A node started with `--require-channel-binding` refuses `cb = none`.
- **Nothing that opens a sealed part.** The node returns only an HPKE-labelled export of a DH the gate chose;
  it is domain-separated from age's own key derivation, and the node never decrypts anything for the gate.
- **First contact proves the recipient.** An enrollment can only bind a key whose private half the enrolling
  node holds, so every published recipient is held by a node.

The gate's identity to the node is ordinary WebPKI TLS.

## 5. Enrollment

Enrollment binds a key to a service at one gate, once (PRD §6, note 1 §2). The binding is a row in that gate's
`key_services`; the key's tenant is fixed by its first enrollment. After enrollment, nothing is sent again but
the proof. A service the gate creates during a handshake gets `allow: [org:<its tenant>]`
([`relay.md`](relay.md) §2.3).

| Authority | Where | Rule |
|---|---|---|
| Service token | Self-hosted gates: `kontra-gate enroll <service>` prints one (and `kontra-gate enroll --api-key <name> [--prefix <p>] [--scope …]` prints an API key, note 2 §3; the key format is [`relay.md`](relay.md) §2.3). Cloud: `POST /v1/enroll` | Single use, 15 minutes, stored as sha256, bound to one service of one tenant (PRD §12). Format `ket_` + 32 base62 characters. |
| Tenant token | Issued by the operator only (Cloud's "Add a node", kontra-cloud#59) | Single use, 15 minutes, bound to the tenant **and a list of service names** it may bind (D40). It creates those services that do not exist yet. |
| API key (`auth`) with scope `enroll:<service>` or `enroll:*` | Cloud (`tunnel.enroll_with_api_key`, default `false` on self-hosted gates) | Enrolls the node's key for the services the scope names. A key without an `enroll:` scope only hires. |

- **Adding a key to a service that already has an enrolled key** — another node becoming a replica of it,
  which also adds a sealing recipient — needs a service token or an `enroll:` scope; a tenant token never does
  it. The service's existing nodes are told on their control streams (`{"t": "enrolled", "service", "key"}`,
  §13), and hiring nodes see it as a recipient change ([`ext-sealed.md`](ext-sealed.md) §7).
- A token is consumed in the transaction that inserts `key_services`, after the proof verified (`UPDATE … SET
  used_at = now() WHERE token_hash = $h AND used_at IS NULL AND expires_at > now() RETURNING …`).
- An enrollment that would bind a key already enrolled under another tenant is `enroll_invalid`; a token for a
  service of another `kind` than declared is `kind_mismatch`.
- A service token is minted only for an existing, enabled tunnel service, and an `enroll:` scope binds only such
  services (naming another is `service_denied`); only a tenant token creates a service. A bound service that is no
  longer an enabled tunnel service is left out like a revoked binding. A token that applies to no declared service
  is `enroll_invalid` and is not consumed (D132).
- A declared service the key was never enrolled for, and is not enrolling in this handshake, is `service_denied`,
  and nothing is enrolled (the handshake is all-or-nothing); an unknown key that nothing in its hello enrolls is
  `unknown_key`, whatever else the hello carries (D133), except a node that only hires and a node that asks for an
  anonymous session on a gate that opens them (§6, D285). A node that only hires: a hello that declares no service
  and carries an `auth` API key with the `hire` scope (and no token) admits its key hire-only, a `tunnel_keys` row in
  the API key's tenant with no binding, without `tunnel.enroll_with_api_key`. A known key with no binding at all is
  admitted only on such a hello, or one that enrolls it, so a hire-only key never hires as `node:<key_id>` (§9) once
  its API key is revoked (D206). A declared service whose binding was revoked (`key_services.revoked_at` set) is left
  out of `welcome.services` and `tunnels.services`; the handshake still succeeds for the others. Only a service or
  tenant token in the hello enrolls it again, never the `auth` API key's `enroll:` scope (D130).
- **The service a session serves comes from `key_services`, never from a request** ([`relay.md`](relay.md)
  §6.1). The `services` in M1 only narrow it.
- Every enrollment, revocation and rotation is a row in `audit`.

## 6. Anonymous sessions (Cloud)

A gate with `tunnel.anonymous` set (Cloud's public gate; never a default) accepts a `hello` that carries `anonymous:
true` (§4) and neither `enroll` nor `auth`, whose key is unknown at the gate or belongs to a live anonymous tenant
(note 2 §2). The setting needs `tenancy: multi`, since an anonymous service's URL names its tenant
([`relay.md`](relay.md) §2.1, §17), and is either `true` or an object of §6.2's limits (D277). A hello without
`anonymous: true` never opens or resumes one (D285): an unknown key that nothing in it enrolls stays `unknown_key`
(§5), which the lost-welcome probe of §2.4 relies on. A gate without the setting answers a hello carrying it from a
key it does not know `anonymous_unavailable`, and so does a gate whose live anonymous tenants already number
`max_tenants` (§6.2). A gate without the setting (turned off while anonymous tenants live) answers a hello carrying
it from a key of an anonymous tenant `anonymous_unavailable` too, and treats any other hello from such a key as §5
treats a new key's: the probe is `unknown_key`; a hello §5 admits moves the key, and the tenant it leaves ends at once
(D292). Every multi-tenant gate runs §6.1's expiry and purge, with or without the setting (§6.2's defaults for what the
block no longer says): a tenant opened before the setting was turned off still ends at its `expires_at` and is purged
`purge_after` later (D304). A hello from a key of a team or account tenant is admitted as §5 says, for its existing
bindings, with or without the field.

### 6.1 What happens

- The gate creates a tenant `anon-<16 lowercase hex>` (8 random bytes; a slug that exists, a tombstone included, is
  drawn again) with `kind = anonymous`, `expires_at = now + ttl` and `created_from` = the client's network, enrolls
  the key for the declared services under it, and answers `welcome` with `anonymous.expires_at`. That slug shape is
  reserved: a tenant is anonymous if and only if its slug is `anon-` and 16 lowercase hex digits (a CHECK, migration
  0003; D287), so an account can never take such a name. The services are at `{public_url}/agents/anon-…/<name>`
  and `/mcp/anon-…/<name>`, with `allow: [org:anon-…]` (the session's own node), plus `anonymous` for those listed in
  M1's `public` (`kontra connect --public <name>`): a shareable, ngrok-style URL anyone may call, within the limits
  below. They are never `listed`.
- The same key reconnecting before expiry gets the same tenant back (the gate looks the key up first). The
  services it declares then are created, bound or enabled again within the services limit, which counts the
  tenant's services that are not disabled (§6.2); a service it no longer declares is disabled (`404`, its binding
  revoked, its tasks in flight failing `service_unavailable`) and keeps its row until the tenant ends; and `public` is
  applied anew (D287).
- **A key of a live anonymous tenant presenting more (D286).** With `enroll` tokens it converts into the tokens'
  tenant, as §6.3 converts with an `auth` key: the key is bound as §5 says, and the anonymous tenant expires at once.
  With an `auth` API key that has no `enroll:` scope (a hire-only key), the hello is admitted as the anonymous
  session, resumed as above, with that key as the session's hiring principal (§9); nothing is converted. Every move
  of a key to another tenant (these conversions, §6.3's, and a new anonymous tenant after expiry) is one transaction
  under the key's row lock that deletes the key's `key_services` rows of the old tenant before it changes
  `tunnel_keys.tenant_id`.
- **A key of an anonymous tenant is not rotated (D304).** A hello whose `rotate_from` names it, the tenant live or
  ended, is `enroll_invalid` (§12), whatever else it carries, and nothing is written: a rotation would admit the new
  key into the tenant without the anonymous session's decision (no `welcome.anonymous`, no end, the full hiring-stream
  cap) and without opening a tenant §6.2 counts. The node's next hello, without `rotate_from`, asks for an anonymous
  session of its own (§4), or enrolls the new key with its tokens or `auth` key.
- Ten minutes before `expires_at` the gate sends `{"t": "expiring", "at"}` on the control stream (§13). At
  `expires_at` it revokes the key's bindings to the tenant's services and disables them (every route answers `404`; a
  dispatch in flight is aborted as a revoked binding's is, §12: its task fails `tunnel_lost` once an upstream task
  exists, and is otherwise retried and fails `service_unavailable` at that next dispatch, [`relay.md`](relay.md) §6.4,
  §13.5; D297), sends `{"t": "revoked"}` and closes the session; the node exits saying that the anonymous session
  expired. The key itself is not revoked (§12): its next hello opens a new anonymous tenant with a new slug (D277).
- `purge_after` (24 h) after expiry the gate deletes what the tenant still holds: the keys still in it (`tunnel_keys`,
  with their bindings and tunnels), its services with their calls ([`relay.md`](relay.md) §13.5), enrollments,
  tasks with their bucket objects (the objects first, by each task's prefix, whether or not it has a `blobs` row,
  D167), and the MCP sessions whose `tenant_id` is the tenant, the
  gate's own `/mcp` sessions (`service` NULL) included (D287), and its `anon_calls` rows (D292). The tenant row
  stays as a tombstone (`tenants.deleted_at`, with `created_from`), so a slug is never reused.
- `kontra login` turns the anonymous tenant into the person's account (§6.3); the services keep their names
  under the account's slug and stop expiring.

### 6.2 Limits (`anon_limits`)

| Limit | Default | `tunnel.anonymous` key |
|---|---|---|
| Session TTL | 2 h | `ttl` |
| Anonymous tenants per network (IPv4 /24, IPv6 /48) at once | 3 | `tenants_per_network` |
| Services per anonymous tenant (not disabled) | 4 | `services` |
| Concurrent tasks per anonymous tenant, as host and as hirer | 4 each | `tasks` |
| Concurrent MCP requests and streams relayed to an anonymous tenant's services (as host) | 8 | `calls` |
| Node-opened (hiring) streams in flight per session | 8 | `hiring_streams` |
| Parts stored per anonymous tenant | 64 MiB ([`relay.md`](relay.md) §6.6) | `parts_bytes` |
| Live anonymous tenants, gate-wide | 1000 | `max_tenants` |
| Purge after expiry | 24 h | `purge_after` |
| Body and rate limits | the gate's standard limits ([`relay.md`](relay.md) §2.2) | |
| Listings | none: anonymous services are never `listed` and never in `search` | |
| Sealing | allowed; recipients are published only on the session's own services' cards | |

Exceeding a creation limit is `rate_limited` in M4 (the node backs off); exceeding a runtime limit is the
relay's `429`. The network is stored as a CIDR (`tenants.created_from`), never the address. How they count
(D277, D287):

- A network's tenants are the anonymous tenants created from it that have neither expired nor been converted; the
  client's address is the edge's ([`relay.md`](relay.md) §2.2). The gate's live anonymous tenants, counted the same
  way over every network, are at most `max_tenants`; a hello that would open one more is `anonymous_unavailable`.
- A tenant's services are its services that are not disabled, whichever hello declared them: a hello whose new names
  would take the tenant past `services` is `service_denied`, and nothing is enrolled. A resumed hello disables what
  it no longer declares (§6.1), so reconnecting with new names never grows a tenant past the limit.
- Tasks as host are the non-terminal tasks of the tenant's services, whoever called them; as hirer, the non-terminal
  tasks whose principal is a key of the tenant. Both are counted under a per-tenant lock when a task is created. Over
  the cap, A2A answers `429` as [`relay.md`](relay.md) §2.2 says (on JSON-RPC, `-32600` `rate limited`), a
  task-augmented MCP call `429` with `-32600`, and `hire` a `CallToolResult` with `isError`.
- A plain MCP call is not a task. The requests the gate relays to the tenant's services, whoever sent them, are at
  most `calls` in flight, together with their open streams: every request with an `id` and a method that is forwarded
  (`initialize`, a plain `tools/call`, `resources/read`, `prompts/get`, `completion/complete`, `server/discover`, a
  method the gate does not know), and each `GET` or `subscriptions/listen` stream for as long as it is open. Over it the
  request or stream is `429` with `-32600` `rate limited` and `Retry-After`, before anything is forwarded or metered,
  so a `--public` MCP service is not an unbounded relay (D304; a stock client's standalone `GET` stream so refused is
  not fatal to it). A notification, a client's response, and what the gate answers itself (`tools/list` from its cache,
  a durable service's `tasks/*`, a gate-written stream) take none. They are counted gate-wide: each request or stream
  being relayed is a row of `anon_calls` (migration 0003), deleted when its answer has been relayed or the stream
  closed, and with the replica that held it when that replica dies (§11; D292). The node's own hiring streams are
  bounded by `hiring_streams` and the edge.
- Parts: a caller's message whose parts would take the tenant's stored parts past `parts_bytes` is `InvalidParams`
  `anonymous storage limit`, before any task row; upstream bytes past it fail the task `upstream_error`, as
  [`relay.md`](relay.md) §6.6 says.
- **What anonymous tenants can keep stored (D304).** The creation limits count live tenants, while an ended tenant
  keeps its tasks and their bucket objects until its purge, `purge_after` after its end. So the parts kept for one
  network are bounded by about (1 + `purge_after` / `ttl`) × `tenants_per_network` × `parts_bytes` (with the defaults,
  13 × 3 × 64 MiB, about 2.4 GiB per /24 or /48), and gate-wide by the same factor × `max_tenants` × `parts_bytes`
  (about 0.8 TiB); a conversion ends a tenant at once and so frees its slot sooner, for the holder of an account key.
  An operator lowers the bound with `purge_after`, `parts_bytes` or the creation limits.

### 6.3 `kontra login` (D278)

`kontra login [--gate URL] [--no-browser]` gets an API key for a gate from its operator's sign-in and keeps it as
that origin's API key in `~/.kontra/credentials` (§2.4), which `kontra connect` then sends as `auth`:

1. It reads `{gate}/.well-known/oauth-protected-resource/mcp` ([`relay.md`](relay.md) §12.1), whose `resource` must be
   the gate's origin followed by `/mcp` (RFC 9728 §3.3: a document for another resource is not used), compared as an
   origin (§4: scheme and host in lower case, the default port elided; the gate writes it from `public_url` as
   configured) and the path `/mcp`, with no userinfo, query or fragment (D296). A gate that serves none has no
   sign-in: `kontra login` says to ask its operator for an API key (`--api-key`) and exits 1.
2. It reads the RFC 8414 metadata of the document's first `authorization_servers` entry (looked for as the gate looks
   for its issuer's, [`relay.md`](relay.md) §12.1), whose `issuer` must be that entry byte for byte (RFC 8414 §3.3:
   a document for another issuer is a mix-up and is not used), which must offer the `authorization_code` grant with
   PKCE `S256` and a `registration_endpoint`, and whose endpoints must be `https://` (`http://` only on a loopback
   host, as `--gate` allows for contributors' gates, §3). Once per gate origin and issuer it registers
   (RFC 7591: `client_name` `kontra CLI`, `redirect_uris` `["http://127.0.0.1/callback"]`,
   `token_endpoint_auth_method` `none`, `grant_types` `["authorization_code"]`) and keeps the `client_id` with the
   credential; a token endpoint that answers `invalid_client` makes it forget the `client_id`, so the next login
   registers again, and so does a login that reused it and got no answer before the bound or an interrupt: an
   authorization server that no longer knows a client does not redirect to it (RFC 6749 §4.1.2.1), so that is all the
   CLI hears of it, and `kontra login` says it forgot the registration (D296).
3. It runs the authorization code flow with PKCE (RFC 7636, `S256`), `state`, and a loopback redirect (RFC 8252 §7.3:
   `http://127.0.0.1:<ephemeral port>/callback`, listening on that port only until an answer arrives, closed before
   the token request, or 10 minutes pass), with `resource` = the gate's origin (RFC 8707). The URL is opened in the
   browser, or printed with `--no-browser`. An answer whose `state` differs ends the login; an `iss` in it must be the
   issuer, and must be there when the metadata says it is sent (RFC 9207). The token request follows no redirect, and
   neither the code nor the verifier is ever printed or logged.
4. In this revision, the token response's `access_token` must be an API key of the gate (`kk_…`); anything else, a
   JWT included, is refused and not stored. The authorization server is the operator's (Cloud's control plane): it
   issues the key for the person's account, with the scopes `hire` and `enroll:*`. **It MUST issue a gate API key
   only when `resource` is the origin of the gate whose keys it issues** (compared as an origin, as above), answer any
   other `invalid_target` (RFC 8707 §2) at its authorization endpoint or its token endpoint, and name that gate's origin
   on its consent page (D305). An API key has no audience: a gate whose RFC 9728 document names another gate's
   authorization server would otherwise receive, once the person signed in through it and connected, a key for the
   person's account at that other gate. `kontra login` cannot enforce it, since the gate it asks names the
   authorization server; it prints the authorization server's origin beside the gate's. This rule is M5's to revise, with
   Cloud's authorization server and the gate's OIDC principals (kontra-cloud#60); `kontra login` holds it in one place.

**Conversion.** A hello whose key belongs to a live anonymous tenant and whose `auth` is an API key of an `account`
tenant with the `enroll:*` scope (`tunnel.enroll_with_api_key`) converts the key, in the admission's transaction: the
anonymous tenant's declared services (those the session serves: enabled and bound to the key, of the declared kind)
are created in the account where they do not exist, under the same names, with `allow: [org:<account>]` (an existing
one of another kind is `kind_mismatch`, and nothing moves); the key moves to the account with bindings to them; and
the anonymous tenant expires at once (§6.1). `welcome` names the account's URLs and has no `anonymous`. The anonymous
URLs answer `404` and their tasks in flight fail `service_unavailable`: a converted service is a new service at a new
URL. An `audit` row records the conversion. A running `kontra connect` converts at its next handshake; `kontra login`
tells the person to restart it. Otherwise the binding is §5's, through the API key's `enroll:*` scope (D296): an
existing account service is bound, not created again, and one that is disabled or not a tunnel service is
`service_denied`; a declared name the session does not serve and the account does not have is `service_denied` (only a
tenant token or the operator creates a service, §5), and nothing moves; a service's other nodes are told of the new
key; a hello declaring no service converts the key hire-only when the API key may hire (§9). A team tenant's key
converts into the team as an account's does; a gate that stopped opening anonymous sessions still converts.

## 7. After `welcome`

Both sides call `websocket.NetConn(ctx, conn, websocket.MessageBinary)` and start yamux: the node as
`yamux.Client`, the gate as `yamux.Server`. Configuration, identical on both sides:

| yamux `Config` | Value | Why |
|---|---|---|
| `AcceptBacklog` | 64 | Bounds streams awaiting `Accept` and the buffered memory a hostile peer can pin before `Accept` |
| `EnableKeepAlive` | `false` | yamux's keepalive closes on the first missed pong; liveness is §11's own loop (D33) |
| `KeepAliveInterval` | 20 s | Must be non-zero even when disabled |
| `ConnectionWriteTimeout` | 15 s | Also the pong deadline of `Ping()` |
| `MaxStreamWindowSize` | 256 KiB | The default; a slow SSE reader stalls only its own stream |
| `StreamOpenTimeout` | 30 s | An unacknowledged open closes the whole session; a wedged peer reconnects |
| `StreamCloseTimeout` | 5 min | |

Both sides accept streams promptly and enforce their own in-flight caps **after** `Accept` (answering `503` over
HTTP when full): yamux bounds only the backlog, not established streams. Every write to the WebSocket, on both
sides, has the deadline `ConnectionWriteTimeout`, so a peer that stops reading closes the session within 15 s
(D155). The first stream the gate opens after `welcome` is the **control stream** (§13).

## 8. Hosting: streams the gate opens

One gate-opened stream carries exactly one HTTP/1.1 exchange for one service.

### 8.1 Preamble

The gate first writes one line, at most 4096 bytes, terminated by `\n`:

```json
{"v": 1, "op": "http", "service": "hello", "kind": "a2a", "task": "<gate task id>", "context": "<gate context id>"}
```

| Field | Rule |
|---|---|
| `op` | `http` (an exchange) or `control` (§13, once per session) |
| `service`, `kind` | Which hosted service this exchange is for. The node refuses (`404` with `Kontra-Node: not_hosted` or `kind_mismatch`) a service it is not hosting on this session or a kind that does not match. |
| `task`, `context` | Present when the exchange belongs to a gate task: the node needs the gate's ids to check sealed headers ([`ext-sealed.md`](ext-sealed.md)). |
| `mcp_session` | For MCP exchanges: the lowercase hex SHA-256 of the client's gate `Mcp-Session-Id`. For every exchange of a durable MCP task (the dispatch's own `initialize`, `tools/call`, `tasks/*`, input responses) it is the hash of the client session that created the task. Absent on an exchange for a 2026-07-28 request and on every exchange of a task a 2026-07-28 request created: they have no session, and a node must not rely on the field there (a sealed service never receives one, D162, D180). |
| `mcp_request_id` | For a `tools/call` exchange (plain or the dispatch of a durable task), and every other exchange of a durable task: the client's original JSON-RPC id of that `tools/call`, as a string. Absent where `mcp_session` is. |

Unknown fields are ignored. The node never forwards the preamble.

### 8.2 The exchange

Then the gate writes an HTTP/1.1 request (origin-form request target, `Connection: close`) and reads the
response; SSE responses stream on the same stream until either side closes it. The gate's HTTP client over the
tunnel has no overall timeout (deadlines come from the request context) and never reuses a stream.

The node is a **fixed-target reverse proxy**, not a SOCKS proxy (`sidecar_not_a_proxy`):

- It sends the request to the configured target of the named service and nowhere else. The request's `Host`
  header and an absolute-form request target are ignored; the scheme, host and port always come from
  `--serve`.
- It percent-decodes and cleans the request path (`.`, `..`, `//`), forwards a path already under the target's
  base path as it is and prefixes any other with the base (the gate's paths come from the upstream card's
  interface, which already holds the base, [`relay.md`](relay.md) §6.2), and answers `400` if the result is not
  under that base path. An MCP target may name its endpoint path (MCP has one endpoint; `server-everything`
  serves `/mcp`), and the gate's `/`, where it sends every MCP exchange, goes to that endpoint exactly as
  `--serve` names it; `kontra connect` still refuses an A2A target with a path (D123, D210).
- `Host` is set to the target's host. `Origin` is removed (MCP servers on loopback refuse a foreign `Host` and
  may refuse a browser `Origin`).
- Hop-by-hop headers are removed. `CONNECT` and requests with `Upgrade` are refused with `400`. `TRACE` is
  refused.
- `Forwarded`/`X-Forwarded-*` are removed, not added: the service sees a local client.
- Response bodies stream as they come: `text/event-stream` is flushed per event, never buffered or compressed.
- The node's own server for tunnel streams sets a header read timeout only, never a write timeout (SSE).
- For an MCP service, stdio or URL, the node answers the server's `roots/list` itself and keeps the client's
  roots from the server (§10, D173).
- Two services whose targets share an upstream (one origin, any loopback name for a loopback host, and the base
  path of one the other's or under it, the root covering every path) reach the same server, so the node keeps their
  sealed state as one: an exchange of either is guarded by the sealed jobs of both ([`ext-sealed.md`](ext-sealed.md)
  §5, D236). A stdio service runs children of its own and shares nothing.

**Responses the node writes itself** carry a `Kontra-Node` header, so the gate never mistakes them for the
service's own ([`relay.md`](relay.md) §6.4): `not_hosted` (404), `kind_mismatch` (404), `full` (503),
`target_unreachable` (502), or `sealed_rejected` (400, [`ext-sealed.md`](ext-sealed.md) §6). The node's `400`s for
`CONNECT`, `Upgrade`, `TRACE`, a path outside the base and an unreadable preamble carry no `Kontra-Node`: the gate
takes them as the service's own, and fails the task `upstream_error` (D151). The node removes any
`Kontra-Node` and `Kontra-Sealed-Reject` header from the service's own responses. A stream that ends without a complete
HTTP response, or while the session is closing, is `tunnel_lost` to the gate.

### 8.3 Blob streams

A part larger than 256 KiB reaches a node as a `url` part whose URL is `kontra-blob:<ref>`
([`relay.md`](relay.md) §10.4). The node fetches it by opening a stream with the preamble `{"v": 1, "op":
"blob", "ref": "<ref>"}`. The gate resolves the ref through its `blobs` row and checks that the row's task is one
this session is serving (an attempt of it is on the session) or one whose buyer is this session's hiring
principal's. It then writes one JSON line, `{"size": <n>}` followed by exactly `n` bytes of the object, or
`{"error": "not_found" | "forbidden" | "too_large"}` (`too_large`: an object over 64 MiB), and closes the stream
(D181). The node refuses an announced size over 64 MiB and more bytes than announced, and, for a sealed part,
authenticates the whole age file before acting on it. **A node never dials a URL taken from a part** (D56).

A blob stream over the session's cap (§14) is closed unanswered, as is one whose object the bucket stops sending
for 30 s. A hosting exchange whose `kontra-blob:` part cannot be fetched answers as §8.2's own refusals do: `400`
without `Kontra-Node` when asking again would fail alike (a refused ref, a bad answer, more bytes than announced),
and `503` with `Kontra-Node: full`, which the gate retries, when it may succeed (D209).

## 9. Hiring: streams the node opens

The node serves `127.0.0.1:7777` (`--listen`) to local clients, a node that only hosts too, unless `--listen off`
turns the hiring endpoint off: such a node binds no local port and hires for no one, and `--local-token` and
`--allow-origin`, which guard `--listen`, are refused beside it, as is `--listen off` without a `--serve` (D208). A
loopback `--listen` binds the port on both loopback families, `127.0.0.1` and `[::1]`: the node sends its clients to
`localhost` (the cards it serves, its documentation), which a stock Linux resolves to `::1` first, so a port it left
free on either family would be one another local user could bind and serve its clients from. A port another process
holds on either family stops the start; a family the machine does not have (no IPv6) is one no one else can bind
either (D262). Before the node binds (at its start, or a restart), another user can bind the port: on a machine
shared with other users, `--local-token` is the remedy. It forwards each request on a stream it opens (D34), with the preamble `{"v": 1, "op": "http"}`, followed by the
request with its path unchanged: `/agents/…`, `/mcp/…`, `/mcp`, `/.well-known/…`. Anything else is refused by the
node with `404`.

**Local requests are checked first** (DNS rebinding and other local users, MCP's Origin rule):

- The node answers `403` (a JSON-RPC error without id on MCP paths) to any request whose `Host` is not
  `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` of its own `--listen` (or, when `--listen` names one
  specific non-loopback address, never `0.0.0.0` or `::`, that address and port, D207), and to any request carrying
  an `Origin` not listed by `--allow-origin` (default: none). It answers no CORS preflight (`OPTIONS` is `405`) and
  forwards `Origin` only when it was allowed.
- `--listen` on a non-loopback address requires `--local-token <file>`; every request must then carry
  `Authorization: Bearer <local token>`, which the node strips before forwarding.
- On a loopback `--listen` without `--local-token`, the node serves only its own user: on Linux it reads the
  owner of each connection's client socket from the kernel's socket table (`/proc/self/net/tcp`, `tcp6`), for
  every connection whatever its source address (a local process may bind any address of the machine), and closes
  a connection of another user, or one it cannot attribute: a socket no process holds any more (inode 0, which the
  kernel lists as uid 0), the overflow uid of a user namespace, or a table it cannot read, about which it warns at
  start. Only where that table does not exist (not Linux) is the check not made: on a machine shared with other
  users, set `--local-token`, which then serves any user that presents it (D207, D229).
- The node serves no other local port: there is no local console (D164).

On the gate the forwarded request is served by the same handler as a request from the network, with three
differences:

- Its principal is the session's: the API key from M1's `auth` when present, else `node:<key_id>` with the key's
  tenant ([`relay.md`](relay.md) §2.3), for anonymous sessions too. A local client never sends a credential to the
  node. So every local client of the node shares one principal's MCP session cap on each service,
  `max_sessions_per_principal` ([`relay.md`](relay.md) §11.2, D231), as every user of one API key does: a client
  beyond it is refused `429` while the node's other sessions of the service are in use.
- Its edge limits are the session's (§6.2 for anonymous sessions), counted with the session as the client:
  `limits.body` and the body timeout as on the network, `rps_per_ip` per session, and in flight the session's
  stream cap (§14), not shared with the same API key's requests from the network (D207).
- Card URLs: the node rewrites every `supportedInterfaces[].url` in a card it serves locally to its own
  `http://localhost:7777{svc}`, the service the card was read for, whatever path the gate's names (the node seals by
  path: a gate's card that pointed a client at another path, `{svc}/.well-known/x`, `/agents//x`, `/mcp` or another
  service's, would have its messages go unsealed), after verifying the gate's card signature with a `jku` equal to
  `{the origin it dialed}/.well-known/jwks.json` ([`relay.md`](relay.md) §3). A card whose gate signature does not
  verify is not served locally. A request other than a `GET` (or `HEAD`) to `/.well-known/…`, or to an `/agents/…`
  path that names no service, is refused `404` here: the gate has no such route (D247). A client builds its endpoint
  from more than the `url`, so a served interface keeps `url`, `protocolBinding` and `protocolVersion` only (an A2A
  client joins an interface's `tenant` into the path), the 0.3 members a compatibility parser reads an endpoint from
  (`url`, `preferredTransport`, `additionalInterfaces` at the top) are left out, and a card with a lone case variant
  of a member a client reads there (`supportedInterfaces`, an interface's `url`, `protocolBinding`,
  `protocolVersion` or `tenant`) is not served (D259).

**The node reads the gate's answers before a local client does**, whatever the service's sealing:

- Redirects: the node never relays a `3xx` of the gate's, on any path: the client gets the node's own `502` (an A2A
  or MCP error body) and no `Location`. The gate has no hiring route that redirects, and a stock client follows a
  `307` or `308` with its own request body to whatever `Location` names, the gate's origin or a local path the node
  does not seal (D258).
- MCP sessions: an answer names the session only on an `initialize` (the node removes `Mcp-Session-Id` from every
  other answer, since a stock client takes the header of any answer for its session), and a session id is used for
  the service it was initialized for only: a request on it to another path, or on an unsealed session to a service
  whose pin says sealed, is `404`, and the client initializes again (D260).

Sealing and unsealing for hired jobs happen in the node, around this forwarding ([`ext-sealed.md`](ext-sealed.md)).

## 10. MCP servers on the node: stdio and roots

`--serve mcp:fs=stdio:<command line>` makes the node own the server process; the gate sees a Streamable HTTP
target like any other.

- The command line is one argument (quote it in the shell); the node splits it with POSIX shell word rules,
  expands a leading `~/` and runs it without a shell, with a scrubbed environment (`PATH`, `HOME`, `LANG` and
  variables named with `--env`). An unquoted `|`, `&`, `;`, `<`, `>`, `(`, `)`, `$` or backtick, or a `#` that
  starts a word, is a usage error, since no shell gives it its meaning; quoted, it passes (D211). Its stderr goes
  to the node's log, capped at 1 MiB a minute. The node never stops reading the child's stdout.
- **One child per MCP session** (MCP 2025-11-25 has one `initialize` per process): an `initialize` without a
  session id spawns a child, forwards the message, and returns a node-minted `Mcp-Session-Id`.
- **Routing.** A response goes to the POST that carried its id. A message carrying the `progressToken` of a
  pending request goes to that request's POST stream. A message carrying
  `_meta["io.modelcontextprotocol/related-task"]` goes to that task's open `tasks/result` POST, if there is one.
  Everything else, every other server-to-client request included (`sampling/createMessage`,
  `elicitation/create`), goes to the session's `GET` stream; `roots/list` never leaves the node (below). While no
  `GET` is open, the bridge queues those messages (at most 256 messages or 1 MiB per session; overflow drops the
  oldest and logs it) and flushes them in order when one opens. Events carry ids `<child start>-<n>`, and a `GET`
  whose `Last-Event-ID` names an event of the `GET` stream replays the queue after it (a `POST` stream's id
  replays nothing: the bridge resumes no `POST` stream); a `GET` that opens while another is open takes its
  place, and the older one ends (D211).
- The child runs in its own process group. `DELETE` closes the child's stdin, sends the group `SIGTERM` after 5 s
  and `SIGKILL` 5 s after that, so a launcher such as `npx` leaves no process behind (D184); a child idle for
  `--session-idle` (default 1 h, at least the gate's `session_idle_s`) is stopped the same way and its session
  answers `404`; a crashed child's session answers `404`, and its group is stopped the same way. At most 8
  children per service (`--max-sessions`), counting a stopping child until its group is gone. Every principal of
  the service, and every durable task's own session, shares these children, so a full bridge makes room (D213): an
  `initialize` beyond `--max-sessions` waits for a child that is stopping already (the gate closed one of a
  principal's sessions idle for 2 minutes for that principal's new one, with a `DELETE`, [`relay.md`](relay.md)
  §11.2, D249), and otherwise stops the longest-idle child, when it has had no exchange (none open, an open `GET` stream
  counting) for at least 5 minutes, and waits for its group to be gone. With no such child it is `503` with
  `Kontra-Node: full`.
- The bridge is an in-process handler on tunnel streams; it listens on no port.
- **Legacy only in v4.0.** The bridge speaks 2025-11-25 and 2025-06-18. A request without a session id that is not
  `initialize` (a 2026-07-28 request among them) is answered `400` with the legacy body of
  [`relay.md`](relay.md) §11.12 rule 3, so a dual-era client falls back to `initialize` (D161).

**Roots stay on the node (D173).** For every MCP server the node hosts, stdio or `mcp:<name>=<url>`, the node is
the server's only source of roots: a server such as `server-filesystem@1.0.0` replaces the directories it was
started with by the client's roots, so roots from the remote client, or from the gate, would decide what the
node's filesystem exposes.

- `--roots <name>=<dir>` names one root of the service `<name>`: an absolute path to a directory that exists when
  `kontra connect` starts. The flag is repeatable, one directory per flag (`--roots fs=/a --roots fs=/b`),
  and a service's roots are those of every flag naming it, in order; a name that no `--serve mcp:` serves is a
  usage error. None by default.
- In the `initialize` it forwards, the node replaces the client's `capabilities.roots` with its own: `{}` (no
  `listChanged`) when roots are configured, absent otherwise.
- It answers the server's `roots/list` itself, with the configured directories as `file://` URIs (an empty list
  when there are none), and never relays it to the remote client. It drops the client's
  `notifications/roots/list_changed`, and every client response that does not answer a server-to-client
  request it relayed on that session and has not seen answered, a result carrying `roots` whatever its id, and
  every response on a 2026-07-28 exchange, with `202` (D212).
- On a 2026-07-28 exchange (a URL service only: the bridge is legacy only) it removes `roots` from the forwarded
  `_meta["io.modelcontextprotocol/clientCapabilities"]`, every `params.inputResponses` entry that carries `roots`,
  and every `roots/list` entry of an upstream `inputRequests`. Configured roots are offered to 2025-11-25 and
  2025-06-18 sessions only: a stateless modern exchange gives the node no point at which to answer.
- It reads these messages with exact keys, as [`relay.md`](relay.md) §11.1 does, and answers one with a repeated
  or case-variant key `400` with `-32600`.

## 11. Liveness and the registry

- **Ping loop, both sides:** `Session.Ping()` every 20 s; three consecutive failures close the session. A silent
  peer is detected within about 75 s on either side.
- On `welcome` the gate inserts `tunnels(session_id, key_id, tenant_id, replica_id, services, hire_principal,
  cli_version, connected_at, last_ping_at)`. It updates `last_ping_at` every 60 s (not per ping) and deletes the
  row when the session closes.
- **One session per key per gate.** A completed handshake for a key that already has a non-draining session at
  this gate supersedes it: the gate closes the older session (its dispatches fail `tunnel_lost`, its row is
  deleted). The older is the earlier handshake, by session id (UUIDv7, minted at the challenge): a registration
  that finds a newer session of its key on a replica that is not draining inserts nothing and closes (D139). The
  replica that supersedes a session another replica holds tells it with `NOTIFY kontra_tunnel, 'session:<id>'` in
  the same transaction; an admission's transaction sends `'enrolled:<key_id>/<tenant_id>/<service>'` on the same
  channel, for §5's `{"t": "enrolled"}` (D138). On the same channel a replica sends
  `'mcp:<tenant_id>/<service>/<replica_id>'` when a node of an MCP service connects to it or leaves it, and
  `'tools:<tenant_id>/<service>'` after an upstream's `notifications/tools/list_changed`, so that every replica
  drops the service's era and tools list ([`relay.md`](relay.md) §11.3, §11.12; D199). Only during
  make-before-break (a drain, §13, or a rotation, §12) are two kept. Several nodes may serve one service, each
  with its own key.
- Each replica upserts `gate_replicas.last_seen_at` every 10 s, and every 10 s deletes the rows of replicas silent
  for more than 30 s (cascading to their `tunnels` rows) and closes the `mcp_sessions` whose tunnel sessions they
  held (`mcp_sessions.replica_id`, `tunnel_lost`). A session whose client reached a dead replica while a live
  one holds its tunnel stays open ([`relay.md`](relay.md) §11.2, D179). The relay never selects a session on a
  replica silent for 30 s ([`relay.md`](relay.md) §6.1).

## 12. Revocation and rotation

- **Revoke** (console, `kontra-gate enroll --revoke <key_id>`): `tunnel_keys.revoked_at` is
  set and `NOTIFY kontra_revoke, 'key:<key_id>'` is sent; the replica holding the session closes every open
  `mcp_sessions` row pinned to it as `revoked`, sends `{"t": "revoked"}` on the control stream and closes it. The
  next handshake is `revoked`. Revocation does not follow `rotated_to`: a key rotated from the revoked one stays
  enrolled, and `kontra-gate enroll --revoke` names the newest still-enrolled key of the chain and the command
  that revokes it (D128). Anonymous expiry is not a key revocation: it revokes the key's bindings to the tenant's
  services, not the key (§6.1).
- **Revoke one binding** (`kontra-gate enroll --revoke <key_id> --service <name>`, D131;
  `key_services.revoked_at`, `NOTIFY kontra_revoke, 'binding:<key_id>/<service>'`): the
  replica holding the key's session removes the service from the session's served set and from
  `tunnels.services`, closes every open `mcp_sessions` row pinned to (that session, that service) as `revoked`,
  aborts the session's in-flight dispatches for the service (they fail `tunnel_lost`, or are retried if no
  upstream task exists), and then sends `{"t": "revoked", "service": "<name>"}` on the control stream; the node
  stops declaring that service until its operator changes `--serve` (a revocation that reaches the session of a
  rotated key in its overlap stops only that session serving it: the active key has its own binding, D129). The
  gate does not rely on the node honouring that message: it opens no further stream for the service
  ([`relay.md`](relay.md) §6.1, `mcp_session_revoked`).
- **Revoke an API key** (`NOTIFY kontra_revoke, 'apikey:<id>'`): every later stream a session opens with it as
  its hiring principal gets `401`, and the replica sends `{"t": "auth_revoked"}` on that session's control stream.
  Hosting on the session is unaffected.
- NOTIFY only shortens the delay: each replica re-reads revocations since its last check whenever its `LISTEN`
  connection (re)connects and at least every 5 s ([`relay.md`](relay.md) §2.3).
- **Rotate** (`kontra connect --rotate`): the node writes a new first line to `identity.key` (the old one stays
  below it, decrypt-only) and connects with the new key and `rotate_from` naming the old one, proving both (§4.1).
  No token is needed: holding the enrolled old key is the authority. `rotate_from` must name a key of this gate
  that is neither revoked nor already rotated (a revoked one is `revoked`, a rotated one `enroll_invalid`; the
  node so refused sends its hello again at once without `rotate_from`, and names that key to that gate no more, so
  that its tokens or `enroll:` scope enroll the new key there, D125), nor a key of an anonymous tenant (`enroll_invalid`,
  §6.1, D304), and the new key must not exist yet; the
  rotation is one transaction, under a row lock on the old key. The gate inserts the new key in the old key's
  tenant, copies its `key_services` rows, a revoked one as revoked (D129), and marks the old key
  `revoked_reason = 'rotated'`, `rotated_to = <new>`, `overlap_until = now + 24 h` (`revoked_at` is set when the
  overlap ends, and the old key's session is then closed without `{"t": "revoked"}`, D140). Rotation is
  make-before-break: the old key's session keeps serving the tasks already on it until they end or the overlap
  passes; new attempts never select a rotated key, and a new handshake of it is refused `revoked` (D127). The
  service's recipients are republished at once without the old key, so new parts are sealed only to the new one;
  parts already sealed to the old key still open on the node, which keeps the old line until 24 h after the last
  gate that confirmed the rotation (PRD §8, D126) and then drops it. The node rewrites `identity.key` under a file
  lock (`flock(2)`); where there is none, it refuses to rotate (D153).
- **Recipients** of a sealed service are the keys enrolled for it, not revoked and not rotated, whether or not
  they are connected (D61; PRD §8 said "connected"), so a task sealed while one replica is offline can still be
  served by it when it returns.

## 13. Control stream, drain and reconnect

The control stream carries JSON lines, gate to node (D35):

| Message | Meaning |
|---|---|
| `{"t": "drain"}` | This replica is shutting down. The node dials a **new** session at once (make-before-break), opens no new hiring streams on the old one, keeps serving inbound streams on it, and closes it when the gate does (at the end of the gate's `drain_timeout`, [`relay.md`](relay.md) §14.3). |
| `{"t": "revoked"}` / `{"t": "revoked", "service": "<name>"}` | §12 |
| `{"t": "auth_revoked"}` | The session's hiring API key was revoked: hiring streams now get `401`; hosting goes on (§12) |
| `{"t": "enrolled", "service": "<name>", "key": "<key_id>"}` | Another key was enrolled for a service this node hosts (§5) |
| `{"t": "expiring", "at": "<RFC 3339>"}` | An anonymous session ends at that time |

The node answers nothing on it. Unknown messages are ignored.

**Reconnect.** After any close other than `revoked`, the node redials with the same key, backing off
exponentially from 1 s to 60 s with full jitter, reset after a session that lived 60 s. It never learns which
tasks it was serving and holds no task state ([`relay.md`](relay.md) §6.4: a task whose session is lost after its
first upstream task id fails `tunnel_lost`).

Front proxies must not cut the tunnel on reload: the Caddy example sets `stream_close_delay`; on Kubernetes the
gate sits behind an L4 load balancer (TLS at the gate) or a Gateway API route with WebSocket timeouts of at least
an hour (D36).

## 14. Limits

| Limit | Value |
|---|---|
| Handshake message | 16 KiB; handshake 10 s |
| Preamble | 4096 bytes |
| Inbound streams in flight per session (node) | 64 (`--max-inflight`); over it the node answers `503` with `Kontra-Node: full` |
| Node-opened streams in flight per session (gate) | 32, 8 for anonymous sessions, a stream counting from its accept until both halves end; over it `429`, and a node that keeps more than that many refused streams open loses its session (D137, D182) |
| Gate-opened streams the gate has closed and the node has not (gate) | 256; over it the gate closes the session (D157) |
| Ping SYNs from a node (gate) | 16 at once, then one every 500 ms; over it the gate closes the session (D155) |
| Blob size per fetch | 64 MiB |
| Sessions per key per gate | 1, plus one during make-before-break |
| Stream ids | yamux allows about 2³¹ opens per side; on `ErrStreamsExhausted` the node reconnects |

A stream is in flight until both its halves have ended, on either side (D137, D157).

## 15. Security properties

| Property | Mechanism |
|---|---|
| The gate never opens a connection to a node | §1: the node dials; cancel, blobs and every request travel inside the tunnel the node opened |
| A session only receives requests for services its key is enrolled for | §5, `key_services`; the relay selects candidates from it and re-checks before every stream; the node refuses other preambles (§8.1) (`key_scope`) |
| The node is not a proxy | §8.2: fixed target, path confined to it, no CONNECT or Upgrade; §8.3: never dials a URL from a part (`sidecar_not_a_proxy`) |
| A key cannot be impersonated at another gate by a gate it dialed | §4.1 origin binding (and channel binding); §2.4 credentials go only to their own origin |
| The node's local endpoint cannot be driven by a web page | §9 Host and Origin checks (`local_rebinding`) |
| A remote client or the gate cannot widen what a hosted MCP server exposes | §10: the node answers `roots/list` from its own `--roots`; a client's roots never reach the server (`stdio_roots`) |
| The gate holds no private key and learns nothing that opens a part | §2, §4.1 |
| A stolen identity key | Revoke (§12) ends the session and every future handshake of that key, not of a key it was rotated to, which is revoked in turn (D128). It does **not** protect parts already sealed to it: age has no forward secrecy, so whoever holds the key and the ciphertext (a database or bucket breach) reads them. Retention ([`relay.md`](relay.md) §5.4) bounds that. |

## 16. Named tests

M2 runs `tunnel_kill`, `sidecar_not_a_proxy` and `key_scope`, with [`relay.md`](relay.md) §16's tunnel tests;
`anon_limits`, `local_rebinding` and `stdio_roots` come with M3 and M4 (D154).
M4 runs `anon_limits`, `anon_lifecycle` and `login_converts` (D282, D297), with `tunnel.anonymous`'s limits
compressed where a row waits on a clock, in the tests' own configuration.

| Test | Expected |
|---|---|
| `tunnel_kill` | §11, §13; [`relay.md`](relay.md) §6.4: SIGKILL the node mid-task; the task fails `tunnel_lost` once an upstream task exists; the node re-registers within 60 s; the next task succeeds |
| `sidecar_not_a_proxy` | §8.2–§8.3: a foreign `Host`, an absolute URL, a `..` or encoded path outside the base (M2: the root base, D123), `CONNECT`, `Upgrade`, and a `url` part pointing at a private address all reach only the configured target or are refused, and the node dials nothing else |
| `key_scope` | §5, §8.1; [`relay.md`](relay.md) §6.1 |
| `replica_kill` | §11; [`relay.md`](relay.md) §14.2 |
| `anon_limits` | §6.2: a 4th anonymous tenant from one /24 is `rate_limited`, and a 4th from one IPv6 /48; one past `max_tenants` is `anonymous_unavailable`; a 5th declared service is `service_denied`, with nothing enrolled, and reconnecting with new names never takes the tenant past 4 services; a 5th concurrent task is `429`, as host and as hirer; a 9th concurrent plain MCP call to the tenant's services is `429`, and so is a 9th relayed request or open `GET` stream of any other kind (D304); a 9th hiring stream is `429`; a caller's parts over the tenant's total are `InvalidParams` before any task row, an upstream's fail the task `upstream_error`; the services are absent from `search`; at `expires_at` the session closes `revoked` and the URLs answer `404` |
| `anon_lifecycle` | §6.1: the same key before expiry gets its tenant back; a `--public` service answers a caller without a credential and a service not public answers `404` to it and to another node; `expiring` arrives before the end; at the end a task in flight fails `tunnel_lost` once an upstream task exists and `service_unavailable` at its next dispatch otherwise (D297), the node exits saying so, and the key's next hello opens a new tenant with a new slug; a probe after an enrollment that did not commit still gets `unknown_key`, and its tokens are sent again (D285); `enroll` from an anonymous key converts it; after `purge_after` the tenant's keys, services, enrollments, tasks, calls, MCP sessions and bucket objects are gone, its row is a tombstone with `created_from` a CIDR, and no table holds the client's address in any form; an `anon-…` slug is refused for an account tenant |
| `login_converts` | §6.3, against an in-repository authorization server: `kontra login --no-browser` registers once, sends PKCE `S256` and `resource` = the gate's origin, accepts the code on its loopback port only, stores the `kk_` key for that origin only (0600), and refuses an access token that is not an API key; `kontra connect` with it converts the tenant: `welcome` has no `anonymous` and names the account's URLs, the services stop expiring, the anonymous URLs answer `404`, and an `audit` row records it; a gate without `auth.mcp_oauth` makes `kontra login` exit 1 naming `--api-key` |
| `local_rebinding` | §9: `Host: evil.example:7777` or `Origin: http://evil.example:7777` on `/mcp/…`, `/agents/…` and `/.well-known/…` gets `403` and opens no stream |
| `stdio_roots` | §10: `server-filesystem@1.0.0` over stdio, started with one directory and served with `--roots` naming another; a client that declares roots of `file:///` gets its first `tools/call` answered in under 2 s and never receives `roots/list`; the server's `list_allowed_directories` names only the node's configured root, and a read of `~/.kontra/identity.key` is refused (D173) |
