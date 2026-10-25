package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/ssrf"
)

func direct(t *testing.T, raw string) *Direct {
	t.Helper()
	d, err := NewDirect(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewDirect(t *testing.T) {
	d := direct(t, "http://Example.COM:9999/agents/hello/")
	if got := d.Origin().String(); got != "http://Example.COM:9999" {
		t.Errorf("origin %s", got)
	}
	if got := d.Base().String(); got != "http://Example.COM:9999/agents/hello/" {
		t.Errorf("base %s", got)
	}
	if d.hostPort != "example.com:9999" {
		t.Errorf("pinned host:port %s", d.hostPort)
	}
	if d2 := direct(t, "https://seller.example"); d2.hostPort != "seller.example:443" {
		t.Errorf("default https port: %s", d2.hostPort)
	}
	for _, bad := range []string{
		"ftp://x/", "https://u:p@x/", "https://x/?q=1", "https://x/#f", "https:///", "https://x:0/", "x",
	} {
		if _, err := NewDirect(bad, true); err == nil {
			t.Errorf("NewDirect(%q) accepted", bad)
		}
	}
	// http:// needs allow_private (D67).
	if _, err := NewDirect("http://seller.example/", false); !errors.Is(err, ssrf.ErrScheme) {
		t.Errorf("http without allow_private: %v", err)
	}
}

// The backend's dialer reaches only its configured host:port (D71): any other one is refused before a
// connection, whatever a card says.
func TestDirectDialsOnlyItsHostPort(t *testing.T) {
	var otherHits atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) }))
	defer other.Close()
	var mineHits atomic.Int64
	mine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mineHits.Add(1)
		io.WriteString(w, r.URL.Path)
	}))
	defer mine.Close()
	d := direct(t, mine.URL+"/base")
	resp, err := d.HTTPClient().Get(other.URL + "/x")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrSteered) || otherHits.Load() != 0 {
		t.Errorf("GET another host:port: %v, hits %d", err, otherHits.Load())
	}
	// A card naming another host: the endpoint is the configured origin with the card's path.
	info, err := ReadInfo([]byte(`{"name":"x","supportedInterfaces":[{"url":"` + other.URL + `/a2a/v1","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Endpoint(d, info), mine.URL+"/a2a/v1"; got != want {
		t.Errorf("endpoint %s, want %s", got, want)
	}
}

func TestReadInfo(t *testing.T) {
	for _, tc := range []struct {
		name, card string
		want       Info
		err        error
	}{
		{"1.0, JSONRPC preferred over HTTP+JSON whatever the order",
			`{"supportedInterfaces":[{"url":"https://elsewhere.example:7/rest","protocolBinding":"HTTP+JSON","protocolVersion":"1.0"},{"url":"https://elsewhere.example:7/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"streaming":true,"extensions":[{"uri":"urn:a"},{"uri":"urn:b","required":true}]}}`,
			Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: "/rpc", Streaming: true, Extensions: []string{"urn:a", "urn:b"}}, nil},
		{"1.0 prefers 1.0 over a 0.3 interface",
			`{"supportedInterfaces":[{"url":"http://h/old","protocolBinding":"JSONRPC","protocolVersion":"0.3"},{"url":"http://h/new","protocolBinding":"HTTP+JSON","protocolVersion":"1.0","tenant":"t1"}],"capabilities":{}}`,
			Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolHTTPJSON, Path: "/new", Tenant: "t1"}, nil},
		{"0.3 card", `{"protocolVersion":"0.3.0","name":"a","url":"http://h:9/a2a","preferredTransport":"JSONRPC","capabilities":{"streaming":true},"x-vendor":1}`,
			Info{ProtocolVersion: "0.3", Binding: a2a.TransportProtocolJSONRPC, Path: "/a2a", Streaming: true}, nil},
		{"0.2.5 card without preferredTransport means JSONRPC", `{"name":"helloworld","url":"http://localhost:9999/","version":"1.0.0","capabilities":{"streaming":true},"defaultInputModes":["text"],"skills":[]}`,
			Info{ProtocolVersion: "0.3", Binding: a2a.TransportProtocolJSONRPC, Path: "/", Streaming: true}, nil},
		{"gRPC only", `{"supportedInterfaces":[{"url":"http://h/","protocolBinding":"GRPC","protocolVersion":"1.0"}],"capabilities":{}}`, Info{}, ErrBadCard},
		{"not an object", `[1]`, Info{}, ErrBadCard},
	} {
		got, err := ReadInfo([]byte(tc.card))
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: %v, want %v", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s:\n got %+v %v\nwant %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestFetchCard(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	cards := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		body, ok := cards[r.URL.Path]
		mu.Unlock()
		switch {
		case r.URL.Path == "/redirect/.well-known/agent-card.json":
			http.Redirect(w, r, "/landed", http.StatusFound)
		case ok:
			io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cards["/agents/x/.well-known/agent-card.json"] = `{"name":"x"}`
	cards["/legacy/.well-known/agent.json"] = `{"name":"legacy","url":"http://h/"}`
	cards["/big/.well-known/agent-card.json"] = `{"pad":"` + strings.Repeat("a", MaxCard) + `"}`
	cards["/notjson/.well-known/agent-card.json"] = `not json`

	f, err := FetchCard(context.Background(), direct(t, srv.URL+"/agents/x"))
	if err != nil || string(f.Raw) != `{"name":"x"}` || f.Path != "/agents/x/.well-known/agent-card.json" {
		t.Errorf("relative to the base path: %+v %v", f, err)
	}
	f, err = FetchCard(context.Background(), direct(t, srv.URL+"/legacy/"))
	if err != nil || !strings.Contains(string(f.Raw), "legacy") || f.Path != "/legacy/.well-known/agent.json" {
		t.Errorf("agent.json fallback: %+v %v", f, err)
	}
	if _, err := FetchCard(context.Background(), direct(t, srv.URL+"/big")); !errors.Is(err, ErrLimit) {
		t.Errorf("a card over 256 KiB: %v", err)
	}
	if _, err := FetchCard(context.Background(), direct(t, srv.URL+"/notjson")); !errors.Is(err, ErrBadCard) {
		t.Errorf("not JSON: %v", err)
	}
	if _, err := FetchCard(context.Background(), direct(t, srv.URL+"/none")); !errors.Is(err, ErrNoCard) {
		t.Errorf("no card: %v", err)
	}
	mu.Lock()
	paths = nil
	mu.Unlock()
	if _, err := FetchCard(context.Background(), direct(t, srv.URL+"/redirect")); err == nil {
		t.Error("a redirecting card URL was accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if p == "/landed" {
			t.Error("the card redirect was followed")
		}
	}
}

func TestBodyLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Write(bytes.Repeat([]byte("a"), MaxBody+1))
		case "/exact":
			w.Write(bytes.Repeat([]byte("a"), MaxBody))
		case "/sse-big":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: small\n\n")
			io.WriteString(w, "data: "+strings.Repeat("b", MaxSSEEvent)+"\n\n")
		case "/sse-many":
			// Many events, 10 MiB in all, none over the event limit (CRLF line ends).
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			chunk := "data: " + strings.Repeat("c", 1<<20) + "\r\n\r\n"
			for range 10 {
				io.WriteString(w, chunk)
			}
		}
	}))
	defer srv.Close()
	hc := direct(t, srv.URL).HTTPClient()
	read := func(path string) (int, error) {
		ctx, ex := WithExchange(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil && !ex.LimitExceeded() {
			t.Errorf("%s: a limit error the exchange did not record", path)
		}
		return len(b), err
	}
	if _, err := read("/big"); !errors.Is(err, ErrLimit) {
		t.Errorf("body over 8 MiB: %v", err)
	}
	if n, err := read("/exact"); err != nil || n != MaxBody {
		t.Errorf("body of exactly 8 MiB: %d %v", n, err)
	}
	if _, err := read("/sse-big"); !errors.Is(err, ErrLimit) {
		t.Errorf("one SSE event over 4 MiB: %v", err)
	}
	if n, err := read("/sse-many"); err != nil || n < 10<<20 {
		t.Errorf("a long stream of small events: %d %v", n, err)
	}
}

// echoAgent answers every message with a completed task and records the headers it received.
type echoAgent struct {
	mu      sync.Mutex
	headers []http.Header
}

func (e *echoAgent) executor() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("ok"))), nil)
		}
	})
}

func (e *echoAgent) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.headers = append(e.headers, r.Header.Clone())
		e.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// The upstream request carries A2A-Version and only the activated URIs the upstream declares, in one
// header, and nothing of the caller's (spec §6.2); a 0.3 upstream is reached through a2av0's transport.
func TestClientHeadersAndBindings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version a2a.ProtocolVersion
		mount   func(a2asrv.RequestHandler) http.Handler
		binding a2a.TransportProtocol
	}{
		{"1.0 JSONRPC", "1.0", func(h a2asrv.RequestHandler) http.Handler { return a2asrv.NewJSONRPCHandler(h) }, a2a.TransportProtocolJSONRPC},
		{"1.0 HTTP+JSON", "1.0", func(h a2asrv.RequestHandler) http.Handler { return a2asrv.NewRESTHandler(h) }, a2a.TransportProtocolHTTPJSON},
		{"0.3 JSONRPC", "0.3", func(h a2asrv.RequestHandler) http.Handler { return a2av0.NewJSONRPCHandler(h) }, a2a.TransportProtocolJSONRPC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &echoAgent{}
			mux := http.NewServeMux()
			mux.Handle("/a2a/", http.StripPrefix("/a2a", agent.record(tc.mount(a2asrv.NewHandler(agent.executor())))))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			d := direct(t, srv.URL)
			info := Info{ProtocolVersion: tc.version, Binding: tc.binding, Path: "/a2a/", Extensions: []string{"urn:declared", "urn:other"}}
			c, err := NewClient(context.Background(), d, info, []string{"urn:declared", "urn:not-declared"})
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
			if err != nil {
				t.Fatal(err)
			}
			if task, ok := res.(*a2a.Task); !ok || task.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("result %T %+v", res, res)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			h := agent.headers[0]
			// 0.3 servers read the extensions from X-A2A-Extensions (a2av0 renames the header).
			extHeader := "A2A-Extensions"
			if tc.version == "0.3" {
				extHeader = "X-A2A-Extensions"
			}
			if got := h.Values(extHeader); len(got) != 1 || got[0] != "urn:declared" {
				t.Errorf("%s %q", extHeader, got)
			}
			if tc.version == "1.0" && h.Get("A2A-Version") != "1.0" {
				t.Errorf("A2A-Version %q", h.Get("A2A-Version"))
			}
			for _, k := range []string{"Authorization", "Cookie", "X-Forwarded-For", "Kontra-Node"} {
				if h.Get(k) != "" {
					t.Errorf("%s reached the upstream", k)
				}
			}
		})
	}
}

func TestClassify(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/429":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/400":
			w.WriteHeader(http.StatusBadRequest)
		case "/rpcerr":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":"1","error":{"code":-32602,"message":"bad thing from the seller"}}`)
		case "/garbage":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":`)
		}
	}))
	defer srv.Close()
	send := func(base string) (context.Context, error, *Exchange) {
		d := direct(t, base)
		path := "/"
		if i := strings.Index(base[len("http://"):], "/"); i >= 0 {
			path = base[len("http://")+i:]
		}
		c, err := NewClient(context.Background(), d, Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: path}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, ex := WithExchange(context.Background())
		_, err = c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
		return ctx, err, ex
	}
	for _, tc := range []struct {
		base  string
		class Class
		retry time.Duration
		msg   string
	}{
		{closedURL, Unreachable, 0, ""},
		{srv.URL + "/503", Unreachable, 0, "503"},
		{srv.URL + "/429", Throttled, 7 * time.Second, "429"},
		{srv.URL + "/400", Protocol, 0, "400"},
		{srv.URL + "/rpcerr", Protocol, 0, "bad thing from the seller"},
		{srv.URL + "/garbage", Protocol, 0, "malformed"},
	} {
		ctx, err, ex := send(tc.base)
		if err == nil {
			t.Fatalf("%s: no error", tc.base)
		}
		f := Classify(ctx, err, ex)
		if f.Class != tc.class || f.RetryAfter != tc.retry || !strings.Contains(f.Message, tc.msg) {
			t.Errorf("%s: %+v, want class %d retry %v message ~%q", tc.base, f, tc.class, tc.retry, tc.msg)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f := Classify(ctx, context.Canceled, nil); f.Class != Canceled {
		t.Errorf("canceled: %+v", f)
	}
	if f := Classify(context.Background(), fmt.Errorf("read: %w", ErrLimit), nil); f.Class != Limit {
		t.Errorf("limit: %+v", f)
	}
	if got := Cap(strings.Repeat("é", MaxMessage)); len(got) > MaxMessage+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("Cap: %d bytes", len(got))
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"": 0, "3": 3 * time.Second, "-1": 0, "x": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
		"999999999": 24 * time.Hour,
	} {
		if got := RetryAfter(in, now); got != want {
			t.Errorf("RetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
