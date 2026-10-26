package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
)

// recorded is one request the recording target received.
type recorded struct {
	Method, RequestURI, Path, Host string
	Header                         http.Header
	Body                           []byte
}

// recorder is the --serve target of sidecar_not_a_proxy: it records every request and answers 200.
type recorder struct {
	URL, Addr string
	srv       *http.Server
	mu        sync.Mutex
	reqs      []recorded
}

func startRecorder() (*recorder, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &recorder{Addr: l.Addr().String(), URL: "http://" + l.Addr().String()}
	r.srv = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, recorded{Method: req.Method, RequestURI: req.RequestURI, Path: req.URL.Path, Host: req.Host,
			Header: req.Header.Clone(), Body: body})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"recorded":true}`)
	})}
	go func() { _ = r.srv.Serve(l) }()
	return r, nil
}

func (r *recorder) requests() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

// canary is a listener that must never be dialed: it counts the connections it accepts.
type canary struct {
	Addr     string
	l        net.Listener
	accepted atomic.Int64
}

func startCanary(ip string) (*canary, error) {
	l, err := net.Listen("tcp", ip+":0")
	if err != nil {
		return nil, err
	}
	c := &canary{Addr: l.Addr().String(), l: l}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			conn.Close()
		}
	}()
	return c, nil
}

// TestSidecarNotAProxy is sidecar_not_a_proxy (spec/tunnel.md §8.1–§8.3, §15, §16; PRD §15), with no
// Postgres or Temporal: the FakeGate (the gate's own tunnel handler, a fixed admission) admits the real
// `kontra connect` binary for service hello, whose --serve target T records every request. Canary
// listeners on 127.0.0.2 and on a second loopback port must never be dialed. Over streams the FakeGate
// opens itself, the test sends what a hostile or broken gate could: a foreign Host, an absolute-form target,
// '..', encoded and '//' paths, CONNECT, an Upgrade, TRACE, a JSON-RPC message whose part is a url at a
// canary, a preamble naming a service the node does not host and one naming another kind.
//
// T receives exactly the requests that may pass, each with T's own Host, its path cleaned under T's base,
// and no Origin, Forwarded or X-Forwarded-*; the url-part message arrives byte-identical (the node reads no
// body and dials no url, §8.3). With M2's root base (non-root --serve bases are refused, SPEC-CONFLICT C1),
// '..' and encoded paths are confined to T, not refused (M2 plan audit): /../../etc/passwd arrives as
// /etc/passwd. The 400 for a path outside a non-root base is internal/node's TestProxyConfines, which builds
// a Serve directly. CONNECT, Upgrade and TRACE are 400 written by the node; `other` is 404 Kontra-Node:
// not_hosted; kind mcp is 404 Kontra-Node: kind_mismatch. Neither canary accepts a connection, and the
// node's only TCP sockets (its fds mapped to /proc/<pid>/net/tcp, M2 plan audit) are to the FakeGate and T.
func TestSidecarNotAProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: runs the kontra connect binary; skipped under -short")
	}
	root, err := TempRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "sidecar-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin, err := nodeBinary()
	if err != nil {
		t.Fatal(err)
	}
	target, err := startRecorder()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.srv.Close() })
	other, err := startCanary("127.0.0.2")
	if err != nil {
		t.Fatalf("127.0.0.2 does not bind here: %v", err)
	}
	t.Cleanup(func() { other.l.Close() })
	second, err := startCanary("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.l.Close() })
	gate, err := StartFakeGate([]string{"hello"}, filepath.Join(dir, "fakegate.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gate.Close() })

	home := filepath.Join(dir, "home")
	n := &Node{
		Name: "sidecar", Home: home, KeyPath: filepath.Join(home, nodekey.DirName, "identity.key"), bin: bin,
		OutPath: filepath.Join(dir, "node.out"), LogPath: filepath.Join(dir, "node.log"),
		opts: NodeOptions{Gate: gate.URL, Serves: map[string]string{"hello": target.URL}},
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "KONTRA_KEY=") {
			n.env = append(n.env, kv)
		}
	}
	n.env = append(n.env, RunEnv+"=sidecar-"+randomHex(4), "HOME="+home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := n.launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Kill() })
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("node log:\n%s\nfake gate log:\n%s", tail(n.LogPath, 60), tail(filepath.Join(dir, "fakegate.log"), 30))
		}
	})
	sess, err := gate.Session(30 * time.Second)
	if err != nil {
		t.Fatalf("%v\n%s", err, tail(n.LogPath, 30))
	}

	hello := tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A, Task: "t-1", Context: "c-1"}
	hostile := "Origin: http://evil.example\r\nForwarded: for=203.0.113.9;host=evil.example\r\n" +
		"X-Forwarded-For: 203.0.113.9\r\nX-Forwarded-Host: evil.example\r\nX-Forwarded-Proto: https\r\n"
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"SendMessage","params":{"message":{"messageId":"m-1","role":"ROLE_USER",`+
		`"parts":[{"url":"http://%s/secret","mediaType":"text/plain"}]}}}`, other.Addr)
	pass := []struct {
		what, raw, path string
		body            string
	}{
		{"a foreign Host", "GET / HTTP/1.1\r\nHost: evil.example:" + port(other.Addr) + "\r\n" + hostile + "\r\n", "/", ""},
		{"an absolute-form target", "GET http://" + other.Addr + "/x HTTP/1.1\r\nHost: " + other.Addr + "\r\n" + hostile + "\r\n", "/x", ""},
		{"a .. path", "GET /../../etc/passwd HTTP/1.1\r\nHost: " + second.Addr + "\r\n\r\n", "/etc/passwd", ""},
		{"an encoded .. path", "GET /%2e%2e/x HTTP/1.1\r\nHost: " + second.Addr + "\r\n\r\n", "/x", ""},
		{"a // path", "GET //" + other.Addr + "/x HTTP/1.1\r\nHost: evil.example\r\n\r\n", "/" + other.Addr + "/x", ""},
		{"a url part at a private address", "POST / HTTP/1.1\r\nHost: " + other.Addr + "\r\nContent-Type: application/json\r\n" + hostile +
			fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body), "/", body},
	}
	for _, c := range pass {
		resp, got, err := Exchange(sess, hello, c.raw, 15*time.Second)
		if err != nil || resp.StatusCode != http.StatusOK || string(got) != `{"recorded":true}` {
			t.Fatalf("%s: %v %v %s, want T's answer", c.what, err, resp, got)
		}
		if v := resp.Header.Get(tunnel.HeaderNode); v != "" {
			t.Fatalf("%s: T's answer carries Kontra-Node %q", c.what, v)
		}
	}
	refused := []struct {
		what   string
		pre    tunnel.Preamble
		raw    string
		status int
		reason tunnel.NodeReason
	}{
		{"CONNECT", hello, "CONNECT " + other.Addr + " HTTP/1.1\r\nHost: " + other.Addr + "\r\n\r\n", http.StatusBadRequest, ""},
		{"an Upgrade", hello, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", http.StatusBadRequest, ""},
		{"TRACE", hello, "TRACE / HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusBadRequest, ""},
		{"a service not hosted", tunnel.Preamble{Op: tunnel.OpHTTP, Service: "other", Kind: tunnel.KindA2A},
			"GET / HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusNotFound, tunnel.NodeNotHosted},
		{"another kind", tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindMCP},
			"GET / HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusNotFound, tunnel.NodeKindMismatch},
	}
	for _, c := range refused {
		resp, got, err := Exchange(sess, c.pre, c.raw, 15*time.Second)
		if err != nil || resp.StatusCode != c.status || resp.Header.Get(tunnel.HeaderNode) != string(c.reason) {
			t.Fatalf("%s: %v HTTP %v Kontra-Node %q %s, want %d with Kontra-Node %q", c.what, err, statusOf(resp),
				headerOf(resp, tunnel.HeaderNode), got, c.status, c.reason)
		}
	}

	reqs := target.requests()
	if len(reqs) != len(pass) {
		t.Fatalf("T received %d requests, want exactly the %d that may pass: %+v", len(reqs), len(pass), reqs)
	}
	for i, r := range reqs {
		c := pass[i]
		if r.Host != target.Addr || r.Path != c.path || r.RequestURI != c.path {
			t.Errorf("%s: T received %s %q (path %q) with Host %q, want %q with Host %q", c.what, r.Method, r.RequestURI, r.Path, r.Host, c.path, target.Addr)
		}
		for name := range r.Header {
			if name == "Origin" || name == "Forwarded" || strings.HasPrefix(name, "X-Forwarded-") {
				t.Errorf("%s: T received %s: %v", c.what, name, r.Header[name])
			}
		}
		if c.body != "" && !bytes.Equal(r.Body, []byte(c.body)) {
			t.Errorf("%s: T received a body that is not the message byte for byte:\n got %s\nwant %s", c.what, r.Body, c.body)
		}
	}
	if a, b := other.accepted.Load(), second.accepted.Load(); a != 0 || b != 0 {
		t.Fatalf("the canaries accepted %d (127.0.0.2) and %d (second loopback port) connections, want none", a, b)
	}
	socks := checkNoListener(t, n.Pid(), "kontra connect")
	for _, s := range socks {
		if s.Remote != gate.Addr && s.Remote != target.Addr {
			t.Fatalf("kontra connect holds a TCP socket to %s: its only peers may be the FakeGate (%s) and T (%s): %+v",
				s.Remote, gate.Addr, target.Addr, socks)
		}
	}
	if len(socks) == 0 {
		t.Fatal("kontra connect holds no TCP socket: the check above proved nothing")
	}
	t.Logf("kontra connect's TCP sockets: %+v", socks)
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func statusOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

func headerOf(r *http.Response, name string) string {
	if r == nil {
		return ""
	}
	return r.Header.Get(name)
}
