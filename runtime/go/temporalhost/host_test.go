package temporalhost

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/runtime/go/codec"
	"github.com/medmahmoudi26/kontra-local/runtime/go/engine"
	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// TestOneInstancePerActorID pins per-process activation.
//
// A shared placement service is not what provides this — the fleet shares one Redis, so a global
// registry would have to be consulted on every activation and would still race. Within a process
// it must hold exactly — two batches for one id would
// share the resource the author opened in Load (one browser, two batches, one commit map).
func TestOneInstancePerActorID(t *testing.T) {
	s := newSessions()

	a1 := s.get("run1-n1")
	a2 := s.get("run1-n1")
	if a1 != a2 {
		t.Error("same actor id must reuse the live instance")
	}

	// Different nodes of one run are different actors: two ids, two resources.
	if s.get("run1-n2") == a1 {
		t.Error("a different actor id must get its own instance")
	}
	if a1.ID() != "run1-n1" {
		t.Errorf("instance carries the wrong id: %q", a1.ID())
	}
}

// TestDropForcesAFreshInstance covers the retry path: a batch that failed dropped its session, so
// the next attempt rebuilds from scratch rather than reusing a half-torn resource.
func TestDropForcesAFreshInstance(t *testing.T) {
	s := newSessions()
	first := s.get("run1-n1")
	s.drop("run1-n1")
	if second := s.get("run1-n1"); second == first {
		t.Error("after drop, the id must build a NEW instance")
	}
}

// TestLockIsPerActorID: the mutex must be per id, or one slow batch would serialize every other
// actor in the process and the worker's concurrency cap would mean nothing.
func TestLockIsPerActorID(t *testing.T) {
	s := newSessions()
	if s.lockFor("a") != s.lockFor("a") {
		t.Error("one id must always get the same lock")
	}
	if s.lockFor("a") == s.lockFor("b") {
		t.Error("different ids must not share a lock")
	}

	// And it actually serializes: two goroutines on one id never overlap.
	lk := s.lockFor("a")
	var mu sync.Mutex
	inFlight, maxSeen := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk.Lock()
			defer lk.Unlock()
			mu.Lock()
			inFlight++
			if inFlight > maxSeen {
				maxSeen = inFlight
			}
			mu.Unlock()
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxSeen > 1 {
		t.Errorf("two batches ran concurrently for one actor id (max in flight %d)", maxSeen)
	}
}

// TestRunBatchRequiresAnActorID: without an id there is no instance to pin and no state to
// resume, so a retry would silently start over instead of skipping committed units.
func TestRunBatchRequiresAnActorID(t *testing.T) {
	h := &Activities{s: newSessions()}
	if _, err := h.RunBatch(context.Background(), engine.RunBatchReq{}); err == nil {
		t.Fatal("RunBatch with no actor_id must fail loud")
	}
}

// memState is an engine.StateStore that holds nothing and errors on nothing — enough for a test
// about the ENVELOPE, which never reads the commit map.
type memState struct{}

func (memState) Contains(context.Context, string) (bool, error)               { return false, nil }
func (memState) Get(context.Context, string, any) error                       { return nil }
func (memState) SetWithTTL(context.Context, string, any, time.Duration) error { return nil }
func (memState) Remove(context.Context, string) error                         { return nil }
func (memState) Save(context.Context) error                                   { return nil }
func (memState) Touch(context.Context) error                                  { return nil }
func (memState) Drop(context.Context) error                                   { return nil }

// TestRunBatchNamesTheMachineItRanOn is the callee half of "provenance travels with the Batch".
//
// The envelope is the ONLY place this can come from. The handler copies it onto the result ref's
// meta, and the handler cannot answer it itself: its workflow polls the actor's shared queue,
// which every Machine's handler polls, while RunBatch is served here — so the handler's own
// hostname would name a Machine chosen by Temporal's dispatch rather than the one that ran the
// Method. Peer of `out["machine"] = MACHINE` in internals/temporal/host.py.
func TestRunBatchNamesTheMachineItRanOn(t *testing.T) {
	engine.Configure(&core.Registry{Name: "t"}) // a load-only Actor: identity passthrough
	h := newActivities("t", "1", nil, 1)
	// Seed the activation table over an in-memory store. `sessions.get` would otherwise build
	// one over statekv, which dials Redis and spends five seconds failing to reach it — this
	// test has nothing to say about state, and a test that needs a cluster is not one.
	h.s.live["run1-n1"] = engine.New("run1-n1", memState{})
	// An empty Batch: enough to exercise the envelope, and it commits no Unit, so nothing
	// reaches activity.RecordHeartbeat outside an activity context.
	resp, err := h.RunBatch(context.Background(), engine.RunBatchReq{ActorID: "run1-n1"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname on this box; there is nothing to compare against")
	}
	if resp.Machine != host {
		t.Errorf("RunBatch reported machine %q, want this host %q", resp.Machine, host)
	}
	// The HOST portion only. A restarted Worker changes pid, and a Machine that reads as a new
	// Machine after every restart is worse than no Machine at all.
	if strings.Contains(resp.Machine, "@") {
		t.Errorf("machine %q carries the pid/queue; record the host portion alone", resp.Machine)
	}
}

// TestCloseIsIdempotent: the workflow schedules Close on every exit path, including paths where
// this process never held the session (a retry that landed on another worker). It must not error.
func TestCloseIsIdempotent(t *testing.T) {
	h := &Activities{s: newSessions()}
	resp, err := h.Close(context.Background(), map[string]any{"actor_id": "never-seen"})
	if err != nil {
		t.Fatalf("Close on an unknown id: %v", err)
	}
	if !resp.Closed {
		t.Error("Close must acknowledge even when there was nothing to close")
	}
}

// TestServeInstallsTheClaimCheckCodec pins the wiring, not the wire format.
//
// The format is already pinned by the cross-language corpus (runtime/go/codec's conformance test).
// What was MISSING was anything asserting the codec is actually installed on the client: the Go
// host dialled Temporal with no DataConverter, so every batch the handler encoded as
// `binary/claim-check-v1` — anything over 128 KiB — would fail to decode, retried to exhaustion.
// Small batches passed throughout, which is exactly why it went unnoticed on the Python side
// until a 200 KB dispatch was tried.
func TestServeInstallsTheClaimCheckCodec(t *testing.T) {
	src, err := os.ReadFile("host.go")
	if err != nil {
		t.Fatalf("read host.go: %v", err)
	}
	if !strings.Contains(string(src), "DataConverter:") {
		t.Error("Serve must dial Temporal with a DataConverter carrying the claim-check codec")
	}
	if !strings.Contains(string(src), "codec.DataConverter") {
		t.Error("the DataConverter must come from runtime/go/codec, not a hand-rolled one")
	}
}

// TestPassthroughConverterWithoutAStore: a local run with no KONTRA_S3_ENDPOINT must still get a
// usable converter. The handler is passthrough under the same condition, so neither side offloads.
func TestPassthroughConverterWithoutAStore(t *testing.T) {
	t.Setenv("KONTRA_S3_ENDPOINT", "")
	store, err := codec.StoreFromEnv(context.Background())
	if err != nil {
		t.Fatalf("StoreFromEnv: %v", err)
	}
	if store != nil {
		t.Fatal("no endpoint must mean no store (passthrough), never a default endpoint")
	}
	if dc := codec.DataConverter(store); dc == nil {
		t.Error("a passthrough converter must still be built")
	}
}
