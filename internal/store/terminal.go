package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Metadata keys the gate writes on a task it ends (spec §6.4, §13.2).
const (
	MetaFailure  = "kontra.failure"
	MetaCanceled = "kontra.canceled"
)

// Values of metadata["kontra.canceled"] (spec §8, §13.2).
const (
	CanceledClient       = "client"
	CanceledDeclined     = "declined"
	CanceledQuoteExpired = "quote_expired"
)

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
	CanceledClient:       "the task was canceled",
	CanceledDeclined:     "the quote was declined",
	CanceledQuoteExpired: "the quote expired",
}

// ReasonText is the default text of a failed status for reason.
func ReasonText(reason FailureReason) string { return reasonText[reason] }

// TerminalStatus builds a terminal status event the gate writes: an agent-role status message with one
// text part, and meta on both the message (spec ext-sealed.md §4a: the gate's terminal message carries
// kontra.failure) and the event, which merges it into the task's metadata. The relay and write_terminal
// both build their terminal events here, so there is one shape (spec §6.4).
func TerminalStatus(info a2a.TaskInfoProvider, state a2a.TaskState, meta map[string]any, text string) *a2a.TaskStatusUpdateEvent {
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
	ev := a2a.NewStatusUpdateEvent(info, state, msg)
	if len(meta) > 0 {
		msg.Metadata = maps.Clone(meta)
		ev.Metadata = maps.Clone(meta)
	}
	return ev
}

// FailedStatus is `failed: <reason>` (spec §6.4): metadata["kontra.failure"] = reason and a text saying
// what happened (ReasonText when text is empty).
func FailedStatus(info a2a.TaskInfoProvider, reason FailureReason, text string) *a2a.TaskStatusUpdateEvent {
	if text == "" {
		text = ReasonText(reason)
	}
	return TerminalStatus(info, a2a.TaskStateFailed, map[string]any{MetaFailure: string(reason)}, text)
}

// CanceledStatus is `canceled` with metadata["kontra.canceled"] = canceled (client, declined,
// quote_expired; none when empty).
func CanceledStatus(info a2a.TaskInfoProvider, canceled, text string) *a2a.TaskStatusUpdateEvent {
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
	Canceled string        // for canceled: metadata["kontra.canceled"] = client | declined | quote_expired
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
		ev, err := w.event(row.Snapshot)
		if err != nil {
			return 0, err
		}
		next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
		if err != nil {
			return 0, fmt.Errorf("store: terminal write to task %s: %w", w.TaskID, err)
		}
		payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
		if err != nil {
			return 0, err
		}
		a, err := l.Append(ctx, nil, Write{
			TaskID: w.TaskID, PrevVersion: row.Version, Kind: KindStatus, State: w.State,
			StatusAt: StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload,
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

// ApplyMetadata logs one metadata event merging patch into the task's metadata at the top level (an
// operator key replaces a same-named key, spec §2.5), allowed after a terminal event. It applies at the
// task's current version, retrying on a concurrent write. An empty patch writes nothing and returns the
// current version; a patch over MaxPatchBytes is a2a.ErrInvalidParams.
//
// SPEC-CONFLICT: spec §5.2 makes a stale version a conflict, never a merge, and §13.3/§14.1 let operator
// patches (OnDispatched's result, kontra.annotate) land while a dispatch runs. a2a-go's processor writes at
// the version it wrote last, so the relay's next event after such a patch is ErrConcurrentModification;
// a2a-go reloads, finds the task not terminal, and stops the execution as a "parallel active execution".
// The store keeps the rule as written; the lifecycle must apply metadata patches while no dispatch runs
// (between turns, or after the terminal event), not concurrently with one.
func (l *Log) ApplyMetadata(ctx context.Context, taskID uuid.UUID, patch map[string]any) (int64, error) {
	payload, err := json.Marshal(metadataRow{Metadata: patch})
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
		if len(patch) == 0 {
			return row.Version, nil
		}
		a, err := l.Append(ctx, nil, Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: KindMetadata,
			Snapshot: mergeMetadata(row.Snapshot, patch), Payload: payload, AfterTerminalOK: true,
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

// AppendReviewMessage is the accepted review-window message of kontra.review_message (spec §7, §14.1,
// D66): in one transaction it consumes the work item (id, taskID), and appends the caller's message from
// it to the task's history as a `message` row, the after-terminal write the terminal rule allows. A work
// item that is missing, another task's or already consumed is ErrWorkItemNotFound or ErrWorkItemConsumed
// and nothing is written; the handler answers accepted: false.
func (l *Log) AppendReviewMessage(ctx context.Context, taskID, workItemID uuid.UUID) (int64, error) {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var seq int64
	err := l.inTx(ctx, func(tx pgx.Tx) error {
		wp, err := consumeWorkItem(ctx, tx, workItemID, taskID)
		if err != nil {
			return err
		}
		if wp.Request == nil || wp.Request.Message == nil || wp.Request.Message.Role != a2a.MessageRoleUser {
			return fmt.Errorf("store: work item %s holds no caller message", workItemID)
		}
		msg := wp.Request.Message
		row, err := l.load(ctx, tx, taskID, " for update")
		if err != nil {
			return err
		}
		var next *a2a.Task
		if row.State.Terminal() {
			next = appendHistory(row.Snapshot, msg)
		} else if next, err = a2aevent.ApplyShallowUpdate(row.Snapshot, msg); err != nil {
			return fmt.Errorf("store: review message for task %s: %w", taskID, err)
		}
		payload, err := json.Marshal(a2a.StreamResponse{Event: msg})
		if err != nil {
			return err
		}
		a, err := l.Append(ctx, tx, Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: KindMessage,
			Snapshot: next, Payload: payload, AfterTerminalOK: true,
		})
		seq = a.Seq
		return err
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}
