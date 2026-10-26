# Examples

Each directory says what it holds and the milestone that builds it; the rest are still the M0 skeleton.
Every example runs **unmodified** behind the gate: none of them imports Kontra code (`examples_test.go` checks
the Go ones), and none has a Kontra-specific endpoint. If an example needs a change to work through the
tunnel, the gate is wrong, not the example.

| Directory | What | Milestone |
|---|---|---|
| `sellers/hello-exec` | `a2a server --exec 'echo Hello World'` (a2a-cli v0.3.1): the README demo seller, as a direct backend (`kontra.yaml`) and through `kontra connect` (`kontra-tunnel.yaml`) | M1 (direct), M2 (tunnel): built |
| `sellers/helloworld` | a2a-samples helloworld at `6603ba3` (a2a-sdk 1.1.0, a 1.0 card), run unmodified by `run.sh` and hosted through `kontra connect`. The 0.2.5 seller at `08dde55c070f` that `card_compat` names is not built: `card_compat` runs against `slowagent`'s stand-in for it (D122) | M2: built |
| `sellers/booking` | A small a2a-go v2.7.0 agent that asks for a date (`input-required`) and confirms, over JSON-RPC and HTTP+JSON: the `input-required` seller (`input_required_sticky`), directly or through `kontra connect` | M2: built |
| `sellers/tck` | An a2a-go v2.7.0 seller that implements the TCK's Gherkin scenarios (keyed on `messageId` prefixes), for `tck_*` | M4 |
| `sellers/temporal-harness` | An OpenAI Agents SDK agent in a Temporal workflow of the seller's own, served as A2A (PRD §16: survives as an example, not core) | M4 |
| `sellers/adk` | A Google ADK agent served as A2A | M4 |
| `sellers/claude-cli` | A `summarize` agent that runs the `claude` CLI as a subprocess, served as A2A | M4 |
| `mcp/fs-stdio` | `@modelcontextprotocol/server-filesystem@1.0.0` over stdio, bridged by `kontra connect` | M3 |
| `mcp/everything` | `@modelcontextprotocol/server-everything@1.0.0 streamableHttp`: an upstream with 2025-11-25 tasks, for `mcp_task_resume` | M3 |
| `front/caddy` | Caddy in front of a gate, for `tck_behind_caddy` | M4 |
| `front/agentgateway` | agentgateway as an optional front proxy | M4 |
| `front/ingress` | Kubernetes ingress (ingress-nginx) with the WebSocket timeouts the tunnel needs | M4 |

Pins are exact (no `^`, no `latest`): the npm MCP servers moved to semver and `latest` now needs Node 24.
