#!/usr/bin/env bash
# Integration tests: `go test -p 1` (no -short) over every package except test/e2e, which has its own
# target and harness. Uses $KONTRA_TEST_PG (a postgres:// URL to a server where the user may create
# databases; internal/pgtest makes and drops one database per test) when set; otherwise starts one
# throwaway container ($KONTRA_TEST_PG_IMAGE, default postgres:17-alpine) on a loopback port and removes
# it on exit. Extra arguments go to `go test` (e.g. -run, -v).
set -euo pipefail
cd "$(dirname "$0")/.."
GO="${GO:-go}"

if [[ -z "${KONTRA_TEST_PG:-}" ]]; then
  name="kontra-it-$$"
  docker run -d --rm --name "$name" -e POSTGRES_USER=kontra -e POSTGRES_PASSWORD=kontra -e POSTGRES_DB=kontra \
    -p 127.0.0.1::5432 "${KONTRA_TEST_PG_IMAGE:-postgres:17-alpine}" >/dev/null
  trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 60); do docker exec "$name" pg_isready -U kontra -q 2>/dev/null && break; sleep 0.5; done
  # pg_isready answers during the image's init restart; wait for a real query through the published port.
  port="$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')"
  export KONTRA_TEST_PG="postgres://kontra:kontra@127.0.0.1:${port}/kontra?sslmode=disable"
  for _ in $(seq 1 60); do docker exec "$name" psql -U kontra -h 127.0.0.1 -d kontra -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
fi

mapfile -t pkgs < <("$GO" list ./... | grep -v '/test/e2e')
"$GO" test -p 1 -count=1 "$@" "${pkgs[@]}"
