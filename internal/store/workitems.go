package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// WorkItemKind is work_items.kind.
type WorkItemKind string

// Work item kinds.
const (
	WorkExecute WorkItemKind = "execute"
	WorkCancel  WorkItemKind = "cancel"
)

// WorkItem is the work queue's payload by reference (spec §8.1, D6): it is never put into Temporal; the
// workflow and the dispatch activity carry only its id.
//
// Ownership and single use are checked where the item is used, never in an Update validator (D66): the
// first dispatch of a turn loads it by (id, task) and consumes it (ConsumeWorkItem); a later dispatch of
// the same turn (a retry, attempt n + 1, a resume) may reload the consumed item it owns (LoadWorkItem);
// kontra.review_message consumes it in its append transaction (AppendReviewMessage). A refused follow-up
// deletes its item (DeleteWorkItem); an unconsumed item otherwise stays until its task is deleted.
//
// DeliveredAt is a follow-up's delivery (spec §6.4, D81): set by the turn's dispatch when the upstream
// first answered the follow-up, in the transaction of the first append from that answer (Write.Deliver,
// Fence.ArmDelivery), or on its own when the answer logged nothing (MarkDelivered). A resume of the turn
// sends the follow-up again only while it is nil.
type WorkItem struct {
	ID          uuid.UUID
	TaskID      uuid.UUID
	Kind        WorkItemKind
	Payload     WorkPayload
	CreatedAt   time.Time
	ConsumedAt  *time.Time
	DeliveredAt *time.Time
}

// WorkPayload is the sanitized copy of a2a-go's workqueue.Payload: the request, and the call context with
// every service parameter removed except A2A-Version and A2A-Extensions (no Authorization, no cookies).
type WorkPayload struct {
	Request     *a2a.SendMessageRequest `json:"request"`
	CallContext map[string]any          `json:"callContext"`
	Owner       Owner                   `json:"owner"`
}

// MarshalPayload encodes the payload for work_items.payload (json, not jsonb, D49).
func MarshalPayload(p WorkPayload) (json.RawMessage, error) {
	if p.Request == nil || p.Request.Message == nil {
		return nil, errors.New("store: a work item needs the request and its message")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("store: encode work item payload: %w", err)
	}
	return b, nil
}

// UnmarshalPayload decodes work_items.payload.
func UnmarshalPayload(b []byte) (WorkPayload, error) {
	var p WorkPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return WorkPayload{}, fmt.Errorf("store: decode work item payload: %w", err)
	}
	return p, nil
}

// InsertWorkItem stores wi in tx (the same transaction that pre-creates a new task), or on its own when tx
// is nil. CreatedAt and ConsumedAt are the table's; a task that does not exist is a2a.ErrTaskNotFound.
func (l *Log) InsertWorkItem(ctx context.Context, tx pgx.Tx, wi WorkItem) error {
	if wi.ID == uuid.Nil || wi.TaskID == uuid.Nil {
		return errors.New("store: a work item needs its id and task")
	}
	if wi.Kind != WorkExecute && wi.Kind != WorkCancel {
		return fmt.Errorf("store: work item kind %q", wi.Kind)
	}
	payload, err := MarshalPayload(wi.Payload)
	if err != nil {
		return err
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	const insert = `insert into kontra.work_items (id, task_id, kind, payload) values ($1, $2, $3, $4::json)`
	args := []any{wi.ID, wi.TaskID, string(wi.Kind), []byte(payload)}
	if tx != nil {
		_, err = tx.Exec(ctx, insert, args...)
	} else {
		_, err = l.writeExec(ctx, insert, args...)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return notFound()
	}
	if err != nil {
		return fmt.Errorf("store: insert work item %s: %w", wi.ID, err)
	}
	return nil
}

// LoadWorkItem returns the work item if it belongs to taskID, consumed or not, with its delivery;
// otherwise ErrWorkItemNotFound.
func (l *Log) LoadWorkItem(ctx context.Context, id, taskID uuid.UUID) (*WorkItem, error) {
	wi := WorkItem{ID: id, TaskID: taskID}
	var payload []byte
	err := l.db.QueryRow(ctx, `select kind, payload, created_at, consumed_at, delivered_at from kontra.work_items
		where id = $1 and task_id = $2`, id, taskID).Scan(&wi.Kind, &payload, &wi.CreatedAt, &wi.ConsumedAt, &wi.DeliveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: work item %s: %w", id, ErrWorkItemNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: load work item %s: %w", id, err)
	}
	if wi.Payload, err = UnmarshalPayload(payload); err != nil {
		return nil, err
	}
	return &wi, nil
}

// ConsumeWorkItem marks the item consumed if it belongs to taskID and is unconsumed. Otherwise it is
// ErrWorkItemNotFound (missing, or another task's) or ErrWorkItemConsumed, and nothing changes.
func (l *Log) ConsumeWorkItem(ctx context.Context, id, taskID uuid.UUID) error {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	return l.inTx(ctx, func(tx pgx.Tx) error {
		_, err := consumeWorkItem(ctx, tx, id, taskID)
		return err
	})
}

// consumeWorkItem consumes (id, taskID) in tx and returns its payload.
func consumeWorkItem(ctx context.Context, tx pgx.Tx, id, taskID uuid.UUID) (WorkPayload, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `update kontra.work_items set consumed_at = now()
		where id = $1 and task_id = $2 and consumed_at is null returning payload`, id, taskID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		var consumed bool
		err = tx.QueryRow(ctx, `select consumed_at is not null from kontra.work_items where id = $1 and task_id = $2`,
			id, taskID).Scan(&consumed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return WorkPayload{}, fmt.Errorf("store: work item %s: %w", id, ErrWorkItemNotFound)
		case err != nil:
			return WorkPayload{}, fmt.Errorf("store: consume work item %s: %w", id, err)
		default:
			return WorkPayload{}, fmt.Errorf("store: work item %s: %w", id, ErrWorkItemConsumed)
		}
	}
	if err != nil {
		return WorkPayload{}, fmt.Errorf("store: consume work item %s: %w", id, err)
	}
	return UnmarshalPayload(payload)
}

// DeleteWorkItem deletes the item if it belongs to taskID: a refused follow-up's content is not kept
// (spec §7, §14.1). Deleting an item that is not there is not an error.
func (l *Log) DeleteWorkItem(ctx context.Context, id, taskID uuid.UUID) error {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	_, err := l.writeExec(ctx, `delete from kontra.work_items where id = $1 and task_id = $2`, id, taskID)
	if err != nil {
		return fmt.Errorf("store: delete work item %s: %w", id, err)
	}
	return nil
}

// FirstWorkItemID returns the task's first execute work item, the one its workflow was started with: a
// resend re-issues the start with it as the Update id (spec §14.5). ErrWorkItemNotFound when there is none.
func (l *Log) FirstWorkItemID(ctx context.Context, taskID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := l.db.QueryRow(ctx, `select id from kontra.work_items where task_id = $1 and kind = 'execute'
		order by created_at, id limit 1`, taskID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("store: task %s: %w", taskID, ErrWorkItemNotFound)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: first work item of task %s: %w", taskID, err)
	}
	return id, nil
}
