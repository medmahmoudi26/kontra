package enroll

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tunnel"
)

// checkRotation checks a hello's rotate_from (§12) under a row lock on the old key, held until the
// transaction ends: two rotations of one key serialize, and the second sees the first's commit. The old key
// must be a key of this gate, stored with the proven public key (bad_proof otherwise), neither revoked
// (revoked) nor already rotated (enroll_invalid); the new key must not exist yet (enroll_invalid). No token
// is needed: holding the enrolled old key is the authority.
//
// SPEC-CONFLICT (M2 plan, C2; the audit's proposal): §12 makes a rotate_from naming a rotated key
// enroll_invalid, which §4 does not retry, so a node whose welcome was lost after the gate committed its
// rotation would be stuck if it sent the same hello again. The gate keeps §12 as written; the node never
// resends a rotation after a lost welcome but redials with the new key alone, which welcome (the rotation
// committed) or unknown_key (it did not; the node then rotates again) answers (internal/node, WP-06). The
// owner's alternative, accepting a rotate_from whose rotated_to is the hello's key as a reconnect, is not
// built.
//
// A rotate_from naming no key of this gate, or a revoked one, is refused as §12 says; the node then sends
// its hello again without rotate_from, so that its tokens or API key may enroll the new key (SPEC-CONFLICT
// in internal/node's connect and on nodekey.Prune, M2 WP-06 review).
func (a *admission) checkRotation(ctx context.Context) error {
	h := a.h
	rf := h.RotateFrom
	if rf.KID == h.KID {
		return tunnel.Refuse(tunnel.CodeEnrollInvalid, "rotate_from names the active key")
	}
	if a.key != nil {
		return tunnel.Refuse(tunnel.CodeEnrollInvalid, "the new key %s already exists at this gate", h.KID)
	}
	old, err := a.lockKey(ctx, rf.KID, a.req.OldPubKey, "rotate_from")
	if err != nil {
		return err
	}
	switch {
	case old == nil:
		return tunnel.Refuse(tunnel.CodeEnrollInvalid, "rotate_from names no key of this gate")
	case old.RevokedAt != nil || old.tenantEnded:
		return tunnel.Refuse(tunnel.CodeRevoked, "rotate_from: the key %s was revoked", old.KeyID)
	case old.RevokedReason == ReasonRotated || old.RotatedTo != "":
		return tunnel.Refuse(tunnel.CodeEnrollInvalid, "rotate_from: the key %s was already rotated", old.KeyID)
	}
	a.old = &old.Key
	return nil
}

// rotate completes a rotation whose new key write already inserted in the old key's tenant: it copies the
// old key's key_services rows to the new key and marks the old key revoked_reason = 'rotated',
// rotated_to = <new>, overlap_until = now + 24 h (revoked_at is set when the overlap ends, EndOverlaps), with
// its audit row. It returns the old key as it now is.
//
// SPEC-CONFLICT (§12 "copies its active key_services rows", with §5 and §4; M2 WP-02 review): a revoked row
// is copied too, keeping its revoked_at. It grants nothing (it stays out of Served, the relay's candidates
// and Recipients), but it keeps §5's rule for the new key: a declared service whose binding was revoked is
// left out and the handshake succeeds. Copying only the active rows would turn that service into one the
// new key "was never enrolled for", so the new key's next handshake with the node's unchanged --serve (the
// redial after a lost welcome, which the audit's C2 protocol relies on, or a restart) would be
// service_denied, which is final and all-or-nothing: the node would lose every service.
func (a *admission) rotate(ctx context.Context) (*Key, error) {
	oldID, newID := a.old.KeyID, a.key.KeyID
	rows, err := a.tx.Query(ctx, `
		insert into kontra.key_services (key_id, tenant_id, service, revoked_at)
		select $1, tenant_id, service, revoked_at from kontra.key_services where key_id = $2
		returning service, revoked_at is not null`, newID, oldID)
	if err != nil {
		return nil, err
	}
	type row struct {
		service string
		revoked bool
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.service, &x.revoked)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	copied, revokedCopies := []string{}, []string{}
	for _, x := range got {
		if x.revoked {
			revokedCopies = append(revokedCopies, x.service)
		} else {
			copied = append(copied, x.service)
		}
	}
	sort.Strings(copied)
	sort.Strings(revokedCopies)
	var overlap time.Time
	err = a.tx.QueryRow(ctx, `
		update kontra.tunnel_keys
		   set revoked_reason = 'rotated', rotated_to = $2, overlap_until = now() + make_interval(secs => $3)
		 where key_id = $1
		returning overlap_until`, oldID, newID, RotationOverlap.Seconds()).Scan(&overlap)
	if err != nil {
		return nil, err
	}
	if _, err := audit.Append(ctx, a.tx, EventRotated, audit.Fields{
		"actor":            "tunnel handshake",
		"old_key":          oldID,
		"new_key":          newID,
		"tenant":           a.old.Tenant,
		"tenant_id":        a.old.TenantID,
		"node_name":        a.h.Name,
		"services":         copied,
		"revoked_services": revokedCopies,
		"overlap_until":    overlap,
		"session_id":       a.req.SessionID,
		"replica_id":       a.req.ReplicaID,
	}); err != nil {
		return nil, err
	}
	// tunnel.md §12: a sealed service's recipients are republished at once without the old key (and with the new
	// one): every replica re-reads the services the rotation copied.
	for _, name := range copied {
		if _, err := a.tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelServices), a.old.Tenant+"/"+name); err != nil {
			return nil, err
		}
	}
	old := *a.old
	old.RevokedReason, old.RotatedTo, old.OverlapUntil = ReasonRotated, newID, &overlap
	return &old, nil
}
