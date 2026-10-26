package enroll

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/config"
)

// tokenRandomLen is the length of a token's random part, base62 (about 190 bits).
const tokenRandomLen = 32

// tokenRE is a token's shape (§5): `ket_` + 32 base62 characters.
var tokenRE = regexp.MustCompile(`^ket_[0-9A-Za-z]{32}$`)

// ValidToken reports whether s has a token's shape. A token of another shape is never looked up.
func ValidToken(s string) bool { return tokenRE.MatchString(s) }

// TokenHash is the stored form of a token, sha256 of its bytes (enrollment_tokens.token_hash).
func TokenHash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// newToken returns `ket_` + 32 base62 characters read from r (crypto/rand when nil), each equally likely.
func newToken(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	out := make([]byte, 0, len(TokenPrefix)+tokenRandomLen)
	out = append(out, TokenPrefix...)
	var buf [64]byte
	for len(out) < cap(out) {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", fmt.Errorf("enroll: token randomness: %w", err)
		}
		for _, c := range buf {
			// 248 = 4 × 62: rejecting the top 8 values keeps every character equally likely.
			if c < 248 && len(out) < cap(out) {
				out = append(out, base62[c%62])
			}
		}
	}
	return string(out), nil
}

// Minted is a token's row as minted, for the operator's message (the token itself is shown once).
type Minted struct {
	ID        uuid.UUID
	ExpiresAt time.Time
	Tenant    string // slug
}

// MintServiceToken mints a service token for one service of a tenant and returns it (shown once). The
// service must exist (ErrUnknownService), be enabled (ErrServiceDisabled), and have backend tunnel
// (ErrNotTunnel): a binding to any other service would be stored but never selected (relay.md §6.1).
// enrollment_tokens keeps sha256 of the token, expires_at = created_at + 15 minutes; the mint is an audit
// row in the same transaction. Nothing is written when it fails.
func (s *Store) MintServiceToken(ctx context.Context, tenantID uuid.UUID, service, actor string) (string, error) {
	tok, _, err := s.MintServiceTokenInfo(ctx, tenantID, service, actor)
	return tok, err
}

// MintServiceTokenInfo is MintServiceToken, also returning the row's id, expiry and tenant slug.
func (s *Store) MintServiceTokenInfo(ctx context.Context, tenantID uuid.UUID, service, actor string) (string, Minted, error) {
	if !config.NameRE.MatchString(service) {
		return "", Minted{}, fmt.Errorf("%w: %q is not a service name", ErrUnknownService, service)
	}
	if actor == "" {
		actor = "kontra-gate enroll"
	}
	tok, err := newToken(nil)
	if err != nil {
		return "", Minted{}, err
	}
	h := TokenHash(tok)
	id, err := uuid.NewV7()
	if err != nil {
		return "", Minted{}, err
	}
	m := Minted{ID: id}
	err = s.write(ctx, func(tx pgx.Tx) error {
		var (
			kind     string
			isTunnel bool
			disabled bool
		)
		err := tx.QueryRow(ctx, `
			select t.slug, s.kind, s.backend -> 'tunnel' is not null, s.disabled_at is not null
			  from kontra.services s join kontra.tenants t on t.id = s.tenant_id
			 where s.tenant_id = $1 and s.name = $2 and t.deleted_at is null
			   and (t.expires_at is null or t.expires_at > now())
			   for share of s`, tenantID, service).Scan(&m.Tenant, &kind, &isTunnel, &disabled)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrUnknownService, service)
		case err != nil:
			return err
		case disabled:
			return fmt.Errorf("%w: %s", ErrServiceDisabled, service)
		case !isTunnel:
			return fmt.Errorf("%w: %s", ErrNotTunnel, service)
		}
		err = tx.QueryRow(ctx, `
			insert into kontra.enrollment_tokens (id, tenant_id, scope, service, token_hash, created_at, expires_at)
			values ($1, $2, 'service', $3, $4, now(), now() + make_interval(secs => $5))
			returning expires_at`, id, tenantID, service, h[:], TokenTTL.Seconds()).Scan(&m.ExpiresAt)
		if err != nil {
			return err
		}
		_, err = audit.Append(ctx, tx, EventTokenMinted, audit.Fields{
			"actor":      actor,
			"token_id":   id,
			"scope":      ScopeService,
			"tenant":     m.Tenant,
			"tenant_id":  tenantID,
			"service":    service,
			"kind":       kind,
			"expires_at": m.ExpiresAt,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrUnknownService) || errors.Is(err, ErrServiceDisabled) || errors.Is(err, ErrNotTunnel) {
			return "", Minted{}, err
		}
		return "", Minted{}, fmt.Errorf("enroll: mint a token for %s: %w", service, err)
	}
	return tok, m, nil
}

// MintTenantToken mints a tenant token bound to the service names it may bind (D40). The tenant must exist
// (ErrUnknownTenant). A listed service that exists must be an enabled tunnel service (ErrServiceDisabled,
// ErrNotTunnel); one that does not exist yet is created by the handshake that uses the token. The mint is
// an audit row in the same transaction.
func (s *Store) MintTenantToken(ctx context.Context, tenantID uuid.UUID, services []string, actor string) (string, error) {
	var names []string
	for _, n := range services {
		if !config.NameRE.MatchString(n) {
			return "", fmt.Errorf("enroll: tenant token: %q is not a service name", n)
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", errors.New("enroll: a tenant token names at least one service it may bind (D40)")
	}
	if actor == "" {
		actor = "operator"
	}
	tok, err := newToken(nil)
	if err != nil {
		return "", err
	}
	h := TokenHash(tok)
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	err = s.write(ctx, func(tx pgx.Tx) error {
		var slug string
		err := tx.QueryRow(ctx, `select slug from kontra.tenants where id = $1 and deleted_at is null
			and (expires_at is null or expires_at > now()) for share`, tenantID).Scan(&slug)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrUnknownTenant, tenantID)
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select name, backend -> 'tunnel' is not null, disabled_at is not null
			from kontra.services where tenant_id = $1 and name = any($2::text[]) order by name`, tenantID, names)
		if err != nil {
			return err
		}
		type svc struct {
			name             string
			tunnel, disabled bool
		}
		existing, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (svc, error) {
			var x svc
			err := r.Scan(&x.name, &x.tunnel, &x.disabled)
			return x, err
		})
		if err != nil {
			return err
		}
		for _, x := range existing {
			if x.disabled {
				return fmt.Errorf("%w: %s", ErrServiceDisabled, x.name)
			}
			if !x.tunnel {
				return fmt.Errorf("%w: %s", ErrNotTunnel, x.name)
			}
		}
		var expires time.Time
		err = tx.QueryRow(ctx, `
			insert into kontra.enrollment_tokens (id, tenant_id, scope, bind_services, token_hash, created_at, expires_at)
			values ($1, $2, 'tenant', $3, $4, now(), now() + make_interval(secs => $5))
			returning expires_at`, id, tenantID, names, h[:], TokenTTL.Seconds()).Scan(&expires)
		if err != nil {
			return err
		}
		_, err = audit.Append(ctx, tx, EventTokenMinted, audit.Fields{
			"actor":         actor,
			"token_id":      id,
			"scope":         ScopeTenant,
			"tenant":        slug,
			"tenant_id":     tenantID,
			"bind_services": names,
			"expires_at":    expires,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrUnknownTenant) || errors.Is(err, ErrServiceDisabled) || errors.Is(err, ErrNotTunnel) {
			return "", err
		}
		return "", fmt.Errorf("enroll: mint a tenant token: %w", err)
	}
	return tok, nil
}

// tokenHashes checks the shape of every token a hello carries and returns their hashes, deduplicated and
// sorted, the order in which Admit consumes (and so locks) them: two handshakes presenting the same tokens
// lock them in the same order. A token of another shape is enroll_invalid without a query.
func tokenHashes(tokens []string) ([][32]byte, bool) {
	out := make([][32]byte, 0, len(tokens))
	for _, t := range tokens {
		if !ValidToken(t) {
			return nil, false
		}
		h := TokenHash(t)
		if !slices.ContainsFunc(out, func(x [32]byte) bool { return x == h }) {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	return out, true
}
