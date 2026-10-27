package sealing

import (
	"bytes"
	"context"
	"encoding/json"
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
	err := sealCommand(context.Background(), args, strings.NewReader(stdin), &out, &errb, home, http.DefaultClient, time.Now)
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
