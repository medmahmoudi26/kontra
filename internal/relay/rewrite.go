package relay

import (
	"errors"
	"fmt"
	"maps"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/hooks"
)

// ErrForeignTask is an upstream event naming another upstream task than the attempt's: the attempt fails
// upstream_error and the event is never rewritten into the gate task (spec §4, D53).
var ErrForeignTask = errors.New("relay: the upstream event names another task than the attempt's")

// IDs maps one attempt's upstream ids to the gate's (spec §4). Upstream ids never reach a caller.
type IDs struct {
	GateTaskID        a2a.TaskID
	GateContextID     string
	UpstreamTaskID    a2a.TaskID // the attempt's first upstream task id; empty until it arrives
	UpstreamContextID string
}

// ToGate rewrites every upstream task id and context id in ev to the gate's, at every depth (task.id,
// task.contextId, status.message.taskId, history[*].taskId and contextId), and removes every kontra.
// key from the metadata of the task, status, artifacts, messages and parts, except kontra.sealed on a
// part of the sealed envelope type (spec §2.5). An event naming another upstream task is an error: the
// attempt fails upstream_error. ev is not modified.
//
// Nested messages are this task's by construction, so their ids are set to the gate's whatever the
// upstream wrote; referenceTaskIds naming the attempt's own upstream task become the gate task's, and
// every other one (another upstream task's id) is dropped.
func (m IDs) ToGate(ev a2a.Event) (a2a.Event, error) {
	switch e := ev.(type) {
	case *a2a.Task:
		if err := m.check(e.ID); err != nil {
			return nil, err
		}
		out := *e
		out.ID, out.ContextID = m.GateTaskID, m.GateContextID
		out.Status.Message = m.message(e.Status.Message)
		if e.History != nil {
			out.History = make([]*a2a.Message, len(e.History))
			for i, h := range e.History {
				out.History[i] = m.message(h)
			}
		}
		if e.Artifacts != nil {
			out.Artifacts = make([]*a2a.Artifact, len(e.Artifacts))
			for i, a := range e.Artifacts {
				out.Artifacts[i] = artifact(a)
			}
		}
		out.Metadata = hooks.StripService(e.Metadata, false)
		return &out, nil
	case *a2a.TaskStatusUpdateEvent:
		if err := m.check(e.TaskID); err != nil {
			return nil, err
		}
		out := *e
		out.TaskID, out.ContextID = m.GateTaskID, m.GateContextID
		out.Status.Message = m.message(e.Status.Message)
		out.Metadata = hooks.StripService(e.Metadata, false)
		return &out, nil
	case *a2a.TaskArtifactUpdateEvent:
		if err := m.check(e.TaskID); err != nil {
			return nil, err
		}
		out := *e
		out.TaskID, out.ContextID = m.GateTaskID, m.GateContextID
		out.Artifact = artifact(e.Artifact)
		out.Metadata = hooks.StripService(e.Metadata, false)
		return &out, nil
	case *a2a.Message:
		if e.TaskID != "" {
			if err := m.check(e.TaskID); err != nil {
				return nil, err
			}
		}
		return m.message(e), nil
	case nil:
		return nil, errors.New("relay: no upstream event")
	}
	return nil, fmt.Errorf("relay: unexpected upstream event %T", ev)
}

func (m IDs) check(upstreamTaskID a2a.TaskID) error {
	if upstreamTaskID == "" {
		return fmt.Errorf("%w: the event names no task", ErrForeignTask)
	}
	if m.UpstreamTaskID != "" && upstreamTaskID != m.UpstreamTaskID {
		return ErrForeignTask
	}
	return nil
}

// message rewrites one nested message: gate ids, references, Kontra's keys stripped from it and its parts.
func (m IDs) message(msg *a2a.Message) *a2a.Message {
	if msg == nil {
		return nil
	}
	out := *msg
	out.TaskID, out.ContextID = m.GateTaskID, m.GateContextID
	out.ReferenceTasks = nil
	for _, r := range msg.ReferenceTasks {
		if m.UpstreamTaskID != "" && r == m.UpstreamTaskID {
			out.ReferenceTasks = append(out.ReferenceTasks, m.GateTaskID)
		}
	}
	out.Metadata = hooks.StripService(msg.Metadata, false)
	out.Parts = parts(msg.Parts)
	return &out
}

func artifact(a *a2a.Artifact) *a2a.Artifact {
	if a == nil {
		return nil
	}
	out := *a
	out.Metadata = hooks.StripService(a.Metadata, false)
	out.Parts = parts(a.Parts)
	return &out
}

// parts strips Kontra's keys from each part's metadata, keeping kontra.sealed on a sealed envelope part;
// a part with nothing to strip is shared, not copied (parts are never modified after they are emitted).
func parts(ps a2a.ContentParts) a2a.ContentParts {
	if ps == nil {
		return nil
	}
	out := make(a2a.ContentParts, len(ps))
	for i, p := range ps {
		if p == nil {
			continue
		}
		md := hooks.StripService(p.Metadata, p.MediaType == hooks.SealedMediaType)
		if len(md) == len(p.Metadata) {
			out[i] = p
			continue
		}
		cp := *p
		cp.Metadata = md
		out[i] = &cp
	}
	return out
}

// ToUpstream substitutes the sticky attempt's upstream ids into a follow-up message (spec §7), drops a
// caller contextId on a first message (D53; UpstreamContextID is then the context the upstream gave an
// earlier task of the same gate context on this backend, or empty) and maps referenceTaskIds to the
// caller's own upstream tasks in the same gate context and backend, dropping the rest (spec §4). msg is
// not modified; message ids pass unchanged.
func (m IDs) ToUpstream(msg *a2a.Message, references map[a2a.TaskID]a2a.TaskID) (*a2a.Message, error) {
	if msg == nil {
		return nil, errors.New("relay: no message to send upstream")
	}
	out := *msg
	out.TaskID, out.ContextID = m.UpstreamTaskID, m.UpstreamContextID
	out.ReferenceTasks = nil
	for _, r := range msg.ReferenceTasks {
		if up, ok := references[r]; ok && up != "" {
			out.ReferenceTasks = append(out.ReferenceTasks, up)
		}
	}
	if msg.Metadata != nil {
		out.Metadata = maps.Clone(msg.Metadata)
	}
	return &out, nil
}
