package stdiobridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/tunnel"
)

// doubleBin is the Go stdio MCP server double (testdata/stdioserver), built once by TestMain.
var doubleBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stdiobridge-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	doubleBin = filepath.Join(dir, "stdioserver")
	if out, err := exec.Command("go", "build", "-o", doubleBin, "./testdata/stdioserver").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build the stdio server double: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuf is a goroutine-safe buffer.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fixture is a bridge serving the double over HTTP on 127.0.0.1 (an httptest server; the node serves it on tunnel
// streams), with its log and the double's records.
type fixture struct {
	t    *testing.T
	b    *Bridge
	url  string
	rec  string
	logs *syncBuf
}

func newFixture(t *testing.T, tweak func(*Config), args ...string) *fixture {
	t.Helper()
	rec := t.TempDir()
	logs := &syncBuf{}
	cfg := Config{Service: "fs", Command: append([]string{doubleBin, "-record", rec}, args...)}
	if tweak != nil {
		tweak(&cfg)
	}
	b, err := New(cfg, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		_ = b.Close()
		if t.Failed() {
			t.Logf("bridge log:\n%s", logs.String())
		}
	})
	return &fixture{t: t, b: b, url: srv.URL + "/", rec: rec, logs: logs}
}

type sseEvent struct{ id, data string }

type reply struct {
	status int
	header http.Header
	body   string
	events []sseEvent
}

// rpc is the JSON-RPC message of an event or a JSON body.
func (r reply) last() map[string]any {
	data := r.body
	if len(r.events) > 0 {
		data = r.events[len(r.events)-1].data
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(data), &m)
	return m
}

func (f *fixture) do(method, sid, version, body string, header ...string) reply {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set(mcpwire.HeaderSessionID, sid)
	}
	if version != "" {
		req.Header.Set(mcpwire.HeaderProtocolVersion, version)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, body, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatalf("read the answer to %s: %v", body, err)
	}
	r := reply{status: resp.StatusCode, header: resp.Header, body: string(b)}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		r.events = parseSSE(string(b))
	}
	return r
}

func (f *fixture) post(sid, body string) reply {
	f.t.Helper()
	v := ""
	if sid != "" {
		v = mcpwire.V20251125
	}
	return f.do(http.MethodPost, sid, v, body)
}

func parseSSE(s string) []sseEvent {
	var out []sseEvent
	for _, block := range strings.Split(s, "\n\n") {
		var ev sseEvent
		has := false
		for _, line := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(line, ":")
			v = strings.TrimPrefix(v, " ")
			switch k {
			case "id":
				ev.id, has = v, true
			case "data":
				ev.data, has = v, true
			}
		}
		if has {
			out = append(out, ev)
		}
	}
	return out
}

// initialize opens a session whose client declares caps, and sends notifications/initialized.
func (f *fixture) initialize(caps string) string {
	f.t.Helper()
	r := f.post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":`+caps+
		`,"clientInfo":{"name":"test","version":"1"}}}`)
	sid := r.header.Get(mcpwire.HeaderSessionID)
	if r.status != http.StatusOK || sid == "" || !strings.Contains(r.body, `"serverInfo"`) {
		f.t.Fatalf("initialize: %d %q %s", r.status, sid, r.body)
	}
	if r := f.post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != http.StatusAccepted {
		f.t.Fatalf("notifications/initialized: %d %s", r.status, r.body)
	}
	return sid
}

// call sends tools/call and returns its event stream.
func (f *fixture) call(sid string, id int, tool, args string, meta ...string) reply {
	f.t.Helper()
	m := ""
	if len(meta) > 0 {
		m = `,"_meta":` + meta[0]
	}
	return f.post(sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s%s}}`, id, tool, args, m))
}

// text is the first text content of a tools/call result.
func text(t *testing.T, r reply) string {
	t.Helper()
	m := r.last()
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no result content in %d %s %v", r.status, r.body, r.events)
	}
	c, _ := content[0].(map[string]any)
	s, _ := c["text"].(string)
	return s
}

// getStream is an open GET stream.
type getStream struct {
	t      *testing.T
	ch     chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

func (f *fixture) get(sid, lastID string) *getStream {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(mcpwire.HeaderSessionID, sid)
	req.Header.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20251125)
	if lastID != "" {
		req.Header.Set(mcpwire.HeaderLastEventID, lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		f.t.Fatalf("GET: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	g := &getStream{t: f.t, ch: make(chan sseEvent, 1024), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(g.done)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		var ev sseEvent
		for {
			line, err := br.ReadString('\n')
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "" && err == nil:
				if ev.data != "" {
					g.ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
			if err != nil {
				return
			}
		}
	}()
	f.t.Cleanup(g.close)
	return g
}

func (g *getStream) close() { g.cancel(); <-g.done }

func (g *getStream) next(within time.Duration) sseEvent {
	g.t.Helper()
	select {
	case ev := <-g.ch:
		return ev
	case <-time.After(within):
		g.t.Fatalf("no event on the GET stream within %v", within)
		return sseEvent{}
	}
}

func (g *getStream) none(within time.Duration) {
	g.t.Helper()
	select {
	case ev := <-g.ch:
		g.t.Fatalf("the GET stream received %s", ev.data)
	case <-time.After(within):
	}
}

// start is a start-<pid>.json record.
type start struct {
	PID, PGID, Grandchild int
}

func (f *fixture) starts() []start {
	f.t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.rec, "start-*.json"))
	var out []start
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			f.t.Fatal(err)
		}
		var s start
		if err := json.Unmarshal(b, &s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f *fixture) onlyStart() start {
	f.t.Helper()
	s := f.starts()
	if len(s) != 1 {
		f.t.Fatalf("%d children started, want 1", len(s))
	}
	return s[0]
}

func (f *fixture) record(kind string, pid int) []byte {
	f.t.Helper()
	var b []byte
	eventually(f.t, 10*time.Second, "the double's "+kind+" record", func() bool {
		var err error
		b, err = os.ReadFile(filepath.Join(f.rec, fmt.Sprintf("%s-%d.json", kind, pid)))
		return err == nil
	})
	return b
}

func (f *fixture) received(pid int) []string {
	b, _ := os.ReadFile(filepath.Join(f.rec, fmt.Sprintf("received-%d.jsonl", pid)))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// --- tests ---

// SplitCommand: POSIX word rules, a leading ~/ expanded, nothing else; shell syntax is refused unquoted.
func TestSplitCommand(t *testing.T) {
	for _, c := range []struct {
		line string
		want []string
	}{
		{"npx -y @modelcontextprotocol/server-filesystem@1.0.0 /srv/kontra-fs", []string{"npx", "-y", "@modelcontextprotocol/server-filesystem@1.0.0", "/srv/kontra-fs"}},
		{"  a\t b\n c  ", []string{"a", "b", "c"}},
		{`a 'b c' "d e" f\ g`, []string{"a", "b c", "d e", "f g"}},
		{`a '' ""`, []string{"a", "", ""}},
		{`a 'it''s' "q\"x" "\$HOME" "\n" 'a\b'`, []string{"a", "its", `q"x`, "$HOME", `\n`, `a\b`}},
		{"a b\\\nc", []string{"a", "bc"}},
		{`~/bin/server ~/docs x~/y '~/q' "~/r"`, []string{"/home/u/bin/server", "/home/u/docs", "x~/y", "~/q", "~/r"}},
		{"~ ~user/x", []string{"~", "~user/x"}},
		{`a '|' "&&" '$X' "#" b#c`, []string{"a", "|", "&&", "$X", "#", "b#c"}},
		{`node "/opt/my server/index.js" --dir=/a,b`, []string{"node", "/opt/my server/index.js", "--dir=/a,b"}},
	} {
		got, err := SplitCommand(c.line, "/home/u")
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("SplitCommand(%q) = %q, %v; want %q", c.line, got, err, c.want)
		}
	}
	for _, c := range []struct{ line, want string }{
		{"", "empty"},
		{"   \t ", "empty"},
		{"a 'b", "unterminated single quote"},
		{`a "b`, "unterminated double quote"},
		{`a \`, "backslash"},
		{"a | b", `'|'`},
		{"a > log", `'>'`},
		{"a; b", `';'`},
		{"a && b", `'&'`},
		{"a $HOME", `'$'`},
		{"a `id`", "'`'"},
		{"a $(id)", `'$'`},
		{"a # comment", `'#'`},
		{"(a)", `'('`},
	} {
		if _, err := SplitCommand(c.line, "/home/u"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("SplitCommand(%q): %v, want %q", c.line, err, c.want)
		}
	}
	if _, err := SplitCommand("~/x", ""); err == nil || !strings.Contains(err.Error(), "no home") {
		t.Errorf("~/ without a home: %v", err)
	}
}

// One child per session: an initialize spawns exactly one child and answers with a node-minted session id; a second
// session gets a second child; a tools/call is answered on an event stream whose ids are <child start>-<n>.
func TestChildPerSession(t *testing.T) {
	f := newFixture(t, nil)
	a := f.initialize(`{}`)
	if len(f.starts()) != 1 || f.b.Sessions() != 1 {
		t.Fatalf("after one initialize: %d children, %d sessions", len(f.starts()), f.b.Sessions())
	}
	b := f.initialize(`{}`)
	if a == b || len(a) != 22 || len(b) != 22 {
		t.Fatalf("session ids %q, %q", a, b)
	}
	starts := f.starts()
	if len(starts) != 2 || starts[0].PID == starts[1].PID || f.b.Sessions() != 2 {
		t.Fatalf("after two initializes: %+v, %d sessions", starts, f.b.Sessions())
	}
	r := f.call(a, 1, "echo", `{"text":"hello from a"}`)
	if r.status != http.StatusOK || len(r.events) != 1 || text(t, r) != "hello from a" {
		t.Fatalf("echo on a: %d %v", r.status, r.events)
	}
	child := f.b.lookup(a, nil)
	if id := r.events[0].id; id != fmt.Sprintf("%d-%d", child.start, 1) && !strings.HasPrefix(id, fmt.Sprintf("%d-", child.start)) {
		t.Errorf("event id %q, want <child start>-<n>", id)
	}
	// The other session's pid answers on b: the calls reach different children.
	pa, pb := text(t, f.call(a, 2, "pid", `{}`)), text(t, f.call(b, 2, "pid", `{}`))
	if pa == pb {
		t.Errorf("both sessions reached the process %s", pa)
	}
	if len(f.starts()) != 2 {
		t.Errorf("calls spawned children: %d", len(f.starts()))
	}
	// A session of one owner is not another owner's (a tunnel session's children are its own).
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.b.ServeHTTP(w, r.WithContext(WithOwner(r.Context(), "another tunnel session")))
	}))
	defer other.Close()
	g := &fixture{t: t, b: f.b, url: other.URL + "/", rec: f.rec, logs: f.logs}
	if r := g.call(a, 3, "echo", `{"text":"x"}`); r.status != http.StatusNotFound {
		t.Errorf("another owner's request: %d %s", r.status, r.body)
	}
	sid := g.initialize(`{}`)
	f.b.CloseOwner("another tunnel session")
	if r := g.call(sid, 1, "echo", `{"text":"x"}`); r.status != http.StatusNotFound {
		t.Errorf("after CloseOwner: %d", r.status)
	}
	if r := f.call(a, 4, "echo", `{"text":"still"}`); text(t, r) != "still" {
		t.Errorf("CloseOwner stopped another owner's session")
	}
}

// Routing (§10): progress to its request's POST stream; a related-task message to its task's open tasks/result POST,
// else to the GET stream.
func TestRouting(t *testing.T) {
	f := newFixture(t, nil)
	sid := f.initialize(`{}`)
	g := f.get(sid, "")

	r := f.call(sid, 2, "progress", `{"n":3}`, `{"progressToken":"tok-1"}`)
	if r.status != http.StatusOK || len(r.events) != 4 {
		t.Fatalf("progress: %d %v", r.status, r.events)
	}
	for i, ev := range r.events[:3] {
		if !strings.Contains(ev.data, `"notifications/progress"`) || !strings.Contains(ev.data, `"progressToken":"tok-1"`) ||
			!strings.Contains(ev.data, fmt.Sprintf(`"progress":%d`, i+1)) {
			t.Errorf("event %d on the POST: %s", i, ev.data)
		}
	}
	if text(t, r) != "progressed" {
		t.Errorf("the response is not last: %v", r.events)
	}
	// A numeric token routes too.
	if r := f.call(sid, 3, "progress", `{"n":1}`, `{"progressToken":7}`); len(r.events) != 2 || !strings.Contains(r.events[0].data, `"progressToken":7`) {
		t.Errorf("numeric token: %v", r.events)
	}
	g.none(300 * time.Millisecond)

	// tasks/result of t1 stays open (the double holds it); a related-task message for t1 lands on it, not on the
	// related call's own stream nor on the GET stream.
	held := make(chan reply, 1)
	go func() {
		held <- f.post(sid, `{"jsonrpc":"2.0","id":"tr-1","method":"tasks/result","params":{"taskId":"t1"}}`)
	}()
	child := f.b.lookup(sid, nil)
	eventually(t, 5*time.Second, "the tasks/result POST open", func() bool {
		child.mu.Lock()
		defer child.mu.Unlock()
		return child.taskRes["t1"] != nil
	})
	time.Sleep(100 * time.Millisecond) // the double read the tasks/result
	rel := f.call(sid, 4, "related", `{"taskId":"t1"}`)
	if len(rel.events) != 1 || text(t, rel) != "related" {
		t.Errorf("the related call's own stream: %v", rel.events)
	}
	tr := <-held
	if len(tr.events) != 2 || !strings.Contains(tr.events[0].data, `"about t1"`) || !strings.Contains(tr.events[0].data, "related-task") ||
		!strings.Contains(tr.events[1].data, "result of t1") {
		t.Errorf("tasks/result stream: %v", tr.events)
	}
	g.none(200 * time.Millisecond)
	// A task with no open tasks/result: the related message goes to the GET stream.
	f.call(sid, 5, "related", `{"taskId":"t2"}`)
	if ev := g.next(5 * time.Second); !strings.Contains(ev.data, `"about t2"`) {
		t.Errorf("GET: %s", ev.data)
	}
}

// The GET queue (§10): a server request sent before any GET is queued and delivered first when one opens, and the
// client's answer reaches the child; Last-Event-ID replays only what follows; the 257th message drops the oldest and
// logs it.
func TestQueue(t *testing.T) {
	f := newFixture(t, nil, "-sample-on-init")
	sid := f.initialize(`{"sampling":{}}`)
	pid := f.onlyStart().PID
	child := f.b.lookup(sid, nil)
	eventually(t, 5*time.Second, "sampling/createMessage queued", func() bool {
		child.mu.Lock()
		defer child.mu.Unlock()
		return len(child.queue) == 1
	})
	g := f.get(sid, "")
	ev := g.next(5 * time.Second)
	if !strings.Contains(ev.data, `"method":"sampling/createMessage"`) || !strings.Contains(ev.data, `"id":"sample-1"`) {
		t.Fatalf("the first GET event: %s", ev.data)
	}
	if r := f.post(sid, `{"jsonrpc":"2.0","id":"sample-1","result":{"role":"assistant","content":{"type":"text","text":"sampled ok"},"model":"m"}}`); r.status != http.StatusAccepted {
		t.Fatalf("the answer: %d %s", r.status, r.body)
	}
	if b := f.record("sample", pid); !strings.Contains(string(b), "sampled ok") {
		t.Errorf("the child's record of the answer: %s", b)
	}
	g.close()

	// Three notifications while no GET is open: all three on the next GET; a GET with the first's id gets the rest.
	f.call(sid, 2, "notify", `{"n":3}`)
	g2 := f.get(sid, "")
	e1, e2, e3 := g2.next(5*time.Second), g2.next(5*time.Second), g2.next(5*time.Second)
	if !strings.Contains(e1.data, "note 1") || !strings.Contains(e2.data, "note 2") || !strings.Contains(e3.data, "note 3") {
		t.Fatalf("queued notes: %s %s %s", e1.data, e2.data, e3.data)
	}
	g2.none(200 * time.Millisecond)
	g2.close()
	g3 := f.get(sid, e1.id)
	if a, b := g3.next(5*time.Second), g3.next(5*time.Second); a.id != e2.id || b.id != e3.id {
		t.Errorf("replay after %s: %s %s, want %s %s", e1.id, a.id, b.id, e2.id, e3.id)
	}
	g3.none(200 * time.Millisecond)
	// Another child's event id is not this child's: the GET starts after what was delivered.
	g3.close()
	g4 := f.get(sid, "12345-1")
	g4.none(200 * time.Millisecond)
	g4.close()

	// Overflow: 257 messages before a GET; the oldest is dropped and logged, the GET gets the other 256 in order.
	f2 := newFixture(t, nil)
	sid2 := f2.initialize(`{}`)
	f2.call(sid2, 2, "notify", `{"n":257}`)
	if !strings.Contains(f2.logs.String(), "the GET queue is full") || strings.Count(f2.logs.String(), "the GET queue is full") != 1 {
		t.Errorf("overflow log: %d lines", strings.Count(f2.logs.String(), "the GET queue is full"))
	}
	g5 := f2.get(sid2, "")
	for i := 2; i <= 257; i++ {
		if ev := g5.next(5 * time.Second); !strings.Contains(ev.data, fmt.Sprintf(`"note %d"`, i)) {
			t.Fatalf("event for note %d: %s", i, ev.data)
		}
	}
	g5.none(200 * time.Millisecond)
}

// Session end (§10): DELETE stops the child within 5 s of closing its stdin and the session is 404; a crashed
// child's session is 404; the 9th initialize is 503 with Kontra-Node: full.
func TestSessionEnd(t *testing.T) {
	f := newFixture(t, nil)
	sid := f.initialize(`{}`)
	pid := f.onlyStart().PID
	start := time.Now()
	if r := f.do(http.MethodDelete, sid, mcpwire.V20251125, ""); r.status != http.StatusOK {
		t.Fatalf("DELETE: %d %s", r.status, r.body)
	}
	eventually(t, 5*time.Second, "the child stopped after DELETE", func() bool { return !alive(pid) })
	t.Logf("the child stopped %v after DELETE", time.Since(start).Round(time.Millisecond))
	if r := f.call(sid, 1, "echo", `{"text":"x"}`); r.status != http.StatusNotFound || !strings.Contains(r.body, "-32001") {
		t.Errorf("after DELETE: %d %s", r.status, r.body)
	}
	if r := f.do(http.MethodGet, sid, mcpwire.V20251125, ""); r.status != http.StatusNotFound {
		t.Errorf("GET after DELETE: %d", r.status)
	}
	if r := f.do(http.MethodDelete, sid, mcpwire.V20251125, ""); r.status != http.StatusNotFound {
		t.Errorf("DELETE after DELETE: %d", r.status)
	}

	// A crash: the call's stream ends without a response, and the session is 404.
	sid2 := f.initialize(`{}`)
	r := f.call(sid2, 1, "crash", `{}`)
	if r.status != http.StatusOK || len(r.events) != 0 {
		t.Errorf("the crashing call: %d %v", r.status, r.events)
	}
	eventually(t, 5*time.Second, "the crashed session answers 404", func() bool {
		return f.call(sid2, 2, "echo", `{"text":"x"}`).status == http.StatusNotFound
	})
	if !strings.Contains(f.logs.String(), "the stdio server exited") {
		t.Errorf("the crash is not logged")
	}
	eventually(t, 15*time.Second, "the stopped children released", func() bool { return f.b.Sessions() == 0 })

	// At most --max-sessions children: the 9th initialize is 503 full and spawns nothing.
	for i := 0; i < DefaultMaxSessions; i++ {
		f.initialize(`{}`)
	}
	before := len(f.starts())
	r = f.post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	if r.status != http.StatusServiceUnavailable || r.header.Get(tunnel.HeaderNode) != string(tunnel.NodeFull) {
		t.Errorf("the 9th initialize: %d %q %s", r.status, r.header.Get(tunnel.HeaderNode), r.body)
	}
	if len(f.starts()) != before {
		t.Errorf("the refused initialize spawned a child")
	}
}

// D213: a full bridge stops its longest-idle child, one with no exchange for at least EvictIdle, for a new session
// rather than answer full, and waits for its group to be gone so that the count never exceeds --max-sessions; a child
// with an exchange open (a GET stream) or idle for less is never stopped, and with none to stop the initialize is 503
// full. A child the gate's DELETE is stopping already (a principal's longest-idle session closed for its new one,
// relay.md §11.2) is waited for rather than answered full.
func TestFullBridgeStopsItsLongestIdleChild(t *testing.T) {
	old := EvictIdle
	EvictIdle = time.Second
	t.Cleanup(func() { EvictIdle = old })
	f := newFixture(t, func(c *Config) { c.MaxSessions = 3 })
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	s1 := f.initialize(`{}`)
	s2 := f.initialize(`{}`)
	s3 := f.initialize(`{}`)
	f.get(s3, "") // s3 has an exchange open: never idle
	// Every child idle for less than EvictIdle: full, and nothing is stopped.
	f.call(s1, 1, "echo", `{"text":"used just now"}`)
	f.call(s2, 1, "echo", `{"text":"used just now"}`)
	if r := f.post("", init); r.status != http.StatusServiceUnavailable || r.header.Get(tunnel.HeaderNode) != string(tunnel.NodeFull) {
		t.Fatalf("an initialize with no child idle long enough: %d %q %s", r.status, r.header.Get(tunnel.HeaderNode), r.body)
	}
	time.Sleep(EvictIdle / 2)
	f.call(s1, 1, "echo", `{"text":"keep s1 busy"}`) // s2 is now the longest idle
	time.Sleep(EvictIdle)
	s4 := f.initialize(`{}`)
	if r := f.call(s2, 2, "echo", `{"text":"x"}`); r.status != http.StatusNotFound {
		t.Fatalf("the longest-idle session after it was stopped: %d %s", r.status, r.body)
	}
	for _, sid := range []string{s1, s3, s4} {
		if r := f.call(sid, 3, "echo", `{"text":"still here"}`); r.status != http.StatusOK || !strings.Contains(text(t, r), "still here") {
			t.Fatalf("session %s after the eviction: %d %s", sid[:6], r.status, r.body)
		}
	}
	if n := f.b.Sessions(); n != 3 {
		t.Fatalf("%d children counted, want --max-sessions (3)", n)
	}
	if !strings.Contains(f.logs.String(), "its longest-idle child is stopped for a new session") {
		t.Error("the eviction is not logged")
	}

	// A DELETE stops a child (the gate closing a principal's longest-idle session): an initialize right behind it waits
	// for its group rather than answer full, though no child is idle long enough.
	if r := f.do(http.MethodDelete, s1, mcpwire.V20251125, ""); r.status != http.StatusOK {
		t.Fatalf("DELETE: %d", r.status)
	}
	s5 := f.initialize(`{}`)
	if r := f.call(s5, 4, "echo", `{"text":"after a DELETE"}`); r.status != http.StatusOK {
		t.Fatalf("the session opened behind a DELETE: %d %s", r.status, r.body)
	}
	if n := f.b.Sessions(); n > 3 {
		t.Fatalf("%d children counted, over --max-sessions (3)", n)
	}
}

// D184: a child that ignores SIGTERM, keeps running when its stdin closes and has a grandchild that ignores SIGTERM
// too (an npx launcher's node) leaves no process of its group 10 s after DELETE, after idle, after its own crash
// and after Close.
func TestProcessGroupStopped(t *testing.T) {
	for _, how := range []string{"delete", "idle", "crash", "close"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(c *Config) {
				if how == "idle" {
					c.SessionIdle = time.Second
				}
			}, "-fork", "-ignore-term", "-hang")
			sid := f.initialize(`{}`)
			s := f.onlyStart()
			if s.PGID != s.PID || s.Grandchild == 0 {
				t.Fatalf("the child is not a group leader with a grandchild: %+v", s)
			}
			from := time.Now()
			switch how {
			case "delete":
				if r := f.do(http.MethodDelete, sid, mcpwire.V20251125, ""); r.status != http.StatusOK {
					t.Fatalf("DELETE: %d", r.status)
				}
			case "idle":
				from = from.Add(time.Second)
			case "crash":
				f.call(sid, 1, "crash", `{}`)
			case "close":
				done := make(chan struct{})
				go func() { _ = f.b.Close(); close(done) }()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Fatal("Close did not return")
				}
				if groupAlive(s.PGID) || alive(s.Grandchild) {
					t.Fatalf("Close returned with the group alive")
				}
			}
			eventually(t, time.Until(from.Add(TerminateAfter+KillAfter+2*time.Second)), "no process of the group left",
				func() bool { return !groupAlive(s.PGID) && !alive(s.Grandchild) && !alive(s.PID) })
			if how != "close" {
				if took := time.Since(from); took < TerminateAfter+KillAfter-time.Second {
					t.Errorf("the group was gone after %v: SIGTERM is ignored, so only SIGKILL at %v could stop it", took, TerminateAfter+KillAfter)
				}
				if r := f.call(sid, 9, "echo", `{"text":"x"}`); r.status != http.StatusNotFound {
					t.Errorf("the stopped session: %d", r.status)
				}
			}
			logs := f.logs.String()
			if !strings.Contains(logs, "SIGTERM") || !strings.Contains(logs, "SIGKILL") {
				t.Errorf("the log does not name SIGTERM and SIGKILL")
			}
		})
	}
}

// Legacy only (D161): a POST without a session that is not initialize, with 2026-07-28 or a legacy header, is 400 with
// rule 3's -32000 body, and spawns nothing; a 2026-07-28 request with a session id is answered the same.
func TestLegacyOnly(t *testing.T) {
	f := newFixture(t, nil)
	modern := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	for _, v := range []string{mcpwire.V20260728, mcpwire.V20251125, ""} {
		r := f.do(http.MethodPost, "", v, modern, mcpwire.HeaderMethod, "tools/list")
		if r.status != http.StatusBadRequest || r.body != string(mcpwire.LegacyNoSessionBody) {
			t.Errorf("version %q: %d %s", v, r.status, r.body)
		}
	}
	if r := f.do(http.MethodPost, "", mcpwire.V20260728, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`); r.status != http.StatusBadRequest ||
		r.body != string(mcpwire.LegacyNoSessionBody) {
		t.Errorf("server/discover: %d %s", r.status, r.body)
	}
	if len(f.starts()) != 0 {
		t.Fatalf("a request without a session spawned a child")
	}
	sid := f.initialize(`{}`)
	if r := f.do(http.MethodPost, sid, mcpwire.V20260728, modern); r.status != http.StatusBadRequest || r.body != string(mcpwire.LegacyNoSessionBody) {
		t.Errorf("a modern request with a session id: %d %s", r.status, r.body)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if r := f.do(m, "", "", ""); r.status != http.StatusBadRequest || r.body != string(mcpwire.LegacyNoSessionBody) {
			t.Errorf("%s without a session: %d %s", m, r.status, r.body)
		}
	}
}

// Strict decoding (D172): an initialize with "capabilities" and "Capabilities" is 400 with -32600 and spawns no
// child; bytes that are not JSON are -32700; a batch is -32600.
func TestStrict(t *testing.T) {
	f := newFixture(t, nil)
	for _, c := range []struct {
		body string
		code int
		id   any
	}{
		{`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"Capabilities":{"roots":{}}}}`, -32600, float64(0)},
		{`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"ROOTS":{}}}}`, -32600, float64(0)},
		{`{"jsonrpc":"2.0","id":0,"method":"initialize","METHOD":"ping"}`, -32600, nil},
		{`{"jsonrpc":"2.0",`, -32700, nil},
		{`{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`, -32600, nil},
		{`[{"jsonrpc":"2.0","id":0,"method":"initialize"}]`, -32600, nil},
	} {
		r := f.post("", c.body)
		m := r.last()
		e, _ := m["error"].(map[string]any)
		if r.status != http.StatusBadRequest || e == nil || e["code"] != float64(c.code) || m["id"] != c.id {
			t.Errorf("%s: %d %s", c.body, r.status, r.body)
		}
	}
	if len(f.starts()) != 0 || f.b.Sessions() != 0 {
		t.Fatalf("a refused initialize spawned a child")
	}
	sid := f.initialize(`{}`)
	pid := f.onlyStart().PID
	if r := f.post(sid, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","NAME":"crash"}}`); r.status != http.StatusBadRequest ||
		!strings.Contains(r.body, `"id":5`) {
		t.Errorf("a case variant on a session: %d %s", r.status, r.body)
	}
	if text(t, f.call(sid, 6, "echo", `{"text":"alive"}`)) != "alive" {
		t.Errorf("the session did not survive")
	}
	for _, line := range f.received(pid) {
		if strings.Contains(line, "NAME") {
			t.Errorf("the child received a refused message: %s", line)
		}
	}
}

// Environment and stderr (§10): the child sees only PATH, HOME, LANG and the --env names; a child writing 5 MiB to
// stderr is capped at 1 MiB in the log, and its stdout keeps flowing.
func TestEnvironmentAndStderr(t *testing.T) {
	t.Setenv("KONTRA_TEST_SECRET", "must-not-leak")
	t.Setenv("KONTRA_TEST_PASSED", "passed")
	stderr := &syncBuf{}
	f := newFixture(t, func(c *Config) {
		c.Env = []string{"KONTRA_TEST_PASSED", "KONTRA_TEST_UNSET_X"}
		c.Stderr = stderr
	})
	sid := f.initialize(`{}`)
	var names []string
	for _, kv := range strings.Split(text(t, f.call(sid, 1, "env", `{}`)), "\n") {
		k, v, _ := strings.Cut(kv, "=")
		names = append(names, k)
		if k == "KONTRA_TEST_PASSED" && v != "passed" {
			t.Errorf("KONTRA_TEST_PASSED=%q", v)
		}
	}
	var want []string
	for _, k := range []string{"HOME", "KONTRA_TEST_PASSED", "LANG", "PATH"} {
		if _, ok := os.LookupEnv(k); ok {
			want = append(want, k)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Errorf("the child's environment: %q, want %q", names, want)
	}

	start := time.Now()
	if r := f.call(sid, 2, "stderr", fmt.Sprintf(`{"bytes":%d}`, 5<<20)); text(t, r) != "wrote stderr" {
		t.Fatalf("stderr call: %v", r.events)
	}
	if r := f.call(sid, 3, "echo", `{"text":"still flowing"}`); text(t, r) != "still flowing" {
		t.Fatalf("stdout after the stderr flood: %v", r.events)
	}
	if time.Since(start) > 50*time.Second {
		t.Skip("the flood took most of a minute: the cap's window may have rolled over")
	}
	if n := stderr.Len(); n > StderrPerMinute+4<<10 || n < StderrPerMinute-8<<10 {
		t.Errorf("%d bytes of stderr logged, want about %d", n, StderrPerMinute)
	}
	if !strings.Contains(f.logs.String(), "more than 1 MiB a minute") {
		t.Errorf("the cap is not logged")
	}
}

// Roots stay on the node (D173, stdio_roots): the double asks roots/list right after notifications/initialized and
// records the answer: the configured directories, in order (a comma in a name kept), or [] without --roots; the
// initialize it saw declared roots {} or none; the client, which declared roots, never receives roots/list; its
// notifications/roots/list_changed and its answer to the node-answered id are 202 and never reach the child.
func TestRoots(t *testing.T) {
	dirA := t.TempDir()
	dirC := filepath.Join(t.TempDir(), "c, with a comma")
	if err := os.Mkdir(dirC, 0o700); err != nil {
		t.Fatal(err)
	}
	flags := func(f ...string) *mcproots.Roots {
		m, err := mcproots.Parse(f) // as kontra connect reads --roots fs=<dir>, one directory per flag
		if err != nil {
			t.Fatal(err)
		}
		return m["fs"]
	}
	for _, c := range []struct {
		name  string
		roots *mcproots.Roots
		want  []string
	}{
		{"one root", flags("fs=" + dirA), []string{dirA}},
		{"two roots", flags("fs="+dirA, "fs="+dirC), []string{dirA, dirC}},
		{"none", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, func(cfg *Config) { cfg.Roots = c.roots })
			sid := f.initialize(`{"roots":{"listChanged":true},"sampling":{}}`)
			g := f.get(sid, "")
			pid := f.onlyStart().PID

			var answer struct {
				Roots []struct{ URI, Name string } `json:"roots"`
			}
			if err := json.Unmarshal(f.record("roots", pid), &answer); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range answer.Roots {
				u, err := url.Parse(r.URI)
				if err != nil || u.Scheme != "file" || u.Host != "" {
					t.Fatalf("root URI %q", r.URI)
				}
				got = append(got, u.Path)
			}
			if !slices.Equal(got, c.want) || answer.Roots == nil {
				t.Errorf("the child's roots: %q (%v), want %q", got, answer.Roots, c.want)
			}
			var init struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			if err := json.Unmarshal(f.record("init", pid), &init); err != nil {
				t.Fatal(err)
			}
			roots, declared := init.Capabilities["roots"]
			if (c.roots != nil) != declared || (declared && string(roots) != "{}") || init.Capabilities["sampling"] == nil {
				t.Errorf("the initialize the child saw declared %v", init.Capabilities)
			}

			if r := f.post(sid, `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`); r.status != http.StatusAccepted {
				t.Errorf("list_changed: %d", r.status)
			}
			if r := f.post(sid, `{"jsonrpc":"2.0","id":"roots-1","result":{"roots":[{"uri":"file:///"}]}}`); r.status != http.StatusAccepted {
				t.Errorf("the client's answer to roots-1: %d", r.status)
			}
			if text(t, f.call(sid, 9, "echo", `{"text":"sync"}`)) != "sync" {
				t.Fatal("echo")
			}
			answers := 0
			for _, line := range f.received(pid) {
				if strings.Contains(line, "list_changed") || strings.Contains(line, `"file:///"`) {
					t.Errorf("the child received %s", line)
				}
				if strings.Contains(line, `"roots-1"`) {
					answers++
				}
			}
			if answers != 1 {
				t.Errorf("the child received %d answers to roots-1, want the node's one", answers)
			}
			g.none(200 * time.Millisecond)
		})
	}
}

// A request id still in flight is refused, and a request whose response was malformed ends without one.
func TestDuplicateIDAndStdinBusy(t *testing.T) {
	f := newFixture(t, nil)
	sid := f.initialize(`{}`)
	held := make(chan reply, 1)
	go func() {
		held <- f.post(sid, `{"jsonrpc":"2.0","id":"same","method":"tasks/result","params":{"taskId":"tz"}}`)
	}()
	child := f.b.lookup(sid, nil)
	eventually(t, 5*time.Second, "the first request in flight", func() bool {
		child.mu.Lock()
		defer child.mu.Unlock()
		return len(child.pending) == 1
	})
	if r := f.post(sid, `{"jsonrpc":"2.0","id":"same","method":"ping"}`); r.status != http.StatusBadRequest || !strings.Contains(r.body, "-32600") {
		t.Errorf("a duplicate id: %d %s", r.status, r.body)
	}
	f.call(sid, 1, "related", `{"taskId":"tz"}`)
	if r := <-held; len(r.events) != 2 {
		t.Errorf("the held request: %v", r.events)
	}
	if errors.Is(child.send([]byte(`{"x":`)), errBusy) {
		t.Errorf("invalid JSON taken for a busy child")
	}
}

// Roots answered ahead (M3 WP-05 review, D173): a client, or a gate forging messages, that answers the server's
// roots/list before the server asks (its ids are predictable) never reaches the child, whatever the order: the node
// forwards a client response only when it answers a request the node relayed to the client. An answer to an id the
// server never sent is dropped too; the answer to a relayed sampling/createMessage still reaches the child, and a
// result with a roots member does not, even under that id.
func TestRootsAnswerAhead(t *testing.T) {
	dirA := t.TempDir()
	f := newFixture(t, func(c *Config) { c.Roots = mcproots.New(dirA) }, "-sample-on-init")
	r := f.post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":{"roots":{"listChanged":true},"sampling":{}},"clientInfo":{"name":"test","version":"1"}}}`)
	sid := r.header.Get(mcpwire.HeaderSessionID)
	if r.status != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d %s", r.status, r.body)
	}
	pid := f.onlyStart().PID
	// Before notifications/initialized: the child has asked nothing yet.
	for _, forged := range []string{
		`{"jsonrpc":"2.0","id":"roots-1","result":{"roots":[{"uri":"file:///"}]}}`,
		`{"jsonrpc":"2.0","id":0,"result":{"roots":[{"uri":"file:///"}]}}`,
		`{"jsonrpc":"2.0","id":"never-asked","result":{}}`,
	} {
		if r := f.post(sid, forged); r.status != http.StatusAccepted || r.body != "" {
			t.Errorf("%s: %d %q", forged, r.status, r.body)
		}
	}
	if r := f.post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != http.StatusAccepted {
		t.Fatalf("notifications/initialized: %d", r.status)
	}
	if b := f.record("roots", pid); strings.Contains(string(b), `"file:///"`) || !strings.Contains(string(b), "file://"+dirA) {
		t.Errorf("the child's roots: %s", b)
	}
	g := f.get(sid, "")
	if ev := g.next(5 * time.Second); !strings.Contains(ev.data, `"id":"sample-1"`) {
		t.Fatalf("the GET's first event: %s", ev.data)
	}
	if r := f.post(sid, `{"jsonrpc":"2.0","id":"sample-1","result":{"roots":[{"uri":"file:///"}]}}`); r.status != http.StatusAccepted {
		t.Errorf("a roots result under the sampling id: %d", r.status)
	}
	if r := f.post(sid, `{"jsonrpc":"2.0","id":"sample-1","result":{"role":"assistant","content":{"type":"text","text":"sampled"},"model":"m"}}`); r.status != http.StatusAccepted {
		t.Fatalf("the sampling answer: %d", r.status)
	}
	if b := f.record("sample", pid); !strings.Contains(string(b), "sampled") {
		t.Errorf("the child's sampling answer: %s", b)
	}
	if text(t, f.call(sid, 9, "echo", `{"text":"sync"}`)) != "sync" {
		t.Fatal("echo")
	}
	for _, line := range f.received(pid) {
		if strings.Contains(line, `"file:///"`) || strings.Contains(line, "never-asked") || strings.Contains(line, `"id":0,"result"`) {
			t.Errorf("the child received %s", line)
		}
	}
}

// Resumability (MCP 2025-11-25: "The server MUST NOT replay messages that would have been delivered on a different
// stream"): a GET whose Last-Event-ID names a POST stream's event replays none of the GET stream's messages, which
// were delivered on the open GET; a GET event's id still replays what follows it.
func TestReplayOnlyTheGETStream(t *testing.T) {
	f := newFixture(t, nil)
	sid := f.initialize(`{}`)
	g := f.get(sid, "")
	r := f.call(sid, 2, "progress", `{"n":1}`, `{"progressToken":"tok"}`)
	if len(r.events) != 2 {
		t.Fatalf("progress: %v", r.events)
	}
	postID := r.events[0].id
	f.call(sid, 3, "notify", `{"n":2}`)
	e1, e2 := g.next(5*time.Second), g.next(5*time.Second)
	if !strings.Contains(e1.data, "note 1") || !strings.Contains(e2.data, "note 2") {
		t.Fatalf("the GET stream: %s %s", e1.data, e2.data)
	}
	g2 := f.get(sid, postID)
	g2.none(300 * time.Millisecond)
	f.call(sid, 4, "notify", `{"n":1}`)
	if ev := g2.next(5 * time.Second); !strings.Contains(ev.data, "note 1") || ev.id == e1.id {
		t.Errorf("the next GET event: %s %s", ev.id, ev.data)
	}
	g2.close()
	g3 := f.get(sid, e1.id)
	if ev := g3.next(5 * time.Second); ev.id != e2.id {
		t.Errorf("replay after the GET event %s: %s, want %s", e1.id, ev.id, e2.id)
	}
}
