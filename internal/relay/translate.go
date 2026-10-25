package relay

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Translate turns one upstream event (ids already rewritten) into the events the gate logs (spec §6.3):
//
//   - the first Task of an attempt: its status as a status event and each artifact as an artifact event;
//   - a later Task (polling, or the first one after a re-attach): only the difference from the gate's
//     projection, a2aevent.Recover semantics, so nothing already in the log is logged twice;
//   - statusUpdate and artifactUpdate as they are, except a first chunk with append: true for an artifact
//     the gate has not seen is logged append: false, and an empty artifact is dropped;
//   - a Message: a terminal completed status carrying it (D5);
//   - an upstream terminal status without a message gains a gate-written agent message naming the state.
func Translate(up a2a.Event, projection *a2a.Task, firstOfAttempt bool) ([]a2a.Event, error) {
	return nil, unimpl.Error("relay.Translate", "WP-04")
}

// Failed builds the terminal failed status the relay or write_terminal logs: an agent-role message with
// one text part saying what happened, and metadata["kontra.failure"] = reason (spec §6.4).
func Failed(info a2a.TaskInfoProvider, reason, text string) *a2a.TaskStatusUpdateEvent {
	panic(unimpl.Error("relay.Failed", "WP-04"))
}
