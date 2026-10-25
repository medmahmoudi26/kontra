#!/usr/bin/env bash
# The hello-exec seller: a2a-cli v0.3.1 exec mode (D15). Plain HTTP on loopback; the gate reaches it as a
# direct backend with allow_private (M1) or through `kontra connect` (M2).
#
#   SELLER_CMD    the command run once per message   (default: echo Hello World)
#   SELLER_HOST   bind address                       (default: 127.0.0.1; 0.0.0.0 in a container)
#   SELLER_PORT   listen port                        (default: 9999)
#   SELLER_CHUNK  stream stdout split on this delimiter (default: unset, no streaming)
set -euo pipefail

want="v0.3"
command -v a2a >/dev/null || { echo "a2a not found: install a2a-cli v0.3.1 (see README.md)" >&2; exit 1; }
have="$(a2a version 2>/dev/null | head -1)"
case "$have" in
  *"$want"*) ;;
  *) echo "warning: a2a-cli ${want}.x expected, found: $have" >&2 ;;
esac

args=(server --exec "${SELLER_CMD:-echo Hello World}" --host "${SELLER_HOST:-127.0.0.1}" --port "${SELLER_PORT:-9999}")
if [[ -n "${SELLER_CHUNK:-}" ]]; then
  args+=(--chunk "$SELLER_CHUNK")
fi
exec a2a "${args[@]}"
