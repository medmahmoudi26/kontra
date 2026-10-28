package node

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// Finding sealing-spec-5: a URL MCP server's 2xx answer labelled neither application/json nor text/event-stream (a
// text/plain one, or none) passed the node without its sealing: a sealed tools/call answered that way reached the
// gate in plaintext. Such a body is read as one JSON message through the filter now, sealed, and relayed as JSON.
func TestReviewSealedResultLabelledTextPlain(t *testing.T) {
	for _, ct := range []string{"text/plain", ""} {
		up := mcptest.New()
		up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if bytes.Contains(b, []byte(`"tools/call"`)) {
				w.Header()["Content-Type"] = nil
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"PLAINTEXT-RESULT"}],"isError":false}}`)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			up.ServeHTTP(w, r)
		})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		target, _ := url.Parse(srv.URL + "/mcp")
		pp := newMCPProxyPair(t, MCPOptions{}, Serve{Kind: tunnel.KindMCP, Name: "web", Target: target})
		hosting, node := hostingFor(t)
		pp.p.seal = hosting
		init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`
		resp, _, _ := pp.raw(t, mcpPreamble("web"), mcpPost(init, "", ""))
		sid := resp.Header.Get(mcpwire.HeaderSessionID)
		sum := sha256.Sum256([]byte("the gate's session id"))
		session := hex.EncodeToString(sum[:])
		call, _, _ := sealedCall(t, node, session, "echo", "1", `{"text":"x"}`)
		resp, body, _ := pp.raw(t, mcpSealedPreamble("web", session, "1"), mcpPost(call, sid, mcpwire.V20251125))
		if strings.Contains(body, "PLAINTEXT-RESULT") {
			t.Fatalf("labelled %q: a sealed call's result reaches the gate unsealed: %d %s", ct, resp.StatusCode, body)
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || !strings.Contains(body, sealed.MediaType) {
			t.Fatalf("labelled %q: %d %q %s", ct, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
}

// Finding mcp-spec-1 (the node): the URL filter's SSE reader kept a leading byte order mark, so the first event's
// field was not "data" and the event passed the roots filter and the sealing unread, while clients dispatch it.
func TestReviewSSEFilterDropsBOM(t *testing.T) {
	var seen []string
	src := "\xEF\xBB\xBFdata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":8,\"result\":{}}\n\n"
	f := &sseFilter{src: bufio.NewReader(strings.NewReader(src)), body: io.NopCloser(nil), max: maxMCPMessage,
		filter: func(data []byte) ([]byte, bool, bool) {
			seen = append(seen, string(data))
			return data, true, !strings.Contains(string(data), `"id":7`) // the first event is refused: the stream ends
		}}
	out, _ := io.ReadAll(f)
	if len(seen) != 1 || !strings.Contains(seen[0], `"id":7`) || len(out) != 0 {
		t.Fatalf("the filter saw %q, relayed %q", seen, out)
	}
}
