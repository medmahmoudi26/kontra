package sealing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// D221 (ext-sealed.md §3, §5, §6): every header names the hired service as the gate's URL path names it. The hosting
// node refuses a request whose service is not the one the preamble names, as its welcome named it ("acme/echo" on a
// multi-tenant gate, "echo" otherwise), or that names none (wrong_job), and seals its replies with that service.
func TestHostingChecksTheService(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const task, ctx = "0193c0de-0000-7000-8000-0000000002a1", "ctx-2a1"
	sealedFor := func(service, msg string) []byte {
		hd := f.reqHeader(b, "", ctx, msg, 0, 1)
		hd.Service = service
		return send(msg, "", "", b.replyTo(), sealedPartJSON(f.seal(hd, "MARKER-"+msg)))
	}
	f.h.SetServicePaths(map[string]string{"echo": "acme/echo"})
	for name, body := range map[string][]byte{
		"no service":                    sealedFor("", "m0"),
		"the service's name alone":      sealedFor("echo", "m1"),
		"another tenant's service":      sealedFor("other/echo", "m2"),
		"another service of the tenant": sealedFor("acme/other", "m3"),
		"a service named like the path": sealedFor("/agents/acme/echo", "m4"),
		"a case variant of the service": sealedFor("ACME/echo", "m5"),
	} {
		if fwd, _, r := f.request(pre(task, ctx), body); r != sealed.RejectWrongJob || bytes.Contains(fwd, []byte("MARKER")) {
			t.Errorf("%s: %q, forwarded %s", name, r, fwd)
		}
	}
	_, x, r := f.request(pre(task, ctx), sealedFor("acme/echo", "m6"))
	if r != "" {
		t.Fatalf("the hired service: %q", r)
	}
	out := readAll(t, f.answer(x, 200, taskAnswer("up-1", "upctx-1", "TASK_STATE_COMPLETED", "the answer", "")))
	n := 0
	w := a2awire.Walker{Binding: a2awire.BindingJSONRPC, Container: func(c *a2awire.Container) error {
		for _, p := range c.Parts {
			sp, ok, err := sealed.ParseDeliveredPart(a2awire.Encode(p))
			if !ok || err != nil {
				continue
			}
			hd, _, err := sealed.Open([]age.Identity{b.key}, sp.Ciphertext)
			if err != nil || hd.Service != "acme/echo" {
				t.Errorf("a reply part's header: %+v %v", hd, err)
			}
			n++
		}
		return nil
	}}
	if _, _, err := w.WalkReply([]byte(out)); err != nil || n == 0 {
		t.Fatalf("the reply: %d sealed parts, %v: %s", n, err, out)
	}

	// MCP: a sealed tools/call, and an answer to an input request, name the service too.
	s := sessionHash("gate-session-svc")
	hd := f.mcpHeader(b, s, "echo", "4")
	hd.Service = "other"
	call := []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":{"sh.kontra/sealed":` +
		envelope(f.seal(hd, `{"text":"MARKER"}`)) + `}}}`)
	if out, r := fromClient(t, f.h.MCP(mcpPre(s, "4", "")), call); r != sealed.RejectWrongJob {
		t.Fatalf("a sealed tools/call naming another service: %q %s", r, out)
	}
	if _, r := fromClient(t, f.h.MCP(mcpPre(s, "4", "")), f.sealedCallBody(b, s, "echo", "4", `{"text":"x"}`, false)); r != "" {
		t.Fatalf("a sealed tools/call naming its service: %q", r)
	}
}

// D221: the hiring node seals every request with the service it hires (the path's {svc} without /agents/ or /mcp/),
// and refuses a reply that names another: the gate delivered the buyer's request to another service of the same node,
// which opened it and answered with a valid auth. A2A: the part is dropped and the local client gets a failed status
// with sealed_rejected; MCP: a sealed_rejected error, nothing of the result.
func TestHiringChecksTheService(t *testing.T) {
	f := newFixture(t)
	hr := newHirer(t, f, pins.Options{})
	path, body := localSend("jsonrpc", "MARKER-request")
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	sreq, job, err := hr.h.Request(context.Background(), "/agents/echo", req)
	if err != nil {
		t.Fatal(err)
	}
	fwd, _ := io.ReadAll(sreq.Body)
	var reqHdr sealed.Header
	w := a2awire.Walker{Binding: a2awire.BindingJSONRPC, Container: func(c *a2awire.Container) error {
		for _, p := range c.Parts {
			if sp, ok, err := sealed.ParseDeliveredPart(a2awire.Encode(p)); ok && err == nil {
				if reqHdr, _, err = sealed.Open([]age.Identity{f.node}, sp.Ciphertext); err != nil {
					t.Fatal(err)
				}
			}
		}
		return nil
	}}
	if _, _, err := w.WalkRequest(fwd); err != nil || reqHdr.Service != "echo" {
		t.Fatalf("the request header names %q (%v), want echo", reqHdr.Service, err)
	}
	reply := func(service string) string {
		rcpt, _ := age.ParseX25519Recipient(reqHdr.ReplyTo)
		rk, _ := sealed.DecodeRK(reqHdr.RK)
		hd := sealed.Header{V: 1, Dir: sealed.DirReply, Service: service, TaskID: "0193c0de-0000-7000-8000-0000000002b1",
			ContextID: reqHdr.ContextID, MessageID: "s1", Part: 0, Parts: 1, Kind: sealed.KindText,
			Kids: []string{tunnel.KeyID(rcpt.String())}}
		hd.Auth, _ = sealed.Auth(rk, hd, []byte("MARKER-reply of "+service))
		ct, err := sealed.Seal([]age.Recipient{rcpt}, hd, []byte("MARKER-reply of "+service))
		if err != nil {
			t.Fatal(err)
		}
		part := sealedPartJSON(ct)
		return `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"0193c0de-0000-7000-8000-0000000002b1","contextId":"` + reqHdr.ContextID +
			`","status":{"state":"TASK_STATE_WORKING","message":{"messageId":"s1","role":"ROLE_AGENT","parts":[` + part + `]}}}}}`
	}
	resp, err := hr.h.Response(context.Background(), job, &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply("other")))})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp); strings.Contains(got, "MARKER-reply") || !strings.Contains(got, "sealed_rejected") {
		t.Fatalf("a reply naming another service reaches the local client: %s", got)
	}

	// MCP: a sealed call's result naming another service, or none.
	fm, hr2, sid := sealedMCPSession(t)
	for i, service := range []string{"other", ""} {
		id := strconv.Itoa(3 + i)
		sent, mjob := mcpLocal(t, hr2, sid, `{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":{"name":"echo","arguments":{"m":"MARKER"}}}`)
		callHdr := openCall(t, fm, sent)
		if callHdr.Service != "fs" {
			t.Fatalf("the sealed call's header names %q, want fs", callHdr.Service)
		}
		rcpt, _ := age.ParseX25519Recipient(callHdr.ReplyTo)
		rk, _ := sealed.DecodeRK(callHdr.RK)
		hd := sealed.Header{V: 1, Dir: sealed.DirReply, Service: service, Session: callHdr.Session, Tool: "echo", MessageID: id,
			Part: 0, Parts: 1, Kind: sealed.KindData, Kids: []string{tunnel.KeyID(rcpt.String())}}
		body := []byte(`{"content":[{"type":"text","text":"MARKER-result"}]}`)
		hd.Auth, _ = sealed.Auth(rk, hd, body)
		ct, err := sealed.Seal([]age.Recipient{rcpt}, hd, body)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := sealed.MCPResult(ct, false)
		got := mcpAnswer(t, hr2, mjob, `{"jsonrpc":"2.0","id":`+id+`,"result":`+string(res)+`}`, false, nil)
		if strings.Contains(got, "MARKER-result") || !strings.Contains(got, `"sh.kontra/failure":"sealed_rejected"`) {
			t.Fatalf("a result naming service %q reaches the local client: %s", service, got)
		}
	}
}

// M3 review robustness-4 (D225): each hosted service's state has a lock of its own, so a service whose commit holds
// its lock (an append and fsync of its service.log, its job files' writes) never holds up another service's
// exchanges; and the node-wide replay checks (D221) have a lock of their own, so they still see what the held
// service recorded without taking its lock.
func TestHostingServicesLockedApart(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const ctx = "ctx-locks"
	first := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "for echo")))
	_, x, r := f.request(pre("0193c0de-0000-7000-8000-0000000003a1", ctx), first)
	if r != "" {
		t.Fatal(r)
	}
	readAll(t, f.answer(x, 200, taskAnswer("up-1", "upctx-1", "TASK_STATE_WORKING", "ok", "")))

	hs := f.h.lock("echo") // as if echo's commit were appending and syncing
	request := func(service, msg, replyToOf string) <-chan sealed.Reject {
		out := make(chan sealed.Reject, 1)
		hd := f.reqHeader(b, "", ctx, msg, 0, 1)
		hd.Service = service
		body := send(msg, "", "", replyToOf, sealedPartJSON(f.seal(hd, "for "+service)))
		p := pre("0193c0de-0000-7000-8000-0000000003a2", ctx)
		p.Service = service
		go func() {
			_, x, err := f.h.Request(context.Background(), p, post(body))
			reason, _ := sealed.ReasonOf(err)
			if err == nil {
				x.Release()
			}
			out <- reason
		}()
		return out
	}
	// Another service's first message, sealed for it, with another buyer's key: served while echo's lock is held.
	other := newBuyer(t)
	hd := f.reqHeader(other, "", ctx, "m2", 0, 1)
	hd.Service = "other"
	p := pre("0193c0de-0000-7000-8000-0000000003a3", ctx)
	p.Service = "other"
	done := make(chan sealed.Reject, 1)
	go func() {
		_, x, err := f.h.Request(context.Background(), p, post(send("m2", "", "", other.replyTo(), sealedPartJSON(f.seal(hd, "for other")))))
		reason, _ := sealed.ReasonOf(err)
		if err == nil {
			x.Release()
		}
		done <- reason
	}()
	select {
	case r := <-done:
		if r != "" {
			t.Fatalf("another service's first message: %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another service's exchange waits for echo's lock")
	}
	// The node-wide check sees the replyTo echo recorded without echo's lock: sealed for "other", it is replayed.
	select {
	case r := <-request("other", "m3", b.replyTo()):
		if r != sealed.RejectReplayed {
			t.Fatalf("a replyTo echo recorded, sealed for another service, while echo's lock is held: %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node-wide replay check waits for echo's lock")
	}
	hs.mu.Unlock()
}

// The node-wide replay check and its reservation are one step (D221): of two first messages with one replyTo, sealed
// for two services of the node and in flight at once, one is forwarded and the other replayed, never both.
func TestHostingNodeWideReplyToIsClaimedOnce(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		b := newBuyer(t)
		ctx := fmt.Sprintf("ctx-race-%d", i)
		results := make(chan sealed.Reject, 2)
		var xs [2]*Exchange
		var wg sync.WaitGroup
		for k, service := range []string{"echo", "other"} {
			hd := f.reqHeader(b, "", ctx, "m", 0, 1)
			hd.Service = service
			body := send("m", "", "", b.replyTo(), sealedPartJSON(f.seal(hd, "x")))
			p := pre(fmt.Sprintf("0193c0de-0000-7000-8000-%012d", 4000+2*i+k), ctx)
			p.Service = service
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, x, err := f.h.Request(context.Background(), p, post(body))
				reason, _ := sealed.ReasonOf(err)
				if err == nil {
					xs[k] = x
				}
				results <- reason
			}()
		}
		wg.Wait()
		close(results)
		var ok, replayed int
		for r := range results {
			switch r {
			case "":
				ok++
			case sealed.RejectReplayed:
				replayed++
			}
		}
		if ok != 1 || replayed != 1 {
			t.Fatalf("round %d: %d forwarded, %d replayed; want one of each", i, ok, replayed)
		}
		for _, x := range xs {
			x.Release()
		}
	}
}
