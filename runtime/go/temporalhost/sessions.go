package temporalhost

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
)

// SessionTaskQueue is ONE live Session's queue: `{name}-{version}-s-{sessionId}` (ADR 0023 §6).
//
// The singular of TaskQueue above and a different plane: that one is polled by every worker of
// this actor version and is where an OPEN lands, so Temporal's dispatch of the open IS the
// placement decision; this one is polled by the single worker that answered that open, which is
// what pins every call of the scope to the process holding the loaded resource.
//
// Derived independently in four languages — here, python `internals.temporal.host`, the caller
// (`kontra.catalog.session_queue`, which closes on it) and `runtime/handler/internal/identity`
// .SessionQueue, which dispatches onto it. runtime/handler/internal is not importable from this module
// (a different module, and internal to handler), so the decoupling rule leaves this a
// re-derivation and shared/conformance/queues.json §session is what holds them to one answer. Not a
// count in a comment: the three comments that carried one disagreed with each other.
//
// NO ID, NO QUEUE. An empty session id would derive `{shared}-s-`, a real queue every Session of
// this actor would share — the pinning gone with nothing failing. OpenSession refuses before it
// gets here; this returns empty so no path can turn a blank id into an address.
func SessionTaskQueue(name, version, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	base := name + "-shared"
	if version != "" {
		base = name + "-" + version
	}
	return base + "-s-" + sessionID
}

// sessionWorker is the part of temporal's worker.Worker a live Session drives. It is an interface
// so the Session machinery is testable with no cluster, which is what lets the two traps in
// sessionWorkers be pinned by tests rather than by care.
type sessionWorker interface {
	Start() error
	Stop()
}

// spawnFn builds (but does not start) the worker that will poll one Session's queue.
type spawnFn func(queue string) (sessionWorker, error)

// sessionWorkers is the set of live Sessions THIS process holds, one worker each.
//
// Not to be confused with `sessions` in host.go, which is the activation table (actor id ->
// loaded instance). This one is the addressing table: session id -> the worker polling that
// Session's own queue.
type sessionWorkers struct {
	spawn   spawnFn
	maxLive int

	mu   sync.Mutex
	live map[string]sessionWorker
	// opening is the ids whose worker is being built. They count against the cap — otherwise a
	// burst of opens all measure an empty table and every one of them is admitted.
	opening map[string]struct{}
	// claims serialises two attempts at ONE Session, so the loser waits and then finds the worker
	// live instead of building a second poller on that queue.
	claims map[string]*sync.Mutex
}

func newSessionWorkers(spawn spawnFn, maxLive int) *sessionWorkers {
	return &sessionWorkers{
		spawn:   spawn,
		maxLive: maxLive,
		live:    map[string]sessionWorker{},
		opening: map[string]struct{}{},
		claims:  map[string]*sync.Mutex{},
	}
}

func (s *sessionWorkers) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// open activates a Session on `queue`. True if this call spawned its worker.
//
// IDEMPOTENT, because a Temporal retry of the open must not build a second worker on one
// Session's queue — two pollers would split the scope's calls across two instances of the actor,
// which is the pinning silently gone on a path that only happens under retry. A retry can arrive
// while the first attempt is still starting its worker, so the claim is what makes this hold,
// not a check.
func (s *sessionWorkers) open(sessionID, queue string) (bool, error) {
	lk := s.claim(sessionID)
	lk.Lock()
	defer lk.Unlock()

	s.mu.Lock()
	if _, held := s.live[sessionID]; held {
		s.mu.Unlock()
		return false, nil
	}
	if len(s.live)+len(s.opening) >= s.maxLive {
		n := len(s.live) + len(s.opening)
		s.mu.Unlock()
		return false, sessionCapReached{held: n, max: s.maxLive, sessionID: sessionID}
	}
	s.opening[sessionID] = struct{}{} // reserved: the cap counts it from here
	s.mu.Unlock()

	w, err := s.spawn(queue)
	if err == nil {
		// A worker that dies on boot must FAIL the open. Reporting success would leave the caller
		// dispatching into a queue nobody polls, which it meets as a hang until ScheduleToStart
		// rather than as an error; failing it lets the retry land on another host.
		err = w.Start()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.opening, sessionID)
	if err != nil {
		return false, fmt.Errorf("session worker for %s on %s: %w", sessionID, queue, err)
	}
	s.live[sessionID] = w
	log.Printf("[sessions] opened %s on %s (%d live)", sessionID, queue, len(s.live))
	return true, nil
}

// close ends a Session this process holds and frees its slot. True if this call ended it.
//
// The worker is stopped WITHOUT WAITING, and that is not a nicety. CloseSession runs ON the
// Session's own queue, so it is an in-flight activity of the very worker being stopped: a
// graceful Stop() waits for in-flight activities to finish, and this one cannot finish until
// Stop() returns. Awaiting it deadlocks, and presents as a close that hangs rather than an
// error. The slot is freed here, synchronously, so the cap is right the moment the scope ends.
func (s *sessionWorkers) close(sessionID string) bool {
	s.mu.Lock()
	w, held := s.live[sessionID]
	if held {
		delete(s.live, sessionID)
	}
	delete(s.claims, sessionID)
	n := len(s.live)
	s.mu.Unlock()

	if !held {
		return false
	}
	go w.Stop()
	log.Printf("[sessions] closed %s (%d live)", sessionID, n)
	return true
}

// claim returns the per-session open lock, creating it on first use.
func (s *sessionWorkers) claim(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, ok := s.claims[sessionID]
	if !ok {
		lk = &sync.Mutex{}
		s.claims[sessionID] = lk
	}
	return lk
}

// sessionCapReached says this host is already holding as many Sessions as it offers.
//
// Deliberately an ORDINARY error: the open is a Temporal activity, so a refusal becomes
// back-pressure — the task returns to the actor's shared queue and any host with a free slot
// takes it. A non-retryable failure here would fail a caller's scope for a fleet that is merely
// busy.
type sessionCapReached struct {
	held, max int
	sessionID string
}

func (e sessionCapReached) Error() string {
	return fmt.Sprintf("host holds %d/%d live Sessions; cannot open %s (KONTRA_MAX_PARALLEL_SESSIONS)",
		e.held, e.max, e.sessionID)
}

// OpenSessionResp is what the open reports back. `pid` is the one fact a caller cannot derive —
// the queue chose the process — so it rides back for tracing and to make pinning observable.
type OpenSessionResp struct {
	SessionID string `json:"session_id"`
	ActorID   string `json:"actor_id"`
	Queue     string `json:"queue"`
	PID       int    `json:"pid"`
	Opened    bool   `json:"opened"`
}

// OpenSession activates a Session on this host and starts polling its queue (ADR 0023 §4, §6).
//
// Runs on the actor's SHARED sessions queue, so Temporal's own dispatch picks the host — that
// choice is the placement decision, and there is nothing else to make it.
func (h *Activities) OpenSession(ctx context.Context, req map[string]any) (*OpenSessionResp, error) {
	sessionID := stringField(req, "session_id", "sessionId")
	if sessionID == "" {
		// No default: a blank id derives a queue every Session of this actor would share, which
		// is the pinning gone with nothing failing.
		return nil, fmt.Errorf("OpenSession payload carries no session_id — see ADR 0023 §6")
	}
	queue := SessionTaskQueue(h.name, h.version, sessionID)
	opened, err := h.live.open(sessionID, queue)
	if err != nil {
		return nil, err
	}
	actorID := sessionActorID(sessionID, stringField(req, "key"))
	// Remember which instance this scope activated, so the close reaches THAT one. Without it a
	// keyed scope would close by session id and leave the keyed instance loaded.
	h.scopeMu.Lock()
	h.scopes[sessionID] = actorID
	h.scopeMu.Unlock()
	return &OpenSessionResp{
		SessionID: sessionID,
		ActorID:   actorID,
		Queue:     queue,
		PID:       os.Getpid(),
		Opened:    opened,
	}, nil
}

// sessionActorID is which actor instance a Session activates: its key if it claimed one, else the
// Session id. Congruent with the actor id runtime/handler/workflow.go derives for a scoped dispatch, and
// with python's internals.temporal.host.session_actor_id.
//
// Keys are optional (ADR 0023 §10). A key is a claim on a shared identity — it carries durable
// state across scopes and across Runs (ADR 0022) — while a bare handle is a private anonymous
// Session whose state begins empty and dies with it.
func sessionActorID(sessionID, key string) string {
	if key != "" {
		return key
	}
	return sessionID
}

// stringField reads the first spelling present. The caller writes snake_case; the camelCase
// alternative is accepted for the same reason python's host accepts it — a JSON payload crossing
// three languages should not fail on a spelling.
func stringField(req map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := req[n].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// CloseSessionResp reports what the close actually ended, so a caller can tell a real teardown
// from a no-op without reading logs.
type CloseSessionResp struct {
	SessionID string `json:"session_id"`
	Closed    bool   `json:"closed"`
	PID       int    `json:"pid"`
}

// CloseSession ends the scope: the actor instance it activated, then the worker polling its
// queue (ADR 0023 §4). Runs ON the Session's own queue, so only the host that holds it can
// answer — which is what makes the close reach the instance rather than some other process.
//
// Closing a Session this process never held is NOT an error. The workflow schedules the close on
// every exit path, and after a host loss the scope is already gone; failing here would turn a
// tidy exit into a failed workflow for work that is genuinely finished.
func (h *Activities) CloseSession(ctx context.Context, req map[string]any) (*CloseSessionResp, error) {
	sessionID := stringField(req, "session_id", "sessionId")
	if sessionID == "" {
		return nil, fmt.Errorf("CloseSession payload carries no session_id — see ADR 0023 §6")
	}

	// The instance first, while its worker is still up: @actor.close is the author's, and it
	// runs on the resource this scope opened.
	h.scopeMu.Lock()
	actorID, known := h.scopes[sessionID]
	delete(h.scopes, sessionID)
	h.scopeMu.Unlock()
	if !known {
		actorID = sessionActorID(sessionID, stringField(req, "key"))
	}
	if actorID != "" {
		lk := h.s.lockFor(actorID)
		lk.Lock()
		if a := h.s.drop(actorID); a != nil {
			if _, err := a.Close(ctx); err != nil {
				lk.Unlock()
				// Free the slot anyway: a resource that refused to close is not a reason to
				// leak a Session slot for the life of the process.
				h.live.close(sessionID)
				return nil, fmt.Errorf("closing %s: %w", actorID, err)
			}
		}
		lk.Unlock()
	}

	closed := h.live.close(sessionID)
	return &CloseSessionResp{SessionID: sessionID, Closed: closed, PID: os.Getpid()}, nil
}
