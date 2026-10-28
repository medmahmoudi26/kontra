// Package login is `kontra login` (spec/tunnel.md §6.3, DM4-11): it gets an API key for a gate from the gate's
// authorization server and keeps it as that origin's API key in ~/.kontra/credentials (§2.4, internal/node's
// Credentials), which kontra connect then sends as the hello's auth. On an anonymous session's key, that handshake
// converts the anonymous tenant into the person's account (§6.3, internal/enroll's conversion).
//
//	kontra login [--gate URL] [--no-browser]
//
// It runs in four steps:
//
//  1. Discover reads {gate}/.well-known/oauth-protected-resource/mcp (RFC 9728, relay.md §12.1), whose resource must
//     be {gate origin}/mcp; a gate without one has no sign-in (ErrNoSignIn: "ask its operator for an API key,
//     --api-key"). The first authorization server's RFC 8414 metadata must name that issuer exactly (§3.3: a
//     document for another issuer is a mix-up), offer authorization_code with PKCE S256 and a registration_endpoint,
//     and its endpoints must be https:// (http:// only on a loopback host, the contributors' and tests' gates).
//  2. Register registers once per gate origin and issuer (RFC 7591, a public client with the loopback redirect); the
//     client_id is kept with the credential.
//  3. Authorize runs the code flow with PKCE S256, state and a loopback redirect on 127.0.0.1 (RFC 8252 §7.3),
//     listening only until a code arrives or 10 minutes pass, with resource = the gate's origin (RFC 8707); the URL
//     is opened in a browser, or printed with --no-browser. An iss in the answer must be the issuer (RFC 9207; required
//     when the metadata says it is sent). The token request follows no redirect.
//  4. The access token must be an API key of the gate (kk_…, accessToken: this revision's rule, M5's to revise);
//     anything else is refused and not stored.
//
// Neither the code nor the verifier is ever printed or logged; nothing the authorization server writes reaches the
// terminal but through tunnel.Printable. The authorization server is the operator's (Cloud's control plane, M5); the
// tests use an in-repository one (internal/oidc/oidctest).
//
// Work package: M4 WP-06.
package login

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/oidc"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// Wait bounds the loopback listener (§6.3 step 3).
const Wait = 10 * time.Minute

// Bounds of what login reads.
const (
	maxBody        = 256 << 10 // a document or an answer of the gate or its authorization server
	requestTimeout = 30 * time.Second
)

// ClientName and RedirectURI are what kontra login registers (§6.3 step 2): the redirect's port is the listener's at
// each login, which a loopback redirect may use (RFC 8252 §7.3).
const (
	ClientName  = "kontra CLI"
	RedirectURI = "http://127.0.0.1/callback"
)

// ErrUsage is a command line login refuses (exit 2).
var ErrUsage = errors.New("usage")

// ErrNoSignIn is a gate that serves no RFC 9728 document for /mcp: it has no sign-in (§6.3 step 1).
var ErrNoSignIn = errors.New("this gate has no sign-in: ask its operator for an API key and pass it to kontra connect --api-key")

const usage = `usage: kontra login [--gate <url>] [--no-browser]

kontra login gets an API key for a gate from the gate's sign-in (its operator's authorization server) and keeps it in
~/.kontra/credentials (0600) for that gate only; kontra connect then hires as it, and an anonymous session's next
handshake with it moves its services to the account (spec/tunnel.md §6.3).
  --gate URL     the gate's public URL (default https://gateway.kontra.sh); https:// except on localhost
  --no-browser   print the sign-in URL instead of opening it in a browser (open it on this machine: the sign-in
                 returns to a listener on 127.0.0.1)
`

// Options are a login's.
type Options struct {
	Gate            *url.URL // --gate (tunnel.CheckGateURL)
	NoBrowser       bool
	CredentialsPath string // ~/.kontra/credentials
	// Client makes every request (nil: NewClient(nil)).
	Client *http.Client
	// Open opens the sign-in URL in a browser (nil: the system's opener; never called with NoBrowser).
	Open func(string) error
	// Out gets the sign-in URL and the outcome; nil discards.
	Out io.Writer
	// Wait bounds the loopback listener (Wait when zero; tests shorten it).
	Wait time.Duration
}

// ParseArgs parses the arguments after `login`; getenv reads HOME for ~/.kontra. A refusal wraps ErrUsage; -h is
// flag.ErrHelp.
func ParseArgs(args []string, getenv func(string) string) (Options, error) {
	fs := flag.NewFlagSet("kontra login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gate := fs.String("gate", tunnel.DefaultGate, "")
	noBrowser := fs.Bool("no-browser", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Options{}, err
		}
		return Options{}, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return Options{}, fmt.Errorf("%w: unexpected argument %q", ErrUsage, tunnel.Printable(fs.Arg(0)))
	}
	u, err := tunnel.CheckGateURL(*gate)
	if err != nil {
		return Options{}, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	home := getenv("HOME")
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil || home == "" {
			return Options{}, fmt.Errorf("%w: no home directory for ~/%s/%s: set HOME", ErrUsage, nodekey.DirName, node.CredentialsFile)
		}
	}
	return Options{Gate: u, NoBrowser: *noBrowser, CredentialsPath: filepath.Join(home, nodekey.DirName, node.CredentialsFile)}, nil
}

// Command runs `kontra login` with args (after the subcommand): the browser URL or the instructions and the outcome on
// stdout. -h prints the usage on stdout.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := ParseArgs(args, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return nil
	}
	if err != nil {
		fmt.Fprint(stderr, usage)
		return err
	}
	o.Out = stdout
	return Run(ctx, o)
}

// Run logs in at o.Gate (§6.3) and stores the API key under the gate's origin.
func Run(ctx context.Context, o Options) error {
	if o.Gate == nil || o.CredentialsPath == "" {
		return fmt.Errorf("%w: no gate or credentials path", ErrUsage)
	}
	if o.Client == nil {
		o.Client = NewClient(nil)
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	creds, err := node.LoadCredentials(o.CredentialsPath)
	if err != nil {
		return err
	}
	s, err := Discover(ctx, o.Client, o.Gate.String())
	if err != nil {
		return err
	}
	clientID := ""
	if oc := creds.For(s.Origin).OAuthClient; oc != nil && oc.Issuer == s.Issuer && oc.ClientID != "" {
		clientID = oc.ClientID
	} else {
		if clientID, err = Register(ctx, o.Client, s); err != nil {
			return err
		}
		if _, err := creds.Update(s.Origin, func(c *node.Credential) {
			c.OAuthClient = &node.OAuthClient{Issuer: s.Issuer, ClientID: clientID}
		}); err != nil {
			return err
		}
	}
	open := o.Open
	switch {
	case o.NoBrowser:
		open = nil
	case open == nil:
		open = openBrowser
	}
	token, err := Authorize(ctx, o.Client, s, clientID, AuthorizeOptions{Open: open, Out: o.Out, Wait: o.Wait})
	if errors.Is(err, errInvalidClient) {
		// The authorization server no longer knows the client: the next login registers again.
		_, _ = creds.Update(s.Origin, func(c *node.Credential) {
			if c.OAuthClient != nil && c.OAuthClient.ClientID == clientID {
				c.OAuthClient = nil
			}
		})
	}
	if err != nil {
		return err
	}
	if err := accessToken(token); err != nil {
		return err
	}
	if _, err := creds.Update(s.Origin, func(c *node.Credential) { c.APIKey = token }); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "kontra login: signed in at %s; the API key is kept in %s for that gate only.\n"+
		"Restart kontra connect: its next handshake hires as the account, and an anonymous session's services move to the "+
		"account at new URLs (spec/tunnel.md §6.3).\n", s.Origin, creds.Path())
	return nil
}

// apiKeyRE is an API key as kontra login accepts one: kk_ (spec/relay.md §2.3, D37), then letters, digits and _, at
// most auth.MaxBearer characters in all (what the gate looks up).
var apiKeyRE = regexp.MustCompile(`^kk_[A-Za-z0-9_]+$`)

// accessToken is §6.3 step 4: the access token must be an API key of the gate (kk_…), which kontra connect sends as
// the hello's auth; anything else, a JWT included, is refused and not stored.
//
// This is the rule of this revision (DM4-11), the only place that holds it: kontra-cloud#60 has the gate accept
// Cloud's JWTs as principals in M5, which may revise it (with the hello's auth) when Cloud's authorization server is
// built.
func accessToken(tok string) error {
	if !apiKeyRE.MatchString(tok) || len(tok) > auth.MaxBearer {
		return errors.New("the authorization server's access token is not an API key of the gate (kk_…), which this " +
			"revision of kontra login needs (spec/tunnel.md §6.3): nothing was stored")
	}
	return nil
}

// NewClient is login's HTTP client: https:// (http:// only to a loopback host, the contributors' and tests' gates and
// authorization servers), the environment's proxy, no redirect followed (a redirect is an error), and a timeout per
// request. tc nil uses the system roots.
func NewClient(tc *tls.Config) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if tc != nil {
		tr.TLSClientConfig = tc.Clone()
	}
	return &http.Client{
		Transport: endpointCheck{tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("kontra login follows no redirect")
		},
		Timeout: requestTimeout,
	}
}

// endpointCheck refuses a request to a URL that is not https://, or http:// on a loopback host.
type endpointCheck struct{ next http.RoundTripper }

func (e endpointCheck) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := ssrf.CheckOperatorURL(r.URL); err != nil {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, fmt.Errorf("kontra login: %s: %w", hostOf(r.URL), err)
	}
	return e.next.RoundTrip(r)
}

func hostOf(u *url.URL) string { return tunnel.Printable(u.Scheme + "://" + u.Host) }

// Server is what discovery found: the gate's origin and its authorization server's endpoints.
type Server struct {
	Origin                string // the gate's origin, the credential's key and the resource requested
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	// ISSRequired: the metadata says authorization responses carry iss (RFC 9207 authorization_response_iss_parameter_supported).
	ISSRequired bool
}

// Discover reads the gate's RFC 9728 document for /mcp and its first authorization server's RFC 8414 metadata (else
// its OpenID configuration, as the gate reads it: oidc.MetadataURLs).
func Discover(ctx context.Context, client *http.Client, gate string) (*Server, error) {
	gu, err := tunnel.CheckGateURL(gate)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	origin, err := tunnel.CanonicalOrigin(gu)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	b, status, err := get(ctx, client, origin+oidc.MetadataPrefix+oidc.MCPPath)
	switch {
	case err != nil:
		return nil, fmt.Errorf("the gate %s: %w", origin, err)
	case status == http.StatusNotFound:
		return nil, ErrNoSignIn
	case status != http.StatusOK:
		return nil, fmt.Errorf("the gate %s answered %d for its sign-in document (RFC 9728)", origin, status)
	}
	prm, err := strictjson.Members(b)
	if err != nil {
		return nil, fmt.Errorf("the gate's sign-in document: %w", err)
	}
	var resource string
	var servers []string
	if json.Unmarshal(prm["resource"], &resource) != nil || json.Unmarshal(prm["authorization_servers"], &servers) != nil || len(servers) == 0 {
		return nil, errors.New("the gate's sign-in document names no resource or no authorization server (RFC 9728)")
	}
	// RFC 9728 §3.3: the document is for the resource it was fetched for, or it is not used.
	if resource != origin+oidc.MCPPath {
		return nil, fmt.Errorf("the gate's sign-in document is for another resource than %s/mcp: not used (RFC 9728 §3.3)", origin)
	}
	s := &Server{Origin: origin, Issuer: servers[0]}
	iu, err := url.Parse(s.Issuer)
	if err == nil {
		err = ssrf.CheckOperatorURL(iu)
	}
	if err != nil || iu.RawQuery != "" || iu.Fragment != "" {
		return nil, fmt.Errorf("the gate's authorization server %q is not an https:// URL", tunnel.Printable(clip(s.Issuer)))
	}
	if err := s.metadata(ctx, client); err != nil {
		return nil, err
	}
	return s, nil
}

// metadata reads and checks the authorization server's metadata (§6.3 step 2; RFC 8414 §3.3 against mix-up).
func (s *Server) metadata(ctx context.Context, client *http.Client) error {
	for _, mu := range oidc.MetadataURLs(s.Issuer) {
		b, status, err := get(ctx, client, mu)
		if err != nil {
			return fmt.Errorf("the authorization server: %w", err)
		}
		if status >= 400 && status < 500 {
			continue
		}
		if status != http.StatusOK {
			return fmt.Errorf("the authorization server answered %d for its metadata", status)
		}
		m, err := strictjson.Members(b)
		if err != nil {
			return fmt.Errorf("the authorization server's metadata: %w", err)
		}
		var md struct {
			Issuer         string   `json:"issuer"`
			Authorization  string   `json:"authorization_endpoint"`
			Token          string   `json:"token_endpoint"`
			Registration   string   `json:"registration_endpoint"`
			Responses      []string `json:"response_types_supported"`
			Grants         []string `json:"grant_types_supported"`
			Challenges     []string `json:"code_challenge_methods_supported"`
			ISSParameter   bool     `json:"authorization_response_iss_parameter_supported"`
			TokenEndpoints []string `json:"token_endpoint_auth_methods_supported"`
		}
		if err := json.Unmarshal(b, &md); err != nil {
			return fmt.Errorf("the authorization server's metadata: %w", err)
		}
		if _, ok := m["issuer"]; !ok || md.Issuer != s.Issuer {
			return errors.New("the authorization server's metadata names another issuer than the gate's sign-in document " +
				"(RFC 8414 §3.3): not used")
		}
		for _, e := range []struct{ name, v string }{{"authorization_endpoint", md.Authorization},
			{"token_endpoint", md.Token}, {"registration_endpoint", md.Registration}} {
			if e.v == "" {
				return fmt.Errorf("the authorization server offers no %s: kontra login needs it (spec/tunnel.md §6.3)", e.name)
			}
			u, err := url.Parse(e.v)
			if err == nil {
				err = ssrf.CheckOperatorURL(u)
			}
			if err != nil || u.Fragment != "" {
				return fmt.Errorf("the authorization server's %s is not an https:// URL", e.name)
			}
		}
		switch {
		case !slices.Contains(md.Challenges, "S256"):
			return errors.New("the authorization server does not offer PKCE S256 (code_challenge_methods_supported)")
		case md.Responses != nil && !slices.Contains(md.Responses, "code"):
			return errors.New("the authorization server does not offer the authorization code flow (response_types_supported)")
		case md.Grants != nil && !slices.Contains(md.Grants, "authorization_code"):
			return errors.New("the authorization server does not offer the authorization_code grant (grant_types_supported)")
		case md.TokenEndpoints != nil && !slices.Contains(md.TokenEndpoints, "none"):
			return errors.New("the authorization server takes no public clients (token_endpoint_auth_methods_supported has no none)")
		}
		s.AuthorizationEndpoint, s.TokenEndpoint, s.RegistrationEndpoint = md.Authorization, md.Token, md.Registration
		s.ISSRequired = md.ISSParameter
		return nil
	}
	return errors.New("the authorization server publishes no metadata (RFC 8414)")
}

// Register registers the CLI as a public client (RFC 7591) and returns its client_id.
func Register(ctx context.Context, client *http.Client, s *Server) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                ClientName,
		"redirect_uris":              []string{RedirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	b, status, err := do(client, req)
	if err != nil {
		return "", fmt.Errorf("the client registration: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("the authorization server refused the client registration: %s", oauthError(b, status))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ClientID == "" {
		return "", errors.New("the client registration answered no client_id")
	}
	return out.ClientID, nil
}

// AuthorizeOptions are how Authorize reaches the person.
type AuthorizeOptions struct {
	Open func(string) error // opens the URL in a browser; nil prints it to Out
	Out  io.Writer
	Wait time.Duration // the listener's bound (Wait when zero)
}

// errInvalidClient is a token endpoint that no longer knows the client.
var errInvalidClient = errors.New("the authorization server does not know this client")

// Authorize runs the authorization code flow with PKCE and a loopback redirect and returns the access token, which
// the caller checks is an API key of the gate. The listener is on 127.0.0.1 only and is closed when Authorize
// returns, after the code, the bound or ctx's end.
func Authorize(ctx context.Context, client *http.Client, s *Server, clientID string, o AuthorizeOptions) (string, error) {
	if o.Wait <= 0 {
		o.Wait = Wait
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	verifier, state := random(32), random(16)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("the loopback listener for the sign-in: %w", err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	au, err := url.Parse(s.AuthorizationEndpoint)
	if err != nil {
		ln.Close()
		return "", err
	}
	q := au.Query()
	for k, v := range map[string]string{
		"response_type": "code", "client_id": clientID, "redirect_uri": redirect, "code_challenge": challenge,
		"code_challenge_method": "S256", "state": state, "resource": s.Origin,
	} {
		q.Set(k, v)
	}
	au.RawQuery = q.Encode()

	type outcome struct {
		code string
		err  error
	}
	got := make(chan outcome, 1)
	deliver := func(o outcome) {
		select {
		case got <- o:
		default:
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(v.Get("state")), []byte(state)) != 1 {
			http.Error(w, "kontra login: this answer is not for the sign-in kontra login started (state); it stops.", http.StatusBadRequest)
			deliver(outcome{err: errors.New("the sign-in's answer carried another state than kontra login sent: refused")})
			return
		}
		if e := v.Get("error"); e != "" {
			http.Error(w, "kontra login: the sign-in was refused.", http.StatusBadRequest)
			deliver(outcome{err: fmt.Errorf("the authorization server refused the sign-in: %s", printable(e, v.Get("error_description")))})
			return
		}
		// RFC 9207: an iss must be the issuer; and it must be there when the metadata says it is sent.
		if iss, ok := v["iss"]; (ok && (len(iss) != 1 || iss[0] != s.Issuer)) || (!ok && s.ISSRequired) {
			http.Error(w, "kontra login: this answer is from another authorization server; it stops.", http.StatusBadRequest)
			deliver(outcome{err: errors.New("the sign-in's answer names another authorization server (iss, RFC 9207): refused")})
			return
		}
		code := v.Get("code")
		if code == "" {
			http.Error(w, "kontra login: the answer carries no code.", http.StatusBadRequest)
			deliver(outcome{err: errors.New("the sign-in's answer carried no code")})
			return
		}
		_, _ = io.WriteString(w, "kontra login: signed in. You may close this tab.\n")
		deliver(outcome{code: code})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // closes the listener
		_ = srv.Close()
	}()

	shown := tunnel.Printable(au.String())
	if o.Open == nil {
		fmt.Fprintf(o.Out, "kontra login: open this URL in a browser on this machine to sign in at %s:\n\n  %s\n\n", s.Origin, shown)
	} else {
		fmt.Fprintf(o.Out, "kontra login: signing in at %s in your browser; if it does not open, open this URL on this machine:\n\n  %s\n\n",
			s.Origin, shown)
		if err := o.Open(au.String()); err != nil {
			fmt.Fprintf(o.Out, "kontra login: the browser could not be opened (%v): open the URL above\n", err)
		}
	}
	timer := time.NewTimer(o.Wait)
	defer timer.Stop()
	var res outcome
	select {
	case res = <-got:
	case <-timer.C:
		return "", fmt.Errorf("no sign-in arrived within %s: run kontra login again", o.Wait)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if res.err != nil {
		return "", res.err
	}
	return redeem(ctx, client, s, clientID, res.code, verifier, redirect)
}

// redeem is the token request (RFC 6749 §4.1.3, RFC 7636 §4.5, RFC 8707): it follows no redirect, and nothing it
// says names the code or the verifier.
func redeem(ctx context.Context, client *http.Client, s *Server, clientID, code, verifier, redirect string) (string, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {clientID},
		"code_verifier": {verifier}, "resource": {s.Origin},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	b, status, err := do(client, req)
	scrub := func(s string) string {
		for _, secret := range []string{code, verifier} {
			s = strings.ReplaceAll(s, secret, "…")
		}
		return s
	}
	if err != nil {
		return "", fmt.Errorf("the token request: %s", scrub(err.Error()))
	}
	if status != http.StatusOK {
		e := scrub(oauthError(b, status))
		if strings.HasPrefix(e, "invalid_client") {
			return "", fmt.Errorf("%w (%s): run kontra login again", errInvalidClient, e)
		}
		return "", fmt.Errorf("the authorization server refused the token request: %s", e)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("the token response carries no access_token")
	}
	if !strings.EqualFold(out.TokenType, "bearer") {
		return "", errors.New("the token response's token_type is not Bearer: nothing was stored")
	}
	return out.AccessToken, nil
}

// get fetches a document, its body bounded.
func get(ctx context.Context, client *http.Client, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	return do(client, req)
}

// do sends req and reads the answer, at most maxBody bytes; a redirect is an error (the client follows none).
func do(client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the URL, whose query may be anything
		}
		return nil, 0, fmt.Errorf("%s: %w", hostOf(req.URL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, fmt.Errorf("%s answered a redirect (%d), which kontra login does not follow", hostOf(req.URL), resp.StatusCode)
	}
	b, err := ssrf.ReadBody(resp.Body, maxBody)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// oauthError is an OAuth error answer (RFC 6749 §5.2) for the terminal: "error: description", printable and bounded.
func oauthError(b []byte, status int) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return printable(e.Error, e.Description)
}

func printable(code, desc string) string {
	s := clip(code)
	if desc != "" {
		s += ": " + clip(desc)
	}
	return tunnel.Printable(s)
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// random is n random bytes, base64url: the PKCE verifier (32 bytes, 43 characters, RFC 7636 §4.1) and state.
func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on a supported platform
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// openBrowser opens u in the system's browser: open on macOS, rundll32 on Windows, xdg-open elsewhere.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
