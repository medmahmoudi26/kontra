// Package pgtest gives integration tests a fresh, migrated database.
//
// KONTRA_TEST_PG names one Postgres server (16 or 17) shared by every package: each call creates its own
// database on it, applies migrations.FS through internal/migrate, and drops the database at cleanup. The
// harness never starts a container itself, so `go test -p 1 ./...` against one server is the only way
// integration tests run (the machine budget allows one Postgres at a time). Under -short every caller is
// skipped; without -short a missing KONTRA_TEST_PG fails the test rather than skipping it, so a run that
// tested nothing cannot read as a pass.
package pgtest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Env is the variable naming the test server, a postgres:// URL with rights to create databases.
const Env = "KONTRA_TEST_PG"

// New returns a pool on a fresh database with the schema applied.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs " + Env + "; skipped under -short")
	}
	t.Fatal(unimpl.Error("pgtest.New", "WP-02"))
	return nil
}
