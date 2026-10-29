package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
)

// sealed_no_plaintext (ext-sealed.md §9, D62, D171, D174, D175): two sealed services hosted by node A, its tunnel on
// replica B: sealedecho (an A2A seller that repeats its input in an artifact and in its status message and asks one
// follow-up) and everything (server-everything@1.0.0 over MCP). Hiring node H, on replica A with an API key, carries
// a random marker M in every input:
//
//	(a) A2A through H's local endpoint: a text part and a 300 KiB file part, both carrying M, then the follow-up
//	    with M;
//	(b) a plain MCP tools/call echo {message: M};
//	(c) a durable task-augmented simulate-research-query on an ambiguous topic holding M, whose one elicitation is
//	    answered with M, and its tasks/result;
//	(d) A2A: a first message holding M that the seller refuses with an error quoting it (M3 review security-1): the
//	    task fails, and the error's text stays on the hosting node.
//
// The local clients receive M in every answer (the echo, the artifact, the status message, the MCP content, the
// elicitation and the task's result), so M crossed the gate both ways; and M, in none of its forms (raw,
// JSON-escaped, hex, base64, base64url), is in any table of schema kontra, either replica's log (debug level), any
// node's log, the Temporal histories, or any bucket object. task_events parts carry the envelope's media type; the
// 300 KiB part is a kontra-blob: part with a blobs row whose object is an age file; the calls rows name the tools,
// with is_error false and task_id set for (c); every statusMessage stored for (c) is "sealed".
func TestSealedNoPlaintext(t *testing.T) {
	h := start(t, Options{
		Tunnel: map[string]TunnelService{"sealedecho": {Sealed: "true"}},
		MCP:    map[string]MCPService{"everything": {Sealed: "true", ToolsDeny: EverythingDeny}},
		Blobs:  true,
		Peer:   true,
	})
	ctx := ctxFor(t, 10*time.Minute)
	seller, err := h.StartEchoSeller()
	if err != nil {
		t.Fatal(err)
	}
	every := h.StartEverything(t, "everything")

	// The hosting node's tunnel on replica B, the hiring node's on A.
	h.Front.Route(h.Gates[1])
	host, _ := startNode(t, ctx, h, "host", map[string]string{"sealedecho": seller.URL}, func(o *NodeOptions) {
		o.MCP = map[string]string{"everything": every.URL}
	})
	waitSealedCard(t, ctx, h, "sealedecho")
	h.Front.Route(h.Gates[0])
	hirer, err := h.StartNode(ctx, NodeOptions{Name: "hirer", APIKey: h.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	local, err := hirer.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r := sessionReplica(ctx, h, host.Session()); r != h.Gates[1].ReplicaID {
		t.Fatalf("the hosting node's session is on %q, want replica B", r)
	}
	if r := sessionReplica(ctx, h, hirer.Session()); r != h.Gates[0].ReplicaID {
		t.Fatalf("the hiring node's session is on %q, want replica A", r)
	}
	m := NewMarker()
	t.Logf("marker %s; hosting node %s on B, hiring node %s on A", m, host.KeyID, hirer.KeyID)

	// (a) A2A: a text part and a 300 KiB file part carrying M, then a follow-up with M.
	c := localA2A(t, ctx, local, "sealedecho")
	file := bytes.Repeat([]byte(m+" 300 KiB of a sealed file "), 300<<10/(len(m)+25)+1)[:300<<10]
	first := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(echoAskPrefix+" "+m),
		&a2a.Part{Content: a2a.Raw(file), MediaType: "application/octet-stream", Filename: "notes.bin"})
	asked := sendTask(t, ctx, c, first)
	if asked.Status.State != a2a.TaskStateInputRequired || asked.Status.Message == nil ||
		asked.Status.Message.Parts[0].Text() != "which one of "+echoAskPrefix+" "+m+"?" {
		t.Fatalf("(a) the first turn: %+v", asked.Status)
	}
	if len(asked.Artifacts) == 0 || len(asked.Artifacts[0].Parts) != 2 ||
		asked.Artifacts[0].Parts[0].Text() != "echo: "+echoAskPrefix+" "+m || !bytes.Equal(asked.Artifacts[0].Parts[1].Raw(), file) {
		t.Fatalf("(a) the echo artifact is not the input: %d artifacts", len(asked.Artifacts))
	}
	turns := seller.Turns()
	if len(turns) != 1 || turns[0][0].Text() != echoAskPrefix+" "+m || !bytes.Equal(turns[0][1].Raw(), file) {
		t.Fatalf("(a) the seller did not receive the plaintext: %d turns", len(turns))
	}
	follow := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(m))
	follow.TaskID, follow.ContextID = asked.ID, asked.ContextID
	done := sendTask(t, ctx, c, follow)
	if done.Status.State != a2a.TaskStateCompleted || done.Status.Message == nil || done.Status.Message.Parts[0].Text() != "done: "+m {
		t.Fatalf("(a) the follow-up: %+v", done.Status)
	}
	echoed := false
	for _, a := range done.Artifacts {
		for _, p := range a.Parts {
			echoed = echoed || p.Text() == "echo: "+m
		}
	}
	if !echoed {
		t.Fatal("(a) the follow-up's echo artifact did not reach the local client")
	}
	a2aTask := done.ID

	// (b) A plain MCP call through the hiring node.
	mc := NewMCPClient(local+"/mcp/everything", "")
	init, err := mc.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(init.Text(), mcpwire.MetaSealed) {
		t.Fatalf("(b) the initialize result does not declare sealing: %s", init.Text())
	}
	r, err := mc.Call(ctx, 1, "echo", map[string]any{"message": m})
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := r.ToolText(); isErr || text != "Echo: "+m || strings.Contains(r.Text(), sealed.ResourceURI) {
		t.Fatalf("(b) echo: %d %s", r.Status, r.Text())
	}

	// (c) A durable call whose upstream asks one elicitation.
	r, err = mc.Request(ctx, 2, "tools/call", map[string]any{"name": "simulate-research-query",
		"arguments": map[string]any{"topic": m, "ambiguous": true}, "task": map[string]any{"ttl": 300000}})
	if err != nil {
		t.Fatal(err)
	}
	task, _ := r.Result()["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	if taskID == "" {
		t.Fatalf("(c) no CreateTaskResult: %d %s", r.Status, r.Text())
	}
	eventually(t, 90*time.Second, "(c) tasks/get input_required", func() bool {
		r, err := mc.Request(ctx, 3, "tasks/get", map[string]any{"taskId": taskID})
		return err == nil && r.Result()["status"] == "input_required"
	})
	s, err := mc.Open(ctx, "POST", mustJSON(t, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tasks/result",
		"params": map[string]any{"taskId": taskID}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	elicit, _ := firstRequest(t, ctx, s, 90*time.Second)
	if elicit["method"] != "elicitation/create" {
		t.Fatalf("(c) the first request on tasks/result: %v", elicit)
	}
	params, _ := json.Marshal(elicit["params"])
	if !strings.Contains(string(params), jsonText(m)) || strings.Contains(string(params), mcpwire.MetaSealed) {
		t.Fatalf("(c) the elicitation the local client gets is not the plaintext: %s", params)
	}
	answer := mustJSON(t, map[string]any{"jsonrpc": "2.0", "id": elicit["id"], "result": map[string]any{"action": "accept",
		"content": map[string]any{"interpretation": m}}})
	if r, err := mc.Post(ctx, answer); err != nil || r.Status != 202 {
		t.Fatalf("(c) the answer: %v %+v", err, r)
	}
	final := answerOn(t, ctx, s, 90*time.Second)
	fb, _ := json.Marshal(final)
	if !strings.Contains(string(fb), "Clarification**: "+jsonText(m)) || strings.Contains(string(fb), sealed.ResourceURI) {
		t.Fatalf("(c) the task's result did not carry the answer back: %s", fb)
	}

	// (d) A first message the seller refuses with an error quoting M: the task fails; neither the node's answer nor the
	// gate's failure text, log line or Temporal history holds M.
	bad := sendTask(t, ctx, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(echoFailPrefix+" "+m)))
	eventually(t, 90*time.Second, "(d) the refused task fails", func() bool {
		got, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: bad.ID})
		return err == nil && got.Status.State == a2a.TaskStateFailed
	})
	if err := h.WaitWorkflowClosed(ctx, bad.ID, time.Minute); err != nil {
		t.Fatal(err)
	}

	// What the gate kept: envelopes only.
	if h.Bucket == nil {
		t.Fatal("no bucket")
	}
	events := strings.Join(texts(t, h, `select payload::text from kontra.task_events where task_id = $1`, string(a2aTask)), "\n")
	if !strings.Contains(events, sealed.MediaType) {
		t.Fatal("(a) task_events carry no part with the envelope's media type")
	}
	if !strings.Contains(events, "kontra-blob:") {
		t.Fatal("(a) task_events carry no kontra-blob: part for the 300 KiB file")
	}
	refs := texts(t, h, `select ref from kontra.blobs where task_id = $1`, string(a2aTask))
	if len(refs) < 2 {
		t.Fatalf("(a) %d blobs rows for the task, want the file part and its echo", len(refs))
	}
	objs, err := h.Bucket.Objects()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		var obj []byte
		for k, b := range objs {
			if strings.HasSuffix(k, "/"+string(a2aTask)+"/"+ref) {
				obj = b
			}
		}
		if !bytes.HasPrefix(obj, []byte("age-encryption.org/v1\n")) || len(obj) < 300<<10 {
			t.Fatalf("(a) the object of blobs row %s is not an age file of the part (%d bytes)", ref, len(obj))
		}
	}
	calls := callsOf(t, h, "everything")
	var echoCall, taskCall *CallRow
	for i := range calls {
		switch calls[i].Tool {
		case "echo":
			echoCall = &calls[i]
		case "simulate-research-query":
			taskCall = &calls[i]
		}
	}
	if echoCall == nil || echoCall.IsError == nil || *echoCall.IsError || echoCall.TaskID != "" {
		t.Fatalf("(b) the echo call's calls row: %+v", echoCall)
	}
	if taskCall == nil || taskCall.IsError == nil || *taskCall.IsError || taskCall.TaskID != taskID {
		t.Fatalf("(c) the task's calls row: %+v (task %s)", taskCall, taskID)
	}
	msgs := texts(t, h, `select m from (select coalesce(payload->>'statusMessage', payload->'task'->>'statusMessage') m
		from kontra.task_events where task_id = $1 and kind in ('task', 'status')) x where m is not null`, taskID)
	if len(msgs) == 0 {
		t.Fatal("(c) no status row with a statusMessage: the upstream's messages were not seen")
	}
	for _, s := range msgs {
		if s != "sealed" {
			t.Fatalf("(c) a stored statusMessage is %q, want \"sealed\"", s)
		}
	}
	if err := h.WaitWorkflowClosed(ctx, a2a.TaskID(taskID), time.Minute); err != nil {
		t.Fatal(err)
	}
	requireMarkerAbsent(t, ctx, h, m)
	// The scan's positive controls: strings each source does hold are found there.
	requireScanSees(t, ctx, h, string(a2aTask), "table kontra.tasks", "table kontra.task_events", "history ")
	requireScanSees(t, ctx, h, "replica ready to serve", "log of gate A", "log of gate B")
	requireScanSees(t, ctx, h, "tunnel session open", "log of node host", "log of node hirer")
	requireScanSees(t, ctx, h, "age-encryption.org/v1", "bucket ")
}

// jsonText is s as it appears inside a JSON string written by Go (HTML characters escaped).
func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
