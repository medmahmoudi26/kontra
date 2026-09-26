#!/bin/sh
# Same assertions as the Ubuntu CI cluster job, for Docker Desktop on macOS.
#
# WITH NO ARGUMENTS IT IS THE PUBLIC QUICKSTART, run out of this checkout: compose's own defaults,
# which are `ghcr.io/medmahmoudi26/*:dev`. Point `KONTRA_*_IMAGE` at local tags to test images you
# just built instead — that is what CI does, for the same reason (it has to test the commit in front
# of it, not the last publish).
#
# ── IT NO LONGER COPIES A SINGLE SCRIPT IN, AND THAT USED TO BE A BUG IT WAS HIDING ─────────────
#
# It copied `control/images/postgres-init.sh` and NOT `logship.sh` or `logline.py`, all three of
# which compose bind-mounted. Docker does not fail on a bind-mount source that does not exist — it
# creates an empty DIRECTORY there. So logship refused to start, said exactly why, restarted, and
# said it again forever; logship had no healthcheck, `docker compose up -d --wait` therefore never
# waited on it, and this script exited 0. It had been passing while shipping zero logs, which is
# indistinguishable from runs that logged nothing.
#
# Both halves are fixed and neither alone would have been enough: the mounts are gone (an inline
# `configs:` entry for the SQL, `kontra-logship` for the shipper), and the image carries a
# HEALTHCHECK so the next missing file fails `--wait` instead of passing quietly.
set -euo pipefail
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kontra-cluster.XXXX")
ws=$(mktemp -d "${TMPDIR:-/tmp}/kontra-workspaces.XXXX")
cleanup() {
  (cd "$tmp" && docker compose down -v) >/dev/null 2>&1 || true
  rm -rf "$ws"
}
trap cleanup EXIT

# TWO FILES, WHICH IS WHAT A `curl` PUTS IN A DIRECTORY. Anything else here is a host dependency the
# quickstart does not have, so the count is checked rather than assumed — same assertion as the
# `docker` job in ci.yml.
mkdir -p "$ws"
cp "$root/docker-compose.quickstart.yml" "$tmp/docker-compose.yml"
cp "$root/.env.quickstart" "$tmp/.env"
# `if`, NOT `[ -n … ] && echo`. Under `set -e` a `&&` list whose left side is false returns non-zero
# and takes the script with it, so the first unset variable would abort the install with no message.
{
  echo "KONTRA_WORKSPACES=$ws"
  # Unset means compose's published defaults — the path a stranger takes. Set means local tags.
  for v in KONTRA_IMAGE KONTRA_ORCHESTRATOR_IMAGE KONTRA_HOST_IMAGE KONTRA_LOGSHIP_IMAGE KONTRA_PULL_POLICY; do
    eval "val=\${$v:-}"
    if [ -n "$val" ]; then echo "$v=$val"; fi
  done
} >> "$tmp/.env"

# THE SAME CHECKER ci.yml RUNS, not a second copy of the rule. It also asserts what a plain `find`
# here could not: that compose itself names no host path but the socket and the workspace, and that
# every `configs:` entry declares a mode.
python3 "$root/scripts/assert-install-is-self-contained.py" "$tmp"

cd "$tmp"
docker compose up -d --wait
curl -fsS http://127.0.0.1:8088/api/health | grep -q '"ok":true'
docker compose logs --no-color cli | grep -q 'console login'
test -f "$ws/hello/actors/hello/actor.json"
test -f "$ws/hello/workflows/hello/workflow.py"

# THE SHIPPER IS ASSERTED NOW, because for a long time it was not and that is the whole point of the
# comment at the top of this file. `--wait` above already refuses to pass an unhealthy logship; this
# says so out loud, so a person reading the output learns that logs were checked.
docker compose ps --format '{{.Service}} {{.Health}}' | grep -q '^logship healthy$' \
  || { echo "logship is not healthy — the logs rail will be empty and nothing else will say so" >&2
       docker compose logs --no-color logship | tail -20 >&2; exit 1; }

# And the catalog the inline `configs:` entry is responsible for. Without it every Dataset write
# fails against a database that does not exist, several services away from anything named postgres.
docker compose exec -T postgres psql -U kontra -d postgres -tAc \
  "SELECT 1 FROM pg_database WHERE datname='kontra_ducklake'" | grep -q 1 \
  || { echo "kontra_ducklake was not created — the ducklake-init config did not run" >&2; exit 1; }

python3 "$root/scripts/assert-loopback-publish.py"
echo "cluster install ok (login, seed, loopback, logship, ducklake catalog). Serve/start the starter with:"
echo "  docker compose exec -d cli sh -c 'kontra workflow serve \"\$KONTRA_WORKSPACES/hello/workflows/hello\"'"
echo "  docker compose exec -T cli sh -c 'kontra workflow start \"\$KONTRA_WORKSPACES/hello/workflows/hello\" --wait'"
