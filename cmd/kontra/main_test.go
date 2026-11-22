package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/node"
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
		{[]string{"console"}, 2, "", "usage:"},                                             // no local console (D164)
		{[]string{"login", "-h"}, 0, "--no-browser", ""},                                   // spec/tunnel.md §6.3 (M4 WP-06)
		{[]string{"login", "--gate", "http://gate.example"}, 2, "", "kontra login: usage"}, // https:// except on localhost
		{[]string{"login", "--bogus"}, 2, "", "usage: kontra login"},
		{[]string{"login", "extra"}, 2, "", "unexpected argument"},
		{[]string{"publish"}, 2, "", "kontra publish: usage: name the listing file"}, // spec/tunnel.md §6.4 (D323)
		{[]string{"publish", "-h"}, 0, "--control-plane URL", ""},
		{[]string{"publish", "--version", "x", "l.yaml"}, 2, "", "usage: kontra publish"},
		{[]string{"publish", "--replace", "--version", "2", "l.yaml"}, 2, "", "--version and --replace"},
		{[]string{"publish", "-h"}, 0, "--replace", ""},
		{[]string{"publish", "--dry-run", "/nonexistent/listing.yaml"}, 1, "", "kontra publish: the listing file: open"},
		{[]string{"--help"}, 0, "kontra publish sends a listing file", ""},
		{[]string{"seal", "--help"}, 0, "usage:\n  kontra seal --service <service URL>", ""},
		{[]string{"seal", "-h"}, 0, "\n  kontra seal --open <part.json>\n", ""},
		{[]string{"seal", "--bogus"}, 2, "", "usage:\n  kontra seal --service"},
		{[]string{"--help"}, 0, "its own binary: kontrad", ""},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(ctx, c.args, &stdout, &stderr); code != c.code ||
			!strings.Contains(stdout.String(), c.out) || !strings.Contains(stderr.String(), c.errOut) {
			t.Errorf("kontra %q = %d, stdout %q, stderr %q; want %d, %q, %q", c.args, code, stdout.String(), stderr.String(),
				c.code, c.out, c.errOut)
		}
	}
}

// spec/tunnel.md §6.3 step 1: a gate without the RFC 9728 document for /mcp has no sign-in: kontra login exits 1 naming
// --api-key, and writes nothing.
func TestLoginWithoutSignIn(t *testing.T) {
	home := isolate(t)
	gate := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gate.Close)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"login", "--gate", gate.URL, "--no-browser"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "kontra login: this gate has no sign-in") || !strings.Contains(stderr.String(), "--api-key") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".kontra", "credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a login without sign-in wrote the credentials file: %v", err)
	}
}

// Round 2 records-4: kontra seal's usage error escapes its reason for the terminal and prints the synopsis as it is
// (no literal \n); --help is no usage error; the base-path refusal cites its decisions, not "M2" or "the owner".
func TestCLITexts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"seal", "--bogus\x1b[31m"}, &stdout, &stderr); code != 2 ||
		strings.Contains(stderr.String(), `\n`) || strings.Contains(stderr.String(), "\x1b") || !strings.Contains(stderr.String(), "\n  kontra seal --open") {
		t.Errorf("a seal usage error = %d, %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"seal", "--help"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 ||
		strings.Contains(stdout.String(), "help requested") {
		t.Errorf("kontra seal --help = %d, %q, %q", code, stdout.String(), stderr.String())
	}
	_, err := node.ParseServe("a2a:x=http://127.0.0.1:9/path")
	if err == nil || strings.Contains(err.Error(), "M2") || strings.Contains(err.Error(), "owner") || !strings.Contains(err.Error(), "D210") {
		t.Errorf("the base-path refusal: %v", err)
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

// kontra publish --dry-run (spec/tunnel.md §6.4, D323): a listing file checked and its request printed, nothing sent;
// an invalid one exits 1 naming each problem.
func TestPublishDryRun(t *testing.T) {
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "listing.yaml"), filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte("service: hello\ntaskTimeoutSeconds: 900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("service: Hello\nprice: { model: per_task, amount: \"1.00\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"publish", good, "--dry-run"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "PUT /v1/services/hello") || !strings.Contains(stdout.String(), "nothing was sent") {
		t.Fatalf("a dry run: %d %s %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"publish", "--dry-run", bad}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "service: \"Hello\"") || !strings.Contains(stderr.String(), `"price" is not a key`) {
		t.Fatalf("an invalid listing: %d %s", code, stderr.String())
	}
}

// D364: kontra connect starts nothing and names kontrad, whatever its arguments; it is no alias, -h included, and it
// touches nothing under ~/.kontra. kontra's usage lists no connect.
func TestRetiredConnect(t *testing.T) {
	home := isolate(t)
	for _, args := range [][]string{
		{"connect"},
		{"connect", "-h"},
		{"connect", "--gate", "https://gate.example", "--serve", "a2a:hello=http://127.0.0.1:9999"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 || stdout.Len() != 0 ||
			stderr.String() != "kontra connect was renamed: run kontrad with the same flags (D364)\n" {
			t.Errorf("kontra %q = %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".kontra")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("kontra connect created ~/.kontra: %v", err)
	}
	var stdout, stderr bytes.Buffer
	run(context.Background(), []string{"--help"}, &stdout, &stderr)
	if strings.Contains(stdout.String(), "kontra connect [") || strings.Contains(stdout.String(), "--serve") {
		t.Errorf("kontra's usage still lists connect:\n%s", stdout.String())
	}
}
