package gatemcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/strictjson"
)

// The four tools (spec §12.2). Their input schemas are spec/schema/mcp-tools.json's $defs; every tool declares
// annotations.readOnlyHint and annotations.destructiveHint.
const (
	ToolSearch   = "search"
	ToolDescribe = "describe"
	ToolHire     = "hire"
	ToolReply    = "reply"
)

// Ref names a service in search results and in describe's and hire's arguments: <name> on a single-tenant gate,
// <tenant>/<name> on a multi-tenant one (§12.2, DM4-4).
type Ref struct {
	Tenant string // "" on a single-tenant gate
	Name   string
}

// ErrUnknownService is a ref that names no service the principal may see or hire: malformed, of the other tenancy's
// form, unknown, disabled, of an expired tenant, or one whose allow: does not admit the principal; one answer for
// all of them (§2.1, D50).
var ErrUnknownService = errors.New("unknown service")

// ParseRef parses a ref as the gate's tenancy writes it; anything else is ErrUnknownService.
func ParseRef(s string, multi bool) (Ref, error) {
	tenant, name, slash := strings.Cut(s, "/")
	switch {
	case multi && slash && config.NameRE.MatchString(tenant) && config.NameRE.MatchString(name):
		return Ref{Tenant: tenant, Name: name}, nil
	case !multi && !slash && config.NameRE.MatchString(s):
		return Ref{Name: s}, nil
	}
	return Ref{}, ErrUnknownService
}

// String is the ref as search returns it.
func (r Ref) String() string {
	if r.Tenant == "" {
		return r.Name
	}
	return r.Tenant + "/" + r.Name
}

// SearchArgs are search's arguments (mcp-tools.json#/$defs/search).
type SearchArgs struct {
	Query  string `json:"query,omitempty"`
	Kind   string `json:"kind,omitempty"`  // a2a | mcp
	Price  string `json:"price,omitempty"` // equality on the price_model annotation; "free": none (DM4-4)
	Cursor string `json:"cursor,omitempty"`
}

// ServiceEntry is one search result.
type ServiceEntry struct {
	Ref         string `json:"ref"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   bool   `json:"available"`       // a connected session of a live replica, or a direct backend
	Price       string `json:"price,omitempty"` // the operator's search.price annotation, verbatim (§13.4)
}

// SearchResult is search's structuredContent (also sent as a text block).
type SearchResult struct {
	Services   []ServiceEntry `json:"services"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// DescribeArgs are describe's arguments.
type DescribeArgs struct {
	Service string `json:"service"`
}

// DescribeResult is describe's structuredContent: the served card (A2A) or the cached, filtered, annotated
// tools/list (MCP; ToolsCached false and no Tools when the cache is cold, DM4-4), and how to call the service.
type DescribeResult struct {
	Ref         string            `json:"ref"`
	Kind        string            `json:"kind"`
	Card        json.RawMessage   `json:"card,omitempty"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	ToolsCached *bool             `json:"toolsCached,omitempty"`
	Sealed      bool              `json:"sealed"`
	Available   bool              `json:"available"`
	URLs        []string          `json:"urls"` // {public_url}/agents/… or {public_url}/mcp/…
}

// HireArgs are hire's arguments: Message is a string or {parts: [Part]} (mcp-tools.json#/$defs/message).
type HireArgs struct {
	Service   string          `json:"service"`
	Message   json.RawMessage `json:"message"`
	ContextID string          `json:"contextId,omitempty"`
}

// ReplyArgs are reply's arguments.
type ReplyArgs struct {
	TaskID  uuid.UUID       `json:"taskId"`
	Message json.RawMessage `json:"message"`
}

// PlainState is the structuredContent of a plain hire or reply (and of tasks/result): {taskId, state, data?}, state
// the A2A state, data the values of the rendered data parts in order, message first, then each artifact's (§12.2,
// DM4-22).
type PlainState struct {
	TaskID string `json:"taskId"`
	State  string `json:"state"`
	Data   []any  `json:"data,omitempty"`
}

// The tools' input schemas: spec/schema/mcp-tools.json's $defs, each made self-contained (the $refs to message and
// part inlined), so a client that does not resolve $ref reads the whole schema. TestToolsListSchemas holds them equal
// to the file under JCS.
const (
	schemaPart = `{"type":"object","description":"An A2A 1.0 Part: exactly one of text, raw (base64), url, data; plus optional filename, mediaType, metadata.",` +
		`"oneOf":[{"required":["text"]},{"required":["raw"]},{"required":["url"]},{"required":["data"]}],` +
		`"properties":{"text":{"type":"string"},"raw":{"type":"string","contentEncoding":"base64"},"url":{"type":"string","format":"uri"},"data":{},` +
		`"filename":{"type":"string"},"mediaType":{"type":"string"},"metadata":{"type":"object"}}}`
	schemaMessage = `{"oneOf":[{"type":"string","minLength":1},{"type":"object","required":["parts"],"additionalProperties":false,` +
		`"properties":{"parts":{"type":"array","minItems":1,"items":` + schemaPart + `}}}]}`
	schemaSearch = `{"type":"object","additionalProperties":false,"properties":{"query":{"type":"string","maxLength":256},` +
		`"kind":{"enum":["a2a","mcp"]},"price":{"type":"string","maxLength":32},"cursor":{"type":"string"}}}`
	schemaDescribe = `{"type":"object","required":["service"],"additionalProperties":false,` +
		`"properties":{"service":{"type":"string","pattern":"^([a-z0-9-]+/)?[a-z0-9-]+$"}}}`
	schemaHire = `{"type":"object","required":["service","message"],"additionalProperties":false,` +
		`"properties":{"service":{"type":"string","pattern":"^([a-z0-9-]+/)?[a-z0-9-]+$"},"message":` + schemaMessage +
		`,"contextId":{"type":"string","maxLength":256}}}`
	schemaReply = `{"type":"object","required":["taskId","message"],"additionalProperties":false,` +
		`"properties":{"taskId":{"type":"string","format":"uuid"},"message":` + schemaMessage + `}}`
)

// tool is one entry of tools/list.
type tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations toolAnnotations `json:"annotations"`
	Execution   *toolExecution  `json:"execution,omitempty"`
}

type toolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

type toolExecution struct {
	TaskSupport string `json:"taskSupport"`
}

// tools are the four tools, in the order tools/list gives them.
var tools = []tool{
	{Name: ToolSearch, Title: "Search services",
		Description: "Find the agents and MCP servers this gate serves that you may call. Returns refs for describe and hire.",
		InputSchema: json.RawMessage(schemaSearch),
		Annotations: toolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
	{Name: ToolDescribe, Title: "Describe a service",
		Description: "Show a service's agent card (A2A) or its tools (MCP), whether it is sealed and available, and the URLs to call it directly.",
		InputSchema: json.RawMessage(schemaDescribe),
		Annotations: toolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
	{Name: ToolHire, Title: "Hire an agent",
		Description: "Send a message to an A2A agent as a new task, and answer its result. A priced task asks a person to approve its quote first, at the URL given.",
		InputSchema: json.RawMessage(schemaHire),
		Annotations: toolAnnotations{OpenWorldHint: true}},
	{Name: ToolReply, Title: "Reply to a task",
		Description: "Answer the question a hired task asks (input required), and answer its new state.",
		InputSchema: json.RawMessage(schemaReply),
		Annotations: toolAnnotations{OpenWorldHint: true}},
}

// ToolsList is the tools/list result for a request of era: the four tools with their input schemas; hire
// declares execution.taskSupport "optional" in the legacy list of a 2025-11-25 session only (§12.5).
func ToolsList(era mcpwire.Era, protocolVersion string) (json.RawMessage, error) {
	list := make([]tool, len(tools))
	copy(list, tools)
	if era == mcpwire.EraLegacy && protocolVersion == mcpwire.V20251125 {
		for i := range list {
			if list[i].Name == ToolHire {
				list[i].Execution = &toolExecution{TaskSupport: "optional"}
			}
		}
	}
	return json.Marshal(map[string]any{"tools": list})
}

// FirstRequest is a hire task's resend key: SHA-256(utf8(service ref) ‖ 0x00 ‖ JCS(arguments)) (§12.3 Retries),
// which replaces §15's request hash for these tasks.
func FirstRequest(ref Ref, arguments json.RawMessage) ([32]byte, error) {
	canon, err := jcs.Transform(arguments)
	if err != nil {
		return [32]byte{}, fmt.Errorf("gatemcp: the arguments' JCS: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(ref.String()))
	h.Write([]byte{0})
	h.Write(canon)
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, nil
}

// argError is a tool call whose arguments break the tool's input schema: a tool execution error (isError), as MCP
// 2025-11-25 answers an input validation error, naming what is wrong but never echoing a value.
type argError struct{ msg string }

func (e *argError) Error() string { return "invalid arguments: " + e.msg }

func badArgs(format string, a ...any) error { return &argError{msg: fmt.Sprintf(format, a...)} }

// members reads an arguments object with exact keys (D172), refusing a member outside allowed.
func members(args json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	m, err := strictjson.Members(args)
	if err != nil {
		return nil, badArgs("arguments must be one object with distinct keys")
	}
	for k := range m {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return nil, badArgs("unknown argument %s", strictjson.Quote(k))
		}
	}
	return m, nil
}

// stringArg reads an optional string argument of at most maxLen characters (0: unbounded).
func stringArg(m map[string]json.RawMessage, key string, maxLen int) (string, bool, error) {
	raw, ok := m[key]
	if !ok {
		return "", false, nil
	}
	var s string
	if bytes.TrimSpace(raw)[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", true, badArgs("%s must be a string", key)
	}
	if maxLen > 0 && utf8.RuneCountInString(s) > maxLen {
		return "", true, badArgs("%s is over %d characters", key, maxLen)
	}
	return s, true, nil
}

var refPattern = regexp.MustCompile(`^([a-z0-9-]+/)?[a-z0-9-]+$`)

// parseSearchArgs reads search's arguments against its schema.
func parseSearchArgs(args json.RawMessage) (SearchArgs, error) {
	m, err := members(args, "query", "kind", "price", "cursor")
	if err != nil {
		return SearchArgs{}, err
	}
	var a SearchArgs
	if a.Query, _, err = stringArg(m, "query", 256); err != nil {
		return a, err
	}
	if a.Kind, _, err = stringArg(m, "kind", 0); err != nil {
		return a, err
	}
	if a.Kind != "" && a.Kind != config.KindA2A && a.Kind != config.KindMCP {
		return a, badArgs("kind must be a2a or mcp")
	}
	if _, ok := m["kind"]; ok && a.Kind == "" {
		return a, badArgs("kind must be a2a or mcp")
	}
	if a.Price, _, err = stringArg(m, "price", 32); err != nil {
		return a, err
	}
	if a.Cursor, _, err = stringArg(m, "cursor", 0); err != nil {
		return a, err
	}
	return a, nil
}

// serviceArg reads the service argument (describe, hire), required, against the ref pattern.
func serviceArg(m map[string]json.RawMessage) (string, error) {
	s, ok, err := stringArg(m, "service", 0)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", badArgs("service is required")
	case !refPattern.MatchString(s):
		return "", badArgs("service is not a service ref")
	}
	return s, nil
}

// parseDescribeArgs reads describe's arguments.
func parseDescribeArgs(args json.RawMessage) (DescribeArgs, error) {
	m, err := members(args, "service")
	if err != nil {
		return DescribeArgs{}, err
	}
	s, err := serviceArg(m)
	return DescribeArgs{Service: s}, err
}

// parseHireArgs reads hire's arguments and builds the message's parts.
func parseHireArgs(args json.RawMessage) (HireArgs, a2a.ContentParts, error) {
	m, err := members(args, "service", "message", "contextId")
	if err != nil {
		return HireArgs{}, nil, err
	}
	var a HireArgs
	if a.Service, err = serviceArg(m); err != nil {
		return a, nil, err
	}
	if a.ContextID, _, err = stringArg(m, "contextId", 256); err != nil {
		return a, nil, err
	}
	raw, ok := m["message"]
	if !ok {
		return a, nil, badArgs("message is required")
	}
	a.Message = raw
	parts, err := ParseMessage(raw)
	return a, parts, err
}

// parseReplyArgs reads reply's arguments and builds the message's parts.
func parseReplyArgs(args json.RawMessage) (ReplyArgs, a2a.ContentParts, error) {
	m, err := members(args, "taskId", "message")
	if err != nil {
		return ReplyArgs{}, nil, err
	}
	var a ReplyArgs
	s, ok, err := stringArg(m, "taskId", 0)
	switch {
	case err != nil:
		return a, nil, err
	case !ok:
		return a, nil, badArgs("taskId is required")
	}
	if a.TaskID, err = uuid.Parse(s); err != nil || len(s) != 36 {
		return a, nil, badArgs("taskId is not a task id")
	}
	raw, ok := m["message"]
	if !ok {
		return a, nil, badArgs("message is required")
	}
	a.Message = raw
	parts, err := ParseMessage(raw)
	return a, parts, err
}

// partKeys are a part's members (mcp-tools.json#/$defs/part).
var partKeys = []string{"text", "raw", "url", "data", "filename", "mediaType", "metadata"}

// ParseMessage builds the A2A parts of a message argument (mcp-tools.json#/$defs/message): a non-empty string is
// one text part; {parts: [Part, …]} is those parts, each with exactly one of text, raw (standard base64), url and
// data, and optional filename, mediaType and metadata (an object), read with exact keys. Anything else is an
// invalid-arguments error.
func ParseMessage(raw json.RawMessage) (a2a.ContentParts, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, badArgs("message is required")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			return nil, badArgs("message must be a non-empty string or {parts: [...]}")
		}
		return a2a.ContentParts{a2a.NewTextPart(s)}, nil
	}
	m, err := strictjson.Members(raw)
	if err != nil || len(m) != 1 {
		return nil, badArgs("message must be a non-empty string or {parts: [...]}")
	}
	list, ok := m["parts"]
	if !ok {
		return nil, badArgs("message must be a non-empty string or {parts: [...]}")
	}
	var elems []json.RawMessage
	if bytes.TrimSpace(list)[0] != '[' || json.Unmarshal(list, &elems) != nil || len(elems) == 0 {
		return nil, badArgs("message.parts must be a non-empty array")
	}
	parts := make(a2a.ContentParts, 0, len(elems))
	for i, e := range elems {
		p, err := parsePart(e)
		if err != nil {
			return nil, badArgs("message.parts[%d]: %s", i, err.Error())
		}
		parts = append(parts, p)
	}
	return parts, nil
}

func parsePart(raw json.RawMessage) (*a2a.Part, error) {
	m, err := strictjson.Members(raw)
	if err != nil {
		return nil, errors.New("not an object with distinct keys")
	}
	for k := range m {
		known := false
		for _, a := range partKeys {
			known = known || k == a
		}
		if !known {
			return nil, fmt.Errorf("unknown member %s", strictjson.Quote(k))
		}
	}
	p := &a2a.Part{}
	n := 0
	str := func(key string) (string, bool, error) {
		v, ok := m[key]
		if !ok {
			return "", false, nil
		}
		var s string
		if bytes.TrimSpace(v)[0] != '"' || json.Unmarshal(v, &s) != nil {
			return "", true, fmt.Errorf("%s must be a string", key)
		}
		return s, true, nil
	}
	if s, ok, err := str("text"); err != nil {
		return nil, err
	} else if ok {
		p.Content, n = a2a.Text(s), n+1
	}
	if s, ok, err := str("raw"); err != nil {
		return nil, err
	} else if ok {
		b, err := base64.StdEncoding.Strict().DecodeString(s)
		if err != nil {
			return nil, errors.New("raw must be standard base64")
		}
		p.Content, n = a2a.Raw(b), n+1
	}
	if s, ok, err := str("url"); err != nil {
		return nil, err
	} else if ok {
		if s == "" {
			return nil, errors.New("url must be a URI")
		}
		p.Content, n = a2a.URL(s), n+1
	}
	if v, ok := m["data"]; ok {
		var d any
		if err := json.Unmarshal(v, &d); err != nil {
			return nil, errors.New("data must be JSON")
		}
		p.Content, n = a2a.Data{Value: d}, n+1
	}
	if n != 1 {
		return nil, errors.New("exactly one of text, raw, url and data")
	}
	if p.Filename, _, err = str("filename"); err != nil {
		return nil, err
	}
	if p.MediaType, _, err = str("mediaType"); err != nil {
		return nil, err
	}
	if v, ok := m["metadata"]; ok {
		if bytes.TrimSpace(v)[0] != '{' || json.Unmarshal(v, &p.Metadata) != nil {
			return nil, errors.New("metadata must be an object")
		}
	}
	return p, nil
}
