# Examples

Each directory says what it holds and the milestone that builds it; the rest are still the M0 skeleton.
Every example runs **unmodified** behind the gate: none of them imports Kontra code (`examples_test.go` checks
the Go ones), and none has a Kontra-specific endpoint. If an example needs a change to work through the
tunnel, the gate is wrong, not the example.

Since M2 the A2A sellers run behind the gate either way: as a direct backend, which the gate dials
(`backend: { a2a: { url } }` in `kontra.yaml`), or hosted through the tunnel (`backend: { tunnel: {} }`, no URL at
the gate), where `kontra connect --serve a2a:<name>=<url>`, running next to the seller, dials the gate and forwards
the gate's requests to `<url>` and nowhere else. `--serve` names the seller's root URL; M2 refuses a target with a
path (D123 in [`../docs/decisions.md`](../docs/decisions.md)). Each built seller's README gives the commands from
an empty database: `hello-exec` both ways, `helloworld` and `booking` through the tunnel.

Since M3 `kontra connect` also hosts MCP servers: `--serve mcp:<name>=<url>` names a Streamable HTTP server's
endpoint, path included, and `--serve 'mcp:<name>=stdio:<command line>'` runs a stdio server, one process per MCP
session. For both, the node answers the server's `roots/list` itself from `--roots` and never passes on a client's
roots (D173). The MCP examples run their servers from pinned `package-lock.json` files, on loopback, with nothing
to leak (D185).

| Directory | What | Milestone |
|---|---|---|
| `sellers/hello-exec` | `a2a server --exec 'echo Hello World'` (a2a-cli v0.3.1): the README demo seller, as a direct backend (`kontra.yaml`) and through `kontra connect` (`kontra-tunnel.yaml`) | M1 (direct), M2 (tunnel): built |
| `sellers/helloworld` | a2a-samples helloworld at `6603ba3` (a2a-sdk 1.1.0, a 1.0 card), run unmodified by `run.sh` and hosted through `kontra connect`; `test/e2e` `TestHelloworldTunnel` runs it through two gate replicas. The 0.2.5 seller at `08dde55c070f` that `card_compat` names is not built: `card_compat` runs against `slowagent`'s stand-in for it (D122, D154) | M2: built |
| `sellers/booking` | A small a2a-go v2.7.0 agent that asks for a date (`input-required`) and confirms, over JSON-RPC and HTTP+JSON: the `input-required` seller (`input_required_sticky`), directly or through `kontra connect` | M2: built |
| `sellers/tck` | An a2a-go v2.7.0 seller that implements the TCK's Gherkin scenarios (keyed on `messageId` prefixes), for `tck_*` | M4 |
| `sellers/temporal-harness` | An OpenAI Agents SDK agent in a Temporal workflow of the seller's own, served as A2A (PRD §16: survives as an example, not core) | M4 |
| `sellers/adk` | A Google ADK agent served as A2A | M4 |
| `sellers/claude-cli` | A `summarize` agent that runs the `claude` CLI as a subprocess, served as A2A | M4 |
| `mcp/fs-stdio` | `@modelcontextprotocol/server-filesystem@1.0.0` over stdio, bridged by `kontra connect` with its allowed directory on the command line and `--roots`; the write tools denied at the gate (`kontra.yaml`) | M3: built |
| `mcp/everything` | `@modelcontextprotocol/server-everything@1.0.0 streamableHttp`, run on loopback with a minimal environment (`run.sh`) and hosted through `kontra connect`: an upstream with 2025-11-25 tasks, for `mcp_task_resume`; `get-env` and `gzip-file-as-resource` denied at the gate | M3: built |
| `front/caddy` | Caddy in front of a gate, for `tck_behind_caddy` | M4 |
| `front/agentgateway` | agentgateway as an optional front proxy | M4 |
| `front/ingress` | Kubernetes ingress (ingress-nginx) with the WebSocket timeouts the tunnel needs | M4 |

Pins are exact (no `^`, no `latest`): the npm MCP servers moved to semver and `latest` now needs Node 24.
