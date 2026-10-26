// Package a2awire reads and rewrites the parts of A2A 1.0 bodies on the nodes (spec/relay.md §10.4,
// spec/tunnel.md §8.3, §9; D56, D165, D181): JSON-RPC and HTTP+JSON, requests and answers, unary and SSE. It is
// the one part walker the hosting and hiring nodes share: the blob inlining here (WP-03), and the sealing around
// it (internal/sealing, WP-08), which seals and opens parts at the same levels.
//
// The levels. Parts live in a Message (its parts) and an Artifact (its parts); a Message appears as a request's
// message (JSON-RPC params.message, HTTP+JSON SendMessageRequest.message), a StreamResponse's or
// SendMessageResponse's message, a Task's history entries and status.message, and a status update's
// status.message; an Artifact as a Task's artifacts entries and an artifact update's artifact. Answers are a
// StreamResponse ({task | message | statusUpdate | artifactUpdate}), a Task, or a list ({tasks}), bare on
// HTTP+JSON and as a JSON-RPC response's result. Nothing below those levels is read: metadata, a data part's
// data and a part's own members other than its content are carried as they came.
//
// Every level read is decoded with exact keys (internal/strictjson, D172): a repeated key, or two equal under
// case folding, is refused, since the gate is not trusted to have checked what it forwards. A body is re-encoded
// from what was parsed, only along the levels that changed, never patched in place: the members of a changed
// level are written in key order, every other value as it came.
//
// Work package: M3 WP-03 (the walker, the inliner); M3 WP-08 adds the sealing levels (and moves
// a2ahttp's jsonrpcLevels here, plan corrections WP-08).
package a2awire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"kontra.sh/kontra/internal/strictjson"
)

// Part is one part object's members, read with exact keys: the content (text, raw, url or data) and
// filename, mediaType and metadata.
type Part map[string]json.RawMessage

// URL returns the part's url member when it is a JSON string.
func (p Part) URL() (string, bool) {
	raw, ok := p["url"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Visit is called for each part of a body; it returns the part to write in its place, or nil to leave it as it
// came. An error stops the walk.
type Visit func(p Part) (Part, error)

// ErrShape is a body that does not have the shape of an A2A 1.0 body at a level the walker reads (an object
// where an array is due, a part that is not an object). It wraps nothing of the sender's.
var ErrShape = errors.New("a2awire: not an A2A 1.0 body")

// Rewrite walks body, one A2A 1.0 JSON body (a request or an answer, JSON-RPC or HTTP+JSON, or one SSE event's
// data), and calls visit for every part at the levels the package doc names. It returns body re-encoded when a
// visit replaced a part, else body itself and changed false. A level with a repeated or case-variant key is a
// *strictjson.KeyError; a level of the wrong JSON type is ErrShape.
func Rewrite(body []byte, visit Visit) (out []byte, changed bool, err error) {
	top, err := strictjson.Members(body)
	if err != nil {
		return nil, false, err
	}
	w := &walker{visit: visit}
	changed, err = w.envelope(top)
	if err != nil || !changed {
		return body, false, err
	}
	return encode(top), true, nil
}

type walker struct {
	visit Visit
}

// envelope is the top level: a JSON-RPC request (params is read for its message), a JSON-RPC response (result is
// an answer), or a bare HTTP+JSON body (a send request or an answer).
func (w *walker) envelope(top map[string]json.RawMessage) (bool, error) {
	if _, ok := top["jsonrpc"]; ok {
		if _, isRequest := top["method"]; isRequest {
			return w.member(top, "params", w.answerOrRequest)
		}
		return w.member(top, "result", w.answerOrRequest)
	}
	return w.answerOrRequest(top)
}

// answerOrRequest reads every member that holds parts at this level: a send request's or a StreamResponse's
// message, a StreamResponse's task, statusUpdate and artifactUpdate, a list's tasks, and a Task's own members
// when the object is a Task.
func (w *walker) answerOrRequest(m map[string]json.RawMessage) (bool, error) {
	changed := false
	for _, f := range []struct {
		name string
		fn   func(map[string]json.RawMessage) (bool, error)
	}{
		{"message", w.message},
		{"task", w.task},
		{"statusUpdate", w.status},
		{"artifactUpdate", w.artifactUpdate},
	} {
		c, err := w.member(m, f.name, f.fn)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	c, err := w.array(m, "tasks", w.task)
	if err != nil {
		return false, err
	}
	changed = changed || c
	if isTask(m) {
		c, err := w.task(m)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	return changed, nil
}

// isTask reports an object with a Task's members (A2A 1.0: id, contextId, status, artifacts, history).
func isTask(m map[string]json.RawMessage) bool {
	_, status := m["status"]
	_, history := m["history"]
	_, artifacts := m["artifacts"]
	return status || history || artifacts
}

// task reads a Task: history entries (Messages), artifacts, status.message.
func (w *walker) task(m map[string]json.RawMessage) (bool, error) {
	a, err := w.array(m, "history", w.message)
	if err != nil {
		return false, err
	}
	b, err := w.array(m, "artifacts", w.artifact)
	if err != nil {
		return false, err
	}
	c, err := w.status(m)
	if err != nil {
		return false, err
	}
	return a || b || c, nil
}

// status reads the status member's message (a Task's, a status update's).
func (w *walker) status(m map[string]json.RawMessage) (bool, error) {
	return w.member(m, "status", func(st map[string]json.RawMessage) (bool, error) {
		return w.member(st, "message", w.message)
	})
}

// artifactUpdate reads an artifact update's artifact.
func (w *walker) artifactUpdate(m map[string]json.RawMessage) (bool, error) {
	return w.member(m, "artifact", w.artifact)
}

// message reads a Message's parts; artifact an Artifact's.
func (w *walker) message(m map[string]json.RawMessage) (bool, error)  { return w.parts(m) }
func (w *walker) artifact(m map[string]json.RawMessage) (bool, error) { return w.parts(m) }

func (w *walker) parts(m map[string]json.RawMessage) (bool, error) {
	raw, ok := m["parts"]
	if !ok || isNull(raw) {
		return false, nil
	}
	elems, err := elements(raw)
	if err != nil {
		return false, err
	}
	changed := false
	for i, e := range elems {
		p, err := strictjson.Members(e)
		if err != nil {
			if errors.Is(err, strictjson.ErrNotObject) {
				return false, fmt.Errorf("%w: a part that is not an object", ErrShape)
			}
			return false, err
		}
		np, err := w.visit(Part(p))
		if err != nil {
			return false, err
		}
		if np != nil {
			elems[i] = encode(np)
			changed = true
		}
	}
	if changed {
		m["parts"] = encodeArray(elems)
	}
	return changed, nil
}

// member decodes m[name] as an object (absent and null skipped) and rewrites it when fn changed it.
func (w *walker) member(m map[string]json.RawMessage, name string, fn func(map[string]json.RawMessage) (bool, error)) (bool, error) {
	raw, ok := m[name]
	if !ok || isNull(raw) {
		return false, nil
	}
	sub, err := strictjson.Members(raw)
	if err != nil {
		if errors.Is(err, strictjson.ErrNotObject) {
			return false, fmt.Errorf("%w: %s is not an object", ErrShape, name)
		}
		return false, err
	}
	changed, err := fn(sub)
	if err != nil || !changed {
		return false, err
	}
	m[name] = encode(sub)
	return true, nil
}

// array decodes m[name] as an array of objects (absent and null skipped) and rewrites it when fn changed one.
func (w *walker) array(m map[string]json.RawMessage, name string, fn func(map[string]json.RawMessage) (bool, error)) (bool, error) {
	raw, ok := m[name]
	if !ok || isNull(raw) {
		return false, nil
	}
	elems, err := elements(raw)
	if err != nil {
		return false, fmt.Errorf("%w: %s is not an array", ErrShape, name)
	}
	changed := false
	for i, e := range elems {
		sub, err := strictjson.Members(e)
		if err != nil {
			if errors.Is(err, strictjson.ErrNotObject) {
				return false, fmt.Errorf("%w: an element of %s is not an object", ErrShape, name)
			}
			return false, err
		}
		c, err := fn(sub)
		if err != nil {
			return false, err
		}
		if c {
			elems[i] = encode(sub)
			changed = true
		}
	}
	if changed {
		m[name] = encodeArray(elems)
	}
	return changed, nil
}

// elements returns the raw elements of a JSON array; anything else is ErrShape.
func elements(raw json.RawMessage) ([]json.RawMessage, error) {
	if firstByte(raw) != '[' {
		return nil, fmt.Errorf("%w: not an array", ErrShape)
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: not an array", ErrShape)
	}
	return out, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

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

// encode writes an object's members in key order, each value as it is (compacted, so that a body re-encoded
// for one SSE data line holds no line break), without HTML escaping.
func encode(m map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(quote(k))
		b.WriteByte(':')
		writeValue(&b, m[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

func encodeArray(elems []json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		writeValue(&b, e)
	}
	b.WriteByte(']')
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v json.RawMessage) {
	if err := json.Compact(b, v); err != nil {
		b.Write(v) // valid JSON already (strictjson checked it); never reached
	}
}

// quote is a JSON string without HTML escaping.
func quote(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
