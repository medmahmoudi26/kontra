package enroll

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/config"
)

// Revocation is one node-key or binding revocation a replica learned of (§12).
type Revocation struct {
	KeyID   string
	Service string // empty: the whole key
	// Reason is a key's revoked_reason: owner, operator, rotated (its overlap ended) or expired; "" when the
	// key's row is gone or could not be read, and for a binding (key_services keeps no reason).
	Reason string
}

// RevocationRefresh is the longest a node-key or binding revocation takes to reach a replica without
// NOTIFY (relay.md §2.3: re-read at least every 5 s).
const RevocationRefresh = 5 * time.Second

const (
	// revocationOverlap re-reads revocations this far behind the previous read's clock: a revocation is
	// stamped when its statement runs and becomes visible when its transaction commits.
	revocationOverlap = time.Minute
	// revocationRetain keeps a revocation in memory this long after it happened. A session admitted before
	// a revocation committed learns of it from Subscribe; this covers a session registered just after the
	// revocation was delivered (its admission read the row before the revocation committed).
	revocationRetain = 10 * time.Minute
	// revocationReadTimeout bounds one re-read; notifyReadTimeout the read of the rows of a batch of NOTIFY
	// payloads, at most notifyBatch of them.
	revocationReadTimeout = 5 * time.Second
	notifyReadTimeout     = time.Second
	notifyBatch           = 1000
)

// Revocations is the replica's view of revoked node keys and key–service bindings (relay.md §2.3, the
// node half of the revocation cache auth.RevocationCache keeps for API keys): refreshed by NOTIFY
// kontra_revoke 'key:<key_id>' and 'binding:<key_id>/<service>', re-read from Postgres (what was revoked
// since the last read, overlapping it) at least every 5 s and whenever the LISTEN connection reconnects.
// NOTIFY only shortens the delay. The tunnel registry subscribes to act on each revocation (close the
// key's sessions, or drop the service from the served set), and asks it before every stream (key_scope).
//
// It also holds the rotated keys still inside their overlap (§12), re-read every time, so that KeyUsable
// turns false when the overlap passes even before the sweep sets revoked_at (EndOverlaps). A binding the
// view holds as revoked is re-checked at every re-read and forgotten once a new enrollment cleared it.
type Revocations struct {
	db       *pgxpool.Pool
	log      *slog.Logger
	now      func() time.Time // tests
	interval time.Duration
	retain   time.Duration

	mu        sync.Mutex
	keys      map[string]*keyState
	bindings  map[bindingRef]*bindingState
	subs      []func(Revocation)
	watermark time.Time // the database's clock at the start of the last successful read
	lastOK    time.Time // this process's clock at the start of the last successful read
	kick      chan struct{}
	pending   []notice // NOTIFY payloads whose rows drain has still to read
	draining  bool     // a drain goroutine is running
	drainers  sync.WaitGroup

	readMu sync.Mutex // one re-read at a time
	emitMu sync.Mutex // subscribers are called one revocation at a time, in the order learned
}

type bindingRef struct{ key, service string }

// notice is one kontra_revoke payload of the tunnel: a key ("key:<key_id>", service empty) or a binding.
type notice struct{ key, service string }

// keyState is a key the view holds: revoked, or rotated and inside its overlap.
type keyState struct {
	revokedAt *time.Time // nil: rotated, not revoked yet
	// fromPayload: revokedAt was set from a NOTIFY payload whose row has not been read yet (Notify).
	fromPayload  bool
	reason       string
	overlapUntil *time.Time
	emitted      bool
}

// bindingState is a binding the view holds as revoked.
type bindingState struct {
	// at is the row's revoked_at when exact; otherwise when the view learned of the revocation (the
	// database's clock): from a NOTIFY payload whose row is not read yet, is gone, or could not be read.
	at      time.Time
	exact   bool
	emitted bool
}

// RevocationsOption configures a Revocations.
type RevocationsOption func(*Revocations)

// WithRevocationsLogger sets the logger (default slog.Default()).
func WithRevocationsLogger(log *slog.Logger) RevocationsOption {
	return func(r *Revocations) { r.log = log }
}

// NewRevocations returns an empty view over db. Run keeps it current; the gate also routes the LISTEN
// connection's kontra_revoke payloads to Notify and its reconnects to Catchup.
func NewRevocations(db *pgxpool.Pool, opts ...RevocationsOption) *Revocations {
	r := &Revocations{
		db:       db,
		log:      slog.Default(),
		now:      time.Now,
		interval: RevocationRefresh,
		retain:   revocationRetain,
		keys:     map[string]*keyState{},
		bindings: map[bindingRef]*bindingState{},
		kick:     make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run keeps the view current until ctx ends: it reads at once, then every 3/5 of RevocationRefresh (so a
// read's own time, and a busy host, stay inside the bound), and whenever a NOTIFY could not be confirmed. A
// failed read is logged and retried at the next tick; Fresh turns false once no read has succeeded for
// RevocationRefresh. Run returns nil when ctx ends.
func (r *Revocations) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval * 3 / 5)
	defer t.Stop()
	for {
		if err := r.refresh(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("node revocations: re-read failed; the registry reads each key's row until one succeeds", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-r.kick:
		}
	}
}

// Notify applies one kontra_revoke payload (pg.Listener.OnChannel); API key payloads are auth's, and
// anything else is ignored (the tables are the truth). It never waits on the database: pg.Listener runs
// every channel's callbacks (kontra_services, the API key revocation cache, the registry's) one at a time
// on one goroutine, which a read here would hold up for as long as the pool is busy.
//
// The payload marks the key or binding revoked at once (fail closed: the NOTIFY is delivered only once the
// revoking transaction committed, and only a revocation sends one), so KeyUsable and BindingRevoked turn
// before any row is read. The view's own goroutine then reads the rows of the payloads queued meanwhile
// (one statement per kind) and emits each revocation with what the row says (the key's reason). A row that
// is gone is a revocation too (its tenant was deleted); a row that cannot be read leaves the payload's
// mark, emits it without a reason and asks for a re-read; a binding whose row is not revoked (enrolled
// again since) loses the payload's mark.
func (r *Revocations) Notify(payload string) {
	var n notice
	switch {
	case strings.HasPrefix(payload, revokeKeyPrefix):
		n.key = strings.TrimPrefix(payload, revokeKeyPrefix)
		if !config.NodeKeyIDRE.MatchString(n.key) {
			r.log.Debug("node revocations: unreadable payload", "payload", payload)
			return
		}
	case strings.HasPrefix(payload, revokeBindingPrefix):
		id, svc, ok := strings.Cut(strings.TrimPrefix(payload, revokeBindingPrefix), "/")
		if !ok || !config.NodeKeyIDRE.MatchString(id) || !config.NameRE.MatchString(svc) {
			r.log.Debug("node revocations: unreadable payload", "payload", payload)
			return
		}
		n = notice{key: id, service: svc}
	default:
		return
	}
	r.mu.Lock()
	if n.service == "" {
		r.holdKeyLocked(n.key)
	} else {
		r.holdBindingLocked(bindingRef{n.key, n.service})
	}
	r.pending = append(r.pending, n)
	start := !r.draining
	if start {
		r.draining = true
		r.drainers.Add(1)
	}
	r.mu.Unlock()
	if start {
		go r.drain()
	}
}

// holdKeyLocked marks a key revoked from its NOTIFY payload, until drain reads its row.
func (r *Revocations) holdKeyLocked(id string) {
	st := r.keys[id]
	if st == nil {
		st = &keyState{}
		r.keys[id] = st
	}
	if st.revokedAt == nil {
		t := r.clock()
		st.revokedAt, st.fromPayload = &t, true
	}
}

// holdBindingLocked marks a binding revoked from its NOTIFY payload, until drain reads its row. A binding
// already held stays as it is: the row's revoked_at tells a new revocation from the one held.
func (r *Revocations) holdBindingLocked(ref bindingRef) {
	if r.bindings[ref] == nil {
		r.bindings[ref] = &bindingState{at: r.clock()}
	}
}

// drain reads the rows of the queued NOTIFY payloads, a batch at a time, until none is left.
func (r *Revocations) drain() {
	defer r.drainers.Done()
	for {
		r.mu.Lock()
		if len(r.pending) == 0 {
			r.draining = false
			r.mu.Unlock()
			return
		}
		n := min(len(r.pending), notifyBatch)
		batch := r.pending[:n:n]
		r.pending = append([]notice(nil), r.pending[n:]...)
		r.mu.Unlock()
		r.apply(batch)
	}
}

// noticedKey is a tunnel_keys row read for a NOTIFY payload.
type noticedKey struct {
	at, overlap *time.Time
	reason      string
}

// apply reads the rows the batch names and applies each payload in order.
func (r *Revocations) apply(batch []notice) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyReadTimeout)
	defer cancel()
	var keyIDs, bindingKeys, bindingServices []string
	for _, n := range batch {
		if n.service == "" {
			keyIDs = append(keyIDs, n.key)
		} else {
			bindingKeys = append(bindingKeys, n.key)
			bindingServices = append(bindingServices, n.service)
		}
	}
	keys, bindings, err := r.readNoticed(ctx, keyIDs, bindingKeys, bindingServices)
	if err != nil {
		r.log.Warn("node revocations: the revoked rows could not be read; marked from the NOTIFY", "payloads", len(batch), "error", err)
	}
	var learned []Revocation
	r.mu.Lock()
	for _, n := range batch {
		if n.service == "" {
			k, ok := keys[n.key]
			switch {
			case err != nil || !ok:
				// Unreadable, or the key is gone (its tenant was deleted): it serves nothing.
				learned = append(learned, r.markKeyLocked(n.key, nil, "", nil)...)
			case k.at == nil:
				// Not revoked (the table is the truth): drop the payload's mark, keep a rotation's overlap.
				if st := r.keys[n.key]; st != nil && st.fromPayload {
					st.revokedAt, st.fromPayload = nil, false
					if st.overlapUntil == nil && k.overlap == nil {
						delete(r.keys, n.key)
					}
				}
				if k.overlap != nil {
					learned = append(learned, r.markKeyLocked(n.key, nil, k.reason, k.overlap)...)
				}
			default:
				learned = append(learned, r.markKeyLocked(n.key, k.at, k.reason, k.overlap)...)
			}
			continue
		}
		ref := bindingRef{n.key, n.service}
		at, ok := bindings[ref]
		switch {
		case err != nil || !ok:
			learned = append(learned, r.markBindingLocked(ref, nil)...)
		case at == nil:
			// Enrolled again since: drop the payload's mark. A dated revocation stays until a re-read clears
			// it: a re-read may have learned it after this read (fail closed).
			if st := r.bindings[ref]; st != nil && !st.exact {
				delete(r.bindings, ref)
			}
		default:
			learned = append(learned, r.markBindingLocked(ref, at)...)
		}
	}
	r.mu.Unlock()
	if err != nil {
		r.askRead()
	}
	r.emit(learned)
}

// readNoticed reads the tunnel_keys rows of keyIDs and the key_services rows of the (key, service) pairs:
// a key or binding absent from the maps has no row; a binding mapped to nil is not revoked.
func (r *Revocations) readNoticed(ctx context.Context, keyIDs, bindingKeys, bindingServices []string) (map[string]noticedKey, map[bindingRef]*time.Time, error) {
	keys := map[string]noticedKey{}
	if len(keyIDs) > 0 {
		rows, err := r.db.Query(ctx, `select key_id, revoked_at, coalesce(revoked_reason, ''), overlap_until
			from kontra.tunnel_keys where key_id = any($1::text[])`, keyIDs)
		if err != nil {
			return nil, nil, err
		}
		type row struct {
			id string
			k  noticedKey
		}
		got, err := pgx.CollectRows(rows, func(cr pgx.CollectableRow) (row, error) {
			var x row
			err := cr.Scan(&x.id, &x.k.at, &x.k.reason, &x.k.overlap)
			return x, err
		})
		if err != nil {
			return nil, nil, err
		}
		for _, x := range got {
			keys[x.id] = x.k
		}
	}
	bindings := map[bindingRef]*time.Time{}
	if len(bindingKeys) > 0 {
		rows, err := r.db.Query(ctx, `
			select ks.key_id, ks.service, ks.revoked_at
			  from kontra.key_services ks join unnest($1::text[], $2::text[]) as u(k, s) on ks.key_id = u.k and ks.service = u.s`,
			bindingKeys, bindingServices)
		if err != nil {
			return nil, nil, err
		}
		type row struct {
			ref bindingRef
			at  *time.Time
		}
		got, err := pgx.CollectRows(rows, func(cr pgx.CollectableRow) (row, error) {
			var x row
			err := cr.Scan(&x.ref.key, &x.ref.service, &x.at)
			return x, err
		})
		if err != nil {
			return nil, nil, err
		}
		for _, x := range got {
			bindings[x.ref] = x.at
		}
	}
	return keys, bindings, nil
}

func (r *Revocations) askRead() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// markKey records a key's state: revoked at at; revoked at an unknown time when at and overlap are both nil
// (a NOTIFY whose row is gone or unreadable); a rotation's overlap when only overlap is set. It returns the
// revocation to emit, if this is the first the view learns of it.
func (r *Revocations) markKey(id string, at *time.Time, reason string, overlap *time.Time) []Revocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.markKeyLocked(id, at, reason, overlap)
}

func (r *Revocations) markKeyLocked(id string, at *time.Time, reason string, overlap *time.Time) []Revocation {
	st := r.keys[id]
	if st == nil {
		st = &keyState{}
		r.keys[id] = st
	}
	if overlap != nil {
		o := *overlap
		st.overlapUntil = &o
	}
	if reason != "" {
		st.reason = reason
	}
	switch {
	case at != nil:
		t := *at
		st.revokedAt, st.fromPayload = &t, false
	case overlap == nil && st.revokedAt == nil:
		t := r.clock()
		st.revokedAt = &t
	}
	if st.revokedAt == nil || st.emitted {
		return nil
	}
	st.emitted = true
	return []Revocation{{KeyID: id, Reason: st.reason}}
}

func (r *Revocations) markBinding(ref bindingRef, at *time.Time) []Revocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.markBindingLocked(ref, at)
}

// markBindingLocked records a binding's revocation: at is its row's revoked_at, nil when the row is gone
// or unreadable. It returns the revocation to emit, if the view has not emitted this one yet.
//
// A binding can be revoked, enrolled again and revoked again, and an enrollment sends no NOTIFY: until a
// re-read forgets the first revocation, the view still holds it when the second arrives. So the row's
// revoked_at tells them apart: a held revocation whose time differs from the row's is a new one, emitted
// again. A revocation held from its NOTIFY alone (no time yet) that a row then dates is taken as the same
// revocation: telling it from a later one would need the NOTIFY to carry the time, and the window (a
// revocation whose row could not be read, enrolled and revoked again before the next successful read) is
// one the registry already covers by reading the rows itself while the view is not Fresh.
func (r *Revocations) markBindingLocked(ref bindingRef, at *time.Time) []Revocation {
	st := r.bindings[ref]
	switch {
	case st == nil:
		st = &bindingState{at: r.clock()}
		if at != nil {
			st.at, st.exact = *at, true
		}
		r.bindings[ref] = st
	case at == nil:
		// Learned again, without its time.
	case !st.exact:
		st.at, st.exact = *at, true
	case !st.at.Equal(*at):
		st.at, st.emitted = *at, false // revoked again after an enrollment the view had not seen
	}
	if st.emitted {
		return nil
	}
	st.emitted = true
	return []Revocation{{KeyID: ref.key, Service: ref.service}}
}

// clock is the database's clock of the last read when there was one, else this process's: it only decides
// when an entry is forgotten. Called with r.mu held.
func (r *Revocations) clock() time.Time {
	if !r.watermark.IsZero() {
		return r.watermark
	}
	return r.now()
}

// emit calls every subscriber with each revocation, one at a time, outside the view's lock.
func (r *Revocations) emit(revs []Revocation) {
	if len(revs) == 0 {
		return
	}
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	r.mu.Lock()
	subs := append([]func(Revocation){}, r.subs...)
	r.mu.Unlock()
	for _, rev := range revs {
		for _, fn := range subs {
			r.call(fn, rev)
		}
	}
}

func (r *Revocations) call(fn func(Revocation), rev Revocation) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("node revocations: a subscriber panicked", "key_id", rev.KeyID, "service", rev.Service, "panic", p)
		}
	}()
	fn(rev)
}

// Catchup re-reads revocations since the last read (pg.Listener.OnReconnect: notifications sent while the
// LISTEN connection was down are lost). It runs synchronously, bounded by a few seconds.
func (r *Revocations) Catchup() {
	if err := r.refresh(context.Background()); err != nil {
		r.log.Warn("node revocations: catch-up read failed", "error", err)
	}
}

// Subscribe registers fn for every revocation learned, by NOTIFY or re-read, once each (a binding revoked,
// enrolled again and revoked again is two revocations). fn runs on the goroutine that learned it (the
// view's NOTIFY reader, Run's, or Catchup's caller's), one revocation at a time, and must not block for
// long.
func (r *Revocations) Subscribe(fn func(Revocation)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, fn)
}

// KeyUsable reports whether a key may carry a new stream at now: not revoked and, when rotated, still
// inside its overlap (relay.md §6.1). A key the view does not hold is usable as far as it knows; while it
// is not Fresh, the caller reads the row (Usable).
func (r *Revocations) KeyUsable(keyID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.keys[keyID]
	switch {
	case st == nil:
		return true
	case st.revokedAt != nil:
		return false
	case st.overlapUntil != nil && !now.Before(*st.overlapUntil):
		return false
	}
	return true
}

// BindingRevoked reports whether the key's binding to service was revoked. A binding enrolled again (an
// enrollment sends no NOTIFY) is reported revoked until the next re-read forgets it (at most 3 s while
// re-reads succeed): fail closed.
func (r *Revocations) BindingRevoked(keyID, service string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.bindings[bindingRef{keyID, service}]
	return ok
}

// Fresh reports whether the last successful re-read began less than 5 s ago; while it is not, the registry
// reads the key's row before opening a stream.
func (r *Revocations) Fresh() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.lastOK.IsZero() && r.now().Sub(r.lastOK) < r.interval
}

// Usable reads the rows of a key and of its binding to service (when service is not empty) and reports
// whether a new stream may use them: the key exists, is not revoked, is not past a rotation's overlap, and
// the binding exists and is not revoked. It is what the registry asks while the view is not Fresh.
func (r *Revocations) Usable(ctx context.Context, keyID, service string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		select exists (select 1 from kontra.tunnel_keys k
		                where k.key_id = $1 and k.revoked_at is null and (k.overlap_until is null or k.overlap_until > now()))
		   and ($2 = '' or exists (select 1 from kontra.key_services ks
		                            where ks.key_id = $1 and ks.service = $2 and ks.revoked_at is null))`,
		keyID, service).Scan(&ok)
	return ok, err
}

// refresh reads the keys revoked since the previous read (less the overlap), or within the retention on
// the first read, every rotated key inside its overlap, and the bindings revoked over the same window; it
// re-checks the bindings held as revoked and forgets those enrolled again, forgets entries older than the
// retention, and emits what it learned. Its start (this process's clock, taken before any statement's
// snapshot) is what Fresh counts from.
func (r *Revocations) refresh(ctx context.Context) error {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, revocationReadTimeout)
	defer cancel()

	r.mu.Lock()
	started := r.now()
	var since *time.Time
	if !r.watermark.IsZero() {
		s := r.watermark.Add(-revocationOverlap)
		since = &s
	}
	var heldKeys, heldServices []string
	held := make(map[bindingRef]bindingState, len(r.bindings))
	for ref, st := range r.bindings {
		heldKeys = append(heldKeys, ref.key)
		heldServices = append(heldServices, ref.service)
		held[ref] = *st
	}
	r.mu.Unlock()

	type keyRow struct {
		id          string
		at, overlap *time.Time
		reason      string
	}
	rows, err := r.db.Query(ctx, `
		with t as (select statement_timestamp() as now)
		select t.now, k.key_id, k.revoked_at, coalesce(k.revoked_reason, ''), k.overlap_until
		  from t left join kontra.tunnel_keys k
		    on k.revoked_at > coalesce($1::timestamptz, t.now - make_interval(secs => $2))
		    or (k.revoked_at is null and k.overlap_until is not null)`, since, r.retain.Seconds())
	if err != nil {
		return err
	}
	var (
		now  time.Time
		keys []keyRow
	)
	for rows.Next() {
		var (
			id     *string
			reason string
			k      keyRow
		)
		if err := rows.Scan(&now, &id, &k.at, &reason, &k.overlap); err != nil {
			rows.Close()
			return err
		}
		if id != nil {
			k.id, k.reason = *id, reason
			keys = append(keys, k)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	lower := now.Add(-r.retain)
	if since != nil {
		lower = *since
	}
	rows, err = r.db.Query(ctx, `select key_id, service, revoked_at from kontra.key_services where revoked_at > $1`, lower)
	if err != nil {
		return err
	}
	type bindingRow struct {
		ref bindingRef
		at  time.Time
	}
	bindings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (bindingRow, error) {
		var b bindingRow
		err := row.Scan(&b.ref.key, &b.ref.service, &b.at)
		return b, err
	})
	if err != nil {
		return err
	}
	var cleared []bindingRef
	if len(heldKeys) > 0 {
		rows, err = r.db.Query(ctx, `
			select u.k, u.s from unnest($1::text[], $2::text[]) as u(k, s)
			 where exists (select 1 from kontra.key_services ks where ks.key_id = u.k and ks.service = u.s and ks.revoked_at is null)`,
			heldKeys, heldServices)
		if err != nil {
			return err
		}
		cleared, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (bindingRef, error) {
			var b bindingRef
			err := row.Scan(&b.key, &b.service)
			return b, err
		})
		if err != nil {
			return err
		}
	}

	var learned []Revocation
	r.mu.Lock()
	inOverlap := map[string]bool{}
	for _, k := range keys {
		if k.at == nil {
			inOverlap[k.id] = true
		}
		learned = append(learned, r.markKeyLocked(k.id, k.at, k.reason, k.overlap)...)
	}
	for id, st := range r.keys {
		switch {
		case st.revokedAt == nil && !inOverlap[id]:
			delete(r.keys, id) // no longer a rotated key in its overlap (its row is gone)
		case st.revokedAt != nil && now.Sub(*st.revokedAt) > r.retain:
			delete(r.keys, id)
		}
	}
	for _, ref := range cleared {
		// Forget a binding enrolled again, unless the view learned a revocation of it since the snapshot
		// above (a NOTIFY applied meanwhile): the read that cleared it may predate that revocation.
		if st := r.bindings[ref]; st != nil && st.at.Equal(held[ref].at) && st.exact == held[ref].exact {
			delete(r.bindings, ref)
		}
	}
	for _, b := range bindings {
		learned = append(learned, r.markBindingLocked(b.ref, &b.at)...)
	}
	for ref, st := range r.bindings {
		if now.Sub(st.at) > r.retain {
			delete(r.bindings, ref)
		}
	}
	r.watermark = now
	r.lastOK = started
	r.mu.Unlock()
	r.emit(learned)
	return nil
}
