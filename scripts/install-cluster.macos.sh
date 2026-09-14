#!/bin/sh
# Same assertions as the Ubuntu CI cluster job, for Docker Desktop on macOS.
# Build the three images first (or point KONTRA_*_IMAGE at published tags).
set -euo pipefail
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kontra-cluster.XXXX")
ws=$(mktemp -d "${TMPDIR:-/tmp}/kontra-workspaces.XXXX")
cleanup() {
  (cd "$tmp" && docker compose down -v) >/dev/null 2>&1 || true
  rm -rf "$ws"
}
trap cleanup EXIT
mkdir -p "$ws"
cp "$root/docker-compose.quickstart.yml" "$tmp/docker-compose.yml"
cp "$root/.env.quickstart" "$tmp/.env"
{
  echo "KONTRA_IMAGE=${KONTRA_IMAGE:-kontra:latest}"
  echo "KONTRA_ORCHESTRATOR_IMAGE=${KONTRA_ORCHESTRATOR_IMAGE:-kontra-orchestrator:latest}"
  echo "KONTRA_HOST_IMAGE=${KONTRA_HOST_IMAGE:-kontra-host:1}"
  echo "KONTRA_PULL_POLICY=${KONTRA_PULL_POLICY:-never}"
  echo "KONTRA_WORKSPACES=$ws"
} >> "$tmp/.env"
cd "$tmp"
docker compose up -d --wait
curl -fsS http://127.0.0.1:8088/api/health | grep -q '"ok":true'
docker compose logs --no-color cluster-init | grep -q 'console login'
test -f "$ws/hello/actors/hello/actor.json"
test -f "$ws/hello/workflows/hello/workflow.py"
python3 "$root/scripts/assert-loopback-publish.py"
echo "cluster install ok (login, seed, loopback). Serve/start the starter with:"
echo "  docker compose exec -d cli sh -c 'kontra workflow serve \"\$KONTRA_WORKSPACES/hello/workflows/hello\"'"
echo "  docker compose exec -T cli sh -c 'kontra workflow start \"\$KONTRA_WORKSPACES/hello/workflows/hello\" --wait'"
