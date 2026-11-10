package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/store"
)

// hireMessage is a hire's A2A message: a new gate-minted messageId each time, as gatemcp builds it.
func hireMessage(text string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
}

// waitState waits up to 20 s for the task to stand at want.
func (r *resendRig) waitState(id a2a.TaskID, want store.State) *store.TaskRow {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		row, err := r.log.Load(context.Background(), uuid.MustParse(string(id)))
		if err != nil {
			r.t.Fatal(err)
		}
		if row.State == want {
			return row
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("task %s stands at %s, want %s", id, row.State, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestHireActivatesTheOperatorsExtensions (spec §2.4, §12.2, §13.4; D294, D300): a tool call of /mcp has no
// A2A-Extensions. A required URI that the operator's card_extensions entries assert (an operator's own extension,
// carried by its hooks) is activated for the buyer: the hire runs Admission, which is told of the URI in extensions,
// and its task runs; the seller never receives the activation, even though its own card declares the URI (an A2A caller's activation
// would reach it). A reply to that task goes through. A required URI of the seller's own, and the sealed extension's
// whoever asserts it, refuse a hire with ExtensionSupportRequiredError before anything else (no Admission, no task
// row, nothing to the seller), and a reply to a hire task of such a service the same way: what the A2A route refuses
// a request without A2A-Extensions with.
func TestHireActivatesTheOperatorsExtensions(t *testing.T) {
	adm := &recordingAdmission{}
	r := newResendRig(t, adm)
	ctx := context.Background()
	p := principal(buyerA)
	const opExt, own = "https://operator.example.com/ext/audit", "https://seller.example.com/ext/own"
	r.backends.mu.Lock()
	r.backends.info.Extensions = []string{opExt, own}
	r.backends.mu.Unlock()
	var mu sync.Mutex
	var ce CardExtensions
	card := func(c CardExtensions) {
		mu.Lock()
		ce = c
		mu.Unlock()
	}
	h := &Hirer{Handlers: r.handlers(r.lc, NewRegistry()), Log: r.log,
		Extensions: func(svc store.ServiceRef) CardExtensions {
			if svc != r.svc {
				t.Errorf("Extensions asked about %+v", svc)
			}
			mu.Lock()
			defer mu.Unlock()
			return ce
		}}
	asked := func() []hooks.AdmitRequest {
		adm.mu.Lock()
		defer adm.mu.Unlock()
		return append([]hooks.AdmitRequest(nil), adm.got...)
	}
	activations := func() []string {
		var out []string
		for _, rec := range r.agent.recorded() {
			out = append(out, rec.header.Values("A2A-Extensions")...)
		}
		return out
	}

	// The operator's required entry: activated for the buyer, told to Admission, never sent to the seller.
	card(CardExtensions{Required: []string{opExt}, Operator: []string{opExt, "https://operator.example.com/ext/other"}})
	out, err := h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("a"))})
	if err != nil {
		t.Fatalf("a hire of a service the operator's entry requires %s of: %v", opExt, err)
	}
	task := r.waitState(a2a.TaskID(out.Task.ID.String()), store.StateInputRequired)
	if got := asked(); len(got) != 1 || len(got[0].Extensions) != 1 || got[0].Extensions[0] != opExt || got[0].TaskID != out.Task.ID.String() {
		t.Fatalf("Admission was asked %+v, want once with extensions [%s]", got, opExt)
	}
	if _, err := h.Reply(ctx, p, task, a2a.ContentParts{a2a.NewTextPart("the blue one")}); err != nil {
		t.Fatalf("a reply to a task of a service the operator's entry requires an extension of: %v", err)
	}
	r.waitState(a2a.TaskID(task.ID.String()), store.StateCompleted)
	if sent := r.agent.sentMessages(t); len(sent) != 2 {
		t.Fatalf("the seller received %d messages, want the hire's and the reply's", len(sent))
	}
	if a := activations(); len(a) != 0 {
		t.Errorf("the seller received A2A-Extensions %q: an activation the gate made for the buyer", a)
	}

	// A task at input-required, for the refused reply below.
	out, err = h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("b"))})
	if err != nil {
		t.Fatal(err)
	}
	waiting := r.waitState(a2a.TaskID(out.Task.ID.String()), store.StateInputRequired)

	// The seller's own required URI, and the sealed extension's even as the operator's entry: refused before anything.
	tasks, admitted, sent := scalarCount(t, r, `select count(*) from kontra.tasks`), len(asked()), len(r.agent.sentMessages(t))
	for _, c := range []struct {
		card CardExtensions
		want string
	}{
		{CardExtensions{Required: []string{own, opExt}, Operator: []string{opExt}}, own},
		{CardExtensions{Required: []string{sealed.URI}, Operator: []string{sealed.URI}}, sealed.URI},
	} {
		card(c.card)
		_, err = h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("c"))})
		var xe *ExtensionsRequiredError
		if !errors.As(err, &xe) || !errors.Is(err, a2a.ErrExtensionSupportRequired) || len(xe.URIs) != 1 || xe.URIs[0] != c.want {
			t.Errorf("a hire of a service requiring %+v: %v, want ExtensionsRequiredError [%s]", c.card, err, c.want)
		}
		if _, err := h.Reply(ctx, p, waiting, a2a.ContentParts{a2a.NewTextPart("the red one")}); !errors.As(err, &xe) || xe.URIs[0] != c.want {
			t.Errorf("a reply to a task of a service requiring %+v: %v", c.card, err)
		}
	}
	if n := scalarCount(t, r, `select count(*) from kontra.tasks`); n != tasks {
		t.Errorf("the refused hires created %d task rows", n-tasks)
	}
	if n := len(asked()); n != admitted {
		t.Errorf("the refused hires asked Admission %d times", n-admitted)
	}
	time.Sleep(time.Second)
	if n := len(r.agent.sentMessages(t)); n != sent {
		t.Errorf("the refused hires and replies sent the seller %d messages", n-sent)
	}
	if row := r.waitState(a2a.TaskID(waiting.ID.String()), store.StateInputRequired); row.Version != waiting.Version {
		t.Errorf("the refused replies changed the task: version %d, was %d", row.Version, waiting.Version)
	}
}

func scalarCount(t *testing.T, r *resendRig, sql string) int64 {
	t.Helper()
	var n int64
	if err := r.log.DB().QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestHireIdempotencyKeyIsOneTask (spec §12.3 Idempotency, D338): hires with an idempotencyKey carry the messageId the
// key mints. Eight of them with one key and the same arguments, sent at once, make one task, and every one answers it;
// a later one with the same key and other arguments (another resend key) is ErrHireKeyReused, as is one with the same
// key on another service; and of two hires with one fresh key on two services sent at once, one creates its task and
// the other is refused: the work queue looks the key up across services under its lock.
func TestHireIdempotencyKeyIsOneTask(t *testing.T) {
	r := newResendRig(t, &recordingAdmission{})
	ctx := context.Background()
	if _, err := r.log.DB().Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
		values ($1, 'tck2', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config')`, r.svc.TenantID); err != nil {
		t.Fatal(err)
	}
	other := r.svc
	other.Name = "tck2"
	p := principal(buyerA)
	h := &Hirer{Handlers: r.handlers(r.lc, NewRegistry()), Log: r.log,
		Starts: func(svc store.ServiceRef) StartReissuer { return lifecycle.NewQueue(svc, r.log, r.lc, r.queues) }}
	// keyedAs is a keyed hire whose key's lock is lock and whose messageIds are mids (the first the hire's own).
	keyedAs := func(svc store.ServiceRef, lock string, mids []string, args string) (HireOutcome, error) {
		msg := hireMessage("book")
		msg.ID = mids[0]
		return h.Hire(ctx, HireRequest{Principal: p, Service: svc, Message: msg, FirstRequest: sha256.Sum256([]byte(args)),
			Keyed: &store.HireKey{Lock: lock, MessageIDs: mids}})
	}
	keyed := func(svc store.ServiceRef, mid string, args string) (HireOutcome, error) {
		return keyedAs(svc, "lock-"+mid, []string{mid}, args)
	}
	mid := uuid.Must(uuid.NewV7()).String()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = map[uuid.UUID]int{}
		errs []error
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := keyed(r.svc, mid, "same arguments")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[out.Task.ID]++
		}()
	}
	wg.Wait()
	if len(errs) > 0 || len(ids) != 1 {
		t.Fatalf("eight hires with one key: tasks %v, errors %v", ids, errs)
	}
	if n := scalarCount(t, r, `select count(*) from kontra.tasks where first_message_id = '`+mid+`'`); n != 1 {
		t.Fatalf("%d tasks of the key", n)
	}
	if _, err := keyed(r.svc, mid, "other arguments"); !errors.Is(err, lifecycle.ErrHireKeyReused) {
		t.Errorf("the key with other arguments: %v", err)
	}
	if _, err := keyed(other, mid, "same arguments"); !errors.Is(err, lifecycle.ErrHireKeyReused) {
		t.Errorf("the key on another service: %v", err)
	}
	if n := scalarCount(t, r, `select count(*) from kontra.tasks where first_message_id = '`+mid+`'`); n != 1 {
		t.Fatalf("a refused hire created a task: %d tasks of the key", n)
	}

	fresh := uuid.Must(uuid.NewV7()).String()
	results := make(chan error, 2)
	for _, svc := range []store.ServiceRef{r.svc, other} {
		go func() {
			_, err := keyed(svc, fresh, "same arguments")
			results <- err
		}()
	}
	var refused, created int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			created++
		case errors.Is(err, lifecycle.ErrHireKeyReused):
			refused++
		default:
			t.Errorf("a hire of the race: %v", err)
		}
	}
	if created != 1 || refused != 1 || scalarCount(t, r, `select count(*) from kontra.tasks where first_message_id = '`+fresh+`'`) != 1 {
		t.Fatalf("one key on two services at once: %d created, %d refused", created, refused)
	}

	// The next period: the hire mints another messageId and finds the task of the period before by its own; two hires
	// of one key, one in each period, sent at once, take the key's one lock and make one task.
	prior := uuid.Must(uuid.NewV7()).String()
	first, err := keyedAs(r.svc, "lock-periods", []string{prior}, "same arguments")
	if err != nil {
		t.Fatal(err)
	}
	again, err := keyedAs(r.svc, "lock-periods", []string{uuid.Must(uuid.NewV7()).String(), prior}, "same arguments")
	if err != nil || !again.Existing || again.Task.ID != first.Task.ID {
		t.Fatalf("a hire of the next period: %+v, %v", again, err)
	}
	p1, p2 := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	race := make(chan HireOutcome, 2)
	for _, mids := range [][]string{{p1, p2}, {p2, p1}} {
		go func() {
			out, err := keyedAs(r.svc, "lock-across", mids, "same arguments")
			if err != nil {
				t.Error(err)
			}
			race <- out
		}()
	}
	if a, b := <-race, <-race; a.Task == nil || b.Task == nil || a.Task.ID != b.Task.ID {
		t.Fatalf("one key across a period's end, at once: two tasks")
	}

	// After the window, the key's task is not found: the hire is a new task, which its messageId (another period's)
	// tells apart from the first.
	if _, err := r.log.DB().Exec(ctx, `update kontra.tasks set created_at = now() - interval '25 hours' where id = $1`, first.Task.ID); err != nil {
		t.Fatal(err)
	}
	later := uuid.Must(uuid.NewV7()).String()
	out, err := keyedAs(r.svc, "lock-periods", []string{later, uuid.Must(uuid.NewV7()).String()}, "same arguments")
	if err != nil || out.Existing || out.Task.ID == first.Task.ID {
		t.Fatalf("a hire of the key after its window: %+v, %v", out, err)
	}
	if n := scalarCount(t, r, `select count(*) from kontra.tasks where first_message_id = '`+later+`'`); n != 1 {
		t.Fatalf("%d tasks of the later messageId", n)
	}
}
