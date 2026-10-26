package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/blob"
)

// DropUnnamedBlobs settles the objects one request stored for a task's message (spec §10.4, §5.4, D167): of refs,
// it returns those that no record of the task names (its creation row, its message rows, its work items) and
// deletes their blobs rows in the same statement; the caller then deletes their objects. Those are what a request
// the gate refused, or answered with another task, stored and nothing will ever read: a new task's whose row was
// never written (a refusal after the parts were stored, a work queue that failed, the later of two identical
// requests, which the work queue answers with the earlier one's task), a follow-up's or a review-window message's
// whose work item was deleted. A task that does not exist names none of them. The refs are fresh, minted for this
// request: only its own write can name them, and it has returned when this runs.
func (l *Log) DropUnnamedBlobs(ctx context.Context, task uuid.UUID, refs []blob.Ref) ([]blob.Ref, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	want := make([]string, 0, len(refs))
	for _, r := range refs {
		want = append(want, string(r))
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var out []blob.Ref
	err := l.inTx(ctx, func(tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `with x as (
				select r.ref from unnest($2::text[]) as r(ref)
				 where not exists (select 1 from kontra.task_events e
				                    where e.task_id = $1 and e.kind in ('task', 'message')
				                      and strpos(e.payload::text, 'kontra-blob:' || r.ref) > 0)
				   and not exists (select 1 from kontra.work_items w
				                    where w.task_id = $1 and strpos(w.payload::text, 'kontra-blob:' || r.ref) > 0)
			), d as (
				delete from kontra.blobs b using x where b.ref = x.ref and b.task_id = $1
			)
			select ref from x`, task, want)
		if err != nil {
			return err
		}
		refs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, r := range refs {
			out = append(out, blob.Ref(r))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: drop the unnamed blobs rows of task %s: %w", task, err)
	}
	return out, nil
}

// BlobBytes is the size of every object the task's blobs rows name: what the bucket holds for it, which
// limits.task_bytes bounds with its logged events (spec §6.6, §10.4).
func (l *Log) BlobBytes(ctx context.Context, task uuid.UUID) (int64, error) {
	var n int64
	if err := l.db.QueryRow(ctx, `select coalesce(sum(size), 0)::bigint from kontra.blobs where task_id = $1`, task).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: the bucket bytes of task %s: %w", task, err)
	}
	return n, nil
}
