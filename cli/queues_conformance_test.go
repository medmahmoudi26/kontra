package main

import (
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/corpus"
	"github.com/medmahmoudi26/kontra/cli/internal/queues"
	"github.com/medmahmoudi26/kontra/cli/internal/tmux"
)

// The CLI ARM of shared/conformance/queues.json — and it is the arm that did not exist.
//
// `queues.Shared` in api.go carried the comment "byte-for-byte the peer of
// runtime/handler/internal/identity.SharedQueue and orchestrator taskQueue()", and nothing in this suite
// ever ran it against either. Its ONE appearance was the contrast half of an assertion about
// workflow queues (`workflowQueue(...) == queues.Shared(...)` must be false), which proves the two
// differ and says nothing about whether this one is right. So the eight-way contract was a
// seven-way contract with a comment on the eighth, in the binary an operator actually types.
//
// The tmux session name is here for the same reason. `kontra serve --tmux` mints it, the Monitor
// looks a pane up by it, and the browser derives it a third time — and when they disagree the
// Worker runs perfectly while its tile says NO SESSION, which is the exact thing ADR 0020 says a
func TestTheSharedQueueMatchesTheCorpus(t *testing.T) {
	for _, c := range corpus.LoadQueues(t).Shared.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := queues.Shared(c.Name, c.Version); got != c.Expect {
				t.Errorf("queues.Shared(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
}

// The two tmux derivations this binary owns, against the corpus's two answers per row.
//
// `worker` is an ACTOR's Worker — a name and a version and nothing else, and a fallback that
// exists only so the string stays attachable. `machine` is a fleet MACHINE's session, which is
// also given the fleet's tag, because a `fleet up` with no Actor placed on it still has
// Terminals. The two fallbacks differ ON PURPOSE and the corpus says why; what must not differ
// is either one of them from its peer across the language boundary.
func TestTheTmuxSessionNamesMatchTheCorpus(t *testing.T) {
	for _, c := range corpus.LoadQueues(t).TmuxSession.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := tmux.Session(c.Actor, c.Version); got != c.Worker {
				t.Errorf("tmux.Session(%q,%q) = %q, want %q", c.Actor, c.Version, got, c.Worker)
			}
			if got := fleetSessionName(c.Actor, c.Version, c.Tag); got != c.Machine {
				t.Errorf("fleetSessionName(%q,%q,%q) = %q, want %q",
					c.Actor, c.Version, c.Tag, got, c.Machine)
			}
		})
	}
}

// A Machine's session name reaches a command run as root over ssh, so every answer this binary
// produces must survive the whitelist the backend applies before it interpolates one. Asserted
// over the corpus rather than on the three rows somebody thought of.
func TestEveryMachineSessionNameIsSafeToInterpolate(t *testing.T) {
	for _, c := range corpus.LoadQueues(t).TmuxSession.Cases {
		if !safeSessionName.MatchString(c.Machine) {
			t.Errorf("%s: %q would be refused by panels/ids.ts:assertSafe('session')", c.Why, c.Machine)
		}
	}
}

// The Go arm of §session_kind — which `@kontra` tags the Monitor will admit (ADR 0043).
//
// THIS SIDE IS THE WRITER AND THE OTHER SIDE IS THE READER, which is exactly why it is a corpus and
// not two literals. `kontra serve --tmux` sets the option; `panels/local.ts:isKontraSession` decides
// whether the wall shows what it finds. A kind this binary writes and the streamer does not know is
// a Worker running perfectly whose tile is absent — ADR 0020's one forbidden failure — and a kind
// the streamer admits and this binary never writes is a door nobody meant to leave open.
func TestTheSessionKindVocabularyMatchesTheCorpus(t *testing.T) {
	for _, c := range corpus.LoadQueues(t).SessionKind.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := tmux.SessionKind(c.Tag); got != c.Kind {
				t.Errorf("SessionKind(%q) = %q, corpus says %q", c.Tag, got, c.Kind)
			}
			if got := tmux.IsKnownSessionKind(c.Tag); got != c.Known {
				t.Errorf("IsKnownSessionKind(%q) = %v, corpus says %v", c.Tag, got, c.Known)
			}
		})
	}
}

// Every tag this binary WRITES must be one the reader will admit.
//
// The corpus pins the vocabulary; this pins that the three writers stay inside it. A `KontraWatchTag`
// that drifted to `watched:` would keep passing every row above — the corpus does not know which
// function produced a string — while every session the CLI tagged vanished from the wall.
func TestEveryTagThisBinaryWritesIsAdmissible(t *testing.T) {
	written := map[string]string{
		"KontraSessionTag":  tmux.KontraSessionTag("probe", "0.1.0"),
		"KontraWorkflowTag": tmux.KontraWorkflowTag("hunt"),
		"KontraWatchTag":    tmux.KontraWatchTag("repl"),
	}
	if len(written) != len(tmux.SessionKinds) {
		t.Fatalf("%d tag writers against %d kinds — a kind with no writer, or a writer with no kind",
			len(written), len(tmux.SessionKinds))
	}
	seen := map[string]bool{}
	for name, tag := range written {
		if !tmux.IsKnownSessionKind(tag) {
			t.Errorf("%s wrote %q, which the reader would refuse", name, tag)
		}
		seen[tmux.SessionKind(tag)] = true
	}
	// Each writer covers a DIFFERENT kind. Three writers all emitting `actor:` would satisfy the
	// loop above and leave two kinds unwritten by anything.
	for _, kind := range tmux.SessionKinds {
		if !seen[kind] {
			t.Errorf("kind %q is in SessionKinds and no writer produces it", kind)
		}
	}
}
