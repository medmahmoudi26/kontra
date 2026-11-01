# kontra

A durable tunnel for A2A agents and MCP servers.

A node runs one CLI, `kontra connect`. It serves what is on its localhost to a gate it chose and
calls what others serve through the same gate. The gate adds durable tasks (Temporal + Postgres),
identity, policy and end-to-end sealing. Nothing on the node listens on a public port.

- Connect to Kontra Cloud's gate, `gateway.kontra.sh`, and you are on the marketplace (Cloud, M5).
- Run the same gate image yourself and you have a private one for your team, with no catalog and no
  money code.

This repository holds the open-source parts: the `kontra` CLI, the `kontra-gate` server image,
the Helm chart and compose file, the extension specs (`escrow`, `sealed`, `input-schema`) and the
examples. The marketplace, listings, escrow and settlement live in a separate private repository
that deploys and configures this one; nothing here imports from it.

## Status

M4. `kontra-gate` relays A2A tasks durably (M1) to agents on a node's localhost through `kontra connect` (M2), hosts
MCP servers beside them, over Streamable HTTP or stdio, with end-to-end sealing (M3; [below](#sealed-end-to-end-m3)),
and since M4:

- serves its own MCP endpoint, `/mcp`: `search`, `describe`, `hire`, `reply` and the MCP Tasks of what was hired, in
  both MCP eras (2025-11-25 and 2026-07-28), so a stock MCP client hires an A2A agent
  ([`spec/relay.md`](spec/relay.md) §12);
- opens anonymous sessions on a gate that offers them: `kontra connect` with no credential gets a tenant of its own
  for two hours (`anon-` and 16 hex digits), and `--public <name>` lets anyone call that service
  ([`spec/tunnel.md`](spec/tunnel.md) §6);
- is an MCP OAuth resource server, and `kontra login` gets an API key from a gate's authorization server; asks a
  remote Admission hook before a task or a session; and is held to the pinned A2A TCK with its listed expected
  failures (`make tck`, and the `tck` workflow);
- comes up on a fresh VPS from `compose.yaml` with built-in ACME ([below](#a-team-gate-on-your-own-vps)).

The demo below is written for the staging gate, `gateway.kontra.run`: the production gate, `gateway.kontra.sh`, opens
no anonymous sessions until Cloud's M5 (D268 and D280 in [`docs/decisions.md`](docs/decisions.md)). The staging gate
has been up since 31 Oct: M4's gate runs deployed it and ran this block on a clean laptop against it
([`docs/gate-runs-m4.md`](docs/gate-runs-m4.md)). It is a staging environment, redeployed as M4 lands.
[A team gate from source](#a-team-gate-from-source) runs the same node, gate and hire on one machine, with an enrolled
node instead of an anonymous session. Every built seller and server is listed in [`examples/`](examples/README.md);
stock agents and MCP servers run unmodified.

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
   [`ext-input-schema.md`](spec/ext-input-schema.md)), with the schema in [`migrations/`](migrations/).
   Every place they depart from the documents above is numbered in [`docs/decisions.md`](docs/decisions.md).

Where a note and the PRD disagree, the later document wins; `docs/README.md` has the section map.

## Five minutes

On a laptop with Go 1.26 and Node 22.19 or later (for the MCP lines; the pinned inspector needs it), and nothing of
Kontra:

<!-- demo:begin -->
```sh
go install kontra.sh/kontra/cmd/kontra@latest
go install github.com/a2aproject/a2a-cli@v0.3.1 && mv "$(go env GOPATH)/bin/a2a-cli" "$(go env GOPATH)/bin/a2a"
export PATH="$PATH:$(go env GOPATH)/bin"
mkdir -p ~/docs && echo 'Notes from my laptop.' > ~/docs/notes.txt

a2a server --exec 'echo Hello World' --port 9999 &          # or your own agent, in docker or not
kontra connect --gate https://gateway.kontra.run --serve a2a:hello=http://127.0.0.1:9999 \
  --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs' --public hello &
#  kontra connect: connected to https://gateway.kontra.run as k1_… (tenant anon-3f9c0a1b2d4e5f67, session …)
#    a2a hello  https://gateway.kontra.run/agents/anon-3f9c0a1b2d4e5f67/hello  (public: anyone may call it)
#    mcp fs  https://gateway.kontra.run/mcp/anon-3f9c0a1b2d4e5f67/fs
#    anonymous session: it ends at … (in 2h0m0s); its URLs then answer 404 (kontra login keeps the services)

a2a send -a http://localhost:7777/agents/anon-<…>/hello/.well-known/agent-card.json "hi"
#  → Hello World: hosted and hired from the same laptop, through the gate
npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp/anon-<…>/fs --transport http \
  --method tools/list
#  → read_file, list_directory, …: the filesystem server on this laptop, as any MCP client sees it
npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp --transport http \
  --method tools/call --tool-name hire --tool-arg service=anon-<…>/hello --tool-arg message=hi
#  → Hello World, hired through the gate's own MCP server (search, describe, hire, reply)
curl -s https://gateway.kontra.run/agents/anon-<…>/hello/.well-known/agent-card.json
#  → the card, for anyone, with no credential: share the URL
```
<!-- demo:end -->

`anon-<…>` stands for your session's slug, which `kontra connect` printed: `anon-` and 16 lowercase hex digits
(`anon-3f9c0a1b2d4e5f67` above). Replace it before running the lines that name it.

What happened: `kontra connect` had no credential for the gate, so it asked for an anonymous session
([`spec/tunnel.md`](spec/tunnel.md) §6): a tenant of its own for two hours, the agent and the filesystem server at the
gate under that slug, and `localhost:7777` for local clients to hire through the gate with the node's identity. Only
`hello` was made public; `fs` answers your node alone. The gate never dials the laptop: everything rides the one
outbound connection. When the session ends, the URLs answer `404` and `kontra connect` exits; run it again for a new
slug. `kontra login` keeps the services under an account on a gate with an authorization server (Cloud's, M5); on any
other gate it says to use `--api-key`. Anonymous sessions are bounded per network, tenant and call
([`spec/tunnel.md`](spec/tunnel.md) §6.2).

Any stock MCP client works where the inspector does: `{ "mcpServers": { "fs": { "url":
"http://localhost:7777/mcp/anon-<…>/fs" } } }`. An MCP server over HTTP is `--serve mcp:name=http://localhost:8000/mcp`.
`scripts/demo-clean-laptop.sh` runs this block, as written, in a fresh container (D280).

## A team gate on your own VPS

The same gate image with Temporal and Postgres, from [`compose.yaml`](compose.yaml): three long-running containers
and three one-shot jobs (Temporal's schema, its namespace, the gate's migrations), nothing published but the gate. On
a fresh VPS with Docker, a DNS name pointing at it and nothing listening on 443, the gate gets its own certificate
(built-in ACME, TLS-ALPN-01):

```sh
git clone https://github.com/medmahmoudi26/kontra && cd kontra
cp deploy/compose/kontra.vps.yaml deploy/compose/kontra.team.yaml
#   edit kontra.team.yaml: your DNS name in public_url and tls.acme.domains, and a database password
printf 'KONTRA_CONFIG=./deploy/compose/kontra.team.yaml\nKONTRA_PORT=443\n' > .env
#   and in .env the three passwords compose.yaml names (KONTRA_DB_PASSWORD as in kontra.team.yaml)
#   the card-signing key, as kontra.vps.yaml says (owned by uid 65532, mode 0400)
docker compose up -d --build --wait
docker compose exec gate kontra-gate enroll -f /etc/kontra/kontra.yaml --api-key me    # an API key, once
docker compose exec gate kontra-gate enroll -f /etc/kontra/kontra.yaml hello           # a node's token
```

`.env`, `deploy/compose/kontra.team.yaml` and the `signing/` and `tls/` directories beside it hold secrets: the
repository's `.gitignore` keeps them out of git and `.dockerignore` out of the image's build. The gate is published on
IPv4 and IPv6 (an AAAA record works as kontra.vps.yaml says); `KONTRA_LISTEN_ADDR` restricts it to one address.

Then, on any machine of the team: `kontra connect --gate https://<your name> --enroll <token> --serve
a2a:hello=http://127.0.0.1:9999`, and `a2a send -a https://<your name>/agents/hello/.well-known/agent-card.json --auth
"Bearer <API key>" "hi"`. The same key reaches the gate's own MCP server at `https://<your name>/mcp`. With a
certificate of your own, keep the default configuration, [`deploy/compose/kontra.yaml`](deploy/compose/kontra.yaml),
which says where it goes. Helm: [`charts/kontra`](charts/kontra).

Upgrading a gate to M4: `git pull && docker compose up -d --build --wait`. The image is `kontra-gate:m4`: without
`--build` an M3 host has no such image to run, and its `kontra-gate:m3` holds M3's embedded migrations. `kontra-gate
migrate` (compose runs it in `gate-migrate`) applies `0003_m4.sql`, which holds the `tasks` table's lock for one
sequential read of it (its header says why); run it before the new replicas start, or when that pause is acceptable.
`gate-migrate` runs the image the `gate` service builds (only `gate` builds it, D313), so to run the migration alone,
build first: `docker compose build gate && docker compose run --rm gate-migrate`. A team or account slug may no longer
be `anon-` and 16 hex digits.

M4's gate runs brought this team gate up on a fresh DigitalOcean droplet (Ubuntu 24.04, Docker 29.1.3) at `f9f5e61`:
built-in ACME got its Let's Encrypt certificate, a node on another machine enrolled, and its service was hired through
the gate ([`docs/gate-runs-m4.md`](docs/gate-runs-m4.md); the first run, at `3b890f3`, found the defect D313 fixed).

PRD §12's pentest list ran against this compose team gate with M3's own surface at `f65760a` (four findings, each
fixed) and again after the M3 review rounds at `33f06a8` (no finding, D266);
[`docs/pentest-m3.md`](docs/pentest-m3.md) records the second run's every probe, what changed since the first, and
the findings and their fixes.

## A team gate from source

For working on the gate: Postgres and Temporal from `compose.dev.yaml`, the gate and the node built from this
checkout, and the helloworld seller hosted through `kontra connect`. With Docker, Go,
[uv](https://docs.astral.sh/uv/) and a2a-cli v0.3.1 as `a2a` (its install is in
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

# The hosting node: enrolled for hello, so its key is one of the service's recipients; it only hosts (--listen off).
HOME=/tmp/seller ./kontra connect --gate http://localhost:8080 --listen off \
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

## Layout

```
cmd/kontra/        the CLI: connect, login, seal, version (publish comes with Cloud's listings, M5)
cmd/kontra-gate/   the gate: serve, migrate, enroll, health, version
internal/          relay, tunnel, gatemcp (/mcp), anon, store (Postgres), lifecycle (Temporal), sealing, …
spec/              tunnel.md, relay.md, ext-escrow.md, ext-sealed.md, ext-input-schema.md, schema/
migrations/        0001_init.sql, 0002_m3.sql, 0003_m4.sql, 0004_m5.sql (the gate's Postgres schema, embedded in kontra-gate)
charts/kontra/     Helm chart for a team gate
compose.yaml       the team gate on one host; deploy/compose/ holds its configurations (kontra.vps.yaml: ACME)
compose.dev.yaml   Temporal + Postgres for working on the gate itself
examples/          sellers/ (a2a server --exec, helloworld, booking, tck; temporal-harness, adk, claude-cli to come),
                   mcp/ (filesystem over stdio, everything), front/ (caddy; agentgateway, ingress to come), sealing/
scripts/           check_specs.py, the test runners, tck.sh, demo-clean-laptop.sh, team-gate-vps.sh
test/              e2e/ (two replicas behind a front), pentest/, tck/, demo/ (the clean laptop), vps/
```

There is no local console (D164): the task thread and the quote page are Cloud's (`app.kontra.sh`, M5).

Apache-2.0.
