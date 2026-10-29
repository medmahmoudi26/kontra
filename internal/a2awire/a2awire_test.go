package a2awire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/strictjson"
)

// textsOf collects the text of every part Rewrite visits.
func textsOf(t *testing.T, body string) []string {
	t.Helper()
	var got []string
	_, changed, err := Rewrite([]byte(body), func(p Part) (Part, error) {
		var s string
		if raw, ok := p["text"]; ok && json.Unmarshal(raw, &s) == nil {
			got = append(got, s)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Rewrite(%s): %v", body, err)
	}
	if changed {
		t.Fatal("a walk that replaced nothing reports a change")
	}
	sort.Strings(got)
	return got
}

func p(s string) string { return `{"text":"` + s + `"}` }

// Every level A2A 1.0 puts parts at, in requests and answers, both bindings; nothing below a part, and nothing in
// metadata or a data part's data.
func TestRewriteLevels(t *testing.T) {
	task := `{"id":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"m","role":"ROLE_AGENT","parts":[` + p("status") + `]}},` +
		`"history":[{"messageId":"h1","role":"ROLE_USER","parts":[` + p("h1") + `,` + p("h1b") + `]},{"messageId":"h2","parts":[` + p("h2") + `]}],` +
		`"artifacts":[{"artifactId":"a","parts":[` + p("art") + `]}],"metadata":{"parts":[` + p("meta") + `]}}`
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"JSON-RPC SendMessage": {`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[` +
			p("one") + `,{"data":{"parts":[` + p("in-data") + `]}}],"metadata":{"x":1}},"metadata":{"parts":[` + p("params-meta") + `]}}}`, []string{"one"}},
		"HTTP+JSON send":            {`{"message":{"parts":[` + p("rest") + `]},"configuration":{"returnImmediately":true}}`, []string{"rest"}},
		"JSON-RPC result task":      {`{"jsonrpc":"2.0","id":"x","result":{"task":` + task + `}}`, []string{"art", "h1", "h1b", "h2", "status"}},
		"JSON-RPC result message":   {`{"jsonrpc":"2.0","id":"x","result":{"message":{"parts":[` + p("msg") + `]}}}`, []string{"msg"}},
		"GetTask result":            {`{"jsonrpc":"2.0","id":"x","result":` + task + `}`, []string{"art", "h1", "h1b", "h2", "status"}},
		"HTTP+JSON task":            {task, []string{"art", "h1", "h1b", "h2", "status"}},
		"ListTasks":                 {`{"tasks":[` + task + `,{"id":"u","status":{"message":{"parts":[` + p("u") + `]}}}],"nextPageToken":""}`, []string{"art", "h1", "h1b", "h2", "status", "u"}},
		"status update":             {`{"statusUpdate":{"taskId":"t","status":{"message":{"parts":[` + p("su") + `]}}}}`, []string{"su"}},
		"artifact update":           {`{"artifactUpdate":{"taskId":"t","artifact":{"artifactId":"a","parts":[` + p("au") + `]},"append":true}}`, []string{"au"}},
		"a JSON-RPC error":          {`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"x","data":{"parts":[` + p("no") + `]}}}`, nil},
		"a request without message": {`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"t"}}`, nil},
		"nulls":                     {`{"message":null,"task":{"status":null,"history":null}}`, nil},
	} {
		if got := textsOf(t, tc.body); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: visited %v, want %v", name, got, tc.want)
		}
	}
}

// A level the walker reads is decoded with exact keys (D172); below it, case variants pass.
func TestRewriteStrict(t *testing.T) {
	visit := func(Part) (Part, error) { return nil, nil }
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"parts":[]},"MESSAGE":{"parts":[]}}}`,
		`{"message":{"parts":[{"url":"a","URL":"b"}]}}`,
		`{"message":{"parts":[],"Parts":[]}}`,
		`{"task":{"status":{"message":{}},"STATUS":{}}}`,
		`{"jsonrpc":"2.0","result":{},"result":{}}`,
	} {
		var ke *strictjson.KeyError
		if _, _, err := Rewrite([]byte(body), visit); !errors.As(err, &ke) {
			t.Errorf("%s: %v, want a *strictjson.KeyError", body, err)
		}
	}
	for _, body := range []string{
		`{"message":{"parts":[{"data":{"a":1,"A":2}}]}}`,
		`{"message":{"parts":[{"text":"x","metadata":{"k":1,"K":2}}]}}`,
	} {
		if _, _, err := Rewrite([]byte(body), visit); err != nil {
			t.Errorf("%s: %v (a level the walker does not read)", body, err)
		}
	}
	for _, body := range []string{`{"message":{"parts":{}}}`, `{"message":{"parts":["x"]}}`, `{"task":{"history":{}}}`, `{"message":[]}`} {
		if _, _, err := Rewrite([]byte(body), visit); !errors.Is(err, ErrShape) {
			t.Errorf("%s: %v, want ErrShape", body, err)
		}
	}
	if _, _, err := Rewrite([]byte(`[1]`), visit); err == nil {
		t.Error("a top-level array passed")
	}
}

// fakeObjects serves refs from memory, counting the fetches.
type fakeObjects struct {
	objs    map[blob.Ref][]byte
	fetched []blob.Ref
	refuse  map[blob.Ref]string
}

func (f *fakeObjects) fetch(_ context.Context, ref blob.Ref, w io.Writer) (int64, error) {
	f.fetched = append(f.fetched, ref)
	if r, ok := f.refuse[ref]; ok {
		return 0, &blob.FetchError{Reason: r}
	}
	b, ok := f.objs[ref]
	if !ok {
		return 0, &blob.FetchError{Reason: blob.ReasonNotFound}
	}
	n, err := w.Write(b)
	return int64(n), err
}

func newRef(t *testing.T) blob.Ref {
	t.Helper()
	r, err := blob.NewRef()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// kontra-blob: url parts become raw parts with the object's bytes, keeping filename, mediaType and metadata;
// an object over SpillOver goes through a temporary file, removed at Close; Len is what WriteTo writes.
func TestInline(t *testing.T) {
	small, big := newRef(t), newRef(t)
	f := &fakeObjects{objs: map[blob.Ref][]byte{
		small: bytes.Repeat([]byte{0, 1, 2, 'k'}, 300<<10/4), // 300 KiB, binary
		big:   bytes.Repeat([]byte("8MiB+"), (SpillOver+5)/5+1),
	}}
	dir := t.TempDir()
	in := &Inliner{Fetch: f.fetch, TempDir: dir}
	body := `{"jsonrpc":"2.0","id":7,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","parts":[` +
		`{"url":"` + big.URL() + `","mediaType":"application/octet-stream","filename":"big.bin","metadata":{"k":"v"}},` +
		`{"text":"between"},{"url":"` + small.URL() + `","mediaType":"image/png"},{"url":"https://example.com/x"}]}}}`
	out, err := in.Inline(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed() {
		t.Fatal("nothing inlined")
	}
	spilled, _ := filepath.Glob(filepath.Join(dir, "kontra-blob-*"))
	if len(spilled) != 1 {
		t.Fatalf("spill files %v, want one for the object over %d bytes", spilled, SpillOver)
	}
	b, err := out.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) != out.Len() {
		t.Fatalf("Len %d, wrote %d", out.Len(), len(b))
	}
	if HasRefs(b) {
		t.Fatal("the scheme survived inlining")
	}
	var req struct {
		Params a2a.SendMessageRequest `json:"params"`
		ID     int                    `json:"id"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("the inlined body does not decode: %v", err)
	}
	parts := req.Params.Message.Parts
	if len(parts) != 4 || req.ID != 7 {
		t.Fatalf("parts %d id %d", len(parts), req.ID)
	}
	if !bytes.Equal(parts[0].Raw(), f.objs[big]) || parts[0].Filename != "big.bin" || parts[0].MediaType != "application/octet-stream" ||
		parts[0].Metadata["k"] != "v" {
		t.Errorf("the big part: %d bytes, %q %q %v", len(parts[0].Raw()), parts[0].Filename, parts[0].MediaType, parts[0].Metadata)
	}
	if parts[1].Text() != "between" || !bytes.Equal(parts[2].Raw(), f.objs[small]) || parts[2].MediaType != "image/png" ||
		parts[3].URL() != "https://example.com/x" {
		t.Errorf("the other parts: %+v", parts[1:])
	}
	if len(f.fetched) != 2 {
		t.Errorf("fetched %v: a url part that is not kontra-blob: is never fetched (D56)", f.fetched)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("Close left %v", left)
	}
}

// A2A 0.3 (spec/relay.md §10.4 with §6.2): the gate speaks a2av0 to a tunnel service whose card offers only 0.3,
// and a2a-go's conversion writes a kontra-blob: url part as a FilePart {"kind": "file", "file": {"uri": …}}. The
// hosting node inlines it as the FilePart's bytes (base64), keeping its name, mimeType and metadata; a file whose
// uri is anything else is passed as it came and never fetched (D56).
func TestInlineLegacyFilePart(t *testing.T) {
	ref := newRef(t)
	obj := bytes.Repeat([]byte{0, 3, 'k', 0xff}, 300<<10/4)
	f := &fakeObjects{objs: map[blob.Ref][]byte{ref: obj}}
	legacy, err := json.Marshal(a2av0.FromV1Parts(a2a.ContentParts{
		{Content: a2a.URL(ref.URL()), MediaType: "application/octet-stream", Filename: "big.bin", Metadata: map[string]any{"k": "v"}},
		{Content: a2a.URL("http://169.254.169.254/latest/meta-data")},
		a2a.NewTextPart("hi"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"kind":"message","messageId":"m","role":"user","parts":` +
		string(legacy) + `}}}`
	out, err := (&Inliner{Fetch: f.fetch}).Inline(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	b, err := out.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(blob.URIScheme)) {
		t.Fatalf("a 0.3 FilePart kept the kontra-blob: scheme: %.300s", b)
	}
	var req struct {
		Params struct {
			Message struct {
				Parts []struct {
					Kind     string         `json:"kind"`
					Text     string         `json:"text"`
					Metadata map[string]any `json:"metadata"`
					File     struct {
						URI      *string `json:"uri"`
						Bytes    []byte  `json:"bytes"`
						MimeType string  `json:"mimeType"`
						Name     string  `json:"name"`
					} `json:"file"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("the inlined 0.3 body does not decode: %v", err)
	}
	parts := req.Params.Message.Parts
	if len(parts) != 3 {
		t.Fatalf("%d parts", len(parts))
	}
	p0 := parts[0]
	if p0.Kind != "file" || p0.File.URI != nil || !bytes.Equal(p0.File.Bytes, obj) || p0.File.Name != "big.bin" ||
		p0.File.MimeType != "application/octet-stream" || p0.Metadata["k"] != "v" {
		t.Fatalf("the inlined FilePart: kind %q, uri %v, %d bytes, %q %q %v", p0.Kind, p0.File.URI, len(p0.File.Bytes), p0.File.Name,
			p0.File.MimeType, p0.Metadata)
	}
	if parts[1].File.URI == nil || *parts[1].File.URI != "http://169.254.169.254/latest/meta-data" || parts[2].Text != "hi" {
		t.Fatalf("the other parts changed: %.300s", b)
	}
	if len(f.fetched) != 1 {
		t.Fatalf("fetched %v, want the one ref", f.fetched)
	}
}

// Markers are spliced in the body's order whatever the order of the visits (a Task's status is visited after
// its history, and re-encoded after it as well; artifacts before both).
func TestInlineOrder(t *testing.T) {
	refs := []blob.Ref{newRef(t), newRef(t), newRef(t)}
	f := &fakeObjects{objs: map[blob.Ref][]byte{refs[0]: []byte("status"), refs[1]: []byte("history"), refs[2]: []byte("artifact")}}
	body := `{"task":{"status":{"message":{"parts":[{"url":"` + refs[0].URL() + `"}]}},"history":[{"parts":[{"url":"` + refs[1].URL() + `"}]}],` +
		`"artifacts":[{"artifactId":"a","parts":[{"url":"` + refs[2].URL() + `"}]}]}}`
	out, err := (&Inliner{Fetch: f.fetch}).Inline(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	b, _ := out.Bytes()
	var sr a2a.StreamResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		t.Fatal(err)
	}
	task := sr.Event.(*a2a.Task)
	if string(task.Status.Message.Parts[0].Raw()) != "status" || string(task.History[0].Parts[0].Raw()) != "history" ||
		string(task.Artifacts[0].Parts[0].Raw()) != "artifact" {
		t.Fatalf("parts in the wrong places: %s", b)
	}
}

// A body without the scheme is passed as it came, unparsed (a body that is not JSON at all included); a refusal,
// a malformed kontra-blob: part and too much in all are errors that leave no file behind.
func TestInlineRefusals(t *testing.T) {
	in := &Inliner{Fetch: (&fakeObjects{}).fetch}
	for _, b := range []string{"not json", `{"message":{"parts":[{"text":"x"}]}}`} {
		out, err := in.Inline(context.Background(), []byte(b))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := out.Bytes()
		if string(got) != b || out.Changed() {
			t.Errorf("%q came out as %q", b, got)
		}
	}
	ok, refused := newRef(t), newRef(t)
	dir := t.TempDir()
	f := &fakeObjects{objs: map[blob.Ref][]byte{ok: bytes.Repeat([]byte("x"), SpillOver+1)}, refuse: map[blob.Ref]string{refused: blob.ReasonForbidden}}
	in = &Inliner{Fetch: f.fetch, TempDir: dir}
	_, err := in.Inline(context.Background(), []byte(`{"message":{"parts":[{"url":"`+ok.URL()+`"},{"url":"`+refused.URL()+`"}]}}`))
	var fe *blob.FetchError
	if !errors.As(err, &fe) || fe.Reason != blob.ReasonForbidden {
		t.Fatalf("a refused fetch: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("a failed inlining left %v", left)
	}
	for _, u := range []string{"kontra-blob:../../etc/passwd", "KONTRA-BLOB:" + string(ok), " kontra-blob:x"} {
		if _, err := in.Inline(context.Background(), []byte(`{"message":{"parts":[{"url":"`+u+`"}]}}`)); !errors.Is(err, ErrReservedURL) {
			t.Errorf("%q: %v, want ErrReservedURL", u, err)
		}
	}
	in.MaxTotal = SpillOver
	if _, err := in.Inline(context.Background(), []byte(`{"message":{"parts":[{"url":"`+ok.URL()+`"}]}}`)); !errors.Is(err, ErrTooMuch) ||
		!strings.Contains(err.Error(), "more than") {
		t.Fatalf("over MaxTotal: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("an inlining over the total left %v", left)
	}
}

// SSE: every event is flushed; ids, comments and events without the scheme pass byte for byte; an event with a
// kontra-blob: part is rewritten in place.
func TestInlineSSE(t *testing.T) {
	ref := newRef(t)
	obj := bytes.Repeat([]byte("sse"), 100_000)
	f := &fakeObjects{objs: map[blob.Ref][]byte{ref: obj}}
	plain := "id: 1\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"statusUpdate\":{\"status\":{\"state\":\"TASK_STATE_WORKING\"}}}}\n\n"
	withRef := "id: 2\nevent: x\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"artifactUpdate\":{\"artifact\":{\"artifactId\":\"a\"," +
		"\"parts\":[{\"url\":\"" + ref.URL() + "\",\"mediaType\":\"text/plain\"}]}}}}\nretry: 5\n\n"
	src := ": keep-alive\n\n" + plain + withRef + "data: {\"message\":\n" + "data: {\"parts\":[]}}\n\n"
	var out bytes.Buffer
	flushes := 0
	in := &Inliner{Fetch: f.fetch}
	if err := in.InlineSSE(context.Background(), strings.NewReader(src), &out, func() { flushes++ }); err != nil {
		t.Fatal(err)
	}
	if flushes != 4 {
		t.Errorf("%d flushes, want one per event (4)", flushes)
	}
	got := out.String()
	if !strings.HasPrefix(got, ": keep-alive\n\n"+plain+"id: 2\nevent: x\ndata: ") {
		t.Fatalf("the events before the rewritten one changed:\n%.300s", got)
	}
	if !strings.HasSuffix(got, "retry: 5\n\ndata: {\"message\":\ndata: {\"parts\":[]}}\n\n") {
		t.Fatalf("the fields after the rewritten data, or the last event, changed:\n%s", got[len(got)-200:])
	}
	ev := got[len(": keep-alive\n\n"+plain+"id: 2\nevent: x\ndata: "):]
	line := ev[:strings.Index(ev, "\n")]
	var resp struct {
		Result a2a.StreamResponse `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("the rewritten event: %v", err)
	}
	au := resp.Result.Event.(*a2a.TaskArtifactUpdateEvent)
	if !bytes.Equal(au.Artifact.Parts[0].Raw(), obj) || au.Artifact.Parts[0].MediaType != "text/plain" {
		t.Fatalf("the rewritten part: %d bytes", len(au.Artifact.Parts[0].Raw()))
	}
	if HasRefs(out.Bytes()) {
		t.Fatal("the scheme survived")
	}
}

// A refused fetch inside an SSE stream ends it with the error.
func TestInlineSSERefused(t *testing.T) {
	ref := newRef(t)
	in := &Inliner{Fetch: (&fakeObjects{}).fetch}
	src := "data: {\"message\":{\"parts\":[{\"url\":\"" + ref.URL() + "\"}]}}\n\n"
	var fe *blob.FetchError
	if err := in.InlineSSE(context.Background(), strings.NewReader(src), io.Discard, nil); !errors.As(err, &fe) {
		t.Fatalf("%v, want the fetch's refusal", err)
	}
}

func TestMediaTypes(t *testing.T) {
	if !IsEventStream("text/event-stream; charset=utf-8") || IsEventStream("application/json") {
		t.Error("IsEventStream")
	}
	if !IsJSON("application/json") || !IsJSON("application/a2a+json; x=1") || IsJSON("text/plain") {
		t.Error("IsJSON")
	}
	if HasRefs([]byte("kontra-blo")) || !HasRefs([]byte("..KoNtRa-BlOb:x")) {
		t.Error("HasRefs")
	}
}
