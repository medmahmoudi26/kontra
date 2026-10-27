package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
)

// FakeGate is the gate's side of the tunnel without Postgres or Temporal (sidecar_not_a_proxy): the real
// tunnelgate.Handler on a loopback listener, with an Admitter that admits every proven hello for a fixed set
// of services and a SessionSink that hands each session to the test. The test then opens streams on the
// session itself and writes whatever it likes on them: crafted preambles and raw HTTP/1.1 requests a real
// gate would never send, to check what `kontra connect` does with them (spec/tunnel.md §8.1, §8.2).
type FakeGate struct {
	URL  string // http://127.0.0.1:<port>: the node's --gate
	Addr string

	l        net.Listener
	srv      *http.Server
	sessions chan *tunnelgate.Session
	logFile  *os.File
}

// fixedAdmitter admits every hello whose proof verified, for served (the gate's admission is
// internal/enroll's, which needs Postgres).
type fixedAdmitter struct{ served []tunnel.WelcomeService }

func (a fixedAdmitter) Admit(_ context.Context, req enroll.Request) (*enroll.Decision, error) {
	kid := tunnel.KeyID(req.Hello.Recipient)
	return &enroll.Decision{
		Key:           enroll.Key{KeyID: kid, TenantID: uuid.Nil, Tenant: "default", PubKey: req.PubKey, NodeName: req.Hello.Name},
		Served:        a.served,
		HirePrincipal: "node:" + kid,
	}, nil
}

// sink hands each admitted session to the test.
type sink struct{ ch chan *tunnelgate.Session }

func (s sink) Add(_ context.Context, sess *tunnelgate.Session, _ *yamux.Session, _ []tunnel.WelcomeService) error {
	select {
	case s.ch <- sess:
		return nil
	default:
		return errors.New("e2e: the fake gate takes one session at a time")
	}
}

// StartFakeGate serves GET /v1/tunnel on 127.0.0.1, admitting every node for the services named: a2a services
// by name, MCP services as "mcp:<name>" (M3 WP-05); logPath receives the handler's log.
func StartFakeGate(services []string, logPath string) (*FakeGate, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &FakeGate{Addr: l.Addr().String(), URL: "http://" + l.Addr().String(), l: l, sessions: make(chan *tunnelgate.Session, 4)}
	if f.logFile, err = os.Create(logPath); err != nil {
		l.Close()
		return nil, err
	}
	logger, err := obs.NewLogger(f.logFile, obs.FormatJSON, slog.LevelDebug)
	if err != nil {
		l.Close()
		f.logFile.Close()
		return nil, err
	}
	var served []tunnel.WelcomeService
	for _, s := range services {
		if name, ok := strings.CutPrefix(s, tunnel.KindMCP+":"); ok {
			served = append(served, tunnel.WelcomeService{Kind: tunnel.KindMCP, Name: name, URL: f.URL + "/mcp/" + name})
			continue
		}
		served = append(served, tunnel.WelcomeService{Kind: tunnel.KindA2A, Name: s, URL: f.URL + "/agents/" + s})
	}
	h, err := tunnelgate.NewHandler(tunnelgate.HandlerConfig{
		PublicURL: f.URL, ChannelBinding: tunnel.CBNone, ReplicaID: "fakegate",
		Admitter: fixedAdmitter{served: served}, Registry: sink{ch: f.sessions}, Logger: logger,
	})
	if err != nil {
		l.Close()
		f.logFile.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+tunnel.Path, h)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.srv.Serve(l) }()
	return f, nil
}

// Session waits for the next admitted session.
func (f *FakeGate) Session(within time.Duration) (*tunnelgate.Session, error) {
	select {
	case s := <-f.sessions:
		return s, nil
	case <-time.After(within):
		return nil, fmt.Errorf("e2e: no session reached the fake gate within %v", within)
	}
}

// Close stops serving; the sessions end with the listener's connections.
func (f *FakeGate) Close() error {
	err := f.srv.Close()
	f.logFile.Close()
	return err
}

// Exchange opens a stream on s, writes the preamble p and then raw (the bytes of one HTTP/1.1 request, as
// crafted as the test likes), and reads the node's answer.
func Exchange(s *tunnelgate.Session, p tunnel.Preamble, raw string, within time.Duration) (*http.Response, []byte, error) {
	st, err := s.Mux().OpenStream()
	if err != nil {
		return nil, nil, err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(within))
	if err := tunnel.WritePreamble(st, p); err != nil {
		return nil, nil, err
	}
	if _, err := io.WriteString(st, raw); err != nil {
		return nil, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}
