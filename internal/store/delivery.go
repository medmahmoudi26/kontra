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

// TurnStatusLogged reports whether the task's log holds a status (or task) row after the latest `message`
// row that appended messageID to the task's history: whether the follow-up turn that message opened has
// logged a status of its own. A resume of a follow-up whose delivery is recorded reads it to tell the
// turn's own end at input-required (logged after the message) from the previous turn's end that the
// upstream still shows while it has not processed the follow-up (logged before it), which is no end of
// this turn (spec §6.4, §7, D81). False when no message row carries messageID.
func (l *Log) TurnStatusLogged(ctx context.Context, taskID uuid.UUID, messageID string) (bool, error) {
	if messageID == "" || !storableText(messageID) {
		return false, nil
	}
	var logged bool
	err := l.db.QueryRow(ctx, `select exists (
			select 1 from kontra.task_events s
			 where s.task_id = m.task_id and s.seq > m.seq and s.kind in ('status', 'task'))
		  from kontra.task_events m
		 where m.task_id = $1 and m.kind = 'message' and m.payload -> 'message' ->> 'messageId' = $2
		 order by m.seq desc limit 1`, taskID, messageID).Scan(&logged)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: turn of message %q in task %s: %w", messageID, taskID, err)
	}
	return logged, nil
}
