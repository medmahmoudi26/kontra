package pg_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
)

func pool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pg.NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// The pool asks the server for the connection bounds and reports itself; values in the URL win.
func TestPoolBounds(t *testing.T) {
	dsn := pgtest.Database(t)
	p := pool(t, dsn)
	ctx := context.Background()
	for setting, want := range map[string]string{
		// The server's show hooks for the tcp_* settings read the socket back, in the unit's raw integer.
		"tcp_user_timeout": "20000", "tcp_keepalives_idle": "5", "tcp_keepalives_interval": "5",
		"tcp_keepalives_count": "3", "application_name": "kontra-gate",
	} {
		var got string
		if err := p.QueryRow(ctx, "select current_setting($1)", setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("server %s = %q, want %q", setting, got, want)
		}
	}
	if got := p.Config().MaxConns; got != pg.DefaultMaxConns {
		t.Errorf("MaxConns = %d, want %d", got, pg.DefaultMaxConns)
	}
	checkClientSocket(t, p)

	q := "?"
	if strings.Contains(dsn, "?") {
		q = "&"
	}
	p2 := pool(t, dsn+q+"pool_max_conns=3&tcp_user_timeout=30000")
	var got string
	if err := p2.QueryRow(ctx, "show tcp_user_timeout").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "30000" || p2.Config().MaxConns != 3 {
		t.Errorf("URL overrides: tcp_user_timeout %q, MaxConns %d", got, p2.Config().MaxConns)
	}
}

// pool_* parameters configure the pool, never the server: every connection built from the URL (the
// pool, the LISTEN connection, migrate) leaves them out of the startup message, which the server would
// refuse with FATAL: unrecognized configuration parameter.
func TestPoolParamsNotSentToServer(t *testing.T) {
	dsn := pgtest.Database(t)
	q := "?"
	if strings.Contains(dsn, "?") {
		q = "&"
	}
	dsn += q + "pool_max_conns=7&pool_min_conns=0&pool_max_conn_lifetime=1h&pool_health_check_period=1m"
	cfg, err := pg.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	for k := range cfg.RuntimeParams {
		if strings.HasPrefix(k, "pool_") {
			t.Errorf("ParseConfig keeps %s=%s as a startup parameter", k, cfg.RuntimeParams[k])
		}
	}
	if p := pool(t, dsn); p.Config().MaxConns != 7 {
		t.Errorf("NewPool MaxConns = %d, want 7", p.Config().MaxConns)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if m, err := migrate.Open(ctx, dsn); err != nil {
		t.Errorf("migrate.Open with pool_max_conns in the URL: %v", err)
	} else {
		if _, err := m.Up(ctx); err != nil {
			t.Errorf("migrate up with pool_max_conns in the URL: %v", err)
		}
		m.Close()
	}

	l := pg.NewListener(cfg, pg.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	lctx, lcancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(lctx) }()
	defer func() {
		lcancel()
		<-done
	}()
	waitFor(t, 10*time.Second, "the listener to come up with pool_max_conns in the URL", l.Healthy)
}

// pool_max_conns counts only as a parameter: the same text elsewhere in the URL (here the password)
// leaves the default pool size.
func TestPoolMaxConnsDetectedAsParameter(t *testing.T) {
	p, err := pg.NewPool(context.Background(), "postgres://u:pool_max_conns@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Config().MaxConns; got != pg.DefaultMaxConns {
		t.Errorf("MaxConns = %d, want %d", got, pg.DefaultMaxConns)
	}
	for _, bad := range []string{"pool_max_conns=many", "pool_max_conn=3"} {
		if _, err := pg.ParseConfig("postgres://u@127.0.0.1:1/db?" + bad); err == nil {
			t.Errorf("ParseConfig accepts %s", bad)
		}
	}
}

type harness struct {
	l          *pg.Listener
	pool       *pgxpool.Pool
	reconnects atomic.Int32
	runErr     chan error
	cancel     context.CancelFunc
}

// start runs a listener on a fresh database and waits until it is up and its first OnReconnect ran.
func start(t *testing.T, opts ...pg.ListenerOption) *harness {
	t.Helper()
	dsn := pgtest.Database(t)
	cfg, err := pg.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{pool: pool(t, dsn), runErr: make(chan error, 1)}
	h.l = pg.NewListener(cfg, append([]pg.ListenerOption{pg.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)...)
	h.l.OnReconnect(func() { h.reconnects.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.runErr <- h.l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.runErr:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	waitFor(t, 10*time.Second, "the listener to come up", func() bool { return h.l.Healthy() && h.reconnects.Load() == 1 })
	return h
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) wait(t *testing.T, id uuid.UUID) <-chan struct{} {
	t.Helper()
	ch, release := h.l.WaitTask(context.Background(), id)
	t.Cleanup(release)
	return ch
}

func (h *harness) notify(t *testing.T, ch pg.Channel, payload string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), "select pg_notify($1, $2)", string(ch), payload); err != nil {
		t.Fatal(err)
	}
}

func woken(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

// A notification arrives only after its transaction commits, and never for a rolled-back one.
func TestNotificationOnlyAfterCommit(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	id := uuid.New()
	ch := h.wait(t, id)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "select pg_notify($1, $2)", string(pg.ChannelTaskEvents), id.String()+":1"); err != nil {
		t.Fatal(err)
	}
	if woken(ch, 700*time.Millisecond) {
		t.Fatal("woken before the transaction committed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !woken(ch, 3*time.Second) {
		t.Fatal("not woken after commit")
	}

	tx, err = h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "select pg_notify($1, $2)", string(pg.ChannelTaskEvents), id.String()+":2"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if woken(ch, 1500*time.Millisecond) {
		t.Fatal("woken by a rolled-back transaction")
	}
}

// WaitTask wakes only for its own task; malformed payloads wake nobody; a released waiter is not woken.
func TestWaitTaskWakesOnlyItsTask(t *testing.T) {
	h := start(t)
	a, b := uuid.New(), uuid.New()
	ca, cb := h.wait(t, a), h.wait(t, b)

	h.notify(t, pg.ChannelTaskEvents, a.String()+":1")
	if !woken(ca, 3*time.Second) {
		t.Fatal("a not woken")
	}
	if woken(cb, 700*time.Millisecond) {
		t.Fatal("b woken by a's event")
	}
	for _, p := range []string{"garbage", b.String(), b.String() + ":0", b.String() + ":x", ":1"} {
		h.notify(t, pg.ChannelTaskEvents, p)
	}
	h.notify(t, pg.ChannelServices, b.String()+":1") // another channel
	if woken(cb, 700*time.Millisecond) {
		t.Fatal("b woken by a malformed payload or another channel")
	}
	h.notify(t, pg.ChannelTaskEvents, b.String()+":2")
	if !woken(cb, 3*time.Second) {
		t.Fatal("b not woken by its own event")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cc, _ := h.l.WaitTask(ctx, a)
	cancel()
	time.Sleep(50 * time.Millisecond)
	h.notify(t, pg.ChannelTaskEvents, a.String()+":2")
	if !woken(ca, 3*time.Second) {
		t.Fatal("a not woken")
	}
	if woken(cc, 300*time.Millisecond) {
		t.Fatal("a waiter whose context ended was woken")
	}
}

func TestOnChannel(t *testing.T) {
	h := start(t)
	got := make(chan string, 16)
	h.l.OnChannel(pg.ChannelServices, func(p string) { got <- "services " + p })
	h.l.OnChannel(pg.ChannelRevoke, func(p string) { got <- "revoke " + p })
	h.notify(t, pg.ChannelServices, "default/hello")
	h.notify(t, pg.ChannelRevoke, "apikey:0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11")
	for _, want := range []string{"services default/hello", "revoke apikey:0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11"} {
		select {
		case g := <-got:
			if g != want {
				t.Errorf("got %q, want %q", g, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %q", want)
		}
	}
}

// pg_terminate_backend on the LISTEN backend: the listener reconnects within 5 s, wakes every waiter,
// runs OnReconnect, and listens again.
func TestReconnectAfterTerminate(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	ca, cb := h.wait(t, a), h.wait(t, b)
	pid := h.l.BackendPID()
	if pid == 0 {
		t.Fatal("no backend pid while healthy")
	}
	var app string
	if err := h.pool.QueryRow(ctx, "select application_name from pg_stat_activity where pid = $1", pid).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if app != "kontra-gate listener" {
		t.Errorf("listener application_name = %q", app)
	}

	began := time.Now()
	var ok bool
	if err := h.pool.QueryRow(ctx, "select pg_terminate_backend($1)", pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("pg_terminate_backend: %v %v", ok, err)
	}
	if !woken(ca, 5*time.Second) || !woken(cb, 5*time.Second) {
		t.Fatal("a reconnect did not wake every waiter within 5 s")
	}
	waitFor(t, 5*time.Second-time.Since(began), "OnReconnect after the reconnect", func() bool { return h.reconnects.Load() == 2 })
	t.Logf("reconnected and woke every waiter in %s", time.Since(began).Round(time.Millisecond))
	if !h.l.Healthy() || h.l.BackendPID() == pid || h.l.BackendPID() == 0 {
		t.Errorf("after reconnect: healthy %v pid %d (was %d)", h.l.Healthy(), h.l.BackendPID(), pid)
	}
	h.notify(t, pg.ChannelTaskEvents, a.String()+":7")
	if !woken(ca, 3*time.Second) {
		t.Fatal("no notification after the reconnect: LISTEN was not issued again")
	}
	if woken(cb, 300*time.Millisecond) {
		t.Fatal("b woken by a's event after the reconnect")
	}
}

// An idle wait times out after about 25 s (the default), runs `select 1` on the LISTEN connection, and
// wakes every waiter.
func TestIdleWaitPingsAndWakes(t *testing.T) {
	h := start(t)
	ch := h.wait(t, uuid.New())
	began := time.Now()
	if !woken(ch, 35*time.Second) {
		t.Fatal("no idle wake within 35 s")
	}
	idle := time.Since(began)
	if idle < 20*time.Second || idle > 32*time.Second {
		t.Errorf("idle wake after %s, want about 25 s", idle)
	}
	var query string
	if err := h.pool.QueryRow(context.Background(), "select query from pg_stat_activity where pid = $1", h.l.BackendPID()).Scan(&query); err != nil {
		t.Fatal(err)
	}
	if query != "select 1" {
		t.Errorf("last statement on the LISTEN connection = %q, want select 1", query)
	}
	t.Logf("idle wake after %s", idle.Round(time.Millisecond))

	// With a short idle timeout the wakes repeat.
	h2 := start(t, pg.WithIdleTimeout(500*time.Millisecond))
	ch2 := h2.wait(t, uuid.New())
	for i := range 3 {
		if !woken(ch2, 3*time.Second) {
			t.Fatalf("idle wake %d missing", i+1)
		}
	}
}

func TestRunTwiceAndStop(t *testing.T) {
	h := start(t)
	if err := h.l.Run(context.Background()); err == nil {
		t.Error("a second Run started")
	}
	h.cancel()
	select {
	case err := <-h.runErr:
		if err != nil {
			t.Errorf("Run after cancel = %v, want nil", err)
		}
		h.runErr <- nil // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if h.l.Healthy() {
		t.Error("healthy after Run returned")
	}
}

// A listener that cannot reach the server keeps retrying and is not healthy.
func TestUnreachable(t *testing.T) {
	cfg, err := pg.ParseConfig("postgres://u@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	l := pg.NewListener(cfg, pg.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v", err)
	}
	if l.Healthy() {
		t.Error("healthy without a server")
	}
}
