package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
)

// Metadata keys the gate writes on a task it ends (spec §6.4, §8).
const (
	MetaFailure  = "kontra.failure"
	MetaCanceled = "kontra.canceled"
)

// CanceledClient is metadata["kontra.canceled"] on a task its caller canceled (spec §8). An earlier release also
// wrote declined and quote_expired (D351 withdrew them); rows that carry them are read as they are.
const CanceledClient = "client"

// MaxPatchBytes bounds an operator metadata patch (D59: patches cross Temporal history inline, at most
// 16 KiB).
const MaxPatchBytes = 16 << 10

// conflictRetries bounds the read-modify-write loops of WriteTerminal and ApplyMetadata, which retry on a
// concurrent write (they are defined at the task's current version, not at a version the caller read).
const conflictRetries = 8

var reasonText = map[FailureReason]string{
	ReasonNoSession:            "no session of this service was available",
	ReasonTunnelLost:           "the connection to the service was lost",
	ReasonUpstreamUnreachable:  "the service could not be reached",
	ReasonUpstreamError:        "the service returned an error",
	ReasonServiceUnavailable:   "the service is unavailable",
	ReasonTimeout:              "the task timed out",
	ReasonSealedRejected:       "a hosting node refused a sealed part",
	ReasonLifecycleUnavailable: "the task's lifecycle could not be started",
}

var canceledText = map[string]string{
	CanceledClient: "the task was canceled",
}

// stateText is TerminalStatus's text when it is given none.
var stateText = map[a2a.TaskState]string{
	a2a.TaskStateCompleted: "the task completed",
	a2a.TaskStateFailed:    "the task failed",
	a2a.TaskStateCanceled:  "the task was canceled",
	a2a.TaskStateRejected:  "the task was rejected",
}

// ReasonText is the default text of a failed status for reason.
func ReasonText(reason FailureReason) string { return reasonText[reason] }

// TerminalStatus builds a terminal status event the gate writes: an agent-role status message with one
// text part, and meta on both the message (spec ext-sealed.md §4a: the gate's terminal message carries
// kontra.failure) and the event, which merges it into the task's metadata. The relay and write_terminal
// both build their terminal events here, so there is one shape (spec §6.4). The text part is never empty:
// without a text it names the state.
func TerminalStatus(info a2a.TaskInfoProvider, state a2a.TaskState, meta map[string]any, text string) *a2a.TaskStatusUpdateEvent {
	if text == "" {
		if text = stateText[state]; text == "" {
			text = "the task's status changed"
		}
	}
	return GateStatus(info, state, meta, text)
}

// FailedStatus is `failed: <reason>` (spec §6.4): metadata["kontra.failure"] = reason and one text part
// saying what happened (ReasonText when text is empty). The reasons are a closed set, checked here for
// every writer: a reason outside it (a caller's bug; relay.Failed takes a string) is written as
// upstream_error, §6.4's reason for an error of the relay that is not a lost upstream, never as itself.
func FailedStatus(info a2a.TaskInfoProvider, reason FailureReason, text string) *a2a.TaskStatusUpdateEvent {
	if !reason.Valid() {
		reason = ReasonUpstreamError
	}
	if text == "" {
		text = ReasonText(reason)
	}
	return TerminalStatus(info, a2a.TaskStateFailed, map[string]any{MetaFailure: string(reason)}, text)
}

// CanceledStatus is `canceled` with metadata["kontra.canceled"] = canceled (client; none when empty, or when
// canceled is outside that closed set).
func CanceledStatus(info a2a.TaskInfoProvider, canceled, text string) *a2a.TaskStatusUpdateEvent {
	if _, ok := canceledText[canceled]; !ok {
		canceled = ""
	}
	if text == "" {
		text = canceledText[canceled]
		if text == "" {
			text = canceledText[CanceledClient]
		}
	}
	var meta map[string]any
	if canceled != "" {
		meta = map[string]any{MetaCanceled: canceled}
	}
	return TerminalStatus(info, a2a.TaskStateCanceled, meta, text)
}

// TerminalWrite is what write_terminal logs when no relay can (spec §14.2): retries exhausted, the
// owning replica dead, timeout, the cancel of an input-required task.
type TerminalWrite struct {
	TaskID   uuid.UUID
	State    State         // failed or canceled (or completed/rejected relayed from an upstream answer)
	Reason   FailureReason // for failed: metadata["kontra.failure"]
	Canceled string        // for canceled: metadata["kontra.canceled"] = client
	Text     string        // the agent-role status message's one text part; empty: the reason's default
}

func (w *TerminalWrite) event(info a2a.TaskInfoProvider) (*a2a.TaskStatusUpdateEvent, error) {
	switch w.State {
	case StateFailed:
		if !w.Reason.Valid() {
			return nil, fmt.Errorf("store: failure reason %q is not one of spec §6.4's", w.Reason)
		}
		return FailedStatus(info, w.Reason, w.Text), nil
	case StateCanceled:
		if _, ok := canceledText[w.Canceled]; w.Canceled != "" && !ok {
			return nil, fmt.Errorf("store: kontra.canceled %q", w.Canceled)
		}
		return CanceledStatus(info, w.Canceled, w.Text), nil
	case StateCompleted, StateRejected:
		if w.Text == "" {
			return nil, fmt.Errorf("store: a %s terminal write needs its text", w.State)
		}
		return TerminalStatus(info, w.State.A2A(), nil, w.Text), nil
	}
	return nil, fmt.Errorf("store: %q is not a terminal state", w.State)
}

// WriteTerminal logs the terminal status event at the current version. A task that is already terminal
// is left alone and its terminal seq returned: the call is idempotent, as a retried activity needs.
func (l *Log) WriteTerminal(ctx context.Context, w TerminalWrite) (int64, error) {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var last error
	for range conflictRetries {
		row, err := l.Load(ctx, w.TaskID)
		if err != nil {
			return 0, err
		}
		if row.State.Terminal() {
			return terminalSeq(ctx, l.db, w.TaskID)
		}
		wr, err := l.terminalWrite(ctx, l.db, row, w)
		if err != nil {
			return 0, err
		}
		a, err := l.Append(ctx, nil, wr)
		if err == nil {
			return a.Seq, nil
		}
		if !errors.Is(err, taskstore.ErrConcurrentModification) {
			return 0, err
		}
		last = err
	}
	return 0, last
}

// terminalWrite is the terminal event w describes for row, in the row's protocol: an A2A status event, or an MCP
// task's status row with its calls row (spec §11.8 cases 2 and 3, mcpTerminal).
func (l *Log) terminalWrite(ctx context.Context, q querier, row *TaskRow, w TerminalWrite) (Write, error) {
	if row.Protocol == ProtocolMCP {
		return l.mcpTerminal(ctx, q, row, w)
	}
	ev, err := w.event(row.Snapshot)
	if err != nil {
		return Write{}, err
	}
	EndReconnect(row.Snapshot, ev)
	next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
	if err != nil {
		return Write{}, fmt.Errorf("store: terminal write to task %s: %w", w.TaskID, err)
	}
	payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
	if err != nil {
		return Write{}, err
	}
	return Write{
		TaskID: w.TaskID, PrevVersion: row.Version, Kind: KindStatus, State: w.State,
		StatusAt: StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload,
	}, nil
}

// ApplyMetadata logs one metadata event merging patch into the task's metadata at the top level (an
// operator key replaces a same-named key, spec §2.5), allowed after a terminal event. It applies at the
// task's current version, retrying on a concurrent write. An empty patch writes nothing and returns the
// current version; a patch over MaxPatchBytes is a2a.ErrInvalidParams.
//
// The version bump is the caller's to time: a2a-go's processor writes at the version it wrote last, so a
// patch applied under a running execution makes its next write ErrConcurrentModification and aborts it.
// Spec §13.3 "Ordering" already rules it out: patches and annotations that arrive while a dispatch, abort
// or forward_cancel is in flight are held in workflow state and applied when it returns.
func (l *Log) ApplyMetadata(ctx context.Context, taskID uuid.UUID, patch map[string]any) (int64, error) {
	return l.ApplyPatch(ctx, taskID, patch, nil)
}

// ApplyPatch is ApplyMetadata with the patch's mcpMeta too (spec §13.3): an MCP task keeps it in its projection
// (MetaMCPMeta), merged key by key, and renders it into the _meta of tasks/get and tasks/result; an A2A task keeps
// none, and a patch of mcpMeta alone writes nothing to it. Both maps together are at most MaxPatchBytes.
func (l *Log) ApplyPatch(ctx context.Context, taskID uuid.UUID, patch, mcpMeta map[string]any) (int64, error) {
	if patch == nil {
		patch = map[string]any{}
	}
	payload, err := json.Marshal(metadataRow{Metadata: patch, MCPMeta: mcpMeta})
	if err != nil {
		return 0, fmt.Errorf("store: encode metadata patch: %w", err)
	}
	if len(payload) > MaxPatchBytes {
		return 0, fmt.Errorf("store: a metadata patch of %d bytes is over %d: %w", len(payload), MaxPatchBytes, a2a.ErrInvalidParams)
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var last error
	for range conflictRetries {
		row, err := l.Load(ctx, taskID)
		if err != nil {
			return 0, err
		}
		mm := mcpMeta
		if row.Protocol != ProtocolMCP {
			mm = nil
		}
		if len(patch) == 0 && len(mm) == 0 {
			return row.Version, nil
		}
		p := payload
		if len(mm) == 0 && len(mcpMeta) > 0 {
			if p, err = json.Marshal(metadataRow{Metadata: patch}); err != nil {
				return 0, err
			}
		}
		a, err := l.Append(ctx, nil, Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: KindMetadata,
			Snapshot: mergeMCPMeta(mergeMetadata(row.Snapshot, patch), mm), Payload: p, AfterTerminalOK: true,
		})
		if err == nil {
			return a.Seq, nil
		}
		if !errors.Is(err, taskstore.ErrConcurrentModification) {
			return 0, err
		}
		last = err
	}
	return 0, last
}
