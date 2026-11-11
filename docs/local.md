# A gate on your machine: the local development profile

Connect your A2A servers to a gate on your own machine and watch them in the same console as Kontra Cloud: every task,
conversation and A2A call, kept in the gate's database, and any task's thread rendered live, A2UI surfaces and AP2
parts included. You can also prompt your agents from the console. It is the gate itself, with `profile: local`
([`spec/relay.md`](../spec/relay.md) §18): no sign-in, no enrollment token, no TLS.

## Five commands

With Docker and Go, from a checkout of this repository:

```sh
docker compose -f compose.dev.yaml up -d --wait                  # 1. Postgres and Temporal, on loopback
go run ./cmd/kontra-gate migrate -f examples/local/kontra.yaml   # 2. the gate's schema
go run ./cmd/kontra-gate serve -f examples/local/kontra.yaml &   # 3. the gate, on 127.0.0.1:8080
go run ./cmd/kontra connect --gate http://127.0.0.1:8080 --listen off \
  --serve a2a:booking=http://127.0.0.1:9995 &                     # 4. your agent, through a node
open http://127.0.0.1:8080/                                       # 5. the console (xdg-open on Linux)
```

`examples/local/kontra.yaml` is two lines: `profile: local` and the Postgres URL; Temporal's default address,
`localhost:7233`, is `compose.dev.yaml`'s. At start the gate warns that it serves every client without credentials
and prints the console's URL.

Step 4 hosts whatever A2A server answers at that URL, with no enrollment: the service `booking` is created on first
connect. `--listen off`: this node only hosts (without it, it also lets local clients call the gate's agents through
`127.0.0.1:7777`). To try it with an agent from this repository, start one first, e.g. the A2UI table-booking example:

```sh
go run ./examples/sellers/a2ui --port 9995 &       # an A2A 1.0 agent with an A2UI form
go run ./examples/sellers/ap2-merchant --port 9993 & # an AP2 v0.2.0 merchant (add --serve a2a:merchant=http://127.0.0.1:9993)
```

Any A2A client can then call `http://127.0.0.1:8080/agents/booking` with no credential (send `A2A-Version: 1.0`), and
the console shows its task as it happens. Every A2A method works, `ListTasks` and the push notification configs
included: every client is one caller, so the refusals a team gate keeps for anonymous callers do not apply, and a push
webhook may listen on `http://127.0.0.1:<port>` (loopback only). To stop: `kill %1 %2 %3` (or Ctrl-C each), then
`docker compose -f compose.dev.yaml down -v` to drop the data.

## What the console shows

The console is an A2A inspector with history: everything it shows is what the gate recorded, labelled as A2A names it.

- **Contexts** (the landing page): in A2A every message belongs to a context, and the agent may answer it with a task.
  A context's page says how it was opened (the `contextId` your client sent, or the one the gate assigned in its
  answer) and which context your agent knows it by, lists the tasks the agent created in it, and draws one timeline of
  every interaction: each caller message with its `contextId`, its `taskId` (or none) and its `referenceTaskIds`.
- **Tasks**, and a task's thread drawn from its event log as it happens: messages, results, A2UI surfaces, AP2
  checkouts, mandates and receipts as cards (decoded for display, never verified), sealed parts as envelopes. Its **A2A
  timeline** labels every item `<Actor> · <A2A type>`: `Caller · SendMessage` (or `SendStreamingMessage`),
  `Callee · Task`, `Callee · TaskStatusUpdateEvent`, `Callee · TaskArtifactUpdateEvent`, `Callee · Message`, and
  `Gate · TaskStatusUpdateEvent` for what the gate wrote itself (a failure, a reconnect wait). Where the gate delivered
  another object than the agent sent (an agent's `Message` reaches the caller as a completed task carrying it; a polled
  `Task` as status and artifact updates), the item says so. Your client's other calls on the task (`GetTask`,
  `SubscribeToTask`, push configurations) and the gate's calls to your agent that carried nothing new are in the
  timeline too. Each item is checked against the pinned A2A specification (1.0, or 0.3 for a 0.3 agent), and the
  task's `taskId` and `contextId`, and your agent's own ids, are shown in full with Copy.
- **Traffic**: one row per A2A call on both legs (your client to the gate, the gate to your agent), refused calls
  included (a missing `A2A-Version`, an unknown service, a body over the limit, a required extension missing), with its
  binding, method, `contextId` and outcome; filter by leg, method or `contextId`. A row opens the exchange: in this
  profile its request and response bodies, or its stream's frames with their SSE ids and times, are kept, credentials
  redacted and never a header ([`spec/relay.md`](../spec/relay.md) §13.6), each validated. A gap is never silent:
  dropped rows are counted on `/v1/info` and in `kontra_traffic_dropped_total`.
- **MCP calls** of the MCP servers your node hosts (`--serve mcp:name=…`).
- **Cards**: one card per service, as its agent card says (name, version, description, skills), with its URL,
  whether an agent is reachable now and how long its ended tasks are kept. The arrow under a card opens the rest of
  a long description and each skill's details. **Compare cards** shows your agent's card as fetched beside the card the
  gate serves for it, the members the gate set or rewrote marked, each validated.
- **Playground**: any of A2A 1.0's eleven methods, on JSON-RPC or HTTP+JSON, from a JSON body prefilled with what the
  method requires, sent to the service's own A2A route as any client sends it; the raw answer or the stream's frames,
  then the exchange the gate recorded with its validation.
- **Run a task**: start a task on a service from its card (its skills, examples and AgentStack initial form), answer
  it when it asks for input (text, a form, an A2UI action), cancel it, or continue its conversation. The console acts
  only on the tasks it started; a task another client started is watch-only.

## Keeping history

Ended tasks are kept 7 days and so are the traffic rows. To keep them, set `0`, which means forever:

```yaml
profile: local
postgres: postgres://kontra:kontra@127.0.0.1:5432/kontra?sslmode=disable
traffic: { retention: 0 }
services:
  booking:                      # a configured service keeps its settings when a node serves it
    kind: a2a
    backend: { tunnel: {} }
    task: { retention: 0 }
```

Going back to a duration does not expire the tasks that ended while it was `0`; `spec/relay.md` §5.4 has the one line
of SQL that does.

## The traffic record on any gate

The Traffic view's rows are the gate's A2A traffic record, `a2a_exchanges` ([`spec/relay.md`](../spec/relay.md) §13.6):
one row per A2A call, on both legs: the calls the gate receives and the calls it makes to your agents. It is on in the
local profile and off on every other gate unless its operator turns it on. Bodies are a separate choice: kept by
default in the local profile, never elsewhere unless the operator opts in, each cut at `body_max`, a push
configuration's token and credentials replaced by `"[redacted]"`, never a header, never a sealed service's:

```yaml
traffic:
  record: true      # default false; true in the local development profile
  retention: 3d     # default 7d; 0 keeps exchanges forever, with their bodies
  bodies: false     # default false; true in the local development profile
  body_max: 256KiB  # each body or stream frame is cut here, marked truncated
```

Recording never slows or fails a call: rows are written in batches, and when the database falls behind they are
dropped and counted in `kontra_traffic_dropped_total{replica}` on the metrics listener (and on the console's
`/v1/info`); past a 64 MiB budget of queued bodies, rows are kept without their bodies and counted in
`kontra_traffic_bodies_dropped_total{replica}`. Turning recording off keeps the rows already written until their retention. Exchanges are deleted by the
retention sweep every 10 minutes, whatever became of their tasks.

## What the profile does not protect against

The gate listens on loopback only (it refuses to start otherwise), answers `421` to a request whose `Host` is not
`127.0.0.1`, `localhost` or `[::1]` (a web page that rebinds its own name to your machine cannot reach it), refuses a
browser's cross-site request to your agents (`403`: a page you have open cannot prompt them), and its console API
refuses another site's page and sends no CORS header. But **anything that runs on your machine, and
any other user of it, can reach the gate: read every conversation, call your agents and act as the shared caller.**
Every client is the same caller, so there is no isolation between clients either. Do not use the profile on a shared
machine, never put a proxy in front of it, and never use it for a team: that is a team gate
([README](../README.md#a-team-gate-on-your-own-vps)), with API keys and TLS.
