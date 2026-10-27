package sealing

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/sealed"
)

// Finding: rules 2 and 5 (and MCP's (session, messageId)) were checked against what the node records only once the
// service has answered, so two identical requests in flight both reached the service. What a request will record is
// held in flight from the node's checks on now: a second copy is replayed while the first is in flight, the hold
// becomes the record when the service accepts the first (a 2xx), and it is dropped, with nothing recorded, when the
// node answers the first itself or the service refuses it, so the gate's retry is not a replay (spec §5).
func TestHostingReplayInFlight(t *testing.T) {
	b := newBuyer(t)
	const ctx = "ctx-r"
	first := func(f *fixture) []byte {
		return send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "pay 100")))
	}
	t1, t2 := "0193c0de-0000-7000-8000-0000000000a1", "0193c0de-0000-7000-8000-0000000000a2"

	// Rule 2: the first message's replyTo is opening a job for t1; under t2 it is replayed.
	f := newFixture(t)
	body := first(f)
	_, x1, r1 := f.request(pre(t1, ctx), body)
	_, _, r2 := f.request(pre(t2, ctx), body)
	if r1 != "" || r2 != sealed.RejectReplayed {
		t.Fatalf("the first message under another gate task while the first is in flight: %q %q", r1, r2)
	}
	// Rule 5: the same message under t1 again, in flight.
	if _, _, r := f.request(pre(t1, ctx), body); r != sealed.RejectReplayed {
		t.Fatalf("the same (messageId, part) in flight: %q", r)
	}
	// The service accepts the first: the hold is the record, and the replays stay refused.
	readAll(t, f.answer(x1, 200, taskAnswer("up-1", "upctx-1", "TASK_STATE_WORKING", "ok", "")))
	if _, _, r := f.request(pre(t2, ctx), body); r != sealed.RejectReplayed {
		t.Fatalf("under another gate task once accepted: %q", r)
	}
	if _, _, r := f.request(pre(t1, ctx), body); r != sealed.RejectReplayed {
		t.Fatalf("the same pair once accepted: %q", r)
	}

	// The service refuses the first (a non-2xx): nothing is recorded, and the retry passes (under another task too).
	f = newFixture(t)
	body = first(f)
	_, x1, _ = f.request(pre(t1, ctx), body)
	readAll(t, f.answer(x1, http.StatusInternalServerError, `{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"boom"}}`))
	_, x1, r1 = f.request(pre(t1, ctx), body)
	if r1 != "" {
		t.Fatalf("the retry after the service refused the first: %q", r1)
	}
	// The node answers it itself (Release, as the proxy's error handler does): the retry passes.
	x1.Release()
	_, x1, r1 = f.request(pre(t1, ctx), body)
	if r1 != "" {
		t.Fatalf("the retry after the node answered the first: %q", r1)
	}
	// A context that ends releases too.
	x1.Release()
	cctx, cancel := context.WithCancel(context.Background())
	if _, _, err := f.h.Request(cctx, pre(t1, ctx), post(body)); err != nil {
		t.Fatal(err)
	}
	cancel()
	for deadline := time.Now().Add(5 * time.Second); ; {
		_, x, r := f.request(pre(t1, ctx), body)
		if r == "" {
			x.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an exchange whose context ended still holds its request: %q", r)
		}
		time.Sleep(time.Millisecond) // the context's AfterFunc runs on its own goroutine
	}
}

// The same, truly concurrent: of many identical sealed first messages (under one gate task, and under several),
// exactly one passes the node's checks.
func TestHostingReplayConcurrent(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	const ctx = "ctx-c"
	body := send("m1", "", "", b.replyTo(), sealedPartJSON(f.seal(f.reqHeader(b, "", ctx, "m1", 0, 1), "pay 100")))
	tasks := []string{"0193c0de-0000-7000-8000-0000000000b1", "0193c0de-0000-7000-8000-0000000000b2", "0193c0de-0000-7000-8000-0000000000b3"}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		passed int
	)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(task string) {
			defer wg.Done()
			_, _, err := f.h.Request(context.Background(), pre(task, ctx), post(body))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				passed++
			} else if r, _ := sealed.ReasonOf(err); r != sealed.RejectReplayed {
				t.Errorf("a concurrent copy refused %v, want replayed", err)
			}
		}(tasks[i%len(tasks)])
	}
	wg.Wait()
	if passed != 1 {
		t.Fatalf("%d identical sealed messages in flight reached the service, want 1", passed)
	}
}

// MCP rule 5 in flight: a sealed tools/call with the same (session, messageId) passes FromClient once while the first
// is in flight; Accepted makes it the record, Release (the node answered, or the server refused) drops it. A sealed
// answer to an input request is held the same way.
func TestHostingMCPReplayInFlight(t *testing.T) {
	f := newFixture(t)
	b := newBuyer(t)
	s1 := sessionHash("gate-session-1")
	body := f.sealedCallBody(b, s1, "transfer", "1", `{"amount":100}`, false)
	x1 := f.h.MCP(mcpPre(s1, "1", ""))
	if _, r := fromClient(t, x1, body); r != "" {
		t.Fatal(r)
	}
	if _, r := fromClient(t, f.h.MCP(mcpPre(s1, "1", "")), body); r != sealed.RejectReplayed {
		t.Fatalf("the same call in flight: %q", r)
	}
	x1.Release() // the node answered it itself
	x2 := f.h.MCP(mcpPre(s1, "1", ""))
	if _, r := fromClient(t, x2, body); r != "" {
		t.Fatalf("the retry after the node answered: %q", r)
	}
	x2.Accepted()
	x2.Release() // after Accepted: nothing
	if _, r := fromClient(t, f.h.MCP(mcpPre(s1, "1", "")), body); r != sealed.RejectReplayed {
		t.Fatalf("the same call once accepted: %q", r)
	}

	// Concurrent copies of one call: exactly one passes.
	other := f.sealedCallBody(b, s1, "transfer", "2", `{"amount":5}`, false)
	om := parse(t, other)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		passed int
	)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := f.h.MCP(mcpPre(s1, "2", ""))
			_, err := x.FromClient(other, om)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				passed++
			}
		}()
	}
	wg.Wait()
	if passed != 1 {
		t.Fatalf("%d identical sealed calls in flight passed, want 1", passed)
	}

	// An input request's sealed answer, twice in flight: the second is replayed; released, it passes again.
	d := newDurable(t)
	hd, _, _ := d.ask(t, "5")
	_ = hd
	ans := d.answer(t, "5", d.hdr("5"), `{"action":"accept","content":{"seat":"1"}}`)
	a1 := d.f.h.MCP(mcpPre(d.session, "1", d.task))
	if _, r := fromClient(t, a1, ans); r != "" {
		t.Fatal(r)
	}
	if _, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), ans); r != sealed.RejectReplayed {
		t.Fatalf("the same answer in flight: %q", r)
	}
	a1.Release()
	a2 := d.f.h.MCP(mcpPre(d.session, "1", d.task))
	if _, r := fromClient(t, a2, ans); r != "" {
		t.Fatalf("the answer after the first was released: %q", r)
	}
	a2.Accepted()
	if _, r := fromClient(t, d.f.h.MCP(mcpPre(d.session, "1", d.task)), ans); r != sealed.RejectReplayed {
		t.Fatalf("the answer once accepted: %q", r)
	}
}
