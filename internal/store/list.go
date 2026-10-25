package store

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListTasks limits (spec §3.1.4, a2a.proto ListTasksRequest).
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
	// DefaultListHistory is the history kept per task when a ListTasks request sets no historyLength
	// (implementation-defined, spec §3.2.4; a2a-go's in-memory store keeps 100 too).
	DefaultListHistory = 100
	// ListPageBytes bounds the snapshot bytes one ListTasks page reads and decodes: the page ends, with a
	// next page token, at the task that brings them to this or more. A page always holds at least one task,
	// so a task larger than the budget (at most limits.task_bytes, as a GetTask of it reads) is still listed.
	// Without it, a page of 100 tasks could hold 100 × limits.task_bytes on the replica, which serves other
	// tenants too (the reason for spec §6.6's limits).
	ListPageBytes = 8 << 20
)

// listBatchStored bounds the on-disk size of the snapshots one ListTasks fetch asks for (pg_column_size,
// which reads no TOAST; a snapshot larger than it is fetched alone), so the rows the server reads past
// the budget are at most one batch. It is not the memory bound: the on-disk size of a compressible snapshot
// is a fraction of its bytes, so the budget counts the bytes as they arrive, one row at a time.
const listBatchStored = 1 << 20

// pageTokenVersion is the first byte of every page token: base64url(version, status_at in Unix
// microseconds as 8 big-endian bytes, the task id's 16 bytes).
const pageTokenVersion byte = 1

// EncodePageToken returns the token of the page after the task (statusAt, id).
func EncodePageToken(statusAt time.Time, id uuid.UUID) string {
	b := make([]byte, 0, 25)
	b = append(b, pageTokenVersion)
	b = binary.BigEndian.AppendUint64(b, uint64(statusAt.UTC().Truncate(time.Microsecond).UnixMicro()))
	b = append(b, id[:]...)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodePageToken reads a token EncodePageToken made; anything else is an error wrapping
// a2a.ErrInvalidParams.
func DecodePageToken(tok string) (time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) != 25 || b[0] != pageTokenVersion {
		return time.Time{}, uuid.Nil, fmt.Errorf("store: page token is not one this gate issued: %w", a2a.ErrInvalidParams)
	}
	at := time.UnixMicro(int64(binary.BigEndian.Uint64(b[1:9]))).UTC()
	id, err := uuid.FromBytes(b[9:])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("store: page token: %w", a2a.ErrInvalidParams)
	}
	return at, id, nil
}

// listStates is the tasks.state values a ListTasks status filter matches: the A2A state, and quoted for
// input-required (a quoted task shows as input-required on A2A, spec §5.1).
func listStates(ts a2a.TaskState) ([]string, error) {
	st, err := StateFromA2A(ts)
	if err != nil {
		return nil, fmt.Errorf("store: status filter %q: %w", ts, a2a.ErrInvalidParams)
	}
	if st == StateInputRequired {
		return []string{string(StateInputRequired), string(StateQuoted)}, nil
	}
	return []string{string(st)}, nil
}

const listFilter = `t.tenant_id = $1 and t.service = $2 and t.buyer_id = $3
	and ($4::text is null or t.context_id = $4::text)
	and ($5::text[] is null or t.state = any($5::text[]))
	and ($6::timestamptz is null or t.status_at >= $6::timestamptz)`

// List implements ListTasks for the caller's own tasks of this service, ordered by (status_at desc, id
// desc), keyset-paginated with gate-minted page tokens; page size 1..100 (default 50), the contextId,
// status and statusTimestampAfter filters, history trimming, artifacts only when asked, totalSize before
// pagination (spec §3.1.4, TCK CORE-LIST-*). A page also ends early, with a next page token, once it has
// read ListPageBytes of snapshots (a2a-go: pageSize is the most a page holds). A bad page token, page
// size, history length or status is InvalidParams; a contextId no task can have (U+0000 or invalid
// UTF-8, which no text column holds) matches nothing. The anonymous principal is refused,
// UnsupportedOperation: its tasks share one buyer (spec §2.3, D20).
func (s *Store) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if req == nil {
		req = &a2a.ListTasksRequest{}
	}
	o, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if o.Via == ViaAnonymous {
		return nil, fmt.Errorf("store: anonymous callers cannot list tasks: %w", a2a.ErrUnsupportedOperation)
	}
	pageSize := req.PageSize
	switch {
	case pageSize == 0:
		pageSize = DefaultPageSize
	case pageSize < 1 || pageSize > MaxPageSize:
		return nil, fmt.Errorf("store: pageSize must be between 1 and %d, got %d: %w", MaxPageSize, pageSize, a2a.ErrInvalidParams)
	}
	history := DefaultListHistory
	if req.HistoryLength != nil {
		if history = *req.HistoryLength; history < 0 {
			return nil, fmt.Errorf("store: historyLength must not be negative, got %d: %w", history, a2a.ErrInvalidParams)
		}
	}
	var contextID, states, after any
	if req.ContextID != "" {
		contextID = req.ContextID
	}
	if req.Status != a2a.TaskStateUnspecified {
		if states, err = listStates(req.Status); err != nil {
			return nil, err
		}
	}
	if req.StatusTimestampAfter != nil {
		after = req.StatusTimestampAfter.UTC().Truncate(time.Microsecond)
	}
	var curAt, curID any
	if req.PageToken != "" {
		at, id, err := DecodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		curAt, curID = at, id
	}
	resp := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, PageSize: pageSize}
	if req.ContextID != "" && !storableText(req.ContextID) {
		return resp, nil
	}
	args := []any{s.svc.TenantID, s.svc.Name, o.BuyerID, contextID, states, after}

	// One snapshot for the count, the page and the snapshots read, so they agree with each other.
	err = pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select count(*) from kontra.tasks t where `+listFilter, args...).Scan(&resp.TotalSize); err != nil {
			return fmt.Errorf("store: count tasks: %w", err)
		}
		rows, err := tx.Query(ctx, `select t.id, t.status_at, pg_column_size(t.snapshot) from kontra.tasks t where `+listFilter+`
			and ($7::timestamptz is null or (t.status_at, t.id) < ($7::timestamptz, $8::uuid))
			order by t.status_at desc, t.id desc limit $9`, append(args, curAt, curID, pageSize+1)...)
		if err != nil {
			return fmt.Errorf("store: list tasks: %w", err)
		}
		keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (listKey, error) {
			var k listKey
			err := r.Scan(&k.id, &k.statusAt, &k.stored)
			return k, err
		})
		if err != nil {
			return fmt.Errorf("store: list tasks: %w", err)
		}
		if err := s.readPage(ctx, tx, o.BuyerID, keys[:min(len(keys), pageSize)], history, req.IncludeArtifacts, resp); err != nil {
			return err
		}
		if n := len(resp.Tasks); n < len(keys) {
			resp.NextPageToken = EncodePageToken(keys[n-1].statusAt, keys[n-1].id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// listKey is one task of a ListTasks page before its snapshot is read.
type listKey struct {
	id       uuid.UUID
	statusAt time.Time
	stored   int64 // pg_column_size(snapshot): on disk, compressed or not
}

// readPage appends the tasks of keys to resp in order, trimmed, until it has read s.listBytes of
// snapshots (at least one task). It fetches them in batches of at most s.listBatch bytes on disk and reads
// and decodes one row at a time, so it holds about the budget plus one snapshot whatever the compression.
func (s *Store) readPage(ctx context.Context, tx pgx.Tx, buyer uuid.UUID, keys []listKey, history int, artifacts bool, resp *a2a.ListTasksResponse) error {
	read := 0
	for i := 0; i < len(keys) && read < s.listBytes; {
		j, stored := i+1, keys[i].stored
		for j < len(keys) && stored+keys[j].stored <= s.listBatch {
			stored += keys[j].stored
			j++
		}
		ids := make([]uuid.UUID, 0, j-i)
		for _, k := range keys[i:j] {
			ids = append(ids, k.id)
		}
		rows, err := tx.Query(ctx, `select t.id, t.snapshot from kontra.tasks t
			where t.id = any($1::uuid[]) and t.tenant_id = $2 and t.service = $3 and t.buyer_id = $4
			order by t.status_at desc, t.id desc`, ids, s.svc.TenantID, s.svc.Name, buyer)
		if err != nil {
			return fmt.Errorf("store: list tasks: %w", err)
		}
		for read < s.listBytes && rows.Next() {
			var (
				id   uuid.UUID
				snap []byte
			)
			if err := rows.Scan(&id, &snap); err != nil {
				rows.Close()
				return fmt.Errorf("store: list tasks: %w", err)
			}
			if id != keys[i].id {
				// The page and the snapshots come from one repeatable-read snapshot; this is a bug.
				rows.Close()
				return fmt.Errorf("store: list tasks: task %s read where %s was listed", id, keys[i].id)
			}
			read += len(snap)
			t, err := decodeTask(snap)
			if err != nil {
				rows.Close()
				return fmt.Errorf("store: task %s snapshot: %w", id, err)
			}
			if history == 0 {
				t.History = nil
			} else if len(t.History) > history {
				t.History = t.History[len(t.History)-history:]
			}
			if !artifacts {
				t.Artifacts = nil
			}
			resp.Tasks = append(resp.Tasks, t)
			i++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: list tasks: %w", err)
		}
		if i < j && read < s.listBytes {
			return fmt.Errorf("store: list tasks: %d of the page's tasks were not read", j-i)
		}
	}
	return nil
}
