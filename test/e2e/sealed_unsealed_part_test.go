package e2e

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// sealed_unsealed_part (ext-sealed.md §5 "Gate", §9): a required sealed service (strict), an optional one (loose) and
// a required sealed MCP service (smcp), hosted by a FakeNode that records every exchange stream the gate opens. A
// stock a2a-go client calling the gate directly with an unsealed text part, over JSON-RPC and HTTP+JSON, and a part
// claiming the envelope's media type in url form, get InvalidRequest (-32600, HTTP 400) before dispatch: no tasks
// row, no work item, no stream to the node. An MCP tools/call without _meta["sh.kontra/sealed"] gets -32600 and
// reaches no upstream. The same unsealed message to the optional service runs, and the stored part carries
// metadata kontra.sealed = false.
func TestSealedUnsealedPart(t *testing.T) {
	h := start(t, Options{
		Tunnel: map[string]TunnelService{"strict": {Sealed: "true"}, "loose": {Sealed: "{required: false}"}},
		MCP:    map[string]MCPService{"smcp": {Sealed: "true"}},
	})
	ctx := ctxFor(t, 10*time.Minute)
	seller, err := h.StartEchoSeller()
	if err != nil {
		t.Fatal(err)
	}
	up := mcptest.New()
	up.Tools = []json.RawMessage{dualEraTool("echo", "")}
	dbl := h.Double(up)
	fake, _ := startFakeNode(t, ctx, h, "host", map[string]FakeService{
		"strict": {Kind: tunnel.KindA2A, Target: seller.URL},
		"loose":  {Kind: tunnel.KindA2A, Target: seller.URL},
		"smcp":   {Kind: tunnel.KindMCP, Target: dbl},
	})
	waitSealedCard(t, ctx, h, "strict")
	waitSealedCard(t, ctx, h, "loose")
	g := h.Gates[0]
	streams, sellerReqs := preamblesFor(fake, "strict"), seller.Requests()
	items := scalar[int](t, h, `select count(*) from kontra.work_items`)

	// A stock a2a-go client, JSON-RPC and HTTP+JSON: InvalidRequest.
	for _, binding := range []Binding{JSONRPC, HTTPJSON} {
		c := h.BuyerA().client(t, g, "strict", binding)
		_, err := c.SendMessage(ctx, newMessage("plain to strict over "+binding.String()))
		if !errors.Is(err, a2a.ErrInvalidRequest) {
			t.Fatalf("an unsealed part over %s: %v, want InvalidRequest", binding, err)
		}
	}
	// The same as raw requests: -32600 in JSON-RPC's HTTP 200, and HTTP+JSON's 400 (spec §2.4: a JSON-RPC error is
	// the body of a 200).
	msg := func(part map[string]any) map[string]any {
		return map[string]any{"message": map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{part}}}
	}
	refused := func(svc, what string, part map[string]any) {
		t.Helper()
		if r, err := h.BuyerA().RPC(ctx, g, svc, "SendMessage", msg(part)); err != nil || r.HTTPStatus != http.StatusOK ||
			r.Error == nil || r.Error.Code != -32600 {
			t.Fatalf("%s: %s as raw JSON-RPC: %v %+v", svc, what, err, r)
		}
		if r, err := h.BuyerA().REST(ctx, g, svc, http.MethodPost, "/message:send", msg(part)); err != nil ||
			r.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("%s: %s over HTTP+JSON: %v %+v", svc, what, err, r)
		}
	}
	refused("strict", "an unsealed text part", map[string]any{"text": "plain"})
	// A part claiming the envelope type in url form: refused on either service.
	for _, svc := range []string{"strict", "loose"} {
		refused(svc, "a url-form envelope part", map[string]any{"url": "https://example.invalid/sealed", "mediaType": sealed.MediaType})
	}
	if n := scalar[int](t, h, `select count(*) from kontra.tasks where service = 'strict'`); n != 0 {
		t.Fatalf("%d tasks rows for the refused messages", n)
	}
	if n := scalar[int](t, h, `select count(*) from kontra.work_items`); n != items {
		t.Fatalf("%d work items for the refused messages", n-items)
	}
	if n := preamblesFor(fake, "strict"); n != streams {
		t.Fatalf("the gate opened %d streams to the node for the refused messages", n-streams)
	}

	// MCP: a tools/call without the envelope.
	mc := NewMCPClient(g.URL+"/mcp/smcp", h.APIKey)
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	mstreams := preamblesFor(fake, "smcp")
	r, err := mc.Call(ctx, 1, "echo", map[string]any{"text": "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _, ok := r.Error(); !ok || code != -32600 {
		t.Fatalf("an unsealed MCP call: %d %s", r.Status, r.Text())
	}
	if up.Calls("echo") != 0 || preamblesFor(fake, "smcp") != mstreams {
		t.Fatalf("the unsealed MCP call reached the upstream (%d calls, %d streams)", up.Calls("echo"), preamblesFor(fake, "smcp")-mstreams)
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "json") {
		t.Errorf("the refusal's content type %q", r.Header.Get("Content-Type"))
	}

	// The optional service runs the same unsealed message, marked kontra.sealed = false.
	res, err := h.BuyerA().Client(t, g, "loose").SendMessage(ctx, newMessage("plain to loose"))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the optional service: %#v", res)
	}
	if seller.Requests() == sellerReqs {
		t.Fatal("the optional service's task did not reach the seller")
	}
	marked := false
	for _, ev := range texts(t, h, `select payload::text from kontra.task_events where task_id = $1`, string(task.ID)) {
		marked = marked || strings.Contains(ev, `"kontra.sealed":false`)
	}
	if !marked {
		t.Fatal("the optional service's stored part carries no metadata kontra.sealed = false")
	}
}
