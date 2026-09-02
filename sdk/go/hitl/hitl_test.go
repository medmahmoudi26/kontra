package hitl_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/sdk/go/hitl"
)

// WHAT A TEST CAN SEE HERE, and it is more than the narration next door: the test environment
// really serialises an upserted memo and merges it into the execution's own `WorkflowInfo`, so a
// workflow can read back exactly the bytes a route would read over `DescribeWorkflowExecution`.
// That is the whole contract of an ask — the memo is what makes a parked run readable with no worker
// anywhere — so these assertions are made on the memo and never on this package's own state.

// Approval is the answer's shape, and the thing `Ask[T]` derives a schema from.
type Approval struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty"`
}

const answerSignal = hitl.AnswerSignalPrefix + "ask-1"

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

type answered struct {
	Answer  Approval                  `json:"answer"`
	Parked  map[string]map[string]any `json:"parked"`
	Settled map[string]map[string]any `json:"settled"`
	Pending []map[string]any          `json:"pending"`
	Left    []map[string]any          `json:"left"`
	Failure string                    `json:"failure"`
	Expired bool                      `json:"expired"`
}

// approving parks on one ask and reports what the memo said before and after — the snapshot taken
// from a second coroutine, because the parked state is only visible while the first one is waiting.
func approving(ctx workflow.Context, opts scenario) (answered, error) {
	var out answered
	done := workflow.NewChannel(ctx)
	workflow.Go(ctx, func(gctx workflow.Context) {
		_ = workflow.Sleep(gctx, time.Second)
		out.Parked = memoAsks(gctx)
		out.Pending = hitl.Pending(gctx)
		done.Send(gctx, nil)
	})

	got, err := hitl.Ask[Approval](ctx, opts.Prompt, opts.options()...)
	done.Receive(ctx, nil)
	out.Answer = got
	out.Settled = memoAsks(ctx)
	out.Left = hitl.Pending(ctx)
	if err != nil {
		out.Failure = err.Error()
		out.Expired = hitl.IsExpired(err)
	}
	return out, nil
}

// scenario is what one test wants asked. A struct rather than options directly, because the options
// are funcs and a workflow argument has to survive the data converter.
type scenario struct {
	Prompt     string         `json:"prompt"`
	Context    map[string]any `json:"context"`
	Deadline   time.Duration  `json:"deadline"`
	Indefinite bool           `json:"indefinite"`
	ID         string         `json:"id"`
}

func (s scenario) options() []hitl.Option {
	var opts []hitl.Option
	if s.Context != nil {
		opts = append(opts, hitl.Context(s.Context))
	}
	if s.Indefinite {
		opts = append(opts, hitl.Indefinite())
	} else if s.Deadline > 0 {
		opts = append(opts, hitl.Deadline(s.Deadline))
	}
	if s.ID != "" {
		opts = append(opts, hitl.ID(s.ID))
	}
	return opts
}

func newEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(approving)
	return env
}

func run(t *testing.T, env *testsuite.TestWorkflowEnvironment, s scenario) answered {
	t.Helper()
	env.ExecuteWorkflow(approving, s)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var got answered
	require.NoError(t, env.GetWorkflowResult(&got))
	return got
}

// ---------------------------------------------------------------------------------------------
// The ask is an event, and the memo is what makes it readable
// ---------------------------------------------------------------------------------------------

// ONE MEMO KEY PER ASK, `kontra.ask.<id>` — a prefix and not one aggregate value, because two
// branches parking in the same workflow task would otherwise write over each other.
func TestAnAskIsPublishedAsItsOwnMemoEntry(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}, "by": "mo"})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: "Approve these 12 hosts?", Deadline: time.Hour})

	parked, ok := got.Parked[hitl.AskMemoPrefix+"ask-1"]
	require.True(t, ok, "a parked run published no ask: %v", got.Parked)
	require.Equal(t, "ask-1", parked["id"])
	require.Equal(t, "Approve these 12 hosts?", parked["prompt"])
	require.Equal(t, "pending", parked["state"])
	require.NotZero(t, parked["askedAt"])
	require.NotZero(t, parked["deadlineAt"])
	// The form is rendered from this, and the answer route validates against it.
	require.NotNil(t, parked["schema"])

	// The workflow's own view is the same envelopes, for an author who wants to branch on them.
	require.Len(t, got.Pending, 1)
	require.Equal(t, "ask-1", got.Pending[0]["id"])
}

// THE ANSWER COMES BACK AS THE TYPE THE AUTHOR DECLARED, and the memo records who said it.
func TestAnAnsweredAskReturnsTheValueAndRecordsTheOperator(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{
			"value": Approval{Approve: true, Note: "checked the sample"}, "by": "mo",
		})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: "Approve?", Deadline: time.Hour})

	require.Empty(t, got.Failure)
	require.True(t, got.Answer.Approve)
	require.Equal(t, "checked the sample", got.Answer.Note)

	settled := got.Settled[hitl.AskMemoPrefix+"ask-1"]
	require.Equal(t, "answered", settled["state"])
	require.NotZero(t, settled["answeredAt"])
	// SELF-ASSERTED, NEVER AUTHENTICATION: recorded exactly as the answering client said it.
	require.Equal(t, "mo", settled["by"])
	// THE SCHEMA IS DROPPED on a settled ask — the largest field, and derivable from the source.
	require.NotContains(t, settled, "schema")
	// THE ANSWER'S VALUE IS NOT WRITTEN BACK: it is already in history where the operator put it.
	require.NotContains(t, settled, "value")
	require.Empty(t, got.Left, "the ask stayed pending after it was answered")
}

// An answer with no label is unlabelled, not attributed to something nobody said.
func TestAnAnswerWithNoLabelRecordsNoOperator(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: "Approve?", Deadline: time.Hour})
	require.NotContains(t, got.Settled[hitl.AskMemoPrefix+"ask-1"], "by")
}

// A client that signalled a bare value rather than `{"value": …}` is still answering the question.
func TestABareAnswerIsStillAnAnswer(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, Approval{Approve: true, Note: "bare"})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: "Approve?", Deadline: time.Hour})
	require.Empty(t, got.Failure)
	require.True(t, got.Answer.Approve)
	require.Equal(t, "bare", got.Answer.Note)
}

// ---------------------------------------------------------------------------------------------
// The deadline is the author's
// ---------------------------------------------------------------------------------------------

// ON EXPIRY THE ASK RETURNS, and what a timeout MEANS stays with the author: retry, escalate, take a
// safe default, or fail the run are four different correct answers to four different questions.
func TestADeadlineThatPassesUnansweredExpiresTheAsk(t *testing.T) {
	got := run(t, newEnv(t), scenario{Prompt: "Approve?", Deadline: 4 * time.Hour})

	require.True(t, got.Expired, "an expiry that does not report as one: %q", got.Failure)
	require.Contains(t, got.Failure, "ask-1")
	require.Contains(t, got.Failure, "Approve?")

	settled := got.Settled[hitl.AskMemoPrefix+"ask-1"]
	require.Equal(t, "expired", settled["state"])
	require.NotZero(t, settled["expiredAt"])
	require.NotContains(t, settled, "answeredAt")
}

// A run that expired must FAIL rather than sit at `running` with nothing moving — the invisible
// failure this whole surface exists to remove, produced by the surface itself.
func TestTheExpiryIsANonRetryableApplicationError(t *testing.T) {
	expiring := func(ctx workflow.Context) error {
		_, err := hitl.Ask[Approval](ctx, "Approve?", hitl.Deadline(time.Hour))
		return err
	}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(expiring)
	env.ExecuteWorkflow(expiring)

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var app *temporal.ApplicationError
	require.True(t, errors.As(err, &app))
	require.Equal(t, "AskExpired", app.Type())
	require.True(t, app.NonRetryable())
}

// PASSING `Indefinite` IS A CHOICE, not a missing value: a durable workflow genuinely can wait, and
// an approval gate on a run that must not proceed unattended is the case for it. The absence of
// `deadlineAt` is what the route renders as "waits indefinitely" rather than as a countdown.
func TestAnIndefiniteAskDeclaresNoDeadlineAndDoesNotExpire(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}})
		// Long after any default would have fired.
	}, 72*time.Hour)

	got := run(t, env, scenario{Prompt: "Approve?", Indefinite: true})

	require.Empty(t, got.Failure)
	require.True(t, got.Answer.Approve)
	require.NotContains(t, got.Parked[hitl.AskMemoPrefix+"ask-1"], "deadlineAt")
}

// The default is applied when the author names nothing — 24 hours, not "forever".
func TestNamingNoDeadlineAppliesTheDefault(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: "Approve?"})

	parked := got.Parked[hitl.AskMemoPrefix+"ask-1"]
	asked, deadline := parked["askedAt"].(float64), parked["deadlineAt"].(float64)
	require.InDelta(t, hitl.DefaultDeadline.Milliseconds(), int64(deadline-asked), 1000)
}

// ---------------------------------------------------------------------------------------------
// Concurrent asks
// ---------------------------------------------------------------------------------------------

// PARALLEL BRANCHES EACH NEEDING A DECISION IS NORMAL HERE, which is why the read route is a LIST
// and the memo key is a prefix. Both asks are live at once, they number in call order, and answering
// one does not touch the other.
func TestTwoAsksAreLiveAtOnceAndAnsweredIndependently(t *testing.T) {
	both := func(ctx workflow.Context) (map[string]any, error) {
		type reply struct {
			got Approval
			err error
		}
		var first, second reply
		done := workflow.NewChannel(ctx)
		var parked []map[string]any

		workflow.Go(ctx, func(gctx workflow.Context) {
			first.got, first.err = hitl.Ask[Approval](gctx, "one?", hitl.Deadline(time.Hour))
			done.Send(gctx, nil)
		})
		workflow.Go(ctx, func(gctx workflow.Context) {
			second.got, second.err = hitl.Ask[Approval](gctx, "two?", hitl.Deadline(time.Hour))
			done.Send(gctx, nil)
		})
		workflow.Go(ctx, func(gctx workflow.Context) {
			_ = workflow.Sleep(gctx, time.Second)
			parked = hitl.Pending(gctx)
			done.Send(gctx, nil)
		})
		for range 3 {
			done.Receive(ctx, nil)
		}
		return map[string]any{
			"one": first.got.Note, "two": second.got.Note,
			"parked": len(parked), "memo": len(memoAsks(ctx)),
			"errs": []string{errText(first.err), errText(second.err)},
		}, nil
	}

	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(both)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(hitl.AnswerSignalPrefix+"ask-2", map[string]any{
			"value": Approval{Approve: true, Note: "second"}, "by": "mo"})
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(hitl.AnswerSignalPrefix+"ask-1", map[string]any{
			"value": Approval{Approve: true, Note: "first"}, "by": "mo"})
	}, 3*time.Second)
	env.ExecuteWorkflow(both)

	require.NoError(t, env.GetWorkflowError())
	var got map[string]any
	require.NoError(t, env.GetWorkflowResult(&got))
	require.Equal(t, float64(2), got["parked"], "the two asks were not live at the same time")
	require.Equal(t, float64(2), got["memo"], "two asks did not publish two memo entries")
	// ANSWERED OUT OF ORDER, and each reached its own question — which is what the id in the signal
	// name buys. One aggregate handler would have delivered the second answer to the first ask.
	require.Equal(t, "first", got["one"])
	require.Equal(t, "second", got["two"])
}

// TWO LIVE ASKS UNDER ONE ID would share a memo key and a signal name, so answering either would
// answer whichever arrived first — a wrong answer delivered to a human's decision.
func TestTwoLiveAsksCannotShareOneID(t *testing.T) {
	clashing := func(ctx workflow.Context) error {
		var second error
		done := workflow.NewChannel(ctx)
		workflow.Go(ctx, func(gctx workflow.Context) {
			_, _ = hitl.Ask[Approval](gctx, "one?", hitl.ID("approve"), hitl.Deadline(time.Hour))
			done.Send(gctx, nil)
		})
		workflow.Go(ctx, func(gctx workflow.Context) {
			_ = workflow.Sleep(gctx, time.Second)
			_, second = hitl.Ask[Approval](gctx, "two?", hitl.ID("approve"), hitl.Deadline(time.Hour))
			done.Send(gctx, nil)
		})
		done.Receive(ctx, nil)
		done.Receive(ctx, nil)
		return second
	}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(clashing)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(hitl.AnswerSignalPrefix+"approve", map[string]any{"value": Approval{}})
	}, 2*time.Second)
	env.ExecuteWorkflow(clashing)

	err := env.GetWorkflowError()
	require.Error(t, err)
	var app *temporal.ApplicationError
	require.True(t, errors.As(err, &app))
	require.Equal(t, "AskIDInUse", app.Type())
	require.True(t, app.NonRetryable(), "an authoring error that retries forever is a hung run")
}

// ---------------------------------------------------------------------------------------------
// What must never travel in one
// ---------------------------------------------------------------------------------------------

// A CONTEXT REACHES HISTORY IN THE CLEAR — the codec is a claim-check, not encryption (ADR 0007) —
// so the ordinary mistake is passing a config that happened to be in scope.
func TestASecretShapedKeyIsNotCarriedIntoAnAsk(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}})
	}, 2*time.Second)

	got := run(t, env, scenario{
		Prompt:   "Approve?",
		Deadline: time.Hour,
		Context: map[string]any{
			"dataset":     "live",
			"n":           12,
			"api_key":     "s3cr3t",
			"aws_secret":  "s3cr3t",
			"nested":      map[string]any{"session_token": "s3cr3t", "host": "acme.com"},
			"tokens_used": 41,
		},
	})

	ctxOut := got.Parked[hitl.AskMemoPrefix+"ask-1"]["context"].(map[string]any)
	require.Equal(t, "live", ctxOut["dataset"])
	require.Equal(t, float64(12), ctxOut["n"])
	// REPLACED, NOT DELETED: a key that vanished reads as a fact the workflow did not have.
	require.Equal(t, hitl.Redacted, ctxOut["api_key"])
	require.Equal(t, hitl.Redacted, ctxOut["aws_secret"])
	require.Equal(t, hitl.Redacted, ctxOut["nested"].(map[string]any)["session_token"])
	require.Equal(t, "acme.com", ctxOut["nested"].(map[string]any)["host"])
	// A short, boring list beats a clever matcher that censors half a decision aid.
	require.Equal(t, float64(41), ctxOut["tokens_used"])
}

// The bound is the point: a memo past Temporal's ceiling fails the workflow AT PARK TIME, turning
// "ask a human" into "the run died". A note in its place leaves an answerable question.
func TestAContextOverTheLimitIsReplacedByANoteRatherThanKillingTheRun(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}})
	}, 2*time.Second)

	got := run(t, env, scenario{
		Prompt:   "Approve?",
		Deadline: time.Hour,
		Context:  map[string]any{"rows": strings.Repeat("x", hitl.MaxContextBytes+1)},
	})

	require.Empty(t, got.Failure)
	ctxOut := got.Parked[hitl.AskMemoPrefix+"ask-1"]["context"].(map[string]any)
	require.Contains(t, ctxOut["kontra"], "was not carried")
	require.NotContains(t, ctxOut, "rows")
}

// A question silently shortened is a question whose meaning may have changed under the person
// answering it, so the cut SAYS it was cut.
func TestAPromptOverTheLimitIsCutAndMarked(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": Approval{Approve: true}})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: strings.Repeat("q", hitl.MaxPromptChars+50), Deadline: time.Hour})

	prompt := got.Parked[hitl.AskMemoPrefix+"ask-1"]["prompt"].(string)
	require.True(t, strings.HasSuffix(prompt, hitl.Elided))
	require.Len(t, []rune(prompt), hitl.MaxPromptChars+len([]rune(hitl.Elided)))
}

// ---------------------------------------------------------------------------------------------
// The shape of the answer
// ---------------------------------------------------------------------------------------------

// `Ask[any]` constrains nothing — the author chose not to, and inventing a rule here would refuse
// answers to their own question.
func TestAnUnconstrainedAskDeclaresNoSchemaAndTakesWhatItIsHanded(t *testing.T) {
	loose := func(ctx workflow.Context) (map[string]any, error) {
		var parked map[string]map[string]any
		done := workflow.NewChannel(ctx)
		workflow.Go(ctx, func(gctx workflow.Context) {
			_ = workflow.Sleep(gctx, time.Second)
			parked = memoAsks(gctx)
			done.Send(gctx, nil)
		})
		got, err := hitl.Ask[any](ctx, "anything?", hitl.Deadline(time.Hour))
		done.Receive(ctx, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"got": got, "schema": parked[hitl.AskMemoPrefix+"ask-1"]["schema"]}, nil
	}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(loose)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": []any{1, 2, 3}})
	}, 2*time.Second)
	env.ExecuteWorkflow(loose)

	require.NoError(t, env.GetWorkflowError())
	var got map[string]any
	require.NoError(t, env.GetWorkflowResult(&got))
	require.Nil(t, got["schema"], "an ask that constrains nothing declared a rule anyway")
	require.Equal(t, []any{float64(1), float64(2), float64(3)}, got["got"])
}

// AN ANSWER THAT DOES NOT FIT IS AN ERROR, NOT A ZERO VALUE. `Approval{}` returned where the answer
// could not be read says `approve: false` — a decision nobody made.
func TestAnAnswerThatDoesNotFitIsRefusedRatherThanZeroed(t *testing.T) {
	env := newEnv(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(answerSignal, map[string]any{"value": "yes please"})
	}, 2*time.Second)

	got := run(t, env, scenario{Prompt: "Approve?", Deadline: time.Hour})

	require.Contains(t, got.Failure, "did not fit")
	require.False(t, got.Answer.Approve, "the zero value must not read as a decision")
	// The ask still settled as answered: a human DID answer, and the archive says so.
	require.Equal(t, "answered", got.Settled[hitl.AskMemoPrefix+"ask-1"]["state"])
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
