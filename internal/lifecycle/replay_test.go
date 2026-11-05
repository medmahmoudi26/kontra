package lifecycle

import (
	"io"
	"log/slog"
	"os"
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

// TestReplayTunnelLossBeforeReconnect replays the histories the code before changeReconnect recorded on the branches
// it gates (D348): a tunnel attempt lost after its first upstream task id that failed tunnel_lost (its replica killed;
// lostWith) and the cancel of a task whose sticky session was gone (cancelSessionGone, failed: tunnel_lost). They hold
// no tunnel-reconnect marker, so GetVersion answers workflow.DefaultVersion on replay and the workflow keeps the
// behaviour they recorded: a task in flight across the deploy replays as it ran. The histories recorded since (the
// e2e_reconnect_ files) carry the marker and replay through the wait.
func TestReplayTunnelLossBeforeReconnect(t *testing.T) {
	logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, name := range []string{
		"e2e_replica_kill_tunnel.json", "e2e_cancel_tunnel_session_gone.json", "tunnel_cancel_session_gone.json",
		"e2e_tunnel_kill.json", "e2e_input_required_sticky_session_gone.json", "e2e_tunnel_drain.json",
	} {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join("testdata", "histories", name)
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), changeReconnect) {
				t.Fatalf("%s carries the %s marker: it was not recorded before the change", name, changeReconnect)
			}
			r := worker.NewWorkflowReplayer()
			r.RegisterWorkflowWithOptions(TaskLifecycle, workflow.RegisterOptions{Name: WorkflowType})
			if err := r.ReplayWorkflowHistoryFromJSONFile(logger, f); err != nil {
				t.Fatalf("replay of %s: %v", f, err)
			}
		})
	}
}
