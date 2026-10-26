# sellers/helloworld

The a2a-samples helloworld seller, **unmodified**, hosted at a gate through `kontra connect` (the tunnel, M2).
It is PRD §18's test for every seller-facing change ("can the helloworld sample still run unmodified?") and the
PRD §14 M2 deliverable "helloworld example through the tunnel".

| File | What |
|---|---|
| `run.sh` | Starts `samples/python/agents/helloworld` of a2a-samples at `6603ba3` as it is, with a pinned toolchain |
| `kontra.yaml` | A team gate on one machine with the tunnel service `hello` |
| `run_test.go` | Tests of `run.sh` (`go test ./examples/sellers/helloworld`; the ones that start the real seller need `KONTRA_A2A_SAMPLES`, a checkout at `6603ba3`); the seller itself has no Go code |

## What the seller is

[a2aproject/a2a-samples](https://github.com/a2aproject/a2a-samples) `samples/python/agents/helloworld` at
commit `6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3` (4 August 2026): a2a-sdk 1.1.0 with a protocol 1.0 card named
`Hello World Agent`, one JSON-RPC interface at `http://127.0.0.1:9999`, `streaming: true`. Its `__main__.py`
binds `127.0.0.1:9999` and nothing else. Every message gets a task: `submitted`, `working` with the message
"Processing request...", the artifact `Hello, World! I have received your request (<your text>)`, then
`completed` with "Request is completed!". It does not support cancel (its executor raises), and its tasks end
at once anyway.

The 0.2.5 helloworld (a2a-samples `08dde55c070f`) that `card_compat` names is not built here: `card_compat`
runs against `slowagent`'s `Legacy025` stand-in for it (D122, `test/e2e/card_compat_test.go`).

## Start the seller: `run.sh`

```sh
examples/sellers/helloworld/run.sh
#  INFO:     Uvicorn running on http://127.0.0.1:9999 (Press CTRL+C to quit)
```

It needs [uv](https://docs.astral.sh/uv/getting-started/installation/) and git. On first run it fetches the
pinned commit of a2a-samples into `${XDG_CACHE_HOME:-~/.cache}/kontra/a2a-samples`; set `A2A_SAMPLES` to use a
checkout of your own (it must be at `6603ba3`). It then runs, in the sample's directory:

```sh
uv run --no-project --python 3.12 --exclude-newer 2026-08-05T00:00:00Z \
  --with-requirements requirements.txt python __main__.py
```

That is the sample's own requirements file and entry point, with the toolchain pinned from outside: CPython
3.12, and the requirements (a2a-sdk 1.1.0 and the unpinned uvicorn, sse-starlette and pytest) resolved as of the
day after the commit. Nothing in the checkout is written: uv builds the environment in its cache, and
`PYTHONDONTWRITEBYTECODE=1` keeps `__pycache__` out. `--no-project` matters: without it uv finds the
repository's `samples/python/pyproject.toml` above the sample, syncs that project into `samples/python/.venv`
and rewrites the tracked `samples/python/uv.lock`. `git -C ~/.cache/kontra/a2a-samples status --ignored` is
clean before and after.

`run.sh` refuses to start, with a message saying why, when uv is missing (`UV` names another binary),
`127.0.0.1:9999` is already taken, the checkout is at another commit, or anything in the sample's directory
differs from the commit (an edit, an extra file, even an ignored one). `Ctrl-C` or `SIGTERM` stops it.

## Through the tunnel (M2)

The gate never connects to the seller. `kontra connect`, running next to it, dials the gate once (outbound,
WebSocket), proves its identity key, and from then on the gate sends each request for `hello` over that
connection; the node forwards it to `http://127.0.0.1:9999` and nowhere else (spec/tunnel.md §8.2).

`kontra.yaml` here is a team gate on one machine: the gate on `127.0.0.1:8080` with public URL
`http://localhost:8080`, Postgres on `localhost:5432` and Temporal on `localhost:7233`, which is what the
repository's contributor compose starts. The service is a tunnel service, with no URL:

```yaml
services:
  hello:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
```

From the repository root, with a2a-cli v0.3.1 as `a2a` (its install is in
[`../hello-exec/README.md`](../hello-exec/README.md#install-a2a-cli-v031)):

```sh
docker compose -f compose.dev.yaml up -d --wait           # Postgres and Temporal
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra

./kontra-gate migrate -f examples/sellers/helloworld/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/sellers/helloworld/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/sellers/helloworld/kontra.yaml &     # loads services: into the database

examples/sellers/helloworld/run.sh &                                 # the seller, on 127.0.0.1:9999

# once the gate answers (curl localhost:8080/healthz): a one-time token for hello, then the node
./kontra connect --gate http://localhost:8080 --serve a2a:hello=http://127.0.0.1:9999 \
  --enroll "$(./kontra-gate enroll -f examples/sellers/helloworld/kontra.yaml hello)" &
#  kontra connect: connected to http://localhost:8080 as k1_… (tenant default, session …)
#    a2a hello  http://localhost:8080/agents/hello

a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" "hi"
#  Task:     01a1…
#  Context:  01a1…
#  Status:   completed (…)
#    Request is completed!
#  Artifacts:
#    […] Hello, World! I have received your request (hi)
#  History:
#    [user] hi
#    [agent] Processing request...

a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" --stream "hi"
#  [status] submitted
#  [status] working: Processing request...
#  [artifact] Hello, World! I have received your request (hi)
#  [status] completed: Request is completed!
```

What happens:

- `kontra-gate enroll … hello` prints a service token (`ket_…`): single use, valid 15 minutes, for `hello` of
  the gate's tenant only. `enroll <service>` needs the service in the database, so the gate must have started
  once with this `kontra.yaml`. The token's work is done in the node's first handshake: the gate binds the
  node's key (`k1_…`, from `~/.kontra/identity.key`, created on first run) to `hello`, and from then on the key
  alone gets the node in (spec/tunnel.md §5).
- When the session opens, the gate fetches the seller's card through it before it sends the session any task,
  then serves that card at `http://localhost:8080/agents/hello/.well-known/agent-card.json` (and at
  `GET /agents/hello`, D10) with only the gate's own interfaces, JSON-RPC and HTTP+JSON, `pushNotifications`
  and bearer security added. Like every card of a service that is not `listed`, it needs the API key:
  `curl -H "Authorization: Bearer $KONTRA_API_KEY" http://localhost:8080/agents/hello/.well-known/agent-card.json`.
- The seller speaks only JSON-RPC; a caller may use either binding at the gate, which relays it as JSON-RPC.
- The seller keeps nothing for Kontra: no Kontra import, no extra endpoint, no changed file. `kontra connect`
  rewrites `Host` to `127.0.0.1:9999` and strips `Origin` and `Forwarded`/`X-Forwarded-*`, so the sample sees a
  local client.

### Restarts, order and failures

- **Restart the node with the same command**, `--enroll` included: `~/.kontra/credentials` records that the
  token was used, so it is not sent again (`an --enroll token this node already enrolled with at this gate is
  not sent again`), and the key reconnects on its own. Keep `~/.kontra/identity.key` and
  `~/.kontra/credentials` together, on the same persistent volume in a container: a node that keeps its key but
  loses its credentials resends a used token, which the gate refuses for good (`enroll_invalid`).
- **Start the seller first.** The card is fetched when the session opens; if the seller is not answering yet,
  the node answers the fetch `502` (`Kontra-Node: target_unreachable`) and the gate fetches again later with
  backoff. A task sent before the gate has the card is retried and fails `service_unavailable` once its retries
  (`task.retries`) are used.
- **The node is up, the seller is down:** the node answers every request `502 target_unreachable`, and a task
  fails with `kontra.failure: tunnel_lost` once its retries are used ("the connection to the service was lost").
- **The node stops or loses its connection** (`Ctrl-C` exits 0; a kill, a network cut): the gate drops the
  session when the connection closes, or after about 75 s of failed pings when it goes silent (spec/tunnel.md
  §11). A task the seller already has (its upstream task id exists) fails `tunnel_lost`; one that had not
  reached it yet is retried, on another node's session of `hello` when there is one. A node that lost its
  connection redials with backoff from 1 s to 60 s (spec/tunnel.md §13).
- `--serve` names the agent's root URL. A target with a path (`http://127.0.0.1:9999/base`) is refused in M2:
  the base would be doubled on the interface path (conflict C1 of the M2 plan, `internal/node/serve.go`).

`docker compose -f compose.dev.yaml down -v` removes Postgres, Temporal and their data.
