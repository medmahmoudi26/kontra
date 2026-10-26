package tunnelgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/lifecycle"
)

// candidatesSQL is relay.md §6.1's candidate query, with each session's key (the attempt row records it).
// The service comes from the enrollment tables only: the key's binding to it, never the request.
const candidatesSQL = `select t.session_id, t.key_id, t.replica_id from kontra.tunnels t
  join kontra.tunnel_keys k   on k.key_id = t.key_id and k.revoked_at is null and k.revoked_reason is distinct from 'rotated'
  join kontra.key_services ks on ks.key_id = t.key_id and ks.service = $2 and ks.revoked_at is null
  join kontra.gate_replicas r on r.replica_id = t.replica_id and r.last_seen_at > now() - interval '30 seconds' and r.draining_at is null
 where t.tenant_id = $1 and $2 = any (t.services)
 order by t.session_id`

// previousSessionSQL is the session of the context's previous task (relay.md §6.1, §7): the latest attempt
// placed on a tunnel session among the caller's earlier tasks in the same gate context and service.
// Contexts are scoped by (tenant, service, buyer, context id), so another buyer's identical context string
// never steers the choice (§4, D53).
const previousSessionSQL = `select a.tunnel_session from kontra.tasks t
  join kontra.attempts a on a.task_id = t.id
 where t.tenant_id = $1 and t.service = $2 and t.buyer_id = $3 and t.context_id = $4 and t.id < $5
   and a.backend = 'tunnel' and a.tunnel_session is not null
 order by t.id desc, a.n desc limit 1`

// Picker is prepare's session choice (relay.md §6.1), on any replica: the candidates are
//
//	SELECT t.session_id, t.replica_id FROM tunnels t
//	  JOIN tunnel_keys k   ON k.key_id = t.key_id AND k.revoked_at IS NULL AND k.revoked_reason IS DISTINCT FROM 'rotated'
//	  JOIN key_services ks ON ks.key_id = t.key_id AND ks.service = $service AND ks.revoked_at IS NULL
//	  JOIN gate_replicas r ON r.replica_id = t.replica_id AND r.last_seen_at > now() - interval '30 seconds' AND r.draining_at IS NULL
//	 WHERE t.tenant_id = $tenant AND $service = ANY (t.services)
//
// round-robin among them; for a new task in a known context the session of the context's previous task
// first while it is a candidate; a retry after a lost session avoids that session when another exists.
// No candidate is lifecycle.ErrNoSession.
//
// The round-robin is this replica's: prepare runs on whichever replica's kontra-lifecycle worker takes it,
// and each replica's Picker turns to the candidate after the one it chose last for the service (in session
// id order, so a candidate set that changes between two choices is still walked in turn). The context's
// previous session is read from Postgres, so it holds whichever replica prepares the task (M2 plan audit,
// skeleton problem 2: SessionRequest carries the task's buyer and context, which prepare has read).
//
// SPEC-CONFLICT (M2 plan, confirmed conflict on relay.md §6.1 and §14.2, "skipping replicas at their
// dispatch capacity"): nothing in 0001 records a replica's dispatches in flight (gate_replicas has no such
// column), prepare runs on any replica and cannot know another's load, and attempts has no index on
// replica_id to count them. The closest faithful behaviour: no capacity skip here; concurrency is bounded
// where it is measured, by each replica's kontra-tunnel-<id> worker (limits.dispatch_slots), and a dispatch
// not started within 15 s follows §6.4's schedule-to-start row (a lost dispatch: another session). A
// capacity-aware choice needs a migration (gate_replicas.inflight, written with the 10 s heartbeat).
type Picker struct {
	db *pgxpool.Pool

	mu   sync.Mutex
	last map[pickKey]uuid.UUID // per service: the session chosen last on this replica
}

type pickKey struct {
	tenant  uuid.UUID
	service string
}

// NewPicker returns a picker over db.
func NewPicker(db *pgxpool.Pool) *Picker { return &Picker{db: db, last: map[pickKey]uuid.UUID{}} }

var _ lifecycle.SessionPicker = (*Picker)(nil)

// candidate is one row of the candidate query.
type candidate struct {
	session uuid.UUID
	keyID   string
	replica string
}

// PickSession implements lifecycle.SessionPicker.
func (p *Picker) PickSession(ctx context.Context, req lifecycle.SessionRequest) (lifecycle.PickedSession, error) {
	if p.db == nil {
		return lifecycle.PickedSession{}, errors.New("tunnelgate: the picker has no database")
	}
	rows, err := p.db.Query(ctx, candidatesSQL, req.TenantID, req.Service)
	if err != nil {
		return lifecycle.PickedSession{}, fmt.Errorf("tunnelgate: candidate sessions of %s: %w", req.Service, err)
	}
	cands, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.session, &c.keyID, &c.replica)
		return c, err
	})
	if err != nil {
		return lifecycle.PickedSession{}, fmt.Errorf("tunnelgate: candidate sessions of %s: %w", req.Service, err)
	}
	if len(cands) == 0 {
		return lifecycle.PickedSession{}, lifecycle.ErrNoSession
	}
	avoided := func(c candidate) bool { return req.Avoid != nil && c.session == *req.Avoid }
	// A retry after a lost session goes to another candidate when there is one (§6.4); the avoided session
	// stays the last resort, under the retry's own backoff.
	if others := slices.DeleteFunc(slices.Clone(cands), avoided); len(others) > 0 {
		cands = others
	}
	key := pickKey{req.TenantID, req.Service}
	if req.ContextID != "" && req.TaskID != uuid.Nil {
		var prev uuid.UUID
		err := p.db.QueryRow(ctx, previousSessionSQL, req.TenantID, req.Service, req.BuyerID, req.ContextID, req.TaskID).Scan(&prev)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return lifecycle.PickedSession{}, fmt.Errorf("tunnelgate: the context's previous session: %w", err)
		default:
			// Best effort (PRD §7): the previous session while it is a candidate.
			if i := slices.IndexFunc(cands, func(c candidate) bool { return c.session == prev }); i >= 0 {
				p.remember(key, cands[i].session)
				return cands[i].picked(), nil
			}
		}
	}
	p.mu.Lock()
	last, seen := p.last[key]
	p.mu.Unlock()
	pick := cands[0]
	if seen {
		// The first candidate after the last choice, in session id order, wrapping around.
		if i := slices.IndexFunc(cands, func(c candidate) bool { return bytes.Compare(c.session[:], last[:]) > 0 }); i >= 0 {
			pick = cands[i]
		}
	}
	p.remember(key, pick.session)
	return pick.picked(), nil
}

func (p *Picker) remember(k pickKey, s uuid.UUID) {
	p.mu.Lock()
	p.last[k] = s
	p.mu.Unlock()
}

func (c candidate) picked() lifecycle.PickedSession {
	return lifecycle.PickedSession{SessionID: c.session, KeyID: c.keyID, ReplicaID: c.replica}
}
