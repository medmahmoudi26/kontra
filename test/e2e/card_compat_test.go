package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// getCard GETs a card route with the buyer's key and returns the body.
func getCard(t *testing.T, ctx context.Context, u, key string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d %s", u, resp.StatusCode, body)
	}
	return body
}

// TestCardCompat is card_compat (spec/relay.md §3, §16; D12): the a2a-samples helloworld seller at protocol
// 0.2.5 behind the gate is served as a 1.0 agent. The seller is slowagent's Legacy025 stand-in for the
// Python seller at a2a-samples 08dde55c070f: that seller's card byte for byte, a2a-go's 0.x JSON-RPC
// binding, and its answer, one "Hello World" Message. The gate fetches the 0.2.5 card from .well-known/agent.json (D71), serves it verbatim at
// agent-card.upstream.json, and serves at {svc} a 1.0 card a stock a2a-go client resolves: the gate's own
// interfaces at protocol 1.0, none of the 0.x interface members, and every seller member byte-identical.
// A 1.0 call through the gate reaches the seller as a 0.x message/send and comes back as a completed task
// carrying the seller's message (spec §6.3, D5), and a stream works the same way.
func TestCardCompat(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 1, Interval: 10 * time.Millisecond, Streaming: true},
		Services: map[string]slowagent.Config{"hello025": {Legacy025: true}},
	})
	ctx := ctxFor(t, 3*time.Minute)
	seller := h.Upstreams["hello025"]
	upstreamCard := []byte(fmt.Sprintf(slowagent.Legacy025Card, seller.URL()+"/"))
	publicURL := h.Gates[0].Svc("hello025") // public_url is replica A's origin in the shared kontra.yaml

	for _, g := range h.Gates {
		if got := getCard(t, ctx, g.Svc("hello025")+"/.well-known/agent-card.upstream.json", h.APIKey); string(got) != string(upstreamCard) {
			t.Fatalf("%s: the upstream card is not served verbatim:\n got %s\nwant %s", g.Name, got, upstreamCard)
		}
		for _, path := range []string{"", "/.well-known/agent-card.json"} {
			served := getCard(t, ctx, g.Svc("hello025")+path, h.APIKey)
			var card map[string]json.RawMessage
			if err := json.Unmarshal(served, &card); err != nil {
				t.Fatalf("%s: the served card is not a JSON object: %v", g.Name, err)
			}
			for _, gone := range []string{"url", "protocolVersion", "preferredTransport", "additionalInterfaces", "supportsAuthenticatedExtendedCard"} {
				if _, ok := card[gone]; ok {
					t.Fatalf("%s: the served card keeps the 0.x member %q: %s", g.Name, gone, served)
				}
			}
			var ifaces []a2a.AgentInterface
			if err := json.Unmarshal(card["supportedInterfaces"], &ifaces); err != nil || len(ifaces) == 0 {
				t.Fatalf("%s: supportedInterfaces %s: %v", g.Name, card["supportedInterfaces"], err)
			}
			bindings := map[a2a.TransportProtocol]bool{}
			for _, i := range ifaces {
				if i.URL != publicURL || i.ProtocolVersion != a2a.Version {
					t.Fatalf("%s: a served interface %+v: want the gate's {svc} %s at protocol %s", g.Name, i, publicURL, a2a.Version)
				}
				bindings[i.ProtocolBinding] = true
			}
			if !bindings[a2a.TransportProtocolJSONRPC] || !bindings[a2a.TransportProtocolHTTPJSON] {
				t.Fatalf("%s: the served card offers %v, want JSONRPC and HTTP+JSON", g.Name, bindings)
			}
			var up map[string]json.RawMessage
			if err := json.Unmarshal(upstreamCard, &up); err != nil {
				t.Fatal(err)
			}
			for _, same := range []string{"name", "description", "version", "defaultInputModes", "defaultOutputModes"} {
				if string(card[same]) != string(up[same]) {
					t.Fatalf("%s: the seller's %q is %s in the served card, %s upstream", g.Name, same, card[same], up[same])
				}
			}
			var skills []map[string]json.RawMessage
			if err := json.Unmarshal(card["skills"], &skills); err != nil || len(skills) != 1 {
				t.Fatalf("%s: skills %s: %v", g.Name, card["skills"], err)
			}
			var upSkills []map[string]json.RawMessage
			_ = json.Unmarshal(up["skills"], &upSkills)
			for k, v := range upSkills[0] {
				if string(skills[0][k]) != string(v) {
					t.Fatalf("%s: skill member %q is %s in the served card, %s upstream", g.Name, k, skills[0][k], v)
				}
			}
		}
	}

	// A stock 1.0 client resolves the served card at {svc} (D10) and calls through it.
	hc := &http.Client{Transport: bearer{key: h.APIKey, next: http.DefaultTransport}, Timeout: time.Minute}
	card, err := agentcard.NewResolver(hc).Resolve(ctx, h.Gates[1].Svc("hello025"))
	if err != nil {
		t.Fatalf("a2a-go's resolver on the served card: %v", err)
	}
	if card.Name != "Hello World Agent" || len(card.Skills) != 1 || card.Skills[0].ID != "hello_world" {
		t.Fatalf("the served card as a2a-go reads it: %+v", card)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Destroy()
	res, err := c.SendMessage(ctx, newMessage("hi"))
	if err != nil {
		t.Fatalf("SendMessage through the served card: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted || task.Status.Message == nil || task.Status.Message.Parts[0].Text() != slowagent.HelloWorld {
		t.Fatalf("the 0.2.5 seller's Message must come back as a completed task carrying it: %#v", res)
	}
	// The card says streaming, so the dispatch streams: one 0.x message/stream, never a 1.0 method.
	if n := seller.Count("message/send") + seller.Count("message/stream"); n != 1 || seller.Sends() != 1 {
		t.Fatalf("the seller received %d 0.x sends, want 1 (requests %v)", n, methods(seller))
	}

	// A stream through the gate, on HTTP+JSON this time, also ends completed with the seller's message.
	s, err := h.BuyerA().SendStreaming(ctx, h.Gates[0], newMessage("hello world"), Call{Service: "hello025", Binding: HTTPJSON})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < 2 || evs[0].Kind != "task" || evs[len(evs)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("the stream through the 0.2.5 seller: %d events, want the task then the completed row", len(evs))
	}
	final, err := fold(evs)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status.Message == nil || final.Status.Message.Parts[0].Text() != slowagent.HelloWorld {
		t.Fatalf("the streamed task's status message: %+v", final.Status.Message)
	}
	checkCarried(t, h, s.TaskID(), ids(evs))
	if n := seller.Count("message/send") + seller.Count("message/stream"); n != 2 || seller.Sends() != 2 {
		t.Fatalf("the seller received %d 0.x sends, want 2 (requests %v)", n, methods(seller))
	}
	for _, r := range seller.Requests() {
		if r.Header.Get("Authorization") != "" || r.Header.Get("A2A-Version") == "1.0" {
			t.Fatalf("the seller got %s with Authorization %q, A2A-Version %q: a 0.x upstream is spoken to as 0.x, with no caller credential",
				r.Method, r.Header.Get("Authorization"), r.Header.Get("A2A-Version"))
		}
	}
}

func methods(a *slowagent.Agent) []string {
	var out []string
	for _, r := range a.Requests() {
		out = append(out, r.Method)
	}
	return out
}
