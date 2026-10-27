package sealing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

const testGate = "https://gate.example:8443"

// testLogger writes the package's log lines to the test's log.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// extensionFor is a sealed service's extension object (ext-sealed.md §1) for recipients.
func extensionFor(required bool, recipients ...age.Recipient) json.RawMessage {
	var rs []string
	for _, r := range recipients {
		rs = append(rs, r.(*age.X25519Recipient).String())
	}
	if rs == nil {
		rs = []string{}
	}
	b, _ := json.Marshal(map[string]any{"uri": sealed.URI, "required": false,
		"params": map[string]any{"recipients": rs, "envelope": sealed.MediaType, "required": required}})
	return b
}

func cardWith(ext json.RawMessage) []byte {
	caps := map[string]any{"streaming": true}
	if ext != nil {
		caps["extensions"] = []json.RawMessage{ext}
	}
	b, _ := json.Marshal(map[string]any{"name": "echo", "capabilities": caps})
	return b
}

// hirer is a hiring node's sealing with the card the test serves.
type hirer struct {
	t    *testing.T
	home string
	h    *Hiring
	out  *bytes.Buffer
	card *Card
	opts pins.Options
	now  time.Time
}

func newHirer(t *testing.T, f *fixture, opts pins.Options) *hirer {
	t.Helper()
	hr := &hirer{t: t, home: t.TempDir(), out: &bytes.Buffer{}, opts: opts, now: f.now,
		card: &Card{Body: cardWith(extensionFor(true, f.node.Recipient()))}}
	hr.open()
	return hr
}

func (hr *hirer) open() {
	hr.t.Helper()
	jobs, err := nodekey.OpenJobKeys(hr.home, "gate.example:8443")
	if err != nil {
		hr.t.Fatal(err)
	}
	ps, err := pins.Open(filepath.Join(hr.home, nodekey.DirName, pins.FileName))
	if err != nil {
		hr.t.Fatal(err)
	}
	hr.h, err = NewHiring(HiringConfig{Jobs: jobs, Pins: ps, Opts: hr.opts, Gate: testGate, Out: hr.out,
		Card: func(context.Context, string) (*Card, error) {
			if hr.card == nil {
				return nil, errors.New("the gate answered HTTP 503")
			}
			return hr.card, nil
		}, Now: func() time.Time { return hr.now }}, testLogger(hr.t))
	if err != nil {
		hr.t.Fatal(err)
	}
}

// fakeGate relays between a hiring node's sealing and a hosting node's, as the gate does for A2A: a new task gets a
// gate task id, its first message goes upstream without contextId, and the upstream's ids are rewritten to the gate's
// in every answer. It records every byte that crossed it.
type fakeGate struct {
	t       *testing.T
	f       *fixture
	hr      *hirer
	seen    bytes.Buffer
	task    string
	ctx     string
	service func(msgBody []byte) string // the echo service's answer body for a forwarded request
	// label, when set, is the Content-Type the gate delivers the answer with ("" none).
	label *string
}

func newFakeGate(t *testing.T) *fakeGate {
	f := newFixture(t)
	g := &fakeGate{t: t, f: f}
	g.hr = newHirer(t, f, pins.Options{})
	return g
}

// rewrite turns the upstream's ids into the gate's (spec/relay.md §4).
func (g *fakeGate) rewrite(s string) string {
	return strings.NewReplacer(`"up-task"`, `"`+g.task+`"`, `"up-ctx"`, `"`+g.ctx+`"`).Replace(s)
}

// do runs one local request through the hiring node, the gate, the hosting node and the service, and returns what
// the local client receives.
func (g *fakeGate) do(path string, body []byte, sse bool) (*http.Response, string) {
	t := g.t
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	sreq, job, err := g.hr.h.Request(context.Background(), "/agents/echo", req)
	if err != nil {
		t.Fatalf("hiring Request: %v", err)
	}
	fwd, _ := io.ReadAll(sreq.Body)
	g.seen.Write(fwd)
	// The gate: a new task's ids, the first message without contextId.
	var top map[string]json.RawMessage
	_ = json.Unmarshal(fwd, &top)
	msgOf := func() map[string]any {
		var m map[string]any
		if raw, ok := top["params"]; ok {
			var p map[string]json.RawMessage
			_ = json.Unmarshal(raw, &p)
			_ = json.Unmarshal(p["message"], &m)
		} else {
			_ = json.Unmarshal(top["message"], &m)
		}
		return m
	}
	m := msgOf()
	if tid, _ := m["taskId"].(string); tid == "" {
		g.task = uuid.NewString()
		g.ctx, _ = m["contextId"].(string)
		delete(m, "contextId")
	} else {
		m["taskId"], m["contextId"] = "up-task", "up-ctx"
	}
	var up []byte
	if _, ok := top["params"]; ok {
		up, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": map[string]any{"message": m}})
		if !sse {
			up, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": m}})
		}
	} else {
		up, _ = json.Marshal(map[string]any{"message": m})
	}
	hreq := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(up))
	plainReq, x, err := g.f.h.Request(context.Background(), pre(g.task, g.ctx), hreq)
	if err != nil {
		t.Fatalf("hosting Request: %v", err)
	}
	plain, _ := io.ReadAll(plainReq.Body)
	ct := "application/json"
	if sse {
		ct = "text/event-stream"
	}
	svcResp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(g.service(plain)))}
	sealedResp, err := g.f.h.Response(context.Background(), x, svcResp)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := io.ReadAll(sealedResp.Body)
	gateBody := g.rewrite(string(sb))
	g.seen.WriteString(gateBody)
	resp := &http.Response{StatusCode: sealedResp.StatusCode, Header: sealedResp.Header.Clone(), Request: req,
		Body: io.NopCloser(strings.NewReader(gateBody))}
	resp.Header.Del("Content-Length")
	if g.label != nil {
		resp.Header.Del("Content-Type")
		if *g.label != "" {
			resp.Header.Set("Content-Type", *g.label)
		}
	}
	out, err := g.hr.h.Response(context.Background(), job, resp)
	if err != nil {
		t.Fatalf("hiring Response: %v", err)
	}
	b, _ := io.ReadAll(out.Body)
	return out, string(b)
}

// echoTask answers with a completed task whose status message and artifact echo the request's text parts.
func echoTask(binding string) func(body []byte) string {
	return func(body []byte) string {
		var texts []string
		w := a2awire.Walker{Container: func(c *a2awire.Container) error {
			for _, p := range c.Parts {
				if s, ok := a2awire.String(p, "text"); ok {
					texts = append(texts, s)
				}
			}
			return nil
		}}
		_, _, _ = w.WalkRequest(body)
		echo := "echo: " + strings.Join(texts, " | ")
		task := map[string]any{"id": "up-task", "contextId": "up-ctx",
			"status": map[string]any{"state": "TASK_STATE_COMPLETED", "message": map[string]any{"messageId": "s1", "role": "ROLE_AGENT",
				"taskId": "up-task", "contextId": "up-ctx", "parts": []any{map[string]any{"text": echo}}}},
			"artifacts": []any{map[string]any{"artifactId": "out", "parts": []any{map[string]any{"text": echo, "mediaType": "text/plain"}}}}}
		if binding == "rest" {
			b, _ := json.Marshal(map[string]any{"task": task})
			return string(b)
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"task": task}})
		return string(b)
	}
}

func localSend(binding, text string) (string, []byte) {
	msg := map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}}
	if binding == "rest" {
		b, _ := json.Marshal(map[string]any{"message": msg})
		return "/agents/echo/message:send", b
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": msg}})
	return "/agents/echo", b
}

// A sealed SendMessage through a gate, both bindings: the local client gets plaintext, and nothing the gate saw holds
// it (sealed_no_plaintext's A2A case at package level); the job's key is renamed to the gate task id and deleted when
// the task is terminal.
func TestFakeGateA2A(t *testing.T) {
	for _, binding := range []string{"jsonrpc", "rest"} {
		t.Run(binding, func(t *testing.T) {
			g := newFakeGate(t)
			g.service = echoTask(binding)
			path, body := localSend(binding, "MARKER-7f3a")
			resp, out := g.do(path, body, false)
			if resp.StatusCode != 200 || strings.Count(out, "echo: MARKER-7f3a") != 2 {
				t.Fatalf("the local client gets %d %s", resp.StatusCode, out)
			}
			if strings.Contains(g.seen.String(), "MARKER-7f3a") {
				t.Fatalf("the gate saw the plaintext: %s", g.seen.String())
			}
			if !strings.Contains(g.seen.String(), sealed.MediaType) {
				t.Fatal("the gate saw no envelope")
			}
			if strings.Contains(out, sealed.MediaType) {
				t.Errorf("the local client gets an envelope: %s", out)
			}
			// Terminal: the job's key is gone (spec/tunnel.md §2.3).
			if _, err := g.hr.h.cfg.Jobs.Load(g.task); err == nil {
				t.Error("the per-job key is kept after the task's end")
			}
		})
	}
}

// Streaming: artifact chunks arrive in order and open; a gate that reorders them makes the local client get a failed
// status with sealed_rejected, and the out-of-order chunk is dropped.
func TestFakeGateA2AStreamChunks(t *testing.T) {
	stream := func(reorder bool) (string, *fakeGate) {
		g := newFakeGate(t)
		g.service = func(body []byte) string {
			var sb strings.Builder
			for i, text := range []string{"c0", "c1", "c2"} {
				ev, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"artifactUpdate": map[string]any{
					"taskId": "up-task", "contextId": "up-ctx", "append": i > 0,
					"artifact": map[string]any{"artifactId": "art", "parts": []any{map[string]any{"text": "CHUNK-" + text}}}}}})
				sb.WriteString("data: " + string(ev) + "\n\n")
			}
			return sb.String()
		}
		msg := map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "go"}}}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": map[string]any{"message": msg}})
		if reorder {
			orig := g.service
			g.service = func(b []byte) string {
				evs := strings.Split(strings.TrimSpace(orig(b)), "\n\n")
				return evs[0] + "\n\n" + evs[2] + "\n\n" + evs[1] + "\n\n"
			}
		}
		_, out := g.do("/agents/echo", body, true)
		return out, g
	}
	out, g := stream(false)
	if strings.Count(out, "CHUNK-") != 3 || strings.Index(out, "CHUNK-c0") > strings.Index(out, "CHUNK-c1") ||
		strings.Index(out, "CHUNK-c1") > strings.Index(out, "CHUNK-c2") || strings.Contains(g.seen.String(), "CHUNK-") {
		t.Fatalf("in order: %s", out)
	}
	// The gate reorders (here, the stream the hosting node sealed in order is delivered c0, c2, c1): the hosting node
	// sealed c2 as chunk 1 and c1 as chunk 2, so the reorder must be made on sealed events.
	g2 := newFakeGate(t)
	g2.service = g.service
	var sealedEvents []string
	{
		msg := map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": "go"}}}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": map[string]any{"message": msg}})
		req := httptest.NewRequest(http.MethodPost, "/agents/echo", bytes.NewReader(body))
		sreq, job, err := g2.hr.h.Request(context.Background(), "/agents/echo", req)
		if err != nil {
			t.Fatal(err)
		}
		fwd, _ := io.ReadAll(sreq.Body)
		var top struct {
			Params struct {
				Message map[string]any `json:"message"`
			} `json:"params"`
		}
		_ = json.Unmarshal(fwd, &top)
		g2.task, g2.ctx = uuid.NewString(), top.Params.Message["contextId"].(string)
		delete(top.Params.Message, "contextId")
		up, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": map[string]any{"message": top.Params.Message}})
		_, x, err := g2.f.h.Request(context.Background(), pre(g2.task, g2.ctx), httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(up)))
		if err != nil {
			t.Fatal(err)
		}
		sr, err := g2.f.h.Response(context.Background(), x, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(g.service(nil)))})
		if err != nil {
			t.Fatal(err)
		}
		all, _ := io.ReadAll(sr.Body)
		sealedEvents = strings.Split(strings.TrimSpace(g2.rewrite(string(all))), "\n\n")
		reordered := sealedEvents[0] + "\n\n" + sealedEvents[2] + "\n\n" + sealedEvents[1] + "\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: req,
			Body: io.NopCloser(strings.NewReader(reordered))}
		lr, err := g2.hr.h.Response(context.Background(), job, resp)
		if err != nil {
			t.Fatal(err)
		}
		local, _ := io.ReadAll(lr.Body)
		got := string(local)
		if !strings.Contains(got, "CHUNK-c0") || strings.Contains(got, "CHUNK-c2") || strings.Contains(got, "CHUNK-c1") ||
			!strings.Contains(got, `"kontra.failure":"sealed_rejected"`) || !strings.Contains(got, "TASK_STATE_FAILED") {
			t.Fatalf("a reordered stream: %s", got)
		}
	}
}

// Rule 4a: in a sealed job an unsealed part is refused (the client gets a failed status), except the single text part
// of a terminal status message the gate wrote, which is delivered with metadata["kontra.sealed"] = false.
func TestHiringRule4a(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   string
		wantText string
		failed   bool
	}{
		{"the gate's terminal failure text", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"@T@","contextId":"@C@","status":{"state":"TASK_STATE_FAILED",` +
			`"message":{"messageId":"g1","role":"ROLE_AGENT","taskId":"@T@","contextId":"@C@","parts":[{"text":"the connection to the service was lost"}],` +
			`"metadata":{"kontra.failure":"tunnel_lost"}}},"metadata":{"kontra.failure":"tunnel_lost"}}}}`, "the connection to the service was lost", false},
		{"the gate's text for an upstream end without a message", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"@T@","contextId":"@C@","status":{"state":"TASK_STATE_COMPLETED",` +
			`"message":{"messageId":"g2","role":"ROLE_AGENT","parts":[{"text":"the service ended the task: completed"}]}}}}}`, "the service ended the task: completed", false},
		{"an unsealed artifact", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"@T@","contextId":"@C@","status":{"state":"TASK_STATE_COMPLETED"},` +
			`"artifacts":[{"artifactId":"a","parts":[{"text":"gate's forgery"}]}]}}}`, "", true},
		{"an unsealed status text that is not the gate's", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"@T@","contextId":"@C@","status":{"state":"TASK_STATE_COMPLETED",` +
			`"message":{"messageId":"g1","role":"ROLE_AGENT","parts":[{"text":"your answer is FORTYTWO"}]}}}}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGate(t)
			path, body := localSend("jsonrpc", "hello")
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			sreq, job, err := g.hr.h.Request(context.Background(), "/agents/echo", req)
			if err != nil {
				t.Fatal(err)
			}
			fwd, _ := io.ReadAll(sreq.Body)
			var top struct {
				Params struct {
					Message map[string]any `json:"message"`
				} `json:"params"`
			}
			_ = json.Unmarshal(fwd, &top)
			task, ctx := uuid.NewString(), top.Params.Message["contextId"].(string)
			ans := strings.NewReplacer("@T@", task, "@C@", ctx).Replace(tc.answer)
			out, err := g.hr.h.Response(context.Background(), job, &http.Response{StatusCode: 200, Request: req,
				Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(ans))})
			if err != nil {
				t.Fatal(err)
			}
			got := readAll(t, out)
			if tc.failed {
				if !strings.Contains(got, `"kontra.failure":"sealed_rejected"`) || strings.Contains(got, "forgery") || strings.Contains(got, "FORTYTWO") {
					t.Fatalf("%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantText) || !strings.Contains(got, `"kontra.sealed":false`) || strings.Contains(got, "sealed_rejected") {
				t.Fatalf("%s", got)
			}
		})
	}
}

// Pins (ext-sealed.md §7, D164, D187): a first sight prints the recipient key ids and seller key thumbprints; a card
// that drops the seller's signature after a signed one is refused locally (no stream); a recipient a second
// enrollment added is refused until --accept-change names the service, and the change is printed.
func TestHiringPins(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	hr.card.SellerKeys, hr.card.Signed = []string{"seller-kid-1"}, true
	path, body := localSend("jsonrpc", "x")
	if _, _, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))); err != nil {
		t.Fatal(err)
	}
	first := hr.out.String()
	if !strings.Contains(first, "first seen") || !strings.Contains(first, f.kid()) || !strings.Contains(first, "seller-kid-1") {
		t.Fatalf("the first sight prints %q", first)
	}

	// A card that cannot be read for a service pinned sealed: refused, nothing is sent in plaintext.
	saved := hr.card
	hr.card = nil
	hr.now = hr.now.Add(2 * infoTTL)
	var le *LocalError
	if _, _, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))); !errors.As(err, &le) || le.Status != http.StatusBadGateway {
		t.Fatalf("an unreadable card of a sealed service: %v", err)
	}
	// A service never seen, whose card cannot be read: not known to be sealed, the request goes as it came.
	if r, j, err := hr.h.Request(context.Background(), "/agents/other", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))); err != nil ||
		j == nil || j.info.sealed || readBody(r) != string(body) {
		t.Fatalf("an unknown service without a card: %v", err)
	}
	hr.card = saved

	// Unsigned after signed: refused, nothing sealed or sent.
	hr.out.Reset()
	hr.card = &Card{Body: hr.card.Body}
	hr.now = hr.now.Add(2 * infoTTL)
	_, _, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	var ce *pins.ChangeError
	if !errors.As(err, &ce) || !strings.Contains(hr.out.String(), "unsigned") {
		t.Fatalf("an unsigned card after a signed one: %v; printed %q", err, hr.out.String())
	}

	// A second node enrolled: a new recipient.
	second, _ := age.GenerateX25519Identity()
	hr.card = &Card{Body: cardWith(extensionFor(true, f.node.Recipient(), second.Recipient())), SellerKeys: []string{"seller-kid-1"}, Signed: true}
	hr.out.Reset()
	hr.now = hr.now.Add(2 * infoTTL)
	_, _, err = hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	newKid := tunnel.KeyID(second.Recipient().String())
	if !errors.As(err, &ce) || !strings.Contains(hr.out.String(), "new_recipient") || !strings.Contains(hr.out.String(), newKid) ||
		!strings.Contains(hr.out.String(), "--accept-change echo") {
		t.Fatalf("a new recipient: %v; printed %q", err, hr.out.String())
	}
	// --accept-change echo accepts it, and the change is printed.
	hr.opts = pins.Options{Accept: []string{"echo"}}
	hr.open()
	hr.out.Reset()
	if _, _, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))); err != nil {
		t.Fatalf("accepted: %v", err)
	}
	if !strings.Contains(hr.out.String(), "changed (new_recipient), accepted") || !strings.Contains(hr.out.String(), newKid) {
		t.Fatalf("the accepted change prints %q", hr.out.String())
	}
	// --pin-recipients refuses a further one even when accepted.
	third, _ := age.GenerateX25519Identity()
	hr.card = &Card{Body: cardWith(extensionFor(true, f.node.Recipient(), second.Recipient(), third.Recipient())), SellerKeys: []string{"seller-kid-1"}, Signed: true}
	hr.opts = pins.Options{PinRecipients: true, Accept: []string{"echo"}}
	hr.open()
	if _, _, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))); !errors.As(err, &ce) {
		t.Fatalf("--pin-recipients: %v", err)
	}
}

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// The hiring node's sealed request (rules 2, 3): every part sealed to all recipients, a contextId minted, the header's
// replyTo and rk the job's, and metadata["kontra.sealed.replyTo"] the same replyTo.
func TestHiringSealsRequest(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	msg := map[string]any{"messageId": "m-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "a"}, map[string]any{"data": map[string]any{"k": 1}}}}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": msg}})
	sreq, job, err := hr.h.Request(context.Background(), "/agents/echo", httptest.NewRequest(http.MethodPost, "/agents/echo", bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	fwd, _ := io.ReadAll(sreq.Body)
	var hds []sealed.Header
	var replyMeta, ctx string
	w := a2awire.Walker{Container: func(c *a2awire.Container) error {
		ctx, _ = a2awire.String(c.Members, "contextId")
		md, _ := strictjson.Members(c.Members["metadata"])
		replyMeta, _ = a2awire.String(md, sealed.MetadataReplyTo)
		for _, p := range c.Parts {
			sp, ok, err := sealed.ParsePart(a2awire.Encode(p))
			if !ok || err != nil {
				t.Fatalf("an unsealed part goes to the gate: %s", a2awire.Encode(p))
			}
			hd, _, err := sealed.Open([]age.Identity{f.node}, sp.Ciphertext)
			if err != nil {
				t.Fatal(err)
			}
			hds = append(hds, hd)
		}
		return nil
	}}
	if _, _, err := w.WalkRequest(fwd); err != nil {
		t.Fatal(err)
	}
	if len(hds) != 2 || ctx == "" || job.job == nil {
		t.Fatalf("%d headers, context %q", len(hds), ctx)
	}
	for i, hd := range hds {
		if hd.Dir != sealed.DirRequest || hd.Part != i || hd.Parts != 2 || hd.ContextID != ctx || hd.MessageID != "m-1" ||
			hd.ReplyTo != replyMeta || hd.RK != job.job.RK || hd.Kids[0] != f.kid() || hd.TaskID != "" {
			t.Errorf("header %+v", hd)
		}
	}
	if hds[1].Kind != sealed.KindData || hds[0].Kind != sealed.KindText {
		t.Errorf("kinds %s %s", hds[0].Kind, hds[1].Kind)
	}
}
