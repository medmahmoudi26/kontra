package mcprelay

import (
	"context"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/unimpl"
)

// Dispatcher is the MCP half of lifecycle.DispatchActivities, for tasks with protocol mcp; internal/relay's
// Dispatcher hands those to it. It runs on the replica holding the attempt's tunnel session
// (kontra-tunnel-<replica>) or on kontra-direct, heartbeats every 5 s, fences every write (D77) and returns
// only an outcome and a reason (D102); nothing it relays enters Temporal history.
//
// It speaks to the upstream in the era of the request that created the task (spec/relay.md §11.12):
//
//   - 2025-11-25 (§11.5 steps 3–6, D22): its own upstream session (an initialize declaring the client's
//     elicitation and sampling capabilities, recorded in attempts.upstream_session); the task-augmented
//     tools/call reusing the client's JSON-RPC id, with ttl = time to the deadline; the upstream taskId as the
//     first upstream task id; tasks/get polling at the upstream's pollInterval (0.5–30 s) and a GET on its
//     session; on input_required, tasks/result on its session to receive the upstream's elicitation/create and
//     sampling/createMessage, each logged as an input_request under the gate id k.<taskId>.<seq>; the client's
//     answers, tailed from the log, written upstream under the original ids; at terminal, tasks/result logged
//     as the result, one calls row with task_id, and its session closed.
//   - 2026-07-28 (D159): stateless requests on the attempt's session with the stored client _meta; a
//     CallToolResult is the result; a CreateTaskResult starts tasks/get polling (Mcp-Name = the upstream id);
//     inputRequests and an InputRequiredResult become input_request events under gate ids, the upstream keys
//     and requestState kept in the event; answers go upstream as tasks/update, or as the tools/call sent again.
//     The first InputRequiredResult counts as the first upstream task id, its first inputRequests key recorded
//     as attempts.upstream_task_id, so the call is never sent afresh after it (D178).
//
// Every exchange carries the client session's hash and the client's request id in its preamble, so a sealed
// call's binding holds (spec/tunnel.md §8.1, spec/ext-sealed.md §5 rule 4). After the first upstream task id,
// a lost tunnel fails the task tunnel_lost (D32, mcp_task_resume).
//
// Work packages: M3 WP-06 (2025-11-25), M3 WP-07 (2026-07-28).
type Dispatcher struct{}

// Dispatch runs one dispatch of an MCP task.
func (d *Dispatcher) Dispatch(ctx context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	return lifecycle.DispatchResult{}, unimpl.Error("mcprelay.Dispatcher.Dispatch", "M3 WP-06")
}

// ForwardCancel sends tasks/cancel upstream when the upstream declared it (legacy: tasks.cancel; modern: the
// extension), and returns at most 10 s later whatever it answered: an MCP cancel always cancels (D57).
func (d *Dispatcher) ForwardCancel(ctx context.Context, in lifecycle.AttemptRef) (lifecycle.ForwardCancelResult, error) {
	return lifecycle.ForwardCancelResult{}, unimpl.Error("mcprelay.Dispatcher.ForwardCancel", "M3 WP-06")
}
