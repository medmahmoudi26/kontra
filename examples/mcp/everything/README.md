# mcp/everything

Skeleton (M0). `@modelcontextprotocol/server-everything@1.0.0 streamableHttp`: an upstream with 2025-11-25 tasks, for `mcp_task_resume` — M3

`server-everything@1.0.0` listens on every interface and ships `get-env` (its whole environment) and
`gzip-file-as-resource` (fetches any URL while `GZIP_ALLOWED_DOMAINS` is empty). Run it bound to loopback with a
minimal environment and `GZIP_ALLOWED_DOMAINS` set, and deny both tools in the gate's service configuration
(D185).
