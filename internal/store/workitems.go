package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
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
type WorkItem struct {
	ID         uuid.UUID
	TaskID     uuid.UUID
	Kind       WorkItemKind
	Payload    WorkPayload
	CreatedAt  time.Time
	ConsumedAt *time.Time
}

// WorkPayload is the sanitized copy of a2a-go's workqueue.Payload: the request, and the call context with
// every service parameter removed except A2A-Version and A2A-Extensions (no Authorization, no cookies).
type WorkPayload struct {
	Request     *a2a.SendMessageRequest `json:"request"`
	CallContext map[string]any          `json:"callContext"`
	Owner       Owner                   `json:"owner"`
}

// InsertWorkItem stores wi in tx (the same transaction that pre-creates a new task).
func (l *Log) InsertWorkItem(ctx context.Context, tx pgx.Tx, wi WorkItem) error {
	return unimpl.Error("store.Log.InsertWorkItem", "WP-02")
}

// LoadWorkItem returns the work item if it belongs to taskID.
func (l *Log) LoadWorkItem(ctx context.Context, id, taskID uuid.UUID) (*WorkItem, error) {
	return nil, unimpl.Error("store.Log.LoadWorkItem", "WP-02")
}

// ConsumeWorkItem marks the item consumed if it belongs to taskID and is unconsumed; otherwise an error.
func (l *Log) ConsumeWorkItem(ctx context.Context, id, taskID uuid.UUID) error {
	return unimpl.Error("store.Log.ConsumeWorkItem", "WP-02")
}

// MarshalPayload encodes the payload for work_items.payload (json, not jsonb, D49).
func MarshalPayload(p WorkPayload) (json.RawMessage, error) {
	return nil, unimpl.Error("store.MarshalPayload", "WP-02")
}
