// Package hitl parks a workflow on a question a human has to answer — the author's half of HITL,
// and the Go peer of `sdk/python/actorkit/hitl.py`.
//
//	ok, err := hitl.Ask[Approval](ctx, "Approve these 12 hosts?",
//		hitl.Context(map[string]any{"dataset": "live", "n": 12}), // what the operator needs to decide
//		hitl.Deadline(4*time.Hour),                               // or hitl.Indefinite()
//	)
//
// THE QUESTION IS CONTEXTUAL, which is why none of it is a static declaration on the workflow or in
// the catalog. What a run needs a human for depends on what it just found: the prompt, the shape of
// the answer and the material to judge it by are all assembled at the moment it parks. A descriptor
// could only ever declare that this workflow asks *something*.
//
// ── THE ASK IS A HISTORY EVENT, NOT A QUERY HANDLER ────────────────────────────────────────────
//
// A query is the house idiom and would be cheaper than this. It fails in the two places that
// matter. A query is answered by a WORKER, so a parked run whose worker is down could not be read at
// all — the exact unpolled-queue blindness this surface exists to remove, and a parked run is
// precisely the run most likely to outlive the process that parked it. And a query writes nothing to
// history, so the ask would never reach the archived reduced log (ADR 0025) while the ANSWER would,
// since `signal` is already a reduced-log category. An archive holding an answer and not its
// question is a record of somebody approving something unspecified.
//
// So an ask is `workflow.UpsertMemo` — one `WorkflowPropertiesModified` event per ask and one per
// answer. Two properties fall out of that choice and both are load-bearing:
//
//   - The memo rides on `DescribeWorkflowExecution` and on the visibility listing, so the
//     orchestrator reads a run's pending asks with ONE RPC, no history scan, and NO WORKER ANYWHERE.
//   - It is the run's own durable state, so an ask survives a worker restart with nothing to
//     reconcile: the workflow replays, `Ask` re-runs, and it re-parks on the same question.
//
// ── ONE ARCHIVE, TWO SDKS ──────────────────────────────────────────────────────────────────────
//
// Every literal below is the Python peer's, and the envelope is field-for-field the `Ask` interface
// in `backend/src/transcript.ts` and the `RunAsk` in `backend/src/hitl.ts`. Nothing
// between here and the transcript translates, so a Go run and a Python run are the same rows in the
// same route with the same reduced log behind them. Reaching the same outcome by another mechanism —
// a query, an aggregate memo key, a signal that carried its id in the payload — would give the two
// SDKs different archives, which is the one thing parity here means.
//
// ── WHAT MUST NEVER TRAVEL IN ONE ──────────────────────────────────────────────────────────────
//
// The context REACHES HISTORY IN THE CLEAR. The codec is a CLAIM-CHECK, NOT ENCRYPTION (ADR 0007):
// under 128 KiB a value rides inline as plain JSON, and over it the value is moved to the blob store
// and replaced by a ref that any reader of the run can dereference. Neither branch hides anything
// from anyone who can read the run — which is exactly what makes an ask useful, and what makes a
// credential in one a leak. {@link Redact} refuses to carry a value under a secret-shaped key rather
// than trusting the rule to be remembered; it names the key it dropped, because a silently thinner
// context is a worse decision aid than an obviously censored one.
//
// THE ANSWER IS NEVER A BATCH VALUE. It comes back as the plain JSON the operator submitted, and
// nothing here publishes it, pushes it to a Dataset or turns it into Units. An answer that became a
// Batch would be content-addressed into the blob plane and materialized into the lake, which is a
// permanent, queryable copy of a human's judgement in a place nobody chose to put it.
//
// ── THE DEADLINE IS THE AUTHOR'S ───────────────────────────────────────────────────────────────
//
// Naming no deadline applies {@link DefaultDeadline}. {@link Indefinite} means wait forever, which
// is legitimate: a durable workflow genuinely can, and an approval gate on a run that must not
// proceed unattended is the case for it. On expiry {@link Ask} returns {@link ErrExpired} — kontra
// does not choose retry, escalate, take a safe default or fail on the author's behalf, because those
// are four different correct answers depending on what was being approved.
//
// ── THE OPERATOR LABEL IS ATTRIBUTION, NEVER AUTHENTICATION ────────────────────────────────────
//
// The appliance is loopback with no credential (ADR 0031), so there is no authenticated identity to
// record and this SDK does not pretend otherwise. `by` is whatever the answering client said it was.
// It is genuinely useful — on a shared box, or reading your own history six weeks later — and
// nothing in kontra gates anything on it. Do not build one that does.
package hitl

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/sdk/go/schema"
	"github.com/medmahmoudi26/kontra/sdk/go/wfstate"
)

const (
	// AskMemoPrefix is the memo key one ask is filed under: `kontra.ask.<id>`. A PREFIX rather than
	// one aggregate key, because concurrent asks are normal here — parallel branches each needing a
	// decision — and an aggregate would make two branches parking in the same workflow task write
	// over each other.
	AskMemoPrefix = "kontra.ask."

	// AnswerSignalPrefix is the signal one ask is answered by: `kontra.answer/<id>`. The ID IS IN
	// THE NAME, not only in the payload, so the reduced log can pair an answer with its question
	// from event metadata alone — `signalName` is a plain string field on the event, and reading it
	// costs no payload decode.
	AnswerSignalPrefix = "kontra.answer/"

	// MaxContextBytes is the most a context may carry into the memo. Temporal's memo has a hard size
	// ceiling and blowing it fails the workflow AT PARK TIME — turning "ask a human" into "the run
	// died". A context past this is replaced by a note saying so, which is a legible ask rather than
	// a dead run.
	MaxContextBytes = 32_768

	// MaxPromptChars is the most a prompt may carry. A prompt is a sentence; anything longer belongs
	// in the context. A prompt over this is cut and MARKED, because a question silently shortened is
	// a question whose meaning may have changed under the person answering it.
	MaxPromptChars = 2_000
	Elided         = " … [prompt truncated by kontra]"

	// Redacted is what a redacted value is replaced with. Visible on purpose — see {@link Redact}.
	Redacted = "[redacted by kontra: an ask travels through history in the clear]"
)

// DefaultDeadline is applied when the author names none. Long enough that an approval reaching a
// human the next working morning still lands, short enough that a forgotten run does not hold a
// fleet for a week. {@link Indefinite} overrides it with "wait indefinitely".
const DefaultDeadline = 24 * time.Hour

// Keys whose VALUE never reaches an ask. Matched on the key alone and case-insensitively, against
// the whole key rather than a substring of it, so `context` and `tokens_used` survive while
// `api_key` and `session_token` do not. Deliberately a short, boring list: a clever matcher that
// censors half a decision aid is worse than one that misses an exotic spelling, because the author
// can see what it dropped and the leak it is aimed at is the ordinary one.
var secretKeyRe = regexp.MustCompile(
	`(?i)^(.*_)?(password|passwd|pwd|secret|secrets|token|api_?key|apikey|access_?key|` +
		`private_?key|credential|credentials|authorization|cookie|session_?key)(_.*)?$`)

// maxDepth is how deep {@link Redact} walks. Bounded so a self-referential or pathological context
// cannot recurse forever inside a workflow task.
const maxDepth = 8

// slot is one live ask, as the workflow holds it while it waits.
type slot struct {
	id string
	// envelope is the memo envelope — the exact object the read route serves back.
	envelope map[string]any
}

// asks is this execution's asks. Deterministic: the counter advances in call order, and a workflow's
// call order is what replay reproduces. Hung off the EXECUTION (sdk/go/wfstate) and never a
// package variable: two runs on one worker would otherwise number each other's asks and read each
// other's answers.
type askState struct {
	n    int
	live []*slot
}

func (s *askState) find(id string) *slot {
	for _, sl := range s.live {
		if sl.id == id {
			return sl
		}
	}
	return nil
}

func (s *askState) drop(id string) {
	for i, sl := range s.live {
		if sl.id == id {
			s.live = append(s.live[:i], s.live[i+1:]...)
			return
		}
	}
}

var state = wfstate.New(func() *askState { return &askState{} })

// ErrExpired is what {@link Ask} returns when the deadline the author declared passed with nobody
// answering. Test for it with {@link IsExpired}.
//
// RETURNED FROM THE `Ask` THAT PARKED, so the decision about what a timeout means stays with the
// person who knew what was being asked. Retry, escalate, take a safe default, or return it and let
// it fail the run — all four are correct answers to different questions and kontra is not in a
// position to pick.
//
// A NON-RETRYABLE `ApplicationError`, like every other refusal this SDK raises: an author who
// returns it gets a run that FAILS with the reason on it, rather than one that reports `running`
// with nothing moving.
var ErrExpired = errors.New("kontra.ask expired unanswered")

// IsExpired reports whether err is an ask that ran out of time — as opposed to a cancelled run, a
// refused ask, or a failure somewhere else.
func IsExpired(err error) bool {
	if errors.Is(err, ErrExpired) {
		return true
	}
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == "AskExpired"
}

// Option tunes one ask. Variadic rather than a struct parameter, so the common ask stays one line
// and the uncommon one names only what it changes.
type Option func(*askOpts)

type askOpts struct {
	context     any
	deadline    time.Duration
	indefinite  bool
	id          string
	schema      map[string]any
	schemaGiven bool
}

// Context is what the operator needs to decide — the Dataset, the counts, the sample. NEVER a
// credential: it reaches history in the clear. See {@link Redact}.
//
// Named for the field it becomes on the ask, not for `workflow.Context`, which it has nothing to do
// with: this is the material the question is about.
func Context(v any) Option { return func(o *askOpts) { o.context = v } }

// Deadline bounds how long this ask waits. Omitted applies {@link DefaultDeadline}; on expiry the
// ask returns {@link ErrExpired} rather than deciding for you.
func Deadline(d time.Duration) Option {
	return func(o *askOpts) { o.deadline, o.indefinite = d, false }
}

// Indefinite waits forever — the explicit peer of Python's `deadline=None`, and a legitimate choice
// rather than a missing value: a durable workflow genuinely can wait, and an approval gate on a
// run that must not proceed unattended is the case for it.
func Indefinite() Option { return func(o *askOpts) { o.indefinite = true } }

// ID gives this ask an id of your own, for when a stable one matters — a retried leg re-asking the
// same question. Defaults to `ask-<n>` in call order.
func ID(id string) Option { return func(o *askOpts) { o.id = id } }

// Schema declares the answer's shape as a JSON Schema document, for an author who wants one this
// SDK cannot derive. Without it the schema is REFLECTED FROM `T` by the same derivation the actor
// catalog uses for a Method's parameters, so an ask's form and a Method's form cannot disagree about
// the same struct. `Ask[any]` declares no shape and accepts whatever it is handed.
func Schema(doc map[string]any) Option {
	return func(o *askOpts) { o.schema, o.schemaGiven = doc, true }
}

// Ask parks this workflow on a question, and returns what a human answered.
//
//	type Approval struct {
//		Approve bool   `json:"approve"`
//		Note    string `json:"note,omitempty"`
//	}
//
//	got, err := hitl.Ask[Approval](ctx, "Approve these 12 hosts?", hitl.Deadline(4*time.Hour))
//	switch {
//	case hitl.IsExpired(err):
//		// nobody answered in time — YOUR decision what that means
//	case err != nil:
//		return err
//	case got.Approve:
//		...
//	}
//
// `T` IS THE ANSWER'S SHAPE, and it does two jobs: the form the operator fills in is rendered from
// its JSON Schema, and the answer route validates against that same document before it signals. Use
// `Ask[any]` for an ask that constrains nothing.
//
// AN ANSWER THAT DOES NOT FIT `T` IS AN ERROR, not a zero value — the one place this diverges from
// Python, and it diverges because Go's zero value is a lie an operator would pay for: an `Approval{}`
// returned where the answer could not be read says `approve: false`, which is a decision nobody
// made. Python returns the plain dict for the same reason, which is the same rule in a language that
// has one. The answer route validated the value against this ask's own schema before signalling, so
// reaching this is a genuine disagreement between the two documents and worth an error either way.
func Ask[T any](ctx workflow.Context, prompt string, opts ...Option) (T, error) {
	var zero T

	o := askOpts{deadline: DefaultDeadline}
	for _, f := range opts {
		f(&o)
	}
	if !o.schemaGiven {
		o.schema = schema.OfType(reflect.TypeFor[T]())
	}

	st := state.Of(ctx)
	st.n++
	askID := o.id
	if askID == "" {
		// DETERMINISTIC, from a counter rather than from a uuid. Both replay identically, and a
		// counter additionally reads as itself in a URL, in a log line and in the `signalName` the
		// reduced log records — which is the whole reason the id is in the signal name at all.
		askID = fmt.Sprintf("ask-%d", st.n)
	}
	if st.find(askID) != nil {
		// TWO LIVE ASKS UNDER ONE ID would share a memo key and a signal name, so answering either
		// would answer whichever arrived first — a wrong answer delivered to a human's decision.
		// Re-USING an id after the first one settled is fine and supported; this refuses only the
		// overlap. Non-retryable, because retrying an authoring error forever is how a run comes to
		// report `running` with nothing moving.
		return zero, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("ask id %q is already pending on this run — two live asks cannot share one id",
				askID), "AskIDInUse", nil)
	}

	key := AskMemoPrefix + askID
	signal := AnswerSignalPrefix + askID

	askedAt := workflow.Now(ctx).UnixMilli()
	wait, deadlineAt := time.Duration(0), int64(0)
	if !o.indefinite {
		wait = o.deadline
		deadlineAt = askedAt + wait.Milliseconds()
	}

	env := envelope(askID, prompt, o.schema, fit(Redact(o.context)), askedAt, deadlineAt)
	sl := &slot{id: askID, envelope: env}
	st.live = append(st.live, sl)

	// FETCHED BEFORE THE MEMO IS WRITTEN. The memo is what makes the ask answerable, so an answer
	// can arrive the instant after it lands; the SDK does buffer a signal whose channel nobody has
	// asked for yet, but relying on that ordering is a race nobody would find until it mattered.
	ch := workflow.GetSignalChannel(ctx, signal)
	if err := workflow.UpsertMemo(ctx, map[string]any{key: env}); err != nil {
		st.drop(askID)
		return zero, err
	}
	logger := workflow.GetLogger(ctx)
	logger.Info("hitl: parked on an ask", "ask", askID, "prompt", env["prompt"])

	var raw json.RawMessage
	var answered, expired bool

	// The timer is cancellable so an answered ask does not leave one running to the deadline. The
	// scope is this ask's alone: cancelling it must not touch anything else the workflow started.
	timerCtx, stopTimer := workflow.WithCancel(ctx)
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(ch, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, &raw)
		answered = true
	})
	if wait > 0 {
		// Temporal shows a timer's Summary on the bar in its own UI, and this is the ask's ID, not
		// its prompt: a timer named after a sentence reads as a sentence that timed out, and this
		// one is here so the deadline timer is drillable back to its question. The same string
		// Python passes as `timeout_summary`.
		fut := workflow.NewTimerWithOptions(timerCtx, wait,
			workflow.TimerOptions{Summary: "kontra.ask/" + askID})
		sel.AddFuture(fut, func(workflow.Future) { expired = true })
	}
	// THE RUN WAS CANCELLED WHILE WAITING FOR A HUMAN, which is a different ending from an expiry
	// and from an answer, and it is recorded as its own state so the ask stops being offered — an
	// answerable question on a cancelled run is a form that signals nothing.
	sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
	sel.Select(ctx)
	stopTimer()

	st.drop(askID)

	switch {
	case answered:
		value, by, err := readAnswer[T](raw)
		// FIRST ANSWER WINS. Anything already queued behind it is dropped rather than overwriting:
		// the workflow may have acted on the first one, and a second that silently replaced it would
		// make the transcript's attribution false.
		for {
			var dup json.RawMessage
			if !ch.ReceiveAsync(&dup) {
				break
			}
			logger.Warn("hitl: a second answer was ignored", "ask", askID)
		}
		closeAsk(ctx, key, env, "answered", map[string]any{
			"answeredAt": workflow.Now(ctx).UnixMilli(), "by": by,
		})
		if err != nil {
			return zero, err
		}
		return value, nil

	case expired:
		now := workflow.Now(ctx).UnixMilli()
		closeAsk(ctx, key, env, "expired", map[string]any{"expiredAt": now})
		waited := time.Duration(now-askedAt) * time.Millisecond
		return zero, temporal.NewApplicationErrorWithOptions(
			fmt.Sprintf("ask %s expired after %.1fs unanswered: %s",
				askID, waited.Seconds(), env["prompt"]),
			"AskExpired",
			temporal.ApplicationErrorOptions{NonRetryable: true, Cause: ErrExpired, Details: []any{askID}})

	default:
		closeAsk(ctx, key, env, "abandoned", map[string]any{
			"abandonedAt": workflow.Now(ctx).UnixMilli(),
		})
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return zero, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("ask %s stopped waiting without an answer", askID), "AskAbandoned", nil)
	}
}

// Pending is what THIS workflow is currently parked on, in ask order.
//
// The workflow's own view of it. Every other reader — the route, the transcript, the chrome — reads
// the memo, which is the same envelopes and needs no worker to answer.
func Pending(ctx workflow.Context) []map[string]any {
	st := state.Of(ctx)
	out := make([]map[string]any, 0, len(st.live))
	for _, sl := range st.live {
		out = append(out, maps(sl.envelope))
	}
	return out
}

// envelope is one ask as the memo carries it — and as `GET /api/runs/:id/asks` serves it back.
//
// ONE SHAPE, NOT TWO. The field names are `backend/src/transcript.ts`'s `Ask` verbatim, so
// nothing between here and the transcript translates: a rename on either side is a missing field a
// test catches, rather than a silent mapping layer that drops one.
func envelope(id, prompt string, doc map[string]any, context any, askedAt, deadlineAt int64) map[string]any {
	env := map[string]any{
		"id":      id,
		"prompt":  clip(prompt),
		"askedAt": askedAt,
		"state":   "pending",
	}
	if doc != nil {
		env["schema"] = doc
	}
	if context != nil {
		env["context"] = context
	}
	if deadlineAt > 0 {
		env["deadlineAt"] = deadlineAt
	}
	return env
}

// closeAsk rewrites one ask's memo entry to its ending.
//
// THE SCHEMA IS DROPPED AND THE CONTEXT IS KEPT. The schema is the largest field and is derivable
// from the workflow's own source, so carrying it on a settled ask is memo weight for nothing. The
// context is the material the decision was made ON, and a settled ask that kept only its prompt
// would record somebody approving something whose particulars are gone.
//
// THE ANSWER'S VALUE IS NOT WRITTEN BACK. It arrived as a signal and is already in history where the
// operator put it; copying it into the memo would make a second, longer-lived copy of a human's
// judgement in a place nobody chose to put it, and would grow the memo by the size of every answer a
// long run ever took.
//
// ON A DISCONNECTED CONTEXT, because one of the three endings is a cancelled run: a close written
// through a cancelled context is the one that records `abandoned`, and it must land.
func closeAsk(ctx workflow.Context, key string, env map[string]any, state string, extra map[string]any) {
	delete(env, "schema")
	env["state"] = state
	for k, v := range extra {
		if v == nil || v == "" {
			continue
		}
		env[k] = v
	}
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	if err := workflow.UpsertMemo(dctx, map[string]any{key: env}); err != nil {
		workflow.GetLogger(ctx).Warn("hitl: an ask's ending was not recorded", "ask", key, "error", err)
	}
}

// readAnswer takes the operator's answer apart: the value as `T`, and the self-asserted label.
//
// LENIENT ABOUT THE ENVELOPE, STRICT ABOUT THE VALUE. A client that signalled a bare value rather
// than `{"value": …}` is answering the question, and refusing it over an envelope would park the run
// on a technicality; a value that does not fit the shape the ask declared is a real disagreement,
// and returning the zero value for it would invent a decision.
func readAnswer[T any](raw json.RawMessage) (T, string, error) {
	var zero T
	if len(raw) == 0 {
		return zero, "", nil
	}
	var body struct {
		Value json.RawMessage `json:"value"`
		By    string          `json:"by"`
	}
	value := raw
	by := ""
	if err := json.Unmarshal(raw, &body); err == nil && (body.Value != nil || body.By != "") {
		value, by = body.Value, body.By
	}
	if len(value) == 0 {
		return zero, by, nil
	}
	var out T
	if err := json.Unmarshal(value, &out); err != nil {
		return zero, by, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("an answer validated against its own schema did not fit %T: %v", zero, err),
			"AnswerMismatch", nil)
	}
	return out, by, nil
}

// Redact drops the value of anything under a secret-shaped key, recursively.
//
// NOT A SECURITY BOUNDARY and not sold as one. It is a guard against the ordinary mistake — passing
// a config struct straight into the context because it happened to be in scope — and it catches that
// one reliably. A secret under a key it does not recognise still travels, which is why the rule in
// the package header is stated as a rule and not as a promise this function keeps.
//
// IT REPLACES RATHER THAN DELETING, and the replacement says why. A key that vanished would read to
// the operator as a fact the workflow did not have, which is a different (and worse) statement than
// "the workflow had this and kontra would not carry it".
//
// IT WORKS ON THE JSON, not on the Go value, which is what lets it reach into a struct an author
// passed: the context is going to history as JSON either way, so the tree that is walked is the tree
// that would be written. A value that will not marshal is not redacted here — {@link fit} replaces
// it wholesale with a note, which is the same answer arrived at one step later.
func Redact(v any) any {
	if v == nil {
		return nil
	}
	tree, err := jsonTree(v)
	if err != nil {
		return v
	}
	return redact(tree, 0)
}

func redact(v any, depth int) any {
	if depth >= maxDepth {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKeyRe.MatchString(k) {
				out[k] = Redacted
				continue
			}
			out[k] = redact(val, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redact(val, depth+1)
		}
		return out
	default:
		return v
	}
}

// jsonTree is `v` as the generic tree it will be written as. One marshal/unmarshal round trip, once
// per ask, which is also what gives {@link fit} an honest size to measure.
func jsonTree(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// fit is a context bounded to {@link MaxContextBytes}, or a note saying it was not carried.
//
// THE BOUND IS THE POINT. Temporal's memo has a hard ceiling and exceeding it fails the workflow at
// the upsert — so an ask carrying one page too many of sample rows would not park, it would kill the
// run. A note in its place leaves an answerable question with a visibly missing aid, which is the
// failure an operator can act on.
//
// MEASURED ON THE COMPACT JSON this SDK writes. Python measures its own `json.dumps`, which spends a
// couple of bytes per field on separators — so the two SDKs cut at very slightly different contexts.
// Both are the same 32 KiB guard against the same failure; neither number is a wire contract.
func fit(context any) any {
	if context == nil {
		return nil
	}
	raw, err := json.Marshal(context)
	if err != nil {
		return map[string]any{"kontra": "context was not JSON-serialisable and was not carried"}
	}
	if len(raw) <= MaxContextBytes {
		return context
	}
	return map[string]any{"kontra": fmt.Sprintf(
		"context was %d bytes, over the %d-byte limit, and was not carried — attach a Dataset name "+
			"or a sample rather than the whole thing", len(raw), MaxContextBytes)}
}

// clip is the prompt, cut and MARKED past {@link MaxPromptChars}. Characters, not bytes: the bound
// is about how much a person is asked to read.
func clip(prompt string) string {
	runes := []rune(prompt)
	if len(runes) <= MaxPromptChars {
		return prompt
	}
	return string(runes[:MaxPromptChars]) + Elided
}

// maps copies an envelope, so a caller reading {@link Pending} cannot rewrite the ask the run is
// parked on.
func maps(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
