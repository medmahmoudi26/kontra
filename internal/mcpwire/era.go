package mcpwire

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"kontra.sh/kontra/internal/strictjson"
)

// The 2026-07-28 rules of one message (spec/relay.md §11.12, D158, D176; M3 WP-07): a modern request's checks
// (CheckModern), the Base64 sentinel of Mcp-Name (DecodeHeaderValue, EncodeHeaderValue), the per-request capabilities
// and the tasks extension in them, -32021's answer, a subscriptions/listen's taskIds, and what the relay reads of an
// upstream's Task or InputRequiredResult (CheckModernTask).

// MethodListen is 2026-07-28's subscriptions/listen.
const MethodListen = "subscriptions/listen"

// Header value encoding (2026-07-28 Streamable HTTP, "Value Encoding"): a value that is not plain ASCII is carried as
// =?base64?<standard Base64 of its UTF-8>?=.
const (
	sentinelPrefix = "=?base64?"
	sentinelSuffix = "?="
)

// ErrHeaderValue is an Mcp-Name (or Mcp-Param-*) value that is neither plain header-safe ASCII nor a well-formed Base64
// sentinel.
var ErrHeaderValue = errors.New("mcpwire: a malformed MCP header value")

// DecodeHeaderValue is an Mcp-Name value as the body compares to it: a Base64 sentinel decoded (standard Base64 with
// padding, valid UTF-8), anything else as it is when it is visible ASCII, space and horizontal tab only. A value that
// is neither is ErrHeaderValue (a 400 with -32020, "a header value contains invalid characters").
func DecodeHeaderValue(v string) (string, error) {
	if len(v) >= len(sentinelPrefix)+len(sentinelSuffix) && strings.HasPrefix(v, sentinelPrefix) && strings.HasSuffix(v, sentinelSuffix) {
		b, err := base64.StdEncoding.Strict().DecodeString(v[len(sentinelPrefix) : len(v)-len(sentinelSuffix)])
		if err != nil || !utf8.Valid(b) {
			return "", fmt.Errorf("%w: a Base64 sentinel that does not decode", ErrHeaderValue)
		}
		return string(b), nil
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < 0x20 && c != '\t') || c > 0x7e {
			return "", fmt.Errorf("%w: a character outside visible ASCII", ErrHeaderValue)
		}
	}
	return v, nil
}

// EncodeHeaderValue is s as a client writes it into Mcp-Name: plain when it is visible ASCII with no white space at
// either end and does not look like a sentinel, else the Base64 sentinel. DecodeHeaderValue(EncodeHeaderValue(s)) is
// s for every valid UTF-8 s.
func EncodeHeaderValue(s string) string {
	plain := s != "" && s[0] != ' ' && s[0] != '\t' && s[len(s)-1] != ' ' && s[len(s)-1] != '\t' &&
		!(strings.HasPrefix(s, sentinelPrefix) && strings.HasSuffix(s, sentinelSuffix))
	for i := 0; plain && i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c > 0x7e {
			plain = false
		}
	}
	if plain {
		return s
	}
	return sentinelPrefix + base64.StdEncoding.EncodeToString([]byte(s)) + sentinelSuffix
}

// NameSource is the params member whose value a modern request's Mcp-Name carries (2026-07-28 Streamable HTTP,
// "Standard Request Headers"; ext-tasks "Routing Headers"): name for tools/call and prompts/get, uri for
// resources/read, taskId for tasks/get, tasks/update and tasks/cancel; ok false for a method with none.
func NameSource(method string) (string, bool) {
	switch method {
	case MethodToolsCall, "prompts/get":
		return "name", true
	case "resources/read":
		return "uri", true
	case "tasks/get", "tasks/update", "tasks/cancel":
		return "taskId", true
	}
	return "", false
}

// CheckModern applies §11.12's checks to a modern request whose body Parse accepted (D158, D176), before policy,
// metering, Admission or forwarding:
//
//   - a response is 400 with -32600 (a 2026-07-28 client sends none); a notification is 202, never forwarded
//     (2026-07-28 defines none on Streamable HTTP);
//   - params._meta's io.modelcontextprotocol/protocolVersion (a string) and io.modelcontextprotocol/clientCapabilities
//     (an object) must be present: 400 with -32602 otherwise ("Per-request protocol fields");
//   - a protocolVersion other than MCP-Protocol-Version is 400 with -32020 HeaderMismatch ("Protocol Version
//     Header"), as is an MCP-Protocol-Version sent more than once (Classify refuses that first; D172: the gate and the
//     upstream read one value);
//   - Mcp-Method must be present once and equal method, and for a method NameSource names, Mcp-Name must be present
//     once and equal that member after DecodeHeaderValue: a missing, repeated, malformed or different one is 400 with
//     -32020, before policy (§11.3: policy reads the body only). A body without that member is 400 with -32602.
//
// Every refusal carries the request's id. Nothing in a refusal repeats a header's or a member's value (a tool name is
// the caller's).
func CheckModern(h http.Header, m *Message) *Refusal {
	switch {
	case m == nil:
		return refuse(http.StatusBadRequest, nil, CodeInvalidRequest, "invalid request", nil)
	case m.Method == "":
		return refuse(http.StatusBadRequest, m.ID, CodeInvalidRequest, "invalid request: a 2026-07-28 client sends no responses", nil)
	case len(m.ID) == 0:
		return &Refusal{Status: http.StatusAccepted}
	}
	meta, _ := Member(m.Params, "_meta")
	if v, ok := Member(meta, MetaProtocolVersion); !ok || firstByte(v) != '"' {
		return refuse(http.StatusBadRequest, m.ID, CodeInvalidParams, "Missing required _meta field: "+MetaProtocolVersion, nil)
	}
	if v, ok := Member(meta, MetaClientCapabilities); !ok || firstByte(v) != '{' {
		return refuse(http.StatusBadRequest, m.ID, CodeInvalidParams, "Missing required _meta field: "+MetaClientCapabilities, nil)
	}
	pv, _ := stringOf(meta, MetaProtocolVersion)
	if vals := h.Values(HeaderProtocolVersion); len(vals) != 1 || pv != vals[0] {
		return refuse(http.StatusBadRequest, m.ID, CodeHeaderMismatch,
			"Header mismatch: MCP-Protocol-Version does not match _meta's protocolVersion", nil)
	}
	if vals := h.Values(HeaderMethod); len(vals) != 1 || vals[0] != m.Method {
		return refuse(http.StatusBadRequest, m.ID, CodeHeaderMismatch, "Header mismatch: Mcp-Method does not match the method", nil)
	}
	src, ok := NameSource(m.Method)
	if !ok {
		return nil
	}
	body, ok := stringOf(m.Params, src)
	if !ok {
		return refuse(http.StatusBadRequest, m.ID, CodeInvalidParams, "Invalid params: params."+src+" is missing or not a string", nil)
	}
	vals := h.Values(HeaderName)
	if len(vals) != 1 {
		return refuse(http.StatusBadRequest, m.ID, CodeHeaderMismatch, "Header mismatch: Mcp-Name is missing", nil)
	}
	dec, err := DecodeHeaderValue(vals[0])
	if err != nil {
		return refuse(http.StatusBadRequest, m.ID, CodeHeaderMismatch, "Header mismatch: Mcp-Name is malformed", nil)
	}
	if dec != body {
		return refuse(http.StatusBadRequest, m.ID, CodeHeaderMismatch, "Header mismatch: Mcp-Name does not match params."+src, nil)
	}
	return nil
}

// refuse is a Refusal carrying a JSON-RPC error with id (null when id is not one).
func refuse(status int, id json.RawMessage, code int, msg string, data any) *Refusal {
	b, err := ErrorBody(id, code, msg, data)
	if err != nil {
		b, _ = ErrorBody(nil, code, msg, data)
	}
	return &Refusal{Status: status, Body: b}
}

// stringOf is obj's member key when it is a JSON string.
func stringOf(obj json.RawMessage, key string) (string, bool) {
	v, ok := Member(obj, key)
	if !ok || firstByte(v) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// ClientCapabilities is a modern request's params._meta["io.modelcontextprotocol/clientCapabilities"], nil when it
// is absent or not an object.
func ClientCapabilities(params json.RawMessage) json.RawMessage {
	meta, _ := Member(params, "_meta")
	caps, ok := Member(meta, MetaClientCapabilities)
	if !ok || firstByte(caps) != '{' {
		return nil
	}
	return caps
}

// DeclaresTasks reports whether a modern request declares the tasks extension: an object at
// clientCapabilities.extensions["io.modelcontextprotocol/tasks"] ("an empty object indicates support"). Parse read
// that member with exact keys (clientReads), so a case variant of it was refused already.
func DeclaresTasks(params json.RawMessage) bool {
	ext, ok := Member(ClientCapabilities(params), "extensions")
	if !ok {
		return false
	}
	v, ok := Member(ext, ExtTasks)
	return ok && firstByte(v) == '{'
}

// WithoutTasks returns params with the tasks extension removed from its per-request capabilities (§11.12: removed
// from every request the gate does not turn into a gate-owned task, so an upstream never answers with a task the gate
// does not own); params come back unchanged when it has none.
func WithoutTasks(params json.RawMessage) (json.RawMessage, error) {
	meta, ok := Member(params, "_meta")
	if !ok || firstByte(meta) != '{' {
		return params, nil
	}
	caps, ok := Member(meta, MetaClientCapabilities)
	if !ok || firstByte(caps) != '{' {
		return params, nil
	}
	ext, ok := Member(caps, "extensions")
	if !ok || firstByte(ext) != '{' {
		return params, nil
	}
	if _, ok := Member(ext, ExtTasks); !ok {
		return params, nil
	}
	ext, err := DeleteMember(ext, ExtTasks)
	if err != nil {
		return nil, err
	}
	if caps, err = SetMember(caps, "extensions", ext); err != nil {
		return nil, err
	}
	if meta, err = SetMember(meta, MetaClientCapabilities, caps); err != nil {
		return nil, err
	}
	return SetMember(params, "_meta", meta)
}

// TasksRequired is -32021's data naming the tasks extension (ext-tasks, "Capability Negotiation"; D176).
var TasksRequired = map[string]any{"requiredCapabilities": map[string]any{"extensions": map[string]any{ExtTasks: map[string]any{}}}}

// MissingTasks is the refusal of a request that needs the tasks extension and does not declare it: 400 with -32021
// (MissingRequiredClientCapability) and data.requiredCapabilities naming the extension (§11.12, D176).
func MissingTasks(id json.RawMessage) *Refusal {
	return refuse(http.StatusBadRequest, id, CodeMissingCapability, "Missing required client capability", TasksRequired)
}

// ListensForTasks reports whether a subscriptions/listen asks for notifications.taskIds: a member of
// params.notifications equal to taskIds under case folding (one a case-insensitive upstream would read as taskIds).
func ListensForTasks(params json.RawMessage) bool {
	n, ok := Member(params, "notifications")
	if !ok || firstByte(n) != '{' {
		return false
	}
	ms, err := members(n)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ms, func(m member) bool { return strings.EqualFold(m.key, "taskIds") })
}

// WithoutTaskIDs returns a subscriptions/listen's params with every member of params.notifications equal to taskIds
// under case folding removed (§11.12: gate-owned tasks are polled, and the gate writes nothing into a relayed
// stream).
func WithoutTaskIDs(params json.RawMessage) (json.RawMessage, error) {
	n, ok := Member(params, "notifications")
	if !ok || firstByte(n) != '{' {
		return params, nil
	}
	ms, err := members(n)
	if err != nil {
		return nil, fmt.Errorf("mcpwire: %w", err)
	}
	kept := ms[:0:0]
	for _, m := range ms {
		if !strings.EqualFold(m.key, "taskIds") {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(ms) {
		return params, nil
	}
	return SetMember(params, "notifications", encodeMembers(kept))
}

// Keys are an object's member keys in the order they are written, nil for anything but an object.
func Keys(obj json.RawMessage) []string {
	if firstByte(obj) != '{' {
		return nil
	}
	ms, err := members(obj)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.key)
	}
	return out
}

// modernTaskReads are the members the modern dispatch reads in the answers to its own requests (§11.12): a
// CallToolResult's, a CreateTaskResult's, an InputRequiredResult's and a DetailedTask's.
var modernTaskReads = []strictjson.Path{{"resultType"}, {"taskId"}, {"status"}, {"statusMessage"}, {"pollIntervalMs"},
	{"ttlMs"}, {"inputRequests"}, {"requestState"}, {"result"}, {"error"}, {"isError"}}

// CheckModernTask checks result, the result of a message Parse accepted from an upstream that answers a request of
// the modern dispatch (its tools/call, tasks/get, tasks/update or tasks/cancel), for a lone case variant of a member
// the dispatch reads there: ErrAmbiguous, as CheckTask does for a 2025-11-25 Task (D172). A result that is not an
// object is the reader's to refuse.
func CheckModernTask(result json.RawMessage) error {
	top, err := strictjson.Members(result)
	if err != nil {
		if errors.Is(err, strictjson.ErrNotObject) {
			return nil
		}
		return wrapStrict(err)
	}
	return wrapStrict(strictjson.CheckReads(top, nil, modernTaskReads))
}

// DiscoverAnswer is what an upstream's answer to the gate's own server/discover says of its era (§11.12, D177).
type DiscoverAnswer int

// The probe's outcomes.
const (
	AnswerUnknown DiscoverAnswer = iota // a modern error, another status, a node's answer, a timeout: the era stays unknown
	AnswerModern                        // a server/discover result
	AnswerLegacy                        // an explicit legacy answer
)

// ClassifyDiscover reads the answer to the gate's own server/discover (§11.12, D177): a 404 or 405 is legacy; a 200
// or 400 whose body is a server/discover result (a JSON-RPC result whose supportedVersions is an array) is modern,
// one whose body is a JSON-RPC error coded -32020, -32021 or -32022 is a modern error and leaves the era unknown, and
// any other body (rule 3's -32000, another error, no JSON-RPC message at all) is legacy; any other status is unknown.
// msg is the answer Parse accepted, nil when the body held none. It returns the result for a modern answer.
func ClassifyDiscover(status int, msg *Message) (DiscoverAnswer, json.RawMessage) {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return AnswerLegacy, nil
	case http.StatusOK, http.StatusBadRequest:
	default:
		return AnswerUnknown, nil
	}
	if msg == nil || msg.Method != "" {
		return AnswerLegacy, nil
	}
	if msg.Error != nil {
		switch msg.Error.Code {
		case CodeHeaderMismatch, CodeMissingCapability, CodeUnsupportedVersion:
			return AnswerUnknown, nil
		}
		return AnswerLegacy, nil
	}
	if v, ok := Member(msg.Result, "supportedVersions"); ok && firstByte(v) == '[' {
		return AnswerModern, msg.Result
	}
	return AnswerLegacy, nil
}
