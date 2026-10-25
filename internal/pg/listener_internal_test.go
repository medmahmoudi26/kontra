package pg

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestParseTaskEvent(t *testing.T) {
	id := uuid.MustParse("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11")
	for payload, ok := range map[string]bool{
		id.String() + ":1":                   true,
		id.String() + ":9001":                true,
		id.String() + ":0":                   false,
		id.String() + ":-1":                  false,
		id.String() + ":x":                   false,
		id.String():                          false,
		"garbage":                            false,
		"":                                   false,
		"0199a3b27c4e7d108f002b9a6e1c0d11:1": false,
	} {
		got, gotOK := parseTaskEvent(payload)
		if gotOK != ok || (ok && got != id) {
			t.Errorf("parseTaskEvent(%q) = %v, %v; want ok=%v", payload, got, gotOK, ok)
		}
	}
}

func newTestListener(t *testing.T) (*Listener, func()) {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	l := NewListener(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() { defer close(stopped); l.dispatch(done) }()
	return l, func() { close(done); <-stopped }
}

func received(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

// Fan-out without a server: a task notification wakes only that task's waiters, a wake never blocks
// (one pending wake per waiter), callbacks run in order on their own goroutine, and a panicking callback
// does not stop the next one.
func TestFanOut(t *testing.T) {
	l, stop := newTestListener(t)
	defer stop()
	a, b := uuid.New(), uuid.New()
	ca, releaseA := l.WaitTask(context.Background(), a)
	ca2, releaseA2 := l.WaitTask(context.Background(), a)
	cb, releaseB := l.WaitTask(context.Background(), b)
	defer releaseB()

	var mu sync.Mutex
	var got []string
	block := make(chan struct{})
	l.OnChannel(ChannelServices, func(p string) {
		if p == "block" {
			<-block
		}
		if p == "panic" {
			panic("boom")
		}
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})

	// A callback that blocks does not delay task wakes.
	l.deliver(&pgconn.Notification{Channel: string(ChannelServices), Payload: "block"})
	for i := range 3 {
		l.deliver(&pgconn.Notification{Channel: string(ChannelTaskEvents), Payload: a.String() + ":" + strconv.Itoa(i+1)})
	}
	if !received(ca, time.Second) || !received(ca2, time.Second) {
		t.Fatal("task a's waiters were not woken")
	}
	if received(ca, 50*time.Millisecond) {
		t.Error("three notifications left more than one pending wake")
	}
	if received(cb, 50*time.Millisecond) {
		t.Error("task b was woken by task a's notification")
	}
	for _, p := range []string{"panic", "default/one", "default/two", "default/three"} {
		l.deliver(&pgconn.Notification{Channel: string(ChannelServices), Payload: p})
	}
	l.deliver(&pgconn.Notification{Channel: string(ChannelRevoke), Payload: "apikey:x"}) // no callback: dropped
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"block", "default/one", "default/two", "default/three"}; !slices.Equal(got, want) {
		t.Errorf("callbacks ran %q, want %q", got, want)
	}

	// Released waiters, and waiters whose context ended, are gone.
	releaseA()
	releaseA() // idempotent
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = l.WaitTask(ctx, a)
	cancel()
	time.Sleep(50 * time.Millisecond)
	releaseA2()
	if n := l.waiterCount(); n != 1 {
		t.Errorf("%d waiters registered, want 1 (b)", n)
	}
	l.deliver(&pgconn.Notification{Channel: string(ChannelTaskEvents), Payload: a.String() + ":9"})
	if received(ca, 50*time.Millisecond) {
		t.Error("a released waiter was woken")
	}
	l.wakeAll()
	if !received(cb, time.Second) {
		t.Error("wakeAll missed task b")
	}
}

func TestNewListenerNamesItself(t *testing.T) {
	cfg, err := ParseConfig("postgres://u@127.0.0.1:1/db?application_name=gate-a")
	if err != nil {
		t.Fatal(err)
	}
	l := NewListener(cfg)
	if got := l.cfg.RuntimeParams["application_name"]; got != "gate-a listener" {
		t.Errorf("application_name = %q", got)
	}
	if cfg.RuntimeParams["application_name"] != "gate-a" {
		t.Error("NewListener changed the caller's config")
	}
	if l.idle != DefaultIdleTimeout || DefaultIdleTimeout != 25*time.Second {
		t.Errorf("idle timeout = %s, want 25s", l.idle)
	}
}

func TestParseConfigBounds(t *testing.T) {
	cfg, err := ParseConfig("postgres://u@db.example.com/k")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"tcp_user_timeout": "20000", "tcp_keepalives_idle": "5", "tcp_keepalives_interval": "5",
		"tcp_keepalives_count": "3", "application_name": "kontra-gate",
	}
	for k, v := range want {
		if cfg.RuntimeParams[k] != v {
			t.Errorf("%s = %q, want %q", k, cfg.RuntimeParams[k], v)
		}
	}
	if cfg.ConnectTimeout != ConnectTimeout || cfg.DialFunc == nil {
		t.Errorf("connect timeout %s, dial func set %v", cfg.ConnectTimeout, cfg.DialFunc != nil)
	}
	cfg, err = ParseConfig("postgres://u@db.example.com/k?tcp_user_timeout=60000&connect_timeout=3&application_name=x")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeParams["tcp_user_timeout"] != "60000" || cfg.ConnectTimeout != 3*time.Second || cfg.RuntimeParams["application_name"] != "x" {
		t.Errorf("URL values must win: %v, %s", cfg.RuntimeParams, cfg.ConnectTimeout)
	}
}
