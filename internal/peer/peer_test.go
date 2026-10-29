package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func TestNewSignerRefusesAShortSecret(t *testing.T) {
	if _, err := NewSigner(secret[:31]); err == nil {
		t.Fatal("a 31-byte secret was accepted")
	}
	if _, err := NewSigner(secret); err != nil {
		t.Fatal(err)
	}
}

// The canonical form: names lowercased and sorted, one "name: value\n" line per value in order; only the
// forwarded headers and every Mcp-Param-*.
func TestForwardedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Add("Accept", "application/json")
	h.Add("Accept", "text/event-stream")
	h.Set("Mcp-Param-Region", "eu")
	h.Set("Last-Event-ID", "ev-7")
	h.Set("Authorization", "Bearer secret")
	h.Set("Mcp-Session-Id", "travels in the assertion")
	h.Set("X-Extra", "1")
	fwd, sum := ForwardedHeaders(h)
	want := "accept: application/json\naccept: text/event-stream\ncontent-type: application/json\nlast-event-id: ev-7\nmcp-param-region: eu\n"
	if sum != sha256.Sum256([]byte(want)) {
		t.Errorf("hash is not over the canonical form %q", want)
	}
	for _, k := range []string{"Authorization", "Mcp-Session-Id", "X-Extra"} {
		if fwd.Get(k) != "" {
			t.Errorf("%s is forwarded", k)
		}
	}
	if fwd.Get("Mcp-Param-Region") != "eu" || len(fwd.Values("Accept")) != 2 {
		t.Errorf("forwarded: %v", fwd)
	}
	h2 := h.Clone()
	h2.Set("Mcp-Param-Region", "us")
	if _, s2 := ForwardedHeaders(h2); s2 == sum {
		t.Error("a changed Mcp-Param-* hashes the same")
	}
}

func assertion() Assertion {
	return Assertion{Replica: "r-a", TenantID: uuid.New(), Service: "fs", TunnelSession: uuid.New(), Principal: "key:kk_t_x",
		BuyerID: uuid.New(), Via: "api_key", MCPSession: "ab", UpstreamSID: "up-1", Headers: "00", Nonce: "n1",
		At: time.Now().UTC()}
}

func TestSignVerify(t *testing.T) {
	s, _ := NewSigner(secret)
	body := sha256.Sum256([]byte(`{"x":1}`))
	a := assertion()
	h, err := s.Sign(a, "POST", Path, body)
	if err != nil {
		t.Fatal(err)
	}
	now := a.At.Add(time.Second)
	got, err := s.Verify(h, "POST", Path, body, now)
	if err != nil || got.UpstreamSID != "up-1" || got.TunnelSession != a.TunnelSession {
		t.Fatalf("%+v %v", got, err)
	}
	// Replayed.
	if _, err := s.Verify(h, "POST", Path, body, now); !errors.Is(err, ErrReplayed) {
		t.Errorf("replay: %v", err)
	}
	// Another method, path or body.
	for _, tc := range []struct {
		m, p string
		b    [32]byte
	}{{"GET", Path, body}, {"POST", "/other", body}, {"POST", Path, sha256.Sum256(nil)}} {
		if _, err := s.Verify(h, tc.m, tc.p, tc.b, now); !errors.Is(err, ErrForged) {
			t.Errorf("%s %s: %v", tc.m, tc.p, err)
		}
	}
	// Another secret.
	o, _ := NewSigner(bytes.Repeat([]byte("z"), 32))
	if _, err := o.Verify(h, "POST", Path, body, now); !errors.Is(err, ErrForged) {
		t.Errorf("other secret: %v", err)
	}
	// Stale: 31 s.
	a2 := assertion()
	a2.Nonce = "n2"
	h2, _ := s.Sign(a2, "POST", Path, body)
	if _, err := s.Verify(h2, "POST", Path, body, a2.At.Add(31*time.Second)); !errors.Is(err, ErrStale) {
		t.Errorf("31 s old: %v", err)
	}
	// The assertion's own bytes changed (an upstream session id swapped): the MAC fails.
	p, m, _ := strings.Cut(h, ".")
	swapped := strings.Replace(p, p[len(p)-4:], "AAAA", 1) + "." + m
	if _, err := s.Verify(swapped, "POST", Path, body, now); !errors.Is(err, ErrForged) {
		t.Errorf("tampered: %v", err)
	}
	for _, bad := range []string{"", ".", "abc", "a.b.c", strings.Repeat("a", 9000) + ".b"} {
		if _, err := s.Verify(bad, "POST", Path, body, now); !errors.Is(err, ErrForged) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// runner records what reaches it and answers 200.
type runner struct {
	mu    sync.Mutex
	got   []Assertion
	hdrs  []http.Header
	bodys [][]byte
}

func (r *runner) RunExchange(ctx context.Context, a Assertion, req *http.Request, w http.ResponseWriter) error {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, a)
	r.hdrs = append(r.hdrs, req.Header.Clone())
	r.bodys = append(r.bodys, b)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: ok\n\n")
	return nil
}

func (r *runner) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// The listener: Path only, the assertion and the headers verified, only the hashed headers reach the runner.
func TestServer(t *testing.T) {
	s, _ := NewSigner(secret)
	run := &runner{}
	srv, err := NewServer(s, run)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c, _ := NewClient(s)
	addr := strings.TrimPrefix(ts.URL, "http://")
	req := func() *http.Request {
		r, _ := http.NewRequest("POST", "/", strings.NewReader(`{"jsonrpc":"2.0"}`))
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Mcp-Name", "echo")
		return r
	}
	resp, err := c.Forward(context.Background(), addr, assertion(), req())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "data: ok\n\n" || run.n() != 1 {
		t.Fatalf("%d %q %d", resp.StatusCode, b, run.n())
	}
	if got := run.hdrs[0]; got.Get(Header) != "" || got.Get("Mcp-Name") != "echo" || string(run.bodys[0]) != `{"jsonrpc":"2.0"}` {
		t.Errorf("reached the runner: %v %q", got, run.bodys[0])
	}

	// Raw requests: a signed one replayed, one whose header changed after signing, an extra header (not forwarded),
	// any other path.
	signed := func(hdr http.Header, body string) http.Header {
		fwd, sum := ForwardedHeaders(hdr)
		a := assertion()
		a.Nonce = uuid.NewString()
		a.Headers = hex.EncodeToString(sum[:])
		v, _ := s.Sign(a, "POST", Path, sha256.Sum256([]byte(body)))
		fwd.Set(Header, v)
		return fwd
	}
	send := func(path string, h http.Header, body string) int {
		r, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
		r.Header = h
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	base := http.Header{}
	base.Set("Mcp-Name", "echo")
	base.Set("Last-Event-ID", "ev-1")
	base.Set("Mcp-Param-Region", "eu")
	h := signed(base, "{}")
	if st := send(Path, h, "{}"); st != 200 {
		t.Fatalf("signed: %d", st)
	}
	if st := send(Path, h, "{}"); st != 401 {
		t.Errorf("replayed: %d", st)
	}
	for _, k := range []string{"Mcp-Name", "Last-Event-ID", "Mcp-Param-Region"} {
		h := signed(base, "{}")
		h.Set(k, "changed")
		if st := send(Path, h, "{}"); st != 401 {
			t.Errorf("%s changed after signing: %d", k, st)
		}
	}
	h = signed(base, "{}")
	h.Set("Mcp-Param-Extra", "added")
	if st := send(Path, h, "{}"); st != 401 {
		t.Errorf("an Mcp-Param-* added after signing: %d", st)
	}
	h = signed(base, "{}")
	h.Set("X-Smuggled", "1")
	h.Set("Authorization", "Bearer x")
	before := run.n()
	if st := send(Path, h, "{}"); st != 200 {
		t.Fatalf("extra header: %d", st)
	}
	if got := run.hdrs[before]; got.Get("X-Smuggled") != "" || got.Get("Authorization") != "" {
		t.Errorf("an extra header reached the runner: %v", got)
	}
	if st := send(Path, signed(base, "{}"), `{"changed":1}`); st != 401 {
		t.Errorf("body changed after signing: %d", st)
	}
	n := run.n()
	for _, p := range []string{"/", "/mcp/fs", Path + "/x", "/v1/tunnel"} {
		if st := send(p, signed(base, "{}"), "{}"); st != 404 {
			t.Errorf("%s: %d", p, st)
		}
	}
	if st := send(Path, http.Header{}, "{}"); st != 401 {
		t.Errorf("no assertion: %d", st)
	}
	if run.n() != n {
		t.Errorf("a refused request reached the runner")
	}
}
