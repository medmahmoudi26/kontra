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

Specification stage (M0). `docs/` is the design; `spec/` is the protocol specification written from it;
code lands against both. Read in this order:

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
   [`ext-input-schema.md`](spec/ext-input-schema.md)), with [`migrations/0001_init.sql`](migrations/0001_init.sql).
   Every place they depart from the documents above is numbered in [`docs/decisions.md`](docs/decisions.md).

Where a note and the PRD disagree, the later document wins; `docs/README.md` has the section map.

## Five minutes (the M4 target)

```sh
brew install kontra
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli && brew install a2a   # a2a-cli v0.3.1
a2a server --exec 'echo Hello World' --port 9999        # or your own agent, in docker or not
kontra connect --serve a2a:hello=http://localhost:9999 \
               --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs'
#  → anonymous session · https://gateway.kontra.sh/agents/anon-7f3a/hello · expires in 2 h
#  → console http://localhost:7778
a2a send -a http://localhost:7777/agents/anon-7f3a/hello "hi"
#  → Hello World · visible in History on :7778, hosted and hired from the same laptop
```

`--gate` defaults to `gateway.kontra.sh`. Any stock MCP client reaches the filesystem server through the
same gate: `{ "mcpServers": { "fs": { "url": "http://localhost:7777/mcp/anon-7f3a/fs" } } }`.
An MCP server over HTTP is `--serve mcp:name=http://localhost:8000`.

A team gate is the same image on your own VPS: `docker compose up` with `compose.yaml`
(lands in M4), then `kontra connect --gate https://gate.team.internal …`.

## Layout (planned)

```
cmd/kontra/        the CLI: connect, console, login, seal, publish
cmd/kontra-gate/   the gate: serve, enroll, migrate
internal/          relay, tunnel, store (Postgres), lifecycle (Temporal), sealing
spec/              tunnel.md, relay.md, ext-escrow.md, ext-sealed.md, ext-input-schema.md, schema/
migrations/        0001_init.sql (the gate's Postgres schema)
charts/kontra/     Helm chart for a team gate
compose.yaml       three containers for a team gate on one VPS
compose.dev.yaml   Temporal + Postgres for working on the gate itself
examples/          sellers/ (a2a server --exec, helloworld, booking, tck, temporal-harness, ADK, claude CLI),
                   mcp/ (filesystem over stdio, everything), front/ (caddy, agentgateway, ingress)
```

Apache-2.0.
