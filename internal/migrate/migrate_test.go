package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/pgtest"
)

func open(t *testing.T, dsn string) *migrate.Migrator {
	t.Helper()
	m, err := migrate.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	var v string
	if err := c.QueryRow(context.Background(), "show server_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	t.Logf("Postgres %s", v)
	return c
}

func scalar[T any](t *testing.T, c *pgx.Conn, q string, args ...any) T {
	t.Helper()
	var v T
	if err := c.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func relations(t *testing.T, c *pgx.Conn, schema string) int {
	return scalar[int](t, c, `select count(*) from pg_class k join pg_namespace n on n.oid = k.relnamespace
		where n.nspname = $1 and k.relkind in ('r','i','S','v')`, schema)
}

func applied(t *testing.T, m *migrate.Migrator) []int64 {
	t.Helper()
	v, err := m.Applied(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// `migrate up` on an empty database creates schema kontra and kontra.goose_db_version and applies 0001;
// nothing lands in public.
func TestUpOnEmptyDatabase(t *testing.T) {
	dsn := pgtest.Database(t)
	c := connect(t, dsn)
	m := open(t, dsn)
	ctx := context.Background()

	if scalar[bool](t, c, "select exists (select 1 from pg_namespace where nspname = 'kontra')") {
		t.Fatal("a fresh database already has schema kontra")
	}
	res, err := m.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Source.Version != 1 || res[0].Direction != "up" || res[0].Error != nil || res[0].Empty {
		t.Fatalf("Up = %v, want 0001 applied", res)
	}
	if !strings.HasSuffix(res[0].Source.Path, "0001_init.sql") {
		t.Errorf("applied %q", res[0].Source.Path)
	}
	if !scalar[bool](t, c, "select to_regclass('kontra.goose_db_version') is not null") {
		t.Error("kontra.goose_db_version missing")
	}
	for _, table := range []string{"tasks", "task_events", "attempts", "work_items", "services", "api_keys", "audit"} {
		if !scalar[bool](t, c, "select to_regclass($1) is not null", "kontra."+table) {
			t.Errorf("kontra.%s missing after up", table)
		}
	}
	if n := relations(t, c, "public"); n != 0 {
		t.Errorf("%d relations in public, want 0 (D37)", n)
	}
	if got := applied(t, m); len(got) != 1 || got[0] != 1 {
		t.Errorf("applied = %v, want [1]", got)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check after up: %v", err)
	}
	// Up again applies nothing.
	res, err = m.Up(ctx)
	if err != nil || len(res) != 0 {
		t.Errorf("second Up = %v, %v; want nothing applied", res, err)
	}
}

func TestStatus(t *testing.T) {
	dsn := pgtest.Database(t)
	m := open(t, dsn)
	ctx := context.Background()
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].State != goose.StatePending || st[0].Source.Version != 1 {
		t.Fatalf("Status on an empty database = %+v, want 0001 pending", st)
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].State != goose.StateApplied || st[0].AppliedAt.IsZero() || !strings.HasSuffix(st[0].Source.Path, "0001_init.sql") {
		t.Fatalf("Status after up = %+v, want 0001 applied with a time", st[0])
	}
}

// Down then up is clean: down removes every relation of 0001 (the version table stays), Check fails at
// version 0, and up again builds the same schema.
func TestDownThenUp(t *testing.T) {
	dsn := pgtest.Database(t)
	c := connect(t, dsn)
	m := open(t, dsn)
	ctx := context.Background()
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	afterUp := relations(t, c, "kontra")

	res, err := m.Down(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source.Version != 1 || res.Direction != "down" {
		t.Fatalf("Down = %v", res)
	}
	if scalar[bool](t, c, "select to_regclass('kontra.tasks') is not null") {
		t.Error("kontra.tasks survives down")
	}
	// Only goose's table (and its primary key index and identity sequence) remain.
	if !scalar[bool](t, c, "select to_regclass('kontra.goose_db_version') is not null") {
		t.Error("down dropped the version table")
	}
	if n := scalar[int](t, c, `select count(*) from pg_class k join pg_namespace n on n.oid = k.relnamespace
		where n.nspname = 'kontra' and k.relname not like 'goose_db_version%'`); n != 0 {
		t.Errorf("%d relations of 0001 left after down", n)
	}
	if got := applied(t, m); len(got) != 0 {
		t.Errorf("applied after down = %v", got)
	}
	err = m.Check(ctx)
	if !errors.Is(err, migrate.ErrSchemaNotCurrent) || !strings.Contains(err.Error(), "at version 0") {
		t.Errorf("Check at version 0 = %v", err)
	}
	if _, err := m.Down(ctx); !errors.Is(err, goose.ErrNoNextVersion) {
		t.Errorf("Down with nothing applied = %v, want ErrNoNextVersion", err)
	}

	res2, err := m.Up(ctx)
	if err != nil || len(res2) != 1 {
		t.Fatalf("Up after down = %v, %v", res2, err)
	}
	if n := relations(t, c, "kontra"); n != afterUp {
		t.Errorf("relations after up/down/up = %d, after the first up %d", n, afterUp)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check: %v", err)
	}
}

// Two first runs at once: both succeed, exactly one applies 0001, and neither trips over the other's
// CREATE SCHEMA.
func TestConcurrentUpAppliesOnce(t *testing.T) {
	for range 3 {
		dsn := pgtest.Database(t)
		a, b := open(t, dsn), open(t, dsn)
		var wg sync.WaitGroup
		results := make([][]*goose.MigrationResult, 2)
		errs := make([]error, 2)
		for i, m := range []*migrate.Migrator{a, b} {
			wg.Go(func() { results[i], errs[i] = m.Up(context.Background()) })
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("concurrent Up: %v / %v", errs[0], errs[1])
		}
		if n := len(results[0]) + len(results[1]); n != 1 {
			t.Fatalf("concurrent Up applied %d migrations in total (%v, %v), want 1", n, results[0], results[1])
		}
		if got := applied(t, a); len(got) != 1 {
			t.Fatalf("applied = %v", got)
		}
	}
}

// A second `migrate up` waits for the lock held by a running one and then applies nothing it should not.
func TestUpWaitsForTheLock(t *testing.T) {
	dsn := pgtest.Database(t)
	holder := connect(t, dsn)
	ctx := context.Background()
	if _, err := holder.Exec(ctx, "select pg_advisory_lock($1)", migrate.LockID); err != nil {
		t.Fatal(err)
	}
	m := open(t, dsn)
	type result struct {
		res []*goose.MigrationResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := m.Up(ctx)
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("Up finished while another migrator held the lock: %v, %v", r.res, r.err)
	case <-time.After(2 * time.Second):
	}
	if scalar[bool](t, holder, "select to_regclass('kontra.tasks') is not null") {
		t.Fatal("0001 applied while the lock was held")
	}
	if _, err := holder.Exec(ctx, "select pg_advisory_unlock($1)", migrate.LockID); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || len(r.res) != 1 {
			t.Fatalf("Up after the lock was released = %v, %v", r.res, r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Up did not finish within 15 s of the lock's release")
	}
}

// Check fails at version 0 and passes at 1; it is read-only, and it refuses a database ahead of the
// binary.
func TestCheck(t *testing.T) {
	dsn := pgtest.Database(t)
	c := connect(t, dsn)
	m := open(t, dsn)
	ctx := context.Background()

	err := m.Check(ctx)
	if !errors.Is(err, migrate.ErrSchemaNotCurrent) || !strings.Contains(err.Error(), "at version 0") || !strings.Contains(err.Error(), "migrate up") {
		t.Errorf("Check on an empty database = %v", err)
	}
	if scalar[bool](t, c, "select exists (select 1 from pg_namespace where nspname = 'kontra')") {
		t.Error("Check created schema kontra")
	}
	if m.Latest() != 1 {
		t.Errorf("Latest = %d, want 1", m.Latest())
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check at version 1 = %v", err)
	}
	if _, err := c.Exec(ctx, "insert into kontra.goose_db_version (version_id, is_applied) values (2, true)"); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(ctx); !errors.Is(err, migrate.ErrSchemaNotCurrent) || !strings.Contains(err.Error(), "does not know") {
		t.Errorf("Check on a database ahead of the gate = %v", err)
	}
}

// New over a caller's *sql.DB leaves the pool to the caller.
func TestNewDoesNotOwnTheDB(t *testing.T) {
	dsn := pgtest.Database(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := migrate.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Errorf("Close closed the caller's pool: %v", err)
	}
}
