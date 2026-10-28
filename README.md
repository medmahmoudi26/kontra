# kontra

A durable tunnel for A2A agents and MCP servers.

A node runs one CLI, `kontra connect`. It serves what is on its localhost to a gate it chose and
calls what others serve through the same gate. The gate adds durable tasks (Temporal + Postgres),
identity, policy and end-to-end sealing. Nothing on the node listens on a public port.

- Connect to `gateway.kontra.sh` and you are on the marketplace.
- Run the same gate image yourself and you have a private one for your team, with no catalog and no
  money code.

This repository holds the open-source parts: the `kontra` CLI, the `kontra-gate` server image,
the Helm chart and compose file, the extension specs (`escrow`, `sealed`, `input-schema`) and the
examples. The marketplace, listings, escrow and settlement live in a separate private repository
that deploys and configures this one; nothing here imports from it.

## Status

Sealing and MCP stage (M3). `kontra-gate` relays A2A tasks durably, to a direct backend since M1 and, since M2, to
an agent on a node's localhost: `kontra connect --gate <url> --serve a2a:<name>=<url>` dials the gate once,
outbound, and the gate sends each request for `<name>` over that connection; the node forwards it to `<url>` and
nowhere else. Stock agents run unmodified behind it: the a2a-samples helloworld seller is hosted through the tunnel
in [`examples/sellers/helloworld`](examples/sellers/helloworld/README.md), and the other built sellers are listed in
[`examples/`](examples/README.md). M3 adds end-to-end sealing, MCP servers hosted the same way, over HTTP or stdio,
and the team gate in `compose.yaml` (below). The gate's own `/mcp` endpoint with `hire`, anonymous sessions,
`kontra login` and the public gate come with M4 (PRD §14; D164 dropped the local console, D170 says what M3 leaves,
in [`docs/decisions.md`](docs/decisions.md)).

`docs/` is the design; `spec/` is the protocol specification written from it; code lands against both. Read in
this order:

1. [`docs/prd-v4.md`](docs/prd-v4.md) — the v4 PRD (21 Oct 2026). The relay, data model,
   security model, milestones and acceptance tests.
2. [`docs/architecture/01-tunnel-and-protocols.html`](docs/architecture/01-tunnel-and-protocols.html)
   — one node type, gate as membership, the Temporal primitive, tunneling MCP servers next to A2A
   agents, bindings. Supersedes PRD §3–4, §6 (target), §8 (keys), §11 (two sidecars).
3. [`docs/architecture/02-deployment-topology.html`](docs/architecture/02-deployment-topology.html)
   — one CLI on the user's machine, gates as clusters, `kontra dev` removed. Supersedes PRD §10
   (embedded Temporal), §11 (`kontra dev`), §14 M4, §15 demo.
4. [`spec/`](spec/) — [`tunnel.md`](spec/tunnel.md), [`relay.md`](spec/relay.md) and the three
   extensions ([`ext-escrow.md`](spec/ext-escrow.md), [`ext-sealed.md`](spec/ext-sealed.md),
   [`ext-input-schema.md`](spec/ext-input-schema.md)), with [`migrations/0001_init.sql`](migrations/0001_init.sql)
   and [`0002_m3.sql`](migrations/0002_m3.sql).
   Every place they depart from the documents above is numbered in [`docs/decisions.md`](docs/decisions.md).

Where a note and the PRD disagree, the later document wins; `docs/README.md` has the section map.

## Through the tunnel today (M2)

A team gate run from source, and the helloworld seller hosted through `kontra connect`. From the repository root,
with Docker, Go, [uv](https://docs.astral.sh/uv/) and a2a-cli v0.3.1 as `a2a` (its install is in
[`examples/sellers/hello-exec`](examples/sellers/hello-exec/README.md#install-a2a-cli-v031)):

```sh
docker compose -f compose.dev.yaml up -d --wait           # Postgres and Temporal
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra

./kontra-gate migrate -f examples/sellers/helloworld/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/sellers/helloworld/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/sellers/helloworld/kontra.yaml &     # loads services: into the database

examples/sellers/helloworld/run.sh &                                 # the seller, unmodified, on 127.0.0.1:9999

# once the gate answers (curl localhost:8080/healthz): a one-time token for hello, then the node
./kontra connect --gate http://localhost:8080 --serve a2a:hello=http://127.0.0.1:9999 \
  --enroll "$(./kontra-gate enroll -f examples/sellers/helloworld/kontra.yaml hello)" &
#  kontra connect: connected to http://localhost:8080 as k1_… (tenant default, session …)
#    a2a hello  http://localhost:8080/agents/hello

a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" "hi"
#  → completed, with the artifact "Hello, World! I have received your request (hi)"
```

The gate never dials the seller, and the seller imports nothing from Kontra. `--serve a2a:` names the agent's root
URL: a target with a path is refused (D123 in [`docs/decisions.md`](docs/decisions.md)). What a lost node or a
seller that is down does to a task, and how to restart the node, is in the
[helloworld README](examples/sellers/helloworld/README.md#restarts-order-and-failures).

## Sealed, end to end (M3)

A service with `sealed: true` is sealed between two nodes. The hiring node, `kontra connect` serving its local
clients on `127.0.0.1:7777`, seals every part a client sends to the keys enrolled for the service; the hosting node
opens them before the service sees anything, and seals what the service answers to a key only the hiring node
holds. The gate stores and relays ciphertext it cannot open, and neither the client nor the service changes or
holds a key ([`spec/ext-sealed.md`](spec/ext-sealed.md)). Two `kontra connect` on one machine, each with its own
`HOME` and so its own identity key, from [`examples/sealing`](examples/sealing/README.md):

```sh
docker compose -f compose.dev.yaml up -d --wait
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra
CFG=examples/sellers/hello-exec/kontra-sealed.yaml
./kontra-gate migrate -f $CFG
export KONTRA_API_KEY="$(./kontra-gate enroll -f $CFG --api-key demo)"
./kontra-gate serve -f $CFG &

examples/sellers/hello-exec/run.sh &                        # the seller on 127.0.0.1:9999

# The hosting node: enrolled for hello, so its key is one of the service's recipients.
HOME=/tmp/seller ./kontra connect --gate http://localhost:8080 --listen 127.0.0.1:7781 \
  --enroll "$(./kontra-gate enroll -f $CFG hello)" --serve a2a:hello=http://127.0.0.1:9999 &

# The hiring node: it hires with the API key, and seals for its local clients on 127.0.0.1:7777.
HOME=/tmp/buyer ./kontra connect --gate http://localhost:8080 --api-key "$KONTRA_API_KEY" &

a2a send -a http://localhost:7777/agents/hello "a secret"
#  → completed, "Hello World": the client never sees an envelope, and Postgres holds only ciphertext
```

The hiring node pins each sealed service it hires in `~/.kontra/pins`: a downgrade to unsealed is always refused,
and a new recipient or seller key waits for `--accept-change <name>`, the key ids printed on stderr for comparison
out of band. MCP calls are sealed the same way between two `kontra connect`. Without a node, `kontra seal --service
<service URL> --api-key <key> <file>` prints a sealed A2A message and `kontra seal --open <answer.json>` opens the
reply: the escape hatch for scripts, not the path. A part over 256 KiB, sealed or not, goes to the gate's
S3-compatible bucket when it has one (`blobs:`), and a node fetches it through its own tunnel.

## MCP servers (M3)

`kontra connect` hosts MCP servers next to agents, and the gate serves each as `/mcp/<name>`, with each client
session pinned to one node's tunnel and reached from any gate replica through the peer hop, per-tool policy and
metering; a task-augmented `tools/call` becomes a durable gate task, as an A2A task is, and 2026-07-28 clients are
served on the same endpoint ([`spec/relay.md`](spec/relay.md) §11). Stock servers run unmodified.
[`examples/mcp/everything`](examples/mcp/everything/README.md) hosts `server-everything@1.0.0` over Streamable HTTP,
with Postgres and Temporal up and the binaries built as above (one example gate at a time: each listens on
`127.0.0.1:8080`):

```sh
PORT=3001 examples/mcp/everything/run.sh &       # npm ci on first use; then http://127.0.0.1:3001/mcp

./kontra-gate migrate -f examples/mcp/everything/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/mcp/everything/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/mcp/everything/kontra.yaml &

./kontra connect --gate http://localhost:8080 --serve mcp:everything=http://127.0.0.1:3001/mcp \
  --enroll "$(./kontra-gate enroll -f examples/mcp/everything/kontra.yaml everything)" &
#    mcp everything  http://localhost:8080/mcp/everything
```

`--serve mcp:<name>=<url>` names the server's endpoint, path included. Any MCP client with a bearer token reaches
the service at `http://localhost:8080/mcp/everything`, and a local one through a node at
`http://localhost:7777/mcp/everything`. [`examples/mcp/fs-stdio`](examples/mcp/fs-stdio/README.md) hosts
`server-filesystem@1.0.0` over stdio: the node runs the server itself, one process per MCP session and without a
shell, from `--serve "mcp:fs=stdio:<command line>" --roots fs=/srv/kontra-fs`. For every MCP server it hosts, the
node answers the server's `roots/list` itself, from `--roots <name>=<dir>` (none by default), so a client's roots
never reach the server.

Per-tool policy is the service's `tools:` in the gate's configuration:

```yaml
services:
  everything:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    tools: { deny: ["get-env", "gzip-file-as-resource"] }
```

A denied tool is missing from `tools/list`, and a call to it is answered `Unknown tool`, as for one that does not
exist; `allow:` narrows the rest (absent, every tool; empty, none). The gate meters each call by tool name, bytes,
duration and the result's top-level `isError`, and never reads arguments or content, which may be sealed.

## A team gate (M3)

`compose.yaml` runs the team gate on one host: the gate built from this repository's `Dockerfile`, terminating TLS
itself, with Temporal and Postgres on the compose network only.
[`deploy/compose/kontra.yaml`](deploy/compose/kontra.yaml) says what to edit (`public_url`, the certificate, the
card-signing key) and serves the examples' four services:

```sh
docker compose up -d --build --wait
docker compose exec gate kontra-gate enroll -f /etc/kontra/kontra.yaml --api-key me     # an API key, once
docker compose exec gate kontra-gate enroll -f /etc/kontra/kontra.yaml hello            # a node's token
kontra connect --gate https://<public_url host> --enroll <token> --serve a2a:hello=http://127.0.0.1:9999
docker compose down            # -v also drops the databases
```

PRD §12's pentest list ran against it, with M3's own surface; [`docs/pentest-m3.md`](docs/pentest-m3.md) records
every probe and the four findings, each fixed.

## Five minutes (the M4 target)

```sh
brew install kontra
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli && brew install a2a   # a2a-cli v0.3.1
a2a server --exec 'echo Hello World' --port 9999        # or your own agent, in docker or not
kontra connect --serve a2a:hello=http://localhost:9999 \
               --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs'
#  → anonymous session · https://gateway.kontra.sh/agents/anon-7f3a/hello · expires in 2 h
a2a send -a http://localhost:7777/agents/anon-7f3a/hello "hi"
#  → Hello World, hosted and hired from the same laptop
```

`--gate` defaults to `gateway.kontra.sh`. Any stock MCP client reaches the filesystem server through the
same gate: `{ "mcpServers": { "fs": { "url": "http://localhost:7777/mcp/anon-7f3a/fs" } } }`.
An MCP server over HTTP is `--serve mcp:name=http://localhost:8000`.

A team gate is the same image on your own VPS: `docker compose up` with `compose.yaml`
(`deploy/compose/kontra.yaml` says what to edit), then `kontra connect --gate https://gate.team.internal …`.

## Layout (planned)

```
cmd/kontra/        the CLI: connect, seal (login and publish come with M4 and M5; no console, D164)
cmd/kontra-gate/   the gate: serve, enroll, migrate, health
internal/          relay, tunnel, store (Postgres), lifecycle (Temporal), sealing
spec/              tunnel.md, relay.md, ext-escrow.md, ext-sealed.md, ext-input-schema.md, schema/
migrations/        0001_init.sql, 0002_m3.sql (the gate's Postgres schema)
charts/kontra/     Helm chart for a team gate
compose.yaml       the team gate on one host: the gate, Temporal and Postgres
compose.dev.yaml   Temporal + Postgres for working on the gate itself
examples/          sellers/ (a2a server --exec, helloworld, booking, tck, temporal-harness, ADK, claude CLI),
                   mcp/ (filesystem over stdio, everything), sealing/, front/ (caddy, agentgateway, ingress)
```

Apache-2.0.
