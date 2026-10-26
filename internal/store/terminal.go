package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

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
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
	ev := a2a.NewStatusUpdateEvent(info, state, msg)
	if len(meta) > 0 {
		msg.Metadata = maps.Clone(meta)
		ev.Metadata = maps.Clone(meta)
	}
	return ev
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

// CanceledStatus is `canceled` with metadata["kontra.canceled"] = canceled (client, declined,
// quote_expired; none when empty, or when canceled is outside that closed set).
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
// The version bump is the caller's to time: a2a-go's processor writes at the version it wrote last, so a
// patch applied under a running execution makes its next write ErrConcurrentModification and aborts it.
// Spec §13.3 "Ordering" already rules it out: patches and annotations that arrive while a dispatch, abort
// or forward_cancel is in flight are held in workflow state and applied when it returns.
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
// it to the task's history as a `message` row, the after-terminal write the terminal rule allows. It
// returns the row's seq.
//
// It is idempotent for its own item, as a retried activity needs: when an earlier call committed and its
// answer was lost (a crash or a timeout after COMMIT), the item is consumed and its message is in the log,
// and the call returns that row's seq again, so the handler still applies the hook's patch and dispute
// flag (D66: they apply when the append commits). A work item that is missing or another task's is
// ErrWorkItemNotFound; one consumed without a message of its own (by a dispatch) is ErrWorkItemConsumed;
// either way nothing is written, and the handler answers accepted: false.
func (l *Log) AppendReviewMessage(ctx context.Context, taskID, workItemID uuid.UUID) (int64, error) {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var seq int64
	err := l.inTx(ctx, func(tx pgx.Tx) error {
		wp, err := consumeWorkItem(ctx, tx, workItemID, taskID)
		if errors.Is(err, ErrWorkItemConsumed) {
			s, ok, ferr := reviewMessageSeq(ctx, tx, workItemID, taskID)
			if ferr != nil {
				return ferr
			}
			if ok {
				seq = s
				return nil
			}
		}
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

// reviewMessageSeq finds the message row an AppendReviewMessage of the consumed item (id, taskID)
// committed. The item is consumed in the transaction that appends its message, so that row's created_at is
// the item's consumed_at (both are the transaction's now()), and the row carries the item's message. ok is
// false when there is no such row: something else consumed the item.
func reviewMessageSeq(ctx context.Context, tx pgx.Tx, id, taskID uuid.UUID) (seq int64, ok bool, err error) {
	var (
		raw        []byte
		consumedAt time.Time
	)
	err = tx.QueryRow(ctx, `select payload, consumed_at from kontra.work_items
		where id = $1 and task_id = $2 and consumed_at is not null`, id, taskID).Scan(&raw, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: work item %s: %w", id, err)
	}
	wp, err := UnmarshalPayload(raw)
	if err != nil {
		return 0, false, err
	}
	if wp.Request == nil || wp.Request.Message == nil {
		return 0, false, nil
	}
	rows, err := tx.Query(ctx, `select seq, payload from kontra.task_events
		where task_id = $1 and kind = 'message' and created_at = $2 order by seq`, taskID, consumedAt)
	if err != nil {
		return 0, false, fmt.Errorf("store: review message of work item %s: %w", id, err)
	}
	type row struct {
		seq     int64
		payload []byte
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.seq, &x.payload)
		return x, err
	})
	if err != nil {
		return 0, false, fmt.Errorf("store: review message of work item %s: %w", id, err)
	}
	for _, f := range found {
		var logged struct {
			Message struct {
				ID string `json:"messageId"`
			} `json:"message"`
		}
		if json.Unmarshal(f.payload, &logged) == nil && logged.Message.ID == wp.Request.Message.ID {
			return f.seq, true, nil
		}
	}
	return 0, false, nil
}
