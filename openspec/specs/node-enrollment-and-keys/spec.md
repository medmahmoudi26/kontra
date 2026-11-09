# node-enrollment-and-keys Specification

## Purpose
This capability covers how a node identifies itself to a gate and how a gate decides which services a node's key may serve. It includes the node's age identity key and key id, per-job keys, the node's credentials file, enrollment through service tokens, tenant tokens and `enroll:` API keys, anonymous sessions on Cloud with their `anon_limits`, `kontra login`, `kontra publish`, and revocation and rotation. The handshake that carries these credentials and the streams that follow it belong to `tunnel`. Sealing envelopes belong to `sealing`, and listing semantics on the control plane belong to `listings-and-pricing`. `spec/tunnel.md` §2, §5, §6 and §12 are normative, together with the decisions they cite in `docs/decisions.md` and PRD §6, §8 and §12.

## Known limitations
- `kontra login` cannot enforce the rule that an authorization server issues a gate API key only for `resource` equal to that gate's origin (D305). The gate being logged into names its own authorization server, so a gate whose RFC 9728 document points at another gate's server could receive a key for the person's account there. The CLI only prints the authorization server's origin next to the gate's. This rule is M5's to revise, together with OIDC principals (kontra-cloud#60). (`spec/tunnel.md` §6.3 step 4)
- `kontra login` accepts only a gate API key (`kk_…`) as the access token. JWT access tokens are refused, which is an interim rule of this revision. (`spec/tunnel.md` §6.3 step 4)
- Anonymous storage is bounded only loosely. One tenant may hold up to `tasks` × `parts_bytes`, and ended tenants keep their objects until `purge_after`. The default bound is about 9.75 GiB per /24 or /48 network and about 3.2 TiB gate-wide (D292 SPEC-CONFLICT with `spec/relay.md` §6.6; D304, D310). (`spec/tunnel.md` §6.2)
- Revoking a key does not protect parts already sealed to it. age has no forward secrecy, so only retention bounds the exposure. (`spec/tunnel.md` §15)

## Requirements

### Requirement: Identity Key File
On first run, `kontra connect` SHALL create an age identity file at `~/.kontra/identity.key`, or at the path that `--key` or `KONTRA_KEY` names, with file mode `0600` in a `0700` directory. It MUST refuse to start if the file is accessible to group or others. The first `AGE-SECRET-KEY-1` line is the active key, which both proves possession and serves as the sealing recipient of every service the node hosts. Later lines are decrypt-only. (`spec/tunnel.md` §2.1; D29)

#### Scenario: Key file too open
- **WHEN** `~/.kontra/identity.key` has mode `0644`
- **THEN** `kontra connect` refuses to start and names the file

#### Scenario: First run
- **WHEN** `kontra connect` runs and no identity file exists
- **THEN** it writes a new age identity, mode `0600`, whose comment lines include the public key and the `k1_` key id

### Requirement: Key Id Derivation
A key id SHALL be `"k1_"` followed by the first 26 characters of the lowercase, unpadded base32 SHA-256 of the canonical `age1…` recipient string, 29 characters in total. The gate MUST store and check the full public key, and MUST NOT authenticate anything by key id alone. (`spec/tunnel.md` §2.2)

#### Scenario: Hello with a mismatched kid
- **WHEN** a hello's `kid` is not the derivation of its `recipient`
- **THEN** the gate refuses it with `bad_proof`

### Requirement: Per-Job Keys
When it hires a sealed service, the node SHALL mint an X25519 key per job (A2A task, MCP task, or MCP session for plain calls) at `~/.kontra/jobs/<gate host>/<job id>.key` (0600), and delete it once the job is terminal and any `kontra.keyRetainUntil` has passed. It MUST NOT delete a key while an exchange of its session is open, and answers `404`, forwarding nothing, for a sealed session it no longer knows. (`spec/tunnel.md` §2.3; D224, D241, D242)

#### Scenario: Session closed by the gate
- **WHEN** the gate answers `404` for a sealed MCP session whose tasks are all done and which has no open exchange
- **THEN** the node deletes that session's job key

#### Scenario: Key kept during an open stream
- **WHEN** an idle sweep runs while a `GET` stream of the session is still open
- **THEN** the key stays until the last exchange ends

#### Scenario: Idle session forgotten
- **WHEN** a sealed MCP session has had no exchange for the service's `session_idle_s` (a day when absent or out of range) and its tasks are done
- **THEN** the node deletes its key

#### Scenario: Key kept for a review window
- **WHEN** a sealed task completes with `kontra.keyRetainUntil` in the future
- **THEN** its job key stays until that time and is deleted after

#### Scenario: Unknown sealed session after restart
- **WHEN** a local client uses a sealed MCP session id from before the node restarted
- **THEN** the node answers `404` without forwarding, and the client initializes again

### Requirement: Credentials Scoped to Gate Origin
The node SHALL keep API keys and enrollment tokens per gate origin in `~/.kontra/credentials`, mode `0600`. It MUST send a credential only to the origin it was issued for. `--api-key` and `--enroll` SHALL accept a literal value, `@file` or `-` (stdin). The file records the SHA-256 of each token that a `welcome` confirmed, and the node never sends those tokens again. A token refused with `enroll_invalid`, or an API key refused with `auth_invalid`, MUST be removed. (`spec/tunnel.md` §2.4; D124)

#### Scenario: Token reused on restart
- **WHEN** a node restarts with the same `--enroll` token after a `welcome` already confirmed it
- **THEN** the node does not send that token in its hello

#### Scenario: Credential of another gate
- **WHEN** a node holds an API key for gate A and dials gate B
- **THEN** the hello to gate B carries no `auth`

### Requirement: Lost-Welcome Probe
When the node did not receive a handshake's outcome after sending its proof (a cut connection, a `1011` close, or a stopped process), it SHALL first send a probe: a hello with the active key alone, without `enroll`, `rotate_from`, `auth` or `anonymous`. A `welcome` means the change committed. `unknown_key`, or `service_denied` for a known key, means it did not, and the full hello follows at once. (`spec/tunnel.md` §2.4; D136, D285; `anon_lifecycle`)

#### Scenario: Enrollment did not commit
- **WHEN** the connection drops after the proof of an enrolling hello, and the enrollment did not commit
- **THEN** the probe gets `unknown_key`, even on a gate with anonymous sessions, and the node sends its tokens again

### Requirement: Enrollment Authorities
The gate SHALL enroll a key for a service only on one of three authorities: a service token (`ket_` + 32 base62, single use, 15 minutes, stored as SHA-256, bound to one existing enabled tunnel service); an operator-issued tenant token bound to a tenant and a list of service names; or an `auth` API key with an `enroll:` scope where `tunnel.enroll_with_api_key` is on (off by default on self-hosted gates). Only a tenant token MAY create a service. (`spec/tunnel.md` §5; PRD §6, §12; D40, D331)

#### Scenario: Service token for a missing service
- **WHEN** an operator runs `kontra-gate enroll` for a service that does not exist or is disabled
- **THEN** no token is minted

#### Scenario: API key without an enroll scope
- **WHEN** a hello carries an `auth` key whose only scope is `hire` and declares a service that the key is not enrolled for
- **THEN** the gate refuses it with `service_denied` and enrolls nothing

#### Scenario: Tenant token creates a service
- **WHEN** a tenant token names a service that does not exist and the hello declares it
- **THEN** the gate creates it with `allow: [org:<tenant>]` and binds the key

### Requirement: Adding a Replica Needs Service Authority
Binding a further key to a service that already has an enrolled key MUST need a service token or an `enroll:` scope, and a tenant token MUST NOT do it. The service's existing nodes SHALL be told with `{"t":"enrolled","service","key"}` on their control streams. (`spec/tunnel.md` §5, §11; D138)

#### Scenario: Tenant token targets an existing service
- **WHEN** a second node presents only a tenant token for a service that already has an enrolled key
- **THEN** the token does not bind that service

#### Scenario: Replica added with a service token
- **WHEN** a second node enrolls for an existing service with a service token
- **THEN** the first node receives `enrolled` naming the service and the new key id

### Requirement: Atomic and All-or-Nothing Admission
The gate SHALL consume a token in the transaction that inserts `key_services`, after the proof verified, with a conditional update on `used_at IS NULL AND expires_at > now()`. Admission is all-or-nothing: a declared service neither enrolled nor enrolling is `service_denied` and nothing is enrolled. A token that applies to no declared service is `enroll_invalid` and not consumed. (`spec/tunnel.md` §5; D132, D133; `key_scope`)

#### Scenario: One undeclarable service
- **WHEN** a hello declares services `a` (with a valid token) and `b` (no authority)
- **THEN** the gate answers `service_denied`, the token for `a` is not consumed, and no binding is written

#### Scenario: Token reuse race
- **WHEN** two handshakes present the same unused token concurrently
- **THEN** at most one consumes it and enrolls

#### Scenario: Key of another tenant
- **WHEN** an enrollment would bind a key already enrolled under another tenant
- **THEN** the gate answers `enroll_invalid`

#### Scenario: Kind mismatch
- **WHEN** a service token for an `mcp` service is presented for a declared `a2a` service of that name
- **THEN** the gate answers `kind_mismatch`

### Requirement: Unknown and Hire-Only Keys
An unknown key that nothing in its hello enrolls MUST be refused with `unknown_key`, whatever else the hello carries. There are two exceptions. A hello with no declared service and an `auth` key with the `hire` scope admits the key hire-only, in the API key's tenant. An anonymous request on a gate that opens anonymous sessions is the other. A known key with no binding is admitted only on such a hire-only hello, or on one that enrolls it. (`spec/tunnel.md` §5; D133, D206, D285)

#### Scenario: Hire-only node
- **WHEN** an unknown key's hello declares no services and carries a `hire`-scoped API key
- **THEN** the gate admits it with no bindings, and its hiring principal is that API key

#### Scenario: Hire-only key after its API key is revoked
- **WHEN** that key later says hello without an `auth` key
- **THEN** it is not admitted to hire as `node:<key_id>`

### Requirement: Session Services Come from Bindings
The services a session serves MUST come from `key_services` and never from a request. The `services` in the hello only narrow them. A binding that was revoked, or a bound service that is no longer an enabled tunnel service, is left out. Only a service token or a tenant token re-enrolls a revoked binding; an `enroll:` scope never does. Every enrollment, revocation and rotation SHALL write an `audit` row. (`spec/tunnel.md` §5; D130; `key_scope`)

#### Scenario: Revoked binding with an enroll scope
- **WHEN** a key whose binding to service `s` was revoked presents an `enroll:*` API key that declares `s`
- **THEN** `s` is not re-enrolled and is left out of `welcome.services`

### Requirement: Anonymous Session Admission
A gate with `tunnel.anonymous` enabled (requires `tenancy: multi`) SHALL admit as anonymous a hello carrying `anonymous: true` and neither `enroll` nor `auth`, from an unknown key or one of a live anonymous tenant; a gate without it MUST answer such a hello `anonymous_unavailable`. A hello without `anonymous: true` MUST NOT open or resume an anonymous session. (`spec/tunnel.md` §4, §6; D277, D285, D292, D309)

#### Scenario: Gate without anonymous sessions
- **WHEN** an unknown key sends `anonymous: true` to a gate without `tunnel.anonymous`
- **THEN** the gate answers `anonymous_unavailable`

#### Scenario: Probe on an anonymous gate
- **WHEN** an unknown key sends a hello without `anonymous` to a gate that opens anonymous sessions
- **THEN** the gate answers `unknown_key`

#### Scenario: Node with credentials never asks
- **WHEN** `kontra connect` holds an API key, or a pending or consumed token, for the origin, or was given `--enroll` or `--api-key`
- **THEN** its hello does not carry `anonymous`

### Requirement: Anonymous Tenant Shape and URLs
For a new anonymous session the gate SHALL create a tenant `anon-<16 lowercase hex>` with `expires_at = now + ttl` and `created_from` the client's network as a CIDR, never the address; enroll the declared services under it with `allow: [org:anon-…]`, plus `anonymous` for those in `--public`; and answer `welcome` with `anonymous.expires_at`. Anonymous services MUST NOT be `listed` or appear in `search`. (`spec/tunnel.md` §6.1, §6.2; D287; `anon_lifecycle`)

#### Scenario: Public anonymous service
- **WHEN** a node connects anonymously with `--public hello`
- **THEN** a caller without a credential can call `hello`, and a service the node did not mark public answers `404` to that caller and to another node

#### Scenario: Account takes an anon slug
- **WHEN** an account tenant is created with a slug of the form `anon-` plus 16 hex digits
- **THEN** the database refuses it

### Requirement: Anonymous Session Resumption and Expiry
The same key reconnecting before `expires_at` SHALL get the same tenant back, with services it no longer declares disabled. The gate SHALL send `expiring` ten minutes before `expires_at`; at it, revoke the key's bindings to the tenant's services, disable them (`404`), abort dispatches in flight, send `revoked` and close. The key itself is not revoked: its next hello opens a new tenant with a new slug. (`spec/tunnel.md` §6.1; D277, D287, D297; `anon_lifecycle`)

#### Scenario: Reconnect before expiry
- **WHEN** an anonymous node reconnects with the same key before `expires_at`
- **THEN** `welcome` names the same `anon-…` tenant

#### Scenario: Expiry
- **WHEN** `expires_at` passes
- **THEN** the node first received `expiring`, the session closes with `revoked`, the URLs answer `404`, and the key's next hello gets a different slug

#### Scenario: Task in flight at expiry
- **WHEN** a task of the tenant is in flight at `expires_at`
- **THEN** it fails `tunnel_lost` if an upstream task exists, and `service_unavailable` at its next dispatch otherwise, and the node exits saying the anonymous session expired

### Requirement: Anonymous Tenant Purge
`purge_after` (default 24 h) after a tenant ends, every multi-tenant gate SHALL delete what it holds: keys, bindings, tunnels, services, calls, enrollments, tasks with their bucket objects, MCP sessions and `anon_calls` rows. The tenant row stays as a tombstone with `created_from`, and no table keeps the client's address. This runs whether or not `tunnel.anonymous` is still on. (`spec/tunnel.md` §6, §6.1; D167, D287, D292, D304, D309; `anon_lifecycle`)

#### Scenario: Purge runs
- **WHEN** `purge_after` has passed since an anonymous tenant ended
- **THEN** its keys, services, enrollments, tasks, calls, MCP sessions and bucket objects are gone, and its tenant row remains as a tombstone

### Requirement: Anonymous Limits
The gate SHALL enforce the `anon_limits` of §6.2 (defaults: `ttl` 2 h, 3 tenants per network, 4 services, 4 tasks as host and as hirer, 8 `calls`, 8 `streams`, 8 `hiring_streams`, 64 MiB `parts_bytes`, 1000 `max_tenants`, `purge_after` 24 h). A creation limit is `rate_limited` at the handshake, `max_tenants` is `anonymous_unavailable`, and a runtime limit is `429` before anything is forwarded or metered. (`spec/tunnel.md` §6.2; D277, D287, D304, D308; `anon_limits`)

#### Scenario: Fourth tenant from one network
- **WHEN** a fourth anonymous tenant is requested from one IPv4 /24, or from one IPv6 /48, while three are live
- **THEN** the handshake is refused `rate_limited`

#### Scenario: Fifth service
- **WHEN** an anonymous hello would bring the tenant to five services that are not disabled
- **THEN** it is `service_denied`, nothing is enrolled, and reconnecting with new names never takes the tenant past four

#### Scenario: Ninth concurrent MCP call
- **WHEN** eight requests or eight open `GET` streams are in flight to an anonymous tenant's MCP services and a ninth of the same kind arrives
- **THEN** the ninth is answered `429` with `-32600`

#### Scenario: Fifth concurrent task
- **WHEN** an anonymous tenant has four non-terminal tasks as host (or as hirer) and a fifth is created
- **THEN** it is refused `429`

#### Scenario: Parts over the tenant total
- **WHEN** a caller's message would take the tenant's stored parts past `parts_bytes`
- **THEN** it is `InvalidParams` `anonymous storage limit` before any task row

### Requirement: Anonymous Key Conversion
A key of a live anonymous tenant presenting `enroll` tokens, or an account or team `auth` key with `enroll:*`, SHALL be converted in the admission's transaction: its served services are created or bound in the target tenant under the same names, the key moves there, the anonymous tenant expires at once, and an `audit` row records it. An `auth` key without an `enroll:` scope converts nothing and only becomes the hiring principal. (`spec/tunnel.md` §6.1, §6.3; D286, D296; `login_converts`)

#### Scenario: Convert after login
- **WHEN** an anonymous node restarts with an account API key that has `enroll:*`
- **THEN** `welcome` has no `anonymous`, names the account's URLs, the services stop expiring, the anonymous URLs answer `404`, and an `audit` row records the conversion

#### Scenario: Conversion hits a kind clash
- **WHEN** the account already has a service of the same name and another kind
- **THEN** the hello is `kind_mismatch` and nothing moves

### Requirement: Kontra Login Flow
`kontra login` SHALL obtain an API key through the gate's advertised OAuth sign-in: RFC 9728 discovery whose `resource` must be `{gate origin}/mcp`, RFC 8414 metadata whose `issuer` must match exactly, one dynamic registration per gate origin and issuer, then the code flow with PKCE `S256`, `state`, a loopback redirect and `resource` = the gate's origin. It MUST store only a `kk_…` token, as that origin's API key (0600). (`spec/tunnel.md` §6.3; D278, D296, D305; `login_converts`)

#### Scenario: Gate without sign-in
- **WHEN** `kontra login` runs against a gate that serves no RFC 9728 document
- **THEN** it exits 1 and tells the person to ask the operator for an API key (`--api-key`)

#### Scenario: Non-API-key access token
- **WHEN** the token endpoint returns a JWT access token
- **THEN** `kontra login` refuses it and stores nothing

#### Scenario: Forgotten client registration
- **WHEN** the token endpoint answers `invalid_client`
- **THEN** `kontra login` forgets the `client_id`, so that the next login registers again

#### Scenario: Mismatched state or issuer
- **WHEN** the callback's `state` differs, or its `iss` is not the issuer
- **THEN** the login ends and nothing is stored

#### Scenario: Secrets never shown
- **WHEN** a login runs with `--no-browser`
- **THEN** the authorization URL is printed, but neither the code nor the PKCE verifier is printed or logged, and the token request follows no redirect

### Requirement: Kontra Publish
`kontra publish` SHALL validate the listing file (one YAML or JSON document, at most 64 KiB) against `spec/schema/listing.json` and stop on a refused file; `--dry-run` stops there too. It signs the person in with `resource` = the control plane's URL, sends the token only to the control plane, follows no redirect and never stores or prints it. It MUST NOT replace an existing listing unseen. It takes no API key and computes no amount. (`spec/tunnel.md` §6.4; D323)

#### Scenario: Existing listing without a version
- **WHEN** the service already has listing version 3 and `kontra publish listing.yaml` is run without `--version` or `--replace`
- **THEN** the CLI prints version 3's price, visibility and published state, names `--version 3`, sends no `PUT`, and exits 1

#### Scenario: Stale version
- **WHEN** the `PUT` names a version that the listing has moved past
- **THEN** the control plane answers `409 stale_version`, nothing is published, and the CLI names the current version

### Requirement: Key Revocation
An operator SHALL revoke a key with `kontra-gate enroll --revoke <key_id>` or the console: the holding replica closes the MCP sessions pinned to the session as `revoked`, sends `revoked` and closes it, and every later handshake of the key is `revoked`. Revocation does not follow `rotated_to`. Each replica MUST re-read revocations at least every 5 s and whenever its `LISTEN` reconnects. (`spec/tunnel.md` §12, §15; D128)

#### Scenario: Revoked key reconnects
- **WHEN** an operator revokes a connected node's key
- **THEN** the node receives `revoked`, its session closes, and its next handshake is refused `revoked`

#### Scenario: Missed NOTIFY
- **WHEN** a replica misses the `NOTIFY` for a revocation
- **THEN** it applies the revocation within 5 s

#### Scenario: Revoking a rotated key
- **WHEN** an operator revokes a key that was rotated to a successor
- **THEN** the successor stays enrolled, and the command names the newest still-enrolled key of the chain and how to revoke it

### Requirement: Binding and API Key Revocation
`kontra-gate enroll --revoke <key_id> --service <name>` SHALL revoke one binding: the holding replica removes the service from the session, closes its pinned MCP sessions, aborts its in-flight dispatches and sends `revoked` with `service`. The gate MUST open no further stream for it regardless of the node. A revoked API key MUST make later hiring streams with it get `401`; hosting continues. (`spec/tunnel.md` §12, §13; D129, D131)

#### Scenario: One binding revoked
- **WHEN** an operator revokes a node's binding to service `s`
- **THEN** the gate opens no further stream for `s` on that session, and the node's other services keep working

#### Scenario: Hiring key revoked
- **WHEN** the API key in a session's `auth` is revoked
- **THEN** the node's next hiring stream gets `401`, the control stream carries `auth_revoked`, and hosted services still receive requests

### Requirement: Key Rotation
`kontra connect --rotate` SHALL write a new first line to the identity file under `flock(2)` keep the old key decrypt-only, and prove both keys with `rotate_from`. In one transaction the gate adds the new key to the old key's tenant, copies its bindings, and marks the old key rotated with a 24 h overlap; a new handshake of the old key is `revoked`. The node drops the old line 24 h after the last gate's confirmation. (`spec/tunnel.md` §12; D125–D127, D129, D140, D153)

#### Scenario: Rotation is make-before-break
- **WHEN** a node rotates while a task runs on the old key's session
- **THEN** the old session keeps serving that task until it ends or the overlap passes, new attempts select only the new key, and parts sealed to the old key still open on the node

#### Scenario: Anonymous key rotation
- **WHEN** a hello's `rotate_from` names a key of an anonymous tenant
- **THEN** it is refused `enroll_invalid` and nothing is written

#### Scenario: rotate_from names a rotated key
- **WHEN** `rotate_from` names a key already rotated
- **THEN** the gate answers `enroll_invalid`, and the node sends its hello again at once without `rotate_from` and never names that key to that gate again

#### Scenario: No file lock available
- **WHEN** `kontra connect --rotate` runs where `flock(2)` is unavailable
- **THEN** it refuses to rotate and leaves the identity file unchanged

### Requirement: Sealing Recipients Follow Bindings
The sealing recipients of a service SHALL be the keys enrolled for it that are neither revoked nor rotated, whether or not they are connected. After a rotation they are republished at once without the old key. (`spec/tunnel.md` §12; D61, D126)

#### Scenario: Offline replica stays a recipient
- **WHEN** one of two enrolled nodes of a sealed service is offline
- **THEN** its key remains a published recipient, and it can serve a task sealed to it after it reconnects
