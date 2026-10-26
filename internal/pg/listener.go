package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Listener timing (pg-store brief §5.3).
const (
	// DefaultIdleTimeout is how long a wait without notifications lasts before the listener runs
	// `select 1` and wakes every waiter for a safety re-read.
	DefaultIdleTimeout = 25 * time.Second
	// PingTimeout bounds that `select 1` and the LISTEN statements after a (re)connect.
	PingTimeout = 5 * time.Second
	// MinBackoff and MaxBackoff bound the jittered reconnect delay.
	MinBackoff = 100 * time.Millisecond
	MaxBackoff = 5 * time.Second
)

// ListenerOption configures a Listener.
type ListenerOption func(*Listener)

// WithIdleTimeout replaces DefaultIdleTimeout (tests).
func WithIdleTimeout(d time.Duration) ListenerOption { return func(l *Listener) { l.idle = d } }

// WithLogger sets the logger for reconnects (default slog.Default()).
func WithLogger(log *slog.Logger) ListenerOption { return func(l *Listener) { l.log = log } }

// Listener holds the replica's LISTEN connection and fans notifications out without blocking.
//
// Task waiters are woken from the connection's goroutine by a non-blocking send on a channel with a
// buffer of one, so a slow waiter never delays the next notification. OnChannel and OnReconnect
// callbacks run in order on a second goroutine, behind an unbounded queue: a callback that queries
// Postgres delays later callbacks, never the connection.
type Listener struct {
	cfg  *pgx.ConnConfig
	idle time.Duration
	log  *slog.Logger

	running atomic.Bool
	healthy atomic.Bool
	pid     atomic.Uint32

	mu        sync.Mutex
	waiters   map[uuid.UUID]map[*waiter]struct{}
	chanFns   map[Channel][]func(string)
	reconnFns []func()

	qmu   sync.Mutex
	queue []func()
	qwake chan struct{}
}

type waiter struct{ ch chan struct{} }

// NewListener returns a listener for the database cfg names. Run must be called for it to deliver. cfg
// should come from ParseConfig (or a pool's Config().ConnConfig) so the connection bounds apply; the
// listener reports itself in pg_stat_activity as "<application_name> listener".
func NewListener(cfg *pgx.ConnConfig, opts ...ListenerOption) *Listener {
	c := cfg.Copy()
	if c.RuntimeParams == nil {
		c.RuntimeParams = map[string]string{}
	}
	name := c.RuntimeParams["application_name"]
	if name == "" {
		name = ApplicationName
	}
	c.RuntimeParams["application_name"] = name + " listener"
	l := &Listener{
		cfg:     c,
		idle:    DefaultIdleTimeout,
		log:     slog.Default(),
		waiters: map[uuid.UUID]map[*waiter]struct{}{},
		chanFns: map[Channel][]func(string){},
		qwake:   make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Run connects, LISTENs on every Channel and delivers until ctx ends. It reconnects with a jittered
// 100 ms to 5 s backoff, and after every (re)connect wakes every waiter and runs every OnReconnect
// callback, because notifications sent during the gap are lost. A wait without notifications times out
// after about 25 s, runs `select 1`, and wakes every waiter for a safety re-read. Run returns nil when
// ctx ends, after the callback queue has drained; it is an error to run a Listener twice at once.
func (l *Listener) Run(ctx context.Context) error {
	if !l.running.CompareAndSwap(false, true) {
		return errors.New("pg: Listener.Run is already running")
	}
	defer l.running.Store(false)

	done := make(chan struct{})
	dispatcherDone := make(chan struct{})
	go func() {
		defer close(dispatcherDone)
		l.dispatch(done)
	}()
	defer func() {
		close(done)
		<-dispatcherDone
	}()

	backoff := MinBackoff
	for {
		conn, err := l.connect(ctx)
		if err == nil {
			backoff = MinBackoff
			l.connected()
			err = l.serve(ctx, conn)
			l.healthy.Store(false)
			l.pid.Store(0)
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
		if ctx.Err() != nil {
			return nil
		}
		delay := backoff/2 + rand.N(backoff/2+1)
		l.log.Warn("postgres LISTEN connection down; reconnecting", "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		backoff = min(backoff*2, MaxBackoff)
	}
}

// connect opens the dedicated connection and LISTENs on every channel.
func (l *Listener) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, l.cfg)
	if err != nil {
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, PingTimeout)
	defer cancel()
	for _, ch := range Channels {
		if _, err := conn.Exec(lctx, "listen "+pgx.Identifier{string(ch)}.Sanitize()); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("listen %s: %w", ch, err)
		}
	}
	return conn, nil
}

// connected marks the listener up, wakes every waiter and queues the OnReconnect callbacks.
func (l *Listener) connected() {
	l.healthy.Store(true)
	l.wakeAll()
	l.mu.Lock()
	fns := append([]func(){}, l.reconnFns...)
	l.mu.Unlock()
	for _, fn := range fns {
		l.enqueue(fn)
	}
}

// serve waits for notifications until the connection fails or ctx ends.
func (l *Listener) serve(ctx context.Context, conn *pgx.Conn) error {
	l.pid.Store(conn.PgConn().PID())
	for {
		wctx, cancel := context.WithTimeout(ctx, l.idle)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			l.deliver(n)
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded) && !conn.IsClosed():
			// An idle wait: prove the connection is alive, then let every waiter re-read in case a
			// notification was lost without the connection noticing.
			pctx, pcancel := context.WithTimeout(ctx, PingTimeout)
			_, perr := conn.Exec(pctx, "select 1")
			pcancel()
			if perr != nil {
				return fmt.Errorf("idle check: %w", perr)
			}
			l.wakeAll()
		default:
			return err
		}
	}
}

func (l *Listener) deliver(n *pgconn.Notification) {
	ch := Channel(n.Channel)
	if ch == ChannelTaskEvents {
		if id, ok := parseTaskEvent(n.Payload); ok {
			l.wakeTask(id)
		}
	}
	l.mu.Lock()
	fns := l.chanFns[ch]
	l.mu.Unlock()
	payload := n.Payload
	for _, fn := range fns {
		l.enqueue(func() { fn(payload) })
	}
}

// parseTaskEvent reads the task id of a "<task_id>:<seq>" payload; anything else is ignored (the table
// is the truth, and every waiter re-reads within the idle timeout anyway).
func parseTaskEvent(payload string) (uuid.UUID, bool) {
	id, seq, ok := strings.Cut(payload, ":")
	if !ok {
		return uuid.Nil, false
	}
	u, err := uuid.Parse(id)
	if err != nil || len(id) != 36 {
		return uuid.Nil, false
	}
	if n, err := strconv.ParseInt(seq, 10, 64); err != nil || n < 1 {
		return uuid.Nil, false
	}
	return u, true
}

func (w *waiter) wake() {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

func (l *Listener) wakeTask(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for w := range l.waiters[id] {
		w.wake()
	}
}

func (l *Listener) wakeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ws := range l.waiters {
		for w := range ws {
			w.wake()
		}
	}
}

func (l *Listener) enqueue(fn func()) {
	l.qmu.Lock()
	l.queue = append(l.queue, fn)
	l.qmu.Unlock()
	select {
	case l.qwake <- struct{}{}:
	default:
	}
}

// dispatch runs queued callbacks in order until done is closed and the queue is empty.
func (l *Listener) dispatch(done <-chan struct{}) {
	for {
		l.qmu.Lock()
		q := l.queue
		l.queue = nil
		l.qmu.Unlock()
		for _, fn := range q {
			l.call(fn)
		}
		if len(q) > 0 {
			continue
		}
		select {
		case <-l.qwake:
		case <-done:
			l.qmu.Lock()
			empty := len(l.queue) == 0
			l.qmu.Unlock()
			if empty {
				return
			}
		}
	}
}

func (l *Listener) call(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			l.log.Error("pg listener callback panicked", "panic", r)
		}
	}()
	fn()
}

// WaitTask returns a channel that receives when a kontra_task_events notification for taskID arrives or
// the listener reconnects or idles, and a func that releases the registration (idempotent; it also
// runs when ctx ends). Waiters re-read task_events: the notification only says "look again". The
// channel holds at most one pending wake and is never closed.
func (l *Listener) WaitTask(ctx context.Context, taskID uuid.UUID) (<-chan struct{}, func()) {
	w := &waiter{ch: make(chan struct{}, 1)}
	l.mu.Lock()
	set := l.waiters[taskID]
	if set == nil {
		set = map[*waiter]struct{}{}
		l.waiters[taskID] = set
	}
	set[w] = struct{}{}
	l.mu.Unlock()

	var once sync.Once
	remove := func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if set := l.waiters[taskID]; set != nil {
				delete(set, w)
				if len(set) == 0 {
					delete(l.waiters, taskID)
				}
			}
		})
	}
	stop := context.AfterFunc(ctx, remove)
	return w.ch, func() {
		stop()
		remove()
	}
}

// waiterCount is the number of registered task waiters (tests).
func (l *Listener) waiterCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ws := range l.waiters {
		n += len(ws)
	}
	return n
}

// OnChannel registers fn for every notification on ch (services and revocations). fn runs on the
// listener's callback goroutine, in notification order.
func (l *Listener) OnChannel(ch Channel, fn func(payload string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chanFns[ch] = append(l.chanFns[ch], fn)
}

// OnReconnect registers fn to run after every (re)connect, e.g. the revocation cache's catch-up read.
// The first connect counts: fn runs once the listener is first up.
func (l *Listener) OnReconnect(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reconnFns = append(l.reconnFns, fn)
}

// Healthy reports whether the LISTEN connection is up (part of /healthz, spec §2.1).
func (l *Listener) Healthy() bool {
	return l.healthy.Load()
}

// BackendPID is the server process id of the LISTEN connection, 0 while it is down.
func (l *Listener) BackendPID() uint32 {
	return l.pid.Load()
}
