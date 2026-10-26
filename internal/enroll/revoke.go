package enroll

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tunnel"
)

// overlapBatch is how many rotated keys one EndOverlaps transaction ends; EndOverlaps runs batches until a
// short one, at most overlapBatches of them a call.
const (
	overlapBatch   = 500
	overlapBatches = 20
)

// LookupKey returns the tunnel_keys row of a key id (the operator's view of one node), ErrKeyNotFound when
// the gate has none.
func (s *Store) LookupKey(ctx context.Context, keyID string) (*Key, error) {
	var k Key
	err := s.db.QueryRow(ctx, `
		select k.key_id, k.tenant_id, t.slug, k.pubkey, k.node_name, k.revoked_at, coalesce(k.revoked_reason, ''),
		       coalesce(k.rotated_to, ''), k.overlap_until
		  from kontra.tunnel_keys k join kontra.tenants t on t.id = k.tenant_id
		 where k.key_id = $1`, keyID).Scan(&k.KeyID, &k.TenantID, &k.Tenant, &k.PubKey, &k.NodeName, &k.RevokedAt,
		&k.RevokedReason, &k.RotatedTo, &k.OverlapUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, keyID)
	}
	if err != nil {
		return nil, fmt.Errorf("enroll: read key %s: %w", keyID, err)
	}
	return &k, nil
}

// successorHops bounds Successor's walk of a rotation chain.
const successorHops = 64

// Successor follows a key's rotated_to chain (§12) to the newest key and returns that key's row, nil when
// the key was never rotated. RevokeKey and RevokeBinding do not follow the chain, so the operator's
// commands use it to say which key still serves the node.
func (s *Store) Successor(ctx context.Context, keyID string) (*Key, error) {
	cur, err := s.LookupKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	var last *Key
	seen := map[string]bool{keyID: true}
	for hops := 0; cur.RotatedTo != ""; hops++ {
		if hops == successorHops || seen[cur.RotatedTo] {
			return nil, fmt.Errorf("enroll: the rotations of %s do not end within %d keys", keyID, successorHops)
		}
		seen[cur.RotatedTo] = true
		if cur, err = s.LookupKey(ctx, cur.RotatedTo); err != nil {
			return nil, err
		}
		last = cur
	}
	return last, nil
}

// RevokeKey sets tunnel_keys.revoked_at (reason owner or operator), appends the audit row and sends
// NOTIFY kontra_revoke 'key:<key_id>' in one transaction (§12, `kontra-gate enroll --revoke <key_id>`).
// The replica holding the key's session closes it on the notification (or at its next re-read), and the
// key's next handshake is revoked. A key already revoked is left as it is (nil); an unknown id is
// ErrKeyNotFound. Revoking a rotated key inside its overlap ends the overlap at once; the key it was
// rotated to stays enrolled (Successor; see the SPEC-CONFLICT on `kontra-gate enroll --revoke`).
func (s *Store) RevokeKey(ctx context.Context, keyID, reason, actor string) error {
	switch reason {
	case ReasonOwner, ReasonOperator, ReasonExpired:
	default:
		return fmt.Errorf("enroll: revoke %s: reason %q, want %s, %s or %s (a rotated key's overlap ends by EndOverlaps)",
			keyID, reason, ReasonOwner, ReasonOperator, ReasonExpired)
	}
	if !config.NodeKeyIDRE.MatchString(keyID) {
		return fmt.Errorf("%w: %q is not a key id (k1_ and 26 base32 characters)", ErrKeyNotFound, keyID)
	}
	if actor == "" {
		actor = "operator"
	}
	err := s.write(ctx, func(tx pgx.Tx) error {
		var (
			tenantID uuid.UUID
			was      string
			revoked  bool
		)
		err := tx.QueryRow(ctx, `select tenant_id, coalesce(revoked_reason, ''), revoked_at is not null
			from kontra.tunnel_keys where key_id = $1 for update`, keyID).Scan(&tenantID, &was, &revoked)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrKeyNotFound, keyID)
		case err != nil:
			return err
		case revoked:
			return nil // already revoked
		}
		if _, err := tx.Exec(ctx, `update kontra.tunnel_keys set revoked_at = now(), revoked_reason = $2 where key_id = $1`,
			keyID, reason); err != nil {
			return err
		}
		f := audit.Fields{"actor": actor, "key_id": keyID, "tenant_id": tenantID, "reason": reason}
		if was == ReasonRotated {
			f["was_rotated"] = true
		}
		if _, err := audit.Append(ctx, tx, EventKeyRevoked, f); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelRevoke), KeyRevokePayload(keyID))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return err
		}
		return fmt.Errorf("enroll: revoke %s: %w", keyID, err)
	}
	return nil
}

// RevokeBinding sets key_services.revoked_at for one service, appends the audit row and sends NOTIFY
// kontra_revoke 'binding:<key_id>/<service>' (§12) in one transaction. The replica holding the key's session
// drops the service from its served set; the key's next handshake leaves the service out of its welcome. A
// binding already revoked is left as it is (nil); a key that was never enrolled for the service is
// ErrBindingNotFound (ErrKeyNotFound when the key itself is unknown).
//
// SPEC-CONFLICT (M2 plan, C9): §12 names no command that revokes one binding, and a team gate has no
// console in M2. `kontra-gate enroll --revoke <key_id> --service <name>` calls this (operator CLI, not wire
// protocol); Cloud and the tests call it directly.
func (s *Store) RevokeBinding(ctx context.Context, keyID, service, actor string) error {
	if !config.NodeKeyIDRE.MatchString(keyID) {
		return fmt.Errorf("%w: %q is not a key id (k1_ and 26 base32 characters)", ErrKeyNotFound, keyID)
	}
	if !config.NameRE.MatchString(service) {
		return fmt.Errorf("%w: %q is not a service name", ErrBindingNotFound, service)
	}
	if actor == "" {
		actor = "operator"
	}
	err := s.write(ctx, func(tx pgx.Tx) error {
		var tenantID uuid.UUID
		err := tx.QueryRow(ctx, `update kontra.key_services set revoked_at = now()
			where key_id = $1 and service = $2 and revoked_at is null returning tenant_id`, keyID, service).Scan(&tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			var key, binding bool
			if err := tx.QueryRow(ctx, `select exists (select 1 from kontra.tunnel_keys where key_id = $1),
				exists (select 1 from kontra.key_services where key_id = $1 and service = $2)`, keyID, service).Scan(&key, &binding); err != nil {
				return err
			}
			switch {
			case !key:
				return fmt.Errorf("%w: %s", ErrKeyNotFound, keyID)
			case !binding:
				return fmt.Errorf("%w: %s, %s", ErrBindingNotFound, keyID, service)
			}
			return nil // already revoked
		}
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, EventBindingRevoked, audit.Fields{
			"actor": actor, "key_id": keyID, "tenant_id": tenantID, "service": service,
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelRevoke), BindingRevokePayload(keyID, service))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) || errors.Is(err, ErrBindingNotFound) {
			return err
		}
		return fmt.Errorf("enroll: revoke %s for %s: %w", keyID, service, err)
	}
	return nil
}

// EndOverlaps sets revoked_at on rotated keys whose overlap_until has passed, with their audit rows and a
// NOTIFY 'key:<key_id>' each (§12: "revoked_at is set when the overlap ends"), and returns their ids. A
// replica's sweep calls it; replicas running it together each end a key once (SKIP LOCKED, and the update
// re-checks revoked_at).
func (s *Store) EndOverlaps(ctx context.Context) ([]string, error) {
	var ended []string
	for range overlapBatches {
		var batch []string
		err := s.write(ctx, func(tx pgx.Tx) error {
			batch = batch[:0]
			rows, err := tx.Query(ctx, `
				with due as (
					select key_id from kontra.tunnel_keys
					 where revoked_reason = 'rotated' and revoked_at is null and overlap_until <= now()
					 order by overlap_until, key_id
					 limit $1
					   for update skip locked)
				update kontra.tunnel_keys k set revoked_at = now()
				  from due where k.key_id = due.key_id and k.revoked_at is null
				returning k.key_id, k.tenant_id, coalesce(k.rotated_to, ''), k.overlap_until`, overlapBatch)
			if err != nil {
				return err
			}
			type row struct {
				keyID, rotatedTo string
				tenantID         uuid.UUID
				overlap          time.Time
			}
			got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
				var x row
				err := r.Scan(&x.keyID, &x.tenantID, &x.rotatedTo, &x.overlap)
				return x, err
			})
			if err != nil {
				return err
			}
			for _, x := range got {
				if _, err := audit.Append(ctx, tx, EventOverlapEnded, audit.Fields{
					"actor": "kontra-gate sweep", "key_id": x.keyID, "tenant_id": x.tenantID, "rotated_to": x.rotatedTo,
					"overlap_until": x.overlap,
				}); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelRevoke), KeyRevokePayload(x.keyID)); err != nil {
					return err
				}
				batch = append(batch, x.keyID)
			}
			return nil
		})
		if err != nil {
			return ended, fmt.Errorf("enroll: end rotation overlaps: %w", err)
		}
		ended = append(ended, batch...)
		if len(batch) < overlapBatch {
			break
		}
	}
	return ended, nil
}

// Recipients is a sealed service's recipients: the keys enrolled for it, neither revoked nor rotated,
// connected or not (§12, D61), as canonical age1… strings in enrollment order. Sealing (M3) publishes them;
// M2 builds the query.
func (s *Store) Recipients(ctx context.Context, tenantID uuid.UUID, service string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		select k.pubkey from kontra.key_services ks join kontra.tunnel_keys k on k.key_id = ks.key_id
		 where ks.tenant_id = $1 and ks.service = $2 and ks.revoked_at is null
		   and k.revoked_at is null and k.revoked_reason is distinct from 'rotated'
		 order by ks.enrolled_at, k.key_id`, tenantID, service)
	if err != nil {
		return nil, fmt.Errorf("enroll: recipients of %s: %w", service, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, fmt.Errorf("enroll: recipients of %s: %w", service, err)
	}
	out := make([]string, 0, len(keys))
	for _, b := range keys {
		pub, err := ecdh.X25519().NewPublicKey(b)
		if err != nil {
			return nil, fmt.Errorf("enroll: recipients of %s: a stored key: %w", service, err)
		}
		r, err := tunnel.FormatRecipient(pub)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
