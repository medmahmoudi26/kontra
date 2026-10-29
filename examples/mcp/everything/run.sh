#!/usr/bin/env bash
# server-everything@1.0.0 (streamableHttp) at http://127.0.0.1:${PORT:-3001}/mcp, from this directory's
# package-lock, bound to loopback, with a minimal environment and GZIP_ALLOWED_DOMAINS set (D185): nothing in its
# environment for get-env to return, and no URL that gzip-file-as-resource may fetch.
set -euo pipefail
cd "$(dirname "$0")"
[[ -d node_modules/@modelcontextprotocol/server-everything ]] || npm ci --ignore-scripts --no-audit --no-fund
exec env -i PATH="$PATH" HOME="$HOME" LANG="${LANG:-C.UTF-8}" PORT="${PORT:-3001}" \
  GZIP_ALLOWED_DOMAINS=example.invalid NODE_OPTIONS="--require=$PWD/loopback.cjs" \
  node node_modules/@modelcontextprotocol/server-everything/dist/index.js streamableHttp
