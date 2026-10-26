package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// Property (spec §9, D75): SnapshotAt(K) equals the projection after the K-th write, for every K of
// randomized sequences of status, artifact, message and metadata writes through every writer of the
// package (Store.Update as a2a-go's processor calls it, ApplyMetadata, WriteTerminal,
// AppendReviewMessage), including message and metadata rows after the terminal row.
func TestSnapshotAtEqualsProjection(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	seen := map[string]int{} // kinds folded, and kinds after the terminal row, across every sequence
	for seed := uint64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 0x6b6f6e747261))
			task := e.newTask(e.svc, buyerA, "", "start")
			id := uid(t, task.ID)
			projections := map[int64]string{}
			record := func() {
				t.Helper()
				row, err := e.log.Load(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(row.Snapshot)
				projections[row.Version] = string(b)
			}
			record()
			var artifacts []a2a.ArtifactID
			terminal := false
			for step := 0; step < 30; step++ {
				if terminal {
					switch rng.IntN(2) {
					case 0:
						if _, err := e.log.ApplyMetadata(ctx, id, randomPatch(rng)); err != nil {
							t.Fatal(err)
						}
					case 1:
						w := uuid.Must(uuid.NewV7())
						if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: w, TaskID: id, Kind: WorkExecute,
							Payload: workPayload(task, fmt.Sprintf("review %d", step), buyerA)}); err != nil {
							t.Fatal(err)
						}
						if _, err := e.log.AppendReviewMessage(ctx, id, w); err != nil {
							t.Fatal(err)
						}
					}
					record()
					continue
				}
				var ev a2a.Event
				switch op := rng.IntN(10); {
				case op < 2:
					states := []a2a.TaskState{a2a.TaskStateWorking, a2a.TaskStateAuthRequired, a2a.TaskStateInputRequired}
					text := ""
					if rng.IntN(2) == 0 {
						text = fmt.Sprintf("status %d", step)
					}
					sev := status(task, states[rng.IntN(len(states))], text)
					if rng.IntN(3) == 0 {
						sev.Metadata = randomPatch(rng)
					}
					ev = sev
				case op < 5:
					if len(artifacts) > 0 && rng.IntN(2) == 0 {
						ev = artifact(task, artifacts[rng.IntN(len(artifacts))], rng.IntN(3) > 0, fmt.Sprintf("chunk %d", step))
					} else {
						aid := a2a.ArtifactID(fmt.Sprintf("art-%d", step))
						artifacts = append(artifacts, aid)
						ev = artifact(task, aid, false, fmt.Sprintf("first %d", step))
					}
				case op < 6:
					ev = userMessage(task, fmt.Sprintf("follow-up %d", step))
				case op < 8:
					if _, err := e.log.ApplyMetadata(ctx, id, randomPatch(rng)); err != nil {
						t.Fatal(err)
					}
					record()
					continue
				case op == 8 && step > 3:
					st, _ := s.Get(ctx, task.ID)
					replaced := *st.Task
					replaced.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
					ev = &replaced // a Task event: replace semantics
				default:
					if step < 12 {
						continue
					}
					if rng.IntN(2) == 0 {
						ev = status(task, a2a.TaskStateCompleted, "done")
					} else {
						if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateFailed, Reason: ReasonUpstreamError, Text: "boom"}); err != nil {
							t.Fatal(err)
						}
						terminal = true
						record()
						continue
					}
				}
				if ev == nil {
					continue
				}
				mustApply(t, ctx, s, task.ID, ev)
				record()
				if se, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && se.Status.State.Terminal() {
					terminal = true
				}
			}
			tail, _ := e.log.Tail(ctx, id)
			if int64(len(projections)) != tail {
				t.Fatalf("%d projections recorded for %d rows", len(projections), tail)
			}
			for k := int64(1); k <= tail; k++ {
				got, err := e.log.SnapshotAt(ctx, id, k)
				if err != nil {
					t.Fatalf("SnapshotAt(%d): %v", k, err)
				}
				b, _ := json.Marshal(got)
				if string(b) != projections[k] {
					t.Fatalf("SnapshotAt(%d) differs from the projection after write %d:\n got %s\nwant %s", k, k, b, projections[k])
				}
			}
			if _, err := e.log.SnapshotAt(ctx, id, tail+1); err == nil {
				t.Error("SnapshotAt past the tail")
			}
			termSeq, _ := e.log.TerminalSeq(ctx, id)
			evs, _ := e.log.Events(ctx, id, 0, 1000)
			for _, ev := range evs {
				seen[string(ev.Kind)]++
				if termSeq > 0 && ev.Seq > termSeq {
					seen["after-terminal "+string(ev.Kind)]++
				}
			}
		})
	}
	for _, k := range []string{"task", "status", "artifact", "message", "metadata", "after-terminal message", "after-terminal metadata"} {
		if seen[k] == 0 {
			t.Errorf("the sequences never produced a %s row: %v", k, seen)
		}
	}
	if _, err := e.log.SnapshotAt(ctx, uuid.Must(uuid.NewV7()), 1); !isNotFound(err) {
		t.Errorf("SnapshotAt of an unknown task: %v", err)
	}
}

func randomPatch(rng *rand.Rand) map[string]any {
	p := map[string]any{}
	for range 1 + rng.IntN(3) {
		k := fmt.Sprintf("k%d", rng.IntN(4))
		switch rng.IntN(3) {
		case 0:
			p[k] = rng.IntN(100)
		case 1:
			p[k] = fmt.Sprintf("v%d", rng.IntN(100))
		default:
			p[k] = map[string]any{"nested": rng.IntN(5) == 0, "list": []any{1, "two"}}
		}
	}
	return p
}
