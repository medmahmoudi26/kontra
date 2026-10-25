package auth

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RevocationCache is the replica's view of revoked API keys (and, from M2, node keys and key-service
// bindings) (spec §2.3). It is refreshed by NOTIFY kontra_revoke, re-read from Postgres (what was revoked
// since the last read) at least every RefreshInterval and whenever the LISTEN connection reconnects;
// NOTIFY only shortens the delay.
//
// It holds the API keys revoked recently enough that a verified key APIKeys still answers from memory
// (CacheTTL) may be one of them: older revocations are read from the key's own row whenever the key is
// looked up again. Re-reads overlap the previous one by readOverlap, so a revocation whose transaction
// committed after the read that should have seen it is still picked up.
//
// A revoked_at is not the only way a key ends, and its stamp need not be recent (an operator revoking "as
// of the leak", a long transaction): each re-read therefore also re-reads the rows of every key APIKeys
// holds in memory, and drops the ones that no longer authenticate (revoked, expired, deleted, or of a
// deleted or expired tenant). So a verified key is answered from memory only while the last successful
// re-read began less than RefreshInterval ago, and every key in memory was either checked by that re-read
// or read from its row after it began.
type RevocationCache struct {
	db       *pgxpool.Pool
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time // tests

	mu        sync.Mutex
	revoked   map[uuid.UUID]time.Time // api key id → revoked_at (the database's clock)
	watermark time.Time               // the database's clock at the start of the last successful read
	lastOK    time.Time               // this process's clock at the start of the last successful read
	seq       uint64                  // re-reads started
	caches    []keyCache              // the verified-key caches each re-read checks
	kick      chan struct{}
	readMu    sync.Mutex // one read at a time
}

// keyCache is a cache of verified API keys that the re-reads keep honest (APIKeys).
type keyCache interface {
	// cachedKeyIDs lists the keys it may answer from memory.
	cachedKeyIDs() []uuid.UUID
	// dropKeys forgets these keys.
	dropKeys(ids []uuid.UUID)
}

// RefreshInterval is the longest a revocation takes to reach a replica without NOTIFY.
const RefreshInterval = 5 * time.Second

const (
	// readOverlap re-reads revocations this far behind the previous read's clock (a revocation is stamped
	// when its statement runs and becomes visible when its transaction commits).
	readOverlap = time.Minute
	// retain keeps a revocation in memory this long after it happened: longer than any verified key is
	// answered from memory (CacheTTL), plus the overlap.
	retain = CacheTTL + 2*readOverlap
	// readTimeout bounds one re-read.
	readTimeout = 5 * time.Second
)

// Payload prefixes of NOTIFY kontra_revoke (spec/tunnel.md §12).
const (
	revokeAPIKey  = "apikey:"
	revokeNodeKey = "key:"     // M2
	revokeBinding = "binding:" // M2
)

// NewRevocationCache returns an empty cache over db.
func NewRevocationCache(db *pgxpool.Pool) *RevocationCache {
	return &RevocationCache{
		db:       db,
		interval: RefreshInterval,
		log:      slog.Default(),
		now:      time.Now,
		revoked:  map[uuid.UUID]time.Time{},
		kick:     make(chan struct{}, 1),
	}
}

// track adds a verified-key cache to the ones every re-read checks.
func (c *RevocationCache) track(kc keyCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caches = append(c.caches, kc)
}

// started is the number of re-reads started so far. APIKeys reads it before it reads a key's row and keeps
// the answer in memory only if it is unchanged then (under its own lock, which a re-read takes to list
// the keys after counting itself): a key whose row was read before a re-read began is then always among
// the keys that re-read checks.
func (c *RevocationCache) started() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// Run keeps the cache current until ctx ends: it reads at once, then every 3/5 of RefreshInterval (so a
// read's own time, and a busy host, stay inside the bound), and whenever a NOTIFY asks for it. A
// failed read is logged and retried at the next tick; once no read has succeeded for RefreshInterval,
// Fresh turns false and APIKeys stops answering from memory. Run returns nil when ctx ends.
func (c *RevocationCache) Run(ctx context.Context) error {
	period := c.interval * 3 / 5
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		if err := c.refresh(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("revocation cache: re-read failed; verified keys are read from Postgres until it succeeds", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-c.kick:
		}
	}
}

// Notify applies one kontra_revoke payload (pg.Listener.OnChannel). "apikey:<id>" marks the key revoked
// at once (the NOTIFY is delivered only when the revoking transaction commits) and asks Run for a re-read.
// Node-key and binding payloads are the tunnel's (M2) and are ignored here; anything else is ignored too
// (the table is the truth).
func (c *RevocationCache) Notify(payload string) {
	switch {
	case strings.HasPrefix(payload, revokeAPIKey):
		id, err := uuid.Parse(strings.TrimPrefix(payload, revokeAPIKey))
		if err != nil {
			c.log.Debug("revocation cache: unreadable payload", "payload", payload)
			return
		}
		c.markAPIKey(id)
		select {
		case c.kick <- struct{}{}:
		default:
		}
	case strings.HasPrefix(payload, revokeNodeKey), strings.HasPrefix(payload, revokeBinding):
		// M2: the tunnel's revocations.
	}
}

// Catchup re-reads revocations since the last read (pg.Listener.OnReconnect: notifications sent while
// the LISTEN connection was down are lost). It runs synchronously, bounded by a few seconds.
func (c *RevocationCache) Catchup() {
	if err := c.refresh(context.Background()); err != nil {
		c.log.Warn("revocation cache: catch-up read failed", "error", err)
	}
}

// APIKeyRevoked reports whether the key with this id was revoked as of the last read or NOTIFY. Expiry
// is not tracked here: APIKeys checks each key's expires_at on every request.
func (c *RevocationCache) APIKeyRevoked(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.revoked[id]
	return ok
}

// Fresh reports whether the last successful read began less than RefreshInterval ago, so a verified key
// may be answered from memory: a key revoked, expired or deleted before that read began is no longer in
// memory, and one ended since has ended less than RefreshInterval ago (spec §2.3: re-read at least every
// 5 s). When re-reads fail, every request reads Postgres once RefreshInterval has passed.
func (c *RevocationCache) Fresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.lastOK.IsZero() && c.now().Sub(c.lastOK) < c.interval
}

// markAPIKey records a revocation learned without a read (a NOTIFY, or Revoke on this replica). Its time
// is the database clock of the last read, or this process's clock before any; it only decides when the
// entry is dropped.
func (c *RevocationCache) markAPIKey(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.watermark
	if at.IsZero() {
		at = time.Now()
	}
	if _, ok := c.revoked[id]; !ok {
		c.revoked[id] = at
	}
}

// refresh reads the API keys revoked since the previous read (less the overlap), or within retain on the
// first read, and drops entries older than retain; then it re-reads the rows of the keys held in memory
// and has the caches drop those that no longer authenticate. Its start (this process's clock, taken before
// either statement's snapshot) is what Fresh counts from.
func (c *RevocationCache) refresh(ctx context.Context) error {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	c.mu.Lock()
	c.seq++
	started := c.now()
	var since *time.Time
	if !c.watermark.IsZero() {
		s := c.watermark.Add(-readOverlap)
		since = &s
	}
	caches := slices.Clone(c.caches)
	c.mu.Unlock()
	var held []uuid.UUID
	for _, kc := range caches {
		held = append(held, kc.cachedKeyIDs()...)
	}

	rows, err := c.db.Query(ctx, `
		with t as (select statement_timestamp() as now)
		select t.now, k.id, k.revoked_at
		from t left join kontra.api_keys k
		  on k.revoked_at > coalesce($1::timestamptz, t.now - make_interval(secs => $2))`,
		since, retain.Seconds())
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		now   time.Time
		found = map[uuid.UUID]time.Time{}
	)
	for rows.Next() {
		var (
			id *uuid.UUID
			at *time.Time
		)
		if err := rows.Scan(&now, &id, &at); err != nil {
			return err
		}
		if id != nil && at != nil {
			found[*id] = *at
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	ended, err := c.endedKeys(ctx, held)
	if err != nil {
		return err
	}
	if len(ended) > 0 {
		for _, kc := range caches {
			kc.dropKeys(ended)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for id, at := range found {
		c.revoked[id] = at
	}
	for id, at := range c.revoked {
		if now.Sub(at) > retain {
			delete(c.revoked, id)
		}
	}
	c.watermark = now
	c.lastOK = started
	return nil
}

// endedKeys returns those of ids that no longer authenticate: no row, revoked, expired, or of a tenant
// deleted or expired (the conditions of APIKeys.Authenticate).
func (c *RevocationCache) endedKeys(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := c.db.Query(ctx, `
		select u.id from unnest($1::uuid[]) as u(id)
		where not exists (
			select 1 from kontra.api_keys k join kontra.tenants t on t.id = k.tenant_id
			where k.id = u.id and k.revoked_at is null and (k.expires_at is null or k.expires_at > now())
			  and t.deleted_at is null and (t.expires_at is null or t.expires_at > now()))`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
