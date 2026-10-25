// Package migrate applies the embedded schema with goose (D28).
//
// goose creates its version table before it runs any migration and never creates a schema, so New first
// runs CREATE SCHEMA IF NOT EXISTS kontra on its own statement, then builds a provider whose version table
// is always the schema-qualified kontra.goose_db_version (an unqualified name breaks under the migration's
// own `set local search_path`, pg-store brief C1/G3). A Postgres session advisory lock serializes
// concurrent `migrate` runs.
package migrate

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Schema is the gate's Postgres schema (D37); VersionTable is goose's table inside it.
const (
	Schema       = "kontra"
	VersionTable = "kontra.goose_db_version"
)

// Migrator applies migrations.FS to one database.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
}

// New creates the schema if needed and returns a migrator over migrations.FS. db must be a session-mode
// connection pool (no PgBouncer transaction pooling: the advisory lock is a session lock).
func New(ctx context.Context, db *sql.DB) (*Migrator, error) {
	return nil, unimpl.Error("migrate.New", "WP-01")
}

// Up applies every pending migration, each in its own transaction.
func (m *Migrator) Up(ctx context.Context) ([]*goose.MigrationResult, error) {
	return nil, unimpl.Error("migrate.Migrator.Up", "WP-01")
}

// Down rolls back the latest applied migration.
func (m *Migrator) Down(ctx context.Context) (*goose.MigrationResult, error) {
	return nil, unimpl.Error("migrate.Migrator.Down", "WP-01")
}

// Status lists every migration and whether it is applied.
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	return nil, unimpl.Error("migrate.Migrator.Status", "WP-01")
}

// Check is what `serve` runs at start: an error unless the database is at the latest embedded version.
// The gate never migrates implicitly; `kontra-gate migrate` is an explicit step.
func (m *Migrator) Check(ctx context.Context) error {
	return unimpl.Error("migrate.Migrator.Check", "WP-01")
}
