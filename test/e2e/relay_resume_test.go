package e2e

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// TestRelayResume is relay_resume (spec/relay.md §9, §16; PRD §15): the buyer's SSE is cut at a random
// seq K; SubscribeToTask with Last-Event-ID: K on the other replica returns the snapshot as of K (no id
// line) equal to the fold of what the buyer had received, then every carried row after K exactly once,
// in seq order, ending after the terminal row. Also: K at or past the terminal seq of a finished task is
// UnsupportedOperationError, a K that is not a seq of this task is InvalidParams, and a seq of another
// buyer's task reads nothing (TaskNotFound).
func TestRelayResume(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 20, Interval: 200 * time.Millisecond, Streaming: true, Binding: "JSONRPC"}})
	ctx := ctxFor(t, 3*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	buyer := h.BuyerA()

	// 1. SendStreamingMessage on replica A; read a random number of events, at least 2 and all before the
	// terminal row; K is the last id received. Then drop the connection.
	s1, err := buyer.SendStreaming(ctx, a, newMessage("relay_resume"), Call{})
	if err != nil {
		t.Fatal(err)
	}
	n := 2 + rand.IntN(10)
	var got1 []Event
	for len(got1) < n {
		ev, err := s1.Next(ctx)
		if err != nil {
			t.Fatalf("stream 1 ended after %d events: %v", len(got1), err)
		}
		if ev.ID == nil {
			t.Fatalf("stream 1: event %d has no id: %s", len(got1), ev.Data)
		}
		if ev.State.Terminal() {
			t.Fatalf("stream 1 reached the terminal row after %d events; the upstream is too fast for K", len(got1))
		}
		got1 = append(got1, ev)
	}
	taskID := s1.TaskID()
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	if *got1[0].ID != 1 || got1[0].Kind != "task" {
		t.Fatalf("a new task's stream starts with the creation snapshot at id 1, got id %d %s", *got1[0].ID, got1[0].Kind)
	}
	checkStrictlyIncreasing(t, "stream 1", ids(got1))
	K := *got1[len(got1)-1].ID
	t.Logf("task %s: stream 1 dropped after %d events, K = %d", taskID, len(got1), K)
	buyerFold, err := fold(got1)
	if err != nil {
		t.Fatalf("the buyer's fold: %v", err)
	}

	// 2. SubscribeToTask on replica B with Last-Event-ID: K, read to the end.
	s2, err := buyer.Subscribe(ctx, b, taskID, &K, Call{})
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s2.ReadAll(ctx)
	if err != nil {
		t.Fatalf("stream 2: %v", err)
	}
	if len(got2) < 2 {
		t.Fatalf("stream 2 has %d events", len(got2))
	}
	lead := got2[0]
	if lead.ID != nil || lead.Kind != "task" {
		t.Fatalf("the resumed stream must lead with the snapshot as a Task without an id, got %s id %v", lead.Kind, lead.ID)
	}
	snapK, err := h.Log().SnapshotAt(ctx, uuid.MustParse(string(taskID)), K)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonical(t, lead.Task()), canonical(t, snapK); got != want {
		t.Fatalf("the leading snapshot is not Log.SnapshotAt(K):\n got %s\nwant %s", got, want)
	}
	if got, want := canonical(t, lead.Task()), canonical(t, buyerFold); got != want {
		t.Fatalf("the leading snapshot is not the buyer's own fold up to K:\n got %s\nwant %s", got, want)
	}
	rest := got2[1:]
	for i, ev := range rest {
		if ev.ID == nil || *ev.ID <= K {
			t.Fatalf("stream 2 event %d: id %v, want an id above K = %d", i+1, ev.ID, K)
		}
	}
	checkStrictlyIncreasing(t, "stream 2", ids(rest))
	last := rest[len(rest)-1]
	if last.State != a2a.TaskStateCompleted {
		t.Fatalf("stream 2 must end right after the terminal (completed) row, ended after a %s %q", last.Kind, last.State)
	}
	for _, ev := range rest[:len(rest)-1] {
		if ev.State.Terminal() {
			t.Fatalf("a terminal row before the last event: %s", ev.Data)
		}
	}
	checkCarried(t, h, taskID, append(ids(got1), ids(rest)...))
	terminal := *last.ID

	// No event carries the upstream's task or context id (spec §4).
	turns := h.Upstream.Turns()
	if len(turns) != 1 {
		t.Fatalf("the upstream ran %d turns, want 1", len(turns))
	}
	for _, ev := range append(append([]Event{}, got1...), got2...) {
		for _, up := range []string{string(turns[0].TaskID), turns[0].ContextID} {
			if strings.Contains(string(ev.Data), up) {
				t.Fatalf("an event carries the upstream's id %s: %s", up, ev.Data)
			}
		}
	}

	// 3. Resume at terminal-1, on both bindings: the snapshot as of terminal-1 without an id, then only the
	// terminal row, then the end.
	prev := terminal - 1
	snapPrev, err := h.Log().SnapshotAt(ctx, uuid.MustParse(string(taskID)), prev)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		g       *Gate
		binding Binding
	}{{a, JSONRPC}, {b, HTTPJSON}} {
		s, err := buyer.Subscribe(ctx, c.g, taskID, &prev, Call{Binding: c.binding})
		if err != nil {
			t.Fatalf("%s: resume at terminal-1: %v", c.binding, err)
		}
		evs, err := s.ReadAll(ctx)
		if err != nil {
			t.Fatalf("%s: resume at terminal-1: %v", c.binding, err)
		}
		if len(evs) != 2 || evs[0].ID != nil || evs[0].Kind != "task" || evs[1].ID == nil || *evs[1].ID != terminal {
			t.Fatalf("%s: resume at terminal-1 = %d events, want the snapshot without an id then the terminal row %d: %+v", c.binding, len(evs), terminal, evs)
		}
		if got, want := canonical(t, evs[0].Task()), canonical(t, snapPrev); got != want {
			t.Fatalf("%s: the snapshot at terminal-1 is not Log.SnapshotAt:\n got %s\nwant %s", c.binding, got, want)
		}
	}

	// 4. Ordinary errors before any SSE header: at the terminal seq, a K that is not a seq of this task,
	// and another buyer with a valid seq.
	refusals := []struct {
		name     string
		buyer    *Buyer
		lei      string
		rpcCode  int
		restCode int
	}{
		{"Last-Event-ID at the terminal seq", buyer, strconv.FormatInt(terminal, 10), -32004, http.StatusBadRequest},
		{"Last-Event-ID past the tail", buyer, strconv.FormatInt(terminal+1000, 10), -32602, http.StatusBadRequest},
		{"Last-Event-ID that is not a number", buyer, "not-a-seq", -32602, http.StatusBadRequest},
		{"another buyer's key with a valid seq", h.BuyerB(), strconv.FormatInt(K, 10), -32001, http.StatusNotFound},
	}
	for _, r := range refusals {
		for _, g := range h.Gates {
			_, err := r.buyer.SubscribeRaw(ctx, g, taskID, r.lei, Call{})
			se, ok := err.(*StreamError)
			if !ok {
				t.Fatalf("%s on %s (JSON-RPC): got %v, want an error before the stream", r.name, g.Name, err)
			}
			if se.HTTPStatus != http.StatusOK || se.RPC == nil || se.RPC.Code != r.rpcCode || strings.HasPrefix(se.ContentType, "text/event-stream") {
				t.Fatalf("%s on %s (JSON-RPC): HTTP %d %s %s, want HTTP 200 with JSON-RPC error %d", r.name, g.Name, se.HTTPStatus, se.ContentType, se.Body, r.rpcCode)
			}
			_, err = r.buyer.SubscribeRaw(ctx, g, taskID, r.lei, Call{Binding: HTTPJSON})
			se, ok = err.(*StreamError)
			if !ok {
				t.Fatalf("%s on %s (HTTP+JSON): got %v, want an error before the stream", r.name, g.Name, err)
			}
			if se.HTTPStatus != r.restCode || strings.HasPrefix(se.ContentType, "text/event-stream") {
				t.Fatalf("%s on %s (HTTP+JSON): HTTP %d %s %s, want HTTP %d", r.name, g.Name, se.HTTPStatus, se.ContentType, se.Body, r.restCode)
			}
		}
	}
	// Without Last-Event-ID a terminal task is UnsupportedOperationError too (spec §9, TCK STREAM-SUB-003).
	if _, err := buyer.Subscribe(ctx, b, taskID, nil, Call{}); !isRPC(err, -32004) {
		t.Fatalf("SubscribeToTask of a terminal task without Last-Event-ID: %v", err)
	}

	checkHistory(t, h, taskID, "e2e_relay_resume", slowagent.ChunkMarker, h.APIKey, h.APIKeyB)
}

func isRPC(err error, code int) bool {
	se, ok := err.(*StreamError)
	return ok && se.RPC != nil && se.RPC.Code == code
}
