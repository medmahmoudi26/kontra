// Package mcpwire is the MCP relay's view of a message (spec/relay.md §11, D19): raw JSON-RPC 2.0 with
// json.RawMessage members, never a typed SDK, because go-sdk v1.8.0 drops Tool.execution and rejects tasks/*.
// Unknown members and unknown methods pass through unchanged.
//
// It holds the rules that read one message on its own: one message per POST (§11.1, mcp_batch_refused), strict
// decoding with exact keys (§11.1, D172, mcp_case_smuggling), the era of a request (§11.12, D158), a modern
// request's header and _meta checks (-32602, -32020; D176), the error codes and reserved keys both revisions
// name, and the removal of Kontra's own keys from protocol-level locations, keeping a sealed service's envelope
// where spec/ext-sealed.md §3 puts it (§2.5, D174, mcp_meta_spoof). Sessions, policy, metering and tasks are
// internal/mcprelay's. The node's MCP filters (internal/mcproots, internal/stdiobridge) and the hosting node's
// sealing (internal/sealing) parse through Parse too.
//
// Work packages: M3 WP-01 (the codec: Parse, ErrorBody, SealedEnvelope, which WP-05's node filters need
// before WP-04), M3 WP-04 (the legacy rules and the scrub), M3 WP-07 (the 2026-07-28 rules).
package mcpwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"kontra.sh/kontra/internal/strictjson"
)

// Protocol revisions (spec/relay.md §0, §11.1, §11.12; D65).
const (
	V20250618 = "2025-06-18"
	V20251125 = "2025-11-25"
	V20260728 = "2026-07-28"
)

// Supported is what -32022 lists (§11.12 rule 4).
var Supported = []string{V20260728, V20251125, V20250618}

// HTTP headers the relay reads or writes.
const (
	HeaderProtocolVersion = "MCP-Protocol-Version"
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
	HeaderParamPrefix     = "Mcp-Param-"
	HeaderLastEventID     = "Last-Event-ID"
)

// JSON-RPC error codes the relay sends or reads.
const (
	CodeParse                 = -32700 // a body that is not JSON (ErrParse)
	CodeInvalidRequest        = -32600 // also "batch not supported" (§11.1)
	CodeMethodNotFound        = -32601
	CodeInvalidParams         = -32602 // also "Unknown tool: <name>" (§11.3) and an unknown or foreign task
	CodeInternal              = -32603
	CodeLegacyNoSession       = -32000 // what legacy servers answer without a session (§11.12 rule 3)
	CodeHeaderMismatch        = -32020 // 2026-07-28
	CodeMissingCapability     = -32021 // 2026-07-28
	CodeUnsupportedVersion    = -32022 // 2026-07-28
	CodeURLElicitationRequire = -32042 // 2025-11-25 only; never sent on a modern request
)

// Reserved _meta keys and extension ids.
const (
	MetaRelatedTask        = "io.modelcontextprotocol/related-task"
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	ExtTasks               = "io.modelcontextprotocol/tasks"
	// KontraPrefix is the prefix of Kontra's own _meta keys (D27), which only the gate or the operator writes.
	KontraPrefix = "sh.kontra/"
	// MetaServerInfo is the _meta key of a 2026-07-28 server/discover result that names the server (DiscoverResult;
	// the gate's own /mcp writes it, M4 WP-05).
	MetaServerInfo = "io.modelcontextprotocol/serverInfo"
	// MetaSessionIdle is the initialize result's _meta key in which the gate states the service's session_idle_s,
	// the seconds an idle session is kept (spec/relay.md §11.2, D224): a hiring node keeps a sealed session's key
	// that long.
	MetaSessionIdle = KontraPrefix + "session_idle_s"
)

// ErrBatch is a POST body that is one JSON value but not an object: an array or a scalar (§11.1). From a client it
// is 400 with an id-less -32600 "batch not supported"; an upstream's message is not relayed.
var ErrBatch = errors.New("mcpwire: batch not supported")

// ErrParse is a body that is not JSON at all: a syntax error (a truncated body, a missing brace), nothing, or
// anything but white space after one value. From a client it is 400 with JSON-RPC 2.0's parse error, -32700 and id
// null (CodeParse); an upstream's message is not relayed, as for ErrBatch. Neither is ever forwarded.
var ErrParse = errors.New("mcpwire: parse error")

// ErrAmbiguous is a message with a repeated key, or two keys equal under case folding, at a level the relay
// reads (§11.1, D172), or a lone case variant of a member the relay reads (clientReads, upstreamReads). From a
// client it is 400 with -32600; from an upstream the message is not relayed (502 with -32603 on a JSON response,
// the end of an SSE stream, is_error true, a dispatch's upstream_error). It wraps the *strictjson.KeyError that
// names the level for the log.
var ErrAmbiguous = errors.New("mcpwire: repeated or case-variant key")

// ErrInvalid is a JSON object that is not a JSON-RPC 2.0 message as MCP sends them: jsonrpc absent or not the
// string "2.0"; a method that is not a non-empty string; an id that is not a string, a number or null; params that are not
// an object; an error that is not {code: integer, message: string, data?}; or a shape that is none of a request
// (a method and a non-null id, no result or error), a notification (a method, no id, no result or error) and a
// response (an id, no method, and exactly one of result and an error that is not null; a result's id not null).
// A client's is answered like ErrAmbiguous (400, -32600, JSON-RPC 2.0's "invalid request"); an upstream's is not
// relayed.
var ErrInvalid = errors.New("mcpwire: invalid JSON-RPC message")

// Source is who wrote a message, which decides the levels Parse checks and the scrub's exceptions (§2.5).
type Source int

// Sources.
const (
	FromClient   Source = iota + 1 // a client's request, notification or response (on a node: what the gate forwards)
	FromUpstream                   // an upstream's response, request or notification
)

// ClientLevels are the levels of a client's message the relay reads or rewrites, below the message object
// (spec/relay.md §11.1, D172): params, params._meta, params._meta["io.modelcontextprotocol/clientCapabilities"]
// and its extensions, params.capabilities (initialize), the top level of params.arguments and of
// params.inputResponses, result and result._meta.
var ClientLevels = []strictjson.Path{
	{"params"},
	{"params", "_meta"},
	{"params", "_meta", MetaClientCapabilities},
	{"params", "_meta", MetaClientCapabilities, "extensions"},
	{"params", "capabilities"},
	{"params", "arguments"},
	{"params", "inputResponses"},
	{"result"},
	{"result", "_meta"},
}

// UpstreamLevels are those of an upstream's message (§11.1): params and params._meta (requests and
// notifications), result, result._meta, each of result.tools and its _meta (tools/list), result.capabilities and
// its experimental and extensions (initialize, server/discover), the task objects the gate reads (a 2025-11-25
// CreateTaskResult's result.task; a 2026-07-28 task's or InputRequiredResult's result.inputRequests, each of its
// entries and their params and params._meta, and a terminal task's result.result and result.error), and error.
var UpstreamLevels = []strictjson.Path{
	{"params"},
	{"params", "_meta"},
	{"result"},
	{"result", "_meta"},
	{"result", "tools", "*"},
	{"result", "tools", "*", "_meta"},
	{"result", "capabilities"},
	{"result", "capabilities", "experimental"},
	{"result", "capabilities", "extensions"},
	{"result", "task"},
	{"result", "inputRequests"},
	{"result", "inputRequests", "*"},
	{"result", "inputRequests", "*", "params"},
	{"result", "inputRequests", "*", "params", "_meta"},
	{"result", "result"},
	{"result", "error"},
	{"error"},
}

// clientReads and upstreamReads are the members Parse, the relay and the node's filters read, by exact name, at the
// message object and at the levels of ClientLevels and UpstreamLevels (the last element of each path is the
// member, the elements before it its level; every level is also a member read at its parent). A lone key that
// equals one of them under case folding without being it is ErrAmbiguous, like a pair: a decoder that matches names
// case-insensitively (encoding/json into a struct, as mark3labs/mcp-go reads a message) takes {"METHOD":
// "tools/call"} for a request, or {"NAME": "rm"} for the tool rm, where the relay reads no method or no name and
// would let the message through as a response or past per-tool policy; and an upstream's {"ISERROR": true} would
// be metered as a success while such a client reads a failure.
//
// SPEC-CONFLICT (D191): relay §11.1 and D172 refuse a level in which "a key repeats, or two keys are equal under
// Unicode case folding", which a lone variant is not, and would let it through. The review of M3 WP-01 (and the
// plan audit's gaps[0] fix: "any key that equals a member it reads ... case-insensitively but not exactly") refuses
// it too, with the same answer as a pair, as §11.1 now says. A reader that starts reading another member adds it
// here, as a level is added to the spec first.
//
// The node's MCP filters (M3 WP-05) read: the client's capabilities.roots in an initialize and roots in its
// per-request clientCapabilities (internal/mcproots replaces or removes them, D173), roots in a client response's
// result (a roots answer, which internal/mcproots drops whatever its id), the progressToken of a client request's
// _meta and of a server's notification (internal/stdiobridge routes progress to the request's POST stream,
// spec/tunnel.md §10).
var (
	clientReads = withLevels(ClientLevels,
		strictjson.Path{"jsonrpc"}, strictjson.Path{"id"}, strictjson.Path{"method"}, strictjson.Path{"error"},
		strictjson.Path{"params", "name"}, strictjson.Path{"params", "uri"}, strictjson.Path{"params", "taskId"},
		strictjson.Path{"params", "task"},
		strictjson.Path{"params", "_meta", MetaProtocolVersion}, strictjson.Path{"params", "_meta", MetaClientInfo},
		strictjson.Path{"params", "_meta", MetaRelatedTask},
		strictjson.Path{"params", "_meta", "progressToken"},
		strictjson.Path{"params", "capabilities", "roots"},
		strictjson.Path{"params", "_meta", MetaClientCapabilities, "roots"},
		strictjson.Path{"result", "roots"},
		// A client's answer to a gate request id: the related-task key the gate rewrites back (M3 WP-06, §11.6).
		strictjson.Path{"result", "_meta", MetaRelatedTask},
		// A 2026-07-28 request (M3 WP-07, §11.12): the tasks extension in its per-request capabilities, which
		// decides a gate-owned task (D159) and which the gate removes from what it forwards; the notifications a
		// subscriptions/listen asks for, whose taskIds the gate removes (D176).
		strictjson.Path{"params", "_meta", MetaClientCapabilities, "extensions", ExtTasks},
		strictjson.Path{"params", "notifications"},
	)
	upstreamReads = withLevels(UpstreamLevels,
		strictjson.Path{"jsonrpc"}, strictjson.Path{"id"}, strictjson.Path{"method"},
		strictjson.Path{"params", "name"}, strictjson.Path{"params", "uri"}, strictjson.Path{"params", "taskId"},
		strictjson.Path{"params", "task"}, strictjson.Path{"params", "_meta", MetaRelatedTask},
		strictjson.Path{"params", "progressToken"},
		strictjson.Path{"result", "isError"}, strictjson.Path{"result", "protocolVersion"},
		strictjson.Path{"result", "tools", "*", "name"}, strictjson.Path{"result", "tools", "*", "execution"},
		strictjson.Path{"result", "capabilities", "tasks"},
		strictjson.Path{"result", "inputRequests", "*", "method"},
		strictjson.Path{"result", "result", "isError"},
		strictjson.Path{"error", "code"}, strictjson.Path{"error", "message"},
		// The durable dispatch's reads of a 2025-11-25 upstream (M3 WP-06, §11.5): a CreateTaskResult's task, and the
		// related-task key of a tasks/result answer, which it rewrites. The members of a Task it reads at result
		// level (a tasks/get or tasks/cancel answer) are not here: every upstream message of every service would
		// then refuse a lone variant of status or taskId in its result, which the gate reads only in the answers to
		// its own task requests; CheckTask checks them there.
		strictjson.Path{"result", "task", "taskId"}, strictjson.Path{"result", "task", "status"},
		strictjson.Path{"result", "task", "statusMessage"}, strictjson.Path{"result", "task", "pollInterval"},
		strictjson.Path{"result", "_meta", MetaRelatedTask},
		// What the relay reads of a 2026-07-28 answer (M3 WP-07, §11.12): resultType (an InputRequiredResult answering
		// a plain call is metered as no outcome), a server/discover result's supportedVersions and its tasks extension
		// (the service's era and gate-owned tasks, D159, D177), and the ttlMs of a tools/list (capped at 600000).
		strictjson.Path{"result", "resultType"}, strictjson.Path{"result", "supportedVersions"},
		strictjson.Path{"result", "ttlMs"}, strictjson.Path{"result", "capabilities", "extensions", ExtTasks},
	)
	// taskReads are the members of a Task (MCP 2025-11-25's tasks/get result, "Result & Task", and tasks/cancel's)
	// that the durable dispatch and forward_cancel read in the answers to their own requests (M3 WP-06, §11.5).
	taskReads = []strictjson.Path{{"taskId"}, {"status"}, {"statusMessage"}, {"pollInterval"}}
)

// CheckTask checks result, the result of a message Parse accepted from an upstream that answers a tasks/get or a
// tasks/cancel the gate sent, for a lone case variant of a Task member the gate reads there (taskId, status,
// statusMessage, pollInterval): ErrAmbiguous, as Parse refuses one of the members it reads in every message
// (D172; see clientReads). A pair of case variants at result level Parse has refused already. A result that is not
// an object is the reader's to refuse, as for Parse's levels.
func CheckTask(result json.RawMessage) error {
	top, err := strictjson.Members(result)
	if err != nil {
		if errors.Is(err, strictjson.ErrNotObject) {
			return nil
		}
		return wrapStrict(err)
	}
	return wrapStrict(strictjson.CheckReads(top, nil, taskReads))
}

// withLevels is reads plus each level of levels as a member read at its parent; a level ending in "*" (every
// element of result.tools) names the member holding them (tools, read at result).
func withLevels(levels []strictjson.Path, reads ...strictjson.Path) []strictjson.Path {
	var out []strictjson.Path
	for _, p := range levels {
		for len(p) > 0 && p[len(p)-1] == "*" {
			p = p[:len(p)-1]
		}
		if len(p) > 0 && !slices.ContainsFunc(out, func(q strictjson.Path) bool { return slices.Equal(p, q) }) {
			out = append(out, p)
		}
	}
	return append(out, reads...)
}

// Message is one JSON-RPC 2.0 message with its members kept raw. A request has Method and ID, a notification
// Method and no ID, a response ID and Result or Error. Parse fills it from members read with exact keys; the
// struct tags are for writing only, never for decoding a message a client or an upstream sent.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Parse decodes one message from src: a POST body, or one message of an upstream's JSON or SSE response.
// Bytes that are not one JSON value are ErrParse (400, -32700); one JSON value that is not an object (an array, a
// scalar) is ErrBatch, refused with 400 and an id-less -32600 before any forwarding, whatever the version header
// says. The message object and every level of ClientLevels or UpstreamLevels are decoded with exact,
// case-sensitive keys (internal/strictjson); a repeated or case-variant key at any of them, or a lone case variant
// of a member the relay reads there (clientReads, upstreamReads), is ErrAmbiguous, so the relay never reads
// another method, tool name, uri, taskId, _meta or isError than the party behind it reads from the same bytes
// (D172).
//
// Message is filled from the message object's exact members, and a response's error object is decoded with exact
// keys too (a repeated or case-variant key in it is ErrAmbiguous from either source: Parse reads it). The object
// must be exactly one of a request, a notification and a response, with jsonrpc "2.0" and params, when present, an
// object; anything else, or a member of the wrong type, is ErrInvalid (see there). Unknown members are left in the
// bytes, which the relay forwards.
//
// When the message object itself was read unambiguously and its id is valid, Parse returns a message carrying that
// id beside an ErrAmbiguous found below the message object or an ErrInvalid, so that a client's refusal carries it
// (§11.1: "the id when it was read unambiguously, null otherwise"); nothing else of that message may be used.
// Otherwise the message is nil.
func Parse(body []byte, src Source) (*Message, error) {
	var levels, reads []strictjson.Path
	switch src {
	case FromClient:
		levels, reads = ClientLevels, clientReads
	case FromUpstream:
		levels, reads = UpstreamLevels, upstreamReads
	default:
		return nil, fmt.Errorf("mcpwire: unknown source %d", src)
	}
	top, err := strictjson.Members(body)
	if err != nil {
		return nil, wrapStrict(err)
	}
	id, hasID := top["id"]
	if hasID && !validID(id) {
		return nil, fmt.Errorf("%w: id is not a string, a number or null", ErrInvalid)
	}
	if err := strictjson.CheckReads(top, levels, reads); err != nil {
		var ke *strictjson.KeyError
		if errors.As(err, &ke) && len(ke.Path) == 0 {
			return nil, wrapStrict(err) // the message object itself is ambiguous: no id was read unambiguously
		}
		return &Message{ID: id}, wrapStrict(err)
	}
	invalid := func(why string) (*Message, error) {
		return &Message{ID: id}, fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	m := &Message{ID: id}
	if v, ok := top["jsonrpc"]; !ok || firstByte(v) != '"' || json.Unmarshal(v, &m.JSONRPC) != nil || m.JSONRPC != "2.0" {
		return invalid(`jsonrpc is not "2.0"`)
	}
	method, hasMethod := top["method"]
	if hasMethod {
		// An empty method is refused too, so that Method == "" means a response and nothing else.
		if firstByte(method) != '"' || json.Unmarshal(method, &m.Method) != nil || m.Method == "" {
			return invalid("method is not a non-empty string")
		}
	}
	if v, ok := top["params"]; ok {
		if firstByte(v) != '{' {
			return invalid("params is not an object")
		}
		m.Params = v
	}
	result, hasResult := top["result"]
	if hasResult {
		m.Result = result
	}
	if v, ok := top["error"]; ok {
		e, err := parseError(v)
		if err != nil {
			return &Message{ID: id}, err
		}
		m.Error = e
	}
	null := hasID && bytes.Equal(bytes.TrimSpace(id), []byte("null"))
	switch {
	case hasMethod && (hasResult || m.Error != nil):
		return invalid("a request or notification carries a result or an error")
	case hasMethod && null:
		return invalid("a request's id is null") // MCP: "the ID MUST NOT be null"
	case hasMethod:
		return m, nil // a request, or a notification without an id
	case !hasID:
		return invalid("no method and no id: neither a request, a notification nor a response")
	case hasResult == (m.Error != nil):
		return invalid("a response carries exactly one of result and error")
	case hasResult && null:
		return invalid("a result's id is null")
	}
	return m, nil
}

// wrapStrict turns internal/strictjson's refusals into Parse's.
func wrapStrict(err error) error {
	var ke *strictjson.KeyError
	switch {
	case errors.As(err, &ke):
		return fmt.Errorf("%w: %w", ErrAmbiguous, err)
	case errors.Is(err, strictjson.ErrSyntax):
		return fmt.Errorf("%w: %w", ErrParse, err)
	case errors.Is(err, strictjson.ErrNotObject):
		return fmt.Errorf("%w: %w", ErrBatch, err)
	default:
		return err
	}
}

// validID reports whether v is a JSON-RPC id: a string, a number or null.
func validID(v json.RawMessage) bool {
	switch c := firstByte(v); {
	case c == '"':
		var s string
		return json.Unmarshal(v, &s) == nil
	case c == 'n':
		return bytes.Equal(bytes.TrimSpace(v), []byte("null"))
	case c == '-' || (c >= '0' && c <= '9'):
		var n json.Number
		return json.Unmarshal(v, &n) == nil
	}
	return false
}

// IDKey is a JSON-RPC id (or a progress token: a string or a number) in a form two equal ids share, for a map: a
// string by its value, a number by its value as a float64 (as JSON.parse reads it, so 1 and 1.0 are one id), ok
// false for anything else (null, an object, an array, a boolean, nothing).
func IDKey(id json.RawMessage) (string, bool) {
	switch c := firstByte(id); {
	case c == '"':
		var s string
		if json.Unmarshal(id, &s) != nil {
			return "", false
		}
		return "s:" + s, true
	case c == '-' || (c >= '0' && c <= '9'):
		var n json.Number
		if json.Unmarshal(id, &n) != nil {
			return "", false
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return "", false
		}
		return "n:" + strconv.FormatFloat(f, 'g', -1, 64), true
	}
	return "", false
}

// IsErrorFlag reads a CallToolResult's top-level isError (exact key): set when it is the JSON literal true; ok false
// when it is present and not a JSON boolean ("true", 1, null), a malformed result, since clients do not agree on it
// (the Python SDK's pydantic reads "true" and 1 as true, the TypeScript SDK's schema refuses the result).
func IsErrorFlag(result json.RawMessage) (set, ok bool) {
	v, has := Member(result, "isError")
	if !has {
		return false, true
	}
	switch string(bytes.TrimSpace(v)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// parseError decodes an error object with exact keys: code an integer, message a string, data any value. A null
// error (some JSON-RPC 1.0 habits write one beside a result) is no error.
func parseError(v json.RawMessage) (*Error, error) {
	if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return nil, nil
	}
	mem, err := strictjson.Members(v)
	if err != nil {
		var ke *strictjson.KeyError
		if errors.As(err, &ke) {
			ke.Path = []string{"error"}
			return nil, fmt.Errorf("%w: %w", ErrAmbiguous, ke)
		}
		return nil, fmt.Errorf("%w: error is not an object", ErrInvalid)
	}
	e := &Error{}
	code, ok := mem["code"]
	if !ok {
		return nil, fmt.Errorf("%w: error has no code", ErrInvalid)
	}
	var n json.Number
	if c := firstByte(code); !(c == '-' || c >= '0' && c <= '9') || json.Unmarshal(code, &n) != nil {
		return nil, fmt.Errorf("%w: error code is not a number", ErrInvalid)
	}
	i, err := strconv.Atoi(n.String())
	if err != nil {
		return nil, fmt.Errorf("%w: error code is not an integer", ErrInvalid)
	}
	e.Code = i
	msg, ok := mem["message"]
	if !ok || firstByte(msg) != '"' || json.Unmarshal(msg, &e.Message) != nil {
		return nil, fmt.Errorf("%w: error message is not a string", ErrInvalid)
	}
	if d, ok := mem["data"]; ok {
		e.Data = d
	}
	return e, nil
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

// Era is a request's protocol era (§11.12).
type Era int

// Eras.
const (
	EraLegacy Era = iota + 1 // 2025-11-25 and 2025-06-18: initialize and sessions
	EraModern                // 2026-07-28: per-request _meta, no session
)

// Refusal is an answer the relay writes itself before forwarding: an HTTP status and a body (a JSON-RPC error,
// id-less unless the request had one).
type Refusal struct {
	Status int
	Body   []byte
}

// initializeNotificationBody is the answer to an initialize without an id (D312): §11.1's invalid request, id null.
var initializeNotificationBody = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`)

// LegacyNoSessionBody is §11.12 rule 3's answer, what a legacy server sends a request without a session: a
// dual-era client reads it as "not a modern server" and falls back to initialize.
var LegacyNoSessionBody = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: No valid session ID provided"}}`)

// Classify applies §11.12's rules 1–5 to a request whose body (nil for GET and DELETE) Parse accepted:
// initialize is legacy; MCP-Protocol-Version 2026-07-28 is modern; a legacy or absent header needs
// Mcp-Session-Id, else the legacy 400 (-32000); any other version is -32022; GET and DELETE without a session
// are 405.
//
// An initialize is a request (MCP lifecycle): one without an id is 400 with -32600 "invalid request" and id null,
// as §11.1 refuses an invalid message, before anything is forwarded (D312). Routed on its method alone, it would
// open a session no response can be matched to, and the relay would forward it as neither a request, which takes a
// calls slot (D304), nor a notification, which is answered 202 (D307).
//
// GET and DELETE with a session take §11.1's versions as a POST does: 2025-11-25, 2025-06-18 or none is legacy, and
// anything else is 400 with -32022 (id null), not forwarded with the pinned request.
//
// SPEC-CONFLICT (D201; relay §11.1, §11.12 rule 5): §11.1 makes a request with 2026-07-28 a modern one, and rule 5
// makes GET and DELETE legacy only; neither says what a GET or DELETE carrying 2026-07-28 and a session gets. The
// closest faithful behaviour: it is modern, so its Mcp-Session-Id is never read (rule 2), and a modern request has
// no GET stream and no session to delete: such a GET or DELETE is one without a session, 405 (rule 5), as a
// 2026-07-28 server answers a GET.
//
// MCP-Protocol-Version is read once, as its field value: a header sent more than once is its lines' values joined
// with ", " (RFC 9110 §5.3), which names no version, so such a request is rule 4's -32022 (and a GET or DELETE
// without a session 405), never classified by one of its lines while the upstream reads another (D172: one reading).
//
// Built by M3 WP-04, which serves the legacy era and needs rules 1, 3, 4 and 5; WP-07 serves what rule 2 sends
// it.
func Classify(r *http.Request, m *Message) (Era, *Refusal) {
	sid := r.Header.Get(HeaderSessionID)
	if r.Method != http.MethodPost {
		// Rule 5: GET and DELETE are legacy only.
		switch v := fieldValue(r.Header, HeaderProtocolVersion); v {
		case V20251125, V20250618, "":
			if sid == "" {
				return 0, &Refusal{Status: http.StatusMethodNotAllowed}
			}
			return EraLegacy, nil
		case V20260728:
			return 0, &Refusal{Status: http.StatusMethodNotAllowed}
		default:
			if sid == "" {
				return 0, &Refusal{Status: http.StatusMethodNotAllowed}
			}
			return 0, unsupportedVersion(nil, v) // §11.1
		}
	}
	if m != nil && m.Method == MethodInitialize {
		if len(m.ID) == 0 {
			return 0, &Refusal{Status: http.StatusBadRequest, Body: initializeNotificationBody}
		}
		return EraLegacy, nil // rule 1, whatever the headers say
	}
	switch v := fieldValue(r.Header, HeaderProtocolVersion); v {
	case V20260728:
		return EraModern, nil // rule 2: its Mcp-Session-Id and Last-Event-ID are never read
	case V20251125, V20250618, "":
		if sid == "" {
			return 0, &Refusal{Status: http.StatusBadRequest, Body: LegacyNoSessionBody} // rule 3
		}
		return EraLegacy, nil
	default:
		var id json.RawMessage
		if m != nil {
			id = m.ID
		}
		return 0, unsupportedVersion(id, v) // rule 4
	}
}

// fieldValue is the field value of the header key: its lines' values joined with ", " (RFC 9110 §5.3), "" when it
// is absent.
func fieldValue(h http.Header, key string) string {
	return strings.Join(h.Values(key), ", ")
}

// unsupportedVersion is rule 4's refusal of MCP-Protocol-Version v: 400 with -32022 naming the supported versions
// and v, with id (null when there is none).
func unsupportedVersion(id json.RawMessage, v string) *Refusal {
	data := map[string]any{"supported": Supported, "requested": v}
	b, err := ErrorBody(id, CodeUnsupportedVersion, "Unsupported protocol version", data)
	if err != nil {
		b, _ = ErrorBody(nil, CodeUnsupportedVersion, "Unsupported protocol version", data)
	}
	return &Refusal{Status: http.StatusBadRequest, Body: b}
}

// SealedEnvelope reports whether v, the value of _meta["sh.kontra/sealed"], is exactly {"v": 1, "envelope":
// <a base64 string>} (spec/ext-sealed.md §3): no other member, decoded with exact keys.
//
// v must be the JSON number 1 exactly (not 1.0 or 1e0), and envelope a JSON string of standard base64 with
// padding and nothing else (no white space), decoding to at least one byte. A repeated member, a case variant
// ("V"), a third member, another v or a non-string envelope is false.
func SealedEnvelope(v json.RawMessage) bool {
	m, err := strictjson.Members(v)
	if err != nil || len(m) != 2 {
		return false
	}
	ver, ok := m["v"]
	if !ok || !bytes.Equal(bytes.TrimSpace(ver), []byte("1")) {
		return false
	}
	env, ok := m["envelope"]
	if !ok || firstByte(env) != '"' {
		return false
	}
	var s string
	if err := json.Unmarshal(env, &s); err != nil {
		return false
	}
	return stdBase64(s)
}

// stdBase64 reports whether s is non-empty standard base64 with padding, in the alphabet only (Go's decoder
// would skip CR and LF).
func stdBase64(s string) bool {
	if s == "" || len(s)%4 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	return err == nil && len(b) > 0
}

// ErrorBody is the JSON of a JSON-RPC error response (id nil writes "id": null).
//
// id must be a JSON-RPC id (a string, a number or null) as read by Parse; data, when not nil, is encoded with
// encoding/json (a json.RawMessage is written as it is, and must be valid JSON).
func ErrorBody(id json.RawMessage, code int, message string, data any) ([]byte, error) {
	if len(bytes.TrimSpace(id)) == 0 {
		id = json.RawMessage("null")
	} else if !validID(id) {
		return nil, fmt.Errorf("mcpwire: %s is not a JSON-RPC id", id)
	}
	e := Error{Code: code, Message: message}
	if data != nil {
		d, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("mcpwire: error data: %w", err)
		}
		e.Data = d
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   Error           `json:"error"`
	}{"2.0", id, e})
}
