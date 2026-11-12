package console

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
)

// originEvent appends an event with its origin (nil: none, a row from before 0007) at a time, as the log does.
func (e *env) originEvent(id uuid.UUID, kind, payload string, origin *string, at time.Time) int64 {
	e.t.Helper()
	var seq int64
	err := e.pool.QueryRow(context.Background(), `with v as (update kontra.tasks set version = version + 1 where id = $1 returning version)
		insert into kontra.task_events (task_id, seq, kind, payload, origin, created_at) select $1, v.version, $2, $3::json, $4::json, $5
		from v returning seq`, id, kind, payload, origin, at).Scan(&seq)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

// exchange records an exchange row.
func (e *env) exchange(leg, method string, task *uuid.UUID, at time.Time, extra string, args ...any) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	cols, vals := "", ""
	if extra != "" {
		parts := strings.SplitN(extra, "|", 2)
		cols, vals = ", "+parts[0], ", "+parts[1]
	}
	taskCol := "task_id"
	if leg == "callee" {
		taskCol = "gate_task_id"
	}
	all := append([]any{id, at, leg, method, task}, args...)
	e.exec(`insert into kontra.a2a_exchanges (id, at, leg, service, method, binding, `+taskCol+`, bytes_in, bytes_out, duration_ms,
		http_status`+cols+`) values ($1, $2, $3, 'hello', $4, 'jsonrpc', $5, 1, 1, 1, 200`+vals+`)`, all...)
	return id
}

// 6.1: a task's timeline: its events with their origin, the A2A object the caller received and their validation,
// merged in time with the exchanges that carried no event; D5's conversion visible; a row without origin shows null.
func TestTaskTimeline(t *testing.T) {
	e := newEnv(t, nil)
	no := false
	t0 := time.Now()
	send := uuid.Must(uuid.NewV7())
	id := e.taskWith("submitted", "hi", "c1", false, &store.Origin{Actor: "caller", Type: "SendMessage", Method: "SendMessage",
		ExchangeID: send.String(), ContextAssigned: &no})
	e.exec(`insert into kontra.a2a_exchanges (id, at, leg, service, method, binding, task_id, bytes_in, bytes_out, duration_ms, http_status)
		values ($1, $2, 'caller', 'hello', 'SendMessage', 'jsonrpc', $3, 1, 1, 1, 200)`, send, t0, id)
	e.exchange("caller", "GetTask", &id, t0.Add(1*time.Second), "")
	ex := e.exchange("callee", "SendMessage", &id, t0.Add(2*time.Second), "callee_task_id|$6", "agent-7")
	origin := `{"actor":"callee","type":"Message","method":"SendMessage","exchange_id":"` + ex.String() + `","converted_to":"TaskStatusUpdateEvent"}`
	e.originEvent(id, "status", fmt.Sprintf(`{"statusUpdate":{"taskId":"%s","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"m2","role":"ROLE_AGENT","parts":[{"text":"done"}]}}}}`, id),
		&origin, t0.Add(3*time.Second))
	// A row from before 0007 (no origin), and not valid A2A (no contextId).
	e.originEvent(id, "status", fmt.Sprintf(`{"statusUpdate":{"taskId":"%s","status":{"state":"TASK_STATE_COMPLETED"}}}`, id), nil, t0.Add(4*time.Second))
	got := e.json("GET", "/v1/tasks/"+id.String()+"/timeline", "", 200)
	its := items(got)
	var labels []string
	for _, it := range its {
		switch it["type"] {
		case "event":
			o, _ := it["origin"].(map[string]any)
			label := fmt.Sprintf("event %v→%v", it["kind"], it["caller_type"])
			if o != nil {
				label += fmt.Sprintf(" %v·%v", o["actor"], o["type"])
			} else {
				label += " origin:null"
			}
			labels = append(labels, label)
		case "exchange":
			labels = append(labels, fmt.Sprintf("exchange %v·%v", it["leg"], it["method"]))
		}
	}
	// The caller's SendMessage carried the task's first event and the callee's SendMessage the status: each is named by
	// its event, not listed on its own; the caller's GetTask, which carried none, is.
	want := []string{"event task→Task caller·SendMessage", "exchange caller·GetTask",
		"event status→TaskStatusUpdateEvent callee·Message", "event status→TaskStatusUpdateEvent origin:null"}
	if fmt.Sprint(labels) != fmt.Sprint(want) {
		t.Fatalf("timeline\n got %v\nwant %v", labels, want)
	}
	conv := its[2]["origin"].(map[string]any)
	if conv["converted_to"] != "TaskStatusUpdateEvent" || conv["exchange_id"] != ex.String() {
		t.Errorf("D5 conversion %v", conv)
	}
	if v := its[2]["validation"].(map[string]any); v["valid"] != true {
		t.Errorf("valid event marked %v", v)
	}
	if v := its[3]["validation"].(map[string]any); v["valid"] != false || !strings.Contains(fmt.Sprint(v["issues"]), "contextId") {
		t.Errorf("an event without contextId: %v", v)
	}
	if op := got["opening"].(map[string]any); op["assigned"] != false || op["context_id"] != "c1" {
		t.Errorf("opening %v", op)
	}
}

// 6.1: the live stream carries each event's origin and caller-facing type too.
func TestEventsStreamCarriesOrigin(t *testing.T) {
	e := newEnv(t, nil)
	id := e.task("submitted", "hi", "c1", false)
	o := `{"actor":"callee","type":"TaskStatusUpdateEvent","method":"SendStreamingMessage"}`
	e.originEvent(id, "status", fmt.Sprintf(`{"statusUpdate":{"taskId":"%s","contextId":"c1","status":{"state":"TASK_STATE_WORKING"}}}`, id),
		&o, time.Now())
	resp := e.do("GET", "/v1/tasks/"+id.String()+"/events?after=1", "")
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	for i := 0; i < 5 && !strings.Contains(string(buf[:n]), "caller_type"); i++ {
		m, _ := resp.Body.Read(buf[n:])
		n += m
	}
	s := string(buf[:n])
	if !strings.Contains(s, `"caller_type":"TaskStatusUpdateEvent"`) || !strings.Contains(s, `"actor":"callee"`) || !strings.Contains(s, `"valid":true`) {
		t.Errorf("stream %s", s)
	}
}

// 6.2: contexts: the new paths and the old aliases, the opening (assigned by the gate, sent by the caller), the callee
// contexts of a context served by two sessions, and a timeline across its tasks.
func TestContexts(t *testing.T) {
	e := newEnv(t, nil)
	yes, no := true, false
	a := e.taskWith("completed", "first", "ctx-g", false, &store.Origin{Actor: "caller", Type: "SendMessage", ContextAssigned: &yes})
	b := e.task("completed", "second", "ctx-g", false)
	s1, s2 := uuid.New(), uuid.New()
	e.exec(`insert into kontra.attempts (task_id, n, backend, tunnel_session, key_id, replica_id, upstream_task_id, upstream_context_id, first_upstream_id_at)
		values ($1, 1, 'tunnel', $3, 'k1_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'r1', 'up-a', 'agent-ctx-1', now()),
		       ($2, 1, 'tunnel', $4, 'k1_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'r1', 'up-b', 'agent-ctx-2', now())`, a, b, s1, s2)
	for _, base := range []string{"/v1/contexts", "/v1/conversations"} {
		list := items(e.json("GET", base, "", 200))
		if len(list) != 1 || list[0]["context_id"] != "ctx-g" || list[0]["tasks"] != float64(2) {
			t.Fatalf("%s: %v", base, list)
		}
		one := e.json("GET", fmt.Sprintf("%s/hello/%s/ctx-g", base, e.buyer), "", 200)
		if len(one["tasks"].([]any)) != 2 {
			t.Errorf("%s one: %v", base, one)
		}
	}
	tl := e.json("GET", fmt.Sprintf("/v1/contexts/hello/%s/ctx-g/timeline", e.buyer), "", 200)
	if op := tl["opening"].(map[string]any); op["assigned"] != true || op["task_id"] != a.String() {
		t.Errorf("opening %v", op)
	}
	cc := tl["callee_contexts"].([]any)
	if len(cc) != 2 || cc[0].(map[string]any)["context_id"] != "agent-ctx-1" || cc[1].(map[string]any)["session"] != s2.String() {
		t.Errorf("callee contexts %v", cc)
	}
	tasks := map[string]bool{}
	for _, it := range items(tl) {
		tasks[it["task_id"].(string)] = true
	}
	if !tasks[a.String()] || !tasks[b.String()] {
		t.Errorf("timeline tasks %v", tasks)
	}
	// Sent by the caller: assigned false.
	e.taskWith("completed", "x", "mine", false, &store.Origin{Actor: "caller", Type: "SendMessage", ContextAssigned: &no})
	if op := e.json("GET", fmt.Sprintf("/v1/contexts/hello/%s/mine/timeline", e.buyer), "", 200)["opening"].(map[string]any); op["assigned"] != false {
		t.Errorf("caller's context: %v", op)
	}
	e.json("GET", fmt.Sprintf("/v1/contexts/hello/%s/nope/timeline", e.buyer), "", 404)
}

// 6.3: the exchanges list filters by leg and context; one exchange with its bodies (validated), its frames, or none.
func TestExchangeDetail(t *testing.T) {
	e := newEnv(t, nil)
	id := e.task("completed", "hi", "c1", false)
	req := `{"jsonrpc":"2.0","id":1,"method":"CreateTaskPushNotificationConfig","params":{"taskId":"t","url":"https://h","token":"[redacted]"}}`
	resp := `{"jsonrpc":"2.0","id":1,"result":{"taskId":"t","id":"p1","url":"https://h","token":"[redacted]"}}`
	with := e.exchange("caller", "CreateTaskPushNotificationConfig", &id, time.Now(), "a2a_version, context_id, request_body, request_size, response_body, response_size|$6, $7, $8, $9, $10, $11",
		"1.0", "c1", []byte(req), int64(len(req)), []byte(resp), int64(len(resp)))
	stream := e.exchange("callee", "SendStreamingMessage", &id, time.Now(), "a2a_version, request_body, request_size|$6, $7, $8", "1.0", []byte(`{}`), int64(2))
	e.exec(`insert into kontra.a2a_exchange_frames (exchange_id, n, offset_ms, sse_id, data, size) values
		($1, 0, 0, null, '{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}}}', 90),
		($1, 1, 12, '2', '{"jsonrpc":"2.0","id":1,"result":{"statusUpdate":{"taskId":"t","status":{"state":"TASK_STATE_COMPLETED"}}}}', 90)`, stream)
	without := e.exchange("caller", "GetTask", &id, time.Now(), "")

	d := e.json("GET", "/v1/exchanges/"+with.String(), "", 200)
	rq := d["request"].(map[string]any)
	if d["bodies_kept"] != true || !strings.Contains(rq["text"].(string), "[redacted]") || rq["validation"].(map[string]any)["valid"] != true {
		t.Errorf("with bodies: %v", d)
	}
	d = e.json("GET", "/v1/exchanges/"+stream.String(), "", 200)
	fr := d["frames"].([]any)
	if len(fr) != 2 || fr[1].(map[string]any)["sse_id"] != "2" || fr[1].(map[string]any)["offset_ms"] != float64(12) ||
		fr[0].(map[string]any)["validation"].(map[string]any)["valid"] != true ||
		fr[1].(map[string]any)["validation"].(map[string]any)["valid"] != false {
		t.Errorf("frames %v", fr)
	}
	d = e.json("GET", "/v1/exchanges/"+without.String(), "", 200)
	if d["bodies_kept"] != false || d["request"] != nil {
		t.Errorf("bodies off: %v", d)
	}
	e.json("GET", "/v1/exchanges/"+uuid.NewString(), "", 404)
	if l := items(e.json("GET", "/v1/exchanges?leg=callee", "", 200)); len(l) != 1 || l[0]["id"] != stream.String() || l[0]["bodies"] != true {
		t.Errorf("leg filter %v", l)
	}
	if l := items(e.json("GET", "/v1/exchanges?context=c1", "", 200)); len(l) != 1 || l[0]["id"] != with.String() {
		t.Errorf("context filter %v", l)
	}
	if l := items(e.json("GET", "/v1/exchanges?task="+id.String(), "", 200)); len(l) != 3 {
		t.Errorf("task filter (both legs) %v", l)
	}
	e.json("GET", "/v1/exchanges?leg=upstream", "", 422)
}

// 6.4: a service's two cards: a 0.3 callee card checked against 0.3, the served card against 1.0, and what the gate set.
func TestServiceCards(t *testing.T) {
	e := newEnv(t, nil)
	callee := `{"name":"Hello","description":"Says hello.","url":"http://127.0.0.1:9/","version":"1","protocolVersion":"0.3.0",
		"capabilities":{"streaming":false},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
	served := `{"name":"Hello","description":"Says hello.","version":"1","supportedInterfaces":[{"url":"http://gate/agents/hello","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
		"capabilities":{"streaming":true,"pushNotifications":true,"extendedAgentCard":false},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
	e.exec(`update kontra.services set upstream_card = $1, card = $2::json where name = 'hello'`, []byte(callee), served)
	got := e.json("GET", "/v1/services/hello/cards", "", 200)
	c, s := got["callee"].(map[string]any), got["served"].(map[string]any)
	if c["version"] != "0.3" || c["validation"].(map[string]any)["valid"] != true {
		t.Errorf("callee %v", c)
	}
	if s["version"] != "1.0" || s["validation"].(map[string]any)["valid"] != true {
		t.Errorf("served %v", s)
	}
	set := fmt.Sprint(got["gate_set"])
	for _, p := range []string{"$.capabilities.streaming", "$.capabilities.pushNotifications", "$.supportedInterfaces", "$.url", "$.protocolVersion"} {
		if p == "$.url" || p == "$.protocolVersion" {
			continue // dropped from the served card: not a member it set
		}
		if !strings.Contains(set, p) {
			t.Errorf("gate_set %s lacks %s", set, p)
		}
	}
	if strings.Contains(set, "$.name") {
		t.Errorf("gate_set %s names a member the gate kept", set)
	}
	e.json("GET", "/v1/services/tools/cards", "", 404)
	e.json("GET", "/v1/services/nope/cards", "", 404)
}

// 6.5: /v1/info names the new features and the body settings.
func TestInfoFeatures(t *testing.T) {
	e := newEnv(t, nil)
	got := e.json("GET", "/v1/info", "", 200)
	b, _ := json.Marshal(got["features"])
	for _, f := range []string{"contexts", "timeline", "bodies", "playground", "cards_compare", "validation"} {
		if !strings.Contains(string(b), `"`+f+`"`) {
			t.Errorf("features %s lack %s", b, f)
		}
	}
	tr := got["traffic"].(map[string]any)
	if _, ok := tr["bodies"]; !ok {
		t.Errorf("traffic %v", tr)
	}
}

// D362: a message-only interaction (the agent answered the console's prompt with a Message): its record is no task,
// so the Tasks list leaves it out, and its context's timeline shows the caller's SendMessage and the callee's Message,
// the caller having received that Message, with no taskId on either.
func TestMessageOnlyInteraction(t *testing.T) {
	e := newEnv(t, nil)
	id := e.taskWith("submitted", "hi", "c9", true, &store.Origin{Actor: "caller", Type: "SendMessage", Method: "SendStreamingMessage"})
	e.exec(`update kontra.tasks set exposed_at = null where id = $1`, id)
	origin := `{"actor":"callee","type":"Message","method":"SendMessage","converted_to":"TaskStatusUpdateEvent"}`
	e.originEvent(id, "status", fmt.Sprintf(`{"statusUpdate":{"taskId":"%s","contextId":"c9","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"m2","role":"ROLE_AGENT","parts":[{"text":"hello"}]}}}}`, id),
		&origin, time.Now())
	e.exec(`update kontra.tasks set state = 'completed', terminal_at = now() where id = $1`, id)

	for _, it := range items(e.json("GET", "/v1/tasks", "", 200)) {
		if it["task_id"] == id.String() {
			t.Fatal("the Tasks list shows a message-only interaction")
		}
	}
	tl := e.json("GET", fmt.Sprintf("/v1/contexts/hello/%s/c9/timeline", e.buyer), "", 200)
	if ids, _ := tl["task_ids"].([]any); len(ids) != 0 {
		t.Errorf("the context's tasks %v, want none", ids)
	}
	var got []string
	for _, it := range items(tl) {
		if it["task_id"] != nil {
			t.Errorf("a message-only item names a task: %v", it)
		}
		o, _ := it["origin"].(map[string]any)
		got = append(got, fmt.Sprintf("%v·%v→%v", o["actor"], o["type"], it["caller_type"]))
	}
	if want := []string{"caller·SendMessage→", "callee·Message→Message"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("timeline %v, want %v", got, want)
	}
	v := e.json("GET", "/v1/tasks/"+id.String(), "", 200)
	if v["exposed"] != false || fmt.Sprint(v["actions"]) != "[continue]" {
		t.Errorf("the interaction %v: want exposed false and only continue", v)
	}
}
