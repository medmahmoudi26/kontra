package mcpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Methods whose messages the scrub treats specially (spec/relay.md §2.5, spec/ext-sealed.md §3).
const (
	MethodInitialize     = "initialize"
	MethodToolsList      = "tools/list"
	MethodToolsCall      = "tools/call"
	MethodElicit         = "elicitation/create"
	MethodSample         = "sampling/createMessage"
	MethodServerDiscover = "server/discover"
	// MetaSealed is the sealed envelope's _meta key (ext-sealed.md §3) and the gate's sealed capability under
	// capabilities.experimental (D27).
	MetaSealed = KontraPrefix + "sealed"
)

// ScrubResult removes every sh.kontra/ key from the protocol-level locations of a result answering method
// (spec/relay.md §2.5). From an upstream: the result's _meta; each tools[]._meta (tools/list);
// capabilities.experimental's and capabilities.extensions' sh.kontra/ keys (initialize, server/discover); the
// task objects' _meta (result.task, each of result.tasks, a terminal task's result.result) and each
// inputRequests entry's params._meta. A method the relay does not know ("": a response whose request it did
// not see) gets every one of those locations. From a client (its response to a server-to-client request
// method): the result's _meta only. It never descends into content, structuredContent or arguments, and a
// result that is not a JSON object is returned as it is (it has no _meta).
//
// sealed is the service's sealed flag on a 2025-11-25 or 2025-06-18 message (D162, D174): a client's response to
// elicitation/create or sampling/createMessage then keeps _meta["sh.kontra/sealed"] when SealedEnvelope accepts
// it. Nothing else keeps it.
//
// The bytes come back unchanged when nothing was removed, so a message the scrub leaves alone is relayed byte for
// byte (§11.2: "Requests and responses pass unchanged otherwise"); an object it rewrites keeps its members'
// order and every other member's bytes.
func ScrubResult(src Source, method string, result json.RawMessage, sealed bool) (json.RawMessage, error) {
	if firstByte(result) != '{' {
		return result, nil
	}
	switch src {
	case FromClient:
		keep := sealed && (method == MethodElicit || method == MethodSample)
		return rewriteMeta(result, keep)
	case FromUpstream:
	default:
		return nil, fmt.Errorf("mcpwire: unknown source %d", src)
	}
	unknown := method == ""
	out, err := rewriteMeta(result, false)
	if err != nil {
		return nil, err
	}
	if unknown || method == MethodToolsList {
		if out, err = rewriteMember(out, "tools", eachElement(func(v json.RawMessage) (json.RawMessage, error) {
			return rewriteMeta(v, false)
		})); err != nil {
			return nil, err
		}
	}
	if unknown || method == MethodInitialize || method == MethodServerDiscover {
		if out, err = rewriteMember(out, "capabilities", func(caps json.RawMessage) (json.RawMessage, error) {
			c, err := rewriteMember(caps, "experimental", dropKontraKeys)
			if err != nil {
				return nil, err
			}
			return rewriteMember(c, "extensions", dropKontraKeys)
		}); err != nil {
			return nil, err
		}
	}
	if unknown || method == MethodToolsCall || strings.HasPrefix(method, "tasks/") {
		noSealed := func(v json.RawMessage) (json.RawMessage, error) { return rewriteMeta(v, false) }
		for _, k := range []string{"task", "result"} {
			if out, err = rewriteMember(out, k, noSealed); err != nil {
				return nil, err
			}
		}
		if out, err = rewriteMember(out, "tasks", eachElement(noSealed)); err != nil {
			return nil, err
		}
		if out, err = rewriteMember(out, "inputRequests", eachElement(func(v json.RawMessage) (json.RawMessage, error) {
			return rewriteMember(v, "params", noSealed)
		})); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ScrubParams does the same for the params of a request or notification (params._meta), from a client or an
// upstream. With sealed set (D174), _meta["sh.kontra/sealed"] that SealedEnvelope accepts is kept on a client's
// tools/call and on an upstream's elicitation/create or sampling/createMessage, and removed everywhere else.
// Params that are absent or not an object are returned as they are.
func ScrubParams(src Source, method string, params json.RawMessage, sealed bool) (json.RawMessage, error) {
	if firstByte(params) != '{' {
		return params, nil
	}
	var keep bool
	switch src {
	case FromClient:
		keep = sealed && method == MethodToolsCall
	case FromUpstream:
		keep = sealed && (method == MethodElicit || method == MethodSample)
	default:
		return nil, fmt.Errorf("mcpwire: unknown source %d", src)
	}
	return rewriteMeta(params, keep)
}

// ScrubMessage applies ScrubParams (a request or notification) or ScrubResult (a response; requestMethod is the
// method of the request it answers, "" when unknown) to the message m that Parse read from body, and returns the
// message's bytes with the scrubbed member put back. body comes back unchanged when nothing was removed. An error
// response carries no _meta the scrub reads and is returned as it is.
func ScrubMessage(src Source, body []byte, m *Message, requestMethod string, sealed bool) ([]byte, error) {
	switch {
	case m == nil:
		return nil, errors.New("mcpwire: ScrubMessage: no message")
	case m.Method != "":
		if m.Params == nil {
			return body, nil
		}
		p, err := ScrubParams(src, m.Method, m.Params, sealed)
		if err != nil || bytes.Equal(p, m.Params) {
			return body, err
		}
		return SetMember(body, "params", p)
	case m.Result != nil:
		r, err := ScrubResult(src, requestMethod, m.Result, sealed)
		if err != nil || bytes.Equal(r, m.Result) {
			return body, err
		}
		return SetMember(body, "result", r)
	}
	return body, nil
}

// IsKontraKey reports whether k starts with sh.kontra/ under Unicode case folding: a decoder that matches names
// case-insensitively (encoding/json into a struct field tagged "sh.kontra/price") reads "SH.KONTRA/PRICE", or one
// spelled with a long s (U+017F), as the gate's own key, so every such spelling is Kontra's (§2.5).
func IsKontraKey(k string) bool {
	n := utf8.RuneCountInString(KontraPrefix)
	r := []rune(k)
	return len(r) >= n && strings.EqualFold(string(r[:n]), KontraPrefix)
}

// member is one member of a JSON object, in its order.
type member struct {
	key string
	raw json.RawMessage
}

// members decodes one JSON object into its members in order, keys unescaped and values kept raw. Repeated keys
// are kept: the rewrites below apply to every one of them, so no reader takes an unscrubbed copy.
func members(b []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	var out []member
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := t.(string)
		if !ok {
			return nil, errNotObject
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{key: k, raw: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

var errNotObject = errors.New("mcpwire: not a JSON object")

// encodeMembers writes an object from its members, in order.
func encodeMembers(ms []member) []byte {
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

// rewriteMember applies fn to the value of every member of obj named key (or a case variant of it: a
// case-insensitive reader may take one for the member) that is a JSON object or array, and returns obj with the
// results; obj comes back unchanged when fn changed nothing or obj is not an object.
func rewriteMember(obj json.RawMessage, key string, fn func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	if firstByte(obj) != '{' {
		return obj, nil
	}
	ms, err := members(obj)
	if err != nil {
		return nil, fmt.Errorf("mcpwire: scrub: %w", err)
	}
	changed := false
	for i, m := range ms {
		if !strings.EqualFold(m.key, key) {
			continue
		}
		if c := firstByte(m.raw); c != '{' && c != '[' {
			continue
		}
		v, err := fn(m.raw)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(v, m.raw) {
			ms[i].raw, changed = v, true
		}
	}
	if !changed {
		return obj, nil
	}
	return encodeMembers(ms), nil
}

// eachElement applies fn to every element of an array, or every member value of an object (a 2026-07-28
// inputRequests is keyed by request key).
func eachElement(fn func(json.RawMessage) (json.RawMessage, error)) func(json.RawMessage) (json.RawMessage, error) {
	return func(v json.RawMessage) (json.RawMessage, error) {
		switch firstByte(v) {
		case '{':
			ms, err := members(v)
			if err != nil {
				return nil, fmt.Errorf("mcpwire: scrub: %w", err)
			}
			changed := false
			for i, m := range ms {
				r, err := fn(m.raw)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(r, m.raw) {
					ms[i].raw, changed = r, true
				}
			}
			if !changed {
				return v, nil
			}
			return encodeMembers(ms), nil
		case '[':
			var elems []json.RawMessage
			if err := json.Unmarshal(v, &elems); err != nil {
				return nil, fmt.Errorf("mcpwire: scrub: %w", err)
			}
			changed := false
			for i, e := range elems {
				r, err := fn(e)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(r, e) {
					elems[i], changed = r, true
				}
			}
			if !changed {
				return v, nil
			}
			var b bytes.Buffer
			b.WriteByte('[')
			for i, e := range elems {
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
}

// rewriteMeta removes the sh.kontra/ keys of obj's _meta; keepSealed keeps an exact "sh.kontra/sealed" whose
// value SealedEnvelope accepts.
func rewriteMeta(obj json.RawMessage, keepSealed bool) (json.RawMessage, error) {
	return rewriteMember(obj, "_meta", func(meta json.RawMessage) (json.RawMessage, error) {
		return dropKontra(meta, keepSealed)
	})
}

// dropKontraKeys removes every sh.kontra/ key of an object.
func dropKontraKeys(obj json.RawMessage) (json.RawMessage, error) { return dropKontra(obj, false) }

func dropKontra(obj json.RawMessage, keepSealed bool) (json.RawMessage, error) {
	if firstByte(obj) != '{' {
		return obj, nil
	}
	ms, err := members(obj)
	if err != nil {
		return nil, fmt.Errorf("mcpwire: scrub: %w", err)
	}
	sealedSeen := 0
	for _, m := range ms {
		if m.key == MetaSealed {
			sealedSeen++
		}
	}
	out := ms[:0:0]
	for _, m := range ms {
		if IsKontraKey(m.key) && !(keepSealed && m.key == MetaSealed && sealedSeen == 1 && SealedEnvelope(m.raw)) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == len(ms) {
		return obj, nil
	}
	return encodeMembers(out), nil
}

// SetMember returns obj with the value of its member key replaced by val (every member of that exact name),
// keeping the order and the other members' bytes; a key obj does not have is appended.
func SetMember(obj []byte, key string, val json.RawMessage) ([]byte, error) {
	ms, err := members(obj)
	if err != nil {
		return nil, fmt.Errorf("mcpwire: %w", err)
	}
	found := false
	for i, m := range ms {
		if m.key == key {
			ms[i].raw, found = val, true
		}
	}
	if !found {
		ms = append(ms, member{key: key, raw: val})
	}
	return encodeMembers(ms), nil
}

// DeleteMember returns obj without its members named key (exactly); obj comes back unchanged without one.
func DeleteMember(obj []byte, key string) ([]byte, error) {
	ms, err := members(obj)
	if err != nil {
		return nil, fmt.Errorf("mcpwire: %w", err)
	}
	out := ms[:0:0]
	for _, m := range ms {
		if m.key != key {
			out = append(out, m)
		}
	}
	if len(out) == len(ms) {
		return obj, nil
	}
	return encodeMembers(out), nil
}

// Member returns the value of obj's member key (the first of that exact name), ok false when it has none or obj
// is not an object.
func Member(obj []byte, key string) (json.RawMessage, bool) {
	if firstByte(obj) != '{' {
		return nil, false
	}
	ms, err := members(obj)
	if err != nil {
		return nil, false
	}
	for _, m := range ms {
		if m.key == key {
			return m.raw, true
		}
	}
	return nil, false
}

// MergeMeta returns obj with the members of add written into its _meta (created when absent), each replacing a
// same-named key and never merged with it (spec/relay.md §2.5, §13.4: "An operator key replaces a same-named
// key"). add's keys are written in sorted order after the kept ones.
func MergeMeta(obj []byte, add map[string]json.RawMessage) ([]byte, error) {
	if len(add) == 0 {
		return obj, nil
	}
	meta, ok := Member(obj, "_meta")
	var ms []member
	if ok && firstByte(meta) == '{' {
		var err error
		if ms, err = members(meta); err != nil {
			return nil, fmt.Errorf("mcpwire: %w", err)
		}
	}
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	kept := ms[:0:0]
	for _, m := range ms {
		if _, replaced := add[m.key]; !replaced {
			kept = append(kept, m)
		}
	}
	for _, k := range keys {
		kept = append(kept, member{key: k, raw: add[k]})
	}
	return SetMember(obj, "_meta", encodeMembers(kept))
}
