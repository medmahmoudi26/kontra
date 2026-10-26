package gate

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
)

// tunnelNode is a node dialed into a replica's /v1/tunnel through its public listener: node.Dial and
// node.Handshake, then yamux. It reads the control stream into controls, and answers every other stream
// the gate opens with one HTTP/1.1 response: card at /.well-known/agent-card.json, 404 otherwise.
type tunnelNode struct {
	id       *nodekey.Identity
	mux      *yamux.Session
	session  uuid.UUID
	welcome  *tunnel.Welcome
	controls chan tunnel.Control
	card     string
	cardGets atomic.Int64
	tasks    atomic.Int64 // preambles that named a task
}

func newNodeIdentity(t *testing.T) *nodekey.Identity {
	t.Helper()
	id, _, err := nodekey.LoadOrCreate(filepath.Join(t.TempDir(), ".kontra", "identity.key"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// dialNode connects id to the gate at base, declaring the a2a services, with the given enrollment tokens.
func dialNode(t *testing.T, base string, id *nodekey.Identity, card string, tokens []string, services ...string) (*tunnelNode, error) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := node.Dialer{UserAgent: node.UserAgent("test")}.Dial(ctx, u)
	if err != nil {
		return nil, err
	}
	h := &tunnel.Hello{Name: "test-node", Agent: node.UserAgent("test"), Enroll: tokens}
	for _, s := range services {
		h.Services = append(h.Services, tunnel.Service{Kind: tunnel.KindA2A, Name: s})
	}
	w, err := node.Handshake(ctx, d, id, h, false)
	if err != nil {
		_ = d.WS.CloseNow()
		return nil, err
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &tunnelNode{id: id, mux: m, session: uuid.MustParse(w.Session), welcome: w, controls: make(chan tunnel.Control, 64), card: card}
	t.Cleanup(func() { _ = m.Close() })
	go n.accept()
	return n, nil
}

func (n *tunnelNode) accept() {
	first := true
	for {
		st, err := n.mux.AcceptStream()
		if err != nil {
			return
		}
		br := bufio.NewReader(st)
		if first {
			first = false
			go func() {
				if p, err := tunnel.ReadPreamble(br); err != nil || p.Op != tunnel.OpControl {
					_ = st.Close()
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
			continue
		}
		go func() {
			defer st.Close()
			p, err := tunnel.ReadPreamble(br)
			if err != nil {
				return
			}
			if p.Task != "" {
				n.tasks.Add(1)
			}
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			resp := &http.Response{StatusCode: http.StatusNotFound, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Close: true,
				Body: http.NoBody}
			if req.URL.Path == "/.well-known/agent-card.json" {
				n.cardGets.Add(1)
				resp.StatusCode = http.StatusOK
				resp.Header.Set("Content-Type", "application/json")
				resp.Body = io.NopCloser(strings.NewReader(n.card))
				resp.ContentLength = int64(len(n.card))
			}
			_ = resp.Write(st)
		}()
	}
}

func (n *tunnelNode) nextControl(t *testing.T, within time.Duration) tunnel.Control {
	t.Helper()
	select {
	case c := <-n.controls:
		return c
	case <-time.After(within):
		t.Fatalf("no control line within %v", within)
		return tunnel.Control{}
	}
}

func (n *tunnelNode) closed(within time.Duration) bool {
	select {
	case <-n.mux.CloseChan():
		return true
	case <-time.After(within):
		return false
	}
}
