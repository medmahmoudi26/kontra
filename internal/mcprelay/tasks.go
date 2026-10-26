package mcprelay

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/unimpl"
)

// Tasks is the client side of gate-owned MCP tasks (spec/relay.md §11.6–§11.8 for legacy readers, §11.12 for
// modern ones). It reads only Postgres, on any replica; a task of another buyer is answered as an unknown one
// (-32602). A task may be read from either era by its buyer, and each era presents the same log:
//
//   - legacy: tasks/get (the projection, operator mcpMeta), tasks/result (blocks on LISTEN until terminal,
//     delivering pending input requests first on its own SSE stream, event ids k.<taskId>.<seq>; then §11.8's
//     stored outcome with the related-task key), tasks/list (the principal's own, newest first, gate cursors;
//     refused for anonymous), tasks/cancel (kontra.cancel, D57: canceled within 10 s whatever the upstream
//     answers). A client response whose id is a gate request id k.<taskId>.<seq> is an input_response, accepted
//     once and only from the buyer (else 202 and dropped).
//   - modern: tasks/get (resultType complete, inputRequests keyed by gate id, a terminal task presented by its
//     terminal event: a stored CallToolResult is completed even with isError), tasks/update (each outstanding
//     key logged once, kontra.input_response signalled), tasks/cancel (kontra.cancel, not awaited, empty ack);
//     tasks/result and tasks/list are -32601; a request that does not declare the extension is 400 with -32021
//     (data.requiredCapabilities naming it, D176). A client that closes its stream during the 2 s inline wait,
//     before the outcome or the CreateTaskResult is sent, makes the gate send the task kontra.cancel (D178).
//
// Work packages: M3 WP-06 (legacy), M3 WP-07 (modern).
type Tasks struct{}

// Serve answers one tasks/* request (or a client response carrying a gate request id) for the buyer.
func (t *Tasks) Serve(ctx context.Context, era mcpwire.Era, buyer uuid.UUID, m *mcpwire.Message) (json.RawMessage, *mcpwire.Error, error) {
	return nil, nil, unimpl.Error("mcprelay.Tasks.Serve", "M3 WP-06")
}

// Create creates a gate-owned task for an admitted task-augmented call (§11.5 steps 1–2, §11.12 steps 2–4):
// the task row (protocol mcp, created quoted on a quote), the work item holding the request with its _meta and
// the client session's hash and JSON-RPC id, and the TaskLifecycle start. It returns once the row is committed.
func (t *Tasks) Create(ctx context.Context, req CreateTask) (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("mcprelay.Tasks.Create", "M3 WP-06")
}

// CreateTask is what a task-augmented call stores.
type CreateTask struct {
	TenantID   uuid.UUID
	Service    string
	BuyerID    uuid.UUID
	Principal  string
	Via        string
	Era        mcpwire.Era
	Request    json.RawMessage // the tools/call as received (the work item, never in Temporal history)
	MCPSession *[32]byte       // the creating client session's hash (legacy), for the preamble (tunnel.md §8.1)
	RequestID  string          // the client's JSON-RPC id as a string, reused upstream (§11.5 step 3)
}
