#!/bin/sh
# First boot initialises the console account; then run the command Compose asked for.
#
# THE APPLIANCE USED TO EXEC `kontra up` HERE. The supported install is now a Compose cluster
# (ADR 0047): this image is the CLI, the Warden, workspace seed/watch, and `kontra init`.
# `exec "$@"` so PID 1 is the command Compose named — Warden serve, workspace watch, or init.
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
