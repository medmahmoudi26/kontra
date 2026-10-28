package enroll

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tunnel"
)

// Admission bounds. The handshake has 10 s from the upgrade (§4), the admission included, so a statement
// or a lock wait (a concurrent handshake of the same key, a concurrent use of the same token) gives up well
// inside it: the node is then closed with the gate's own error and redials.
const (
	admitStatementTimeout = 5 * time.Second
	admitLockTimeout      = 3 * time.Second
	// admitAttempts is how often Admit runs its transaction when Postgres aborts it for a deadlock or a
	// serialization failure, or a concurrent handshake inserted the same key or service first: the next
	// attempt sees what the other committed.
	admitAttempts = 3
)

// serviceLockSpace is the high half of the advisory lock that serializes the enrollments of one service
// (Admit, under the tenant token rule "never adds a key to a service that already has one"): "ktsv", then a
// 32-bit FNV-1a hash of the tenant id and the name. It is far from the gate's other keys ("kontra" + two
// bytes: audit.LockID, the service sync, migrate) and from Kontra Cloud's.
const serviceLockSpace int64 = 0x6b747376 << 32

// errRetry is a transaction to run again: a concurrent handshake committed the row this one was inserting.
var errRetry = errors.New("enroll: a concurrent handshake committed first")

// Authorities of an enrollment, as audited.
const (
	viaServiceToken = "service_token"
	viaTenantToken  = "tenant_token"
	viaAPIKey       = "api_key"
)

// Admit decides a proven hello in one transaction (§4, §5, §12): the key's lookup (a known key's stored
// public key must equal the hello's), revocation (revoked), the API key (auth_invalid), the tokens and
// scopes (enroll_invalid, kind_mismatch, service_denied), the rotation (under a row lock on the old key),
// the inserts and the audit rows, and the NOTIFY kontra_tunnel announcing each binding added to a service
// that already had a key (EnrolledPayload, §5). A refusal is a *tunnel.HandshakeError; anything else is the
// gate's own failure.
//
// The key's row is locked (SELECT … FOR UPDATE) for the whole of Admit's transaction, so concurrent
// admissions of one key serialize: a rotation, an enrollment and a revocation of it see each other's
// commits. The lock ends when Admit commits, before the session is registered, so it does not serialize
// two handshakes of one key through the registry: both are admitted, one after the other, and then race
// in tunnelgate.Registry.Add. The M2 audit's row lock "on tunnel_keys in Add so concurrent handshakes of
// one key serialize" (confirmed conflict on §11 and §5) is therefore still open for WP-04: Add must lock
// the key's tunnel_keys row in its supersede transaction. The tokens are consumed in sorted order with
// UPDATE … SET used_at = now() … WHERE used_at IS NULL AND expires_at > now() RETURNING, so of concurrent
// handshakes presenting one token exactly one commits it.
//
// Precedence. The checks run in this order and the first refusal wins (the M2 plan audit asked for one,
// relay.md §16 key_scope):
//
//  1. bad_proof: the hello's key id or recipient does not match the proven public key, or the gate stores
//     the key id with another public key (or the public key under another id);
//  2. revoked: the key is revoked, its tenant has ended, or it is a rotated key (below);
//  3. the rotation (§12): rotate_from naming the active key, no key of this gate, or a new key that already
//     exists is enroll_invalid; an old key stored with another public key is bad_proof, a revoked one
//     revoked, a rotated one enroll_invalid;
//  4. auth_invalid: an auth API key that is unknown, revoked or expired (or any, when the gate takes none);
//     then anonymous sessions (spec/tunnel.md §6, DM4-18, DM4-19; anonymousSession): a hello asking for one
//     (anonymous: true, neither enroll nor auth) from an unknown key, or from a key of an anonymous tenant, opens or
//     resumes it (anonymous_unavailable, rate_limited, service_denied, kind_mismatch from its limits), or
//     anonymous_unavailable on a gate without tunnel.anonymous; a key of a live anonymous tenant presenting no
//     tokens is resumed on a hire-only auth key and is otherwise unknown_key; one presenting tokens, of an ended
//     anonymous tenant, or of a live one on a gate without tunnel.anonymous (a hello that does not ask), goes on below
//     as a new key would, and is moved (anon.MoveKey) if admitted;
//  5. enroll_invalid: a token that is malformed, unknown, used or expired, of another tenant than the key's
//     or the other authorities', or that applies to no declared service (a service token for a service the
//     hello does not declare; a tenant token listing none of them);
//  6. unknown_key: an unknown key that nothing in the hello enrolls, unless its hello declares no service and
//     carries an auth API key with the hire scope: such a key is admitted hire-only (spec/tunnel.md §9); and a
//     known key without any binding (one admitted hire-only, or its rotation) whose hello declares no service and
//     carries no auth API key with the hire scope: it hires only as such a key, never as node:<key_id>;
//  7. kind_mismatch: a declared service the hello enrolls, or the key is bound to, whose kind is not the
//     declared one;
//  8. service_denied: a declared service the key was never enrolled for and that no authority in the hello
//     enrolls, or that an authority cannot enroll (a missing service for a service token or an enroll:
//     scope, a disabled or non-tunnel one, a tenant token for a service that already has a key).
//
// So a hello carrying a consumed token and a service nothing enrolls is enroll_invalid, not service_denied.
// A declared service whose binding was revoked is never refused: it is left out of Served (§5), and only a
// one-time token in the hello enrolls it again, never the auth API key's enroll: scope (SPEC-CONFLICT in
// run).
func (s *Store) Admit(ctx context.Context, req Request) (*Decision, error) {
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	hashes, ok := tokenHashes(req.Hello.Enroll)
	if !ok {
		return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid, "an enrollment token is not ket_ followed by 32 base62 characters")
	}
	// The API key is authenticated before the transaction (its own read) and its refusal raised in its
	// place in the order above.
	var (
		principal *auth.Principal
		authErr   *tunnel.HandshakeError
	)
	if req.Hello.Auth != "" {
		if s.opts.Keys == nil {
			authErr = tunnel.Refuse(tunnel.CodeAuthInvalid, "this gate accepts no API keys (auth.api_keys)")
		} else {
			p, err := s.opts.Keys.Authenticate(ctx, req.Hello.Auth)
			switch {
			case errors.Is(err, auth.ErrUnauthenticated):
				authErr = tunnel.Refuse(tunnel.CodeAuthInvalid, "the auth API key is unknown, revoked or expired")
			case err != nil:
				return nil, fmt.Errorf("enroll: admit: %w", err)
			default:
				principal = &p
			}
		}
	}
	for attempt := 1; ; attempt++ {
		var d *Decision
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select set_config('statement_timeout', $1, true), set_config('lock_timeout', $2, true)`,
				fmt.Sprint(max(admitStatementTimeout, s.lockTimeout+time.Second).Milliseconds()), fmt.Sprint(s.lockTimeout.Milliseconds())); err != nil {
				return err
			}
			a := &admission{s: s, tx: tx, req: req, h: req.Hello, hashes: hashes, principal: principal, authErr: authErr}
			var err error
			d, err = a.run(ctx)
			return err
		})
		if err == nil {
			return d, nil
		}
		var herr *tunnel.HandshakeError
		if errors.As(err, &herr) {
			return nil, herr
		}
		code := pgCode(err)
		again := errors.Is(err, errRetry) || code == "40001" || code == "40P01"
		if !again || attempt == admitAttempts || ctx.Err() != nil {
			return nil, fmt.Errorf("enroll: admit %s: %w", req.Hello.KID, err)
		}
	}
}

// checkRequest checks that the request is the hello the proof was made for: the decoded keys are the
// hello's recipients and the key ids derive from them (ParseHello checked the latter; Admit does not trust
// its caller with the identity it records).
func checkRequest(req Request) error {
	h := req.Hello
	if h == nil {
		return errors.New("enroll: admit: no hello")
	}
	if err := sameKey(h.KID, h.Recipient, req.PubKey); err != nil {
		return tunnel.Refuse(tunnel.CodeBadProof, "%v", err)
	}
	switch {
	case h.RotateFrom == nil && req.OldPubKey != nil:
		return errors.New("enroll: admit: an old public key without rotate_from")
	case h.RotateFrom != nil:
		if req.OldPubKey == nil {
			return tunnel.Refuse(tunnel.CodeBadProof, "rotate_from without a proof for it")
		}
		if err := sameKey(h.RotateFrom.KID, h.RotateFrom.Recipient, req.OldPubKey); err != nil {
			return tunnel.Refuse(tunnel.CodeBadProof, "rotate_from: %v", err)
		}
	}
	return nil
}

func sameKey(kid, recipient string, pub []byte) error {
	p, err := tunnel.ParseRecipient(recipient)
	if err != nil {
		return err
	}
	if !bytes.Equal(p.Bytes(), pub) {
		return errors.New("the recipient is not the proven public key")
	}
	if kid != tunnel.KeyID(recipient) {
		return errors.New("the key id is not its recipient's")
	}
	return nil
}

// admission is one run of Admit's transaction.
type admission struct {
	s         *Store
	tx        pgx.Tx
	req       Request
	h         *tunnel.Hello
	hashes    [][32]byte
	principal *auth.Principal
	authErr   *tunnel.HandshakeError

	key    *Key // the active key's row, nil when it is new
	old    *Key // rotate_from's row
	tenant uuid.UUID
	slug   string
	// hireOnly: a new key admitted on its auth API key alone, with no service (step 6's second
	// SPEC-CONFLICT).
	hireOnly bool

	// Anonymous sessions (spec/tunnel.md §6, DM4-18, DM4-19; anonymous.go). anonKey is the key's row when it belongs
	// to an anonymous tenant, live or ended, and is neither revoked nor rotated: what the hello makes of it is decided
	// after the rotation and API key checks (anonymousSession). moveFrom is such a key that §5's rules below enroll as
	// if it were new: write moves it (anon.MoveKey) instead of inserting it; endTenant is the live anonymous tenant
	// that move converts, ended at once (anon.End).
	anonKey   *keyRow
	moveFrom  *Key
	endTenant uuid.UUID
}

// keyRow is a tunnel_keys row with its tenant's state.
type keyRow struct {
	Key
	tenantEnded bool
	anonymous   bool // its tenant is an anonymous tenant (kind anonymous)
}

// consumed is one token this handshake consumed.
type consumed struct {
	id       uuid.UUID
	tenantID uuid.UUID
	scope    string
	service  string   // service scope
	bind     []string // tenant scope
}

// planned is what the handshake does for one declared service.
type planned struct {
	svc     tunnel.Service
	bound   bool   // the key has an active binding for it (after the rotation's copy)
	revoked bool   // the key's binding for it was revoked
	via     string // the authority that enrolls it now; "" when none does
	token   *consumed
	// From the services row:
	exists, tunnel, enabled bool
	kind                    string
	others                  bool // another key has an active binding for it
	create                  bool // a tenant token creates it
}

func (a *admission) run(ctx context.Context) (*Decision, error) {
	h := a.h

	// 1–2. The active key, by its full public key (§2.2), locked for the transaction.
	key, err := a.lockKey(ctx, h.KID, a.req.PubKey, "the key")
	if err != nil {
		return nil, err
	}
	if key != nil {
		if key.anonymous && key.RevokedAt == nil && key.RevokedReason != ReasonRotated && key.RotatedTo == "" && h.RotateFrom == nil {
			// A key of an anonymous tenant: its tenant's end does not revoke it (§6.1, §12; DM4-10).
			if err := a.lockAnonymousTenant(ctx, key); err != nil {
				return nil, err
			}
			a.anonKey = key
		} else {
			if herr := refuseUnusable(key, "the key"); herr != nil {
				return nil, herr
			}
			a.key = &key.Key
		}
	}

	// 3. The rotation (§12).
	if h.RotateFrom != nil {
		if err := a.checkRotation(ctx); err != nil {
			return nil, err
		}
	}

	// 4. The API key.
	if a.authErr != nil {
		return nil, a.authErr
	}

	// An anonymous session (spec/tunnel.md §6): opened or resumed here, or the key of an anonymous tenant handed to
	// §5's rules below as a key to move.
	if d, err := a.anonymousSession(ctx); d != nil || err != nil {
		return d, err
	}

	// 5. The tokens, and the tenant they and the key name.
	switch {
	case a.key != nil:
		a.tenant, a.slug = a.key.TenantID, a.key.Tenant
	case a.old != nil:
		a.tenant, a.slug = a.old.TenantID, a.old.Tenant
	}
	toks, err := a.consume(ctx)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]bool, len(h.Services))
	for _, d := range h.Services {
		declared[d.Name] = true
	}
	for _, t := range toks {
		if a.tenant == uuid.Nil {
			a.tenant = t.tenantID
		} else if t.tenantID != a.tenant {
			return nil, a.otherTenant("an enrollment token")
		}
		switch t.scope {
		case ScopeService:
			if !declared[t.service] {
				return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid,
					"an enrollment token is for service %q, which this hello does not declare", t.service)
			}
		case ScopeTenant:
			if !slices.ContainsFunc(t.bind, func(n string) bool { return declared[n] }) {
				return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid,
					"an enrollment token binds none of the declared services (it may bind %v)", t.bind)
			}
		}
	}

	// The key's bindings: its own, or the old key's on a rotation (every row is copied with its revoked_at,
	// so a revoked one is left out of the served set as it was before the rotation, and stays so).
	bindings, err := a.bindings(ctx)
	if err != nil {
		return nil, err
	}

	// Which authority enrolls each declared service that is not bound yet: a service token, else an enroll:
	// scope, else a tenant token (the narrowest authority last: it never adds a key to a served service).
	//
	// SPEC-CONFLICT (§5, §12; M2 WP-02 review): a declared service whose binding was revoked is "left out"
	// (§5), and §5 does not say what may enroll it again. The auth API key never does: it rides on every
	// hello as the hiring principal, so with an enroll: scope it would undo the operator's revocation at the
	// node's next handshake (a restart with the same --serve). Only a one-time token in this hello, a service
	// token or a tenant token under its own rule, enrolls a revoked binding again: minting one is the
	// operator's explicit act. The owner decides whether a revoked binding may be enrolled again at all.
	scopes := a.principal != nil && a.s.opts.EnrollWithAPIKey
	plans := make([]*planned, len(h.Services))
	enrolls, usesScope := false, false
	for i, d := range h.Services {
		p := &planned{svc: d}
		plans[i] = p
		if revoked, ok := bindings[d.Name]; ok {
			p.bound, p.revoked = !revoked, revoked
		}
		if p.bound {
			continue
		}
		for _, t := range toks {
			if t.scope == ScopeService && t.service == d.Name {
				p.via, p.token = viaServiceToken, t
				break
			}
		}
		if p.via == "" && !p.revoked && scopes &&
			(a.principal.HasScope(auth.ScopeEnrollAll) || a.principal.HasScope(auth.EnrollScope(d.Name))) {
			p.via = viaAPIKey
			usesScope = true
		}
		if p.via == "" {
			for _, t := range toks {
				if t.scope == ScopeTenant && slices.Contains(t.bind, d.Name) {
					p.via, p.token = viaTenantToken, t
					break
				}
			}
		}
		if p.via != "" {
			enrolls = true
		}
	}
	if usesScope {
		switch pt := a.principal.TenantID; {
		case pt == nil:
			return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid, "the auth API key has no tenant to enroll under")
		case a.tenant == uuid.Nil:
			a.tenant = *pt
		case *pt != a.tenant:
			return nil, a.otherTenant("the auth API key")
		}
	}

	// 6. An unknown key that nothing enrolls.
	//
	// SPEC-CONFLICT (M2 plan audit, gap 1 and conflict C2): §4 makes service_denied final, and an unknown
	// key whose hello carries only an API key that cannot enroll (tunnel.enroll_with_api_key false, or no
	// enroll: scope for the declared services) would read as service_denied. But a node whose welcome was
	// lost redials with its active key and no tokens to learn whether the gate committed: welcome means it
	// did, and unknown_key (retried, §4) that it did not, so that it may send its tokens again. Such a
	// hello is therefore unknown_key whatever else it carries; service_denied stays for a known key, and
	// for a hello that enrolls something but not everything it declares.
	//
	// SPEC-CONFLICT (spec/tunnel.md §5 and D133 against §4 and §9; the M3 plan's conflict on §4–§5, WP-03):
	// §5 makes an unknown key that nothing in its hello enrolls unknown_key "whatever else the hello carries",
	// while §4 lets a hello declare no services ("a node that only hires") and §9 gives its streams the
	// principal of the hello's auth API key; a session needs a tunnel_keys row (tunnels.key_id references it).
	// The closest faithful behaviour: an unknown key whose hello declares no service and carries an auth API
	// key with the hire scope (and no token, which step 5 refused already) is admitted hire-only, its
	// tunnel_keys row in the API key's tenant and no binding, audited with the API key; without such a key it
	// stays unknown_key (D133), so the lost-welcome probe, which carries no auth, still learns that nothing
	// committed. The owner may want §5 to say so.
	//
	// Such a key is no node of its tenant: no binding was ever made for it. §9 gives a session without auth the
	// principal node:<key_id> with the key's tenant, which the services' org:<tenant> allow entries admit; for a
	// hire-only key that would turn one use of a hire-scoped API key into a hiring credential that outlives the API
	// key's revocation (§12: "every later stream a session opens with it as its hiring principal gets 401"; M3
	// WP-03 review). So a known key without any binding (key_services row, revoked or not; a rotation's are the
	// old key's) is admitted only on a hello that carries an auth API key with the hire scope (any such key, whose
	// principal the session then hires as) or that enrolls it. A hello of it declaring no service without such a
	// key is unknown_key, as before its first hello, retried by the node until its operator gives a valid
	// --api-key; one declaring services nothing enrolls stays service_denied (step 8); a revoked or expired API key
	// is auth_invalid (step 4).
	//
	// Hire-only admission does not need tunnel.enroll_with_api_key: that option governs the enroll: scopes, which
	// bind a key to services (§5's table), and a hire-only admission binds nothing, under the hire scope; with
	// the option's default (false on self-hosted gates) a node could otherwise never hire through such a gate
	// without hosting a service. §5's "a key without an enroll: scope only hires" is read as that key's whole
	// authority. The owner may want §5 to name this admission, and whether it needs the option.
	if a.key == nil && a.old == nil && !enrolls {
		if len(h.Services) > 0 || a.principal == nil || !a.principal.MayHire() || a.principal.TenantID == nil {
			return nil, tunnel.Refuse(tunnel.CodeUnknownKey, "the key %s is not enrolled at this gate, and nothing in this hello enrolls it", h.KID)
		}
		a.hireOnly = true
		a.tenant = *a.principal.TenantID
	}
	if (a.key != nil || a.old != nil) && len(h.Services) == 0 && len(bindings) == 0 && (a.principal == nil || !a.principal.MayHire()) {
		return nil, tunnel.Refuse(tunnel.CodeUnknownKey,
			"the key %s only hires (no service is bound to it): its hello must carry an API key with the hire scope", h.KID)
	}
	if a.slug == "" && a.tenant != uuid.Nil {
		if err := a.readTenant(ctx); err != nil {
			return nil, err
		}
	}

	// 7–8. The services rows, under a lock per service the handshake enrolls.
	if err := a.readServices(ctx, plans); err != nil {
		return nil, err
	}
	var mismatch, denied []string
	for _, p := range plans {
		switch {
		case p.bound:
			// SPEC-CONFLICT (M2 plan audit, wrongInPlan on C10): a bound service that is no longer an enabled
			// tunnel service (disabled, or switched to a direct backend) is left out of the served set like a
			// revoked binding, not service_denied: the handshake is all-or-nothing and service_denied is not
			// retried, so the node would lose every other service.
			if p.exists && p.tunnel && p.enabled && p.kind != p.svc.Kind {
				mismatch = append(mismatch, fmt.Sprintf("%s is %s, declared %s", p.svc.Name, p.kind, p.svc.Kind))
			}
		case p.via == "":
			if !p.revoked {
				denied = append(denied, p.svc.Name+": the key is not enrolled for it")
			}
		case !p.exists && p.via == viaTenantToken:
			p.create = true
		case !p.exists:
			// SPEC-CONFLICT (M2 plan, C10): §5 does not say what an enroll: scope naming a missing service
			// does. Only tenant tokens (and the operator) create services.
			denied = append(denied, p.svc.Name+": no such service")
		case !p.enabled || !p.tunnel:
			denied = append(denied, p.svc.Name+": not an enabled tunnel service")
		case p.kind != p.svc.Kind:
			mismatch = append(mismatch, fmt.Sprintf("%s is %s, declared %s", p.svc.Name, p.kind, p.svc.Kind))
		case p.via == viaTenantToken && p.others:
			denied = append(denied, p.svc.Name+": it already has an enrolled key, which a tenant token never adds to")
		}
	}
	if len(mismatch) > 0 {
		return nil, tunnel.Refuse(tunnel.CodeKindMismatch, "%s", joinClip(mismatch))
	}
	if len(denied) > 0 {
		return nil, tunnel.Refuse(tunnel.CodeServiceDenied, "%s; nothing was enrolled", joinClip(denied))
	}

	// The writes.
	return a.write(ctx, toks, plans)
}

// lockKey reads and locks the tunnel_keys row of a key, keyed by its full public key: a row with the key
// id and another public key, or the public key under another key id, is bad_proof (§2.2: the gate always
// checks the full public key).
func (a *admission) lockKey(ctx context.Context, kid string, pub []byte, what string) (*keyRow, error) {
	rows, err := a.tx.Query(ctx, `
		select k.key_id, k.tenant_id, t.slug, k.pubkey, k.node_name, k.revoked_at, coalesce(k.revoked_reason, ''),
		       coalesce(k.rotated_to, ''), k.overlap_until,
		       t.deleted_at is not null or (t.expires_at is not null and t.expires_at <= now()), t.kind = 'anonymous'
		  from kontra.tunnel_keys k join kontra.tenants t on t.id = k.tenant_id
		 where k.key_id = $1 or k.pubkey = $2
		 order by k.key_id
		   for update of k`, kid, pub)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*keyRow, error) {
		var k keyRow
		err := r.Scan(&k.KeyID, &k.TenantID, &k.Tenant, &k.PubKey, &k.NodeName, &k.RevokedAt, &k.RevokedReason,
			&k.RotatedTo, &k.OverlapUntil, &k.tenantEnded, &k.anonymous)
		return &k, err
	})
	if err != nil {
		return nil, err
	}
	switch {
	case len(found) == 0:
		return nil, nil
	case len(found) > 1 || found[0].KeyID != kid || !bytes.Equal(found[0].PubKey, pub):
		return nil, tunnel.Refuse(tunnel.CodeBadProof, "%s: this gate stores %s with another public key", what, kid)
	}
	return found[0], nil
}

// lockAnonymousTenant locks the tenant of a key of an anonymous tenant (FOR NO KEY UPDATE), live or ended, right after
// the key's row and before anything else the admission locks or writes, and reads again under the lock whether it has
// ended (an expiry committed meanwhile is seen). Every transaction that takes a key and its anonymous tenant takes them
// in that order, and the anonymous sweeps take a tenant with SKIP LOCKED and its keys with NOWAIT (anon.Sweeper): so a
// resume, a key's move (anon.MoveKey) and a conversion's end of the tenant (anon.End) never wait on a sweep that waits
// on them (the M4 WP-03 review's 40P01s: the expiry sweep updating the bindings MoveKey deleted, the purge deleting
// the key this admission holds). The expiry sweep skips the tenant until this admission commits.
func (a *admission) lockAnonymousTenant(ctx context.Context, k *keyRow) error {
	err := a.tx.QueryRow(ctx, `select deleted_at is not null or (expires_at is not null and expires_at <= now())
		from kontra.tenants where id = $1 for no key update`, k.TenantID).Scan(&k.tenantEnded)
	if err != nil {
		return fmt.Errorf("enroll: lock the anonymous tenant of key %s: %w", k.KeyID, err)
	}
	return nil
}

// refuseUnusable is revoked for a key that may open no new session: revoked, of an ended tenant, or rotated.
//
// SPEC-CONFLICT (M2 plan, C2; accepted by the audit): §12 does not say what a hello whose active key is a
// rotated key still inside its overlap gets. It is revoked: the old key's existing session keeps the tasks
// already on it, but a new session of it could serve nothing new (new attempts never select a rotated key).
func refuseUnusable(k *keyRow, what string) *tunnel.HandshakeError {
	switch {
	case k.RevokedAt != nil:
		return tunnel.Refuse(tunnel.CodeRevoked, "%s %s was revoked", what, k.KeyID)
	case k.tenantEnded:
		return tunnel.Refuse(tunnel.CodeRevoked, "%s %s belongs to a tenant that has ended", what, k.KeyID)
	case k.RevokedReason == ReasonRotated || k.RotatedTo != "":
		return tunnel.Refuse(tunnel.CodeRevoked, "%s %s was rotated to %s; connect with that key", what, k.KeyID, k.RotatedTo)
	}
	return nil
}

func (a *admission) otherTenant(what string) *tunnel.HandshakeError {
	if a.key != nil || a.old != nil {
		return tunnel.Refuse(tunnel.CodeEnrollInvalid, "%s is for another tenant than the one the key is enrolled under", what)
	}
	return tunnel.Refuse(tunnel.CodeEnrollInvalid, "%s is for another tenant than this hello's other authorities", what)
}

// consume marks the hello's tokens used, in sorted order. One that is unknown, used or expired is
// enroll_invalid (the transaction rolls back, so the others stay unused). The message does not say which.
func (a *admission) consume(ctx context.Context) ([]*consumed, error) {
	var out []*consumed
	for _, h := range a.hashes {
		t := &consumed{}
		err := a.tx.QueryRow(ctx, `
			update kontra.enrollment_tokens set used_at = now()
			 where token_hash = $1 and used_at is null and expires_at > now()
			returning id, tenant_id, scope, coalesce(service, ''), coalesce(bind_services, '{}')`, h[:]).
			Scan(&t.id, &t.tenantID, &t.scope, &t.service, &t.bind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, tunnel.Refuse(tunnel.CodeEnrollInvalid, "an enrollment token is unknown, already used or expired")
		}
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// bindings returns the key's key_services rows (service → revoked), or on a rotation the old key's.
func (a *admission) bindings(ctx context.Context) (map[string]bool, error) {
	var of string
	switch {
	case a.key != nil:
		of = a.key.KeyID
	case a.old != nil:
		of = a.old.KeyID
	default:
		return map[string]bool{}, nil
	}
	rows, err := a.tx.Query(ctx, `select service, revoked_at is not null from kontra.key_services where key_id = $1`, of)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for rows.Next() {
		var (
			name    string
			revoked bool
		)
		if err := rows.Scan(&name, &revoked); err != nil {
			rows.Close()
			return nil, err
		}
		out[name] = revoked
	}
	rows.Close()
	return out, rows.Err()
}

// readTenant reads the slug of the tenant the authorities name; an ended tenant enrolls nothing.
func (a *admission) readTenant(ctx context.Context) error {
	var ended bool
	err := a.tx.QueryRow(ctx, `select slug, deleted_at is not null or (expires_at is not null and expires_at <= now())
		from kontra.tenants where id = $1`, a.tenant).Scan(&a.slug, &ended)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ended) {
		return tunnel.Refuse(tunnel.CodeEnrollInvalid, "the enrollment's tenant has ended")
	}
	return err
}

// readServices takes the advisory lock of every service the handshake enrolls (in name order), then reads
// the declared services' rows, and for each enrolled one whether another key has an active binding for it.
// The lock serializes enrollments of one service, so the tenant token rule and the announcements see every
// committed binding; it is not a row lock, so it never waits on the service sync's or a card keeper's
// writes of the row.
func (a *admission) readServices(ctx context.Context, plans []*planned) error {
	if a.tenant == uuid.Nil || len(plans) == 0 {
		return nil
	}
	var lock []string
	names := make([]string, 0, len(plans))
	for _, p := range plans {
		names = append(names, p.svc.Name)
		if p.via != "" {
			lock = append(lock, p.svc.Name)
		}
	}
	sort.Strings(lock)
	for _, n := range lock {
		if _, err := a.tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, serviceLock(a.tenant, n)); err != nil {
			return err
		}
	}
	self := []string{}
	if a.key != nil {
		self = append(self, a.key.KeyID)
	}
	if a.old != nil {
		self = append(self, a.old.KeyID)
	}
	rows, err := a.tx.Query(ctx, `
		select s.name, s.kind, s.backend -> 'tunnel' is not null, s.disabled_at is null,
		       exists (select 1 from kontra.key_services ks join kontra.tunnel_keys k on k.key_id = ks.key_id
		                where ks.tenant_id = s.tenant_id and ks.service = s.name and ks.revoked_at is null
		                  and k.revoked_at is null and not (ks.key_id = any($3::text[])))
		  from kontra.services s
		 where s.tenant_id = $1 and s.name = any($2::text[])`, a.tenant, names, self)
	if err != nil {
		return err
	}
	byName := make(map[string]*planned, len(plans))
	for _, p := range plans {
		byName[p.svc.Name] = p
	}
	for rows.Next() {
		var (
			name string
			x    planned
		)
		if err := rows.Scan(&name, &x.kind, &x.tunnel, &x.enabled, &x.others); err != nil {
			rows.Close()
			return err
		}
		if p := byName[name]; p != nil {
			p.exists, p.kind, p.tunnel, p.enabled, p.others = true, x.kind, x.tunnel, x.enabled, x.others
		}
	}
	rows.Close()
	return rows.Err()
}

// serviceLock is the advisory lock key of one service's enrollments.
func serviceLock(tenantID uuid.UUID, name string) int64 {
	h := fnv.New32a()
	h.Write(tenantID[:])
	h.Write([]byte(name))
	return serviceLockSpace | int64(h.Sum32())
}

// write inserts what the decision needs, in one transaction with the checks: the new key (or the rotation),
// the services a tenant token creates, the bindings, the tokens' used_by_key and the audit rows.
func (a *admission) write(ctx context.Context, toks []*consumed, plans []*planned) (*Decision, error) {
	h := a.h
	newKey := a.key == nil
	keyID := h.KID
	if newKey && a.moveFrom != nil {
		// A key of an anonymous tenant, enrolled here as §5 enrolls a new key: it moves (DM4-19), its old bindings
		// deleted first.
		if err := anon.MoveKey(ctx, a.tx, keyID, a.tenant); err != nil {
			return nil, err
		}
		if _, err := a.tx.Exec(ctx, `update kontra.tunnel_keys set node_name = $2 where key_id = $1`, keyID, h.Name); err != nil {
			return nil, err
		}
		a.key = &Key{KeyID: keyID, TenantID: a.tenant, Tenant: a.slug, PubKey: a.req.PubKey, NodeName: h.Name}
	} else if newKey {
		k := Key{KeyID: keyID, TenantID: a.tenant, Tenant: a.slug, PubKey: a.req.PubKey, NodeName: h.Name}
		_, err := a.tx.Exec(ctx, `insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, $4)`,
			keyID, a.tenant, a.req.PubKey, h.Name)
		if pgCode(err) == "23505" {
			return nil, errRetry // another handshake inserted the key: the next attempt sees it as known
		}
		if err != nil {
			return nil, err
		}
		a.key = &k
	} else if a.key.NodeName != h.Name {
		if _, err := a.tx.Exec(ctx, `update kontra.tunnel_keys set node_name = $2 where key_id = $1`, keyID, h.Name); err != nil {
			return nil, err
		}
		a.key.NodeName = h.Name
	}

	var rotated *Key
	if a.old != nil {
		r, err := a.rotate(ctx)
		if err != nil {
			return nil, err
		}
		rotated = r
	}

	var (
		enrolled  []Binding
		audited   []any
		created   []string
		apiKeyUse bool
	)
	for _, p := range plans {
		if p.via == "" {
			continue
		}
		if p.create {
			if err := a.createService(ctx, p.svc); err != nil {
				return nil, err
			}
			created = append(created, p.svc.Name)
			p.exists, p.tunnel, p.enabled, p.kind = true, true, true, p.svc.Kind
		}
		_, err := a.tx.Exec(ctx, `
			insert into kontra.key_services (key_id, tenant_id, service) values ($1, $2, $3)
			on conflict (key_id, service) do update set revoked_at = null, enrolled_at = now()
			 where key_services.revoked_at is not null`, keyID, a.tenant, p.svc.Name)
		if err != nil {
			return nil, err
		}
		p.bound, p.revoked = true, false
		row := map[string]any{"service": p.svc.Name, "kind": p.svc.Kind, "via": p.via}
		if p.token != nil {
			row["token_id"] = p.token.id
		}
		if p.via == viaAPIKey {
			apiKeyUse = true
		}
		audited = append(audited, row)
		if p.others {
			enrolled = append(enrolled, Binding{KeyID: keyID, TenantID: a.tenant, Tenant: a.slug, Service: p.svc.Name})
		}
	}
	for _, t := range toks {
		if _, err := a.tx.Exec(ctx, `update kontra.enrollment_tokens set used_by_key = $2 where id = $1`, t.id, keyID); err != nil {
			return nil, err
		}
	}
	// A rotation's new key is the rotation's own row; a consumed token is audited even when the service it
	// names was already bound.
	if (newKey && a.old == nil) || len(audited) > 0 || len(toks) > 0 {
		f := audit.Fields{
			"actor":      "tunnel handshake",
			"key_id":     keyID,
			"tenant":     a.slug,
			"tenant_id":  a.tenant,
			"node_name":  h.Name,
			"new_key":    newKey,
			"services":   append([]any{}, audited...),
			"session_id": a.req.SessionID,
			"replica_id": a.req.ReplicaID,
		}
		if apiKeyUse || a.hireOnly {
			f["api_key"] = a.principal.ID
		}
		if a.hireOnly {
			f["hire_only"] = true
		}
		if a.moveFrom != nil {
			f["moved_from"] = a.moveFrom.Tenant
			f["moved_from_id"] = a.moveFrom.TenantID
		}
		if len(toks) > 0 {
			ids := make([]string, len(toks))
			for i, t := range toks {
				ids[i] = t.id.String()
			}
			f["token_ids"] = ids
		}
		if _, err := audit.Append(ctx, a.tx, EventEnrolled, f); err != nil {
			return nil, err
		}
	}
	for _, name := range created {
		if _, err := a.tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelServices), a.slug+"/"+name); err != nil {
			return nil, err
		}
	}
	// A new binding adds a recipient to a sealed service (ext-sealed.md §1, D61): every replica re-reads the
	// services it binds, which republishes their recipients (a service's first key included, which sends no
	// 'enrolled:' notice to other nodes).
	for _, row := range audited {
		m, _ := row.(map[string]any)
		name, _ := m["service"].(string)
		if name == "" || slices.Contains(created, name) {
			continue
		}
		if _, err := a.tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelServices), a.slug+"/"+name); err != nil {
			return nil, err
		}
	}
	// DM4-19: enroll from a key of a live anonymous tenant converts it; that tenant ends at once (its URLs 404, its
	// session told revoked).
	if a.endTenant != uuid.Nil && a.endTenant != a.tenant {
		if _, err := anon.End(ctx, a.tx, a.endTenant, "converted"); err != nil {
			return nil, err
		}
	}
	// §5: the services' other nodes are told once the binding commits, by every replica (this one included)
	// on its NOTIFY kontra_tunnel, whatever becomes of the new key's session afterwards.
	for _, b := range enrolled {
		if _, err := a.tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelTunnel), EnrolledPayload(b)); err != nil {
			return nil, err
		}
	}

	d := &Decision{Key: *a.key, Enrolled: enrolled, Rotated: rotated}
	d.Key.Tenant, d.Key.TenantID = a.slug, a.tenant
	for _, p := range plans {
		if p.bound && p.exists && p.tunnel && p.enabled && p.kind == p.svc.Kind {
			d.Served = append(d.Served, tunnel.WelcomeService{Kind: p.svc.Kind, Name: p.svc.Name,
				URL: a.s.serviceURL(p.svc.Kind, a.slug, p.svc.Name)})
		}
	}
	if a.principal != nil {
		d.HirePrincipal = a.principal.ID
		hire := *a.principal
		d.Hire = &hire
	} else {
		d.HirePrincipal = string(auth.KindNode) + ":" + keyID
	}
	return d, nil
}

// createService inserts a service a tenant token creates (§5, relay.md §2.3): backend tunnel, the hello's
// kind, allow [org:<tenant>], source db; with its audit row (the NOTIFY kontra_services follows the
// bindings). A row inserted meanwhile (the sync, the operator) runs the transaction again.
func (a *admission) createService(ctx context.Context, d tunnel.Service) error {
	allow := []string{"org:" + a.slug}
	tag, err := a.tx.Exec(ctx, `
		insert into kontra.services (tenant_id, name, kind, backend, allow, source)
		values ($1, $2, $3, '{"tunnel": {}}', $4, 'db')
		on conflict (tenant_id, name) do nothing`, a.tenant, d.Name, d.Kind, allow)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRetry
	}
	_, err = audit.Append(ctx, a.tx, EventServiceCreated, audit.Fields{
		"actor":     "tunnel handshake",
		"tenant":    a.slug,
		"tenant_id": a.tenant,
		"service":   d.Name,
		"kind":      d.Kind,
		"backend":   "tunnel",
		"allow":     allow,
		"source":    "db",
		"key_id":    a.h.KID,
	})
	return err
}

// joinClip joins refusal details, keeping the message far below the handshake's 16 KiB.
func joinClip(parts []string) string {
	var b bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			b.WriteString("; ")
		}
		if b.Len()+len(p) > 1024 {
			fmt.Fprintf(&b, "and %d more", len(parts)-i)
			break
		}
		b.WriteString(p)
	}
	return b.String()
}
