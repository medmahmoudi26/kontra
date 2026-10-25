package lifecycle

import (
	"errors"
	"net"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/sdk/client"

	"github.com/medmahmoudi26/kontra/internal/store"
)

// unreachableTemporal is a client whose every call fails at once with a refused connection: Temporal is
// down. The address is a port nothing listens on.
func unreachableTemporal(t *testing.T) *Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c, err := client.NewLazyClient(client.Options{HostPort: addr, Namespace: "kontra"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return NewClient(c)
}

// Write fills the SSE writer's StreamStart only when it returns the task: an Update that is not accepted
// (here Temporal is unreachable) leaves it empty after the task row or the work item was committed, so
// the writer can tell an accepted turn from a refused one whatever a2a-go reports after Write (spec §9).
func TestStreamStartOnlyWhenWriteAcceptsTheTurn(t *testing.T) {
	e := newPGEnv(t)
	q := NewQueue(e.svc, e.log, unreachableTemporal(t), QueueOptions{Owner: ownerFrom, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}})

	cctx, pre := caller(true)
	var ss StreamStart
	_, err := q.Write(WithStreamStart(cctx, &ss), payloadOf(requestOf("m-1", "hello")))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a new task with Temporal down: %v", err)
	}
	if n := e.count(`select count(*) from kontra.tasks where id = $1`, pre); n != 1 {
		t.Fatalf("%d task rows: the task row is committed before the start", n)
	}
	if ss != (StreamStart{}) {
		t.Errorf("a new task whose start failed filled %+v", ss)
	}

	id := e.task(store.StateInputRequired, nil)
	fctx, _ := caller(false)
	req := requestOf("m-2", "more")
	req.Message.TaskID = a2a.TaskID(id.String())
	ss = StreamStart{}
	_, err = q.Write(WithStreamStart(fctx, &ss), payloadOf(req))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a follow-up with Temporal down: %v", err)
	}
	if n := e.count(`select count(*) from kontra.work_items where task_id = $1`, id); n != 1 {
		t.Fatalf("%d work items: the follow-up's item is committed before the Update", n)
	}
	if ss != (StreamStart{}) {
		t.Errorf("a follow-up whose Update was not accepted filled %+v", ss)
	}
}
