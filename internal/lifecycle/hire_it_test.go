package lifecycle

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// quotedCaller is caller(true) with a quote decision.
func quotedCaller() (context.Context, string) {
	id, _ := ids.NewTaskID()
	ctx := context.WithValue(context.Background(), ownerKey{}, buyer)
	return hooks.WithAdmitted(ctx, hooks.Admitted{TaskID: string(id), Decision: hooks.Decision{Outcome: hooks.Quote,
		Quote: &hooks.QuoteTerms{ApproveURL: "https://app.kontra.sh/#/t/" + string(id), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}}), string(id)
}

// The work queue's hire path (relay.md §12.2, §12.3; M4 WP-04): a hire's new task is marked hired with the hire's
// resend key as first_request and the gate-minted messageId; a hire of the same key while the task is quoted, or less
// than the retry window after it left quoted, is answered with that task (StreamStart.New false) whatever its
// messageId and pre-minted id, and two arriving together make one task (the key's lock); an allowed hire, another
// key, and a retry past the window are new tasks.
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
	hire := Hire{Key: key, RetryWindow: time.Minute}

	write := func(cctx context.Context, h Hire) (string, StreamStart) {
		t.Helper()
		var ss StreamStart
		id, err := q.Write(WithStreamStart(WithHire(cctx, h), &ss), payloadOf(requestOf(uuid.NewString(), "hi")))
		if err != nil {
			t.Fatal(err)
		}
		return string(id), ss
	}
	qctx, pre := quotedCaller()
	first, ss := write(qctx, hire)
	if first != pre || !ss.New {
		t.Fatalf("a hire: task %s (pre-minted %s), stream start %+v", first, pre, ss)
	}
	var hired bool
	var firstReq []byte
	var firstMsg string
	if err := e.pool.QueryRow(ctx, `select hired, first_request, first_message_id from kontra.tasks where id = $1`, first).Scan(&hired, &firstReq, &firstMsg); err != nil {
		t.Fatal(err)
	}
	if !hired || string(firstReq) != string(key[:]) || firstMsg == "" {
		t.Fatalf("the hire task: hired %v, first_request %x, first_message_id %q", hired, firstReq, firstMsg)
	}

	// A retry while quoted, two at once: both answered with the first task.
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c2, _ := quotedCaller()
			var ss StreamStart
			id, err := q.Write(WithStreamStart(WithHire(c2, hire), &ss), payloadOf(requestOf(uuid.NewString(), "hi")))
			if err != nil || ss.New {
				t.Errorf("a retry: %v, %+v", err, ss)
			}
			got[i] = string(id)
		}()
	}
	wg.Wait()
	if got[0] != first || got[1] != first {
		t.Fatalf("retries answered %v, want %s", got, first)
	}
	if n := e.count(`select count(*) from kontra.tasks`); n != 1 {
		t.Fatalf("%d tasks after the retries", n)
	}
	// Two new hires of another key arriving together make one task, through the key's lock.
	other := Hire{Key: sha256.Sum256([]byte("other")), RetryWindow: time.Minute}
	var mu sync.Mutex
	made := map[string]bool{}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c2, _ := quotedCaller()
			id, err := q.Write(WithHire(c2, other), payloadOf(requestOf(uuid.NewString(), "hi")))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			made[string(id)] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(made) != 1 {
		t.Fatalf("two identical hires made %d tasks", len(made))
	}

	// Out of quoted (the client canceled it): still a retry within the window.
	if _, _, err := lc.Cancel(ctx, a2a.TaskID(first)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the quoted task canceled", func() bool { return e.state(first) == store.StateCanceled })
	c3, _ := quotedCaller()
	if id, ss := write(c3, hire); id != first || ss.New {
		t.Fatalf("a retry within the window after it left quoted: %s %+v", id, ss)
	}
	// Past the window: a new task.
	e.exec(`update kontra.tasks set terminal_at = now() - interval '2 minutes' where id = $1`, first)
	c4, _ := quotedCaller()
	if id, ss := write(c4, hire); id == first || !ss.New {
		t.Fatalf("a hire past the retry window answered %s %+v", id, ss)
	}
	// An allowed hire is never a retry target.
	actx, _ := caller(true)
	allowed := Hire{Key: sha256.Sum256([]byte("allowed")), RetryWindow: time.Minute}
	d1, _ := write(actx, allowed)
	d.set(d1, turn{state: a2a.TaskStateCompleted, outcome: DispatchTerminal})
	actx2, _ := caller(true)
	if d2, ss := write(actx2, allowed); d2 == d1 || !ss.New {
		t.Fatalf("a second allowed hire answered the first task: %s %+v", d2, ss)
	}
}
