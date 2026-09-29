#!/bin/sh
# Porter's boot, and the two things it cannot do for itself.
#
# ── 1. IT HAS NO S3 CONFIGURATION ──────────────────────────────────────────────────────────────
#
# `porter serve` takes twelve flags and not one of them is an endpoint, key, secret, path-style or
# SSL switch. Point `--ducklake-data-path` at `s3://…` and it will attach the catalog happily and
# then fail every scan, because DuckDB has no credential for the store the catalog describes.
#
# DuckDB's own answer is a PERSISTENT SECRET — written once to its secret store and loaded
# automatically by every later connection, including the one Porter opens through ADBC. So this
# writes it from the same `KONTRA_S3_*` variables every other service in this stack reads, which
# keeps one set of credentials describing one object store.
#
# `CREATE OR REPLACE` because a container restart must not fail on a secret it wrote last time.
#
# ── 2. IT HAS NO AUTHENTICATION ────────────────────────────────────────────────────────────────
#
# There is no credential on the Flight port. The compose service therefore publishes no host port
# and this is reachable only on the compose network, with the orchestrator as the authenticated
# front door — the posture `victorialogs` already has for the same reason. Nothing here can enforce
# that; it is a property of the compose file, and the header of Dockerfile.porter says so too.
set -eu

: "${KONTRA_S3_ENDPOINT:?KONTRA_S3_ENDPOINT is required — Porter cannot reach the lake without it}"
: "${KONTRA_S3_ACCESS_KEY:?KONTRA_S3_ACCESS_KEY is required}"
: "${KONTRA_S3_SECRET_KEY:?KONTRA_S3_SECRET_KEY is required}"
: "${KONTRA_DUCKLAKE_CATALOG_URL:?KONTRA_DUCKLAKE_CATALOG_URL is required (postgres://… form)}"

# The secret wants a bare `host:port`; every other variable in this stack carries the scheme,
# because that is what an HTTP client needs. Strip it here rather than keeping a second variable
# whose only difference from the first is five characters — two spellings of one address is how
# they drift apart.
s3_host="$(printf '%s' "$KONTRA_S3_ENDPOINT" | sed -e 's#^https\?://##' -e 's#/*$##')"
case "$KONTRA_S3_ENDPOINT" in
  https://*) s3_ssl=true ;;
  *)         s3_ssl=false ;;
esac

echo "porter: writing DuckDB S3 secret for ${s3_host} (ssl=${s3_ssl}, path-style)"
duckdb -c "
INSTALL httpfs; LOAD httpfs;
CREATE OR REPLACE PERSISTENT SECRET kontra_lake (
  TYPE S3,
  KEY_ID '${KONTRA_S3_ACCESS_KEY}',
  SECRET '${KONTRA_S3_SECRET_KEY}',
  ENDPOINT '${s3_host}',
  -- PATH STYLE, NOT VIRTUAL HOST. SeaweedFS addresses buckets as a path segment, and the
  -- orchestrator's own s3Setup() sets the same thing for the same reason.
  URL_STYLE 'path',
  USE_SSL ${s3_ssl},
  REGION '${KONTRA_S3_REGION:-us-east-1}'
);
" > /dev/null

exec porter serve \
  --db ":memory:" \
  --ducklake \
  --ducklake-catalog-type postgres \
  --ducklake-catalog-dsn "${KONTRA_DUCKLAKE_CATALOG_URL}" \
  --ducklake-data-path "s3://${KONTRA_S3_BUCKET:-kontra}/" \
  --ducklake-name "${KONTRA_PORTER_LAKE_NAME:-lake}" \
  --port "${KONTRA_PORTER_PORT:-32010}" \
  --status-port "${KONTRA_PORTER_STATUS_PORT:-9091}" \
  "$@"
