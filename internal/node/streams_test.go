package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// sendHosted makes one exchange the gate opens on s for the hosted service hello (a stream with its preamble),
// as the relay does.
func sendHosted(t *testing.T, s *tunnelgate.Session, body string) *http.Response {
	t.Helper()
	res, err := hostedExchange(s, body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func hostedExchange(s *tunnelgate.Session, body string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := upstream.NewTunnel("hello", tunnel.KindA2A, sessOpener{s: s, service: "hello"})
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.Endpoint(tb, upstream.Info{Path: "/"}), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := tb.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.ReadAll(res.Body)
	res.Body.Close()
	return res, nil
}

func sendMessageWith(ref blob.Ref) string {
	return `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[` +
		`{"url":"` + ref.URL() + `","mediaType":"application/octet-stream"}]}}}`
}

// serveTarget serves a --serve target on 127.0.0.1.
func serveTarget(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// A burst of local requests larger than the gate's cap of node-opened streams (spec/tunnel.md §14: 32) never
// costs the node its session: it keeps at most 32 node-opened streams in flight per session, queues the others a
// while, and answers 429 (Retry-After) to those it cannot open in time, opening no stream for them. The gate
// therefore never meets a node holding more refused streams than it allows, and hosting on the session goes on.
// (A stream the node has seen end still counts at the gate until the gate's goroutine reads the node's FIN, so the
// gate may briefly count a few more than 32 and answer one of them 429 itself, with Retry-After: a refusal the
// local client sees as such, never a lost session.)
func TestHiringBurstKeepsTheSession(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Hire: slow} })
	tn := mustNode(t, g, testConfig(t, g.url, ""))
	tn.n.slotWait = 2 * time.Second
	tn.run(t)
	tn.welcomed(t)
	s1 := g.session(t)
	port := tn.localPort(t)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		hist    = map[int]int{}
		retries atomic.Int32
		peak    atomic.Int64
	)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				if n := s1.NodeInFlight(); n > peak.Load() {
					peak.Store(n)
				}
			}
		}
	}()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 300, MaxConnsPerHost: 0}}
	defer client.CloseIdleConnections()
	for range 200 {
		wg.Go(func() {
			res, err := client.Get("http://127.0.0.1:" + port + "/agents/hello/tasks/t1")
			code := -1
			if err == nil {
				code = res.StatusCode
				if code == http.StatusTooManyRequests && res.Header.Get("Retry-After") != "" {
					retries.Add(1)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
			mu.Lock()
			hist[code]++
			mu.Unlock()
		})
	}
	wg.Wait()
	close(stop)
	t.Logf("statuses %v; the gate's peak of node-opened streams in flight %d", hist, peak.Load())
	for code, n := range hist {
		if code != http.StatusOK && code != http.StatusTooManyRequests {
			t.Errorf("%d local requests answered %d: want 200, or 429 when no stream could be opened in time", n, code)
		}
	}
	if int(retries.Load()) != hist[http.StatusTooManyRequests] {
		t.Errorf("a 429 without Retry-After")
	}
	if hist[http.StatusOK] < 32 {
		t.Errorf("only %d requests served", hist[http.StatusOK])
	}
	if strings.Contains(g.logs.String(), "the node keeps more refused streams open than the gate allows") {
		t.Fatal("the gate closed the session of a node whose local clients sent a burst")
	}
	if strings.Contains(tn.logs.String(), "tunnel session closed") || !tn.running() {
		t.Fatal("the node lost its session to a burst of local requests")
	}
	g.noSession(t, 200*time.Millisecond)
	eventually(t, 10*time.Second, "every node-opened stream ended at the gate", func() bool { return s1.NodeInFlight() == 0 })
}

// failingBlobs serves objects on blob streams, ends the stream of a broken ref unanswered (the gate's own failure
// in transit), and records the refs asked for.
type failingBlobs struct {
	blobGate
	broken map[blob.Ref]bool
}

func (f *failingBlobs) ServeBlob(ctx context.Context, s *tunnelgate.Session, ref string, w io.Writer) error {
	if f.broken[blob.Ref(ref)] {
		return errors.New("the bucket went away")
	}
	return f.blobGate.ServeBlob(ctx, s, ref, w)
}

// A hosted exchange whose part waits for a node-opened stream (spec/tunnel.md §8.3, §14) shares the session's 32
// with hiring: its blob fetch waits for a slot instead of being refused by the gate; one that cannot have a slot
// in time, or whose blob stream fails in transit, is answered 503 Kontra-Node: full, which the gate retries, never
// the final 400 kept for a ref the gate refuses (TestHostingInlinesBlobs).
func TestHostingBlobFetchWaitsForASlot(t *testing.T) {
	ref, _ := blob.NewRef()
	broken, _ := blob.NewRef()
	obj := bytes.Repeat([]byte{0xbe, 0xef, 's'}, 300<<10/3)
	bg := &failingBlobs{blobGate: blobGate{objs: map[blob.Ref][]byte{ref: obj}}, broken: map[blob.Ref]bool{broken: true}}
	release := make(chan struct{})
	held := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Blobs: bg, Hire: held} })
	target := &recordingTarget{}
	tn := mustNode(t, g, testConfig(t, g.url, "", mustServe(t, "a2a:hello="+serveTarget(t, target))))
	tn.n.slotWait = time.Second
	tn.run(t)
	tn.welcomed(t)
	s := g.session(t)
	port := tn.localPort(t)

	// 32 hiring requests held open by the gate: every node-opened stream of the session is taken.
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	defer client.CloseIdleConnections()
	for range tunnel.GateMaxInflight {
		wg.Go(func() {
			if res, err := client.Get("http://127.0.0.1:" + port + "/agents/x"); err == nil {
				res.Body.Close()
			}
		})
	}
	eventually(t, 10*time.Second, "32 hiring streams in flight", func() bool { return s.NodeInFlight() == tunnel.GateMaxInflight })
	accepted := s.NodeStreamsAccepted()

	// No slot within the wait: 503 full, no stream opened, nothing reaches the agent.
	res := sendHosted(t, s, sendMessageWith(ref))
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeFull) {
		t.Fatalf("a blob fetch that found no slot: %d Kontra-Node %q, want 503 full (retried by the gate)", res.StatusCode,
			res.Header.Get(tunnel.HeaderNode))
	}
	if got := s.NodeStreamsAccepted(); got != accepted {
		t.Fatalf("the node opened %d streams over the gate's cap", got-accepted)
	}

	// A slot frees within the wait: the fetch goes on, and the agent receives the bytes. The gate counts a stream
	// down only once its own goroutine has read the node's FIN, so right after 32 streams end together it may still
	// count one the node has seen end, and end the blob stream unanswered (§14): that exchange is 503 full, which the
	// relay makes again, never a final 400.
	type result struct {
		res *http.Response
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := hostedExchange(s, sendMessageWith(ref))
		done <- result{res, err}
	}()
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	res = r.res
	if res.StatusCode == http.StatusServiceUnavailable && res.Header.Get(tunnel.HeaderNode) == string(tunnel.NodeFull) {
		t.Log("the gate still counted a stream the node had seen end: 503 full, made again as the relay does")
		res = sendHosted(t, s, sendMessageWith(ref))
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a blob fetch that waited for a slot: %d %q", res.StatusCode, res.Header.Get(tunnel.HeaderNode))
	}
	target.mu.Lock()
	got := target.bodies
	target.mu.Unlock()
	var req struct {
		Params a2a.SendMessageRequest `json:"params"`
	}
	if len(got) != 1 || json.Unmarshal(got[0], &req) != nil || !bytes.Equal(req.Params.Message.Parts[0].Raw(), obj) {
		t.Fatalf("the agent received %d requests, want one with the object's bytes", len(got))
	}

	// A blob stream the gate ends unanswered (its own failure): 503 full, not a final 400.
	res = sendHosted(t, s, sendMessageWith(broken))
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeFull) {
		t.Fatalf("a blob stream that failed in transit: %d Kontra-Node %q, want 503 full", res.StatusCode, res.Header.Get(tunnel.HeaderNode))
	}
	if !tn.running() || strings.Contains(g.logs.String(), "the node keeps more refused streams open") {
		t.Fatal("the session was lost")
	}
}

// setNextStreamID moves a yamux session's next stream id (yamux v0.1.2 keeps it unexported): ids run out after
// about 2³¹ opens, which a test cannot make.
func setNextStreamID(t *testing.T, mux *yamux.Session, id uint32) {
	t.Helper()
	f := reflect.ValueOf(mux).Elem().FieldByName("nextStreamID")
	if !f.IsValid() || f.Kind() != reflect.Uint32 {
		t.Fatal("yamux.Session has no nextStreamID: the yamux version changed")
	}
	atomic.StoreUint32((*uint32)(unsafe.Pointer(f.UnsafeAddr())), id)
}

// currentMux is the yamux session of the node's current session, and its welcome's session id.
func (n *Node) currentMux() (*yamux.Session, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.current == nil {
		return nil, ""
	}
	return n.current.mux, n.current.id
}

// The node's own stream ids (spec/tunnel.md §14: on ErrStreamsExhausted the node reconnects): an open that takes
// one of the last ids retires the session and dials a new one, and so does an open refused ErrStreamsExhausted
// (that request is 503); hiring goes on on the new session.
func TestNodeRedialsWhenItsStreamIDsRunOut(t *testing.T) {
	router := &hireRouter{}
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Hire: router} })
	tn := mustNode(t, g, testConfig(t, g.url, "")).run(t)
	tn.welcomed(t)
	s1 := g.session(t)
	port := tn.localPort(t)
	hiresOn := func(want string) func() bool {
		return func() bool {
			res, body := localGet(t, port, "/agents/hello", nil)
			var seen hireSeen
			return res.StatusCode == http.StatusOK && json.Unmarshal(body, &seen) == nil && seen.Session.String() == want
		}
	}
	current := func(want string) *yamux.Session {
		var mux *yamux.Session
		eventually(t, 10*time.Second, "the node's current session "+want, func() bool {
			m, id := tn.n.currentMux()
			mux = m
			return id == want
		})
		return mux
	}

	// One of the last ids: the open succeeds, and the node dials a new session at once.
	setNextStreamID(t, current(s1.ID.String()), math.MaxUint32-2)
	if !hiresOn(s1.ID.String())() {
		t.Fatal("the open with one of the last stream ids failed")
	}
	s2 := g.session(t)
	eventually(t, 10*time.Second, "hiring on the new session", hiresOn(s2.ID.String()))

	// Exhausted: the open fails, that request is 503, and the node dials a new session.
	setNextStreamID(t, current(s2.ID.String()), math.MaxUint32-1)
	if res, _ := localGet(t, port, "/agents/hello", nil); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an open refused ErrStreamsExhausted: %d, want 503", res.StatusCode)
	}
	s3 := g.session(t)
	eventually(t, 10*time.Second, "hiring on the third session", hiresOn(s3.ID.String()))
	if !strings.Contains(tn.logs.String(), "stream ids are nearly exhausted") {
		t.Fatal("the node did not say why it dialed a new session")
	}
}

// auth_revoked (spec/tunnel.md §12, §13): the node logs it, drops the revoked API key from its credentials and from
// every later hello, and goes on hosting. A reconnect (a network blip, a drain) then carries no auth: with it, the
// gate would refuse the hello auth_invalid, which §4 makes final, and kontra connect would stop hosting.
func TestNodeAuthRevokedDropsTheAPIKey(t *testing.T) {
	const apiKey = "kontra_test_revoked_0123456789"
	g := newTestGate(t, nil)
	cfg := testConfig(t, g.url, "")
	cfg.APIKey = apiKey
	tn := mustNode(t, g, cfg).run(t)
	tn.welcomed(t)
	if r := g.adm.next(t); r.Hello.Auth != apiKey {
		t.Fatalf("the first hello's auth %q", r.Hello.Auth)
	}
	s := g.session(t)
	// The gate refuses the revoked key from now on, as enroll.Admit does.
	g.adm.set(func(_ int, req enroll.Request) (*enroll.Decision, error) {
		if req.Hello.Auth != "" {
			return nil, tunnel.Refuse(tunnel.CodeAuthInvalid, "the auth API key is unknown, revoked or expired")
		}
		return admitAll(req), nil
	})
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlAuthRevoked}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the auth_revoked line", func() bool {
		return strings.Contains(tn.logs.String(), "the gate revoked the session's hiring API key")
	})
	eventually(t, 5*time.Second, "the API key dropped from the credentials file", func() bool { return tn.creds(t).APIKey == "" })
	for _, w := range g.wires.All() {
		_ = w.Cut()
	}
	if r := g.adm.next(t); r.Hello.Auth != "" {
		t.Fatalf("the hello after auth_revoked carries the revoked API key")
	}
	g.session(t)
	if !tn.running() {
		t.Fatal("kontra connect stopped after auth_revoked and a reconnect")
	}
}
