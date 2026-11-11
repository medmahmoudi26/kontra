package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
)

// Prompting (design §5): the console sends real A2A requests to the gate's own public listener, HTTP+JSON, as the one
// anonymous caller every client of the profile is, so the traffic passes the edge, the version and extension checks,
// Admission and the traffic record as any client's would. A task the console starts is recorded in console_tasks;
// the console replies to, cancels and continues only those (taskView.Actions).

// Extension URIs the console speaks (Kontra Cloud's console: gate/tasks/hire.py), activated when the card declares
// them.
const (
	ExtA2UI091          = "https://a2ui.org/a2a-extension/a2ui/v0.9.1"
	ExtA2UI09           = "https://a2ui.org/a2a-extension/a2ui/v0.9"
	ExtStructuredInput  = "https://github.com/a2aproject/experimental-ext-structured-input/tree/main/v1"
	ExtFormService      = "https://a2a-extensions.agentstack.beeai.dev/services/form/v1"
	ExtFormRequest      = "https://a2a-extensions.agentstack.beeai.dev/ui/form_request/v1"
	ExtAP2              = "https://github.com/google-agentic-commerce/ap2/v1"
	a2uiMIME            = "application/a2ui+json"
	a2uiMIMELegacy      = "application/json+a2ui"
	textMax             = 64 << 10
	dataMax             = 256 << 10
	bodyMax             = 512 << 10
	catalogsMax         = 8
	skillsMax, examples = 20, 3
)

// a2uiExtensions are A2UI's extension URIs the console draws, newest first.
var a2uiExtensions = []string{ExtA2UI091, ExtA2UI09}

// spoken are the extensions the console speaks for the person: a card that requires any other is not started from
// the console (start form: unsupported_extensions).
var spoken = map[string]bool{ExtA2UI091: true, ExtA2UI09: true, ExtStructuredInput: true, ExtFormService: true,
	ExtFormRequest: true, ExtAP2: true}

var catalogRE = regexp.MustCompile(`^[\x21-\x7e]{1,512}$`)

// ---------------------------------------------------------------- the card

type cardExt struct {
	URI      string          `json:"uri"`
	Required bool            `json:"required"`
	Params   json.RawMessage `json:"params"`
}

type card struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Capabilities struct {
		Extensions []cardExt `json:"extensions"`
	} `json:"capabilities"`
	DefaultInputModes []string `json:"defaultInputModes"`
	Skills            []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Examples    []any    `json:"examples"`
		InputModes  []string `json:"inputModes"`
	} `json:"skills"`
}

func (k *card) declares(uri string) bool {
	for _, e := range k.Capabilities.Extensions {
		if e.URI == uri {
			return true
		}
	}
	return false
}

// a2ui is the A2UI extensions the card declares that the console draws, newest first.
func (k *card) a2ui() []string {
	var out []string
	for _, u := range a2uiExtensions {
		if k.declares(u) {
			out = append(out, u)
		}
	}
	return out
}

func (k *card) acceptsJSON() bool {
	modes := append([]string{}, k.DefaultInputModes...)
	for _, s := range k.Skills {
		modes = append(modes, s.InputModes...)
	}
	for _, m := range modes {
		if strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0])) == "application/json" {
			return true
		}
	}
	return false
}

// initialForm is the card's AgentStack initial form (its form service entry's params.form_demands.initial_form), as
// the card has it: the frontend normalises it (web/static/js/agentstack.js normalizeForm).
func (k *card) initialForm() json.RawMessage {
	for _, e := range k.Capabilities.Extensions {
		if e.URI != ExtFormService || len(e.Params) == 0 {
			continue
		}
		var p struct {
			FormDemands struct {
				InitialForm json.RawMessage `json:"initial_form"`
			} `json:"form_demands"`
		}
		if json.Unmarshal(e.Params, &p) == nil && len(p.FormDemands.InitialForm) > 0 && p.FormDemands.InitialForm[0] == '{' {
			return p.FormDemands.InitialForm
		}
	}
	return nil
}

// served is a service the console may prompt: an enabled A2A service with a served card.
type served struct {
	name   string
	sealed bool
	card   card
	raw    []byte
}

func (c *Console) servedA2A(ctx context.Context, name string) (*served, error) {
	if !config.NameRE.MatchString(name) {
		return nil, errNoService
	}
	var (
		s    served
		kind string
		raw  []byte
	)
	err := c.d.DB.QueryRow(ctx, `select s.name, s.kind, s.sealed <> 'off', s.card::text from kontra.services s
		where s.name = $1 and s.disabled_at is null`, name).Scan(&s.name, &kind, &s.sealed, &raw)
	if isNoRows(err) {
		return nil, errNoService
	}
	if err != nil {
		return nil, err
	}
	if kind != "a2a" {
		return nil, fail(http.StatusConflict, "not_an_agent", "this service is an MCP server: call its tools from an MCP client")
	}
	if len(raw) == 0 {
		return nil, fail(http.StatusServiceUnavailable, "service_unavailable",
			"the gate has no card of this agent yet: is its node connected (kontra connect)?")
	}
	s.raw = raw
	if err := json.Unmarshal(raw, &s.card); err != nil {
		return nil, fail(http.StatusBadGateway, "invalid_card", "the agent's card does not parse")
	}
	return &s, nil
}

func clip(s string, n int) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) > n {
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return &s
}

// ---------------------------------------------------------------- GET /v1/services/{name}/form

type startSkill struct {
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	Examples    []string `json:"examples"`
}

func (c *Console) startForm(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	s, err := c.servedA2A(ctx, r.PathValue("name"))
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	skills := []startSkill{}
	for _, k := range s.card.Skills {
		if len(skills) == skillsMax {
			break
		}
		name := k.Name
		if name == "" {
			name = k.ID
		}
		if name == "" {
			continue
		}
		sk := startSkill{Name: *clip(name, 200), Description: clip(k.Description, 2000), Examples: []string{}}
		for _, e := range k.Examples {
			if t, ok := e.(string); ok && strings.TrimSpace(t) != "" && len(sk.Examples) < examples {
				sk.Examples = append(sk.Examples, *clip(t, 500))
			}
		}
		skills = append(skills, sk)
	}
	unsupported := []string{}
	for _, e := range s.card.Capabilities.Extensions {
		if e.Required && !spoken[e.URI] {
			unsupported = append(unsupported, e.URI)
		}
	}
	var a2ui *string
	if got := s.card.a2ui(); len(got) > 0 {
		a2ui = &got[0]
	}
	out := map[string]any{
		"service": s.name, "kind": "a2a", "name": clip(s.card.Name, 200), "description": clip(s.card.Description, 2000),
		"skills": skills, "accepts_json": s.card.acceptsJSON(), "sealed": s.sealed, "a2ui": a2ui, "ap2": s.card.declares(ExtAP2),
		"unsupported_extensions": unsupported, "input_schema": nil, "initial_form": nil, "initial_form_note": nil,
	}
	if f := s.card.initialForm(); f != nil {
		out["initial_form"] = f
	}
	c.answer(w, r, out, nil)
}

// ---------------------------------------------------------------- request bodies

// startBody is POST /v1/tasks's (and continue's) body: Kontra Cloud's HireRequest in caller/callee words.
type startBody struct {
	Service      string          `json:"service"`
	Text         *string         `json:"text"`
	Data         json.RawMessage `json:"data"`
	Form         *formValues     `json:"form"`
	A2UICatalogs []string        `json:"a2ui_catalogs"`
}

type formValues struct {
	Values json.RawMessage `json:"values"`
}

// messageBody is a reply's (POST /v1/tasks/{id}/messages): Kontra Cloud's TaskMessage.
type messageBody struct {
	Text            *string         `json:"text"`
	A2UI            json.RawMessage `json:"a2ui"`
	Data            json.RawMessage `json:"data"`
	StructuredInput *struct {
		ID *string `json:"id"`
	} `json:"structured_input"`
	A2UICatalogs  []string        `json:"a2ui_catalogs"`
	A2UIDataModel json.RawMessage `json:"a2ui_data_model"`
	FormResponse  *formValues     `json:"form_response"`
}

var errInvalid = func(msg string) error { return fail(http.StatusUnprocessableEntity, "invalid_request", msg) }

func decodeBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, bodyMax+1))
	if err != nil {
		return errInvalid("the body could not be read")
	}
	if len(b) > bodyMax {
		return fail(http.StatusRequestEntityTooLarge, "too_large", "the body is larger than 512 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errInvalid("the body is not the JSON this route takes: " + err.Error())
	}
	return nil
}

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

func checkText(t *string) error {
	if t == nil {
		return nil
	}
	if *t == "" || len(*t) > textMax || !utf8.ValidString(*t) {
		return errInvalid("text: 1 to 64 KiB of UTF-8")
	}
	return nil
}

func checkCatalogs(c []string) error {
	if c == nil {
		return nil
	}
	if len(c) < 1 || len(c) > catalogsMax {
		return errInvalid("a2ui_catalogs: 1 to 8 catalog ids")
	}
	for _, id := range c {
		if !catalogRE.MatchString(id) {
			return errInvalid("a2ui_catalogs: a catalog id is printable ASCII without a space")
		}
	}
	return nil
}

func capabilities(catalogs []string) map[string]any {
	return map[string]any{"v0.9": map[string]any{"supportedCatalogIds": catalogs}, "v0.9.1": map[string]any{"supportedCatalogIds": catalogs}}
}

// firstMessage builds a task's first message (or a continuation's, in contextID) and the extensions it activates.
func firstMessage(s *served, b *startBody, contextID string) (map[string]any, []string, error) {
	if err := checkText(b.Text); err != nil {
		return nil, nil, err
	}
	if err := checkCatalogs(b.A2UICatalogs); err != nil {
		return nil, nil, err
	}
	hasData := len(b.Data) > 0 && string(bytes.TrimSpace(b.Data)) != "null"
	if b.Text == nil && !hasData && b.Form == nil {
		return nil, nil, errInvalid("a task needs text, data or a form")
	}
	if hasData && b.Form != nil {
		return nil, nil, errInvalid("a task carries data or a form, not both")
	}
	if hasData && (!isObject(b.Data) || len(b.Data) > dataMax) {
		return nil, nil, errInvalid("data: a JSON object of at most 256 KiB")
	}
	parts := []any{}
	if hasData {
		parts = append(parts, map[string]any{"data": b.Data, "mediaType": "application/json"})
	}
	metadata := map[string]any{}
	text := b.Text
	if b.Form != nil {
		if !s.card.declares(ExtFormService) || s.card.initialForm() == nil {
			return nil, nil, fail(http.StatusUnprocessableEntity, "no_initial_form", "this agent declares no initial form: write your request")
		}
		if !isObject(b.Form.Values) {
			return nil, nil, errInvalid("form.values: an object")
		}
		metadata[ExtFormService] = map[string]any{"form_fulfillments": map[string]any{"initial_form": map[string]any{"values": b.Form.Values}}}
		if text == nil {
			t := "The form is filled in."
			text = &t
		}
	}
	if text != nil {
		parts = append(parts, map[string]any{"text": *text})
	}
	msg := map[string]any{"messageId": uuid.Must(uuid.NewV7()).String(), "role": "ROLE_USER", "parts": parts}
	if contextID != "" {
		msg["contextId"] = contextID
	}
	exts := []string{}
	if a := s.card.a2ui(); len(a) > 0 && b.A2UICatalogs != nil {
		metadata["a2uiClientCapabilities"] = capabilities(b.A2UICatalogs)
		exts = append(exts, a...)
	}
	for _, u := range []string{ExtStructuredInput, ExtFormService, ExtFormRequest, ExtAP2} {
		if s.card.declares(u) {
			exts = append(exts, u)
		}
	}
	if len(metadata) > 0 {
		msg["metadata"] = metadata
	}
	return msg, exts, nil
}

// replyMessage builds a reply to a task waiting for input.
func replyMessage(s *served, taskID string, b *messageBody) (map[string]any, []string, error) {
	if err := checkText(b.Text); err != nil {
		return nil, nil, err
	}
	if err := checkCatalogs(b.A2UICatalogs); err != nil {
		return nil, nil, err
	}
	present := func(raw json.RawMessage) bool { return len(raw) > 0 && string(bytes.TrimSpace(raw)) != "null" }
	answers := 0
	for _, x := range []bool{present(b.A2UI), present(b.Data), b.FormResponse != nil} {
		if x {
			answers++
		}
	}
	if b.Text == nil && answers == 0 {
		return nil, nil, errInvalid("a reply needs text, an A2UI action or a form's answer")
	}
	if answers > 1 {
		return nil, nil, errInvalid("a reply carries one of an A2UI action, data and a form_response, not two")
	}
	if b.StructuredInput != nil && !present(b.Data) {
		return nil, nil, errInvalid("structured_input goes with a form's answer (data)")
	}
	parts := []any{}
	if present(b.A2UI) {
		var ev struct {
			Version string          `json:"version"`
			Action  json.RawMessage `json:"action"`
		}
		if !isObject(b.A2UI) || json.Unmarshal(b.A2UI, &ev) != nil || (ev.Version != "v0.9" && ev.Version != "v0.9.1") || !isObject(ev.Action) ||
			len(b.A2UI) > dataMax {
			return nil, nil, errInvalid("a2ui: A2UI's {version: v0.9 | v0.9.1, action}")
		}
		mime := a2uiMIMELegacy
		if ev.Version == "v0.9.1" {
			mime = a2uiMIME
		}
		parts = append(parts, map[string]any{"data": b.A2UI, "mediaType": mime, "metadata": map[string]any{"mimeType": mime}})
	}
	if present(b.Data) {
		if !isObject(b.Data) || len(b.Data) > dataMax {
			return nil, nil, errInvalid("data: a JSON object of at most 256 KiB")
		}
		part := map[string]any{"data": b.Data}
		if b.StructuredInput != nil {
			tag := map[string]any{"type": "structured_input"}
			if b.StructuredInput.ID != nil {
				tag["id"] = *b.StructuredInput.ID
			}
			part["metadata"] = map[string]any{ExtStructuredInput: tag}
		}
		parts = append(parts, part)
	}
	text := b.Text
	metadata := map[string]any{}
	if b.FormResponse != nil {
		if !isObject(b.FormResponse.Values) {
			return nil, nil, errInvalid("form_response.values: an object")
		}
		metadata[ExtFormRequest] = map[string]any{"values": b.FormResponse.Values}
		if text == nil {
			t := "The form is filled in."
			text = &t
		}
	}
	if text != nil {
		parts = append(parts, map[string]any{"text": *text})
	}
	if b.A2UICatalogs != nil {
		metadata["a2uiClientCapabilities"] = capabilities(b.A2UICatalogs)
	}
	if present(b.A2UIDataModel) {
		if !isObject(b.A2UIDataModel) {
			return nil, nil, errInvalid("a2ui_data_model: an object")
		}
		metadata["a2uiClientDataModel"] = b.A2UIDataModel
	}
	msg := map[string]any{"messageId": uuid.Must(uuid.NewV7()).String(), "role": "ROLE_USER", "taskId": taskID, "parts": parts}
	if len(metadata) > 0 {
		msg["metadata"] = metadata
	}
	exts := []string{}
	if present(b.A2UI) || b.A2UICatalogs != nil {
		exts = append(exts, s.card.a2ui()...)
	}
	if b.StructuredInput != nil {
		exts = append(exts, ExtStructuredInput)
	}
	if b.FormResponse != nil {
		exts = append(exts, ExtFormRequest)
	}
	if s.card.declares(ExtAP2) {
		exts = append(exts, ExtAP2)
	}
	return msg, exts, nil
}

// ---------------------------------------------------------------- the gate's own A2A

// gateAnswer is what the gate answered: the task's id, its state, its context.
type gateAnswer struct {
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
	ContextID string `json:"context_id"`
	MessageID string `json:"message_id,omitempty"`
}

// wireState is an A2A 1.0 state (TASK_STATE_INPUT_REQUIRED) in the gate's words (input-required).
func wireState(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(s, "TASK_STATE_"), "_", "-"))
	if s == "" {
		return "submitted"
	}
	return s
}

// call sends one HTTP+JSON A2A request to the gate's own public listener as the anonymous caller.
func (c *Console) call(ctx context.Context, service, path string, body any, exts []string) (*http.Response, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	u := strings.TrimRight(c.d.Config.PublicURL, "/") + "/agents/" + url.PathEscape(service) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("User-Agent", "kontra-console")
	if len(exts) > 0 {
		req.Header.Set("A2A-Extensions", strings.Join(exts, ", "))
	}
	resp, err := c.d.HTTP.Do(req)
	if err != nil {
		return nil, nil, fail(http.StatusServiceUnavailable, "gate_unavailable", "the gate's own A2A route did not answer: "+err.Error())
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, fail(http.StatusBadGateway, "gate_unavailable", "the gate's answer broke off")
	}
	return resp, got, nil
}

// refusal turns the gate's A2A error answer (a google.rpc.Status) into the API's error.
func refusal(status int, body []byte, what string) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("the gate answered %d to %s", status, what)
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	reason := ""
	if len(e.Error.Details) > 0 {
		reason = e.Error.Details[0].Reason
	}
	switch {
	case reason == "TASK_NOT_CANCELABLE" || reason == "UNSUPPORTED_OPERATION" || status == http.StatusConflict:
		return fail(http.StatusConflict, "task_not_cancelable", msg)
	case status == http.StatusNotFound:
		return fail(http.StatusNotFound, "service_not_found", msg)
	case status == http.StatusTooManyRequests:
		return fail(http.StatusTooManyRequests, "rate_limited", msg)
	case status >= 500:
		return fail(http.StatusBadGateway, "gate_refused", msg)
	}
	return fail(http.StatusUnprocessableEntity, "gate_refused", msg)
}

// send sends a follow-up with message:send, returning at once (the task as the gate moved it): a follow-up names an
// exposed task (D362), which the answer is.
func (c *Console) send(ctx context.Context, service string, msg map[string]any, exts []string) (*gateAnswer, error) {
	resp, body, err := c.call(ctx, service, "/message:send",
		map[string]any{"message": msg, "configuration": map[string]any{"returnImmediately": true}}, exts)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, refusal(resp.StatusCode, body, "the message")
	}
	var got struct {
		Task *struct {
			ID        string `json:"id"`
			ContextID string `json:"contextId"`
			Status    struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
		Message *struct {
			TaskID    string `json:"taskId"`
			ContextID string `json:"contextId"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &got) != nil {
		return nil, fail(http.StatusBadGateway, "invalid_response", "the gate's answer is not JSON")
	}
	switch {
	case got.Task != nil && got.Task.ID != "":
		return &gateAnswer{TaskID: got.Task.ID, State: wireState(got.Task.Status.State), ContextID: got.Task.ContextID}, nil
	case got.Message != nil && got.Message.TaskID != "":
		return &gateAnswer{TaskID: got.Message.TaskID, State: "working", ContextID: got.Message.ContextID}, nil
	}
	return nil, fail(http.StatusBadGateway, "invalid_response", "the gate answered no task")
}

// streamStart sends a new message with message:stream, as the A2A Inspector does, so that an agent answering with a
// Message is answered to the console as it is to any client (D362), instead of message:send with returnImmediately,
// which always makes a task. The gate writes the stream's headers once it has admitted the message and recorded it,
// so the console reads the record the gate made (by the message's id, which it minted), remembers it, and leaves the
// stream: the interaction goes on without it (D8), and the console shows it from the log. The record is the gate's
// internal id: a task once the agent answers with one, a message-only interaction otherwise.
func (c *Console) streamStart(ctx context.Context, service string, msg map[string]any, exts []string) (*gateAnswer, error) {
	b, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel() // leaves the stream once the record is read
	u := strings.TrimRight(c.d.Config.PublicURL, "/") + "/agents/" + url.PathEscape(service) + "/message:stream"
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("User-Agent", "kontra-console")
	if len(exts) > 0 {
		req.Header.Set("A2A-Extensions", strings.Join(exts, ", "))
	}
	resp, err := c.d.HTTP.Do(req)
	if err != nil {
		return nil, fail(http.StatusServiceUnavailable, "gate_unavailable", "the gate's own A2A route did not answer: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, refusal(resp.StatusCode, got, "the message")
	}
	messageID, _ := msg["messageId"].(string)
	var (
		id        uuid.UUID
		contextID string
		state     string
	)
	err = c.d.DB.QueryRow(ctx, `select t.id, t.context_id, t.state from kontra.tasks t
		where t.tenant_id = (select id from kontra.tenants where slug = 'default') and t.service = $1 and t.first_message_id = $2
		order by t.created_at desc limit 1`, service, messageID).Scan(&id, &contextID, &state)
	if isNoRows(err) {
		return nil, fail(http.StatusBadGateway, "invalid_response", "the gate opened the stream but recorded no message")
	}
	if err != nil {
		return nil, err
	}
	return &gateAnswer{TaskID: id.String(), State: state, ContextID: contextID}, nil
}

// remember records a task the console started.
func (c *Console) remember(ctx context.Context, taskID string) error {
	id, err := uuid.Parse(taskID)
	if err != nil {
		return fail(http.StatusBadGateway, "invalid_response", "the gate's task id is not a task id")
	}
	_, err = c.d.DB.Exec(ctx, `insert into kontra.console_tasks (task_id) values ($1) on conflict do nothing`, id)
	return err
}

// ---------------------------------------------------------------- POST /v1/tasks

func (c *Console) startTask(w http.ResponseWriter, r *http.Request) {
	var b startBody
	if err := decodeBody(r, &b); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	got, err := c.start(r.Context(), &b, "")
	c.answer(w, r, got, err)
}

func (c *Console) start(ctx context.Context, b *startBody, contextID string) (*gateAnswer, error) {
	s, err := c.servedA2A(ctx, b.Service)
	if err != nil {
		return nil, err
	}
	if s.sealed {
		return nil, fail(http.StatusConflict, "sealed_service", "this service is sealed: the console does not start its tasks")
	}
	msg, exts, err := firstMessage(s, b, contextID)
	if err != nil {
		return nil, err
	}
	got, err := c.streamStart(ctx, s.name, msg, exts)
	if err != nil {
		return nil, err
	}
	if err := c.remember(ctx, got.TaskID); err != nil {
		return nil, err
	}
	got.MessageID = msg["messageId"].(string)
	return got, nil
}

// ---------------------------------------------------------------- the console's own tasks

// own reads a task the console may act on with action, or the refusal: 404 when unknown, 403 when another client's.
func (c *Console) own(ctx context.Context, r *http.Request, action string) (*taskView, *served, error) {
	id, err := taskIDOf(r)
	if err != nil {
		return nil, nil, err
	}
	v, err := c.loadTask(ctx, id, false)
	if err != nil {
		return nil, nil, err
	}
	if !v.Console {
		return nil, nil, fail(http.StatusForbidden, "not_console_task",
			"another client started this task: the console watches it and does not act on it")
	}
	ok := false
	for _, a := range v.Actions {
		ok = ok || a == action
	}
	if !ok {
		return nil, nil, fail(http.StatusConflict, "action_unavailable", fmt.Sprintf("the task is %s: it cannot be %s now", v.State,
			map[string]string{"reply": "answered", "cancel": "canceled", "continue": "continued"}[action]))
	}
	s, err := c.servedA2A(ctx, v.Service)
	if err != nil {
		return nil, nil, err
	}
	return v, s, nil
}

func (c *Console) taskMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b messageBody
	if err := decodeBody(r, &b); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	v, s, err := c.own(ctx, r, "reply")
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	msg, exts, err := replyMessage(s, v.TaskID, &b)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	got, err := c.send(ctx, s.name, msg, exts)
	if err == nil && got.TaskID != v.TaskID {
		err = fail(http.StatusBadGateway, "invalid_response", "the gate answered another task")
	}
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	c.answer(w, r, map[string]any{"task_id": v.TaskID, "message_id": msg["messageId"], "state": got.State}, nil)
}

func (c *Console) cancelTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b struct{}
	if err := decodeBody(r, &b); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	v, s, err := c.own(ctx, r, "cancel")
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	resp, body, err := c.call(ctx, s.name, "/tasks/"+url.PathEscape(v.TaskID)+":cancel", map[string]any{}, nil)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		c.answer(w, r, nil, refusal(resp.StatusCode, body, "the cancel"))
		return
	}
	var t struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
		Task *struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"task"`
	}
	_ = json.Unmarshal(body, &t)
	state := t.Status.State
	if t.Task != nil {
		state = t.Task.Status.State
	}
	c.answer(w, r, map[string]any{"task_id": v.TaskID, "state": wireState(state)}, nil)
}

func (c *Console) continueTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b startBody
	if err := decodeBody(r, &b); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	v, _, err := c.own(ctx, r, "continue")
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	if b.Service != "" && b.Service != v.Service {
		c.answer(w, r, nil, errInvalid("service: a continuation is on the task's own service"))
		return
	}
	b.Service = v.Service
	got, err := c.start(ctx, &b, v.ContextID)
	if errors.Is(err, nil) && got != nil {
		c.answer(w, r, got, nil)
		return
	}
	c.answer(w, r, nil, err)
}
