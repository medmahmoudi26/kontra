package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// ListTasks (spec §3.1.4, TCK CORE-LIST-*): scoped to the caller, ordered by status_at desc then id desc,
// page size 1..100 (default 50), the filters, page tokens, totalSize before pagination, artifacts only
// when asked, history trimming.
func TestList(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	base := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	at := func(i int) *time.Time { ts := base.Add(time.Duration(i) * time.Minute); return &ts }

	// Seven tasks of A: ids ascend with creation, status times as given (two share one).
	minutes := []int{5, 1, 3, 3, 7, 2, 6}
	var tasks []*a2a.Task
	for i, m := range minutes {
		ctxID := "ctx-other"
		if i%2 == 0 {
			ctxID = "ctx-1"
		}
		task, _ := e.newTaskAt(e.svc, buyerA, ctxID, "q", at(m))
		tasks = append(tasks, task)
	}
	// One of them completes later, with an artifact and a longer history.
	done := tasks[1]
	mustApply(t, ctx, s, done.ID, userMessage(done, "more"))
	mustApply(t, ctx, s, done.ID, artifact(done, "a1", false, "result"))
	fin := status(done, a2a.TaskStateCompleted, "ok")
	fin.Status.Timestamp = at(9)
	mustApply(t, ctx, s, done.ID, fin)
	// Another buyer's tasks and A's task on another service are out of scope.
	e.newTaskAt(e.svc, buyerB, "ctx-1", "theirs", at(8))
	other := e.service("other", 60, 60)
	e.newTaskAt(other, buyerA, "ctx-1", "elsewhere", at(8))

	// The expected order: status time desc, then id desc.
	type key struct {
		at time.Time
		id a2a.TaskID
	}
	var want []key
	for i, task := range tasks {
		m := minutes[i]
		if task.ID == done.ID {
			m = 9
		}
		want = append(want, key{*at(m), task.ID})
	}
	slices.SortFunc(want, func(a, b key) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return strings.Compare(string(b.id), string(a.id))
	})
	ids := func(ts []*a2a.Task) []a2a.TaskID {
		var out []a2a.TaskID
		for _, t := range ts {
			out = append(out, t.ID)
		}
		return out
	}
	var wantIDs []a2a.TaskID
	for _, k := range want {
		wantIDs = append(wantIDs, k.id)
	}

	all, err := s.List(ctx, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all.Tasks); !slices.Equal(got, wantIDs) {
		t.Fatalf("order\n got %v\nwant %v", got, wantIDs)
	}
	if all.TotalSize != 7 || all.PageSize != DefaultPageSize || all.NextPageToken != "" {
		t.Errorf("total %d, page size %d, token %q", all.TotalSize, all.PageSize, all.NextPageToken)
	}
	for _, task := range all.Tasks {
		if task.Artifacts != nil {
			t.Errorf("task %s has artifacts without includeArtifacts", task.ID)
		}
	}
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), `"artifacts"`) {
		t.Errorf("artifacts present in the JSON: %s", raw)
	}
	if !strings.Contains(string(raw), `"nextPageToken":""`) {
		t.Errorf("nextPageToken missing on the last page: %s", raw)
	}

	// Pages of three: the tokens round-trip and the pages tile the full order.
	var paged []a2a.TaskID
	token := ""
	for page := 0; ; page++ {
		resp, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if resp.TotalSize != 7 || resp.PageSize != 3 {
			t.Errorf("page %d: total %d, size %d", page, resp.TotalSize, resp.PageSize)
		}
		paged = append(paged, ids(resp.Tasks)...)
		if resp.NextPageToken == "" {
			if len(resp.Tasks) != 1 {
				t.Errorf("last page has %d tasks", len(resp.Tasks))
			}
			break
		}
		if len(resp.Tasks) != 3 {
			t.Fatalf("page %d has %d tasks", page, len(resp.Tasks))
		}
		token = resp.NextPageToken
	}
	if !slices.Equal(paged, wantIDs) {
		t.Errorf("paged\n got %v\nwant %v", paged, wantIDs)
	}

	for name, req := range map[string]*a2a.ListTasksRequest{
		"page size 101":       {PageSize: 101},
		"page size -1":        {PageSize: -1},
		"a bad token":         {PageToken: "not-a-token"},
		"a foreign token":     {PageToken: "MjAyNi0xMC0yNVQxMjowMDowMFpfeA=="},
		"negative history":    {HistoryLength: ptr(-5)},
		"an unknown status":   {Status: "TASK_STATE_RUNNING"},
		"a truncated token":   {PageToken: token[:len(token)-2]},
		"a token with a byte": {PageToken: token + "AA"},
	} {
		if _, err := s.List(ctx, req); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("%s: %v, want InvalidParams", name, err)
		}
	}

	byCtx, err := s.List(ctx, &a2a.ListTasksRequest{ContextID: "ctx-1", PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if byCtx.TotalSize != 4 || len(byCtx.Tasks) != 2 || byCtx.NextPageToken == "" {
		t.Errorf("contextId filter: total %d, %d tasks, token %q", byCtx.TotalSize, len(byCtx.Tasks), byCtx.NextPageToken)
	}
	for _, task := range byCtx.Tasks {
		if task.ContextID != "ctx-1" {
			t.Errorf("contextId filter returned %s in %s", task.ID, task.ContextID)
		}
	}
	// A contextId no task can have (a NUL or invalid UTF-8, which no text column holds) matches nothing.
	for _, c := range []string{"ctx-1\x00", "ctx\xff"} {
		resp, err := s.List(ctx, &a2a.ListTasksRequest{ContextID: c})
		if err != nil || resp.TotalSize != 0 || len(resp.Tasks) != 0 || resp.NextPageToken != "" || resp.PageSize != DefaultPageSize {
			t.Errorf("contextId %q: %+v %v, want an empty page", c, resp, err)
		}
	}
	completed, err := s.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateCompleted, IncludeArtifacts: true, HistoryLength: ptr(1)})
	if err != nil {
		t.Fatal(err)
	}
	if completed.TotalSize != 1 || len(completed.Tasks) != 1 || completed.Tasks[0].ID != done.ID {
		t.Fatalf("status filter: %+v", completed)
	}
	if got := completed.Tasks[0]; len(got.Artifacts) != 1 || len(got.History) != 1 || got.History[0].Parts[0].Text() != "more" {
		t.Errorf("includeArtifacts/historyLength 1: artifacts %v, history %v", got.Artifacts, got.History)
	}
	full, _ := s.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateCompleted})
	if len(full.Tasks[0].History) != 2 {
		t.Errorf("unset historyLength trimmed history to %d", len(full.Tasks[0].History))
	}
	none, _ := s.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateCompleted, HistoryLength: ptr(0)})
	if none.Tasks[0].History != nil {
		t.Errorf("historyLength 0 kept %d messages", len(none.Tasks[0].History))
	}
	after, err := s.List(ctx, &a2a.ListTasksRequest{StatusTimestampAfter: at(5)})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range after.Tasks {
		if task.Status.Timestamp.Before(*at(5)) {
			t.Errorf("statusTimestampAfter returned %s at %v", task.ID, task.Status.Timestamp)
		}
	}
	if after.TotalSize != 4 {
		t.Errorf("statusTimestampAfter: %d tasks, want 4 (minutes 5, 6, 7, 9)", after.TotalSize)
	}
	submitted, _ := s.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateSubmitted})
	if submitted.TotalSize != 6 {
		t.Errorf("submitted filter: %d", submitted.TotalSize)
	}

	if _, err := s.List(asOwner(context.Background(), Owner{BuyerID: buyerA.BuyerID, Principal: "anonymous", Via: ViaAnonymous}),
		&a2a.ListTasksRequest{}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("anonymous List: %v", err)
	}
	if _, err := s.List(context.Background(), &a2a.ListTasksRequest{}); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("List without a caller: %v", err)
	}
	var _ taskstore.Store = s
}

func ptr[T any](v T) *T { return &v }

// ListTasks reads at most about ListPageBytes of snapshots per page, plus the task that crosses it (spec
// §6.6: one tenant must not exhaust a shared replica): a page of large tasks stops early with a next
// token, whether the snapshots compress on disk or not, and a page always holds at least one task.
func TestListByteBudget(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	base := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	const partBytes = 3 << 20
	var want []a2a.TaskID // newest first
	for i := range 4 {
		at := base.Add(time.Duration(i) * time.Minute)
		task, _ := e.newTaskAt(e.svc, buyerA, "", "q", &at)
		text := strings.Repeat("a", partBytes) // compresses to almost nothing on disk
		if i%2 == 1 {
			b := make([]byte, partBytes/2)
			_, _ = rand.Read(b)
			text = hex.EncodeToString(b) // does not
		}
		mustApply(t, ctx, s, task.ID, artifact(task, "big", false, text))
		want = append([]a2a.TaskID{task.ID}, want...)
	}
	oldest := base.Add(-time.Minute)
	small, _ := e.newTaskAt(e.svc, buyerA, "", "q", &oldest) // the oldest, and small
	want = append(want, small.ID)

	walk := func(name string, req a2a.ListTasksRequest) [][]a2a.TaskID {
		t.Helper()
		var pages [][]a2a.TaskID
		for {
			resp, err := s.List(ctx, &req)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if resp.TotalSize != 5 {
				t.Errorf("%s: totalSize %d", name, resp.TotalSize)
			}
			var page []a2a.TaskID
			for _, task := range resp.Tasks {
				page = append(page, task.ID)
				if (task.ID != small.ID && req.IncludeArtifacts != (len(task.Artifacts) == 1)) || (!req.IncludeArtifacts && task.Artifacts != nil) {
					t.Errorf("%s: task %s has %d artifacts", name, task.ID, len(task.Artifacts))
				}
			}
			pages = append(pages, page)
			if resp.NextPageToken == "" || len(pages) > 10 {
				return pages
			}
			req.PageToken = resp.NextPageToken
		}
	}
	check := func(name string, pages [][]a2a.TaskID, sizes ...int) {
		t.Helper()
		var all []a2a.TaskID
		var got []int
		for _, p := range pages {
			all = append(all, p...)
			got = append(got, len(p))
		}
		if !slices.Equal(got, sizes) || !slices.Equal(all, want) {
			t.Errorf("%s: pages of %v, want %v; order %v, want %v", name, got, sizes, all, want)
		}
	}
	// 8 MiB: the third 3 MiB task crosses the budget and ends the first page.
	check("default budget", walk("default budget", a2a.ListTasksRequest{}), 3, 2)
	check("with artifacts", walk("with artifacts", a2a.ListTasksRequest{IncludeArtifacts: true}), 3, 2)
	check("page size 2", walk("page size 2", a2a.ListTasksRequest{PageSize: 2}), 2, 2, 1)
	// A budget smaller than any task still makes progress, one task a page; the last two tasks come in one
	// fetch (both small on disk), and the page that ends inside it leaves the second for the next page.
	var stored int64
	if err := e.pool.QueryRow(ctx, `select sum(pg_column_size(snapshot)) from kontra.tasks where id = any($1::uuid[])`,
		[]string{string(want[3]), string(small.ID)}).Scan(&stored); err != nil || stored > listBatchStored {
		t.Fatalf("the last two tasks take %d bytes on disk (%v), want at most %d", stored, err, listBatchStored)
	}
	s.listBytes = 1
	check("a tiny budget", walk("a tiny budget", a2a.ListTasksRequest{}), 1, 1, 1, 1, 1)
	// One snapshot per fetch gives the same pages.
	s.listBytes, s.listBatch = ListPageBytes, 1
	check("one row per fetch", walk("one row per fetch", a2a.ListTasksRequest{}), 3, 2)
}
