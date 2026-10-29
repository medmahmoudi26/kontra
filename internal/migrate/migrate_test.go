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

// `migrate up` on an empty database creates schema kontra and kontra.goose_db_version and applies 0001 and
// 0002, in order; nothing lands in public.
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
	if len(res) != 2 {
		t.Fatalf("Up = %v, want 0001 and 0002 applied", res)
	}
	for i, name := range []string{"0001_init.sql", "0002_m3.sql"} {
		r := res[i]
		if r.Source.Version != int64(i+1) || r.Direction != "up" || r.Error != nil || r.Empty || !strings.HasSuffix(r.Source.Path, name) {
			t.Errorf("Up[%d] = %v, want %s applied", i, r, name)
		}
	}
	if !scalar[bool](t, c, "select to_regclass('kontra.goose_db_version') is not null") {
		t.Error("kontra.goose_db_version missing")
	}
	for _, table := range []string{"tasks", "task_events", "attempts", "work_items", "services", "api_keys", "audit", "blobs"} {
		if !scalar[bool](t, c, "select to_regclass($1) is not null", "kontra."+table) {
			t.Errorf("kontra.%s missing after up", table)
		}
	}
	if n := relations(t, c, "public"); n != 0 {
		t.Errorf("%d relations in public, want 0 (D37)", n)
	}
	if got := applied(t, m); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("applied = %v, want [1 2]", got)
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
	if len(st) != 2 || st[0].State != goose.StatePending || st[0].Source.Version != 1 || st[1].State != goose.StatePending || st[1].Source.Version != 2 {
		t.Fatalf("Status on an empty database = %+v, want 0001 and 0002 pending", st)
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"0001_init.sql", "0002_m3.sql"} {
		if len(st) != 2 || st[i].State != goose.StateApplied || st[i].AppliedAt.IsZero() || !strings.HasSuffix(st[i].Source.Path, name) {
			t.Fatalf("Status after up = %+v, want %s applied with a time", st, name)
		}
	}
}

// Down then up is clean: the first down removes 0002's relations and column (Check then fails at version 1),
// the second every relation of 0001 (the version table stays; Check fails at version 0), and up again builds
// the same schema.
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
	if res.Source.Version != 2 || res.Direction != "down" {
		t.Fatalf("first Down = %v", res)
	}
	if scalar[bool](t, c, "select to_regclass('kontra.blobs') is not null") || !scalar[bool](t, c, "select to_regclass('kontra.tasks') is not null") {
		t.Error("the first down did not undo exactly 0002")
	}
	if scalar[bool](t, c, `select exists (select 1 from information_schema.columns
		where table_schema = 'kontra' and table_name = 'gate_replicas' and column_name = 'peer_addr')`) {
		t.Error("gate_replicas.peer_addr survives 0002's down")
	}
	if scalar[bool](t, c, "select to_regclass('kontra.mcp_sessions_virtual') is not null") {
		t.Error("mcp_sessions_virtual survives 0002's down")
	}
	if err := m.Check(ctx); !errors.Is(err, migrate.ErrSchemaNotCurrent) || !strings.Contains(err.Error(), "at version 1") {
		t.Errorf("Check at version 1 = %v", err)
	}
	if res, err = m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if res.Source.Version != 1 || res.Direction != "down" {
		t.Fatalf("second Down = %v", res)
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
	if err != nil || len(res2) != 2 {
		t.Fatalf("Up after down = %v, %v", res2, err)
	}
	if n := relations(t, c, "kontra"); n != afterUp {
		t.Errorf("relations after up/down/up = %d, after the first up %d", n, afterUp)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check: %v", err)
	}
}

// Two first runs at once: both succeed, each migration is applied exactly once, and neither trips over the
// other's CREATE SCHEMA.
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
		if n := len(results[0]) + len(results[1]); n != 2 {
			t.Fatalf("concurrent Up applied %d migrations in total (%v, %v), want 2", n, results[0], results[1])
		}
		if got := applied(t, a); len(got) != 2 {
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
		if r.err != nil || len(r.res) != 2 {
			t.Fatalf("Up after the lock was released = %v, %v", r.res, r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Up did not finish within 15 s of the lock's release")
	}
}

// Check fails at version 0 and passes at 2, the latest; it is read-only, and it refuses a database ahead of
// the binary.
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
	if m.Latest() != 2 {
		t.Errorf("Latest = %d, want 2", m.Latest())
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check at version 2 = %v", err)
	}
	if _, err := c.Exec(ctx, "insert into kontra.goose_db_version (version_id, is_applied) values (3, true)"); err != nil {
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
