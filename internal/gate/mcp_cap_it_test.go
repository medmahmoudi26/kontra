package gate

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/stdiobridge"
	"kontra.sh/kontra/internal/tenant"
)

// D213, D231 through a real gate and an in-process `kontra connect` (relay.md §11.2, tunnel.md §10): a stdio
// service at --max-sessions 2 and max_sessions_per_principal 2. One principal's third initialize while its sessions
// are in recent use is refused 429 by the gate, closing nothing; once they have been idle for the gate's EvictIdle,
// the third closes the longest idle, whose upstream DELETE stops that child before the new initialize reaches the
// full bridge (the bridge's own EvictIdle, longer, has not passed: without the gate it would be 503 full). Another
// principal holds no session there, so the gate closes none for it, and the full bridge stops its longest-idle child
// (idle for at least the bridge's EvictIdle) rather than answer full. Each closed session answers 404, every other one
// keeps working. Both EvictIdles are shortened here.
func TestSessionsPerPrincipalThroughTheGate(t *testing.T) {
	old, oldGate := stdiobridge.EvictIdle, mcprelay.EvictIdle
	stdiobridge.EvictIdle = 4 * time.Second // longer than any exchange of the test's under load, before it waits on purpose
	mcprelay.EvictIdle = 2 * time.Second
	t.Cleanup(func() { stdiobridge.EvictIdle, mcprelay.EvictIdle = old, oldGate })
	bin := filepath.Join(t.TempDir(), "stdioserver")
	if out, err := exec.Command("go", "build", "-o", bin, "../stdiobridge/testdata/stdioserver").CombinedOutput(); err != nil {
		t.Fatalf("build the stdio server double: %v\n%s", err, out)
	}
	e := newGateEnv(t)
	services := `services:
  fs:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    max_sessions_per_principal: 2
`
	run := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	team, err := tenant.EnsureTeam(t.Context(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(t.Context(), team, "fs", "test")
	if err != nil {
		t.Fatal(err)
	}
	rn := startNode(t, t.TempDir(), "--gate", run.url, "--enroll", tok, "--max-sessions", "2",
		"--serve", "mcp:fs=stdio:'"+bin+"' -record '"+t.TempDir()+"'")
	rn.local(t)
	endpoint := run.url + "/mcp/fs"
	keyA, keyB := e.mintKey("buyer-a"), e.mintKey("buyer-b")
	initialize := func(key string) string {
		t.Helper()
		var sid string
		eventually(t, 20*time.Second, "an initialize through the gate", func() bool {
			st, h, _ := mcpPost(t, endpoint, key, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
				`"capabilities":{},"clientInfo":{"name":"cap","version":"1"}}}`)
			sid = h.Get("Mcp-Session-Id")
			return st == http.StatusOK && sid != ""
		})
		if st, _, body := mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); st != http.StatusAccepted {
			t.Fatalf("notifications/initialized: %d %s", st, body)
		}
		return sid
	}
	echo := func(key, sid, text string) int {
		t.Helper()
		st, _, body := mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"`+text+`"}}}`)
		if st == http.StatusOK && !strings.Contains(body, text) {
			t.Fatalf("echo %s: %s", text, body)
		}
		return st
	}

	third := func() (int, string, string) {
		t.Helper()
		st, h, body := mcpPost(t, endpoint, keyA, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
			`"capabilities":{},"clientInfo":{"name":"cap","version":"1"}}}`)
		return st, h.Get("Mcp-Session-Id"), body
	}
	a1 := initialize(keyA)
	a2 := initialize(keyA)
	if st := echo(keyA, a2, "a2"); st != http.StatusOK {
		t.Fatalf("a2: %d", st)
	}
	// A's sessions are in recent use: its third initialize is refused by the gate, and closes nothing.
	if st, sid, body := third(); st != http.StatusTooManyRequests || sid != "" || !strings.Contains(body, "too many sessions") {
		t.Fatalf("A's third initialize, its sessions in use: %d %q %s", st, sid, body)
	}
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.mcp_sessions where closed_at is not null`); n != 0 {
		t.Fatalf("a refused initialize closed %d sessions", n)
	}
	// The bridge is full (2 children): once A's sessions are idle for the gate's EvictIdle, A's third initialize
	// opens, the gate having closed a1, A's longest idle.
	time.Sleep(mcprelay.EvictIdle + 500*time.Millisecond)
	st, a3, body := third()
	if st != http.StatusOK || a3 == "" {
		t.Fatalf("A's third initialize at a full bridge: %d %s", st, body)
	}
	if st := echo(keyA, a1, "a1"); st != http.StatusNotFound {
		t.Fatalf("A's longest-idle session, closed for the third: %d", st)
	}
	if got := scalar[string](t, e.pool, `select coalesce(string_agg(close_reason, ','), '') from kontra.mcp_sessions where closed_at is not null`); got != "idle" {
		t.Fatalf("the closed sessions' reasons: %q", got)
	}
	if st := echo(keyA, a2, "a2 again"); st != http.StatusOK {
		t.Fatalf("A's other session: %d", st)
	}
	if st := echo(keyA, a3, "a3"); st != http.StatusOK {
		t.Fatalf("A's third session: %d", st)
	}

	// B holds no session: the gate closes none for it, and the full bridge stops its longest-idle child, a2 (A's
	// third session was used last).
	time.Sleep(stdiobridge.EvictIdle + 500*time.Millisecond)
	b1 := initialize(keyB)
	if st := echo(keyB, b1, "b1"); st != http.StatusOK {
		t.Fatalf("B's session: %d", st)
	}
	if st := echo(keyA, a2, "a2 evicted"); st != http.StatusNotFound {
		t.Fatalf("the bridge's longest-idle child after B's initialize: %d", st)
	}
	if st := echo(keyA, a3, "a3 again"); st != http.StatusOK {
		t.Fatalf("A's session the bridge kept: %d", st)
	}
	if !strings.Contains(rn.logs.String(), "its longest-idle child is stopped for a new session") {
		t.Error("the node does not log the eviction")
	}
}
