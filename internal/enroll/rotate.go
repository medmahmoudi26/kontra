package enroll

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/audit"
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
// old key's active key_services rows to the new key and marks the old key revoked_reason = 'rotated',
// rotated_to = <new>, overlap_until = now + 24 h (revoked_at is set when the overlap ends, EndOverlaps), with
// its audit row. It returns the old key as it now is.
func (a *admission) rotate(ctx context.Context) (*Key, error) {
	oldID, newID := a.old.KeyID, a.key.KeyID
	rows, err := a.tx.Query(ctx, `
		insert into kontra.key_services (key_id, tenant_id, service)
		select $1, tenant_id, service from kontra.key_services where key_id = $2 and revoked_at is null
		returning service`, newID, oldID)
	if err != nil {
		return nil, err
	}
	copied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var overlap time.Time
	err = a.tx.QueryRow(ctx, `
		update kontra.tunnel_keys
		   set revoked_reason = 'rotated', rotated_to = $2, overlap_until = now() + make_interval(secs => $3)
		 where key_id = $1
		returning overlap_until`, oldID, newID, RotationOverlap.Seconds()).Scan(&overlap)
	if err != nil {
		return nil, err
	}
	if copied == nil {
		copied = []string{}
	}
	if _, err := audit.Append(ctx, a.tx, EventRotated, audit.Fields{
		"actor":         "tunnel handshake",
		"old_key":       oldID,
		"new_key":       newID,
		"tenant":        a.old.Tenant,
		"tenant_id":     a.old.TenantID,
		"node_name":     a.h.Name,
		"services":      copied,
		"overlap_until": overlap,
		"session_id":    a.req.SessionID,
		"replica_id":    a.req.ReplicaID,
	}); err != nil {
		return nil, err
	}
	old := *a.old
	old.RevokedReason, old.RotatedTo, old.OverlapUntil = ReasonRotated, newID, &overlap
	return &old, nil
}
