package anon

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// blob writes a blobs row of the task (a ref of the table's shape).
func (e *pgEnv) blob(tenant, task uuid.UUID) {
	e.t.Helper()
	var b [16]byte
	_, _ = rand.Read(b[:])
	ref := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	e.exec(`insert into kontra.blobs (ref, tenant_id, task_id, size) values ($1, $2, $3, 10)`, ref, tenant, task)
}

// purgeDue makes the tenant expired purge_after (lim's) and more ago.
func (e *pgEnv) purgeDue(tenant uuid.UUID) {
	e.t.Helper()
	e.exec(`update kontra.tenants set expires_at = now() - interval '2 hours' where id = $1`, tenant)
}

// T §6.1, D167 (M4 WP-03 review, blocker): the purge deletes the bucket objects of every task of the tenant, by prefix,
// whether or not it has a blobs row: an upstream part stored for an event that was then refused (over parts_bytes) has
// an object and no row. Its rows go only after its objects.
func TestPurgeDeletesTheObjectsOfEveryTask(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	lim := Defaults
	lim.PurgeAfter = time.Hour
	tn := e.mustOpen(lim, OpenRequest{Key: newKey(1), Network: netOf("203.0.113.9"), Services: svcs("a")})
	refused := e.insertTask(tn.ID, "a", "failed", "anonymous") // an object of a refused event: no blobs row
	stored := e.insertTask(tn.ID, "a", "completed", "anonymous")
	e.blob(tn.ID, stored)
	e.purgeDue(tn.ID)
	objects := &fakeObjects{}
	n, err := NewSweeper(e.pool, lim, objects, nil).Purge(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Purge = %d %v", n, err)
	}
	if left := scalar[int](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1`, tn.ID); left != 0 {
		t.Fatalf("%d task rows kept", left)
	}
	for _, task := range []uuid.UUID{refused, stored} {
		if !slices.Contains(objects.calls, task) {
			t.Errorf("the rows of task %s were purged, its bucket objects never deleted (DeleteTask calls %v)", task, objects.calls)
		}
	}
}

// T §6.1 (M4 WP-03 review, lock order): an admission takes the key's row, then its tenant's (enroll.lockKey, then the
// anonymous tenant, then anon.Open's resume). A purge of that tenant meanwhile neither deadlocks with it nor waits on
// it: it leaves the tenant to its next pass, which purges it once the admission committed. Both with the tenant's
// services already disabled by Expire and with Expire never run (the purge then ends the tenant itself).
func TestPurgeNeverWaitsOnAnAdmissionOfItsKey(t *testing.T) {
	for _, c := range []struct {
		name    string
		expired bool
	}{{"expired before", true}, {"never expired", false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newPGEnv(t)
			ctx := context.Background()
			lim := Defaults
			lim.PurgeAfter = time.Hour
			k := newKey(3)
			tn := e.mustOpen(lim, OpenRequest{Key: k, Network: netOf("203.0.113.11"), Services: svcs("a")})
			e.purgeDue(tn.ID)
			sw := NewSweeper(e.pool, lim, nil, nil)
			if c.expired {
				if n, err := sw.Expire(ctx); err != nil || n != 1 {
					t.Fatalf("Expire = %d %v", n, err)
				}
			}

			// The admission: its transaction holds the key's row (enroll.lockKey).
			adm, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer adm.Rollback(context.Background())
			if _, err := adm.Exec(ctx, `select 1 from kontra.tunnel_keys where key_id = $1 for update`, k.ID); err != nil {
				t.Fatal(err)
			}
			// A purge pass starts: it either returns (the tenant left for later) or waits on the admission.
			type result struct {
				n   int
				err error
			}
			done := make(chan result, 1)
			go func() {
				n, err := sw.Purge(ctx)
				done <- result{n, err}
			}()
			var first *result
			deadline := time.Now().Add(20 * time.Second)
			for first == nil {
				select {
				case r := <-done:
					first = &r
				default:
				}
				if first != nil || scalar[int](t, e.pool, `select count(*) from pg_stat_activity
					where datname = current_database() and wait_event_type = 'Lock'`) > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the purge neither returned nor waited")
				}
				time.Sleep(20 * time.Millisecond)
			}
			// The admission goes on: Open's resume reads the key's (ended) tenant.
			lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			got, rerr := resume(lctx, adm, tn.ID)
			cancel()
			if rerr != nil {
				_ = adm.Rollback(context.Background())
			} else if err := adm.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			if first == nil {
				r := <-done
				first = &r
			}
			if rerr != nil || first.err != nil || got != nil {
				t.Fatalf("lock order: the admission's resume %v (tenant %v), the purge %v", rerr, got, first.err)
			}
			// The next pass purges it.
			if first.n == 0 {
				if n, err := sw.Purge(ctx); err != nil || n != 1 {
					t.Fatalf("the next pass: %d %v", n, err)
				}
			}
			if n := scalar[int](t, e.pool, `select count(*) from kontra.tunnel_keys`); n != 0 {
				t.Fatalf("%d keys after the purge", n)
			}
		})
	}
}

// hangingObjects is a bucket whose DeleteTask waits until its context ends (a bucket that hangs), recording the tasks
// asked for, at most limit of them at once.
type hangingObjects struct {
	mu       sync.Mutex
	calls    []uuid.UUID
	inflight int
	most     int
}

func (h *hangingObjects) DeleteTask(ctx context.Context, _, task uuid.UUID) error {
	h.mu.Lock()
	h.calls = append(h.calls, task)
	h.inflight++
	h.most = max(h.most, h.inflight)
	h.mu.Unlock()
	<-ctx.Done()
	h.mu.Lock()
	h.inflight--
	h.mu.Unlock()
	return ctx.Err()
}

// T §6.1 (M4 WP-03 review, a hanging bucket): a purge pass deletes a tenant's objects several tasks at once, each
// within objectDeleteTimeout, and stops at the first bucket failure: a bucket that hangs costs a pass one timeout, not
// one per tenant, and keeps every row.
func TestPurgePassStopsAtTheFirstBucketFailure(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	lim := Defaults
	lim.PurgeAfter = time.Hour
	defer func(d time.Duration) { objectDeleteTimeout = d }(objectDeleteTimeout)
	objectDeleteTimeout = 500 * time.Millisecond
	const tenants, tasksEach = 4, 3
	for i := range tenants {
		tn := e.mustOpen(lim, OpenRequest{Key: newKey(10 + i), Network: netOf("198.51.100.1"), Services: svcs("a")})
		for range tasksEach {
			e.blob(tn.ID, e.insertTask(tn.ID, "a", "completed", "anonymous"))
		}
		e.purgeDue(tn.ID)
	}
	objects := &hangingObjects{}
	start := time.Now()
	n, err := NewSweeper(e.pool, lim, objects, nil).Purge(ctx)
	took := time.Since(start)
	if n != 0 || err == nil {
		t.Fatalf("a hanging bucket: purged %d (%v)", n, err)
	}
	if took > 3*objectDeleteTimeout {
		t.Errorf("the pass took %s: a hanging bucket held it once per tenant, not once (timeout %s)", took, objectDeleteTimeout)
	}
	tenantsAsked := scalar[int](t, e.pool, `select count(distinct tenant_id) from kontra.tasks where id = any($1::uuid[])`, objects.calls)
	if tenantsAsked != 1 || len(objects.calls) != tasksEach || objects.most != tasksEach {
		t.Errorf("DeleteTask for %d tasks of %d tenants, %d at once: want the first tenant's %d tasks at once, then the pass stopped",
			len(objects.calls), tenantsAsked, objects.most, tasksEach)
	}
	if left := scalar[int](t, e.pool, `select count(*) from kontra.tasks`); left != tenants*tasksEach {
		t.Fatalf("%d task rows left, want every one", left)
	}
}

// slowObjects is a bucket whose DeleteTask takes d.
type slowObjects struct{ d time.Duration }

func (s slowObjects) DeleteTask(ctx context.Context, _, _ uuid.UUID) error {
	select {
	case <-time.After(s.d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// T §6.1 (M4 WP-03 review): a purge pass ends at its budget; the next pass goes on where it stopped.
func TestPurgePassEndsAtItsBudget(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	lim := Defaults
	lim.PurgeAfter = time.Hour
	defer func(d time.Duration) { purgeBudget = d }(purgeBudget)
	purgeBudget = 600 * time.Millisecond
	const tenants = 8
	for i := range tenants {
		tn := e.mustOpen(lim, OpenRequest{Key: newKey(20 + i), Network: netOf("198.51.100.1"), Services: svcs("a")})
		e.blob(tn.ID, e.insertTask(tn.ID, "a", "completed", "anonymous"))
		e.purgeDue(tn.ID)
	}
	sw := NewSweeper(e.pool, lim, slowObjects{200 * time.Millisecond}, nil)
	start := time.Now()
	n, err := sw.Purge(ctx)
	if took := time.Since(start); took > purgeBudget+time.Second || n >= tenants || err == nil {
		t.Fatalf("a pass over its budget: %d purged in %s (%v)", n, took, err)
	}
	total := n
	for range tenants {
		m, _ := sw.Purge(ctx)
		total += m
		if total == tenants {
			break
		}
	}
	if total != tenants {
		t.Fatalf("%d of %d purged over the passes", total, tenants)
	}
}

// gatedObjects is a bucket whose DeleteTask waits for release, recording each task asked for.
type gatedObjects struct {
	mu      sync.Mutex
	calls   []uuid.UUID
	asked   chan struct{}
	release chan struct{}
}

func (g *gatedObjects) DeleteTask(ctx context.Context, _, task uuid.UUID) error {
	g.mu.Lock()
	g.calls = append(g.calls, task)
	g.mu.Unlock()
	select {
	case g.asked <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// T §6.1 (M4 WP-03 review): every replica runs the purge; a tenant one replica is purging is skipped by the others, so
// they neither delete its objects again nor purge its rows under it.
func TestPurgeClaimsATenant(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	lim := Defaults
	lim.PurgeAfter = time.Hour
	tn := e.mustOpen(lim, OpenRequest{Key: newKey(30), Network: netOf("198.51.100.1"), Services: svcs("a")})
	task := e.insertTask(tn.ID, "a", "completed", "anonymous")
	e.blob(tn.ID, task)
	e.purgeDue(tn.ID)

	first := &gatedObjects{asked: make(chan struct{}, 1), release: make(chan struct{})}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := NewSweeper(e.pool, lim, first, nil).Purge(ctx)
		done <- result{n, err}
	}()
	select {
	case <-first.asked:
	case <-time.After(20 * time.Second):
		t.Fatal("the first replica's purge never reached the bucket")
	}
	other := &fakeObjects{}
	if n, err := NewSweeper(e.pool, lim, other, nil).Purge(ctx); n != 0 || err != nil || len(other.calls) != 0 {
		t.Fatalf("another replica's pass while the first purges: %d %v, DeleteTask %v", n, err, other.calls)
	}
	if left := scalar[int](t, e.pool, `select count(*) from kontra.tasks where id = $1`, task); left != 1 {
		t.Fatal("another replica purged the rows while the first was deleting the objects")
	}
	close(first.release)
	if r := <-done; r.n != 1 || r.err != nil {
		t.Fatalf("the first replica's purge: %d %v", r.n, r.err)
	}
}
