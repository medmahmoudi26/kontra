package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// markDeliveredSQL sets a follow-up's delivered_at under a dispatch's fence: only while attempt n is the
// task's latest, open, and holds the dispatch id (the same check, and the same FOR SHARE, as appendSQL's).
const markDeliveredSQL = `
update kontra.work_items w set delivered_at = now()
 where w.id = $1 and w.task_id = $2 and w.delivered_at is null
   and exists (
	select 1 from kontra.attempts a
	 where a.task_id = w.task_id and a.n = $3 and a.ended_at is null and a.dispatch_id = $4
	   and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)
	   for share of a)`

// markDeliveredClassifySQL tells why markDeliveredSQL wrote nothing.
const markDeliveredClassifySQL = `
select w.delivered_at is not null,
       exists (select 1 from kontra.attempts a
                where a.task_id = w.task_id and a.n = $3 and a.ended_at is null and a.dispatch_id = $4
                  and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n))
  from kontra.work_items w where w.id = $1 and w.task_id = $2`

// MarkDelivered sets the delivered_at of the follow-up work item workItemID under the dispatch's fence f,
// for an upstream answer to the follow-up from which the dispatch logs nothing (a task exactly as the
// projection already shows it): the delivery is then written on its own, before anything else is logged
// (spec §6.4, D81). An answer that logs an event records the delivery in that event's append instead
// (Fence.ArmDelivery).
//
// An item that already has its delivered_at keeps it, and is not an error. A fence that no longer holds is
// ErrFenced (and the fence records it); an item that is missing or another task's is ErrWorkItemNotFound.
func (l *Log) MarkDelivered(ctx context.Context, f *Fence, workItemID uuid.UUID) error {
	if f == nil {
		return errors.New("store: a follow-up's delivery is marked by its dispatch, under its fence")
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	tag, err := l.writeExec(ctx, markDeliveredSQL, workItemID, f.TaskID, f.N, f.DispatchID)
	if err != nil {
		return fmt.Errorf("store: delivery of work item %s: %w", workItemID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var delivered, holds bool
	err = l.db.QueryRow(ctx, markDeliveredClassifySQL, workItemID, f.TaskID, f.N, f.DispatchID).Scan(&delivered, &holds)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("store: work item %s: %w", workItemID, ErrWorkItemNotFound)
	case err != nil:
		return fmt.Errorf("store: delivery of work item %s: %w", workItemID, err)
	case !holds:
		f.markFenced()
		return fmt.Errorf("store: task %s attempt %d: %w", f.TaskID, f.N, ErrFenced)
	}
	return nil // delivered already
}

// TurnStatusLogged reports whether the task's log holds a status (or task) row after version since: given a
// follow-up's WorkItem.TaskVersion, whether the turn that follow-up opened has logged a status of its own.
// Every row of the turn is logged after the version its work item was stored at (spec §9), and between the
// two only metadata rows can be (§13.3: no turn runs then), so this needs neither the turn's `message` row
// (a2a-go appends none for a messageId already in history) nor any read inside the payloads (D49). A
// resume of a follow-up whose delivery is recorded reads it to tell the turn's own end at input-required
// (after since) from the previous turn's end that the upstream still shows while it has not processed the
// follow-up (at or before since), which is no end of this turn (spec §6.4, §7, D81). An item stored without
// its version (since 0) counts as logged: the projection's state then ends the turn, as before D81.
func (l *Log) TurnStatusLogged(ctx context.Context, taskID uuid.UUID, since int64) (bool, error) {
	if since <= 0 {
		return true, nil
	}
	var logged bool
	err := l.db.QueryRow(ctx, `select exists (select 1 from kontra.task_events
		where task_id = $1 and seq > $2 and kind in ('status', 'task'))`, taskID, since).Scan(&logged)
	if err != nil {
		return false, fmt.Errorf("store: turn after version %d of task %s: %w", since, taskID, err)
	}
	return logged, nil
}
