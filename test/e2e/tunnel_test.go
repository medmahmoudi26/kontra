package e2e

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// The M2 gate (PRD §14 M2 row: "Kill the tunnel process mid-task: task fails or retries exactly as section 7
// says; reconnect re-registers within backoff"; spec/tunnel.md §16, spec/relay.md §16). Each test is in its
// own file: tunnel_kill_test.go (TestTunnelKill, and TestNodeRotateAfterLeak), input_required_sticky_test.go
// (TestInputRequiredSticky), cancel_tunnel_test.go (TestCancelForwardedTunnel), key_scope_test.go
// (TestKeyScope), sidecar_test.go (TestSidecarNotAProxy), replica_kill_tunnel_test.go (TestReplicaKillTunnel),
// helloworld_tunnel_test.go (TestHelloworldTunnel) and tunnel_drain_test.go (TestTunnelDrain), with
// lost_welcome_test.go (TestTunnelLostWelcome), stale_view_test.go (TestTunnelStaleRevocationView) and
// card_pending_test.go (TestTunnelCardPending) beside them. This file holds what they share.

// retries is a task.retries value for Options and TunnelService.
func retries(n int) *int { return &n }

// tunnelKeys are the strings no tunnel test's TaskLifecycle history may hold: the chunks' text, the API
// keys and the caller's cookie (events and credentials never enter history, spec §14.1).
func tunnelForbidden(h *Harness, extra ...string) []string {
	return append([]string{slowagent.ChunkMarker, h.APIKey, h.APIKeyB, "e2e-cookie"}, extra...)
}

// startNode mints one service token per served service (`kontra-gate enroll <service>`) and starts a node
// serving them, waiting for its welcome and its tunnels row; it returns the node and its session.
func startNode(t *testing.T, ctx context.Context, h *Harness, name string, serves map[string]string, more ...func(*NodeOptions)) (*Node, string) {
	t.Helper()
	o := NodeOptions{Name: name, Serves: serves}
	for _, svc := range slices.Sorted(mapKeys(serves)) {
		tok, err := h.EnrollToken(ctx, svc)
		if err != nil {
			t.Fatal(err)
		}
		o.Enroll = append(o.Enroll, tok)
	}
	for _, f := range more {
		f(&o)
	}
	n, err := h.StartNode(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("node %s (key %s): session %s on replica %s", n.Name, n.KeyID, n.Session(), h.Front.Routed().Name)
	return n, n.Session()
}

// TaskFailure reads tasks.state and the projection's kontra.failure metadata.
func (h *Harness) TaskFailure(ctx context.Context, taskID a2a.TaskID) (state, failure string, err error) {
	err = h.Pool.QueryRow(ctx, `select state, coalesce(snapshot::json->'metadata'->>'kontra.failure', '')
		from kontra.tasks where id = $1`, string(taskID)).Scan(&state, &failure)
	return state, failure, err
}

// sessionReplica reads the replica of a tunnels row, "" when the row is gone.
func sessionReplica(ctx context.Context, h *Harness, session string) string {
	var r string
	_ = h.Pool.QueryRow(ctx, `select replica_id from kontra.tunnels where session_id = $1`, session).Scan(&r)
	return r
}

// waitRowGone waits until the tunnels row of session is deleted and returns how long it took.
func waitRowGone(t *testing.T, ctx context.Context, h *Harness, session string, within time.Duration, why string) time.Duration {
	t.Helper()
	start := time.Now()
	eventually(t, within, "the tunnels row of session "+session+" gone ("+why+")", func() bool {
		return sessionReplica(ctx, h, session) == ""
	})
	return time.Since(start)
}

// attemptsOf waits until ok holds for the task's attempts and returns them.
func attemptsOf(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID, within time.Duration, what string, ok func([]Attempt) bool) []Attempt {
	t.Helper()
	var as []Attempt
	eventually(t, within, what, func() bool {
		var err error
		as, err = h.Attempts(ctx, taskID)
		return err == nil && ok(as)
	})
	return as
}

// endedAs waits until the task is terminal and its last attempt has ended, and returns the task's state,
// its kontra.failure and the attempts.
func endedAs(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID, within time.Duration) (string, string, []Attempt) {
	t.Helper()
	var state, failure string
	eventually(t, within, "task "+string(taskID)+" terminal", func() bool {
		var err error
		state, failure, err = h.TaskFailure(ctx, taskID)
		return err == nil && (state == "completed" || state == "failed" || state == "canceled" || state == "rejected")
	})
	as := attemptsOf(t, ctx, h, taskID, 30*time.Second, "the last attempt of "+string(taskID)+" ended", func(as []Attempt) bool {
		return len(as) > 0 && as[len(as)-1].EndedAt != nil
	})
	return state, failure, as
}

// sendsOf counts the agent's sends (SendMessage, SendStreamingMessage) that carried the message messageID.
func sendsOf(a *slowagent.Agent, messageID string) int {
	n := 0
	for _, r := range a.Requests() {
		if (r.Method == "SendMessage" || r.Method == "SendStreamingMessage") && r.MessageID == messageID {
			n++
		}
	}
	return n
}

// requestsForTask counts the agent's requests that carried a message or named a task with upstream id up.
func requestsForTask(a *slowagent.Agent, up string) int {
	n := 0
	for _, r := range a.Requests() {
		if up != "" && r.TaskID == up {
			n++
		}
	}
	return n
}

// --- what a process holds: its TCP sockets ---

// tcpSocket is one TCP socket a process holds (an fd), read from the network namespace's tables.
type tcpSocket struct {
	Local, Remote string // host:port
	State         string // the kernel's hex state: 01 established, 0A listen, …
}

// processSockets maps the socket:[inode] links under /proc/<pid>/fd to the rows of /proc/<pid>/net/tcp and
// tcp6. Those tables list every socket of the process's network namespace, not the process's own (M2 plan
// audit: /proc/self/net/tcp and /proc/1/net/tcp are identical), so only the inodes the process holds an fd
// for are kept.
func processSockets(pid int) ([]tcpSocket, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if err != nil {
			continue
		}
		if ino, ok := strings.CutPrefix(link, "socket:["); ok {
			inodes[strings.TrimSuffix(ino, "]")] = true
		}
	}
	var out []tcpSocket
	for _, table := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "net", table))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || !inodes[fields[9]] {
				continue
			}
			local, err1 := procAddr(fields[1])
			remote, err2 := procAddr(fields[2])
			if err1 != nil || err2 != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %v %v", table, err1, err2)
			}
			out = append(out, tcpSocket{Local: local, Remote: remote, State: fields[3]})
		}
		f.Close()
	}
	return out, nil
}

// procAddr decodes /proc/net/tcp's ADDR:PORT, the address as 32-bit words in host (little-endian) order.
func procAddr(s string) (string, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return "", fmt.Errorf("address %q", s)
	}
	raw, err := hex.DecodeString(h)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", fmt.Errorf("address %q", s)
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		ip[i], ip[i+1], ip[i+2], ip[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return "", fmt.Errorf("port %q", p)
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return net.JoinHostPort(ip.String(), strconv.FormatUint(port, 10)), nil
}

// tcpListen is the kernel's state of a listening socket.
const tcpListen = "0A"

// checkNoListener fails if the process listens on any TCP socket but a loopback one (kontra connect's hiring
// endpoint, --listen 127.0.0.1, spec/tunnel.md §9): the gate can never have opened a connection to it (§1, §15).
// It returns the process's other sockets.
func checkNoListener(t *testing.T, pid int, who string) []tcpSocket {
	t.Helper()
	socks, err := processSockets(pid)
	if err != nil {
		t.Fatalf("the sockets of %s (pid %d): %v", who, pid, err)
	}
	var out []tcpSocket
	for _, s := range socks {
		if s.State != tcpListen {
			out = append(out, s)
			continue
		}
		host, _, _ := net.SplitHostPort(s.Local)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			t.Fatalf("%s (pid %d) listens on %s: a gate could connect to it", who, pid, s.Local)
		}
	}
	return out
}
