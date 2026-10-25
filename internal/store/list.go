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
)

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
// pagination (spec §3.1.4, TCK CORE-LIST-*). A bad page token, page size, history length or status is
// InvalidParams. The anonymous principal is refused, UnsupportedOperation: its tasks share one buyer
// (spec §2.3, D20).
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
	args := []any{s.svc.TenantID, s.svc.Name, o.BuyerID, contextID, states, after}

	var total int
	if err := s.db.QueryRow(ctx, `select count(*) from kontra.tasks t where `+listFilter, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("store: count tasks: %w", err)
	}
	rows, err := s.db.Query(ctx, `select t.id, t.status_at, t.snapshot from kontra.tasks t where `+listFilter+`
		and ($7::timestamptz is null or (t.status_at, t.id) < ($7::timestamptz, $8::uuid))
		order by t.status_at desc, t.id desc limit $9`, append(args, curAt, curID, pageSize+1)...)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	type listed struct {
		id       uuid.UUID
		statusAt time.Time
		snap     []byte
	}
	page, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (listed, error) {
		var l listed
		err := r.Scan(&l.id, &l.statusAt, &l.snap)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	resp := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, TotalSize: total, PageSize: pageSize}
	if len(page) > pageSize {
		last := page[pageSize-1]
		resp.NextPageToken = EncodePageToken(last.statusAt, last.id)
		page = page[:pageSize]
	}
	for _, l := range page {
		t, err := decodeTask(l.snap)
		if err != nil {
			return nil, fmt.Errorf("store: task %s snapshot: %w", l.id, err)
		}
		if history == 0 {
			t.History = nil
		} else if len(t.History) > history {
			t.History = t.History[len(t.History)-history:]
		}
		if !req.IncludeArtifacts {
			t.Artifacts = nil
		}
		resp.Tasks = append(resp.Tasks, t)
	}
	return resp, nil
}
