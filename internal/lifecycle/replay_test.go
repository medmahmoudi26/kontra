package lifecycle

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// TestReplayRecordedHistories replays every history in testdata/histories against TaskLifecycle (spec
// §14.4, D46): a change to the workflow that would break a running task fails here. The histories are
// recorded by TestQueueWriteOverTemporal with KONTRA_RECORD_HISTORIES=1 against the development server;
// test/e2e adds the e2e_ files its runs record (KONTRA_RECORD_HISTORIES=1, never over an existing file). A
// recording is never re-recorded over once a change is gated: the v0_ files were recorded before
// list-push-at-start and replay through workflow.DefaultVersion, which is what keeps that gate honest.
func TestReplayRecordedHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "histories", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no recorded histories in testdata/histories")
	}
	logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			r := worker.NewWorkflowReplayer()
			r.RegisterWorkflowWithOptions(TaskLifecycle, workflow.RegisterOptions{Name: WorkflowType})
			if err := r.ReplayWorkflowHistoryFromJSONFile(logger, f); err != nil {
				t.Fatalf("replay of %s: %v", f, err)
			}
		})
	}
}

// TestReplayDetectsAChangedWorkflow is the replay test's control: a workflow that schedules other
// commands under the same type fails to replay the same histories.
func TestReplayDetectsAChangedWorkflow(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "histories", "*.json"))
	if len(files) == 0 {
		t.Fatal("no recorded histories in testdata/histories")
	}
	changed := func(ctx workflow.Context, in Input) error {
		return workflow.Sleep(ctx, time.Minute)
	}
	logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(changed, workflow.RegisterOptions{Name: WorkflowType})
	err := r.ReplayWorkflowHistoryFromJSONFile(logger, files[0])
	// TMPRL1100 is the SDK's nondeterminism error.
	if err == nil || !(strings.Contains(err.Error(), "TMPRL1100") || strings.Contains(strings.ToLower(err.Error()), "nondeterministic")) {
		t.Fatalf("a changed workflow replayed %s: %v", files[0], err)
	}
}
