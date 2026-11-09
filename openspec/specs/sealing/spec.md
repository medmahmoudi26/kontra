# sealing Specification

## Purpose
Sealing is the extension `https://kontra.sh/ext/sealed/v1`: end-to-end encryption of a hired service's content between the hiring node (`kontra connect` on the buyer's side) and the hosting node (`kontra connect` hosting the service), so that the gate stores and relays only ciphertext it cannot open. This capability covers the card entry and MCP capability, what is sealed, the envelope, the keys, the obligations of the hiring node, the hosting node and the gate, `kontra seal`, rejections, and the threat model of a dishonest gate that splices, replays, cuts or re-routes a sealed exchange. Transport, enrollment of identity keys, relaying and hiring themselves belong to the `tunnel`, `node-enrollment-and-keys`, `relay-a2a`, `relay-mcp` and `hiring` capabilities. `spec/ext-sealed.md` is normative (envelope v1 layout in its §3); `docs/prd-v4.md` §8 is background, superseded where they differ.

## Known limitations
- **D227:** when the gate ends a sealed first message's exchange before the service replies, the hosting node records the request as accepted but never indexes the upstream task; the gate cannot list it, but a service whose task ids can be guessed lets the gate read it on another exchange (`spec/ext-sealed.md` §5 "A job's upstream task is learned…"; kontra#4).
- **D251:** a gate can seal a first message of its own, under a new gate task, into a buyer's gate context with that context's recorded upstream `contextId`; the hosting node cannot tell it from the buyer's next task, and an agent that keeps a conversation per context answers from the buyer's sealed conversation, sealed to the gate's key (`spec/ext-sealed.md` §7; kontra#4).
- **D252:** a sealed MCP call's task whose `CreateTaskResult` the hosting node never read is bound to no sealed session; an upstream that does not bind tasks to sessions can hand it to another session by an id the gate knows (`spec/ext-sealed.md` §5; kontra#4).
- **D253:** an MCP server that mints no `Mcp-Session-Id` is treated as one upstream session, so its sealed input requests are indexed under one empty session key (`spec/ext-sealed.md` §5 "MCP: a sealed job holds its upstream session"; kontra#4).
- **D255:** an upstream session that a sealed job held stays held until a `DELETE` of it is answered `2xx` or `404`; the hosting node's held-session set grows by one for every sealed session that is never deleted (`spec/ext-sealed.md` §5; kontra#4).
- **D256:** on a sealed MCP session, `resources/read`, `prompts/get`, `completion/complete`, `logging/setLevel` and other non-`tools/call` methods are not sealed; the gate can send them itself and read state a sealed call set up in the session (`spec/ext-sealed.md` §2, §7; kontra#4).
- A gate can replay a sealed first message to **another** hosting node of the same service within `max_age`: that node has not seen the `replyTo`, so the service runs the request again (the reply still goes to the original buyer's key) (`spec/ext-sealed.md` §7).
- A gate can forge a sealed request of its own (age does not authenticate senders); it gains only what a buyer could, apart from the D251 residual (`spec/ext-sealed.md` §7).
- A gate can decline a sealed MCP input request with an error answer of its own; this denies the request, it never answers it (D246, `spec/ext-sealed.md` §7).
- Recipient substitution, a dropped extension or a stripped seller signature is caught only by pins: on first use the hiring node trusts what the gate serves, and a `kid`-only seller signature with no key set named by the gate is pinned unverified (D223, D228, `spec/ext-sealed.md` §7).
- A stolen identity key opens every part already sealed to it: age has no forward secrecy; revocation stops future use and retention bounds the exposure (`spec/ext-sealed.md` §7).
- Not sealed, by design: task states, task/context/message/artifact ids, `metadata`, a part's `filename`, error messages (replaced by fixed text), the MCP tool name, `isError`, `tools/list`, and every MCP method other than those named in "Sealed Content" (`spec/ext-sealed.md` §2).
- Sealing an MCP call works only when both ends run `kontra connect`; a stock MCP client talking straight to the gate sees no envelope (`spec/ext-sealed.md` §2).
- `kontra seal --open` checks direction, key id, `auth` and `service` only; it does not apply the hiring node's per-message part, ordering and chunk checks, so a gate can drop, reorder or repeat parts of a reply opened this way undetected (`internal/sealing/seal.go` `openParts`; not stated by `spec/ext-sealed.md` §5.1).
- Envelope v1 uses X25519 only; post-quantum recipients are planned for a `v=2` and are not v4 work (`spec/ext-sealed.md` §8).

## Requirements

### Requirement: Sealed Card Entry
For a sealed service the gate SHALL publish the extension object (`uri` `https://kontra.sh/ext/sealed/v1`, entry `required: false`, `params.envelope` the v1 media type, `params.required` from configuration, default `true`) in the A2A card's `capabilities.extensions`, or for MCP under `capabilities.experimental["sh.kontra/sealed"]` of the `initialize` result, with `params.recipients` the service's enrolled identity keys that are neither revoked nor rotated (`spec/ext-sealed.md` §1; D27, D61).

#### Scenario: Recipients follow revocation
- **WHEN** a sealed service has two enrolled node keys and one of them is revoked
- **THEN** the served card's `params.recipients` lists only the key that is not revoked
- **AND** the entry's own `required` is `false` while `params.required` reflects the service's configuration

#### Scenario: Recipients do not depend on connection
- **WHEN** an enrolled, unrevoked node of a sealed service is disconnected
- **THEN** its key is still listed in `params.recipients`

#### Scenario: Sealing needs a tunnel backend
- **WHEN** a service is configured sealed on a direct (non-tunnel) backend
- **THEN** the gate does not serve that service rather than relay it in plaintext

### Requirement: Sealed A2A Service Needs A2A 1.0
A sealed A2A service SHALL be unavailable when its upstream card offers no A2A 1.0 interface: its card routes answer `503` and its tasks fail `service_unavailable` (`spec/ext-sealed.md` §1; D165).

#### Scenario: Upstream offers only A2A 0.3
- **WHEN** a sealed service's upstream card offers no A2A 1.0 JSON-RPC or HTTP+JSON interface
- **THEN** the gate answers the service's card route `503`
- **AND** a task sent to the service fails `service_unavailable`

### Requirement: Sealed Content
The nodes SHALL seal, both directions: for A2A, the content field (`text`, `raw`, `url` or `data`) of every part of messages and artifacts; for MCP, `tools/call` `arguments`, a result's `content` and `structuredContent`, the `params` of `elicitation/create` and `sampling/createMessage` requests made for a sealed call or its task and the client's results to them, and a sealed call's task `statusMessage`, which is replaced by `"sealed"` (`spec/ext-sealed.md` §2; D219, D264).

#### Scenario: A marker never reaches the gate in plaintext
- **WHEN** a sealed A2A task, a sealed plain MCP call, and a sealed durable MCP call whose upstream asks one elicitation each carry a known marker in every input, answer and output
- **THEN** the marker is absent from the gate's database, logs and bucket objects (`sealed_no_plaintext`)

### Requirement: Sealed Errors Keep Their Code Only
On a sealed job the hosting node SHALL relay a service's error with its code and status and fixed text in place of its message and data (a non-JSON MCP error becomes the node's own JSON-RPC error), and SHALL drop an artifact's `name` and `description`; the hiring node SHALL send a client's error answer to a sealed input request with its code and fixed text only; the gate SHALL keep no sealed service's error text (`spec/ext-sealed.md` §2; D195, D220, D238, D246).

#### Scenario: Error quoting the input
- **WHEN** a sealed A2A first message is refused by the service with an error whose message quotes a marker from the input
- **THEN** the error reaches the gate with its code and fixed text only
- **AND** the marker is absent from the gate's database and logs

#### Scenario: Artifact metadata dropped
- **WHEN** the service returns an artifact with a `name` and `description` on a sealed job
- **THEN** the hosting node relays the artifact without them
- **AND** the hiring node drops any `name` or `description` it receives on a sealed job's artifact

### Requirement: Envelope v1 Format
A sealed payload SHALL be a binary (not armored) age file with X25519 recipient stanzas whose plaintext is `header || 0x00 || body`, the header being the JCS serialization of an ASCII-only object with the members `spec/ext-sealed.md` §3 lists per direction and protocol, and the body encoded by `kind` (`spec/ext-sealed.md` §3).

#### Scenario: Sealed A2A part shape
- **WHEN** a node seals an A2A part
- **THEN** the part is `raw` with media type `application/vnd.kontra.sealed+age;v=1`, keeps its `filename`, and carries the original media type in `metadata["kontra.sealed"].original`

#### Scenario: Sealed MCP call shape
- **WHEN** the hiring node seals a `tools/call`
- **THEN** it sends `params.arguments` as `{}` and `params._meta["sh.kontra/sealed"] = {"v": 1, "envelope": …}`
- **AND** it sends no `Mcp-Param-*` header for the call

#### Scenario: Sealed MCP result shape
- **WHEN** the hosting node seals a `CallToolResult`
- **THEN** `content` is one embedded resource with URI `kontra:sealed`, `structuredContent` is dropped, and `isError` stays outside the envelope

### Requirement: Strict Header Parsing
A receiver SHALL reject with `bad_header` an envelope header that is not ASCII, has a duplicate, case-variant or unknown member, has a `v` other than `1`, carries a member forbidden for its `dir` (`iat`, `replyTo`, `rk` on a reply; `auth` on a request), has `op` on an A2A header, lacks it on an MCP one or mismatches `dir`, has `part` outside `0..parts-1` or `kids` not sorted and unique, or differs from the JCS re-serialization of what was parsed (`spec/ext-sealed.md` §3, §8; D172, D239).

#### Scenario: Non-canonical header
- **WHEN** a sealed part's header has its members in non-JCS order or contains white space
- **THEN** the receiving node rejects it with `bad_header`

#### Scenario: Unknown member
- **WHEN** a header carries a member not listed in §3
- **THEN** the receiving node rejects it with `bad_header`

#### Scenario: Unknown version
- **WHEN** a header carries `"v": 2`
- **THEN** the receiving node rejects it with `bad_header`

### Requirement: Whole-File Authentication Before Use
A node SHALL authenticate the whole age file before acting on any of its plaintext and SHALL reject a truncated, extended, altered or armored file as `undecryptable`; a ciphertext over 256 KiB that the gate stores and delivers as a `kontra-blob:` part SHALL be fetched through the node's own tunnel and inlined before it is opened, and an unfetched one rejected (`spec/ext-sealed.md` §3; `tunnel` capability).

#### Scenario: Truncated ciphertext
- **WHEN** the gate delivers a sealed part whose age file has its last chunk removed
- **THEN** the receiving node rejects it with `undecryptable` and forwards nothing of it

### Requirement: Keys And Their Holders
Each hosting node SHALL publish only the public half of its X25519 identity key; the hiring node SHALL mint a fresh X25519 per-job key and a fresh 32-byte `rk` per A2A task, MCP task and MCP session, publish the per-job public key only as the header's `replyTo` and the equal `metadata["kontra.sealed.replyTo"]`, and send `rk` only inside sealed requests; the gate SHALL hold no private key that opens a part (`spec/ext-sealed.md` §4; PRD §8, §12; `node-enrollment-and-keys` capability).

#### Scenario: replyTo metadata mismatch
- **WHEN** a sealed A2A message's `metadata["kontra.sealed.replyTo"]` differs from the `replyTo` in its sealed header
- **THEN** the hosting node rejects it with `bad_header`

#### Scenario: Two tasks, two keys
- **WHEN** a local client starts two new sealed A2A tasks through the hiring node
- **THEN** each task's request headers carry a different `replyTo` and a different `rk`

### Requirement: Hiring Node Pin Check Before Sealing
The hiring node SHALL check the service's pin before sealing anything: when it cannot read or verify the card of a service its pin says is sealed, it SHALL refuse the request with `502` and send nothing; with no pin or an unsealed pin it SHALL keep the card's absence as unknown for 5 s only (`spec/ext-sealed.md` §5 hiring rule 1, §7; D214).

#### Scenario: Card unreadable for a pinned sealed service
- **WHEN** the gate answers the card route of a service pinned sealed with `503`
- **THEN** the hiring node refuses the local client's request with `502`
- **AND** no request body reaches the gate

### Requirement: Pins Refuse Downgrade And Unaccepted Change
`kontra connect` SHALL keep a pin per (gate, service) of sealed-ness, recipient key ids, seller key thumbprints and signed-ness; it SHALL always refuse sealed-to-unsealed and signed-to-unsigned, and SHALL refuse a new recipient or a new or removed seller key unless the operator accepts the change pending at the run's first check with `--accept-change` (never, with `--pin-recipients`), naming the change on stderr and in its log (`spec/ext-sealed.md` §7; D164, D187, D223, D228, D248).

#### Scenario: Gate drops the extension
- **WHEN** a service pinned sealed is served with a card that no longer carries the sealed extension
- **THEN** the hiring node refuses to hire it, whatever `--accept-change` says

#### Scenario: Gate adds a recipient
- **WHEN** a pinned sealed service's card gains a recipient the pin does not hold and the operator has not accepted the change
- **THEN** the hiring node refuses to seal to the new set
- **AND** names the new recipient key id on stderr and in its log

### Requirement: Hiring Node Seals Every Outgoing Part
For a service whose verified card or `initialize` result carries the extension, the hiring node SHALL seal every part of each outgoing message (MCP: `arguments`, and answers to input requests) to all `recipients`, with a request header whose `service` is the hired service as the request path names it, minting a `contextId` for a new A2A task when the client gave none (`spec/ext-sealed.md` §5 hiring rules 2–3; D221).

#### Scenario: Context minted
- **WHEN** a local client sends a first message with no `contextId` to a sealed service
- **THEN** the hiring node sets a fresh `contextId` on the message and in its sealed header

#### Scenario: Sealed to every recipient
- **WHEN** a sealed service lists two recipients
- **THEN** the request header's `kids` holds both key ids, sorted, and either hosting node can open it

### Requirement: Hiring Node Refuses Cross-Service Follow-Ups And Redirects
The hiring node SHALL refuse a follow-up of a task whose job another service holds, SHALL refuse a non-`GET` request to a path that names no service, and SHALL never relay a gate `3xx` to its local client (`spec/ext-sealed.md` §5 hiring rules 1, 3; D247, D258, D259).

#### Scenario: Follow-up to the wrong service
- **WHEN** a local client sends a follow-up naming a task whose job was opened for service A to service B
- **THEN** the hiring node refuses the request locally and seals nothing

#### Scenario: Gate redirect
- **WHEN** the gate answers a hiring request with a `3xx`
- **THEN** the hiring node answers its local client `502` without a `Location` header

### Requirement: Hiring Node Opens Replies Against Its Job
The hiring node SHALL drop a job's sealed part, handing the client `kontra.failure: sealed_rejected`, unless it opens with the job's key and its header has `dir: "res"`, the hired `service`, the job's key id in `kids`, a valid `auth` under the job's `rk`, the job's ids (A2A `taskId`, `contextId`; MCP `session`, `tool`, `messageId`, a task's `taskId`), and for MCP the `op` of its role (`result` or `input`) (`spec/ext-sealed.md` §5 hiring rule 4; D218, D221, D239).

#### Scenario: Forged reply without rk
- **WHEN** the gate delivers a reply part sealed to the job's `replyTo` whose `auth` was not computed with the job's `rk`
- **THEN** the hiring node drops the part and the local client receives `kontra.failure: sealed_rejected`

#### Scenario: Reply from another service of the hosting node
- **WHEN** a reply header names a service other than the one the job hired
- **THEN** the hiring node drops the part with `wrong_job`

#### Scenario: Reply spliced from another task
- **WHEN** a reply part's header names a gate task other than the one the answer is for
- **THEN** the hiring node drops the part with `wrong_task`

#### Scenario: Input request presented as a result
- **WHEN** the gate delivers a sealed MCP input request's envelope as a `tools/call` result
- **THEN** the hiring node refuses it because its `op` is not `"result"`

### Requirement: Hiring Node Enforces Part And Chunk Order
The hiring node SHALL accept a message's sealed parts only as exactly `0..parts-1` under the message's `messageId`, and per `artifactId` each chunk's parts exactly `0..parts-1`, chunks in order, each `(chunk, part)` at most once unless byte-identical; an `append: false` artifact replaces what was held, and a polled Task's first sight of an artifact may start at any chunk (`spec/ext-sealed.md` §5 hiring rule 4; D215, D216).

#### Scenario: Gate reorders chunks
- **WHEN** the gate delivers chunk 2 of an artifact before chunk 1 on a streamed sealed task
- **THEN** the hiring node drops the out-of-order part and the local client receives `sealed_rejected`

#### Scenario: Gate repeats a chunk with other bytes
- **WHEN** the gate delivers a `(chunk, part)` the node already opened, with different ciphertext
- **THEN** the hiring node rejects it as `replayed`

#### Scenario: Gate drops a part of a message
- **WHEN** a reply message carries part 0 of a header saying `parts: 2` and no part 1
- **THEN** the hiring node drops the message's sealed content and the local client receives `sealed_rejected`

### Requirement: Hiring Node Refuses Unsealed Content In A Sealed Job
In a job whose request was sealed, the hiring node SHALL drop every unsealed part of a message, artifact or result and hand the client `kontra.failure: sealed_rejected`, except the single text part of a terminal status message the gate wrote, which it SHALL hand on marked `metadata["kontra.sealed"] = false`; it SHALL accept an MCP `CreateTaskResult` only for a sealed call that asked for a task, with nothing beside `task` and `_meta` (`spec/ext-sealed.md` §5 hiring rule 4a; D244, D264).

#### Scenario: Gate injects plaintext into a sealed reply
- **WHEN** the gate adds an unsealed text part to a `working` status message of a sealed task
- **THEN** the hiring node drops it and the local client receives `kontra.failure: sealed_rejected`

#### Scenario: Gate's own terminal failure text
- **WHEN** the gate fails a sealed task with a terminal status whose single text part carries `metadata["kontra.failure"]`
- **THEN** the hiring node hands that part to the client marked `metadata["kontra.sealed"] = false`

#### Scenario: Gate writes an MCP task status message
- **WHEN** a sealed session's task status carries a `statusMessage` other than `"sealed"`
- **THEN** the hiring node hands its client `"sealed"`

### Requirement: Hiring Node MCP Input Request Handling
The hiring node SHALL open a sealed MCP input request only with `op: "input"`, the job's `session`, a sealed call's `tool` and, for a durable task, its gate `taskId`, dropping a `messageId` already opened there; it SHALL seal the client's answer to the gate request id that carried the request, refuse a second answer, and on a sealed session relay no other server request but `ping` and `roots/list` (`spec/ext-sealed.md` §5 hiring rule 4; D218, D232, D243, D260, D261).

#### Scenario: Replayed input request
- **WHEN** the gate delivers the same sealed input request twice within a durable task
- **THEN** the hiring node hands the local client the first one only and drops the second as `replayed`

#### Scenario: Second answer refused
- **WHEN** the local client answers an input request the node relayed, then answers the same gate request id again
- **THEN** the hiring node refuses the second answer locally

### Requirement: Hiring Node MCP Session Binding
The hiring node SHALL refuse an `initialize` result whose `Mcp-Session-Id` is not visible ASCII, starts with `/`, or names a session it already holds; SHALL remove `Mcp-Session-Id` from every other answer; SHALL use a session only for the service it was initialized for; and SHALL refuse a local request that reuses the id of a sealed request of the session (`spec/ext-sealed.md` §5 hiring rule 4c; D226, D240, D241, D245, D260).

#### Scenario: Gate reuses a held session id
- **WHEN** an `initialize` result names an `Mcp-Session-Id` the hiring node already holds for another local client
- **THEN** the hiring node refuses that `initialize` result

### Requirement: Gate Relays Ciphertext Untouched
The gate SHALL never decrypt, re-encrypt or alter a sealed part or an MCP envelope, and SHALL keep the envelope where §3 puts it when scrubbing Kontra's own keys; it SHALL refuse a part that claims the envelope media type in any form but `raw` (`InvalidRequest` from a caller, `upstream_error` from a service) (`spec/ext-sealed.md` §3, §5 "Gate"; D174; `relay-a2a`, `relay-mcp` capabilities).

#### Scenario: Envelope type in text form
- **WHEN** a caller sends a part with media type `application/vnd.kontra.sealed+age;v=1` as a `text` part
- **THEN** the gate refuses the message with `InvalidRequest`

### Requirement: Gate Refuses Unsealed Content To A Required Service
For a service with `params.required: true` the gate SHALL refuse before dispatch, with `InvalidRequest` (`-32600`), a message carrying a part that is not the envelope type and an MCP `tools/call` without `_meta["sh.kontra/sealed"]`; with `params.required: false` it SHALL pass an unsealed part with `metadata["kontra.sealed"] = false` (`spec/ext-sealed.md` §5 "Gate"; PRD §8).

#### Scenario: Unsealed part to a required service
- **WHEN** a caller sends an unsealed text part to a service whose `params.required` is `true`
- **THEN** the gate answers `InvalidRequest` before any task or stream exists (`sealed_unsealed_part`)

#### Scenario: Unsealed part to an optional service
- **WHEN** a caller sends an unsealed part to a service whose `params.required` is `false`
- **THEN** the gate relays it with `metadata["kontra.sealed"] = false`

### Requirement: Gate Names Its Ids In The Preamble
The gate SHALL name its ids in the stream preamble to the hosting node (`task` and `context` for A2A; `mcp_session` and `mcp_request_id` for MCP, on every exchange of a durable MCP task too), and its upstream `tools/call` dispatch SHALL reuse the client's JSON-RPC id (`spec/ext-sealed.md` §5 "Gate"; `tunnel` capability).

#### Scenario: Durable task dispatch
- **WHEN** the gate dispatches a sealed `tools/call` of a durable MCP task to the hosting node
- **THEN** the preamble names `mcp_session` and `mcp_request_id`, and the forwarded `tools/call` carries the client's JSON-RPC id

### Requirement: Hosting Node Rule 1 For Every Sealed Request
For every sealed request (A2A message, MCP `tools/call`, MCP answer) the hosting node SHALL reject it unless `dir` is `"req"` and the sealed parts are exactly `0..parts-1` under one shared binding (`bad_header`), its key id is in `kids` (`not_a_recipient`), `iat ≤ now + 300 s` and `now − iat ≤ --sealed-max-age`, default 25 h (`replayed`), and `service` is the hosted service the preamble names (`wrong_job`) (`spec/ext-sealed.md` §5 hosting rule 1; D221).

#### Scenario: Stale iat
- **WHEN** the gate replays a sealed request whose `iat` is older than `--sealed-max-age`
- **THEN** the hosting node rejects it with `replayed`

#### Scenario: Delivered to another service of the same node
- **WHEN** the gate delivers a buyer's sealed request for service A to service B hosted by the same node
- **THEN** the hosting node rejects it with `wrong_job`

#### Scenario: Not a recipient
- **WHEN** a sealed request's `kids` does not include the hosting node's key id
- **THEN** the hosting node rejects it with `not_a_recipient`

### Requirement: Hosting Node Binds A New A2A Task
For a sealed A2A message whose header has no `taskId`, the hosting node SHALL reject it unless the forwarded message has no `taskId` (`wrong_task`) and no `contextId` or the one recorded for the preamble's gate context, the header's `contextId` is the preamble's `context` (`wrong_context`), its `messageId` is the message's (`wrong_message`), and its `replyTo` opened no job for a different gate task on any service of the node (`replayed`) (`spec/ext-sealed.md` §5 hosting rule 2; D221).

#### Scenario: First message replayed under another task on the same node
- **WHEN** the gate resends a sealed first message, already accepted for gate task T1, under gate task T2 to the same hosting node
- **THEN** the hosting node rejects it with `replayed` (`sealed_replay`)

#### Scenario: Gate's retry after a node-written 502
- **WHEN** the hosting node answered a sealed first message itself with a `502` and the gate retries it under the same gate task
- **THEN** the hosting node accepts the retry

#### Scenario: Header context differs from the preamble
- **WHEN** the gate delivers a sealed first message under a preamble whose gate context is not the header's `contextId`
- **THEN** the hosting node rejects it with `wrong_context`

### Requirement: Hosting Node Binds A Follow-Up To Its Job
For a sealed A2A message whose header names task T, the hosting node SHALL reject it with `wrong_job` unless T is the preamble's `task`, job T exists, and the header's `replyTo` and `rk` equal job T's; and SHALL reject it unless the forwarded `taskId` and `contextId` are T's recorded upstream ids (`spec/ext-sealed.md` §5 hosting rule 3).

#### Scenario: Follow-up spliced into another buyer's task
- **WHEN** the gate delivers a sealed follow-up of buyer A's job under buyer B's gate task
- **THEN** the hosting node rejects it with `wrong_job` (`sealed_replay`)

### Requirement: Hosting Node Binds An MCP Call
For a sealed `tools/call` the hosting node SHALL reject with `wrong_message` a header whose `op` is not `"call"`, whose `tool` is not `params.name`, whose `session` is not the preamble's `mcp_session`, or whose `messageId` is not the preamble's `mcp_request_id` and the forwarded JSON-RPC id; with `bad_header` a call whose `arguments` is not `{}`; and, once the session has a job, with `wrong_job` a call whose `replyTo` and `rk` are not the job's (`spec/ext-sealed.md` §5 hosting rule 4; D239).

#### Scenario: MCP call resent on another session
- **WHEN** the gate resends a sealed `tools/call` on a different MCP session
- **THEN** the hosting node rejects it with `wrong_message` (`sealed_replay`)

#### Scenario: Answer delivered as a call
- **WHEN** the gate delivers the buyer's sealed answer to an input request as a `tools/call`
- **THEN** the hosting node rejects it with `wrong_message` because its `op` is not `"call"`

#### Scenario: Another buyer's call on a held session
- **WHEN** a sealed call with a different `replyTo` arrives on a session whose job is already open
- **THEN** the hosting node rejects it with `wrong_job`

### Requirement: Hosting Node Seals Durable MCP Tasks To The Job
For a sealed `tools/call` that creates a task, the hosting node SHALL bind the upstream task named in the `CreateTaskResult` to the session's job and seal that task's result, input requests and `statusMessage` (as `"sealed"`) to the job's `replyTo`; a `tasks/*` request on a sealed session naming a task not bound to the job SHALL be rejected `wrong_task` (`spec/ext-sealed.md` §5 hosting rule 4; D218, D252, D264).

#### Scenario: Unbound task named on a sealed session
- **WHEN** the gate sends `tasks/get` on a sealed session naming an upstream task whose `CreateTaskResult` the node never read
- **THEN** the hosting node rejects it with `wrong_task`

### Requirement: Hosting Node Accepts Only Sealed Answers To Sealed Input Requests
The hosting node SHALL forward an answer to an input request it sealed only if it is sealed exactly as `{"v": 1, "envelope": …}` (`bad_header`), with `op: "answer"`, the preamble's `session`, and the request's `tool`, `taskId` and `messageId` for a request sealed under `--sealed-max-age` ago (`wrong_message`), unanswered (`replayed`), with the job's `replyTo` and `rk` (`wrong_job`), the request identified from the node's own relay record (`spec/ext-sealed.md` §5 rule 4b; D175, D218, D243, D253).

#### Scenario: Gate answers an elicitation in plaintext
- **WHEN** the gate sends an unsealed answer to a sealed durable task's elicitation
- **THEN** the hosting node refuses it with `bad_header` and the upstream receives nothing
- **AND** the gate fails the task `sealed_rejected` with an audit row

#### Scenario: Answer for a request never sealed
- **WHEN** the gate sends a sealed answer naming a `messageId` the hosting node never sealed
- **THEN** the hosting node refuses it with `wrong_message`

#### Scenario: Answer after max age
- **WHEN** the gate sends a sealed answer to an input request sealed more than `--sealed-max-age` ago
- **THEN** the hosting node refuses it with `wrong_message`

#### Scenario: Answer replayed
- **WHEN** the gate resends a sealed answer the service already accepted
- **THEN** the hosting node refuses it with `replayed`

### Requirement: Hosting Node Replay Sets
The hosting node SHALL reject with `replayed` a sealed request whose `(messageId, part)` pair the job already forwarded, or, for MCP, whose `(session, messageId)` pair any service of the node already forwarded; it SHALL keep the per-service `replyTo` and `(session, messageId)` sets at least `--sealed-max-age` plus 300 s, and a job at least 7 days after its last exchange or that window if longer (`spec/ext-sealed.md` §5 hosting rule 5; D221, D227).

#### Scenario: MCP call resent on the same session
- **WHEN** the gate resends a sealed `tools/call` on the same MCP session after the service accepted it
- **THEN** the hosting node rejects it with `replayed` (`sealed_replay`)

#### Scenario: A2A message resent in its own task
- **WHEN** the gate resends an accepted sealed follow-up under the same gate task
- **THEN** the hosting node rejects it with `replayed`

### Requirement: Hosting Node Records Only Accepted Requests
The hosting node SHALL open a job and record its `replyTo` and pairs only once the service has accepted the request or had it handed whole before the gate ended the exchange; it SHALL write the record durably before handing the request, remove it when the node answers the request itself, and after a restart treat a request that was with the service as accepted (`spec/ext-sealed.md` §5; D227, D233, D254).

#### Scenario: Crash while the service holds the request
- **WHEN** the hosting node stops while a sealed first message is with the service, restarts, and the gate resends the message under another gate task
- **THEN** the hosting node rejects the resend with `replayed`

### Requirement: Only A Job's Own Exchanges Reach Its Task
The hosting node SHALL let an exchange reach a sealed job's upstream task, context or upstream MCP session only when its preamble names that job's gate task (MCP: session): it SHALL reject a request naming another job's upstream task, `referenceTaskIds` included (`wrong_task`), or another gate context's upstream context (`wrong_context`), and refuse a service reply naming a sealed job's task on another exchange before forwarding any of it (`spec/ext-sealed.md` §5; D217, D226, D237).

#### Scenario: Gate reads a sealed task on another exchange
- **WHEN** the gate sends `GetTask` naming a sealed job's upstream task under a preamble of another gate task
- **THEN** the hosting node rejects it with `wrong_task`

#### Scenario: Gate references a sealed task
- **WHEN** a message under another gate task lists a sealed job's upstream task in `referenceTaskIds`
- **THEN** the hosting node rejects it with `wrong_task`

#### Scenario: Gate uses a held upstream session
- **WHEN** an exchange whose preamble names another session sends any message on the upstream MCP session a sealed job went on
- **THEN** the hosting node refuses it and relays nothing the server sends on that session to the exchange

### Requirement: Hosting Node Refuses Side Channels Around Sealed State
On a service with sealed jobs, or started with `--sealed-required`, the hosting node SHALL reject with `bad_header` a push configuration, a task list (`ListTasks`; MCP `tasks/list` outside a sealed session), a non-A2A-1.0 method, a body that is not one JSON object, an unsealed part beside sealed ones, an unsealed message or `tools/call` into a sealed job's context or session, an unsealed message with references, and a 2026-07-28 MCP request (`spec/ext-sealed.md` §5; D162, D227, D234–D237, D252).

#### Scenario: Push config to a service with sealed jobs
- **WHEN** the gate sends a message with an inline push notification config to a service that has sealed jobs
- **THEN** the hosting node rejects it with `bad_header`

#### Scenario: Gate's unsealed call on a sealed session
- **WHEN** the gate sends an unsealed `tools/call` on a session with a sealed job
- **THEN** the hosting node rejects it with `bad_header`

#### Scenario: Gate's plaintext beside the buyer's sealed parts
- **WHEN** the gate adds an unsealed part to a buyer's sealed message
- **THEN** the hosting node rejects the message with `bad_header`

#### Scenario: Dot segments hide a task list
- **WHEN** the gate sends `GET /x/../tasks` to a service with sealed jobs
- **THEN** the hosting node reads the cleaned route and rejects it as a task list with `bad_header`

### Requirement: Hosting Node Seals Every Reply To The Job
The hosting node SHALL hand the service plaintext parts (original `kind`, `mediaType`, `filename`), SHALL seal every part of every reply of a sealed job (status messages, artifacts, artifact updates; MCP results and input requests) to the job's `replyTo` with a reply header naming the hosted service and carrying `auth`, and SHALL reuse the ciphertext of a part it already sealed with the same header and body in the job (`spec/ext-sealed.md` §5; D216, D221).

#### Scenario: Unchanged parts on polling
- **WHEN** the gate polls a sealed upstream task twice and a part has not changed
- **THEN** the hosting node relays byte-identical ciphertext for that part both times

#### Scenario: Sealed-required node
- **WHEN** a hosting node started with `--sealed-required svc` receives an unsealed message for `svc`
- **THEN** it rejects the request with `bad_header`

### Requirement: Node Rejection Answer
A hosting node that refuses a sealed request SHALL answer the exchange itself with HTTP `400`, headers `Kontra-Node: sealed_rejected` and `Kontra-Sealed-Reject: <reason>` (one of `undecryptable`, `bad_header`, `wrong_task`, `wrong_context`, `wrong_message`, `wrong_job`, `replayed`, `not_a_recipient`), and a JSON-RPC error body with code `-32602` and message `sealed part rejected: <reason>` (`spec/ext-sealed.md` §6).

#### Scenario: Rejection shape
- **WHEN** the hosting node refuses a sealed JSON-RPC request with `wrong_job`
- **THEN** it answers `400` with `Kontra-Sealed-Reject: wrong_job` and a `-32602` error carrying the request's id

### Requirement: Gate Handling Of A Node Rejection
The gate SHALL record a hosting node's sealed rejection as an audit row naming the reason and fail the task `sealed_rejected`, and SHALL NOT retry it; a plain MCP call's client SHALL get the node's JSON-RPC error with HTTP `200`, its request id and the session header, its `calls` row marked `is_error` (`spec/ext-sealed.md` §5 "Gate", §6; D202; `docs/pentest-m3.md` F-4).

#### Scenario: Gate records and does not retry
- **WHEN** the hosting node answers a sealed A2A request `400` with `Kontra-Sealed-Reject: wrong_job`
- **THEN** the gate writes an audit row naming `wrong_job`, fails the task `sealed_rejected`, and does not resend the request

#### Scenario: Plain MCP call refused
- **WHEN** the hosting node refuses a sealed plain `tools/call`
- **THEN** the client receives the node's JSON-RPC error with HTTP `200` and the call is recorded `is_error`

### Requirement: Kontra Seal Without A Node
`kontra seal --service <url>` SHALL fetch and verify the service's card against its pin, mint a per-job key and `rk` stored under `~/.kontra/jobs/<gate host>/`, and print one A2A `Message` whose single part is the sealed input with `metadata["kontra.sealed.replyTo"]` set; `--api-key` SHALL be sent only to the `--service` origin, never to the gate's key set, and no key SHALL be read from the environment (`spec/ext-sealed.md` §5.1; D222).

#### Scenario: Sealed message printed
- **WHEN** an operator runs `kontra seal --service <url> file`
- **THEN** it prints a user `Message` with a fresh `messageId`, a `contextId`, one sealed part whose header names the service, and `metadata["kontra.sealed.replyTo"]`

### Requirement: Kontra Seal Opens And Follows Up Per Job
`kontra seal --open` SHALL write plaintext only for replies with `dir: "res"`, a valid `auth` and their job's `service`, binding the job to the gate task the reply names; `--task <id>` SHALL seal a follow-up with that job's key, `rk` and `contextId`, and SHALL refuse a task with no job (`spec/ext-sealed.md` §5.1; D221).

#### Scenario: Follow-up without an opened reply
- **WHEN** an operator runs `kontra seal --task T` for a task whose replies were never opened with `--open`
- **THEN** `kontra seal` refuses and says to `--open` one of the task's replies first

#### Scenario: Reply of another service
- **WHEN** `kontra seal --open` is given a reply whose header names a service other than its job's
- **THEN** it refuses the reply and writes no plaintext
