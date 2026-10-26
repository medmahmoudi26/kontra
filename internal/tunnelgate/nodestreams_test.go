package tunnelgate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/tunnel"
)

// whoAmI is a stand-in for the gate's router: it authenticates the request as the edge's routes do
// (auth.Authenticator, which takes the session's principal from the hire source) and answers who it is.
type whoAmI struct {
	mu      sync.Mutex
	calls   int
	release chan struct{} // when set, every request waits for it
	entered chan struct{}
}

type whoAnswer struct {
	Principal string    `json:"principal"`
	Via       string    `json:"via"`
	Buyer     uuid.UUID `json:"buyer"`
	Tenant    string    `json:"tenant"`
	KeepRefs  bool      `json:"keep_refs"`
	Session   bool      `json:"session"`
	AuthzSeen bool      `json:"authz_seen"`
	Remote    string    `json:"remote"`
}

func (h *whoAmI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.calls++
	release, entered := h.release, h.entered
	h.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	p, _, err := auth.NewAuthenticator(nil).Authenticate(r, uuid.Nil)
	if errors.Is(err, auth.ErrUnauthenticated) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, hs := HiringSession(r.Context())
	_ = json.NewEncoder(w).Encode(whoAnswer{Principal: p.ID, Via: string(p.Via), Buyer: p.BuyerID, Tenant: p.TenantSlug,
		KeepRefs: blob.KeepRefs(r.Context()), Session: hs, AuthzSeen: r.Header.Get("Authorization") != "", Remote: r.RemoteAddr})
}

func (h *whoAmI) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// fakeKeys is a HireKeys whose answer the test sets.
type fakeKeys struct {
	mu       sync.Mutex
	err      error
	checks   int
	holds    int
	releases int
}

func (k *fakeKeys) Hold(auth.Principal) func() {
	k.mu.Lock()
	k.holds++
	k.mu.Unlock()
	return func() { k.mu.Lock(); k.releases++; k.mu.Unlock() }
}

func (k *fakeKeys) Revalidate(_ context.Context, p auth.Principal) (auth.Principal, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.checks++
	if k.err != nil {
		return auth.Principal{}, k.err
	}
	return p, nil
}

func (k *fakeKeys) counts() (holds, releases, checks int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.holds, k.releases, k.checks
}

// hiringNode is a node's yamux.Client over a completed handshake with the gate's control stream read into
// controls, and the session the gate registered.
type hiringNode struct {
	m        *yamux.Session
	s        *Session
	controls chan tunnel.Control
}

func dialHiring(t *testing.T, g *testGate) *hiringNode {
	t.Helper()
	d, _, err := g.connect(t, newIdentity(t), newHello("hello"), false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	ctl, err := m.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	n := &hiringNode{m: m, s: g.sink.next(t), controls: make(chan tunnel.Control, 16)}
	go func() {
		br := bufio.NewReader(ctl)
		if p, err := tunnel.ReadPreamble(br); err != nil || p.Op != tunnel.OpControl {
			return
		}
		for {
			c, err := tunnel.ReadControl(br)
			if err != nil {
				return
			}
			n.controls <- c
		}
	}()
	return n
}

// open opens a node-opened stream with preamble p and writes raw after it; it does not close it.
func (n *hiringNode) open(t *testing.T, p tunnel.Preamble, raw string) *yamux.Stream {
	t.Helper()
	st, err := n.m.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WritePreamble(st, p); err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		if _, err := io.WriteString(st, raw); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// do runs one hiring exchange and returns the response, its body read, the stream closed.
func (n *hiringNode) do(t *testing.T, raw string) (*http.Response, []byte) {
	t.Helper()
	st := n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, raw)
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		t.Fatalf("hiring exchange: %v", err)
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("hiring exchange body: %v", err)
	}
	return res, b
}

func get(path string, headers ...string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: x\r\n" + strings.Join(headers, "") + "\r\n"
}

// A hiring request is served by the gate's router with the session's principal (spec/tunnel.md §9, relay.md
// §2.3): node:<key_id> with ids.NodeBuyer and the key's tenant, whatever Authorization the node forwards (never
// read, removed); its answers keep kontra-blob: refs; the edge sees one client address per session. A path outside
// the four prefixes is 404 and never reaches the router.
func TestHiringNodePrincipal(t *testing.T) {
	who := &whoAmI{}
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.NodeStreams = &NodeStreams{Hire: who} }})
	n := dialHiring(t, g)
	kid := n.s.KeyID
	res, body := n.do(t, get("/agents/hello", "Authorization: Bearer kk_someone_else\r\n"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("hiring request: %d %s", res.StatusCode, body)
	}
	var a whoAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	buyer, _ := ids.NodeBuyer(ids.Namespace, kid)
	if a.Principal != "node:"+kid || a.Via != "node" || a.Buyer != buyer || a.Tenant != "team" || !a.KeepRefs || !a.Session || a.AuthzSeen {
		t.Fatalf("the router saw %+v; want node:%s, buyer %s, tenant team, keep refs, the session, no Authorization", a, kid, buyer)
	}
	if !strings.HasPrefix(a.Remote, "[fd") || a.Remote != n.s.hireAddr().String() {
		t.Fatalf("the edge's client address %q, want the session's own %s", a.Remote, n.s.hireAddr())
	}
	for _, path := range []string{"/agents/hello/tasks", "/mcp", "/mcp/fs", "/.well-known/jwks.json"} {
		if res, _ := n.do(t, get(path)); res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	before := who.count()
	for _, path := range []string{"/v1/tunnel", "/healthz", "/", "/mcpx", "/agentsx/hello", "/metrics"} {
		if res, _ := n.do(t, get(path)); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, res.StatusCode)
		}
	}
	if who.count() != before {
		t.Fatal("a path outside the four prefixes reached the router")
	}
	if got := n.s.NodeStreamsAccepted(); got != 11 {
		t.Errorf("%d node-opened streams accepted, want 11", got)
	}
	eventually(t, 5*time.Second, "every stream ended on both sides", func() bool { return n.s.NodeInFlight() == 0 })
	// An unreadable preamble and a control stream from the node are 400.
	st, err := n.m.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(st, "not a preamble\n")
	if res, err := http.ReadResponse(bufio.NewReader(st), nil); err != nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad preamble: %v %v", res, err)
	}
	_ = st.Close()
	if res, _ := n.do2(t, tunnel.Preamble{Op: tunnel.OpControl}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("op control from the node: %d", res.StatusCode)
	}
}

func (n *hiringNode) do2(t *testing.T, p tunnel.Preamble) (*http.Response, []byte) {
	t.Helper()
	st := n.open(t, p, "")
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// A session that hires with its handshake's API key takes the key's principal, re-validated on every request and
// held under the revocation cache's watch for the session's life. Once the key no longer authenticates, every
// hiring request is 401 without asking again, and the node is told {"t": "auth_revoked"} once (spec/tunnel.md
// §12); APIKeyRevoked (NOTIFY kontra_revoke 'apikey:<id>') does the same for a key found revoked elsewhere.
func TestHiringAPIKeyPrincipal(t *testing.T) {
	who := &whoAmI{}
	keys := &fakeKeys{}
	keyID := uuid.Must(uuid.NewV7())
	hire := auth.Principal{Kind: auth.KindKey, ID: "key:kk_hire_abcdefgh", TenantID: &testTenantID, TenantSlug: "team",
		BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaAPIKey, Scopes: []auth.Scope{auth.ScopeHire}, KeyID: &keyID}
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.NodeStreams = &NodeStreams{Hire: who, Keys: keys} }})
	g.adm.set(func(req enroll.Request) (*enroll.Decision, error) {
		d := admitAll(req)
		d.HirePrincipal, d.Hire = hire.ID, &hire
		return d, nil
	})
	n := dialHiring(t, g)
	if h, _, _ := keys.counts(); h != 1 {
		t.Fatalf("the hiring key was held %d times, want once for the session", h)
	}
	res, body := n.do(t, get("/agents/hello"))
	var a whoAnswer
	_ = json.Unmarshal(body, &a)
	if res.StatusCode != http.StatusOK || a.Principal != hire.ID || a.Buyer != hire.BuyerID || a.Via != "api_key" {
		t.Fatalf("hiring with the API key: %d %+v", res.StatusCode, a)
	}
	keys.mu.Lock()
	keys.err = auth.ErrUnauthenticated
	keys.mu.Unlock()
	if res, _ := n.do(t, get("/agents/hello")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the revocation: %d, want 401", res.StatusCode)
	}
	select {
	case c := <-n.controls:
		if c.T != tunnel.CtlAuthRevoked {
			t.Fatalf("control %+v, want auth_revoked", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node was not told auth_revoked")
	}
	_, _, checks := keys.counts()
	for range 2 {
		if res, _ := n.do(t, get("/agents/hello")); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a later request: %d", res.StatusCode)
		}
	}
	if _, _, c := keys.counts(); c != checks {
		t.Errorf("the revoked key was checked again (%d times): it stays revoked for the session", c-checks)
	}
	if !n.s.HireRevoked() || n.s.Mux().IsClosed() {
		t.Fatal("the session must stay open, hosting on it unaffected, with its hiring revoked")
	}
	n.s.authRevoked(g.h.log) // a second report of the same revocation tells nothing more
	select {
	case c := <-n.controls:
		t.Fatalf("a second control line %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
	_ = n.m.Close()
	eventually(t, 10*time.Second, "the hold released when the session ended", func() bool {
		_, r, _ := keys.counts()
		return r == 1
	})
}

// Registry.APIKeyRevoked reaches only the sessions hiring with that key.
func TestRegistryAPIKeyRevoked(t *testing.T) {
	r := NewRegistry(RegistryConfig{})
	k1, k2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	mk := func(k *uuid.UUID) *Session {
		s := &Session{ID: uuid.Must(uuid.NewV7()), KeyID: "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa", reg: r}
		if k != nil {
			s.hire = &auth.Principal{Kind: auth.KindKey, KeyID: k}
		}
		r.sessions[s.ID] = s
		return s
	}
	a, b, c := mk(&k1), mk(&k2), mk(nil)
	r.APIKeyRevoked(k1)
	r.async.Wait()
	if !a.HireRevoked() || b.HireRevoked() || c.HireRevoked() {
		t.Fatalf("revoked: %v %v %v, want only the first", a.HireRevoked(), b.HireRevoked(), c.HireRevoked())
	}
}

// The in-flight cap (§14, D137, D182): 33 concurrent hiring streams give one 429 and keep the session; a node
// that keeps more than 32 refused streams open loses it.
func TestNodeOpenedStreamsCap(t *testing.T) {
	who := &whoAmI{release: make(chan struct{}), entered: make(chan struct{}, 64)}
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.NodeStreams = &NodeStreams{Hire: who} }})
	n := dialHiring(t, g)
	var held []*yamux.Stream
	for range tunnel.GateMaxInflight {
		held = append(held, n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello")))
	}
	for i := range tunnel.GateMaxInflight {
		select {
		case <-who.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d requests reached the router", i, tunnel.GateMaxInflight)
		}
	}
	over := n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello"))
	_ = over.SetReadDeadline(time.Now().Add(10 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(over), nil)
	if err != nil || res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("the 33rd stream: %v %v, want 429 with Retry-After", res, err)
	}
	if who.count() != tunnel.GateMaxInflight || n.s.Mux().IsClosed() {
		t.Fatalf("%d requests reached the router (want %d), session closed %v", who.count(), tunnel.GateMaxInflight, n.s.Mux().IsClosed())
	}
	// The node keeps that refused stream open, and 31 more: still within the cap of refused streams.
	refused := []*yamux.Stream{over}
	for range tunnel.GateMaxInflight - 1 {
		refused = append(refused, n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello")))
	}
	eventually(t, 10*time.Second, "64 node-opened streams accepted", func() bool { return n.s.NodeStreamsAccepted() == 2*tunnel.GateMaxInflight })
	time.Sleep(100 * time.Millisecond)
	if n.s.Mux().IsClosed() {
		t.Fatal("the session closed with 32 refused streams open")
	}
	// One more refused stream kept open: the session is closed.
	_, _ = n.m.OpenStream()
	eventually(t, 10*time.Second, "the gate closed the session", n.s.Mux().IsClosed)
	eventually(t, 10*time.Second, "the node's session ended", n.m.IsClosed)
	if !strings.Contains(g.logs.String(), "more refused streams open than the gate allows") {
		t.Fatal("the gate did not log why it closed the session")
	}
	close(who.release)
	_ = held
}

// A stream counts until both halves end: three rounds of 32 completed exchanges, each held open by the node until
// the round ends, keep the session open, and the gate's yamux forgets every stream once the node closes them.
func TestNodeOpenedStreamsFreedWhenTheNodeCloses(t *testing.T) {
	who := &whoAmI{}
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.NodeStreams = &NodeStreams{Hire: who} }})
	n := dialHiring(t, g)
	for round := range 3 {
		var held []*yamux.Stream
		for range tunnel.GateMaxInflight {
			st := n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello"))
			_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(st)
			res, err := http.ReadResponse(br, nil)
			if err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("round %d: %v %v", round, res, err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			if n, err := br.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("round %d: read %d, %v after the answer; want the gate's close (EOF)", round, n, err)
			}
			held = append(held, st)
		}
		if n.s.Mux().IsClosed() {
			t.Fatalf("round %d: the session closed within the cap", round)
		}
		if got := n.s.NodeInFlight(); got != tunnel.GateMaxInflight {
			t.Fatalf("round %d: %d in flight while the node holds its halves, want %d", round, got, tunnel.GateMaxInflight)
		}
		for _, st := range held {
			_ = st.Close()
		}
		eventually(t, 10*time.Second, "every stream of the round ended", func() bool { return n.s.NodeInFlight() == 0 })
		eventually(t, 10*time.Second, "the gate's yamux forgot the round's streams", func() bool { return n.s.Mux().NumStreams() == 1 })
	}
	if n.s.Mux().IsClosed() || n.m.IsClosed() {
		t.Fatal("the session closed although the node closed every stream")
	}
}

// fakeStore is a blob.Store over memory whose Open may report a size without data.
type fakeStore struct {
	objs  map[blob.Key][]byte
	sizes map[blob.Key]int64 // reported instead of the object's
}

func (f *fakeStore) Put(context.Context, blob.Key, io.Reader, int64) error {
	return errors.New("read only")
}
func (f *fakeStore) Open(_ context.Context, k blob.Key) (io.ReadCloser, int64, error) {
	if n, ok := f.sizes[k]; ok {
		return io.NopCloser(strings.NewReader("")), n, nil
	}
	b, ok := f.objs[k]
	if !ok {
		return nil, 0, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}
func (f *fakeStore) Presign(context.Context, blob.Key, time.Duration) (string, error) {
	return "", errors.New("never for a node")
}
func (f *fakeStore) Delete(context.Context, blob.Key) error                 { return nil }
func (f *fakeStore) DeleteTask(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// Blob streams (spec/tunnel.md §8.3, D181) through the session, fetched with blob.Fetch as a node does: a task
// served on the session and a task its hiring principal's buyer owns are served; an unknown ref is not_found; a
// task of another tenant that the session neither serves nor hires is forbidden; a row or a store object over 64
// MiB is too_large (no 64 MiB write); without a bucket every ref is not_found.
func TestBlobStreams(t *testing.T) {
	type entry struct {
		row  BlobRow
		data []byte
	}
	refs := map[string]blob.Ref{}
	for _, name := range []string{"served", "hired", "other", "bigrow", "bigobj", "missing", "unknown"} {
		r, err := blob.NewRef()
		if err != nil {
			t.Fatal(err)
		}
		refs[name] = r
	}
	other := uuid.Must(uuid.NewV7())
	store := &fakeStore{objs: map[blob.Key][]byte{}, sizes: map[blob.Key]int64{}}
	rows := map[blob.Ref]*entry{}
	var session uuid.UUID
	var hireBuyer uuid.UUID
	add := func(name string, tenant uuid.UUID, serves bool, buyer uuid.UUID, size int64, data []byte) {
		r := BlobRow{Row: blob.Row{Ref: refs[name], Tenant: tenant, Task: uuid.Must(uuid.NewV7()), Size: size}, Buyer: buyer, Serves: serves}
		rows[refs[name]] = &entry{row: r, data: data}
		if data != nil {
			store.objs[r.Key()] = data
		}
	}
	served := bytes.Repeat([]byte("served "), 300<<10/7)
	hired := []byte("the hiring node's own answer")
	blobs := &Blobs{Store: store, Lookup: func(_ context.Context, ref blob.Ref, s uuid.UUID) (BlobRow, bool, error) {
		e, ok := rows[ref]
		if !ok {
			return BlobRow{}, false, nil
		}
		row := e.row
		row.Serves = row.Serves && s == session
		if row.Buyer == uuid.Nil {
			row.Buyer = hireBuyer
		}
		return row, true, nil
	}}
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.NodeStreams = &NodeStreams{Blobs: blobs} }})
	n := dialHiring(t, g)
	session = n.s.ID
	hireBuyer, _ = ids.NodeBuyer(ids.Namespace, n.s.KeyID)
	add("served", testTenantID, true, uuid.Must(uuid.NewV7()), int64(len(served)), served)
	add("hired", testTenantID, false, uuid.Nil, int64(len(hired)), hired) // the buyer is the session's own
	add("other", other, false, uuid.Must(uuid.NewV7()), 4, []byte("nope"))
	add("bigrow", testTenantID, true, uuid.Nil, blob.MaxObject+1, nil)
	add("bigobj", testTenantID, true, uuid.Nil, 10, nil)
	store.sizes[rows[refs["bigobj"]].row.Key()] = 65 << 20
	add("missing", testTenantID, true, uuid.Nil, 10, nil) // a row without its object

	fetch := func(ref blob.Ref) ([]byte, error) {
		st, err := n.m.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = blob.Fetch(ctx, st, ref, &buf)
		return buf.Bytes(), err
	}
	for name, want := range map[string][]byte{"served": served, "hired": hired} {
		got, err := fetch(refs[name])
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, %v", name, len(got), err)
		}
	}
	for name, reason := range map[string]string{"unknown": blob.ReasonNotFound, "other": blob.ReasonForbidden, "bigrow": blob.ReasonTooLarge,
		"bigobj": blob.ReasonTooLarge, "missing": blob.ReasonNotFound} {
		_, err := fetch(refs[name])
		var fe *blob.FetchError
		if !errors.As(err, &fe) || fe.Reason != reason {
			t.Errorf("%s: %v, want %s", name, err, reason)
		}
	}
	// A session whose hiring API key was revoked hires nothing: a task it does not serve is forbidden.
	n.s.hire = &auth.Principal{Kind: auth.KindKey, BuyerID: hireBuyer, KeyID: &other}
	n.s.hireRevoked.Store(true)
	if _, err := fetch(refs["hired"]); err == nil || !strings.Contains(err.Error(), blob.ReasonForbidden) {
		t.Errorf("a hired task after auth_revoked: %v", err)
	}
	if got, err := fetch(refs["served"]); err != nil || !bytes.Equal(got, served) {
		t.Errorf("a served task after auth_revoked (hosting goes on): %v", err)
	}

	// No bucket: not_found.
	g2 := newTestGate(t, gateOpts{})
	n2 := dialHiring(t, g2)
	st, err := n2.m.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	var fe *blob.FetchError
	if _, err := blob.Fetch(context.Background(), st, refs["served"], io.Discard); !errors.As(err, &fe) || fe.Reason != blob.ReasonNotFound {
		t.Fatalf("a gate without a bucket: %v", err)
	}
	// No router: 404.
	if res, _ := n2.do(t, get("/agents/hello")); res.StatusCode != http.StatusNotFound {
		t.Fatalf("a gate without a hiring router: %d", res.StatusCode)
	}
}

// The writes of a hiring answer have their own deadline: a node that stops reading an endless answer frees the
// handler (D155), and the stream ends.
func TestHiringWriteDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for tunnel.ConnectionWriteTimeout")
	}
	done := make(chan error, 1)
	endless := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				done <- err
				return
			}
			_ = http.NewResponseController(w).Flush()
		}
	})
	g := newTestGate(t, gateOpts{mod: func(c *HandlerConfig) { c.NodeStreams = &NodeStreams{Hire: endless} }})
	n := dialHiring(t, g)
	st := n.open(t, tunnel.Preamble{Op: tunnel.OpHTTP}, get("/agents/hello"))
	defer st.Close()
	select { // the node never reads
	case err := <-done:
		if err == nil {
			t.Fatal("no error")
		}
	case <-time.After(tunnel.ConnectionWriteTimeout + 10*time.Second):
		t.Fatal("the handler is still writing to a node that does not read")
	}
}
