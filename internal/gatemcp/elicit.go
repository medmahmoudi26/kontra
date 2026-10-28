package gatemcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/store"
)

// CodeURLElicitationRequired is URLElicitationRequiredError (MCP 2025-11-25), hire's answer to a legacy client that
// declared elicitation.url when Admission quotes (§12.3, D21). Never on a modern request (D24, DM4-2).
const CodeURLElicitationRequired = -32042

// MethodElicitationComplete is the notification a session gets when the task of a URL elicitation it received
// leaves quoted (§12.3).
const MethodElicitationComplete = "notifications/elicitation/complete"

// Elicitations keeps the URL elicitations hire sent: (elicitationId, task, session) in tasks.elicitation_id and
// tasks.elicitation_session (§12.3), and writes notifications/elicitation/complete on one GET stream of the session
// (DM4-21).
type Elicitations struct {
	log    *store.Log
	waker  store.TaskWaker
	logger *slog.Logger
	// rescan is how often an open GET stream reads the session's elicitations without a wake: it finds the tasks
	// quoted after it opened (their wakes then reach it), and a task that left quoted between two reads.
	rescan    time.Duration
	keepAlive time.Duration
}

// NewElicitations writes to log and is woken by waker.
func NewElicitations(log *store.Log, waker store.TaskWaker, logger *slog.Logger) *Elicitations {
	if logger == nil {
		logger = slog.Default()
	}
	return &Elicitations{log: log, waker: waker, logger: logger.With("component", "gate-mcp-elicitations"),
		rescan: 2 * time.Second, keepAlive: keepAliveEvery}
}

// Record mints an elicitationId for a quoted task and stores it with the session that receives the -32042, or
// returns the one already stored (a retry, §12.3), the session that asks now taking the notification. ok is false
// when the task is no longer quoted (its notification is then due, or sent): the caller answers it as it is now.
func (e *Elicitations) Record(ctx context.Context, taskID uuid.UUID, session [32]byte) (string, bool, error) {
	return e.log.RecordElicitation(ctx, taskID, uuid.NewString(), session)
}

// Stream serves a legacy session's GET stream (§12.3, DM4-21): keep-alives every 15 s (§2.2) and, for each task
// whose -32042 went to this session, once it leaves quoted (while the stream is open, or less than RetryWindow before
// it opened), one notifications/elicitation/complete {elicitationId}, written on this stream only if this stream's
// claim (tasks.elicitation_notified_at) won: a message goes on one stream of the session, on whichever replica, never
// on another session's. No event ids; the stream is not resumed. It returns when the client goes.
func (e *Elicitations) Stream(ctx context.Context, w http.ResponseWriter, sid string, sess *mcprelay.Session) {
	s := mcprelay.NewGateStream(w, sid)
	if s.Start() != nil {
		return
	}
	wakes := make(chan struct{}, 1)
	waiting := map[uuid.UUID]func(){}
	defer func() {
		for _, stop := range waiting {
			stop()
		}
	}()
	watch := func(id uuid.UUID) {
		if _, ok := waiting[id]; ok {
			return
		}
		ch, stop := e.waker.WaitTask(ctx, id)
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-ch:
					select {
					case wakes <- struct{}{}:
					default:
					}
				case <-done:
					return
				}
			}
		}()
		waiting[id] = func() { stop(); close(done) }
	}
	rescan := time.NewTicker(e.rescan)
	defer rescan.Stop()
	keep := time.NewTicker(e.keepAlive)
	defer keep.Stop()
	for {
		list, err := e.log.SessionElicitations(ctx, sess.Hash, sess.BuyerID, RetryWindow)
		if err != nil && ctx.Err() != nil {
			return
		}
		if err != nil {
			e.logger.Warn("a session's URL elicitations could not be read", "error", err)
		}
		for _, el := range list {
			if el.State == store.StateQuoted {
				watch(el.TaskID)
				continue
			}
			id, won, err := e.log.ClaimElicitationComplete(ctx, el.TaskID, sess.Hash)
			if stop, ok := waiting[el.TaskID]; ok {
				stop()
				delete(waiting, el.TaskID)
			}
			if err != nil || !won {
				continue
			}
			msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": MethodElicitationComplete,
				"params": map[string]any{"elicitationId": id}})
			if err := s.Event("", msg); err != nil {
				e.logger.Info("an elicitation/complete was claimed but its stream closed", "task_id", el.TaskID, "error", err)
				return
			}
			e.logger.Info("elicitation/complete sent", "task_id", el.TaskID)
		}
		select {
		case <-ctx.Done():
			return
		case <-wakes:
		case <-rescan.C:
		case <-keep.C:
			if s.KeepAlive() != nil {
				return
			}
		}
	}
}
