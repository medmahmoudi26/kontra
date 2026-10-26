package tunnelgate

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
)

// pickEnv is one database with the team tenant, two tunnel services, and the rows the candidate query
// reads (tunnel_keys, key_services, gate_replicas, tunnels), written directly.
type pickEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	log    *store.Log
	team   uuid.UUID
	svc    store.ServiceRef
	picker *Picker
	acts   *lifecycle.LifecycleActivities
}

// tunnelOnly is prepare's BackendResolver: every service is a tunnel service.
type tunnelOnly struct{}

func (tunnelOnly) BackendFor(context.Context, uuid.UUID, string) (store.Backend, error) {
	return store.BackendTunnel, nil
}

func newPickEnv(t *testing.T) *pickEnv {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	team, err := tenant.EnsureTeam(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"hello", "other"} {
		if _, err := pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
			values ($1, $2, 'a2a', '{"tunnel": {}}'::jsonb, 'config')`, team, s); err != nil {
			t.Fatal(err)
		}
	}
	e := &pickEnv{t: t, pool: pool, log: store.NewLog(pool), team: team, picker: NewPicker(pool),
		svc: store.ServiceRef{TenantID: team, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: time.Hour}}
	e.acts = &lifecycle.LifecycleActivities{Log: e.log, Backends: tunnelOnly{}, Sessions: e.picker}
	return e
}

func (e *pickEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// replica inserts a live gate replica.
func (e *pickEnv) replica(id string) string {
	e.exec(`insert into kontra.gate_replicas (replica_id, version) values ($1, 'test')`, id)
	return id
}

// key inserts a node key bound to the services.
func (e *pickEnv) key(services ...string) string {
	e.t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 26)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	id := "k1_" + string(b)
	pub := make([]byte, 32)
	_, _ = rand.Read(pub)
	e.exec(`insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, 'node')`, id, e.team, pub)
	for _, s := range services {
		e.exec(`insert into kontra.key_services (key_id, tenant_id, service) values ($1, $2, $3)`, id, e.team, s)
	}
	return id
}

// session inserts a tunnels row of key on replica serving the services; ids are UUIDv7, so the later
// session sorts after the earlier.
func (e *pickEnv) session(key, replica string, services ...string) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tunnels (session_id, key_id, tenant_id, replica_id, services, cli_version)
		values ($1, $2, $3, $4, $5::text[], 'test')`, id, key, e.team, replica, services)
	return id
}

// task creates a submitted task of buyer in the gate context.
func (e *pickEnv) task(buyer uuid.UUID, contextID string) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	if _, err := e.log.CreateTask(context.Background(), nil, store.NewTask{ID: id, Service: e.svc, Protocol: store.ProtocolA2A,
		Owner: store.Owner{BuyerID: buyer, Principal: "key:kk_test_0001", Via: store.ViaAPIKey}, ContextID: contextID,
		State: store.StateSubmitted, Snapshot: snap}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *pickEnv) pick(req lifecycle.SessionRequest) (lifecycle.PickedSession, error) {
	if req.TenantID == uuid.Nil {
		req.TenantID = e.team
	}
	if req.Service == "" {
		req.Service = "hello"
	}
	return e.picker.PickSession(context.Background(), req)
}

func (e *pickEnv) prepare(task uuid.UUID, n int, prev, avoid string) lifecycle.PrepareResult {
	e.t.Helper()
	res, err := e.acts.Prepare(context.Background(), lifecycle.PrepareInput{TaskID: task.String(), N: n, PreviousOutcome: prev, AvoidSession: avoid})
	if err != nil {
		e.t.Fatalf("prepare(%s, %d): %v", task, n, err)
	}
	return res
}

// The candidates of relay.md §6.1: a session of a key enrolled for the service, its binding not revoked,
// its key neither revoked nor rotated, on a replica seen within 30 s that is not draining, serving the
// service; round-robin among them; no candidate is ErrNoSession.
func TestPickerCandidates(t *testing.T) {
	e := newPickEnv(t)
	ra, rb := e.replica("r-a"), e.replica("r-b")
	if _, err := e.pick(lifecycle.SessionRequest{}); !errors.Is(err, lifecycle.ErrNoSession) {
		t.Fatalf("no session: %v", err)
	}
	k1, k2 := e.key("hello"), e.key("hello", "other")
	s1 := e.session(k1, ra, "hello")
	s2 := e.session(k2, rb, "hello", "other")

	// Two candidates alternate over successive choices.
	var got []uuid.UUID
	for range 4 {
		p, err := e.pick(lifecycle.SessionRequest{})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p.SessionID)
		switch p.SessionID {
		case s1:
			if p.KeyID != k1 || p.ReplicaID != ra {
				t.Errorf("s1 picked as %+v", p)
			}
		case s2:
			if p.KeyID != k2 || p.ReplicaID != rb {
				t.Errorf("s2 picked as %+v", p)
			}
		}
	}
	if got[0] == got[1] || got[1] == got[2] || got[2] == got[3] || got[0] != got[2] {
		t.Fatalf("not round-robin: %v", got)
	}
	// Only s2 serves "other"; another tenant has none.
	for range 2 {
		if p, err := e.pick(lifecycle.SessionRequest{Service: "other"}); err != nil || p.SessionID != s2 {
			t.Fatalf("other: %+v %v", p, err)
		}
	}
	if _, err := e.pick(lifecycle.SessionRequest{TenantID: uuid.Must(uuid.NewV7())}); !errors.Is(err, lifecycle.ErrNoSession) {
		t.Fatalf("another tenant: %v", err)
	}

	only := func(what string, want uuid.UUID) {
		t.Helper()
		for range 3 {
			p, err := e.pick(lifecycle.SessionRequest{})
			if err != nil || p.SessionID != want {
				t.Fatalf("%s: %+v %v, want only %s", what, p, err, want)
			}
		}
	}
	// A draining replica and one silent for 30 s are skipped (§6.1, §14.3).
	e.exec(`update kontra.gate_replicas set draining_at = now() where replica_id = $1`, rb)
	only("r-b draining", s1)
	e.exec(`update kontra.gate_replicas set draining_at = null, last_seen_at = now() where replica_id = $1`, rb)
	e.exec(`update kontra.gate_replicas set last_seen_at = now() - interval '31 seconds' where replica_id = $1`, ra)
	only("r-a silent", s2)
	e.exec(`update kontra.gate_replicas set last_seen_at = now() where replica_id = $1`, ra)

	// The service comes from the key's binding, never from the request or the row's services alone.
	e.exec(`update kontra.key_services set revoked_at = now() where key_id = $1 and service = 'hello'`, k2)
	only("k2's binding revoked", s1)
	e.exec(`update kontra.key_services set revoked_at = null where key_id = $1`, k2)
	e.exec(`update kontra.tunnel_keys set revoked_at = now(), revoked_reason = 'operator' where key_id = $1`, k1)
	only("k1 revoked", s2)
	e.exec(`update kontra.tunnel_keys set revoked_at = null, revoked_reason = 'rotated' where key_id = $1`, k1)
	only("k1 rotated", s2)
	e.exec(`update kontra.tunnel_keys set revoked_reason = null where key_id = $1`, k1)
	e.exec(`update kontra.tunnels set services = '{other}' where session_id = $1`, s1)
	only("s1 no longer serving hello", s2)
	e.exec(`update kontra.tunnels set services = '{hello}' where session_id = $1`, s1)

	// A retry after a lost session avoids it when another candidate exists, and takes it when it is the
	// only one (under the retry's own backoff).
	for range 3 {
		if p, err := e.pick(lifecycle.SessionRequest{Avoid: &s1}); err != nil || p.SessionID != s2 {
			t.Fatalf("avoiding s1: %+v %v", p, err)
		}
	}
	e.exec(`delete from kontra.tunnels where session_id = $1`, s2)
	if p, err := e.pick(lifecycle.SessionRequest{Avoid: &s1}); err != nil || p.SessionID != s1 {
		t.Fatalf("avoiding the only candidate: %+v %v", p, err)
	}
}

// For a new task in a known context, the session of the context's previous task is chosen while it is a
// candidate (relay.md §6.1, §7; PRD §7 best effort); another buyer's identical context string, or another
// context, does not steer the choice; a session that is no longer a candidate, or the one a retry avoids,
// falls back to round-robin.
func TestPickerContextAffinity(t *testing.T) {
	e := newPickEnv(t)
	ra := e.replica("r-a")
	k1, k2 := e.key("hello"), e.key("hello")
	s1, s2 := e.session(k1, ra, "hello"), e.session(k2, ra, "hello")
	buyer, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	first := e.task(buyer, "conv")
	// The round-robin would answer s1 first; place the first task on s2.
	if err := e.log.BeginAttempt(context.Background(), store.Attempt{TaskID: first, N: 1, Backend: store.BackendTunnel,
		TunnelSession: &s2, KeyID: k2, ReplicaID: ra}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		next := e.task(buyer, "conv")
		p, err := e.pick(lifecycle.SessionRequest{TaskID: next, N: 1, BuyerID: buyer, ContextID: "conv"})
		if err != nil || p.SessionID != s2 {
			t.Fatalf("task %d in the context: %+v %v, want the previous task's session s2", i, p, err)
		}
	}
	// Another buyer with the same string, and another context, are round-robin: both sessions come up.
	seen := map[uuid.UUID]bool{}
	for _, req := range []lifecycle.SessionRequest{
		{TaskID: e.task(other, "conv"), BuyerID: other, ContextID: "conv"},
		{TaskID: e.task(other, "conv"), BuyerID: other, ContextID: "conv"},
		{TaskID: e.task(buyer, "elsewhere"), BuyerID: buyer, ContextID: "elsewhere"},
		{TaskID: e.task(buyer, "elsewhere"), BuyerID: buyer, ContextID: "elsewhere"},
	} {
		p, err := e.pick(req)
		if err != nil {
			t.Fatal(err)
		}
		seen[p.SessionID] = true
	}
	if !seen[s1] || !seen[s2] {
		t.Fatalf("round-robin outside the context: %v", seen)
	}
	// The retry of a task whose session was lost leaves it, even in its context.
	next := e.task(buyer, "conv")
	if p, err := e.pick(lifecycle.SessionRequest{TaskID: next, N: 2, BuyerID: buyer, ContextID: "conv", Avoid: &s2}); err != nil || p.SessionID != s1 {
		t.Fatalf("a retry avoiding the context's session: %+v %v", p, err)
	}
	// The previous session gone: the choice falls back to the candidates.
	e.exec(`delete from kontra.tunnels where session_id = $1`, s2)
	if p, err := e.pick(lifecycle.SessionRequest{TaskID: e.task(buyer, "conv"), N: 1, BuyerID: buyer, ContextID: "conv"}); err != nil || p.SessionID != s1 {
		t.Fatalf("the context's session gone: %+v %v", p, err)
	}
}

// Prepare for a tunnel service (relay.md §6.1, §6.4, §14.2): the attempt row records backend tunnel,
// tunnel_session, key_id and replica_id, and the queue is kontra-tunnel-<replica>; successive tasks
// alternate between two sessions; a retry after tunnel_lost (AvoidSession) takes the other session and ends
// the lost attempt tunnel_lost; a retried prepare answers the attempt as written; with no candidate the
// attempt is written ended no_session (the tunnel_has_session check holds) and NoSession is answered; a
// node connecting meanwhile lets the next attempt be placed.
func TestPrepareTunnel(t *testing.T) {
	e := newPickEnv(t)
	ctx := context.Background()
	buyer := uuid.Must(uuid.NewV7())

	// No candidate: attempt 1 is no_session, and nothing to dispatch.
	lonely := e.task(buyer, "c-lonely")
	res := e.prepare(lonely, 1, "", "")
	row, _ := e.log.Load(ctx, lonely)
	if !res.NoSession || res.Queue != "" || res.SessionID != "" || res.Backend != "tunnel" || !res.Deadline.Equal(row.Deadline.UTC()) {
		t.Fatalf("prepare without a candidate %+v", res)
	}
	if again := e.prepare(lonely, 1, "", ""); !again.NoSession {
		t.Fatalf("a retried prepare %+v", again)
	}
	a1, err := e.log.GetAttempt(ctx, lonely, 1)
	if err != nil || a1.Backend != store.BackendTunnel || a1.Outcome != store.OutcomeNoSession || a1.EndedAt == nil || a1.TunnelSession != nil {
		t.Fatalf("no_session attempt %+v %v", a1, err)
	}
	st, err := e.acts.AttemptState(ctx, lifecycle.AttemptRef{TaskID: lonely.String(), N: 1})
	if err != nil || !st.Ended || st.FirstUpstreamIDAt != nil || st.Backend != "tunnel" {
		t.Fatalf("attempt_state %+v %v", st, err)
	}

	// A node connects during the backoff: the next attempt is placed on it.
	ra, rb := e.replica("r-a"), e.replica("r-b")
	k1, k2 := e.key("hello"), e.key("hello")
	s1 := e.session(k1, ra, "hello")
	res = e.prepare(lonely, 2, "", "")
	if res.NoSession || res.Queue != lifecycle.QueueTunnel(ra) || res.SessionID != s1.String() {
		t.Fatalf("prepare after the node connected %+v", res)
	}
	a2, _ := e.log.GetAttempt(ctx, lonely, 2)
	if a2.Backend != store.BackendTunnel || a2.TunnelSession == nil || *a2.TunnelSession != s1 || a2.KeyID != k1 || a2.ReplicaID != ra || a2.EndedAt != nil {
		t.Fatalf("attempt 2 %+v", a2)
	}
	if a1, _ := e.log.GetAttempt(ctx, lonely, 1); a1.Outcome != store.OutcomeNoSession {
		t.Fatalf("attempt 1 changed: %+v", a1)
	}

	// Two sessions: successive tasks alternate.
	s2 := e.session(k2, rb, "hello")
	queues := map[string]string{s1.String(): lifecycle.QueueTunnel(ra), s2.String(): lifecycle.QueueTunnel(rb)}
	var placed []string
	for i := range 4 {
		id := e.task(buyer, fmt.Sprintf("c-%d", i))
		r := e.prepare(id, 1, "", "")
		if r.Queue != queues[r.SessionID] {
			t.Fatalf("task %d: %+v", i, r)
		}
		placed = append(placed, r.SessionID)
	}
	if placed[0] == placed[1] || placed[1] == placed[2] || placed[2] == placed[3] {
		t.Fatalf("successive tasks did not alternate: %v", placed)
	}

	// A retry after tunnel_lost leaves the lost session for the other.
	lost := e.task(buyer, "c-lost")
	first := e.prepare(lost, 1, "", "")
	if _, err := e.log.BeginDispatch(ctx, lost, 1, strings.TrimPrefix(first.Queue, "kontra-tunnel-")); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a retried prepare answers what the first wrote
		retry := e.prepare(lost, 2, string(store.OutcomeTunnelLost), first.SessionID)
		if retry.SessionID == first.SessionID || retry.SessionID == "" {
			t.Fatalf("the retry after tunnel_lost went to %s again: %+v", first.SessionID, retry)
		}
	}
	if a, _ := e.log.GetAttempt(ctx, lost, 1); a.Outcome != store.OutcomeTunnelLost || a.EndedAt == nil {
		t.Fatalf("the lost attempt %+v", a)
	}

	// Without a picker, a tunnel service is service_unavailable.
	e.acts.Sessions = nil
	if _, err := e.acts.Prepare(ctx, lifecycle.PrepareInput{TaskID: e.task(buyer, "c-x").String(), N: 1}); err == nil ||
		!strings.Contains(err.Error(), "places no tunnel attempts") {
		t.Fatalf("prepare without a picker: %v", err)
	}
}
