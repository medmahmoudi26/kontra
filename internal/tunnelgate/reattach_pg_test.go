package tunnelgate

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// The reconnect wait's reads and writes over Postgres (relay.md §6.4, D342..D344): reattach_session, the tasks a
// registering session wakes, and the waiting status.

// placedTask creates a task with attempt 1 on session s of key on replica, with a first upstream task id, the task
// working.
func (e *pickEnv) placedTask(key, replica string, s uuid.UUID) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	id := e.task(uuid.Must(uuid.NewV7()), "ctx-"+uuid.NewString())
	if err := e.log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendTunnel, TunnelSession: &s, KeyID: key, ReplicaID: replica}); err != nil {
		e.t.Fatal(err)
	}
	f, err := e.log.BeginDispatch(ctx, id, 1, replica)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.log.SetFirstUpstreamID(ctx, f, "up-"+id.String(), "up-ctx"); err != nil {
		e.t.Fatal(err)
	}
	e.status(id, a2a.TaskStateWorking)
	return id
}

// status appends a status event at the task's current version.
func (e *pickEnv) status(id uuid.UUID, st a2a.TaskState) {
	e.t.Helper()
	ctx := context.Background()
	row, err := e.log.Load(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	ev := a2a.NewStatusUpdateEvent(row.Snapshot, st, nil)
	next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	payload, _ := json.Marshal(a2a.StreamResponse{Event: ev})
	state, _ := store.StateFromA2A(st)
	if _, err := e.log.Append(ctx, nil, store.Write{TaskID: id, PrevVersion: row.Version, Kind: store.KindStatus, State: state,
		StatusAt: store.StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *pickEnv) reattach(id uuid.UUID, bind bool) lifecycle.ReattachResult {
	e.t.Helper()
	r, err := e.acts.ReattachSession(context.Background(), lifecycle.ReattachInput{TaskID: id.String(), N: 1, Bind: bind})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// reattach_session finds a session of the attempt's own node key only, on a live replica that is not draining, with
// its binding and key usable; with Bind it records it on the attempt. A rotation is followed to the new key; a revoked
// key or binding is Gone; an ended attempt is Gone with Bind.
func TestReattachSession(t *testing.T) {
	e := newPickEnv(t)
	ctx := context.Background()
	r1, r2, r3 := e.replica("r-1"), e.replica("r-2"), e.replica("r-3")
	k, other := e.key("hello"), e.key("hello")
	a := e.session(k, r1, "hello")
	id := e.placedTask(k, r1, a)

	// The session the attempt was placed on is still connected: it is a candidate (a stream reset, not a lost session).
	if r := e.reattach(id, false); r.SessionID != a.String() || r.Queue != lifecycle.QueueTunnel(r1) {
		t.Fatalf("the attempt's own live session: %+v", r)
	}
	e.exec(`delete from kontra.tunnels where session_id = $1`, a)
	// Another node of the service is connected: never a candidate.
	e.session(other, r2, "hello")
	if r := e.reattach(id, true); r != (lifecycle.ReattachResult{}) {
		t.Fatalf("another node's session: %+v, want none", r)
	}
	// The node's session on a draining replica, or one not serving the service: none.
	e.session(k, r3, "hello")
	e.exec(`update kontra.gate_replicas set draining_at = now() where replica_id = $1`, r3)
	e.session(k, r2, "other")
	if r := e.reattach(id, true); r != (lifecycle.ReattachResult{}) {
		t.Fatalf("draining or not serving: %+v, want none", r)
	}
	// The node is back on r-2: bound.
	b := e.session(k, r2, "hello")
	r := e.reattach(id, true)
	if r.SessionID != b.String() || r.ReplicaID != r2 || r.Queue != lifecycle.QueueTunnel(r2) || r.Gone != "" {
		t.Fatalf("the node back: %+v", r)
	}
	att, err := e.log.GetAttempt(ctx, id, 1)
	if err != nil || att.TunnelSession == nil || *att.TunnelSession != b || att.ReplicaID != r2 || att.KeyID != k || att.EndedAt != nil {
		t.Fatalf("the attempt after the bind: %+v %v", att, err)
	}

	// A rotation (tunnel.md §12): the new key is the same node; its session is found, and bound with its key.
	e.exec(`delete from kontra.tunnels where key_id = $1`, k)
	k2 := e.key("hello")
	e.exec(`update kontra.tunnel_keys set revoked_reason = 'rotated', rotated_to = $2, overlap_until = now() + interval '1 day' where key_id = $1`, k, k2)
	c := e.session(k2, r1, "hello")
	if r := e.reattach(id, true); r.SessionID != c.String() {
		t.Fatalf("the rotated key's session: %+v", r)
	}
	if att, _ := e.log.GetAttempt(ctx, id, 1); att.KeyID != k2 {
		t.Fatalf("the attempt's key after a rotation: %s, want %s", att.KeyID, k2)
	}
	e.exec(`delete from kontra.tunnels where key_id = $1`, k2)

	// Revoked: the binding of the chain's live key, then the key itself.
	e.exec(`update kontra.key_services set revoked_at = now() where key_id = $1`, k2)
	if r := e.reattach(id, true); r.Gone != lifecycle.ReattachBindingRevoked {
		t.Fatalf("binding revoked: %+v", r)
	}
	e.exec(`update kontra.tunnel_keys set revoked_at = now(), revoked_reason = 'owner' where key_id = $1`, k2)
	e.exec(`update kontra.tunnel_keys set revoked_at = now() where key_id = $1`, k) // its overlap ended
	if r := e.reattach(id, true); r.Gone != lifecycle.ReattachKeyRevoked {
		t.Fatalf("key revoked: %+v", r)
	}

	// An ended attempt is not bound.
	k3 := e.key("hello")
	s3 := e.session(k3, r1, "hello")
	id3 := e.placedTask(k3, r1, s3)
	if err := e.log.EndAttempt(ctx, id3, 1, store.OutcomeCanceled); err != nil {
		t.Fatal(err)
	}
	if r := e.reattach(id3, true); r.Gone != lifecycle.ReattachAttemptEnded {
		t.Fatalf("ended attempt: %+v", r)
	}
	if r := e.reattach(id3, false); r.SessionID != s3.String() {
		t.Fatalf("an ended attempt's node, looked for only (lateCancel): %+v", r)
	}
}

// The tasks a registering session wakes (D344): open A2A tasks whose latest attempt, on the session's key or a key
// rotated to it, has a first upstream task id and a session that is no live candidate; never a task still on a live
// session, a terminal one, another key's, or one whose attempt ended.
func TestWaitingForKey(t *testing.T) {
	e := newPickEnv(t)
	ctx := context.Background()
	r1, r2 := e.replica("r-1"), e.replica("r-2")
	k, other := e.key("hello"), e.key("hello")
	gone := e.session(k, r1, "hello")
	waiting := e.placedTask(k, r1, gone)
	live := e.session(k, r2, "hello")
	running := e.placedTask(k, r2, live)
	ended := e.placedTask(k, r1, gone)
	if err := e.log.EndAttempt(ctx, ended, 1, store.OutcomeTunnelLost); err != nil {
		t.Fatal(err)
	}
	done := e.placedTask(k, r1, gone)
	e.status(done, a2a.TaskStateCompleted)
	o := e.session(other, r1, "hello")
	others := e.placedTask(other, r1, o)
	e.exec(`delete from kontra.tunnels where session_id = any($1)`, []uuid.UUID{gone, o})
	// The replica of the running task's session goes silent: it is waiting too.
	newS := e.session(k, r1, "hello")
	got, err := e.log.WaitingForKey(ctx, e.team, k, newS)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []uuid.UUID{waiting}) {
		t.Fatalf("waiting for %s: %v, want only %s (running %s, ended %s, done %s, other's %s)", k, got, waiting, running, ended, done, others)
	}
	e.exec(`update kontra.gate_replicas set last_seen_at = now() - interval '31 seconds' where replica_id = $1`, r2)
	got, _ = e.log.WaitingForKey(ctx, e.team, k, newS)
	if want := []uuid.UUID{waiting, running}; !slices.Equal(got, want) {
		t.Fatalf("with r-2 silent: %v, want %v", got, want)
	}
	// A key rotated to: the old key's tasks are its node's.
	k2 := e.key("hello")
	e.exec(`update kontra.tunnel_keys set revoked_reason = 'rotated', rotated_to = $2, overlap_until = now() + interval '1 day' where key_id = $1`, k, k2)
	got, _ = e.log.WaitingForKey(ctx, e.team, k2, e.session(k2, r1, "hello"))
	if len(got) != 2 {
		t.Fatalf("for the rotated-to key: %v", got)
	}
	if got, _ := e.log.WaitingForKey(ctx, uuid.Must(uuid.NewV7()), k, newS); len(got) != 0 {
		t.Fatalf("another tenant: %v", got)
	}
}

// The waiting status (D341): working, the gate's text, kontra.reconnect {waiting, since, until}; once per wait
// (idempotent); nothing for a task at input-required, whose question stays the status.
func TestReconnectWaitStatus(t *testing.T) {
	e := newPickEnv(t)
	ctx := context.Background()
	r1 := e.replica("r-1")
	k := e.key("hello")
	s := e.session(k, r1, "hello")
	id := e.placedTask(k, r1, s)
	since := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	in := lifecycle.ReconnectWaitInput{TaskID: id.String(), N: 1, Since: since, Until: since.Add(15 * time.Minute)}
	for range 2 {
		r, err := e.acts.ReconnectWait(ctx, in)
		if err != nil || !r.Logged {
			t.Fatalf("reconnect_wait: %+v %v", r, err)
		}
	}
	evs, err := e.log.Events(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var waits int
	var last a2a.StreamResponse
	for _, ev := range evs {
		var sr a2a.StreamResponse
		if json.Unmarshal(ev.Payload, &sr) == nil {
			if st, ok := sr.Event.(*a2a.TaskStatusUpdateEvent); ok && st.Metadata[store.MetaReconnect] != nil {
				waits++
				last = sr
			}
		}
	}
	if waits != 1 {
		t.Fatalf("%d waiting statuses, want 1", waits)
	}
	st := last.Event.(*a2a.TaskStatusUpdateEvent)
	m, _ := st.Metadata[store.MetaReconnect].(map[string]any)
	if st.Status.State != a2a.TaskStateWorking || st.Status.Message == nil || st.Status.Message.Role != a2a.MessageRoleAgent ||
		len(st.Status.Message.Parts) != 1 || st.Status.Message.Parts[0].Text() != lifecycle.ReconnectText(15*time.Minute) ||
		m["state"] != "waiting" || m["since"] != "2026-11-05T10:00:00Z" || m["until"] != "2026-11-05T10:15:00Z" {
		t.Fatalf("the waiting status %+v, meta %v", st, m)
	}
	if lifecycle.ReconnectText(15*time.Minute) != "The agent's node disconnected. Waiting up to 15 minutes for it to reconnect." {
		t.Fatalf("text %q", lifecycle.ReconnectText(15*time.Minute))
	}
	row, _ := e.log.Load(ctx, id)
	if store.ReconnectState(row.Snapshot) != store.ReconnectWaiting || row.State != store.StateWorking {
		t.Fatalf("the projection: %s %v", row.State, row.Snapshot.Metadata)
	}

	// At input-required: nothing.
	q := e.placedTask(k, r1, s)
	e.status(q, a2a.TaskStateInputRequired)
	before, _ := e.log.Load(ctx, q)
	if r, err := e.acts.ReconnectWait(ctx, lifecycle.ReconnectWaitInput{TaskID: q.String(), N: 1, Since: since, Until: since.Add(time.Minute)}); err != nil || r.Logged {
		t.Fatalf("input-required: %+v %v", r, err)
	}
	if after, _ := e.log.Load(ctx, q); after.Version != before.Version {
		t.Fatal("a status was logged on an input-required task")
	}
}
