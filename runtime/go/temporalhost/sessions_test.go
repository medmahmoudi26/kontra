package temporalhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
)

// fakeWorker stands in for temporal's worker.Worker with the two methods a live Session drives it
// by. Injected, so everything below runs with no cluster.
type fakeWorker struct {
	queue        string
	startErr     error
	mu           sync.Mutex
	started      bool
	stopCalled   bool
	stopped      bool
	mayStop      chan struct{} // held closed so a test can pin that the close does not wait
	stopEntered  chan struct{}
	stopFinished chan struct{}
}

func newFakeWorker(queue string) *fakeWorker {
	w := &fakeWorker{
		queue:        queue,
		mayStop:      make(chan struct{}),
		stopEntered:  make(chan struct{}),
		stopFinished: make(chan struct{}),
	}
	close(w.mayStop) // stops immediately unless a test blocks it
	return w
}

func (w *fakeWorker) Start() error {
	if w.startErr != nil {
		return w.startErr
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
	return nil
}

func (w *fakeWorker) Stop() {
	w.mu.Lock()
	w.stopCalled = true
	w.mu.Unlock()
	close(w.stopEntered)
	<-w.mayStop
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	close(w.stopFinished)
}

func (w *fakeWorker) didStop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

// spy is the spawn seam plus the record of what it built.
type spy struct {
	mu    sync.Mutex
	built []*fakeWorker
}

func (s *spy) spawn(queue string) (sessionWorker, error) {
	// Slow on purpose, because the real one is: a Session's worker connects and starts polling.
	// Without it a concurrent-open test would pass on scheduling luck rather than on the claim
	// being atomic.
	time.Sleep(2 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	w := newFakeWorker(queue)
	s.built = append(s.built, w)
	return w, nil
}

func (s *spy) queues() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.built))
	for _, w := range s.built {
		out = append(out, w.queue)
	}
	return out
}

func (s *spy) at(i int) *fakeWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.built[i]
}

// probeHost is an actor host with the Session worker injected: `crawler@0.1.0`, four live
// Sessions unless a test says otherwise.
func probeHost(t *testing.T, maxLive int) (*Activities, *spy) {
	t.Helper()
	s := &spy{}
	return newActivities("crawler", "0.1.0", s.spawn, maxLive), s
}

// TestOpeningAScopeSpawnsAWorkerOnThatSessionsQueue is the whole addressing scheme in one test.
//
// The queue name IS the address (ADR 0023 §6): the host must derive the same string the caller
// closes on and the handler dispatches onto. Nothing registers that contract — a mismatch is a
// scope whose Method calls sit in a queue nobody polls until ScheduleToStart, which reads exactly
// like a slow actor.
func TestOpeningAScopeSpawnsAWorkerOnThatSessionsQueue(t *testing.T) {
	h, built := probeHost(t, 4)

	out, err := h.OpenSession(context.Background(), map[string]any{"session_id": "3f9a"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if got := built.queues(); len(got) != 1 || got[0] != "crawler-0.1.0-s-3f9a" {
		t.Fatalf("spawned workers on %v, want [crawler-0.1.0-s-3f9a]", got)
	}
	if out.Queue != "crawler-0.1.0-s-3f9a" {
		t.Errorf("open reported queue %q", out.Queue)
	}
	if !out.Opened {
		t.Error("the open that spawned the worker must report it")
	}
}

// TestAnOpenWithNoSessionIDIsRefused: there is no default. A blank id derives a queue every
// Session of this actor would share, which is the pinning gone with nothing failing.
func TestAnOpenWithNoSessionIDIsRefused(t *testing.T) {
	h, built := probeHost(t, 4)

	if _, err := h.OpenSession(context.Background(), map[string]any{"session_id": ""}); err == nil {
		t.Fatal("an open with no session id must fail loud")
	}
	if got := built.queues(); len(got) != 0 {
		t.Errorf("a refused open still spawned %v", got)
	}
}

// TestReopeningASessionDoesNotBuildASecondWorker is the idempotency the open must have.
//
// The open is a Temporal activity, so it retries — and a retry can arrive while the first attempt
// is still in flight (that is what a StartToClose timeout on a slow host looks like). Two workers
// polling one Session's queue would split its calls across two instances of the actor: the
// pinning gone, silently, on a path that only happens under retry.
func TestReopeningASessionDoesNotBuildASecondWorker(t *testing.T) {
	h, built := probeHost(t, 4)
	req := map[string]any{"session_id": "3f9a"}

	first, err := h.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := h.OpenSession(context.Background(), req)
	if err != nil {
		t.Fatalf("retried open: %v", err)
	}
	if !first.Opened || second.Opened {
		t.Errorf("opened flags %v/%v: the retry must report that it spawned nothing",
			first.Opened, second.Opened)
	}
	if got := built.queues(); len(got) != 1 {
		t.Fatalf("a retry built %d workers on one Session's queue: %v", len(got), got)
	}

	// And concurrently, because a retry does not wait for the attempt it is replacing.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.OpenSession(context.Background(), map[string]any{"session_id": "beef"}); err != nil {
				t.Errorf("concurrent open: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := built.queues(); len(got) != 2 {
		t.Fatalf("concurrent opens built %d workers, want 2 (one per Session): %v", len(got), got)
	}
	if h.live.len() != 2 {
		t.Errorf("%d live Sessions, want 2", h.live.len())
	}
}

// TestTheCapCountsLiveSessionsNotActivitySlots is TRAP 2.
//
// MaxConcurrentActivityExecutionSize stopped being this cap the moment the open activity returned
// as soon as it had spawned the Session's worker: it frees its slot immediately, so a 2-slot
// worker will happily hold six live Sessions. The bound has to be a count of live Sessions or it
// is not a bound at all — which is why both opens below have RETURNED and the third is still
// refused.
func TestTheCapCountsLiveSessionsNotActivitySlots(t *testing.T) {
	h, _ := probeHost(t, 2)

	for _, id := range []string{"s1", "s2"} {
		if _, err := h.OpenSession(context.Background(), map[string]any{"session_id": id}); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	_, err := h.OpenSession(context.Background(), map[string]any{"session_id": "s3"})
	if err == nil {
		t.Fatal("a host at its live-Session cap must refuse the open")
	}
	if h.live.len() != 2 {
		t.Errorf("%d live Sessions after a refusal, want 2", h.live.len())
	}

	// And the refusal is RETRYABLE: it is back-pressure, not a broken scope. The task goes back
	// to the actor's shared queue and any host with a free slot takes it. Non-retryable here
	// would fail a caller's scope for a fleet that is merely busy.
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.NonRetryable() {
		t.Errorf("the cap refusal is non-retryable: %v", err)
	}
}

// TestClosingASessionFreesItsSlot: the cap is a live count, so a scope that ended must give its
// slot back — otherwise a long-lived host stops answering opens after N scopes, ever.
func TestClosingASessionFreesItsSlot(t *testing.T) {
	h, _ := probeHost(t, 1)

	if _, err := h.OpenSession(context.Background(), map[string]any{"session_id": "s1"}); err != nil {
		t.Fatalf("open s1: %v", err)
	}
	if _, err := h.CloseSession(context.Background(), map[string]any{"session_id": "s1"}); err != nil {
		t.Fatalf("close s1: %v", err)
	}
	if h.live.len() != 0 {
		t.Fatalf("%d live Sessions after the close, want 0", h.live.len())
	}
	if _, err := h.OpenSession(context.Background(), map[string]any{"session_id": "s2"}); err != nil {
		t.Fatalf("the freed slot did not admit the next scope: %v", err)
	}
}
