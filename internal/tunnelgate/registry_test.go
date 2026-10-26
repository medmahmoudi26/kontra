package tunnelgate

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// pairNode is the node's side of an in-process session: the preambles of the exchange streams the gate
// opens, and the lines of its control stream.
type pairNode struct {
	preambles chan tunnel.Preamble
	controls  chan tunnel.Control
}

// muxSession is a Session over an in-process yamux pair (no registry, no database) serving the given a2a
// services, with its control stream open (the Handler's job). The node answers each exchange stream with
// one line and keeps it open.
func muxSession(t *testing.T, services ...string) (*Session, *pairNode) {
	t.Helper()
	node, gate := tunneltest.NewPair(t).Sessions(t, nil)
	s := &Session{ID: uuid.Must(uuid.NewV7()), KeyID: "k1_" + strings.Repeat("a", 26), TenantID: testTenantID,
		mux: gate, served: map[string]string{}}
	for _, name := range services {
		s.served[name] = tunnel.KindA2A
	}
	n := &pairNode{preambles: make(chan tunnel.Preamble, 16), controls: make(chan tunnel.Control, 16)}
	go func() {
		for {
			st, err := node.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(st)
				p, err := tunnel.ReadPreamble(br)
				if err != nil {
					return
				}
				if p.Op == tunnel.OpControl {
					for {
						c, err := tunnel.ReadControl(br)
						if err != nil {
							return
						}
						n.controls <- c
					}
				}
				n.preambles <- p
				_, _ = io.WriteString(st, "hello from "+p.Service+"\n")
			}()
		}
	}()
	c, err := openControl(gate)
	if err != nil {
		t.Fatal(err)
	}
	s.control = c
	return s, n
}

// hold puts s in r as Add does, without the database.
func hold(r *Registry, s *Session) {
	s.mu.Lock()
	s.reg, s.registered = r, true
	s.mu.Unlock()
	r.mu.Lock()
	r.sessions[s.ID] = s
	r.mu.Unlock()
}

func (n *pairNode) control(t *testing.T) tunnel.Control {
	t.Helper()
	select {
	case c := <-n.controls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no control line")
		return tunnel.Control{}
	}
}

func (n *pairNode) noControl(t *testing.T) {
	t.Helper()
	select {
	case c := <-n.controls:
		t.Fatalf("unexpected control line %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

// Every gate-opened stream starts with its preamble (§8.1); a binding revocation aborts the streams of its
// service at once (reads fail ErrSessionGone, buffered data or not) and refuses new ones, the other
// service's streams going on; Close aborts all of them and refuses every new stream.
func TestExchangesAreAborted(t *testing.T) {
	s, n := muxSession(t, "hello", "recon")
	ctx := context.Background()
	open := func(service string) *bufio.Reader {
		t.Helper()
		c, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindA2A, Task: "t-1", Context: "c-1"})
		if err != nil {
			t.Fatalf("OpenExchange(%s): %v", service, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if p := <-n.preambles; p != (tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindA2A, Task: "t-1", Context: "c-1"}) {
			t.Fatalf("preamble %+v", p)
		}
		return bufio.NewReader(c)
	}
	hello := open("hello")
	recon := open("recon")
	time.Sleep(50 * time.Millisecond) // both answers are buffered on the gate's side
	if s.InFlight() != 2 {
		t.Fatalf("%d streams in flight", s.InFlight())
	}

	if !s.dropService("hello", errors.New("binding revoked")) {
		t.Fatal("dropService: hello was not served")
	}
	if _, err := hello.ReadString('\n'); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a read on an aborted stream: %v", err)
	}
	if _, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a new stream for the revoked service: %v", err)
	}
	if line, err := recon.ReadString('\n'); err != nil || line != "hello from recon\n" {
		t.Fatalf("the other service's stream: %q, %v", line, err)
	}
	if err := s.Allows("recon", tunnel.KindA2A); err != nil {
		t.Fatal(err)
	}
	if s.InFlight() != 1 {
		t.Fatalf("%d streams in flight after the abort", s.InFlight())
	}

	if err := s.Close("test over"); err != nil {
		t.Fatal(err)
	}
	if _, err := recon.ReadString('\n'); !errors.Is(err, ErrSessionGone) || !strings.Contains(err.Error(), "test over") {
		t.Fatalf("a read after Close: %v", err)
	}
	if _, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: "recon", Kind: tunnel.KindA2A}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a stream on a closed session: %v", err)
	}
	if !s.mux.IsClosed() {
		t.Fatal("Close left the multiplexer open")
	}
}

// OpenExchange refuses what is not one exchange (a programming error, not a lost session), and, as a lost
// session, a service of another kind or one the session does not serve.
func TestOpenExchangeRefusals(t *testing.T) {
	s, _ := muxSession(t, "hello")
	ctx := context.Background()
	if _, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpControl}); err == nil || errors.Is(err, ErrSessionGone) {
		t.Fatalf("a control preamble: %v", err)
	}
	if _, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindMCP}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("another kind: %v", err)
	}
	if _, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: "other", Kind: tunnel.KindA2A}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a service the session does not serve: %v", err)
	}
	if err := (&Session{}).Allows("hello", tunnel.KindA2A); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("an empty session: %v", err)
	}
}

// The gate-internal NOTIFY payloads (pg.ChannelTunnel): a superseded session held here is closed; an
// enrollment is announced to this replica's sessions of the tenant serving the service, unless this
// replica announced it itself or the session is the enrolled key's; anything malformed is ignored.
func TestNotifyPayloads(t *testing.T) {
	r := NewRegistry(RegistryConfig{ReplicaID: "r-test"})
	held, n := muxSession(t, "hello")
	hold(r, held)

	b := enroll.Binding{KeyID: "k1_" + strings.Repeat("b", 26), TenantID: testTenantID, Service: "hello"}
	if got := EnrolledPayload(b); got != "enrolled:"+b.KeyID+"/"+testTenantID.String()+"/hello" {
		t.Fatalf("EnrolledPayload = %s", got)
	}
	for _, p := range []string{"", "session:", "session:nope", "enrolled:x/y/z", "enrolled:" + b.KeyID + "/" + testTenantID.String(),
		"enrolled:" + b.KeyID + "/nope/hello", "key:" + b.KeyID, "other"} {
		r.Notify(p)
	}
	n.noControl(t)
	// Another replica's enrollment: told.
	r.Notify(EnrolledPayload(b))
	if c := n.control(t); c != (tunnel.Control{T: tunnel.CtlEnrolled, Service: "hello", Key: b.KeyID}) {
		t.Fatalf("control %+v", c)
	}
	// This replica's own (remembered before its commit), another tenant's, the session's own key's, and a
	// service it does not serve: not told.
	own := enroll.Binding{KeyID: "k1_" + strings.Repeat("c", 26), TenantID: testTenantID, Service: "hello"}
	r.remember(EnrolledPayload(own))
	r.Notify(EnrolledPayload(own))
	r.Notify(EnrolledPayload(enroll.Binding{KeyID: b.KeyID, TenantID: uuid.Must(uuid.NewV7()), Service: "hello"}))
	r.Notify(EnrolledPayload(enroll.Binding{KeyID: held.KeyID, TenantID: testTenantID, Service: "hello"}))
	r.Notify(EnrolledPayload(enroll.Binding{KeyID: b.KeyID, TenantID: testTenantID, Service: "recon"}))
	n.noControl(t)
	// The memo is used once: the same payload again is another enrollment.
	r.Notify(EnrolledPayload(own))
	if c := n.control(t); c.Key != own.KeyID {
		t.Fatalf("control %+v", c)
	}

	if got := SessionPayload(held.ID); got != "session:"+held.ID.String() {
		t.Fatalf("SessionPayload = %s", got)
	}
	r.Notify(SessionPayload(uuid.Must(uuid.NewV7()))) // not held here
	time.Sleep(50 * time.Millisecond)
	if held.mux.IsClosed() {
		t.Fatal("a payload for another session closed this one")
	}
	r.Notify(SessionPayload(held.ID))
	eventually(t, 5*time.Second, "the superseded session closes", held.mux.IsClosed)
	if err := held.Allows("hello", tunnel.KindA2A); !errors.Is(err, ErrSessionGone) || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("Allows after a supersede: %v", err)
	}
	eventually(t, 5*time.Second, "the registry forgets the superseded session", func() bool { return r.Count() == 0 })
}

// Revoked: a binding drops the service and tells the node {"t":"revoked","service"}; a key closes its
// sessions after {"t":"revoked"}, except a rotated key whose overlap ended (no message); another key's
// revocation touches nothing.
func TestRevokedWithoutDatabase(t *testing.T) {
	r := NewRegistry(RegistryConfig{ReplicaID: "r-test"})
	add := func(services ...string) (*Session, *pairNode) {
		s, n := muxSession(t, services...)
		hold(r, s)
		return s, n
	}
	s1, n1 := add("hello", "recon")
	r.Revoked(enroll.Revocation{KeyID: "k1_" + strings.Repeat("z", 26)})
	n1.noControl(t)

	r.Revoked(enroll.Revocation{KeyID: s1.KeyID, Service: "hello"})
	if c := n1.control(t); c != (tunnel.Control{T: tunnel.CtlRevoked, Service: "hello"}) {
		t.Fatalf("control %+v", c)
	}
	if s1.Serves("hello") || !s1.Serves("recon") || s1.mux.IsClosed() {
		t.Fatalf("after the binding revocation: served %v, closed %v", s1.Served(), s1.mux.IsClosed())
	}
	r.Revoked(enroll.Revocation{KeyID: s1.KeyID, Service: "hello"}) // not served any more: nothing
	n1.noControl(t)

	r.Revoked(enroll.Revocation{KeyID: s1.KeyID, Reason: enroll.ReasonOperator})
	if c := n1.control(t); c != (tunnel.Control{T: tunnel.CtlRevoked}) {
		t.Fatalf("control %+v", c)
	}
	eventually(t, 5*time.Second, "the revoked key's session closes", s1.mux.IsClosed)

	s2, n2 := add("hello")
	r.Revoked(enroll.Revocation{KeyID: s2.KeyID, Reason: enroll.ReasonRotated})
	eventually(t, 5*time.Second, "the rotated key's session closes", s2.mux.IsClosed)
	n2.noControl(t)
}

// Drain tells every session once, CloseAll closes them all and refuses later registrations.
func TestDrainWithoutDatabase(t *testing.T) {
	r := NewRegistry(RegistryConfig{ReplicaID: "r-test"})
	var nodes []*pairNode
	var sessions []*Session
	for range 2 {
		s, n := muxSession(t, "hello")
		hold(r, s)
		nodes, sessions = append(nodes, n), append(sessions, s)
	}
	r.Drain(context.Background())
	for _, n := range nodes {
		if c := n.control(t); c.T != tunnel.CtlDrain {
			t.Fatalf("control %+v", c)
		}
	}
	r.Drain(context.Background())
	for _, n := range nodes {
		n.noControl(t)
	}
	r.CloseAll(context.Background())
	for _, s := range sessions {
		if !s.mux.IsClosed() {
			t.Fatal("CloseAll left a session open")
		}
	}
	_, gm := tunneltest.NewPair(t).Sessions(t, nil)
	if err := r.Add(context.Background(), &Session{ID: uuid.Must(uuid.NewV7())}, gm, nil); !errors.Is(err, errClosing) {
		t.Fatalf("Add after CloseAll: %v", err)
	}
}
