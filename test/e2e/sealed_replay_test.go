package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// sealed_replay (ext-sealed.md §5–§7, §9; D175): sealed parts sent straight to the gate with an API key, as kontra
// seal prints them (the binary, ext-sealed.md §5.1), to sealedecho hosted by a node:
//
//   - a first message M1 opens task T1 (input-required); the same message sent by buyer B is a new task T2, which
//     the hosting node refuses replayed (its replyTo opened T1's job): T2 fails sealed_rejected;
//   - T1's sealed follow-up (kontra seal --task T1, once --open bound the job to T1) sent into T3, another live job,
//     is refused wrong_job: T3 fails sealed_rejected; the same follow-up into T1 completes it;
//   - a sealed MCP tools/call (sealed here as a hiring node seals it) resent on the same session is refused replayed,
//     and on another session wrong_message: each a JSON-RPC error carrying the reason;
//   - each rejection has its audit row, the A2A ones an attempt whose outcome is sealed_rejected and no second
//     attempt; the seller and the MCP upstream received none of the refused requests;
//   - the gate's own retry: with the seller unreachable the node answers 502 target_unreachable, which ends attempt 1
//     tunnel_lost (relay.md §6.4's row for a response the node wrote itself; the gate's log shows the node's reason
//     for that attempt), and the gate's next attempt, once it is back, is accepted and completes (a request the node
//     answered itself opens no job).
//
// The residual of §7 (a replay delivered within max_age to another hosting node of the service, which has not seen
// its replyTo) is logged, not asserted as a pass.
func TestSealedReplay(t *testing.T) {
	h := start(t, Options{
		Tunnel: map[string]TunnelService{"vault": {Sealed: "true", Retries: retries(3)}},
		MCP:    map[string]MCPService{"dbl": {Sealed: "true"}},
	})
	ctx := ctxFor(t, 10*time.Minute)
	seller, err := h.StartEchoSeller()
	if err != nil {
		t.Fatal(err)
	}
	cut, err := NewProxy(strings.TrimPrefix(seller.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	h.OnClose(func() error { cut.Close(); return nil })
	up := mcptest.New()
	up.Tools = []json.RawMessage{dualEraTool("echo", "")}
	dbl := h.Double(up)
	startNode(t, ctx, h, "host", map[string]string{"vault": "http://" + cut.Addr}, func(o *NodeOptions) {
		o.MCP = map[string]string{"dbl": dbl}
	})
	waitSealedCard(t, ctx, h, "vault")
	g := h.Gates[0]
	sealHome := tempDir(t, h, "seal-home")
	seal := func(args ...string) []byte {
		t.Helper()
		return kontraSeal(t, h, sealHome, args...)
	}
	send := func(key string, msg []byte) *a2a.Task {
		t.Helper()
		b := &Buyer{Key: key}
		r, err := b.RPC(ctx, g, "vault", "SendMessage", map[string]any{"message": json.RawMessage(msg)})
		if err != nil {
			t.Fatal(err)
		}
		return rpcTask(t, r)
	}

	// T1: a sealed first message, input-required.
	m1 := seal("--context", "ctx-replay", "--media-type", "text/plain", writeFile(t, sealHome, "m1.txt", echoAskPrefix+" first"))
	t1 := send(h.APIKey, m1)
	if t1.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("T1: %+v", t1.Status)
	}
	answer, _ := json.Marshal(t1)
	if out := seal("--open", writeFile(t, sealHome, "t1.json", string(answer))); !strings.Contains(string(out), "which one of "+echoAskPrefix+" first?") {
		t.Fatalf("kontra seal --open of T1's answer: %q", out)
	}
	before := seller.Requests()

	// T2: the same message from another buyer, a new task: replayed.
	t2 := send(h.APIKeyB, m1)
	if t2.ID == t1.ID {
		t.Fatalf("buyer B's resend is buyer A's task %s", t1.ID)
	}
	waitSealedRejected(t, ctx, h, t2.ID, "replayed")
	if n := seller.Requests(); n != before {
		t.Fatalf("the seller received %d requests from the replayed first message", n-before)
	}

	// T3: another live job in the same context; T1's follow-up sent into it: wrong_job.
	m3 := seal("--context", "ctx-replay", "--media-type", "text/plain", writeFile(t, sealHome, "m3.txt", echoAskPrefix+" third"))
	t3 := send(h.APIKey, m3)
	if t3.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("T3: %+v", t3.Status)
	}
	before = seller.Requests()
	f1 := seal("--task", string(t1.ID), "--media-type", "text/plain", writeFile(t, sealHome, "f1.txt", "the answer"))
	var into3 map[string]any
	if err := json.Unmarshal(f1, &into3); err != nil {
		t.Fatal(err)
	}
	if t3.ContextID != t1.ContextID {
		t.Fatalf("T3's context %s is not T1's %s", t3.ContextID, t1.ContextID)
	}
	into3["taskId"] = string(t3.ID)
	b3, _ := json.Marshal(into3)
	send(h.APIKey, b3)
	waitSealedRejected(t, ctx, h, t3.ID, "wrong_job")
	if n := seller.Requests(); n != before {
		t.Fatalf("the seller received %d requests from the refused replays", n-before)
	}
	// The same follow-up into T1, its own job: accepted.
	done := send(h.APIKey, f1)
	if done.ID != t1.ID {
		t.Fatalf("the follow-up answered task %s, want T1 %s", done.ID, t1.ID)
	}
	eventually(t, 60*time.Second, "T1 completed by its follow-up", func() bool {
		s, _ := h.TaskState(ctx, t1.ID)
		return s == "completed"
	})
	last := seller.Turns()
	if got := last[len(last)-1]; len(got) != 1 || string(got[0].Raw()) != "the answer" || got[0].MediaType != "text/plain" {
		t.Fatalf("the seller's last turn is not the follow-up's plaintext: %d parts", len(got))
	}

	// MCP: a sealed tools/call resent on the same session, and on another.
	mc := NewMCPClient(g.URL+"/mcp/dbl", h.APIKey)
	init, err := mc.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ms := newMCPSealer(t, init, "dbl")
	body := ms.call(t, mc.SID, 7, "echo", map[string]any{"text": "sealed hello"})
	r, err := mc.Post(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if hd, plain := ms.open(t, r); hd.Tool != "echo" || hd.MessageID != "7" || hd.Service != "dbl" || !strings.Contains(string(plain), "sealed hello") {
		t.Fatalf("the sealed call's answer: %+v %s", hd, plain)
	}
	calls := up.Calls("echo")
	r, err = mc.Post(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if code, msg, _, ok := r.Error(); !ok || code != mcpwire.CodeInvalidParams || !strings.Contains(msg, "replayed") {
		t.Fatalf("the call resent on its session: %d %s", r.Status, r.Text())
	}
	other := NewMCPClient(g.URL+"/mcp/dbl", h.APIKey)
	if _, err := other.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	r, err = other.Post(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if code, msg, _, ok := r.Error(); !ok || code != mcpwire.CodeInvalidParams || !strings.Contains(msg, "wrong_message") {
		t.Fatalf("the call resent on another session: %d %s", r.Status, r.Text())
	}
	if n := up.Calls("echo"); n != calls {
		t.Fatalf("the MCP upstream received %d of the refused calls", n-calls)
	}
	for _, reason := range []string{"replayed", "wrong_message"} {
		if n := auditRows(t, h, `"protocol":"mcp"`, `"reason":"`+reason+`"`, `"tool":"echo"`); n != 1 {
			t.Fatalf("%d audit rows for the MCP call refused %s, want 1", n, reason)
		}
	}

	// The gate's own retry after the node's 502: the seller unreachable, then back.
	cut.Cut()
	m4 := seal("--media-type", "text/plain", writeFile(t, sealHome, "m4.txt", "retried"))
	got := make(chan *a2a.Task, 1)
	go func() {
		b := &Buyer{Key: h.APIKey}
		r, err := b.RPC(ctx, g, "vault", "SendMessage", map[string]any{"message": json.RawMessage(m4)})
		if err != nil {
			t.Error(err)
			got <- nil
			return
		}
		var task a2a.Task
		if err := json.Unmarshal(taskJSON(r.Result), &task); err != nil {
			t.Errorf("the retried task's answer: %s", r.Body)
			got <- nil
			return
		}
		got <- &task
	}()
	var t4 a2a.TaskID
	eventually(t, 60*time.Second, "the first attempt ended by the node's 502", func() bool {
		ids := texts(t, h, `select t.id::text from kontra.tasks t join kontra.attempts a on a.task_id = t.id
			where t.service = 'vault' and a.n = 1 and a.outcome is not null and t.id not in ($1, $2, $3)`,
			string(t1.ID), string(t2.ID), string(t3.ID))
		if len(ids) == 1 {
			t4 = a2a.TaskID(ids[0])
		}
		return len(ids) == 1
	})
	cut.Restore()
	var t4done *a2a.Task
	select {
	case t4done = <-got:
	case <-time.After(90 * time.Second):
		t.Fatal("the retried task did not answer within 90 s")
	}
	if t4done == nil || t4done.ID != t4 || t4done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the retried task: %+v", t4done)
	}
	atts, err := h.Attempts(ctx, t4)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) < 2 || atts[0].Outcome != "tunnel_lost" || atts[len(atts)-1].Outcome != "completed" {
		t.Fatalf("the retried task's attempts: %+v, want attempt 1 tunnel_lost and the last completed", atts)
	}
	// Attempt 1 ended on the hosting node's own 502 Kontra-Node: target_unreachable, not on a lost tunnel or any other
	// failure §6.4 also retries: the dispatch logged the node's reason for that task and attempt.
	if upstreamFailures(t, h, t4, 1, "(Kontra-Node: target_unreachable)") == 0 {
		t.Fatalf("no replica logged attempt 1 of %s failing on the node's 502 Kontra-Node: target_unreachable", t4)
	}
	t.Logf("the gate's retry: attempt 1 %s, attempt %d %s", atts[0].Outcome, len(atts), atts[len(atts)-1].Outcome)
	t.Logf("ext-sealed.md §7's residual, not tested as a pass: a first message replayed within max_age to another " +
		"hosting node of the service, which has not seen its replyTo, is executed again there; the reply still goes " +
		"to the original buyer's key")
}

// upstreamFailures counts the records of both replicas' logs where the dispatch of the task's attempt n logged a
// failed upstream call whose error holds reason (relay executor.go's "upstream call failed").
func upstreamFailures(t *testing.T, h *Harness, task a2a.TaskID, n int, reason string) int {
	t.Helper()
	count := 0
	for _, g := range h.Gates {
		for _, line := range strings.Split(readFile(g.LogPath), "\n") {
			var rec struct {
				Msg     string `json:"msg"`
				TaskID  string `json:"task_id"`
				Attempt int    `json:"attempt"`
				Error   string `json:"error"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if rec.Msg == "upstream call failed" && rec.TaskID == string(task) && rec.Attempt == n && strings.Contains(rec.Error, reason) {
				count++
			}
		}
	}
	return count
}

// kontraSeal runs the kontra binary's `seal` with HOME home and, unless args name --open, --service the Front's vault
// URL and --api-key the harness's buyer A key (kontra seal reads no key from the environment), and returns its stdout.
func kontraSeal(t *testing.T, h *Harness, home string, args ...string) []byte {
	t.Helper()
	bin, err := nodeBinary()
	if err != nil {
		t.Fatal(err)
	}
	full := []string{"seal"}
	if len(args) == 0 || args[0] != "--open" {
		full = append(full, "--service", h.Front.URL+"/agents/vault", "--api-key", h.APIKey)
	}
	full = append(full, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, RunEnv + "=" + h.Run()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("kontra %s: %v\n%s", strings.Join(full, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// taskJSON is the task of a SendMessage result ({"task": …}, or the task itself).
func taskJSON(result json.RawMessage) json.RawMessage {
	var wrapped struct {
		Task json.RawMessage `json:"task"`
	}
	if json.Unmarshal(result, &wrapped) == nil && len(wrapped.Task) > 0 {
		return wrapped.Task
	}
	return result
}

// rpcTask is the task a SendMessage answered with.
func rpcTask(t *testing.T, r *RPCResult) *a2a.Task {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("SendMessage: %d %s", r.HTTPStatus, r.Body)
	}
	var task a2a.Task
	if err := json.Unmarshal(taskJSON(r.Result), &task); err != nil || task.ID == "" {
		t.Fatalf("SendMessage did not answer a task: %v %s", err, r.Body)
	}
	return &task
}

// waitSealedRejected waits for a task to fail sealed_rejected and checks what the gate recorded: an audit row naming
// the task and the node's reason, one attempt whose outcome is sealed_rejected, and no retry.
func waitSealedRejected(t *testing.T, ctx context.Context, h *Harness, task a2a.TaskID, reason string) {
	t.Helper()
	eventually(t, 60*time.Second, "task "+string(task)+" failed sealed_rejected", func() bool {
		s, f, err := h.TaskFailure(ctx, task)
		return err == nil && s == "failed" && f == "sealed_rejected"
	})
	if n := auditRows(t, h, `"task_id":"`+string(task)+`"`, `"reason":"`+reason+`"`, `"event":"sealed.rejected"`); n != 1 {
		t.Fatalf("task %s: %d sealed.rejected audit rows with reason %s, want 1", task, n, reason)
	}
	// None is retried: the attempt that drew the refusal is the last one, and stays so.
	time.Sleep(2 * time.Second)
	atts, err := h.Attempts(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) == 0 || atts[len(atts)-1].Outcome != "sealed_rejected" {
		t.Fatalf("task %s's attempts: %+v", task, atts)
	}
	for _, a := range atts[:len(atts)-1] {
		if a.Outcome == "sealed_rejected" {
			t.Fatalf("task %s was retried after a refusal: %+v", task, atts)
		}
	}
}

// auditRows counts the audit rows whose JCS holds every one of substrs.
func auditRows(t *testing.T, h *Harness, substrs ...string) int {
	t.Helper()
	n := 0
	for _, row := range texts(t, h, `select convert_from(row_jcs, 'UTF8') from kontra.audit`) {
		ok := true
		for _, s := range substrs {
			ok = ok && strings.Contains(row, s)
		}
		if ok {
			n++
		}
	}
	return n
}

// mcpSealer seals tools/call arguments as a hiring node does (ext-sealed.md §3, hiring rule 3), for a test that
// sends sealed bodies straight to the gate: the recipients from the initialize result's
// capabilities.experimental["sh.kontra/sealed"], one per-job key and rk.
type mcpSealer struct {
	service    string // the hired service, as the gate's URL path names it (D221)
	recipients []age.Recipient
	kids       []string
	job        *age.X25519Identity
	rk         []byte
}

func newMCPSealer(t *testing.T, init *MCPResponse, service string) *mcpSealer {
	t.Helper()
	caps, _ := init.Result()["capabilities"].(map[string]any)
	exp, _ := caps["experimental"].(map[string]any)
	entry, _ := exp[mcpwire.MetaSealed].(map[string]any)
	params, _ := entry["params"].(map[string]any)
	list, _ := params["recipients"].([]any)
	s := &mcpSealer{service: service}
	for _, v := range list {
		str, _ := v.(string)
		r, err := age.ParseX25519Recipient(str)
		if err != nil {
			t.Fatalf("a recipient of the sealed capability: %v", err)
		}
		s.recipients = append(s.recipients, r)
		s.kids = append(s.kids, tunnel.KeyID(str))
	}
	if len(s.recipients) == 0 {
		t.Fatalf("the initialize result declares no sealed recipient: %s", init.Text())
	}
	slices.Sort(s.kids)
	var err error
	if s.job, err = age.GenerateX25519Identity(); err != nil {
		t.Fatal(err)
	}
	if s.rk, err = sealed.NewRK(); err != nil {
		t.Fatal(err)
	}
	return s
}

// call is a sealed tools/call body for the session sid (its gate Mcp-Session-Id) under JSON-RPC id id.
func (s *mcpSealer) call(t *testing.T, sid string, id int, tool string, args any) string {
	t.Helper()
	plain, _ := json.Marshal(args)
	body, err := jcs.Transform(plain)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sid))
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, Service: s.service, IAT: time.Now().Unix(),
		Session: hex.EncodeToString(sum[:]), Tool: tool, MessageID: strconv.Itoa(id), Op: sealed.OpCall, Part: 0, Parts: 1, Kind: sealed.KindData,
		Kids: s.kids, ReplyTo: s.job.Recipient().String(), RK: sealed.EncodeRK(s.rk)}
	ct, err := sealed.Seal(s.recipients, hd, body)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{},"_meta":{%q:%s}}}`,
		id, tool, mcpwire.MetaSealed, mustJSON(t, sealed.NewMCPEnvelope(ct)))
}

// open opens a sealed tools/call result with the job's key and checks its auth.
func (s *mcpSealer) open(t *testing.T, r *MCPResponse) (sealed.Header, []byte) {
	t.Helper()
	res := r.Result()
	if res == nil {
		t.Fatalf("no result: %d %s", r.Status, r.Text())
	}
	raw, _ := json.Marshal(res)
	ct, ok, err := sealed.ParseMCPResult(raw)
	if !ok || err != nil {
		t.Fatalf("the result is not sealed: %v %s", err, raw)
	}
	hd, plain, err := sealed.Open([]age.Identity{s.job}, ct)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealed.VerifyAuth(s.rk, hd, plain); err != nil {
		t.Fatalf("the result's auth: %v", err)
	}
	return hd, plain
}
