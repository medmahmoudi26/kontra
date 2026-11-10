package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"kontra.sh/kontra/internal/sealed"
)

// AP2 v0.2.0 as the example merchant speaks it (examples/sellers/ap2-merchant/main.go; its README). The gate knows
// none of these names: the test does, as a shopper would.
const (
	ap2Ext                = "https://github.com/google-agentic-commerce/ap2/v1"
	ap2KeyCheckout        = "ap2.checkout"
	ap2KeyCheckoutMandate = "ap2.mandates.CheckoutMandateSdJwt"
	ap2KeyCheckoutReceipt = "ap2.CheckoutReceipt"
)

func ap2b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func ap2sha(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// ap2Mandate is a closed Checkout Mandate over checkoutJWT, as a shopping agent presents it: an SD-JWT (RFC 9901)
// whose checkout_jwt is selectively disclosed, checkout_hash the base64url SHA-256 of checkoutJWT
// (docs/ap2/checkout_mandate.md), signed ES256 with a key of the test's own (which the merchant does not check).
func ap2Mandate(t *testing.T, checkoutJWT string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	disc, _ := json.Marshal([]any{ap2b64(salt), "checkout_jwt", checkoutJWT})
	disclosure := ap2b64(disc)
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "dc+sd-jwt"})
	payload, _ := json.Marshal(map[string]any{"vct": "mandate.checkout.1", "checkout_hash": ap2b64(ap2sha(checkoutJWT)),
		"_sd": []string{ap2b64(ap2sha(disclosure))}, "_sd_alg": "sha-256", "iat": time.Now().Unix()})
	input := ap2b64(header) + "." + ap2b64(payload)
	r, s, err := ecdsa.Sign(rand.Reader, key, ap2sha(input))
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + ap2b64(sig) + "~" + disclosure + "~"
}

// ap2Part is the first part of m whose data is an object holding key, and that value.
func ap2Part(t *testing.T, where string, m *a2a.Message, key string) (*a2a.Part, any) {
	t.Helper()
	if m != nil {
		for _, p := range m.Parts {
			if obj, ok := jsonValue(t, p.Data()).(map[string]any); ok && obj[key] != nil {
				return p, obj[key]
			}
		}
	}
	raw, _ := json.Marshal(m)
	t.Fatalf("%s: no data part holding %s in %s", where, key, raw)
	return nil, nil
}

// jcsValue is v as JSON under RFC 8785 (JCS).
func jcsValue(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return jcsOf(t, raw)
}

// samePart fails unless got's data and metadata are want's under JCS.
func samePart(t *testing.T, where string, got, want *a2a.Part) {
	t.Helper()
	if g, w := jcsValue(t, got.Data()), jcsValue(t, want.Data()); g != w {
		t.Fatalf("%s: the part's data\n%s\nis not, under JCS,\n%s", where, g, w)
	}
	if g, w := jcsValue(t, got.Metadata), jcsValue(t, want.Metadata); g != w {
		t.Fatalf("%s: the part's metadata %s is not, under JCS, %s", where, g, w)
	}
}

// statusMessage is the message of a SendMessage answer's or a stream's last event.
func statusMessage(t *testing.T, where string, ev a2a.Event) (a2a.TaskState, *a2a.Message) {
	t.Helper()
	switch e := ev.(type) {
	case *a2a.Task:
		return e.Status.State, e.Status.Message
	case *a2a.TaskStatusUpdateEvent:
		return e.Status.State, e.Status.Message
	}
	t.Fatalf("%s: %T, want a task or a status update", where, ev)
	return "", nil
}

// merchantView is the task as the merchant itself holds it, read from the merchant directly (GetTask needs no AP2
// activation): what it received and what it answered, before any gate.
func merchantView(t *testing.T, ctx context.Context, h *Harness, merchantURL string, task a2a.TaskID) *a2a.Task {
	t.Helper()
	as, err := h.Attempts(ctx, task)
	if err != nil || len(as) == 0 || as[len(as)-1].UpstreamTaskID == "" {
		t.Fatalf("the attempts of %s: %+v %v", task, as, err)
	}
	c, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{a2a.NewAgentInterface(merchantURL, a2a.TransportProtocolJSONRPC)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Destroy()
	got, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(as[len(as)-1].UpstreamTaskID)})
	if err != nil {
		t.Fatalf("the merchant's own GetTask: %v", err)
	}
	return got
}

// received is the user message in the merchant's history that holds key.
func received(t *testing.T, where string, task *a2a.Task, key string) *a2a.Message {
	t.Helper()
	for _, m := range task.History {
		if m.Role != a2a.MessageRoleUser {
			continue
		}
		for _, p := range m.Parts {
			if obj, ok := jsonValue(t, p.Data()).(map[string]any); ok && obj[key] != nil {
				return m
			}
		}
	}
	t.Fatalf("%s: the merchant's history holds no message with %s", where, key)
	return nil
}

// ap2Extension is the AP2 entry of a card's capabilities.extensions, under JCS.
func ap2Extension(t *testing.T, card []byte) string {
	t.Helper()
	var c struct {
		Capabilities struct {
			Extensions []json.RawMessage `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(card, &c); err != nil {
		t.Fatalf("a card: %v", err)
	}
	for _, e := range c.Capabilities.Extensions {
		var x struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(e, &x) == nil && x.URI == ap2Ext {
			return jcsOf(t, e)
		}
	}
	t.Fatalf("no AP2 entry in %s", card)
	return ""
}

// headerClient is an a2a-go client of a card resolved at base, sending h with every request (both bindings).
func headerClient(t *testing.T, ctx context.Context, base string, h http.Header) *a2aclient.Client {
	t.Helper()
	card, err := agentcard.DefaultResolver.Resolve(ctx, base)
	if err != nil {
		t.Fatalf("resolve %s: %v", base, err)
	}
	hc := &http.Client{Transport: addHeaders{h, http.DefaultTransport}, Timeout: 2 * time.Minute}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return c
}

// addHeaders adds request headers (A2A-Extensions).
type addHeaders struct {
	h    http.Header
	next http.RoundTripper
}

func (x addHeaders) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, vs := range x.h {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return x.next.RoundTrip(r)
}

// TestAP2Passthrough is ap2_passthrough (spec/relay.md §2.4, §2.6, §12.2; D294, D326, D328, D351).
// Single tenant. The example AP2 merchant (examples/sellers/ap2-merchant: a2a-go v2.7.0, run
// unmodified, its card requiring AP2 v0.2.0 with params.roles [merchant]) is hosted by a node whose tunnel is on
// replica B, as the service ap2 and as the sealed service ap2sealed; the operator's Admission is the e2e double, which
// allows. The test is the shopper: it knows AP2's data-part keys, the gate does not. Asserted:
//
//   - both replicas serve the merchant's AP2 card entry, required and params included, equal under JCS to the
//     merchant's own; so does the hiring node's local card of ap2sealed;
//   - a SendMessage that does not activate AP2 is -32008 and writes no task row;
//   - JSON-RPC SendMessage with A2A-Extensions naming AP2 (plain): the response echoes the URI, Admission's request
//     lists it in extensions, and the merchant answers input-required with an ap2.checkout part (it refuses a message
//     without the activation, so the activation reached it): the part's data and metadata equal under JCS to the
//     merchant's own copy (read from the merchant directly), the checkout JWT byte for byte; GetTask on B the same;
//   - a Checkout Mandate bound to that checkout, in an ap2.mandates.CheckoutMandateSdJwt part with part and message
//     metadata, sent through B: the merchant's history holds the part's data and metadata, and the message's metadata,
//     equal under JCS to what was sent; the task completes with an ap2.CheckoutReceipt part equal under JCS to the
//     merchant's;
//   - the same round trip over HTTP+JSON, streamed, with the activation echoed on the stream;
//   - sealed (through a hiring node on replica A): -32008 without the activation; with it, the checkout, the mandate
//     and the receipt cross intact both ways (the merchant's copies equal under JCS), while the gate's task log holds
//     the sealed envelope's media type and none of the checkout JWT, the mandate, the receipt or their keys, and the
//     part and message metadata stay plaintext (the message's with the hiring node's kontra.sealed.replyTo added);
//   - /mcp hire of ap2 is isError naming the A2A URL and the AP2 URI, and writes no task row.
func TestAP2Passthrough(t *testing.T) {
	adm, err := StartAdmission()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	h := start(t, Options{Tunnel: map[string]TunnelService{"ap2": {}, "ap2sealed": {Sealed: "true"}}, Admission: adm})
	ctx := ctxFor(t, 8*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	merchant, err := h.StartExampleSeller(ctx, "ap2-merchant")
	if err != nil {
		t.Fatal(err)
	}
	hostOn(t, ctx, h, b, map[string]string{"ap2": merchant.URL, "ap2sealed": merchant.URL})
	waitSealedCard(t, ctx, h, "ap2sealed")
	buyer := h.BuyerA()
	activate := http.Header{"A2A-Extensions": {ap2Ext}}

	// The card entry, as the merchant wrote it.
	own, st := getBody(ctx, merchant.URL+"/.well-known/agent-card.json", "")
	if st != http.StatusOK {
		t.Fatalf("the merchant's own card: %d", st)
	}
	want := ap2Extension(t, own)
	if !strings.Contains(want, `"required":true`) || !strings.Contains(want, `"params":{"roles":["merchant"]}`) {
		t.Fatalf("the merchant's AP2 entry %s", want)
	}
	for _, g := range h.Gates {
		for _, svc := range []string{"ap2", "ap2sealed"} {
			if got := ap2Extension(t, getCard(t, ctx, g.Svc(svc)+"/.well-known/agent-card.json", h.APIKey)); got != want {
				t.Fatalf("%s serves %s's AP2 entry\n%s\nwant the merchant's\n%s", g.Name, svc, got, want)
			}
		}
	}

	// Not activated: -32008, no task row.
	before := taskCount(t, h)
	res, err := buyer.RPCCall(ctx, a, "SendMessage", newMessage("I would like the example item"), Call{Service: "ap2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || res.Error.Code != -32008 {
		t.Fatalf("SendMessage without AP2: %s, want -32008", res.Body)
	}
	if n := taskCount(t, h); n != before {
		t.Fatalf("the refused message wrote %d task rows", n-before)
	}

	// JSON-RPC, activated: the checkout.
	res, err = buyer.RPCCall(ctx, a, "SendMessage", newMessage("I would like the example item"), Call{Service: "ap2", Header: activate})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Values("A2A-Extensions"); len(got) != 1 || got[0] != ap2Ext {
		t.Errorf("the activation echoed: %q, want %s", got, ap2Ext)
	}
	task := rpcTask(t, res)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("the first turn: %s", task.Status.State)
	}
	if reqs := adm.Requests(); len(reqs) == 0 || reqs[len(reqs)-1].TaskID != string(task.ID) || !slices.Contains(reqs[len(reqs)-1].Extensions, ap2Ext) {
		t.Errorf("Admission's request for the task: %+v, want extensions listing %s", reqs, ap2Ext)
	}
	coPart, co := ap2Part(t, "the checkout", task.Status.Message, ap2KeyCheckout)
	view := merchantView(t, ctx, h, merchant.URL, task.ID)
	ownPart, ownCo := ap2Part(t, "the merchant's checkout", view.Status.Message, ap2KeyCheckout)
	samePart(t, "the checkout", coPart, ownPart)
	jwt, _ := co.(map[string]any)["checkout_jwt"].(string)
	if ownJWT, _ := ownCo.(map[string]any)["checkout_jwt"].(string); jwt == "" || jwt != ownJWT {
		t.Fatalf("the checkout JWT %q is not the merchant's %q byte for byte", jwt, ownJWT)
	}
	got, err := buyer.Client(t, b, "ap2").GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	gotPart, _ := ap2Part(t, "GetTask on B", got.Status.Message, ap2KeyCheckout)
	samePart(t, "GetTask on B", gotPart, ownPart)

	// The mandate, through B.
	mandatePart := a2a.NewDataPart(map[string]any{ap2KeyCheckoutMandate: ap2Mandate(t, jwt)})
	mandatePart.Metadata = map[string]any{"shopper.example/part": "mandate-1", "nested": map[string]any{"b": 2, "a": []any{1, "x"}}}
	mandate := a2a.NewMessage(a2a.MessageRoleUser, mandatePart)
	mandate.TaskID, mandate.ContextID = task.ID, task.ContextID
	mandate.Metadata = map[string]any{"shopper.example/ref": "order-42"}
	res, err = buyer.RPCCall(ctx, b, "SendMessage", &a2a.SendMessageRequest{Message: mandate}, Call{Service: "ap2", Header: activate})
	if err != nil {
		t.Fatal(err)
	}
	done := rpcTask(t, res)
	if done.ID != task.ID || done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the mandate's turn: %s %s", done.ID, done.Status.State)
	}
	view = merchantView(t, ctx, h, merchant.URL, task.ID)
	in := received(t, "the mandate", view, ap2KeyCheckoutMandate)
	inPart, _ := ap2Part(t, "the mandate received", in, ap2KeyCheckoutMandate)
	samePart(t, "the mandate", inPart, mandatePart)
	if g, w := jcsValue(t, in.Metadata), jcsValue(t, mandate.Metadata); g != w {
		t.Fatalf("the mandate message's metadata reached the merchant as %s, sent %s", g, w)
	}
	receiptPart, _ := ap2Part(t, "the receipt", done.Status.Message, ap2KeyCheckoutReceipt)
	ownReceipt, _ := ap2Part(t, "the merchant's receipt", view.Status.Message, ap2KeyCheckoutReceipt)
	samePart(t, "the receipt", receiptPart, ownReceipt)

	// HTTP+JSON, streamed: the same round trip.
	streamTurn := func(g *Gate, req *a2a.SendMessageRequest, want a2a.TaskState) (a2a.TaskID, *a2a.Message) {
		t.Helper()
		stream, err := buyer.SendStreaming(ctx, g, req, Call{Service: "ap2", Binding: HTTPJSON, Header: activate})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if got := stream.Header.Values("A2A-Extensions"); len(got) != 1 || got[0] != ap2Ext {
			t.Errorf("the stream's activation echoed: %q", got)
		}
		evs, err := stream.ReadAll(ctx)
		if err != nil || len(evs) == 0 {
			t.Fatalf("the stream: %d events, %v", len(evs), err)
		}
		state, msg := statusMessage(t, "the stream's last event", evs[len(evs)-1].Event)
		if state != want {
			t.Fatalf("the stream ended %s, want %s", state, want)
		}
		return stream.TaskID(), msg
	}
	streamed, msg := streamTurn(a, newMessage("I would like the example item"), a2a.TaskStateInputRequired)
	coPart, co = ap2Part(t, "the streamed checkout", msg, ap2KeyCheckout)
	ownPart, _ = ap2Part(t, "the merchant's streamed checkout", merchantView(t, ctx, h, merchant.URL, streamed).Status.Message, ap2KeyCheckout)
	samePart(t, "the streamed checkout", coPart, ownPart)
	jwt, _ = co.(map[string]any)["checkout_jwt"].(string)
	mandatePart = a2a.NewDataPart(map[string]any{ap2KeyCheckoutMandate: ap2Mandate(t, jwt)})
	mandatePart.Metadata = map[string]any{"shopper.example/part": "mandate-2"}
	mandate = a2a.NewMessage(a2a.MessageRoleUser, mandatePart)
	mandate.TaskID = streamed
	_, msg = streamTurn(b, &a2a.SendMessageRequest{Message: mandate}, a2a.TaskStateCompleted)
	view = merchantView(t, ctx, h, merchant.URL, streamed)
	inPart, _ = ap2Part(t, "the streamed mandate received", received(t, "the streamed mandate", view, ap2KeyCheckoutMandate), ap2KeyCheckoutMandate)
	samePart(t, "the streamed mandate", inPart, mandatePart)
	receiptPart, _ = ap2Part(t, "the streamed receipt", msg, ap2KeyCheckoutReceipt)
	ownReceipt, _ = ap2Part(t, "the merchant's streamed receipt", view.Status.Message, ap2KeyCheckoutReceipt)
	samePart(t, "the streamed receipt", receiptPart, ownReceipt)

	// Sealed, through a hiring node on replica A.
	hirer, err := h.StartNode(ctx, NodeOptions{Name: "hirer", APIKey: h.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	local, err := hirer.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	localCard, st := getBody(ctx, local+"/agents/ap2sealed/.well-known/agent-card.json", "")
	if st != http.StatusOK {
		t.Fatalf("the hiring node's local card: %d", st)
	}
	if got := ap2Extension(t, localCard); got != want {
		t.Fatalf("the hiring node's local card has the AP2 entry\n%s\nwant the merchant's\n%s", got, want)
	}
	before = taskCount(t, h)
	if _, err := headerClient(t, ctx, local+"/agents/ap2sealed", nil).SendMessage(ctx, newMessage("I would like the example item")); !errors.Is(err, a2a.ErrExtensionSupportRequired) {
		t.Fatalf("a sealed SendMessage without AP2: %v, want ExtensionSupportRequiredError", err)
	}
	if n := taskCount(t, h); n != before {
		t.Fatalf("the refused sealed message wrote %d task rows", n-before)
	}
	sc := headerClient(t, ctx, local+"/agents/ap2sealed", activate)
	first := sendTask(t, ctx, sc, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("I would like the example item")))
	if first.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("the sealed first turn: %s", first.Status.State)
	}
	coPart, co = ap2Part(t, "the sealed checkout", first.Status.Message, ap2KeyCheckout)
	ownPart, _ = ap2Part(t, "the merchant's sealed checkout", merchantView(t, ctx, h, merchant.URL, first.ID).Status.Message, ap2KeyCheckout)
	samePart(t, "the sealed checkout", coPart, ownPart)
	jwt, _ = co.(map[string]any)["checkout_jwt"].(string)
	token := ap2Mandate(t, jwt)
	mandatePart = a2a.NewDataPart(map[string]any{ap2KeyCheckoutMandate: token})
	mandatePart.Metadata = map[string]any{"shopper.example/part": "sealed-mandate"}
	mandate = a2a.NewMessage(a2a.MessageRoleUser, mandatePart)
	mandate.TaskID, mandate.ContextID = first.ID, first.ContextID
	mandate.Metadata = map[string]any{"shopper.example/ref": "sealed-order"}
	sdone := sendTask(t, ctx, sc, mandate)
	if sdone.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the sealed mandate's turn: %s", sdone.Status.State)
	}
	view = merchantView(t, ctx, h, merchant.URL, first.ID)
	in = received(t, "the sealed mandate", view, ap2KeyCheckoutMandate)
	inPart, _ = ap2Part(t, "the sealed mandate received", in, ap2KeyCheckoutMandate)
	samePart(t, "the sealed mandate", inPart, mandatePart)
	// The hiring node adds its reply-to recipient (ext-sealed.md: kontra.sealed.replyTo); the rest is the shopper's.
	inMeta := map[string]any{}
	for k, v := range in.Metadata {
		inMeta[k] = v
	}
	if r, _ := inMeta["kontra.sealed.replyTo"].(string); !strings.HasPrefix(r, "age1") {
		t.Errorf("the sealed mandate message carries no kontra.sealed.replyTo: %v", in.Metadata)
	}
	delete(inMeta, "kontra.sealed.replyTo")
	if g, w := jcsValue(t, inMeta), jcsValue(t, mandate.Metadata); g != w {
		t.Fatalf("the sealed mandate message's metadata reached the merchant as %s, sent %s", g, w)
	}
	receiptPart, receipt := ap2Part(t, "the sealed receipt", sdone.Status.Message, ap2KeyCheckoutReceipt)
	ownReceipt, _ = ap2Part(t, "the merchant's sealed receipt", view.Status.Message, ap2KeyCheckoutReceipt)
	samePart(t, "the sealed receipt", receiptPart, ownReceipt)
	// What the gate kept of the sealed task: envelopes, and the parts' plaintext metadata.
	if err := h.WaitWorkflowClosed(ctx, first.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	events := strings.Join(texts(t, h, `select payload::text from kontra.task_events where task_id = $1`, string(first.ID)), "\n")
	if !strings.Contains(events, sealed.MediaType) || !strings.Contains(events, "sealed-mandate") {
		t.Fatalf("the sealed task's log carries no envelope, or not the part's plaintext metadata")
	}
	receiptJWT, _ := receipt.(string)
	for _, secret := range []string{jwt, token, strings.Split(token, "~")[0], receiptJWT, ap2KeyCheckout, ap2KeyCheckoutMandate, ap2KeyCheckoutReceipt} {
		if secret == "" || strings.Contains(events, secret) {
			t.Fatalf("the sealed task's log holds %.60q in the clear", secret)
		}
	}

	// /mcp hire: AP2 is required, a hire cannot activate it.
	before = taskCount(t, h)
	s := OpenGateSession(t, ctx, a, h.APIKey, `{}`)
	hire := Tool(t, s.Tool("hire", `{"service":"ap2","message":"I would like the example item"}`))
	if !hire.IsError || !strings.Contains(hire.Text(), h.Front.URL+"/agents/ap2") || !strings.Contains(hire.Text(), ap2Ext) {
		t.Fatalf("hire of the merchant: %+v, want isError naming %s and %s", hire, h.Front.URL+"/agents/ap2", ap2Ext)
	}
	if n := taskCount(t, h); n != before {
		t.Fatalf("the refused hire wrote %d task rows", n-before)
	}
}
