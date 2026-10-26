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
	"encoding/json"
	"errors"
	"net/http"

	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/unimpl"
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
	CodeParse                 = -32700
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
)

// ErrBatch is a POST body that is not a single JSON object (§11.1).
var ErrBatch = errors.New("mcpwire: batch not supported")

// ErrAmbiguous is a message with a repeated key, or two keys equal under case folding, at a level the relay
// reads (§11.1, D172). From a client it is 400 with -32600; from an upstream the message is not relayed (502
// with -32603 on a JSON response, the end of an SSE stream, is_error true, a dispatch's upstream_error). It wraps
// the *strictjson.KeyError that names the level for the log.
var ErrAmbiguous = errors.New("mcpwire: repeated or case-variant key")

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
// Anything but one JSON object (an array, a scalar, trailing data) is ErrBatch, refused with 400 and an id-less
// -32600 before any forwarding, whatever the version header says. The message object and every level of
// ClientLevels or UpstreamLevels are decoded with exact, case-sensitive keys (internal/strictjson); a repeated or
// case-variant key at any of them is ErrAmbiguous, so the relay never reads another method, tool name, uri,
// taskId, _meta or isError than the party behind it reads from the same bytes (D172).
func Parse(body []byte, src Source) (*Message, error) {
	return nil, unimpl.Error("mcpwire.Parse", "M3 WP-01")
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

// Classify applies §11.12's rules 1–5 to a request whose body (nil for GET and DELETE) Parse accepted:
// initialize is legacy; MCP-Protocol-Version 2026-07-28 is modern; a legacy or absent header needs
// Mcp-Session-Id, else the legacy 400 (-32000); any other version is -32022; GET and DELETE without a session
// are 405.
func Classify(r *http.Request, m *Message) (Era, *Refusal) {
	return 0, &Refusal{Status: http.StatusNotImplemented, Body: []byte(unimpl.Error("mcpwire.Classify", "M3 WP-07").Error())}
}

// CheckModern applies §11.12's checks to a modern request: _meta's protocolVersion and clientCapabilities
// present (400, -32602); protocolVersion equal to MCP-Protocol-Version (400, -32020 HeaderMismatch, D176);
// Mcp-Method equal to method and Mcp-Name equal to params.name, params.uri or params.taskId after Base64
// decoding (400, -32020). A response from a modern client is 400 with -32600.
func CheckModern(h http.Header, m *Message) *Refusal {
	return &Refusal{Status: http.StatusNotImplemented, Body: []byte(unimpl.Error("mcpwire.CheckModern", "M3 WP-07").Error())}
}

// ScrubResult removes every sh.kontra/ key from the protocol-level locations of a result to method (§2.5). From
// an upstream: the result's _meta, tools[]._meta of tools/list, initialize's and server/discover's _meta and
// capabilities (capabilities.experimental["sh.kontra/sealed"], sh.kontra/ keys of capabilities.extensions), task
// objects' _meta and inputRequests[*].params._meta. From a client (its response to a server-to-client request
// method): the result's _meta. It never descends into content, structuredContent or arguments.
//
// sealed is the service's sealed flag on a 2025-11-25 or 2025-06-18 message (D162, D174): a client's response to
// elicitation/create or sampling/createMessage then keeps _meta["sh.kontra/sealed"] when SealedEnvelope accepts
// it. Nothing else keeps it.
func ScrubResult(src Source, method string, result json.RawMessage, sealed bool) (json.RawMessage, error) {
	return nil, unimpl.Error("mcpwire.ScrubResult", "M3 WP-04")
}

// ScrubParams does the same for the params of a request or notification (params._meta). With sealed set
// (D174), _meta["sh.kontra/sealed"] that SealedEnvelope accepts is kept on a client's tools/call and on an
// upstream's elicitation/create or sampling/createMessage, and removed everywhere else.
func ScrubParams(src Source, method string, params json.RawMessage, sealed bool) (json.RawMessage, error) {
	return nil, unimpl.Error("mcpwire.ScrubParams", "M3 WP-04")
}

// SealedEnvelope reports whether v, the value of _meta["sh.kontra/sealed"], is exactly {"v": 1, "envelope":
// <a base64 string>} (spec/ext-sealed.md §3): no other member, decoded with exact keys.
func SealedEnvelope(v json.RawMessage) bool {
	panic(unimpl.Error("mcpwire.SealedEnvelope", "M3 WP-01"))
}

// ErrorBody is the JSON of a JSON-RPC error response (id nil writes "id": null).
func ErrorBody(id json.RawMessage, code int, message string, data any) ([]byte, error) {
	return nil, unimpl.Error("mcpwire.ErrorBody", "M3 WP-01")
}
