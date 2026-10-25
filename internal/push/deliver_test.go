package push

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
)

var activityOpts = activity.RegisterOptions{Name: lifecycle.ActPushWebhook}

type (
	activityInfo  = activity.Info
	encodedValues = converter.EncodedValues
)

// runPush runs one push_webhook in the background and returns its result channel.
func runPush(ctx context.Context, d *Deliverer, id uuid.UUID, cid string) <-chan pushOut {
	ch := make(chan pushOut, 1)
	go func() {
		r, err := d.PushWebhook(ctx, pushInput(id.String(), cid))
		ch <- pushOut{r, err}
	}()
	return ch
}

type pushOut struct {
	res lifecycle.PushResult
	err error
}

func waitOut(t *testing.T, ch <-chan pushOut, within time.Duration) pushOut {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(within):
		t.Fatalf("push_webhook did not end within %s", within)
	}
	return pushOut{}
}

// bodies returns the request bodies in arrival order.
func bodies(ds []delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.body
	}
	return out
}

// TestDeliveryInSeqOrder: each row's snapshot is delivered in seq order, consecutive artifact rows read
// together coalesce into the snapshot at the last one, every status change and the terminal snapshot are
// delivered, delivered_seq follows, and the activity ends after the terminal snapshot (spec §10.2).
func TestDeliveryInSeqOrder(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))

	seqs := []int64{1}
	seqs = append(seqs, e.status(id, store.StateWorking))    // 2
	e.artifact(id, "a1", "one", false)                       // 3 coalesced
	e.artifact(id, "a1", "two", true)                        // 4 coalesced
	seqs = append(seqs, e.artifact(id, "a1", "three", true)) // 5
	seqs = append(seqs, e.status(id, store.StateInputRequired))
	seqs = append(seqs, e.userMessage(id, "blue"))
	seqs = append(seqs, e.metadata(id, map[string]any{"kontra.escrow.state": "held"}))
	seqs = append(seqs, e.status(id, store.StateWorking))
	seqs = append(seqs, e.artifact(id, "a2", "lone", false))
	term := e.status(id, store.StateCompleted)
	seqs = append(seqs, term)

	d := NewDeliverer(e.log, private, fast())
	o := waitOut(t, runPush(context.Background(), d, id, cid), 10*time.Second)
	if o.err != nil || o.res != (lifecycle.PushResult{DeliveredSeq: term, Stopped: StoppedTerminal}) {
		t.Fatalf("push_webhook: %+v %v", o.res, o.err)
	}
	got := bodies(r.requests())
	if len(got) != len(seqs) {
		t.Fatalf("%d deliveries, want %d (seqs %v)", len(got), len(seqs), seqs)
	}
	for i, seq := range seqs {
		if want := e.expectedBody(id, seq); got[i] != want {
			t.Errorf("delivery %d is not the snapshot at seq %d:\n got %s\nwant %s", i, seq, got[i], want)
		}
	}
	if seq, failed := e.deliveredSeq(id, cid); seq != term || failed {
		t.Errorf("delivered_seq %d failed %v, want %d", seq, failed, term)
	}
	// Run again (a restart after the terminal snapshot): nothing more is sent.
	o = waitOut(t, runPush(context.Background(), d, id, cid), 5*time.Second)
	if o.err != nil || o.res.Stopped != StoppedTerminal || len(r.requests()) != len(seqs) {
		t.Errorf("a second run: %+v %v, %d requests", o.res, o.err, len(r.requests()))
	}
	// Rows after the terminal one (an accepted review message, metadata) are never delivered.
	e.metadata(id, map[string]any{"kontra.escrow.state": "released"})
	o = waitOut(t, runPush(context.Background(), d, id, cid), 5*time.Second)
	if o.res.Stopped != StoppedTerminal || len(r.requests()) != len(seqs) {
		t.Errorf("after a metadata row past the terminal one: %+v, %d requests", o.res, len(r.requests()))
	}
}

// TestDeliveryTailsTheLog: an activity started on a running task waits for new rows (woken or polled) and
// delivers them as they come, in order, until the terminal snapshot.
func TestDeliveryTailsTheLog(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	d := NewDeliverer(e.log, private, fast())
	out := runPush(context.Background(), d, id, cid)
	var seqs []int64
	seqs = append(seqs, 1)
	for _, s := range []store.State{store.StateWorking, store.StateInputRequired, store.StateWorking, store.StateFailed} {
		time.Sleep(120 * time.Millisecond)
		seqs = append(seqs, e.status(id, s))
	}
	o := waitOut(t, out, 10*time.Second)
	if o.err != nil || o.res.Stopped != StoppedTerminal {
		t.Fatalf("%+v %v", o.res, o.err)
	}
	got := bodies(r.delivered())
	if len(got) != len(seqs) {
		t.Fatalf("%d deliveries, want %d", len(got), len(seqs))
	}
	for i, seq := range seqs {
		if got[i] != e.expectedBody(id, seq) {
			t.Errorf("delivery %d is not the snapshot at seq %d", i, seq)
		}
	}
}

// TestDeliveryRecoversInOrder is push_delivery's shape at test speed: the webhook is down while the task
// runs and ends, then comes back; every status change and the terminal snapshot arrive in seq order, a
// config created after the task started is delivered too, and delivered_seq never moves backwards.
// (TestPushDeliveryThroughTheLifecycle runs it with the spec's timing: 2 minutes down, 1 s doubling.)
func TestDeliveryRecoversInOrder(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	var (
		mu   sync.Mutex
		down = true
	)
	r := newReceiver(t, func(int) int {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	id := e.newTask(buyerA)
	c1, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/one")))
	d := NewDeliverer(e.log, private, fast())
	out1 := runPush(context.Background(), d, id, c1)
	all := []int64{1, e.status(id, store.StateWorking)}
	// A config created after the task started starts at the task's current state.
	late, err := cs.Save(context.Background(), tid(id), pushConfig(r.url("/two")))
	if err != nil {
		t.Fatal(err)
	}
	out2 := runPush(context.Background(), d, id, late.ID)
	all = append(all, e.artifact(id, "a", "x", false), e.status(id, store.StateInputRequired),
		e.status(id, store.StateWorking), e.status(id, store.StateCompleted))
	// delivered_seq is polled while the webhook is down and after: it never moves backwards.
	stop := make(chan struct{})
	backwards := make(chan string, 1)
	go func() {
		var last1, last2 int64
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			s1, _ := e.deliveredSeq(id, c1)
			s2, _ := e.deliveredSeq(id, late.ID)
			if s1 < last1 || s2 < last2 {
				select {
				case backwards <- fmt.Sprintf("%d after %d / %d after %d", s1, last1, s2, last2):
				default:
				}
			}
			last1, last2 = s1, s2
		}
	}()
	time.Sleep(time.Second)
	if n := len(r.delivered()); n != 0 {
		t.Fatalf("%d deliveries while down", n)
	}
	if n := len(r.requests()); n < 6 {
		t.Fatalf("only %d attempts in 1 s with 10 ms doubling to 80 ms: the deliveries are not retried", n)
	}
	mu.Lock()
	down = false
	mu.Unlock()
	o1, o2 := waitOut(t, out1, 10*time.Second), waitOut(t, out2, 10*time.Second)
	close(stop)
	select {
	case s := <-backwards:
		t.Errorf("delivered_seq moved backwards: %s", s)
	default:
	}
	term := all[len(all)-1]
	for _, o := range []pushOut{o1, o2} {
		if o.err != nil || o.res != (lifecycle.PushResult{DeliveredSeq: term, Stopped: StoppedTerminal}) {
			t.Fatalf("push_webhook: %+v %v", o.res, o.err)
		}
	}
	var one, two []string
	for _, dl := range r.delivered() {
		if dl.path == "/one" {
			one = append(one, dl.body)
		} else {
			two = append(two, dl.body)
		}
	}
	if len(one) != len(all) {
		t.Fatalf("config 1 got %d deliveries, want %d", len(one), len(all))
	}
	for i, seq := range all {
		if one[i] != e.expectedBody(id, seq) {
			t.Errorf("config 1 delivery %d is not the snapshot at seq %d", i, seq)
		}
	}
	// The late config was set at seq 2 (working): it gets that state, then every change.
	if want := all[1:]; len(two) != len(want) {
		t.Fatalf("the late config got %d deliveries, want %d", len(two), len(want))
	}
	for i, seq := range all[1:] {
		if two[i] != e.expectedBody(id, seq) {
			t.Errorf("late config delivery %d is not the snapshot at seq %d", i, seq)
		}
	}
	for _, dl := range r.requests() {
		if !strings.Contains(dl.body, `"id":"`+id.String()+`"`) {
			t.Fatalf("a delivery for another task: %s", dl.body)
		}
	}
}

// fakeClock is a clock whose sleeps return at once and advance it.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	return ctx.Err()
}

// TestRetryBackoffAndBudget: a non-2xx is retried 1 s doubling to 5 min for at most 24 h from the first
// failure of the seq; then the config is marked failed, the activity ends, and the task is unaffected
// (spec §10.2). The clock is fake; the requests are real.
func TestRetryBackoffAndBudget(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	clk := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	var attemptsAt []time.Time // each attempt on the fake clock
	r := newReceiver(t, func(int) int {
		attemptsAt = append(attemptsAt, clk.Now()) // under the receiver's lock
		return http.StatusInternalServerError
	})
	id := e.newTask(buyerA)
	e.status(id, store.StateWorking)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	before, err := e.log.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	lb := &logBuffer{}
	o := fast()
	o.RetryMin, o.RetryMax, o.Budget = 0, 0, 0 // the spec's defaults
	o.Now, o.Sleep, o.Logger = clk.Now, clk.Sleep, newLogger(lb)
	d := NewDeliverer(e.log, private, o)
	start := clk.Now()
	out := waitOut(t, runPush(context.Background(), d, id, cid), 60*time.Second)
	if out.err != nil || out.res.Stopped != StoppedFailed {
		t.Fatalf("push_webhook: %+v %v", out.res, out.err)
	}
	if elapsed := clk.Now().Sub(start); elapsed != RetryBudget {
		t.Errorf("gave up after %s of retries, want exactly %s", elapsed, RetryBudget)
	}
	// The waits between attempts: 1 s doubling to 5 min, the last one cut to the budget.
	r.mu.Lock()
	var waits []time.Duration
	for i := 1; i < len(attemptsAt); i++ {
		waits = append(waits, attemptsAt[i].Sub(attemptsAt[i-1]))
	}
	r.mu.Unlock()
	for _, s := range clk.slept {
		if s > beatEvery {
			t.Fatalf("one sleep of %s: waits must be cut in steps of at most %s to heartbeat", s, beatEvery)
		}
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256}
	for i, w := range want {
		if i >= len(waits) || waits[i] != w*time.Second {
			t.Fatalf("waits %v, want 1 s doubling: %v s", waits[:min(len(waits), 12)], want)
		}
	}
	for i := len(want); i < len(waits)-1; i++ {
		if waits[i] != RetryMax {
			t.Fatalf("wait %d is %s, want the 5 min cap", i, waits[i])
		}
	}
	if last := waits[len(waits)-1]; last <= 0 || last > RetryMax {
		t.Errorf("the last wait is %s: cut to what is left of the budget", last)
	}
	attempts := len(r.requests())
	if wantN := len(waits) + 1; attempts != wantN {
		t.Errorf("%d attempts for %d waits", attempts, len(waits))
	}
	if _, failed := e.deliveredSeq(id, cid); !failed {
		t.Error("the config is not marked failed")
	}
	after, _ := e.log.Load(context.Background(), id)
	if after.Version != before.Version || after.State != before.State {
		t.Errorf("the task changed: version %d→%d, state %s→%s", before.Version, after.Version, before.State, after.State)
	}
	if live, _ := cs.LiveConfigIDs(context.Background(), id); len(live) != 0 {
		t.Errorf("a failed config is still live: %v", live)
	}
	// A restarted activity of a failed config ends at once.
	again := waitOut(t, runPush(context.Background(), d, id, cid), 5*time.Second)
	if again.res.Stopped != StoppedFailed || len(r.requests()) != attempts {
		t.Errorf("a failed config's second run: %+v, %d requests", again.res, len(r.requests()))
	}
	if s := containsAny(lb.String(), secretToken, secretCreds); s != "" {
		t.Errorf("the log carries %q", s)
	}
	if !strings.Contains(lb.String(), "status=500") || !strings.Contains(lb.String(), "marked failed") {
		t.Errorf("the log does not record the failures:\n%s", lb.String()[:min(len(lb.String()), 600)])
	}
}

// TestRedirectIsAFailedDelivery: a redirecting URL is accepted when set, its 3xx is a failed delivery
// (retried, then the config fails), and the redirect target receives no connection (spec §10.2, D60).
func TestRedirectIsAFailedDelivery(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	target := newReceiver(t, nil)
	redir := newReceiver(t, func(int) int { return http.StatusFound })
	redir.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		redir.serve(&discard{}, req)
		http.Redirect(w, req, target.url("/target"), http.StatusFound)
	})
	id := e.newTask(buyerA)
	s, err := cs.Save(context.Background(), tid(id), pushConfig(redir.url("/redirect")))
	if err != nil {
		t.Fatalf("a redirecting URL is accepted when set: %v", err)
	}
	o := fast()
	o.Budget = 300 * time.Millisecond
	d := NewDeliverer(e.log, private, o)
	out := waitOut(t, runPush(context.Background(), d, id, s.ID), 10*time.Second)
	if out.err != nil || out.res.Stopped != StoppedFailed {
		t.Fatalf("push_webhook: %+v %v", out.res, out.err)
	}
	if n := len(redir.requests()); n < 2 {
		t.Errorf("the 3xx was not retried: %d attempts", n)
	}
	if n := target.conns.Load(); n != 0 {
		t.Fatalf("the redirect target received %d connections", n)
	}
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

// TestHungReceiverTimesOut: a receiver that does not answer within the timeout is a failed delivery, and
// the next attempt is delivered.
func TestHungReceiverTimesOut(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	r.hold = 3 * time.Second
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	e.status(id, store.StateRejected)
	o := fast()
	o.Timeout = 200 * time.Millisecond
	d := NewDeliverer(e.log, private, o)
	out := runPush(context.Background(), d, id, cid)
	time.Sleep(500 * time.Millisecond)
	r.mu.Lock()
	r.hold = 0
	r.mu.Unlock()
	res := waitOut(t, out, 10*time.Second)
	if res.err != nil || res.res.Stopped != StoppedTerminal {
		t.Fatalf("%+v %v", res.res, res.err)
	}
	if n := len(r.requests()); n < 3 {
		t.Errorf("%d requests: the timed-out attempts were not retried", n)
	}
}

// TestDeletedConfigEndsTheActivity: an idle activity ends when its config is deleted; one whose config is
// already gone ends at once.
func TestDeletedConfigEndsTheActivity(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	d := NewDeliverer(e.log, private, fast())
	out := runPush(context.Background(), d, id, cid)
	time.Sleep(200 * time.Millisecond) // the task row is delivered; the activity idles
	if err := cs.Delete(context.Background(), tid(id), cid); err != nil {
		t.Fatal(err)
	}
	o := waitOut(t, out, 5*time.Second)
	if o.err != nil || o.res != (lifecycle.PushResult{DeliveredSeq: 1, Stopped: StoppedDeleted}) {
		t.Fatalf("%+v %v", o.res, o.err)
	}
	o = waitOut(t, runPush(context.Background(), d, id, cid), 5*time.Second)
	if o.err != nil || o.res.Stopped != StoppedDeleted {
		t.Fatalf("a run for a deleted config: %+v %v", o.res, o.err)
	}
	// A config deleted while its delivery fails stops the retries.
	failing := newReceiver(t, func(int) int { return http.StatusBadGateway })
	c2, _ := e.saveInline(cs, id, buyerA, pushConfig(failing.url("/hook")))
	out = runPush(context.Background(), d, id, c2)
	time.Sleep(200 * time.Millisecond)
	_ = cs.Delete(context.Background(), tid(id), c2)
	o = waitOut(t, out, 5*time.Second)
	if o.err != nil || o.res.Stopped != StoppedDeleted {
		t.Fatalf("a config deleted while failing: %+v %v", o.res, o.err)
	}
}

// TestCanceledActivityStops: the activity's context ending (a closed workflow's next heartbeat) ends it
// with the context's error, while idle and while retrying.
func TestCanceledActivityStops(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, func(int) int { return http.StatusServiceUnavailable })
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	ctx, cancel := context.WithCancel(context.Background())
	out := runPush(ctx, NewDeliverer(e.log, private, fast()), id, cid)
	time.Sleep(200 * time.Millisecond)
	cancel()
	if o := waitOut(t, out, 5*time.Second); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("%+v %v", o.res, o.err)
	}
}

// TestResumeFromHeartbeatOrDeliveredSeq: a restarted activity resumes from the larger of its heartbeat
// details and push_configs.delivered_seq (spec §10.2), and keeps a failing seq's budget; it heartbeats its
// progress as it delivers.
func TestResumeFromHeartbeatOrDeliveredSeq(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	for _, s := range []store.State{store.StateWorking, store.StateInputRequired, store.StateWorking, store.StateInputRequired, store.StateWorking} {
		e.status(id, s) // 2..6
	}
	term := e.status(id, store.StateCompleted) // 7
	d := NewDeliverer(e.log, private, fast())

	run := func(hb *progress, delivered int64) ([]string, []progress) {
		t.Helper()
		e.exec(`update kontra.push_configs set delivered_seq = $3 where task_id = $1 and id = $2`, id, cid, delivered)
		start := len(r.requests())
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestActivityEnvironment()
		env.RegisterActivityWithOptions(d.PushWebhook, activityOpts)
		if hb != nil {
			env.SetHeartbeatDetails(*hb)
		}
		var (
			mu    sync.Mutex
			beats []progress
		)
		env.SetOnActivityHeartbeatListener(func(_ *activityInfo, details encodedValues) {
			var p progress
			if details.HasValues() && details.Get(&p) == nil {
				mu.Lock()
				beats = append(beats, p)
				mu.Unlock()
			}
		})
		v, err := env.ExecuteActivity(lifecycle.ActPushWebhook, pushInput(id.String(), cid))
		if err != nil {
			t.Fatal(err)
		}
		var res lifecycle.PushResult
		if err := v.Get(&res); err != nil || res.Stopped != StoppedTerminal || res.DeliveredSeq != term {
			t.Fatalf("result %+v %v", res, err)
		}
		return bodies(r.requests()[start:]), beats
	}

	// Heartbeat details ahead of delivered_seq (the row write was lost): resume after seq 5.
	got, beats := run(&progress{Seq: 5}, 2)
	if len(got) != 2 || got[0] != e.expectedBody(id, 6) || got[1] != e.expectedBody(id, term) {
		t.Fatalf("resumed from the heartbeat: %d deliveries", len(got))
	}
	// The SDK sends the first heartbeat at once (later ones are batched): it carries the resumed position.
	if len(beats) == 0 || beats[0].Seq != 5 {
		t.Errorf("heartbeats %+v: the first must carry the resumed position 5", beats)
	}
	// delivered_seq ahead of the heartbeat details (the heartbeat was throttled away): resume after seq 4.
	got, _ = run(&progress{Seq: 2}, 4)
	if len(got) != 3 || got[0] != e.expectedBody(id, 5) {
		t.Fatalf("resumed from delivered_seq: %d deliveries", len(got))
	}
	// No heartbeat details: delivered_seq alone.
	got, _ = run(nil, 6)
	if len(got) != 1 || got[0] != e.expectedBody(id, term) {
		t.Fatalf("resumed without details: %d deliveries", len(got))
	}
	if seq, _ := e.deliveredSeq(id, cid); seq != term {
		t.Errorf("delivered_seq %d", seq)
	}
}

// TestRestartKeepsTheSeqBudget: a seq that was failing before a restart keeps its first failure, so its
// 24 h budget is not renewed by a restart.
func TestRestartKeepsTheSeqBudget(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, func(int) int { return http.StatusInternalServerError })
	id := e.newTask(buyerA)
	e.status(id, store.StateWorking) // 2
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	e.exec(`update kontra.push_configs set delivered_seq = 1 where task_id = $1`, id)
	clk := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	o := fast()
	o.RetryMin, o.RetryMax, o.Budget = 0, 0, 0
	o.Now, o.Sleep = clk.Now, clk.Sleep
	d := NewDeliverer(e.log, private, o)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(d.PushWebhook, activityOpts)
	// Seq 2 first failed 23 h 59 min ago (fake time), backing off at the cap.
	env.SetHeartbeatDetails(progress{Seq: 1, FailingSeq: 2, FirstFailure: clk.Now().Add(-RetryBudget + time.Minute), Wait: RetryMax})
	v, err := env.ExecuteActivity(lifecycle.ActPushWebhook, pushInput(id.String(), cid))
	if err != nil {
		t.Fatal(err)
	}
	var res lifecycle.PushResult
	_ = v.Get(&res)
	if res.Stopped != StoppedFailed {
		t.Fatalf("%+v", res)
	}
	if n := len(r.requests()); n != 2 {
		t.Errorf("%d attempts in the last minute of the budget, want 2 (now, then at the budget's end)", n)
	}
}

// TestDeliveredSeqNeverMovesBackwards: the write after a delivery keeps the larger seq.
func TestDeliveredSeqNeverMovesBackwards(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig("http://127.0.0.1:9/hook"))
	d := NewDeliverer(e.log, private, fast())
	ctx := context.Background()
	for _, s := range []int64{3, 7, 5, 2, 7} {
		if err := d.markDelivered(ctx, id, cid, s); err != nil {
			t.Fatal(err)
		}
	}
	if seq, _ := e.deliveredSeq(id, cid); seq != 7 {
		t.Fatalf("delivered_seq %d, want 7", seq)
	}
}

// TestRateCapPerHost: deliveries to one destination host are spaced 1/(10/N) s apart on a replica, N the
// live replicas in gate_replicas, draining ones included, at least 1 (D72).
func TestRateCapPerHost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := NewDeliverer(e.log, private, DelivererOptions{})
	if n, err := d.liveReplicas(ctx); err != nil || n != 0 {
		t.Fatalf("no replicas: %d %v", n, err)
	}
	if n := d.liveReplicaCount(ctx); n != 1 {
		t.Fatalf("N with no replica row is %d, want at least 1", n)
	}
	e.exec(`insert into kontra.gate_replicas (replica_id, version) values ('r1', 'test'), ('r2', 'test')`)
	e.exec(`insert into kontra.gate_replicas (replica_id, version, draining_at) values ('r3', 'test', now())`)
	e.exec(`insert into kontra.gate_replicas (replica_id, version, last_seen_at) values ('dead', 'test', now() - interval '1 minute')`)
	if n, err := d.liveReplicas(ctx); err != nil || n != 3 {
		t.Fatalf("live replicas %d %v, want 3 (the draining one counts, the silent one does not)", n, err)
	}
	// The cached N is refreshed only every 10 s.
	if n := d.liveReplicaCount(ctx); n != 1 {
		t.Errorf("N refreshed early: %d", n)
	}
	d.mu.Lock()
	d.replicasAt = d.replicasAt.Add(-ReplicaRefresh)
	d.mu.Unlock()
	if n := d.liveReplicaCount(ctx); n != 3 {
		t.Errorf("N after the refresh: %d", n)
	}

	// Two configs to one host, N = 2: the limiter gives each delivery to that host a slot 200 ms after the
	// previous one's, the first at the earliest when the push_webhooks start. A delivery is sent at its slot or
	// later (after a read of its row), so the i-th to arrive arrives at least i·200 ms after the start. Gaps
	// between two arrivals are not bounded: one delivery's lag after its slot can shrink the next gap.
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	for range 3 {
		e.status(id, store.StateWorking)
	}
	e.status(id, store.StateCompleted) // 5 rows
	c1, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/a")))
	c2, _ := e.saveInline(cs, id, buyerA, &a2a.PushConfig{URL: r.url("/b")})
	e.exec(`update kontra.push_configs set delivered_seq = 0 where task_id = $1`, id)
	o := fast()
	o.Replicas = func(context.Context) (int, error) { return 2, nil }
	d2 := NewDeliverer(e.log, private, o)
	begin := time.Now()
	out1, out2 := runPush(ctx, d2, id, c1), runPush(ctx, d2, id, c2)
	waitOut(t, out1, 20*time.Second)
	waitOut(t, out2, 20*time.Second)
	got := r.requests()
	if len(got) != 10 {
		t.Fatalf("%d deliveries, want 10", len(got))
	}
	slices.SortFunc(got, func(a, b delivery) int { return a.at.Compare(b.at) })
	for i, g := range got {
		if at, want := g.at.Sub(begin), time.Duration(i)*200*time.Millisecond; at < want {
			t.Fatalf("delivery %d to one host %s after the start, want ≥ %s with N = 2 (5 a second)", i, at, want)
		}
	}
	// Another host is not held back by this one's slots.
	other := newReceiver(t, nil)
	id2 := e.newTask(buyerA)
	e.status(id2, store.StateCompleted)
	c3, _ := e.saveInline(cs, id2, buyerA, &a2a.PushConfig{URL: strings.Replace(other.url("/c"), "127.0.0.1", "localhost", 1)})
	start := time.Now()
	waitOut(t, runPush(ctx, d2, id2, c3), 5*time.Second)
	if time.Since(start) > time.Second {
		t.Errorf("a delivery to another host waited %s", time.Since(start))
	}
}

// TestCredentialsNeverLogged: across failing deliveries (refused addresses, refused connections, errors,
// timeouts, a redirect) and refused or accepted Saves, no log line, no returned error, and no
// operator-readable column carries the config's token, credentials, or the secret in its URL.
func TestCredentialsNeverLogged(t *testing.T) {
	e := newEnv(t)
	lb := &logBuffer{}
	log := newLogger(lb)
	sig := &signals{}
	res := fakeResolver{"private.example": {"10.0.0.1"}, "hooks.example": {"93.184.215.14"}}
	strict := ssrf.Policy{Resolver: res}
	cs := NewConfigStore(e.pool, sig, strict, log)
	csPriv := NewConfigStore(e.pool, sig, private, log)
	ctx := context.Background()
	id := e.newTask(buyerA)
	secretURL := func(base string) string { return base + "/" + secretPath + "?key=" + secretPath }

	// Refused Saves name the rule, not the URL or the credentials.
	var errs []error
	for _, u := range []string{secretURL("https://private.example"), secretURL("http://hooks.example"), secretURL("https://10.0.0.1"), "::" + secretPath} {
		_, err := cs.Save(ctx, tid(id), pushConfig(u))
		errs = append(errs, err)
	}
	bad := pushConfig(secretURL("https://hooks.example"))
	bad.Token = secretToken + "\r\n"
	_, err := cs.Save(ctx, tid(id), bad)
	errs = append(errs, err)
	for _, err := range errs {
		if err == nil {
			t.Fatal("a refused Save succeeded")
		}
	}

	// Deliveries that fail every way there is.
	r500 := newReceiver(t, func(int) int { return http.StatusInternalServerError })
	hung := newReceiver(t, nil)
	hung.hold = 2 * time.Second
	redir := newReceiver(t, func(int) int { return http.StatusMovedPermanently })
	e.status(id, store.StateWorking)
	var cids []string
	s, err := cs.Save(ctx, tid(id), pushConfig(secretURL("https://hooks.example"))) // public, not listening: refused at dial on a real network or unreachable
	if err != nil {
		t.Fatal(err)
	}
	cids = append(cids, s.ID)
	for _, u := range []string{secretURL(r500.srv.URL), secretURL(hung.srv.URL), secretURL(redir.srv.URL), secretURL("http://127.0.0.1:1")} {
		s, err := csPriv.Save(ctx, tid(id), pushConfig(u))
		if err != nil {
			t.Fatal(err)
		}
		cids = append(cids, s.ID)
	}
	// The private name: refused at dial time (DNS rebinding), with the strict policy's dialer.
	e.exec(`update kontra.push_configs set config = jsonb_set(config, '{url}', to_jsonb($2::text)) where task_id = $1 and id = $3`,
		id, secretURL("https://private.example"), cids[0])
	e.status(id, store.StateCompleted) // set while the task ran: each config has its terminal snapshot to deliver
	o := fast()
	o.Budget, o.Timeout, o.Logger = 400*time.Millisecond, 100*time.Millisecond, log
	dStrict := NewDeliverer(e.log, strict, o)
	dPriv := NewDeliverer(e.log, private, o)
	var outs []<-chan pushOut
	outs = append(outs, runPush(ctx, dStrict, id, cids[0]))
	for _, c := range cids[1:] {
		outs = append(outs, runPush(ctx, dPriv, id, c))
	}
	for _, out := range outs {
		res := waitOut(t, out, 20*time.Second)
		if res.err != nil {
			errs = append(errs, res.err)
		}
		if res.res.Stopped != StoppedFailed {
			t.Errorf("a failing config ended %+v %v", res.res, res.err)
		}
	}
	// A canceled activity's error, and the bad-input errors, carry nothing either.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = dPriv.PushWebhook(cctx, pushInput(id.String(), cids[1]))
	errs = append(errs, err)

	logs := lb.String()
	if !strings.Contains(logs, "delivery failed") {
		t.Fatalf("the failures were not logged at all:\n%s", logs)
	}
	for _, needle := range []string{secretToken, secretCreds, secretPath} {
		if strings.Contains(logs, needle) {
			t.Errorf("the log carries %q:\n%s", needle, logs)
		}
		for _, err := range errs {
			if err != nil && strings.Contains(err.Error(), needle) {
				t.Errorf("an error carries %q: %v", needle, err)
			}
		}
	}
	// Operator-readable columns: every table but push_configs.config (spec §5.3, §13.5).
	for _, table := range []string{"tasks", "task_events", "work_items", "attempts", "audit", "gate_replicas", "services", "api_keys"} {
		rows, err := e.pool.Query(ctx, `select t::text from kontra.`+table+` t`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			if n := containsAny(s, secretToken, secretCreds, secretPath); n != "" {
				t.Errorf("kontra.%s carries %q", table, n)
			}
		}
		rows.Close()
	}
	var others string
	if err := e.pool.QueryRow(ctx, `select coalesce(string_agg(concat_ws(',', task_id, id, delivered_seq, failed_at, created_at), ';'), '')
		from kontra.push_configs`).Scan(&others); err != nil {
		t.Fatal(err)
	}
	if n := containsAny(others, secretToken, secretCreds, secretPath); n != "" {
		t.Errorf("push_configs columns other than config carry %q", n)
	}
	for _, s := range sig.list() {
		if n := containsAny(s.ConfigID, secretToken, secretCreds, secretPath); n != "" {
			t.Errorf("kontra.push carries %q", n)
		}
	}
}

// TestUndeliverableConfigGivesUp: a stored config that does not decode, or a log row that cannot be folded
// (a bucket-only part; M1 has no bucket), would fail every retry, and the workflow waits for its pushes
// (D78): the config is marked failed and the activity ends instead of erroring forever.
func TestUndeliverableConfigGivesUp(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	d := NewDeliverer(e.log, private, fast())
	ctx := context.Background()

	id := e.newTask(buyerA)
	bad, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	e.exec(`update kontra.push_configs set config = '{"url": 5}' where task_id = $1 and id = $2`, id, bad)
	o := waitOut(t, runPush(ctx, d, id, bad), 5*time.Second)
	if o.err != nil || o.res.Stopped != StoppedFailed {
		t.Fatalf("a config that does not decode: %+v %v", o.res, o.err)
	}
	if _, failed := e.deliveredSeq(id, bad); !failed {
		t.Error("the undecodable config is not marked failed")
	}

	// A row only in the bucket after seq 2: delivered up to 2, then given up.
	id2 := e.newTask(buyerA)
	e.status(id2, store.StateWorking)
	c2, _ := e.saveInline(cs, id2, buyerA, pushConfig(r.url("/hook2")))
	e.exec(`update kontra.push_configs set delivered_seq = 0 where task_id = $1`, id2)
	e.exec(`update kontra.tasks set version = 3 where id = $1`, id2)
	e.exec(`insert into kontra.task_events (task_id, seq, kind, payload_ref) values ($1, 3, 'artifact', 'blobs/x')`, id2)
	o = waitOut(t, runPush(ctx, d, id2, c2), 5*time.Second)
	if o.err != nil || o.res != (lifecycle.PushResult{DeliveredSeq: 2, Stopped: StoppedFailed}) {
		t.Fatalf("an unfoldable row: %+v %v", o.res, o.err)
	}
	// The same row met by the initial fold of a restarted activity.
	c3, _ := e.saveInline(cs, id2, buyerA, &a2a.PushConfig{URL: r.url("/hook3")})
	e.exec(`update kontra.push_configs set delivered_seq = 3 where task_id = $1 and id = $2`, id2, c3)
	o = waitOut(t, runPush(ctx, d, id2, c3), 5*time.Second)
	if o.err != nil || o.res.Stopped != StoppedFailed {
		t.Fatalf("an unfoldable row in the initial fold: %+v %v", o.res, o.err)
	}
}

// TestDeletedConfigEndsDuringBackoff: a config deleted while its delivery is failing ends its push_webhook
// within one heartbeat step of the deletion, not after the backoff (up to 5 min) it was waiting out, which
// would hold the task's workflow open that long (spec §10.2, D78). The clock is fake; the requests are real.
func TestDeletedConfigEndsDuringBackoff(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	clk := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	id := e.newTask(buyerA)
	var (
		cid       string
		deletedAt time.Time
	)
	r := newReceiver(t, func(n int) int {
		if n == 10 { // the next wait is the 5 min cap: the caller deletes the config now
			if _, err := e.pool.Exec(context.Background(), `delete from kontra.push_configs where task_id = $1 and id = $2`, id, cid); err != nil {
				t.Error(err)
			}
			deletedAt = clk.Now()
		}
		return http.StatusServiceUnavailable
	})
	cid, _ = e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	o := fast()
	o.RetryMin, o.RetryMax, o.Budget = 0, 0, 0 // the spec's timing
	o.Now, o.Sleep = clk.Now, clk.Sleep
	out := waitOut(t, runPush(context.Background(), NewDeliverer(e.log, private, o), id, cid), 30*time.Second)
	if out.err != nil || out.res.Stopped != StoppedDeleted {
		t.Fatalf("push_webhook: %+v %v", out.res, out.err)
	}
	r.mu.Lock()
	at := deletedAt
	r.mu.Unlock()
	if at.IsZero() {
		t.Fatalf("the config was never deleted (%d requests)", len(r.requests()))
	}
	if after := clk.Now().Sub(at); after > beatEvery {
		t.Fatalf("the activity ended %s after its config was deleted: it slept out its backoff", after)
	}
	if n := len(r.requests()); n != 10 {
		t.Errorf("%d attempts, want none after the deletion", n)
	}
}

// TestFoldReadsAreBoundedAndBeat: a restarted push_webhook folds the log up to its position in reads of at
// most ReadBytes of stored rows, never the whole log at once, and heartbeats between reads, so folding a
// large task neither holds it all twice in memory nor outlasts the heartbeat timeout.
func TestFoldReadsAreBoundedAndBeat(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))
	e.status(id, store.StateWorking)
	const rows = 12
	for i := range rows {
		part := make([]byte, 4<<10) // 8 KiB of hex: stored as it is (it does not compress)
		if _, err := rand.Read(part); err != nil {
			t.Fatal(err)
		}
		e.artifact(id, a2a.ArtifactID(fmt.Sprintf("a%d", i)), hex.EncodeToString(part), false)
	}
	term := e.status(id, store.StateCompleted)
	e.exec(`update kontra.push_configs set delivered_seq = $3 where task_id = $1 and id = $2`, id, cid, term-1)
	o := fast()
	o.ReadBytes = 4 << 10 // smaller than one row: one row a read
	d := NewDeliverer(e.log, private, o)
	var (
		mu    sync.Mutex
		beats int
		first = -1 // beats before the first delivery
	)
	d.onBeat = func() {
		mu.Lock()
		beats++
		if first < 0 && len(r.requests()) > 0 {
			first = beats - 1
		}
		mu.Unlock()
	}
	out := waitOut(t, runPush(context.Background(), d, id, cid), 10*time.Second)
	if out.err != nil || out.res.Stopped != StoppedTerminal {
		t.Fatalf("push_webhook: %+v %v", out.res, out.err)
	}
	if got := r.delivered(); len(got) != 1 || got[0].body != e.expectedBody(id, term) {
		t.Fatalf("%d deliveries, want the terminal snapshot only", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	// The fold reads each artifact row on its own (the small rows before them share a read) and
	// heartbeats after each read.
	if first < rows {
		t.Fatalf("%d heartbeats before the first delivery, want at least one per read of the fold (%d large rows)", first, rows)
	}
}

// TestPushWebhookFindsItsConfigByRef: push_webhook's input names the config by its ConfigRef (history never
// carries a config id, which may be the caller's); it finds the config among the task's, and a ref that
// names none (the config was deleted) ends it at once.
func TestPushWebhookFindsItsConfigByRef(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	if _, err := cs.Save(context.Background(), tid(id), &a2a.PushConfig{ID: callerConfigID, URL: r.url("/hook")}); err != nil {
		t.Fatal(err)
	}
	e.status(id, store.StateCompleted)
	d := NewDeliverer(e.log, private, fast())
	in := lifecycle.PushInput{TaskID: id.String(), ConfigID: lifecycle.ConfigRef(id, callerConfigID), Deadline: time.Now().Add(time.Hour)}
	res, err := d.PushWebhook(context.Background(), in)
	if err != nil || res.Stopped != StoppedTerminal || len(r.delivered()) == 0 {
		t.Fatalf("by ref: %+v %v, %d deliveries", res, err, len(r.delivered()))
	}
	// The raw id names no config: it is not a ref.
	in.ConfigID = callerConfigID
	before := len(r.requests())
	if res, err := d.PushWebhook(context.Background(), in); err != nil || res.Stopped != StoppedDeleted || len(r.requests()) != before {
		t.Fatalf("by the raw id: %+v %v", res, err)
	}
}

// D72's N before it is known (a replica's start, when Temporal hands it many push_webhook retries at
// once): a reader during the first count waits for it instead of taking N = 1, and a first count that
// fails gives a conservative N (UnknownReplicas), not 1, until a count succeeds. Once N is known, a failed
// count keeps it.
func TestReplicaCountBeforeItIsKnown(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	o := fast()
	o.Replicas = func(context.Context) (int, error) { <-release; return 4, nil }
	d := NewDeliverer(nil, private, o)
	first, second := make(chan int, 1), make(chan int, 1)
	go func() { first <- d.liveReplicaCount(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		counting := d.counting != nil
		d.mu.Unlock()
		if counting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first count never started")
		}
		time.Sleep(time.Millisecond)
	}
	go func() { second <- d.liveReplicaCount(ctx) }()
	select {
	case n := <-second:
		t.Fatalf("a reader during the first count got N = %d before it ended (the gate has 4 replicas)", n)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if a, b := <-first, <-second; a != 4 || b != 4 {
		t.Errorf("N = %d and %d, want 4", a, b)
	}

	var fail atomic.Bool
	fail.Store(true)
	now := time.Now()
	o2 := fast()
	o2.Now = func() time.Time { return now }
	o2.Replicas = func(context.Context) (int, error) {
		if fail.Load() {
			return 0, errors.New("db blip")
		}
		return 3, nil
	}
	d2 := NewDeliverer(nil, private, o2)
	if n := d2.liveReplicaCount(ctx); n != UnknownReplicas {
		t.Errorf("a failed first count: N = %d, want %d", n, UnknownReplicas)
	}
	if n := d2.liveReplicaCount(ctx); n != UnknownReplicas {
		t.Errorf("before the next refresh: N = %d, want %d", n, UnknownReplicas)
	}
	fail.Store(false)
	now = now.Add(ReplicaRefresh)
	if n := d2.liveReplicaCount(ctx); n != 3 {
		t.Errorf("after a count: N = %d, want 3", n)
	}
	fail.Store(true)
	now = now.Add(ReplicaRefresh)
	if n := d2.liveReplicaCount(ctx); n != 3 {
		t.Errorf("a failed count once N is known: N = %d, want the last, 3", n)
	}
}
