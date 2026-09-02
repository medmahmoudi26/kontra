package hitl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/sdk/go/hitl"
)

// WHAT THE UNIT TESTS CANNOT SEE, against a real server: that a parked run is readable with NO
// WORKER ANYWHERE.
//
// That is the whole reason an ask is a memo rather than a query, and it is the one claim the test
// environment cannot make — it has no server to describe and no worker to take away. So this file
// parks a run, STOPS THE WORKER, reads the ask over `DescribeWorkflowExecution` with nothing polling
// the queue, answers it, and brings a new worker up to take the answer.
//
//	KONTRA_TEMPORAL_LIVE=1 go test ./lib/hitl -run Live -v
//
// An ephemeral dev server from the Temporal CLI, in memory, leaving nothing behind.

const liveQueue = "kontra-hitl-live"

func devServer(t *testing.T) *testsuite.DevServer {
	t.Helper()
	if os.Getenv("KONTRA_TEMPORAL_LIVE") != "1" {
		t.Skip("set KONTRA_TEMPORAL_LIVE=1 to run against a real Temporal server")
	}
	exe := os.Getenv("KONTRA_TEMPORAL_CLI")
	if exe == "" {
		home, _ := os.UserHomeDir()
		if cached := filepath.Join(home, ".temporalio", "bin", "temporal"); fileExists(cached) {
			exe = cached
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	srv, err := testsuite.StartDevServer(ctx, testsuite.DevServerOptions{
		ExistingPath: exe,
		LogLevel:     "error",
	})
	require.NoError(t, err, "start dev server")
	t.Cleanup(func() { _ = srv.Stop() })
	return srv
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// gate parks on one ask and returns what the human said.
func gate(ctx workflow.Context, prompt string) (Approval, error) {
	return hitl.Ask[Approval](ctx, prompt,
		hitl.Context(map[string]any{"dataset": "live", "n": 12, "api_key": "s3cr3t"}),
		hitl.Deadline(time.Hour))
}

// twoGates parks on two asks at once — parallel branches each needing a decision, which is the case
// the read route serves as a LIST.
func twoGates(ctx workflow.Context) ([]string, error) {
	out := make([]string, 2)
	done := workflow.NewChannel(ctx)
	for i, prompt := range []string{"approve the crawl?", "approve the publish?"} {
		workflow.Go(ctx, func(gctx workflow.Context) {
			got, err := hitl.Ask[Approval](gctx, prompt, hitl.Deadline(time.Hour))
			if err != nil {
				out[i] = "error: " + err.Error()
			} else {
				out[i] = got.Note
			}
			done.Send(gctx, nil)
		})
	}
	done.Receive(ctx, nil)
	done.Receive(ctx, nil)
	return out, nil
}

// expiring parks on an ask nobody will answer.
func expiring(ctx workflow.Context) (Approval, error) {
	return hitl.Ask[Approval](ctx, "approve within two seconds?", hitl.Deadline(2*time.Second))
}

// waiting parks indefinitely — the explicit "this run does not proceed unattended".
func waiting(ctx workflow.Context) (Approval, error) {
	return hitl.Ask[Approval](ctx, "approve whenever", hitl.Indefinite())
}

func liveWorker(t *testing.T, c client.Client) worker.Worker {
	t.Helper()
	w := worker.New(c, liveQueue, worker.Options{})
	w.RegisterWorkflow(gate)
	w.RegisterWorkflow(twoGates)
	w.RegisterWorkflow(expiring)
	w.RegisterWorkflow(waiting)
	require.NoError(t, w.Start())
	return w
}

// describeAsks is the orchestrator's read, exactly: ONE RPC, no history scan, no worker.
func describeAsks(t *testing.T, c client.Client, id string) map[string]map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := c.DescribeWorkflowExecution(ctx, id, "")
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for key, p := range resp.GetWorkflowExecutionInfo().GetMemo().GetFields() {
		if len(key) < len(hitl.AskMemoPrefix) || key[:len(hitl.AskMemoPrefix)] != hitl.AskMemoPrefix {
			continue
		}
		var env map[string]any
		require.NoError(t, converter.GetDefaultDataConverter().FromPayload(p, &env))
		out[key] = env
	}
	return out
}

// awaitAsks polls the describe until `n` asks are published — the same poll a UI does, and the only
// honest way to know a run has parked.
func awaitAsks(t *testing.T, c client.Client, id string, n int) map[string]map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		asks := describeAsks(t, c, id)
		if len(asks) >= n {
			return asks
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s published %d asks, expected %d", id, len(asks), n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// THE WHOLE POINT, END TO END: park, take the worker away, read the ask anyway, answer it, and let a
// NEW worker pick the run back up. A query handler would have failed at step two, and a parked run
// is precisely the one most likely to outlive the process that parked it.
func TestLiveAParkedRunIsReadableWithNoWorkerAndAnswerableAfterOne(t *testing.T) {
	srv := devServer(t)
	c := srv.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	id := fmt.Sprintf("hitl-live-%d", time.Now().UnixNano())
	w := liveWorker(t, c)
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: liveQueue},
		gate, "Approve these 12 hosts?")
	require.NoError(t, err)

	asks := awaitAsks(t, c, id, 1)
	w.Stop() // nothing polls this run's queue from here until the answer is in

	// READ WITH NO WORKER ANYWHERE. One RPC, no history scan.
	asks = describeAsks(t, c, id)
	parked := asks[hitl.AskMemoPrefix+"ask-1"]
	require.NotNil(t, parked, "a parked run with no worker published no readable ask")
	require.Equal(t, "pending", parked["state"])
	require.Equal(t, "Approve these 12 hosts?", parked["prompt"])
	require.NotNil(t, parked["schema"], "the form has nothing to render from")
	material := parked["context"].(map[string]any)
	require.Equal(t, "live", material["dataset"])
	// A context reaches history in the clear, so the guard has to hold on the real wire too.
	require.Equal(t, hitl.Redacted, material["api_key"])

	// The answer is a durable signal: it lands whether or not anybody is polling.
	require.NoError(t, c.SignalWorkflow(ctx, id, "", hitl.AnswerSignalPrefix+"ask-1",
		map[string]any{"value": Approval{Approve: true, Note: "checked the sample"}, "by": "mo"}))

	// A NEW WORKER, which is a REPLAY: `Ask` re-runs, re-parks on the same question, and takes the
	// answer that was waiting. Nothing had to be reconciled.
	w2 := liveWorker(t, c)
	defer w2.Stop()

	var got Approval
	require.NoError(t, run.Get(ctx, &got))
	require.True(t, got.Approve)
	require.Equal(t, "checked the sample", got.Note)

	settled := describeAsks(t, c, id)[hitl.AskMemoPrefix+"ask-1"]
	require.Equal(t, "answered", settled["state"])
	require.Equal(t, "mo", settled["by"], "the operator label did not survive to the archive")
	require.NotContains(t, settled, "schema")
}

// WHAT THE ARCHIVED REDUCED LOG CAN SAY, and the reason an ask is emitted at all: the memo upsert is
// a history EVENT naming the ask, and the answer is a signal whose NAME carries the same id — so the
// pair survives in a log that decodes no payloads (ADR 0025).
func TestLiveTheAskAndItsAnswerAreBothHistoryEvents(t *testing.T) {
	srv := devServer(t)
	c := srv.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	id := fmt.Sprintf("hitl-live-%d", time.Now().UnixNano())
	w := liveWorker(t, c)
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: liveQueue},
		gate, "Approve?")
	require.NoError(t, err)

	awaitAsks(t, c, id, 1)
	require.NoError(t, c.SignalWorkflow(ctx, id, "", hitl.AnswerSignalPrefix+"ask-1",
		map[string]any{"value": Approval{Approve: true}, "by": "mo"}))
	require.NoError(t, run.Get(ctx, nil))

	var memoKeys, signals []string
	it := c.GetWorkflowHistory(ctx, id, run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for it.HasNext() {
		e, err := it.Next()
		require.NoError(t, err)
		switch e.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_PROPERTIES_MODIFIED:
			for k := range e.GetWorkflowPropertiesModifiedEventAttributes().GetUpsertedMemo().GetFields() {
				memoKeys = append(memoKeys, k)
			}
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
			signals = append(signals, e.GetWorkflowExecutionSignaledEventAttributes().GetSignalName())
		}
	}
	// One per ask and one per answer — the park and its ending.
	require.Equal(t, []string{hitl.AskMemoPrefix + "ask-1", hitl.AskMemoPrefix + "ask-1"}, memoKeys)
	// THE ID IS IN THE NAME, so an archive can pair the answer with its question from metadata alone.
	require.Equal(t, []string{hitl.AnswerSignalPrefix + "ask-1"}, signals)
}

// PARALLEL BRANCHES EACH NEEDING A DECISION: two asks live at once, both in ONE describe, answered
// out of order and each reaching its own question.
func TestLiveConcurrentAsksAppearInOneList(t *testing.T) {
	srv := devServer(t)
	c := srv.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	id := fmt.Sprintf("hitl-live-%d", time.Now().UnixNano())
	w := liveWorker(t, c)
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: liveQueue}, twoGates)
	require.NoError(t, err)

	asks := awaitAsks(t, c, id, 2)
	require.Len(t, asks, 2)
	for _, key := range []string{hitl.AskMemoPrefix + "ask-1", hitl.AskMemoPrefix + "ask-2"} {
		require.Equal(t, "pending", asks[key]["state"], key)
	}

	// The SECOND one first, which is what tells an id-in-the-name apart from one handler for all.
	require.NoError(t, c.SignalWorkflow(ctx, id, "", hitl.AnswerSignalPrefix+"ask-2",
		map[string]any{"value": Approval{Approve: true, Note: "second"}}))
	require.NoError(t, c.SignalWorkflow(ctx, id, "", hitl.AnswerSignalPrefix+"ask-1",
		map[string]any{"value": Approval{Approve: true, Note: "first"}}))

	var out []string
	require.NoError(t, run.Get(ctx, &out))
	require.Equal(t, []string{"first", "second"}, out)
}

// ON EXPIRY THE RUN FAILS WITH THE REASON ON IT, rather than reporting `running` with nothing moving.
func TestLiveADeadlineThatPassesExpiresTheAskAndFailsTheRun(t *testing.T) {
	srv := devServer(t)
	c := srv.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	id := fmt.Sprintf("hitl-live-%d", time.Now().UnixNano())
	w := liveWorker(t, c)
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: liveQueue}, expiring)
	require.NoError(t, err)

	err = run.Get(ctx, nil)
	require.Error(t, err)
	require.True(t, hitl.IsExpired(err), "an expiry that does not report as one: %v", err)

	settled := describeAsks(t, c, id)[hitl.AskMemoPrefix+"ask-1"]
	require.Equal(t, "expired", settled["state"])
	require.NotZero(t, settled["expiredAt"])
}

// AN INDEFINITE ASK DECLARES NO DEADLINE, which is what the route renders as "waits indefinitely"
// rather than as a countdown to a number nobody meant.
func TestLiveAnIndefiniteAskCarriesNoDeadline(t *testing.T) {
	srv := devServer(t)
	c := srv.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	id := fmt.Sprintf("hitl-live-%d", time.Now().UnixNano())
	w := liveWorker(t, c)
	defer w.Stop()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: liveQueue}, waiting)
	require.NoError(t, err)

	parked := awaitAsks(t, c, id, 1)[hitl.AskMemoPrefix+"ask-1"]
	require.NotContains(t, parked, "deadlineAt")

	require.NoError(t, c.SignalWorkflow(ctx, id, "", hitl.AnswerSignalPrefix+"ask-1",
		map[string]any{"value": Approval{Approve: true}}))
	require.NoError(t, run.Get(ctx, nil))
}
