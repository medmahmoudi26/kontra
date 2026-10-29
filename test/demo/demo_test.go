// Package demo tests the README demo and its clean-laptop kit (DM4-13, DM4-15, DM4-31; the M4 plan's gate run B2):
// README.md's block between the demo markers, scripts/demo-clean-laptop.sh against a stub docker (nothing is built or
// run), and, with KONTRA_KIT_TESTS=1, the laptop image (test/demo/Dockerfile) and a rehearsal of the whole run against
// a local multi-tenant gate with anonymous sessions, answering for gateway.kontra.run on a docker network.
//
// Work package: M4 WP-08.
package demo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/test/kit"
)

const (
	script      = "scripts/demo-clean-laptop.sh"
	placeholder = "anon-<…>"
	installLine = "go install kontra.sh/kontra/cmd/kontra@latest"
)

func readme(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(kit.Root(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// block is README's text between the demo markers (the markers excluded).
func block(t *testing.T, text string) string {
	t.Helper()
	const begin, end = "\n<!-- demo:begin -->\n", "\n<!-- demo:end -->\n"
	if n := strings.Count(text, begin); n != 1 {
		t.Fatalf("README.md has %d <!-- demo:begin --> lines, want 1", n)
	}
	if n := strings.Count(text, end); n != 1 {
		t.Fatalf("README.md has %d <!-- demo:end --> lines, want 1", n)
	}
	i, j := strings.Index(text, begin), strings.Index(text, end)
	if j < i {
		t.Fatal("README.md's demo:end comes before demo:begin")
	}
	return text[i+len(begin) : j+1]
}

// commands are the block's commands as scripts/demo-clean-laptop.sh reads them: the fence lines dropped, lines ending in
// a backslash joined with the next, blank and comment-only lines dropped.
func commands(b string) []string {
	var out []string
	cur := ""
	for _, line := range strings.Split(strings.TrimSuffix(b, "\n"), "\n") {
		if strings.HasPrefix(line, "```") {
			continue
		}
		if strings.HasSuffix(line, `\`) {
			cur += strings.TrimSuffix(line, `\`)
			continue
		}
		cur += line
		trimmed := strings.TrimLeft(cur, " \t")
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			out = append(out, trimmed)
		}
		cur = ""
	}
	return out
}

var slugRE = regexp.MustCompile(`^anon-[0-9a-f]{16}$`)

// The README's demo block (DM4-13; the M4 plan's WP-08 acceptance; the audit's gaps 12): the markers, the real a2a-cli
// syntax (D15), the staging gate, --public, the go install at @latest, the slug as the documented placeholder, pinned
// MCP packages; no Homebrew, no console on :7778 (D164), no short slug, no production gate in the block.
func TestREADMEDemoBlock(t *testing.T) {
	text := readme(t)
	b := block(t, text)
	if !strings.HasPrefix(b, "```sh\n") || !strings.HasSuffix(b, "```\n") {
		t.Errorf("the demo block is not one ```sh fence:\n%s", b)
	}
	cmds := commands(b)
	has := func(pred func(string) bool) bool { return slices.ContainsFunc(cmds, pred) }
	for _, want := range []struct {
		what string
		ok   func(string) bool
	}{
		{"the install at @latest", func(c string) bool { return c == installLine }},
		{"a2a-cli v0.3.1 by go install", func(c string) bool {
			return strings.HasPrefix(c, "go install github.com/a2aproject/a2a-cli@v0.3.1 && mv ")
		}},
		{"a2a server --exec", func(c string) bool { return strings.HasPrefix(c, "a2a server --exec 'echo Hello World' --port 9999 &") }},
		{"kontra connect --gate https://gateway.kontra.run … --public", func(c string) bool {
			return strings.HasPrefix(c, "kontra connect --gate https://gateway.kontra.run ") && strings.Contains(c, " --public hello") &&
				strings.Contains(c, "--serve a2a:hello=http://127.0.0.1:9999") &&
				strings.Contains(c, "--serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs'") &&
				strings.HasSuffix(c, "&")
		}},
		{"a2a send -a …/.well-known/agent-card.json", func(c string) bool {
			return regexp.MustCompile(`^a2a send -a http://localhost:7777/agents/anon-<…>/hello/\.well-known/agent-card\.json "hi"$`).MatchString(c)
		}},
		{"a stock MCP client on the node's /mcp/anon-…/fs", func(c string) bool {
			return regexp.MustCompile(`^npx -y @modelcontextprotocol/inspector@\d+\.\d+\.\d+ --cli http://localhost:7777/mcp/anon-<…>/fs --transport http\s+--method tools/list$`).MatchString(c)
		}},
		{"hire on /mcp", func(c string) bool {
			return regexp.MustCompile(`^npx -y @modelcontextprotocol/inspector@\d+\.\d+\.\d+ --cli http://localhost:7777/mcp --transport http\s+--method tools/call --tool-name hire --tool-arg service=anon-<…>/hello --tool-arg message=hi$`).MatchString(c)
		}},
		{"the public URL without a credential", func(c string) bool {
			return c == "curl -s https://gateway.kontra.run/agents/anon-<…>/hello/.well-known/agent-card.json"
		}},
	} {
		if !has(want.ok) {
			t.Errorf("the demo block has no command for %s:\n%s", want.what, strings.Join(cmds, "\n"))
		}
	}
	if n := slices.IndexFunc(cmds, func(c string) bool { return c == installLine }); n != 0 {
		t.Errorf("the kontra install is command %d, want the first", n+1)
	}
	if c := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "kontra connect ") }); c < 0 ||
		slices.ContainsFunc(cmds[:c], func(s string) bool { return strings.Contains(s, placeholder) }) {
		t.Error("a command names the slug before kontra connect prints it")
	}
	// The inspector is pinned to one version everywhere.
	pins := regexp.MustCompile(`@modelcontextprotocol/inspector@(\S+)`).FindAllStringSubmatch(b, -1)
	for _, p := range pins {
		if p[1] != pins[0][1] || p[1] == "latest" {
			t.Errorf("inspector pins %v: want one version, not latest", pins)
		}
	}

	// What the block must not hold, and what the whole README must not.
	for _, bad := range []string{":7778", "brew ", "console", "gateway.kontra.sh", "kontra dev"} {
		if strings.Contains(b, bad) {
			t.Errorf("the demo block holds %q", bad)
		}
	}
	for _, bad := range []*regexp.Regexp{regexp.MustCompile(`:7778\b`), regexp.MustCompile(`brew install kontra`),
		regexp.MustCompile(`\ba2a serve(\s|$)`), regexp.MustCompile(`a2a send \S*http`)} {
		if loc := bad.FindStringIndex(text); loc != nil {
			t.Errorf("README.md holds %q", text[loc[0]:loc[1]])
		}
	}

	// Slugs: in the block's commands only the placeholder; anywhere in the README, the placeholder or anon- and 16
	// lowercase hex digits; and the README says what the placeholder stands for.
	if !strings.Contains(b, placeholder) {
		t.Errorf("the demo block never names the slug as %s", placeholder)
	}
	for _, c := range cmds {
		rest := strings.ReplaceAll(c, placeholder, "")
		if strings.Contains(rest, "anon-") {
			t.Errorf("command %q names a slug other than the placeholder", c)
		}
	}
	for _, m := range regexp.MustCompile(`anon-[^\s/)`+"`"+`"',;.]*`).FindAllString(text, -1) {
		if m != placeholder && m != "anon-" && !slugRE.MatchString(m) {
			t.Errorf("README.md names the slug %q: want %s or anon- and 16 lowercase hex digits", m, placeholder)
		}
	}
	if !strings.Contains(text, "`anon-<…>` stands for your session's slug") || !strings.Contains(text, "16 lowercase hex digits") {
		t.Error("README.md does not say what anon-<…> stands for")
	}
}

// docker stubs docker for the script: info answers unless STUB_DOCKER=down; anything else is unexpected.
var dockerStub = `[ "$1" = info ] && [ "${STUB_DOCKER:-up}" = up ] && exit 0
[ "$1" = info ] && { echo "Cannot connect to the Docker daemon" >&2; exit 1; }
echo "docker stub: not expected: $*" >&2; exit 9`

func headSHA(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", kit.Root(t), "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		t.Skipf("git: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestScriptsLint(t *testing.T) { kit.Lint(t, script, "test/demo/laptop.sh") }

// The script refuses to start without docker (or its daemon), git or curl, naming what is missing, and builds nothing.
func TestDemoRefusesWithoutPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave string
		env   []string
		want  string
	}{
		{"no docker", "docker", nil, "docker"},
		{"no curl", "curl", nil, "curl"},
		{"no git", "git", nil, "git"},
		{"no docker daemon", "", []string{"STUB_DOCKER=down"}, "docker cannot reach its daemon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := slices.DeleteFunc(append(append([]string(nil), kit.BaseTools...), "curl"), func(s string) bool { return s == tc.leave })
			stubs := map[string]string{"docker": dockerStub}
			if tc.leave == "docker" {
				stubs = nil
			}
			bin, log := kit.Sandbox(t, tools, stubs)
			_, stderr, code := kit.Script(t, script, bin, log, tc.env, "--dry-run", "--readme", filepath.Join(kit.Root(t), "README.md"))
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q: want 2 naming %q", code, stderr, tc.want)
			}
			for _, c := range kit.StubCalls(t, log) {
				if c != "docker info" {
					t.Errorf("a refused start called %q", c)
				}
			}
		})
	}
	bin, log := kit.Sandbox(t, append(append([]string(nil), kit.BaseTools...), "curl"), map[string]string{"docker": dockerStub})
	for _, args := range [][]string{{"--published"}, {"--published", "--expect", "nothex"}, {"--frobnicate"}} {
		if _, stderr, code := kit.Script(t, script, bin, log, nil, args...); code != 2 {
			t.Errorf("%q: exit %d (%s), want 2", args, code, stderr)
		}
	}
}

// --dry-run prints the README's commands as the laptop runs them, exactly the two substitutions, the host's checks and
// the teardown, and calls nothing but docker info.
func TestDemoDryRun(t *testing.T) {
	sha := headSHA(t)
	bin, log := kit.Sandbox(t, append(append([]string(nil), kit.BaseTools...), "curl"), map[string]string{"docker": dockerStub})
	rd := filepath.Join(kit.Root(t), "README.md")
	stdout, stderr, code := kit.Script(t, script, bin, log, nil, "--dry-run", "--readme", rd)
	if code != 0 {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	cmds := commands(block(t, readme(t)))
	var listed []string
	for _, l := range strings.Split(stdout, "\n") {
		if m := regexp.MustCompile(`^\s+(\d+)  (.*)$`).FindStringSubmatch(l); m != nil {
			listed = append(listed, m[2])
		}
	}
	if !slices.Equal(listed, cmds) {
		t.Errorf("the script lists the commands\n  %s\nwant README's\n  %s", strings.Join(listed, "\n  "), strings.Join(cmds, "\n  "))
	}
	for _, want := range []string{
		"teardown, in a trap on EXIT/INT/TERM: docker rm -f kontra-demo-" + sha + "; docker image rm kontra-demo:" + sha + ";",
		"  (a) " + installLine + "  ->  (cd /src && go install ./cmd/kontra)",
		"  (b) anon-<…>  ->  the slug kontra connect prints",
		"GET https://gateway.kontra.run/agents/<slug>/hello/.well-known/agent-card.json",
		`JSON-RPC SendMessage "hi" to https://gateway.kontra.run/agents/<slug>/hello (Hello World)`,
		"mode source (target laptop-src), gate https://gateway.kontra.run",
		"dry run: nothing built, run or removed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if subs := regexp.MustCompile(`(?m)^  \([a-z]\) `).FindAllString(stdout, -1); len(subs) != 2 {
		t.Errorf("%d substitutions listed, want exactly (a) and (b):\n%s", len(subs), stdout)
	}
	stdout, _, code = kit.Script(t, script, bin, log, nil, "--dry-run", "--published", "--readme", rd)
	if code != 0 || !strings.Contains(stdout, "(a) none: "+installLine+" runs as written") || !strings.Contains(stdout, "(target laptop)") {
		t.Errorf("--published --dry-run: exit %d\n%s", code, stdout)
	}
	if calls := kit.StubCalls(t, log); slices.ContainsFunc(calls, func(c string) bool { return c != "docker info" }) {
		t.Errorf("--dry-run called %q", calls)
	}

	// A README without the markers, or with the install line twice, is refused.
	for name, mutate := range map[string]func(string) string{
		"no markers": func(s string) string { return strings.ReplaceAll(s, "<!-- demo:begin -->", "") },
		"two installs": func(s string) string {
			return strings.Replace(s, installLine+"\n", installLine+"\n"+installLine+"\n", 1)
		},
	} {
		p := filepath.Join(t.TempDir(), "README.md")
		if err := os.WriteFile(p, []byte(mutate(readme(t))), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, stderr, code := kit.Script(t, script, bin, log, nil, "--dry-run", "--readme", p); code != 2 {
			t.Errorf("%s: exit %d (%s), want 2", name, code, stderr)
		}
	}
}

// The laptop image (the M4 plan's WP-08 acceptance): it builds from HEAD, and before the README's install step it
// holds no kontra and no a2a binary, an empty home, Go 1.26.10 with the default GOPATH off PATH, Node 22.23.4; the
// source target has the archive at /src (read-only for the laptop), the published one none.
func TestDemoImage(t *testing.T) {
	kit.Need(t)
	sha := headSHA(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	root := kit.Root(t)
	img := "kontra-demo:" + sha
	t.Cleanup(func() { _, _ = kit.Run(context.Background(), "", nil, "docker", "image", "rm", img) })
	probe := `set -u
for b in kontra a2a a2a-cli; do command -v "$b" && echo "FOUND $b"; done
ls "$(go env GOPATH)/bin" 2>/dev/null | sed 's/^/GOPATHBIN /'
echo "HOME $(ls -A "$HOME" | tr '\n' ' ')"
echo "USER $(id -un) $(id -u)"
echo "GOPATH $(go env GOPATH)"
echo "PATH $PATH"
echo "GO $(go version)"
echo "NODE $(node --version) NPX $(npx --version)"
if [ -e /src ]; then echo "SRC $(ls /src | tr '\n' ' ')"; touch /src/x 2>/dev/null && echo "SRC WRITABLE"; else echo "NOSRC"; fi`
	for _, tc := range []struct {
		args []string
		src  bool
	}{{nil, true}, {[]string{"--published"}, false}} {
		if out, err := kit.Run(ctx, root, nil, filepath.Join(root, script), append([]string{"--build-only"}, tc.args...)...); err != nil {
			t.Fatalf("--build-only %v: %v\n%s", tc.args, err, out)
		}
		out, err := kit.Run(ctx, "", nil, "docker", "run", "--rm", "--network", "none", img, "bash", "-c", probe)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		t.Logf("%v:\n%s", tc.args, out)
		for _, bad := range []string{"FOUND", "GOPATHBIN", "SRC WRITABLE", "/go/bin"} {
			if strings.Contains(out, bad) {
				t.Errorf("%v: the laptop holds %q before the install step:\n%s", tc.args, bad, out)
			}
		}
		for _, want := range []string{"HOME .bash_logout .bashrc .profile \n", "USER laptop 1000", "GOPATH /home/laptop/go",
			"GO go version go1.26.10 linux/", "NODE v22.23.4 NPX "} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: no %q in:\n%s", tc.args, want, out)
			}
		}
		if tc.src != strings.Contains(out, "SRC README.md ") || tc.src == strings.Contains(out, "NOSRC") {
			t.Errorf("%v: /src %v:\n%s", tc.args, tc.src, out)
		}
		if tc.src && (!strings.Contains(out, " go.mod ") || !strings.Contains(out, " test ") || !strings.Contains(out, " cmd ")) {
			t.Errorf("/src is not the whole archive:\n%s", out)
		}
	}
}

// The whole kit against a local gate (O2: the kits are tested locally; nothing touches staging): compose.yaml with a
// multi-tenant configuration with tunnel.anonymous, TLS from a run CA for gateway.kontra.run, listening on 443 under that
// name (a network alias) on the project's network. scripts/demo-clean-laptop.sh runs README's block verbatim in the
// laptop on that network, trusting the run CA, and checks the public URL from this host at the gate's address. The
// gate then holds one anonymous tenant, the slug the laptop printed, created from a /24.
func TestDemoRehearsal(t *testing.T) {
	kit.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	const name = "gateway.kontra.run"
	img := kit.GateImage(t)
	override := `services:
  gate-migrate: { image: ` + img + ` }
  gate:
    image: ` + img + `
    sysctls: { net.ipv4.ip_unprivileged_port_start: "0" }
    networks: { default: { aliases: [` + name + `] } }
    healthcheck:
      test: ["CMD", "/usr/local/bin/kontra-gate", "health", "--url", "https://127.0.0.1:443/healthz", "-f", "/etc/kontra/kontra.yaml"]
`
	s := kit.NewStack(t, "kitdemo", override)
	ca := kit.NewCA(t)
	cert, key := ca.Leaf(t, name)
	kit.WriteOwned(t, filepath.Join(s.Dir, "tls", "tls.crt"), cert, 0o444)
	kit.WriteOwned(t, filepath.Join(s.Dir, "tls", "tls.key"), key, 0o400)
	kit.WriteOwned(t, filepath.Join(s.Dir, "signing", "kontra.key"), kit.SigningKey(t), 0o400)
	cfg := fmt.Sprintf(`# The demo's rehearsal gate (test/demo): spec/schema/examples/config.staging.json's shape, TLS from the run's CA.
listen: ":443"
public_url: https://%s
tenancy: multi
signing_key: file:/etc/kontra/signing/kontra.key
tls: { cert: /etc/kontra/tls/tls.crt, key: /etc/kontra/tls/tls.key }
postgres: postgres://kontra:kontra@postgres:5432/kontra
temporal: { address: "temporal:7233", namespace: kontra }
metrics: { listen: ":9090" }
auth: { api_keys: true }
tunnel: { anonymous: true }
`, name)
	kit.WriteOwned(t, filepath.Join(s.Dir, "kontra.yaml"), []byte(cfg), 0o644)
	caFile := filepath.Join(s.Dir, "ca.pem")
	if err := os.WriteFile(caFile, ca.PEM, 0o644); err != nil {
		t.Fatal(err)
	}
	s.SetEnv(kit.Env("KONTRA_CONFIG", filepath.Join(s.Dir, "kontra.yaml"), "KONTRA_SIGNING_DIR", filepath.Join(s.Dir, "signing"),
		"KONTRA_TLS_DIR", filepath.Join(s.Dir, "tls"), "KONTRA_LISTEN_ADDR", "127.0.0.1", "KONTRA_PORT", kit.FreePort(t),
		"KONTRA_METRICS_PORT", kit.FreePort(t))...)
	if out, err := s.Compose(ctx, "up", "-d", "--wait", "--no-build"); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	ip, err := s.IP(ctx, "gate")
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "transcript.log")
	root := kit.Root(t)
	out, err := kit.Run(ctx, root, nil, filepath.Join(root, script), "--network", s.Network(), "--ca-file", caFile,
		"--resolve", ip, "--transcript", transcript)
	t.Logf("the run:\n%s", kit.Tail(out, 80))
	if err != nil {
		t.Fatalf("scripts/demo-clean-laptop.sh: %v", err)
	}
	b, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	tr := string(b)
	for _, want := range []string{
		"kontra-demo: substitution (a): " + installLine + "  ->  (cd /src && go install ./cmd/kontra)",
		"kontra-demo: substitution (b): anon-<…>  ->  anon-",
		`kontra-demo: ok 7: "Hello World"`, `kontra-demo: ok 8: "read_file"`, `kontra-demo: ok 9: "Hello World"`,
		"kontra-demo: ready", "demo-clean-laptop: host checks passed", "demo-clean-laptop: PASS (slug anon-",
	} {
		if !strings.Contains(tr, want) {
			t.Errorf("the transcript lacks %q", want)
		}
	}
	m := regexp.MustCompile(`PASS \(slug (anon-[0-9a-f]{16}),`).FindStringSubmatch(tr)
	if m == nil {
		t.Fatal("no slug in the transcript")
	}
	rows, err := s.Compose(ctx, "exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "kontra", "-At", "-c",
		"select slug, kind, masklen(created_from), expires_at > now() from kontra.tenants where kind = 'anonymous'")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(rows); got != m[1]+"|anonymous|24|t" {
		t.Errorf("the gate's anonymous tenants: %q, want the run's slug %s created from a /24 and live", got, m[1])
	}
	if out, err := kit.Run(ctx, "", nil, "docker", "ps", "-a", "-q", "--filter", "name=kontra-demo-"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("a kontra-demo container is left: %q %v", out, err)
	}
}
