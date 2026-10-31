package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/pg"
)

// ErrUnauthenticated is a missing, unknown, revoked or expired credential: HTTP 401 on every route. Any
// other error from Authenticate is the gate's own (Postgres unreachable) and is not the caller's fault.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrKeyNotFound is Revoke's error for an id no key has.
var ErrKeyNotFound = errors.New("auth: no such API key")

// API key format (spec §2.3, D68): kk_<operator prefix>_<random>.
const (
	// KeyLiteral starts every key the gate mints.
	KeyLiteral = "kk_"
	// MaxOperatorPrefix is the longest operator prefix ([a-z0-9]).
	MaxOperatorPrefix = 16
	// RandomLen is the length of the random part, base62 (about 190 bits).
	RandomLen = 32
	// ShownRandom is how much of the random part the stored prefix keeps; the other 24 characters (about
	// 143 bits) are never shown.
	ShownRandom = 8
	// DefaultOperatorPrefix is the operator prefix of a key whose name has no [a-z0-9] character.
	DefaultOperatorPrefix = "k"
)

// Authentication bounds.
const (
	// MaxBearer is the longest bearer value looked up; anything longer is 401 without a query.
	MaxBearer = 4096
	// CacheTTL is how long a verified key is answered from memory before Postgres is read again. A
	// revocation, an expiry or a deletion (of the key or its tenant) still applies within RefreshInterval:
	// the revocation cache is checked on every request, each of its re-reads re-reads the rows of the keys
	// held in memory, and the key cache is bypassed whenever that cache is not current.
	CacheTTL = 5 * time.Minute
	// CacheMax bounds the verified keys kept in memory.
	CacheMax = 10000
	// LastUsedEvery is the most often last_used_at is written for one key, by this replica and (the
	// update's own guard) by all replicas together.
	LastUsedEvery = time.Minute
	// writeTimeout bounds a mint, a revocation and a last_used_at write, on the server and the client.
	writeTimeout = 10 * time.Second
	// mintAttempts is how many fresh keys Mint tries when a prefix collides.
	mintAttempts = 5
)

var operatorPrefixRE = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// GateKeyRE is the shape of a key the gate mints. Keys an operator writes itself (Cloud) may look like
// anything api_keys.prefix accepts (D37), so it is never used to refuse a bearer.
var GateKeyRE = regexp.MustCompile(`^kk_[a-z0-9]{1,16}_[A-Za-z0-9]{32}$`)

// OperatorPrefix is the operator prefix `kontra-gate enroll --api-key <name>` uses when it is given no
// --prefix: the name lowercased, reduced to [a-z0-9] and cut to 16 characters, or "k" if that leaves
// nothing (D68).
func OperatorPrefix(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			if b.Len() == MaxOperatorPrefix {
				break
			}
		}
	}
	if b.Len() == 0 {
		return DefaultOperatorPrefix
	}
	return b.String()
}

// CheckOperatorPrefix accepts 1 to 16 characters [a-z0-9].
func CheckOperatorPrefix(p string) error {
	if !operatorPrefixRE.MatchString(p) {
		return fmt.Errorf("prefix %q: want 1 to 16 characters [a-z0-9] (keys are kk_<prefix>_<random>, D68)", p)
	}
	return nil
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// GenerateKey returns a new key kk_<operatorPrefix>_<32 base62 characters> read from r (crypto/rand when
// nil), and its stored prefix: everything before the random part and the random part's first 8
// characters, at most 28 characters.
func GenerateKey(r io.Reader, operatorPrefix string) (secret, prefix string, err error) {
	if err := CheckOperatorPrefix(operatorPrefix); err != nil {
		return "", "", err
	}
	if r == nil {
		r = rand.Reader
	}
	random := make([]byte, 0, RandomLen)
	var buf [64]byte
	for len(random) < RandomLen {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", "", fmt.Errorf("auth: key randomness: %w", err)
		}
		for _, c := range buf {
			// 248 = 4 × 62: rejecting the top 8 values keeps every character equally likely.
			if c < 248 && len(random) < RandomLen {
				random = append(random, base62[c%62])
			}
		}
	}
	head := KeyLiteral + operatorPrefix + "_"
	return head + string(random), head + string(random[:ShownRandom]), nil
}

// KeyHash is the stored form of a key, sha256 of its bytes (api_keys.hash).
func KeyHash(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

// APIKeys authenticates and mints API keys over kontra.api_keys (sha256 of the key, prefix shown).
type APIKeys struct {
	db      *pgxpool.Pool
	revoked *RevocationCache
	log     *slog.Logger

	// Tests replace these.
	rand          io.Reader
	now           func() time.Time
	ttl           time.Duration
	lastUsedEvery time.Duration

	mu       sync.Mutex
	cache    map[[32]byte]*cachedKey
	lastUsed map[uuid.UUID]time.Time // when this replica last wrote, or tried to write, last_used_at
	held     map[uuid.UUID]*heldKey  // the hiring API keys of tunnel sessions (Hold)
	writes   sync.WaitGroup
}

// heldKey is a key a tunnel session hires with (Hold): every re-read of the revocation cache checks it, as it
// checks the keys in cache, and one found ended stays ended while it is held.
type heldKey struct {
	n         int // the sessions holding it
	ended     bool
	p         Principal  // as last read from its row; valid when loaded
	expiresAt *time.Time // the key's, or its tenant's when earlier
	loaded    bool
}

type cachedKey struct {
	p         Principal
	expiresAt *time.Time
	loaded    time.Time
}

// NewAPIKeys returns the API key authenticator. Revocation is checked on every request through revoked,
// whose re-reads also re-read the rows of the keys this authenticator holds in memory; with revoked nil
// (enroll, tests) no key is answered from memory and every request reads Postgres.
func NewAPIKeys(db *pgxpool.Pool, revoked *RevocationCache) *APIKeys {
	k := &APIKeys{
		db:            db,
		revoked:       revoked,
		log:           slog.Default(),
		now:           time.Now,
		ttl:           CacheTTL,
		lastUsedEvery: LastUsedEvery,
		cache:         map[[32]byte]*cachedKey{},
		lastUsed:      map[uuid.UUID]time.Time{},
		held:          map[uuid.UUID]*heldKey{},
	}
	if revoked != nil {
		revoked.track(k)
	}
	return k
}

// Authenticate maps a bearer key to its principal key:<prefix>, with the key's tenant, buyer and scopes.
// An unknown, revoked or expired key (or one whose tenant is deleted or expired) is ErrUnauthenticated.
// last_used_at is written at most once a minute, after the answer, off the request's path.
func (k *APIKeys) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if bearer == "" || len(bearer) > MaxBearer {
		return Principal{}, ErrUnauthenticated
	}
	h := KeyHash(bearer)
	now := k.now()
	if k.revoked != nil && k.revoked.Fresh() {
		k.mu.Lock()
		ck := k.cache[h]
		k.mu.Unlock()
		if ck != nil && now.Sub(ck.loaded) < k.ttl {
			if k.revoked.APIKeyRevoked(*ck.p.KeyID) || expired(ck.expiresAt, now) {
				k.forget(h)
				return Principal{}, ErrUnauthenticated
			}
			k.touch(*ck.p.KeyID, now)
			return clonePrincipal(ck.p), nil
		}
	}

	var (
		id, tenantID, buyer   uuid.UUID
		slug, prefix          string
		scopes                []string
		expiresAt, tenantEnds *time.Time
		revokedAt             *time.Time
		reads                 uint64
	)
	if k.revoked != nil {
		reads = k.revoked.started() // before the row is read: see remember
	}
	err := k.db.QueryRow(ctx, `
		select k.id, k.tenant_id, t.slug, k.buyer_id, k.prefix, k.scopes, k.expires_at, t.expires_at, k.revoked_at
		from kontra.api_keys k join kontra.tenants t on t.id = k.tenant_id
		where k.hash = $1 and t.deleted_at is null and (t.expires_at is null or t.expires_at > now())`,
		h[:]).Scan(&id, &tenantID, &slug, &buyer, &prefix, &scopes, &expiresAt, &tenantEnds, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: api key lookup: %w", err)
	}
	// The key ends with its tenant (an anonymous tenant expires, spec/tunnel.md §6).
	if tenantEnds != nil && (expiresAt == nil || tenantEnds.Before(*expiresAt)) {
		expiresAt = tenantEnds
	}
	if revokedAt != nil || expired(expiresAt, now) || (k.revoked != nil && k.revoked.APIKeyRevoked(id)) {
		k.forget(h)
		return Principal{}, ErrUnauthenticated
	}
	p := Principal{
		Kind:       KindKey,
		ID:         string(KindKey) + ":" + prefix,
		TenantID:   &tenantID,
		TenantSlug: slug,
		BuyerID:    buyer,
		Via:        ViaAPIKey,
		KeyID:      &id,
	}
	for _, s := range scopes {
		p.Scopes = append(p.Scopes, Scope(s))
	}
	if k.revoked != nil {
		k.remember(h, &cachedKey{p: clonePrincipal(p), expiresAt: expiresAt, loaded: now}, reads)
	}
	k.touch(id, now)
	return p, nil
}

func expired(at *time.Time, now time.Time) bool { return at != nil && !now.Before(*at) }

func clonePrincipal(p Principal) Principal {
	p.Scopes = slices.Clone(p.Scopes)
	if p.TenantID != nil {
		t := *p.TenantID
		p.TenantID = &t
	}
	if p.KeyID != nil {
		id := *p.KeyID
		p.KeyID = &id
	}
	return p
}

// remember keeps a verified key in memory, unless a re-read of the revocation cache began after its row
// was read (reads is the count of re-reads started before that): such a re-read may have listed the keys
// in memory before this one joined them, and so not checked it, and this row may predate a change it saw.
// The count is compared under k.mu, which a re-read takes to list the keys after counting itself.
func (k *APIKeys) remember(h [32]byte, ck *cachedKey, reads uint64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.revoked.started() != reads {
		return
	}
	if len(k.cache) >= CacheMax {
		for kh, c := range k.cache {
			if ck.loaded.Sub(c.loaded) >= k.ttl {
				delete(k.cache, kh)
			}
		}
		for kh := range k.cache {
			if len(k.cache) < CacheMax {
				break
			}
			delete(k.cache, kh)
		}
	}
	k.cache[h] = ck
}

func (k *APIKeys) forget(h [32]byte) {
	k.mu.Lock()
	delete(k.cache, h)
	k.mu.Unlock()
}

// forgetKey drops every cached entry of one key id (a revocation on this replica).
func (k *APIKeys) forgetKey(id uuid.UUID) { k.dropKeys([]uuid.UUID{id}) }

// cachedKeyIDs lists the keys that may be answered from memory (those within the TTL, and the held keys not
// ended), for the revocation cache's re-read.
func (k *APIKeys) cachedKeyIDs() []uuid.UUID {
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(k.cache)+len(k.held))
	for _, c := range k.cache {
		if c.p.KeyID != nil && now.Sub(c.loaded) < k.ttl {
			ids = append(ids, *c.p.KeyID)
		}
	}
	for id, h := range k.held {
		if !h.ended {
			ids = append(ids, id)
		}
	}
	return ids
}

// dropKeys drops every cached entry of these key ids (a revocation on this replica, or keys a re-read
// found ended), and marks those of them that are held ended.
func (k *APIKeys) dropKeys(ids []uuid.UUID) {
	drop := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for h, c := range k.cache {
		if c.p.KeyID != nil && drop[*c.p.KeyID] {
			delete(k.cache, h)
		}
	}
	for id := range drop {
		if h := k.held[id]; h != nil {
			h.ended = true
		}
	}
}

// Hold keeps the key of p, a principal Authenticate returned (a tunnel session's hiring API key, spec/tunnel.md
// §9), under the revocation cache's watch until release is called: every re-read checks its row, as it checks
// the keys answered from memory, so Revalidate can answer from memory while the cache is fresh, and a key found
// revoked, expired or deleted stays refused while it is held, however long ago that happened. A principal that
// is not an API key's holds nothing. release is safe to call more than once.
func (k *APIKeys) Hold(p Principal) (release func()) {
	if p.Kind != KindKey || p.KeyID == nil {
		return func() {}
	}
	id := *p.KeyID
	k.mu.Lock()
	h := k.held[id]
	if h == nil {
		h = &heldKey{}
		k.held[id] = h
	}
	h.n++
	k.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			if h := k.held[id]; h != nil {
				if h.n--; h.n <= 0 {
					delete(k.held, id)
				}
			}
		})
	}
}

// Revalidate re-validates p, a principal Authenticate returned earlier (a tunnel session's hiring API key), and
// returns it as the key's row says now (its scopes, tenant and buyer): spec/relay.md §2.3's "revocation is
// checked on every request", for the requests a node opens with the key its handshake carried (spec/tunnel.md
// §12). A key revoked (by NOTIFY or a re-read), expired, deleted, or of a tenant that ended is
// ErrUnauthenticated; any other error is the gate's own (Postgres unreachable).
//
// A held key (Hold) is answered from memory while the revocation cache is fresh, after one read of its row;
// otherwise, and for a key not held, its row is read.
func (k *APIKeys) Revalidate(ctx context.Context, p Principal) (Principal, error) {
	if p.Kind != KindKey || p.KeyID == nil {
		return Principal{}, ErrUnauthenticated
	}
	id := *p.KeyID
	now := k.now()
	if k.revoked != nil && k.revoked.APIKeyRevoked(id) {
		k.dropKeys([]uuid.UUID{id})
		return Principal{}, ErrUnauthenticated
	}
	k.mu.Lock()
	h := k.held[id]
	var (
		ended, fromMemory bool
		mem               Principal
	)
	if h != nil {
		ended = h.ended
		if !ended && h.loaded && k.revoked != nil && k.revoked.Fresh() && !expired(h.expiresAt, now) {
			fromMemory, mem = true, clonePrincipal(h.p)
		}
	}
	k.mu.Unlock()
	switch {
	case ended:
		return Principal{}, ErrUnauthenticated
	case fromMemory:
		return mem, nil
	}

	var (
		tenantID, buyer       uuid.UUID
		slug, prefix          string
		scopes                []string
		expiresAt, tenantEnds *time.Time
		revokedAt             *time.Time
		reads                 uint64
	)
	if k.revoked != nil {
		reads = k.revoked.started() // before the row is read, as remember does
	}
	err := k.db.QueryRow(ctx, `
		select k.tenant_id, t.slug, k.buyer_id, k.prefix, k.scopes, k.expires_at, t.expires_at, k.revoked_at
		from kontra.api_keys k join kontra.tenants t on t.id = k.tenant_id
		where k.id = $1 and t.deleted_at is null and (t.expires_at is null or t.expires_at > now())`,
		id).Scan(&tenantID, &slug, &buyer, &prefix, &scopes, &expiresAt, &tenantEnds, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		k.dropKeys([]uuid.UUID{id})
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: api key re-read: %w", err)
	}
	if tenantEnds != nil && (expiresAt == nil || tenantEnds.Before(*expiresAt)) {
		expiresAt = tenantEnds
	}
	if revokedAt != nil || expired(expiresAt, now) || (k.revoked != nil && k.revoked.APIKeyRevoked(id)) {
		k.dropKeys([]uuid.UUID{id})
		return Principal{}, ErrUnauthenticated
	}
	out := Principal{
		Kind:       KindKey,
		ID:         string(KindKey) + ":" + prefix,
		TenantID:   &tenantID,
		TenantSlug: slug,
		BuyerID:    buyer,
		Via:        ViaAPIKey,
		KeyID:      &id,
	}
	for _, s := range scopes {
		out.Scopes = append(out.Scopes, Scope(s))
	}
	k.mu.Lock()
	if h := k.held[id]; h != nil && !h.ended && k.revoked != nil && k.revoked.started() == reads {
		h.p, h.expiresAt, h.loaded = clonePrincipal(out), expiresAt, true
	}
	k.mu.Unlock()
	k.touch(id, now)
	return out, nil
}

// touch writes last_used_at for id unless this replica wrote (or tried to) within lastUsedEvery. The
// update's own guard keeps the column to one write a minute across replicas.
func (k *APIKeys) touch(id uuid.UUID, now time.Time) {
	k.mu.Lock()
	if last, ok := k.lastUsed[id]; ok && now.Sub(last) < k.lastUsedEvery {
		k.mu.Unlock()
		return
	}
	k.lastUsed[id] = now
	if len(k.lastUsed) > 2*CacheMax {
		for kid, t := range k.lastUsed {
			if now.Sub(t) >= k.lastUsedEvery {
				delete(k.lastUsed, kid)
			}
		}
	}
	k.mu.Unlock()
	k.writes.Add(1)
	go func() {
		defer k.writes.Done()
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		_, err := k.db.Exec(ctx, `update kontra.api_keys set last_used_at = now()
			where id = $1 and (last_used_at is null or last_used_at < now() - make_interval(secs => $2))`,
			id, k.lastUsedEvery.Seconds())
		if err != nil {
			k.log.Debug("api key last_used_at not written", "api_key_id", id, "error", err)
		}
	}()
}

// Wait returns when every last_used_at write started so far has finished (shutdown, tests).
func (k *APIKeys) Wait() { k.writes.Wait() }

// MintRequest is what `kontra-gate enroll --api-key` asks for.
type MintRequest struct {
	TenantID  uuid.UUID
	Name      string
	BuyerID   *uuid.UUID // nil mints a new buyer
	Scopes    []Scope    // default [hire]
	ExpiresAt *time.Time
	// Prefix is the operator prefix, 1 to 16 characters [a-z0-9], so key: globs can name a group of keys
	// (key:kk_<prefix>_*); empty derives it from Name (OperatorPrefix).
	Prefix string
	// Actor is who mints, for the audit row (default "kontra-gate enroll").
	Actor string
}

// Minted is a new key. Secret is shown once and never stored; only its sha256 is.
type Minted struct {
	ID       uuid.UUID
	Prefix   string // api_keys.prefix; the principal is key:<Prefix>
	Secret   string
	BuyerID  uuid.UUID
	TenantID uuid.UUID
	Scopes   []Scope
}

// Principal is the minted key's principal id.
func (m Minted) Principal() string { return string(KindKey) + ":" + m.Prefix }

// MaxKeyName bounds api_keys.name.
const MaxKeyName = 128

// CheckKeyName accepts 1 to 128 characters of printable UTF-8.
func CheckKeyName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > MaxKeyName || !utf8.ValidString(name) {
		return fmt.Errorf("key name %q: want 1 to %d characters", name, MaxKeyName)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("key name %q: control or non-printing character", name)
		}
	}
	return nil
}

// Mint creates a key in one transaction with its audit row, minting again on a prefix collision
// (spec §2.3). The secret is returned once; api_keys keeps its sha256 and its prefix.
func (k *APIKeys) Mint(ctx context.Context, req MintRequest) (Minted, error) {
	if err := CheckKeyName(req.Name); err != nil {
		return Minted{}, err
	}
	op := req.Prefix
	if op == "" {
		op = OperatorPrefix(req.Name)
	}
	if err := CheckOperatorPrefix(op); err != nil {
		return Minted{}, err
	}
	scopes := []Scope{ScopeHire}
	if len(req.Scopes) > 0 {
		scopes = nil
		for _, s := range req.Scopes {
			if _, err := ParseScope(string(s)); err != nil {
				return Minted{}, err
			}
			if !slices.Contains(scopes, s) {
				scopes = append(scopes, s)
			}
		}
	}
	if req.TenantID == uuid.Nil {
		return Minted{}, errors.New("auth: mint: a key needs a tenant")
	}
	buyer := uuid.Nil
	if req.BuyerID != nil {
		buyer = *req.BuyerID
	} else {
		b, err := uuid.NewV7()
		if err != nil {
			return Minted{}, err
		}
		buyer = b
	}
	actor := req.Actor
	if actor == "" {
		actor = "kontra-gate enroll"
	}
	scopeText := make([]string, len(scopes))
	for i, s := range scopes {
		scopeText[i] = string(s)
	}

	for attempt := 0; ; attempt++ {
		secret, prefix, err := GenerateKey(k.rand, op)
		if err != nil {
			return Minted{}, err
		}
		if !config.KeyPrefixRE.MatchString(prefix) {
			return Minted{}, fmt.Errorf("auth: mint: prefix %q does not fit api_keys.prefix", prefix)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return Minted{}, err
		}
		h := KeyHash(secret)
		err = k.inTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into kontra.api_keys (id, tenant_id, buyer_id, name, prefix, hash, scopes, expires_at)
				values ($1, $2, $3, $4, $5, $6, $7, $8)`, id, req.TenantID, buyer, req.Name, prefix, h[:], scopeText, req.ExpiresAt)
			if err != nil {
				return err
			}
			_, err = audit.Append(ctx, tx, audit.EventAPIKeyMinted, audit.Fields{
				"actor":      actor,
				"key_id":     id,
				"principal":  string(KindKey) + ":" + prefix,
				"name":       req.Name,
				"tenant_id":  req.TenantID,
				"buyer_id":   buyer,
				"scopes":     scopeText,
				"expires_at": req.ExpiresAt,
			})
			return err
		})
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			return Minted{ID: id, Prefix: prefix, Secret: secret, BuyerID: buyer, TenantID: req.TenantID, Scopes: scopes}, nil
		case errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			(pgErr.ConstraintName == "api_keys_prefix_key" || pgErr.ConstraintName == "api_keys_hash_key"):
			if attempt+1 >= mintAttempts {
				return Minted{}, fmt.Errorf("auth: mint: %d prefixes collided in a row: %w", mintAttempts, err)
			}
			continue
		case errors.As(err, &pgErr) && pgErr.Code == "23503":
			return Minted{}, fmt.Errorf("auth: mint: tenant %s does not exist", req.TenantID)
		default:
			return Minted{}, fmt.Errorf("auth: mint: %w", err)
		}
	}
}

// Revoke sets the key's revoked_at, appends its audit row and sends NOTIFY kontra_revoke 'apikey:<id>'
// in one transaction (spec §2.3, spec/tunnel.md §12), then applies it on this replica at once. Other
// replicas apply it on the NOTIFY, or at their next re-read (RefreshInterval) when it is lost. A key
// already revoked is left as it is (false, nil); an unknown id is ErrKeyNotFound.
func (k *APIKeys) Revoke(ctx context.Context, id uuid.UUID, actor string) (bool, error) {
	if actor == "" {
		actor = "operator"
	}
	changed := false
	err := k.inTx(ctx, func(tx pgx.Tx) error {
		var (
			prefix   string
			tenantID uuid.UUID
		)
		err := tx.QueryRow(ctx, `update kontra.api_keys set revoked_at = clock_timestamp()
			where id = $1 and revoked_at is null returning prefix, tenant_id`, id).Scan(&prefix, &tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `select exists (select 1 from kontra.api_keys where id = $1)`, id).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrKeyNotFound
			}
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, audit.EventAPIKeyRevoked, audit.Fields{
			"actor":     actor,
			"key_id":    id,
			"principal": string(KindKey) + ":" + prefix,
			"tenant_id": tenantID,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelRevoke), RevokePayload(id)); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return false, err
		}
		return false, fmt.Errorf("auth: revoke %s: %w", id, err)
	}
	if k.revoked != nil && k.revoked.markAPIKey(id) {
		k.revoked.emit([]uuid.UUID{id})
	}
	k.forgetKey(id)
	return changed, nil
}

// RevokePayload is the kontra_revoke payload of an API key revocation, "apikey:<id>".
func RevokePayload(id uuid.UUID) string { return revokeAPIKey + id.String() }

// inTx runs fn in a transaction bounded by writeTimeout on the server (statement_timeout) and on the
// client, detached from the caller's cancellation so a write that started is not cut half way (D8).
func (k *APIKeys) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout+2*time.Second)
	defer cancel()
	return pgx.BeginFunc(ctx, k.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('statement_timeout', $1, true)`,
			fmt.Sprint(writeTimeout.Milliseconds())); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Authenticator turns a request's credential into a principal: an API key, or none (the anonymous
// principal, admitted only by a service whose allow: list says so). On the MCP routes, with a resource server, also a
// JWT of the MCP authorization server (AuthenticateMCP, mcp_oauth.go; M4 WP-06).
type Authenticator struct {
	keys *APIKeys
	mcp  MCPResourceServer // nil without auth.mcp_oauth
}

// NewAuthenticator returns the edge's authenticator. keys nil (auth.api_keys: false) refuses every bearer
// credential with ErrUnauthenticated.
func NewAuthenticator(keys *APIKeys) *Authenticator {
	return &Authenticator{keys: keys}
}

// Authenticate reads Authorization. No credential returns the anonymous principal with the given buyer
// and authenticated=false, so the route can decide (only an allow: list with `anonymous` admits it). A bad
// credential (not exactly one `Bearer <token>`, an unknown, revoked or expired key, or a token that is
// not an API key, such as an OIDC JWT, which M1 does not accept) is ErrUnauthenticated even on an
// anonymous service. Any other error is the gate's own.
//
// A request a node opened on its tunnel session (HireSourceFrom) takes the session's principal and nothing
// else: Authorization is not read (spec/tunnel.md §9: a local client never sends a credential to the node, and
// the gate never lets a node's request choose its principal), and a session whose hiring API key was revoked is
// ErrUnauthenticated, the 401 of every later stream it opens (§12).
func (a *Authenticator) Authenticate(r *http.Request, anonymousBuyer uuid.UUID) (p Principal, authenticated bool, err error) {
	if src, ok := HireSourceFrom(r.Context()); ok {
		p, err := src.HirePrincipal(r.Context())
		if err != nil {
			return Principal{}, false, err
		}
		return p, true, nil
	}
	token, present, err := BearerToken(r.Header)
	if !present {
		return Anonymous(anonymousBuyer), false, nil
	}
	if err != nil || a.keys == nil {
		return Principal{}, false, ErrUnauthenticated
	}
	p, err = a.keys.Authenticate(r.Context(), token)
	if err != nil {
		return Principal{}, false, err
	}
	return p, true, nil
}

// BearerToken reads the one Authorization header: present reports whether any Authorization header was
// sent; err is ErrUnauthenticated when it is not exactly one `Bearer <token>` (scheme case-insensitive,
// RFC 6750 §2.1) with a token free of whitespace.
func BearerToken(h http.Header) (token string, present bool, err error) {
	vals := h.Values("Authorization")
	if len(vals) == 0 {
		return "", false, nil
	}
	if len(vals) > 1 {
		return "", true, ErrUnauthenticated
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", true, ErrUnauthenticated
	}
	tok = strings.TrimLeft(tok, " ")
	if tok == "" || strings.ContainsFunc(tok, unicode.IsSpace) {
		return "", true, ErrUnauthenticated
	}
	return tok, true, nil
}
