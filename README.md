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
MCP servers beside them, over Streamable HTTP or stdio, with end-to-end sealing (M3), and since M4:

- serves its own MCP endpoint, `/mcp`: `search`, `describe`, `hire`, `reply` and the MCP Tasks of what was hired, in
  both MCP eras (2025-11-25 and 2026-07-28), so a stock MCP client hires an A2A agent
  ([`spec/relay.md`](spec/relay.md) §12);
- opens anonymous sessions on a gate that offers them: `kontra connect` with no credential gets a tenant of its own
  for two hours (`anon-` and 16 hex digits), and `--public <name>` lets anyone call that service
  ([`spec/tunnel.md`](spec/tunnel.md) §6);
- is an MCP OAuth resource server, and `kontra login` gets an API key from a gate's authorization server; asks a
  remote Admission hook before a task or a session; and passes the pinned A2A TCK in CI (`make tck`);
- comes up on a fresh VPS from `compose.yaml` with built-in ACME ([below](#a-team-gate-on-your-own-vps)).

The demo below runs against the staging gate, `gateway.kontra.run`: the production gate, `gateway.kontra.sh`, opens no
anonymous sessions until Cloud's M5 (DM4-1 and DM4-13 in [`docs/decisions.md`](docs/decisions.md)). Every built
seller and server is listed in [`examples/`](examples/README.md); stock agents and MCP servers run unmodified.

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

On a laptop with Go 1.26 and Node 22 (for the MCP lines), and nothing of Kontra:

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
`scripts/demo-clean-laptop.sh` runs this block, as written, in a fresh container (DM4-13).

## A team gate on your own VPS

The same gate image with Temporal and Postgres, from [`compose.yaml`](compose.yaml): three long-running containers
and two one-shot jobs, nothing published but the gate. On a fresh VPS with Docker, a DNS name pointing at it and
nothing listening on 443, the gate gets its own certificate (built-in ACME, TLS-ALPN-01):

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

Then, on any machine of the team: `kontra connect --gate https://<your name> --enroll <token> --serve
a2a:hello=http://127.0.0.1:9999`, and `a2a send -a https://<your name>/agents/hello/.well-known/agent-card.json --auth
"Bearer <API key>" "hi"`. The same key reaches the gate's own MCP server at `https://<your name>/mcp`. With a
certificate of your own, keep the default configuration, [`deploy/compose/kontra.yaml`](deploy/compose/kontra.yaml),
which says where it goes. Helm: [`charts/kontra`](charts/kontra).

Upgrading a gate to M4: `kontra-gate migrate` (compose runs it in `gate-migrate`) applies `0003_m4.sql`, which holds
the `tasks` table's lock for one sequential read of it (its header says why); run it before the new replicas start, or
when that pause is acceptable. A team or account slug may no longer be `anon-` and 16 hex digits.

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

The gate never dials the seller, and the seller imports nothing from Kontra. `--serve` names the agent's root
URL: a target with a path is refused (D123 in [`docs/decisions.md`](docs/decisions.md)). What a lost node or a
seller that is down does to a task, and how to restart the node, is in the
[helloworld README](examples/sellers/helloworld/README.md#restarts-order-and-failures).

## Layout

```
cmd/kontra/        the CLI: connect, login, seal, version (publish comes with Cloud's listings, M5)
cmd/kontra-gate/   the gate: serve, migrate, enroll, health, version
internal/          relay, tunnel, gatemcp (/mcp), anon, store (Postgres), lifecycle (Temporal), sealing, …
spec/              tunnel.md, relay.md, ext-escrow.md, ext-sealed.md, ext-input-schema.md, schema/
migrations/        0001_init.sql, 0002_m3.sql, 0003_m4.sql (the gate's Postgres schema, embedded in kontra-gate)
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
