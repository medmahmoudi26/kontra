package mcprelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/upstream"
)

// Finding mcp-spec-1: the gate's SSE reader kept a leading U+FEFF, so the first event's field was "U+FEFF data": the
// relay passed that event unread (no strict decoding, no §2.5 scrub, no meter) while every client, which drops the
// byte order mark (WHATWG SSE; the TypeScript SDK's TextDecoderStream), dispatched it.
func TestReviewSSEReaderDropsBOM(t *testing.T) {
	r := newSSEReader(strings.NewReader("\xEF\xBB\xBFdata: {\"a\":1}\n\ndata: {\"b\":2}\n\n"))
	ev, err := r.next()
	if err != nil || !ev.hasData || ev.data != `{"a":1}` {
		t.Fatalf("the first event of a stream that starts with a BOM: %+v %v", ev, err)
	}
	if ev, err := r.next(); err != nil || ev.data != `{"b":2}` {
		t.Fatalf("the second: %+v %v", ev, err)
	}
	// Only one, and only at the start.
	r = newSSEReader(strings.NewReader("data: x\n\n\xEF\xBB\xBFdata: y\n\n"))
	if ev, _ := r.next(); ev.data != "x" {
		t.Fatalf("%+v", ev)
	}
	if ev, _ := r.next(); ev.hasData {
		t.Fatalf("a BOM inside the stream was dropped: %+v", ev)
	}
}

// Finding mcp-spec-2: the relay took only ids of the same type and spelling for the answer to the client's request,
// while the TypeScript SDK matches Number(response.id): an upstream answering "7" (isError true) and then 7
// (isError false) had the gate meter the second while the client acted on the first.
func TestReviewSameIDAsClients(t *testing.T) {
	for _, tc := range []struct {
		resp, req string
		want      bool
	}{
		{`7`, `7`, true}, {`7.0`, `7`, true}, {`7e0`, `7`, true}, {`"7"`, `7`, true}, {`" 7 "`, `7`, true},
		{`"7.0"`, `7`, true}, {`"0x7"`, `7`, true}, {`""`, `0`, true}, {`"-0"`, `0`, true},
		{`"7a"`, `7`, false}, {`"inf"`, `7`, false}, {`8`, `7`, false}, {`"07"`, `"7"`, false}, {`7`, `"7"`, false},
		{`"abc"`, `"abc"`, true}, {`"abc"`, `"ABC"`, false}, {`null`, `7`, false},
	} {
		if got := sameID(json.RawMessage(tc.resp), json.RawMessage(tc.req)); got != tc.want {
			t.Errorf("sameID(%s, %s) = %v, want %v", tc.resp, tc.req, got, tc.want)
		}
	}
}

// Finding mcp-spec-3: a top-level isError present but not a JSON boolean was metered is_error false and relayed.
func TestReviewIsErrorFlag(t *testing.T) {
	for _, tc := range []struct {
		result   string
		set, ok  bool
		isErrRow bool
	}{
		{`{"content":[]}`, false, true, false}, {`{"isError":true}`, true, true, true}, {`{"isError":false}`, false, true, false},
		{`{"isError":"true"}`, false, false, true}, {`{"isError":1}`, false, false, true}, {`{"isError":null}`, false, false, true},
	} {
		set, ok := mcpwire.IsErrorFlag(json.RawMessage(tc.result))
		if set != tc.set || ok != tc.ok {
			t.Errorf("IsErrorFlag(%s) = %v %v", tc.result, set, ok)
		}
		c := &callMeter{}
		c.observe(&mcpwire.Message{ID: json.RawMessage("1"), Result: json.RawMessage(tc.result)})
		if c.isError == nil || *c.isError != tc.isErrRow {
			t.Errorf("metered %s: %v", tc.result, c.isError)
		}
	}
	// The relay refuses it: 502 with -32603, metered an error.
	h := &Handler{log: slog.New(slog.DiscardHandler)}
	o := &relayOpts{method: mcpwire.MethodToolsCall, reqID: json.RawMessage("1"), meter: &callMeter{}}
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":"true"}}`)
	m, err := mcpwire.Parse(raw, mcpwire.FromUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if _, ref := h.process(o, m, raw); ref == nil || ref.Status != http.StatusBadGateway || o.meter.isError == nil || !*o.meter.isError {
		t.Fatalf("a string isError: refusal %+v, metered %v", ref, o.meter.isError)
	}
}

// cutBackend answers every request with an event stream that ends, cut, after one notification.
type cutBackend struct{}

func (cutBackend) Kind() upstream.Kind { return upstream.KindTunnel }
func (cutBackend) Origin() *url.URL    { u, _ := url.Parse("http://kontra-node.invalid"); return u }
func (cutBackend) Base() *url.URL      { u, _ := url.Parse("http://kontra-node.invalid/"); return u }
func (cutBackend) HTTPClient() *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := io.MultiReader(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n"),
			errReader{errors.New("yamux: stream reset")})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body), Request: r}, nil
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type heldTunnels struct{ held bool }

func (t heldTunnels) MCPBackend(uuid.UUID, uuid.UUID, string) (upstream.Backend, error) {
	if !t.held {
		return nil, errors.New("the session is gone")
	}
	return cutBackend{}, nil
}

// Finding mcp-spec-4: a call whose answer stream was cut by a tunnel loss (the node killed, its session dropped) was
// classified unreachable, so a modern task sent again with its answers failed upstream_unreachable, not
// tunnel_lost (§11.12, D178). A cut read on a tunnelled link whose session this replica no longer holds is
// tunnel_lost now; one whose session is still held stays unreachable.
func TestReviewCutStreamOnLostTunnel(t *testing.T) {
	for _, held := range []bool{false, true} {
		k := &mcpLink{backend: cutBackend{}, tunnelled: true, tunnels: heldTunnels{held: held}, service: "fs"}
		_, _, _, err := k.call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`),
			json.RawMessage("1"), nil)
		xe := asExchange(err)
		want := classTunnelLost
		if held {
			want = classUnreachable
		}
		if xe.class != want {
			t.Errorf("held %v: class %d (%s), want %d", held, xe.class, xe.msg, want)
		}
	}
}
