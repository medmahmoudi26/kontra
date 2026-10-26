# Sealed extension `https://kontra.sh/ext/sealed/v1`

Status: **draft** (M0, revision 2 after the adversarial review). Open source. Normative. RFC 2119 key words.

Sealing is end to end between two nodes. The hiring node seals what it sends to the hosting node's identity
keys; the hosting node unseals before its local service sees anything, and seals what the service answers to a
key only the hiring node holds. The gate stores and relays ciphertext it cannot open: it holds no private key
that decrypts a part (PRD §8, §12). Neither the service nor the client ever handles a key.

## 1. Card entry and MCP capability

A2A, in the served card's `capabilities.extensions`, written by the gate for a service with `sealed: true`
([`relay.md`](relay.md) §3):

```json
{
  "uri": "https://kontra.sh/ext/sealed/v1",
  "description": "Parts are end-to-end sealed to the service's recipients; the gate relays ciphertext.",
  "required": false,
  "params": {
    "recipients": ["age1…", "age1…"],
    "envelope": "application/vnd.kontra.sealed+age;v=1",
    "required": true
  }
}
```

- `recipients`: the identity keys enrolled for the service, not revoked and not rotated, whether or not their
  nodes are connected ([`tunnel.md`](tunnel.md) §12, D61). Sealing to all of them lets any replica serve the task.
- `params.required` (config `services.<name>.sealed: {required: bool}`, default `true`): the gate refuses a
  message to the service that carries an unsealed part (§5).
- The extension entry itself is `required: false`: a client that ignores it can still call a service whose
  `params.required` is false, unsealed.
- A sealed A2A service needs an upstream card that offers an A2A 1.0 interface: the nodes seal and open the 1.0
  JSON-RPC and HTTP+JSON shapes only. Without one the service is unavailable, as for an unverified seller card
  ([`relay.md`](relay.md) §3): card routes answer `503` and tasks fail `service_unavailable` (D165).

MCP (revision 2025-11-25, which has no `capabilities.extensions`): the same object in the `initialize` result
under `capabilities.experimental["sh.kontra/sealed"]` (D27). It is not signed: a hiring node takes it from an
`initialize` result received over its own tunnel and records it like a card's recipients (§7).

## 2. What is sealed

**A2A.** The content of every `Part` in messages and artifacts, both directions: the one field of `text`, `raw`,
`url` or `data` that the part carries. Not sealed: task states, task and context ids, message and artifact ids,
`metadata` (the lifecycle and escrow read it), a part's `filename` (PRD §8: the name stays outside; do not put
secrets in it).

**MCP.** `tools/call` `params.arguments`; the `CallToolResult`'s `content` and `structuredContent` on whichever
response carries it (the `tools/call` response, or the `tasks/result` of a task the sealed call created); and, for
a task the sealed call created, the `params` of the upstream's `elicitation/create` and `sampling/createMessage`
requests, the client's results to them, and the task's `statusMessage` (which the hosting node replaces with
`"sealed"`). Not sealed: the tool name, `isError`, `_meta` outside `sh.kontra/sealed`, `tools/list`, task states,
and every other method (`resources/*`, `prompts/*`, notifications). Sealing applies only when **both ends run
`kontra connect`**: a stock MCP client talking straight to the gate sees no envelope (note 1 §6). So the gate holds
no plaintext of a sealed MCP call, plain or durable (D62). Envelope v1 binds an MCP call to a 2025-11-25 session
(§3, `session`), so a sealed MCP service is served to 2025-11-25 and 2025-06-18 requests only: the gate answers a
2026-07-28 request to it as a legacy server would, and a dual-era client falls back to `initialize`
([`relay.md`](relay.md) §11.12, D162). The hiring node never sends `Mcp-Param-*` headers for a sealed call.

## 3. Envelope v1

Media type `application/vnd.kontra.sealed+age;v=1`. The ciphertext is an [age](https://age-encryption.org/v1)
file in binary form (not armored) encrypted to the recipients with X25519 stanzas. Its plaintext is

```
plaintext = header || 0x00 || body
```

`header` is the RFC 8785 (JCS) serialization of a JSON object, ASCII only. The receiver re-serializes the header
it parsed and compares bytes: a header that is not canonical, has a duplicate key or an unknown field is rejected.

| Header field | A2A request (`dir: "req"`) | A2A reply (`dir: "res"`) | MCP request | MCP reply |
|---|---|---|---|---|
| `v` | `1` | `1` | `1` | `1` |
| `dir` | `"req"` | `"res"` | `"req"` | `"res"` |
| `iat` | Unix seconds when sealed | — | Unix seconds when sealed | — |
| `taskId` | the gate task id when known (a follow-up turn); absent on a new task's first message | the gate task id | — | the gate task id, for a durable task's result |
| `contextId` | the message's `contextId` (always set: the hiring node mints one for a new sealed task) | the task's context id | — | — |
| `session` | — | — | lowercase hex SHA-256 of the gate `Mcp-Session-Id` | same |
| `tool` | — | — | `params.name` | same |
| `messageId` / `artifactId` | the message's `messageId` | the message's `messageId`, or the artifact's `artifactId` | the client's JSON-RPC request id, as a string; for an answer to an input request, that request's `messageId` | same; for an input request, the upstream server's JSON-RPC id of it, as a string |
| `chunk` | — | for artifact updates, the 0-based count of updates for that `artifactId` this node sealed; 0 for a whole artifact | — | — |
| `part`, `parts` | the part's index in its message, and the message's number of sealed parts | the part's index and the count in its message, artifact or update | `0`, `1` | `0`, `1` |
| `kind` | `"text"` \| `"raw"` \| `"url"` \| `"data"` | same | `"data"` | `"data"` |
| `mediaType` | the original part's `mediaType`, or `""` | same | `""` | `""` |
| `kids` | the `key_id`s of the recipients, sorted | the hiring node's per-job key id | as A2A | as A2A |
| `replyTo` | the hiring node's per-job recipient (`age1…`) for this job | — | same | — |
| `rk` | 32 random bytes, base64url, per job: the reply key | — | same | — |
| `auth` | — | `base64url(HMAC-SHA256(rk, JCS(header without auth) ‖ 0x00 ‖ body))` | — | same |

`body` by `kind`: `text` the UTF-8 string; `raw` the bytes; `url` the UTF-8 URL; `data` the JCS of the JSON value
(for MCP requests, `arguments`; for MCP replies, `{"content": …, "structuredContent": …}`).

**The sealed part (A2A 1.0):**

```json
{"raw": "<base64 of the age ciphertext>",
 "mediaType": "application/vnd.kontra.sealed+age;v=1",
 "filename": "<the original filename, if any>",
 "metadata": {"kontra.sealed": {"original": "<the original mediaType, or the kind>"}}}
```

A part whose `mediaType` is the envelope type MUST be `raw`, from a caller or from a service; any other form is
refused by the gate (`InvalidRequest` from a caller, `upstream_error` from a service). The gate stores one whose
ciphertext exceeds 256 KiB (measured on the bytes, before base64) in its bucket and delivers it to a node as a
`kontra-blob:` part, fetched through the node's own tunnel ([`tunnel.md`](tunnel.md) §8.3); the node
authenticates the whole age file before acting on it.

**MCP:** the hiring node sends `params.arguments = {}` and `params._meta["sh.kontra/sealed"] = {"v": 1,
"envelope": "<base64 age ciphertext>"}`. The hosting node's reply replaces `content` with one embedded resource
and drops `structuredContent`:

```json
{"content": [{"type": "resource", "resource": {"uri": "kontra:sealed", "mimeType": "application/vnd.kontra.sealed+age;v=1", "blob": "<base64>"}}],
 "isError": false}
```

`isError` stays outside: the gate meters on it ([`relay.md`](relay.md) §11.4).

An upstream's `elicitation/create` or `sampling/createMessage` inside a sealed task (an **input request**) is
sealed by the hosting node like a reply (`dir: "res"`, to the job's `replyTo`, with `auth`), its `params` replaced
by `{"_meta": {<the related-task key>, "sh.kontra/sealed": {"v": 1, "envelope": "…"}}}`, the body the JCS of the
original `params` without `_meta`, `session` and `tool` those of the call that created the task, and `messageId`
the upstream server's own JSON-RPC id for the request, as a string, which the hosting node reads as it relays the
request (D175). The gate rewrites that JSON-RPC id to its own gate request id (`k.<taskId>.<seq>`,
[`relay.md`](relay.md) §11.7) and back on the answer, and never touches the envelope, which §2.5 of
[`relay.md`](relay.md) keeps exactly there (D174). The hiring node's answer is sealed like a request (`dir:
"req"`, `iat`, to the recipients, with the job's `replyTo` and `rk`) with the same `session`, `tool` and
`messageId` as the input request it answers, its `result` replaced the same way.

## 4. Keys and who holds them

| Key | Held by | Published as |
|---|---|---|
| Identity key (X25519) of each hosting node | the node (`~/.kontra/identity.key`) | `recipients[]` |
| Per-job key (X25519) of the hiring node: one per A2A task, MCP task, or MCP session (its plain calls and the tasks created in it) | the node (`~/.kontra/jobs/…`, [`tunnel.md`](tunnel.md) §2.3) | `replyTo` inside sealed request headers, and `metadata["kontra.sealed.replyTo"]` (must be equal, else `bad_header`) |
| `rk`, the reply key | inside the sealed request only | never |

The gate holds no private key and never sees `rk`.

## 5. Rules for each party

**Hiring node** (on `localhost:7777`, for a service whose verified card — or, for MCP, whose `initialize` result —
carries the extension):

1. Checks the service's pin (§7) before sealing anything.
2. Mints a per-job key and `rk`, and a `contextId` when the client gave none.
3. Seals every part of the outgoing message (MCP: `arguments`) to all `recipients`, with the request header of §3.
4. Opens every sealed part it receives for the job: checks `dir: "res"`, the ids (`taskId`/`contextId`, or MCP
   `session`/`tool`/`messageId`), its own key id in `kids`, `auth`, that each message's parts are exactly
   `0..parts-1` once, and, per `artifactId`, that each `(chunk, part)` arrives at most once except as a
   byte-identical repeat (the hosting node reuses ciphertext), that each chunk's parts are exactly `0..parts-1`,
   and that chunks arrive in order; an `append: false` artifact replaces everything the node held for that
   `artifactId`. A part that fails is dropped and the local client receives a failed status (`kontra.failure:
   sealed_rejected`). An MCP input request must carry the job's `session` and the task's `tool`, and a
   `messageId` it has not opened before in that task (`replayed` otherwise, and dropped); the node keeps which
   gate request id carried it, so that it seals the local client's answer to that id with that `messageId` (§3).
4a. In a job whose request was sealed, it refuses every unsealed part of a message, artifact or result, except the
   single text part of a terminal status message the gate wrote (it carries `metadata["kontra.failure"]`, or names
   an upstream terminal state that came without a message, [`relay.md`](relay.md) §6.3–§6.4), which it hands to
   the local client with `metadata["kontra.sealed"] = false`. Any other unsealed part is dropped, and the client
   receives `kontra.failure: sealed_rejected`.
5. Hands the local client plaintext parts with their original `mediaType`.

**Gate:**

- Never decrypts, never re-encrypts, never alters a sealed part or an MCP envelope, which its scrub of Kontra's
  keys keeps exactly where §3 puts it ([`relay.md`](relay.md) §2.5, D174).
- For a service with `params.required: true`, refuses before dispatch a message with a part whose `mediaType` is
  not the envelope type, or an MCP `tools/call` without `sh.kontra/sealed`, with `InvalidRequest` (`-32600`, as
  PRD §8 says; `sealed_unsealed_part`). With `required: false`, an unsealed part passes, and the gate sets
  `metadata["kontra.sealed"] = false` on it.
- Tells the hosting node the gate's ids in the stream preamble ([`tunnel.md`](tunnel.md) §8.1): `task` and
  `context` for A2A; `mcp_session` and `mcp_request_id` for MCP, on every exchange of a durable MCP task too; and
  the dispatch's upstream `tools/call` reuses the client's JSON-RPC id ([`relay.md`](relay.md) §11.5).
- Logs a node's rejection (§6) and fails the task `sealed_rejected` ([`relay.md`](relay.md) §6.4).

**Hosting node.** It keeps **durable job state** under `~/.kontra/hosted/<gate host>/<service>/` (0600, node-local
clocks only: a job is kept until 7 days after its last exchange; the per-service sets below for at least
`--sealed-max-age`). Per job (keyed by the gate task id; for plain MCP calls, by the session hash): the `replyTo`
and `rk` of its first sealed request, the upstream task and context ids the service returned for it, the
`(messageId, part)` pairs it forwarded, and, per gate task of the job, the MCP input requests it sealed and has
not seen answered, each with the time it sealed it. Per service: every `replyTo` that opened a job, with that
job's gate task id; the upstream context id recorded for each gate context; and, for MCP, every `(session,
messageId)` forwarded.
It reads every body it checks or seals with exact keys, as [`relay.md`](relay.md) §11.1 does, and refuses one with
a repeated or case-variant key at a level it reads, a service's reply included (`bad_header`, D172): the gate is
not trusted to have checked a request, and the node seals and opens bodies by re-encoding what it parsed, never
by patching bytes in place.
For each sealed part of an inbound message it decrypts with its identity keys, then checks:

1. `dir: "req"` and the message's parts exactly `0..parts-1`, each once (`bad_header`); its own key id in `kids`
   (`not_a_recipient`); `iat ≤ now + 300 s` and `now − iat ≤ max_age` (`replayed`), where `max_age` is
   `--sealed-max-age`, default 25 h, which must cover the operator's longest quote expiry plus `task.timeout` (a
   paid task waits for a person before it is dispatched).
2. **A2A, no `taskId` in the header** (a new task's first message): the forwarded message carries no `taskId`, and
   either no `contextId` or the upstream context id this node recorded for the preamble's gate context
   (`wrong_context`); the header's `contextId` equals the preamble's `context`; `messageId` equals the message's;
   and the header's `replyTo` has not opened a job for a **different** gate task on this node for this service
   (`replayed`; a first message whose preamble task equals the job it opened is the gate's own retry, and is
   accepted again subject to rule 5).
3. **A2A, `taskId` T in the header**: T equals the preamble's `task`, job T exists, and the header's `replyTo` and
   `rk` equal the job's (`wrong_job` otherwise); the forwarded body's `taskId`/`contextId` are T's recorded
   upstream ids.
4. **MCP request** (a `tools/call`): `tool` = `params.name`, `session` = the preamble's `mcp_session`,
   `messageId` = the preamble's `mcp_request_id` (`wrong_message` otherwise); once a job exists for the session,
   the header's `replyTo` and `rk` equal the job's (`wrong_job`); for a durable task the node binds the upstream
   `taskId` in the `CreateTaskResult` to the job and seals that task's `tasks/result` content, input requests and
   `statusMessage` to the job's `replyTo`.
4b. **MCP answer to an input request** (a JSON-RPC response the gate forwards on an exchange of a durable task):
   `session` = the preamble's `mcp_session`, `tool` = that of the call that created the preamble's `task`, and
   `messageId` = the response's JSON-RPC id, which names an input request this node sealed for that task less than
   `max_age` ago (`wrong_message` otherwise) and has not seen answered (`replayed`); the header's `replyTo` and
   `rk` equal the job's (`wrong_job`). Once the service has accepted it, the input request is answered.
5. For a request (rules 2–4), the `(messageId, part)` pair has not been forwarded before for the job, and for MCP
   the `(session, messageId)` pair not before for this service, whatever the job (`replayed`). An answer is
   checked by rule 4b alone: its `messageId` is the server's id, from another id space than the client's.

**Answers must be sealed (D175).** In a job whose request was sealed, a JSON-RPC response whose id is that of an
input request this node sealed MUST itself be sealed and pass rule 4b. One that carries no
`_meta["sh.kontra/sealed"]`, or one whose envelope is not exactly `{"v": 1, "envelope": "<base64>"}`, is refused
`bad_header` (§6), and nothing of it is forwarded to the service; a sealed one that fails a check above is refused
with that check's reason. This is the hosting node's half of the hiring node's rule 4a: a gate cannot answer the
service's question in plaintext on the buyer's behalf.

A job is opened, and its `replyTo` and pairs recorded, **only once the service has accepted the request**; a
request the node answered itself (a node-written `502`, say) opens and records nothing, so the gate's retry of it
is not a replay.

Then it replaces each sealed part with the plaintext part (original `kind`, `mediaType`, `filename`) and forwards
to the service, and seals every part of every reply (status messages, artifacts, artifact updates; for MCP the
result) to the job's `replyTo`, with the reply header and `auth`. A job whose request was not sealed gets unsealed
replies, unless the node was started with `--sealed-required <service>`, in which case it refuses the unsealed
request itself (`bad_header`). It reuses the ciphertext of a part it already sealed with the same header and body
during the job's life, so that the gate's polling of an upstream task sees identical bytes for unchanged parts.

## 5.1 Without a node: `kontra seal`

The escape hatch for scripting (PRD §8), not the path. `kontra seal --service <service URL> [--context <id>]
[--task <id>] [file | -]` fetches and verifies the service's card, mints a per-job key and `rk`, and prints one A2A
`Message` JSON (role user, a fresh `messageId`, the `contextId` given or minted, `taskId` when given) whose single
part is the sealed file, with `metadata["kontra.sealed.replyTo"]` set. The per-job key is written under
`~/.kontra/jobs/<gate host>/` so `kontra seal --open <part.json>` can open replies. Sealing in-process without the
CLI means reproducing §3 exactly; the age calls (Go `filippo.io/age`, Python `pyrage`) are documented in the
example, not offered as an SDK.

## 6. Rejections

A hosting node that refuses a sealed request answers the exchange itself with HTTP `400`, headers
`Kontra-Node: sealed_rejected` and `Kontra-Sealed-Reject: <reason>`, and a JSON-RPC error body (`-32602`, message
`sealed part rejected: <reason>`). Reasons: `undecryptable`, `bad_header`, `wrong_task`, `wrong_context`,
`wrong_message`, `wrong_job`, `replayed`, `not_a_recipient`. The gate records the rejection (an `audit` row and
the task's failure) and does not retry it (`sealed_replay`).

## 7. What sealing protects, and what it does not

`kontra connect` keeps a **pin per (gate, service)** in `~/.kontra/pins`: whether the service is sealed, the
recipient key ids, the seller key thumbprints that verified its upstream card, and whether the upstream card was
signed. A change from sealed to unsealed, or from signed to unsigned, is always refused. With `--pin-recipients`,
any new recipient or seller key is refused; without it, the change is refused too until the operator accepts it
with `--accept-change <service>`, and `kontra connect` names the change on stderr and in its log (there is no
local console, D164). An enrollment that adds a recipient is announced to the service's other
nodes ([`tunnel.md`](tunnel.md) §5).

| Threat | Holds? |
|---|---|
| The gate's database, logs or bucket read by someone (breach, operator, backup) | **Yes**: they hold ciphertext only (`sealed_no_plaintext`) |
| A buyer replaying another buyer's sealed part into its own task | **Yes**: the hosting node refuses it (§5: `replayed` for a first message, `wrong_job` for a follow-up), and a part that slipped through would make the service reply to the original buyer's `replyTo` |
| A gate replaying, reordering, dropping or duplicating sealed parts within a task | **Yes**: `messageId`/`artifactId`, `part`/`parts`, `chunk`, `iat` and the persisted job state |
| A gate replaying a first message to **another** hosting node of the same service within `max_age` | **No**: that node has not seen the `replyTo`. The service re-executes the request; the reply still goes to the original buyer's key |
| A gate forging a reply | **Yes** for service content: it does not know `rk`. The gate can still write its own failure text, which the hiring node marks unsealed (§5, rule 4a) |
| A gate answering a sealed task's input request itself: in plaintext, with an answer replayed, or for a request never sealed or sealed more than `max_age` ago | **Yes**: the hosting node forwards only a sealed answer that passes rule 4b; an unsealed one is refused `bad_header`, and the service never sees it (§5, D175) |
| A gate forging a request of its own | Not prevented (age does not authenticate senders), but it gains nothing it could not get as a buyer: the reply goes to its own key, and it cannot continue another buyer's job (`wrong_job`) |
| A gate that **substitutes recipients**, drops the extension, or strips the seller's signature | **Only with pins**: the buyer learns recipients from what the gate serves. Pins refuse a downgrade always, and a new recipient or seller key when `--pin-recipients` is set. `kontra connect` prints the recipient key ids and the seller key thumbprints on stderr and in its log when it first sees a service and on every change, for out-of-band comparison (D164, D187) |
| An MCP task's input requests, answers and status messages | **Yes**, for a task a sealed call created (§2) |
| A stolen identity key | Revocation stops future use. Parts already sealed to it open for whoever holds the key and the ciphertext: age has no forward secrecy. Retention ([`relay.md`](relay.md) §5.4) bounds the exposure |

So: confidentiality against an honest-but-curious or breached gate always; against an active gate only with pinned
recipients, and with the residuals above (D31).

## 8. Versions

The envelope's `v` and the media type parameter make later versions additive. A `v=2` is expected to use age's
native post-quantum hybrid recipients (`age1pq1…`, ML-KEM-768 + X25519, age v1.3.0+), which cannot be mixed with
X25519 recipients in one file: every hosting node of a service and the hiring node's per-job key would switch
together. Not v4 work.

## 9. Named tests

| Test | Expected |
|---|---|
| `sealed_no_plaintext` | A sealed task (A2A, a plain MCP call, and a durable MCP call whose upstream asks one elicitation) carrying a known marker string in every input, answer and output: the marker is absent from Postgres, the gate's logs and the bucket's objects |
| `sealed_replay` | A sealed part resent under another task id is rejected by the receiving node (`wrong_job` for a follow-up, `replayed` for a first message on the same node, `replayed` for an MCP call resent on the same session and `wrong_message` for one resent on another session), and the gate records the rejection; the gate's own retry of the same task after a node-written `502` is accepted; the residual of §7 (another node within `max_age`) is stated, not tested as a pass |
| `sealed_unsealed_part` | An unsealed part to a `required: true` service: `InvalidRequest` before dispatch |
