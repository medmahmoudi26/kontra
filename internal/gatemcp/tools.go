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
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
)

// The tools (spec §12.2): search, describe, hire and reply, and the tools that follow, list, cancel and continue a
// hire task for a client that sees only tools (D332..D339). Their input schemas are spec/schema/mcp-tools.json's
// $defs; every tool declares annotations.readOnlyHint and annotations.destructiveHint.
const (
	ToolSearch     = "search"
	ToolDescribe   = "describe"
	ToolHire       = "hire"
	ToolReply      = "reply"
	ToolStatus     = "status"
	ToolTasks      = "tasks"
	ToolCancel     = "cancel"
	ToolArtifact   = "artifact"
	ToolPushSet    = "push_set"
	ToolPushList   = "push_list"
	ToolPushDelete = "push_delete"
)

// Ref names a service in search results and in describe's and hire's arguments: <name> on a single-tenant gate,
// <tenant>/<name> on a multi-tenant one (§12.2, D271).
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
	Price  string `json:"price,omitempty"` // equality on the price_model annotation; "free": none (D271)
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
// tools/list (MCP; ToolsCached false and no Tools when the cache is cold, D271), and how to call the service.
type DescribeResult struct {
	Ref         string            `json:"ref"`
	Kind        string            `json:"kind"`
	Card        json.RawMessage   `json:"card,omitempty"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	ToolsCached *bool             `json:"toolsCached,omitempty"`
	Sealed      bool              `json:"sealed"`
	Available   bool              `json:"available"`
	URLs        []string          `json:"urls"` // {public_url}/agents/… or {public_url}/mcp/…
	// Capabilities is what the service can do, normalized (D339): from the served card for A2A, the tool count for MCP.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities is describe's normalized summary of a service (D339). For A2A, from the served card: streaming and
// pushNotifications (capabilities), the default input and output modes, the skills, the extensions with whether each is
// required, and inputSchema, params.schema of the card's https://kontra.sh/ext/input-schema/v1 entry (the hire message
// then carries {parts: [{data: {...}}]}, spec/ext-input-schema.md). For MCP, the number of tools describe shows and
// whether the list was cached.
type Capabilities struct {
	Streaming         *bool             `json:"streaming,omitempty"`
	PushNotifications *bool             `json:"pushNotifications,omitempty"`
	InputModes        []string          `json:"inputModes,omitempty"`
	OutputModes       []string          `json:"outputModes,omitempty"`
	Skills            []SkillSummary    `json:"skills,omitempty"`
	Extensions        []ExtensionInCard `json:"extensions,omitempty"`
	InputSchema       json.RawMessage   `json:"inputSchema,omitempty"`
	Tools             *int              `json:"tools,omitempty"`
	ToolsCached       *bool             `json:"toolsCached,omitempty"`
}

// SkillSummary is one skill of a served card as describe's capabilities show it.
type SkillSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// ExtensionInCard is one extension of a served card: its URI and whether a request must activate it (§2.4).
type ExtensionInCard struct {
	URI      string `json:"uri"`
	Required bool   `json:"required"`
}

// InputSchemaURI is the input schema extension (spec/ext-input-schema.md).
const InputSchemaURI = "https://kontra.sh/ext/input-schema/v1"

// cardCapabilities reads a served A2A card's capabilities (D339); nil when the card is none or not an object.
func cardCapabilities(card json.RawMessage) *Capabilities {
	if len(card) == 0 {
		return nil
	}
	var c struct {
		Capabilities struct {
			Streaming         *bool `json:"streaming"`
			PushNotifications *bool `json:"pushNotifications"`
			Extensions        []struct {
				URI      string `json:"uri"`
				Required bool   `json:"required"`
				Params   struct {
					Schema json.RawMessage `json:"schema"`
				} `json:"params"`
			} `json:"extensions"`
		} `json:"capabilities"`
		DefaultInputModes  []string       `json:"defaultInputModes"`
		DefaultOutputModes []string       `json:"defaultOutputModes"`
		Skills             []SkillSummary `json:"skills"`
	}
	if json.Unmarshal(card, &c) != nil {
		return nil
	}
	out := &Capabilities{Streaming: c.Capabilities.Streaming, PushNotifications: c.Capabilities.PushNotifications,
		InputModes: c.DefaultInputModes, OutputModes: c.DefaultOutputModes, Skills: c.Skills}
	for _, x := range c.Capabilities.Extensions {
		out.Extensions = append(out.Extensions, ExtensionInCard{URI: x.URI, Required: x.Required})
		if x.URI == InputSchemaURI && firstByte(x.Params.Schema) == '{' && out.InputSchema == nil {
			out.InputSchema = x.Params.Schema
		}
	}
	return out
}

// HireArgs are hire's arguments: Message is a string or {parts: [Part]} (mcp-tools.json#/$defs/message).
type HireArgs struct {
	Service   string          `json:"service"`
	Message   json.RawMessage `json:"message"`
	ContextID string          `json:"contextId,omitempty"`
	// IdempotencyKey, 1..128 characters of [A-Za-z0-9._:-]: a hire with the same key, of the same buyer and principal,
	// within 24 h answers the task the first one created (§12.3 Idempotency, D338).
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// ReplyArgs are reply's arguments.
type ReplyArgs struct {
	TaskID  uuid.UUID       `json:"taskId"`
	Message json.RawMessage `json:"message"`
}

// HireResult is the structuredContent of every CallToolResult a hire or a reply answers, and of a hire task's
// tasks/result (and a modern tasks/get's result): what a person needs, since a client may show its model and its user
// nothing else (§12.2; D289, D324). taskId and state (the A2A state) always; url, the task's thread on the operator's
// page (its approve URL, kontra.approve.url, exactly as the URL elicitation carries it), whenever the task has one;
// quote, the quote's terms, while the task is quoted; text, the text the content's text blocks carry (the agent's
// answer, its question, or what the gate says of the task), each of its parts bounded as the content's is (§10.4);
// data, the values of the rendered data parts in order, message first, then each artifact's; files, the file parts'
// links. The hire and reply tools declare it as their outputSchema (schemaHireResult).
type HireResult struct {
	TaskID string `json:"taskId"`
	State  string `json:"state"`
	// ContextID is the task's A2A context: a hire with it continues the conversation as a new task (§4, §7, D336).
	ContextID string `json:"contextId,omitempty"`
	// Service is the task's service ref, what a hire that continues the conversation names (D336).
	Service string      `json:"service,omitempty"`
	URL     string      `json:"url,omitempty"`
	Quote   *QuoteTerms `json:"quote,omitempty"`
	Text    string      `json:"text,omitempty"`
	Data    []any       `json:"data,omitempty"`
	Files   []FileRef   `json:"files,omitempty"`
}

// QuoteTerms are a quoted task's terms as a HireResult shows them: the approve URL and the quote's expiry (RFC 3339,
// UTC), hooks.json's QuoteTerms.
type QuoteTerms struct {
	ApproveURL string `json:"approveUrl"`
	ExpiresAt  string `json:"expiresAt"`
}

// FileRef is a rendered file part as a HireResult names it: the URI of its content block (an embedded resource's
// kontra:task/… name, its bytes in the content; a link's URL), its file name and media type when it has them.
type FileRef struct {
	URI       string `json:"uri"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}

// The tools' input schemas: spec/schema/mcp-tools.json's $defs, each made self-contained (the $refs to message and
// part inlined), so a client that does not resolve $ref reads the whole schema. TestToolsListSchemas holds them equal
// to the file under JCS.
const (
	schemaPart = `{"type":"object","description":"An A2A 1.0 Part: exactly one of text, raw (base64), url, data; plus optional filename, mediaType, metadata.",` +
		`"oneOf":[{"required":["text"]},{"required":["raw"]},{"required":["url"]},{"required":["data"]}],` +
		`"properties":{"text":{"type":"string"},"raw":{"type":"string","contentEncoding":"base64"},"url":{"type":"string","format":"uri"},"data":{"not":{"type":"null"}},` +
		`"filename":{"type":"string"},"mediaType":{"type":"string"},"metadata":{"type":"object"}}}`
	schemaMessage = `{"oneOf":[{"type":"string","minLength":1},{"type":"object","required":["parts"],"additionalProperties":false,` +
		`"properties":{"parts":{"type":"array","minItems":1,"items":` + schemaPart + `}}}]}`
	schemaSearch = `{"type":"object","additionalProperties":false,"properties":{"query":{"type":"string","maxLength":256},` +
		`"kind":{"enum":["a2a","mcp"]},"price":{"type":"string","maxLength":32},"cursor":{"type":"string"}}}`
	schemaDescribe = `{"type":"object","required":["service"],"additionalProperties":false,` +
		`"properties":{"service":{"type":"string","pattern":"^([a-z0-9-]+/)?[a-z0-9-]+$"}}}`
	schemaHire = `{"type":"object","required":["service","message"],"additionalProperties":false,` +
		`"properties":{"service":{"type":"string","pattern":"^([a-z0-9-]+/)?[a-z0-9-]+$"},"message":` + schemaMessage +
		`,"contextId":{"type":"string","maxLength":256},` +
		`"idempotencyKey":{"type":"string","minLength":1,"maxLength":128,"pattern":"^[A-Za-z0-9._:-]+$"}}}`
	schemaReply = `{"type":"object","required":["taskId","message"],"additionalProperties":false,` +
		`"properties":{"taskId":{"type":"string","format":"uuid"},"message":` + schemaMessage + `}}`
	schemaTaskID = `{"type":"string","format":"uuid"}`
	schemaStatus = `{"type":"object","required":["taskId"],"additionalProperties":false,"properties":{"taskId":` + schemaTaskID +
		`,"after":{"type":"integer","minimum":0},"wait":{"type":"integer","minimum":0,"maximum":25}}}`
	schemaTasks = `{"type":"object","additionalProperties":false,"properties":{` +
		`"service":{"type":"string","pattern":"^([a-z0-9-]+/)?[a-z0-9-]+$"},"contextId":{"type":"string","maxLength":256},` +
		`"state":{"enum":["TASK_STATE_SUBMITTED","TASK_STATE_WORKING","TASK_STATE_INPUT_REQUIRED","TASK_STATE_AUTH_REQUIRED",` +
		`"TASK_STATE_COMPLETED","TASK_STATE_FAILED","TASK_STATE_CANCELED","TASK_STATE_REJECTED","active","ended"]},` +
		`"cursor":{"type":"string"}}}`
	schemaCancel   = `{"type":"object","required":["taskId"],"additionalProperties":false,"properties":{"taskId":` + schemaTaskID + `}}`
	schemaArtifact = `{"type":"object","required":["taskId","artifactId"],"additionalProperties":false,"properties":{"taskId":` +
		schemaTaskID + `,"artifactId":{"type":"string","minLength":1,"maxLength":256}}}`
	schemaPushSet = `{"type":"object","required":["taskId","url"],"additionalProperties":false,"properties":{"taskId":` +
		schemaTaskID + `,"url":{"type":"string","format":"uri"},"token":{"type":"string"},` +
		`"authentication":{"type":"object","required":["scheme"],"additionalProperties":false,` +
		`"properties":{"scheme":{"type":"string","minLength":1},"credentials":{"type":"string"}}}}}`
	schemaPushList   = schemaCancel
	schemaPushDelete = `{"type":"object","required":["taskId","configId"],"additionalProperties":false,"properties":{"taskId":` +
		schemaTaskID + `,"configId":{"type":"string","minLength":1}}}`
)

// schemaHireResult is the outputSchema of hire and reply: mcp-tools.json#/$defs/hireResult, HireResult's shape, strict
// (a stock client validates structuredContent against it). It uses only keywords JSON Schema draft-07 and 2020-12
// read alike and names no $schema, since a client's validator may default to either (D324).
const schemaHireResult = `{"type":"object","description":"A hire task as hire, reply, status and cancel answer it: taskId and ` +
	`state always; contextId and service, which a hire that continues the conversation names; ` +
	`url, the task's page on the operator's site, whenever the task has one; quote while a person must approve it there; ` +
	`text, the agent's answer or question or what the gate says of the task; data and files from the agent's parts.",` +
	`"required":["taskId","state"],"additionalProperties":false,"properties":{` +
	`"taskId":{"type":"string","format":"uuid"},` +
	`"state":` + schemaState + `,` +
	`"contextId":{"type":"string"},"service":{"type":"string"},` +
	`"url":{"type":"string","pattern":"^https://"},` +
	`"quote":{"type":"object","required":["approveUrl","expiresAt"],"additionalProperties":false,` +
	`"properties":{"approveUrl":{"type":"string","pattern":"^https://"},"expiresAt":{"type":"string","format":"date-time"}}},` +
	`"text":{"type":"string"},` +
	`"data":{"type":"array","minItems":1},` +
	`"files":` + schemaFiles + `}}`

// schemaState is an A2A task state (mcp-tools.json#/$defs/state) and schemaFiles a result's file links
// (mcp-tools.json#/$defs/files).
const (
	schemaState = `{"enum":["TASK_STATE_SUBMITTED","TASK_STATE_WORKING","TASK_STATE_INPUT_REQUIRED","TASK_STATE_AUTH_REQUIRED",` +
		`"TASK_STATE_COMPLETED","TASK_STATE_FAILED","TASK_STATE_CANCELED","TASK_STATE_REJECTED"]}`
	schemaFiles = `{"type":"array","minItems":1,"items":{"type":"object","required":["uri"],"additionalProperties":false,` +
		`"properties":{"uri":{"type":"string"},"name":{"type":"string"},"mediaType":{"type":"string"}}}}`
)

// The output schemas of the tools that follow a hire task (D332..D337), strict, as hireResult is (D324).
const (
	// schemaEvent is one row of a task's log as status renders it (mcp-tools.json#/$defs/event).
	schemaEvent = `{"type":"object","required":["seq","kind","at"],"additionalProperties":false,"properties":{` +
		`"seq":{"type":"integer","minimum":1},"kind":{"enum":["task","status","artifact","message"]},"state":` + schemaState + `,` +
		`"role":{"enum":["user","agent"]},"text":{"type":"string"},"data":{"type":"array","minItems":1},"files":` + schemaFiles + `,` +
		`"artifactId":{"type":"string"},"name":{"type":"string"},"at":{"type":"string","format":"date-time"}}}`
	// schemaStatusResult is status's structuredContent: the hire result, and the log after the cursor asked
	// (mcp-tools.json#/$defs/statusResult).
	schemaStatusResult = `{"type":"object","description":"A hire task as status answers it: the hire result, and when after was ` +
		`given the task's events after it (more when the answer was cut) and the cursor to ask from next.",` +
		`"required":["taskId","state","cursor"],"additionalProperties":false,"properties":{` +
		`"taskId":{"type":"string","format":"uuid"},"state":` + schemaState + `,"contextId":{"type":"string"},"service":{"type":"string"},` +
		`"url":{"type":"string","pattern":"^https://"},` +
		`"quote":{"type":"object","required":["approveUrl","expiresAt"],"additionalProperties":false,` +
		`"properties":{"approveUrl":{"type":"string","pattern":"^https://"},"expiresAt":{"type":"string","format":"date-time"}}},` +
		`"text":{"type":"string"},"data":{"type":"array","minItems":1},"files":` + schemaFiles + `,` +
		`"events":{"type":"array","items":` + schemaEvent + `},"cursor":{"type":"integer","minimum":0},"more":{"type":"boolean"}}}`
	// schemaTasksResult is tasks's structuredContent (mcp-tools.json#/$defs/tasksResult).
	schemaTasksResult = `{"type":"object","required":["tasks"],"additionalProperties":false,"properties":{` +
		`"tasks":{"type":"array","items":{"type":"object","required":["taskId","service","contextId","state","statusAt"],` +
		`"additionalProperties":false,"properties":{"taskId":{"type":"string","format":"uuid"},"service":{"type":"string"},` +
		`"contextId":{"type":"string"},"state":` + schemaState + `,"statusAt":{"type":"string","format":"date-time"},` +
		`"text":{"type":"string","maxLength":200}}}},"nextCursor":{"type":"string"}}}`
	// schemaArtifactResult is artifact's structuredContent (mcp-tools.json#/$defs/artifactResult).
	schemaArtifactResult = `{"type":"object","required":["taskId","artifactId"],"additionalProperties":false,"properties":{` +
		`"taskId":{"type":"string","format":"uuid"},"artifactId":{"type":"string"},"name":{"type":"string"},` +
		`"description":{"type":"string"},"text":{"type":"string"},"data":{"type":"array","minItems":1},"files":` + schemaFiles + `}}`
	// schemaPushConfig is one push config as the push tools show it, its secrets left out
	// (mcp-tools.json#/$defs/pushConfig).
	schemaPushConfig = `{"type":"object","required":["configId","url","token"],"additionalProperties":false,"properties":{` +
		`"configId":{"type":"string"},"url":{"type":"string"},"token":{"type":"boolean"},"scheme":{"type":"string"}}}`
	// schemaPushResult is push_set's, push_list's and push_delete's structuredContent (mcp-tools.json#/$defs/pushResult).
	schemaPushResult = `{"type":"object","required":["taskId","configs"],"additionalProperties":false,"properties":{` +
		`"taskId":{"type":"string","format":"uuid"},"configs":{"type":"array","items":` + schemaPushConfig + `}}}`
)

// tool is one entry of tools/list.
type tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  toolAnnotations `json:"annotations"`
	Execution    *toolExecution  `json:"execution,omitempty"`
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

// tools are the tools, in the order tools/list gives them. Their descriptions are short and tell a model the loop
// (D339): search and describe, hire (idempotencyKey for retries), status with after and wait to follow, reply when it
// asks, cancel, tasks to find old ones, artifact for one artifact.
var tools = []tool{
	{Name: ToolSearch, Title: "Search services",
		Description: "Find the agents and MCP servers this gate serves that you may call. Returns refs for describe and hire.",
		InputSchema: json.RawMessage(schemaSearch),
		Annotations: toolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
	{Name: ToolDescribe, Title: "Describe a service",
		Description: "Show a service's capabilities (skills, input and output modes, its input schema when it has one) and agent card (A2A) " +
			"or its tools (MCP), whether it is sealed and available, and the URLs to call it directly.",
		InputSchema: json.RawMessage(schemaDescribe),
		Annotations: toolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
	{Name: ToolHire, Title: "Hire an agent",
		Description: "Send a message to an A2A agent as a new task and answer its result, or its state after 25 s. " +
			"Pass idempotencyKey to retry safely: the same key answers the same task. To continue a conversation, hire again " +
			"with the contextId and service of an earlier result. Follow a running task with status. A priced task asks a " +
			"person to approve its quote first, at the URL given.",
		InputSchema: json.RawMessage(schemaHire), OutputSchema: json.RawMessage(schemaHireResult),
		Annotations: toolAnnotations{OpenWorldHint: true}},
	{Name: ToolReply, Title: "Reply to a task",
		Description: "Answer the question a hired task asks (state input required), and answer its new state. " +
			"After the task ended, continue the conversation with hire and its contextId instead.",
		InputSchema: json.RawMessage(schemaReply), OutputSchema: json.RawMessage(schemaHireResult),
		Annotations: toolAnnotations{OpenWorldHint: true}},
	{Name: ToolStatus, Title: "Follow a task",
		Description: "A hired task's state and result. With after (0 for the whole history) it returns the task's events " +
			"after that cursor and the result in short (read an artifact in full with artifact); with wait (seconds, up to 25) " +
			"it first waits for something new. Loop status {taskId, after: cursor, wait: 25} to follow a task until it ends " +
			"or asks a question.",
		InputSchema: json.RawMessage(schemaStatus), OutputSchema: json.RawMessage(schemaStatusResult),
		Annotations: toolAnnotations{ReadOnlyHint: true}},
	{Name: ToolTasks, Title: "List your tasks",
		Description: "List the tasks you hired, newest first, optionally of one service, one contextId, or a state " +
			"(an A2A state, active or ended). Pass nextCursor back as cursor for the next page.",
		InputSchema: json.RawMessage(schemaTasks), OutputSchema: json.RawMessage(schemaTasksResult),
		Annotations: toolAnnotations{ReadOnlyHint: true}},
	{Name: ToolCancel, Title: "Cancel a task",
		Description: "Cancel a hired task that has not ended, and answer it once canceled.",
		InputSchema: json.RawMessage(schemaCancel), OutputSchema: json.RawMessage(schemaHireResult),
		Annotations: toolAnnotations{DestructiveHint: true, IdempotentHint: true}},
	{Name: ToolArtifact, Title: "Read an artifact",
		Description: "One artifact of a hired task, in full: its text, data and files (fresh links). Artifact ids are in status events.",
		InputSchema: json.RawMessage(schemaArtifact), OutputSchema: json.RawMessage(schemaArtifactResult),
		Annotations: toolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
	{Name: ToolPushSet, Title: "Set a push webhook",
		Description: "Have the gate POST a hired task's updates to an https URL you own (A2A push notifications), with an optional token.",
		InputSchema: json.RawMessage(schemaPushSet), OutputSchema: json.RawMessage(schemaPushResult),
		Annotations: toolAnnotations{IdempotentHint: true, OpenWorldHint: true}},
	{Name: ToolPushList, Title: "List push webhooks",
		Description: "The push webhooks set on a hired task.",
		InputSchema: json.RawMessage(schemaPushList), OutputSchema: json.RawMessage(schemaPushResult),
		Annotations: toolAnnotations{ReadOnlyHint: true, IdempotentHint: true}},
	{Name: ToolPushDelete, Title: "Delete a push webhook",
		Description: "Stop and remove one push webhook of a hired task.",
		InputSchema: json.RawMessage(schemaPushDelete), OutputSchema: json.RawMessage(schemaPushResult),
		Annotations: toolAnnotations{DestructiveHint: true, IdempotentHint: true}},
}

// ToolsList is the tools/list result for a request of era: the tools with their input schemas; hire
// declares execution.taskSupport "optional" in the legacy list of a 2025-11-25 session only (§12.5). The modern list
// (2026-07-28 removed Tool.execution: the server decides, D159) is one page with resultType "complete", ttlMs
// ModernListTTL and cacheScope "private" (§12.5; 2026-07-28 CacheableResult, as §11.12 writes a relayed list).
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
	if era == mcpwire.EraModern {
		return json.Marshal(map[string]any{"tools": list, "resultType": resultComplete,
			"ttlMs": ModernListTTL.Milliseconds(), "cacheScope": "private"})
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
	m, err := members(args, "service", "message", "contextId", "idempotencyKey")
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
	var keyed bool
	if a.IdempotencyKey, keyed, err = stringArg(m, "idempotencyKey", 128); err != nil {
		return a, nil, err
	}
	if keyed && !idempotencyKeyRE.MatchString(a.IdempotencyKey) {
		return a, nil, badArgs("idempotencyKey must be 1 to 128 characters of A-Z, a-z, 0-9, '.', '_', ':' and '-'")
	}
	raw, ok := m["message"]
	if !ok {
		return a, nil, badArgs("message is required")
	}
	a.Message = raw
	parts, err := ParseMessage(raw)
	return a, parts, err
}

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// taskIDArg reads the required taskId argument: a task id as the gate writes it (a lowercase UUID).
func taskIDArg(m map[string]json.RawMessage) (uuid.UUID, error) {
	s, ok, err := stringArg(m, "taskId", 0)
	switch {
	case err != nil:
		return uuid.Nil, err
	case !ok:
		return uuid.Nil, badArgs("taskId is required")
	}
	id, err := uuid.Parse(s)
	if err != nil || len(s) != 36 {
		return uuid.Nil, badArgs("taskId is not a task id")
	}
	return id, nil
}

// intArg reads an optional integer argument within [lo, hi] (hi < 0: unbounded); a JSON number with a fraction or an
// exponent is not one.
func intArg(m map[string]json.RawMessage, key string, lo, hi int64) (int64, bool, error) {
	raw, ok := m[key]
	if !ok {
		return 0, false, nil
	}
	t := bytes.TrimSpace(raw)
	var n int64
	if len(t) == 0 || bytes.ContainsAny(t, ".eE") || json.Unmarshal(t, &n) != nil {
		return 0, true, badArgs("%s must be an integer", key)
	}
	if n < lo || (hi >= 0 && n > hi) {
		if hi >= 0 {
			return 0, true, badArgs("%s must be between %d and %d", key, lo, hi)
		}
		return 0, true, badArgs("%s must be at least %d", key, lo)
	}
	return n, true, nil
}

// StatusArgs are status's arguments: After is -1 when absent.
type StatusArgs struct {
	TaskID uuid.UUID
	After  int64
	Wait   time.Duration
}

func parseStatusArgs(args json.RawMessage) (StatusArgs, error) {
	m, err := members(args, "taskId", "after", "wait")
	if err != nil {
		return StatusArgs{}, err
	}
	a := StatusArgs{After: -1}
	if a.TaskID, err = taskIDArg(m); err != nil {
		return a, err
	}
	if n, ok, err := intArg(m, "after", 0, -1); err != nil {
		return a, err
	} else if ok {
		a.After = n
	}
	w, _, err := intArg(m, "wait", 0, int64(PlainWait/time.Second))
	if err != nil {
		return a, err
	}
	a.Wait = time.Duration(w) * time.Second
	return a, nil
}

// TasksArgs are the tasks tool's arguments.
type TasksArgs struct {
	Service   string
	ContextID string
	State     string // an A2A state, "active", "ended", or "" (any)
	Cursor    string
}

func parseTasksArgs(args json.RawMessage) (TasksArgs, error) {
	m, err := members(args, "service", "contextId", "state", "cursor")
	if err != nil {
		return TasksArgs{}, err
	}
	var a TasksArgs
	if _, ok := m["service"]; ok {
		if a.Service, err = serviceArg(m); err != nil {
			return a, err
		}
	}
	if a.ContextID, _, err = stringArg(m, "contextId", 256); err != nil {
		return a, err
	}
	var has bool
	if a.State, has, err = stringArg(m, "state", 0); err != nil {
		return a, err
	}
	if has && statesOf(a.State) == nil {
		return a, badArgs("state must be an A2A task state (TASK_STATE_…), active or ended")
	}
	if a.Cursor, _, err = stringArg(m, "cursor", 0); err != nil {
		return a, err
	}
	return a, nil
}

// statesOf is the tasks rows' states the tasks tool's state argument selects: every state whose A2A state it is (a
// quoted task is input-required on the wire, §5.1), the states that have not ended (active) or have (ended); nil for
// anything else.
func statesOf(s string) []store.State {
	all := []store.State{store.StateQuoted, store.StateSubmitted, store.StateWorking, store.StateAuthRequired,
		store.StateInputRequired, store.StateCompleted, store.StateFailed, store.StateCanceled, store.StateRejected}
	var out []store.State
	for _, st := range all {
		switch {
		case s == "active" && !st.Terminal(), s == "ended" && st.Terminal(), string(st.A2A()) == s:
			out = append(out, st)
		}
	}
	return out
}

// ArtifactArgs are the artifact tool's arguments.
type ArtifactArgs struct {
	TaskID     uuid.UUID
	ArtifactID string
}

func parseArtifactArgs(args json.RawMessage) (ArtifactArgs, error) {
	m, err := members(args, "taskId", "artifactId")
	if err != nil {
		return ArtifactArgs{}, err
	}
	var a ArtifactArgs
	if a.TaskID, err = taskIDArg(m); err != nil {
		return a, err
	}
	var ok bool
	if a.ArtifactID, ok, err = stringArg(m, "artifactId", 256); err != nil {
		return a, err
	}
	if !ok || a.ArtifactID == "" {
		return a, badArgs("artifactId is required")
	}
	return a, nil
}

// parseTaskOnly reads the arguments of cancel and push_list: {taskId}.
func parseTaskOnly(args json.RawMessage) (uuid.UUID, error) {
	m, err := members(args, "taskId")
	if err != nil {
		return uuid.Nil, err
	}
	return taskIDArg(m)
}

// PushSetArgs are push_set's arguments, as the A2A push config they set (§10.1).
type PushSetArgs struct {
	TaskID uuid.UUID
	Config a2a.PushConfig
}

func parsePushSetArgs(args json.RawMessage) (PushSetArgs, error) {
	m, err := members(args, "taskId", "url", "token", "authentication")
	if err != nil {
		return PushSetArgs{}, err
	}
	var a PushSetArgs
	if a.TaskID, err = taskIDArg(m); err != nil {
		return a, err
	}
	u, ok, err := stringArg(m, "url", 0)
	switch {
	case err != nil:
		return a, err
	case !ok || u == "":
		return a, badArgs("url is required")
	}
	a.Config.URL = u
	if a.Config.Token, _, err = stringArg(m, "token", 0); err != nil {
		return a, err
	}
	if raw, ok := m["authentication"]; ok {
		am, err := members(raw, "scheme", "credentials")
		if err != nil || firstByte(raw) != '{' {
			return a, badArgs("authentication must be {scheme, credentials?}")
		}
		scheme, ok, err := stringArg(am, "scheme", 0)
		if err != nil || !ok || scheme == "" {
			return a, badArgs("authentication.scheme is required")
		}
		creds, _, err := stringArg(am, "credentials", 0)
		if err != nil {
			return a, badArgs("authentication.credentials must be a string")
		}
		a.Config.Auth = &a2a.PushAuthInfo{Scheme: scheme, Credentials: creds}
	}
	return a, nil
}

// parsePushDeleteArgs reads push_delete's arguments.
func parsePushDeleteArgs(args json.RawMessage) (uuid.UUID, string, error) {
	m, err := members(args, "taskId", "configId")
	if err != nil {
		return uuid.Nil, "", err
	}
	id, err := taskIDArg(m)
	if err != nil {
		return id, "", err
	}
	c, ok, err := stringArg(m, "configId", 0)
	switch {
	case err != nil:
		return id, "", err
	case !ok || c == "":
		return id, "", badArgs("configId is required")
	}
	return id, c, nil
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
// data (any JSON value but null), and optional filename, mediaType and metadata (an object), read with exact keys,
// and each one a2a-go reads back as it writes it. Anything else is an invalid-arguments error, before any task exists.
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
		// A null value is no content to a2a-go (it writes {"data":null} and cannot read it back): a task whose message
		// held one could never be loaded (mcp-tools.json's data excludes null).
		var d any
		if err := json.Unmarshal(v, &d); err != nil {
			return nil, errors.New("data must be JSON")
		}
		if d == nil {
			return nil, errors.New("data must not be null")
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
	if !roundTrips(p) {
		return nil, errors.New("not a part A2A can carry")
	}
	return p, nil
}

// roundTrips reports whether a2a-go reads p back from what it writes. The task the gate creates stores the message
// as a2a-go encodes it, and every later read of the task decodes it: a part that does not survive that would leave a
// task no reader can load (stuck, counted against an anonymous host's tasks cap, breaking the buyer's tasks/list).
// The A2A routes cannot take one (a2a-go's decoding refuses it); a tool argument is built here, so it is checked here.
func roundTrips(p *a2a.Part) bool {
	b, err := json.Marshal(p)
	if err != nil {
		return false
	}
	var back a2a.Part
	return json.Unmarshal(b, &back) == nil
}
