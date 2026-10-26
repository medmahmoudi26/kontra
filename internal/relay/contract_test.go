package relay

// Contract tests: the a2a-go probes P1–P14 (a2a-go brief, Appendix A; D7) as tests against the module's
// resolved a2a-go v2.7.0 and grpc v1.84.0. Each pins one behaviour of a2a-go the relay is built around
// (or works around). A bump of a2a-go, or of a dependency that changes them, fails here first.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
)

// The versions the probes ran on, and the ones the module resolves (goose raised grpc to v1.84.0): the
// a2a-go code compiled into this test, and the module's selected versions in go.mod.
func TestContractVersions(t *testing.T) {
	pc := reflect.ValueOf(a2asrv.NewHandler).Pointer()
	file, _ := runtime.FuncForPC(pc).FileLine(pc)
	if !strings.Contains(filepath.ToSlash(file), "a2a-go/v2@v2.7.0/") {
		t.Errorf("a2a-go is compiled from %s; the contract tests pin v2.7.0: re-run the probes before moving (D7)", file)
	}
	gomod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for mod, v := range map[string]string{
		"github.com/a2aproject/a2a-go/v2": "v2.7.0", "google.golang.org/grpc": "v1.84.0", "github.com/a2aproject/a2a-go": "v0.3.15",
	} {
		found := false
		for _, line := range strings.Split(string(gomod), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == mod {
				found = true
				if f[1] != v {
					t.Errorf("go.mod selects %s %s; the contract tests ran on %s: re-run the probes (D7)", mod, f[1], v)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in go.mod", mod)
		}
	}
}

// probeStore mimics the gate's store: every Create/Update appends its event to a per-task log.
type probeStore struct {
	*taskstore.InMemory
	mu  sync.Mutex
	log map[a2a.TaskID][]*eventqueue.Message
}

func newProbeStore() *probeStore {
	return &probeStore{InMemory: taskstore.NewInMemory(nil), log: map[a2a.TaskID][]*eventqueue.Message{}}
}

func (s *probeStore) Create(ctx context.Context, t *a2a.Task) (taskstore.TaskVersion, error) {
	v, err := s.InMemory.Create(ctx, t)
	if err == nil {
		s.append(t.ID, t, v)
	}
	return v, err
}

func (s *probeStore) Update(ctx context.Context, r *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	v, err := s.InMemory.Update(ctx, r)
	if err == nil {
		s.append(r.Task.ID, r.Event, v)
	}
	return v, err
}

func (s *probeStore) append(id a2a.TaskID, e a2a.Event, v taskstore.TaskVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log[id] = append(s.log[id], &eventqueue.Message{Event: e, TaskVersion: v, Protocol: a2a.Version})
}

func (s *probeStore) since(id a2a.TaskID, i int) []*eventqueue.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.log[id]
	if i >= len(l) {
		return nil
	}
	return append([]*eventqueue.Message(nil), l[i:]...)
}

func (s *probeStore) state(id a2a.TaskID) a2a.TaskState {
	st, err := s.InMemory.Get(context.Background(), id)
	if err != nil {
		return ""
	}
	return st.Task.Status.State
}

// slicePuller replays the log from the start; tailPuller starts at the tail when the reader is created,
// optionally skipping user-message rows (C11).
type slicePuller struct{ s *probeStore }

func (p *slicePuller) Pull(_ context.Context, tid a2a.TaskID, cur eventqueue.PullCursor) (*eventqueue.PullResponse, error) {
	i, _ := cur.(int)
	msgs := p.s.since(tid, i)
	return &eventqueue.PullResponse{Messages: msgs, Cursor: i + len(msgs)}, nil
}
func (p *slicePuller) Close(context.Context) error { return nil }

type tailPuller struct {
	s        *probeStore
	start    int
	skipUser bool
}

func (p *tailPuller) Pull(_ context.Context, tid a2a.TaskID, cur eventqueue.PullCursor) (*eventqueue.PullResponse, error) {
	i, ok := cur.(int)
	if !ok {
		i = p.start
	}
	raw := p.s.since(tid, i)
	var out []*eventqueue.Message
	for _, m := range raw {
		if msg, isMsg := m.Event.(*a2a.Message); isMsg && p.skipUser && msg.Role == a2a.MessageRoleUser {
			continue
		}
		out = append(out, m)
	}
	return &eventqueue.PullResponse{Messages: out, Cursor: i + len(raw)}, nil
}
func (p *tailPuller) Close(context.Context) error { return nil }

// probeTemporal is a NewPushQueue work queue whose Write JSON round-trips the payload (as Temporal's
// converter would) and runs the HandlerFn in a goroutine (the activity on another replica).
type probeTemporal struct {
	mu       sync.Mutex
	handler  workqueue.HandlerFn
	override func(*workqueue.Payload) a2a.TaskID
	before   func(ctx context.Context, p *workqueue.Payload) error
	rawJSON  []string
	errs     chan error
	sync     bool // run the handler before Write returns (the turn ends before the subscription reads)
}

func (ft *probeTemporal) write(ctx context.Context, p *workqueue.Payload) (a2a.TaskID, error) {
	if ft.override != nil && p.Type == workqueue.PayloadTypeExecute && p.ExecuteRequest.Message.TaskID == "" {
		p.TaskID = ft.override(p)
	}
	if ft.before != nil {
		if err := ft.before(ctx, p); err != nil {
			return "", err
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var cp workqueue.Payload
	if err := json.Unmarshal(raw, &cp); err != nil {
		return "", err
	}
	ft.mu.Lock()
	ft.rawJSON = append(ft.rawJSON, string(raw))
	ft.mu.Unlock()
	run := func() {
		_, err := ft.handler(context.Background(), &cp)
		if ft.errs != nil {
			ft.errs <- err
		}
	}
	if ft.sync {
		run()
	} else {
		go run()
	}
	return cp.TaskID, nil
}

type probeCluster struct {
	h     a2asrv.RequestHandler
	store *probeStore
	ft    *probeTemporal
}

type probeConfig struct {
	tail, skipUser bool
	// wrap, when set, is the store the handler writes through, over the probe store the puller reads.
	wrap func(*probeStore) taskstore.Store
}

func newProbeCluster(exec a2asrv.AgentExecutor, cfg probeConfig, opts ...a2asrv.RequestHandlerOption) *probeCluster {
	s := newProbeStore()
	qm := eventqueue.NewPullQueueManager(func(ctx context.Context, tid a2a.TaskID) (eventqueue.Puller, error) {
		if cfg.tail {
			return &tailPuller{s: s, start: len(s.since(tid, 0)), skipUser: cfg.skipUser}, nil
		}
		return &slicePuller{s: s}, nil
	}, eventqueue.PullConfig{PollInterval: 20 * time.Millisecond})
	ft := &probeTemporal{errs: make(chan error, 16)}
	q, hfn := workqueue.NewPushQueue(workqueue.WriterFunc(ft.write))
	ft.handler = hfn
	var st taskstore.Store = s
	if cfg.wrap != nil {
		st = cfg.wrap(s)
	}
	all := append([]a2asrv.RequestHandlerOption{a2asrv.WithClusterMode(a2asrv.ClusterConfig{QueueManager: qm, WorkQueue: q, TaskStore: st})}, opts...)
	return &probeCluster{h: a2asrv.NewHandler(exec, all...), store: s, ft: ft}
}

func probeMsg(text string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
}

func execFunc(fn func(ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool)) a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) { fn(ctx, ec, yield) }
	})
}

func completing() a2asrv.AgentExecutor {
	return execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	})
}

// P1: in cluster mode a bare Message reply never reaches the caller: SendMessage hangs (why D5 turns an
// upstream Message into a completed task).
func TestP1MessageReplyHangsInClusterMode(t *testing.T) {
	t.Parallel()
	c := newProbeCluster(execFunc(func(_ context.Context, _ *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("stateless answer")), nil)
	}), probeConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q")})
	if !errors.Is(err, context.DeadlineExceeded) || res != nil {
		t.Errorf("a bare Message reply: %T %v; a2a-go now delivers it (D5 may be revisited)", res, err)
	}
}

// P1b: Task then completed works; task and context ids are UUIDv7.
func TestP1bTaskReply(t *testing.T) {
	t.Parallel()
	c := newProbeCluster(completing(), probeConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q")})
	if err != nil {
		t.Fatal(err)
	}
	task := res.(*a2a.Task)
	u, _ := uuid.Parse(string(task.ID))
	cu, _ := uuid.Parse(task.ContextID)
	if task.Status.State != a2a.TaskStateCompleted || u.Version() != 7 || cu.Version() != 7 {
		t.Errorf("%s %s v%d v%d", task.Status.State, task.ID, u.Version(), cu.Version())
	}
}

type cancelProbe struct {
	cancelErr   error
	started     chan struct{}
	execCtxDone atomic.Bool
}

func (p *cancelProbe) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		close(p.started)
		<-ctx.Done()
		p.execCtxDone.Store(true)
	}
}

func (p *cancelProbe) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if p.cancelErr != nil {
			yield(nil, p.cancelErr)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func runCancelProbe(t *testing.T, p *cancelProbe) (a2a.TaskState, error, a2a.TaskState) {
	c := newProbeCluster(p, probeConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q"), Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	tid := res.(*a2a.Task).ID
	<-p.started
	time.Sleep(50 * time.Millisecond)
	ct, cerr := c.h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: tid})
	var cstate a2a.TaskState
	if ct != nil {
		cstate = ct.Status.State
	}
	time.Sleep(300 * time.Millisecond)
	return cstate, cerr, c.store.state(tid)
}

// P2a: an executor Cancel that errors marks the task failed (why D4 decides cancel in an interceptor).
func TestP2aCancelErrorFailsTask(t *testing.T) {
	t.Parallel()
	p := &cancelProbe{cancelErr: a2a.ErrUnsupportedOperation, started: make(chan struct{})}
	_, cerr, stored := runCancelProbe(t, p)
	if !errors.Is(cerr, a2a.ErrTaskNotCancelable) || stored != a2a.TaskStateFailed {
		t.Errorf("cancel err %v, stored %s", cerr, stored)
	}
}

// P2b: a successful cancel does not cancel the running execution's context.
func TestP2bCancelLeavesExecutionRunning(t *testing.T) {
	t.Parallel()
	p := &cancelProbe{started: make(chan struct{})}
	cstate, cerr, stored := runCancelProbe(t, p)
	if cerr != nil || cstate != a2a.TaskStateCanceled || stored != a2a.TaskStateCanceled || p.execCtxDone.Load() {
		t.Errorf("cancel %s %v stored %s; running execution's context canceled: %v", cstate, cerr, stored, p.execCtxDone.Load())
	}
}

// P3a–P3d: a2a-go checks ids and event order: why the relay rewrites ids and never yields a first status.
func TestP3IDAndOrderChecks(t *testing.T) {
	t.Parallel()
	up := a2a.TaskInfo{TaskID: "upstream-task-1", ContextID: "upstream-ctx-1"}
	send := func(exec a2asrv.AgentExecutor, wait time.Duration) (a2a.SendMessageResult, error) {
		c := newProbeCluster(exec, probeConfig{})
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		return c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q")})
	}
	res, err := send(execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			yield(a2a.NewStatusUpdateEvent(up, a2a.TaskStateWorking, nil), nil)
		}
	}), 5*time.Second)
	if task, ok := res.(*a2a.Task); !ok || task.Status.State != a2a.TaskStateFailed {
		t.Errorf("P3a mid-stream id mismatch: %+v %v", res, err)
	}
	res, err = send(execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(a2a.NewSubmittedTask(a2a.TaskInfo{TaskID: up.TaskID, ContextID: ec.ContextID}, ec.Message), nil)
	}), time.Second)
	if res != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("P3b first-event id mismatch: %T %v", res, err)
	}
	res, err = send(execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("late")), nil)
		}
	}), 5*time.Second)
	if task, ok := res.(*a2a.Task); !ok || task.Status.State != a2a.TaskStateFailed {
		t.Errorf("P3c message after task: %+v %v", res, err)
	}
	res, err = send(execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil)
	}), time.Second)
	if res != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("P3d status first: %T %v", res, err)
	}
}

const probeSend = `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`

func probePost(t *testing.T, url, body string, hdr map[string][]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// P4/P5: A2A-Version and A2A-Extensions as a2a-go v2.7.0 reads them (why D9 normalizes them in front).
func TestP4VersionAndExtensionHeaders(t *testing.T) {
	t.Parallel()
	const ext = "https://kontra.sh/ext/escrow/v1"
	enf, err := a2asrv.NewAgentCardEnforcer(&a2a.AgentCard{Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: ext, Required: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(completing(), a2asrv.WithCallInterceptors(enf))))
	defer srv.Close()
	for _, tc := range []struct {
		name string
		hdr  map[string][]string
		code string // "" for success
	}{
		{"no A2A-Version", map[string][]string{"A2A-Extensions": {ext}}, "-32009"},
		{"A2A-Version 1.0.0", map[string][]string{"A2A-Version": {"1.0.0"}, "A2A-Extensions": {ext}}, "-32009"},
		{"one URI", map[string][]string{"A2A-Version": {"1.0"}, "A2A-Extensions": {ext}}, ""},
		{"comma list", map[string][]string{"A2A-Version": {"1.0"}, "A2A-Extensions": {ext + ", urn:other"}}, "-32008"},
		{"two header lines", map[string][]string{"A2A-Version": {"1.0"}, "A2A-Extensions": {ext, "urn:other"}}, ""},
		{"X- alias only", map[string][]string{"A2A-Version": {"1.0"}, "X-A2A-Extensions": {ext}}, "-32008"},
	} {
		_, body, h := probePost(t, srv.URL, probeSend, tc.hdr)
		ok := !strings.Contains(body, `"error"`)
		if tc.code == "" && !ok || tc.code != "" && !strings.Contains(body, `"code":`+tc.code) {
			t.Errorf("%s: %s", tc.name, body)
		}
		if h.Get("A2A-Extensions") != "" {
			t.Errorf("%s: a2a-go now echoes A2A-Extensions (%q)", tc.name, h.Get("A2A-Extensions"))
		}
	}
}

// P6: a2a-go's SSE ids are random UUIDv4 and Last-Event-ID is ignored (why D2 owns the SSE writer).
func TestP6SSEIdsAndLastEventID(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	c := newProbeCluster(execFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) || !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) ||
			!yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("chunk-1")), nil) {
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}), probeConfig{tail: true, skipUser: true})
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(c.h))
	defer srv.Close()
	stream := func(body string, hdr map[string]string, n int) []string {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("A2A-Version", "1.0")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var lines []string
		for len(lines) < 2*n && sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "id:") || strings.HasPrefix(l, "data:") {
				lines = append(lines, l)
			}
		}
		return lines
	}
	lines := stream(strings.Replace(probeSend, `"SendMessage"`, `"SendStreamingMessage"`, 1), nil, 3)
	var ids []string
	var taskID string
	for _, l := range lines {
		if s, ok := strings.CutPrefix(l, "id:"); ok {
			ids = append(ids, strings.TrimSpace(s))
		}
		if s, ok := strings.CutPrefix(l, "data:"); ok && taskID == "" {
			var env struct {
				Result a2a.StreamResponse `json:"result"`
			}
			if json.Unmarshal([]byte(s), &env) == nil && env.Result.Event != nil {
				taskID = string(env.Result.Event.TaskInfo().TaskID)
			}
		}
	}
	for _, id := range ids {
		if u, err := uuid.Parse(id); err != nil || u.Version() != 4 {
			t.Errorf("SSE id %q is not a UUIDv4", id)
		}
	}
	if len(ids) < 2 || taskID == "" {
		t.Fatalf("ids %v task %q", ids, taskID)
	}
	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	sub := stream(`{"jsonrpc":"2.0","id":2,"method":"SubscribeToTask","params":{"id":"`+taskID+`"}}`, map[string]string{"Last-Event-ID": ids[1]}, 1)
	if len(sub) < 2 || !strings.Contains(sub[1], `"task":`) || !strings.Contains(sub[1], "chunk-1") {
		t.Errorf("resubscribe with Last-Event-ID did not start with the full current task: %v", sub)
	}
}

func twoTurns(sawStored *atomic.Bool) a2asrv.AgentExecutor {
	return execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				// Late enough that the blocking SendMessage reads the store first (P15 is the other order).
				time.Sleep(100 * time.Millisecond)
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, nil), nil)
			}
			return
		}
		sawStored.Store(ec.StoredTask.ID == ec.TaskID && ec.Message.TaskID == ec.TaskID && len(ec.StoredTask.History) == 2)
		time.Sleep(50 * time.Millisecond)
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	})
}

func runTwoTurns(t *testing.T, cfg probeConfig, override bool) (a2a.SendMessageResult, error, *probeCluster, a2a.TaskID, bool) {
	var saw atomic.Bool
	c := newProbeCluster(twoTurns(&saw), cfg)
	minted := a2a.TaskID(uuid.Must(uuid.NewV7()).String())
	if override {
		c.ft.override = func(*workqueue.Payload) a2a.TaskID { return minted }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("turn1")})
	if err != nil {
		t.Fatal(err)
	}
	task := res.(*a2a.Task)
	if override && task.ID != minted {
		t.Errorf("P7: Write's task id override was not honoured: %s", task.ID)
	}
	follow := probeMsg("turn2")
	follow.TaskID = task.ID
	res2, err2 := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: follow})
	return res2, err2, c, task.ID, saw.Load()
}

// P7: a puller replaying from the log's start breaks turn 2 (the reader stops on turn 1's final event).
func TestP7ReplayingPullerBreaksFollowUps(t *testing.T) {
	t.Parallel()
	res, err, _, _, _ := runTwoTurns(t, probeConfig{}, true)
	if err == nil || !strings.Contains(err.Error(), "queue is closed") {
		t.Errorf("turn 2 with a replaying puller: %T %v", res, err)
	}
}

// P7b: a tail puller that emits user-message rows answers turn 2 with the buyer's own message.
func TestP7bUserRowsReturnTheBuyersMessage(t *testing.T) {
	t.Parallel()
	res, err, _, _, _ := runTwoTurns(t, probeConfig{tail: true}, false)
	if m, ok := res.(*a2a.Message); !ok || m.Role != a2a.MessageRoleUser || err != nil {
		t.Errorf("turn 2: %T %v", res, err)
	}
}

// P7c: a tail puller that skips user rows (the gate's) completes turn 2; the executor sees the stored task.
func TestP7cTailPullerSkippingUserRows(t *testing.T) {
	t.Parallel()
	res, err, _, _, saw := runTwoTurns(t, probeConfig{tail: true, skipUser: true}, false)
	if task, ok := res.(*a2a.Task); !ok || err != nil || task.Status.State != a2a.TaskStateCompleted || !saw {
		t.Errorf("turn 2: %T %v executor saw the stored task with history: %v", res, err, saw)
	}
}

// P8: the work queue payload carries every inbound header, Authorization and cookies included (why D6
// keeps it out of Temporal).
func TestP8PayloadCarriesHeaders(t *testing.T) {
	t.Parallel()
	c := newProbeCluster(completing(), probeConfig{tail: true, skipUser: true})
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(c.h))
	defer srv.Close()
	probePost(t, srv.URL, probeSend, map[string][]string{"A2A-Version": {"1.0"}, "Authorization": {"Bearer kontra_sk_SECRET123"}, "Cookie": {"session=abc"}})
	c.ft.mu.Lock()
	defer c.ft.mu.Unlock()
	if len(c.ft.rawJSON) == 0 || !strings.Contains(c.ft.rawJSON[0], "kontra_sk_SECRET123") || !strings.Contains(c.ft.rawJSON[0], "session=abc") {
		t.Errorf("the payload no longer carries the request's credentials: %v", c.ft.rawJSON)
	}
}

// P9: the first cursor a2a-go passes is nil, and a type assertion on it fails (the example puller's bug).
func TestP9NilCursor(t *testing.T) {
	var cur eventqueue.PullCursor
	if _, ok := cur.(string); ok {
		t.Error("a nil cursor asserted as a string")
	}
}

// ctxStore behaves like pgx: a write on a canceled context fails.
type ctxStore struct{ *probeStore }

func (s *ctxStore) Update(ctx context.Context, r *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if err := ctx.Err(); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return s.probeStore.Update(ctx, r)
}

func runExecErr(t *testing.T, honorCtx bool) (stored a2a.TaskState, handlerErr error) {
	var c *probeCluster
	exec := execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) || !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		time.Sleep(50 * time.Millisecond)
		yield(nil, errors.New("tunnel_lost: yamux session closed"))
	})
	cfg := probeConfig{tail: true, skipUser: true}
	if honorCtx {
		cfg.wrap = func(s *probeStore) taskstore.Store { return &ctxStore{s} }
	}
	c = newProbeCluster(exec, cfg)
	ps := c.store
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, _ := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q"), Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	handlerErr = <-c.ft.errs
	time.Sleep(50 * time.Millisecond)
	if task, ok := res.(*a2a.Task); ok {
		stored = ps.state(task.ID)
	}
	return stored, handlerErr
}

// P10a: an executor error with a store that ignores the context marks the task failed (no message).
func TestP10aExecutorErrorFailsTask(t *testing.T) {
	t.Parallel()
	stored, herr := runExecErr(t, false)
	if stored != a2a.TaskStateFailed {
		t.Errorf("stored %s handler %v", stored, herr)
	}
}

// P10b: with a store that honours the context (pgx), SetTaskFailed runs on a canceled context and the
// task stays working; the HandlerFn returns the executor's error (why D8 writes on WithoutCancel and the
// relay emits explicit terminal events).
func TestP10bExecutorErrorWithCtxStore(t *testing.T) {
	t.Parallel()
	stored, herr := runExecErr(t, true)
	if stored != a2a.TaskStateWorking || herr == nil || !strings.Contains(herr.Error(), "tunnel_lost") {
		t.Errorf("stored %s handler %v", stored, herr)
	}
}

// P11: a2av0's parser yields no interface for a 0.2 card without preferredTransport, and a struct round
// trip drops unknown fields (why the card is rewritten as raw JSON and 0.2 cards get JSONRPC, D11/D12).
func TestP11CompatCards(t *testing.T) {
	t.Parallel()
	parse := a2av0.NewAgentCardParser()
	for name, tc := range map[string]struct {
		raw  string
		want int
	}{
		"0.2 (url only)":                {`{"name":"a","description":"d","url":"http://x/","version":"1","capabilities":{"streaming":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`, 0},
		"0.2.6 (no preferredTransport)": {`{"protocolVersion":"0.2.6","name":"a","description":"d","url":"http://x/","version":"1","capabilities":{},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[]}`, 0},
		"0.3 (preferredTransport)":      {`{"protocolVersion":"0.3.0","name":"a","description":"d","url":"http://x/","preferredTransport":"JSONRPC","version":"1","capabilities":{},"defaultInputModes":["text"],"defaultOutputModes":["text"],"skills":[],"x-vendor":"keep-me"}`, 1},
	} {
		c, err := parse([]byte(tc.raw))
		if err != nil || len(c.SupportedInterfaces) != tc.want {
			t.Errorf("%s: %v interfaces %d", name, err, len(c.SupportedInterfaces))
			continue
		}
		out, _ := json.Marshal(c)
		if strings.Contains(string(out), "keep-me") {
			t.Errorf("%s: the parser now keeps unknown fields", name)
		}
	}
	var card a2a.AgentCard
	_ = json.Unmarshal([]byte(`{"name":"a","supportedInterfaces":[],"capabilities":{},"x-vendor":"keep-me"}`), &card)
	if out, _ := json.Marshal(&card); strings.Contains(string(out), "keep-me") {
		t.Error("a2a.AgentCard now keeps unknown fields")
	}
}

// P12a/b: SubscribeToTask on an input-required task blocks after the snapshot; on a completed task in
// cluster mode it yields the snapshot and no error (why the gate's SSE writer refuses terminal tasks).
func TestP12SubscribeInterruptedAndTerminal(t *testing.T) {
	t.Parallel()
	var saw atomic.Bool
	c := newProbeCluster(twoTurns(&saw), probeConfig{tail: true, skipUser: true})
	ctx := context.Background()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("t1")})
	if err != nil {
		t.Fatal(err)
	}
	tid := res.(*a2a.Task).ID
	sctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var got []string
	var endErr error
	for ev, err := range c.h.SubscribeToTask(sctx, &a2a.SubscribeToTaskRequest{ID: tid}) {
		if err != nil {
			endErr = err
			break
		}
		got = append(got, fmt.Sprintf("%T", ev))
	}
	if len(got) != 1 || got[0] != "*a2a.Task" || endErr == nil {
		t.Errorf("P12a: %v %v", got, endErr)
	}
	follow := probeMsg("t2")
	follow.TaskID = tid
	if _, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: follow}); err != nil {
		t.Fatal(err)
	}
	got, endErr = nil, nil
	for ev, err := range c.h.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: tid}) {
		if err != nil {
			endErr = err
			break
		}
		got = append(got, string(ev.(*a2a.Task).Status.State))
	}
	if len(got) != 1 || got[0] != string(a2a.TaskStateCompleted) || endErr != nil {
		t.Errorf("P12b: %v %v", got, endErr)
	}
}

// P13: a2a-go's resolver fetches a non-root URL as the card itself (why D10 serves the card at {svc}).
func TestP13ResolverOnAgentPath(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	srvURL := ""
	mux.Handle("/agents/recon", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(completing())))
	mux.HandleFunc("/agents/recon/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(&a2a.AgentCard{Name: "recon", SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(srvURL+"/agents/recon", a2a.TransportProtocolJSONRPC)}}).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	card, err := agentcard.DefaultResolver.Resolve(context.Background(), srv.URL+"/agents/recon")
	if err != nil || card == nil || card.Name != "" || len(card.SupportedInterfaces) != 0 {
		t.Errorf("non-root URL: %+v %v", card, err)
	} else if _, err := a2aclient.NewFromCard(context.Background(), card); err == nil {
		t.Error("a client was built from the empty card")
	}
	card, err = agentcard.DefaultResolver.Resolve(context.Background(), srv.URL+"/agents/recon/.well-known/agent-card.json")
	if err != nil || card.Name != "recon" {
		t.Errorf("well-known URL: %+v %v", card, err)
	}
}

// P14: Write pre-creating the gate task (gate ids, submitted) works: returnImmediately answers it at
// once with the gate's context id, the executor sees the stored task, and the run completes.
func TestP14WritePreCreatesTask(t *testing.T) {
	t.Parallel()
	var sawStored atomic.Bool
	release := make(chan struct{})
	gateCtx := "ctx-" + uuid.Must(uuid.NewV7()).String()
	var c *probeCluster
	c = newProbeCluster(execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		sawStored.Store(ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateSubmitted && len(ec.StoredTask.History) == 1)
		<-release
		if yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	}), probeConfig{tail: true, skipUser: true})
	c.ft.before = func(ctx context.Context, p *workqueue.Payload) error {
		if p.Type != workqueue.PayloadTypeExecute || p.ExecuteRequest.Message.TaskID != "" {
			return nil
		}
		_, err := c.store.Create(ctx, &a2a.Task{ID: p.TaskID, ContextID: gateCtx, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted},
			History: []*a2a.Message{p.ExecuteRequest.Message}})
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q"), Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	tk := res.(*a2a.Task)
	if tk.Status.State != a2a.TaskStateSubmitted || tk.ContextID != gateCtx || time.Since(start) > 2*time.Second {
		t.Errorf("returnImmediately: %s %s after %v", tk.Status.State, tk.ContextID, time.Since(start))
	}
	close(release)
	<-c.ft.errs
	final, _ := c.h.GetTask(ctx, &a2a.GetTaskRequest{ID: tk.ID})
	if !sawStored.Load() || final == nil || final.Status.State != a2a.TaskStateCompleted {
		t.Errorf("executor saw the pre-created task: %v, final %+v", sawStored.Load(), final)
	}
}

// P15: a blocking SendMessage whose turn already ended at input-required when its subscription read the
// store answers eventqueue.ErrQueueClosed: the snapshot is input-required (not terminal), every queued
// event is older and skipped, and the pull reader closes after its final event. The gate answers the
// stored task instead (finishedTurn, TestFinishedTurnAnswersTheTask).
func TestP15FinishedTurnRace(t *testing.T) {
	t.Parallel()
	c := newProbeCluster(execFunc(func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, nil), nil)
		}
	}), probeConfig{tail: true, skipUser: true})
	c.ft.sync = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := c.h.SendMessage(ctx, &a2a.SendMessageRequest{Message: probeMsg("q")})
	if !errors.Is(err, eventqueue.ErrQueueClosed) {
		t.Errorf("a turn finished before the subscription: %T %v; a2a-go fixed the race (finishedTurn may go)", res, err)
	}
}
