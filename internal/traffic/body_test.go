package traffic

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Redact replaces exactly the credential members the A2A specification defines, wherever a push config appears, and
// keeps every other byte; agent content with a token member and no url is left alone.
func TestRedact(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"1.0 config params", `{"jsonrpc":"2.0","id":1,"method":"CreateTaskPushNotificationConfig","params":{"taskId":"t","url":"https://h/x","token":"t0k","authentication":{"scheme":"Bearer","credentials":"s3cret"}}}`,
			`{"jsonrpc":"2.0","id":1,"method":"CreateTaskPushNotificationConfig","params":{"taskId":"t","url":"https://h/x","token":"[redacted]","authentication":{"scheme":"Bearer","credentials":"[redacted]"}}}`},
		{"token before url", `{"token": "t0k", "url": "https://h/x"}`, `{"token": "[redacted]", "url": "https://h/x"}`},
		{"inline on SendMessage", `{"message":{"parts":[{"text":"hi"}]},"configuration":{"taskPushNotificationConfig":{"url":"https://h","token":"t0k"}}}`,
			`{"message":{"parts":[{"text":"hi"}]},"configuration":{"taskPushNotificationConfig":{"url":"https://h","token":"[redacted]"}}}`},
		{"list answer", `{"configs":[{"url":"https://a","token":"one"},{"url":"https://b","token":"two"}],"nextPageToken":"p"}`,
			`{"configs":[{"url":"https://a","token":"[redacted]"},{"url":"https://b","token":"[redacted]"}],"nextPageToken":"p"}`},
		{"0.3 shape", `{"pushNotificationConfig":{"url":"https://h","token":"t0k","authentication":{"schemes":["Bearer"],"credentials":"s3cret"}}}`,
			`{"pushNotificationConfig":{"url":"https://h","token":"[redacted]","authentication":{"schemes":["Bearer"],"credentials":"[redacted]"}}}`},
		{"agent content", `{"parts":[{"data":{"token":"abc","credentials":"keep"}}]}`, `{"parts":[{"data":{"token":"abc","credentials":"keep"}}]}`},
		{"not JSON", `token: t0k`, `token: t0k`},
		{"cut JSON", `{"configs":[{"url":"https://a","token":"one"},{"url":"https://b","tok`, `{"configs":[{"url":"https://a","token":"[redacted]"},{"url":"https://b","tok`},
		{"cut inside a token", `{"url":"https://a","token":"abcd`, `{"url":"https://a","token":"[redacted]"`},
		{"non-string token", `{"url":"u","token":{"x":1}}`, `{"url":"u","token":"[redacted]"}`},
	} {
		if got := string(Redact([]byte(tc.in))); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// KeepBody keeps a small body whole and cuts a large one, marked with its full size; nil for nothing.
func TestKeepBody(t *testing.T) {
	if KeepBody(nil, 10) != nil || KeepBody([]byte("x"), 0) != nil {
		t.Error("an empty body or bodies off kept something")
	}
	b := KeepBody([]byte(`{"a":1}`), 1024)
	if string(b.Data) != `{"a":1}` || b.Size != 7 || b.Truncated {
		t.Errorf("small: %+v", b)
	}
	b = KeepBody([]byte(strings.Repeat("z", 5000)), 1024)
	if len(b.Data) != 1024 || b.Size != 5000 || !b.Truncated {
		t.Errorf("large: %d bytes %+v", len(b.Data), b.Size)
	}
	b = KeepPrefix([]byte(strings.Repeat("z", 1024)), 9000, 1024)
	if len(b.Data) != 1024 || b.Size != 9000 || !b.Truncated {
		t.Errorf("prefix: %d bytes size %d", len(b.Data), b.Size)
	}
}

// Frames: data lines joined, ids kept, comments and keep-alives not frames, CRLF read, a long event cut with its size,
// frames past FramesTimes × max dropped and counted, an unterminated last event kept.
func TestFrames(t *testing.T) {
	f := NewFrames(time.Now(), 100)
	_, _ = f.Write([]byte(": keep-alive\n\n"))
	_, _ = f.Write([]byte("id: 1\ndata: {\"a\":\ndata: 1}\n\n"))
	_, _ = f.Write([]byte("id: 2\r\ndata: x\r\n\r\n"))
	_, _ = f.Write([]byte("data: " + strings.Repeat("q", 500) + "\n\n"))
	_, _ = f.Write([]byte("data: last"))
	frames, dropped := f.Done()
	if dropped != 0 || len(frames) != 4 {
		t.Fatalf("%d frames, %d dropped: %+v", len(frames), dropped, frames)
	}
	if frames[0].SSEID != "1" || string(frames[0].Data) != "{\"a\":\n1}" || frames[0].Truncated {
		t.Errorf("frame 0: %+v", frames[0])
	}
	if frames[1].SSEID != "2" || string(frames[1].Data) != "x" {
		t.Errorf("frame 1 (CRLF): %+v", frames[1])
	}
	if len(frames[2].Data) != 100 || !frames[2].Truncated || frames[2].Size < 500 {
		t.Errorf("frame 2: %d bytes, %+v", len(frames[2].Data), frames[2].Size)
	}
	if string(frames[3].Data) != "last" {
		t.Errorf("frame 3: %q", frames[3].Data)
	}

	// The exchange's total: FramesTimes × max.
	f = NewFrames(time.Now(), 100)
	for range FramesTimes + 5 {
		_, _ = f.Write([]byte("data: " + strings.Repeat("w", 100) + "\n\n"))
	}
	frames, dropped = f.Done()
	if len(frames) != FramesTimes || dropped != 5 {
		t.Errorf("%d frames kept, %d dropped; want %d and 5", len(frames), dropped, FramesTimes)
	}
}

// Past the body budget, an exchange is queued without its bodies, counted, and its metadata still recorded; the
// budget frees as batches are written.
func TestBodyBudget(t *testing.T) {
	db := &stuckDB{release: make(chan struct{})}
	close(db.release)
	r := New(db, nil).KeepBodies(1024)
	r.budget = 3000
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	big := &Body{Data: make([]byte, 1000), Size: 1000}
	for range 5 {
		r.Record(Exchange{At: time.Now(), Service: "hello", Method: "SendMessage", HTTPStatus: 200, Request: big})
	}
	if r.BodiesDropped() != 2 {
		t.Errorf("bodies dropped %d, want 2 (3000 bytes of budget, 5 × 1000 queued)", r.BodiesDropped())
	}
	cancel()
	<-done
	if db.rows != 5 || r.Dropped() != 0 {
		t.Errorf("stored %d, dropped %d; want all 5 stored", db.rows, r.Dropped())
	}
	if r.queued.Load() != 0 {
		t.Errorf("%d bytes still counted as queued after the write", r.queued.Load())
	}
}

// A recorder without bodies strips any it is handed; a note for a sealed service strips them too.
func TestBodiesOffAndNoBodiesNote(t *testing.T) {
	r := New(&stuckDB{}, nil)
	if r.BodyMax() != 0 {
		t.Fatal("bodies on by default")
	}
	r.Record(Exchange{Request: &Body{Data: []byte("x"), Size: 1}})
	e := <-r.ch
	if e.Request != nil {
		t.Error("a recorder without bodies queued one")
	}
	ctx, n := WithNote(context.Background())
	SetNoBodies(ctx)
	if id, ok := ExchangeID(ctx); !ok || id != n.ID() || id == uuid.Nil {
		t.Errorf("ExchangeID = %v, %v", id, ok)
	}
	e = Exchange{Request: &Body{Data: []byte("x")}, Frames: []Frame{{Data: []byte("y")}}}
	n.Fill(&e)
	if e.Request != nil || e.Frames != nil || e.ID != n.ID() {
		t.Errorf("filled %+v", e)
	}
}

// A callee-leg row keeps its gate task; a caller-leg row never has one.
func TestRowLegs(t *testing.T) {
	g := uuid.New()
	tid := uuid.New()
	row := Exchange{Leg: LegCallee, GateTaskID: &g, TaskID: &tid, CalleeTaskID: "agent-task-1", A2AVersion: "0.3",
		Request: &Body{Data: []byte("{}"), Size: 2}}.row()
	if row[8] != nil || row[16] != LegCallee || row[17] != g || row[18] != "agent-task-1" || row[19] != "0.3" ||
		string(row[20].([]byte)) != "{}" || row[21] != int64(2) {
		t.Errorf("callee row %v", row[16:])
	}
	row = Exchange{Leg: "bogus", GateTaskID: &g, CalleeTaskID: "x", TaskID: &tid}.row()
	if row[8] != &tid || row[16] != LegCaller || row[17] != nil || row[18] != nil || row[20] != nil || row[21] != nil {
		t.Errorf("caller row %v", row[16:])
	}
}
