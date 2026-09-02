// Package temporalhost serves a Go actor as a Temporal activity worker (ADR 0018) — the peer of
// Python's internals/temporal/host.py, and byte-compatible with it: same queue derivation, same
// activity names, same payload keys, so the Go handler cannot tell which SDK is on the other end.
//
// An actor process registers `RunBatch` and `Close` and polls `{actor}-{version}-sessions`
// directly. There is no sidecar, no placement service, no HTTP app and no actor type name — the
// handler still owns the workflow, and Temporal splits workflow and activity across languages by
// design, which is what made the sidecar removable at all.
//
// SINGLE ACTIVATION IS A PER-PROCESS CONCERN. What serialises work for one actor id is Temporal:
// one backing workflow per id, blocking on its activity. So a map plus a mutex is the whole of
// it, and no cluster-wide registry is needed or would help.
//
// HEARTBEATS ARE THE LIVENESS CHECK. A batch beats per committed unit, so a stuck unit stops
// beating and HeartbeatTimeout catches it. Without that, StartToCloseTimeout is the only bound
// and it cannot tell a long batch from a wedged one.
package temporalhost

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/medmahmoudi26/kontra/runtime/go/codec"
	"github.com/medmahmoudi26/kontra/runtime/go/engine"
	"github.com/medmahmoudi26/kontra/runtime/go/registrar"
	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// TaskQueue is the sessions queue this host binds: `{name}-{version}-sessions`, or
// `{name}-shared-sessions` when a version is absent.
//
// The handler builds the same string from its workflow's OWN task queue — it cannot read env,
// because workflow code must stay deterministic — so a mismatch here means an actor that
// registers, polls nothing, and looks like a healthy idle Worker while every run hangs to
// StartToClose. shared/conformance/queues.json §sessions is what holds the two to one answer;
// host_conformance_test.go is this package's arm.
func TaskQueue(name, version string) string {
	base := name + "-shared"
	if version != "" {
		base = name + "-" + version
	}
	return base + "-sessions"
}

// Machine is the Machine this host runs on — the host portion of the Temporal worker identity,
// which is exactly what the fleet's poller listing shows per Machine (`11@kf-dns-01@nscheck-0.1.0`
// is `pid@host@queue`). The pid is deliberately dropped: a restarted Worker is the same Machine,
// and keeping it would make one Droplet read as a new Machine after every restart.
//
// Snapshotted at init because a hostname does not change under a live process and this is read
// once per Batch. Peer of MACHINE in internals/temporal/host.py, derived the same way on both
// sides — neither SDK sets a custom Temporal identity, so the SDK default (`{pid}@{hostname}` in
// Python, `{pid}@{hostname}@{queue}` here) carries the same host portion this reads.
//
// On the fleet it is the Droplet's name (`kf-dns-01`): a Worker is a pair of systemd units ON the
// Machine, not a container, so nothing stands between the two
// (backend/src/infra/programs/machine.ts). Empty when the lookup fails, which reads
// downstream as unrecorded — the one honest answer to "which Machine" when the OS will not say.
var Machine = func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}()

// sessions is this process's activation table: at most one live instance per actor id, and one
// batch at a time for that id. Two concurrent batches would share the instance the author opened
// in Load — one browser, two batches writing the same commit map.
type sessions struct {
	mu    sync.Mutex
	live  map[string]*engine.KontraActor
	locks map[string]*sync.Mutex
}

func newSessions() *sessions {
	return &sessions{live: map[string]*engine.KontraActor{}, locks: map[string]*sync.Mutex{}}
}

// lockFor returns the per-id mutex, creating it on first use.
func (s *sessions) lockFor(actorID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, ok := s.locks[actorID]
	if !ok {
		lk = &sync.Mutex{}
		s.locks[actorID] = lk
	}
	return lk
}

// get returns the live instance for an id, building one if this process holds none. Keyed by the
// actor id the workflow derives, so two nodes of one run get two instances (two resources) and a
// retry of the SAME node reuses the instance if this process still holds it.
func (s *sessions) get(actorID string) *engine.KontraActor {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.live[actorID]
	if !ok {
		a = engine.New(actorID, nil)
		s.live[actorID] = a
	}
	return a
}

func (s *sessions) drop(actorID string) *engine.KontraActor {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.live[actorID]
	delete(s.live, actorID)
	delete(s.locks, actorID)
	return a
}

// Activities is the set this actor serves, closed over its activation table and the Sessions this
// process holds. ONE of these per host process, registered on the shared sessions queue AND on
// every live Session's own queue — so a Method call arriving on a Session's queue meets the same
// activation table as the open that placed it.
type Activities struct {
	s    *sessions
	live *sessionWorkers
	// name/version derive this actor's queues. The host reads them from its registry; a Session's
	// worker inherits them, because the queue name is the address (ADR 0023 §6).
	name, version string

	// scopes records which actor id each live Session in THIS process activated: written by
	// OpenSession, read by CloseSession, so ending one scope cannot close an instance another
	// live scope is still holding — two scopes on one key share the instance, because a key IS
	// the identity (ADR 0022).
	scopeMu sync.Mutex
	scopes  map[string]string
}

func newActivities(name, version string, spawn spawnFn, maxLive int) *Activities {
	return &Activities{
		s:       newSessions(),
		live:    newSessionWorkers(spawn, maxLive),
		name:    name,
		version: version,
		scopes:  map[string]string{},
	}
}

// RunBatch runs one batch on the instance pinned to req.ActorID.
func (h *Activities) RunBatch(ctx context.Context, req engine.RunBatchReq) (*engine.RunBatchResp, error) {
	if req.ActorID == "" {
		return nil, fmt.Errorf("RunBatch payload carries no actor_id")
	}
	lk := h.s.lockFor(req.ActorID)
	lk.Lock()
	defer lk.Unlock()

	a := h.s.get(req.ActorID)
	// Beat per committed unit. The field names are a cross-language contract with
	// backend/src/heartbeat.ts, where every field is optional and defaults to 0 — so a
	// wrong name reports 0/0 forever rather than erroring. Peer of _beat in engine.py.
	a.SetHeartbeat(func(done, total, isolated int) {
		activity.RecordHeartbeat(ctx, map[string]any{
			"node": req.NodeID, "done": done, "total": total, "isolated": isolated,
		})
	})

	resp, err := a.RunBatch(ctx, req)
	if err != nil {
		// A dead resource nulls the instance inside the engine; drop the whole session so the
		// retry rebuilds it from scratch rather than reusing a half-torn one.
		h.s.drop(req.ActorID)
		return nil, err
	}
	// PROVENANCE TRAVELS WITH THE BATCH — peer of `out["machine"]` in host.py. The handler
	// copies this onto the result ref's meta, the caller's Batch reads it from there with no
	// fetch, and publish writes it as the row's Machine. Stamped HERE, in the process that ran
	// the Method: the handler workflow polls a queue every Machine's handler polls, so its own
	// hostname would name a Machine at random rather than this one.
	resp.Machine = Machine
	return resp, nil
}

// Close tears the instance down. The workflow schedules it on every exit path, including a
// failed batch — which is exactly when a loaded resource must not leak.
func (h *Activities) Close(ctx context.Context, req map[string]any) (*engine.CloseResp, error) {
	actorID, _ := req["actor_id"].(string)
	if actorID == "" {
		return &engine.CloseResp{Closed: true}, nil
	}
	lk := h.s.lockFor(actorID)
	lk.Lock()
	defer lk.Unlock()

	if a := h.s.drop(actorID); a != nil {
		return a.Close(ctx)
	}
	return &engine.CloseResp{Closed: true}, nil
}

// Serve runs the actor as a Temporal activity worker — THE way (*kontra.Actor).Run() boots a Go
// actor. It blocks until interrupted.
func Serve(r *core.Registry) error { return serve(r, worker.InterruptCh()) }

// serve is Serve's whole body with the stop channel handed in. Split for ONE reason: a test that
// boots a real host against a real server has to be able to take it down again, and
// `worker.InterruptCh()` can only be closed by signalling the test process itself. Serve is still
// the only way an actor starts.
func serve(r *core.Registry, stop <-chan interface{}) error {
	engine.Configure(r)

	registrar.SelfRegister(r) // best-effort; no-op unless KONTRA_ORCHESTRATOR_URL is set
	engine.ServeMetrics(r.Name, r.Version)

	// The claim-check codec is NOT optional. The handler's client encodes any payload over
	// 128 KiB into a `binary/claim-check-v1` ref, and this worker executes that activity — so
	// without the matching codec every over-threshold batch dies on "Unknown payload encoding",
	// retried to exhaustion. It passes through when KONTRA_S3_ENDPOINT is unset, exactly as the
	// handler's does, so a local no-S3 run is unaffected.
	casStore, err := codec.StoreFromEnv(context.Background())
	if err != nil {
		return fmt.Errorf("claim-check store: %w", err)
	}

	c, err := client.Dial(client.Options{
		HostPort:      getenv("KONTRA_ADDRESS", "localhost:7233"),
		Namespace:     getenv("KONTRA_NAMESPACE", "default"),
		DataConverter: codec.DataConverter(casStore),
	})
	if err != nil {
		return fmt.Errorf("temporal dial: %w", err)
	}
	defer c.Close()

	queue := TaskQueue(r.Name, r.Version)
	// NOT the live-Session cap any more (ADR 0023 §6). An open returns as soon as its Session's
	// worker is up and frees this slot immediately, so activity slots stop bounding live
	// Sessions — sessionWorkers counts those. This bounds concurrent activity executions on the
	// shared queue, which is what it actually is.
	w := worker.New(c, queue, worker.Options{
		MaxConcurrentActivityExecutionSize: maxParallelSessions(),
	})

	// A live Session's worker: same client, same activities, its OWN queue. Built here rather
	// than in sessions.go so that file stays free of the Temporal SDK and its two traps stay
	// testable without a cluster (ADR 0023 §6).
	var h *Activities
	spawn := func(sessionQueue string) (sessionWorker, error) {
		sw := worker.New(c, sessionQueue, worker.Options{})
		sw.RegisterActivityWithOptions(h.RunBatch, activity.RegisterOptions{Name: "RunBatch"})
		sw.RegisterActivityWithOptions(h.Close, activity.RegisterOptions{Name: "Close"})
		// The close is served HERE, on the Session's own queue, so only the host holding the
		// Session can answer it — that is what makes it reach the loaded instance.
		sw.RegisterActivityWithOptions(h.CloseSession, activity.RegisterOptions{Name: "CloseSession"})
		return sw, nil
	}
	h = newActivities(r.Name, r.Version, spawn, maxParallelSessions())

	// The SHARED queue carries the open, and only the open: Temporal's dispatch of it is the
	// placement decision. RunBatch and Close stay here too for the unscoped path, where a
	// dispatch stands alone and never opens a scope.
	w.RegisterActivityWithOptions(h.OpenSession, activity.RegisterOptions{Name: "OpenSession"})
	w.RegisterActivityWithOptions(h.RunBatch, activity.RegisterOptions{Name: "RunBatch"})
	w.RegisterActivityWithOptions(h.Close, activity.RegisterOptions{Name: "Close"})

	log.Printf("[host] %s@%s serving OpenSession/RunBatch/Close on %s", r.Name, r.Version, queue)
	return w.Run(stop)
}

func maxParallelSessions() int {
	if v := os.Getenv("KONTRA_MAX_PARALLEL_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 4
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
