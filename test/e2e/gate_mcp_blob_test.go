package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// merge-2 of M4's review (relay.md §10.4, §12.2; D303): a hire of a seller that answers a part over 256 KiB, made through
// a hiring node's /mcp (the README demo's path), answers that part as a resource_link to a presigned GET of the bucket,
// which a client fetches: never as kontra-blob:<ref>, which no client can fetch (a request on a hiring stream keeps
// refs, and the node inlines the parts of A2A answers only).
func TestGateMCPBlobThroughANode(t *testing.T) {
	h := start(t, Options{Tunnel: map[string]TunnelService{"kinds": {}}, Blobs: true})
	ctx := ctxFor(t, 5*time.Minute)
	seller := StartSeller(t)
	hostOn(t, ctx, h, h.Gates[0], map[string]string{"kinds": seller.URL})
	hirer, err := h.StartNode(ctx, NodeOptions{Name: "hirer", APIKey: h.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	local, err := hirer.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mc := NewMCPClient(local+"/mcp", "")
	mc.Capabilities = `{}`
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatalf("/mcp through the node: %v", err)
	}
	r, err := mc.Call(ctx, 2, "hire", map[string]any{"service": "kinds", "message": "big:1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text(), "kontra-blob:") {
		t.Fatalf("the hire's answer through the node names a kontra-blob: ref:\n%.2000s", r.Text())
	}
	res := Tool(t, r)
	if res.IsError || res.State() != "TASK_STATE_COMPLETED" {
		t.Fatalf("the hire through the node: %.2000s", r.Text())
	}
	if n := count(t, h, `select count(*) from kontra.blobs where task_id::text = $1`, res.TaskID()); n != 1 {
		t.Fatalf("%d blobs rows for the hire task, want its one large part", n)
	}
	var links []string
	for _, c := range res.Content {
		if c.Type == "resource_link" {
			links = append(links, c.URI)
		}
	}
	if len(links) != 1 || !strings.HasPrefix(links[0], "http") {
		t.Fatalf("the large part's resource links %q, want one presigned GET", links)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, links[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(b) != len("0:")+BigPart || !strings.HasPrefix(string(b), "0:bbb") {
		t.Fatalf("the presigned GET: %d, %d bytes", resp.StatusCode, len(b))
	}
}
