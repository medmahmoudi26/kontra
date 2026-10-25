package store

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// BenchmarkAppend measures the NOTIFY commit ceiling of spec §5.2 ("M1 measures that ceiling", pg-store
// brief C6): the commit rate of Log.Append, the one statement every event costs, with pg_notify (the
// production statement) and with the same statement without it, 8 writers each on its own task.
//
//	KONTRA_TEST_PG=… go test -run '^$' -bench BenchmarkAppend -benchtime 6000x -count 2 ./internal/store/
//
// Measured on the M1 development host, 25 Oct 2026: 2 vCPUs shared by the benchmark, Postgres 17.11 in
// Docker and other workloads (load average 4 to 6 during the runs); the test server runs with fsync and
// synchronous_commit off. Two runs of -count 2, so four samples each:
//
//	notify=true   612, 722, 887, 729 commits/s
//	notify=false  941, 777, 880, 1042 commits/s
//
// The host is CPU-bound well before the cluster-wide NOTIFY lock (pg-store brief C6 measured about 850
// to 1,000 NOTIFY-ing commits/s in local Docker with synchronous_commit on), and the samples overlap, so
// these are a floor for this host, not the target's ceiling.
//
// Decision for M1: no coalescing. At several hundred event commits a second per Postgres cluster, a
// streaming upstream emitting tens of chunks a second per task stays well below the ceiling at M1's
// scale. The target database (Scaleway/CNPG, shared with Cloud's tables, D37) must be measured with this
// benchmark, with durable commits, before Cloud runs on it; coalescing runs of artifact-append chunks
// into one event (never for a sealed service, spec §6.6) is the escape hatch if it binds there.
func BenchmarkAppend(b *testing.B) {
	e := newEnv(b)
	noNotify := strings.Replace(appendSQL,
		"pg_notify('kontra_task_events', e.task_id::text || ':' || e.seq::text)", "null", 1)
	if noNotify == appendSQL {
		b.Fatal("the benchmark's statement variant no longer matches appendSQL")
	}
	for _, c := range []struct {
		name string
		sql  string
	}{{"notify=true", appendSQL}, {"notify=false", noNotify}} {
		b.Run(c.name, func(b *testing.B) {
			l := NewLog(e.pool)
			l.appendStmt = c.sql
			var failed atomic.Int64
			b.SetParallelism(4) // 4 × GOMAXPROCS writers: 8 on the 2-CPU host
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				task := e.newTask(e.svc, buyerA, "", "bench")
				id := uid(b, task.ID)
				ev := status(task, a2a.TaskStateWorking, "")
				payload, _ := json.Marshal(a2a.StreamResponse{Event: ev})
				snap := *task
				snap.Status = ev.Status
				version := int64(1)
				for pb.Next() {
					if _, err := l.Append(context.Background(), nil, Write{TaskID: id, PrevVersion: version, Kind: KindStatus,
						State: StateWorking, Snapshot: &snap, Payload: payload}); err != nil {
						failed.Add(1)
						return
					}
					version++
				}
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "commits/s")
			if failed.Load() > 0 {
				b.Fatalf("%d writers failed", failed.Load())
			}
		})
	}
}
