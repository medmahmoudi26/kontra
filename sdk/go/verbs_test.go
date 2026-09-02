package kontra_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	kontra "github.com/medmahmoudi26/kontra-local/sdk/go"
	"github.com/medmahmoudi26/kontra-local/sdk/go/hitl"
	"github.com/medmahmoudi26/kontra-local/sdk/go/narrate"
)

// The two things a workflow says out loud, from the top level.
//
// `lib/narrate` and `lib/hitl` each hold one half in isolation and both are untouched. WHAT THIS
// FILE HOLDS IS THE PAIR: that `kontra.Speak` and `kontra.Ask` exist side by side, that they are
// the same mechanisms the owning packages implement rather than second ones, and that the
// distinction between them survives being easier to reach — `Speak` returns immediately, `Ask`
// stops the run until a human moves it.
//
// WHAT THIS ENVIRONMENT CAN AND CANNOT SEE, which is why the assertions are shaped as they are.
// `testWorkflowEnvironmentImpl.newTimer` takes `TimerOptions` and never reads them, so a narration's
// SENTENCE is dropped before anything here could assert it — the same limitation `narrate_test.go`
// records, and the reason `narrate_live_test.go` exists. What it CAN see is how many timers were
// scheduled and for how long, and — for an ask — the real serialised memo, merged into the
// execution's own `WorkflowInfo` exactly as a route would read it over `DescribeWorkflowExecution`.

const sentinel = "kontra-sentinel-9f2a1c-do-not-leak"

const answerSignal = hitl.AnswerSignalPrefix + "ask-1"

// approval is the answer's shape, and what `Ask[T]` derives a schema from.
type approval struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty"`
}

// timers records what a workflow scheduled: one entry per timer, in order.
type timers struct{ d []time.Duration }

func envFor(t *testing.T, wf any, rec *timers) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf)
	if rec != nil {
		env.SetOnTimerScheduledListener(func(_ string, d time.Duration) { rec.d = append(rec.d, d) })
	}
	return env
}

// ── the workflows under test ──

// speaking narrates every sentence through the TOP-LEVEL verb.
func speaking(ctx workflow.Context, sentences []string) (int, error) {
	for _, s := range sentences {
		if err := kontra.Speak(ctx, s); err != nil {
			return 0, err
		}
	}
	n, _ := narrate.Said(ctx)
	return n, nil
}

// saying is the same run through the OWNING package's name — the control the parity is measured
// against.
func saying(ctx workflow.Context, sentences []string) (int, error) {
	for _, s := range sentences {
		if err := narrate.Say(ctx, s); err != nil {
			return 0, err
		}
	}
	n, _ := narrate.Said(ctx)
	return n, nil
}

// speakingViaNarrate is the SECOND DOOR — the spelling a caller-only Go module reaches, because
// importing this package would link the actor host (redis, S3) into a binary that only writes
// workflows. Same word, same call; see verbs.go for why Go needs two doors where Python needs one.
func speakingViaNarrate(ctx workflow.Context, sentences []string) (int, error) {
	for _, s := range sentences {
		if err := narrate.Speak(ctx, s); err != nil {
			return 0, err
		}
	}
	n, _ := narrate.Said(ctx)
	return n, nil
}

// mixing spends one budget from both names, which is what "one function" has to mean.
func mixing(ctx workflow.Context, n int) (int, error) {
	for i := 0; i < n; i++ {
		if err := kontra.Speak(ctx, "unit dispatched"); err != nil {
			return 0, err
		}
	}
	if err := narrate.Say(ctx, "and one more, from the other name"); err != nil {
		return 0, err
	}
	said, _ := narrate.Said(ctx)
	return said, nil
}

type asked struct {
	Answer  approval                  `json:"answer"`
	Parked  map[string]map[string]any `json:"parked"`
	Settled map[string]map[string]any `json:"settled"`
	Failure string                    `json:"failure"`
}

// asking parks on one question through the TOP-LEVEL verb, and reports what the memo said before
// and after — the snapshot taken from a second coroutine, because the parked state is only visible
// while the first one is waiting.
func asking(ctx workflow.Context, context map[string]any) (asked, error) {
	var out asked
	done := workflow.NewChannel(ctx)
	workflow.Go(ctx, func(gctx workflow.Context) {
		_ = workflow.Sleep(gctx, time.Second)
		out.Parked = memoAsks(gctx)
		done.Send(gctx, nil)
	})

	got, err := kontra.Ask[approval](ctx, "Approve these 12 hosts?",
		hitl.Context(context), hitl.Deadline(time.Hour))
	done.Receive(ctx, nil)
	out.Answer = got
	out.Settled = memoAsks(ctx)
	if err != nil {
		out.Failure = err.Error()
	}
	return out, nil
}

// memoAsks is every `kontra.ask.*` entry this run has published, decoded — the same read the
// orchestrator's `readAsks` does over one RPC.
func memoAsks(ctx workflow.Context) map[string]map[string]any {
	out := map[string]map[string]any{}
	memo := workflow.GetInfo(ctx).Memo
	if memo == nil {
		return out
	}
	for key, p := range memo.GetFields() {
		if !strings.HasPrefix(key, hitl.AskMemoPrefix) {
			continue
		}
		var env map[string]any
		if err := decode(p, &env); err == nil {
			out[key] = env
		}
	}
	return out
}

func decode(p *commonpb.Payload, out any) error {
	return converter.GetDefaultDataConverter().FromPayload(p, out)
}

// ---------------------------------------------------------------------------------------------
// The pair, at the top level
// ---------------------------------------------------------------------------------------------

// SPEAK IS SAY, reached by the name the top level exposes — the same one command, of the same
// duration. A second implementation would be a second budget, a second redaction, and eventually a
// second shape of turn for the same sentence.
func TestSpeakWritesExactlyWhatSayWrites(t *testing.T) {
	var viaVerb, viaPackage, viaNarrate timers
	const sentence = "41 of 119 apexes do not resolve"

	env := envFor(t, speaking, &viaVerb)
	env.ExecuteWorkflow(speaking, []string{sentence})
	require.NoError(t, env.GetWorkflowError())

	control := envFor(t, saying, &viaPackage)
	control.ExecuteWorkflow(saying, []string{sentence})
	require.NoError(t, control.GetWorkflowError())

	// THE SECOND DOOR writes the same event as the front one, which is what makes it a door and
	// not a fork: a caller-only module's turns must be indistinguishable in the transcript.
	second := envFor(t, speakingViaNarrate, &viaNarrate)
	second.ExecuteWorkflow(speakingViaNarrate, []string{sentence})
	require.NoError(t, second.GetWorkflowError())

	require.Len(t, viaVerb.d, 1, "a sentence is one command")
	require.Equal(t, viaPackage.d, viaVerb.d, "the two names must write the same event")
	require.Equal(t, viaPackage.d, viaNarrate.d, "the two doors must write the same event")

	// POSITIVE, and that is not a preference: Go's SDK returns an already-completed future for
	// `d <= 0` without issuing a command, so a zero-duration timer — Python's exact spelling —
	// would write no event at all and the sentence would be silently gone.
	require.Positive(t, viaVerb.d[0], "a non-positive timer writes nothing on this SDK")
}

// SPEAK RETURNS IMMEDIATELY, which is the whole difference from `Ask`: it schedules its one timer
// and the run carries on, with nothing parked and nobody to answer.
func TestSpeakReportsAndReturns(t *testing.T) {
	var rec timers
	env := envFor(t, speaking, &rec)
	env.ExecuteWorkflow(speaking, []string{"batch 1 of 3", "batch 2 of 3", "batch 3 of 3"})

	require.True(t, env.IsWorkflowCompleted(), "a narration parked the run")
	require.NoError(t, env.GetWorkflowError())
	var said int
	require.NoError(t, env.GetWorkflowResult(&said))
	require.Equal(t, 3, said)
	require.Len(t, rec.d, 3)
}

// THE SHAPE ERROR IS BOUNDED THE SAME WAY, and the budget is ONE budget: a run that spends it
// through `Speak` is silent through `Say` too, because they are one function. The run SURVIVES —
// a run that died at hour six because its author was too talkative is a worse outcome than a
// long transcript — and the refusal is itself a turn, which is the whole difference between
// refusing and going quiet.
func TestSpeakSpendsTheSameBudgetAsSay(t *testing.T) {
	var rec timers
	env := envFor(t, mixing, &rec)
	env.ExecuteWorkflow(mixing, narrate.MaxSentences+5)

	require.NoError(t, env.GetWorkflowError(), "the budget must refuse the narration, never the run")
	var said int
	require.NoError(t, env.GetWorkflowResult(&said))
	require.Equal(t, narrate.MaxSentences, said)
	// One timer per carried sentence, plus the one the refusal itself is written on.
	require.Len(t, rec.d, narrate.MaxSentences+1)
}

// A SENTENCE THAT IS TOO LONG IS AN AUTHORING ERROR — deterministic, found the first time the
// workflow runs, and REFUSED rather than truncated: half a sentence may mean something its author
// did not write.
func TestSpeakRefusesAnOversizedSentenceRatherThanTruncating(t *testing.T) {
	var rec timers
	env := envFor(t, speaking, &rec)
	env.ExecuteWorkflow(speaking, []string{strings.Repeat("x", narrate.MaxSentenceBytes+1)})

	err := env.GetWorkflowError()
	require.Error(t, err)
	require.True(t, narrate.IsRefused(err), "the refusal lost its type on the way out: %v", err)
	require.Empty(t, rec.d, "the refused sentence was written anyway")
}

// ASK STOPS THE RUN, publishes its own question the moment it parks, and hands back the type the
// author declared. Field for field the envelope `lib/hitl` writes, because it IS that call.
func TestAskParksTheRunAndPublishesTheSameQuestion(t *testing.T) {
	env := envFor(t, asking, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{
			"value": approval{Approve: true, Note: "checked the sample"}, "by": "mo",
		})
	}, 2*time.Second)

	env.ExecuteWorkflow(asking, map[string]any{"hosts": 12})
	require.NoError(t, env.GetWorkflowError())
	var got asked
	require.NoError(t, env.GetWorkflowResult(&got))

	require.Empty(t, got.Failure)
	parked, ok := got.Parked[hitl.AskMemoPrefix+"ask-1"]
	require.True(t, ok, "a parked run published no ask: %v", got.Parked)
	require.Equal(t, "Approve these 12 hosts?", parked["prompt"])
	require.Equal(t, "pending", parked["state"])
	require.NotNil(t, parked["schema"], "the form is rendered from this")

	require.True(t, got.Answer.Approve)
	require.Equal(t, "checked the sample", got.Answer.Note)
	settled := got.Settled[hitl.AskMemoPrefix+"ask-1"]
	require.Equal(t, "answered", settled["state"])
	require.Equal(t, "mo", settled["by"])
}

// ---------------------------------------------------------------------------------------------
// What neither may carry
// ---------------------------------------------------------------------------------------------

// BOTH REACH HISTORY IN THE CLEAR. The codec is a claim-check, not encryption (ADR 0007): a small
// value rides inline as plain JSON and anyone who can read the run can read it. So the rule is one
// rule over the pair.
//
// THE ASK HALF IS ASSERTED ON THE REAL MEMO, which this environment genuinely serialises. THE SPEAK
// HALF IS ASSERTED ON THE LINE THAT WOULD BE WRITTEN — `narrate.Summary` is the gate every sentence
// passes through before it becomes a Summary, and the environment drops the Summary itself (see the
// file header), so asserting on the timer would assert nothing at all.
//
// WHAT THIS DOES NOT PROVE, because neither guard is a boundary and neither is sold as one: a
// credential in a sentence that names nothing, or in a prompt, still travels. The guards catch the
// ORDINARY mistake — a format string or a config map that happened to be in scope.
func TestNeitherVerbCarriesACredentialIntoHistory(t *testing.T) {
	line, err := narrate.Summary("authenticating with api_key=" + sentinel)
	require.NoError(t, err)
	require.NotContains(t, line, sentinel, "a credential reached the sentence a narration writes")
	require.Equal(t, "authenticating with api_key="+narrate.Redacted, line)

	env := envFor(t, asking, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": approval{Approve: true}})
	}, 2*time.Second)

	env.ExecuteWorkflow(asking, map[string]any{"hosts": 12, "api_key": sentinel})
	require.NoError(t, env.GetWorkflowError())
	var got asked
	require.NoError(t, env.GetWorkflowResult(&got))

	parked := got.Parked[hitl.AskMemoPrefix+"ask-1"]
	context, ok := parked["context"].(map[string]any)
	require.True(t, ok, "the ask carried no context: %v", parked)
	require.Equal(t, hitl.Redacted, context["api_key"])
	// …and the decision aid survives, which is why redaction replaces rather than deletes.
	require.EqualValues(t, 12, context["hosts"])
}
