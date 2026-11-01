package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/strictjson"
)

// Algorithms the gate accepts (D276): never none, never an HMAC (whose key would be the public key set).
const (
	AlgES256 = "ES256"
	AlgRS256 = "RS256"
	AlgEdDSA = "EdDSA"
)

// Bounds of the key set.
const (
	// maxKeys bounds the keys of a set the gate keeps (the rest are ignored), and maxCandidates the keys one token's
	// signature is tried against (a token without kid, or several keys under one kid).
	maxKeys       = 64
	maxCandidates = 8
	// minRSABits is the smallest RSA modulus accepted (RFC 7518 §3.3: 2048 bits or larger).
	minRSABits = 2048
	// failBackoff is how long after a failed fetch the verifier answers ErrUnavailable without fetching again, so an
	// issuer that is down is not asked once per request.
	failBackoff = 5 * time.Second
)

// Config configures a Verifier.
type Config struct {
	Issuer string // auth.oidc.issuer or auth.mcp_oauth.issuer, compared byte for byte with the metadata's issuer and every token's iss
	// Client fetches the metadata and the key set: §10.3's operator-configured endpoint client (D284). Nil is
	// ssrf.NewOperatorClient with the system roots.
	Client *http.Client
	Logger *slog.Logger
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
}

// Verifier verifies the issuer's JWTs. It is safe for concurrent use; one fetch of the issuer is in flight at a time.
type Verifier struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	keys      *keySet   // nil before the first fetch
	jwksURI   string    // the metadata's jwks_uri
	metaAt    time.Time // when the metadata was read
	fetchedAt time.Time // when the key set was read
	failedAt  time.Time // the last failed fetch
	inflight  chan struct{}
}

// NewVerifier checks cfg. It does not fetch anything: the issuer's metadata and key set are read on first use.
func NewVerifier(cfg Config) (*Verifier, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: issuer: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("oidc: the issuer must be an https:// URL without userinfo, query or fragment (RFC 8414 §2)")
	}
	if cfg.Client == nil {
		cfg.Client = ssrf.NewOperatorClient(ssrf.OperatorOptions{})
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Verifier{cfg: cfg, log: cfg.Logger.With("component", "oidc")}, nil
}

// Issuer is the configured issuer.
func (v *Verifier) Issuer() string { return v.cfg.Issuer }

// Verify checks token's signature, issuer, audience (it must contain audience) and times. Any failure of the token is
// ErrInvalidToken (a 401), never a detail an attacker can probe; an issuer the gate could not read is ErrUnavailable.
func (v *Verifier) Verify(ctx context.Context, token, audience string) (Claims, error) {
	if !auth.LooksLikeJWT(token) {
		return Claims{}, ErrInvalidToken
	}
	parts := strings.SplitN(token, ".", 3)
	hdr, err := parseHeader(parts[0])
	if err != nil {
		return Claims{}, v.invalid("header", err)
	}
	payload, err := b64url(parts[1])
	if err != nil {
		return Claims{}, v.invalid("payload", err)
	}
	sig, err := b64url(parts[2])
	if err != nil || len(sig) == 0 {
		return Claims{}, v.invalid("signature", errors.New("not base64url, or empty"))
	}
	// The claims first: a token for another issuer or audience, or one that has expired, never makes the gate fetch.
	c, err := v.claims(payload, audience)
	if err != nil {
		return Claims{}, v.invalid("claims", err)
	}
	keys, err := v.candidates(ctx, hdr)
	if err != nil {
		return Claims{}, err
	}
	input := []byte(parts[0] + "." + parts[1])
	for _, k := range keys {
		if k.verify(hdr.alg, input, sig) {
			return c, nil
		}
	}
	return Claims{}, v.invalid("signature", errors.New("no key of the set verifies it"))
}

func (v *Verifier) invalid(what string, err error) error {
	v.log.Debug("a bearer token was refused", "part", what, "error", err)
	return ErrInvalidToken
}

// header is what the gate reads of a JWS header.
type header struct {
	alg string
	kid string // "" when absent
}

func parseHeader(seg string) (header, error) {
	b, err := b64url(seg)
	if err != nil {
		return header{}, err
	}
	m, err := strictjson.Members(b)
	if err != nil {
		return header{}, err
	}
	var h header
	if err := str(m, "alg", &h.alg, true); err != nil {
		return header{}, err
	}
	if err := str(m, "kid", &h.kid, false); err != nil {
		return header{}, err
	}
	switch h.alg {
	case AlgES256, AlgRS256, AlgEdDSA:
	default:
		return header{}, fmt.Errorf("alg %s is not accepted", strictjson.Quote(h.alg))
	}
	// RFC 7515 §4.1.11: a critical extension the gate does not understand makes the token invalid; it understands none.
	if _, ok := m["crit"]; ok {
		return header{}, errors.New("crit names extensions the gate does not process")
	}
	return h, nil
}

// claims checks the payload's iss, sub, aud, exp and nbf (§12.1, D276).
func (v *Verifier) claims(payload []byte, audience string) (Claims, error) {
	m, err := strictjson.Members(payload)
	if err != nil {
		return Claims{}, err
	}
	c := Claims{members: m}
	if err := str(m, "iss", &c.Issuer, true); err != nil {
		return Claims{}, err
	}
	if c.Issuer != v.cfg.Issuer {
		return Claims{}, errors.New("iss is not the configured issuer")
	}
	if err := str(m, "sub", &c.Subject, true); err != nil {
		return Claims{}, err
	}
	if c.Subject == "" {
		return Claims{}, errors.New("sub is empty")
	}
	if c.Audience, err = audiences(m["aud"]); err != nil {
		return Claims{}, err
	}
	if !slices.Contains(c.Audience, audience) {
		return Claims{}, errors.New("aud does not contain the route's canonical URI")
	}
	now := v.cfg.Now()
	exp, ok, err := numericDate(m, "exp")
	switch {
	case err != nil:
		return Claims{}, err
	case !ok:
		return Claims{}, errors.New("no exp")
	case !now.Before(exp.Add(Skew)):
		return Claims{}, errors.New("expired")
	}
	c.Expiry = exp
	nbf, ok, err := numericDate(m, "nbf")
	switch {
	case err != nil:
		return Claims{}, err
	case ok && now.Add(Skew).Before(nbf):
		return Claims{}, errors.New("not valid yet (nbf)")
	}
	return c, nil
}

// str reads a string member; a member of another type is an error, and so is a missing one when required.
func str(m map[string]json.RawMessage, key string, dst *string, required bool) error {
	raw, ok := m[key]
	if !ok {
		if required {
			return fmt.Errorf("no %s", key)
		}
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil || !strings.HasPrefix(strings.TrimSpace(string(raw)), `"`) {
		return fmt.Errorf("%s is not a string", key)
	}
	return nil
}

// audiences reads aud: a string or an array of strings (RFC 7519 §4.1.3).
func audiences(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, errors.New("no aud")
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && strings.HasPrefix(strings.TrimSpace(string(raw)), `"`) {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, errors.New("aud is neither a string nor an array of strings")
	}
	return many, nil
}

// numericDate reads a NumericDate (RFC 7519 §2: seconds since the epoch, a JSON number).
func numericDate(m map[string]json.RawMessage, key string) (time.Time, bool, error) {
	raw, ok := m[key]
	if !ok {
		return time.Time{}, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1<<40 {
		return time.Time{}, false, fmt.Errorf("%s is not a NumericDate", key)
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)), true, nil
}

// b64url decodes unpadded base64url strictly: padding, white space, line breaks (which encoding/base64 skips) and
// the standard alphabet are refused.
func b64url(s string) ([]byte, error) {
	if strings.ContainsAny(s, "=+/\r\n \t") {
		return nil, errors.New("not unpadded base64url")
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

// candidates are the keys a token's signature is tried against: those of its kid (or, without one, every key) whose
// type fits alg. A kid the cached set does not hold refetches the set, at most once every RefetchEvery; an expired
// set is fetched again.
func (v *Verifier) candidates(ctx context.Context, h header) ([]*jwk, error) {
	set, err := v.current(ctx, false)
	if err != nil {
		return nil, err
	}
	found := set.find(h)
	if len(found) == 0 && h.kid != "" {
		// An unknown kid: perhaps a rotation (D276).
		if set, err = v.current(ctx, true); err != nil {
			return nil, err
		}
		found = set.find(h)
	}
	if len(found) == 0 {
		return nil, v.invalid("kid", errors.New("no key of the set fits the token's kid and alg"))
	}
	return found, nil
}

// current returns the key set, fetching it when there is none, when it is older than KeySetTTL, or (refetch) when it
// is older than RefetchEvery. A refetch the rate does not allow returns the set as it is.
func (v *Verifier) current(ctx context.Context, refetch bool) (*keySet, error) {
	for {
		v.mu.Lock()
		now := v.cfg.Now()
		fresh := v.keys != nil && now.Sub(v.fetchedAt) < KeySetTTL
		switch {
		case fresh && !refetch:
			set := v.keys
			v.mu.Unlock()
			return set, nil
		case fresh && now.Sub(v.fetchedAt) < RefetchEvery:
			set := v.keys
			v.mu.Unlock()
			return set, nil
		case !v.failedAt.IsZero() && now.Sub(v.failedAt) < failBackoff:
			set := v.keys
			v.mu.Unlock()
			if fresh {
				return set, nil
			}
			return nil, fmt.Errorf("%w (the last fetch failed less than %s ago)", ErrUnavailable, failBackoff)
		}
		if wait := v.inflight; wait != nil {
			v.mu.Unlock()
			select {
			case <-wait:
				refetch = false // the fetch that ran is the refetch this token asked for
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		v.inflight = done
		metaStale := v.jwksURI == "" || now.Sub(v.metaAt) >= KeySetTTL
		jwksURI := v.jwksURI
		v.mu.Unlock()

		// Detached from the request: other requests wait on this fetch; the client's timeout bounds it.
		set, uri, err := v.fetch(context.WithoutCancel(ctx), metaStale, jwksURI)

		v.mu.Lock()
		v.inflight = nil
		close(done)
		at := v.cfg.Now()
		if err != nil {
			v.failedAt = at
			fresh := v.keys != nil && at.Sub(v.fetchedAt) < KeySetTTL
			set := v.keys
			v.mu.Unlock()
			v.log.Warn("the OIDC issuer's metadata or key set could not be read", "issuer", v.cfg.Issuer, "error", err)
			if fresh {
				return set, nil // a refetch that failed keeps the set it was to refresh
			}
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if metaStale {
			v.metaAt = at
		}
		v.keys, v.jwksURI, v.fetchedAt, v.failedAt = set, uri, at, time.Time{}
		v.mu.Unlock()
		return set, nil
	}
}

// fetch reads the metadata when stale (else uses jwksURI) and then the key set.
func (v *Verifier) fetch(ctx context.Context, meta bool, jwksURI string) (*keySet, string, error) {
	if meta {
		uri, err := v.metadata(ctx)
		if err != nil {
			return nil, "", err
		}
		jwksURI = uri
	}
	b, status, err := v.get(ctx, jwksURI)
	if err != nil {
		return nil, "", fmt.Errorf("jwks_uri: %w", err)
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("jwks_uri answered %d", status)
	}
	set, err := parseKeySet(b)
	if err != nil {
		return nil, "", fmt.Errorf("jwks_uri: %w", err)
	}
	return set, jwksURI, nil
}

// MetadataURLs are where the issuer's metadata is looked for, in order (D276, D296): RFC 8414's well-known URI
// (inserted before the issuer's path, §3.1), the same appended to the issuer as relay.md §12.1 writes it, then the
// OpenID configuration appended (OpenID Connect Discovery §4) and inserted. Duplicates are dropped: for an issuer
// without a path the list is the first and the third.
func MetadataURLs(issuer string) []string {
	u, err := url.Parse(issuer)
	if err != nil {
		return nil
	}
	base := u.Scheme + "://" + u.Host
	path := strings.TrimRight(u.EscapedPath(), "/")
	var out []string
	for _, c := range []string{
		base + "/.well-known/oauth-authorization-server" + path,
		base + path + "/.well-known/oauth-authorization-server",
		base + path + "/.well-known/openid-configuration",
		base + "/.well-known/openid-configuration" + path,
	} {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// metadata reads the issuer's metadata and returns its jwks_uri. A document is accepted only when its issuer is the
// configured one byte for byte (RFC 8414 §3.3, OpenID Connect Discovery §4.3); a 4xx moves on to the next URL.
func (v *Verifier) metadata(ctx context.Context) (string, error) {
	var tried []string
	for _, mu := range MetadataURLs(v.cfg.Issuer) {
		b, status, err := v.get(ctx, mu)
		if err != nil {
			return "", fmt.Errorf("metadata %s: %w", mu, err)
		}
		if status >= 400 && status < 500 {
			tried = append(tried, fmt.Sprintf("%s: %d", mu, status))
			continue
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("metadata %s answered %d", mu, status)
		}
		m, err := strictjson.Members(b)
		if err != nil {
			return "", fmt.Errorf("metadata %s: %w", mu, err)
		}
		var iss, jwks string
		if err := str(m, "issuer", &iss, true); err != nil {
			return "", fmt.Errorf("metadata %s: %w", mu, err)
		}
		if iss != v.cfg.Issuer {
			return "", fmt.Errorf("metadata %s names another issuer", mu)
		}
		if err := str(m, "jwks_uri", &jwks, true); err != nil {
			return "", fmt.Errorf("metadata %s: %w", mu, err)
		}
		ju, err := url.Parse(jwks)
		if err == nil {
			err = ssrf.CheckOperatorURL(ju)
		}
		if err != nil {
			return "", fmt.Errorf("metadata %s: jwks_uri: %w", mu, err)
		}
		return jwks, nil
	}
	return "", fmt.Errorf("no metadata (%s)", strings.Join(tried, "; "))
}

// get fetches one operator-endpoint URL, its body bounded (§10.3: 256 KiB).
func (v *Verifier) get(ctx context.Context, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.cfg.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, fmt.Errorf("answered %d: redirects are not followed (relay.md §10.3)", resp.StatusCode)
	}
	b, err := ssrf.ReadBody(resp.Body, ssrf.MaxOperatorBody)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// keySet is a parsed JWKS: the keys the gate can use.
type keySet struct {
	keys []*jwk
}

// find returns the keys a token with h may be verified by: of its kid when it has one, else all, whose type and alg
// fit h.alg; at most maxCandidates.
func (s *keySet) find(h header) []*jwk {
	var out []*jwk
	for _, k := range s.keys {
		if h.kid != "" && k.kid != h.kid {
			continue
		}
		if !k.fits(h.alg) {
			continue
		}
		out = append(out, k)
		if len(out) == maxCandidates {
			break
		}
	}
	return out
}

// jwk is one usable public key.
type jwk struct {
	kid string
	alg string // the JWK's alg when it names one
	ec  *ecdsa.PublicKey
	rsa *rsa.PublicKey
	ed  ed25519.PublicKey
}

func (k *jwk) fits(alg string) bool {
	if k.alg != "" && k.alg != alg {
		return false
	}
	switch alg {
	case AlgES256:
		return k.ec != nil
	case AlgRS256:
		return k.rsa != nil
	case AlgEdDSA:
		return k.ed != nil
	}
	return false
}

func (k *jwk) verify(alg string, input, sig []byte) bool {
	switch alg {
	case AlgES256:
		if k.ec == nil || len(sig) != 64 {
			return false
		}
		h := sha256.Sum256(input)
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(k.ec, h[:], r, s)
	case AlgRS256:
		if k.rsa == nil {
			return false
		}
		h := sha256.Sum256(input)
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], sig) == nil
	case AlgEdDSA:
		return k.ed != nil && len(sig) == ed25519.SignatureSize && ed25519.Verify(k.ed, input, sig)
	}
	return false
}

// parseKeySet reads a JWKS (RFC 7517 §5). Keys the gate cannot use (another kty or curve, use other than sig,
// key_ops without verify, a short RSA modulus, a private key's members, a malformed member) are skipped, not fatal:
// an issuer may publish encryption keys beside its signing keys.
func parseKeySet(b []byte) (*keySet, error) {
	top, err := strictjson.Members(b)
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(top["keys"], &raws); err != nil || top["keys"] == nil {
		return nil, errors.New("not a JWKS: no keys array")
	}
	set := &keySet{}
	for _, raw := range raws {
		if len(set.keys) == maxKeys {
			break
		}
		if k, err := parseJWK(raw); err == nil {
			set.keys = append(set.keys, k)
		}
	}
	if len(set.keys) == 0 {
		return nil, errors.New("the JWKS holds no ES256, RS256 or EdDSA signing key")
	}
	return set, nil
}

func parseJWK(raw json.RawMessage) (*jwk, error) {
	m, err := strictjson.Members(raw)
	if err != nil {
		return nil, err
	}
	var kty, use, crv string
	k := &jwk{}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"kty", &kty}, {"use", &use}, {"crv", &crv}, {"kid", &k.kid}, {"alg", &k.alg}} {
		if err := str(m, f.name, f.dst, false); err != nil {
			return nil, err
		}
	}
	if use != "" && use != "sig" {
		return nil, errors.New("not a signing key")
	}
	if ops, ok := m["key_ops"]; ok {
		var list []string
		if err := json.Unmarshal(ops, &list); err != nil || !slices.Contains(list, "verify") {
			return nil, errors.New("key_ops without verify")
		}
	}
	if _, private := m["d"]; private {
		return nil, errors.New("a private key in a public key set")
	}
	coord := func(name string) ([]byte, error) {
		var s string
		if err := str(m, name, &s, true); err != nil {
			return nil, err
		}
		return b64url(s)
	}
	switch kty {
	case "EC":
		if crv != "P-256" {
			return nil, errors.New("an EC key not on P-256")
		}
		x, err := coord("x")
		if err != nil {
			return nil, err
		}
		y, err := coord("y")
		if err != nil {
			return nil, err
		}
		if len(x) != 32 || len(y) != 32 {
			return nil, errors.New("P-256 coordinates are 32 bytes")
		}
		if k.ec, err = ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...)); err != nil {
			return nil, err
		}
	case "RSA":
		n, err := coord("n")
		if err != nil {
			return nil, err
		}
		e, err := coord("e")
		if err != nil {
			return nil, err
		}
		ev := new(big.Int).SetBytes(e)
		if len(e) == 0 || len(e) > 4 || !ev.IsInt64() || ev.Int64() < 3 || ev.Int64()%2 == 0 {
			return nil, errors.New("an RSA exponent the gate does not use")
		}
		nv := new(big.Int).SetBytes(n)
		if nv.BitLen() < minRSABits {
			return nil, fmt.Errorf("an RSA modulus under %d bits", minRSABits)
		}
		k.rsa = &rsa.PublicKey{N: nv, E: int(ev.Int64())}
	case "OKP":
		if crv != "Ed25519" {
			return nil, errors.New("an OKP key that is not Ed25519")
		}
		x, err := coord("x")
		if err != nil {
			return nil, err
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, errors.New("an Ed25519 key is 32 bytes")
		}
		k.ed = ed25519.PublicKey(x)
	default:
		return nil, errors.New("a key type the gate does not use")
	}
	if k.alg != "" && !k.fits(k.alg) {
		return nil, errors.New("the key's alg does not fit its type")
	}
	return k, nil
}
