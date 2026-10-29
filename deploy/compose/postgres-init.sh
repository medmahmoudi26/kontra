#!/bin/sh
# Runs once, when the compose Postgres initialises its volume (docker-entrypoint-initdb.d). The gate and Temporal get
# roles of their own, neither of them a superuser: kontra owns the database kontra (the gate's tables are in its
# schema kontra, D37), and temporal may create the two databases temporal-schema makes (temporal, temporal_visibility).
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v kontra_pw="$KONTRA_DB_PASSWORD" -v temporal_pw="$TEMPORAL_DB_PASSWORD" <<'SQL'
create role kontra login password :'kontra_pw';
create database kontra owner kontra;
create role temporal login createdb password :'temporal_pw';
SQL
