package temporalhost

// A retry resumes from the heartbeat, through Temporal's own activity seam (PRD D1, §10.3; ADR 0060)
// — the Go peer of tests/test_resume_from_checkpoint.py.
//
// The engine tests hand the checkpoint in with ResumeFrom. These go through the HOST's RunBatch
// inside the SDK's TestActivityEnvironment, so the two halves an engine test cannot see are real:
// what RecordHeartbeat actually carried out of attempt 1, and what GetHeartbeatDetails actually
// decodes for attempt 2.
//
// "A NEW PROCESS" IS MODELLED, NOT ASSUMED: attempt 2 runs on a fresh activation table over a state
// store that holds nothing — which is also the proof that resume no longer reads Redis. The object
// store is the one thing the attempts share, because it is the one thing that outlives the worker.
//
// "A NEW EXECUTION" IS MODELLED THE SAME WAY, with the one difference that is the point: it has NO
// heartbeat details. Temporal hands details to the next attempt of one activity and never across
// executions, so a re-dispatch of the same Batch can only resume from what it LISTS (owner decision A7).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/medmahmoudi26/kontra/runtime/go/engine"
	"github.com/medmahmoudi26/kontra/runtime/go/unitstore"
	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// objects is an in-memory S3: the pushed-record blobs and the commit objects both attempts see.
type objects struct{ m map[string][]byte }

func (o *objects) Put(_ context.Context, k string, b []byte) error {
	if o.m == nil {
		o.m = map[string][]byte{}
	}
	o.m[k] = b
	return nil
}

func (o *objects) Get(_ context.Context, k string) ([]byte, error) {
	b, ok := o.m[k]
	if !ok {
		return nil, unitstore.ErrNotFound
	}
	return b, nil
}

// List is a prefix LIST, in no promised order — what a fresh execution resumes from.
func (o *objects) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for k := range o.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func (o *objects) rows() int {
	n := 0
	for k := range o.m {
		if strings.HasPrefix(k, "units/") {
			n++
		}
	}
	return n
}

const actorID = "run1-n1"

// attempt runs RunBatch once in its own activity environment — one worker PROCESS — with `details`
// as the previous attempt's heartbeat (nil on a first attempt). Returns the error and every beat.
func attempt(t *testing.T, store *objects, m core.MethodFunc, units []any, details any) (*engine.RunBatchResp, error, []map[string]any) {
	t.Helper()
	return attemptOf(t, store, m, engine.RunBatchReq{
		ActorID: actorID, Method: "scan", Units: units, RunID: "r", NodeID: "n"}, details)
}

// attemptOf is attempt with the whole request in the caller's hands: the instance, the run and the
// dispatch's node id are what a cross-execution resume is keyed by, or deliberately not.
func attemptOf(t *testing.T, store *objects, m core.MethodFunc, req engine.RunBatchReq, details any) (*engine.RunBatchResp, error, []map[string]any) {
	t.Helper()
	r := &core.Registry{Name: "t"}
	r.AddMethod("scan", m)
	engine.ConfigureWith(r, unitstore.New(store, ""))

	h := newActivities("t", "1", nil, 1)
	h.s.live[req.ActorID] = engine.New(req.ActorID, memState{}) // a state hash that holds nothing

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(h.RunBatch, activity.RegisterOptions{Name: "RunBatch"})
	var beats []map[string]any
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, d converter.EncodedValues) {
		var beat map[string]any
		if err := d.Get(&beat); err == nil {
			beats = append(beats, beat)
		}
	})
	if details != nil {
		env.SetHeartbeatDetails(details)
	}
	val, err := env.ExecuteActivity("RunBatch", req)
	if err != nil {
		return nil, err, beats
	}
	var resp engine.RunBatchResp
	if err := val.Get(&resp); err != nil {
		t.Fatal(err)
	}
	return &resp, nil, beats
}

func units(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"v": fmt.Sprintf("u%d", i)}
	}
	return out
}

// dies commits k Units and then loses its resource — the Go engine's retryable death.
func dies(k int, ran *[]string) core.MethodFunc {
	return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for u := range b.All() {
			if len(*ran) == k {
				return &core.SessionLostError{Msg: "worker killed"}
			}
			*ran = append(*ran, u.Str("v"))
			ds.Push(map[string]any{"u": u.Str("v")})
		}
		return b.Err()
	}
}

func lives(ran *[]string) core.MethodFunc {
	return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for u := range b.All() {
			*ran = append(*ran, u.Str("v"))
			ds.Push(map[string]any{"u": u.Str("v")})
		}
		return b.Err()
	}
}

// THE acceptance property, at the unit level: attempt 1 commits k of n and the worker dies; attempt
// 2, on another process with an empty state hash, runs EXACTLY the n-k Units that did not commit, and
// the batch returns n Units of output, none twice.
func TestABatchKilledAfterKOfNCommitsResumesWithOnlyTheRest(t *testing.T) {
	const n, k = 7, 4
	store := &objects{}
	var ran1 []string
	_, err, beats1 := attempt(t, store, dies(k, &ran1), units(n), nil)
	if err == nil {
		t.Fatal("attempt 1 must fail")
	}
	if len(ran1) != k || len(beats1) == 0 {
		t.Fatalf("attempt 1 ran %v and beat %d times", ran1, len(beats1))
	}

	var ran2 []string
	resp, err, beats2 := attempt(t, store, lives(&ran2), units(n), beats1[len(beats1)-1])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ran2) != "[u4 u5 u6]" {
		t.Errorf("attempt 2 ran %v, want exactly the n-k Units that had not committed", ran2)
	}
	if !resp.Done || len(resp.Results) != n {
		t.Errorf("results = %d (done=%v), want all %d", len(resp.Results), resp.Done, n)
	}
	if store.rows() != n {
		t.Errorf("store holds %d rows, want %d — none twice", store.rows(), n)
	}
	// The FIRST beat attempt 2 sends is the engine's seed, before Load and before the Method runs
	// (PR #43's review), and it already names exactly what attempt 1 finished — so a second death
	// at any point after it leaves attempt 3 no less than attempt 1 did. Its LAST beat is not
	// asserted, and that is not an oversight: the SDK throttles beats, and in this environment the
	// ones a SUCCESSFUL attempt sends after its first never reached the listener — they do not need
	// to, the result supersedes them.
	if len(beats2) == 0 {
		t.Fatal("attempt 2 never beat")
	}
	first := beats2[0]["checkpoint"].(map[string]any)
	if fmt.Sprint(first["done"]) != fmt.Sprintf("[[0 %d]]", k-1) {
		t.Errorf("attempt 2's first checkpoint done = %v, want exactly attempt 1's units 0..%d", first["done"], k-1)
	}
	if beats2[0]["done"] != float64(k) {
		t.Errorf("attempt 2's first beat counts done = %v, want the %d folded back", beats2[0]["done"], k)
	}
}

// Heartbeat details from some other batch must not skip a single Unit.
func TestACheckpointForADifferentBatchIsDiscarded(t *testing.T) {
	store := &objects{}
	stale := map[string]any{"node": "n", "done": 3, "total": 3, "checkpoint": map[string]any{
		"v": 1, "batch_id": "0000000000000000", "done": [][]int{{0, 2}}, "failed": []int{},
		"manifest_ref": "commits/run=r/actor=t/actor_id=run1-n1/batch=0000000000000000/"}}
	var ran []string
	if _, err, _ := attempt(t, store, lives(&ran), units(3), stale); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ran) != "[u0 u1 u2]" {
		t.Errorf("ran %v, want every Unit — a stale checkpoint was applied by index", ran)
	}
}

// A Unit the checkpoint calls finished with no commit object is data the store lost. The activity
// fails NON-RETRYABLE and names it — the next attempt would read the same checkpoint and the same
// store — with the same error type Python's host raises.
func TestAMissingCommitObjectFailsTheActivityLoudlyAndWithoutRetry(t *testing.T) {
	store := &objects{}
	var ran []string
	_, _, beats := attempt(t, store, dies(2, &ran), units(3), nil)
	last := beats[len(beats)-1]
	ref := last["checkpoint"].(map[string]any)["manifest_ref"].(string)
	delete(store.m, unitstore.CommitKey(ref, 0))

	_, err, _ := attempt(t, store, lives(new([]string)), units(3), last)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) {
		t.Fatalf("err = %v, want an ApplicationError", err)
	}
	if app.Type() != "CommitLost" || !app.NonRetryable() {
		t.Errorf("type=%q non-retryable=%v, want CommitLost and non-retryable", app.Type(), app.NonRetryable())
	}
	if !strings.Contains(err.Error(), unitstore.CommitKey(ref, 0)) {
		t.Errorf("err = %v, want it to name the missing key", err)
	}
}

// ---- across executions: a re-dispatch resumes from a LISTING of the batch's commits (A7) ----------

func keyed(node, run string, n int) engine.RunBatchReq {
	return engine.RunBatchReq{ActorID: "acme.com", Method: "scan", Units: units(n), RunID: run, NodeID: node}
}

// THE cross-execution property (owner decision A7): execution 1 commits k of n and ends; the caller
// re-dispatches the SAME Batch on the SAME key — a new execution, a fresh process, NO heartbeat
// details, and a NEW node id, because a caller mints one per dispatch. It runs exactly the n-k Units
// that had not committed and returns n Units of output, none twice.
func TestAReDispatchOnTheSameKeyResumesFromTheCommitObjects(t *testing.T) {
	const n, k = 6, 4
	store := &objects{}
	var ran1 []string
	if _, err, _ := attemptOf(t, store, dies(k, &ran1), keyed("n-1", "r", n), nil); err == nil {
		t.Fatal("execution 1 must fail")
	}

	var ran2 []string
	resp, err, beats := attemptOf(t, store, lives(&ran2), keyed("n-2", "r", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ran2) != "[u4 u5]" {
		t.Errorf("execution 2 ran %v, want exactly the Units no earlier execution committed", ran2)
	}
	if !resp.Done || len(resp.Results) != n {
		t.Errorf("results = %d (done=%v), want all %d", len(resp.Results), resp.Done, n)
	}
	if store.rows() != n {
		t.Errorf("store holds %d rows, want %d — none twice", store.rows(), n)
	}
	if len(beats) == 0 || fmt.Sprint(beats[0]["checkpoint"].(map[string]any)["done"]) != fmt.Sprintf("[[0 %d]]", k-1) {
		t.Errorf("execution 2's first beat must already name the folded Units 0..%d: %v", k-1, beats)
	}
}

// An identical call made again on the same instance IS a retry (BatchID's rule), and every Unit of
// it already has its commit object — so nothing runs and the outputs come back.
func TestABatchAnEarlierExecutionFinishedRunsNothing(t *testing.T) {
	store := &objects{}
	var ran1, ran2 []string
	first, err, _ := attemptOf(t, store, lives(&ran1), keyed("n-1", "r", 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err, _ := attemptOf(t, store, lives(&ran2), keyed("n-2", "r", 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ran1) != 3 || len(ran2) != 0 {
		t.Errorf("ran %v then %v, want all three then none", ran1, ran2)
	}
	if fmt.Sprint(again.Results) != fmt.Sprint(first.Results) {
		t.Errorf("results %v, want the first execution's refs folded back %v", again.Results, first.Results)
	}
}

// The listing is keyed by the instance: another key running the same Units runs them all.
func TestAnotherInstanceDoesNotResumeThisOnesBatch(t *testing.T) {
	store := &objects{}
	if _, err, _ := attemptOf(t, store, dies(2, new([]string)), keyed("n", "r", 3), nil); err == nil {
		t.Fatal("execution 1 must fail")
	}
	var ran []string
	other := keyed("n", "r", 3)
	other.ActorID = "other.org"
	if _, err, _ := attemptOf(t, store, lives(&ran), other, nil); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 3 {
		t.Errorf("ran %v, want every Unit — another instance's commits were folded in", ran)
	}
}

// `run=` scopes the listing: a Unit's output is refs into its run's `units/` prefix, and folding
// them into another run would hand it rows its Dataset does not hold.
func TestAnotherRunDoesNotResumeThisRunsBatch(t *testing.T) {
	store := &objects{}
	if _, err, _ := attemptOf(t, store, dies(2, new([]string)), keyed("n", "r1", 3), nil); err == nil {
		t.Fatal("execution 1 must fail")
	}
	var ran []string
	if _, err, _ := attemptOf(t, store, lives(&ran), keyed("n", "r2", 3), nil); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 3 {
		t.Errorf("ran %v, want every Unit — another run's commits were folded in", ran)
	}
}

// The batch id hashes its Units, so an object under its prefix naming a Unit past its length is not
// this batch's: CommitLost, non-retryable, naming the key — and nothing runs.
func TestAListedUnitTheBatchCannotHoldFailsLoudlyAndWithoutRetry(t *testing.T) {
	store := &objects{}
	req := keyed("n", "r", 3)
	bid := engine.BatchID("scan", req.Units, nil)
	prefix := unitstore.CommitPrefix("", "r", "acme.com", bid)
	stray := unitstore.CommitKey(prefix, 7)
	_ = store.Put(context.Background(), stray, []byte(`{"v":1,"batch_id":"`+bid+`","unit":7,"out":[]}`))

	var ran []string
	_, err, _ := attemptOf(t, store, lives(&ran), req, nil)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != "CommitLost" || !app.NonRetryable() {
		t.Fatalf("err = %v, want a non-retryable CommitLost", err)
	}
	if !strings.Contains(err.Error(), stray) || len(ran) != 0 {
		t.Errorf("err = %v, ran %v — want the key named and nothing run", err, ran)
	}
}

// S3 is mandatory for an actor that commits Units (owner decision A8): Serve refuses one with no
// object store BEFORE it registers, serves metrics or dials anything, and names the variable. An
// actor that declares no Method commits nothing and is not refused.
func TestServeRefusesACommittingActorWithNoObjectStore(t *testing.T) {
	t.Setenv("KONTRA_S3_ENDPOINT", "")
	// EVERYTHING serve() WOULD REACH IS POINTED NOWHERE, so a refusal that regresses fails this
	// test fast instead of registering with, serving metrics beside, or polling whatever Temporal
	// answers on the default address of the machine running it. Found the hard way: with the
	// refusal deliberately disabled and nothing redirected, serve() went on to dial the default
	// address and sat there until the test binary's timeout killed it.
	t.Setenv("KONTRA_ORCHESTRATOR_URL", "")
	t.Setenv("KONTRA_METRICS_ADDR", "off")
	t.Setenv("KONTRA_ADDRESS", "127.0.0.1:1")
	stop := make(chan interface{})
	close(stop)
	r := &core.Registry{Name: "enrich"}
	r.AddMethod("scan", lives(new([]string)))
	err := serve(r, stop)
	if err == nil || !strings.Contains(err.Error(), "KONTRA_S3_ENDPOINT") || !strings.Contains(err.Error(), "enrich") {
		t.Fatalf("serve = %v, want a refusal naming the actor and KONTRA_S3_ENDPOINT", err)
	}
	if err := requireObjectStore(&core.Registry{Name: "passthrough"}); err != nil {
		t.Errorf("a load-only actor was refused: %v", err)
	}
	engine.ConfigureWith(r, unitstore.New(&objects{}, ""))
	if err := requireObjectStore(r); err != nil {
		t.Errorf("an actor with a store was refused: %v", err)
	}
}
