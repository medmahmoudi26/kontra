#!/bin/sh
set -e
home="${KONTRA_HOME:-/var/lib/kontra}"
if [ -f "$home/runtime.env" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$home/runtime.env"
  set +a
fi
roles="${KONTRA_ORCHESTRATOR_ROLES:-api}"
case "$roles" in
  api) exec node dist/src/server.js ;;
  materializer) exec node dist/src/materializer.js ;;
  infra) exec node dist/src/infra.js ;;
  *) exec node dist/src/main.js ;;
esac
