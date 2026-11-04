package node

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// Fixtures shaped as the credentials an operator is handed: a gate API key (relay.md §2.3: kk_<operator prefix>_
// and 32 base62) and a Kontra Cloud account key (kontra_sk_ and 43 base64url). Neither is a real key.
const (
	gateAPIKey     = "kk_team_Q7mZ2rT9vX4nB8kL1pW6sY3dF5hJ0cG2"
	cloudAccessKey = "kontra_sk_Zm9vYmFyYmF6cXV4cXV1eGNvcmdlZ3JhdWx0Z2FycGx5"
)

// --enroll names what it was given when that is another credential, says where enrollment tokens come from on
// either kind of gate, and never prints the value (a usage error, before anything starts; New refuses the same).
func TestEnrollNamesTheCredential(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string { return map[string]string{"HOME": home}[k] }
	file := filepath.Join(home, "token")
	if err := os.WriteFile(file, []byte(gateAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := "from the gate's operator: Kontra Cloud's console (Network, Add a node / Add a service) or " +
		"`kontra-gate enroll <service>` on a self-hosted gate"
	for _, c := range []struct {
		name, value, stdin string
		want               []string
	}{
		{"gate API key", gateAPIKey, "", []string{
			"--enroll: that is a gate API key (kk_…): pass it with --api-key; it enrolls a node only with an enroll: " +
				"scope, and only for services that already exist", "(an enrollment token is ket_ and 32 letters or digits, " + source + ")"}},
		{"gate API key from a file", "@" + file, "", []string{"that is a gate API key (kk_…): pass it with --api-key"}},
		{"gate API key on stdin", "-", gateAPIKey, []string{"that is a gate API key (kk_…)"}},
		{"Cloud account key", cloudAccessKey, "", []string{
			"--enroll: that is a Kontra Cloud account key for its control plane API; the gate never takes it",
			"(an enrollment token is ket_ and 32 letters or digits, " + source + ")"}},
		{"something else", "not-a-token", "", []string{"--enroll: not an enrollment token (ket_ and 32 letters or digits, " + source + ")"}},
		{"a cut token", "ket_AAAAAAAAAAAAAAAA", "", []string{"--enroll: not an enrollment token (ket_ and 32"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseArgs([]string{"--name", "build-box", "--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", c.value},
				getenv, strings.NewReader(c.stdin))
			var uerr *UsageError
			if !errors.As(err, &uerr) {
				t.Fatalf("parseArgs = %v, want a usage error", err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%v\nwant it to say %q", err, w)
				}
			}
			for _, secret := range []string{gateAPIKey, cloudAccessKey, "Q7mZ2rT9", "Zm9vYmFy", "AAAAAAAAAAAAAAAA"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error prints the value: %v", err)
				}
			}
		})
	}

	// A token of the right shape passes; New refuses the others with the same words.
	if _, err := parseArgs([]string{"--name", "build-box", "--serve", "a2a:hello=http://127.0.0.1:9999", "--enroll", tokA},
		getenv, nil); err != nil {
		t.Fatalf("a token: %v", err)
	}
	for value, want := range map[string]string{
		gateAPIKey:     "that is a gate API key (kk_…): pass it with --api-key",
		cloudAccessKey: "that is a Kontra Cloud account key for its control plane API",
		"ket_short":    "not an enrollment token (ket_ and 32 letters or digits, " + source + ")",
	} {
		gate, _ := url.Parse("https://gate.example")
		target, _ := url.Parse("http://127.0.0.1:9999")
		cfg := testConfig(t, gate, "", a2aServe("hello", target))
		cfg.Enroll = []string{value}
		_, err := New(cfg, nil)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), value) {
			t.Errorf("New with --enroll %.12s…: %v, want %q and not the value", value, err, want)
		}
	}
}

// The service_denied of a known key that declares a service it is not enrolled for (the admission's step 8) names
// the services; notEnrolled reads them back, only for services the hello declared.
func TestNotEnrolled(t *testing.T) {
	declared := []tunnel.Service{{Kind: tunnel.KindA2A, Name: "crawler"}, {Kind: tunnel.KindA2A, Name: "bypass"},
		{Kind: tunnel.KindA2A, Name: "scanner"}, {Kind: tunnel.KindMCP, Name: "fs"}}
	denied := func(msg string) *tunnel.HandshakeError { return tunnel.Refuse(tunnel.CodeServiceDenied, "%s", msg) }
	for _, c := range []struct {
		name  string
		herr  *tunnel.HandshakeError
		want  []string
		wantO bool
	}{
		{"one", denied("scanner: the key is not enrolled for it; nothing was enrolled"), []string{"scanner"}, true},
		{"two", denied("scanner: the key is not enrolled for it; fs: the key is not enrolled for it; nothing was enrolled"),
			[]string{"scanner", "fs"}, true},
		{"with another denial", denied("scanner: the key is not enrolled for it; bypass: not an enabled tunnel service; nothing was enrolled"),
			[]string{"scanner"}, false},
		{"a name the hello did not declare", denied("other: the key is not enrolled for it; nothing was enrolled"), nil, false},
		{"an escape in the name", denied("scan\x1bner: the key is not enrolled for it; nothing was enrolled"), nil, false},
		{"another reason", denied("scanner: no such service; nothing was enrolled"), nil, false},
		{"a tenant token on a served service", denied("crawler: it already has an enrolled key, which a tenant token never adds to; nothing was enrolled"), nil, false},
		{"another code", tunnel.Refuse(tunnel.CodeEnrollInvalid, "scanner: the key is not enrolled for it"), nil, false},
	} {
		names, only := notEnrolled(c.herr, declared)
		if !slices.Equal(names, c.want) || (len(names) > 0 && only != c.wantO) {
			t.Errorf("%s: notEnrolled = %q, %v; want %q, %v", c.name, names, only, c.want, c.wantO)
		}
	}
}

func TestAddServiceHint(t *testing.T) {
	for _, c := range []struct {
		names  []string
		others bool
		want   string
	}{
		{[]string{"scanner"}, true, "to add scanner to this node, get an enrollment token that names it (Kontra Cloud: " +
			"the console's Network page, the node's Add a service, or Add a node naming only scanner; a self-hosted gate: " +
			"kontra-gate enroll scanner, once its services: has it), then run kontra connect again with --enroll <token> " +
			"and every --serve flag: the node's key keeps the services it is already enrolled for; until then, the same " +
			"command without --serve scanner serves the others"},
		{[]string{"scanner", "fs"}, true, "to add scanner and fs to this node, get an enrollment token that names them " +
			"(Kontra Cloud: the console's Network page, the node's Add a service, or Add a node naming only scanner and fs; " +
			"a self-hosted gate: kontra-gate enroll <service> for each, once its services: has them), then run kontra " +
			"connect again with --enroll <token> and every --serve flag: the node's key keeps the services it is already " +
			"enrolled for; until then, the same command without their --serve flags serves the others"},
		{[]string{"scanner"}, false, "to add scanner to this node, get an enrollment token that names it (Kontra Cloud: " +
			"the console's Network page, the node's Add a service, or Add a node naming only scanner; a self-hosted gate: " +
			"kontra-gate enroll scanner, once its services: has it), then run kontra connect again with --enroll <token> " +
			"and every --serve flag: the node's key keeps the services it is already enrolled for"},
	} {
		if got := addServiceHint(c.names, c.others); got != c.want {
			t.Errorf("addServiceHint(%q, %v) =\n%s\nwant\n%s", c.names, c.others, got, c.want)
		}
	}
}

// A node enrolled for crawler and bypass, restarted with --serve scanner as well and no token naming it: the gate
// refuses the hello service_denied, Run ends with it, and the log says once how to add scanner. The gate's words
// are unchanged.
func TestServiceDeniedHint(t *testing.T) {
	g := newTestGate(t, nil)
	rec := newRecorder(t)
	const msg = "scanner: the key is not enrolled for it; nothing was enrolled"
	g.adm.set(func(int, enroll.Request) (*enroll.Decision, error) {
		return nil, tunnel.Refuse(tunnel.CodeServiceDenied, msg)
	})
	cfg := testConfig(t, g.url, "", a2aServe("crawler", rec.url()), a2aServe("bypass", rec.url()), a2aServe("scanner", rec.url()))
	tn := mustNode(t, g, cfg).run(t)
	err := tn.result(t, 10*time.Second)
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) || herr.Code != tunnel.CodeServiceDenied || herr.Message != msg {
		t.Fatalf("Run = %v, want the gate's service_denied as it is", err)
	}
	logs := tn.logs.String()
	if n := strings.Count(logs, "to add scanner to this node"); n != 1 {
		t.Fatalf("the hint is logged %d times:\n%s", n, logs)
	}
	for _, w := range []string{"the console's Network page, the node's Add a service", "kontra-gate enroll scanner",
		"--enroll <token> and every --serve flag", "without --serve scanner serves the others", "services=[scanner]"} {
		if !strings.Contains(logs, w) {
			t.Errorf("the hint does not say %q:\n%s", w, logs)
		}
	}

	// Any other service_denied gets no hint.
	g2 := newTestGate(t, nil)
	g2.adm.set(func(int, enroll.Request) (*enroll.Decision, error) {
		return nil, tunnel.Refuse(tunnel.CodeServiceDenied, "scanner: no such service; nothing was enrolled")
	})
	cfg2 := testConfig(t, g2.url, "", a2aServe("scanner", rec.url()))
	tn2 := mustNode(t, g2, cfg2).run(t)
	if err := tn2.result(t, 10*time.Second); handshakeCode(err) != tunnel.CodeServiceDenied {
		t.Fatalf("Run = %v", err)
	}
	if logs := tn2.logs.String(); strings.Contains(logs, "to add") {
		t.Errorf("a hint for another refusal:\n%s", logs)
	}
}
