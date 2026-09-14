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
  api|api,*|*,api|*,api,*)
    i=0
    while [ ! -f "$home/config.yaml" ]; do
      i=$((i + 1))
      if [ "$i" -gt 120 ]; then
        echo "orchestrator: timed out waiting for $home/config.yaml (cli runs kontra init)" >&2
        exit 1
      fi
      sleep 1
    done
    ;;
esac
case "$roles" in
  api) exec node dist/src/server.js ;;
  materializer) exec node dist/src/materializer.js ;;
  infra) exec node dist/src/infra.js ;;
  *) exec node dist/src/main.js ;;
esac
