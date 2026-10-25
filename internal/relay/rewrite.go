package relay

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

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
func (m IDs) ToGate(ev a2a.Event) (a2a.Event, error) {
	return nil, unimpl.Error("relay.IDs.ToGate", "WP-04")
}

// ToUpstream substitutes the sticky attempt's upstream ids into a follow-up message (spec §7), drops a
// caller contextId on a first message (D53) and maps referenceTaskIds to the caller's own upstream tasks
// in the same gate context and backend, dropping the rest (spec §4).
func (m IDs) ToUpstream(msg *a2a.Message, references map[a2a.TaskID]a2a.TaskID) (*a2a.Message, error) {
	return nil, unimpl.Error("relay.IDs.ToUpstream", "WP-04")
}
