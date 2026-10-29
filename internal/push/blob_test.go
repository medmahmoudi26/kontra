package push

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/blob/blobtest"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// A push receiver gets a part in the bucket as a presigned GET that fetches the object's bytes (spec §10.4: push
// receivers, like callers), rendered at delivery; the log keeps kontra-blob:<ref>. A part naming another task's
// object stays unrendered (D181).
func TestDeliveryRendersBucketParts(t *testing.T) {
	bucket := blobtest.New(t, "push")
	e := newEnv(t)
	ctx := context.Background()
	cs := NewConfigStore(e.pool, nil, private, nil)
	r := newReceiver(t, nil)
	id := e.newTask(buyerA)
	other := e.newTask(buyerB)
	cid, _ := e.saveInline(cs, id, buyerA, pushConfig(r.url("/hook")))

	data := bytes.Repeat([]byte("kontra-"), 300<<10/7)
	own, ownRow, err := blob.Externalize(ctx, bucket.Store, e.svc.TenantID, id, &a2a.Part{Content: a2a.Raw(data), MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, foreignRow, err := blob.Externalize(ctx, bucket.Store, e.svc.TenantID, other, a2a.NewTextPart(strings.Repeat("o", 300<<10)))
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.Insert(ctx, e.pool, []blob.Row{ownRow, foreignRow}); err != nil {
		t.Fatal(err)
	}
	ev := a2a.NewArtifactEvent(info(id), own, foreign)
	if _, err := write(ctx, e.log, id, store.KindArtifact, "", ev); err != nil {
		t.Fatal(err)
	}
	term := e.status(id, store.StateCompleted)

	opts := fast()
	opts.Render = blob.NewRenderer(bucket.Store, e.pool, quiet)
	d := NewDeliverer(e.log, private, opts)
	o := waitOut(t, runPush(ctx, d, id, cid), 10*time.Second)
	if o.err != nil || o.res != (lifecycle.PushResult{DeliveredSeq: term, Stopped: StoppedTerminal}) {
		t.Fatalf("push_webhook: %+v %v", o.res, o.err)
	}
	got := r.requests()
	if len(got) == 0 {
		t.Fatal("nothing delivered")
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal([]byte(got[len(got)-1].body), &sr); err != nil {
		t.Fatal(err)
	}
	task, ok := sr.Event.(*a2a.Task)
	if !ok || len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 2 {
		t.Fatalf("terminal snapshot %s", got[len(got)-1].body)
	}
	ps := task.Artifacts[0].Parts
	u, _ := ps[0].Content.(a2a.URL)
	if !strings.HasPrefix(string(u), bucket.Endpoint+"/") || ps[0].MediaType != "application/octet-stream" {
		t.Fatalf("the task's own part reached the receiver as %v", ps[0].Content)
	}
	resp, err := http.Get(string(u))
	if err != nil {
		t.Fatal(err)
	}
	fetched, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(fetched, data) {
		t.Fatalf("the presigned GET: %d, %d bytes (want the %d stored)", resp.StatusCode, len(fetched), len(data))
	}
	if ps[1].Content != a2a.URL(foreignRow.Ref.URL()) {
		t.Errorf("another task's ref was rendered for this task: %v", ps[1].Content)
	}
	// The log keeps the gate's names.
	if row, err := e.log.Load(ctx, id); err != nil || row.Snapshot.Artifacts[0].Parts[0].Content != a2a.URL(ownRow.Ref.URL()) {
		t.Errorf("the stored projection changed: %v", err)
	}
}
