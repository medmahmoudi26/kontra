package mcproots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/mcpwire"
)

func parse(t *testing.T, src mcpwire.Source, body string) *mcpwire.Message {
	t.Helper()
	m, err := mcpwire.Parse([]byte(body), src)
	if err != nil {
		t.Fatalf("Parse(%s): %v", body, err)
	}
	return m
}

// jsonEq compares two JSON values by meaning.
func jsonEq(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("not JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want: %s", want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// --roots: absolute existing directories, repeatable, one per flag, in flag order, a comma in a name kept.
func TestParse(t *testing.T) {
	a, c := t.TempDir(), filepath.Join(t.TempDir(), "with,comma")
	if err := os.Mkdir(c, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(a, "file")
	os.WriteFile(file, nil, 0o600)
	got, err := Parse([]string{"fs=" + a, "fs=" + c, "other=" + a + "/", "fs=" + a})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got["fs"].Dirs(), []string{a, c}) || !slices.Equal(got["other"].Dirs(), []string{a}) || len(got) != 2 {
		t.Fatalf("Parse: fs %v, other %v (%d services)", got["fs"].Dirs(), got["other"].Dirs(), len(got))
	}
	for _, bad := range []struct{ flag, want string }{
		{"fs", "want <service>="},
		{"=" + a, "want <service>="},
		{"fs=", "want <service>="},
		{"fs=relative/dir", "absolute"},
		{"fs=" + filepath.Join(a, "missing"), "no such file"},
		{"fs=" + file, "not a directory"},
	} {
		if _, err := Parse([]string{bad.flag}); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("Parse(%q): %v, want %q", bad.flag, err, bad.want)
		}
	}
	if got, err := Parse(nil); err != nil || len(got) != 0 {
		t.Errorf("no flags: %v %v", got, err)
	}
}

// The node's answer: the directories as file:// URIs in order, an empty list without roots; the capability {} only
// when roots are configured.
func TestListResultAndCapability(t *testing.T) {
	jsonEq(t, New("/srv/a", "/srv/b,c", "/srv/d e").ListResult(),
		`{"roots":[{"uri":"file:///srv/a","name":"a"},{"uri":"file:///srv/b,c","name":"b,c"},{"uri":"file:///srv/d%20e","name":"d e"}]}`)
	jsonEq(t, (*Roots)(nil).ListResult(), `{"roots":[]}`)
	jsonEq(t, New().ListResult(), `{"roots":[]}`)
	if c := New("/a").Capability(); string(c) != `{}` {
		t.Errorf("capability with roots: %s", c)
	}
	if c := (*Roots)(nil).Capability(); c != nil {
		t.Errorf("capability without roots: %s", c)
	}
}

// From the client (spec/tunnel.md §10): the initialize's roots capability is the node's; list_changed and the
// answer to a node-answered roots/list are dropped; the modern _meta and inputResponses lose roots.
func TestFromClient(t *testing.T) {
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{"sampling":{},"roots":{"listChanged":true}},"clientInfo":{"name":"c","version":"1"}}}`
	for _, c := range []struct {
		roots *Roots
		want  string
	}{
		{New("/srv/a"), `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"sampling":{},"roots":{}},"clientInfo":{"name":"c","version":"1"}}}`},
		{nil, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"sampling":{}},"clientInfo":{"name":"c","version":"1"}}}`},
	} {
		s := NewSession(c.roots)
		d, err := s.FromClient(mcpwire.EraLegacy, []byte(init), parse(t, mcpwire.FromClient, init))
		if err != nil || d.Action != Forward {
			t.Fatalf("initialize: %v %v", d.Action, err)
		}
		jsonEq(t, d.Body, c.want)
	}
	// A client that declared no capabilities still gets the node's roots declared, and no roots without them.
	bare := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`
	d, err := NewSession(New("/srv/a")).FromClient(mcpwire.EraLegacy, []byte(bare), parse(t, mcpwire.FromClient, bare))
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, d.Body, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{}}}}`)
	if d, _ := NewSession(nil).FromClient(mcpwire.EraLegacy, []byte(bare), parse(t, mcpwire.FromClient, bare)); string(d.Body) != bare {
		t.Errorf("an initialize with nothing to change was rewritten: %s", d.Body)
	}

	s := NewSession(New("/srv/a"))
	changed := `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`
	if d, _ := s.FromClient(mcpwire.EraLegacy, []byte(changed), parse(t, mcpwire.FromClient, changed)); d.Action != Drop {
		t.Errorf("list_changed: %v", d.Action)
	}
	// The server asks roots/list "r1": the node answers it; the client's own answer to r1 (as 1.0 or "r1") is
	// dropped, an answer to another id is forwarded unchanged.
	ask := `{"jsonrpc":"2.0","id":"r1","method":"roots/list"}`
	d, err = s.FromServer(mcpwire.EraLegacy, []byte(ask), parse(t, mcpwire.FromUpstream, ask))
	if err != nil || d.Action != Answer {
		t.Fatalf("roots/list: %v %v", d.Action, err)
	}
	jsonEq(t, d.Reply, `{"jsonrpc":"2.0","id":"r1","result":{"roots":[{"uri":"file:///srv/a","name":"a"}]}}`)
	ask2 := `{"jsonrpc":"2.0","id":7,"method":"roots/list","params":{}}`
	if d, _ := s.FromServer(mcpwire.EraLegacy, []byte(ask2), parse(t, mcpwire.FromUpstream, ask2)); d.Action != Answer {
		t.Fatalf("roots/list 7: %v", d.Action)
	}
	for _, ans := range []string{
		`{"jsonrpc":"2.0","id":"r1","result":{"roots":[{"uri":"file:///"}]}}`,
		`{"jsonrpc":"2.0","id":7.0,"result":{"roots":[{"uri":"file:///"}]}}`,
		`{"jsonrpc":"2.0","id":"r1","error":{"code":-32601,"message":"no"}}`,
	} {
		if d, _ := s.FromClient(mcpwire.EraLegacy, []byte(ans), parse(t, mcpwire.FromClient, ans)); d.Action != Drop {
			t.Errorf("%s: %v, want drop", ans, d.Action)
		}
	}
	other := `{"jsonrpc":"2.0","id":"s1","result":{"model":"m","role":"assistant","content":{"type":"text","text":"x"}}}`
	if d, _ := s.FromClient(mcpwire.EraLegacy, []byte(other), parse(t, mcpwire.FromClient, other)); d.Action != Forward || string(d.Body) != other {
		t.Errorf("another response: %v %s", d.Action, d.Body)
	}
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"roots":"kept"}}}`
	if d, _ := s.FromClient(mcpwire.EraLegacy, []byte(call), parse(t, mcpwire.FromClient, call)); d.Action != Forward || string(d.Body) != call {
		t.Errorf("a call: %v %s", d.Action, d.Body)
	}

	// Modern: roots leaves clientCapabilities, and an inputResponses entry answering roots (any case) is removed;
	// the rest of the request, member order included, is kept.
	modern := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x","_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"roots":{},"elicitation":{}}},` +
		`"inputResponses":{"a":{"roots":[{"uri":"file:///"}]},"b":{"action":"accept","content":{}},"c":{"ROOTS":[]}},"requestState":"s"}}`
	d, err = s.FromClient(mcpwire.EraModern, []byte(modern), parse(t, mcpwire.FromClient, modern))
	if err != nil || d.Action != Forward {
		t.Fatalf("modern: %v %v", d.Action, err)
	}
	want := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x","_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}},` +
		`"inputResponses":{"b":{"action":"accept","content":{}}},"requestState":"s"}}`
	if string(d.Body) != want {
		t.Errorf("modern:\n got %s\nwant %s", d.Body, want)
	}
}

// From the server: roots/list is answered on a legacy session and dropped on a modern exchange; an inputRequests
// entry asking roots/list is removed, the elicitation beside it kept.
func TestFromServer(t *testing.T) {
	s := NewSession(nil)
	ask := `{"jsonrpc":"2.0","id":"r1","method":"roots/list"}`
	d, err := s.FromServer(mcpwire.EraLegacy, []byte(ask), parse(t, mcpwire.FromUpstream, ask))
	if err != nil || d.Action != Answer {
		t.Fatalf("legacy roots/list: %v %v", d.Action, err)
	}
	jsonEq(t, d.Reply, `{"jsonrpc":"2.0","id":"r1","result":{"roots":[]}}`)
	if d, _ := NewSession(nil).FromServer(mcpwire.EraModern, []byte(ask), parse(t, mcpwire.FromUpstream, ask)); d.Action != Drop {
		t.Errorf("modern roots/list: %v", d.Action)
	}
	ir := `{"jsonrpc":"2.0","id":5,"result":{"resultType":"input_required","inputRequests":{` +
		`"k1":{"method":"roots/list","params":{}},"k2":{"method":"elicitation/create","params":{"message":"ok?","requestedSchema":{"type":"object"}}}},"requestState":"z"}}`
	d, err = s.FromServer(mcpwire.EraModern, []byte(ir), parse(t, mcpwire.FromUpstream, ir))
	if err != nil || d.Action != Forward {
		t.Fatalf("InputRequiredResult: %v %v", d.Action, err)
	}
	want := `{"jsonrpc":"2.0","id":5,"result":{"resultType":"input_required","inputRequests":{` +
		`"k2":{"method":"elicitation/create","params":{"message":"ok?","requestedSchema":{"type":"object"}}}},"requestState":"z"}}`
	if string(d.Body) != want {
		t.Errorf("InputRequiredResult:\n got %s\nwant %s", d.Body, want)
	}
	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"roots/list"}]}}`,
		`{"jsonrpc":"2.0","id":"e1","method":"elicitation/create","params":{"message":"m","requestedSchema":{"type":"object"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"x"}}`,
	} {
		if d, _ := s.FromServer(mcpwire.EraLegacy, []byte(msg), parse(t, mcpwire.FromUpstream, msg)); d.Action != Forward || string(d.Body) != msg {
			t.Errorf("%s: %v %s", msg, d.Action, d.Body)
		}
	}
}

// A session forgets the oldest answered ids past its bound, never the newest.
func TestAnsweredIsBounded(t *testing.T) {
	s := NewSession(nil)
	for i := 0; i < maxAnswered+10; i++ {
		s.remember(json.RawMessage(itoa(i)))
	}
	if s.Answered(json.RawMessage("5")) || !s.Answered(json.RawMessage("10")) || !s.Answered(json.RawMessage(itoa(maxAnswered+9))) || len(s.answered) != maxAnswered {
		t.Errorf("answered: %d ids", len(s.answered))
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
