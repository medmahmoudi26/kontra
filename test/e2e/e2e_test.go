package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/test/e2e/slowagent"
)

func TestMain(m *testing.M) {
	code := m.Run()
	removeRoot()
	os.Exit(code)
}

// start brings the harness up for one test. Under -short every e2e test is skipped; without -short a
// missing tool or a harness that cannot start fails the test, so `go test ./...` never reports an e2e
// pass that tested nothing. On a failed test the replicas' and Temporal's logs are printed before the
// harness is removed.
func start(t *testing.T, o Options) *Harness {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: needs Postgres, the temporal CLI and two gate replicas; skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("harness logs:\n%s", h.LogTail(150))
		}
	})
	// Before the harness stops them: nothing it started listens off loopback (D185).
	t.Cleanup(func() {
		if err := h.CheckLoopback(); err != nil {
			t.Error(err)
		}
	})
	t.Logf("harness up: replicas A=%s (%s) B=%s (%s), upstream %s", h.Gates[0].ReplicaID, h.Gates[0].URL,
		h.Gates[1].ReplicaID, h.Gates[1].URL, h.Upstream.URL())
	return h
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func newMessage(text string) *a2a.SendMessageRequest {
	return &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))}
}

// ids returns the ids of the events that carry one, in order.
func ids(evs []Event) []int64 {
	var out []int64
	for _, e := range evs {
		if e.ID != nil {
			out = append(out, *e.ID)
		}
	}
	return out
}

// checkStrictlyIncreasing fails unless seqs strictly increase.
func checkStrictlyIncreasing(t *testing.T, what string, seqs []int64) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("%s: ids not strictly increasing at %d: %v", what, i, seqs)
		}
	}
}

// checkCarried fails unless the ids a buyer received are exactly the log's carried seqs: in order, no gap,
// no duplicate.
func checkCarried(t *testing.T, h *Harness, taskID a2a.TaskID, got []int64) {
	t.Helper()
	want, err := h.CarriedSeqs(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	checkStrictlyIncreasing(t, "the buyer's ids", got)
	if !slices.Equal(got, want) {
		t.Fatalf("the buyer's ids are not the log's carried seqs:\n got %v\nwant %v", got, want)
	}
}

// canonical is a task as JSON, for comparisons.
func canonical(t *testing.T, task *a2a.Task) string {
	t.Helper()
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fold is the buyer's own fold of the events it received (a2a-go's update rules): the first must be the
// task.
func fold(evs []Event) (*a2a.Task, error) {
	if len(evs) == 0 {
		return nil, errors.New("no events")
	}
	task, ok := evs[0].Event.(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("the first event is a %s, not the task", evs[0].Kind)
	}
	task = cloneTask(task)
	for _, e := range evs[1:] {
		next, err := a2aevent.ApplyUpdate(task, e.Event)
		if err != nil {
			return nil, err
		}
		task = next
	}
	return task, nil
}

func cloneTask(t *a2a.Task) *a2a.Task {
	b, _ := json.Marshal(t)
	var c a2a.Task
	_ = json.Unmarshal(b, &c)
	return &c
}

// readUntil reads events until ok returns true for one (included), or the stream ends.
func readUntil(t *testing.T, ctx context.Context, s *Stream, ok func(Event) bool) []Event {
	t.Helper()
	var evs []Event
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("stream ended before the expected event (%d read): %v", len(evs), err)
		}
		evs = append(evs, ev)
		if ok(ev) {
			return evs
		}
	}
}

// artifactParts are the texts of a task's artifact parts, artifact by artifact.
func artifactParts(task *a2a.Task) []string {
	var out []string
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			out = append(out, string(a.ID)+":"+p.Text())
		}
	}
	return out
}

// checkUpstreamHeaders fails if the upstream received a caller's Authorization or Cookie header, a gate
// API key, or a push config (spec §6.2, §10.1).
func checkUpstreamHeaders(t *testing.T, h *Harness, a *slowagent.Agent) {
	t.Helper()
	reqs := a.Requests()
	if len(reqs) == 0 {
		t.Fatal("the upstream received no request")
	}
	for _, r := range reqs {
		for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if v := r.Header.Values(name); len(v) > 0 {
				t.Errorf("the upstream received %s on %s: %q", name, r.Method, v)
			}
		}
		for _, secret := range []string{h.APIKey, h.APIKeyB, "taskPushNotificationConfig", "pushNotificationConfig", "e2e-cookie"} {
			if bytes.Contains(r.Body, []byte(secret)) {
				t.Errorf("the upstream received %q in the body of %s", secret, r.Method)
			}
			for name, vs := range r.Header {
				for _, v := range vs {
					if strings.Contains(v, secret) {
						t.Errorf("the upstream received %q in header %s of %s", secret, name, r.Method)
					}
				}
			}
		}
	}
}

// --- Temporal histories ---

// historyText is everything a history says, its payloads decoded: what a check for content that must
// never enter Temporal history (events, credentials) reads.
func historyText(t *testing.T, js []byte) string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(js, &doc); err != nil {
		t.Fatalf("history is not JSON: %v", err)
	}
	var b strings.Builder
	b.Write(js)
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok && (k == "data" || k == "details") {
					if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
						b.WriteByte('\n')
						b.Write(dec)
					}
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	return b.String()
}

var identity = regexp.MustCompile(`"identity":\s*"[^"]*"`)

// scrubbed is the host name's stand-in in a recorded history: the workers' identity, and the replica ids
// (<host>-<hex>) inside payloads and in the task queues named after them (kontra-ctl-<replica id>).
const scrubbed = "kontra-test"

// scrubHost removes host from a history export (temporal workflow show -o json): from its text (task
// queue names, failure messages) and from inside every payload, whose data is base64 the text does not
// show (attempt_state's result carries replicaId). A payload whose decoded bytes hold host is re-encoded
// with it replaced; every other byte of the export is kept as it is. A host name shorter than three
// characters is left alone: replacing it would mangle unrelated text.
func scrubHost(js []byte, host string) ([]byte, error) {
	if len(host) < 3 {
		return js, nil
	}
	var doc any
	if err := json.Unmarshal(js, &doc); err != nil {
		return nil, fmt.Errorf("history is not JSON: %w", err)
	}
	swap := map[string]string{} // base64 text -> base64 text, the host replaced inside
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if str, ok := e.(string); ok && (k == "data" || k == "details") {
					if dec, err := base64.StdEncoding.DecodeString(str); err == nil && bytes.Contains(dec, []byte(host)) {
						swap[str] = base64.StdEncoding.EncodeToString(bytes.ReplaceAll(dec, []byte(host), []byte(scrubbed)))
					}
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	for from, to := range swap {
		js = bytes.ReplaceAll(js, []byte(`"`+from+`"`), []byte(`"`+to+`"`))
	}
	return bytes.ReplaceAll(js, []byte(host), []byte(scrubbed)), nil
}

// checkHistory exports the task's TaskLifecycle history (temporal workflow show -o json), checks that it
// holds none of the forbidden strings (events and credentials never enter history), replays it against
// the current TaskLifecycle (spec §14.4: the replayer of WP-05), and with KONTRA_RECORD_HISTORIES=1 writes it
// to internal/lifecycle/testdata/histories/<name>.json unless that file exists.
func checkHistory(t *testing.T, h *Harness, taskID a2a.TaskID, name string, forbidden ...string) []byte {
	t.Helper()
	ctx := ctxFor(t, 3*time.Minute)
	if err := h.WaitWorkflowClosed(ctx, taskID, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	js, err := h.WorkflowHistory(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	text := historyText(t, js)
	// The workflow's input payload names the task: the payloads were decoded, so the checks below read them.
	if !strings.Contains(text, `"taskId":"`+string(taskID)+`"`) {
		t.Fatalf("the history's payloads were not decoded: no input naming %s", taskID)
	}
	for _, f := range forbidden {
		if f != "" && strings.Contains(text, f) {
			t.Errorf("the TaskLifecycle history of %s holds %q", taskID, f)
		}
	}
	// The workers' identity (pid@host), the sticky and control queues and the replica ids in payloads name
	// the host: replay reads none of them, and a recording must not carry the developer's machine.
	js = identity.ReplaceAll(js, []byte(`"identity": "`+scrubbed+`"`))
	if host, err := os.Hostname(); err == nil && host != "" {
		if js, err = scrubHost(js, host); err != nil {
			t.Fatal(err)
		}
		if len(host) >= 3 && strings.Contains(historyText(t, js), host) {
			t.Fatalf("the history of %s still names the host %q after the scrub", taskID, host)
		}
	}
	path := filepath.Join(t.TempDir(), name+".json")
	if err := os.WriteFile(path, js, 0o644); err != nil {
		t.Fatal(err)
	}
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(lifecycle.TaskLifecycle, workflow.RegisterOptions{Name: lifecycle.WorkflowType})
	if err := r.ReplayWorkflowHistoryFromJSONFile(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), path); err != nil {
		t.Fatalf("the history of %s does not replay against TaskLifecycle: %v", taskID, err)
	}
	if os.Getenv(RecordEnv) == "1" {
		dst := filepath.Join("..", "..", "internal", "lifecycle", "testdata", "histories", name+".json")
		if _, err := os.Stat(dst); err == nil {
			t.Logf("%s exists; a recorded history is never re-recorded over", dst)
		} else if err := os.WriteFile(dst, js, 0o644); err != nil {
			t.Fatal(err)
		} else {
			t.Logf("recorded %s", dst)
		}
	}
	return js
}
