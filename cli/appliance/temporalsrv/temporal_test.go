package temporalsrv

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/components/nexusoperations"
)

// echoWorkflow is the smallest thing that produces a history worth surviving: one workflow task,
// one completion event carrying a result.
func echoWorkflow(_ workflow.Context, in string) (string, error) { return "echo:" + in, nil }

const testQueue = "appliance-restart-test"

// A RESTART MUST NOT BE A RESET, and this is the test that says so.
//
// ADR 0031 embeds Temporal in the binary, and the temptation of an embedded dev server is the
// in-memory one — it is faster, it needs no directory, and every test passes. It is also the
// version where `kontra runs` answers "no such run" about a run that finished an hour ago,
// where a control-plane restart loses every in-flight workflow's history, and where the recovery
// story that the whole system leans on does not exist. So: start, run a workflow to completion,
// stop the server, start a NEW one over the same data directory, and read the finished run back.
//
// Note what is asserted after the restart: not that the file exists, but that the workflow's
// RESULT and its history events come back. A schema that was re-created over the old file would
// pass a file-exists check and fail this one.
func TestRestartPreservesACompletedRunsHistory(t *testing.T) {
	dir := t.TempDir()
	port := freeTestPort(t)

	srv, stopFirst := startForTest(t, Options{DataDir: dir, Port: port})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := client.DialContext(ctx, client.Options{HostPort: srv.Address(), Namespace: DefaultNamespace})
	if err != nil {
		t.Fatalf("dialing the embedded server at %s: %v", srv.Address(), err)
	}
	w := worker.New(c, testQueue, worker.Options{})
	w.RegisterWorkflow(echoWorkflow)
	if err := w.Start(); err != nil {
		t.Fatalf("starting the worker: %v", err)
	}

	run, err := c.ExecuteWorkflow(ctx,
		client.StartWorkflowOptions{ID: "restart-survivor", TaskQueue: testQueue},
		echoWorkflow, "hello")
	if err != nil {
		t.Fatalf("starting the workflow: %v", err)
	}
	var got string
	if err := run.Get(ctx, &got); err != nil {
		t.Fatalf("waiting for the workflow: %v", err)
	}
	if got != "echo:hello" {
		t.Fatalf("workflow result = %q, want %q", got, "echo:hello")
	}
	wfID, runID := run.GetID(), run.GetRunID()

	// Down goes the control plane, worker and all.
	w.Stop()
	c.Close()
	stopFirst()

	// …and up comes a NEW server over the same directory. Same port, so anything that was talking
	// to the old one is talking to this one.
	restarted, _ := startForTest(t, Options{DataDir: dir, Port: port})
	if restarted.DatabasePath() != filepath.Join(dir, "temporal.db") {
		t.Fatalf("the restarted server opened %s, not the directory it was given", restarted.DatabasePath())
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	c2, err := client.DialContext(ctx2, client.Options{HostPort: restarted.Address(), Namespace: DefaultNamespace})
	if err != nil {
		t.Fatalf("dialing the restarted server: %v", err)
	}
	defer c2.Close()

	// The result — which lives in the completion event, so this is a history read.
	var after string
	if err := c2.GetWorkflow(ctx2, wfID, runID).Get(ctx2, &after); err != nil {
		t.Fatalf("reading the completed run after the restart: %v", err)
	}
	if after != "echo:hello" {
		t.Fatalf("after the restart the run returned %q, want %q", after, "echo:hello")
	}

	// The status, and the events themselves.
	desc, err := c2.DescribeWorkflowExecution(ctx2, wfID, runID)
	if err != nil {
		t.Fatalf("describing the completed run after the restart: %v", err)
	}
	if s := desc.GetWorkflowExecutionInfo().GetStatus(); s != enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		t.Fatalf("after the restart the run's status is %v, want COMPLETED", s)
	}
	iter := c2.GetWorkflowHistory(ctx2, wfID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	events := 0
	for iter.HasNext() {
		if _, err := iter.Next(); err != nil {
			t.Fatalf("reading history after the restart: %v", err)
		}
		events++
	}
	if events < 5 {
		t.Fatalf("history after the restart has %d events; a completed workflow has more", events)
	}
}

// The three settings the compose `temporal` service carried, READ BACK THROUGH THE SERVER'S OWN
// SETTINGS rather than looked up in our own map.
//
// The distinction is the whole test. Asserting `DynamicConfig()[someKey] == 2000` proves that we
// put 2000 in a map; asserting that the SETTING resolves to 2000 proves the key reaches it. That
// is the difference between a limit that is raised and a limit that is written down — and it is
// exactly the difference `limit.numPendingNexusOperations` fell into (see the test below).
func TestTheLoadBearingLimitsReachTheSettingsTheyName(t *testing.T) {
	col := dynamicconfig.NewCollection(DynamicConfig(), log.NewNoopLogger())
	const mib16 = 16 * 1024 * 1024

	// Blob size: a large root-seed set rides INLINE in the interpreter's workflow argument.
	if got := dynamicconfig.BlobSizeLimitError.Get(col)(DefaultNamespace); got != mib16 {
		t.Errorf("blobSize.error = %d, want %d (the default 2 MiB rejects a large seed set)", got, mib16)
	}
	if got := dynamicconfig.BlobSizeLimitWarn.Get(col)(DefaultNamespace); got != mib16 {
		t.Errorf("blobSize.warn = %d, want %d (left at 512 KiB it warns on every normal dispatch)", got, mib16)
	}
	// Pending Nexus operations: one per in-flight node, and KONTRA_MAX_INFLIGHT_NODES is 64.
	if got := nexusoperations.MaxConcurrentOperations.Get(col)(DefaultNamespace); got != 2000 {
		t.Errorf("pending Nexus operation cap = %d, want 2000 (the default is below KONTRA_MAX_INFLIGHT_NODES=64)", got)
	}
	// Search attributes: the orchestrator registers Kontra* at startup and upserts immediately.
	if got := dynamicconfig.ForceSearchAttributesCacheRefreshOnRead.Get(col)(); !got {
		t.Error("forceSearchAttributesCacheRefreshOnRead is false; a freshly registered search attribute would not be visible to the upsert that follows it")
	}
}

// THE COMPOSE SPELLING IS A DEAD KEY, and this test is here so that the claim is checkable rather
// than asserted in a comment.
//
// `docker-compose.yml` passes `--dynamic-config-value limit.numPendingNexusOperations=2000`. No
// such key is registered by the server, so the value never reaches the setting it was meant for
// and the live cap has been the packaged default all along. An unregistered key is not an error
// — that is the trap — so the only way to see it is to read the setting back.
func TestTheComposeSpellingOfTheNexusCapNeverReachedTheSetting(t *testing.T) {
	const composeKey = "limit.numPendingNexusOperations"
	col := dynamicconfig.NewCollection(
		dynamicconfig.StaticClient{dynamicconfig.MakeKey(composeKey): 2000},
		log.NewNoopLogger(),
	)
	if got := nexusoperations.MaxConcurrentOperations.Get(col)(DefaultNamespace); got == 2000 {
		t.Fatalf("%s now resolves to 2000 — the server registers it again, so DynamicConfig() should use it", composeKey)
	}
}

func TestStartTemporalRefusesWithoutADataDirectory(t *testing.T) {
	_, err := Start(Options{})
	if err == nil {
		t.Fatal("an embedded server with no data directory must be refused, not made ephemeral")
	}
	if !strings.Contains(err.Error(), "DataDir") {
		t.Errorf("the refusal must name the missing option, got: %v", err)
	}
}

// A hostname where an IP is wanted reaches the server config as a literal and the listener never
// binds — a failure that surfaces far from its cause.
func TestStartTemporalRefusesAHostnameAsBindAddress(t *testing.T) {
	_, err := Start(Options{DataDir: t.TempDir(), BindIP: "localhost"})
	if err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Fatalf("a hostname bind address must be refused by name, got: %v", err)
	}
}

// The collision with a control plane that is already running — the visible half of ADR 0031's
// open question about a machine that runs both topologies. It must name the port and suggest the
// compose service, because "address already in use" from inside fx does not.
func TestStartTemporalNamesThePortCollision(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	_, err = Start(Options{DataDir: t.TempDir(), Port: port})
	if err == nil {
		t.Fatal("starting on an occupied port must fail before the server boots")
	}
	for _, want := range []string{"already listening", "compose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the collision error must contain %q, got: %v", want, err)
		}
	}
}

// startForTest boots a server and returns it with an idempotent stop. The `once` is not
// decoration: the restart test stops the first server mid-test and cleanup would otherwise stop
// it a second time, and a Temporal server shut down twice panics on its own closed channels.
func startForTest(t *testing.T, opts Options) (*Server, func()) {
	t.Helper()
	srv, err := Start(opts)
	if err != nil {
		t.Fatalf("starting the embedded server: %v", err)
	}
	var once sync.Once
	stop := func() { once.Do(srv.Stop) }
	t.Cleanup(stop)
	return srv, stop
}

// freeTestPort picks a port the server can then bind. The listener is closed before we return it,
// which is what Start's own pre-flight check expects to find.
func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	return port
}

// THE NAMESPACE THE APPLIANCE CREATES MUST CARRY KONTRA'S SEARCH ATTRIBUTES, or no dispatch on it
// can ever succeed.
//
// The Go handler's backing workflow upserts `KontraRunId` and `KontraActor`; a namespace with no
// mapping for them fails every workflow task with `BadSearchAttributes` and Temporal retries the
// task forever — so the actor never runs, nothing errors, and the caller sees a schedule-to-close
// timeout naming a Nexus operation, three layers from the cause. MEASURED against
// `kontra up --orchestrator=none`, which until this was a control plane that could not run
// anything.
//
// The names are the contract with `control/orchestrator/src/visibility.ts` and with `handler/`. This test
// is a table, not a behaviour: it exists so that removing one is a failing build rather than a
// run that hangs.
func TestKontraSearchAttributesCoverWhatTheHandlerStamps(t *testing.T) {
	got := kontraSearchAttributes()
	for _, name := range []string{"KontraTenant", "KontraRunId", "KontraActor", "KontraTag"} {
		typ, ok := got[name]
		if !ok {
			t.Errorf("%s is not registered on the appliance's namespace; every workflow that stamps it will fail its task forever", name)
			continue
		}
		// KEYWORD, not TEXT: `visibility.ts` defines all four as keywords and a type mismatch is
		// its own `BadSearchAttributes`, with the same invisible failure mode.
		if typ != enumspb.INDEXED_VALUE_TYPE_KEYWORD {
			t.Errorf("%s = %v, want KEYWORD (the type control/orchestrator/src/visibility.ts defines)", name, typ)
		}
	}
	if len(got) != 4 {
		t.Errorf("the appliance registers %d search attributes; visibility.ts defines 4 — they must stay in step", len(got))
	}
}
