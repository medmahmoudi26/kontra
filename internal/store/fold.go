package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/google/uuid"
)

// Folder folds a task's rows, in seq order, into the task as it was after the last one (spec §9, D75):
//
//   - the first row is the `task` row, the creation snapshot;
//   - up to the terminal row, task, status, artifact and message rows go through a2a-go's update rules
//     (a2aevent.ApplyShallowUpdate, the default materializer's, so the fold equals the projection a2a-go
//     wrote);
//   - after it, message rows (accepted review-window messages) are appended to history directly, because
//     ApplyUpdate refuses any update to a terminal task; any other carried row there is a corrupt log;
//   - metadata rows are always a top-level merge of their patch into the task's metadata.
//
// The writers of this package build every projection with the same functions, so the fold of rows 1..K
// equals the projection after the K-th write. A Folder owns what it decodes; Snapshot returns a copy.
type Folder struct {
	task *a2a.Task
	seq  int64
}

// Seq is the seq of the last row folded, 0 before the first.
func (f *Folder) Seq() int64 { return f.seq }

// Terminal reports whether the folded task is terminal.
func (f *Folder) Terminal() bool { return f.task != nil && f.task.Status.State.Terminal() }

// Snapshot returns a deep copy of the folded task, nil before the first row.
func (f *Folder) Snapshot() (*a2a.Task, error) {
	if f.task == nil {
		return nil, nil
	}
	b, err := json.Marshal(f.task)
	if err != nil {
		return nil, err
	}
	return decodeTask(b)
}

// Apply folds the next row. Rows must come in seq order without a gap (seq = version, so a task's log
// has none); a row whose payload is only in the bucket cannot be folded (no bucket in M1, spec §10.4).
func (f *Folder) Apply(ev Event) error {
	if ev.Seq != f.seq+1 {
		return fmt.Errorf("store: fold of task %s: seq %d after %d", ev.TaskID, ev.Seq, f.seq)
	}
	if len(ev.Payload) == 0 {
		return fmt.Errorf("store: fold of task %s: row %d has no inline payload", ev.TaskID, ev.Seq)
	}
	next, err := foldRow(f.task, ev)
	if err != nil {
		return fmt.Errorf("store: fold of task %s at seq %d (%s): %w", ev.TaskID, ev.Seq, ev.Kind, err)
	}
	f.task, f.seq = next, ev.Seq
	return nil
}

func foldRow(task *a2a.Task, ev Event) (*a2a.Task, error) {
	if ev.Kind == KindMetadata {
		if task == nil {
			return nil, errors.New("a metadata row before the task row")
		}
		patch, err := decodeMetadataRow(ev.Payload)
		if err != nil {
			return nil, err
		}
		return mergeMetadata(task, patch), nil
	}
	if !ev.Kind.Carried() && ev.Kind != KindMessage {
		return nil, fmt.Errorf("%s rows are MCP's, not an A2A task's", ev.Kind)
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(ev.Payload, &sr); err != nil {
		return nil, err
	}
	if task == nil {
		t, ok := sr.Event.(*a2a.Task)
		if ev.Kind != KindTask || !ok {
			return nil, errors.New("the first row is not the task row")
		}
		return t, nil
	}
	if task.Status.State.Terminal() {
		msg, ok := sr.Event.(*a2a.Message)
		if ev.Kind != KindMessage || !ok {
			return nil, errors.New("only message and metadata rows follow the terminal row")
		}
		return appendHistory(task, msg), nil
	}
	return a2aevent.ApplyShallowUpdate(task, sr.Event)
}

// mergeMetadata returns task with patch merged at the top level of its metadata: an operator key
// replaces a same-named key, never merged with it (spec §2.5). The input is not modified.
func mergeMetadata(task *a2a.Task, patch map[string]any) *a2a.Task {
	next := *task
	next.Metadata = maps.Clone(task.Metadata)
	if next.Metadata == nil {
		next.Metadata = make(map[string]any, len(patch))
	}
	maps.Copy(next.Metadata, patch)
	return &next
}

// appendHistory returns task with msg appended to its history. The input is not modified.
func appendHistory(task *a2a.Task, msg *a2a.Message) *a2a.Task {
	next := *task
	next.History = append(slices.Clone(task.History), msg)
	return &next
}

// metadataRow is the payload of a metadata row.
type metadataRow struct {
	Metadata map[string]any `json:"metadata"`
}

func decodeMetadataRow(b []byte) (map[string]any, error) {
	var r metadataRow
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Metadata == nil {
		return nil, errors.New(`a metadata row without "metadata"`)
	}
	return r.Metadata, nil
}

// SnapshotAt folds rows 1..seq into the task as it was after seq (spec §9; see Folder). It is an error
// wrapping a2a.ErrInvalidParams when seq is not a seq of this task, and a2a.ErrTaskNotFound when the task
// does not exist.
func (l *Log) SnapshotAt(ctx context.Context, taskID uuid.UUID, seq int64) (*a2a.Task, error) {
	tail, err := l.Tail(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if tail == 0 {
		return nil, notFound()
	}
	if seq < 1 || seq > tail {
		return nil, fmt.Errorf("store: %d is not a seq of this task: %w", seq, a2a.ErrInvalidParams)
	}
	var f Folder
	for f.Seq() < seq {
		evs, err := l.Events(ctx, taskID, f.Seq(), int(min(seq-f.Seq(), 1000)))
		if err != nil {
			return nil, err
		}
		if len(evs) == 0 {
			return nil, fmt.Errorf("store: task %s: rows after %d are gone", taskID, f.Seq())
		}
		for _, ev := range evs {
			if err := f.Apply(ev); err != nil {
				return nil, err
			}
		}
	}
	return f.Snapshot()
}
