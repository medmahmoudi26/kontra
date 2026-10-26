// Package strictjson decodes the JSON objects the gate and the nodes act on with exact keys (spec/relay.md
// §2.4, §11.1, D172). encoding/json decoding into a struct matches keys case-insensitively and keeps the last
// of a repeated key, so {"method": "tools/call", "METHOD": "ping"} reads as ping while Node's JSON.parse and
// go-sdk read tools/call; a relay that forwards the bytes it received must never read other members than the
// party behind it. Every level the reader acts on is decoded here first, and a level in which a key repeats, or
// two keys are equal under Unicode case folding (strings.EqualFold), is refused.
//
// It is a leaf: it knows JSON, never a protocol. Its readers name the levels, which are exactly the spec's
// (§11.1, §2.4), below the message object:
//
//   - internal/mcpwire.ClientLevels, an MCP client's message: params, params._meta,
//     params._meta["io.modelcontextprotocol/clientCapabilities"] and its extensions, params.capabilities, the
//     top level of params.arguments and of params.inputResponses, result, result._meta.
//   - internal/mcpwire.UpstreamLevels, an MCP upstream's message: params, params._meta (requests and
//     notifications), result, result._meta, each of result.tools and its _meta, result.capabilities and its
//     experimental and extensions, the task objects the gate reads (result.task; result.inputRequests, each of
//     its entries and their params and params._meta; result.result, result.error), error.
//   - internal/a2ahttp's jsonrpcLevels, an A2A JSON-RPC request: params, params.metadata, params.configuration,
//     params.message, params.message.metadata, each of params.message.parts and each part's metadata.
//
// The hosting node (internal/sealing) and the node's MCP filters (internal/mcproots, internal/stdiobridge) use
// it too, since a gate is not trusted to have checked what it forwards.
//
// Work package: M3 WP-01 (a leaf: WP-04, WP-05, WP-07 and WP-08 use it).
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"unicode"
)

// ErrNotObject is a level that is not one JSON object (with nothing after it).
var ErrNotObject = errors.New("strictjson: not a JSON object")

// KeyError is a level with a repeated key, or two keys equal under case folding. Path names the level, Key the
// second of the two keys; both are for the log, never sent.
type KeyError struct {
	Path []string
	Key  string
}

func (e *KeyError) Error() string { return "strictjson: repeated or case-variant key " + e.Key }

// Members decodes one JSON object and returns its members by their exact keys, refusing (*KeyError) a key that
// appears twice or two keys equal under case folding, and refusing (ErrNotObject) anything but one object.
//
// Keys are compared after JSON unescaping, so "method" and "\u006dethod" are the same key. The values are the
// members' raw bytes, valid JSON each.
func Members(b []byte) (map[string]json.RawMessage, error) {
	m, err := members(b, nil)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotObject, kindOf(b))
	}
	return m, nil
}

// members decodes b as one object; it returns nil, nil for a value that is valid JSON but not an object (the
// caller decides whether that is an error) and an error wrapping ErrNotObject for anything that is not one JSON
// value. at names the level for a KeyError.
func members(b []byte, at []string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		// A scalar or an array: still one JSON value only when nothing follows.
		if d, ok := tok.(json.Delim); ok && d == '[' {
			if err := skipRest(dec, 1); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
			}
		}
		if err := expectEOF(dec); err != nil {
			return nil, err
		}
		return nil, nil
	}
	m := map[string]json.RawMessage{}
	folded := map[string]string{} // fold key -> the first key seen with it
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: a member name is not a string", ErrNotObject)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
		}
		if _, dup := m[key]; dup {
			return nil, &KeyError{Path: slices.Clone(at), Key: key}
		}
		f := foldKey(key)
		if _, dup := folded[f]; dup {
			return nil, &KeyError{Path: slices.Clone(at), Key: key}
		}
		folded[f] = key
		m[key] = v
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	if err := expectEOF(dec); err != nil {
		return nil, err
	}
	return m, nil
}

// skipRest consumes the tokens of a value whose opening delimiter was read, depth levels deep.
func skipRest(dec *json.Decoder, depth int) error {
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			default:
				depth--
			}
		}
	}
	return nil
}

// expectEOF refuses anything but white space after the value.
func expectEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%w: data after the value", ErrNotObject)
		}
		return fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	return nil
}

// foldKey maps s to one representative of its class under Unicode simple case folding, the relation
// strings.EqualFold tests: each rune becomes the smallest rune of its fold orbit, so EqualFold(a, b) holds exactly
// when foldKey(a) == foldKey(b) (both are valid UTF-8 here: the decoder replaced invalid bytes).
func foldKey(s string) string {
	var b []rune
	for _, r := range s {
		min := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < min {
				min = f
			}
		}
		b = append(b, min)
	}
	return string(b)
}

func kindOf(b []byte) string {
	b = bytes.TrimLeft(b, " \t\r\n")
	if len(b) == 0 {
		return "empty"
	}
	switch b[0] {
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	default:
		return "a number"
	}
}

// Path names a level below the top object by member names; "*" stands for every element of an array or every
// member of an object (result.inputRequests is keyed by the server's request keys).
type Path []string

// Check decodes body as one object and checks it and every level paths name that is present and is an object.
// A level that is absent, null or not an object is not checked here: the reader that decodes it refuses a wrong
// type itself.
//
// Every level Check passes through on the way to a named one is decoded with exact keys too (a reader that walks
// to params._meta reads params), so a named level's parent is checked even when it is not named itself. A
// *KeyError's Path names the level by member names, with an array element's index in decimal.
func Check(body []byte, paths ...Path) error {
	top, err := Members(body)
	if err != nil {
		return err
	}
	return CheckMembers(top, paths...)
}

// CheckMembers is Check for an object whose members Members has already returned.
func CheckMembers(top map[string]json.RawMessage, paths ...Path) error {
	return newTrie(paths).walkMembers(top, nil)
}

// trie is the levels paths name, merged, so that each level is decoded once.
type trie struct {
	children map[string]*trie
	star     *trie
}

func newTrie(paths []Path) *trie {
	root := &trie{}
	for _, p := range paths {
		n := root
		for _, seg := range p {
			if seg == "*" {
				if n.star == nil {
					n.star = &trie{}
				}
				n = n.star
				continue
			}
			if n.children == nil {
				n.children = map[string]*trie{}
			}
			c := n.children[seg]
			if c == nil {
				c = &trie{}
				n.children[seg] = c
			}
			n = c
		}
	}
	return root
}

// walk checks the level raw, at path at, and the levels below it that n names.
func (n *trie) walk(raw json.RawMessage, at []string) error {
	switch firstByte(raw) {
	case '{':
		m, err := members(raw, at)
		if err != nil {
			return err
		}
		return n.walkMembers(m, at)
	case '[':
		if n.star == nil {
			return nil
		}
		elems, err := elements(raw)
		if err != nil {
			return err
		}
		for i, e := range elems {
			if err := n.star.walk(e, append(slices.Clip(at), strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *trie) walkMembers(m map[string]json.RawMessage, at []string) error {
	// Named members first, in a fixed order, then every member for "*": the first KeyError found is the same
	// on every run.
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if v, ok := m[name]; ok {
			if err := n.children[name].walk(v, append(slices.Clip(at), name)); err != nil {
				return err
			}
		}
	}
	if n.star != nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if err := n.star.walk(m[k], append(slices.Clip(at), k)); err != nil {
				return err
			}
		}
	}
	return nil
}

// elements returns the raw elements of a JSON array.
func elements(raw json.RawMessage) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	var out []json.RawMessage
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// firstByte is the first byte of raw that is not JSON white space, 0 when there is none.
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
