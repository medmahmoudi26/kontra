package sealing

import (
	"os"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
)

// M3 review round 4: regression tests of the hosting node's sealing.

// r4-hosting-1 (D252): the buyer's sealed task-augmented tools/call is handed whole to the server, and the gate ends
// the exchange before the server's CreateTaskResult is read: End records the call (D233), but the task is never bound
// to the job. On the buyer's own session (its preamble and upstream session, which the gate chooses) tasks/result and
// tasks/get of it are wrong_task, tasks/list leaves it out and its status notification is dropped; on another session
// tasks/list is refused.
func TestReview4UnboundTaskAfterCut(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("buyer-session-task")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-T")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "longRun", "1", `{"prompt":"SECRET-PROMPT"}`, true)); reason != "" {
		t.Fatal(reason)
	}
	x.handed.Store(true)
	x.End() // the gate ended the exchange before the server's answer
	list := f.h.MCPOn(mcpPre(s1, "", ""), "up-T")
	if _, r := fromClient(t, list, []byte(`{"jsonrpc":"2.0","id":50,"method":"tasks/list","params":{}}`)); r != "" {
		t.Fatalf("tasks/list on the buyer's session: %s", r)
	}
	out, keep, err := list.FromServer([]byte(`{"jsonrpc":"2.0","id":50,"result":{"tasks":[{"taskId":"up-task-1","status":"completed","statusMessage":"SECRET-STATUS from SECRET-PROMPT","createdAt":"2026-10-27T12:00:00Z","lastUpdatedAt":"2026-10-27T12:00:00Z","ttl":60000}]}}`))
	if err != nil || !keep || strings.Contains(string(out), "SECRET") || strings.Contains(string(out), "up-task-1") {
		t.Errorf("tasks/list on the buyer's session lists the unbound task: %v %v %s", err, keep, out)
	}
	for _, method := range []string{"tasks/result", "tasks/get", "tasks/cancel"} {
		y := f.h.MCPOn(mcpPre(s1, "", ""), "up-T")
		if _, r := fromClient(t, y, []byte(`{"jsonrpc":"2.0","id":51,"method":"`+method+`","params":{"taskId":"up-task-1"}}`)); r != sealed.RejectWrongTask {
			t.Errorf("%s of the buyer's unbound task on its session: %q, want wrong_task", method, r)
		}
	}
	note := `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"up-task-1","status":"completed","statusMessage":"SECRET-STATUS"}}`
	if out, keep, err := f.h.MCPOn(mcpPre(s1, "", ""), "up-T").FromServer([]byte(note)); err != nil || keep {
		t.Errorf("the unbound task's status notification on the buyer's session: %v %v %s", err, keep, out)
	}
	other := f.h.MCPOn(mcpPre(sessionHash("gate-own-session"), "", ""), "up-other")
	if _, r := fromClient(t, other, []byte(`{"jsonrpc":"2.0","id":52,"method":"tasks/list","params":{}}`)); r != sealed.RejectBadHeader {
		t.Errorf("tasks/list on another session of a service with sealed state: %q, want bad_header", r)
	}
}

// r4-hosting-2 (D253): a server that mints no Mcp-Session-Id. The buyer's sealed plain call's sampling request, sent on
// the call's own stream, is sealed; the gate's own plaintext answer to it, with no Mcp-Session-Id, under a preamble that
// names no session or another session, is refused bad_header. The buyer's sealed answer on its own session passes.
func TestReview4SessionlessPlainAnswer(t *testing.T) {
	for _, other := range []string{"", sessionHash("gate-own-session")} {
		f := newFixture(t)
		b := newBuyer(t)
		s1 := sessionHash("buyer-session")
		x := f.h.MCPOn(mcpPre(s1, "1", ""), "")
		if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "sampleLLM", "1", `{"prompt":"SECRET-PROMPT"}`, false)); reason != "" {
			t.Fatal(reason)
		}
		x.Accepted()
		samp := fromServer(t, x, `{"jsonrpc":"2.0","id":0,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"SECRET-PROMPT"}}],"maxTokens":10}}`)
		if strings.Contains(samp, "SECRET-PROMPT") || !strings.Contains(samp, mcpwire.MetaSealed) {
			t.Fatalf("not sealed: %s", samp)
		}
		ans := []byte(`{"jsonrpc":"2.0","id":0,"result":{"role":"assistant","content":{"type":"text","text":"GATE-WROTE-THIS"},"model":"m"}}`)
		if out, r := fromClient(t, f.h.MCPOn(mcpPre(other, "", ""), ""), ans); r != sealed.RejectBadHeader {
			t.Errorf("preamble session %q: the gate's plaintext answer to the buyer's sealed input request: %q %s", other, r, out)
		}
		sealedAns := []byte(`{"jsonrpc":"2.0","id":0,"result":{"_meta":{"sh.kontra/sealed":` +
			envelope(f.seal(f.answerHeader(b, s1, "sampleLLM", "0", ""), `{"role":"assistant","content":{"type":"text","text":"BUYER-ANSWER"},"model":"m"}`)) + `}}}`)
		if out, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "", ""), ""), sealedAns); r != "" || !strings.Contains(string(out), "BUYER-ANSWER") {
			t.Errorf("the buyer's sealed answer on its own session: %q %s", r, out)
		}
	}
}

// r4-hosting-3 (D254), A2A: the node restarts while the buyer's first sealed message (a blocking SendMessage: the
// service's headers come with its answer) is with the service. Its records were written before it was handed and are
// applied as the node starts: the service is guarded (ListTasks is refused, D227) and the message resent under its
// gate task is a replay. A request the node answered itself before the restart (Release) records nothing: the gate's
// retry of it is accepted.
func TestReview4RestartA2AFirstMessageInFlight(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const t1, t2, ctx = "0193c0de-0000-7000-8000-0000000004a1", "0193c0de-0000-7000-8000-0000000004a2", "ctx-r4"
	body := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "BUYER PLAINTEXT")))
	_, x, r := f.request(pre(t1, ctx), body)
	if r != "" {
		t.Fatal(r)
	}
	x.handed.Store(true) // the transport read the body whole: the service has it
	f.open()             // the node restarts (a crash): x's End never runs
	if _, _, r := f.requestAtMethod(pre(t2, ""), `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"historyLength":10}}`); r != sealed.RejectBadHeader {
		t.Errorf("after a restart, ListTasks on a service whose sealed first message is with the service: %q, want bad_header", r)
	}
	if _, _, r := f.request(pre(t1, ctx), body); r != sealed.RejectReplayed {
		t.Errorf("after a restart, the buyer's first message resent under its gate task: %q, want replayed", r)
	}

	g := newFixture(t)
	c := newBuyer(t)
	body = send("m1", "", "", c.replyTo(), sealedPartJSON(g.seal(g.reqHeader(c, "", ctx, "m1", 0, 1), "BUYER PLAINTEXT")))
	_, y, r := g.request(pre(t1, ctx), body)
	if r != "" {
		t.Fatal(r)
	}
	y.Release() // the node answered the request itself (a 502)
	g.open()
	if _, _, r := g.request(pre(t1, ctx), body); r != "" {
		t.Errorf("after a restart, the gate's retry of a request the node answered itself: %q", r)
	}
}

// r4-hosting-3 (D254), MCP: the node restarts while the buyer's first sealed call on an upstream session is with the
// server. After the restart the session is the job's: the gate's unsealed call on it, under a preamble of no session,
// is refused, and the buyer's call resent is a replay.
func TestReview4RestartMCPCallInFlight(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("buyer-session-login")
	call := f.sealedCallBody(b, s1, "login", "1", `{"secret":"SECRET-LOGIN"}`, false)
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-R")
	if _, reason := fromClient(t, x, call); reason != "" {
		t.Fatal(reason)
	}
	f.open() // the node restarts with the call handed: neither Accepted nor End runs
	g := f.h.MCPOn(mcpPre("", "", ""), "up-R")
	if _, r := fromClient(t, g, []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)); r != sealed.RejectBadHeader {
		t.Errorf("after a restart, the gate's unsealed call on the buyer's upstream session: %q, want bad_header", r)
	}
	if _, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "1", ""), "up-R"), call); r != sealed.RejectReplayed {
		t.Errorf("after a restart, the buyer's call resent: %q, want replayed", r)
	}
}

// r4-hosting-4 (D255): the upstream session a sealed job's calls set up stays the job's after the job is swept (7 days
// after its last sealed exchange), across a restart too: the gate's unsealed call on it is refused under a preamble of
// no session and under the buyer's own. A DELETE of the session answered by the server (SessionGone) ends the hold.
func TestReview4SessionHeldAfterSweep(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("buyer-session-login")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-L")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "login", "1", `{"secret":"SECRET-LOGIN"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	x.Accepted()
	fromServer(t, x, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}],"isError":false}}`)
	f.now = f.now.Add(JobRetention + time.Hour)
	if err := f.h.Sweep(f.now); err != nil {
		t.Fatal(err)
	}
	f.open()
	whoami := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	for _, s := range []string{"", s1} {
		if _, r := fromClient(t, f.h.MCPOn(mcpPre(s, "", ""), "up-L"), whoami); r != sealed.RejectBadHeader {
			t.Errorf("after the job's sweep, the gate's unsealed call on the buyer's upstream session (preamble %q): %q, want bad_header", s, r)
		}
	}
	f.h.MCPOn(mcpPre("", "", ""), "up-L").SessionGone()
	if _, r := fromClient(t, f.h.MCPOn(mcpPre("", "", ""), "up-L"), whoami); r != "" {
		t.Errorf("once the server ended the session: %q", r)
	}
}

// r4-hosting-5 (D256): on a sealed session the gate can send any method but tools/call unsealed (resources/read,
// prompts/get, completion/complete, logging/setLevel) and read what the server exposes there: §2 lists them as unsealed,
// and §7 states the residual.
func TestReview4OtherMethodsOnASealedSessionAreAResidual(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("buyer-session-doc")
	x := f.h.MCPOn(mcpPre(s1, "1", ""), "up-D")
	if _, reason := fromClient(t, x, f.sealedCallBody(b, s1, "openDocument", "1", `{"text":"SECRET-DOCUMENT"}`, false)); reason != "" {
		t.Fatal(reason)
	}
	x.Accepted()
	for _, method := range []string{"resources/read", "prompts/get", "completion/complete", "logging/setLevel"} {
		if _, r := fromClient(t, f.h.MCPOn(mcpPre(s1, "", ""), "up-D"), []byte(`{"jsonrpc":"2.0","id":9,"method":"`+method+`","params":{}}`)); r != "" {
			t.Fatalf("%s on a sealed session: %q (the residual changed: say so in ext-sealed.md §7)", method, r)
		}
	}
	spec, err := os.ReadFile("../../spec/ext-sealed.md")
	if err != nil {
		t.Fatal(err)
	}
	_, s7, _ := strings.Cut(string(spec), "## 7. What sealing protects")
	s7, _, _ = strings.Cut(s7, "## 8.")
	found := false
	for _, line := range strings.Split(s7, "\n") {
		if strings.HasPrefix(line, "|") && strings.Contains(line, "resources/read") && strings.Contains(line, "prompts/get") &&
			strings.Contains(line, "D256") {
			found = true
		}
	}
	if !found {
		t.Fatal("ext-sealed.md §7 states no residual for the gate's own unsealed methods on a sealed session (D256)")
	}
}
