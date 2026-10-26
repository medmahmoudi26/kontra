package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// fakeSaver is internal/push's inline save for these tests: one push_configs row per (task, URL).
type fakeSaver struct{ db *pgxpool.Pool }

func (f fakeSaver) SaveInline(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, _ store.Owner, cfg *a2a.PushConfig) (string, bool, error) {
	id := "inline:" + cfg.URL
	tag, err := tx.Exec(ctx, `insert into kontra.push_configs (task_id, id, config) values ($1, $2, '{}') on conflict do nothing`, taskID, id)
	if err != nil {
		return "", false, err
	}
	if tag.RowsAffected() != 1 {
		return "", false, nil // an update: nothing to announce
	}
	return id, true, nil
}

func (f fakeSaver) DeleteInline(ctx context.Context, taskID uuid.UUID, configID string) error {
	_, err := f.db.Exec(ctx, `delete from kontra.push_configs where task_id = $1 and id = $2`, taskID, configID)
	return err
}

func followUpOf(taskID, msgID, text string) *a2a.SendMessageRequest {
	req := requestOf(msgID, text)
	req.Message.TaskID = a2a.TaskID(taskID)
	return req
}

// TestQueueRacesOverTemporal is the work queue's races against Postgres and a development server, with the
// real lifecycle activities and workflow and a fake dispatch: a concurrent second follow-up, a metadata row
// while the workflow catches up with a turn end, a refused follow-up's inline push config, and two
// identical new-task requests arriving together (spec §7, §10.1, §13.3, §15).
func TestQueueRacesOverTemporal(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &fakeDispatch{t: t, log: e.log, script: map[string][]turn{}, calls: map[string][]DispatchInput{}}
	w, err := NewWorkers(c, WorkerConfig{ReplicaID: "it-replica", DrainTimeout: time.Second}, Activities{Lifecycle: e.acts, Dispatch: d})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	q := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Push: fakeSaver{db: e.pool}, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}})
	ctx := context.Background()
	follower, _ := caller(false)

	// A second follow-up sent while the first one's turn is dispatched but has logged nothing yet is
	// refused at once with UnsupportedOperationError (spec §7), not left to time out as a 503.
	t.Run("concurrent second follow-up", func(t *testing.T) {
		cctx, pre := caller(true)
		holdA := make(chan struct{})
		defer func() {
			select {
			case <-holdA:
			default:
				close(holdA)
			}
		}()
		d.set(pre,
			turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted},
			turn{hold: holdA, state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
		if _, err := q.Write(cctx, payloadOf(requestOf("x-1", "task x"))); err != nil {
			t.Fatal(err)
		}
		eventually(t, "task x at input-required", func() bool { return e.state(pre) == store.StateInputRequired })
		if _, err := q.Write(follower, payloadOf(followUpOf(pre, "x-2", "the first answer"))); err != nil {
			t.Fatalf("the first follow-up: %v", err)
		}
		eventually(t, "the first follow-up's dispatch running", func() bool { return d.count(pre) == 2 })
		began := time.Now()
		_, err := q.Write(follower, payloadOf(followUpOf(pre, "x-3", "a second answer")))
		took := time.Since(began)
		if !errors.Is(err, a2a.ErrUnsupportedOperation) || !errors.Is(err, ErrRefused) || errors.Is(err, ErrUnavailable) {
			t.Fatalf("a concurrent second follow-up after %v: %v, want UnsupportedOperationError", took, err)
		}
		if took > 3*time.Second {
			t.Errorf("the refusal took %v: it is synchronous", took)
		}
		if n := e.count(`select count(*) from kontra.work_items where task_id = $1 and payload::text like '%a second answer%'`, pre); n != 0 {
			t.Errorf("the refused follow-up's work item was kept")
		}
		close(holdA)
		eventually(t, "task x completed", func() bool { return e.state(pre) == store.StateCompleted })
	})

	// A metadata row logged after the turn end the follow-up answers (an operator patch the workflow
	// applies once the dispatch returns) moves tasks.version, not the turn end: the follow-up waiting for
	// the workflow to catch up is still accepted (spec §7, §13.3).
	t.Run("metadata row while the workflow catches up", func(t *testing.T) {
		cctx, pre := caller(true)
		hold := make(chan struct{})
		defer func() {
			select {
			case <-hold:
			default:
				close(hold)
			}
		}()
		d.set(pre,
			turn{state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted, holdAfter: hold},
			turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
		if _, err := q.Write(cctx, payloadOf(requestOf("y-1", "task y"))); err != nil {
			t.Fatal(err)
		}
		// The dispatch has logged input-required and not returned yet: the workflow still counts the turn
		// as running.
		eventually(t, "task y at input-required", func() bool { return e.state(pre) == store.StateInputRequired })
		res := make(chan error, 1)
		go func() {
			_, err := q.Write(follower, payloadOf(followUpOf(pre, "y-2", "an answer")))
			res <- err
		}()
		eventually(t, "the follow-up's work item", func() bool {
			return e.count(`select count(*) from kontra.work_items where task_id = $1`, pre) == 2
		})
		time.Sleep(300 * time.Millisecond)
		if _, err := e.log.ApplyMetadata(ctx, uuid.MustParse(pre), map[string]any{"kontra.escrow.note": "patched"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		close(hold)
		select {
		case err := <-res:
			if err != nil {
				t.Fatalf("the follow-up answering the turn end: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the follow-up did not return")
		}
		eventually(t, "task y completed", func() bool { return e.state(pre) == store.StateCompleted })
	})

	// A refused follow-up's inline push config is not kept (spec §7, §10.1): the caller was told the
	// request was refused. An accepted one's is.
	t.Run("refused follow-up keeps no push config", func(t *testing.T) {
		cctx, pre := caller(true)
		hold := make(chan struct{})
		defer func() {
			select {
			case <-hold:
			default:
				close(hold)
			}
		}()
		d.set(pre,
			turn{hold: hold, state: a2a.TaskStateInputRequired, outcome: DispatchInterrupted},
			turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
		if _, err := q.Write(cctx, payloadOf(requestOf("z-1", "task z"))); err != nil {
			t.Fatal(err)
		}
		eventually(t, "task z's turn running", func() bool { return d.count(pre) == 1 })
		fu := followUpOf(pre, "z-2", "while running")
		fu.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://hooks.example/refused", Token: "tok"}}
		if _, err := q.Write(follower, payloadOf(fu)); !errors.Is(err, ErrRefused) {
			t.Fatalf("a follow-up while a turn runs: %v", err)
		}
		if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, pre); n != 0 {
			t.Errorf("%d push configs: the refused follow-up's webhook stays on the task", n)
		}
		close(hold)
		eventually(t, "task z at input-required", func() bool { return e.state(pre) == store.StateInputRequired })
		ok := followUpOf(pre, "z-3", "the answer")
		ok.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://hooks.example/accepted", Token: "tok"}}
		if _, err := q.Write(follower, payloadOf(ok)); err != nil {
			t.Fatalf("the follow-up at input-required: %v", err)
		}
		if n := e.count(`select count(*) from kontra.push_configs where task_id = $1 and id = 'inline:https://hooks.example/accepted'`, pre); n != 1 {
			t.Errorf("%d push configs: the accepted follow-up's config is kept", n)
		}
		eventually(t, "task z completed", func() bool { return e.state(pre) == store.StateCompleted })
	})

	// Two identical requests arriving together make one task: the second answers with the first's (spec
	// §15), so the work is not dispatched twice.
	t.Run("concurrent resend", func(t *testing.T) {
		for round := range 3 {
			var (
				mu      sync.Mutex
				arrived int
				both    = make(chan struct{})
			)
			// Both requests pass resend detection's first look at the same moment.
			barrier := func() time.Time {
				mu.Lock()
				arrived++
				k := arrived
				if k == 2 {
					close(both)
				}
				mu.Unlock()
				if k <= 2 {
					<-both
				}
				return time.Now()
			}
			rq := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}, Now: barrier})
			msgID := fmt.Sprintf("dup-%d", round)
			var (
				wg   sync.WaitGroup
				got  [2]a2a.TaskID
				errs [2]error
			)
			for i := range 2 {
				cctx, pre := caller(true)
				d.set(pre, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
				wg.Add(1)
				go func() {
					defer wg.Done()
					got[i], errs[i] = rq.Write(cctx, payloadOf(requestOf(msgID, "the same request")))
				}()
			}
			wg.Wait()
			if errs[0] != nil || errs[1] != nil || got[0] != got[1] {
				t.Fatalf("round %d: ids %v errors %v, want one task for both", round, got, errs)
			}
			if n := e.count(`select count(*) from kontra.tasks where first_message_id = $1`, msgID); n != 1 {
				t.Fatalf("round %d: %d tasks for one request sent twice at once", round, n)
			}
		}
	})
}
