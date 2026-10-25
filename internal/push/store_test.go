package push

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/medmahmoudi26/kontra/internal/auth"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/ssrf"
	"github.com/medmahmoudi26/kontra/internal/store"
)

func tid(id uuid.UUID) a2a.TaskID { return a2a.TaskID(id.String()) }

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestSaveIsIdempotent: a config without an id is keyed by (task, URL, token), so a2a-go's per-turn re-save
// neither duplicates the row nor starts a second delivery; one with an id is updated in place (spec §10.1).
func TestSaveIsIdempotent(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()
	id := e.newTask(buyerA)

	first, err := cs.Save(ctx, tid(id), pushConfig("http://127.0.0.1:9/hook"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.TaskID != tid(id) || first.Token != secretToken || first.Auth.Credentials != secretCreds {
		t.Fatalf("saved %+v", first)
	}
	for range 3 { // a2a-go re-saves the inline config on every turn
		again, err := cs.Save(ctx, tid(id), pushConfig("http://127.0.0.1:9/hook"))
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID {
			t.Fatalf("re-save got id %s, want %s", again.ID, first.ID)
		}
	}
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 1 {
		t.Fatalf("%d rows after four saves of one config", n)
	}
	if got := sig.list(); len(got) != 1 || got[0] != (lifecycle.PushSignal{ConfigID: first.ID}) {
		t.Fatalf("kontra.push sent %+v, want once for the inserted row", got)
	}
	live, err := cs.LiveConfigIDs(ctx, id)
	if err != nil || len(live) != 1 || live[0] != first.ID {
		t.Fatalf("live configs %v %v: one delivery", live, err)
	}

	// A re-save with new credentials updates the same row, still without a second delivery.
	cfg := pushConfig("http://127.0.0.1:9/hook")
	cfg.Auth.Credentials = "rotated"
	if s, err := cs.Save(ctx, tid(id), cfg); err != nil || s.ID != first.ID {
		t.Fatalf("re-save with new credentials: %+v %v", s, err)
	}
	if got, _ := cs.Get(ctx, tid(id), first.ID); got.Auth.Credentials != "rotated" {
		t.Errorf("the stored credentials are %q", got.Auth.Credentials)
	}
	// Another token is another config.
	other := pushConfig("http://127.0.0.1:9/hook")
	other.Token = "another-token"
	o, err := cs.Save(ctx, tid(id), other)
	if err != nil || o.ID == first.ID {
		t.Fatalf("a config with another token: %+v %v", o, err)
	}
	// An explicit id is the key: saving it again with another URL updates that row.
	named := &a2a.PushConfig{ID: "cfg-1", URL: "http://127.0.0.1:9/a"}
	if _, err := cs.Save(ctx, tid(id), named); err != nil {
		t.Fatal(err)
	}
	named.URL = "http://127.0.0.1:9/b"
	if s, err := cs.Save(ctx, tid(id), named); err != nil || s.ID != "cfg-1" || s.URL != "http://127.0.0.1:9/b" {
		t.Fatalf("update by id: %+v %v", s, err)
	}
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 3 {
		t.Fatalf("%d rows, want 3", n)
	}
	if n := len(sig.list()); n != 3 {
		t.Fatalf("%d kontra.push signals, want one per inserted row (3)", n)
	}
}

// TestSaveInlineThenResave: the work queue saves the inline config in its transaction (SaveInline); a2a-go's
// later re-save of the same config at dispatch finds that row (one row, one delivery).
func TestSaveInlineThenResave(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()

	var (
		id       uuid.UUID
		cid      string
		inserted bool
	)
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		id = e.newTaskTx(tx, buyerA)
		var err error
		cid, inserted, err = cs.SaveInline(ctx, tx, id, buyerA, pushConfig("http://127.0.0.1:9/hook"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !inserted || cid == "" {
		t.Fatalf("SaveInline: %q %v", cid, inserted)
	}
	if seq, _ := e.deliveredSeq(id, cid); seq != 0 {
		t.Errorf("a new task's inline config starts at %d, want 0 (its first delivery is the task row)", seq)
	}
	if len(sig.list()) != 0 {
		t.Error("SaveInline sent kontra.push: the work queue sends it (or the workflow lists configs at start)")
	}
	s, err := cs.Save(ctx, tid(id), pushConfig("http://127.0.0.1:9/hook"))
	if err != nil || s.ID != cid {
		t.Fatalf("a2a-go's re-save: %+v %v", s, err)
	}
	if len(sig.list()) != 0 || e.count(`select count(*) from kontra.push_configs where task_id = $1`, id) != 1 {
		t.Fatal("the re-save inserted a row or started a delivery")
	}
	// A second SaveInline of the same config (a resend that reached Write) inserts nothing, and has
	// nothing to announce.
	if c2, ins := e.saveInline(cs, id, buyerA, pushConfig("http://127.0.0.1:9/hook")); c2 != "" || ins {
		t.Fatalf("SaveInline again: %q %v", c2, ins)
	}
	// A rolled-back transaction leaves nothing (a refused request stores nothing, spec §10.1).
	_ = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, _, err := cs.SaveInline(ctx, tx, id, buyerA, pushConfig("http://127.0.0.1:9/other")); err != nil {
			t.Fatal(err)
		}
		return errors.New("rollback")
	})
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 1 {
		t.Fatalf("%d rows after a rolled-back SaveInline", n)
	}
	// DeleteInline removes what SaveInline inserted for a refused follow-up, without kontra.push.
	c3, _ := e.saveInline(cs, id, buyerA, pushConfig("http://127.0.0.1:9/refused"))
	if err := cs.DeleteInline(ctx, id, c3); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 1 || len(sig.list()) != 0 {
		t.Fatalf("after DeleteInline: %d rows, %d signals", n, len(sig.list()))
	}
	// SaveInline refuses a private literal even offline, and the anonymous principal.
	strict := NewConfigStore(e.pool, sig, ssrf.Policy{}, nil)
	err = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, _, err := strict.SaveInline(ctx, tx, id, buyerA, pushConfig("https://10.0.0.1/hook"))
		return err
	})
	if !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("SaveInline of a private literal: %v", err)
	}
	err = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, _, err := cs.SaveInline(ctx, tx, id, anon, pushConfig("http://127.0.0.1:9/hook"))
		return err
	})
	if !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("SaveInline for the anonymous principal: %v", err)
	}
}

// TestInitialPosition: a config set on a task that has progressed starts at the task's current state: its
// first delivery is the snapshot at the last task, status or artifact row.
func TestInitialPosition(t *testing.T) {
	e := newEnv(t)
	cs := NewConfigStore(e.pool, nil, private, nil)
	id := e.newTask(buyerA)
	e.status(id, store.StateWorking)           // 2
	art := e.artifact(id, "a1", "part", false) // 3
	e.metadata(id, map[string]any{"x": "y"})   // 4
	s, err := cs.Save(context.Background(), tid(id), pushConfig("http://127.0.0.1:9/hook"))
	if err != nil {
		t.Fatal(err)
	}
	if seq, _ := e.deliveredSeq(id, s.ID); seq != art-1 {
		t.Fatalf("delivered_seq %d, want %d (just before the last carried row)", seq, art-1)
	}
}

// TestLimits: the 11th config of a task and the 257th delivering config of a buyer are InvalidParams
// (spec §10.1); a failed config, and one whose terminal snapshot was delivered, are not delivering.
func TestLimits(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()

	id := e.newTask(buyerA)
	for i := range MaxPerTask {
		if _, err := cs.Save(ctx, tid(id), &a2a.PushConfig{URL: fmt.Sprintf("http://127.0.0.1:9/h%d", i)}); err != nil {
			t.Fatalf("config %d: %v", i+1, err)
		}
	}
	_, err := cs.Save(ctx, tid(id), &a2a.PushConfig{URL: "http://127.0.0.1:9/h11"})
	if !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("the 11th config: %v, want InvalidParams", err)
	}
	_, _, err = func() (string, bool, error) {
		var c string
		var ins bool
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			var err error
			c, ins, err = cs.SaveInline(ctx, tx, id, buyerA, &a2a.PushConfig{URL: "http://127.0.0.1:9/inline11"})
			return err
		})
		return c, ins, err
	}()
	if !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("the 11th config inline: %v", err)
	}
	// Re-saving an existing config at the limit is an update, not an 11th.
	if _, err := cs.Save(ctx, tid(id), &a2a.PushConfig{URL: "http://127.0.0.1:9/h3"}); err != nil {
		t.Fatalf("re-save at the limit: %v", err)
	}
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != MaxPerTask {
		t.Fatalf("%d configs on the task", n)
	}

	// Buyer A has 10 delivering configs; fill up to 256 over more tasks.
	need := MaxPerBuyer - MaxPerTask
	var last uuid.UUID
	for need > 0 {
		last = e.newTask(buyerA)
		for i := 0; i < MaxPerTask && need > 0; i++ {
			if _, err := cs.Save(ctx, tid(last), &a2a.PushConfig{URL: fmt.Sprintf("http://127.0.0.1:9/b%d", i)}); err != nil {
				t.Fatalf("delivering config %d: %v", MaxPerBuyer-need+1, err)
			}
			need--
		}
	}
	next := e.newTask(buyerA)
	if _, err := cs.Save(ctx, tid(next), &a2a.PushConfig{URL: "http://127.0.0.1:9/257"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("the 257th delivering config: %v, want InvalidParams", err)
	}
	// Another buyer is not affected.
	idB := e.newTask(buyerB)
	if _, err := cs.Save(ctx, tid(idB), &a2a.PushConfig{URL: "http://127.0.0.1:9/b"}); err != nil {
		t.Fatalf("buyer B: %v", err)
	}
	// A failed config stops delivering: one slot frees.
	e.exec(`update kontra.push_configs set failed_at = now() where task_id = $1 and config->>'url' = 'http://127.0.0.1:9/h0'`, id)
	if _, err := cs.Save(ctx, tid(next), &a2a.PushConfig{URL: "http://127.0.0.1:9/257"}); err != nil {
		t.Fatalf("after a config failed: %v", err)
	}
	if _, err := cs.Save(ctx, tid(next), &a2a.PushConfig{URL: "http://127.0.0.1:9/258"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("over the limit again: %v", err)
	}
	// A config whose terminal snapshot was delivered is done: the task ends, the deliveries reach its
	// terminal row, and the buyer has room again.
	term := e.status(id, store.StateCompleted)
	e.exec(`update kontra.push_configs set delivered_seq = $2 where task_id = $1`, id, term)
	if _, err := cs.Save(ctx, tid(next), &a2a.PushConfig{URL: "http://127.0.0.1:9/258"}); err != nil {
		t.Fatalf("after a task's configs delivered its terminal snapshot: %v", err)
	}
	// A terminal task whose terminal snapshot is not delivered yet still counts.
	e.exec(`update kontra.push_configs set delivered_seq = $2 - 1 where task_id = $1`, id, term)
	if _, err := cs.Save(ctx, tid(next), &a2a.PushConfig{URL: "http://127.0.0.1:9/259"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("a pending terminal snapshot must count: %v", err)
	}
}

// TestAnonymousIsRefused: the anonymous principal cannot set push configs (spec §2.3), whether the context
// carries it or the task is an anonymous one.
func TestAnonymousIsRefused(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()
	id := e.newTask(anon)
	if _, err := cs.Save(ctx, tid(id), pushConfig("http://127.0.0.1:9/hook")); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("Save on an anonymous task: %v", err)
	}
	mine := e.newTask(buyerA)
	actx := auth.WithPrincipal(ctx, auth.Anonymous(anon.BuyerID))
	if _, err := cs.Save(actx, tid(mine), pushConfig("http://127.0.0.1:9/hook")); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("Save by the anonymous principal: %v", err)
	}
	if n := e.count(`select count(*) from kontra.push_configs`); n != 0 || len(sig.list()) != 0 {
		t.Fatalf("%d rows, %d signals after refusals", n, len(sig.list()))
	}
	// Reading stays possible: a2a-go's processor lists configs after every event of every dispatch.
	if l, err := cs.List(ctx, tid(id)); err != nil || len(l) != 0 {
		t.Errorf("List on an anonymous task: %v %v", l, err)
	}
}

// TestGetListDelete covers the rest of a2a-go's ConfigStore: a missing config is TaskNotFound, deleting a
// missing config is not an error, a Delete that removed a row sends kontra.push {deleted}, and a task that
// does not exist is TaskNotFound.
func TestGetListDelete(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()
	id := e.newTask(buyerA)
	a, _ := cs.Save(ctx, tid(id), &a2a.PushConfig{ID: "a", URL: "http://127.0.0.1:9/a", Token: "ta"})
	b, _ := cs.Save(ctx, tid(id), &a2a.PushConfig{URL: "http://127.0.0.1:9/b"})

	got, err := cs.Get(ctx, tid(id), "a")
	if err != nil || got.ID != "a" || got.URL != "http://127.0.0.1:9/a" || got.Token != "ta" || got.TaskID != tid(id) {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if _, err := cs.Get(ctx, tid(id), "missing"); !errors.Is(err, a2a.ErrTaskNotFound) || !errors.Is(err, a2apush.ErrPushConfigNotFound) {
		t.Errorf("Get of a missing config: %v", err)
	}
	if _, err := cs.Get(ctx, "not-a-task", "a"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("Get on a malformed task id: %v", err)
	}
	list, err := cs.List(ctx, tid(id))
	if err != nil || len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("List: %+v %v", list, err)
	}
	if l, err := cs.List(ctx, tid(e.newTask(buyerA))); err != nil || l == nil || len(l) != 0 {
		t.Errorf("List of a task without configs: %#v %v (want an empty, non-nil slice)", l, err)
	}
	if _, err := cs.Save(ctx, tid(uuid.Must(uuid.NewV7())), pushConfig("http://127.0.0.1:9/x")); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("Save on a task that does not exist: %v", err)
	}

	before := len(sig.list())
	if err := cs.Delete(ctx, tid(id), "missing"); err != nil {
		t.Errorf("Delete of a missing config: %v", err)
	}
	if len(sig.list()) != before {
		t.Error("Delete of a missing config sent kontra.push")
	}
	if err := cs.Delete(ctx, tid(id), "a"); err != nil {
		t.Fatal(err)
	}
	if s := sig.list(); s[len(s)-1] != (lifecycle.PushSignal{ConfigID: "a", Deleted: true}) {
		t.Errorf("Delete sent %+v", s[len(s)-1])
	}
	if _, err := cs.Get(ctx, tid(id), "a"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}

	// LiveConfigIDs leaves failed configs out; DeleteAll announces every deletion.
	c, _ := cs.Save(ctx, tid(id), &a2a.PushConfig{URL: "http://127.0.0.1:9/c"})
	e.exec(`update kontra.push_configs set failed_at = now() where task_id = $1 and id = $2`, id, c.ID)
	live, err := cs.LiveConfigIDs(ctx, id)
	if err != nil || len(live) != 1 || live[0] != b.ID {
		t.Fatalf("LiveConfigIDs: %v %v", live, err)
	}
	before = len(sig.list())
	if err := cs.DeleteAll(ctx, tid(id)); err != nil {
		t.Fatal(err)
	}
	if s := sig.list()[before:]; len(s) != 2 || !s[0].Deleted || !s[1].Deleted {
		t.Errorf("DeleteAll sent %+v", s)
	}
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 0 {
		t.Errorf("%d configs after DeleteAll", n)
	}
}

// TestSSRFWebhookRefusedWhenSet is ssrf_webhook's set-time half through the store (spec §16, D60): the
// named targets and a name resolving to a private address are InvalidParams, nothing is stored, no
// kontra.push is sent, and no connection is made to any of them. A redirecting URL is accepted.
func TestSSRFWebhookRefusedWhenSet(t *testing.T) {
	e := newEnv(t)
	port, conns := countingListener(t)
	sig := &signals{}
	res := fakeResolver{
		"localhost":        {"127.0.0.1", "::1"},
		"private.example":  {"127.0.0.1"},
		"redirect.example": {"93.184.215.14"},
	}
	cs := NewConfigStore(e.pool, sig, ssrf.Policy{Resolver: res}, nil)
	ctx := context.Background()
	id := e.newTask(buyerA)
	for _, u := range []string{
		"https://169.254.169.254/hook",
		"https://10.0.0.1/hook",
		"https://localhost:" + port + "/hook",
		"https://[::ffff:10.0.0.1]/hook",
		"https://[64:ff9b::a00:1]/hook",
		"https://private.example:" + port + "/hook",
		"http://127.0.0.1:" + port + "/hook",
	} {
		if _, err := cs.Save(ctx, tid(id), pushConfig(u)); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("Save(%s) = %v, want InvalidParams", u, err)
		}
	}
	if n := e.count(`select count(*) from kontra.push_configs`); n != 0 || len(sig.list()) != 0 {
		t.Fatalf("refused configs left %d rows and %d signals", n, len(sig.list()))
	}
	if _, err := cs.Save(ctx, tid(id), pushConfig("https://redirect.example/302")); err != nil {
		t.Fatalf("a redirecting URL must be accepted when set (D60): %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Fatalf("setting the configs opened %d connections", n)
	}
}

// TestResaveSkipsTheDNSCheck: a config re-saved with the URL it already has is not checked again, so a
// name that stopped resolving (or resolves privately now) never fails a2a-go's per-turn re-save; a changed
// URL is checked.
func TestResaveSkipsTheDNSCheck(t *testing.T) {
	e := newEnv(t)
	res := fakeResolver{"hooks.example": {"93.184.215.14"}}
	cs := NewConfigStore(e.pool, nil, ssrf.Policy{Resolver: res}, nil)
	ctx := context.Background()
	id := e.newTask(buyerA)
	s, err := cs.Save(ctx, tid(id), pushConfig("https://hooks.example/hook"))
	if err != nil {
		t.Fatal(err)
	}
	res["hooks.example"] = []string{"10.0.0.7"}
	if again, err := cs.Save(ctx, tid(id), pushConfig("https://hooks.example/hook")); err != nil || again.ID != s.ID {
		t.Fatalf("re-save after a DNS change: %+v %v", again, err)
	}
	named := &a2a.PushConfig{ID: "n", URL: "https://93.184.215.14/a"}
	if _, err := cs.Save(ctx, tid(id), named); err != nil {
		t.Fatal(err)
	}
	named.URL = "https://hooks.example/b" // now private
	if _, err := cs.Save(ctx, tid(id), named); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("an update to a private URL: %v", err)
	}
	if got, _ := cs.Get(ctx, tid(id), "n"); got.URL != "https://93.184.215.14/a" {
		t.Errorf("the refused update changed the URL to %s", got.URL)
	}
}

// TestConfigsOnEndedTasksAreDone: a config set on a task that has ended is done when it is set (an ended
// task has nothing left to deliver, and once its workflow has closed nothing would deliver it): it is
// stored, listed and deletable, but it is not delivering, so configs set on ended tasks never fill the
// buyer's 256 and refuse a config on a live task.
func TestConfigsOnEndedTasksAreDone(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()
	made := 0
	for made < MaxPerBuyer+MaxPerTask {
		id := e.newTask(buyerA)
		e.status(id, store.StateWorking)
		term := e.status(id, store.StateCompleted)
		e.metadata(id, map[string]any{"x": "y"}) // rows after the terminal one are not delivered
		for i := 0; i < MaxPerTask; i++ {
			s, err := cs.Save(ctx, tid(id), &a2a.PushConfig{URL: fmt.Sprintf("http://127.0.0.1:9/x%d", i)})
			if err != nil {
				t.Fatalf("config %d, on an ended task: %v", made+1, err)
			}
			if seq, failed := e.deliveredSeq(id, s.ID); seq != term || failed {
				t.Fatalf("a config set on an ended task: delivered_seq %d failed %v, want done at the terminal row %d", seq, failed, term)
			}
			made++
		}
		if l, err := cs.List(ctx, tid(id)); err != nil || len(l) != MaxPerTask {
			t.Fatalf("List: %d configs, %v", len(l), err)
		}
	}
	if n := len(sig.list()); n != 0 {
		t.Errorf("%d kontra.push signals for configs that have nothing to deliver", n)
	}
	live := e.newTask(buyerA)
	e.status(live, store.StateWorking)
	if _, err := cs.Save(ctx, tid(live), &a2a.PushConfig{URL: "http://127.0.0.1:9/live"}); err != nil {
		t.Fatalf("a config on a live task after %d configs on ended tasks: %v", made, err)
	}
	if got := sig.list(); len(got) != 1 {
		t.Errorf("kontra.push %+v: once for the live task's config", got)
	}
	// A push_webhook for one of them ends at once without a delivery.
	d := NewDeliverer(e.log, private, fast())
	rcv := newReceiver(t, nil)
	id := e.newTask(buyerA)
	e.status(id, store.StateCompleted)
	s, err := cs.Save(ctx, tid(id), pushConfig(rcv.url("/hook")))
	if err != nil {
		t.Fatal(err)
	}
	o := waitOut(t, runPush(ctx, d, id, s.ID), 5*time.Second)
	if o.err != nil || o.res.Stopped != StoppedTerminal || len(rcv.requests()) != 0 {
		t.Fatalf("push_webhook of a config set after the end: %+v %v, %d requests", o.res, o.err, len(rcv.requests()))
	}
	// The same through the work queue's inline save (a follow-up racing the task's end).
	ended := e.newTask(buyerA)
	term := e.status(ended, store.StateCompleted)
	cid, inserted := e.saveInline(cs, ended, buyerA, &a2a.PushConfig{URL: "http://127.0.0.1:9/inline"})
	if seq, _ := e.deliveredSeq(ended, cid); !inserted || seq != term {
		t.Fatalf("SaveInline on an ended task: %q inserted %v, delivered_seq %d, want %d", cid, inserted, seq, term)
	}
}

// TestSetAgainRevivesAFailedConfig: a failed config set again on a task that has not ended delivers again,
// from where it stopped: its failure is cleared, it is live, kontra.push announces it, and it counts
// against the buyer's limit again. On an ended task it stays as it is (there is nothing left to deliver).
func TestSetAgainRevivesAFailedConfig(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	ctx := context.Background()
	id := e.newTask(buyerA)
	e.status(id, store.StateWorking)
	if _, err := cs.Save(ctx, tid(id), &a2a.PushConfig{ID: "c", URL: "http://127.0.0.1:9/broken"}); err != nil {
		t.Fatal(err)
	}
	e.status(id, store.StateWorking)
	e.exec(`update kontra.push_configs set failed_at = now(), delivered_seq = 2 where task_id = $1`, id)
	before := len(sig.list())
	s, err := cs.Save(ctx, tid(id), &a2a.PushConfig{ID: "c", URL: "http://127.0.0.1:9/fixed"})
	if err != nil || s.URL != "http://127.0.0.1:9/fixed" {
		t.Fatalf("set again: %+v %v", s, err)
	}
	seq, failed := e.deliveredSeq(id, "c")
	if failed || seq != 2 {
		t.Fatalf("after setting a failed config again: failed %v, delivered_seq %d (want live, from 2)", failed, seq)
	}
	if live, _ := cs.LiveConfigIDs(ctx, id); len(live) != 1 || live[0] != "c" {
		t.Errorf("live configs %v", live)
	}
	if got := sig.list()[before:]; len(got) != 1 || got[0] != (lifecycle.PushSignal{ConfigID: "c"}) {
		t.Errorf("kontra.push %+v: the revived config must be announced", got)
	}
	// The same through the work queue's inline save: announced (its id), not inserted.
	e.exec(`update kontra.push_configs set failed_at = now() where task_id = $1`, id)
	if cid, ins := e.saveInline(cs, id, buyerA, &a2a.PushConfig{ID: "c", URL: "http://127.0.0.1:9/fixed-again"}); cid != "c" || ins {
		t.Fatalf("SaveInline of a failed config: %q inserted %v, want announced and not inserted", cid, ins)
	}
	if _, failed := e.deliveredSeq(id, "c"); failed {
		t.Error("SaveInline left the config failed")
	}
	// A revived config counts again: at the buyer's limit it is refused, and stays failed.
	e.exec(`update kontra.push_configs set failed_at = now() where task_id = $1`, id)
	for n := 0; n < MaxPerBuyer; {
		other := e.newTask(buyerA)
		for i := 0; i < MaxPerTask && n < MaxPerBuyer; i++ {
			if _, err := cs.Save(ctx, tid(other), &a2a.PushConfig{URL: fmt.Sprintf("http://127.0.0.1:9/f%d", i)}); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	if _, err := cs.Save(ctx, tid(id), &a2a.PushConfig{ID: "c", URL: "http://127.0.0.1:9/fixed"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("reviving a config over the buyer's limit: %v", err)
	}
	if _, failed := e.deliveredSeq(id, "c"); !failed {
		t.Error("a refused revival cleared the failure")
	}
	// On an ended task a failed config set again stays failed, and nothing is announced.
	ended := e.newTask(buyerB)
	if _, err := cs.Save(ctx, tid(ended), &a2a.PushConfig{ID: "d", URL: "http://127.0.0.1:9/broken"}); err != nil {
		t.Fatal(err)
	}
	e.exec(`update kontra.push_configs set failed_at = now() where task_id = $1`, ended)
	e.status(ended, store.StateCompleted)
	before = len(sig.list())
	if _, err := cs.Save(ctx, tid(ended), &a2a.PushConfig{ID: "d", URL: "http://127.0.0.1:9/fixed"}); err != nil {
		t.Fatal(err)
	}
	if _, failed := e.deliveredSeq(ended, "d"); !failed || len(sig.list()) != before {
		t.Errorf("a failed config of an ended task set again: failed %v, %d signals", failed, len(sig.list())-before)
	}
}

// TestCheckSet: a config set again on an existing task with the URL it already has is not resolved again
// (a follow-up that re-sends its inline config is never refused for a DNS change); a new URL is checked.
func TestCheckSet(t *testing.T) {
	e := newEnv(t)
	res := fakeResolver{"hooks.example": {"93.184.215.14"}}
	pol := ssrf.Policy{Resolver: res}
	cs := NewConfigStore(e.pool, nil, pol, nil)
	ctx := context.Background()
	id := e.newTask(buyerA)
	if _, err := cs.Save(ctx, tid(id), pushConfig("https://hooks.example/hook")); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Save(ctx, tid(id), &a2a.PushConfig{ID: "n", URL: "https://hooks.example/named"}); err != nil {
		t.Fatal(err)
	}
	res["hooks.example"] = []string{"10.0.0.7"} // the name now resolves privately
	for _, c := range []*a2a.PushConfig{pushConfig("https://hooks.example/hook"), {ID: "n", URL: "https://hooks.example/named"}} {
		if err := CheckSet(ctx, e.pool, id, c, pol); err != nil {
			t.Errorf("CheckSet of an unchanged config %+v: %v", c.URL, err)
		}
	}
	for _, c := range []*a2a.PushConfig{
		pushConfig("https://hooks.example/new"),           // a new (URL, token)
		{ID: "n", URL: "https://hooks.example/changed"},   // a changed URL
		{ID: "other", URL: "https://hooks.example/named"}, // another config with that URL
	} {
		if err := CheckSet(ctx, e.pool, id, c, pol); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("CheckSet of %+v: %v, want InvalidParams", c.URL, err)
		}
	}
	// On another task the URL is new.
	if err := CheckSet(ctx, e.pool, e.newTask(buyerA), pushConfig("https://hooks.example/hook"), pol); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("CheckSet on another task: %v", err)
	}
}

// TestDeliveringCountReachesTheBuyersTasksByIndex: the per-buyer count every new config runs under the
// buyer's lock reaches the buyer's tasks by an index on (tenant_id, service, buyer_id) and their configs by
// primary key, never by scanning every buyer's tasks or configs.
func TestDeliveringCountReachesTheBuyersTasksByIndex(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 50,000 tasks of 1,000 buyers, a push config on every tenth.
	e.exec(`insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state, version, snapshot, status_at)
		select ('0192f0b4-0000-7000-8000-' || lpad(to_hex(g), 12, '0'))::uuid, $1, 'hello', 'a2a',
			('0192f0b4-0000-7000-9000-' || lpad(to_hex(g % 1000), 12, '0'))::uuid, 'key:x', 'api_key', 'c', 'working', 1, '{}'::json, now()
		from generate_series(1, 50000) g`, e.svc.TenantID)
	e.exec(`insert into kontra.task_events (task_id, seq, kind, payload) select id, 1, 'task', '{}'::json from kontra.tasks`)
	e.exec(`insert into kontra.push_configs (task_id, id, config)
		select id, 'c', '{"url":"https://x/"}'::jsonb from kontra.tasks where substr(id::text, 36, 1) = '0'`)
	e.exec(`analyze`)
	buyer := uuid.MustParse("0192f0b4-0000-7000-9000-000000000008") // 50 tasks, about half with a config
	var n int
	if err := e.pool.QueryRow(ctx, deliveringSQL, buyer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var want int
	if err := e.pool.QueryRow(ctx, `select count(*) from kontra.push_configs p join kontra.tasks t on t.id = p.task_id
		where t.buyer_id = $1`, buyer).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if n != want || n == 0 {
		t.Fatalf("delivering count %d, want %d", n, want)
	}
	rows, err := e.pool.Query(ctx, `explain (analyze, costs off) `+deliveringSQL, buyer)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	if strings.Contains(text, "Seq Scan on tasks") || strings.Contains(text, "Seq Scan on push_configs") ||
		!strings.Contains(text, "buyer_id = ") {
		t.Fatalf("the per-buyer count scans every buyer's tasks or configs:\n%s", text)
	}
	t.Logf("plan:\n%s", text)
}
