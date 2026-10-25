// Package migrate applies the embedded schema with goose (D28).
//
// goose creates its version table before it runs any migration and never creates a schema, so Up, Down
// and Status first run CREATE SCHEMA IF NOT EXISTS kontra on its own statement, and the provider's
// version table is always the schema-qualified kontra.goose_db_version (an unqualified name breaks under
// the migration's own `set local search_path`, pg-store brief C1/G3). One Postgres advisory lock,
// LockID, serializes concurrent `migrate` runs: goose holds it as a session lock while it migrates, and
// the schema bootstrap takes it as a transaction lock (CREATE SCHEMA IF NOT EXISTS is not safe against a
// concurrent CREATE SCHEMA), so a second `migrate up` waits for the first and then applies nothing.
//
// Check, which `serve` runs, is read-only: it never creates the schema or the version table.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/medmahmoudi26/kontra/internal/pg"
	"github.com/medmahmoudi26/kontra/migrations"
)

// Schema is the gate's Postgres schema (D37); VersionTable is goose's table inside it.
const (
	Schema       = "kontra"
	VersionTable = "kontra.goose_db_version"
)

// LockID is the advisory lock key of `kontra-gate migrate` ("kontra" in ASCII, then 0001): the gate's own,
// so another application's goose on the same database neither waits for it nor holds it.
const LockID int64 = 0x6b6f6e7472610001

// lockWait is how long a migrator waits for another one: polled every second for 15 minutes.
const (
	lockPollSeconds = 1
	lockPolls       = 900
)

// ErrSchemaNotCurrent is wrapped by Check's error when the database is not at the embedded version.
var ErrSchemaNotCurrent = errors.New("database schema is not at this gate's version")

// Migrator applies migrations.FS to one database.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
	versions []int64 // embedded, ascending
	ownsDB   bool
}

// New returns a migrator over migrations.FS. db must be a session-mode connection pool (no PgBouncer
// transaction pooling: the advisory lock is a session lock). New touches no table; ctx is unused and kept
// for the call sites' symmetry.
func New(ctx context.Context, db *sql.DB) (*Migrator, error) {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(LockID), lock.WithLockTimeout(lockPollSeconds, lockPolls))
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithTableName(VersionTable),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	m := &Migrator{db: db, provider: p}
	for _, s := range p.ListSources() {
		m.versions = append(m.versions, s.Version)
	}
	slices.Sort(m.versions)
	return m, nil
}

// Open connects to a postgres:// URL (with pg's connection bounds) and returns a migrator that owns the
// connection pool; Close closes it.
func Open(ctx context.Context, dsn string) (*Migrator, error) {
	cfg, err := pg.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	m, err := New(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	m.ownsDB = true
	return m, nil
}

// Close closes the connection pool if Open created it.
func (m *Migrator) Close() error {
	if m.ownsDB {
		return m.db.Close()
	}
	return nil
}

// Latest is the newest embedded version.
func (m *Migrator) Latest() int64 {
	if len(m.versions) == 0 {
		return 0
	}
	return m.versions[len(m.versions)-1]
}

// ensureSchema creates the schema under the migration lock, so two first runs cannot race on it.
func (m *Migrator) ensureSchema(ctx context.Context) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx's rollback is a no-op
	if _, err := tx.ExecContext(ctx, "select pg_advisory_xact_lock($1)", LockID); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "create schema if not exists "+Schema); err != nil {
		return fmt.Errorf("migrate: create schema %s: %w", Schema, err)
	}
	return tx.Commit()
}

// Up applies every pending migration, each in its own transaction. Nothing pending returns no results.
func (m *Migrator) Up(ctx context.Context) ([]*goose.MigrationResult, error) {
	if err := m.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return m.provider.Up(ctx)
}

// Down rolls back the latest applied migration. The schema itself stays: it holds the version table.
// goose.ErrNoNextVersion means nothing was applied.
func (m *Migrator) Down(ctx context.Context) (*goose.MigrationResult, error) {
	if err := m.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return m.provider.Down(ctx)
}

// Status lists every migration and whether it is applied.
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	if err := m.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return m.provider.Status(ctx)
}

// Applied returns the applied versions, ascending, read the way goose reads them (the latest row per
// version decides). It is read-only: a database without the version table has none.
func (m *Migrator) Applied(ctx context.Context) ([]int64, error) {
	var exists bool
	if err := m.db.QueryRowContext(ctx, "select to_regclass($1) is not null", VersionTable).Scan(&exists); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if !exists {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, "select version_id, is_applied from "+VersionTable+" order by id desc")
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var applied []int64
	for rows.Next() {
		var v int64
		var ok bool
		if err := rows.Scan(&v, &ok); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		if ok && v > 0 {
			applied = append(applied, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	slices.Sort(applied)
	return applied, nil
}

// Check is what `serve` runs at start: an error wrapping ErrSchemaNotCurrent unless the database has
// exactly the embedded migrations applied. The gate never migrates implicitly; `kontra-gate migrate` is
// an explicit step. A database ahead of this binary (a newer gate migrated it) is refused too.
func (m *Migrator) Check(ctx context.Context) error {
	applied, err := m.Applied(ctx)
	if err != nil {
		return err
	}
	if slices.Equal(applied, m.versions) {
		return nil
	}
	var unknown, pending []int64
	for _, v := range applied {
		if !slices.Contains(m.versions, v) {
			unknown = append(unknown, v)
		}
	}
	for _, v := range m.versions {
		if !slices.Contains(applied, v) {
			pending = append(pending, v)
		}
	}
	current := int64(0)
	if len(applied) > 0 {
		current = applied[len(applied)-1]
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%w: it has migration(s) %v that this gate (version %d) does not know: run a newer gate",
			ErrSchemaNotCurrent, unknown, m.Latest())
	}
	return fmt.Errorf("%w: it is at version %d, this gate needs %d (pending %v): run kontra-gate migrate up",
		ErrSchemaNotCurrent, current, m.Latest(), pending)
}
