package kontra

import (
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/sdk/go/hitl"
	"github.com/medmahmoudi26/kontra/sdk/go/narrate"
)

// ── THE TWO THINGS A WORKFLOW SAYS OUT LOUD ────────────────────────────────────────────────────
//
//	kontra.Speak(ctx, fmt.Sprintf("batch %d of %d", i, n))      // tell the operator where you are
//	got, err := kontra.Ask[Approval](ctx, "Approve these 12 hosts?")
//
// THEY ARE A PAIR, AND THE DIFFERENCE IS THE WHOLE REASON THERE ARE TWO. {@link Speak} costs
// history and RETURNS IMMEDIATELY; {@link Ask} costs history AND STOPS THE RUN until a human moves
// it. Confusing them turns a progress line into a stalled run, which is why they are exposed
// side by side here rather than met separately in two packages.
//
// THE SAME PAIR PYTHON EXPOSES, under the same two words: `from kontra import ask, speak`. One
// vocabulary across the SDKs is not decoration — the transcript is ONE READER over both, and a Go
// run's turns are indistinguishable from a Python run's because these delegate to the same
// mechanisms the Python peers use (a Summary-carrying timer; a `kontra.ask.<id>` memo entry) rather
// than reaching the same outcome another way.
//
// EXACTLY TWO NAMES, as in Python. Everything else about either verb stays in the package that owns
// it — `lib/hitl` for an ask's options, {@link hitl.IsExpired} and {@link hitl.Pending}, `lib/narrate`
// for {@link narrate.IsRefused}, {@link narrate.Said} and the budget constants — because a second
// set of names for the same things is how two vocabularies start.
//
// ── THE FRONT DOOR IS NOT THE ONLY DOOR, AND IN GO IT CANNOT BE ────────────────────────────────
//
// This package is the ACTOR surface, so importing it links the actor host: redis, S3, the engine.
// A Go module that only WRITES WORKFLOWS — `examples/go/dnssweep`, whose go.mod says so — depends
// on none of that, and must not start to merely because it wanted to narrate a phase. Python has no
// such split: its two verbs are resolved lazily, per module, by `actorkit/__init__.py`, so the top
// level costs an author nothing it did not already cost. Go resolves imports at the package.
//
// So the same pair is reachable BOTH ways, and the word is the same either way: {@link narrate.Speak}
// and {@link hitl.Ask} for a caller-only module, `kontra.Speak` and `kontra.Ask` here for anyone
// already inside this SDK. Both spellings are the same call — these delegate rather than
// reimplement, which is what keeps one budget, one redaction, and one shape of turn.
//
// They take a `workflow.Context`, which is not a contradiction with living beside the Actor
// surface: this package is the peer of Python's `actorkit`, one import for the author's whole
// surface. Nothing here runs in an Actor; a Method's body has no workflow context to pass.

// Speak writes one sentence into this run's transcript, here, at this point in the run — and
// returns immediately.
//
//	for i, wave := range waves {
//		kontra.Speak(ctx, fmt.Sprintf("batch %d of %d", i+1, len(waves)))  // a phase, not a Unit
//		...
//	}
//
// A SENTENCE PER PHASE, NEVER ONE PER UNIT. It is exactly {@link narrate.Say} — same timer, same
// Summary, same turn — so it inherits both of that package's bounds unchanged, and being easier to
// reach softens neither: a sentence over {@link narrate.MaxSentenceBytes} is REFUSED rather than
// truncated, and past {@link narrate.MaxSentences} in one run the narration is refused (the run is
// not), once, in the transcript. A sentence in the body of a 623-Unit sweep is the shape error
// those bounds exist to stop — invisible in a test, ten minutes of pure waiting in production.
//
// NEVER A CREDENTIAL. It reaches history in the clear (the codec is a claim-check, not encryption —
// ADR 0007); {@link narrate.Redact} catches the ordinary mistake and is not a boundary.
func Speak(ctx workflow.Context, sentence string) error {
	// DELEGATION, NOT A SECOND IMPLEMENTATION: one budget, one redaction, one shape of turn,
	// however it was spelled — and every existing narrate.Say caller is untouched.
	return narrate.Speak(ctx, sentence)
}

// AskOption tunes one ask. An ALIAS of {@link hitl.Option}, not a new type, so `lib/hitl`'s own
// options pass straight into {@link Ask} and there is exactly one set of names for them:
//
//	got, err := kontra.Ask[Approval](ctx, "Approve these 12 hosts?",
//		hitl.Context(map[string]any{"n": 12}),
//		hitl.Deadline(4*time.Hour))
type AskOption = hitl.Option

// Ask parks this workflow on a question, and returns what a human answered.
//
//	got, err := kontra.Ask[Approval](ctx, "Approve these 12 hosts?")
//	switch {
//	case hitl.IsExpired(err):
//		// nobody answered in time — YOUR decision what that means
//	case err != nil:
//		return err
//	case got.Approve:
//		...
//	}
//
// IT STOPS THE RUN, which is the whole difference from {@link Speak}: this one waits for a person,
// and a run parked on it sits at `running` until somebody answers or the deadline expires. Say what
// a run is DOING with `Speak`; use `Ask` only where it must not proceed unattended.
//
// `T` IS THE ANSWER'S SHAPE — the form the operator fills in is rendered from its JSON Schema, and
// the answer route validates against that same document. `Ask[any]` constrains nothing. The options,
// the expiry predicate and the pending list are {@link hitl}'s; this is that package's `Ask`, reached
// by the name the top level exposes.
//
// NEVER A CREDENTIAL IN THE CONTEXT. It reaches history in the clear; {@link hitl.Redact} drops a
// value under a secret-shaped key and is not a boundary.
func Ask[T any](ctx workflow.Context, prompt string, opts ...AskOption) (T, error) {
	return hitl.Ask[T](ctx, prompt, opts...)
}
