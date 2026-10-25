#!/usr/bin/env bash
# Apply every migration up, down, and up again on a scratch Postgres 17, and check the down leaves nothing.
# Uses $KONTRA_TEST_PG (a postgres:// URL to an empty database) when set, otherwise a throwaway container.
set -euo pipefail
cd "$(dirname "$0")/.."

up() { awk '/^-- \+goose Up/{f=1;next} /^-- \+goose Down/{f=0} f' "$1"; }
down() { awk '/^-- \+goose Down/{f=1;next} f' "$1"; }

if [[ -n "${KONTRA_TEST_PG:-}" ]]; then
  psql_() { psql "$KONTRA_TEST_PG" -v ON_ERROR_STOP=1 -q --single-transaction "$@"; }
else
  name="kontra-migrate-check-$$"
  docker run -d --rm --name "$name" -e POSTGRES_USER=kontra -e POSTGRES_PASSWORD=kontra -e POSTGRES_DB=kontra "${KONTRA_TEST_PG_IMAGE:-postgres:17-alpine}" >/dev/null
  trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 60); do docker exec "$name" pg_isready -U kontra -q 2>/dev/null && break; sleep 0.5; done
  sleep 1
  psql_() { docker exec -i "$name" psql -U kontra -d kontra -v ON_ERROR_STOP=1 -q --single-transaction "$@"; }
fi

# Every migration runs in one transaction, as goose runs it; nothing may land outside the kontra schema.
count() { psql_ -At -c "select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where n.nspname = '$1' and c.relkind in ('r','i','S','v')"; }

files=(migrations/*.sql)
for f in "${files[@]}"; do up "$f" | psql_; done
{ cat scripts/migration_tests.sql; echo "rollback;"; } | psql_ -At | grep -q 'migration behaviour: ok' \
  || { echo "migration behaviour tests failed" >&2; { cat scripts/migration_tests.sql; echo "rollback;"; } | psql_ >&2; exit 1; }
after_up=$(count kontra)
[[ "$(count public)" == "0" ]] || { echo "a migration created relations in public" >&2; exit 1; }
for ((i=${#files[@]}-1; i>=0; i--)); do down "${files[$i]}" | psql_; done
after_down=$(count kontra)
for f in "${files[@]}"; do up "$f" | psql_; done
[[ "$after_down" == "0" ]] || { echo "down left $after_down relations behind" >&2; exit 1; }
[[ "$(count kontra)" == "$after_up" ]] || { echo "up after down differs from the first up" >&2; exit 1; }
echo "migrations: ${#files[@]} file(s), $after_up relations, up/down/up clean"
