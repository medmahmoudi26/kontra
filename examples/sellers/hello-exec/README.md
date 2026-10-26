# sellers/hello-exec

The README demo seller: a2a-cli v0.3.1's exec mode, `a2a server --exec 'echo Hello World'`, served behind the
gate as a **direct backend** (M1) and, from M2, through `kontra connect` (the tunnel). The seller is unmodified
and imports nothing from Kontra.

Status: works end to end with the M1 gate (`kontra-gate migrate`, `enroll --api-key` and `serve`): the polling
fallback, streaming with `SELLER_CHUNK`, and a cancel forwarded to the seller.

## What the seller is

`a2a server --exec CMD` runs `CMD` once per message and answers with its standard output (exit 0: `completed`;
anything else: `failed` with standard error). Its card is a 1.0 card, HTTP+JSON only, with
`capabilities.streaming: true`, yet without `--chunk` it refuses streaming with `UnsupportedOperationError`.
The gate handles that by falling back, in the same attempt, to `SendMessage` with `returnImmediately: true` and
polling `GetTask` (D15; the gate logs `the upstream refused streaming; polling (D15)`). With `--chunk=$'\n'` each
output line streams as an artifact chunk instead.

Exec mode cannot ask for input (`input-required`); `input_required_sticky` uses `sellers/booking` (M2).

## Install a2a-cli v0.3.1

```sh
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli && brew install a2a
# or: go install github.com/a2aproject/a2a-cli@v0.3.1 && mv "$(go env GOPATH)/bin/a2a-cli" "$(go env GOPATH)/bin/a2a"
a2a version
```

`run.sh` takes the binary from `A2A` when it is not `a2a` on your `PATH` (`A2A=/opt/a2a-cli/a2a ./run.sh`). Stop
one seller before starting another on the same port.

The syntax is a2a-cli's own (D15): the subcommand is `server` (not `serve`), `send` takes the agent with
`-a <card URL>` or `-e <endpoint>`, and credentials go in `--auth` (there is no `-H`).

## Run it behind a gate (M1, direct backend)

`kontra.yaml` in this directory is a team gate on one machine: the gate on `127.0.0.1:8080`, Postgres on
`localhost:5432` and Temporal on `localhost:7233`, which is what the repository's contributor compose starts.
The commands below run from the repository root.

```sh
examples/sellers/hello-exec/run.sh &       # a2a server --exec 'echo Hello World' --host 127.0.0.1 --port 9999

docker compose -f compose.dev.yaml up -d --wait     # Postgres and Temporal for a gate run from source
go build -o kontra-gate ./cmd/kontra-gate

./kontra-gate migrate -f examples/sellers/hello-exec/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/sellers/hello-exec/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/sellers/hello-exec/kontra.yaml &

a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" "hi"
#  -> completed, with the artifact "Hello World"
```

`-a http://localhost:8080/agents/hello` works because the gate also serves the card at `GET {svc}` (D10).
The gate's card is the seller's with the gate's interfaces (JSON-RPC and HTTP+JSON), `streaming` and
`pushNotifications` set to `true`, and bearer security; M1 does not sign it yet (M3).

`kontra.yaml` points the service at `http://127.0.0.1:9999` with `allow_private: true`: the gate's dialer
refuses loopback and private addresses otherwise, and permits plain `http://` only with it (spec/relay.md §10.3,
D67). The URL is a base: the gate fetches the card from `/.well-known/agent-card.json` under it and dials only
`127.0.0.1:9999`, whatever the card's interface URL says (D71).

The gate's own endpoints: `GET http://localhost:8080/healthz` (200 while Postgres and Temporal answer and the
gate is not draining) and the metrics listener, `:9090/metrics` by default (`metrics.listen`).

## Streaming

```sh
SELLER_CHUNK=$'\n' SELLER_CMD='for i in 1 2 3; do echo line $i; sleep 1; done' examples/sellers/hello-exec/run.sh &
a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" --stream "go"
#  -> submitted, working, then "line 1", "line 2", "line 3" a second apart as artifact chunks, then completed
```

## A cancel (cancel_forwarded)

```sh
SELLER_CMD='sleep 20; echo done' examples/sellers/hello-exec/run.sh &
a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" --async -o json "go"   # the task, with its id
a2a task cancel -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" --timeout 60s <task-id>
#  -> canceled: the gate forwarded CancelTask upstream, and no completed event follows
```

The gate's polling path sends `returnImmediately: true` upstream, so the upstream task id exists at once and a
cancel has something to cancel (spec/relay.md §6.2, §8). Exec mode answers `CancelTask` only once its command has
ended, and the gate waits at most 30 s for that answer: `forward_cancel`, the call that sends `CancelTask`
upstream, has a 30 s start-to-close (spec/relay.md §14.2; the 60 s of §8 is the whole cancel's budget). With
`sleep 20` the seller, and so the gate, answers when the 20 s are up, well inside it; `--timeout 60s` leaves
a2a-cli (whose default is 30 s) room for the rest of the cancel. A command that still has more than 30 s to run
when the cancel arrives (a longer `SELLER_CMD`) answers too late: the forward is cut, the cancel answers
`TaskNotCancelableError` ("the service could not be reached to cancel the task") and the task continues to its
end. The task shows `canceled` in the gate's log as soon as the seller reports it. a2a-cli v0.3.1's exec mode
can also end a canceled task `failed` (its cancel races the command's exit), which is why the automated
`cancel_forwarded` test uses an a2a-go seller that handles cancel (D80). During M1 (then with `sleep 30`), 20
cancels through the gate and 50 straight to the seller all ended `canceled`.

## Reload and drain

`kill -HUP <gate pid>` applies a changed `services:` block (an `allow:` list, a timeout, a removed service,
which then answers 404) without a restart; other keys take effect at the next start. `kill -TERM <gate pid>`
drains: `/healthz` answers 503, no new dispatch starts on this replica, a task already running is relayed to its
end (or until `drain_timeout`, 10 minutes by default), and then the gate exits (spec/relay.md §14.3). A second
`SIGTERM` stops it at once.

## Through the tunnel (M2)

```sh
./run.sh
kontra connect --gate http://localhost:8080 --serve a2a:hello=http://localhost:9999 --enroll "$(kontra-gate enroll hello)"
```
