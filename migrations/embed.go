// Package migrations embeds the gate's Postgres schema.
//
// The SQL files in this directory are the single source: `kontra-gate migrate` applies these exact bytes
// through goose (D28), and scripts/check_migrations.sh applies the same files with psql. Nothing copies
// them. Every table lives in the schema `kontra` (D37); goose's version table is `kontra.goose_db_version`,
// and `kontra-gate migrate` creates the schema before goose runs (internal/migrate).
package migrations

import "embed"

// FS holds every migration, NNNN_name.sql in goose's single-file format (`-- +goose Up` / `-- +goose Down`).
//
//go:embed *.sql
var FS embed.FS
