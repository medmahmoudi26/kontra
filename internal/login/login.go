// Package login is `kontra login` (spec/tunnel.md §6.3, DM4-11): it gets an API key for a gate from the gate's
// authorization server and keeps it as that origin's API key in ~/.kontra/credentials (§2.4, internal/node's
// Credentials), which kontra connect then sends as the hello's auth. On an anonymous session's key, that handshake
// converts the anonymous tenant into the person's account (§6.3, internal/anon's Convert).
//
//	kontra login [--gate URL] [--no-browser]
//
// It runs in four steps:
//
//  1. Discover reads {gate}/.well-known/oauth-protected-resource/mcp (RFC 9728, relay.md §12.1); a gate without
//     one has no sign-in (ErrNoSignIn: "ask its operator for an API key, --api-key").
//  2. The first authorization server's RFC 8414 metadata must offer authorization_code with PKCE S256 and a
//     registration_endpoint; Register registers once per gate origin (RFC 7591, a public client with the loopback
//     redirect) and the client_id is kept with the credential.
//  3. Authorize runs the code flow with PKCE S256, state and a loopback redirect on 127.0.0.1 (RFC 8252 §7.3),
//     listening only until a code arrives or 10 minutes pass, with resource = the gate's origin (RFC 8707); the URL
//     is opened in a browser, or printed with --no-browser.
//  4. The access token must be an API key of the gate (kk_…); anything else is refused and not stored.
//
// The authorization server is the operator's (Cloud's control plane, M5); the e2e tests use an in-repository one.
//
// Work package: M4 WP-06.
package login

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"kontra.sh/kontra/internal/unimpl"
)

// Wait bounds the loopback listener (§6.3 step 3).
const Wait = 10 * time.Minute

// ErrUsage is a command line login refuses (exit 2).
var ErrUsage = errors.New("usage")

// ErrNoSignIn is a gate that serves no RFC 9728 document for /mcp: it has no sign-in (§6.3 step 1).
var ErrNoSignIn = errors.New("this gate has no sign-in: ask its operator for an API key and pass it to kontra connect --api-key")

// Command runs `kontra login` with args (after the subcommand): the browser URL or the instructions on stdout, the
// outcome on stderr.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return unimpl.Error("kontra login", "M4 WP-06")
}

// Server is what discovery found: the gate's origin and its authorization server's endpoints.
type Server struct {
	Origin                string // the gate's origin, the credential's key and the resource requested
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
}

// Discover reads the gate's RFC 9728 document for /mcp and its first authorization server's RFC 8414 metadata.
func Discover(ctx context.Context, client *http.Client, gate string) (*Server, error) {
	return nil, unimpl.Error("login.Discover", "M4 WP-06")
}

// Register registers the CLI as a public client (RFC 7591) and returns its client_id.
func Register(ctx context.Context, client *http.Client, s *Server) (string, error) {
	return "", unimpl.Error("login.Register", "M4 WP-06")
}

// Authorize runs the authorization code flow with PKCE and a loopback redirect and returns the access token, which
// the caller checks is an API key of the gate. open opens a URL in the browser (nil: print it to out).
func Authorize(ctx context.Context, client *http.Client, s *Server, clientID string, open func(string) error, out io.Writer) (string, error) {
	return "", unimpl.Error("login.Authorize", "M4 WP-06")
}
