package catalog

// The write half of "provenance travels with the Batch", on the Go caller's side (ADR 0023 §22's
// reversal). What goes on the wire is the whole contract here — the activity is served by the
// orchestrator, in another language, and nothing on this side would notice a field going missing.

import (
	"context"
	"strings"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// publishes runs one Write inside a real workflow environment and returns the payload the
// publishBatch activity received.
func publishes(t *testing.T, w *DatasetWriter, b *Batch) map[string]any {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	var sent map[string]any
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) (map[string]any, error) {
			sent = in
			return map[string]any{"rows": 1}, nil
		}, activity.RegisterOptions{Name: PublishBatchActivity})

	wf := func(ctx workflow.Context) error {
		_, err := w.Write(ctx, b)
		return err
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the publishing workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	return sent
}

func TestWriteForwardsTheBatchsMachineAndItsRealVersion(t *testing.T) {
	// It used to send `"0"` for an unset version and no Machine at all, so a four-Machine run
	// landed every row carrying one node and one version between them. Both are facts about the
	// Method call that produced the rows, and the Batch is the only thing here that knows them.
	sent := publishes(t, Dataset("lame").Writer(), batchFromRef(
		BareRef{Sha256: "abc", Meta: map[string]string{"n": "3", "machine": "kf-dns-01"}},
		"nscheck", "0.1.0"))

	if sent["machine"] != "kf-dns-01" {
		t.Errorf("machine = %v, want kf-dns-01", sent["machine"])
	}
	if sent["version"] != "0.1.0" {
		t.Errorf("version = %v, want the Actor's real version, not the placeholder '0'", sent["version"])
	}
}

func TestWriteForwardsAnUnrecordedMachineAsUnrecorded(t *testing.T) {
	// A Batch paged straight out of a Dataset was produced by the lake, not by a Machine. It
	// must arrive EMPTY — the publish activity is what turns that into SQL NULL — and never as a
	// stand-in a reader would take for a real value.
	sent := publishes(t, Dataset("lame").Writer(), batchFromRef(
		BareRef{Sha256: "abc", Meta: map[string]string{"n": "1"}}, "", ""))

	if sent["machine"] != "" {
		t.Errorf("machine = %v, want empty (unrecorded)", sent["machine"])
	}
	if sent["version"] != "" {
		t.Errorf("version = %v, want empty (unrecorded), never '0'", sent["version"])
	}
}

// TempDataset is the Go peer of Python's `catalog.dataset.temp()` (temp-datasets slice 01): it
// mints a framework-derived `tmp_` name from the owning Run and records that Run through the one
// temp-specific activity, before any Batch lands. The wire is the whole contract — the activity is
// served by the orchestrator in another language — so the payload is what this pins.
func TestTempDatasetIsFrameworkNamedAndOwnedByItsRun(t *testing.T) {
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	var opened map[string]any
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) error {
			opened = in
			return nil
		}, activity.RegisterOptions{Name: OpenTempDatasetActivity})

	var name, owner string
	wf := func(ctx workflow.Context) error {
		w, err := TempDataset(ctx)
		if err != nil {
			return err
		}
		name = w.Name
		owner = workflow.GetInfo(ctx).WorkflowExecution.ID
		return nil
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the temp-opening workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}

	// The name is the framework's, derived from the Run, and visibly temporary — and it SAYS which
	// Run, which `tmp_<execution RunID>` did not (that is a different id from the owner, and it does
	// not survive continue-as-new).
	if !strings.HasPrefix(name, "tmp_") {
		t.Errorf("temp name = %q, want a tmp_ prefix so it is visibly temporary", name)
	}
	if !strings.Contains(name, tempSlug(owner)) {
		t.Errorf("temp name = %q, want the owning Run %q in it so it is traceable by eye", name, owner)
	}
	// Owned by its Run: the activity recorded the owning workflow id, and named the same Dataset.
	if opened["owner"] != owner {
		t.Errorf("recorded owner = %v, want the owning Run %q", opened["owner"], owner)
	}
	if opened["dataset"] != name {
		t.Errorf("recorded dataset = %v, want the minted temp name %q", opened["dataset"], name)
	}
}

// promotes runs one InsertFrom inside a real workflow environment and returns the payload the
// promoteDataset activity received (nil when InsertFrom errored before reaching it), and the error.
func promotes(
	t *testing.T, target *DatasetHandle, src PromotionSource, opts ...PageOption,
) (map[string]any, error) {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	var sent map[string]any
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) (map[string]any, error) {
			sent = in
			return map[string]any{"rows": 7}, nil
		}, activity.RegisterOptions{Name: PromoteDatasetActivity})

	wf := func(ctx workflow.Context) error {
		_, err := target.InsertFrom(ctx, src, opts...)
		return err
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the promoting workflow never completed")
	}
	return sent, env.GetWorkflowError()
}

func TestInsertFromCarriesTargetSourceAndSelectAndStampsNoPromoter(t *testing.T) {
	// Slice 02, Go side. Promotion sends the target, the source and the SELECT `where` built — and
	// NO runId/machine, which is load-bearing: promotion carries the producing Run's provenance,
	// so a promoter's identity on the wire would be the four-Machine `node='w'` bug again.
	sent, err := promotes(t, Dataset("lame"), &DatasetWriter{Name: "tmp_abc"}, Where("NOT ok"))
	if err != nil {
		t.Fatal(err)
	}
	if sent["target"] != "lame" {
		t.Errorf("target = %v, want lame", sent["target"])
	}
	if sent["source"] != "tmp_abc" {
		t.Errorf("source = %v, want tmp_abc", sent["source"])
	}
	if sent["sql"] != `SELECT * FROM "tmp_abc" WHERE NOT ok` {
		t.Errorf("sql = %v, want the where shorthand expanded over the source", sent["sql"])
	}
	if _, ok := sent["runId"]; ok {
		t.Error("runId rode along — promotion must not stamp the promoting workflow over the producer")
	}
	if _, ok := sent["machine"]; ok {
		t.Error("machine rode along — provenance is the source row's, never this call's")
	}
}

func TestInsertFromDefaultsToTheWholeSource(t *testing.T) {
	sent, err := promotes(t, Dataset("kept"), &DatasetWriter{Name: "tmp_abc"})
	if err != nil {
		t.Fatal(err)
	}
	if sent["sql"] != `SELECT * FROM "tmp_abc"` {
		t.Errorf("sql = %v, want a bare select over the whole source", sent["sql"])
	}
}

func TestInsertFromRefusesWhereAndQueryTogether(t *testing.T) {
	// The same "not both" rule the pager holds: two ways to name the filter is one too many.
	sent, err := promotes(t, Dataset("lame"), &DatasetWriter{Name: "tmp_abc"},
		Where("NOT ok"), Query("SELECT 1"))
	if err == nil {
		t.Fatal("passing Where AND Query should error")
	}
	if sent != nil {
		t.Error("the activity ran despite the conflicting options")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %v, want it to name the Where-OR-Query rule", err)
	}
}

func TestInsertFromRefusesAnUnopenedTemp(t *testing.T) {
	// A temp mints its name at open; a zero-value writer has none, and promoting it is a caller
	// ordering bug that must not build SQL against "".
	sent, err := promotes(t, Dataset("lame"), &DatasetWriter{})
	if err == nil {
		t.Fatal("promoting an unopened temp should error")
	}
	if sent != nil {
		t.Error("the activity ran against an unopened temp")
	}
	if !strings.Contains(err.Error(), "unopened") {
		t.Errorf("error = %v, want it to name the unopened temp", err)
	}
}

// tagAndPublish runs fn inside a workflow with BOTH write activities registered — tagDataset
// (capturing every record write, in order) and publishBatch (capturing every row publish) — and
// returns what each received. The KontraTag mirror is a workflow-internal UpsertTypedSearchAttributes
// command, not an activity, so it is not captured here; the workflow completing without error is
// what proves the command was accepted (ADR 0029 §4).
func tagAndPublish(
	t *testing.T, b *Batch, fn func(workflow.Context) error,
) (tagged []map[string]any, published []map[string]any) {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	// The Into flow reaches a Method call's Nexus dispatch before it publishes, so mock it too —
	// returning the same ref the direct-Write tests hand in. Unused by the direct-Write tests, which
	// never dispatch, and an un-consumed mock is harmless.
	env.OnNexusOperation(ServiceName, runOp, mock.Anything, mock.Anything).
		Return(&nexus.HandlerStartOperationResultSync[BareRef]{Value: b.Ref}, nil).Maybe()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) (map[string]any, error) {
			tagged = append(tagged, in)
			return map[string]any{"tags": []string{in["tag"].(string)}}, nil
		}, activity.RegisterOptions{Name: TagDatasetActivity})
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) (map[string]any, error) {
			published = append(published, in)
			return map[string]any{"rows": 1}, nil
		}, activity.RegisterOptions{Name: PublishBatchActivity})

	env.RegisterWorkflow(fn)
	env.ExecuteWorkflow(fn)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the tagging workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	return tagged, published
}

func taggedBatch() *Batch {
	return batchFromRef(BareRef{Sha256: "abc", Meta: map[string]string{"n": "3"}}, "nscheck", "0.1.0")
}

func TestWriteWithATagWritesTheRecordKeyedByTheRun(t *testing.T) {
	// AC 1 and 2 (ADR 0029 §4), Go side. A tagged destination's first Write lands the durable,
	// authoritative Dataset record — keyed by this Run's workflow id — through the tagDataset
	// activity, and only THEN mirrors to KontraTag. The mirror is a workflow command issued after
	// the activity's Get returns, so "record before Temporal" is structural; here the record write
	// itself is pinned. The workflow completing proves the KontraTag command was accepted for a live
	// run. The wire is the whole contract — the activity is served by the orchestrator in TS.
	var runID string
	tagged, published := tagAndPublish(t, taggedBatch(), func(ctx workflow.Context) error {
		runID = workflow.GetInfo(ctx).WorkflowExecution.ID
		_, err := Dataset("lame").WithTag("prod-sweep").Writer().Write(ctx, taggedBatch())
		return err
	})

	if len(tagged) != 1 {
		t.Fatalf("tagDataset ran %d times, want exactly 1", len(tagged))
	}
	if tagged[0]["tag"] != "prod-sweep" {
		t.Errorf("recorded tag = %v, want prod-sweep", tagged[0]["tag"])
	}
	if tagged[0]["runId"] != runID {
		t.Errorf("record keyed by %v, want the Run's workflow id %q", tagged[0]["runId"], runID)
	}
	if len(published) != 1 {
		t.Errorf("publishBatch ran %d times, want 1 (the rows still land)", len(published))
	}
}

func TestATagRidesThroughIntoOnAMethodCall(t *testing.T) {
	// The caller flow ADR 0028 §2 makes canonical: the Method call publishes into the named
	// destination, so a tagged destination tags the Run without the caller writing a publish line.
	tagged, published := tagAndPublish(t, taggedBatch(), func(ctx workflow.Context) error {
		dest := Dataset("lame").WithTag("prod-sweep")
		_, _, err := Actor("nscheck", "0.1.0").Call(ctx, "ask", []any{1, 2, 3}, Into(dest))
		return err
	})
	if len(tagged) != 1 || tagged[0]["tag"] != "prod-sweep" {
		t.Errorf("Into(tagged handle) did not tag the Run: %v", tagged)
	}
	if len(published) != 1 {
		t.Errorf("publishBatch ran %d times, want 1", len(published))
	}
}

func TestTheTagIsAppliedOnceAcrossAStreamingPublish(t *testing.T) {
	// A destination named once and published into per chunk pays for the record write and the
	// KontraTag mirror ONCE — the tagged guard, so a long streaming publish is not N record writes.
	tagged, published := tagAndPublish(t, taggedBatch(), func(ctx workflow.Context) error {
		w := Dataset("lame").WithTag("prod-sweep").Writer()
		if _, err := w.Write(ctx, taggedBatch()); err != nil {
			return err
		}
		if _, err := w.Write(ctx, taggedBatch()); err != nil {
			return err
		}
		return nil
	})
	if len(tagged) != 1 {
		t.Errorf("tagDataset ran %d times across two Writes, want exactly 1", len(tagged))
	}
	if len(published) != 2 {
		t.Errorf("publishBatch ran %d times, want 2 (every chunk still lands)", len(published))
	}
}

func TestAnUntaggedWriteNeverTouchesTheRecord(t *testing.T) {
	// No tag means no record and no mirror — only DEVIATION is stored (ADR 0029 §1). A plain Write
	// must not manufacture a tag row.
	tagged, published := tagAndPublish(t, taggedBatch(), func(ctx workflow.Context) error {
		_, err := Dataset("lame").Writer().Write(ctx, taggedBatch())
		return err
	})
	if len(tagged) != 0 {
		t.Errorf("tagDataset ran %d times for an untagged writer, want 0", len(tagged))
	}
	if len(published) != 1 {
		t.Errorf("publishBatch ran %d times, want 1", len(published))
	}
}

func TestWithTagDoesNotMutateTheSharedReadHandle(t *testing.T) {
	// WithTag returns a COPY: a read handle reused for paging must not become a write policy that
	// tags a Run behind the caller's back.
	base := Dataset("lame")
	_ = base.WithTag("prod-sweep")
	if base.tag != "" {
		t.Errorf("WithTag mutated the base handle's tag to %q, want it left empty", base.tag)
	}
}

func TestAnExplicitlyVersionedWriterStillWins(t *testing.T) {
	// `Dataset(name).Writer()` on a versioned handle is a caller asking for that partition on
	// purpose, which is a different thing from the empty default the Batch now fills. Deleting
	// the default must not delete the choice.
	w := &DatasetWriter{Name: "lame", Version: "pinned"}
	sent := publishes(t, w, batchFromRef(
		BareRef{Sha256: "abc", Meta: map[string]string{"n": "1", "machine": "kf-dns-01"}},
		"nscheck", "0.1.0"))

	if sent["version"] != "pinned" {
		t.Errorf("version = %v, want the caller's pinned one", sent["version"])
	}
	if sent["machine"] != "kf-dns-01" {
		t.Errorf("machine = %v; it is measured, never a caller's label", sent["machine"])
	}
}

// tempSlug's golden lived here and is GONE: it asserted
// "runs_2026-08-19T14_49_20_00_00_sweep" with the comment "want the same mapping Python's _slug
// makes", which was a value hand-copied out of an implementation nothing ran — _slug's own
// docstring cited a `tests/test_temp_dataset.py` that does not exist. It is
// conformance/slug.json now, driven from slug_conformance_test.go here, tests/test_slug_conformance.py
// in Python and backend/src/data/slug.conformance.test.ts in the orchestrator (ADR 0035 rule two).
