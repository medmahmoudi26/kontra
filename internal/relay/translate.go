package relay

import (
	"errors"
	"fmt"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/store"
)

// ErrMalformed is an upstream event the gate cannot log: an unknown state, an event without its task.
var ErrMalformed = errors.New("relay: malformed upstream event")

// ErrReservedPart is an upstream event with a url part in the gate's reserved kontra-blob: scheme (spec §10.4,
// D181): only the gate writes such parts, so every one in the log names an object of its own task. The task
// fails upstream_error.
var ErrReservedPart = errors.New("relay: the service sent a url part in the gate's reserved kontra-blob: scheme")

// Translate turns one upstream event (ids already rewritten) into the events the gate logs (spec §6.3):
//
//   - the first Task of an attempt: each artifact as an artifact event, then its status as a status
//     event (artifacts first, so a Task that arrives already terminal still logs them);
//   - a later Task (polling, or the first one after a re-attach): only the difference from the gate's
//     projection, a2aevent.Recover semantics, so nothing already in the log is logged twice;
//   - statusUpdate and artifactUpdate as they are, except a first chunk with append: true for an artifact
//     the gate has not seen is logged append: false, and an empty artifact is dropped;
//   - a Message: a terminal completed status carrying it (D5);
//   - an upstream terminal status without a message gains a gate-written agent message naming the state.
//
// up and projection are not modified. The relay calls CheckReserved on up first.
func Translate(up a2a.Event, projection *a2a.Task, firstOfAttempt bool) ([]a2a.Event, error) {
	if projection == nil {
		return nil, errors.New("relay: no projection to translate against")
	}
	var out []a2a.Event
	switch e := up.(type) {
	case *a2a.Message:
		msg := *e
		msg.TaskID, msg.ContextID = projection.ID, projection.ContextID
		out = append(out, a2a.NewStatusUpdateEvent(projection, a2a.TaskStateCompleted, &msg))
	case *a2a.Task:
		if _, err := validState(e.Status.State); err != nil {
			return nil, err
		}
		if !firstOfAttempt {
			out = a2aevent.Recover(projection, e)
			break
		}
		for _, a := range e.Artifacts {
			if a == nil || len(a.Parts) == 0 {
				continue
			}
			out = append(out, &a2a.TaskArtifactUpdateEvent{TaskID: projection.ID, ContextID: projection.ContextID, Artifact: a})
		}
		st := a2a.NewStatusUpdateEvent(projection, e.Status.State, e.Status.Message)
		st.Status.Timestamp = e.Status.Timestamp
		st.Metadata = e.Metadata
		out = append(out, st)
	case *a2a.TaskStatusUpdateEvent:
		out = append(out, e)
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact == nil || len(e.Artifact.Parts) == 0 {
			return nil, nil
		}
		if e.Append && !slices.ContainsFunc(projection.Artifacts, func(a *a2a.Artifact) bool { return a.ID == e.Artifact.ID }) {
			cp := *e
			cp.Append = false
			out = append(out, &cp)
		} else {
			out = append(out, e)
		}
	default:
		return nil, fmt.Errorf("%w: %T", ErrMalformed, up)
	}
	for i, ev := range out {
		st, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			continue
		}
		state, err := validState(st.Status.State)
		if err != nil {
			return nil, err
		}
		if state.Terminal() && st.Status.Message == nil {
			cp := *st
			cp.Status.Message = a2a.NewMessageForTask(a2a.MessageRoleAgent, projection,
				a2a.NewTextPart("the service ended the task: "+string(state)))
			out[i] = &cp
		}
	}
	return out, nil
}

// CheckReserved is ErrReservedPart for an upstream event any part of which the gate may log (a status message's,
// an artifact's, a Message's) is a url part in the gate's reserved kontra-blob: scheme, in any case (D181):
// only the gate writes such parts, so every one in the log names an object of its own task, and the task fails
// upstream_error. A Task's history is not checked: the relay never logs it, and it may echo what the service was
// sent.
func CheckReserved(ev a2a.Event) error {
	reserved := false
	switch e := ev.(type) {
	case *a2a.Task:
		reserved = e.Status.Message != nil && reservedIn(e.Status.Message.Parts)
		for _, a := range e.Artifacts {
			reserved = reserved || (a != nil && reservedIn(a.Parts))
		}
	case *a2a.TaskStatusUpdateEvent:
		reserved = e.Status.Message != nil && reservedIn(e.Status.Message.Parts)
	case *a2a.TaskArtifactUpdateEvent:
		reserved = e.Artifact != nil && reservedIn(e.Artifact.Parts)
	case *a2a.Message:
		reserved = reservedIn(e.Parts)
	}
	if reserved {
		return ErrReservedPart
	}
	return nil
}

// reservedIn reports whether parts include a url part in the gate's reserved kontra-blob: scheme.
func reservedIn(ps a2a.ContentParts) bool {
	for _, p := range ps {
		if p == nil {
			continue
		}
		if u, ok := p.Content.(a2a.URL); ok && blob.Reserved(string(u)) {
			return true
		}
	}
	return false
}

func validState(s a2a.TaskState) (store.State, error) {
	st, err := store.StateFromA2A(s)
	if err != nil {
		return "", fmt.Errorf("%w: task state %q", ErrMalformed, s)
	}
	return st, nil
}

// Failed builds the terminal failed status the relay logs: an agent-role message with one text part
// saying what happened, and metadata["kontra.failure"] = reason (spec §6.4). The shape and the closed set
// of reasons live in one place, store.FailedStatus, which write_terminal uses too: a reason outside the
// set is written as upstream_error.
func Failed(info a2a.TaskInfoProvider, reason, text string) *a2a.TaskStatusUpdateEvent {
	return store.FailedStatus(info, store.FailureReason(reason), text)
}
