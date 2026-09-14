#!/bin/sh
set -eu
# CREATE DATABASE IF NOT EXISTS is not valid in Postgres; ignore "already exists".
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
SELECT 'CREATE DATABASE kontra_ducklake' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'kontra_ducklake')\gexec
SQL
