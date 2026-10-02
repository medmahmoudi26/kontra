package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	enumspb "go.temporal.io/api/enums/v1"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// fixedTime keeps list_workers' lastPoll deterministic across the test.
var fixedTime = time.Unix(1700000000, 0)

// fakeMCPDescriber is a Temporal describer stand-in so list_workers never dials.
type fakeMCPDescriber struct{ byQueue map[string][]pollerInfo }

func (f *fakeMCPDescriber) Pollers(_ context.Context, queue string, _ enumspb.TaskQueueType) ([]pollerInfo, error) {
	return f.byQueue[queue], nil
}
func (f *fakeMCPDescriber) Close() {}

// fakeMCPDocker is an imageAPI stand-in so deploy_actor never touches a daemon: the base
// image reads as present (no build), a build streams one line, tag/push are no-ops.
type fakeMCPDocker struct{}

func (fakeMCPDocker) ImageList(context.Context, image.ListOptions) ([]image.Summary, error) {
	// Non-empty AND carrying the checkout's own SDK digest, so ensureBase reads the base as
	// current and skips the build. Present-but-unlabelled is no longer enough: that is exactly
	// the stale base ensureBase now exists to catch, and returning it here would tar the whole
	// repo on every run of this test.
	lbl := map[string]string{}
	if root, err := cliutil.FindRepoRoot(""); err == nil {
		if d, err := sdkDigest(root); err == nil {
			lbl[sdkLabel] = d
		}
	}
	return []image.Summary{{Labels: lbl}}, nil
}
func (fakeMCPDocker) ImageBuild(_ context.Context, _ io.Reader, _ types.ImageBuildOptions) (types.ImageBuildResponse, error) {
	return types.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"stream":"built one layer\n"}`))}, nil
}
func (fakeMCPDocker) ImageTag(context.Context, string, string) error { return nil }
func (fakeMCPDocker) ImagePush(context.Context, string, image.PushOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// fakeScaleDocker is a STATEFUL containerAPI stand-in: it tracks created worker containers
// in memory so scale up→down→zero reconciles against a real fleet, no daemon touched.
type fakeScaleDocker struct {
	created  map[string]string // name → id
	noLocal  bool              // ImageList returns empty → registry fallback + pull
	pulled   []string          // refs pulled
	startErr error             // if set, ContainerStart fails (exercises orphan cleanup)
}

func (f *fakeScaleDocker) ImageList(context.Context, image.ListOptions) ([]image.Summary, error) {
	if f.noLocal {
		return nil, nil // no local worker image → registry fallback + pull
	}
	return []image.Summary{{}}, nil // local worker image present → kontra/<name>-worker:<ver>
}
func (f *fakeScaleDocker) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.pulled = append(f.pulled, ref)
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeScaleDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]types.Container, error) {
	out := make([]types.Container, 0, len(f.created))
	for name, id := range f.created {
		out = append(out, types.Container{ID: id, Names: []string{"/" + name}})
	}
	return out, nil
}
func (f *fakeScaleDocker) ContainerCreate(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	if f.created == nil {
		f.created = map[string]string{}
	}
	id := "id-" + name
	f.created[name] = id
	return container.CreateResponse{ID: id}, nil
}
func (f *fakeScaleDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	return f.startErr
}
func (f *fakeScaleDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	for name, cid := range f.created {
		if cid == id {
			delete(f.created, name)
		}
	}
	return nil
}

// fakeOrch is a minimal orchestrator: the routes the MCP tools call. POST routes that carry a
// body (actors) echo it back, so a tool test can assert the payload the agent sent.
//
// IT MUST ONLY SERVE ROUTES server.ts SERVES. It used to implement `POST /api/runs` as an
// unconditional 200 that echoed whatever body it got, which is how `dispatch_actor` kept passing
// after the real route stopped accepting `{graph}` — the fake was asserting the CLI agrees with
// itself. Adding a handler here is a claim about the server; check it against server.ts first.
func fakeOrch(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/actors", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { // register_actor — echo the upserted record
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"key": "echo@0.1.0", "name": "echo", "version": "0.1.0",
			"operations": []map[string]any{{"name": "run"}}, "digest": "sha256:d",
		}})
	})
	// run_status — GET /api/runs/:runId, the route the server actually serves. It answers in
	// BOTH dimensions; `/status` and `/output` went with the interpreter, and this fake used to
	// implement them, which is how three tools kept passing against routes that were gone.
	mux.HandleFunc("/api/runs/wf1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runId": "wf1", "execution": "running", "lifecycle": "executing", "settled": false,
		})
	})
	// recover_run — GET /api/runs/:runId by the durable server-minted id.
	mux.HandleFunc("/api/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"runId": "r1", "execution": "completed"})
	})
	mux.HandleFunc("/api/datasets", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"actor": "echo", "version": "0.1.0", "runId": "r1"}})
	})
	mux.HandleFunc("/api/runs/r1/explore", func(w http.ResponseWriter, r *http.Request) {
		// query_dataset now reads the explore manifest (per-actor datasets), not the removed
		// /api/datasets/urls. The bearer is sent from the env; the fake accepts any.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"datasets": []map[string]any{{
				"actor": "echo", "version": "0.1.0", "dt": "2026-08-03T17-00-00", "view": "echo",
				"state": "complete", "rows": 1,
				"urls": []string{"http://blob/output/echo/version=0.1.0/dt=2026-08-03T17-00-00/data.parquet"},
			}},
		})
	})
	return httptest.NewServer(mux)
}

func newTestServer(t *testing.T) (*mcpServer, func()) {
	srv := fakeOrch(t)
	return &mcpServer{api: newAPI(srv.URL)}, srv.Close
}

// callTool drives a tools/call and returns (text, isError).
func callTool(t *testing.T, s *mcpServer, name string, args map[string]any) (string, bool) {
	t.Helper()
	argsRaw, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(argsRaw)})
	resp := s.handle(&rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: params})
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/call %s returned no result", name)
	}
	res := resp.Result.(map[string]any)
	isErr, _ := res["isError"].(bool)
	content := res["content"].([]map[string]any)
	return content[0]["text"].(string), isErr
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	s, done := newTestServer(t)
	defer done()

	init := s.handle(&rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize"})
	r := init.Result.(map[string]any)
	if r["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v", r["protocolVersion"])
	}
	if r["serverInfo"].(map[string]any)["name"] != "kontra" {
		t.Fatalf("serverInfo = %v", r["serverInfo"])
	}

	// notifications get no reply
	if resp := s.handle(&rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); resp != nil {
		t.Fatalf("notification should get no reply, got %v", resp)
	}

	list := s.handle(&rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/list"})
	tools := list.Result.(map[string]any)["tools"].([]map[string]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl["name"].(string)] = true
	}
	for _, want := range []string{
		"list_actors", "describe_actor", "run_status",
		"list_datasets", "query_dataset", "recover_run",
		"register_actor", "list_workers", "deploy_actor", "scale_actor",
		// ADR 0037: `fleet_down` REFUSES a Fleet somebody is holding, so an agent that cannot list
		// the holders has only the refusal text to plan against. A tool that answers "why won't
		// this Fleet die" is the pair to a verb that will not do it.
		"fleet_leases",
	} {
		if !names[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
	// The tool list is what an agent PLANS against, so a tool whose route no longer exists is
	// worse than a missing one: the plan is made, then fails at the call. These four went with
	// the interpreter (ADR 0023 §12) — two graph verbs, a run-output reader the Datasets tools
	// replaced, and `dispatch_actor`, whose `POST /api/runs {graph}` the route now refuses with
	// `{"error":"file is required"}` because a Run is one execution of a CALLER's workflow.
	for _, gone := range []string{"create_graph", "dispatch_graph", "query_output", "dispatch_actor", "test_execute"} {
		if names[gone] {
			t.Errorf("tools/list still advertises %q, whose route was deleted with the interpreter", gone)
		}
	}
}

// The agent loop over MCP tools: list -> watch. There is no dispatch step any more — an agent
// starts work with `kontra workflow serve|start`, not a tool (ADR 0023 §12).
func TestMCPAgentLoop(t *testing.T) {
	s, done := newTestServer(t)
	defer done()

	// list_actors
	text, isErr := callTool(t, s, "list_actors", nil)
	if isErr || !strings.Contains(text, "echo") {
		t.Fatalf("list_actors = %q (isErr=%v)", text, isErr)
	}

	// run_status — both dimensions, from the one route that serves them.
	text, isErr = callTool(t, s, "run_status", map[string]any{"workflowId": "wf1"})
	if isErr || !strings.Contains(text, "execution") {
		t.Fatalf("run_status = %q", text)
	}
}

// The catalog/dataset tools that the agent-loop test doesn't exercise: each must hit its route
// and return the payload (not an error). register_actor echoes its body, so this also asserts
// the agent sent the right shape.
func TestMCPCatalogAndDatasetTools(t *testing.T) {
	s, done := newTestServer(t)
	defer done()

	// describe_actor resolves a bare name to its single registered version.
	text, isErr := callTool(t, s, "describe_actor", map[string]any{"name": "echo"})
	if isErr || !strings.Contains(text, `"version": "0.1.0"`) {
		t.Fatalf("describe_actor = %q (isErr=%v)", text, isErr)
	}

	// register_actor defaults key to name@version and posts name+version; the fake echoes it.
	text, isErr = callTool(t, s, "register_actor", map[string]any{"name": "echo", "version": "0.2.0"})
	if isErr {
		t.Fatalf("register_actor errored: %q", text)
	}
	var reg map[string]any
	_ = json.Unmarshal([]byte(text), &reg)
	if reg["key"] != "echo@0.2.0" || reg["name"] != "echo" || reg["version"] != "0.2.0" {
		t.Fatalf("register_actor sent %v — want key defaulted to echo@0.2.0", reg)
	}

	// list_datasets lists the materialized datasets.
	text, isErr = callTool(t, s, "list_datasets", nil)
	if isErr || !strings.Contains(text, "echo") {
		t.Fatalf("list_datasets = %q (isErr=%v)", text, isErr)
	}

	// query_dataset returns the run's per-actor datasets with presigned parquet URLs, read
	// from the explore manifest and filtered to the requested actor.
	text, isErr = callTool(t, s, "query_dataset", map[string]any{"actor": "echo", "version": "0.1.0", "runId": "r1"})
	if isErr || !strings.Contains(text, "output/echo/version=0.1.0") {
		t.Fatalf("query_dataset = %q (isErr=%v)", text, isErr)
	}

	// recover_run fetches a run by its runId.
	text, isErr = callTool(t, s, "recover_run", map[string]any{"runId": "r1"})
	if isErr || !strings.Contains(text, "completed") {
		t.Fatalf("recover_run = %q (isErr=%v)", text, isErr)
	}
}

// list_workers joins the catalog with LIVE Temporal pollers (faked here — tests never dial).
func TestMCPListWorkers(t *testing.T) {
	old := newDescriber
	newDescriber = func() (queueDescriber, error) {
		return &fakeMCPDescriber{byQueue: map[string][]pollerInfo{
			"echo-0.1.0": {{Identity: "worker-a", LastAccess: fixedTime}},
		}}, nil
	}
	t.Cleanup(func() { newDescriber = old })

	s, done := newTestServer(t)
	defer done()
	text, isErr := callTool(t, s, "list_workers", nil)
	if isErr {
		t.Fatalf("list_workers errored: %q", text)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("list_workers result not a JSON array: %q", text)
	}
	// echo@0.1.0 has a live poller; (orchestrator) queue has none.
	var echo map[string]any
	for _, r := range rows {
		if r["actor"] == "echo" {
			echo = r
		}
	}
	if echo == nil || echo["live"].(float64) != 1 || !strings.Contains(text, "worker-a") {
		t.Fatalf("list_workers rows = %v — want echo with live=1, worker-a", rows)
	}
}

// deploy_actor builds via a FAKE docker engine (no daemon) and, crucially, writes NO docker
// build output to the process stdout — that's the JSON-RPC channel. host_only skips push.
func TestMCPDeployActor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "actor.json"), []byte(`{"name":"echo","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := newDocker
	newDocker = func() (imageAPI, error) { return fakeMCPDocker{}, nil }
	t.Cleanup(func() { newDocker = old })

	s, done := newTestServer(t)
	defer done()

	var text string
	var isErr bool
	// Capture the process stdout across the call: the build stream must NOT leak onto it.
	leaked := withStdout(t, func() { text, isErr = callTool(t, s, "deploy_actor", map[string]any{"actor": dir, "host_only": true}) })
	if isErr {
		t.Fatalf("deploy_actor errored: %q", text)
	}
	if leaked != "" {
		t.Fatalf("deploy_actor leaked docker output onto the JSON-RPC stdout: %q", leaked)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(text), &res)
	if res["hostImage"] != "kontra/echo:0.1.0" || res["image"] != nil {
		t.Fatalf("deploy_actor (host_only) result = %v — want hostImage set, image empty", res)
	}
}

// scale_actor reconciles worker containers toward `replicas` — run (1), scale out (2),
// scale in (1), stop (0) — against a stateful fake engine (no daemon).
func TestMCPScaleActor(t *testing.T) {
	fake := &fakeScaleDocker{created: map[string]string{}}
	old := newContainerDocker
	newContainerDocker = func() (containerAPI, error) { return fake, nil }
	t.Cleanup(func() { newContainerDocker = old })

	s, done := newTestServer(t)
	defer done()

	scaleTo := func(n int) map[string]any {
		text, isErr := callTool(t, s, "scale_actor", map[string]any{"name": "echo", "replicas": n})
		if isErr {
			t.Fatalf("scale_actor(%d) errored: %q", n, text)
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatalf("scale_actor(%d) result not JSON: %q", n, text)
		}
		return r
	}

	// run 2 workers
	r := scaleTo(2)
	if int(r["running"].(float64)) != 2 || len(fake.created) != 2 {
		t.Fatalf("scale→2: running=%v created=%d", r["running"], len(fake.created))
	}
	if r["image"] != "kontra/echo-worker:0.1.0" {
		t.Fatalf("expected local self-contained worker image, got %v", r["image"])
	}
	// scale in to 1 — the newest index is removed, index 0 kept
	r = scaleTo(1)
	if int(r["running"].(float64)) != 1 || len(fake.created) != 1 {
		t.Fatalf("scale→1: running=%v created=%d", r["running"], len(fake.created))
	}
	if _, keptZero := fake.created["kontra-echo-0.1.0-0"]; !keptZero {
		t.Fatalf("scale-in should keep the lowest index; have %v", fake.created)
	}
	// stop all
	r = scaleTo(0)
	if int(r["running"].(float64)) != 0 || len(fake.created) != 0 {
		t.Fatalf("scale→0: running=%v created=%d", r["running"], len(fake.created))
	}
}

// F2: with no local worker image, scale resolves to the registry image and PULLS it before
// starting a worker (else ContainerCreate would fail with "No such image").
func TestMCPScaleActorPullsRegistryImage(t *testing.T) {
	fake := &fakeScaleDocker{created: map[string]string{}, noLocal: true}
	old := newContainerDocker
	newContainerDocker = func() (containerAPI, error) { return fake, nil }
	t.Cleanup(func() { newContainerDocker = old })

	s, done := newTestServer(t)
	defer done()
	text, isErr := callTool(t, s, "scale_actor", map[string]any{"name": "echo", "replicas": 1})
	if isErr {
		t.Fatalf("scale_actor errored: %q", text)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(text), &r)
	want := defaultRegistry + "/echo:0.1.0"
	if r["image"] != want {
		t.Fatalf("expected registry image %q, got %v", want, r["image"])
	}
	if len(fake.pulled) != 1 || fake.pulled[0] != want {
		t.Fatalf("expected a pull of %q, got %v", want, fake.pulled)
	}
}

// F1: when ContainerStart fails after a successful create, the created container is removed
// (not left orphaned carrying the managed label, which would poison the next reconcile).
func TestMCPScaleActorRemovesOrphanOnStartFailure(t *testing.T) {
	fake := &fakeScaleDocker{created: map[string]string{}, startErr: errors.New("boom")}
	old := newContainerDocker
	newContainerDocker = func() (containerAPI, error) { return fake, nil }
	t.Cleanup(func() { newContainerDocker = old })

	s, done := newTestServer(t)
	defer done()
	text, isErr := callTool(t, s, "scale_actor", map[string]any{"name": "echo", "replicas": 1})
	if !isErr || !strings.Contains(text, "boom") {
		t.Fatalf("expected a start-failure error result, got text=%q isErr=%v", text, isErr)
	}
	if len(fake.created) != 0 {
		t.Fatalf("start failure must leave NO orphaned container, have %v", fake.created)
	}
}

func TestMCPToolErrorIsResultNotProtocolError(t *testing.T) {
	s, done := newTestServer(t)
	defer done()
	// unknown actor -> resolveActor fails -> tool result with isError, not a JSON-RPC error
	text, isErr := callTool(t, s, "describe_actor", map[string]any{"name": "nope"})
	if !isErr || !strings.Contains(text, "not registered") {
		t.Fatalf("expected an isError tool result, got text=%q isErr=%v", text, isErr)
	}
}

// The stdio transport end-to-end: newline-delimited JSON-RPC in, newline-delimited out.
func TestMCPStdioTransport(t *testing.T) {
	s, done := newTestServer(t)
	defer done()

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // no reply
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_actors","arguments":{}}}`,
	}, "\n") + "\n")
	var out strings.Builder
	if err := s.serve(in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 { // initialize + tools/call replies; the notification produced none
		t.Fatalf("expected 2 response lines, got %d: %q", len(lines), out.String())
	}
	var initResp rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil || initResp.ID == nil {
		t.Fatalf("bad initialize response: %q", lines[0])
	}
	if !strings.Contains(lines[1], "echo") {
		t.Fatalf("tools/call response missing catalog: %q", lines[1])
	}
}

// What a person actually pastes.
//
// The loop this whole feature exists for is "draw it, copy the URL, hand it to an agent" — so what
// arrives at the tool is a LINK, not an id. A tool that took only a bare id would answer "not
// found" and put the burden of extracting one on the person who just copied the link, which is the
// opposite of the point.
func TestScratchIDTakesAUrlOrAnId(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8088/scratch/2f9ab1":          "2f9ab1",
		"https://kontra.run/scratch/2f9ab1":             "2f9ab1",
		"https://kontra.run/scratch/2f9ab1/":            "2f9ab1",
		"http://localhost:8088/scratch/2f9ab1?tab=spec": "2f9ab1",
		"http://localhost:8088/scratch/2f9ab1#notes":    "2f9ab1",
		// A bare id has no separators and falls through unchanged — the two spellings are one path.
		"  2f9ab1  ": "2f9ab1",
		"2f9ab1":     "2f9ab1",
		"":           "",
	}
	for in, want := range cases {
		if got := scratchID(in); got != want {
			t.Errorf("scratchID(%q) = %q, want %q", in, got, want)
		}
	}
}
