package publish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/login"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/oidc/oidctest"
)

// The schema's fixtures hold the CLI to the schema (D323): every spec/schema/examples/listing.*.json is a listing
// Parse accepts, every spec/schema/examples/invalid/listing.*.json one it refuses with Problems, as
// scripts/check_specs.py holds the schema to the same files.
func TestSchemaFixtures(t *testing.T) {
	valid, _ := filepath.Glob("../../spec/schema/examples/listing.*.json")
	invalid, _ := filepath.Glob("../../spec/schema/examples/invalid/listing.*.json")
	if len(valid) < 5 || len(invalid) < 20 {
		t.Fatalf("%d valid and %d refused fixtures found", len(valid), len(invalid))
	}
	for _, f := range valid {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
	for _, f := range invalid {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var p Problems
		if _, err := Parse(b); !errors.As(err, &p) || len(p) == 0 {
			t.Errorf("%s: accepted (%v)", filepath.Base(f), err)
		}
	}
}

// YAML as a publisher writes it: off and on stay strings, an unquoted amount is refused with the hint to quote it, a
// key twice, two documents, a scalar, an unknown key and a file over MaxFile are refused, and the globs of tools
// count together (at most 64, which the schema cannot say).
func TestParseYAML(t *testing.T) {
	l, err := Parse([]byte(`# hello, sold per task
service: hello
kind: a2a
visibility: unlisted
sealed: off
price:
  model: per_task
  amount: "1.50"
  reviewWindowSeconds: 86400
  taskTimeoutSeconds: 600
`))
	if err != nil {
		t.Fatal(err)
	}
	if l.Service != "hello" || l.Kind != "a2a" || l.Visibility != "unlisted" || l.Sealed != "off" || !l.Published ||
		l.Price.Model != ModelPerTask || *l.Price.Amount != "1.50" || *l.Price.ReviewWindowSeconds != 86400 || *l.Price.TaskTimeoutSeconds != 600 {
		t.Fatalf("parsed %+v %+v", l, l.Price)
	}
	globs := make([]string, 40)
	for i := range globs {
		globs[i] = fmt.Sprintf("t%d", i)
	}
	list, _ := json.Marshal(globs)
	for name, c := range map[string]struct{ file, want string }{
		"an unquoted amount":  {"service: hello\nprice: { model: per_task, amount: 1.50 }\n", `price.amount: want a string (quote it`},
		"a key twice":         {"service: hello\nservice: other\nprice: { model: free }\n", "not YAML"},
		"two documents":       {"service: hello\nprice: { model: free }\n---\nservice: other\n", "more than one YAML document"},
		"a scalar":            {"hello\n", "the listing: want a mapping"},
		"empty":               {"", "empty"},
		"an unknown key":      {"service: hello\nprice: { model: free }\nquote: 1\n", `"quote" is not a key`},
		"a version":           {"service: hello\nprice: { model: free }\nversion: 2\n", `"version" is not a key`},
		"a non-string key":    {"service: hello\nprice: { model: free }\n1: x\n", "the listing: want a mapping"},
		"globs over 64":       {"service: t\nkind: mcp\nprice: { model: free }\ntools: { allow: " + string(list) + ", deny: " + string(list) + " }\n", "at most 64 globs"},
		"an integer as float": {"service: hello\nprice: { model: per_task, amount: \"1.00\", taskTimeoutSeconds: 60.5 }\n", "taskTimeoutSeconds: want an integer"},
		"currency":            {"service: hello\nprice: { model: per_task, amount: \"1.00\", currency: EUR }\n", `"EUR": listings are priced in USD`},
		"per_task on mcp":     {"service: t\nkind: mcp\nprice: { model: per_task, amount: \"1.00\" }\ntasks: durable\n", "tasks: required"},
	} {
		_, err := Parse([]byte(c.file))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := Parse(bytes.Repeat([]byte("#"), MaxFile+1)); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("a file over MaxFile: %v", err)
	}
	// An integral number written as a float is an integer, as JSON Schema reads one.
	if l, err := Parse([]byte("service: hello\nprice: { model: per_task, amount: \"1.00\", taskTimeoutSeconds: 6e2 }\n")); err != nil || *l.Price.TaskTimeoutSeconds != 600 {
		t.Errorf("6e2: %v", err)
	}
}

// The request is Cloud's ListingPut (D-M5-95): kind, visibility, sealed, published and currency always written, the
// price's other keys as the file wrote them, tools and tasks only when written (an empty allow sent as []), and the
// version given.
func TestBody(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"service: echo\nprice: { model: free }\n",
			`{"kind":"a2a","visibility":"public","price":{"model":"free","currency":"USD"},"sealed":"off","published":true,"version":0}`},
		{"service: search\nkind: mcp\nvisibility: unlisted\nprice: { model: per_call, amount: \"0.002\", tools: { deep: \"0.01\" }, freePerMonth: 100, capPerMonth: \"10.00\" }\ntools: { allow: [], deny: [admin-*] }\ntasks: durable\npublished: false\n",
			`{"kind":"mcp","visibility":"unlisted","price":{"model":"per_call","amount":"0.002","currency":"USD","tools":{"deep":"0.01"},"freePerMonth":100,"capPerMonth":"10.00"},"tools":{"allow":[],"deny":["admin-*"]},"tasks":"durable","sealed":"off","published":false,"version":7}`},
		{"service: research\nkind: mcp\nprice: { model: per_task, amount: \"5.00\", reviewWindowSeconds: 3600, taskTimeoutSeconds: 1800 }\ntasks: required\nsealed: optional\ntools: { deny: [] }\n",
			`{"kind":"mcp","visibility":"public","price":{"model":"per_task","amount":"5.00","currency":"USD","reviewWindowSeconds":3600,"taskTimeoutSeconds":1800},"tools":{"deny":[]},"tasks":"required","sealed":"optional","published":true,"version":7}`},
	} {
		l, err := Parse([]byte(c.file))
		if err != nil {
			t.Fatal(err)
		}
		v := int64(7)
		if l.Service == "echo" {
			v = 0
		}
		b, err := l.Body(v)
		if err != nil || string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", l.Service, b, c.want)
		}
	}
}

// Flags before or after the file; one file; --version a count; --control-plane an https:// URL (http:// on a loopback
// host); -h is the help.
func TestParseArgs(t *testing.T) {
	env := func(string) string { return "/home/p" }
	o, err := ParseArgs([]string{"listing.yaml", "--dry-run", "--version", "3", "--gate", "http://localhost:8080"}, env)
	if err != nil || o.File != "listing.yaml" || !o.DryRun || o.Version == nil || *o.Version != 3 || o.Gate.String() != "http://localhost:8080" ||
		o.CredentialsPath != "/home/p/.kontra/credentials" {
		t.Fatalf("%+v %v", o, err)
	}
	if o, err := ParseArgs([]string{"--control-plane", "https://api.example.com/", "l.yaml"}, env); err != nil || o.ControlPlane != "https://api.example.com/" ||
		o.Gate.String() != "https://gateway.kontra.sh" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{{}, {"a.yaml", "b.yaml"}, {"--version", "-1", "a.yaml"}, {"--version", "x", "a.yaml"},
		{"--control-plane", "http://api.example.com", "a.yaml"}, {"--control-plane", "https://api.example.com/?x=1", "a.yaml"},
		{"--gate", "http://gate.example.com", "a.yaml"}, {"--nope", "a.yaml"}} {
		if _, err := ParseArgs(bad, env); !errors.Is(err, ErrUsage) {
			t.Errorf("%q: %v, want a usage error", bad, err)
		}
	}
	if _, err := ParseArgs([]string{"-h"}, env); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}

// --dry-run checks the file and prints the request, signing in nowhere and sending nothing (no gate, no client).
func TestDryRun(t *testing.T) {
	var out bytes.Buffer
	o := Options{File: "l.yaml", DryRun: true, Out: &out,
		ReadFile: func(string) ([]byte, error) {
			return []byte("service: hello\nprice: { model: per_task, amount: \"1.00\" }\n"), nil
		}}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "PUT /v1/services/hello") || !strings.Contains(out.String(), `"amount": "1.00"`) ||
		!strings.Contains(out.String(), "nothing was sent") {
		t.Fatalf("dry run: %s", out.String())
	}
	o.ReadFile = func(string) ([]byte, error) {
		return []byte("service: hello\nprice: { model: per_task, amount: \"1\" , currency: EUR}\n"), nil
	}
	var p Problems
	if err := Run(context.Background(), o); !errors.As(err, &p) {
		t.Fatalf("an invalid file in a dry run: %v", err)
	}
}

// rig is a gate's RFC 9728 document in front of an oidctest authorization server (issuing JWTs for the resource
// asked), a fake control plane serving GET and PUT /v1/services/{name} as Cloud's does, and a HOME.
type rig struct {
	t     *testing.T
	as    *oidctest.Server
	gate  *httptest.Server
	cp    *httptest.Server
	creds string

	mu       sync.Mutex
	listings map[string]int64 // service -> current version
	puts     []put
	gets     int
	answer   func(w http.ResponseWriter, r *http.Request) bool // overrides the control plane when it returns true
}

type put struct {
	auth string
	body map[string]any
	raw  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	as, err := oidctest.New(oidctest.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	r := &rig{t: t, as: as, creds: filepath.Join(t.TempDir(), ".kontra", "credentials"), listings: map[string]int64{}}
	r.gate = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/.well-known/oauth-protected-resource/mcp" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": r.gate.URL + "/mcp", "authorization_servers": []string{as.Issuer()}})
	}))
	t.Cleanup(r.gate.Close)
	r.cp = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.cp.Close)
	return r
}

// issued is the access token the authorization server issued last.
func (r *rig) issued() string {
	trs := r.as.TokenRequests()
	if len(trs) == 0 {
		return ""
	}
	return trs[len(trs)-1].Issued
}

func errorJSON(w http.ResponseWriter, status int, code, msg string, detail any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "detail": detail})
}

// serve is the control plane: the bearer must be the token the authorization server issued for it; GET answers a
// ServiceView (404 service_not_found for none), PUT checks the version read (409 stale_version) and answers the
// ListingView.
func (r *rig) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	answer := r.answer
	r.mu.Unlock()
	if answer != nil && answer(w, req) {
		return
	}
	if req.Header.Get("Authorization") != "Bearer "+r.issued() || r.issued() == "" {
		errorJSON(w, http.StatusUnauthorized, "invalid_token", "unknown session", nil)
		return
	}
	name, ok := strings.CutPrefix(req.URL.Path, "/v1/services/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, exists := r.listings[name]
	switch req.Method {
	case http.MethodGet:
		r.gets++
		if !exists {
			errorJSON(w, http.StatusNotFound, "service_not_found", "no such service", nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "endpoint": "https://gateway.example/agents/acme/" + name,
			"listing": map[string]any{"version": cur}})
	case http.MethodPut:
		b, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		r.puts = append(r.puts, put{auth: req.Header.Get("Authorization"), body: body, raw: string(b)})
		if v, _ := body["version"].(float64); int64(v) != cur {
			errorJSON(w, http.StatusConflict, "stale_version", "the listing changed since it was read; read it again", map[string]any{"version": cur})
			return
		}
		r.listings[name] = cur + 1
		price, _ := body["price"].(map[string]any)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"service": name, "version": cur + 1, "kind": body["kind"],
			"visibility": body["visibility"], "published": body["published"], "price": map[string]any{
				"model": price["model"], "amount": price["amount"], "currency": "USD"}})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// options publish file's text at the rig's gate, the control plane at cp ("" for the gate's authorization server),
// the browser oidctest.Follow.
func (r *rig) options(out io.Writer, cp, file string) Options {
	gate, _ := url.Parse(r.gate.URL)
	return Options{File: "listing.yaml", Gate: gate, ControlPlane: cp, CredentialsPath: r.creds, Out: out,
		Client:   login.NewClient(&tls.Config{RootCAs: r.as.CAPool(), MinVersion: tls.VersionTLS12}),
		Open:     func(u string) error { go func() { _, _ = r.as.Follow(context.Background(), u) }(); return nil },
		Login:    login.Options{Wait: 20 * time.Second},
		ReadFile: func(string) ([]byte, error) { return []byte(file), nil }}
}

const perTask = "service: hello\nprice: { model: per_task, amount: \"1.00\", reviewWindowSeconds: 120, taskTimeoutSeconds: 600 }\n"

// A publish (§6.4, D323): the person signs in at the gate's authorization server with resource = the control plane
// (the client registration kontra login keeps, made once and reused); the listing goes to PUT /v1/services/hello
// with that token, version 0 for a service the control plane does not know; the next publish reads version 1 and
// replaces it. The token is not kept, and nothing but the registration is written to the credentials.
func TestPublish(t *testing.T) {
	r := newRig(t)
	var out bytes.Buffer
	if err := Run(context.Background(), r.options(&out, r.cp.URL, perTask)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "hello is published at version 1: per_task 1.00 USD, public, on "+r.cp.URL) {
		t.Fatalf("output: %s", out.String())
	}
	auths := r.as.Authorizations()
	if len(auths) != 1 || auths[0].Get("resource") != r.cp.URL || r.as.TokenRequests()[0].Form.Get("resource") != r.cp.URL {
		t.Fatalf("the sign-in asked for %v, want resource %s", auths, r.cp.URL)
	}
	if len(r.puts) != 1 || r.puts[0].auth != "Bearer "+r.issued() || r.puts[0].raw !=
		`{"kind":"a2a","visibility":"public","price":{"model":"per_task","amount":"1.00","currency":"USD","reviewWindowSeconds":120,"taskTimeoutSeconds":600},"sealed":"off","published":true,"version":0}` {
		t.Fatalf("PUT: %+v", r.puts)
	}
	creds, err := node.LoadCredentials(r.creds)
	if err != nil {
		t.Fatal(err)
	}
	c := creds.For(r.gate.URL)
	if c.APIKey != "" || c.OAuthClient == nil || c.OAuthClient.Issuer != r.as.Issuer() {
		t.Fatalf("the credentials after a publish: %+v", c)
	}
	raw, _ := os.ReadFile(r.creds)
	if strings.Contains(string(raw), r.issued()) {
		t.Fatal("the control plane's token was written to the credentials")
	}

	// Again: the same registration, the version read, the endpoint printed.
	out.Reset()
	if err := Run(context.Background(), r.options(&out, r.cp.URL, strings.Replace(perTask, `"1.00"`, `"2.00"`, 1))); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(r.as.Registrations()) != 1 || len(r.puts) != 2 || r.puts[1].body["version"] != float64(1) ||
		!strings.Contains(out.String(), "at version 2: per_task 2.00 USD") || !strings.Contains(out.String(), "https://gateway.example/agents/acme/hello") {
		t.Fatalf("the second publish: %d registrations, %+v\n%s", len(r.as.Registrations()), r.puts, out.String())
	}
	// Unpublished: said so.
	out.Reset()
	if err := Run(context.Background(), r.options(&out, r.cp.URL, perTask+"published: false\n")); err != nil || !strings.Contains(out.String(), "unpublished") {
		t.Fatalf("unpublish: %v %s", err, out.String())
	}
}

// --version names the version the publisher read: sent as is, with no read; a stale one is the control plane's 409
// stale_version, said with the version it is at now, and nothing is published.
func TestPublishStaleVersion(t *testing.T) {
	r := newRig(t)
	r.listings["hello"] = 4
	var out bytes.Buffer
	o := r.options(&out, r.cp.URL, perTask)
	v := int64(3)
	o.Version = &v
	err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "changed since version 3 was read: it is at version 4 now (409 stale_version)") ||
		!strings.Contains(err.Error(), "--version 4") {
		t.Fatalf("a stale --version: %v", err)
	}
	if r.gets != 0 || len(r.puts) != 1 || r.listings["hello"] != 4 {
		t.Fatalf("%d reads, %d puts, version %d", r.gets, len(r.puts), r.listings["hello"])
	}
	v = 4
	if err := Run(context.Background(), o); err != nil || r.listings["hello"] != 5 {
		t.Fatalf("the current --version: %v (version %d)", err, r.listings["hello"])
	}
}

// With no --control-plane the control plane is the gate's authorization server (Cloud's: its issuer is its public
// URL, D-M5-38): the token is asked for that resource and the routes are under it.
func TestPublishControlPlaneIsTheIssuer(t *testing.T) {
	var r *rig
	as, err := oidctest.New(oidctest.Config{Hook: func(w http.ResponseWriter, req *http.Request) bool {
		if !strings.HasPrefix(req.URL.Path, "/v1/services/") {
			return false
		}
		r.serve(w, req)
		return true
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	r = newRig(t)
	r.as = as
	r.gate.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": r.gate.URL + "/mcp", "authorization_servers": []string{as.Issuer()}})
	})
	var out bytes.Buffer
	if err := Run(context.Background(), r.options(&out, "", perTask)); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if auths := as.Authorizations(); len(auths) != 1 || auths[0].Get("resource") != as.Issuer() || len(r.puts) != 1 {
		t.Fatalf("resource %v, %d puts", auths, len(r.puts))
	}
}

// What the control plane refuses reaches the terminal as its status, code and message, printable: a person's sign-in
// it does not take (403 fresh_login_required), a listing it refuses (422, a message with an escape sequence), a
// redirect (not followed: the token goes nowhere else), and an answer to the publish that is not a listing.
func TestPublishRefusals(t *testing.T) {
	r := newRig(t)
	var redirected bool
	for _, c := range []struct {
		name   string
		answer func(w http.ResponseWriter, req *http.Request) bool
		want   []string
	}{
		{"fresh_login_required", func(w http.ResponseWriter, req *http.Request) bool {
			errorJSON(w, http.StatusForbidden, "fresh_login_required", "an API key cannot do this", nil)
			return true
		}, []string{"HTTP 403 fresh_login_required: an API key cannot do this", "a person's recent sign-in"}},
		{"422", func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method == http.MethodGet {
				return false
			}
			errorJSON(w, http.StatusUnprocessableEntity, "amount_too_small", "a task is priced at 0.50 USD at least\x1b[2J", nil)
			return true
		}, []string{"HTTP 422 amount_too_small: a task is priced at 0.50 USD at least\\x1b[2J", "nothing was published"}},
		{"redirect", func(w http.ResponseWriter, req *http.Request) bool {
			if req.URL.Path == "/elsewhere" {
				redirected = true
			}
			http.Redirect(w, req, "/elsewhere", http.StatusTemporaryRedirect)
			return true
		}, []string{"redirect (307)"}},
		{"not a listing", func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method == http.MethodGet {
				return false
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
			return true
		}, []string{"not a listing"}},
	} {
		r.mu.Lock()
		r.answer = c.answer
		r.mu.Unlock()
		var out bytes.Buffer
		err := Run(context.Background(), r.options(&out, r.cp.URL, perTask))
		for _, w := range c.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %v, want %q", c.name, err, w)
			}
		}
		if err != nil && strings.Contains(err.Error(), "\x1b") {
			t.Errorf("%s: an escape sequence reached the terminal", c.name)
		}
	}
	if redirected {
		t.Error("kontra publish followed the control plane's redirect")
	}
}

// A gate with no sign-in has no control plane to publish to: said so, nothing sent.
func TestPublishNoSignIn(t *testing.T) {
	r := newRig(t)
	r.gate.Config.Handler = http.NotFoundHandler()
	err := Run(context.Background(), r.options(io.Discard, r.cp.URL, perTask))
	if err == nil || !strings.Contains(err.Error(), "no sign-in") || len(r.puts) != 0 {
		t.Fatalf("%v, %d puts", err, len(r.puts))
	}
}
