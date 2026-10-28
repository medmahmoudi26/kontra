package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
)

func TestSubcommands(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		args        []string
		code        int
		out, errOut string
	}{
		{[]string{"version"}, 0, "kontra", ""},
		{nil, 2, "", "usage:"},
		{[]string{"nope"}, 2, "", "usage:"},
		{[]string{"seal"}, 2, "", "kontra seal --service <service URL>"},
		{[]string{"seal", "--service", "http://gate.example/agents/x", "-"}, 2, "", "https:// except on localhost"},
		{[]string{"seal", "--open", "/nonexistent/part.json"}, 1, "", "kontra seal: open /nonexistent/part.json"},
		{[]string{"connect", "-h"}, 0, "--sealed-max-age DURATION", ""},
		{[]string{"connect", "-h"}, 0, "--accept-change NAME", ""},
		{[]string{"console"}, 2, "", "usage:"},                                               // no local console (D164)
		{[]string{"login"}, 1, "", "kontra login: kontra login: not implemented (M4 WP-06)"}, // the M4 skeleton's stub
		{[]string{"publish"}, 1, "", "kontra publish: not until M5"},
		{[]string{"connect", "-h"}, 0, "--serve a2a:NAME=URL", ""},
		{[]string{"connect", "-h"}, 0, "--listen ADDR", ""},
		{[]string{"connect", "-h"}, 0, "mcp:NAME=stdio:CMD", ""},
		{[]string{"connect", "-h"}, 0, "--roots NAME=DIR", ""},
		{[]string{"connect", "-h"}, 0, "--public NAME", ""},                        // spec/tunnel.md §6.1 (M4 WP-03)
		{[]string{"connect", "-h"}, 0, "exits 1 saying the anonymous session", ""}, // §6.1
	} {
		var stdout, stderr bytes.Buffer
		if code := run(ctx, c.args, &stdout, &stderr); code != c.code ||
			!strings.Contains(stdout.String(), c.out) || !strings.Contains(stderr.String(), c.errOut) {
			t.Errorf("kontra %q = %d, stdout %q, stderr %q; want %d, %q, %q", c.args, code, stdout.String(), stderr.String(),
				c.code, c.out, c.errOut)
		}
	}
}

// isolate gives the test a HOME of its own and no KONTRA_KEY from the environment.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KONTRA_KEY", "")
	return home
}

// The flags' defaults and values (spec/tunnel.md §2.1, §2.4, §3, §14).
func TestConnectFlags(t *testing.T) {
	home := isolate(t)
	cfg, err := node.ParseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999"}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gate.String() != tunnel.DefaultGate || cfg.Gate.String() != "https://gateway.kontra.sh" || cfg.MaxInflight != 64 ||
		cfg.KeyPath != filepath.Join(home, ".kontra", "identity.key") || cfg.CredentialsPath != filepath.Join(home, ".kontra", "credentials") ||
		cfg.Name == "" || cfg.LogJSON {
		t.Fatalf("defaults: %+v", cfg)
	}

	// KONTRA_KEY is honoured.
	t.Setenv("KONTRA_KEY", "/run/secrets/kontra.key")
	if cfg, err := node.ParseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999"}, os.Getenv); err != nil || cfg.KeyPath != "/run/secrets/kontra.key" {
		t.Fatalf("KONTRA_KEY: %q %v", cfg.KeyPath, err)
	}
	t.Setenv("KONTRA_KEY", "")

	// --enroll @file and --enroll - read the token; --api-key too.
	tok := "ket_0123456789abcdefghijklmnopqrstuv"
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = node.ParseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", "@" + file, "--api-key", "kk_live_x"}, os.Getenv)
	if err != nil || !slices.Equal(cfg.Enroll, []string{tok}) || cfg.APIKey != "kk_live_x" {
		t.Fatalf("--enroll @file: %v %q %v", cfg.Enroll, cfg.APIKey, err)
	}
	withStdin(t, tok+"\n", func() {
		cfg, err = node.ParseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", "-"}, os.Getenv)
	})
	if err != nil || !slices.Equal(cfg.Enroll, []string{tok}) {
		t.Fatalf("--enroll -: %v %v", cfg.Enroll, err)
	}
}

// withStdin runs f with os.Stdin reading s.
func withStdin(t *testing.T, s string, f func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { io.WriteString(w, s); w.Close() }()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()
	f()
}

// Refused command lines are usage errors (exit 2) with a message naming what is wrong; nothing starts.
func TestConnectRefusesFlags(t *testing.T) {
	home := isolate(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--serve", "mcp:fs=stdio:npx 'unterminated"}, "unterminated"},
		{[]string{"--serve", "mcp:fs=stdio:npx server > log"}, "without a shell"},
		{[]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--roots", "hello=/"}, "no --serve mcp:hello"},
		{[]string{"--serve", "mcp:fs=http://127.0.0.1:8000/mcp", "--roots", "fs=relative"}, "absolute"},
		{[]string{"--serve", "mcp:fs=http://127.0.0.1:8000/mcp", "--env", "A=b"}, "variable name"},
		{[]string{"--serve", "mcp:fs=http://127.0.0.1:8000/mcp", "--max-sessions", "0"}, "--max-sessions"},
		{[]string{"--serve", "mcp:fs=http://127.0.0.1:8000/mcp", "--session-idle", "0s"}, "--session-idle"},
		{[]string{"--serve", "a2a:x=http://h:1/base"}, "base path"},
		{[]string{"--serve", "a2a:x=http://h:1/base"}, "conflict"},
		{[]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--gate", "http://gate.example"}, "--gate"},
		{[]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--gate", "https://gate.example/?k=v"}, "no query"},
		{[]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", "nope"}, "not an enrollment token"},
		{[]string{"--gate", "https://gate.example", "--listen", "0.0.0.0:7777"}, "--local-token"},
		{[]string{"--listen", "127.0.0.1"}, "host:port"},
		{[]string{"--local-token", "/nonexistent/token-file"}, "--local-token"},
		{[]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--bogus"}, "bogus"},
		{[]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--public", "other"}, "--public other: no --serve serves"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"connect"}, c.args...), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("connect %q = %d, %q; want 2 and %q", c.args, code, stderr.String(), c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".kontra")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused command line created ~/.kontra: %v", err)
	}
}

// A key file readable by others stops kontra connect (exit 1) before it dials (§2.1).
func TestConnectRefusesAReadableKey(t *testing.T) {
	home := isolate(t)
	key := filepath.Join(home, ".kontra", "identity.key")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("# not even a key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"connect", "--gate", "http://127.0.0.1:1", "--listen", "127.0.0.1:0", "--serve", "a2a:hello=http://127.0.0.1:9"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "mode 0644") {
		t.Fatalf("connect with a 0644 key = %d, %q", code, stderr.String())
	}
}

type refuseAll struct{}

func (refuseAll) Admit(context.Context, enroll.Request) (*enroll.Decision, error) {
	return nil, tunnel.Refuse(tunnel.CodeUnknownKey, "unexpected")
}

type noSink struct{}

func (noSink) Add(context.Context, *tunnelgate.Session, *yamux.Session, []tunnel.WelcomeService) error {
	return errors.New("unexpected")
}

// A handshake refused with a code the node does not retry ends kontra connect with exit 1 and the code
// (spec/tunnel.md §4): here bad_origin, from a gate whose public_url is another origin.
func TestConnectExitsOnANonRetryableCode(t *testing.T) {
	isolate(t)
	h, err := tunnelgate.NewHandler(tunnelgate.HandlerConfig{
		PublicURL: "https://gate.example", ChannelBinding: tunnel.CBNone, ReplicaID: "r", Admitter: refuseAll{},
		Registry: noSink{}, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(h.ServeHTTP))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"connect", "--gate", srv.URL, "--listen", "127.0.0.1:0", "--serve", "a2a:hello=http://127.0.0.1:9"}, &stdout, &stderr)
	if ctx.Err() != nil {
		t.Fatal("kontra connect kept retrying a non-retryable code")
	}
	if code != 1 || !strings.Contains(stderr.String(), "bad_origin") {
		t.Fatalf("connect = %d, stderr %q", code, stderr.String())
	}
}

// refuseHostile refuses every hello service_denied with a message carrying terminal escape sequences: an
// OSC 52 clipboard write, a screen clear and a bidi override.
type refuseHostile struct{}

func (refuseHostile) Admit(context.Context, enroll.Request) (*enroll.Decision, error) {
	return nil, tunnel.Refuse(tunnel.CodeServiceDenied, "denied\x1b]52;c;cm0gLXJmIH4K\x07\x1b[2J\u202etxt.exe")
}

// What a gate says reaches the operator's terminal without its escape sequences (M2 review, security-5): a
// refusal's message, printed as kontra connect's final error, has its control characters escaped, as the
// logs' quoted values do.
func TestConnectPrintsNoGateEscapes(t *testing.T) {
	isolate(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	defer srv.Close()
	gate, err := tunnelgate.NewHandler(tunnelgate.HandlerConfig{
		PublicURL: srv.URL, ChannelBinding: tunnel.CBNone, ReplicaID: "r", Admitter: refuseHostile{},
		Registry: noSink{}, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	h = gate
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"connect", "--gate", srv.URL, "--listen", "127.0.0.1:0", "--serve", "a2a:hello=http://127.0.0.1:9"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "service_denied") {
		t.Fatalf("connect = %d, stderr %q", code, stderr.String())
	}
	for name, out := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
		if i := strings.IndexFunc(out, func(r rune) bool { return r != '\n' && !unicode.IsGraphic(r) }); i >= 0 {
			t.Fatalf("%s carries %q from the gate: %q", name, out[i:i+1], out)
		}
	}
	if !strings.Contains(stderr.String(), `denied\x1b]52;c;cm0gLXJmIH4K\a\x1b[2J\u202etxt.exe`) {
		t.Fatalf("the refusal's message is not shown escaped:\n%s", stderr.String())
	}
}

// admitAnonymous admits every hello as an anonymous session that ends at end (spec/tunnel.md §6.1), and records
// whether the hello asked for one.
type admitAnonymous struct {
	end   time.Time
	asked chan bool
}

func (a admitAnonymous) Admit(_ context.Context, req enroll.Request) (*enroll.Decision, error) {
	a.asked <- req.Hello.Anonymous
	d := &enroll.Decision{
		Key:           enroll.Key{KeyID: req.Hello.KID, TenantID: uuid.Must(uuid.NewV7()), Tenant: "anon-0123456789abcdef", PubKey: req.PubKey},
		HirePrincipal: "node:" + req.Hello.KID,
		Anonymous:     &enroll.Anonymous{ExpiresAt: a.end},
	}
	for _, s := range req.Hello.Services {
		d.Served = append(d.Served, tunnel.WelcomeService{Kind: s.Kind, Name: s.Name, URL: "https://gate.example/agents/anon-0123456789abcdef/" + s.Name})
	}
	return d, nil
}

type acceptSink struct{}

func (acceptSink) Add(context.Context, *tunnelgate.Session, *yamux.Session, []tunnel.WelcomeService) error {
	return nil
}

// An anonymous session's end makes kontra connect exit 1 with a message naming the end (spec/tunnel.md §6.1, M4 WP-03):
// it printed the anonymous URL and the end, the gate's {"t": "expiring"} and then {"t": "revoked"} at the end.
func TestConnectExitsWhenTheAnonymousSessionExpires(t *testing.T) {
	isolate(t)
	end := time.Now().Add(3 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
	adm := admitAnonymous{end: end, asked: make(chan bool, 4)}
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	defer srv.Close()
	gate, err := tunnelgate.NewHandler(tunnelgate.HandlerConfig{
		PublicURL: srv.URL, ChannelBinding: tunnel.CBNone, ReplicaID: "r", Admitter: adm, Registry: acceptSink{},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	h = gate
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"connect", "--gate", srv.URL, "--listen", "127.0.0.1:0", "--serve", "a2a:hello=http://127.0.0.1:9",
		"--public", "hello"}, &stdout, &stderr)
	if ctx.Err() != nil {
		t.Fatal("kontra connect did not stop at the anonymous session's end")
	}
	stamp := end.Format(time.RFC3339)
	if code != 1 || !strings.Contains(stderr.String(), "kontra connect: the anonymous session expired at "+stamp) {
		t.Fatalf("connect = %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{
		"https://gate.example/agents/anon-0123456789abcdef/hello  (public: anyone may call it)",
		"anonymous session: it ends at " + stamp,
		"kontra connect: the anonymous session ends at " + stamp,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if asked := <-adm.asked; !asked {
		t.Error("the hello did not ask for an anonymous session")
	}
	select {
	case <-adm.asked:
		t.Error("kontra connect redialed after the end")
	default:
	}
}
