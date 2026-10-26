# mcp/fs-stdio

Skeleton (M0). `@modelcontextprotocol/server-filesystem@1.0.0` over stdio, bridged by `kontra connect` — M3

The command passes the server its allowed directory explicitly, and the client's roots never reach it: the node
answers the server's `roots/list` itself from `--roots`, none by default (D173). The write tools are denied in
the gate's service configuration unless the directory is meant to be written.

```sh
kontra connect --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 /srv/kontra-fs'
```
