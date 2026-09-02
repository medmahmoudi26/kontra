package narrate_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/sdk/go/narrate"
)

// WHAT THE UNIT TESTS CANNOT SEE, against a real server.
//
// `narrate_test.go` asserts that `Say` issues one positive-duration timer, which is a decision this
// SDK owns. It cannot assert that the server ACCEPTS a 1ns timer, that the Summary survives onto
// `TimerStarted` rather than being dropped somewhere in the SDK — the test environment DOES drop it,
// which is exactly why this file exists — or what the whole thing costs a history. "The SDK writes a
// Summary nothing can read" is precisely the failure that would pass every test next door.
//
// The Python peer is tests/test_narration_live.py, and the numbers below are ITS numbers: five
// events a sentence, measured against Temporal Server 1.31.0. A Go narration that cost a different
// shape would be the two SDKs writing two different archives.
//
//	KONTRA_TEMPORAL_LIVE=1 go test ./lib/narrate -run Live -v
//
// It starts an ephemeral dev server from the Temporal CLI, in memory, and leaves nothing behind.

const liveQueue = "kontra-narrate-live"

// devServer is an ephemeral server for one test, or a skip when the box has no CLI to run one.
func devServer(t *testing.T) *testsuite.DevServer {
	t.Helper()
	if os.Getenv("KONTRA_TEMPORAL_LIVE") != "1" {
		t.Skip("set KONTRA_TEMPORAL_LIVE=1 to run against a real Temporal server")
	}
	// Whatever is cached is preferred to a download, so a box with no network still runs this.
	exe := os.Getenv("KONTRA_TEMPORAL_CLI")
	if exe == "" {
		home, _ := os.UserHomeDir()
		if cached := filepath.Join(home, ".temporalio", "bin", "temporal"); exists(cached) {
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

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// narrating says everything it is given, in order.
func narrating(ctx workflow.Context, sentences []string) (string, error) {
	for _, s := range sentences {
		if err := narrate.Say(ctx, s); err != nil {
			return "", err
		}
	}
	return "ok", nil
}

// silent is the control. It imports narrate with everything else and calls nothing.
func silent(workflow.Context) (string, error) { return "ok", nil }

func runLive(t *testing.T, c client.Client, wf any, arg ...any) []*historypb.HistoryEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	w := worker.New(c, liveQueue, worker.Options{})
	w.RegisterWorkflow(narrating)
	w.RegisterWorkflow(silent)
	require.NoError(t, w.Start())
	defer w.Stop()

	id := fmt.Sprintf("narrate-live-%d", time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: id, TaskQueue: liveQueue,
	}, wf, arg...)
	require.NoError(t, err)
	var out string
	require.NoError(t, run.Get(ctx, &out))

	var events []*historypb.HistoryEvent
	it := c.GetWorkflowHistory(ctx, id, run.GetRunID(), false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for it.HasNext() {
		e, err := it.Next()
		require.NoError(t, err)
		events = append(events, e)
	}
	return events
}

// THE ONE THING THAT MATTERS: the sentence is ON the event, in the field the reduced log reads. A
// Summary that did not survive the round trip would leave `control/orchestrator/src/history.ts` with nothing
// to put in the transcript, and every narration would render as a bare timer.
func TestLiveASentenceLandsOnTimerStartedAsUserMetadata(t *testing.T) {
	srv := devServer(t)
	said := []string{
		"41 of 119 apexes do not resolve",
		"dispatching the crawler over 78 live hosts",
	}
	events := runLive(t, srv.Client(), narrating, said)

	var summaries []string
	for _, e := range events {
		if e.GetEventType() == enumspb.EVENT_TYPE_TIMER_STARTED {
			summaries = append(summaries, summaryOf(t, e))
		}
	}
	require.Equal(t, said, summaries,
		"the sentences did not reach history as timer summaries, in order")
}

// FIVE EVENTS A SENTENCE, measured — the number MaxSentences is derived from, so it is asserted
// rather than remembered. `TimerStarted` carrying it, `TimerFired`, and the three-event workflow task
// the firing wakes.
func TestLiveASentenceCostsFiveHistoryEvents(t *testing.T) {
	srv := devServer(t)

	quiet := len(runLive(t, srv.Client(), silent))
	one := len(runLive(t, srv.Client(), narrating, []string{"one"}))
	three := len(runLive(t, srv.Client(), narrating, []string{"one", "two", "three"}))

	require.Equal(t, 5, one-quiet, "a sentence costs %d events, not 5", one-quiet)
	require.Equal(t, 15, three-quiet, "three sentences cost %d events, not 15", three-quiet)
}

// A 1ns timer is what Go's SDK forces (a zero one issues no command at all), so the server has to
// accept it. If it ever rejected a sub-millisecond timer, every narration would fail the run.
func TestLiveTheServerAcceptsTheSmallestPositiveTimer(t *testing.T) {
	srv := devServer(t)
	events := runLive(t, srv.Client(), narrating, []string{"a sentence"})

	var started, fired int
	for _, e := range events {
		switch e.GetEventType() {
		case enumspb.EVENT_TYPE_TIMER_STARTED:
			started++
		case enumspb.EVENT_TYPE_TIMER_FIRED:
			fired++
		case enumspb.EVENT_TYPE_TIMER_CANCELED:
			t.Errorf("a narration left a cancelled timer behind, which reads as a failure event")
		}
	}
	require.Equal(t, 1, started)
	require.Equal(t, 1, fired, "the timer was started but never fired")
}

// summaryOf reads the sentence off an event the way the reduced log does: user metadata, decoded
// with the data converter, never a payload of the workflow's own.
func summaryOf(t *testing.T, e *historypb.HistoryEvent) string {
	t.Helper()
	meta := e.GetUserMetadata()
	require.NotNil(t, meta, "a narration's event carried no user metadata")
	require.NotNil(t, meta.GetSummary(), "a narration's event carried no summary")
	var s string
	require.NoError(t, converter.GetDefaultDataConverter().FromPayload(meta.GetSummary(), &s))
	return s
}
