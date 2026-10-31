// Package oidctest is an in-repository OAuth 2.1 authorization server for the tests of the MCP resource server
// (internal/oidc, spec/relay.md §12.1) and of kontra login (internal/login, spec/tunnel.md §6.3): the shape of the
// operator's authorization server (Cloud's control plane, M5), over HTTPS with a throwaway CA.
//
//   - RFC 8414 metadata at the issuer's well-known URI (and, as Config.Discovery says, the OpenID configuration);
//   - a JWKS of ES256, RS256 and EdDSA keys whose kids rotate (Rotate), and Sign to issue JWTs with them, or forged
//     ones (alg none, HS256, a key the set does not list, a flipped signature byte) for the refusals;
//   - dynamic client registration (RFC 7591) of public clients with loopback redirects;
//   - an authorization endpoint that approves at once, checks the PKCE S256 challenge's presence and the redirect URI
//     (a loopback redirect matches on any port, RFC 8252 §7.3), and answers with code, state and iss (RFC 9207);
//   - a token endpoint that verifies the code_verifier against the challenge and issues Config.MintKey's access token
//     (a kk_ API key minted on a gate's database: DBMinter), or a JWT when MintKey is nil (the variant kontra login
//     must refuse).
//
// Every request is recorded (Registrations, Authorizations, TokenRequests, Fetches) so that tests can assert what a
// client sent. Follow plays the browser: it requests an authorization URL and delivers the redirect to the CLI's
// loopback listener.
//
// Work package: M4 WP-06 (used by M4 WP-07's e2e tests too).
package oidctest

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
)

// Algorithms the server signs with; AlgNone and AlgHS256 forge.
const (
	AlgES256 = "ES256"
	AlgRS256 = "RS256"
	AlgEdDSA = "EdDSA"
	AlgNone  = "none"
	AlgHS256 = "HS256"
)

// Discovery documents the server serves (Config.Discovery).
const (
	DiscoveryOAuth  = "oauth"  // RFC 8414 only (the default)
	DiscoveryOpenID = "openid" // the OpenID configuration only (the verifier's fallback)
	DiscoveryBoth   = "both"
)

// Paths of the endpoints, under the server's origin.
const (
	PathJWKS      = "/jwks"
	PathAuthorize = "/authorize"
	PathToken     = "/token"
	PathRegister  = "/register"
)

// Config configures a Server.
type Config struct {
	// IssuerPath is the issuer's path under the server's origin ("" for none, e.g. "/tenant1").
	IssuerPath string
	// Discovery is which metadata documents are served (DiscoveryOAuth when empty).
	Discovery string
	// Subject is the sub of whoever the authorization endpoint approves ("user-1" when empty).
	Subject string
	// MintKey issues the token endpoint's access token for the approved subject and the authorization's resource; nil
	// issues an ES256 JWT for that resource instead (an access token that is not an API key).
	MintKey func(ctx context.Context, subject, resource string) (string, error)
	// NoISS leaves iss out of authorization responses and authorization_response_iss_parameter_supported false.
	NoISS bool
	// Metadata edits the metadata document before it is served (a wrong issuer, an http:// endpoint, no S256...).
	Metadata func(m map[string]any)
	// Hook is asked first for every request; when it returns true it has answered it.
	Hook func(w http.ResponseWriter, r *http.Request) bool
}

// Server is a running authorization server. Close it when done (New registers nothing for cleanup).
type Server struct {
	cfg    Config
	srv    *httptest.Server
	issuer string
	caPEM  []byte
	pool   *x509.CertPool

	mu       sync.Mutex
	keys     []*key // published, in order; the last of each alg is its current one
	nkid     int
	clients  map[string]Registration
	grants   map[string]*grant
	regs     []Registration
	auths    []url.Values
	tokens   []TokenRequest
	fetches  map[string]int
	lastCode string
}

// key is one signing key.
type key struct {
	alg  string
	kid  string
	priv crypto.Signer
}

// grant is an authorization code the token endpoint redeems once.
type grant struct {
	clientID, redirectURI, challenge, resource, subject string
	expires                                             time.Time
	used                                                bool
}

// Registration is one dynamic client registration: the client_id issued and the request's metadata.
type Registration struct {
	ClientID string
	Request  map[string]any
}

// TokenRequest is one token request: its form, and whether its code_verifier matched the code's challenge.
type TokenRequest struct {
	Form       url.Values
	VerifierOK bool
	Issued     string // the access token issued ("" when refused)
}

// New starts a server over HTTPS (a CA of its own; Client and CAPool trust it) with one key of each algorithm.
func New(cfg Config) (*Server, error) {
	if cfg.Subject == "" {
		cfg.Subject = "user-1"
	}
	if cfg.Discovery == "" {
		cfg.Discovery = DiscoveryOAuth
	}
	s := &Server{cfg: cfg, clients: map[string]Registration{}, grants: map[string]*grant{}, fetches: map[string]int{}}
	for _, alg := range []string{AlgES256, AlgRS256, AlgEdDSA} {
		if _, err := s.Rotate(alg); err != nil {
			return nil, err
		}
	}
	leaf, caPEM, pool, err := newCA()
	if err != nil {
		return nil, err
	}
	s.caPEM, s.pool = caPEM, pool
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	s.srv.StartTLS()
	s.issuer = s.srv.URL + cfg.IssuerPath
	return s, nil
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Issuer is the issuer identifier (https://127.0.0.1:<port>[IssuerPath]).
func (s *Server) Issuer() string { return s.issuer }

// URL is the server's origin.
func (s *Server) URL() string { return s.srv.URL }

// CAPool trusts the server's certificate.
func (s *Server) CAPool() *x509.CertPool { return s.pool }

// CAPEM is the throwaway CA's certificate, PEM (for SSL_CERT_FILE).
func (s *Server) CAPEM() []byte { return slices.Clone(s.caPEM) }

// WriteCA writes CAPEM to dir/oidctest-ca.pem and returns its path (a gate process or kontra login takes it as
// SSL_CERT_FILE).
func (s *Server) WriteCA(dir string) (string, error) {
	p := filepath.Join(dir, "oidctest-ca.pem")
	return p, os.WriteFile(p, s.caPEM, 0o600)
}

// Client is an HTTP client that trusts the server and follows no redirect.
func (s *Server) Client() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: s.pool, MinVersion: tls.VersionTLS12}, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

// MetadataPath is the path of the RFC 8414 document (the well-known URI inserted before the issuer's path, §3.1).
func (s *Server) MetadataPath() string {
	return "/.well-known/oauth-authorization-server" + s.cfg.IssuerPath
}

// OpenIDPath is the path of the OpenID configuration (appended to the issuer, OpenID Connect Discovery §4).
func (s *Server) OpenIDPath() string { return s.cfg.IssuerPath + "/.well-known/openid-configuration" }

// Rotate adds a new key for alg, which becomes its current one; the earlier keys stay published. It returns the kid.
func (s *Server) Rotate(alg string) (string, error) {
	k, err := newKey(alg)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nkid++
	k.kid = fmt.Sprintf("%s-%d", strings.ToLower(alg), s.nkid)
	s.keys = append(s.keys, k)
	return k.kid, nil
}

// Retire unpublishes the key kid.
func (s *Server) Retire(kid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = slices.DeleteFunc(s.keys, func(k *key) bool { return k.kid == kid })
}

// Token describes a JWT Sign issues.
type Token struct {
	Alg      string // AlgES256 when empty; AlgNone and AlgHS256 forge
	Kid      string // the current key of Alg when empty; with Unpublished, the kid the header names
	Subject  string // Config.Subject when empty
	Audience []string
	Issuer   string    // the server's when empty
	Expiry   time.Time // an hour from now when zero
	// NotBefore is written as nbf when not zero.
	NotBefore time.Time
	// Unpublished signs with a fresh key of Alg that the JWKS does not list.
	Unpublished bool
	// Tamper flips the first byte of the signature.
	Tamper bool
	// Claims are added to (and override) the claims above.
	Claims map[string]any
	// Header members are added to (and override) the header.
	Header map[string]any
}

// Sign issues a JWT.
func (s *Server) Sign(t Token) (string, error) {
	if t.Alg == "" {
		t.Alg = AlgES256
	}
	if t.Subject == "" {
		t.Subject = s.cfg.Subject
	}
	if t.Issuer == "" {
		t.Issuer = s.issuer
	}
	if t.Expiry.IsZero() {
		t.Expiry = time.Now().Add(time.Hour)
	}
	claims := map[string]any{"iss": t.Issuer, "sub": t.Subject, "exp": t.Expiry.Unix(), "iat": time.Now().Unix()}
	switch len(t.Audience) {
	case 0:
	case 1:
		claims["aud"] = t.Audience[0]
	default:
		claims["aud"] = t.Audience
	}
	if !t.NotBefore.IsZero() {
		claims["nbf"] = t.NotBefore.Unix()
	}
	for k, v := range t.Claims {
		claims[k] = v
	}
	hdr := map[string]any{"alg": t.Alg, "typ": "at+jwt"}
	var signer *key
	switch t.Alg {
	case AlgNone, AlgHS256:
	default:
		if t.Unpublished {
			k, err := newKey(t.Alg)
			if err != nil {
				return "", err
			}
			k.kid = t.Kid
			if k.kid == "" {
				k.kid = "unpublished-" + uuid.NewString()[:8]
			}
			signer = k
		} else if signer = s.current(t.Alg, t.Kid); signer == nil {
			return "", fmt.Errorf("oidctest: no key %q of %s", t.Kid, t.Alg)
		}
		hdr["kid"] = signer.kid
	}
	for k, v := range t.Header {
		hdr[k] = v
	}
	hb, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64(hb) + "." + b64(cb)
	var sig []byte
	switch t.Alg {
	case AlgNone:
	case AlgHS256:
		// The classic confusion: an HMAC keyed with the public key set the verifier holds.
		mac := hmac.New(sha256.New, s.jwks())
		mac.Write([]byte(input))
		sig = mac.Sum(nil)
	default:
		if sig, err = signer.sign([]byte(input)); err != nil {
			return "", err
		}
	}
	if t.Tamper && len(sig) > 0 {
		sig[0] ^= 0xff
	}
	return input + "." + b64(sig), nil
}

// SignRaw signs header and payload as written (base64url-encoded here) with the current key of alg, or the key kid:
// for tokens Sign cannot express (a repeated member, a header naming another alg than its key's).
func (s *Server) SignRaw(alg, kid string, header, payload []byte) (string, error) {
	k := s.current(alg, kid)
	if k == nil {
		return "", fmt.Errorf("oidctest: no key %q of %s", kid, alg)
	}
	input := b64(header) + "." + b64(payload)
	sig, err := k.sign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

// Current is the kid of alg's current key.
func (s *Server) Current(alg string) string {
	if k := s.current(alg, ""); k != nil {
		return k.kid
	}
	return ""
}

// MustSign is Sign for tests that cannot proceed without the token.
func (s *Server) MustSign(t Token) string {
	tok, err := s.Sign(t)
	if err != nil {
		panic(err)
	}
	return tok
}

func (s *Server) current(alg, kid string) *key {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.keys) - 1; i >= 0; i-- {
		if k := s.keys[i]; k.alg == alg && (kid == "" || k.kid == kid) {
			return k
		}
	}
	return nil
}

// Registrations are the dynamic client registrations, in order.
func (s *Server) Registrations() []Registration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.regs)
}

// Authorizations are the authorization requests' parameters, in order.
func (s *Server) Authorizations() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auths)
}

// TokenRequests are the token requests, in order.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokens)
}

// LastCode is the last authorization code issued.
func (s *Server) LastCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCode
}

// Fetches is how many GET requests path received (the metadata paths, PathJWKS).
func (s *Server) Fetches(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches[path]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Hook != nil && s.cfg.Hook(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		s.mu.Lock()
		s.fetches[r.URL.Path]++
		s.mu.Unlock()
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == s.MetadataPath() && s.cfg.Discovery != DiscoveryOpenID:
		s.metadata(w)
	case r.Method == http.MethodGet && r.URL.Path == s.OpenIDPath() && s.cfg.Discovery != DiscoveryOAuth:
		s.metadata(w)
	case r.Method == http.MethodGet && r.URL.Path == PathJWKS:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.jwks())
	case r.Method == http.MethodPost && r.URL.Path == PathRegister:
		s.register(w, r)
	case r.Method == http.MethodGet && r.URL.Path == PathAuthorize:
		s.authorize(w, r)
	case r.Method == http.MethodPost && r.URL.Path == PathToken:
		s.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

// MetadataDocument is the metadata document as served (before Config.Metadata edits it).
func (s *Server) MetadataDocument() map[string]any {
	return map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.srv.URL + PathAuthorize,
		"token_endpoint":                                 s.srv.URL + PathToken,
		"registration_endpoint":                          s.srv.URL + PathRegister,
		"jwks_uri":                                       s.srv.URL + PathJWKS,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"scopes_supported":                               []string{"mcp"},
		"authorization_response_iss_parameter_supported": !s.cfg.NoISS,
	}
}

func (s *Server) metadata(w http.ResponseWriter) {
	m := s.MetadataDocument()
	if s.cfg.Metadata != nil {
		s.cfg.Metadata(m)
	}
	writeJSON(w, http.StatusOK, m)
}

// jwks is the published key set.
func (s *Server) jwks() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]map[string]any, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, k.public())
	}
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return b
}

// register is RFC 7591: a public client (token_endpoint_auth_method none) with loopback or https redirect URIs.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "not JSON")
		return
	}
	uris, ok := req["redirect_uris"].([]any)
	if !ok || len(uris) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range uris {
		su, _ := u.(string)
		if p, err := url.Parse(su); err != nil || !(p.Scheme == "https" || p.Scheme == "http" && isLoopback(p.Hostname())) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "a redirect URI is https:// or a loopback http://")
			return
		}
	}
	if m, _ := req["token_endpoint_auth_method"].(string); m != "none" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients (none)")
		return
	}
	s.mu.Lock()
	id := fmt.Sprintf("client-%d-%s", len(s.regs)+1, uuid.NewString()[:8])
	reg := Registration{ClientID: id, Request: req}
	s.clients[id] = reg
	s.regs = append(s.regs, reg)
	s.mu.Unlock()
	out := map[string]any{}
	for k, v := range req {
		out[k] = v
	}
	out["client_id"] = id
	out["client_id_issued_at"] = time.Now().Unix()
	writeJSON(w, http.StatusCreated, out)
}

// authorize approves at once: response_type code, a registered client and redirect URI, a PKCE S256 challenge.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.auths = append(s.auths, q)
	reg, known := s.clients[q.Get("client_id")]
	s.mu.Unlock()
	if !known {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || !redirectRegistered(reg, redirect) {
		http.Error(w, "redirect_uri not registered", http.StatusBadRequest)
		return
	}
	fail := func(code string) {
		v := redirect.Query()
		v.Set("error", code)
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		redirect.RawQuery = v.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type")
		return
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 43:
		fail("invalid_request")
		return
	}
	code := randomString()
	s.mu.Lock()
	s.grants[code] = &grant{clientID: reg.ClientID, redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"),
		resource: q.Get("resource"), subject: s.cfg.Subject, expires: time.Now().Add(time.Minute)}
	s.lastCode = code
	s.mu.Unlock()
	v := redirect.Query()
	v.Set("code", code)
	if st := q.Get("state"); st != "" {
		v.Set("state", st)
	}
	if !s.cfg.NoISS {
		v.Set("iss", s.issuer)
	}
	redirect.RawQuery = v.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// redirectRegistered matches a redirect URI against the client's: exactly, except that a loopback http:// URI matches
// on any port (RFC 8252 §7.3).
func redirectRegistered(reg Registration, u *url.URL) bool {
	uris, _ := reg.Request["redirect_uris"].([]any)
	for _, x := range uris {
		su, _ := x.(string)
		r, err := url.Parse(su)
		if err != nil {
			continue
		}
		if r.String() == u.String() {
			return true
		}
		if r.Scheme == "http" && u.Scheme == "http" && isLoopback(r.Hostname()) && r.Hostname() == u.Hostname() &&
			r.EscapedPath() == u.EscapedPath() && u.RawQuery == "" && u.Fragment == "" {
			return true
		}
	}
	return false
}

// token redeems a code once: the client, the redirect URI and the resource of its authorization, and the PKCE
// verifier (BASE64URL(SHA256(code_verifier)) equal to the challenge, RFC 7636 §4.6).
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "not a form")
		return
	}
	f := r.PostForm
	rec := TokenRequest{Form: f}
	s.mu.Lock()
	g := s.grants[f.Get("code")]
	if g != nil {
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		rec.VerifierOK = b64(sum[:]) == g.challenge
	}
	s.mu.Unlock()
	refuse := func(code, why string) {
		s.mu.Lock()
		s.tokens = append(s.tokens, rec)
		s.mu.Unlock()
		oauthError(w, http.StatusBadRequest, code, why)
	}
	switch {
	case f.Get("grant_type") != "authorization_code":
		refuse("unsupported_grant_type", "authorization_code only")
		return
	case g == nil || g.used || time.Now().After(g.expires):
		refuse("invalid_grant", "unknown, used or expired code")
		return
	case f.Get("client_id") != g.clientID:
		refuse("invalid_client", "the code is another client's")
		return
	case f.Get("redirect_uri") != g.redirectURI:
		refuse("invalid_grant", "redirect_uri differs from the authorization's")
		return
	case !rec.VerifierOK:
		refuse("invalid_grant", "code_verifier does not match the code_challenge")
		return
	case f.Has("resource") && f.Get("resource") != g.resource:
		refuse("invalid_target", "resource differs from the authorization's")
		return
	}
	s.mu.Lock()
	g.used = true
	s.mu.Unlock()
	var tok string
	var err error
	if s.cfg.MintKey != nil {
		tok, err = s.cfg.MintKey(r.Context(), g.subject, g.resource)
	} else {
		tok, err = s.Sign(Token{Subject: g.subject, Audience: []string{g.resource}})
	}
	switch {
	case errors.Is(err, ErrInvalidTarget):
		refuse("invalid_target", "this authorization server issues no key for that resource")
		return
	case err != nil:
		refuse("server_error", "could not issue the access token")
		return
	}
	rec.Issued = tok
	s.mu.Lock()
	s.tokens = append(s.tokens, rec)
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 3600})
}

// Follow plays the browser for an authorization URL a client printed or opened: it requests it from this server,
// which approves and redirects, then delivers that redirect to its loopback target (the CLI's listener). tamper, when
// given, may change the redirect before it is delivered (a wrong state, an error). It returns the listener's status.
func (s *Server) Follow(ctx context.Context, authorizeURL string, tamper ...func(*url.URL)) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authorizeURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return 0, fmt.Errorf("oidctest: the authorization endpoint answered %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return 0, err
	}
	for _, f := range tamper {
		f(loc)
	}
	if !isLoopback(loc.Hostname()) || loc.Scheme != "http" {
		return 0, fmt.Errorf("oidctest: the redirect %s is not a loopback http:// URL", loc.Redacted())
	}
	cb, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return 0, err
	}
	plain := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cresp, err := plain.Do(cb)
	if err != nil {
		return 0, err
	}
	cresp.Body.Close()
	return cresp.StatusCode, nil
}

// ErrInvalidTarget is a MintKey's refusal of the authorization's resource: the token endpoint answers invalid_target
// (RFC 8707 §2) and issues nothing.
var ErrInvalidTarget = errors.New("oidctest: no key is issued for that resource")

// ForResource mints with mint only when the authorization's resource is gate, the origin of the one gate whose API
// keys the server issues, and refuses any other with ErrInvalidTarget (spec/tunnel.md §6.3 step 4, D305): an API key
// has no audience, so a gate whose RFC 9728 document names this server would otherwise receive a key for the person's
// account at another gate.
func ForResource(gate string, mint func(ctx context.Context, subject, resource string) (string, error)) func(ctx context.Context, subject, resource string) (string, error) {
	return func(ctx context.Context, subject, resource string) (string, error) {
		if resource != gate {
			return "", fmt.Errorf("%w: %q, not %q", ErrInvalidTarget, resource, gate)
		}
		return mint(ctx, subject, resource)
	}
}

// DBMinter mints the access token as a kk_ API key of the account tenant on the database of the gate at origin gate,
// with the scopes Cloud's authorization server gives a kontra login key (hire and enroll:*, spec/tunnel.md §6.3), and
// only for that gate's origin as the resource (ForResource).
func DBMinter(pool *pgxpool.Pool, tenantID uuid.UUID, gate string) func(ctx context.Context, subject, resource string) (string, error) {
	keys := auth.NewAPIKeys(pool, nil)
	return ForResource(gate, func(ctx context.Context, subject, _ string) (string, error) {
		m, err := keys.Mint(ctx, auth.MintRequest{TenantID: tenantID, Name: "kontra login " + subject, Prefix: "login",
			Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}, Actor: "oidctest"})
		if err != nil {
			return "", err
		}
		return m.Secret, nil
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func oauthError(w http.ResponseWriter, status int, code, why string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": why})
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return b64(b)
}

func newKey(alg string) (*key, error) {
	k := &key{alg: alg}
	var err error
	switch alg {
	case AlgES256:
		k.priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case AlgRS256:
		k.priv, err = rsa.GenerateKey(rand.Reader, 2048)
	case AlgEdDSA:
		_, k.priv, err = ed25519.GenerateKey(rand.Reader)
	default:
		return nil, fmt.Errorf("oidctest: no keys for %s", alg)
	}
	return k, err
}

// public is the key's JWK (with its alg and use).
func (k *key) public() map[string]any {
	m := map[string]any{"kid": k.kid, "alg": k.alg, "use": "sig"}
	switch p := k.priv.Public().(type) {
	case *ecdsa.PublicKey:
		b, _ := p.Bytes()
		m["kty"], m["crv"], m["x"], m["y"] = "EC", "P-256", b64(b[1:33]), b64(b[33:])
	case *rsa.PublicKey:
		m["kty"], m["n"], m["e"] = "RSA", b64(p.N.Bytes()), b64(big.NewInt(int64(p.E)).Bytes())
	case ed25519.PublicKey:
		m["kty"], m["crv"], m["x"] = "OKP", "Ed25519", b64(p)
	}
	return m
}

func (k *key) sign(input []byte) ([]byte, error) {
	switch p := k.priv.(type) {
	case *ecdsa.PrivateKey:
		h := sha256.Sum256(input)
		r, s, err := ecdsa.Sign(rand.Reader, p, h[:])
		if err != nil {
			return nil, err
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return sig, nil
	case *rsa.PrivateKey:
		h := sha256.Sum256(input)
		return rsa.SignPKCS1v15(rand.Reader, p, crypto.SHA256, h[:])
	case ed25519.PrivateKey:
		return ed25519.Sign(p, input), nil
	}
	return nil, errors.New("oidctest: unknown key")
}

// newCA makes a throwaway CA and a leaf certificate for 127.0.0.1, ::1 and localhost signed by it.
func newCA() (tls.Certificate, []byte, *x509.CertPool, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "oidctest CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	var caPEM bytes.Buffer
	if err := pem.Encode(&caPEM, &pem.Block{Type: "CERTIFICATE", Bytes: caDER}); err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, caPEM.Bytes(), pool, nil
}
