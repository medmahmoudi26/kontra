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

Specification stage. `docs/` is the current design; code lands against it. Read in this order:

1. [`docs/prd-v4.md`](docs/prd-v4.md) — the v4 PRD (21 Oct 2026). The relay, data model,
   security model, milestones and acceptance tests.
2. [`docs/architecture/01-tunnel-and-protocols.html`](docs/architecture/01-tunnel-and-protocols.html)
   — one node type, gate as membership, the Temporal primitive, tunneling MCP servers next to A2A
   agents, bindings. Supersedes PRD §3–4, §6 (target), §8 (keys), §11 (two sidecars).
3. [`docs/architecture/02-deployment-topology.html`](docs/architecture/02-deployment-topology.html)
   — one CLI on the user's machine, gates as clusters, `kontra dev` removed. Supersedes PRD §10
   (embedded Temporal), §11 (`kontra dev`), §14 M4, §15 demo.

Where a note and the PRD disagree, the later document wins; `docs/README.md` has the section map.

## Five minutes

```sh
brew install kontra
a2a serve --exec 'echo Hello World' --port 9999         # or your own agent, in docker or not
kontra connect --serve a2a:hello=http://localhost:9999   # --gate defaults to gateway.kontra.sh
a2a send http://localhost:7777/agents/anon-7f3a/hello "hi"
```

An MCP server is `--serve mcp:name=http://localhost:8000` or `--serve mcp:name=stdio:<command>`.

## Layout (planned)

```
cmd/kontra/        the CLI: connect, console, login, seal, publish
cmd/kontra-gate/   the gate: serve, enroll, migrate
internal/          relay, tunnel, store (Postgres), lifecycle (Temporal), sealing
spec/              ext-escrow.md, ext-sealed.md, ext-input-schema.md, tunnel.md, relay.md
charts/kontra/     Helm chart for a team gate
compose.yaml       three containers for a team gate on one VPS
compose.dev.yaml   Temporal + Postgres for working on the gate itself
examples/          sellers/ (a2a serve --exec, synapse, ADK, claude CLI), front/ (caddy, ingress)
```

Apache-2.0.
