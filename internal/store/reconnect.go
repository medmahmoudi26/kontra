package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A task whose tunnel session is lost after its first upstream task id waits for its node to reconnect (spec
// relay.md §6.4, D340..D349). The buyer is told in band: a status event the gate writes on entering the wait, and
// the re-attach's status event on resuming, each with metadata["kontra.reconnect"].

// MetaReconnect is the metadata key of a reconnect wait's status events (D341): {state: "waiting", since, until} on
// entering it, {state: "resumed", at} on the re-attach that ends it. Only the gate writes it (spec §2.5).
const MetaReconnect = "kontra.reconnect"

// Values of metadata["kontra.reconnect"].state.
const (
	ReconnectWaiting = "waiting"
	ReconnectResumed = "resumed"
)

// ReconnectWaitingMeta is metadata["kontra.reconnect"] on entering the wait: since and until in RFC 3339 (UTC,
// seconds).
func ReconnectWaitingMeta(since, until time.Time) map[string]any {
	return map[string]any{"state": ReconnectWaiting, "since": since.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339)}
}

// ReconnectResumedMeta is metadata["kontra.reconnect"] on the re-attach that ends the wait.
func ReconnectResumedMeta(at time.Time) map[string]any {
	return map[string]any{"state": ReconnectResumed, "at": at.UTC().Format(time.RFC3339)}
}

// ReconnectState is the state a task's metadata["kontra.reconnect"] names ("" when it has none).
func ReconnectState(t *a2a.Task) string {
	if t == nil {
		return ""
	}
	m, _ := t.Metadata[MetaReconnect].(map[string]any)
	s, _ := m["state"].(string)
	return s
}

// GateStatus is a status event the gate writes itself: an agent-role status message with one text part, and meta
// on both the message and the event, which merges it into the task's metadata (TerminalStatus is its terminal form).
func GateStatus(info a2a.TaskInfoProvider, state a2a.TaskState, meta map[string]any, text string) *a2a.TaskStatusUpdateEvent {
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
	ev := a2a.NewStatusUpdateEvent(info, state, msg)
	if len(meta) > 0 {
		msg.Metadata = maps.Clone(meta)
		ev.Metadata = maps.Clone(meta)
	}
	return ev
}

// ReconnectWait is what LogReconnectWait did.
type ReconnectWait struct {
	Seq    int64 // the status row's seq; 0 when nothing was logged
	Logged bool  // the waiting status is in the log (this call's, or an earlier call's for the same since)
	State  State // the task's state after the call
}

// LogReconnectWait logs the gate's waiting status of a reconnect wait that began at since and ends at until (D341):
// working, an agent-role message with the one text part text, metadata["kontra.reconnect"] =
// ReconnectWaitingMeta(since, until). It is a status event like any other: A2A streams, SubscribeToTask, push, /mcp
// and an operator's console read it from the log; working ends no turn anywhere (§9).
//
// Nothing is logged for a task that is terminal, quoted, or waits at input-required or auth-required: their status
// message is the question or the instructions the caller acts on, and stays the status (D341). It is idempotent: a
// task whose metadata already names this wait (state waiting, the same since) is left as it is, so a retried
// activity logs one row.
func (l *Log) LogReconnectWait(ctx context.Context, taskID uuid.UUID, since, until time.Time, text string) (ReconnectWait, error) {
	meta := ReconnectWaitingMeta(since, until)
	var last error
	for range conflictRetries {
		row, err := l.Load(ctx, taskID)
		if err != nil {
			return ReconnectWait{}, err
		}
		switch row.State {
		case StateSubmitted, StateWorking:
		default:
			return ReconnectWait{State: row.State}, nil
		}
		if row.Protocol != ProtocolA2A || row.Snapshot == nil {
			return ReconnectWait{State: row.State}, nil
		}
		if m, _ := row.Snapshot.Metadata[MetaReconnect].(map[string]any); m != nil && m["state"] == ReconnectWaiting && m["since"] == meta["since"] {
			return ReconnectWait{Logged: true, State: row.State}, nil
		}
		ev := GateStatus(row.Snapshot, a2a.TaskStateWorking, map[string]any{MetaReconnect: meta}, text)
		next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
		if err != nil {
			return ReconnectWait{}, err
		}
		payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
		if err != nil {
			return ReconnectWait{}, err
		}
		ap, err := l.Append(ctx, nil, Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: KindStatus, State: StateWorking,
			StatusAt: StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload,
		})
		if err == nil {
			return ReconnectWait{Seq: ap.Seq, Logged: true, State: StateWorking}, nil
		}
		if !errors.Is(err, taskstore.ErrConcurrentModification) {
			return ReconnectWait{}, err
		}
		last = err
	}
	return ReconnectWait{}, last
}

// RebindAttempt records on attempt n the tunnel session a reconnect wait found (D343): tunnel_session, key_id and
// replica_id, before the resume dispatch that runs on kontra-tunnel-<replica> and writes its own fence. Attempt n must
// be the task's latest and open, else ErrAttemptNotOpen (ErrAttemptNotFound when it does not exist).
func (l *Log) RebindAttempt(ctx context.Context, taskID uuid.UUID, n int, session uuid.UUID, keyID, replicaID string) error {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	tag, err := l.writeExec(ctx, `update kontra.attempts a set tunnel_session = $3, key_id = $4, replica_id = $5
			where a.task_id = $1 and a.n = $2 and a.ended_at is null and a.backend = 'tunnel'
			  and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)`,
		taskID, n, session, keyID, replicaID)
	if err != nil {
		return fmt.Errorf("store: rebind attempt %d of task %s: %w", n, taskID, err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := l.GetAttempt(ctx, taskID, n); err != nil {
			return err
		}
		return fmt.Errorf("store: attempt %d of task %s: %w", n, taskID, ErrAttemptNotOpen)
	}
	return nil
}

// keyChainSQL is a node key and every key it was rotated to (tunnel.md §12: rotate_from proves the old key, so the
// new key is the same node), as far as 16 rotations, with its depth.
const keyChainSQL = `with recursive chain(key_id, depth) as (
	select $1::text, 0
	union all
	select k.rotated_to, c.depth + 1 from chain c join kontra.tunnel_keys k on k.key_id = c.key_id
	 where k.rotated_to is not null and c.depth < 16
)`

// waitingForKeySQL is the open tasks whose latest attempt is a tunnel attempt with a first upstream task id on a key
// of keyChain's backward chain (the key that connected, or one rotated to it), and whose recorded session is not a
// live candidate any more (no tunnels row, or its replica silent or draining). It starts from the open tasks
// (tasks_open, a partial index on terminal_at is null) and reads each one's latest attempt by its primary key, so it
// needs no index of its own and no migration (D344).
const waitingForKeySQL = `with recursive back(key_id, depth) as (
	select $1::text, 0
	union all
	select k.key_id, b.depth + 1 from back b join kontra.tunnel_keys k on k.rotated_to = b.key_id
	 where b.depth < 16
)
select t.id from kontra.tasks t
  join lateral (select a.* from kontra.attempts a where a.task_id = t.id order by a.n desc limit 1) a on true
 where t.terminal_at is null and t.tenant_id = $2 and t.protocol = 'a2a'
   and a.backend = 'tunnel' and a.ended_at is null and a.first_upstream_id_at is not null
   and a.key_id in (select key_id from back)
   and (a.tunnel_session is distinct from $3::uuid)
   and not exists (select 1 from kontra.tunnels x
                     join kontra.gate_replicas r on r.replica_id = x.replica_id
                                                and r.last_seen_at > now() - interval '30 seconds' and r.draining_at is null
                    where x.session_id = a.tunnel_session)
 order by t.id
 limit $4`

// MaxReconnectSignals bounds the tasks one session registration signals (WaitingForKey): a node with more tasks
// waiting than that has the rest found by their own periodic check, within seconds (D344).
const MaxReconnectSignals = 1000

// WaitingForKey returns the tasks a session of keyID, just registered in tenant tenantID as session, may resume
// (D344): open A2A tasks whose latest attempt is a tunnel attempt with a first upstream task id, on keyID or a key
// rotated to it, whose recorded session is no live candidate. The signal it feeds carries ids only; each workflow
// checks for itself whether the session is a candidate for it (reattach_session). At most MaxReconnectSignals.
func (l *Log) WaitingForKey(ctx context.Context, tenantID uuid.UUID, keyID string, session uuid.UUID) ([]uuid.UUID, error) {
	rows, err := l.db.Query(ctx, waitingForKeySQL, keyID, tenantID, session, MaxReconnectSignals)
	if err != nil {
		return nil, fmt.Errorf("store: tasks waiting for key %s: %w", keyID, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("store: tasks waiting for key %s: %w", keyID, err)
	}
	return ids, nil
}

// ReattachCandidate is a tunnel session a reconnect wait may resume its attempt on (FindReattachSession).
type ReattachCandidate struct {
	SessionID uuid.UUID
	KeyID     string
	ReplicaID string
}

// ReattachGone says why no session of the node can ever serve the attempt again.
type ReattachGone string

// Reasons a reconnect wait gives up at once (D342).
const (
	ReattachKeyRevoked     ReattachGone = "key_revoked"     // the key (and every key it was rotated to) is revoked
	ReattachBindingRevoked ReattachGone = "binding_revoked" // no usable key of the chain is still enrolled for the service
)

// reattachCandidateSQL is relay.md §6.1's candidate query narrowed to the attempt's node (D342): a connected session
// of the attempt's key, or of a key it was rotated to, whose key is not revoked and not past a rotation's overlap,
// whose binding to the service is not revoked, on a live replica that is not draining. The newest key of the chain
// first, then the newest session.
const reattachCandidateSQL = keyChainSQL + `
select t.session_id, t.key_id, t.replica_id from chain c
  join kontra.tunnels t       on t.key_id = c.key_id
  join kontra.tunnel_keys k   on k.key_id = t.key_id and k.revoked_at is null
                             and (k.revoked_reason is distinct from 'rotated' or k.overlap_until > now())
  join kontra.key_services ks on ks.key_id = t.key_id and ks.service = $3 and ks.revoked_at is null
  join kontra.gate_replicas r on r.replica_id = t.replica_id and r.last_seen_at > now() - interval '30 seconds' and r.draining_at is null
 where t.tenant_id = $2 and $3 = any (t.services)
 order by c.depth desc, t.session_id desc
 limit 1`

// reattachUsableSQL counts the keys of the chain that could still connect and serve the service: neither revoked nor
// rotated (tunnel.md §12: a rotated key's new handshake is refused; its session already up is reattachCandidateSQL's),
// and of those the ones with a binding to the service not revoked.
const reattachUsableSQL = keyChainSQL + `
select count(*) filter (where k.revoked_at is null and k.revoked_reason is distinct from 'rotated'),
       count(*) filter (where k.revoked_at is null and k.revoked_reason is distinct from 'rotated'
                          and exists (select 1 from kontra.key_services ks
                                       where ks.key_id = k.key_id and ks.service = $3 and ks.revoked_at is null))
  from chain c join kontra.tunnel_keys k on k.key_id = c.key_id and k.tenant_id = $2`

// FindReattachSession looks for a session the attempt of key keyID may resume on (D342): nil and "" while the node
// has none connected (the wait goes on), nil and a ReattachGone when none can ever come (the key, or its binding to
// the service, revoked), or the candidate.
func (l *Log) FindReattachSession(ctx context.Context, tenantID uuid.UUID, service, keyID string) (*ReattachCandidate, ReattachGone, error) {
	if keyID == "" {
		return nil, ReattachKeyRevoked, nil
	}
	var c ReattachCandidate
	err := l.db.QueryRow(ctx, reattachCandidateSQL, keyID, tenantID, service).Scan(&c.SessionID, &c.KeyID, &c.ReplicaID)
	switch {
	case err == nil:
		return &c, "", nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, "", fmt.Errorf("store: a session to resume on for key %s: %w", keyID, err)
	}
	var keys, bound int
	if err := l.db.QueryRow(ctx, reattachUsableSQL, keyID, tenantID, service).Scan(&keys, &bound); err != nil {
		return nil, "", fmt.Errorf("store: the keys of %s: %w", keyID, err)
	}
	switch {
	case keys == 0:
		return nil, ReattachKeyRevoked, nil
	case bound == 0:
		return nil, ReattachBindingRevoked, nil
	}
	return nil, "", nil
}
