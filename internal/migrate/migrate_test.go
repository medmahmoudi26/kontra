package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/migrations"
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

// `migrate up` on an empty database creates schema kontra and kontra.goose_db_version and applies 0001, 0002,
// 0003 and 0004, in order; nothing lands in public.
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
	if len(res) != 4 {
		t.Fatalf("Up = %v, want 0001 to 0004 applied", res)
	}
	for i, name := range []string{"0001_init.sql", "0002_m3.sql", "0003_m4.sql", "0004_m5.sql"} {
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
	if got := applied(t, m); len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 3 || got[3] != 4 {
		t.Errorf("applied = %v, want [1 2 3 4]", got)
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
	if len(st) != 4 {
		t.Fatalf("Status on an empty database = %+v, want 0001 to 0004 pending", st)
	}
	for i, s := range st {
		if s.State != goose.StatePending || s.Source.Version != int64(i+1) {
			t.Fatalf("Status on an empty database = %+v, want 0001 to 0004 pending", st)
		}
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"0001_init.sql", "0002_m3.sql", "0003_m4.sql", "0004_m5.sql"} {
		if len(st) != 4 || st[i].State != goose.StateApplied || st[i].AppliedAt.IsZero() || !strings.HasSuffix(st[i].Source.Path, name) {
			t.Fatalf("Status after up = %+v, want %s applied with a time", st, name)
		}
	}
}

// Down then up is clean: the first down removes 0004's function and index (Check then fails at version 3), the second
// 0003's columns, indexes and checks (version 2), the third 0002's relations and column (version 1), the fourth every
// relation of 0001 (the version table stays; Check fails at version 0), and up again builds the same schema.
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
	if res.Source.Version != 4 || res.Direction != "down" {
		t.Fatalf("first Down = %v", res)
	}
	if scalar[bool](t, c, "select to_regprocedure('kontra.audit_append(bytea)') is not null") ||
		scalar[bool](t, c, "select to_regclass('kontra.audit_prev_hash') is not null") {
		t.Error("audit_append or audit_prev_hash survives 0004's down")
	}
	if !scalar[bool](t, c, "select to_regclass('kontra.tasks_hired') is not null") {
		t.Error("the first down undid more than 0004")
	}
	if err := m.Check(ctx); !errors.Is(err, migrate.ErrSchemaNotCurrent) || !strings.Contains(err.Error(), "at version 3") {
		t.Errorf("Check at version 3 = %v", err)
	}

	if res, err = m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if res.Source.Version != 3 || res.Direction != "down" {
		t.Fatalf("second Down = %v", res)
	}
	for _, col := range []string{"hired", "elicitation_notified_at"} {
		if scalar[bool](t, c, `select exists (select 1 from information_schema.columns
			where table_schema = 'kontra' and table_name = 'tasks' and column_name = $1)`, col) {
			t.Errorf("tasks.%s survives 0003's down", col)
		}
	}
	for _, idx := range []string{"tasks_hired", "tenants_anonymous_network"} {
		if scalar[bool](t, c, "select to_regclass($1) is not null", "kontra."+idx) {
			t.Errorf("%s survives 0003's down", idx)
		}
	}
	if scalar[bool](t, c, `select exists (select 1 from pg_constraint where conname in ('anonymous_slug', 'elicitation_notified_has_id'))`) {
		t.Error("a check of 0003 survives its down")
	}
	if !scalar[bool](t, c, "select to_regclass('kontra.blobs') is not null") {
		t.Error("the second down undid more than 0003")
	}
	if err := m.Check(ctx); !errors.Is(err, migrate.ErrSchemaNotCurrent) || !strings.Contains(err.Error(), "at version 2") {
		t.Errorf("Check at version 2 = %v", err)
	}

	if res, err = m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if res.Source.Version != 2 || res.Direction != "down" {
		t.Fatalf("third Down = %v", res)
	}
	if scalar[bool](t, c, "select to_regclass('kontra.blobs') is not null") || !scalar[bool](t, c, "select to_regclass('kontra.tasks') is not null") {
		t.Error("the third down did not undo exactly 0002")
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
		t.Fatalf("fourth Down = %v", res)
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
	if err != nil || len(res2) != 4 {
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
		if n := len(results[0]) + len(results[1]); n != 4 {
			t.Fatalf("concurrent Up applied %d migrations in total (%v, %v), want 4", n, results[0], results[1])
		}
		if got := applied(t, a); len(got) != 4 {
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
		if r.err != nil || len(r.res) != 4 {
			t.Fatalf("Up after the lock was released = %v, %v", r.res, r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Up did not finish within 15 s of the lock's release")
	}
}

// Check fails at version 0 and passes at 4, the latest; it is read-only, and it refuses a database ahead of
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
	if m.Latest() != 4 {
		t.Errorf("Latest = %d, want 4", m.Latest())
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check at version 4 = %v", err)
	}
	if _, err := c.Exec(ctx, "insert into kontra.goose_db_version (version_id, is_applied) values (5, true)"); err != nil {
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

// Migration 0003 (M4) runs in one transaction whose first ALTER TABLE holds ACCESS EXCLUSIVE on tasks until it
// commits, so each scan of tasks under it is a pause for the replicas still serving. It scans tasks once, to build
// tasks_hired; the check on elicitation_notified_at, added NOT VALID (its column is new, null on every row), costs no
// second one (M4 WP-01's review). Counted with the migration transaction's own statistics.
func TestM4ScansTasksOnce(t *testing.T) {
	dsn := pgtest.Database(t)
	m := open(t, dsn)
	ctx := context.Background()
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 { // 0004, then 0003
		if _, err := m.Down(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := applied(t, m); len(got) != 2 || got[1] != 2 {
		t.Fatalf("applied after two downs = %v, want [1 2]", got)
	}
	b, err := fs.ReadFile(migrations.FS, "0003_m4.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	up, down := strings.Index(s, "-- +goose Up"), strings.Index(s, "-- +goose Down")
	if up < 0 || down < up {
		t.Fatal("0003_m4.sql: no Up section")
	}
	c := connect(t, dsn)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, s[up:down]); err != nil {
		t.Fatalf("0003 up: %v", err)
	}
	var scans int64
	if err := tx.QueryRow(ctx, `select coalesce((select seq_scan from pg_stat_xact_user_tables
		where schemaname = 'kontra' and relname = 'tasks'), 0)`).Scan(&scans); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Errorf("0003 scanned tasks %d times under its lock, want once (the tasks_hired build)", scans)
	}
}
