#!/usr/bin/env bash
# The hello-exec service: a2a-cli v0.3.1 exec mode (D15). Plain HTTP on loopback; the gate reaches it as a
# direct backend with allow_private (M1) or through `kontra connect` (M2).
#
#   SERVICE_CMD    the command run once per message   (default: echo Hello World)
#   SERVICE_HOST   bind address                       (default: 127.0.0.1; 0.0.0.0 in a container)
#   SERVICE_PORT   listen port                        (default: 9999)
#   SERVICE_CHUNK  stream stdout split on this delimiter (default: unset, no streaming)
#   A2A           the a2a-cli binary                 (default: a2a on PATH)
set -euo pipefail

want="v0.3"
a2a="${A2A:-a2a}"
command -v "$a2a" >/dev/null || { echo "$a2a not found: install a2a-cli v0.3.1 (see README.md), or set A2A to its path" >&2; exit 1; }
have="$("$a2a" version 2>/dev/null | head -1)"
case "$have" in
  *"$want"*) ;;
  *) echo "warning: a2a-cli ${want}.x expected, found: $have" >&2 ;;
esac

args=(server --exec "${SERVICE_CMD:-echo Hello World}" --host "${SERVICE_HOST:-127.0.0.1}" --port "${SERVICE_PORT:-9999}")
if [[ -n "${SERVICE_CHUNK:-}" ]]; then
  args+=(--chunk "$SERVICE_CHUNK")
fi
exec "$a2a" "${args[@]}"
