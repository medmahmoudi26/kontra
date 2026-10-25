// Package pgtest gives integration tests a fresh database, empty or migrated.
//
// KONTRA_TEST_PG names one Postgres server (16 or 17) shared by every package: each call creates its own
// database on it, applies migrations.FS through internal/migrate when asked, and drops the database at
// cleanup. The harness never starts a container itself, so `go test -p 1 ./...` against one server is the
// only way integration tests run (the machine budget allows one Postgres at a time; `make
// test-integration` starts one when the variable is unset). Under -short every caller is skipped; without
// -short a missing KONTRA_TEST_PG fails the test rather than skipping it, so a run that tested nothing
// cannot read as a pass.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/migrate"
	"github.com/medmahmoudi26/kontra/internal/pg"
)

// Env is the variable naming the test server, a postgres:// URL with rights to create databases.
const Env = "KONTRA_TEST_PG"

// Prefix starts the name of every database pgtest creates.
const Prefix = "kontra_test_"

// Server returns KONTRA_TEST_PG, skipping under -short and failing when it is unset.
func Server(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs " + Env + "; skipped under -short")
	}
	dsn := os.Getenv(Env)
	if dsn == "" {
		t.Fatal(Env + " is not set: integration tests need a Postgres 16 or 17 server (make test-integration starts one)")
	}
	return dsn
}

// Database creates an empty database on the test server and returns its postgres:// URL. The database
// is dropped (with its connections) when the test ends.
func Database(t testing.TB) string {
	t.Helper()
	server := Server(t)
	u, err := url.Parse(server)
	if err != nil {
		t.Fatalf("%s: %v", Env, err)
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		t.Fatal(err)
	}
	name := Prefix + hex.EncodeToString(r[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, server)
	if err != nil {
		t.Fatalf("connect to %s: %v", Env, err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, server)
		if err != nil {
			t.Errorf("drop database %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	u.Path = "/" + name
	return u.String()
}

// New returns a pool on a fresh database with the schema applied. The pool is closed before the
// database is dropped.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := Database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := migrate.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Up(ctx); err != nil {
		m.Close()
		t.Fatalf("migrate up: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := pg.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // runs before Database's drop: cleanups run last-registered first
	return pool
}
