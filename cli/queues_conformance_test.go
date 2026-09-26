package main

import (
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/corpus"
	"github.com/medmahmoudi26/kontra/cli/internal/queues"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
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
// The Worker name is here for the same reason. `kontra serve` mints it, `cli/fleet.go` derives a
// Machine's, and TypeScript derives the Worker's a third time — and when they disagree the Worker
// runs perfectly under a name nothing else can find. It was the tmux session name until the Monitor
// was deleted; the folding outlived tmux because changing it renames every Worker at once.
func TestTheSharedQueueMatchesTheCorpus(t *testing.T) {
	for _, c := range corpus.LoadQueues(t).Shared.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := queues.Shared(c.Name, c.Version); got != c.Expect {
				t.Errorf("queues.Shared(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
}

// The Worker NAME this binary derives, against the corpus.
//
// ONE DERIVATION NOW AND THERE WERE TWO. The corpus's `machine` column was a fleet MACHINE's tmux
// session — derived by `cli/fleet.go:fleetSessionName`, given the fleet's tag as well because a
// `fleet up` with no Actor placed on it still had Terminals — and it went with the Monitor along
// with the converge that created those sessions. `worker` is what is left: an Actor's Worker, a
// name and a version and nothing else, and a fallback that exists so the string still identifies
// something. What must not differ is this from its peer across the language boundary.
func TestTheWorkerNamesMatchTheCorpus(t *testing.T) {
	for _, c := range corpus.LoadQueues(t).TmuxSession.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := cliutil.ActorWorkerName(c.Actor, c.Version); got != c.Worker {
				t.Errorf("ActorWorkerName(%q,%q) = %q, want %q", c.Actor, c.Version, got, c.Worker)
			}
		})
	}
}
