package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// TestVersionConflict is version_conflict (spec/relay.md §5.2, §16; D60): two Store.Updates with the same
// PrevVersion from two replicas, round after round, on a task the gate created and holds at
// input-required: exactly one commits, the other gets ErrConcurrentModification (the PRD's "409": no route
// writes by version, D60), the log's seqs 1..N have no gap and equal the task's version, and both replicas
// then serve the winners' writes through GetTask. The two "replicas" here are two store instances, each on
// its own connection pool, over the harness's database: Store.Update is not reachable from the gate's HTTP
// surface, which is why D60 states this test at the store.
func TestVersionConflict(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 1, Interval: 10 * time.Millisecond, Streaming: true}})
	ctx := ctxFor(t, 3*time.Minute)
	res, err := h.BuyerA().Client(t, h.Gates[0], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" version_conflict"))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage answered %#v, want an input-required task", res)
	}

	var (
		tenantID uuid.UUID
		owner    store.Owner
	)
	if err := h.Pool.QueryRow(ctx, `select tenant_id, buyer_id, principal, via from kontra.tasks where id = $1`, string(task.ID)).
		Scan(&tenantID, &owner.BuyerID, &owner.Principal, &owner.Via); err != nil {
		t.Fatal(err)
	}
	owner.TenantID = &tenantID
	svc, err := h.Log().LoadService(ctx, tenantID, "slow")
	if err != nil {
		t.Fatal(err)
	}
	pool2, err := pgxpool.New(ctx, h.PostgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	ownerOf := func(context.Context) (store.Owner, error) { return owner, nil }
	replicas := [2]*store.Store{store.New(h.Pool, svc, ownerOf, nil, nil), store.New(pool2, svc, ownerOf, nil, nil)}

	const rounds = 25
	wins := [2]int{}
	for i := range rounds {
		st, err := replicas[i%2].Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		ev := a2a.NewArtifactEvent(st.Task, a2a.NewTextPart(fmt.Sprintf("round %02d", i)))
		next, err := a2aevent.ApplyUpdate(st.Task, ev)
		if err != nil {
			t.Fatal(err)
		}
		var (
			errs [2]error
			done = make(chan int, 2)
			go1  = make(chan struct{})
		)
		for k := range replicas {
			go func() {
				<-go1
				_, errs[k] = replicas[k].Update(ctx, &taskstore.UpdateRequest{Task: next, Event: ev, PrevTask: st.Task, PrevVersion: st.Version})
				done <- k
			}()
		}
		close(go1)
		<-done
		<-done
		committed := 0
		for k, e := range errs {
			switch {
			case e == nil:
				committed++
				wins[k]++
			case errors.Is(e, taskstore.ErrConcurrentModification):
			default:
				t.Fatalf("round %d, replica %d: %v, want nil or ErrConcurrentModification", i, k, e)
			}
		}
		if committed != 1 {
			t.Fatalf("round %d: %d of two updates at version %d committed (%v), want exactly one", i, committed, st.Version, errs)
		}
	}
	t.Logf("updates won by replica: %v", wins)

	var version int64
	if err := h.Pool.QueryRow(ctx, `select version from kontra.tasks where id = $1`, string(task.ID)).Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := h.Rows(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if r.Seq != int64(i+1) {
			t.Fatalf("the log has a gap: row %d is seq %d", i+1, r.Seq)
		}
	}
	if int64(len(rows)) != version {
		t.Fatalf("the log has %d rows, the task is at version %d: seq = version", len(rows), version)
	}
	for _, g := range h.Gates {
		got, err := h.BuyerA().Client(t, g, "").GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range got.Artifacts {
			for _, p := range a.Parts {
				if len(p.Text()) == 8 && p.Text()[:6] == "round " {
					n++
				}
			}
		}
		if n != rounds {
			t.Fatalf("GetTask on %s shows %d of the %d committed writes", g.Name, n, rounds)
		}
	}
}
