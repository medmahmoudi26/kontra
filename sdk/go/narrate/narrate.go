// Package narrate is an author's own sentence about their own run — the `say` half of the
// transcript, and the Go peer of `sdk/python/actorkit/narrate.py`.
//
//	if err := narrate.Say(ctx, fmt.Sprintf("%d apexes in scope from the paid-programs list", len(apexes))); err != nil {
//		return out, err
//	}
//	...
//	narrate.Say(ctx, fmt.Sprintf("%d of %d resolve; dispatching the crawler", live, len(apexes)))
//
// DERIVED TURNS MAKE AN UNTOOLED WORKFLOW READABLE; NARRATION MAKES A TOOLED ONE EXPLAIN ITSELF.
// Everything else in a transcript is inferred from event metadata — which Actor, which Method, how
// many machines, what failed, how long. None of it can say what the run MEANT by any of it, because
// that is not in the log: it is in the head of the person who wrote the workflow. `Say` is the one
// place they can put it, and it lands in the transcript at the point in the run where it was
// written, in among the turns it is about.
//
// ── IT IS A TIMER, AND THAT IS THE WHOLE MECHANISM ─────────────────────────────────────────────
//
// A narration is a timer whose Temporal user metadata IS the sentence. Four properties made that
// the only shape that works here, and they are the same four in both SDKs.
//
//   - METADATA, NOT PAYLOAD — the same route the Method name takes (`catalog.DispatchSummary`). A
//     payload on this deployment may be a claim-check ref (ADR 0007), so a sentence carried in one
//     would cost a blob GET per turn to read. A Summary is read straight off the event by
//     `control/orchestrator/src/history.ts`, and Temporal's own UI renders it on the timer's bar label.
//   - IT REACHES THE ARCHIVE. The reduced log is what ADR 0025 stores, and a Summary is part of a
//     reduced event — so an author's sentences are still there when Temporal's 24-hour retention
//     has dropped the history they came from. That is the requirement that rules out a memo, which
//     is what `lib/hitl` writes and for its own different reason.
//   - NO WORKER, NO REGISTRATION, NO QUEUE. An activity carries a Summary too, and it needs a
//     function registered on the CALLER's own worker and a queue somebody polls — so a narration
//     would be able to sit queued, retry, and time out. Absurd properties for a sentence.
//   - IT IS DETERMINISTIC. A timer is a workflow command like any other: it replays, it needs no
//     side channel, and `Say` can be called from anywhere inside a workflow with nothing wired up.
//
// ── WHERE GO AND PYTHON DIVERGE, AND WHY THE ARCHIVE DOES NOT ──────────────────────────────────
//
// Python writes `workflow.sleep(0, summary=…)`. THE SAME CALL IN GO WRITES NOTHING: this SDK
// short-circuits a non-positive timer in the client (`internal/workflow.go`, `NewTimerWithOptions`:
// `if d <= 0 { settable.Set(true, nil); return future }`), so no command is issued, no
// `TimerStarted` reaches history, and the sentence is silently gone — a narration API that passes
// every unit test and archives nothing. So the duration here is {@link tick}, the smallest positive
// one, and `narrate_live_test.go` asserts against a real server that what lands is byte-identical
// to Python's five events with the sentence on `TimerStarted`.
//
// ── HISTORY EVENTS ARE NOT FREE, AND THIS REPO HAS MEASURED THE WALL ───────────────────────────
//
// One narration is EXACTLY FIVE HISTORY EVENTS — `TimerStarted` carrying the sentence, `TimerFired`,
// and the three-event workflow task the firing wakes — and about one second of wall clock, which is
// the server's own timer granularity. Per phase both numbers are nothing. Per Unit they are the
// failure this repo has already had twice: a publish/subscribe pattern that wrote its data into
// history twice and died around 8k refs with `GrpcMessageTooLarge`, and a blob-cursor poll loop that
// accounted for 86% of one workflow's history.
//
// So the two ways to get this wrong are bounded, and DIFFERENTLY, because they are two different
// mistakes with two different right costs:
//
//   - A SENTENCE THAT IS TOO LONG IS AN AUTHORING ERROR. It is deterministic, it happens the first
//     time the workflow runs, and it happens on sentence one. {@link Say} REFUSES rather than
//     truncating: a sentence cut in the middle may have changed its meaning under the person
//     reading it, and a bound nobody is told about is not a bound.
//   - A SENTENCE PER UNIT IS A SHAPE ERROR, and it only shows up at scale — in production, on the
//     run that mattered, which is exactly when killing the run is the wrong answer. Narration is
//     decoration; a run that died at hour six because its author was too talkative is a worse
//     outcome than a long transcript. So the run survives and the NARRATION is refused: past
//     {@link MaxSentences} this package says so once, in the transcript, in the author's own
//     channel, and then stays silent for the rest of the run.
//
// ── WHAT MUST NEVER TRAVEL IN ONE ──────────────────────────────────────────────────────────────
//
// A narration REACHES HISTORY IN THE CLEAR, the same way an ask does: the codec is a claim-check,
// not encryption (ADR 0007), and a Summary is far too small to be offloaded. {@link Redact} replaces
// the value of anything written as a secret-shaped assignment — `token=…`, `Authorization: Bearer …`
// — because the ordinary mistake is a format string that interpolated a variable that happened to be
// in scope. It is not a scanner and is not sold as one.
//
// AND IT IS NOT A PLACE FOR A PAYLOAD. `Say` takes a sentence and nothing else — no context, no
// fields, no object. That is the design and not an omission: values belong in a Dataset, where they
// are queryable, deduplicated and out of history. A narration says what the run means; the lake says
// what it found.
package narrate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/sdk/go/catalog"
	"github.com/medmahmoudi26/kontra/sdk/go/wfstate"
)

// MaxSentenceBytes is how many UTF-8 BYTES one sentence may take.
//
// ONE BUDGET, NOT TWO — `catalog.SummaryBudget`, the same number a dispatch's Method line and a
// Session's open are built to fit, because every Summary this SDK writes is the same kind of thing
// in the same place. BYTES, NOT CHARACTERS, because that is what the wire and the codec's offload
// threshold are counted in, and the two differ on exactly the prose an operator is most likely to
// write — an em dash, a hostname in a non-Latin script, the `·` this repo separates fields with.
//
// It is roughly two sentences of English. That is the size narration is FOR: a phase marker, a count
// with its meaning attached, the reason the next thing is about to happen. Anything that does not
// fit is not a narration — it is a payload, and belongs in a Dataset.
const MaxSentenceBytes = catalog.SummaryBudget

// MaxSentences is how many sentences one run may narrate before this package stops carrying them.
//
// PER EXECUTION, which is per history — so a run that continues-as-new starts a fresh budget,
// correctly: the new leg is a new history and the old one is already closed and archived.
//
// The number is chosen from both ends. Five events a sentence puts the ceiling at ~1,000 events,
// which cannot be what kills a workflow that Temporal will carry to 51,200 and that this repo has
// measured dying of other things at 8,000 refs. And an author narrating a phase at a time writes
// perhaps twenty in a long run, so the honest use never comes near it while the per-Unit
// mistake trips it inside the first two hundred Units.
const MaxSentences = 200

// Redacted is what a redacted value is replaced with. Short, because it is spent out of the same 200
// bytes as the sentence, and VISIBLE, because a sentence that quietly lost a word is a worse account
// than one that says which word it would not carry.
const Redacted = "[redacted]"

// tick is the duration a narration's timer asks for.
//
// THE SMALLEST POSITIVE ONE, and it is positive for a reason that is not a preference: Go's SDK
// returns an already-completed future for `d <= 0` without issuing a command, so a zero-duration
// timer — Python's exact spelling — would write no event at all here. The wait is not the point; the
// EVENT is, and the sentence rides on `TimerStarted`, which the server writes before anything is
// waited for. What the wall clock actually costs is the server's timer granularity (~1s), the same
// as Python's, and not something this package can shorten.
const tick = time.Nanosecond

// A secret written as an ASSIGNMENT — `token=abc`, `api_key: abc`, `password = abc`. The shape a
// leaked credential actually takes in a formatted string, and the only shape matched, because
// anything looser eats prose: `token bucket refilled` must survive, and a matcher keyed on the word
// alone would redact `bucket`.
//
// THE SECOND SPELLING of `hitl.secretKeyRe`'s word list, deliberately and not shared: that one is
// anchored to a WHOLE key in a mapping, this one has to find a word inside a sentence, and forcing
// one regexp to do both would make the shared thing wrong for both.
//
// THE SCHEME WORD IS STEPPED OVER, NOT CAPTURED. `Authorization: Bearer eyJhbGciOi…` is the commonest
// spelling of all of these, and a matcher that took the first word after the colon would redact
// `Bearer` and leave the credential standing beside it.
var secretAssignmentRe = regexp.MustCompile(
	`(?i)\b(?:[A-Za-z0-9]+[_-])?` +
		`(?:password|passwd|pwd|secret|secrets|token|api[_-]?key|apikey|access[_-]?key|` +
		`private[_-]?key|credential|credentials|authorization|cookie|session[_-]?key)` +
		`(?:[_-][A-Za-z0-9]+)?` +
		`\s*[:=]\s*(?:(?:bearer|basic|token)\s+)?(\S+)`)

// `Authorization: Bearer …` without the header name, which is how it is usually pasted, and `Basic …`
// beside it. Bounded to something long enough to be a credential rather than a word, so `bearer of
// bad news` is prose and `Bearer eyJhbGciOi…` is not.
var bearerRe = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]{12,})`)

// spent is the whole of the run's narration, once the budget is gone. Built to fit the same bound as
// any other sentence, and it names the fix rather than only the rule.
const spent = "kontra: narration budget spent — %d sentences in one run, and the rest of this run " +
	"is not narrated. Narrate a phase, not a Unit."

// state is this execution's narration, as the run holds it. Deterministic: `said` advances in call
// order, and a workflow's call order is what replay reproduces — so the budget is spent at the same
// sentence on a replay as on the original. Hung off the EXECUTION and never a package variable; see
// sdk/go/wfstate for what a package variable would do to every other run on the worker.
type state struct {
	said int
	// stopped: the budget was spent, the note was written, and nothing further is carried.
	stopped bool
}

var narration = wfstate.New(func() *state { return &state{} })

// Refused is a sentence this package would not write, returned from the {@link Say} that wrote it.
//
// RETURNED, NOT SWALLOWED, because every case that reaches here is deterministic: the same call
// refuses on every replay and on every run of the same code, so it is found the first time the
// workflow is exercised rather than in production. The alternative — carrying a bad sentence anyway —
// puts something in the transcript that is not what the author wrote, which is the one thing a
// record of somebody's own words must not do.
//
// A NON-RETRYABLE `ApplicationError`, so an author who returns it from their workflow gets a run
// that FAILS with the reason on it, rather than a workflow task that retries forever behind a run
// still reporting `running` — the invisible failure this whole surface exists to remove, produced by
// the surface itself. `errors.As` reaches it; {@link IsRefused} is the short spelling.
//
// THE SENTENCE-PER-UNIT CASE DOES NOT COME THROUGH HERE, deliberately: see {@link MaxSentences}.
func Refused(why string) error {
	return temporal.NewNonRetryableApplicationError(why, "NarrationRefused", nil)
}

// IsRefused reports whether err is this package refusing a sentence — for an author who wants to
// carry on rather than fail the run over a line of commentary.
func IsRefused(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == "NarrationRefused"
}

// Say writes one sentence into this run's transcript, here, at this point in the run.
//
//	narrate.Say(ctx, fmt.Sprintf("%d of %d apexes resolve; crawling those", live, len(apexes)))
//
// It becomes a `narration` turn in the transcript, in among the derived turns, at the instant it was
// written — and a labelled timer in Temporal's own UI, which is the same fact seen from the other
// side.
//
// COSTS FIVE HISTORY EVENTS AND ABOUT A SECOND, both measured. Narrate a phase; never a Unit. The
// package header says what happens if you do it anyway, and {@link MaxSentences} is where.
//
// Returns {@link Refused} for a sentence over {@link MaxSentenceBytes} — deterministic, and an
// authoring error. An empty sentence writes nothing and returns nil: a workflow that narrates
// nothing is unchanged, and that is what an empty one is.
func Say(ctx workflow.Context, sentence string) error {
	line, err := Summary(sentence)
	if err != nil {
		return err
	}
	if line == "" {
		// NOTHING IS STRICTLY BETTER THAN A BLANK TURN. `summaryOf` reads an empty Summary as no
		// Summary, so an empty sentence would land as a bare timer with no text on it — a row in
		// the transcript where a sentence was meant to be, which reads as a bug in the reader
		// rather than as an empty format string in the workflow.
		workflow.GetLogger(ctx).Warn("narrate.Say was given an empty sentence; nothing was written")
		return nil
	}

	st := narration.Of(ctx)
	if st.stopped {
		return nil
	}
	if st.said >= MaxSentences {
		st.stopped = true
		workflow.GetLogger(ctx).Warn(
			"narrate.Say: the narration budget is spent and the rest of this run will not be "+
				"narrated. Narrate a phase, not a Unit.", "sentences", MaxSentences)
		// THE REFUSAL IS ITSELF A TURN, which is the whole difference between refusing and going
		// quiet. An operator reading the transcript is told the account of this run is incomplete,
		// in the same place they would have read the rest of it.
		return emit(ctx, fmt.Sprintf(spent, MaxSentences))
	}

	st.said++
	return emit(ctx, line)
}

// Speak tells the operator where this run has got to. One sentence, and it RETURNS IMMEDIATELY.
//
//	for i, wave := range waves {
//		narrate.Speak(ctx, fmt.Sprintf("batch %d of %d", i+1, len(waves)))  // a PHASE, not a Unit
//		...
//	}
//
// ── THE PAIR, AND WHY THERE ARE TWO ────────────────────────────────────────────────────────────
//
// `Speak` and `Ask` are the two things a workflow says out loud, under the same two words Python
// uses (`from actorkit import ask, speak`) so the two SDKs keep ONE VOCABULARY and the transcript
// stays one reader over both:
//
//   - `Speak` COSTS HISTORY AND RETURNS IMMEDIATELY. Five events and about a second, then the run
//     carries on. Nobody has to be watching, and nothing is waiting for anyone.
//   - {@link hitl.Ask} COSTS HISTORY AND STOPS THE RUN, until a human answers or its deadline
//     expires.
//
// Reaching for the wrong one turns a progress line into a stalled run — a run sitting at
// `running` waiting for a person nobody told to look.
//
// ── A SENTENCE PER PHASE, NOT PER UNIT ─────────────────────────────────────────────────────────
//
// IT IS EXACTLY {@link Say} — the same timer, the same Summary, the same turn in the same
// transcript — so it inherits the same two bounds and neither is softened by being easier to reach:
// a sentence over {@link MaxSentenceBytes} is refused rather than truncated, and past
// {@link MaxSentences} in one run the narration is refused (the run is not), once, in the
// transcript. A `Speak` in the body of a 623-Unit sweep is the shape error those bounds exist to
// stop; counts and results belong in a Dataset, where they are queryable and out of history.
//
// ── WHY IT IS ALSO HERE, AND NOT ONLY AT THE TOP LEVEL ─────────────────────────────────────────
//
// The front door carries the pair as {@link kontra.Speak} and {@link kontra.Ask}, which is the
// literal peer of Python's top-level import. But package `kontra` is the ACTOR surface: importing
// it links the actor host — redis, S3, the engine — and a Go module that only WRITES WORKFLOWS
// (examples/go/dnssweep) deliberately depends on none of that. Python needs no such split because
// its verbs are resolved lazily per module; Go resolves imports at the package. So a caller-only
// module reaches this spelling, the front door reaches the other, and both are the same call.
func Speak(ctx workflow.Context, sentence string) error {
	// DELEGATION, NOT A SECOND IMPLEMENTATION: one budget, one redaction, one shape of turn,
	// however it was spelled — and every existing Say caller is untouched.
	return Say(ctx, sentence)
}

// Said is how many sentences this run has narrated so far, and whether the budget is spent — the
// workflow's own view of its own account, for a caller that wants to branch on it rather than
// discover the ceiling in a transcript.
func Said(ctx workflow.Context) (n int, budgetSpent bool) {
	st := narration.Of(ctx)
	return st.said, st.stopped
}

// Summary is the one line a narration puts on its own event — normalised, redacted, and within
// budget.
//
// A PLAIN FUNCTION, like `catalog.DispatchSummary`, and for the same reason: the whole discipline of
// what does and does not reach history is decidable without a Temporal cluster, so it is assertable
// in a unit test rather than only in an integration one.
//
// THE BOUND IS CHECKED ON WHAT WOULD BE WRITTEN, not on what was passed in — redaction happens
// first, so a sentence is never refused for the size of a credential that was never going to be
// carried anyway.
func Summary(sentence string) (string, error) {
	line := Redact(OneLine(sentence))
	if size := len(line); size > MaxSentenceBytes {
		short := line
		if len(short) > 60 {
			short = trimRunes(short, 60)
		}
		return "", Refused(fmt.Sprintf(
			"a narration is one line of at most %d bytes and this one is %d: %s… — shorten it, or "+
				"put the detail in a Dataset. It is not truncated, because half a sentence may mean "+
				"something its author did not write.", MaxSentenceBytes, size, short))
	}
	return line, nil
}

// OneLine is `sentence` with every run of whitespace collapsed to one space, and trimmed.
//
// A SUMMARY IS SINGLE-LINE by Temporal's own definition — it is a bar label, rendered as single-line
// markdown — so a sentence built with a newline in it would break the surface it was written for.
// This is normalisation and not truncation: every word survives, in order, and the author's meaning
// with them.
func OneLine(sentence string) string {
	return strings.Join(strings.FieldsFunc(sentence, unicode.IsSpace), " ")
}

// Redact is `sentence` with the value of any secret-shaped assignment replaced by {@link Redacted}.
//
// NOT A SECURITY BOUNDARY and not sold as one — see the package header. It is a guard against the
// ordinary mistake, a format string that interpolated something that happened to be in scope, and it
// catches that one where the sentence names what it is carrying.
//
// IT REPLACES RATHER THAN DROPPING THE SENTENCE, and it does not fail. A run that died because its
// narration was impolite would be an outage created by a safety rule, and the author's fix — stop
// putting the value in the sentence — is the same either way.
func Redact(sentence string) string {
	// Assignments first, so a named header is redacted as a whole; the bare-scheme pass then picks
	// up the pasted `Bearer …` that named nothing, and cannot re-match what the first pass left
	// behind ({@link Redacted} carries characters no credential pattern here accepts).
	return cutGroup(bearerRe, cutGroup(secretAssignmentRe, sentence))
}

// cutGroup replaces the FIRST CAPTURING GROUP of every match with {@link Redacted}, keeping
// everything around it. Only the value is replaced; the key stays, because "there was a token here
// and kontra would not carry it" is a more useful line than a sentence with a hole in it.
func cutGroup(re *regexp.Regexp, s string) string {
	spans := re.FindAllStringSubmatchIndex(s, -1)
	if spans == nil {
		return s
	}
	var b strings.Builder
	at := 0
	for _, m := range spans {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		b.WriteString(s[at:m[2]])
		b.WriteString(Redacted)
		at = m[3]
	}
	b.WriteString(s[at:])
	return b.String()
}

// emit is the one command this package issues.
//
// A TIMER, AWAITED. The wait is not the point — the event is — and the sentence is on `TimerStarted`,
// which is written before anything is waited for. So a fire-and-forget narration would still reach
// history, and it is not done, for two reasons that are about the transcript rather than about the
// sentence. A workflow that finished before its timer fired would CANCEL it, and `TimerCanceled` is a
// failure-category event: every sentence would leave one behind, and a run that failed after
// narrating would report its last failure as an author's remark. And an abandoned coroutine at
// workflow completion is exactly the kind of warning a decoration must not earn.
//
// So `Say` pays the round trip. It is also, usefully, the most visible part of the price.
func emit(ctx workflow.Context, line string) error {
	return workflow.NewTimerWithOptions(ctx, tick, workflow.TimerOptions{Summary: line}).Get(ctx, nil)
}

// trimRunes keeps at most n BYTES of s without cutting a character in half — the excerpt a refusal
// quotes back at its author.
func trimRunes(s string, n int) string {
	kept := 0
	for i, r := range s {
		if i+len(string(r)) > n {
			break
		}
		kept = i + len(string(r))
	}
	return s[:kept]
}
