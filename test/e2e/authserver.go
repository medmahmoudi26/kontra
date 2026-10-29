package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/oidc/oidctest"
)

// M4 (WP-07): the operator's authorization server for the MCP resource server (spec/relay.md §12.1, D276, D296)
// and kontra login (spec/tunnel.md §6.3, D278): internal/oidc/oidctest over HTTPS on 127.0.0.1, with a throwaway
// CA that the replicas and kontra login trust through SSL_CERT_FILE (a file under the run's directory). Its token
// endpoint issues, by default, a kk_ API key of an account tenant of the gate's database with the scopes hire and
// enroll:* (oidctest.DBMinter, once the test names the tenant with MintFor); with MintJWT it issues an ES256 JWT for
// the authorization's resource instead (an access token that is not an API key).

// AuthServer is the authorization server double.
type AuthServer struct {
	*oidctest.Server
	CAFile string // the CA certificate, PEM: SSL_CERT_FILE for the replicas and kontra login

	mu   sync.Mutex
	mint func(ctx context.Context, subject, resource string) (string, error)
	jwt  bool
}

// StartAuthServer starts the authorization server and writes its CA under dir.
func StartAuthServer(dir string) (*AuthServer, error) {
	as := &AuthServer{}
	srv, err := oidctest.New(oidctest.Config{MintKey: as.issue})
	if err != nil {
		return nil, err
	}
	as.Server = srv
	if as.CAFile, err = srv.WriteCA(dir); err != nil {
		srv.Close()
		return nil, err
	}
	return as, nil
}

// issue is the token endpoint's access token: a JWT for the resource with MintJWT, else the minter's key.
func (as *AuthServer) issue(ctx context.Context, subject, resource string) (string, error) {
	as.mu.Lock()
	mint, jwt := as.mint, as.jwt
	as.mu.Unlock()
	if jwt {
		return as.Sign(oidctest.Token{Alg: oidctest.AlgES256, Subject: subject, Audience: []string{resource}})
	}
	if mint == nil {
		return "", errors.New("e2e: the authorization server mints for no tenant yet (MintFor)")
	}
	return mint(ctx, subject, resource)
}

// MintFor makes the token endpoint issue kk_ keys of the tenant tenantID (hire, enroll:*), minted in pool.
func (as *AuthServer) MintFor(pool *pgxpool.Pool, tenantID uuid.UUID) {
	as.mu.Lock()
	as.mint = oidctest.DBMinter(pool, tenantID)
	as.mu.Unlock()
}

// MintJWT makes the token endpoint issue a JWT instead of an API key (on), or API keys again (off).
func (as *AuthServer) MintJWT(on bool) {
	as.mu.Lock()
	as.jwt = on
	as.mu.Unlock()
}

// authorizeURLRE is the authorization URL kontra login --no-browser prints (internal/login).
var authorizeURLRE = regexp.MustCompile(`https://\S+` + regexp.QuoteMeta(oidctest.PathAuthorize) + `\?\S+`)

// LoginResult is one kontra login run.
type LoginResult struct {
	Code   int    // its exit code
	Stdout string // what it printed
	Stderr string
	URL    string // the authorization URL it printed, "" when none
	Follow int    // the status of the loopback callback the browser stand-in reached (0: none)
}

// Login runs `kontra login --gate gate --no-browser` with HOME=home and SSL_CERT_FILE=caFile (and the run's
// markers), reads the authorization URL it prints and plays the browser with as.Follow (authorize, then the redirect
// to the CLI's loopback port), and waits for it to exit. follow false leaves the URL unvisited (the login then waits
// for its bound); a nil as visits nothing either.
func (h *Harness) Login(ctx context.Context, as *AuthServer, gate, home, caFile string, follow bool) (*LoginResult, error) {
	bin, err := nodeBinary()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "login", "--gate", gate, "--no-browser")
	var env []string
	for _, kv := range h.env() {
		if !bytes.HasPrefix([]byte(kv), []byte("HOME=")) && !bytes.HasPrefix([]byte(kv), []byte("SSL_CERT_FILE=")) {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "HOME="+home)
	if caFile != "" {
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+caFile)
	}
	cmd.SysProcAttr = procAttr()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	res := &LoginResult{}
	var stdout bytes.Buffer
	lines := bufio.NewReader(io.TeeReader(out, &stdout))
	for {
		line, rerr := lines.ReadString('\n')
		if m := authorizeURLRE.FindString(line); m != "" && res.URL == "" {
			res.URL = m
			if follow && as != nil {
				st, ferr := as.Follow(cctx, m)
				if ferr != nil {
					res.Follow = -1
				} else {
					res.Follow = st
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	werr := cmd.Wait()
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	var exit *exec.ExitError
	switch {
	case werr == nil:
	case errors.As(werr, &exit):
		res.Code = exit.ExitCode()
	default:
		return res, fmt.Errorf("e2e: kontra login: %w", werr)
	}
	return res, nil
}

// CredentialsFile is a HOME's ~/.kontra/credentials.
func CredentialsFile(home string) string {
	return filepath.Join(home, nodekey.DirName, node.CredentialsFile)
}

// fileMode is a file's permission bits, 0 when it cannot be read.
func fileMode(path string) os.FileMode {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Mode().Perm()
}
