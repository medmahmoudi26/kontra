package blob

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Row is one blobs row (migration 0002): the ref, the task's tenant and id, and the object's size.
type Row struct {
	Ref    Ref
	Tenant uuid.UUID
	Task   uuid.UUID
	Size   int64
}

// Key is where the row's object lives.
func (r Row) Key() Key { return Key{Tenant: r.Tenant, Task: r.Task, Ref: r.Ref} }

// Execer is what a pool and a transaction both offer for a write.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Querier is what a pool and a transaction both offer for a read.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Insert writes rows; a ref already stored is left as it is (a retried transaction, or an event and its work
// item naming the same part).
func Insert(ctx context.Context, q Execer, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	refs, tenants, tasks, sizes := Columns(rows)
	_, err := q.Exec(ctx, `insert into kontra.blobs (ref, tenant_id, task_id, size)
		select * from unnest($1::text[], $2::uuid[], $3::uuid[], $4::bigint[])
		on conflict (ref) do nothing`, refs, tenants, tasks, sizes)
	if err != nil {
		return fmt.Errorf("blob: insert blobs rows: %w", err)
	}
	return nil
}

// Columns are rows as the column arrays Insert and the store's statements bind.
func Columns(rows []Row) (refs []string, tenants, tasks []uuid.UUID, sizes []int64) {
	for _, r := range rows {
		refs = append(refs, string(r.Ref))
		tenants = append(tenants, r.Tenant)
		tasks = append(tasks, r.Task)
		sizes = append(sizes, r.Size)
	}
	return refs, tenants, tasks, sizes
}

// Lookup reads the rows of refs; a ref without a row is absent from the result.
func Lookup(ctx context.Context, q Querier, refs []Ref) (map[Ref]Row, error) {
	out := map[Ref]Row{}
	if len(refs) == 0 {
		return out, nil
	}
	want := make([]string, 0, len(refs))
	for _, r := range refs {
		want = append(want, string(r))
	}
	rows, err := q.Query(ctx, `select ref, tenant_id, task_id, size from kontra.blobs where ref = any($1::text[])`, want)
	if err != nil {
		return nil, fmt.Errorf("blob: read blobs rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		var ref string
		if err := rows.Scan(&ref, &r.Tenant, &r.Task, &r.Size); err != nil {
			return nil, fmt.Errorf("blob: read blobs rows: %w", err)
		}
		r.Ref = Ref(ref)
		out[r.Ref] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blob: read blobs rows: %w", err)
	}
	return out, nil
}

// RefsIn returns the refs of every kontra-blob:<ref> in b (an event's payload, a work item's request), in
// order of appearance, each once. It looks at bytes only: a ref is 26 base32 characters after the scheme.
func RefsIn(b []byte) []Ref {
	var out []Ref
	seen := map[Ref]bool{}
	scheme := []byte(URIScheme)
	for {
		i := bytes.Index(b, scheme)
		if i < 0 {
			return out
		}
		b = b[i+len(scheme):]
		if len(b) < 26 {
			return out
		}
		r := Ref(b[:26])
		if r.Valid() && (len(b) == 26 || !isRefChar(b[26])) && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
}

func isRefChar(c byte) bool { return c >= 'a' && c <= 'z' || c >= '2' && c <= '7' }

// Pending holds the objects one request or one dispatch stored whose rows are not written yet. Each row is
// written by the write that logs, or stores, a payload naming its ref, in that write's own transaction
// (spec/relay.md §10.4: "a blobs row in the event's transaction"): the task's creation row (a new task's first
// message), a follow-up's work item, an event a dispatch logs from the upstream (store.Log.CreateTask,
// InsertWorkItem, Append).
//
// A row stays pending after it was written: a write that is refused and tried again (a version conflict, a
// retried transaction) writes it again, which changes nothing.
type Pending struct {
	mu   sync.Mutex
	rows map[Ref]Row
}

// NewPending returns an empty set.
func NewPending() *Pending { return &Pending{rows: map[Ref]Row{}} }

// Add records a stored object.
func (p *Pending) Add(r Row) {
	p.mu.Lock()
	p.rows[r.Ref] = r
	p.mu.Unlock()
}

// For returns the pending rows of task whose refs payloads name.
func (p *Pending) For(task uuid.UUID, payloads ...[]byte) []Row {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rows) == 0 {
		return nil
	}
	var out []Row
	seen := map[Ref]bool{}
	for _, b := range payloads {
		for _, ref := range RefsIn(b) {
			if r, ok := p.rows[ref]; ok && r.Task == task && !seen[ref] {
				seen[ref] = true
				out = append(out, r)
			}
		}
	}
	return out
}

type pendingKey struct{}

// WithPending attaches p to ctx: the request's interceptors, or the dispatch, and the store's writes below them
// share it.
func WithPending(ctx context.Context, p *Pending) context.Context {
	return context.WithValue(ctx, pendingKey{}, p)
}

// PendingFrom returns the set attached to ctx, nil when there is none.
func PendingFrom(ctx context.Context) *Pending {
	p, _ := ctx.Value(pendingKey{}).(*Pending)
	return p
}

// PendingRows returns the rows pending on ctx for task whose refs payloads name: what a write that stores those
// payloads must write in its own transaction.
func PendingRows(ctx context.Context, task uuid.UUID, payloads ...[]byte) []Row {
	return PendingFrom(ctx).For(task, payloads...)
}
