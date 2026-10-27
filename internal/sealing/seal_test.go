package sealing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// sealGate serves a signed card of a sealed service and the gate's key set, as a gate does (spec/relay.md §3).
type sealGate struct {
	srv  *httptest.Server
	keys *jws.KeySet
	card []byte // served at /agents/echo/.well-known/agent-card.json
}

func newSealGate(t *testing.T, node *age.X25519Identity) *sealGate {
	t.Helper()
	keys, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	g := &sealGate{keys: keys}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		b, _ := keys.JWKS()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /agents/echo/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(g.card)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	g.card = g.sign(t, cardWith(extensionFor(true, node.Recipient())))
	return g
}

// sign adds the gate's signature (jku = the server's origin) to card.
func (g *sealGate) sign(t *testing.T, card []byte) []byte {
	t.Helper()
	sig, err := g.keys.Sign(card, g.srv.URL+jws.JWKSPath, &jws.Upstream{SHA256: strings.Repeat("0", 64), URL: g.srv.URL + "/agents/echo/.well-known/agent-card.upstream.json"})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := strictjson.Members(card)
	sb, _ := json.Marshal([]jws.Signature{sig})
	m["signatures"] = sb
	return a2awire.Encode(m)
}

func runSeal(t *testing.T, home string, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := sealCommand(context.Background(), args, strings.NewReader(stdin), &out, &errb, home, "", http.DefaultClient, time.Now)
	return out.String(), errb.String(), err
}

// kontra seal prints a Message whose one part opens with the hosting node's identity; --open opens a reply part
// sealed to the job's key; a card whose signature does not verify is refused.
func TestSealCommand(t *testing.T) {
	node, _ := age.GenerateX25519Identity()
	g := newSealGate(t, node)
	home := t.TempDir()
	out, stderr, err := runSeal(t, home, "MARKER file contents", "--service", g.srv.URL+"/agents/echo", "--context", "ctx-9", "-")
	if err != nil {
		t.Fatalf("kontra seal: %v (%s)", err, stderr)
	}
	if !strings.Contains(stderr, "first seen") {
		t.Errorf("the pin's first sight is not printed: %q", stderr)
	}
	var msg struct {
		MessageID string            `json:"messageId"`
		Role      string            `json:"role"`
		ContextID string            `json:"contextId"`
		Parts     []json.RawMessage `json:"parts"`
		Metadata  map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if msg.Role != "ROLE_USER" || msg.ContextID != "ctx-9" || msg.MessageID == "" || len(msg.Parts) != 1 || strings.Contains(out, "MARKER") {
		t.Fatalf("the message: %s", out)
	}
	sp, ok, err := sealed.ParsePart(msg.Parts[0])
	if !ok || err != nil {
		t.Fatalf("the part: %v", err)
	}
	hd, body, err := sealed.Open([]age.Identity{node}, sp.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "MARKER file contents" || hd.ContextID != "ctx-9" || hd.MessageID != msg.MessageID || hd.ReplyTo != msg.Metadata[sealed.MetadataReplyTo] ||
		hd.Kids[0] != tunnel.KeyID(node.Recipient().String()) || hd.Kind != sealed.KindRaw {
		t.Fatalf("header %+v body %q", hd, body)
	}

	// A reply sealed to the job's key, as a hosting node seals it.
	u, _ := url.Parse(g.srv.URL)
	jobs, err := nodekey.OpenJobKeys(home, u.Host)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(jobs.Dir())
	var jobID string
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".key"); ok {
			jobID = id
		}
	}
	key, err := jobs.Load(jobID)
	if err != nil || key.Recipient().String() != hd.ReplyTo {
		t.Fatalf("the job's key: %v", err)
	}
	rk, _ := sealed.DecodeRK(hd.RK)
	rh := sealed.Header{V: 1, Dir: sealed.DirReply, TaskID: "t1", ContextID: "ctx-9", MessageID: "r1", Part: 0, Parts: 1, Kind: sealed.KindText,
		Kids: []string{tunnel.KeyID(hd.ReplyTo)}}
	rh.Auth, _ = sealed.Auth(rk, rh, []byte("the reply"))
	ct, err := sealed.Seal([]age.Recipient{key.Recipient()}, rh, []byte("the reply"))
	if err != nil {
		t.Fatal(err)
	}
	part, _ := sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: "text"})
	pf := filepath.Join(t.TempDir(), "part.json")
	if err := os.WriteFile(pf, part, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, _, err := runSeal(t, home, "", "--open", pf)
	if err != nil || opened != "the reply" {
		t.Fatalf("--open: %q %v", opened, err)
	}
	// A reply whose auth is not the job's is refused.
	rh.Auth = sealed.EncodeRK(make([]byte, 32))
	ct, _ = sealed.Seal([]age.Recipient{key.Recipient()}, rh, []byte("forged"))
	part, _ = sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: "text"})
	_ = os.WriteFile(pf, part, 0o600)
	if out, _, err := runSeal(t, home, "", "--open", pf); err == nil || strings.Contains(out, "forged") {
		t.Fatalf("a forged reply opens: %q %v", out, err)
	}

	// A tampered card (a recipient the gate did not sign) is refused, and nothing is sealed.
	other, _ := age.GenerateX25519Identity()
	m, _ := strictjson.Members(g.card)
	signed := m["signatures"]
	m2, _ := strictjson.Members(cardWith(extensionFor(true, other.Recipient())))
	m2["signatures"] = signed
	g.card = a2awire.Encode(m2)
	if out, _, err := runSeal(t, t.TempDir(), "x", "--service", g.srv.URL+"/agents/echo", "-"); err == nil || out != "" {
		t.Fatalf("a tampered card: %q %v", out, err)
	}
}

// The card of a service that is not listed is served only to a caller its allow: admits (relay.md §2.1): kontra seal
// sends its API key, --api-key (or @file) or $KONTRA_API_KEY, to the gate's origin, and without one is refused. A
// follow-up (--task) is sealed with the task's job key, rk and contextId, which --open bound to the task its reply
// names (ext-sealed.md §5 rule 3); a task with no job is refused (SealCommand's SPEC-CONFLICTs).
func TestSealAPIKeyAndFollowUp(t *testing.T) {
	node, _ := age.GenerateX25519Identity()
	g := newSealGate(t, node)
	var seen []string
	inner := g.srv.Config.Handler
	g.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		if strings.HasPrefix(r.URL.Path, "/agents/") && r.Header.Get("Authorization") != "Bearer kk_test" {
			http.NotFound(w, r) // the unknown-service answer of relay §2.1
			return
		}
		inner.ServeHTTP(w, r)
	})
	home := t.TempDir()
	svc := g.srv.URL + "/agents/echo"
	if _, _, err := runSeal(t, home, "x", "--service", svc, "-"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("without a key: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("kk_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen = nil
	out, stderr, err := runSeal(t, home, "first", "--service", svc, "--api-key", "@"+keyFile, "--context", "ctx-1", "-")
	if err != nil {
		t.Fatalf("with --api-key @file: %v (%s)", err, stderr)
	}
	if len(seen) == 0 {
		t.Fatal("no GET reached the gate")
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, "Bearer kk_test") {
			t.Errorf("a GET to the gate's origin without the key: %q", s)
		}
	}
	var out2 bytes.Buffer
	if err := sealCommand(context.Background(), []string{"--service", svc, "-"}, strings.NewReader("env"), &out2, io.Discard, t.TempDir(),
		"kk_test", http.DefaultClient, time.Now); err != nil {
		t.Fatalf("with $KONTRA_API_KEY: %v", err)
	}
	if _, _, err := runSeal(t, home, "x", "--service", svc, "--api-key", "a b", "-"); !errors.Is(err, ErrUsage) {
		t.Fatalf("--api-key with a space: %v", err)
	}
	var first struct {
		MessageID string            `json:"messageId"`
		ContextID string            `json:"contextId"`
		Parts     []json.RawMessage `json:"parts"`
		Metadata  map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatal(err)
	}
	sp, _, _ := sealed.ParsePart(first.Parts[0])
	hd, _, err := sealed.Open([]age.Identity{node}, sp.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}

	// No job is bound to the task yet: the follow-up is refused.
	if _, _, err := runSeal(t, home, "second", "--service", svc, "--api-key", "kk_test", "--task", "task-1", "-"); err == nil ||
		!strings.Contains(err.Error(), "--open") {
		t.Fatalf("a follow-up before --open: %v", err)
	}
	// The hosting node's reply names the gate task: --open binds the job to it.
	u, _ := url.Parse(g.srv.URL)
	jobs, _ := nodekey.OpenJobKeys(home, u.Host)
	rk, _ := sealed.DecodeRK(hd.RK)
	rh := sealed.Header{V: 1, Dir: sealed.DirReply, TaskID: "task-1", ContextID: "ctx-1", MessageID: "r1", Part: 0, Parts: 1,
		Kind: sealed.KindText, Kids: []string{tunnel.KeyID(hd.ReplyTo)}}
	rh.Auth, _ = sealed.Auth(rk, rh, []byte("the reply"))
	replyTo, err := age.ParseX25519Recipient(hd.ReplyTo)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := sealed.Seal([]age.Recipient{replyTo}, rh, []byte("the reply"))
	part, _ := sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: "text"})
	pf := filepath.Join(t.TempDir(), "part.json")
	_ = os.WriteFile(pf, part, 0o600)
	if opened, _, err := runSeal(t, home, "", "--open", pf); err != nil || opened != "the reply" {
		t.Fatalf("--open: %q %v", opened, err)
	}
	if k, err := jobs.Load("task-1"); err != nil || k.Recipient().String() != hd.ReplyTo {
		t.Fatalf("the job is not bound to task-1: %v", err)
	}
	// A whole Task, as SendMessage answers it: its history holds the client's own sealed message (sealed to the
	// hosting node, which no job key opens), skipped; the reply in its status message opens.
	task := `{"id":"task-1","contextId":"ctx-1","status":{"state":"TASK_STATE_INPUT_REQUIRED","message":{"messageId":"r1",` +
		`"role":"ROLE_AGENT","parts":[` + string(part) + `]}},"history":[{"messageId":"` + first.MessageID + `","role":"ROLE_USER",` +
		`"parts":[` + string(first.Parts[0]) + `]}]}`
	tf := filepath.Join(t.TempDir(), "task.json")
	_ = os.WriteFile(tf, []byte(task), 0o600)
	if opened, _, err := runSeal(t, home, "", "--open", tf); err != nil || opened != "the reply" {
		t.Fatalf("--open of a Task with the client's own sealed message in its history: %q %v", opened, err)
	}
	// A part no job key here opens, alone, opens nothing.
	hf := filepath.Join(t.TempDir(), "request.json")
	_ = os.WriteFile(hf, first.Parts[0], 0o600)
	if _, _, err := runSeal(t, home, "", "--open", hf); err == nil || !strings.Contains(err.Error(), "no sealed part") {
		t.Fatalf("--open of the request's own part: %v", err)
	}
	if opened, _, err := runSeal(t, home, "", "--open", pf); err != nil || opened != "the reply" {
		t.Fatalf("--open again, once bound: %q %v", opened, err)
	}

	// The follow-up: the task's job key, rk and contextId; taskId in the header and the message.
	out, _, err = runSeal(t, home, "second", "--service", svc, "--api-key", "kk_test", "--task", "task-1", "-")
	if err != nil {
		t.Fatalf("the follow-up: %v", err)
	}
	var follow struct {
		TaskID    string            `json:"taskId"`
		ContextID string            `json:"contextId"`
		Parts     []json.RawMessage `json:"parts"`
		Metadata  map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &follow); err != nil {
		t.Fatal(err)
	}
	sp, _, _ = sealed.ParsePart(follow.Parts[0])
	fh, body, err := sealed.Open([]age.Identity{node}, sp.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second" || fh.TaskID != "task-1" || fh.ReplyTo != hd.ReplyTo || fh.RK != hd.RK || fh.ContextID != "ctx-1" ||
		follow.TaskID != "task-1" || follow.ContextID != "ctx-1" || follow.Metadata[sealed.MetadataReplyTo] != hd.ReplyTo {
		t.Fatalf("the follow-up's header %+v, message %+v", fh, follow)
	}
}
