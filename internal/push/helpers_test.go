package push

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
)

var (
	buyerA = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000a"), Principal: "key:kk_a_00000001", Via: store.ViaAPIKey}
	buyerB = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000b"), Principal: "key:kk_b_00000001", Via: store.ViaAPIKey}
	anon   = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-0000000000ff"), Principal: "anonymous", Via: store.ViaAnonymous}
)

// Secrets the credential tests look for in logs, errors, columns and Temporal history.
const (
	secretToken = "tok-5ecret-7f3a91"
	secretCreds = "cr3d-5ecret-b44e02"
	secretPath  = "p4th-5ecret-c0ffee"
)

// env is one fresh migrated database with a tenant and a direct service.
type env struct {
	t    testing.TB
	dsn  string
	pool *pgxpool.Pool
	log  *store.Log
	svc  store.ServiceRef

	clock      sync.Mutex
	now        func() time.Time // time.Now when nil
	lastStatus time.Time        // the timestamp of the last status env.status wrote
}

// statusTime is the timestamp of the next status env.status writes: now, at the millisecond the A2A wire
// (and so a snapshot) keeps (a2a writes .000Z), and at least 1 ms after the previous one. Two statuses
// written within one millisecond would otherwise give byte-identical snapshots, which no delivery can tell
// apart (seqOf).
func (e *env) statusTime() time.Time {
	e.clock.Lock()
	defer e.clock.Unlock()
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	ts := now().UTC().Truncate(time.Millisecond)
	if !ts.After(e.lastStatus) {
		ts = e.lastStatus.Add(time.Millisecond)
	}
	e.lastStatus = ts
	return ts
}

func newEnv(t testing.TB) *env {
	t.Helper()
	dsn := pgtest.Migrated(t)
	e := &env{t: t, dsn: dsn, pool: pgtest.Pool(t, dsn)}
	e.log = store.NewLog(e.pool)
	tenant := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, tenant)
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, timeout_s, retries)
		values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 3600, 2)`, tenant)
	e.svc = store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// newTaskTx pre-creates a submitted task in tx the way the work queue does.
func (e *env) newTaskTx(tx pgx.Tx, o store.Owner) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), "ctx-1"
	now := time.Now().UTC().Truncate(time.Millisecond)
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "ctx-1",
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted, Timestamp: &now}, History: []*a2a.Message{msg}}
	if _, err := e.log.CreateTask(context.Background(), tx, store.NewTask{ID: id, Service: e.svc, Protocol: store.ProtocolA2A,
		Owner: o, ContextID: "ctx-1", State: store.StateSubmitted, Snapshot: snap}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) newTask(o store.Owner) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		id = e.newTaskTx(tx, o)
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// write appends ev the way a2a-go's processor does: the projection after the event (a2a-go's own update
// rules, as the fold applies them) and the event as the row's payload.
func write(ctx context.Context, log *store.Log, id uuid.UUID, kind store.EventKind, state store.State, ev a2a.Event) (int64, error) {
	row, err := log.Load(ctx, id)
	if err != nil {
		return 0, err
	}
	next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
	if err != nil {
		return 0, err
	}
	w := store.Write{TaskID: id, PrevVersion: row.Version, Kind: kind, State: state, Snapshot: next, Payload: payload}
	if kind == store.KindStatus {
		w.StatusAt = store.StatusTime(next.Status.Timestamp)
	}
	a, err := log.Append(ctx, nil, w)
	return a.Seq, err
}

func info(id uuid.UUID) a2a.TaskInfo {
	return a2a.TaskInfo{TaskID: a2a.TaskID(id.String()), ContextID: "ctx-1"}
}

func (e *env) status(id uuid.UUID, s store.State) int64 {
	e.t.Helper()
	ev := a2a.NewStatusUpdateEvent(info(id), s.A2A(), nil)
	now := e.statusTime()
	ev.Status.Timestamp = &now
	seq, err := write(context.Background(), e.log, id, store.KindStatus, s, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

func (e *env) artifact(id uuid.UUID, artifactID a2a.ArtifactID, text string, appendTo bool) int64 {
	e.t.Helper()
	ev := a2a.NewArtifactUpdateEvent(info(id), artifactID, a2a.NewTextPart(text))
	ev.Append = appendTo
	seq, err := write(context.Background(), e.log, id, store.KindArtifact, "", ev)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

func (e *env) userMessage(id uuid.UUID, text string) int64 {
	e.t.Helper()
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.TaskID, m.ContextID = a2a.TaskID(id.String()), "ctx-1"
	seq, err := write(context.Background(), e.log, id, store.KindMessage, "", m)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

func (e *env) metadata(id uuid.UUID, patch map[string]any) int64 {
	e.t.Helper()
	seq, err := e.log.ApplyMetadata(context.Background(), id, patch)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

// expectedBody is what a delivery of the snapshot at seq carries.
func (e *env) expectedBody(id uuid.UUID, seq int64) string {
	e.t.Helper()
	snap, err := e.log.SnapshotAt(context.Background(), id, seq)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := json.Marshal(a2a.StreamResponse{Event: snap})
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) deliveredSeq(id uuid.UUID, configID string) (seq int64, failed bool) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(), `select delivered_seq, failed_at is not null from kontra.push_configs
		where task_id = $1 and id = $2`, id, configID).Scan(&seq, &failed); err != nil {
		e.t.Fatal(err)
	}
	return seq, failed
}

// signals records kontra.push.
type signals struct {
	mu  sync.Mutex
	got []lifecycle.PushSignal
}

func (s *signals) SignalPush(_ context.Context, _ a2a.TaskID, sig lifecycle.PushSignal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sig)
	return nil
}

func (s *signals) list() []lifecycle.PushSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]lifecycle.PushSignal(nil), s.got...)
}

// fakeResolver maps host names to addresses; anything else does not resolve.
type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range f[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

// delivery is one request a receiver got.
type delivery struct {
	at     time.Time
	method string
	path   string
	header http.Header
	body   string
	status int // what the receiver answered
}

// receiver is a webhook: it records every request and answers status(n) for the n-th (from 1).
type receiver struct {
	srv   *httptest.Server
	conns atomic.Int64
	mu    sync.Mutex
	got   []delivery
	// status answers the n-th request; nil answers 200.
	status func(n int) int
	// hold delays every answer (a hung receiver).
	hold time.Duration
}

func newReceiver(t testing.TB, status func(n int) int) *receiver {
	t.Helper()
	r := &receiver{status: status}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	r.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			r.conns.Add(1)
		}
	}
	r.srv.Start()
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) serve(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	status, hold := http.StatusOK, r.hold
	if r.status != nil {
		status = r.status(len(r.got) + 1)
	}
	r.got = append(r.got, delivery{at: time.Now(), method: req.Method, path: req.URL.Path, header: req.Header.Clone(), body: string(b), status: status})
	r.mu.Unlock()
	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-req.Context().Done():
			return
		}
	}
	if status >= 300 && status < 400 {
		w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
	}
	w.WriteHeader(status)
}

func (r *receiver) requests() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

// delivered returns the requests the receiver answered 2xx (and did not hold past the client's timeout).
func (r *receiver) delivered() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []delivery
	for _, d := range r.got {
		if d.status >= 200 && d.status < 300 {
			out = append(out, d)
		}
	}
	return out
}

func (r *receiver) url(path string) string { return r.srv.URL + path }

// logBuffer captures a logger's output for the credential greps.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newLogger(lb *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// fast is a delivery timing for tests: the spec's shape, scaled down.
func fast() DelivererOptions {
	return DelivererOptions{RetryMin: 10 * time.Millisecond, RetryMax: 80 * time.Millisecond, Budget: time.Minute,
		IdlePoll: 50 * time.Millisecond, Timeout: 2 * time.Second, Replicas: func(context.Context) (int, error) { return 1, nil },
		Logger: quiet}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// private is the push.allow_private policy the loopback receivers need (http:// on loopback, D67).
var private = ssrf.Policy{AllowPrivate: true}

func pushConfig(url string) *a2a.PushConfig {
	return &a2a.PushConfig{URL: url, Token: secretToken, Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: secretCreds}}
}

// saveInline saves cfg for task id the way the work queue does, in a transaction of its own.
func (e *env) saveInline(cs *ConfigStore, id uuid.UUID, o store.Owner, cfg *a2a.PushConfig) (string, bool) {
	e.t.Helper()
	var (
		cid      string
		inserted bool
	)
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		cid, inserted, err = cs.SaveInline(context.Background(), tx, id, o, cfg)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return cid, inserted
}

func containsAny(s string, needles ...string) string {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return n
		}
	}
	return ""
}

// pushInput is push_webhook's input for the config with id configID, named by its ConfigRef as the
// workflow names it (a task id that is not one is passed through, for the bad-input tests).
func pushInput(taskID, configID string) lifecycle.PushInput {
	ref := configID
	if id, err := uuid.Parse(taskID); err == nil && configID != "" {
		ref = lifecycle.ConfigRef(id, configID)
	}
	return lifecycle.PushInput{TaskID: taskID, ConfigID: ref, Deadline: time.Now().Add(time.Hour)}
}
