package lifecycle

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/temporaltest"
)

// The work queue's hire path (relay.md §12.2, §12.3; M4 WP-04): a hire's new task is marked hired with the hire's
// resend key as first_request and the gate-minted messageId. A hire without an idempotencyKey is never answered with
// an earlier task (D351 removed the retry of a quoted hire): the same key again is a new task.
func TestQueueWriteHire(t *testing.T) {
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
	q := NewQueue(e.svc, e.log, lc, QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}})
	ctx := context.Background()
	key := sha256.Sum256([]byte("hello\x00{\"message\":\"hi\",\"service\":\"hello\"}"))
	hire := Hire{Key: key}

	write := func(cctx context.Context, h Hire) (string, StreamStart) {
		t.Helper()
		var ss StreamStart
		id, err := q.Write(WithStreamStart(WithHire(cctx, h), &ss), payloadOf(requestOf(uuid.NewString(), "hi")))
		if err != nil {
			t.Fatal(err)
		}
		return string(id), ss
	}
	actx, pre := caller(true)
	first, ss := write(actx, hire)
	if first != pre || !ss.New {
		t.Fatalf("a hire: task %s (pre-minted %s), stream start %+v", first, pre, ss)
	}
	d.set(first, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
	var hired bool
	var firstReq []byte
	var firstMsg string
	if err := e.pool.QueryRow(ctx, `select hired, first_request, first_message_id from kontra.tasks where id = $1`, first).Scan(&hired, &firstReq, &firstMsg); err != nil {
		t.Fatal(err)
	}
	if !hired || string(firstReq) != string(key[:]) || firstMsg == "" {
		t.Fatalf("the hire task: hired %v, first_request %x, first_message_id %q", hired, firstReq, firstMsg)
	}
	// The same unkeyed hire again: a new task.
	actx2, _ := caller(true)
	if second, ss := write(actx2, hire); second == first || !ss.New {
		t.Fatalf("a second unkeyed hire answered the first task: %s %+v", second, ss)
	}
	if n := e.count(`select count(*) from kontra.tasks`); n != 2 {
		t.Fatalf("%d tasks after two hires, want 2", n)
	}
}
