package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/sealed"
)

// The M3 gate (D171, D172; spec/relay.md §16, spec/tunnel.md §16, spec/ext-sealed.md §9): sealing and MCP hosting
// through two gate replicas, Temporal, Postgres, a bucket, hosting and hiring nodes, the stock MCP servers and the
// dual-era double. The six gate tests the owner named: sealed_no_plaintext_test.go (TestSealedNoPlaintext),
// sealed_replay_test.go (TestSealedReplay), sealed_unsealed_part_test.go (TestSealedUnsealedPart),
// mcp_session_cross_node_test.go (TestMCPSessionCrossNode), mcp_tool_denied_test.go (TestMCPToolDenied) and
// mcp_task_resume_test.go (TestMCPTaskResume). With them: card_passthrough_test.go (TestCardPassthrough),
// mcp_misc_test.go (TestMCPBatchRefused, TestMCPMetaSpoof, TestMCPSessionRevoked, TestMCPCaseSmuggling,
// TestA2ACaseSmuggling), stdio_roots_test.go (TestStdioRoots), local_rebinding_test.go (TestLocalRebinding,
// TestHiring), mcp_modern_test.go (TestMCPModernTask, TestMCPEraFallback, TestMCPModernEverything2),
// mcp_peer_hop_test.go (TestMCPPeerHop) and sealed_blob_test.go (TestSealedBlob). This file holds what they share.

// startFakeNode dials a FakeNode (fakenode.go: it ignores the control stream and records every preamble the gate
// sends) through the Front, with a key of its own enrolled for each of its services, and waits for its tunnels row.
func startFakeNode(t *testing.T, ctx context.Context, h *Harness, name string, services map[string]FakeService) (*FakeNode, *nodekey.Identity) {
	t.Helper()
	key, _, err := nodekey.LoadOrCreate(filepath.Join(tempDir(t, h, "fake-"+name), "identity.key"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, svc := range slices.Sorted(maps.Keys(services)) {
		tok, err := h.EnrollToken(ctx, svc)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
	}
	fake, err := DialFakeNodeServices(ctx, h.Front.URL, key, services, tokens)
	if err != nil {
		t.Fatal(err)
	}
	h.OnClose(fake.Close)
	eventually(t, 15*time.Second, "the FakeNode's tunnels row", func() bool { return sessionReplica(ctx, h, fake.Session) != "" })
	return fake, key
}

// preamblesFor counts the gate-opened exchange streams a FakeNode received for a service.
func preamblesFor(f *FakeNode, service string) int {
	n := 0
	for _, p := range f.Preambles() {
		if p.Service == service {
			n++
		}
	}
	return n
}

// localA2A is a stock a2a-go client resolved from a hiring node's local card ({local}/agents/{svc}), with no
// credential: the node hires as its API key's principal.
func localA2A(t *testing.T, ctx context.Context, local, svc string) *a2aclient.Client {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	card, err := agentcard.DefaultResolver.Resolve(rctx, local+"/agents/"+svc)
	if err != nil {
		t.Fatalf("resolve %s's local card: %v", svc, err)
	}
	c, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Destroy() })
	return c
}

// sendTask sends msg and returns the task it answers with (blocking, so the answer is the turn's last state).
func sendTask(t *testing.T, ctx context.Context, c *a2aclient.Client, msg *a2a.Message) *a2a.Task {
	t.Helper()
	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := c.SendMessage(sctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage answered %T, want a task", res)
	}
	return task
}

func ptr[T any](v T) *T { return &v }

// waitSealedCard waits until both replicas serve svc's card with the sealed extension and a recipient (an age1 key:
// a node enrolled for it has its tunnel up), and returns it.
func waitSealedCard(t *testing.T, ctx context.Context, h *Harness, svc string) []byte {
	t.Helper()
	var body []byte
	eventually(t, 90*time.Second, svc+"'s sealed card with a recipient on both replicas", func() bool {
		for _, g := range h.Gates {
			b, st := getBody(ctx, g.URL+"/agents/"+svc+"/.well-known/agent-card.json", h.APIKey)
			if st != http.StatusOK || !bytes.Contains(b, []byte(sealed.URI)) || !bytes.Contains(b, []byte("age1")) {
				return false
			}
			body = b
		}
		return true
	})
	return body
}

// getBody GETs u with the key (none when "") and returns the body and the status.
func getBody(ctx context.Context, u, key string) ([]byte, int) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}

// scalar runs a query that returns one value.
func scalar[T any](t *testing.T, h *Harness, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := h.Pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// texts runs a query that returns one text column.
func texts(t *testing.T, h *Harness, sql string, args ...any) []string {
	t.Helper()
	rows, err := h.Pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// requireMarkerAbsent fails the test where the marker is found (Harness.ScanMarker).
func requireMarkerAbsent(t *testing.T, ctx context.Context, h *Harness, marker string) {
	t.Helper()
	found, err := h.ScanMarker(ctx, marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		var where []string
		for _, f := range found {
			where = append(where, f.String())
		}
		t.Fatalf("the marker %q was found in: %s", marker, strings.Join(where, "; "))
	}
}

// requireScanSees fails unless ScanMarker finds needle in a source whose name starts with each of prefixes: a
// positive control, showing that the scan reads the tables, logs, histories and objects it claims to.
func requireScanSees(t *testing.T, ctx context.Context, h *Harness, needle string, prefixes ...string) {
	t.Helper()
	found, err := h.ScanMarker(ctx, needle)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prefixes {
		seen := false
		for _, f := range found {
			seen = seen || strings.HasPrefix(f.Where, p)
		}
		if !seen {
			t.Fatalf("the scan does not see %q in %s: it would miss a marker there", needle, p)
		}
	}
}

// CallRow is one calls row.
type CallRow struct {
	Tool       string
	IsError    *bool
	TaskID     string
	MCPSession string
	Principal  string
}

// callsOf reads the service's calls rows, oldest first.
func callsOf(t *testing.T, h *Harness, service string) []CallRow {
	t.Helper()
	rows, err := h.Pool.Query(context.Background(), `select tool, is_error, coalesce(task_id::text, ''),
		coalesce(encode(mcp_session, 'hex'), ''), principal from kontra.calls where service = $1 order by at, id`, service)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CallRow, error) {
		var c CallRow
		err := r.Scan(&c.Tool, &c.IsError, &c.TaskID, &c.MCPSession, &c.Principal)
		return c, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustJSON marshals v.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fileContains reports whether the file holds s.
func fileContains(path, s string) bool {
	b, err := os.ReadFile(path)
	return err == nil && bytes.Contains(b, []byte(s))
}

// taskResultStream opens a tasks/result for taskID on c and reads it until the first server-to-client request, which
// it returns with the open stream; a stream that ends first, or none within the deadline, fails the test.
func firstRequest(t *testing.T, ctx context.Context, s *MCPStream, within time.Duration) (map[string]any, SSEEvent) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		ev, err := s.Next(rctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("the stream ended before a server-to-client request")
			}
			t.Fatalf("no server-to-client request on the stream within %v: %v", within, err)
		}
		m := ev.Message()
		if _, ok := m["method"]; ok && m["id"] != nil {
			return m, ev
		}
	}
}

// answerOn reads s until the response to a request (no method), and returns it.
func answerOn(t *testing.T, ctx context.Context, s *MCPStream, within time.Duration) map[string]any {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		ev, err := s.Next(rctx)
		if err != nil {
			t.Fatalf("no answer on the stream within %v: %v", within, err)
		}
		m := ev.Message()
		if _, isReq := m["method"]; !isReq && (m["result"] != nil || m["error"] != nil) {
			return m
		}
	}
}

// tempDir is a directory under the run's for a test's files.
func tempDir(t *testing.T, h *Harness, name string) string {
	t.Helper()
	d, err := os.MkdirTemp(h.Dir, name+"-")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// writeFile writes a file under dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
