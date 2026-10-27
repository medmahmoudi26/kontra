// Package mcproots keeps a hosted MCP server's roots on the node (spec/tunnel.md §10, spec/relay.md §11.10,
// D173). server-filesystem@1.0.0 replaces the directories it was started with by the client's roots, so roots
// from the remote client, or forged by the gate, would decide what the node's filesystem exposes: the identity
// key, the job keys, and code execution through a write to ~/.bashrc. For every MCP server a node hosts, stdio
// (internal/stdiobridge) or mcp:<name>=<url> (internal/node's fixed-target proxy), the node is the server's only
// source of roots:
//
//   - kontra connect --roots <name>=<dir> names one root of a service: an absolute path to a directory that
//     exists at start. The flag is repeatable, one directory per flag (--roots fs=/a --roots fs=/b), so a
//     directory's name may hold a comma; a service's roots are those of every flag naming it, in order. None by
//     default.
//   - In the initialize it forwards, the client's capabilities.roots is replaced by the node's own: {} (no
//     listChanged) when roots are configured, absent otherwise.
//   - The server's roots/list is answered by the node with the configured directories as file:// URIs (an empty
//     list when there are none) and never relayed to the remote client: the stdio bridge writes the answer to the
//     child's stdin; the proxy of a URL service removes the event from the response stream and POSTs the answer
//     to the target on the same upstream session.
//   - The client's notifications/roots/list_changed, and any client response whose id is that of a roots/list
//     the node answered, are dropped with 202.
//   - On a 2026-07-28 exchange (URL services; the bridge is legacy only) roots is removed from the forwarded
//     _meta["io.modelcontextprotocol/clientCapabilities"], every params.inputResponses entry that carries roots is
//     removed, and every roots/list entry of an upstream inputRequests is removed. Configured roots are offered to
//     legacy sessions only.
//
// Messages are read with exact keys (internal/mcpwire.Parse, D172); one with a repeated or case-variant key is
// answered 400 with -32600 and not forwarded, by the caller, before Session sees it. The removals here also take
// every case variant of the member they remove: a server that decodes with encoding/json would read one as the
// member.
//
// Work package: M3 WP-05.
package mcproots

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"kontra.sh/kontra/internal/mcpwire"
)

// Methods the filter acts on.
const (
	MethodRootsList    = "roots/list"
	MethodRootsChanged = "notifications/roots/list_changed"
)

// maxAnswered bounds the ids of node-answered roots/list requests a session remembers (the oldest are forgotten).
const maxAnswered = 1024

// Roots is one hosted service's configured roots.
type Roots struct {
	dirs []string // absolute, existing, cleaned
}

// New returns roots of dirs as they are (Parse checks them); a test helper and the stdio bridge's Config build them
// so.
func New(dirs ...string) *Roots { return &Roots{dirs: append([]string(nil), dirs...)} }

// Parse reads every --roots value, each <name>=<dir> with one directory, and returns each service's roots with
// its directories in flag order, refusing a relative path or a directory that does not exist. kontra connect
// refuses a name that no --serve mcp: serves (a usage error). A directory named twice for one service is kept once,
// at its first place.
func Parse(flags []string) (map[string]*Roots, error) {
	out := map[string]*Roots{}
	for _, f := range flags {
		name, dir, ok := strings.Cut(f, "=")
		if !ok || name == "" || dir == "" {
			return nil, fmt.Errorf("--roots %q: want <service>=<absolute directory>", clip(f))
		}
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("--roots %s=%q: not an absolute path", name, clip(dir))
		}
		dir = filepath.Clean(dir)
		st, err := os.Stat(dir)
		switch {
		case err != nil:
			return nil, fmt.Errorf("--roots %s=%q: %w", name, clip(dir), errors.Unwrap(err))
		case !st.IsDir():
			return nil, fmt.Errorf("--roots %s=%q: not a directory", name, clip(dir))
		}
		r := out[name]
		if r == nil {
			r = &Roots{}
			out[name] = r
		}
		if !containsString(r.dirs, dir) {
			r.dirs = append(r.dirs, dir)
		}
	}
	return out, nil
}

// Dirs are the configured directories, in order.
func (r *Roots) Dirs() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.dirs...)
}

// Capability is what the node declares as capabilities.roots in a forwarded initialize: {} when roots are
// configured, nil (the member removed) otherwise.
func (r *Roots) Capability() json.RawMessage {
	if r == nil || len(r.dirs) == 0 {
		return nil
	}
	return json.RawMessage(`{}`)
}

// ListResult is the result of the node's answer to roots/list: {"roots": [{"uri": "file:///…", "name": "…"}]}, the
// configured directories in order, an empty list when there are none.
func (r *Roots) ListResult() json.RawMessage {
	type root struct {
		URI  string `json:"uri"`
		Name string `json:"name,omitempty"`
	}
	list := []root{}
	for _, d := range r.Dirs() {
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(d)}
		list = append(list, root{URI: u.String(), Name: filepath.Base(d)})
	}
	b, _ := json.Marshal(struct {
		Roots []root `json:"roots"`
	}{list})
	return b
}

// Action is what to do with one message the node sees.
type Action int

// Actions.
const (
	Forward Action = iota + 1 // forward Body (the message, possibly rewritten)
	Drop                      // answer the sender 202 (client side) or remove the event (server side); forward nothing
	Answer                    // send Reply to the server instead of relaying the message to the client
)

func (a Action) String() string {
	switch a {
	case Forward:
		return "forward"
	case Drop:
		return "drop"
	case Answer:
		return "answer"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// Decision is FromClient's or FromServer's verdict.
type Decision struct {
	Action Action
	Body   []byte // Forward: the message to forward, the input bytes themselves when nothing changed
	Reply  []byte // Answer: the node's response to the server's roots/list, one JSON-RPC message
}

// Session is the roots state of one MCP session (stdio child or upstream session): the ids of the roots/list
// requests the node answered, so that a client response carrying one is dropped. It is safe for concurrent use.
type Session struct {
	roots *Roots

	mu       sync.Mutex
	answered map[string]struct{}
	order    []string
}

// NewSession starts the roots state of one session; r nil means no roots configured.
func NewSession(r *Roots) *Session {
	return &Session{roots: r, answered: map[string]struct{}{}}
}

// Answered reports whether id is that of a roots/list the node answered on this session.
func (s *Session) Answered(id json.RawMessage) bool {
	k, ok := mcpwire.IDKey(id)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok = s.answered[k]
	return ok
}

func (s *Session) remember(id json.RawMessage) {
	k, ok := mcpwire.IDKey(id)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.answered[k]; ok {
		return
	}
	s.answered[k] = struct{}{}
	s.order = append(s.order, k)
	for len(s.order) > maxAnswered {
		delete(s.answered, s.order[0])
		s.order = s.order[1:]
	}
}

// FromClient filters one message from the remote client (what the gate forwards) before it reaches the server.
// body is the message as received and m what mcpwire.Parse(body, FromClient) read from it without error; era is
// the exchange's (an initialize is always legacy).
//
//   - notifications/roots/list_changed is dropped;
//   - a response whose id is that of a roots/list the node answered on this session is dropped;
//   - an initialize's params.capabilities.roots is replaced by the node's (Roots.Capability);
//   - roots is removed from params._meta["io.modelcontextprotocol/clientCapabilities"], and every
//     params.inputResponses entry whose value carries roots is removed (2026-07-28; harmless elsewhere);
//   - everything else is forwarded as it came.
func (s *Session) FromClient(era mcpwire.Era, body []byte, m *mcpwire.Message) (Decision, error) {
	if m == nil {
		return Decision{}, errors.New("mcproots: no message")
	}
	switch {
	case m.Method == MethodRootsChanged:
		return Decision{Action: Drop}, nil
	case m.Method == "":
		if s.Answered(m.ID) {
			return Decision{Action: Drop}, nil
		}
		return Decision{Action: Forward, Body: body}, nil
	}
	if len(m.Params) == 0 || firstByte(m.Params) != '{' {
		return Decision{Action: Forward, Body: body}, nil
	}
	params := []byte(m.Params)
	var err error
	if m.Method == mcpwire.MethodInitialize {
		if params, err = replaceCapability(params, s.roots.Capability()); err != nil {
			return Decision{}, err
		}
	}
	if params, err = stripMetaRoots(params); err != nil {
		return Decision{}, err
	}
	if params, err = stripInputResponses(params); err != nil {
		return Decision{}, err
	}
	if bytes.Equal(params, m.Params) {
		return Decision{Action: Forward, Body: body}, nil
	}
	out, err := mcpwire.SetMember(body, "params", params)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Action: Forward, Body: out}, nil
}

// FromServer filters one message from the server before it reaches the remote client. body is the message as
// received and m what mcpwire.Parse(body, FromUpstream) read from it without error; era is the exchange's.
//
//   - a roots/list request is answered by the node on a legacy session (Answer, its id remembered) and dropped on
//     a 2026-07-28 exchange, which has no session to answer it on (a modern server sends no requests);
//   - every roots/list entry of a result's inputRequests (an InputRequiredResult, or a task's) is removed;
//   - everything else is forwarded as it came.
func (s *Session) FromServer(era mcpwire.Era, body []byte, m *mcpwire.Message) (Decision, error) {
	if m == nil {
		return Decision{}, errors.New("mcproots: no message")
	}
	if m.Method == MethodRootsList {
		if len(m.ID) == 0 || era == mcpwire.EraModern {
			return Decision{Action: Drop}, nil
		}
		s.remember(m.ID)
		reply, err := json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
		}{"2.0", m.ID, s.roots.ListResult()})
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: Answer, Reply: reply}, nil
	}
	if m.Method != "" || len(m.Result) == 0 || firstByte(m.Result) != '{' {
		return Decision{Action: Forward, Body: body}, nil
	}
	result, err := stripInputRequests(m.Result)
	if err != nil {
		return Decision{}, err
	}
	if bytes.Equal(result, m.Result) {
		return Decision{Action: Forward, Body: body}, nil
	}
	out, err := mcpwire.SetMember(body, "result", result)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Action: Forward, Body: out}, nil
}

// replaceCapability writes capabilities.roots = c into an initialize's params (removing every case variant first),
// or removes it when c is nil; a params without capabilities gets one only when c is set, and a capabilities that
// is not an object is left as it came (the server refuses such an initialize).
func replaceCapability(params []byte, c json.RawMessage) ([]byte, error) {
	caps, ok, err := member(params, "capabilities")
	if err != nil {
		return nil, err
	}
	if !ok {
		if c == nil {
			return params, nil
		}
		return setMember(params, "capabilities", append(append([]byte(`{"roots":`), c...), '}'))
	}
	if firstByte(caps) != '{' {
		return params, nil
	}
	out, err := deleteFold(caps, "roots")
	if err != nil {
		return nil, err
	}
	if c != nil {
		if out, err = setMember(out, "roots", c); err != nil {
			return nil, err
		}
	}
	if bytes.Equal(out, caps) {
		return params, nil
	}
	return setMember(params, "capabilities", out)
}

// stripMetaRoots removes roots (and its case variants) from params._meta["io.modelcontextprotocol/clientCapabilities"].
func stripMetaRoots(params []byte) ([]byte, error) {
	meta, ok, err := member(params, "_meta")
	if err != nil || !ok || firstByte(meta) != '{' {
		return params, err
	}
	cc, ok, err := member(meta, mcpwire.MetaClientCapabilities)
	if err != nil || !ok || firstByte(cc) != '{' {
		return params, err
	}
	out, err := deleteFold(cc, "roots")
	if err != nil || bytes.Equal(out, cc) {
		return params, err
	}
	if meta, err = setMember(meta, mcpwire.MetaClientCapabilities, out); err != nil {
		return nil, err
	}
	return setMember(params, "_meta", meta)
}

// stripInputResponses removes every params.inputResponses entry whose value is an object with a roots member (or a
// case variant of it): the client's answer to a roots/list, which the node never relayed.
func stripInputResponses(params []byte) ([]byte, error) {
	ir, ok, err := member(params, "inputResponses")
	if err != nil || !ok {
		return params, err
	}
	out, err := filterEntries(ir, func(v json.RawMessage) (bool, error) {
		if firstByte(v) != '{' {
			return false, nil
		}
		ms, err := members(v)
		if err != nil {
			return false, err
		}
		for _, m := range ms {
			if strings.EqualFold(m.key, "roots") {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil || bytes.Equal(out, ir) {
		return params, err
	}
	return setMember(params, "inputResponses", out)
}

// stripInputRequests removes every result.inputRequests entry whose method is roots/list.
func stripInputRequests(result []byte) ([]byte, error) {
	ir, ok, err := member(result, "inputRequests")
	if err != nil || !ok {
		return result, err
	}
	out, err := filterEntries(ir, func(v json.RawMessage) (bool, error) {
		if firstByte(v) != '{' {
			return false, nil
		}
		m, ok, err := member(v, "method")
		if err != nil || !ok {
			return false, err
		}
		var method string
		if json.Unmarshal(m, &method) != nil {
			return false, nil
		}
		return method == MethodRootsList, nil
	})
	if err != nil || bytes.Equal(out, ir) {
		return result, err
	}
	return setMember(result, "inputRequests", out)
}

// filterEntries removes the members of an object (keyed entries) or the elements of an array for which drop is true;
// v comes back as it came when nothing is removed or it is neither.
func filterEntries(v []byte, drop func(json.RawMessage) (bool, error)) ([]byte, error) {
	switch firstByte(v) {
	case '{':
		ms, err := members(v)
		if err != nil {
			return nil, err
		}
		kept := ms[:0:0]
		for _, m := range ms {
			d, err := drop(m.raw)
			if err != nil {
				return nil, err
			}
			if !d {
				kept = append(kept, m)
			}
		}
		if len(kept) == len(ms) {
			return v, nil
		}
		return encode(kept), nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(v, &elems); err != nil {
			return nil, fmt.Errorf("mcproots: %w", err)
		}
		var kept []json.RawMessage
		for _, e := range elems {
			d, err := drop(e)
			if err != nil {
				return nil, err
			}
			if !d {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(elems) {
			return v, nil
		}
		var b bytes.Buffer
		b.WriteByte('[')
		for i, e := range kept {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(e)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	}
	return v, nil
}

// kv is one member of a JSON object, in its order.
type kv struct {
	key string
	raw json.RawMessage
}

// members decodes one JSON object into its members in order (repeated keys kept), values raw.
func members(b []byte) ([]kv, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("mcproots: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("mcproots: not a JSON object")
	}
	var out []kv
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("mcproots: %w", err)
		}
		k, ok := t.(string)
		if !ok {
			return nil, errors.New("mcproots: not a JSON object")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("mcproots: %w", err)
		}
		out = append(out, kv{key: k, raw: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("mcproots: %w", err)
	}
	return out, nil
}

// encode writes an object from its members, in order.
func encode(ms []kv) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// member returns the value of obj's member key (exactly; the first one), ok false when there is none.
func member(obj []byte, key string) (json.RawMessage, bool, error) {
	ms, err := members(obj)
	if err != nil {
		return nil, false, err
	}
	for _, m := range ms {
		if m.key == key {
			return m.raw, true, nil
		}
	}
	return nil, false, nil
}

// setMember replaces the value of every member named key, or appends one.
func setMember(obj []byte, key string, val json.RawMessage) ([]byte, error) {
	ms, err := members(obj)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range ms {
		if ms[i].key == key {
			ms[i].raw, found = val, true
		}
	}
	if !found {
		ms = append(ms, kv{key: key, raw: val})
	}
	return encode(ms), nil
}

// deleteFold removes every member whose key equals key under case folding; obj comes back as it came without one.
func deleteFold(obj []byte, key string) ([]byte, error) {
	ms, err := members(obj)
	if err != nil {
		return nil, err
	}
	kept := ms[:0:0]
	for _, m := range ms {
		if !strings.EqualFold(m.key, key) {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(ms) {
		return obj, nil
	}
	return encode(kept), nil
}

func firstByte(raw []byte) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c
	}
	return 0
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// clip shortens a value an error quotes back.
func clip(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
