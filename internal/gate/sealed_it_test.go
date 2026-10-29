package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/blob/blobtest"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
)

// Sealing through a real gate (spec/ext-sealed.md; sealed_no_plaintext's A2A case, sealed_unsealed_part): a stock
// a2a-go client on a hiring node's 127.0.0.1 endpoint completes tasks, unary and streaming, over JSON-RPC and
// HTTP+JSON, to hello-exec hosted by another node, and answers an input-required question with a sealed follow-up
// (§5 rule 3); the agent receives plaintext and the client reads plaintext, while Postgres (tasks, task_events)
// and the bucket hold only envelopes. A 300 KiB part travels through the bucket as ciphertext both ways and opens on
// the node once its whole age file is read. An unsealed part sent to the gate itself is refused InvalidRequest
// before any task exists.
func TestSealedThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	bt := blobtest.New(t, "sealed")
	services := `blobs: "` + bt.URL + `"
services:
  vault:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
    task: { timeout: 10m, retries: 3 }
  booth:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
    task: { timeout: 10m, retries: 3 }
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	team, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewAPIKeys(e.pool, nil)
	defer keys.Wait()
	buyer, err := keys.Mint(ctx, auth.MintRequest{TenantID: team, Name: "buyer", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(ctx, team, "vault", "test")
	if err != nil {
		t.Fatal(err)
	}
	boothToken, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(ctx, team, "booth", "test")
	if err != nil {
		t.Fatal(err)
	}
	agent := &helloExec{}
	host := startNode(t, t.TempDir(), "--gate", a.url, "--name", "host", "--serve", "a2a:vault="+serveHelloExec(t, agent),
		"--serve", "a2a:booth="+serveAsker(t), "--enroll", token, "--enroll", boothToken)
	host.local(t)
	hostKey := host.keyID(t)
	eventually(t, 20*time.Second, "the gate serves vault's card with the host's key as a recipient", func() bool {
		status, body := cardRoute(t, a.url, buyer.Secret, "vault")
		return status == http.StatusOK && strings.Contains(body, sealed.URI) && strings.Contains(body, "age1")
	})
	hirer := startNode(t, t.TempDir(), "--gate", a.url, "--name", "hirer", "--api-key", buyer.Secret)
	local := hirer.local(t)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	card, err := agentcard.DefaultResolver.Resolve(rctx, local+"/agents/vault")
	cancel()
	if err != nil {
		t.Fatalf("resolve the local card: %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Destroy() })
	if !strings.Contains(hirer.logs.String(), "first seen") || !strings.Contains(hirer.logs.String(), hostKey) {
		t.Errorf("the hiring node did not print the pin's first sight with the host's key id %s", hostKey)
	}

	marker := "MARKER-" + uuid.NewString()[:8]
	hi := sendMessage(t, c, text(marker), false)
	if hi.Status.State != a2a.TaskStateCompleted || hi.Status.Message == nil || hi.Status.Message.Parts[0].Text() != "Hello World" {
		t.Fatalf("a sealed task: %+v", hi.Status)
	}
	if got := agent.received(); got[len(got)-1][0].Text() != marker {
		t.Fatalf("the agent received %q", got[len(got)-1][0].Text())
	}

	// Streaming.
	sctx, scancel := context.WithTimeout(ctx, 60*time.Second)
	streamMarker := "MARKER-stream-" + uuid.NewString()[:8]
	var final a2a.TaskState
	var texts []string
	for ev, err := range c.SendStreamingMessage(sctx, &a2a.SendMessageRequest{Message: text(streamMarker)}) {
		if err != nil {
			scancel()
			t.Fatalf("the stream: %v", err)
		}
		switch v := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			final = v.Status.State
			if v.Status.Message != nil {
				for _, p := range v.Status.Message.Parts {
					texts = append(texts, p.Text())
				}
			}
		case *a2a.Task:
			final = v.Status.State
		}
	}
	scancel()
	if final != a2a.TaskStateCompleted || !strings.Contains(strings.Join(texts, " "), "Hello World") {
		t.Fatalf("a sealed stream ended %s with %q", final, texts)
	}

	// The HTTP+JSON binding: the same through the hiring node's /message:send.
	rc, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithConfig(a2aclient.Config{
		PreferredTransports: []a2a.TransportProtocol{a2a.TransportProtocolHTTPJSON}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rc.Destroy() })
	restMarker := "MARKER-rest-" + uuid.NewString()[:8]
	if rt := sendMessage(t, rc, text(restMarker), false); rt.Status.State != a2a.TaskStateCompleted || rt.Status.Message.Parts[0].Text() != "Hello World" {
		t.Fatalf("a sealed task over HTTP+JSON: %+v", rt.Status)
	}
	if got := agent.received(); got[len(got)-1][0].Text() != restMarker {
		t.Fatalf("the agent received %q over HTTP+JSON", got[len(got)-1][0].Text())
	}

	// input-required and a follow-up (rule 3: the follow-up's header names the gate task, the gate gives the
	// upstream's ids to the service).
	bctx, bcancel := context.WithTimeout(ctx, 30*time.Second)
	boothCard, err := agentcard.DefaultResolver.Resolve(bctx, local+"/agents/booth")
	bcancel()
	if err != nil {
		t.Fatalf("resolve booth's local card: %v", err)
	}
	bc, err := a2aclient.NewFromCard(ctx, boothCard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bc.Destroy() })
	asked := sendMessage(t, bc, text("MARKER-ask"), false)
	if asked.Status.State != a2a.TaskStateInputRequired || asked.Status.Message == nil ||
		asked.Status.Message.Parts[0].Text() != "which one of MARKER-ask?" {
		t.Fatalf("the question: %+v", asked.Status)
	}
	followUp := text("MARKER-answer")
	followUp.TaskID, followUp.ContextID = asked.ID, asked.ContextID
	done := sendMessage(t, bc, followUp, false)
	if done.Status.State != a2a.TaskStateCompleted || done.Status.Message.Parts[0].Text() != "done: MARKER-answer" {
		t.Fatalf("the follow-up: %+v", done.Status)
	}

	// A 300 KiB part: ciphertext through the bucket, both ways.
	big := bytes.Repeat([]byte("MARKER-big-"), 300<<10/11)
	bigTask := sendMessage(t, c, a2a.NewMessage(a2a.MessageRoleUser, &a2a.Part{Content: a2a.Raw(big), MediaType: "application/octet-stream"}), false)
	if bigTask.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the big part's task: %+v", bigTask.Status)
	}
	got := agent.received()
	if p := got[len(got)-1][0]; !bytes.Equal(p.Raw(), big) {
		t.Fatalf("the agent received %d raw bytes, want the 300 KiB part", len(p.Raw()))
	}
	if len(bigTask.Artifacts) != 1 || !bytes.Equal(bigTask.Artifacts[0].Parts[0].Raw(), big) {
		t.Fatalf("the hiring client received %+v, want the agent's 300 KiB artifact", bigTask.Artifacts)
	}
	rows, err := e.pool.Query(ctx, `select ref, tenant_id, task_id from kontra.blobs`)
	if err != nil {
		t.Fatal(err)
	}
	var objs []blob.Key
	for rows.Next() {
		var k blob.Key
		var ref string
		if err := rows.Scan(&ref, &k.Tenant, &k.Task); err != nil {
			t.Fatal(err)
		}
		k.Ref = blob.Ref(ref)
		objs = append(objs, k)
	}
	if len(objs) < 2 {
		t.Fatalf("%d objects in the bucket, want the two 300 KiB parts", len(objs))
	}
	for _, k := range objs {
		r, _, err := bt.Store.Open(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		if bytes.Contains(b, []byte("MARKER")) || !bytes.HasPrefix(b, []byte("age-encryption.org/v1")) {
			t.Fatalf("a bucket object is not an age file (%d bytes)", len(b))
		}
	}

	// Postgres holds envelopes only.
	for _, q := range []string{`select coalesce(string_agg(x::text, ' '), '') from kontra.task_events x`,
		`select coalesce(string_agg(x::text, ' '), '') from kontra.tasks x`} {
		dump := scalar[string](t, e.pool, q)
		if strings.Contains(dump, "MARKER") || strings.Contains(dump, "Hello World") {
			t.Fatalf("plaintext in Postgres: %s", q)
		}
		if !strings.Contains(dump, sealed.MediaType) {
			t.Fatalf("no envelope in Postgres: %s", q)
		}
	}
	for _, l := range []string{host.logs.String(), hirer.logs.String()} {
		if strings.Contains(l, "MARKER") {
			t.Fatal("a plaintext part reached a log")
		}
	}

	// D221: a message sealed for vault that the gate delivers to booth, another service of the same node (one identity
	// key opens both): the hosting node refuses it wrong_job, so booth's agent never gets it, and the task fails
	// sealed_rejected. The same message to vault, the service it names, completes.
	_, vaultCard := cardRoute(t, a.url, buyer.Secret, "vault")
	misdirected := sealedFor(t, vaultCard, "vault", "MARKER-misdirected")
	postSend := func(service string, msg []byte) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, a.url+"/agents/"+service, strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":`+string(msg)+`,"configuration":{"returnImmediately":true}}}`))
		req.Header.Set("Authorization", "Bearer "+buyer.Secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var r struct {
			Result struct {
				Task struct {
					ID string `json:"id"`
				} `json:"task"`
			} `json:"result"`
		}
		if err := json.Unmarshal(b, &r); err != nil || r.Result.Task.ID == "" {
			t.Fatalf("SendMessage to %s: %d %s", service, resp.StatusCode, b)
		}
		return r.Result.Task.ID
	}
	stateOf := func(id string) string {
		var st string
		eventually(t, 60*time.Second, "the task's end", func() bool {
			st = scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, id)
			return st == "completed" || st == "failed"
		})
		return st
	}
	if st := stateOf(postSend("booth", misdirected)); st != "failed" {
		t.Fatalf("a message sealed for vault, delivered to booth: %s", st)
	}
	if audit := scalar[string](t, e.pool, `select coalesce(string_agg(convert_from(row_jcs, 'UTF8'), ' '), '') from kontra.audit`); !strings.Contains(audit, "wrong_job") {
		t.Fatalf("no audit row of the node's wrong_job refusal: %s", audit)
	}
	if st := stateOf(postSend("vault", sealedFor(t, vaultCard, "vault", "MARKER-direct"))); st != "completed" {
		t.Fatalf("a message sealed for vault, delivered to vault: %s", st)
	}
	if got := agent.received(); got[len(got)-1][0].Text() != "MARKER-direct" {
		t.Fatalf("vault's agent received %q", got[len(got)-1][0].Text())
	}

	// An unsealed part straight to the gate: InvalidRequest, no task.
	before := scalar[int](t, e.pool, `select count(*) from kontra.tasks`)
	req, _ := http.NewRequest(http.MethodPost, a.url+"/agents/vault", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"`+uuid.NewString()+`","role":"ROLE_USER","parts":[{"text":"plain"}]}}}`))
	req.Header.Set("Authorization", "Bearer "+buyer.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "-32600") {
		t.Fatalf("an unsealed part to a sealed service: %d %s", resp.StatusCode, b)
	}
	if after := scalar[int](t, e.pool, `select count(*) from kontra.tasks`); after != before {
		t.Fatalf("a refused unsealed message created %d tasks", after-before)
	}
}

// sealedFor is a new task's first message with one text part sealed for service (its header's service member, D221)
// to the recipients of card, a served card carrying the sealed extension, as kontra seal builds it.
func sealedFor(t *testing.T, card, service, text string) []byte {
	t.Helper()
	var c struct {
		Capabilities struct {
			Extensions []struct {
				URI    string `json:"uri"`
				Params struct {
					Recipients []string `json:"recipients"`
				} `json:"params"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(card), &c); err != nil {
		t.Fatal(err)
	}
	var (
		recipients []age.Recipient
		kids       []string
	)
	for _, x := range c.Capabilities.Extensions {
		if x.URI != sealed.URI {
			continue
		}
		for _, r := range x.Params.Recipients {
			rc, err := age.ParseX25519Recipient(r)
			if err != nil {
				t.Fatal(err)
			}
			recipients, kids = append(recipients, rc), append(kids, tunnel.KeyID(r))
		}
	}
	slices.Sort(kids)
	job, _ := age.GenerateX25519Identity()
	rk, _ := sealed.NewRK()
	msgID, ctxID := uuid.NewString(), uuid.NewString()
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, Service: service, IAT: time.Now().Unix(), ContextID: ctxID,
		MessageID: msgID, Part: 0, Parts: 1, Kind: sealed.KindText, MediaType: "text/plain", Kids: kids,
		ReplyTo: job.Recipient().String(), RK: sealed.EncodeRK(rk)}
	ct, err := sealed.Seal(recipients, hd, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	part, _ := sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: "text/plain"})
	msg, _ := json.Marshal(map[string]any{"messageId": msgID, "contextId": ctxID, "role": "ROLE_USER", "parts": []json.RawMessage{part},
		"metadata": map[string]string{sealed.MetadataReplyTo: job.Recipient().String()}})
	return msg
}

// askExec asks which one, then completes with the follow-up's text.
type askExec struct{}

func (askExec) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := ec.Message.Parts[0].Text()
		if ec.StoredTask != nil {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+text))), nil)
			return
		}
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec,
			a2a.NewTextPart("which one of "+text+"?"))), nil)
	}
}

func (askExec) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// serveAsker serves askExec on 127.0.0.1 like serveHelloExec.
func serveAsker(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, agentCard)
	})
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(askExec{})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}
