package node

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/stdiobridge"
	"kontra.sh/kontra/internal/tunnel"
)

// --serve mcp: (spec/tunnel.md §10): a URL target may name its endpoint path; a stdio command line is split with
// POSIX word rules and a leading ~/ expanded with the HOME kontra connect was given; an A2A target keeps D123's
// root-only rule.
func TestParseServeMCP(t *testing.T) {
	for _, c := range []struct {
		in, path string
	}{
		{"mcp:everything=http://127.0.0.1:3001/mcp", "/mcp"},
		{"mcp:everything=http://127.0.0.1:3001/mcp/", "/mcp/"},
		{"mcp:web=http://127.0.0.1:3001", "/"},
		{"mcp:web=https://mcp.internal.example/v1/mcp", "/v1/mcp"},
	} {
		s, err := parseServe(c.in, "/home/u")
		if err != nil || s.Kind != tunnel.KindMCP || s.Target == nil || s.Target.Path != c.path || s.Command != nil {
			t.Errorf("%s: %+v %v, want path %q", c.in, s, err, c.path)
		}
		if err := checkServe(s); err != nil {
			t.Errorf("%s: checkServe: %v", c.in, err)
		}
	}
	s, err := parseServe(`mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs "/srv/my dir"`, "/home/u")
	if err != nil || s.Kind != tunnel.KindMCP || s.Target != nil ||
		!slices.Equal(s.Command, []string{"npx", "-y", "@modelcontextprotocol/server-filesystem@1.0.0", "/home/u/docs", "/srv/my dir"}) {
		t.Fatalf("stdio: %+v %v", s, err)
	}
	if err := checkServe(s); err != nil {
		t.Errorf("stdio: checkServe: %v", err)
	}
	if _, err := parseServe("a2a:hello=http://127.0.0.1:9999/base", "/home/u"); !errors.Is(err, ErrBasePath) {
		t.Errorf("an A2A base path: %v", err)
	}
	for _, bad := range []Serve{
		{Kind: tunnel.KindA2A, Name: "x", Command: []string{"a"}},
		{Kind: tunnel.KindMCP, Name: "x", Command: []string{}},
		{Kind: tunnel.KindMCP, Name: "x", Command: []string{""}},
		{Kind: tunnel.KindMCP, Name: "x"},
	} {
		if err := checkServe(bad); err == nil {
			t.Errorf("checkServe(%+v) accepted it", bad)
		}
	}
}

// The MCP flags: --roots in flag order per service, --env names once each, --session-idle and --max-sessions, with
// their defaults.
func TestParseArgsMCP(t *testing.T) {
	home := t.TempDir()
	env := func(k string) string { return map[string]string{"HOME": home}[k] }
	a, c := t.TempDir(), filepath.Join(t.TempDir(), "c,d")
	if err := os.Mkdir(c, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseArgs([]string{"--serve", "mcp:fs=stdio:~/bin/server /srv", "--serve", "mcp:web=http://127.0.0.1:3001/mcp",
		"--roots", "fs=" + a, "--roots", "fs=" + c, "--roots", "web=" + a, "--env", "KONTRA_E2E_RUN", "--env", "X_Y",
		"--env", "KONTRA_E2E_RUN", "--session-idle", "90m", "--max-sessions", "3"}, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Serves) != 2 || !slices.Equal(cfg.Serves[0].Command, []string{home + "/bin/server", "/srv"}) || cfg.Serves[1].Target.Path != "/mcp" {
		t.Errorf("serves: %+v", cfg.Serves)
	}
	if !slices.Equal(cfg.Roots["fs"].Dirs(), []string{a, c}) || !slices.Equal(cfg.Roots["web"].Dirs(), []string{a}) {
		t.Errorf("roots: %v %v", cfg.Roots["fs"].Dirs(), cfg.Roots["web"].Dirs())
	}
	if !slices.Equal(cfg.Env, []string{"KONTRA_E2E_RUN", "X_Y"}) || cfg.SessionIdle != 90*time.Minute || cfg.MaxSessions != 3 {
		t.Errorf("env %v, idle %v, max %d", cfg.Env, cfg.SessionIdle, cfg.MaxSessions)
	}
	cfg, err = parseArgs([]string{"--serve", "mcp:fs=stdio:server"}, env, nil)
	if err != nil || cfg.SessionIdle != stdiobridge.DefaultSessionIdle || cfg.MaxSessions != stdiobridge.DefaultMaxSessions ||
		len(cfg.Roots) != 0 || len(cfg.Env) != 0 {
		t.Errorf("defaults: %+v %v", cfg, err)
	}
}

// New refuses --roots for a service that is not an MCP --serve (a Config built by hand), and builds a bridge per
// stdio service and a roots filter per URL service.
func TestNewMCPServices(t *testing.T) {
	home := t.TempDir()
	env := func(k string) string { return map[string]string{"HOME": home}[k] }
	cfg, err := parseArgs([]string{"--gate", "http://localhost:1", "--serve", "mcp:fs=stdio:server", "--serve",
		"mcp:web=http://127.0.0.1:3001/mcp", "--serve", "a2a:hello=http://127.0.0.1:9999"}, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roots = map[string]*mcproots.Roots{"hello": mcproots.New(home)}
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "--roots hello") {
		t.Errorf("roots for an A2A service: %v", err)
	}
	cfg.Roots = map[string]*mcproots.Roots{"fs": mcproots.New(home)}
	n, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n.proxy.bridges["fs"] == nil || n.proxy.mcp["web"] == nil || n.proxy.mcp["fs"] != nil || n.proxy.bridges["web"] != nil ||
		len(n.proxy.bridges)+len(n.proxy.mcp) != 2 {
		t.Errorf("bridges %v, filters %v", n.proxy.bridges, n.proxy.mcp)
	}
	q := n.proxy.forSession([]string{"fs", "web"}, nil)
	if q.bridges["fs"] != n.proxy.bridges["fs"] || q.mcp["web"] != n.proxy.mcp["web"] {
		t.Errorf("a session's proxy does not share the bridges and filters")
	}
	n.proxy.close()
}
