package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/pg"
	"github.com/medmahmoudi26/kontra/internal/pgtest"
	"github.com/medmahmoudi26/kontra/internal/store"
)

// pgEnv is one migrated database with a service, a task store for ownerA, and two writers, each woken by
// its own LISTEN connection, as two replicas would be.
type pgEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	log    *store.Log
	svc    store.ServiceRef
	st     *store.Store
	w1, w2 *Writer
}

type ownerKey struct{}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	dsn := pgtest.Migrated(t)
	e := &pgEnv{t: t, pool: pgtest.Pool(t, dsn)}
	e.log = store.NewLog(e.pool)
	tenant := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	for _, q := range []string{
		`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config')`,
	} {
		if _, err := e.pool.Exec(ctx, q, tenant); err != nil {
			t.Fatal(err)
		}
	}
	e.svc = store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retention: 24 * time.Hour}
	e.st = store.New(e.pool, e.svc, func(ctx context.Context) (store.Owner, error) { return ownerA, nil }, nil, e.log)
	opts := Options{KeepAlive: time.Hour, Reread: time.Hour} // only NOTIFY wakes them: a missed wake hangs the test
	e.w1 = NewWriterWithOptions(e.log, listener(t, dsn), opts)
	e.w2 = NewWriterWithOptions(e.log, listener(t, dsn), opts)
	return e
}

func listener(t *testing.T, dsn string) *pg.Listener {
	t.Helper()
	cfg, err := pg.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	l := pg.NewListener(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for !l.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("the listener did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return l
}

// newTask creates a submitted task the way the work queue does.
func (e *pgEnv) newTask(text string) *a2a.Task {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), "ctx-"+id.String()
	task := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: msg.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{msg}}
	if _, err := e.log.CreateTask(context.Background(), nil, store.NewTask{
		ID: id, Service: e.svc, Protocol: store.ProtocolA2A, Owner: ownerA, ContextID: msg.ContextID, State: store.StateSubmitted, Snapshot: task,
	}); err != nil {
		e.t.Fatal(err)
	}
	return task
}

// apply writes ev as a2a-go's processor does: the next projection over the stored one, at its version.
func (e *pgEnv) apply(task *a2a.Task, ev a2a.Event) int64 {
	e.t.Helper()
	ctx := context.Background()
	cur, err := e.st.Get(ctx, task.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	next, err := a2aevent.ApplyShallowUpdate(cur.Task, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := e.st.Update(ctx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevTask: cur.Task, PrevVersion: cur.Version})
	if err != nil {
		e.t.Fatalf("apply %T: %v", ev, err)
	}
	return int64(v)
}

func infoOf(task *a2a.Task) a2a.TaskInfo {
	return a2a.TaskInfo{TaskID: task.ID, ContextID: task.ContextID}
}

func (e *pgEnv) status(task *a2a.Task, st a2a.TaskState, text string) int64 {
	var m *a2a.Message
	if text != "" {
		m = a2a.NewMessageForTask(a2a.MessageRoleAgent, infoOf(task), a2a.NewTextPart(text))
	}
	return e.apply(task, a2a.NewStatusUpdateEvent(infoOf(task), st, m))
}

func (e *pgEnv) chunk(task *a2a.Task, i int) int64 {
	return e.apply(task, &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Append: i > 0,
		Artifact: &a2a.Artifact{ID: "out", Parts: a2a.ContentParts{a2a.NewTextPart(fmt.Sprintf("chunk %d;", i))}}})
}

func (e *pgEnv) metadata(task *a2a.Task, patch map[string]any) int64 {
	e.t.Helper()
	seq, err := e.log.ApplyMetadata(context.Background(), uuid.MustParse(string(task.ID)), patch)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

// carried returns the seqs of the task's carried rows, in order.
func (e *pgEnv) carried(task *a2a.Task) []int64 {
	e.t.Helper()
	evs, err := e.log.Events(context.Background(), uuid.MustParse(string(task.ID)), 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []int64
	for _, ev := range evs {
		if ev.Kind.Carried() {
			out = append(out, ev.Seq)
		}
	}
	return out
}

// open starts a stream through w on its own server; header adds request headers.
func (e *pgEnv) open(h http.HandlerFunc, header http.Header) *streamRes {
	return serve(e.t, h, header)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The unit half of relay_resume (spec §9, §16): a buyer reads a new task's stream up to K and drops it;
// a resubscribe with Last-Event-ID: K on another replica sends the snapshot as of K (equal to
// Log.SnapshotAt and to the buyer's own fold) without an id, then every carried row after K exactly once,
// in order, through the live tail, and ends right after the terminal row. Then the edge cases: K =
// terminal-1, K = terminal, K not a seq, another buyer, a malformed id.
func TestRelayResumeOverPostgres(t *testing.T) {
	e := newPGEnv(t)
	task := e.newTask("count to twenty")
	e.status(task, a2a.TaskStateWorking, "")
	for i := 0; i < 5; i++ {
		e.chunk(task, i)
	}
	// Stream 1: SendStreamingMessage of the new task on replica 1, from the creation row.
	s1 := e.open(func(rw http.ResponseWriter, r *http.Request) {
		e.w1.SendStreaming(rw, r, Stream{Binding: JSONRPC, RequestID: json.RawMessage(`1`), Service: e.svc, Owner: ownerA},
			func(context.Context) (Start, error) { return Start{TaskID: task.ID, New: true}, nil })
	}, nil)
	var (
		got1 []int64
		fold *a2a.Task
	)
	const k = 5
	for len(got1) == 0 || got1[len(got1)-1] < k {
		ev, ok := s1.nextData(t)
		if !ok {
			t.Fatal("stream 1 ended early")
		}
		got1 = append(got1, ev.id)
		switch v := decode(t, JSONRPC, ev.data, "1").(type) {
		case *a2a.Task:
			fold = v
		default:
			next, err := a2aevent.ApplyShallowUpdate(fold, v)
			if err != nil {
				t.Fatal(err)
			}
			fold = next
		}
	}
	s1.cancel() // the buyer drops at K
	if got1[0] != 1 || got1[len(got1)-1] != k {
		t.Fatalf("stream 1 ids %v", got1)
	}

	// Stream 2 on replica 2 with Last-Event-ID: K, while the task goes on.
	kk := int64(k)
	s2 := e.open(func(rw http.ResponseWriter, r *http.Request) {
		e.w2.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: e.svc, Owner: ownerA, TaskID: task.ID, LastEventID: &kk})
	}, nil)
	lead, _ := s2.nextData(t)
	if lead.id != 0 {
		t.Fatalf("the resumed snapshot has an id: %d", lead.id)
	}
	snapK, err := e.log.SnapshotAt(context.Background(), uuid.MustParse(string(task.ID)), k)
	if err != nil {
		t.Fatal(err)
	}
	leadTask := decode(t, HTTPJSON, lead.data, "").(*a2a.Task)
	if jsonOf(t, leadTask) != jsonOf(t, snapK) || jsonOf(t, leadTask) != jsonOf(t, fold) {
		t.Fatalf("snapshot as of %d:\n stream  %s\n log     %s\n buyer   %s", k, jsonOf(t, leadTask), jsonOf(t, snapK), jsonOf(t, fold))
	}
	go func() {
		for i := 5; i < 12; i++ {
			e.chunk(task, i)
			if i == 7 {
				e.metadata(task, map[string]any{"kontra.note": "half way"}) // not carried
			}
			time.Sleep(5 * time.Millisecond)
		}
		e.status(task, a2a.TaskStateCompleted, "twenty")
	}()
	var got2 []int64
	for {
		ev, ok := s2.nextData(t)
		if !ok {
			break
		}
		if ev.id <= k || (len(got2) > 0 && ev.id <= got2[len(got2)-1]) {
			t.Fatalf("id %d after %v (K = %d)", ev.id, got2, k)
		}
		got2 = append(got2, ev.id)
	}
	carried := e.carried(task)
	union := append(slices.Clone(got1), got2...)
	if fmt.Sprint(union) != fmt.Sprint(carried) {
		t.Fatalf("stream 1 up to K plus stream 2: %v\nthe log's carried seqs:      %v", union, carried)
	}
	terminalSeq := carried[len(carried)-1]
	if got2[len(got2)-1] != terminalSeq {
		t.Fatalf("stream 2 ended at %d, the terminal row is %d", got2[len(got2)-1], terminalSeq)
	}

	// Resume at terminal-1: the snapshot as of it, then only the terminal row.
	before := terminalSeq - 1
	s3 := e.open(func(rw http.ResponseWriter, r *http.Request) {
		e.w1.Subscribe(rw, r, Stream{Binding: JSONRPC, RequestID: json.RawMessage(`"x"`), Service: e.svc, Owner: ownerA, TaskID: task.ID, LastEventID: &before})
	}, nil)
	rest := s3.rest(t)
	if len(rest) != 2 || rest[0].id != 0 || rest[1].id != terminalSeq {
		t.Fatalf("resume at terminal-1: %+v", rest)
	}
	if st := decode(t, JSONRPC, rest[0].data, `"x"`).(*a2a.Task).Status.State; st != a2a.TaskStateWorking {
		t.Errorf("snapshot as of terminal-1 is %s", st)
	}

	// Errors before any SSE header.
	nonSeq := terminalSeq + 50
	for _, c := range []struct {
		name  string
		owner store.Owner
		id    a2a.TaskID
		k     *int64
		want  string
	}{
		{"K = terminal", ownerA, task.ID, &terminalSeq, "UNSUPPORTED_OPERATION"},
		{"K not a seq", ownerA, task.ID, &nonSeq, "INVALID_PARAMS"},
		{"no K on a terminal task", ownerA, task.ID, nil, "UNSUPPORTED_OPERATION"},
		{"another buyer", ownerB, task.ID, &before, "TASK_NOT_FOUND"},
		{"a malformed id", ownerA, "../" + task.ID, &before, "TASK_NOT_FOUND"},
	} {
		rec := httptest.NewRecorder()
		e.w2.Subscribe(rec, httptest.NewRequest("GET", "/", nil), Stream{Binding: HTTPJSON, Service: e.svc, Owner: c.owner, TaskID: c.id, LastEventID: c.k})
		if !strings.Contains(rec.Body.String(), c.want) || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// The log carries no upstream id: the stream is the gate's rows, ids already rewritten (spec §4).
	for _, ev := range rest {
		if strings.Contains(ev.data, "upstream") {
			t.Errorf("an event mentions upstream: %s", ev.data)
		}
	}
}

// What the snapshot as of K holds: a metadata row and a follow-up message before K, which the stream never
// carried; after the terminal row, an accepted review message is folded into history (D75).
func TestResumeSnapshotFoldsUncarriedRows(t *testing.T) {
	e := newPGEnv(t)
	task := e.newTask("book")
	e.status(task, a2a.TaskStateInputRequired, "which one?")
	e.metadata(task, map[string]any{"kontra.escrow.state": "held"})
	e.apply(task, a2a.NewMessageForTask(a2a.MessageRoleUser, infoOf(task), a2a.NewTextPart("the blue one")))
	k := e.status(task, a2a.TaskStateWorking, "")
	s := e.open(func(rw http.ResponseWriter, r *http.Request) {
		e.w1.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: e.svc, Owner: ownerA, TaskID: task.ID, LastEventID: &k})
	}, nil)
	lead, _ := s.nextData(t)
	snap := decode(t, HTTPJSON, lead.data, "").(*a2a.Task)
	followUp := slices.ContainsFunc(snap.History, func(m *a2a.Message) bool {
		return m.Role == a2a.MessageRoleUser && m.Parts[0].Text() == "the blue one"
	})
	if snap.Metadata["kontra.escrow.state"] != "held" || !followUp || snap.Status.State != a2a.TaskStateWorking {
		t.Fatalf("snapshot as of %d: %s", k, jsonOf(t, snap))
	}
	// A NUL in relayed content reaches the caller (json columns, D49).
	end := e.status(task, a2a.TaskStateCompleted, "done\x00ok")
	ev, _ := s.nextData(t)
	if ev.id != end || decode(t, HTTPJSON, ev.data, "").(*a2a.TaskStatusUpdateEvent).Status.Message.Parts[0].Text() != "done\x00ok" {
		t.Fatalf("terminal row: %+v", ev)
	}
	if _, ok := s.nextData(t); ok {
		t.Fatal("the stream stayed open after the terminal row")
	}
}

// SendStreamingMessage of a follow-up over Postgres: the snapshot at V leads without an id even though the
// turn's rows after V are already in the log (a fast dispatch), and the stream closes after the first row
// past V that ends the turn, not at the input-required snapshot.
func TestFollowUpStreamOverPostgres(t *testing.T) {
	e := newPGEnv(t)
	task := e.newTask("book")
	v := e.status(task, a2a.TaskStateInputRequired, "which one?")
	// The dispatch of the follow-up turn runs before the stream starts reading.
	e.apply(task, a2a.NewMessageForTask(a2a.MessageRoleUser, infoOf(task), a2a.NewTextPart("blue")))
	w1 := e.status(task, a2a.TaskStateWorking, "")
	ir := e.status(task, a2a.TaskStateInputRequired, "and the size?")
	e.status(task, a2a.TaskStateWorking, "")
	s := e.open(func(rw http.ResponseWriter, r *http.Request) {
		e.w1.SendStreaming(rw, r, Stream{Binding: JSONRPC, RequestID: json.RawMessage(`7`), Service: e.svc, Owner: ownerA},
			func(context.Context) (Start, error) { return Start{TaskID: task.ID, After: v, FollowUp: true}, nil })
	}, nil)
	got := s.rest(t)
	if len(got) != 3 || got[0].id != 0 || got[1].id != w1 || got[2].id != ir {
		t.Fatalf("follow-up stream: %+v", got)
	}
	if st := decode(t, JSONRPC, got[0].data, "7").(*a2a.Task).Status.State; st != a2a.TaskStateInputRequired {
		t.Errorf("lead state %s", st)
	}
}
