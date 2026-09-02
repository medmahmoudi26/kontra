package narrate_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra-local/sdk/go/catalog"
	"github.com/medmahmoudi26/kontra-local/sdk/go/narrate"
)

// THE UNIT TESTS CANNOT SEE THE SENTENCE, and that is a property of the test environment rather
// than of this package: `testWorkflowEnvironmentImpl.newTimer` takes `TimerOptions` and never reads
// them, so a Summary written here is dropped before anything could assert it. What that environment
// CAN say is how many timers were scheduled and for how long — which is the budget and the
// zero-duration trap, both of them things a unit test should hold. The sentence itself is asserted
// against a real server in narrate_live_test.go, where it lands on a real `TimerStarted`.

const sentinel = "s3cr3t-nobody-should-ever-read-this"

// timers records what a workflow scheduled: one entry per timer, in order.
type timers struct{ d []time.Duration }

func envWith(t *testing.T, wf any, rec *timers) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf)
	env.SetOnTimerScheduledListener(func(_ string, d time.Duration) {
		rec.d = append(rec.d, d)
	})
	return env
}

// saying narrates every sentence it is given and reports how many the run believes it said.
func saying(ctx workflow.Context, sentences []string) (int, error) {
	for _, s := range sentences {
		if err := narrate.Say(ctx, s); err != nil {
			return 0, err
		}
	}
	n, _ := narrate.Said(ctx)
	return n, nil
}

func TestASentenceIsOneTimerAndNothingElse(t *testing.T) {
	var rec timers
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, []string{"41 of 119 apexes do not resolve"})

	require.NoError(t, env.GetWorkflowError())
	require.Len(t, rec.d, 1, "a sentence is one command")
}

// THE GO TRAP, PINNED. Python writes `sleep(0)`; the same duration here returns an already-completed
// future without issuing a command, so the sentence would never reach history — a narration API that
// passes every other test in this file and archives nothing.
func TestTheTimerAsksForAPositiveDuration(t *testing.T) {
	var rec timers
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, []string{"one", "two"})

	require.NoError(t, env.GetWorkflowError())
	require.Len(t, rec.d, 2)
	for i, d := range rec.d {
		require.Positive(t, d, "sentence %d asked for a timer Go's SDK drops on the floor", i)
	}
}

func TestAWorkflowThatNarratesNothingIsUnchanged(t *testing.T) {
	var rec timers
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, []string{})

	require.NoError(t, env.GetWorkflowError())
	require.Empty(t, rec.d, "a workflow that says nothing issued a command")
}

// NOTHING IS STRICTLY BETTER THAN A BLANK TURN: an empty Summary reads as no Summary, so the timer
// would land in the transcript as a row where a sentence was meant to be.
func TestAnEmptySentenceWritesNothingRatherThanABlankTurn(t *testing.T) {
	var rec timers
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, []string{"", "   ", "\n\t"})

	require.NoError(t, env.GetWorkflowError())
	require.Empty(t, rec.d)
	var said int
	require.NoError(t, env.GetWorkflowResult(&said))
	require.Zero(t, said, "an empty sentence spent budget")
}

func TestTheBoundIsTheSameNumberAsEveryOtherSummary(t *testing.T) {
	require.Equal(t, catalog.SummaryBudget, narrate.MaxSentenceBytes,
		"narration and a dispatch's Method line are the same kind of thing in the same place")
}

// REFUSED, NOT TRUNCATED: half a sentence may mean something its author did not write, and a bound
// nobody is told about is not a bound.
func TestASentenceOverTheBoundIsRefusedRatherThanTruncated(t *testing.T) {
	line, err := narrate.Summary(strings.Repeat("x", narrate.MaxSentenceBytes+1))
	require.Error(t, err)
	require.Empty(t, line)
	require.True(t, narrate.IsRefused(err))
	require.Contains(t, err.Error(), "not truncated")
}

func TestASentenceExactlyAtTheBoundIsCarried(t *testing.T) {
	line, err := narrate.Summary(strings.Repeat("x", narrate.MaxSentenceBytes))
	require.NoError(t, err)
	require.Len(t, line, narrate.MaxSentenceBytes)
}

// The wire counts bytes and so does the offload threshold this bound protects. A rule counted in
// characters would let a sentence of CJK or of em dashes through at nearly three times the size it
// claimed — on exactly the prose least likely to be checked by an English-speaking reviewer.
func TestTheBoundIsInBytesAndNotInCharacters(t *testing.T) {
	sentence := strings.Repeat("日", narrate.MaxSentenceBytes/3+1)
	require.Less(t, len([]rune(sentence)), narrate.MaxSentenceBytes)
	require.Greater(t, len(sentence), narrate.MaxSentenceBytes)

	_, err := narrate.Summary(sentence)
	require.Error(t, err)
	require.True(t, narrate.IsRefused(err))
}

// A refusal must fail the RUN, not its workflow task. A task failure retries forever behind a run
// still reporting `running` — the invisible failure this whole surface exists to remove.
func TestTheRefusalFailsTheRunRatherThanItsWorkflowTask(t *testing.T) {
	var rec timers
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, []string{strings.Repeat("x", narrate.MaxSentenceBytes+1)})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var app *temporal.ApplicationError
	require.True(t, errors.As(err, &app), "a refusal that is not an ApplicationError retries forever")
	require.Equal(t, "NarrationRefused", app.Type())
	require.True(t, app.NonRetryable())
	require.Empty(t, rec.d, "the refused sentence was written anyway")
}

func TestTheSentenceIsNormalisedToOneLine(t *testing.T) {
	line, err := narrate.Summary("  41 of 119\n  apexes\tdo not resolve  ")
	require.NoError(t, err)
	require.Equal(t, "41 of 119 apexes do not resolve", line)
}

// ---------------------------------------------------------------------------------------------
// A sentence never carries a credential
// ---------------------------------------------------------------------------------------------

// The ordinary mistake: a format string that interpolated something that happened to be in scope. A
// narration reaches history in the clear — the codec is a claim-check, not encryption (ADR 0007).
func TestACredentialWrittenAsAnAssignmentDoesNotReachHistory(t *testing.T) {
	for _, sentence := range []string{
		"authenticating with api_key=" + sentinel,
		"authenticating with api_key: " + sentinel,
		"retrying with token = " + sentinel,
		"header Authorization: Bearer " + sentinel,
		"the client sent password=" + sentinel + " and gave up",
	} {
		line, err := narrate.Summary(sentence)
		require.NoError(t, err)
		require.NotContains(t, line, sentinel, sentence)
		// REPLACED, NOT DROPPED: "there was a token here and kontra would not carry it" is a more
		// useful line than a sentence with a hole in it.
		require.Contains(t, line, narrate.Redacted, sentence)
	}
	line, _ := narrate.Summary("authenticating with api_key=" + sentinel)
	require.Equal(t, "authenticating with api_key="+narrate.Redacted, line)
}

func TestABareBearerTokenIsRedactedEvenWhenNothingNamesIt(t *testing.T) {
	line, err := narrate.Summary("Bearer " + sentinel + " was rejected")
	require.NoError(t, err)
	require.Equal(t, "Bearer "+narrate.Redacted+" was rejected", line)
}

// A matcher keyed on the word alone would redact `bucket`, and a guard that mangles ordinary
// sentences is one authors route around.
func TestProseThatMerelyMentionsASecretWordSurvives(t *testing.T) {
	for _, prose := range []string{
		"token bucket refilled to 40",
		"3 hosts rejected the credential and were dropped",
		"bearer of bad news: 41 of 119 apexes do not resolve",
	} {
		line, err := narrate.Summary(prose)
		require.NoError(t, err)
		require.Equal(t, prose, line)
	}
}

// The bound is checked on what would be WRITTEN, so a sentence is never refused for the size of a
// credential that was never going to be carried anyway.
func TestRedactionNeverFailsAndNeverCostsTheAuthorTheBound(t *testing.T) {
	line, err := narrate.Summary("dispatching with token=" + strings.Repeat("z", 400))
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(line, narrate.Redacted))
	require.LessOrEqual(t, len(line), narrate.MaxSentenceBytes)
}

// ---------------------------------------------------------------------------------------------
// A sentence per phase; never a sentence per Unit
// ---------------------------------------------------------------------------------------------

// A per-Unit loop is a SHAPE error and only shows up at scale, which is exactly when killing the run
// is the wrong answer. So the run survives, the narration stops, and the stopping is itself a turn.
func TestASentencePerUnitIsRefusedAndTheTranscriptSaysSo(t *testing.T) {
	var rec timers
	sentences := make([]string, narrate.MaxSentences+10)
	for i := range sentences {
		sentences[i] = fmt.Sprintf("unit %d done", i)
	}
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, sentences)

	require.NoError(t, env.GetWorkflowError(), "a talkative run must not die of it")
	// The budget, plus ONE: the note saying the rest of the run is not narrated.
	require.Len(t, rec.d, narrate.MaxSentences+1)

	var said int
	require.NoError(t, env.GetWorkflowResult(&said))
	require.Equal(t, narrate.MaxSentences, said)
}

// A HARD CEILING ON WHAT NARRATION CAN EVER ADD TO A HISTORY: five events a sentence, so ~1,000.
func TestTheBudgetIsAHardCeilingOnWhatNarrationCanAddToAHistory(t *testing.T) {
	var rec timers
	sentences := make([]string, narrate.MaxSentences*3)
	for i := range sentences {
		sentences[i] = "x"
	}
	env := envWith(t, saying, &rec)
	env.ExecuteWorkflow(saying, sentences)

	require.NoError(t, env.GetWorkflowError())
	require.Len(t, rec.d, narrate.MaxSentences+1, "the ceiling is not a ceiling")
}

func TestTheRefusalItselfFitsTheBoundItIsAnnouncing(t *testing.T) {
	note := fmt.Sprintf(
		"kontra: narration budget spent — %d sentences in one run, and the rest of this run is "+
			"not narrated. Narrate a phase, not a Unit.", narrate.MaxSentences)
	require.LessOrEqual(t, len(note), narrate.MaxSentenceBytes)
	line, err := narrate.Summary(note)
	require.NoError(t, err)
	require.Equal(t, note, line)
}

// A package-level counter — the obvious Go spelling — would let one talkative run silence every
// other run on the same worker, in a way that would only ever show up under load. It is also what
// makes a continued run correct: continue-as-new is a new execution and a new history, so it is
// entitled to a new budget.
func TestTheBudgetIsPerRunAndNotPerWorkerProcess(t *testing.T) {
	loud := make([]string, narrate.MaxSentences+10)
	for i := range loud {
		loud[i] = "x"
	}

	var first timers
	env := envWith(t, saying, &first)
	env.ExecuteWorkflow(saying, loud)
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, first.d, narrate.MaxSentences+1)

	var second timers
	env2 := envWith(t, saying, &second)
	env2.ExecuteWorkflow(saying, []string{"a", "b", "c"})
	require.NoError(t, env2.GetWorkflowError())
	require.Len(t, second.d, 3, "a second run started where the first left off")
}
