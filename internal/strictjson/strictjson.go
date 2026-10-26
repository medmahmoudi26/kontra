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
	"encoding/json"
	"errors"

	"kontra.sh/kontra/internal/unimpl"
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
func Members(b []byte) (map[string]json.RawMessage, error) {
	return nil, unimpl.Error("strictjson.Members", "M3 WP-01")
}

// Path names a level below the top object by member names; "*" stands for every element of an array or every
// member of an object (result.inputRequests is keyed by the server's request keys).
type Path []string

// Check decodes body as one object and checks it and every level paths name that is present and is an object.
// A level that is absent, null or not an object is not checked here: the reader that decodes it refuses a wrong
// type itself.
func Check(body []byte, paths ...Path) error {
	return unimpl.Error("strictjson.Check", "M3 WP-01")
}
