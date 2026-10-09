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
	r := &core.Registry{Name: "t"}
	r.AddMethod("scan", m)
	engine.ConfigureWith(r, unitstore.New(store, ""))

	h := newActivities("t", "1", nil, 1)
	h.s.live[actorID] = engine.New(actorID, memState{}) // a state hash that holds nothing

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
	val, err := env.ExecuteActivity("RunBatch", engine.RunBatchReq{
		ActorID: actorID, Method: "scan", Units: units, RunID: "r", NodeID: "n"})
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
	// The FIRST beat attempt 2 sends already names what attempt 1 finished, plus its own first
	// commit. Its LAST beat is not asserted, and that is not an oversight: the SDK throttles beats,
	// and in this environment the ones a SUCCESSFUL attempt sends after its first never reached the
	// listener — they do not need to, the result supersedes them. The first is what a second death
	// would leave behind.
	if len(beats2) == 0 {
		t.Fatal("attempt 2 never beat")
	}
	first := beats2[0]["checkpoint"].(map[string]any)
	if fmt.Sprint(first["done"]) != fmt.Sprintf("[[0 %d]]", k) {
		t.Errorf("attempt 2's first checkpoint done = %v, want units 0..%d", first["done"], k)
	}
}

// Heartbeat details from some other batch must not skip a single Unit.
func TestACheckpointForADifferentBatchIsDiscarded(t *testing.T) {
	store := &objects{}
	stale := map[string]any{"node": "n", "done": 3, "total": 3, "checkpoint": map[string]any{
		"v": 1, "batch_id": "0000000000000000", "done": [][]int{{0, 2}}, "failed": []int{},
		"manifest_ref": "commits/run=r/actor=t/shard=n/batch=0000000000000000/"}}
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
