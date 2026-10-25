# sellers/hello-exec

The README demo seller: a2a-cli v0.3.1's exec mode, `a2a server --exec 'echo Hello World'`, served behind the
gate as a **direct backend** (M1) and, from M2, through `kontra connect` (the tunnel). The seller is unmodified
and imports nothing from Kontra.

Status: the script and the configuration are final for M1; the gate commands below work once the M1 work
packages that build `kontra-gate migrate`, `enroll --api-key` and `serve` have landed (see the M1 plan).

## What the seller is

`a2a server --exec CMD` runs `CMD` once per message and answers with its standard output (exit 0: `completed`;
anything else: `failed` with standard error). Its card is a 1.0 card, HTTP+JSON only, with
`capabilities.streaming: true`, yet without `--chunk` it refuses streaming with `UnsupportedOperationError`.
The gate handles that by falling back, in the same attempt, to `SendMessage` with `returnImmediately: true` and
polling `GetTask` (D15). With `--chunk=$'\n'` each output line streams as an artifact chunk instead.

Exec mode cannot ask for input (`input-required`); `input_required_sticky` uses `sellers/booking` (M2).

## Install a2a-cli v0.3.1

```sh
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli && brew install a2a
# or: go install github.com/a2aproject/a2a-cli@v0.3.1 && mv "$(go env GOPATH)/bin/a2a-cli" "$(go env GOPATH)/bin/a2a"
a2a version
```

The syntax is a2a-cli's own (D15): the subcommand is `server` (not `serve`), `send` takes the agent with
`-a <card URL>` or `-e <endpoint>`, and credentials go in `--auth` (there is no `-H`).

## Run it behind a gate (M1, direct backend)

```sh
./run.sh                                   # a2a server --exec 'echo Hello World' --host 127.0.0.1 --port 9999

# a gate with the service below (kontra.yaml in this directory), Postgres and Temporal running:
kontra-gate migrate -f kontra.yaml
export KONTRA_API_KEY="$(kontra-gate enroll -f kontra.yaml --api-key demo)"
kontra-gate serve -f kontra.yaml

a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" "hi"
#  -> Hello World
```

`-a http://localhost:8080/agents/hello` works because the gate also serves the card at `GET {svc}` (D10).
The gate's card is the seller's with the gate's interfaces (JSON-RPC and HTTP+JSON), `streaming` and
`pushNotifications` set to `true`, and bearer security; M1 does not sign it yet (M3).

`kontra.yaml` points the service at `http://127.0.0.1:9999` with `allow_private: true`: the gate's dialer
refuses loopback and private addresses otherwise (spec/relay.md §10.3).

## A cancel (cancel_forwarded)

```sh
SELLER_CMD='sleep 30; echo done' SELLER_PORT=9998 ./run.sh &
# point services.hello at :9998, then:
a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" --async "go"   # prints the task id
a2a task cancel -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" <task-id>
#  -> TASK_STATE_CANCELED: the gate forwarded CancelTask upstream, and no completed event follows
```

The gate's polling path sends `returnImmediately: true` upstream, so the upstream task id exists at once and a
cancel has something to cancel (spec/relay.md §6.2, §8).

## Streaming

```sh
SELLER_CHUNK=$'\n' SELLER_CMD='for i in 1 2 3; do echo line $i; sleep 1; done' ./run.sh
a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" --stream "go"
```

## Through the tunnel (M2)

```sh
./run.sh
kontra connect --gate http://localhost:8080 --serve a2a:hello=http://localhost:9999 --enroll "$(kontra-gate enroll hello)"
```
