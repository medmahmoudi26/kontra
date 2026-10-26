package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// M2 harness (WP-08). The replicas share one public_url, which a node's hello must name as its origin
// (spec/tunnel.md §4: bad_origin otherwise), so the public port is a Front: a TCP proxy that forwards each
// new connection to the replica the test chose (Front.Route), replica A by default. Tunnel services are
// services with backend tunnel, each served by a `kontra connect` subprocess (a Node) of the binary built
// once from ./cmd/kontra, with its own HOME under the run's directory (its identity key and credentials)
// and a slowagent (or the helloworld seller) as its --serve target. Every Node carries KONTRA_E2E_RUN and
// the script's KONTRA_E2E_SESSION, so scripts/test_e2e.sh can prove none outlived the run.

// The spec's numbers every tunnel test's deadlines derive from (spec/tunnel.md §11, §13; relay.md §14.2).
const (
	// HeartbeatTimeout is a dispatch activity's heartbeat timeout: a dispatch whose replica died is seen
	// gone within it (relay.md §14.2).
	HeartbeatTimeout = 30 * time.Second
	// ReapAfter is how long a silent replica's gate_replicas row (and with it its tunnels rows) lives:
	// replicas are reaped once silent for 30 s, checked every 10 s (tunnel.md §11).
	ReapAfter = 30 * time.Second
	// Reregister bounds a node's redial: the backoff is capped at 60 s (tunnel.md §13), so a node whose
	// session closed has a new one within a minute of being able to dial.
	Reregister = 60 * time.Second
)

// --- the Front ---

// Front is the public_url listener of the harness.
type Front struct {
	Addr string // 127.0.0.1:<port>
	URL  string // http://127.0.0.1:<port>: public_url

	l     net.Listener
	mu    sync.Mutex
	to    *Gate                 // nil: new connections are refused
	conns map[net.Conn]struct{} // both ends of every forwarded connection
	count map[string]int        // connections forwarded, by replica name
	lose  int                   // the next connections whose welcome is lost (LoseWelcome)
	lost  []string              // the session ids of the welcomes lost, in order
}

// NewFront listens on a loopback port; until Route, it refuses every connection.
func NewFront() (*Front, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Front{Addr: l.Addr().String(), URL: "http://" + l.Addr().String(), l: l, conns: map[net.Conn]struct{}{},
		count: map[string]int{}}
	go f.serve()
	return f, nil
}

// Route sends every new connection to g from now on; established ones are left alone. A nil g refuses new
// connections (the gate is unreachable: a node's dial fails at once).
func (f *Front) Route(g *Gate) {
	f.mu.Lock()
	f.to = g
	f.mu.Unlock()
}

// Routed returns the replica new connections go to, nil when they are refused.
func (f *Front) Routed() *Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.to
}

// Forwarded is the number of connections the Front has forwarded to the replica g.
func (f *Front) Forwarded(g *Gate) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count[g.Name]
}

func (f *Front) serve() {
	for {
		c, err := f.l.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		g := f.to
		lose := false
		if g != nil {
			f.count[g.Name]++
			if f.lose > 0 {
				f.lose--
				lose = true
			}
		}
		f.mu.Unlock()
		if g == nil {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			c.Close()
			continue
		}
		go f.forward(c, g, lose)
	}
}

func (f *Front) forward(c net.Conn, g *Gate, lose bool) {
	u, err := net.DialTimeout("tcp", g.Addr, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	f.mu.Lock()
	f.conns[c], f.conns[u] = struct{}{}, struct{}{}
	f.mu.Unlock()
	if lose {
		f.loseWelcome(c, u)
	} else {
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
		go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
		<-done
	}
	c.Close()
	u.Close()
	f.mu.Lock()
	delete(f.conns, c)
	delete(f.conns, u)
	f.mu.Unlock()
}

// welcomeMarker opens the gate's welcome (spec/tunnel.md §4 M4): a text message the gate writes unmasked
// and uncompressed (internal/tunnelgate: CompressionDisabled), so its JSON is in the TCP stream as is.
var (
	welcomeMarker  = []byte(`"t":"welcome"`)
	welcomeSession = regexp.MustCompile(`"session":"([0-9a-f-]{36})"`)
)

// LoseWelcome makes the Front lose the gate's welcome on each of the next n connections it forwards (a
// welcome lost after the gate committed the admission: D124, D139). Such a connection is forwarded both
// ways until the gate writes its welcome; the Front then closes the node's side without passing the welcome
// on, and holds the gate's side open and silent, reading and discarding whatever the gate sends, until the
// gate closes it or the Front closes. The node sees its connection end after the proof (internal/node
// ErrNoOutcome) and redials; the gate sees a live connection, a half-open session it goes on to register
// after its card fetch through the session, which the node never answers, has timed out.
func (f *Front) LoseWelcome(n int) {
	f.mu.Lock()
	f.lose += n
	f.mu.Unlock()
}

// LostWelcomes returns the session ids of the welcomes the Front lost, in order.
func (f *Front) LostWelcomes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lost)
}

// lostSession records the session id of a lost welcome once *welcome holds it, and then clears *welcome;
// it gives up on a welcome that names none within 4 KiB.
func (f *Front) lostSession(welcome *[]byte) {
	if *welcome == nil {
		return
	}
	if m := welcomeSession.FindSubmatch(*welcome); m != nil {
		f.mu.Lock()
		f.lost = append(f.lost, string(m[1]))
		f.mu.Unlock()
		*welcome = nil
	} else if len(*welcome) > 4<<10 {
		*welcome = nil
	}
}

// loseWelcome forwards c (the node) and u (the gate) for LoseWelcome. It returns once the gate's side has
// ended.
func (f *Front) loseWelcome(c, u net.Conn) {
	go func() { _, _ = io.Copy(u, c) }() // ends with the node's side; the gate's side stays open
	buf := make([]byte, 32<<10)
	var (
		window  []byte // the end of what was forwarded, for a marker split across two reads
		welcome []byte // the lost welcome, from its marker on, until its session id is read
		dropped bool
	)
	for {
		k, err := u.Read(buf)
		if k > 0 {
			chunk := buf[:k]
			switch {
			case dropped:
				if welcome != nil {
					welcome = append(welcome, chunk...)
				}
			default:
				window = append(window, chunk...)
				i := bytes.Index(window, welcomeMarker)
				if i < 0 {
					if _, werr := c.Write(chunk); werr != nil {
						return
					}
					window = window[max(0, len(window)-len(welcomeMarker)):]
					break
				}
				// The welcome: the node's side ends here, without it (its session id recorded first, when
				// this read holds it, so a node that sees the end finds it in LostWelcomes).
				dropped = true
				welcome = append([]byte(nil), window[i:]...)
				f.lostSession(&welcome)
				c.Close()
			}
			f.lostSession(&welcome) // a welcome split across reads: its session id may come later
		}
		if err != nil {
			return
		}
	}
}

// Close stops the Front and closes every connection it forwards.
func (f *Front) Close() {
	f.l.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.to = nil
	for c := range f.conns {
		c.Close()
		delete(f.conns, c)
	}
}

// --- the node binary ---

var (
	nodeBuildOnce sync.Once
	nodeBin       string
	nodeBuildErr  error
)

// nodeBinary builds ./cmd/kontra once per test binary, under TempRoot.
func nodeBinary() (string, error) {
	nodeBuildOnce.Do(func() {
		root, err := TempRoot()
		if err != nil {
			nodeBuildErr = err
			return
		}
		dir, err := os.MkdirTemp(root, "bin-")
		if err != nil {
			nodeBuildErr = err
			return
		}
		nodeBin = filepath.Join(dir, "kontra")
		cmd := exec.Command("go", "build", "-o", nodeBin, "kontra.sh/kontra/cmd/kontra")
		if out, err := cmd.CombinedOutput(); err != nil {
			nodeBuildErr = fmt.Errorf("e2e: build kontra: %v\n%s", err, out)
		}
	})
	return nodeBin, nodeBuildErr
}

// --- nodes ---

// NodeOptions configure one `kontra connect`.
type NodeOptions struct {
	Name   string            // --name, and the HOME directory's name under the run's (default node-<n>)
	Serves map[string]string // service name → target URL (--serve a2a:<name>=<url>)
	Enroll []string          // service tokens from Harness.EnrollToken (--enroll, each)
	Rotate bool              // --rotate
	Home   string            // HOME: another node's to share its ~/.kontra (default a fresh directory)
	Key    string            // --key: an identity key file (default ~/.kontra/identity.key)
	Gate   string            // --gate (default the Front's URL)
	Args   []string          // extra flags
}

// Node is one `kontra connect` subprocess. Its stdout (the welcome lines) goes to OutPath and its JSON log
// to LogPath, both appended to across restarts: Log reads every launch's lines, LaunchLog only the current
// launch's.
type Node struct {
	Name    string
	Home    string
	KeyPath string
	OutPath string
	LogPath string
	KeyID   string // the active key's id, read from the identity file at each welcome

	bin  string
	env  []string
	opts NodeOptions

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	seen    []string // session ids of the welcomes before the current launch
	logFrom int64    // LogPath's size when the current launch began: where its lines start
}

// welcomeRE is the line `kontra connect` prints on stdout for each welcome (internal/node printWelcome).
var welcomeRE = regexp.MustCompile(`^kontra connect: connected to (\S+) as (k1_[a-z2-7]{26}) \(tenant (\S+), session ([0-9a-f-]{36})\)$`)

// LaunchNode starts a node and returns at once, without waiting for a welcome (a node the gate refuses).
func (h *Harness) LaunchNode(o NodeOptions) (*Node, error) {
	bin, err := nodeBinary()
	if err != nil {
		return nil, err
	}
	if o.Gate == "" {
		o.Gate = h.Front.URL
	}
	h.mu.Lock()
	if o.Name == "" {
		o.Name = fmt.Sprintf("node-%d", len(h.nodes)+1)
	}
	h.mu.Unlock()
	home := o.Home
	if home == "" {
		if home, err = os.MkdirTemp(h.Dir, "home-"+o.Name+"-"); err != nil {
			return nil, err
		}
	}
	key := o.Key
	if key == "" {
		key = filepath.Join(home, nodekey.DirName, "identity.key")
	}
	n := &Node{
		Name: o.Name, Home: home, KeyPath: key, bin: bin, opts: o,
		OutPath: filepath.Join(h.Dir, "node-"+o.Name+".out"), LogPath: filepath.Join(h.Dir, "node-"+o.Name+".log"),
	}
	// The harness's environment, less what would steer the node (KONTRA_KEY, HOME), plus its own HOME.
	for _, kv := range h.env() {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "KONTRA_KEY=") {
			n.env = append(n.env, kv)
		}
	}
	n.env = append(n.env, "HOME="+home)
	if err := n.launch(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.nodes = append(h.nodes, n)
	h.mu.Unlock()
	return n, nil
}

// args is the node's command line.
func (n *Node) args() []string {
	args := []string{"connect", "--gate", n.opts.Gate, "--name", n.Name, "--log", "json"}
	for _, name := range slices.Sorted(mapKeys(n.opts.Serves)) {
		args = append(args, "--serve", "a2a:"+name+"="+n.opts.Serves[name])
	}
	for _, t := range n.opts.Enroll {
		args = append(args, "--enroll", t)
	}
	if n.opts.Rotate {
		args = append(args, "--rotate")
	}
	if n.opts.Key != "" {
		args = append(args, "--key", n.opts.Key)
	}
	return append(args, n.opts.Args...)
}

func (n *Node) launch() error {
	out, err := os.OpenFile(n.OutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(n.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		out.Close()
		return err
	}
	if err := n.markLaunch(); err != nil {
		out.Close()
		logf.Close()
		return err
	}
	cmd := exec.Command(n.bin, n.args()...)
	cmd.Env = n.env
	cmd.Stdout, cmd.Stderr = out, logf
	cmd.SysProcAttr = procAttr()
	if err := cmd.Start(); err != nil {
		out.Close()
		logf.Close()
		return fmt.Errorf("e2e: start node %s: %w", n.Name, err)
	}
	done := make(chan struct{})
	n.mu.Lock()
	n.cmd, n.done, n.waitErr = cmd, done, nil
	n.mu.Unlock()
	go func() {
		err := cmd.Wait()
		out.Close()
		logf.Close()
		n.mu.Lock()
		n.waitErr = err
		n.mu.Unlock()
		close(done)
	}()
	return nil
}

// markLaunch records what the launch about to start must not count as its own: the welcomes printed so
// far, and where LogPath ends now. The previous launch has exited, so nothing writes the log meanwhile.
func (n *Node) markLaunch() error {
	var size int64
	switch fi, err := os.Stat(n.LogPath); {
	case err == nil:
		size = fi.Size()
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	seen := n.sessionIDs()
	n.mu.Lock()
	n.seen, n.logFrom = seen, size
	n.mu.Unlock()
	return nil
}

// Welcome is one welcome line of the node's stdout.
type Welcome struct {
	Origin, KeyID, Tenant, Session string
}

// Welcomes reads every welcome the node printed, across its launches, in order.
func (n *Node) Welcomes() []Welcome {
	b, err := os.ReadFile(n.OutPath)
	if err != nil {
		return nil
	}
	var out []Welcome
	for _, line := range strings.Split(string(b), "\n") {
		if m := welcomeRE.FindStringSubmatch(line); m != nil {
			out = append(out, Welcome{Origin: m[1], KeyID: m[2], Tenant: m[3], Session: m[4]})
		}
	}
	return out
}

// sessionIDs are the session ids of every welcome the node printed so far.
func (n *Node) sessionIDs() []string {
	var ids []string
	for _, w := range n.Welcomes() {
		ids = append(ids, w.Session)
	}
	return ids
}

// Session is the session id of the node's latest welcome, "" before any.
func (n *Node) Session() string {
	ws := n.Welcomes()
	if len(ws) == 0 {
		return ""
	}
	return ws[len(ws)-1].Session
}

// Pid is the node process's id (the current launch).
func (n *Node) Pid() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cmd.Process.Pid
}

// Exited reports whether the current launch has ended.
func (n *Node) Exited() bool {
	n.mu.Lock()
	done := n.done
	n.mu.Unlock()
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// Wait waits up to within for the current launch to end and returns its exit code.
func (n *Node) Wait(within time.Duration) (int, error) {
	n.mu.Lock()
	done := n.done
	n.mu.Unlock()
	select {
	case <-done:
	case <-time.After(within):
		return -1, fmt.Errorf("e2e: node %s still running after %v", n.Name, within)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var exit *exec.ExitError
	switch {
	case n.waitErr == nil:
		return 0, nil
	case errors.As(n.waitErr, &exit):
		return exit.ExitCode(), nil
	}
	return -1, n.waitErr
}

// WaitSession waits until the node has printed a welcome of this launch and that session's tunnels row is
// in (the gate registers a session after its card fetch, relay.md §3), and returns the session id and the
// replica holding it. A node that exits meanwhile is an error.
func (n *Node) WaitSession(ctx context.Context, h *Harness, within time.Duration) (session, replica string, err error) {
	deadline := time.Now().Add(within)
	for {
		n.mu.Lock()
		seen := n.seen
		n.mu.Unlock()
		if id := n.Session(); id != "" && !slices.Contains(seen, id) {
			err := h.Pool.QueryRow(ctx, `select replica_id from kontra.tunnels where session_id = $1`, id).Scan(&replica)
			switch {
			case err == nil:
				if k, kerr := nodekey.Load(n.KeyPath); kerr == nil {
					n.KeyID = k.KeyID()
				}
				return id, replica, nil
			case !errors.Is(err, pgx.ErrNoRows):
				return "", "", err
			}
		}
		if n.Exited() {
			code, _ := n.Wait(0)
			return "", "", fmt.Errorf("e2e: node %s exited (%d) before a session of this launch was registered:\n%s", n.Name, code, tail(n.LogPath, 30))
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("e2e: node %s has no registered session within %v:\n%s", n.Name, within, tail(n.LogPath, 30))
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// StartNode starts a node and waits for its welcome and its tunnels row; it returns the node with its key
// id set.
func (h *Harness) StartNode(ctx context.Context, o NodeOptions) (*Node, error) {
	n, err := h.LaunchNode(o)
	if err != nil {
		return nil, err
	}
	if _, _, err := n.WaitSession(ctx, h, Reregister); err != nil {
		return n, err
	}
	return n, nil
}

// Kill sends SIGKILL to the node (its process group) and waits until it is gone (tunnel_kill).
func (n *Node) Kill() error {
	n.mu.Lock()
	cmd, done := n.cmd, n.done
	n.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("e2e: node %s (pid %d) did not exit after SIGKILL", n.Name, cmd.Process.Pid)
	}
}

// Term sends SIGTERM to the node and returns its exit code (0: kontra connect stops cleanly on SIGTERM).
func (n *Node) Term() (int, error) {
	n.mu.Lock()
	cmd := n.cmd
	n.mu.Unlock()
	if !n.Exited() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	return n.Wait(15 * time.Second)
}

// Relaunch starts the node again, after it exited, with the same HOME and the same command line: the
// same key and its original --enroll flags (M2 plan audit, gap 1: a restart passes the flags it was
// started with, and a token a welcome confirmed is never sent again). It does not wait for a welcome.
func (n *Node) Relaunch() error {
	if !n.Exited() {
		return fmt.Errorf("e2e: node %s is still running", n.Name)
	}
	return n.launch()
}

// Restart relaunches the node (Relaunch) and waits for a new session's welcome and tunnels row, within the
// spec's re-registration bound.
func (n *Node) Restart(ctx context.Context, h *Harness) error {
	if err := n.Relaunch(); err != nil {
		return err
	}
	_, _, err := n.WaitSession(ctx, h, Reregister)
	return err
}

// Log returns the node's JSON log lines, of every launch, that contain every one of substrs.
func (n *Node) Log(substrs ...string) []string {
	return n.logLines(0, substrs)
}

// LaunchLog is Log over the current launch's lines only: what a restarted node logged, never what an
// earlier launch did (a check after Restart or Relaunch).
func (n *Node) LaunchLog(substrs ...string) []string {
	n.mu.Lock()
	from := n.logFrom
	n.mu.Unlock()
	return n.logLines(from, substrs)
}

// logLines returns the lines of LogPath from the byte offset from on that contain every one of substrs.
func (n *Node) logLines(from int64, substrs []string) []string {
	b, err := os.ReadFile(n.LogPath)
	if err != nil {
		return nil
	}
	if from > int64(len(b)) {
		return nil
	}
	b = b[from:]
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		ok := true
		for _, s := range substrs {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

// --- enrollment and the registry ---

// EnrollToken mints a service token with `kontra-gate enroll -f kontra.yaml <service>` (spec/tunnel.md §5).
// The gate must have loaded its services: (Start has).
func (h *Harness) EnrollToken(ctx context.Context, service string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.bin, "enroll", "-f", h.configPath, service)
	cmd.Env = h.env()
	cmd.SysProcAttr = procAttr()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("e2e: kontra-gate enroll %s: %v\n%s", service, err, stderr.String())
	}
	token := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(token, "ket_") {
		return "", fmt.Errorf("e2e: enroll %s printed %q, not a token", service, token)
	}
	return token, nil
}

// Revoke runs `kontra-gate enroll -f kontra.yaml --revoke <key_id> [--service <name>]`: the key, or with
// service one of its bindings (spec/tunnel.md §12).
func (h *Harness) Revoke(ctx context.Context, keyID, service string) error {
	args := []string{"enroll", "-f", h.configPath, "--revoke", keyID}
	if service != "" {
		args = append(args, "--service", service)
	}
	if out, err := h.gateCommand(ctx, args...); err != nil {
		return fmt.Errorf("e2e: kontra-gate %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// Sessions reads the node key's tunnels rows: session id → replica id.
func (h *Harness) Sessions(ctx context.Context, keyID string) (map[string]string, error) {
	rows, err := h.Pool.Query(ctx, `select session_id::text, replica_id from kontra.tunnels where key_id = $1`, keyID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	defer rows.Close()
	for rows.Next() {
		var s, r string
		if err := rows.Scan(&s, &r); err != nil {
			return nil, err
		}
		out[s] = r
	}
	return out, rows.Err()
}

// SessionServices reads tunnels.services of a session; ok is false when the row is gone.
func (h *Harness) SessionServices(ctx context.Context, session string) (services []string, ok bool, err error) {
	err = h.Pool.QueryRow(ctx, `select services from kontra.tunnels where session_id = $1`, session).Scan(&services)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return services, err == nil, err
}

// WaitCard waits until the tunnel service's card is stored (services.card, fetched through a session,
// relay.md §3) and every replica still running serves it at its card route.
func (h *Harness) WaitCard(ctx context.Context, service string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		var stored bool
		err := h.Pool.QueryRow(ctx, `select card is not null from kontra.services where name = $1`, service).Scan(&stored)
		ok := err == nil && stored
		for _, g := range h.Gates {
			if ok && !g.Exited() {
				ok = status(ctx, "GET", g.URL+"/agents/"+service+"/.well-known/agent-card.json", h.APIKey) == 200
			}
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: the card of %s is not stored and served within %v (%v)", service, within, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Agent starts another slowagent for the test (a node's --serve target), closed with the harness.
func (h *Harness) Agent(cfg slowagent.Config) (*slowagent.Agent, error) {
	a := slowagent.New(cfg)
	if _, err := a.Start(context.Background()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.agents = append(h.agents, a)
	h.mu.Unlock()
	return a, nil
}

// OnClose runs fn when the harness closes, before the replicas stop (last added, first run).
func (h *Harness) OnClose(fn func() error) {
	h.mu.Lock()
	h.cleanup = append(h.cleanup, fn)
	h.mu.Unlock()
}
