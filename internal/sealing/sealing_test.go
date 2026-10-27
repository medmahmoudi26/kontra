package sealing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// fixture is a hosting node with one identity key, its job state in a temporary directory and a clock the test moves.
type fixture struct {
	t    *testing.T
	dir  string
	node *age.X25519Identity
	old  *age.X25519Identity // a rotated key that still opens (§12's overlap)
	h    *Hosting
	now  time.Time
}

func newFixture(t *testing.T, required ...string) *fixture {
	t.Helper()
	node, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	old, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), "hosted", "gate.example:8443"), node: node, old: old,
		now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	f.open(required...)
	return f
}

// open (re)opens the hosting side on the fixture's directory: a node restart.
func (f *fixture) open(required ...string) {
	f.t.Helper()
	h, err := NewHosting(HostingConfig{
		Dir:        f.dir,
		Identities: func() []age.Identity { return []age.Identity{f.node, f.old} },
		KeyIDs: func() []string {
			return []string{tunnel.KeyID(f.node.Recipient().String()), tunnel.KeyID(f.old.Recipient().String())}
		},
		MaxAge:   DefaultMaxAge,
		Required: required,
		Now:      func() time.Time { return f.now },
	}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.h = h
}

func (f *fixture) kid() string { return tunnel.KeyID(f.node.Recipient().String()) }

// buyer is a hiring node's job: its per-job key (replyTo) and rk.
type buyer struct {
	key *age.X25519Identity
	rk  []byte
}

func newBuyer(t *testing.T) buyer {
	t.Helper()
	k, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	rk, err := sealed.NewRK()
	if err != nil {
		t.Fatal(err)
	}
	return buyer{key: k, rk: rk}
}

func (b buyer) replyTo() string { return b.key.Recipient().String() }

// reqHeader is an A2A request header of b for part i of n.
func (f *fixture) reqHeader(b buyer, task, ctx, msg string, i, n int) sealed.Header {
	return sealed.Header{V: 1, Dir: sealed.DirRequest, IAT: f.now.Unix(), TaskID: task, ContextID: ctx, MessageID: msg,
		Part: i, Parts: n, Kind: sealed.KindText, Kids: []string{f.kid()}, ReplyTo: b.replyTo(), RK: sealed.EncodeRK(b.rk)}
}

// seal seals body under hd to the node.
func (f *fixture) seal(hd sealed.Header, body string) []byte {
	f.t.Helper()
	ct, err := sealed.Seal([]age.Recipient{f.node.Recipient()}, hd, []byte(body))
	if err != nil {
		f.t.Fatal(err)
	}
	return ct
}

// rawSeal encrypts hdr ‖ 0x00 ‖ body to r as it is (a header Seal would refuse: not canonical, wrong kids).
func rawSeal(t *testing.T, r age.Recipient, hdr []byte, body string) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := age.Encrypt(&out, r)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(hdr)
	w.Write([]byte{0})
	w.Write([]byte(body))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func sealedPartJSON(ct []byte) string {
	p, _ := sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: "text"})
	return string(p)
}

// send is a JSON-RPC SendMessage whose message carries parts, ids and the replyTo metadata.
func send(msgID, ctx, task, replyTo string, parts ...string) []byte {
	msg := map[string]any{"messageId": msgID, "role": "ROLE_USER", "parts": json.RawMessage("[" + strings.Join(parts, ",") + "]")}
	if ctx != "" {
		msg["contextId"] = ctx
	}
	if task != "" {
		msg["taskId"] = task
	}
	if replyTo != "" {
		msg["metadata"] = map[string]string{sealed.MetadataReplyTo: replyTo}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "SendMessage", "params": map[string]any{"message": msg}})
	return b
}

func pre(task, ctx string) tunnel.Preamble {
	return tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: "echo", Kind: tunnel.KindA2A, Task: task, Context: ctx}
}

func post(body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// request runs Request and returns the forwarded body, or the refusal's reason.
func (f *fixture) request(p tunnel.Preamble, body []byte) ([]byte, *Exchange, sealed.Reject) {
	f.t.Helper()
	r, x, err := f.h.Request(context.Background(), p, post(body))
	if err != nil {
		reason, ok := sealed.ReasonOf(err)
		if !ok {
			f.t.Fatalf("Request: %v (not a refusal)", err)
		}
		return nil, x, reason
	}
	out, _ := io.ReadAll(r.Body)
	return out, x, ""
}

// answer runs Response with a JSON body and status.
func (f *fixture) answer(x *Exchange, status int, body string) *http.Response {
	f.t.Helper()
	resp := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body))}
	out, err := f.h.Response(context.Background(), x, resp)
	if err != nil {
		f.t.Fatalf("Response: %v", err)
	}
	return out
}

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// taskAnswer is a JSON-RPC SendMessage result {task} of upstream ids with one status message and one artifact.
func taskAnswer(upTask, upCtx, state, statusText, artifactText string) string {
	t := map[string]any{"id": upTask, "contextId": upCtx,
		"status": map[string]any{"state": state, "message": map[string]any{"messageId": "status-1", "role": "ROLE_AGENT",
			"parts": []any{map[string]any{"text": statusText}}}}}
	if artifactText != "" {
		t["artifacts"] = []any{map[string]any{"artifactId": "a1", "parts": []any{map[string]any{"text": artifactText}}}}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "result": map[string]any{"task": t}})
	return string(b)
}

// opened is what a buyer reads of a sealed reply part.
type openedReply struct {
	hd   sealed.Header
	body string
	raw  string
}

// openReplies opens every sealed part of a reply body with the buyer's key and checks its auth.
func openReplies(t *testing.T, b buyer, body string) []openedReply {
	t.Helper()
	var out []openedReply
	w := a2awire.Walker{Container: func(c *a2awire.Container) error {
		for _, p := range c.Parts {
			sp, ok, err := sealed.ParsePart(a2awire.Encode(p))
			if !ok {
				t.Fatalf("an unsealed part in a sealed reply: %s", a2awire.Encode(p))
			}
			if err != nil {
				t.Fatal(err)
			}
			hd, plain, err := sealed.Open([]age.Identity{b.key}, sp.Ciphertext)
			if err != nil {
				t.Fatalf("a reply part does not open with the buyer's key: %v", err)
			}
			if err := sealed.VerifyAuth(b.rk, hd, plain); err != nil {
				t.Fatalf("auth: %v", err)
			}
			out = append(out, openedReply{hd: hd, body: string(plain), raw: string(p["raw"])})
		}
		return nil
	}}
	if _, _, err := w.WalkReply([]byte(body)); err != nil {
		t.Fatalf("WalkReply(%s): %v", body, err)
	}
	return out
}

// A first sealed message opens, its plaintext reaches the service, and the service's reply comes back sealed to the
// job's replyTo with the gate's ids, auth, and the same ciphertext when the gate polls an unchanged task.
func TestHostingOpensAndSeals(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000001", "ctx-1"
	body := send("m1", "", "", b.replyTo(),
		sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 2), "secret marker one")),
		sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 1, 2), "secret marker two")))
	fwd, x, reason := f.request(pre(task, ctx), body)
	if reason != "" {
		t.Fatalf("refused %s", reason)
	}
	if !strings.Contains(string(fwd), "secret marker one") || !strings.Contains(string(fwd), "secret marker two") ||
		strings.Contains(string(fwd), sealed.MediaType) {
		t.Fatalf("the service does not get the plaintext parts: %s", fwd)
	}
	resp := f.answer(x, http.StatusOK, taskAnswer("up-1", "upctx-1", "TASK_STATE_COMPLETED", "done: secret", "the artifact secret"))
	out := readAll(t, resp)
	if strings.Contains(out, "secret") {
		t.Fatalf("a reply carries plaintext: %s", out)
	}
	parts := openReplies(t, b, out)
	if len(parts) != 2 {
		t.Fatalf("%d sealed reply parts, want 2", len(parts))
	}
	for _, p := range parts {
		if p.hd.Dir != sealed.DirReply || p.hd.TaskID != task || p.hd.ContextID != ctx || p.hd.Kids[0] != tunnel.KeyID(b.replyTo()) {
			t.Errorf("reply header %+v", p.hd)
		}
	}
	if parts[0].hd.ArtifactID != "a1" && parts[1].hd.ArtifactID != "a1" {
		t.Errorf("no artifact header: %+v", parts)
	}

	// The gate polls GetTask on the job's exchange: the unchanged task comes back with the same bytes.
	get, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "GetTask", "params": map[string]any{"id": "up-1"}})
	_, gx, reason := f.request(pre(task, ctx), get)
	if reason != "" {
		t.Fatalf("GetTask refused %s", reason)
	}
	answer := taskAnswer("up-1", "upctx-1", "TASK_STATE_COMPLETED", "done: secret", "the artifact secret")
	answer = strings.Replace(answer, `{"task":`, ``, 1)
	answer = strings.TrimSuffix(answer, "}}") + "}"
	again := openReplies(t, b, readAll(t, f.answer(gx, http.StatusOK, answer)))
	if len(again) != 2 || again[0].raw != parts[0].raw && again[0].raw != parts[1].raw {
		t.Fatalf("polling an unchanged task gives other ciphertext")
	}
	seen := map[string]bool{parts[0].raw: true, parts[1].raw: true}
	for _, p := range again {
		if !seen[p.raw] {
			t.Errorf("a part sealed again with new bytes on an unchanged task")
		}
	}
}

// Each hosting rule produces exactly its reason (spec/ext-sealed.md §5, §6).
func TestHostingRules(t *testing.T) {
	const task, ctx = "0193c0de-0000-7000-8000-000000000002", "ctx-2"
	type tc struct {
		name  string
		setup func(f *fixture, b buyer) // runs before
		body  func(f *fixture, b buyer) []byte
		pre   tunnel.Preamble
		want  sealed.Reject
	}
	stranger, _ := age.GenerateX25519Identity()
	one := func(f *fixture, b buyer, mutate func(*sealed.Header)) []byte {
		hd := f.reqHeader(b, "", ctx, "m1", 0, 1)
		mutate(&hd)
		return send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(hd, "body")))
	}
	opened := func(f *fixture, b buyer, upTask string) {
		// Opens job task with b, accepted by the service, which answered upstream ids up-1 / upctx-1.
		body := send("first", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "first", 0, 1), "x")))
		_, x, reason := f.request(pre(task, ctx), body)
		if reason != "" {
			f.t.Fatalf("setup refused %s", reason)
		}
		readAll(f.t, f.answer(x, 200, taskAnswer(upTask, "upctx-1", "TASK_STATE_INPUT_REQUIRED", "more?", "")))
	}
	cases := []tc{
		{name: "undecryptable", want: sealed.RejectUndecryptable, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			hd := f.reqHeader(b, "", ctx, "m1", 0, 1)
			hd.Kids = []string{tunnel.KeyID(stranger.Recipient().String())}
			ct, err := sealed.Seal([]age.Recipient{stranger.Recipient()}, hd, []byte("x"))
			if err != nil {
				t.Fatal(err)
			}
			return send("m1", "", "", b.replyTo(), sealedPartJSON(ct))
		}},
		{name: "bad_header non-canonical", want: sealed.RejectBadHeader, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			hd := f.reqHeader(b, "", ctx, "m1", 0, 1)
			c, _ := hd.Canonical()
			spaced := bytes.Replace(c, []byte(`,"dir"`), []byte(`, "dir"`), 1)
			return send("m1", "", "", b.replyTo(), sealedPartJSON(rawSeal(t, f.node.Recipient(), spaced, "x")))
		}},
		{name: "bad_header missing parts", want: sealed.RejectBadHeader, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 2), "x")))
		}},
		{name: "bad_header duplicated part index", want: sealed.RejectBadHeader, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			p := sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 2), "x"))
			return send("m1", "", "", b.replyTo(), p, p)
		}},
		{name: "bad_header replyTo metadata differs", want: sealed.RejectBadHeader, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			other := newBuyer(t)
			return send("m1", "", "", other.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x")))
		}},
		{name: "not_a_recipient", want: sealed.RejectNotRecipient, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			hd := f.reqHeader(b, "", ctx, "m1", 0, 1)
			hd.Kids = []string{tunnel.KeyID(stranger.Recipient().String())}
			c, _ := hd.Canonical()
			return send("m1", "", "", b.replyTo(), sealedPartJSON(rawSeal(t, f.node.Recipient(), c, "x")))
		}},
		{name: "replayed iat too old", want: sealed.RejectReplayed, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return one(f, b, func(h *sealed.Header) { h.IAT = f.now.Add(-DefaultMaxAge - time.Minute).Unix() })
		}},
		{name: "replayed iat in the future", want: sealed.RejectReplayed, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return one(f, b, func(h *sealed.Header) { h.IAT = f.now.Add(301 * time.Second).Unix() })
		}},
		{name: "replayed replyTo of another job", want: sealed.RejectReplayed, pre: pre("0193c0de-0000-7000-8000-0000000000ff", ctx),
			setup: func(f *fixture, b buyer) { opened(f, b, "up-1") },
			body: func(f *fixture, b buyer) []byte {
				return send("again", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "again", 0, 1), "x")))
			}},
		{name: "replayed pair", want: sealed.RejectReplayed, pre: pre(task, ctx),
			setup: func(f *fixture, b buyer) { opened(f, b, "up-1") },
			body: func(f *fixture, b buyer) []byte {
				return send("first", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "first", 0, 1), "x")))
			}},
		{name: "wrong_context header", want: sealed.RejectWrongContext, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return one(f, b, func(h *sealed.Header) { h.ContextID = "another" })
		}},
		{name: "wrong_context message", want: sealed.RejectWrongContext, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return send("m1", "upstream-ctx-of-nobody", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x")))
		}},
		{name: "wrong_message", want: sealed.RejectWrongMessage, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return send("m2", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x")))
		}},
		{name: "wrong_task first message naming a task", want: sealed.RejectWrongTask, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return send("m1", ctx, "up-9", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x")))
		}},
		{name: "wrong_task header task is not the preamble's", want: sealed.RejectWrongTask, pre: pre(task, ctx),
			setup: func(f *fixture, b buyer) { opened(f, b, "up-1") },
			body: func(f *fixture, b buyer) []byte {
				return send("m3", "upctx-1", "up-1", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "0193c0de-0000-7000-8000-0000000000aa", ctx, "m3", 0, 1), "x")))
			}},
		{name: "wrong_task follow-up naming another upstream task", want: sealed.RejectWrongTask, pre: pre(task, ctx),
			setup: func(f *fixture, b buyer) { opened(f, b, "up-1") },
			body: func(f *fixture, b buyer) []byte {
				return send("m3", "upctx-1", "up-2", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, task, ctx, "m3", 0, 1), "x")))
			}},
		{name: "wrong_job follow-up of another buyer", want: sealed.RejectWrongJob, pre: pre(task, ctx),
			setup: func(f *fixture, b buyer) { opened(f, b, "up-1") },
			body: func(f *fixture, b buyer) []byte {
				other := newBuyer(t)
				return send("m3", "upctx-1", "up-1", other.replyTo(), sealedPartJSON(f.seal(f.reqHeader(other, task, ctx, "m3", 0, 1), "x")))
			}},
		{name: "wrong_job follow-up of no job", want: sealed.RejectWrongJob, pre: pre(task, ctx), body: func(f *fixture, b buyer) []byte {
			return send("m3", "upctx-1", "up-1", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, task, ctx, "m3", 0, 1), "x")))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			b := newBuyer(t)
			if c.setup != nil {
				c.setup(f, b)
			}
			_, _, reason := f.request(c.pre, c.body(f, b))
			if reason != c.want {
				t.Fatalf("reason %q, want %q", reason, c.want)
			}
		})
	}
}

// A follow-up of the job is accepted, and a rotated key's old line still opens (tunnel.md §12's overlap).
func TestHostingFollowUpAndOldKey(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000003", "ctx-3"
	body := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "hi")))
	_, x, reason := f.request(pre(task, ctx), body)
	if reason != "" {
		t.Fatal(reason)
	}
	readAll(t, f.answer(x, 200, taskAnswer("up-3", "upctx-3", "TASK_STATE_INPUT_REQUIRED", "more?", "")))
	// The follow-up is sealed to the old key (a part sealed before the node rotated).
	hd := f.reqHeader(b, task, ctx, "m2", 0, 1)
	hd.Kids = []string{tunnel.KeyID(f.old.Recipient().String())}
	ct, err := sealed.Seal([]age.Recipient{f.old.Recipient()}, hd, []byte("the follow-up"))
	if err != nil {
		t.Fatal(err)
	}
	fwd, _, reason := f.request(pre(task, ctx), send("m2", "upctx-3", "up-3", b.replyTo(), sealedPartJSON(ct)))
	if reason != "" {
		t.Fatalf("follow-up refused %s", reason)
	}
	if !strings.Contains(string(fwd), "the follow-up") {
		t.Fatalf("forwarded %s", fwd)
	}
}

// A request the node answered itself (a 502, target_unreachable) opens no job and records no pair: the gate's retry of
// it is accepted. Once the service accepted it, the same request is a replay.
func TestHostingNodeAnswerOpensNoJob(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000004", "ctx-4"
	body := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x")))
	_, x, reason := f.request(pre(task, ctx), body)
	if reason != "" {
		t.Fatal(reason)
	}
	f.answer(x, http.StatusBadGateway, "kontra connect: the service did not answer")
	if _, err := os.Stat(filepath.Join(f.dir, "echo", "jobs", task+".json")); err == nil {
		t.Fatal("a job was opened by a request the service never accepted")
	}
	_, x, reason = f.request(pre(task, ctx), body)
	if reason != "" {
		t.Fatalf("the gate's retry refused %s", reason)
	}
	readAll(t, f.answer(x, 200, taskAnswer("up-4", "upctx-4", "TASK_STATE_WORKING", "on it", "")))
	if _, _, reason = f.request(pre(task, ctx), body); reason != sealed.RejectReplayed {
		t.Fatalf("the same request after the service accepted it: %q, want replayed", reason)
	}
}

// --sealed-required refuses an unsealed request (bad_header); without it an unsealed request passes as it came.
func TestHostingSealedRequired(t *testing.T) {
	plain := send("m1", "c", "", "", `{"text":"hello"}`)
	f := newFixture(t, "echo")
	if _, _, reason := f.request(pre("0193c0de-0000-7000-8000-000000000005", "c"), plain); reason != sealed.RejectBadHeader {
		t.Fatalf("an unsealed request under --sealed-required: %q", reason)
	}
	g := newFixture(t)
	fwd, x, reason := g.request(pre("0193c0de-0000-7000-8000-000000000005", "c"), plain)
	if reason != "" || !strings.Contains(string(fwd), "hello") {
		t.Fatalf("an unsealed request: %q %s", reason, fwd)
	}
	out := readAll(t, g.answer(x, 200, taskAnswer("u", "uc", "TASK_STATE_COMPLETED", "plain answer", "")))
	if !strings.Contains(out, "plain answer") {
		t.Fatalf("an unsealed job's reply is sealed: %s", out)
	}
}

// An exchange never reaches a sealed job's upstream task unless it is that job's: a GetTask naming it on another gate
// task is wrong_task, and a reply naming it on an unsealed exchange is refused before anything of it is forwarded.
func TestHostingGuardsSealedJobs(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000006", "ctx-6"
	_, x, _ := f.request(pre(task, ctx), send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x"))))
	readAll(t, f.answer(x, 200, taskAnswer("up-6", "upctx-6", "TASK_STATE_WORKING", "secret status", "")))

	other := "0193c0de-0000-7000-8000-0000000000bb"
	get, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "GetTask", "params": map[string]any{"id": "up-6"}})
	if _, _, reason := f.request(pre(other, "c"), get); reason != sealed.RejectWrongTask {
		t.Fatalf("GetTask of a sealed job's task on another exchange: %q", reason)
	}
	// An unsealed exchange whose reply names the sealed job's task (a service that answers with it).
	_, ux, reason := f.request(pre(other, "c"), send("m9", "c", "", "", `{"text":"plain"}`))
	if reason != "" {
		t.Fatal(reason)
	}
	resp := f.answer(ux, 200, taskAnswer("up-6", "upctx-6", "TASK_STATE_WORKING", "secret status", ""))
	out := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderNode) != string(tunnel.NodeSealedRejected) ||
		strings.Contains(out, "secret status") {
		t.Fatalf("a reply naming a sealed job's task on another exchange: %d %s", resp.StatusCode, out)
	}
}

// The node's refusal is 400 with Kontra-Node: sealed_rejected, Kontra-Sealed-Reject and a -32602 body (§6).
func TestRejectAnswer(t *testing.T) {
	status, h, body := RejectAnswer(json.RawMessage(`7`), reject(sealed.RejectWrongJob, "detail never sent"))
	if status != http.StatusBadRequest || h.Get(tunnel.HeaderNode) != "sealed_rejected" || h.Get(tunnel.HeaderSealedReject) != "wrong_job" {
		t.Fatalf("%d %v", status, h)
	}
	var m struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.ID != 7 || m.Error.Code != -32602 || m.Error.Message != "sealed part rejected: wrong_job" {
		t.Fatalf("body %s", body)
	}
	if bytes.Contains(body, []byte("detail")) {
		t.Fatal("the refusal's detail is sent")
	}
}

// Artifact updates are sealed as chunks 0, 1, 2 of their artifactId; a later snapshot of the artifact reuses the
// ciphertext the gate holds and seals only what grew, as the next chunk.
func TestHostingArtifactChunks(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000007", "ctx-7"
	stream, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage",
		"params": map[string]any{"message": map[string]any{"messageId": "m1", "role": "ROLE_USER",
			"metadata": map[string]string{sealed.MetadataReplyTo: b.replyTo()},
			"parts":    []json.RawMessage{json.RawMessage(sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "go")))}}}})
	_, x, reason := f.request(pre(task, ctx), stream)
	if reason != "" {
		t.Fatal(reason)
	}
	var sse strings.Builder
	for i, text := range []string{"chunk zero", "chunk one", "chunk two"} {
		ev, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"artifactUpdate": map[string]any{
			"taskId": "up-7", "contextId": "upctx-7", "append": i > 0,
			"artifact": map[string]any{"artifactId": "art", "parts": []any{map[string]any{"text": text}}}}}})
		sse.WriteString("data: " + string(ev) + "\n\n")
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse.String()))}
	out, err := f.h.Response(context.Background(), x, resp)
	if err != nil {
		t.Fatal(err)
	}
	all := readAll(t, out)
	if strings.Contains(all, "chunk") {
		t.Fatalf("plaintext in the stream: %s", all)
	}
	var raws []string
	for i, ev := range strings.Split(strings.TrimSpace(all), "\n\n") {
		data := strings.TrimPrefix(ev, "data: ")
		ps := openReplies(t, b, data)
		if len(ps) != 1 || ps[0].hd.Chunk == nil || *ps[0].hd.Chunk != i || ps[0].hd.ArtifactID != "art" {
			t.Fatalf("event %d: %+v", i, ps)
		}
		raws = append(raws, ps[0].raw)
	}
	// The gate polls the task: its artifact holds the three chunks and a fourth part.
	get, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "GetTask", "params": map[string]any{"id": "up-7"}})
	_, gx, reason := f.request(pre(task, ctx), get)
	if reason != "" {
		t.Fatal(reason)
	}
	snap, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"id": "up-7", "contextId": "upctx-7",
		"status": map[string]any{"state": "TASK_STATE_WORKING"}, "artifacts": []any{map[string]any{"artifactId": "art", "parts": []any{
			map[string]any{"text": "chunk zero"}, map[string]any{"text": "chunk one"}, map[string]any{"text": "chunk two"}, map[string]any{"text": "chunk three"}}}}}})
	ps := openReplies(t, b, readAll(t, f.answer(gx, 200, string(snap))))
	if len(ps) != 4 {
		t.Fatalf("%d parts", len(ps))
	}
	for i := 0; i < 3; i++ {
		if ps[i].raw != raws[i] {
			t.Errorf("part %d of the snapshot is sealed again: the gate would log it as changed", i)
		}
	}
	if *ps[3].hd.Chunk != 3 || ps[3].hd.Part != 0 || ps[3].hd.Parts != 1 || ps[3].body != "chunk three" {
		t.Errorf("the grown part: %+v %q", ps[3].hd, ps[3].body)
	}
}

// A service reply that is not an A2A answer with exact keys is refused, and nothing of it is forwarded (D172).
func TestHostingRefusesAmbiguousReply(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000008", "ctx-8"
	_, x, _ := f.request(pre(task, ctx), send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x"))))
	resp := f.answer(x, 200, `{"jsonrpc":"2.0","id":7,"result":{"task":{"id":"u","contextId":"c","status":{"state":"TASK_STATE_COMPLETED",`+
		`"message":{"messageId":"s","role":"ROLE_AGENT","parts":[{"text":"leak","TEXT":"x"}]}}}}}`)
	out := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get(tunnel.HeaderSealedReject) != "bad_header" || strings.Contains(out, "leak") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
}

// Sweep drops a job 7 days after its last exchange, and the replay sets past max_age.
func TestHostingSweep(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-000000000009", "ctx-9"
	_, x, _ := f.request(pre(task, ctx), send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "x"))))
	readAll(t, f.answer(x, 200, taskAnswer("up-9", "upctx-9", "TASK_STATE_COMPLETED", "ok", "")))
	jobFile := filepath.Join(f.dir, "echo", "jobs", task+".json")
	if fi, err := os.Stat(jobFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the job's file: %v %v", fi, err)
	}
	if err := f.h.Sweep(f.now.Add(JobRetention - time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jobFile); err != nil {
		t.Fatal("a job swept before its retention")
	}
	if err := f.h.Sweep(f.now.Add(JobRetention + time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jobFile); err == nil {
		t.Fatal("a job kept past its retention")
	}
	svc, _ := f.h.st.loadService("echo")
	if len(svc.ReplyTo) != 0 || len(svc.Tasks) != 0 {
		t.Fatalf("the service's sets keep a swept job: %+v", svc)
	}
}

// Every header the hosting node seals is canonical and its body is the part's content (JCS for data).
func TestPlainPartCodec(t *testing.T) {
	for _, raw := range []string{`{"text":"t","mediaType":"text/plain"}`, `{"raw":"AAEC","filename":"f.bin","metadata":{"k":1}}`,
		`{"url":"https://x.example/a"}`, `{"data":{"b":1,"a":[1,2]}}`} {
		p, err := strictjson.Members([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		pl, err := plainOfPart(a2awire.Part(p))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		hd := sealed.Header{V: 1, Dir: sealed.DirReply, Part: 0, Parts: 1, Kind: pl.kind, MediaType: pl.mediaType, Kids: []string{"k1_x"}}
		back, err := plainPart(hd, pl.body, sealedPartOf([]byte("ct"), a2awire.Part(p), pl))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := jcs.Transform([]byte(raw))
		got, _ := jcs.Transform(a2awire.Encode(back))
		if !bytes.Equal(want, got) {
			t.Errorf("%s -> %s", want, got)
		}
	}
	for _, bad := range []string{`{"text":"a","raw":"AA=="}`, `{"file":{"uri":"x"}}`, `{"mediaType":"x"}`, `{"text":1}`} {
		p, _ := strictjson.Members([]byte(bad))
		if _, err := plainOfPart(a2awire.Part(p)); err == nil {
			t.Errorf("%s is sealed", bad)
		}
	}
	_ = base64.StdEncoding
	_ = errors.New
}
