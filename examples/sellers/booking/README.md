# sellers/booking

A small A2A agent that books something for a date and has to ask for the date first: the examples'
`input-required` seller (spec/relay.md §7, `input_required_sticky`). It is written with a2a-go v2.7.0 and imports
nothing from Kontra; it lives in this module only so that `go build ./...` builds it and the e2e tests can run
it.

| File | What |
|---|---|
| `main.go` | The agent: card, JSON-RPC and HTTP+JSON, the executor |
| `main_test.go` | Its behaviour over both bindings (`go test ./examples/sellers/booking`) |
| `kontra.yaml` | A team gate on one machine with the tunnel service `booking` |

## What it does

- A first message stops the task at `input-required` and asks for the date of whatever the message says:
  "For which date shall I book "a table for two"? Reply with the date as YYYY-MM-DD."
- A reply (a message with the task's id) carrying a `YYYY-MM-DD` calendar date books it: `working`, an artifact
  named `booking` ("Booked "a table for two" on Monday, 2026-11-02."), then `completed`. The date may be the
  date of an ISO 8601 date-time (`2026-11-02T19:00:00Z` books 2026-11-02; the time is not used). A reply
  without a date it can read (or with an impossible one, `2026-02-30`) asks again and the task keeps waiting.
- `CancelTask` ends a task that is not over yet `canceled`, while it waits for its date as well as while it is
  being booked; a completed task is not cancelable (`TaskNotCancelableError`).
- Tasks live in memory: a restarted agent knows none of its old tasks.
- A request body over 4 MiB, the gate's default `limits.body`, is refused (a parse error: JSON-RPC `-32700`,
  HTTP+JSON `400`), and the agent quotes at most the first 80 characters of a message back.

One thing in `main.go` is there for a2a-go rather than for the booking: a2a-go v2.7.0 can fail a `CancelTask`
that arrives while the turn that just stopped at `input-required` is still being cleaned up
(`TaskNotCancelableError` or "queue is closed", a race its own code marks as a TODO), so `cancelRetry` makes
such a cancel again while the task is still waiting. A cancel that loses the same race to a reply that
completes the booking would fail "queue is closed", an internal error; `cancelRetry` answers it
`TaskNotCancelableError`, as any cancel of a completed task is answered.

```
booking [--host 127.0.0.1] [--port 9998] [--binding both|jsonrpc|http+json]
```

It serves its card at `/.well-known/agent-card.json`, JSON-RPC at its root (`http://127.0.0.1:9998`) and
HTTP+JSON under `/rest`, all under one root URL, which is what `kontra connect --serve` needs (an M2 node
hosts an agent at its root URL only). `--binding` narrows both the routes and the card; with both, a gate uses
JSON-RPC. `--port 0` takes any free port. The first line on standard output is the root URL; `SIGINT` or
`SIGTERM` stops it. `--host 0.0.0.0` listens on every interface (in a container); the card then still names
`127.0.0.1`, which neither a gate nor a node uses: each dials the address it was configured with (D71).

## Talk to it directly

With a2a-cli v0.3.1 as `a2a` (install: [`../hello-exec/README.md`](../hello-exec/README.md#install-a2a-cli-v031)):

```sh
go run ./examples/sellers/booking &        # http://127.0.0.1:9998

a2a send -a http://127.0.0.1:9998 "a table for two"
#  Status:   input-required (…)
#    For which date shall I book "a table for two"? Reply with the date as YYYY-MM-DD.
#  Resume:     a2a send --task-id <task id> "<reply>"
a2a send -a http://127.0.0.1:9998 --task-id <task id> "2026-11-02"
#  Status:   completed (…)
#    Confirmed: Booked "a table for two" on Monday, 2026-11-02.
#  Artifacts:
#    [booking] Booked "a table for two" on Monday, 2026-11-02.

a2a send -a http://127.0.0.1:9998 --transport rest "a haircut"     # the same over HTTP+JSON
a2a task cancel -a http://127.0.0.1:9998 <its task id>
#  Status:   canceled (…)
#    The booking was canceled.
```

## Through the tunnel (M2)

`kontra.yaml` here is a team gate on one machine (the gate on `127.0.0.1:8080`, Postgres on `localhost:5432`
and Temporal on `localhost:7233`, which the repository's `compose.dev.yaml` starts) with one tunnel service,
`booking`. From the repository root:

```sh
docker compose -f compose.dev.yaml up -d --wait
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra && go build -o booking ./examples/sellers/booking

./kontra-gate migrate -f examples/sellers/booking/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/sellers/booking/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/sellers/booking/kontra.yaml &

./booking &                                                      # http://127.0.0.1:9998
./kontra connect --gate http://localhost:8080 --serve a2a:booking=http://127.0.0.1:9998 \
  --enroll "$(./kontra-gate enroll -f examples/sellers/booking/kontra.yaml booking)" &
#  kontra connect: connected to http://localhost:8080 as k1_… (tenant default, session …)
#    a2a booking  http://localhost:8080/agents/booking

a2a send -a http://localhost:8080/agents/booking --auth "Bearer $KONTRA_API_KEY" "a table for two"
#  Status:   input-required (…)
#    For which date shall I book "a table for two"? Reply with the date as YYYY-MM-DD.
a2a send -a http://localhost:8080/agents/booking --auth "Bearer $KONTRA_API_KEY" --task-id <task id> "2026-11-02"
#  Status:   completed (…)
#    Confirmed: Booked "a table for two" on Monday, 2026-11-02.
```

The task id is the gate's own, not the agent's (spec/relay.md §4). The reply goes to the same upstream task, on
the same tunnel session, in the same attempt: the gate recorded that session when it placed the first turn,
and a reply never goes to another node, even when several nodes serve `booking` (spec/relay.md §7). `--stream`
works for both turns: `submitted` then `input-required` for the first, `working`, the artifact and `completed`
for the reply.

### Cancels, and when a cancel cannot be delivered

A task waiting for its date has no dispatch running at the gate, so a cancel is all that touches it:

| The situation | `a2a task cancel -a http://localhost:8080/agents/booking --auth … <task id>` |
|---|---|
| node and agent up | The gate forwards `CancelTask` through the node; the task ends `canceled` |
| `kontra connect` up, the agent stopped | `Error: … the service could not be reached to cancel the task` (`TaskNotCancelableError`). The node answered `502 target_unreachable`: the cancel was **not delivered**, and the task **stays `input-required`**, unchanged (spec/relay.md §8) |
| `kontra connect` gone (killed, or its connection lost) | The task ends `failed` with `kontra.failure: tunnel_lost`: the session that holds the upstream task is gone, so nothing could reach it again (M2's resolution of spec/relay.md §8's "session gone" row) |

Only a lost session fails a task `tunnel_lost`; an agent that is down behind a live node does not. The same
holds for a reply: with the session gone (the node killed, even if it has reconnected since with a new session),
the reply ends the task `failed: tunnel_lost` without reaching any agent. In this example the agent itself
forgets its tasks when it restarts, so a reply to a task it held before its restart ends `failed:
upstream_error` ("task not found"): that is the agent's own answer, relayed.

Restart `kontra connect` with the same command, `--enroll` included: the token was recorded as used in
`~/.kontra/credentials` and is not sent again. Keep that file next to `~/.kontra/identity.key` (the same
persistent volume in a container).

`docker compose -f compose.dev.yaml down -v` removes Postgres, Temporal and their data.
