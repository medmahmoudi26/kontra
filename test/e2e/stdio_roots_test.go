package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kontra.sh/kontra/internal/nodekey"
)

// stdio_roots (spec/tunnel.md §10, §16; D173, D184, D185): server-filesystem@1.0.0 over stdio, started with <dirB>
// and served with --roots fs=<dirA>, behind a node whose tunnel is on replica B, the client on replica A. The client
// declares roots and answers the server's first request id with roots of file:/// anyway: its first tools/call is
// answered in under 2 s, it never receives roots/list (on its POST streams or its GET stream),
// list_allowed_directories names only <dirA>, a file of <dirA> reads, and read_text_file of the node's
// ~/.kontra/identity.key is an error. The stdio child runs in a process group of its own with only PATH, HOME, LANG
// and the run's markers in its environment.
func TestStdioRoots(t *testing.T) {
	h := start(t, Options{MCP: map[string]MCPService{"fs": {
		ToolsDeny: []string{"write_file", "edit_file", "create_directory", "move_file"}}}, Peer: true})
	ctx := ctxFor(t, 5*time.Minute)
	dirA, dirB := tempDir(t, h, "roots-a"), tempDir(t, h, "roots-b")
	writeFile(t, dirA, "hello.txt", "hello from dir A")
	writeFile(t, dirB, "secret.txt", "dir B is not served")
	h.Front.Route(h.Gates[1])
	n, _ := startNode(t, ctx, h, "fs", nil, func(o *NodeOptions) {
		o.MCP = map[string]string{"fs": h.FilesystemCommand(t, dirB)}
		o.Roots = []string{"fs=" + dirA}
	})
	identity := filepath.Join(n.Home, nodekey.DirName, "identity.key")
	if _, err := os.Stat(identity); err != nil {
		t.Fatalf("the node's identity key: %v", err)
	}

	mc := NewMCPClient(h.Gates[0].URL+"/mcp/fs", h.APIKey)
	mc.Capabilities = `{"roots":{"listChanged":true},"elicitation":{}}`
	init, err := mc.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(init.Text(), "secure-filesystem-server") {
		t.Fatalf("initialize: %s", init.Text())
	}
	gctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stream, err := mc.Get(gctx, "")
	if err != nil || stream.Status != http.StatusOK {
		t.Fatalf("the GET stream: %v", err)
	}
	var received []SSEEvent
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for {
			ev, err := stream.Next(gctx)
			if err != nil {
				return
			}
			received = append(received, ev)
		}
	}()
	// A client that would answer roots/list with file:///, sent ahead of any request (the TS SDK numbers its first
	// request 0): dropped at the node.
	if r, err := mc.Post(ctx, `{"jsonrpc":"2.0","id":0,"result":{"roots":[{"uri":"file:///"}]}}`); err != nil || r.Status != http.StatusAccepted {
		t.Fatalf("the forged roots answer: %v %+v", err, r)
	}
	var posts []*MCPResponse
	call := func(id int, tool string, args map[string]any) (string, bool) {
		t.Helper()
		r, err := mc.Call(ctx, id, tool, args)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != http.StatusOK {
			t.Fatalf("%s: %d %s", tool, r.Status, r.Text())
		}
		posts = append(posts, r)
		return r.ToolText()
	}
	started := time.Now()
	text, isErr := call(1, "list_allowed_directories", map[string]any{})
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the first tools/call took %v, want under 2 s", took)
	}
	if isErr || !strings.Contains(text, dirA) || strings.Contains(text, dirB) || slices.Contains(strings.Split(text, "\n"), "/") {
		t.Fatalf("list_allowed_directories: %q", text)
	}
	if text, isErr := call(2, "read_text_file", map[string]any{"path": filepath.Join(dirA, "hello.txt")}); isErr || text != "hello from dir A" {
		t.Fatalf("a file of dir A: %q %v", text, isErr)
	}
	if text, isErr := call(3, "read_text_file", map[string]any{"path": identity}); !isErr || strings.Contains(text, "AGE-SECRET-KEY") {
		t.Fatalf("the node's identity key: %q %v", text, isErr)
	}
	if text, isErr := call(4, "read_text_file", map[string]any{"path": filepath.Join(dirB, "secret.txt")}); !isErr || strings.Contains(text, "not served") {
		t.Fatalf("a file of dir B: %q %v", text, isErr)
	}
	time.Sleep(2 * time.Second) // what the GET stream would carry by now
	cancel()
	<-streamDone
	for _, r := range posts {
		for _, m := range r.Messages() {
			if m["method"] == "roots/list" {
				t.Fatalf("a POST stream carried roots/list: %s", r.Text())
			}
		}
	}
	for _, ev := range received {
		if ev.Message()["method"] == "roots/list" {
			t.Fatalf("the GET stream carried roots/list: %s", ev.Data)
		}
	}

	// The stdio child: its own process group, a minimal environment with the run's markers (D184, D185).
	children := childrenOf(n.Pid())
	if len(children) == 0 {
		t.Fatal("the node runs no stdio child")
	}
	for _, pid := range children {
		pg, err := syscall.Getpgid(pid)
		if err != nil || pg != pid {
			t.Errorf("stdio child %d is in process group %d, want its own", pid, pg)
		}
		env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, kv := range bytes.Split(env, []byte{0}) {
			if name, _, ok := strings.Cut(string(kv), "="); ok {
				names = append(names, name)
			}
		}
		allowed := []string{"PATH", "HOME", "LANG", RunEnv, SessionEnv}
		for _, name := range names {
			if !slices.Contains(allowed, name) {
				t.Errorf("stdio child %d has %s in its environment", pid, name)
			}
		}
		if !slices.Contains(names, RunEnv) {
			t.Errorf("stdio child %d does not carry %s (the leak check could not find it)", pid, RunEnv)
		}
	}
	b, _ := json.Marshal(children)
	t.Logf("stdio children of the node: %s", b)
}

// childrenOf lists the processes whose parent is pid.
func childrenOf(pid int) []int {
	var out []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) > 1 && f[1] == strconv.Itoa(pid) {
			out = append(out, p)
		}
	}
	return out
}
