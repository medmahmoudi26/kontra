package hiring

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/sealing"
)

// r3-hiring-1 (D247): a gate's signed card for a sealed (and pinned) service whose interface URL names a path the
// hiring node would not seal: the card the node serves names the service's own path, and a real a2a-go client that
// follows it sends its message sealed. A non-GET request to a path that names no service is refused here.
func TestReview3InterfaceURLIsTheService(t *testing.T) {
	for _, ifPath := range []string{"/agents/hello", "/agents//hello", "/agents/hello/.well-known/x", "/.well-known/hello", "/mcp"} {
		t.Run(strings.ReplaceAll(ifPath, "/", "_"), func(t *testing.T) {
			node, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			public := "http://127.0.0.1:18443"
			g := newFakeGate(t, public)
			ext, _ := json.Marshal(map[string]any{"uri": sealed.URI, "required": false,
				"params": map[string]any{"recipients": []string{node.Recipient().String()}, "envelope": sealed.MediaType, "required": true}})
			unsigned := `{"name":"hello","description":"says hello","version":"1.0.0",` +
				`"supportedInterfaces":[{"url":"` + public + ifPath + `","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
				`"capabilities":{"streaming":false,"extensions":[` + string(ext) + `]},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
			sig, err := g.keys.Sign([]byte(unsigned), public+jws.JWKSPath, &jws.Upstream{SHA256: strings.Repeat("0", 64), URL: public + "/x", Verified: false})
			if err != nil {
				t.Fatal(err)
			}
			sb, _ := json.Marshal([]jws.Signature{sig})
			card := []byte(strings.TrimSuffix(unsigned, "}") + `,"signatures":` + string(sb) + `}`)

			var mu sync.Mutex
			var seen bytes.Buffer
			gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/.well-known/jwks.json":
					g.jwks.ServeHTTP(w, r)
				case r.Method == http.MethodGet && (r.URL.Path == "/agents/hello" || r.URL.Path == "/agents/hello/.well-known/agent-card.json"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(card)
				case r.Method == http.MethodPost:
					b, _ := io.ReadAll(r.Body)
					mu.Lock()
					seen.WriteString(r.URL.Path + " " + string(b) + "\n")
					mu.Unlock()
					var req struct {
						ID json.RawMessage `json:"id"`
					}
					_ = json.Unmarshal(b, &req)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":{"message":{"messageId":"m2","role":"ROLE_AGENT","parts":[{"text":"ok"}]}}}`)
				default:
					http.NotFound(w, r)
				}
			})
			tun := &fakeTunnel{gate: gate}
			gateURL, _ := url.Parse(public)
			s, err := New(Config{Listen: "127.0.0.1:0", Gate: gateURL}, tun, nil)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			jobs, err := nodekey.OpenJobKeys(home, "127.0.0.1:18443")
			if err != nil {
				t.Fatal(err)
			}
			ps, err := pins.Open(filepath.Join(home, nodekey.DirName, pins.FileName))
			if err != nil {
				t.Fatal(err)
			}
			hs, err := sealing.NewHiring(sealing.HiringConfig{Jobs: jobs, Pins: ps, Opts: pins.Options{PinRecipients: true}, Gate: public, Card: s.Card}, nil)
			if err != nil {
				t.Fatal(err)
			}
			s.SetSealing(hs)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { _ = s.Serve(ctx, ln); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			base := "http://" + ln.Addr().String()
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			eventually(t, func() bool { return s.Local().Port() == port })

			resolved, err := agentcard.DefaultResolver.Resolve(context.Background(), base+"/agents/hello")
			if err != nil {
				t.Fatal(err)
			}
			if pin, ok, _ := ps.Get(public, "hello"); !ok || !pin.Sealed {
				t.Fatalf("the service is not pinned sealed: %+v %v", pin, ok)
			}
			for _, i := range resolved.SupportedInterfaces {
				if i.URL != base[:len("http://")]+"localhost:"+port+"/agents/hello" {
					t.Fatalf("the served card's interface: %s", i.URL)
				}
			}
			cl, err := a2aclient.NewFromCard(context.Background(), resolved)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = cl.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("SECRET-MARKER-R3"))})
			// A client sent straight to the gate's path is refused here, or sealed (/mcp is the gate's own MCP server,
			// relay.md §12, no sealed service's).
			if ifPath == "/mcp" {
				ifPath = "/agents/hello"
			}
			res, err := http.Post(base+ifPath, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"SECRET-MARKER-R3"}]}}}`))
			if err == nil {
				_ = res.Body.Close()
			}
			mu.Lock()
			got := seen.String()
			mu.Unlock()
			if strings.Contains(got, "SECRET-MARKER-R3") {
				t.Fatalf("the gate received the sealed service's message in plaintext: %.400s", got)
			}
		})
	}
}
