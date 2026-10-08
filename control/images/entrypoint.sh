#!/bin/sh
# First boot initialises the console account; then run the command Compose asked for.
#
# THIS IMAGE IS NOT THE CONTROL PLANE (ADR 0047): it is the CLI, the Warden, workspace seed/watch
# and `kontra init`. `exec "$@"` so PID 1 is the command Compose named — Warden serve, workspace
# watch, or init.
set -e

if [ -n "${KONTRA_SKIP_INIT:-}" ]; then
  echo "kontra: skipping init (KONTRA_SKIP_INIT is set)"
else
  kontra init
fi

if [ "$#" -eq 0 ]; then
  echo "kontra: no command; this image is not the control plane. Use docker compose." >&2
  exec kontra version
fi
exec "$@"
