package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/pgtest"
)

// env is one fresh migrated database with a tenant and a service.
type env struct {
	t      testing.TB
	dsn    string
	pool   *pgxpool.Pool
	log    *Log
	tenant uuid.UUID
	svc    ServiceRef
}

var (
	buyerA = Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000a"), Principal: "key:kk_a_00000001", Via: ViaAPIKey}
	buyerB = Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000b"), Principal: "key:kk_b_00000001", Via: ViaAPIKey}
)

func newEnv(t testing.TB) *env {
	t.Helper()
	dsn := pgtest.Migrated(t)
	e := &env{t: t, dsn: dsn, pool: pgtest.Pool(t, dsn), tenant: uuid.Must(uuid.NewV7())}
	e.log = NewLog(e.pool)
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, e.tenant)
	e.svc = e.service("hello", 3600, 604800)
	e.svc.Timeout, e.svc.Retention = time.Hour, 7*24*time.Hour
	return e
}

// service inserts a direct a2a service with the given timeout and retention (seconds); the returned
// ref leaves Timeout and Retention zero, so the services row decides.
func (e *env) service(name string, timeoutS, retentionS int) ServiceRef {
	e.t.Helper()
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, timeout_s, retention_s)
		values ($1, $2, 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', $3, $4)`, e.tenant, name, timeoutS, retentionS)
	return ServiceRef{TenantID: e.tenant, Tenant: "default", Name: name, Kind: "a2a"}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

type ownerKey struct{}

func asOwner(ctx context.Context, o Owner) context.Context {
	return context.WithValue(ctx, ownerKey{}, o)
}

func ctxOwner(ctx context.Context) (Owner, error) {
	o, ok := ctx.Value(ownerKey{}).(Owner)
	if !ok {
		return Owner{}, fmt.Errorf("test: no owner: %w", a2a.ErrUnauthenticated)
	}
	return o, nil
}

func (e *env) store(guard FailureGuard) *Store { return New(e.pool, e.svc, ctxOwner, guard, e.log) }

// newTask pre-creates a submitted task the way the work queue does and returns its snapshot.
func (e *env) newTask(svc ServiceRef, o Owner, contextID, text string) *a2a.Task {
	e.t.Helper()
	task, _ := e.newTaskAt(svc, o, contextID, text, nil)
	return task
}

func (e *env) newTaskAt(svc ServiceRef, o Owner, contextID, text string, statusAt *time.Time) (*a2a.Task, Appended) {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	if contextID == "" {
		contextID = uuid.Must(uuid.NewV7()).String()
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), contextID
	task := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: contextID,
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted, Timestamp: statusAt}, History: []*a2a.Message{msg}}
	a, err := e.log.CreateTask(context.Background(), nil, NewTask{
		ID: id, Service: svc, Protocol: ProtocolA2A, Owner: o, ContextID: contextID, State: StateSubmitted, Snapshot: task,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return task, a
}

// apply writes ev through s the way a2a-go's processor does: the next snapshot from the default
// materializer (ApplyShallowUpdate) over the stored task, at the stored version.
func apply(ctx context.Context, s *Store, id a2a.TaskID, ev a2a.Event) (taskstore.TaskVersion, error) {
	st, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	next, err := a2aevent.ApplyShallowUpdate(st.Task, ev)
	if err != nil {
		return 0, err
	}
	return s.Update(ctx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevTask: st.Task, PrevVersion: st.Version})
}

func mustApply(t *testing.T, ctx context.Context, s *Store, id a2a.TaskID, ev a2a.Event) taskstore.TaskVersion {
	t.Helper()
	v, err := apply(ctx, s, id, ev)
	if err != nil {
		t.Fatalf("apply %T: %v", ev, err)
	}
	return v
}

func info(task *a2a.Task) a2a.TaskInfo {
	return a2a.TaskInfo{TaskID: task.ID, ContextID: task.ContextID}
}

func status(task *a2a.Task, st a2a.TaskState, text string) *a2a.TaskStatusUpdateEvent {
	var msg *a2a.Message
	if text != "" {
		msg = a2a.NewMessageForTask(a2a.MessageRoleAgent, info(task), a2a.NewTextPart(text))
	}
	return a2a.NewStatusUpdateEvent(info(task), st, msg)
}

func artifact(task *a2a.Task, id a2a.ArtifactID, appendTo bool, text string) *a2a.TaskArtifactUpdateEvent {
	return &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Append: appendTo,
		Artifact: &a2a.Artifact{ID: id, Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}
}

func userMessage(task *a2a.Task, text string) *a2a.Message {
	return a2a.NewMessageForTask(a2a.MessageRoleUser, info(task), a2a.NewTextPart(text))
}

func uid(t testing.TB, id a2a.TaskID) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(string(id))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// seqs returns the task's event seqs and kinds in order.
func (e *env) seqs(id a2a.TaskID) ([]int64, []EventKind) {
	e.t.Helper()
	evs, err := e.log.Events(context.Background(), uid(e.t, id), 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	var s []int64
	var k []EventKind
	for _, ev := range evs {
		s = append(s, ev.Seq)
		k = append(k, ev.Kind)
	}
	return s, k
}

// notifications collects kontra_task_events payloads on a LISTEN connection of its own.
type notifications struct {
	mu   sync.Mutex
	got  []string
	conn *pgx.Conn
}

func listen(t *testing.T, dsn string) *notifications {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "listen kontra_task_events"); err != nil {
		t.Fatal(err)
	}
	n := &notifications{conn: conn}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			nt, err := conn.WaitForNotification(ctx)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.got = append(n.got, nt.Payload)
			n.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		conn.Close(context.Background())
	})
	return n
}

// waitFor returns the payloads for taskID once there are want of them, or after a few seconds.
func (n *notifications) waitFor(taskID a2a.TaskID, want int) []string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		n.mu.Lock()
		var mine []string
		for _, p := range n.got {
			if strings.HasPrefix(p, string(taskID)+":") {
				mine = append(mine, p)
			}
		}
		n.mu.Unlock()
		if len(mine) >= want || time.Now().After(deadline) {
			// Give a duplicate the chance to show up.
			if len(mine) >= want {
				time.Sleep(200 * time.Millisecond)
				n.mu.Lock()
				mine = mine[:0]
				for _, p := range n.got {
					if strings.HasPrefix(p, string(taskID)+":") {
						mine = append(mine, p)
					}
				}
				n.mu.Unlock()
			}
			return mine
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isNotFound(err error) bool { return errors.Is(err, a2a.ErrTaskNotFound) }
