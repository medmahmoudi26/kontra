package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// appendArtifact logs an artifact row whose text part is text, at the task's current version, and
// returns its seq.
func (e *env) appendArtifact(task *a2a.Task, text string) int64 {
	e.t.Helper()
	ctx := context.Background()
	id := uid(e.t, task.ID)
	cur, err := e.log.Load(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	payload, err := json.Marshal(a2a.StreamResponse{Event: artifact(task, "out", false, text)})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.log.Append(ctx, nil, Write{TaskID: id, PrevVersion: cur.Version, Kind: KindArtifact, Snapshot: cur.Snapshot, Payload: payload})
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Seq
}

// incompressible is n bytes of random base64 text: Postgres stores it at about its own size.
func incompressible(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n*3/4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// EventsWithin ends a read at the row that brings its payload bytes to the budget, whether the rows are
// stored at their size or compressed to a fraction of it, returns a row larger than the budget alone,
// says whether rows may follow, and pages through the log exactly once in order.
func TestEventsWithin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	task := e.newTask(e.svc, buyerA, "", "hi") // 1
	id := uid(t, task.ID)
	big := []int64{
		e.appendArtifact(task, incompressible(t, 1<<20)), // 2
		e.appendArtifact(task, incompressible(t, 1<<20)), // 3
		e.appendArtifact(task, incompressible(t, 1<<20)), // 4
	}
	squeezed := []int64{
		e.appendArtifact(task, strings.Repeat("a", 2<<20)), // 5: stored at a fraction of its 2 MiB
		e.appendArtifact(task, strings.Repeat("b", 2<<20)), // 6
		e.appendArtifact(task, strings.Repeat("c", 2<<20)), // 7
	}
	var stored int64
	if err := e.pool.QueryRow(ctx, `select pg_column_size(payload) from kontra.task_events where task_id = $1 and seq = $2`, id, squeezed[0]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored > 1<<20 {
		t.Fatalf("a repeated 2 MiB text is stored at %d bytes: the test needs a compressed row", stored)
	}
	for i := range 5 {
		e.appendArtifact(task, strings.Repeat("s", 10+i)) // 8..12
	}
	sizes := map[int64]int{}
	all, err := e.log.Events(ctx, id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range all {
		sizes[ev.Seq] = len(ev.Payload)
	}
	seqsOf := func(evs []Event) (out []int64, bytes int) {
		for _, ev := range evs {
			out = append(out, ev.Seq)
			bytes += len(ev.Payload)
		}
		return out, bytes
	}

	for _, c := range []struct {
		name     string
		after    int64
		limit    int
		budget   int
		want     []int64
		wantMore bool
	}{
		{"stored at their size", 0, 64, 3 << 19, []int64{1, big[0], big[1]}, true},
		{"compressed rows count their bytes", big[2], 64, 3 << 20, []int64{squeezed[0], squeezed[1]}, true},
		{"one row over the budget comes alone", big[0], 64, 1 << 10, []int64{big[1]}, true},
		{"the row limit", squeezed[2], 2, 1 << 20, []int64{8, 9}, true},
		{"the tail", squeezed[2], 64, 1 << 20, []int64{8, 9, 10, 11, 12}, false},
		{"nothing after the tail", 12, 64, 1 << 20, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			evs, more, err := e.log.EventsWithin(ctx, id, c.after, c.limit, c.budget)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := seqsOf(evs)
			if fmt.Sprint(got) != fmt.Sprint(c.want) || more != c.wantMore {
				t.Errorf("rows %v more %v, want %v %v", got, more, c.want, c.wantMore)
			}
			for _, ev := range evs {
				if len(ev.Payload) != sizes[ev.Seq] || ev.Kind == "" || ev.TaskID != id {
					t.Errorf("row %d: %d bytes, kind %q", ev.Seq, len(ev.Payload), ev.Kind)
				}
			}
		})
	}

	// Paging with a budget visits every row once, in order, each read at most the budget plus one row.
	var (
		cursor int64
		seen   []int64
	)
	for reads := 0; ; reads++ {
		if reads > 20 {
			t.Fatal("paging did not end")
		}
		evs, more, err := e.log.EventsWithin(ctx, id, cursor, 64, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		got, n := seqsOf(evs)
		if last := evs[len(evs)-1]; n-len(last.Payload) >= 1<<20 {
			t.Errorf("a read of %v held %d bytes", got, n)
		}
		seen = append(seen, got...)
		cursor = got[len(got)-1]
		if !more {
			break
		}
	}
	if fmt.Sprint(seen) != fmt.Sprint([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}) {
		t.Errorf("paged %v", seen)
	}
	if _, _, err := e.log.EventsWithin(ctx, uuid.Must(uuid.NewV7()), 0, 64, 1); err != nil {
		t.Errorf("another task: %v", err)
	}
}
