package a2awire

import (
	"encoding/json"
	"errors"
	"fmt"

	"kontra.sh/kontra/internal/strictjson"
)

// The sealing levels (spec/ext-sealed.md §5 "Hosting node", plan corrections WP-08; D172): the hosting and hiring
// nodes read every A2A body they seal or open with exact keys at the levels they read, and re-encode what they parsed.
// Walker is the part walker of Rewrite with what sealing needs besides the parts: the message or artifact that holds
// them (its ids, its role, an update's append and lastChunk) and the object that holds that (a Task, a status update,
// an artifact update, an answer message), whose ids a node checks against its job.

// JSONRPCRequestLevels are the levels of an A2A JSON-RPC request the gate reads itself or through a2a-go's second
// decoding, below the request object (spec/relay.md §2.4, D172): params (tenant, the task id), params.metadata,
// params.configuration, params.message, params.message.metadata, each of params.message.parts and each part's
// metadata (the kontra. keys of spec §2.5). internal/a2ahttp refuses a request ambiguous at one of them; the hosting
// node reads a request at the same levels.
var JSONRPCRequestLevels = []strictjson.Path{
	{"params"},
	{"params", "metadata"},
	{"params", "configuration"},
	{"params", "message"},
	{"params", "message", "metadata"},
	{"params", "message", "parts", "*"},
	{"params", "message", "parts", "*", "metadata"},
}

// HTTPJSONRequestLevels are the same levels of an HTTP+JSON SendMessageRequest (the body is params' shape): the
// request, its metadata, configuration and message, the message's metadata, each part and each part's metadata.
var HTTPJSONRequestLevels = []strictjson.Path{
	{"metadata"},
	{"configuration"},
	{"message"},
	{"message", "metadata"},
	{"message", "parts", "*"},
	{"message", "parts", "*", "metadata"},
}

// Where is where a container (a message or an artifact) sits in a body.
type Where int

// Places of a container.
const (
	WhereRequest        Where = iota + 1 // a send request's message
	WhereAnswer                          // a SendMessageResponse's or a StreamResponse's message (no task)
	WhereStatus                          // a Task's or a status update's status.message
	WhereHistory                         // an entry of a Task's history
	WhereArtifact                        // an entry of a Task's artifacts: a whole artifact
	WhereArtifactUpdate                  // an artifact update's artifact
)

// Kind is the kind of object the walk reads around containers.
type Kind int

// Objects.
const (
	KindTask           Kind = iota + 1 // a Task (an answer, a StreamResponse's task, an entry of a list)
	KindStatusUpdate                   // a StreamResponse's statusUpdate
	KindArtifactUpdate                 // a StreamResponse's artifactUpdate
	KindMessage                        // a request's or an answer's message
)

// Container is one message or artifact with its parts, read with exact keys. A visitor may replace its parts or set
// its members; the walk then re-encodes it and every level above it.
type Container struct {
	Where Where
	// Members are the container's members other than parts (messageId, role, contextId, taskId, artifactId,
	// metadata, ...), as they came.
	Members map[string]json.RawMessage
	// Parts are its parts, each read with exact keys (and a part's metadata too); nil when it has none.
	Parts []Part
	// Owner is the object that holds the container: the Task (history, status, artifacts), the status or artifact
	// update, or for a request's or an answer's message the message itself. Read only.
	Owner map[string]json.RawMessage
	// OwnerKind is Owner's kind.
	OwnerKind Kind

	changed bool
}

// SetParts replaces the container's parts.
func (c *Container) SetParts(ps []Part) {
	c.Parts = ps
	c.changed = true
}

// Set replaces (or adds) one of the container's members; a nil value removes it.
func (c *Container) Set(name string, v json.RawMessage) {
	if v == nil {
		delete(c.Members, name)
	} else {
		c.Members[name] = v
	}
	c.changed = true
}

// String returns the container member name as a string, ok false when it is absent or not a string.
func String(m map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := m[name]
	if !ok || firstByte(raw) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Walker reads an A2A 1.0 body (a JSON-RPC request or response, or an HTTP+JSON body; one SSE event's data) at the
// levels the package doc names, calling Object for each Task, status update, artifact update and request or answer
// message it reads, before the containers inside it, and Container for each message and artifact. Either may be nil.
// An error from one stops the walk and is returned as it is.
type Walker struct {
	Object    func(kind Kind, m map[string]json.RawMessage) error
	Container func(c *Container) error
}

// WalkRequest walks a send request (a JSON-RPC SendMessage or SendStreamingMessage, or an HTTP+JSON
// SendMessageRequest body) and returns it re-encoded when a visitor changed a container, else body itself and
// changed false. A level with a repeated or case-variant key is a *strictjson.KeyError, one of the wrong JSON type
// ErrShape; every metadata level and each part are read with exact keys.
func (w Walker) WalkRequest(body []byte) (out []byte, changed bool, err error) {
	return w.walk(body, true)
}

// WalkReply walks an answer: a JSON-RPC response's result, or an HTTP+JSON answer (one SSE event's data included):
// a StreamResponse or SendMessageResponse ({task | message | statusUpdate | artifactUpdate}), a Task, or a list
// ({tasks}).
func (w Walker) WalkReply(body []byte) (out []byte, changed bool, err error) {
	return w.walk(body, false)
}

func (w Walker) walk(body []byte, request bool) (out []byte, changed bool, err error) {
	top, err := strictjson.Members(body)
	if err != nil {
		return nil, false, err
	}
	ww := &walk{w: w}
	_, jsonrpc := top["jsonrpc"]
	switch {
	case jsonrpc && request:
		changed, err = ww.member(top, "params", ww.request)
	case jsonrpc:
		changed, err = ww.member(top, "result", ww.answer)
	case request:
		changed, err = ww.request(top)
	default:
		changed, err = ww.answer(top)
	}
	if err != nil || !changed {
		return body, false, err
	}
	return encode(top), true, nil
}

type walk struct {
	w Walker
}

// request is a send request's params (JSON-RPC) or body (HTTP+JSON): its message.
func (ww *walk) request(m map[string]json.RawMessage) (bool, error) {
	if raw, ok := m["metadata"]; ok && !isNull(raw) {
		if _, err := strictjson.Members(raw); err != nil {
			return false, levelErr(err, "metadata")
		}
	}
	if raw, ok := m["configuration"]; ok && !isNull(raw) {
		if _, err := strictjson.Members(raw); err != nil {
			return false, levelErr(err, "configuration")
		}
	}
	return ww.member(m, "message", func(msg map[string]json.RawMessage) (bool, error) {
		if err := ww.object(KindMessage, msg); err != nil {
			return false, err
		}
		return ww.container(WhereRequest, msg, msg, KindMessage)
	})
}

// answer is a JSON-RPC result or an HTTP+JSON answer: a StreamResponse or SendMessageResponse ({task | message |
// statusUpdate | artifactUpdate}), a Task, or a list ({tasks}).
func (ww *walk) answer(m map[string]json.RawMessage) (bool, error) {
	changed := false
	add := func(c bool, err error) error {
		changed = changed || c
		return err
	}
	if err := add(ww.member(m, "message", func(msg map[string]json.RawMessage) (bool, error) {
		if err := ww.object(KindMessage, msg); err != nil {
			return false, err
		}
		return ww.container(WhereAnswer, msg, msg, KindMessage)
	})); err != nil {
		return false, err
	}
	if err := add(ww.member(m, "task", ww.task)); err != nil {
		return false, err
	}
	if err := add(ww.member(m, "statusUpdate", func(u map[string]json.RawMessage) (bool, error) {
		if err := ww.object(KindStatusUpdate, u); err != nil {
			return false, err
		}
		return ww.status(u, KindStatusUpdate)
	})); err != nil {
		return false, err
	}
	if err := add(ww.member(m, "artifactUpdate", func(u map[string]json.RawMessage) (bool, error) {
		if err := ww.object(KindArtifactUpdate, u); err != nil {
			return false, err
		}
		return ww.member(u, "artifact", func(a map[string]json.RawMessage) (bool, error) {
			return ww.container(WhereArtifactUpdate, a, u, KindArtifactUpdate)
		})
	})); err != nil {
		return false, err
	}
	if err := add(ww.array(m, "tasks", ww.task)); err != nil {
		return false, err
	}
	if isTask(m) {
		if err := add(ww.task(m)); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// task reads a Task: the object, then its history, its status message and its artifacts.
func (ww *walk) task(t map[string]json.RawMessage) (bool, error) {
	if err := ww.object(KindTask, t); err != nil {
		return false, err
	}
	a, err := ww.array(t, "history", func(h map[string]json.RawMessage) (bool, error) {
		return ww.container(WhereHistory, h, t, KindTask)
	})
	if err != nil {
		return false, err
	}
	b, err := ww.status(t, KindTask)
	if err != nil {
		return false, err
	}
	c, err := ww.array(t, "artifacts", func(art map[string]json.RawMessage) (bool, error) {
		return ww.container(WhereArtifact, art, t, KindTask)
	})
	if err != nil {
		return false, err
	}
	return a || b || c, nil
}

// status reads owner's status.message.
func (ww *walk) status(owner map[string]json.RawMessage, kind Kind) (bool, error) {
	return ww.member(owner, "status", func(st map[string]json.RawMessage) (bool, error) {
		return ww.member(st, "message", func(msg map[string]json.RawMessage) (bool, error) {
			return ww.container(WhereStatus, msg, owner, kind)
		})
	})
}

func (ww *walk) object(kind Kind, m map[string]json.RawMessage) error {
	if ww.w.Object == nil {
		return nil
	}
	return ww.w.Object(kind, m)
}

// container reads a message's or an artifact's parts (each part and its metadata with exact keys), calls the
// visitor, and writes back what it changed.
func (ww *walk) container(where Where, m, owner map[string]json.RawMessage, ownerKind Kind) (bool, error) {
	if raw, ok := m["metadata"]; ok && !isNull(raw) {
		if _, err := strictjson.Members(raw); err != nil {
			return false, levelErr(err, "metadata")
		}
	}
	c := &Container{Where: where, Members: map[string]json.RawMessage{}, Owner: owner, OwnerKind: ownerKind}
	for k, v := range m {
		if k != "parts" {
			c.Members[k] = v
		}
	}
	if raw, ok := m["parts"]; ok && !isNull(raw) {
		elems, err := elements(raw)
		if err != nil {
			return false, err
		}
		for _, e := range elems {
			p, err := strictjson.Members(e)
			if err != nil {
				if errors.Is(err, strictjson.ErrNotObject) {
					return false, fmt.Errorf("%w: a part that is not an object", ErrShape)
				}
				return false, err
			}
			if md, ok := p["metadata"]; ok && !isNull(md) {
				if _, err := strictjson.Members(md); err != nil {
					return false, levelErr(err, "a part's metadata")
				}
			}
			c.Parts = append(c.Parts, Part(p))
		}
	}
	if ww.w.Container == nil {
		return false, nil
	}
	if err := ww.w.Container(c); err != nil {
		return false, err
	}
	if !c.changed {
		return false, nil
	}
	for k := range m {
		delete(m, k)
	}
	for k, v := range c.Members {
		m[k] = v
	}
	if c.Parts != nil {
		elems := make([]json.RawMessage, len(c.Parts))
		for i, p := range c.Parts {
			elems[i] = encode(p)
		}
		m["parts"] = encodeArray(elems)
	}
	return true, nil
}

func (ww *walk) member(m map[string]json.RawMessage, name string, fn func(map[string]json.RawMessage) (bool, error)) (bool, error) {
	return (&walker{}).member(m, name, fn)
}

func (ww *walk) array(m map[string]json.RawMessage, name string, fn func(map[string]json.RawMessage) (bool, error)) (bool, error) {
	return (&walker{}).array(m, name, fn)
}

// levelErr is a metadata level that is not an object, or is ambiguous.
func levelErr(err error, what string) error {
	if errors.Is(err, strictjson.ErrNotObject) {
		return fmt.Errorf("%w: %s is not an object", ErrShape, what)
	}
	return err
}

// Encode writes an object's members in key order, each value as it is (compacted), without HTML escaping: how the
// nodes re-encode a level they changed.
func Encode(m map[string]json.RawMessage) []byte { return encode(m) }

// EncodeArray writes a JSON array of the values as they are (compacted).
func EncodeArray(elems []json.RawMessage) []byte { return encodeArray(elems) }

// Quote is a JSON string without HTML escaping.
func Quote(s string) []byte { return quote(s) }
