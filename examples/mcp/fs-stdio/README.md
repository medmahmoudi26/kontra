# mcp/fs-stdio

`@modelcontextprotocol/server-filesystem@1.0.0` over stdio, bridged by `kontra connect` (M3): the node owns the
server process, one per MCP session, and the gate sees a Streamable HTTP MCP endpoint like any other
(spec/tunnel.md §10). The server is unmodified and imports nothing from Kontra; `package-lock.json` pins it and
everything it depends on.

## What the node does for it

- `--serve 'mcp:fs=stdio:<command line>'` is one argument. `kontra connect` splits it into words (POSIX quotes and
  backslashes, a leading `~/` expanded) and runs it **without a shell**, so a pipe, a redirection or a `$VAR`
  is refused: quote it, or run a script.
- Each `initialize` starts one process, and its MCP session ends with it: `DELETE`, `--session-idle` (default 1 h),
  a crash, a tunnel session that closes, or `kontra connect` stopping. A stopped process gets its stdin closed,
  then its whole process group `SIGTERM` 5 s later and `SIGKILL` 5 s after that, so `npx`'s `node` never outlives
  it (D184).
- At most `--max-sessions` processes (default 8) run per service; one more `initialize` gets `503` until one ends.
  A durable task holds one too, for as long as it runs (its dispatch opens its own session, D22).
- The process gets `PATH`, `HOME` and `LANG` from `kontra connect`'s environment, and the variables `--env NAME`
  names; nothing else. Its stderr goes to the node's log, at most 1 MiB a minute.
- **Roots stay on the node** (D173). `server-filesystem@1.0.0` replaces the directories it was started with by the
  client's roots, so a buyer (or the gate) that declared `file:///` would read and write the whole machine:
  `~/.kontra/identity.key`, the job keys, `~/.bashrc`. The node never relays the client's roots: it answers the
  server's `roots/list` itself, from `--roots fs=<dir>` (repeatable, one directory each; none by default, an empty
  list), and drops the client's `roots/list_changed`.
- Only MCP 2025-11-25 and 2025-06-18: a 2026-07-28 request gets the legacy `400`, and a dual-era client falls back
  to `initialize` (D161).

## Run it behind a gate (M3)

`kontra.yaml` in this directory is a team gate on one machine (the gate on `127.0.0.1:8080`, Postgres and Temporal
from the repository's `compose.dev.yaml`), with one MCP service, `fs`, hosted through the tunnel. Its write tools
(`write_file`, `edit_file`, `create_directory`, `move_file`) are denied: remove the line only for a directory that
is meant to be written. The commands below run from the repository root.

```sh
(cd examples/mcp/fs-stdio && npm ci --ignore-scripts)      # server-filesystem@1.0.0 as the lock pins it
mkdir -p /srv/kontra-fs                                     # a dedicated directory: the only one the server sees

docker compose -f compose.dev.yaml up -d --wait
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra
./kontra-gate migrate -f examples/mcp/fs-stdio/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/mcp/fs-stdio/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/mcp/fs-stdio/kontra.yaml &

./kontra connect --gate http://localhost:8080 \
  --serve "mcp:fs=stdio:$PWD/examples/mcp/fs-stdio/node_modules/.bin/mcp-server-filesystem /srv/kontra-fs" \
  --roots fs=/srv/kontra-fs \
  --enroll "$(./kontra-gate enroll -f examples/mcp/fs-stdio/kontra.yaml fs)" &
#  kontra connect: connected to http://localhost:8080 as k1_… (tenant default, session …)
#    mcp fs  http://localhost:8080/mcp/fs
```

The allowed directory is on the server's command line, and `--roots` names the same one: the server then lists
exactly it (`list_allowed_directories`), whatever roots a client declares. Without `--roots` the node declares no
roots capability and the server keeps its command-line directory.

Any MCP client with a bearer token reaches it at `http://localhost:8080/mcp/fs`, for example:

```sh
curl -s -D - http://localhost:8080/mcp/fs -H "Authorization: Bearer $KONTRA_API_KEY" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"curl","version":"1"}}}'
#  -> 200, the server's InitializeResult, and the gate's Mcp-Session-Id
```

`npx -y @modelcontextprotocol/server-filesystem@1.0.0 /srv/kontra-fs` works as the command line too, but it
downloads the package (and the latest versions its ranges allow) on a session's first start, and pins only the
server itself; the lock above pins everything.
