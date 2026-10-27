# mcp/everything

`@modelcontextprotocol/server-everything@1.0.0 streamableHttp`, hosted through `kontra connect` (M3): an MCP
server over Streamable HTTP with 2025-11-25 tasks (`simulate-research-query`), elicitation, sampling and roots, the
upstream of `mcp_task_resume`. The server is unmodified and imports nothing from Kontra; `package-lock.json` pins it
and everything it depends on.

## Run it safely (D185)

`server-everything@1.0.0` listens on **every interface** (`app.listen(port)`) with CORS `*`, and ships two tools
that are dangerous on a shared machine: `get-env` returns its whole environment, and `gzip-file-as-resource`
fetches any URL while `GZIP_ALLOWED_DOMAINS` is empty. `run.sh` therefore starts it

- bound to `127.0.0.1` (`loopback.cjs`, preloaded with `NODE_OPTIONS=--require=…`, makes every `listen()` that names
  no host, which Node binds on every interface, bind loopback),
- with a minimal environment (`PATH`, `HOME`, `LANG`, `PORT`), so `get-env` has nothing to give away,
- with `GZIP_ALLOWED_DOMAINS=example.invalid`, a name that matches nothing,

and `kontra.yaml` denies `get-env` and `gzip-file-as-resource` at the gate (spec/relay.md §11.3).

## Run it behind a gate (M3)

`kontra.yaml` in this directory is a team gate on one machine (the gate on `127.0.0.1:8080`, Postgres and Temporal
from the repository's `compose.dev.yaml`), with one MCP service, `everything`, hosted through the tunnel. The
commands below run from the repository root.

```sh
PORT=3001 examples/mcp/everything/run.sh &       # npm ci on first use; then http://127.0.0.1:3001/mcp

docker compose -f compose.dev.yaml up -d --wait
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra
./kontra-gate migrate -f examples/mcp/everything/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/mcp/everything/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/mcp/everything/kontra.yaml &

./kontra connect --gate http://localhost:8080 --serve mcp:everything=http://127.0.0.1:3001/mcp \
  --enroll "$(./kontra-gate enroll -f examples/mcp/everything/kontra.yaml everything)" &
#  kontra connect: connected to http://localhost:8080 as k1_… (tenant default, session …)
#    mcp everything  http://localhost:8080/mcp/everything
```

`--serve mcp:<name>=<url>` names the server's MCP endpoint, path included (`/mcp`): the node sends every MCP
exchange the gate forwards to that URL and nowhere else, with `Host` set to it and `Origin` removed (spec/tunnel.md
§8.2). It forwards them unchanged except for roots: the node answers the server's `roots/list` itself, from
`--roots everything=<dir>` (none by default, an empty list), and never relays a client's roots to the server
(D173).
