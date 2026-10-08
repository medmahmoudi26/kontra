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
// HEARTBEATS ARE THE LIVENESS CHECK, AND THEY NO LONGER MEAN "A UNIT COMMITTED". A batch beats per
// committed unit AND on a timer derived from the bound Temporal is enforcing (see the keepalive in
// RunBatch), because a Unit that legitimately runs for 45 minutes used to send nothing and be killed
// for silence while it was working — measured, three `hunt` runs, 0 rows each (GitHub #19).
//
// SO SILENCE NO LONGER MEANS STUCK, and that is a deliberate trade rather than an oversight. A
// wedged Unit now beats until StartToCloseTimeout catches it an hour later instead of the heartbeat
// catching it in two minutes. What keeps the two distinguishable is the `alive` counter on the
// timer beat: it rises while `done` does not, so a reader can still tell slow from stopped.
package temporalhost

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"

	"github.com/medmahmoudi26/kontra/sdk/go/temporaltls"
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

// Machine is the Machine this host runs on — field two of the Temporal worker identity, which is
// exactly what the fleet's poller listing shows per Machine (`11@kf-dns-01@nscheck-0.1.0` is
// `pid@host@queue`). The pid is deliberately dropped from what a Batch reports: a restarted Worker
// is the same Machine, and keeping it would make one Droplet read as a new Machine after every
// restart.
//
// Snapshotted at init because a hostname does not change under a live process and this is read
// once per Batch. Peer of MACHINE in internals/temporal/host.py, derived the same way on both
// sides. It USED to note that "neither SDK sets a custom Temporal identity"; both do now
// (workerid.go, internals/workerid.py), and this is the value they compose — so the host portion
// of the identity and the Machine on a Batch are one string by construction, not by two
// derivations agreeing.
//
// On the fleet it is the Droplet's name (`kf-dns-01`): a Worker is a pair of systemd units ON the
// Machine, not a container, so nothing stands between the two
// (control/orchestrator/src/infra/programs/machine.ts). Empty when the lookup fails, which reads
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
	// tc is the host's Temporal client, kept so an activity can publish onto the RUN workflow's
	// stream. `workflowstreams.NewClientFromActivity` addresses the activity's OWN workflow —
	// which here is `actor-<actor>-<run>-<node>`, not the run a console subscribes to — so the
	// run workflow has to be named explicitly, and naming it needs a client.
	tc client.Client
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

// keepaliveEvery is how often a long Unit says it is still alive, derived from the bound Temporal
// is enforcing on this attempt rather than named as a second constant.
//
// A THIRD OF THE WINDOW, so two consecutive ticks can be lost to scheduling, a slow RecordHeartbeat
// or a busy event loop and the attempt still lives. Halving it would leave no margin at all; a
// tenth would beat thirty times a Unit for no added safety.
//
// ZERO MEANS NO KEEPALIVE, and that is the correct reading of an absent bound: with no
// HeartbeatTimeout there is nothing to miss, so beating on a timer would be noise Temporal stores in
// the activity's mutable state on every tick. A negative value cannot arrive from Temporal but is
// folded into the same answer rather than producing a Ticker that panics.
// heartbeatBoundOf reports the HeartbeatTimeout Temporal is enforcing, or 0 when there is no
// activity behind this context.
//
// SEPARATE FROM {@link keepaliveEvery} so the arithmetic stays testable without an activity: the
// SDK's `GetInfo` panics off the activity path, so a test that wanted to check the interval would
// otherwise have to fake a whole Temporal context to divide a duration by three.
func heartbeatBoundOf(ctx context.Context) time.Duration {
	if !activity.IsActivity(ctx) {
		return 0
	}
	return activity.GetInfo(ctx).HeartbeatTimeout
}

func keepaliveEvery(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return timeout / 3
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
	// control/orchestrator/src/heartbeat.ts, where every field is optional and defaults to 0 — so a
	// wrong name reports 0/0 forever rather than erroring. Peer of _beat in engine.py.
	// `last` carries the most recent unit counts into the progress beat below, so a progress
	// heartbeat never REPLACES the liveness numbers with a payload that lacks them.
	// heartbeat.ts defaults every missing field to 0, so a beat without `done` would read as a
	// batch that had made no progress at all.
	var lk2 sync.Mutex
	last := map[string]any{"node": req.NodeID, "done": 0, "total": len(req.Units), "isolated": 0}

	a.SetHeartbeat(func(done, total, isolated int) {
		lk2.Lock()
		last["done"], last["total"], last["isolated"] = done, total, isolated
		beat := map[string]any{}
		for k, v := range last {
			beat[k] = v
		}
		lk2.Unlock()
		// THE CHECKPOINT, WHICH IS THE DURABLE HALF OF THIS BEAT.
		//
		// The three counters above say how many, never WHICH, so nothing can resume from them. This
		// says which, in the encoding the Python peer and the orchestrator share
		// (shared/conformance/checkpoint.json) — and because heartbeat details live in the
		// activity's own history, it is the one copy of the commit map a cache cannot lose
		// (ADR 0059).
		//
		// Read here rather than passed through SetHeartbeat because that signature is exported and
		// has callers outside this repository. The runner publishes immediately before it beats, so
		// this is the current state and not a lagging copy.
		beat["checkpoint"] = a.Checkpoint()
		activity.RecordHeartbeat(ctx, beat)
	})

	// THE AUTHOR'S OWN PROGRESS, ONTO THE SAME WIRE.
	//
	// @actor.healthcheck returns whatever the author thinks describes this actor's work — for
	// the desync scanner that is hosts probed, techniques sent, signals found, claims withdrawn.
	// It used to go to `log.Printf` and nowhere else, i.e. to a file inside a container.
	//
	// Temporal already returns an activity's heartbeat details from DescribeWorkflowExecution, so
	// putting it here makes `temporal workflow describe -w <run>` show live actor progress with
	// no log shipper, no mounted volume, and no shell into the box — which is the only form that
	// works for a Machine in a Fleet.
	//
	// MERGED, NOT SUBSTITUTED: the liveness counts stay in the payload (see `last`), and the
	// author's keys are namespaced under `progress` so an author who returns `{"done": ...}`
	// cannot overwrite the field the orchestrator reads to decide whether a batch is moving.
	//
	// IT ALSO WENT ONTO THE RUN'S WORKFLOW STREAM, and that half is gone. A Workflow Stream lives
	// in the workflow's memory and dies with the workflow, so nothing published through it could
	// be read once the run closed — the console pane fed by it was empty for anybody who opened a
	// finished run. The heartbeat survives because it does not have that property: Temporal keeps
	// it with the activity, and `temporal workflow describe -w <run>` shows it with no log
	// shipper, no mounted volume and no shell into the box.
	a.SetProgress(func(v any) {
		lk2.Lock()
		beat := map[string]any{"progress": v}
		for k, val := range last {
			beat[k] = val
		}
		lk2.Unlock()
		activity.RecordHeartbeat(ctx, beat)
	})

	// THE PER-METHOD STREAM WAS WIRED HERE — `kontra.Stream(s, rec)` onto `<actor>/<method>` —
	// and the verb, the sink and the topic naming are all gone with it. A Method narrates through
	// the host's logger, which carries the run, the Worker identity and the Temporal context on
	// every line, and reports what it found through the output Dataset. Both outlive the run.

	/*
	 * ── KEEPALIVE: BEAT ON A TIMER, NOT ONLY ON A COMMIT (GitHub #19) ───────────────────────────
	 *
	 * Both beats above are EVENT-DRIVEN: `SetHeartbeat` fires when a Unit commits, `SetProgress`
	 * when the author's healthcheck reports. A Unit that legitimately runs for longer than the
	 * heartbeat bound fires neither, so the attempt is killed for silence while it is working.
	 *
	 * MEASURED: `hunt`'s sweep dispatches one Unit per host against that host's whole technique
	 * class — 3,630 techniques x 3 oracle writes = 10,890 requests, paced at 250ms, ~45 minutes of
	 * wall clock. Against a two-minute bound the first beat was due 43 minutes after the attempt had
	 * already been killed. Three runs died this way (`hunt-1789736693`, `-1789738508`,
	 * `-1789742802`) — 75 minutes, 0 rows, and no error on the parent: `/api/runs` said `running`
	 * and the Nexus operation carried `attempt: 1, lastFailure: none`. No polite pacing fits 10,890
	 * requests into two minutes, so the bound was wrong for the workload rather than the reverse.
	 *
	 * THE INTERVAL IS DERIVED, NOT NAMED. `activity.GetInfo` knows the bound Temporal is actually
	 * enforcing on THIS attempt — including a per-dispatch `heartbeat_seconds` override — so the
	 * keepalive follows it automatically and there is no second constant to keep in step. A third
	 * of the window is the usual margin: two ticks may be lost to scheduling and the attempt still
	 * lives. This is the `lambdaworker` shape the issue cites, where `ShutdownDeadlineBuffer` is
	 * derived from `WorkerStopTimeout` rather than written down.
	 *
	 * WHAT THIS GIVES UP, STATED PLAINLY. "Silence for this long means stuck, not busy" was the
	 * whole design of the bound, and a keepalive means a genuinely WEDGED Unit now beats forever
	 * instead of being killed in two minutes. That is a real loss and it is the right trade: the
	 * failure it prevents was measured and cost three runs, and the failure it admits is still
	 * caught — by `runStartToClose` (one hour), an hour later. What keeps it diagnosable is that the
	 * tick carries `alive`, a counter that rises while `done` does not: a reader can tell a Unit
	 * that is working slowly from one that has stopped, which a bare repeat of the counts could not.
	 */
	// `activity.GetInfo` PANICS outside an activity context — unlike `RecordHeartbeat`, which is a
	// tolerant no-op, which is why the beats above can be wired unconditionally. `RunBatch` is
	// called directly by this package's own tests (and by anything embedding the host), so asking
	// first is not defensive padding: without it, adding a keepalive turns every such call into a
	// panic. `host_test.go:TestRunBatchNamesTheMachineItRanOn` is what found that.
	if every := keepaliveEvery(heartbeatBoundOf(ctx)); every > 0 {
		stop := make(chan struct{})
		var alive int64
		ticker := time.NewTicker(every)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					alive++
					lk2.Lock()
					beat := map[string]any{"alive": alive}
					for k, v := range last {
						beat[k] = v
					}
					lk2.Unlock()
					// Best-effort: outside an activity, or after the attempt is gone, this is a
					// no-op. Liveness must never be the thing that fails a batch.
					activity.RecordHeartbeat(ctx, beat)
				}
			}
		}()
		defer close(stop)
	}

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

	conn, err := temporaltls.ConnectionOptions(nil)
	if err != nil {
		return fmt.Errorf("temporal TLS: %w", err)
	}
	c, err := client.Dial(client.Options{
		HostPort:          getenv("KONTRA_ADDRESS", "localhost:7233"),
		Namespace:         getenv("KONTRA_NAMESPACE", "default"),
		DataConverter:     codec.DataConverter(casStore),
		ConnectionOptions: conn,
		// The CLIENT's identity, which the server records against the calls this process MAKES —
		// the stream signals in `stream.go` above all. Field three is the ROLE and not a queue,
		// because a client polls none. See workerid.go.
		Identity: WorkerIdentity(getenv("KONTRA_WORKER_ROLE", "actor")),
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
		// STATED, not inherited. The SDK's default for this is already `<pid>@<host>@<queue>`;
		// saying it here is what lets `stream.go` put the SAME string on a record, so a stream
		// item and an `ActivityTaskStarted` event join by equality rather than by two derivations
		// agreeing. BuildID names the Bundle — versioning stays off. See workerid.go.
		Identity: WorkerIdentity(queue),
		BuildID:  BuildID(),
	})

	// A live Session's worker: same client, same activities, its OWN queue. Built here rather
	// than in sessions.go so that file stays free of the Temporal SDK and its two traps stay
	// testable without a cluster (ADR 0023 §6).
	var h *Activities
	spawn := func(sessionQueue string) (sessionWorker, error) {
		// Its own queue, so its own identity — a Session worker that reported under the shared
		// queue's name would make five live scopes read as one Worker in a poller listing.
		sw := worker.New(c, sessionQueue, worker.Options{
			Identity: WorkerIdentity(sessionQueue),
			BuildID:  BuildID(),
		})
		sw.RegisterActivityWithOptions(h.RunBatch, activity.RegisterOptions{Name: "RunBatch"})
		sw.RegisterActivityWithOptions(h.Close, activity.RegisterOptions{Name: "Close"})
		// The close is served HERE, on the Session's own queue, so only the host holding the
		// Session can answer it — that is what makes it reach the loaded instance.
		sw.RegisterActivityWithOptions(h.CloseSession, activity.RegisterOptions{Name: "CloseSession"})
		return sw, nil
	}
	h = newActivities(r.Name, r.Version, spawn, maxParallelSessions())
	h.tc = c

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
