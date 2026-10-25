package e2e

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// payload is a Temporal payload as `temporal workflow show -o json` exports it.
func payload(t *testing.T, v string) map[string]any {
	t.Helper()
	return map[string]any{
		"metadata": map[string]any{"encoding": base64.StdEncoding.EncodeToString([]byte("json/plain"))},
		"data":     base64.StdEncoding.EncodeToString([]byte(v)),
	}
}

// scrubHost takes the host name out of a history's text and out of its payloads, whose base64 data the text
// does not show, and leaves every other byte alone. Runs without the harness.
func TestScrubHost(t *testing.T) {
	const host = "dev-box-7"
	doc := map[string]any{"events": []any{
		map[string]any{"eventId": "5", "activityTaskScheduledEventAttributes": map[string]any{
			"taskQueue": map[string]any{"name": "kontra-ctl-" + host + "-a7c00987"},
			"input":     map[string]any{"payloads": []any{payload(t, `{"taskId":"t1","n":1}`)}},
		}},
		map[string]any{"eventId": "6", "activityTaskCompletedEventAttributes": map[string]any{
			"identity": "2833969@" + host + "@",
			"result":   map[string]any{"payloads": []any{payload(t, `{"replicaId":"`+host+`-a7c00987","firstUpstreamIdAt":null}`)}},
		}},
	}}
	js, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(historyText(t, js), `"replicaId":"`+host+`-a7c00987"`) {
		t.Fatal("the control: the payload names the host")
	}
	out, err := scrubHost(js, host)
	if err != nil {
		t.Fatal(err)
	}
	text := historyText(t, out)
	if strings.Contains(text, host) {
		t.Fatalf("the host is still in the history:\n%s", text)
	}
	for _, want := range []string{`"replicaId":"kontra-test-a7c00987"`, `kontra-ctl-kontra-test-a7c00987`, `"taskId":"t1"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the scrubbed history lacks %s:\n%s", want, text)
		}
	}
	// The payload without the host is untouched, byte for byte; the export is still JSON.
	untouched := payload(t, `{"taskId":"t1","n":1}`)["data"].(string)
	if !strings.Contains(string(out), `"`+untouched+`"`) {
		t.Error("a payload without the host was re-encoded")
	}
	if !json.Valid(out) {
		t.Fatal("the scrubbed history is not JSON")
	}
	if got, _ := scrubHost(js, "x"); string(got) != string(js) {
		t.Error("a one-character host name changed the history")
	}
}

// The e2e recordings carry no machine's host name: every replica id in their payloads, and every worker
// identity, is the scrubbed stand-in. Runs without the harness, on whatever machine recorded the files.
func TestRecordedHistoriesNameNoHost(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "lifecycle", "testdata", "histories", "e2e_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no e2e recordings in internal/lifecycle/testdata/histories")
	}
	replica := regexp.MustCompile(`"replicaId":\s*"([^"]*)"`)
	queue := regexp.MustCompile(`kontra-(?:ctl|tunnel)-([A-Za-z0-9._-]+)`)
	ident := regexp.MustCompile(`"identity":\s*"([^"]*)"`)
	for _, f := range files {
		js, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := historyText(t, js)
		for _, m := range replica.FindAllStringSubmatch(text, -1) {
			if m[1] != "" && !strings.HasPrefix(m[1], scrubbed+"-") {
				t.Errorf("%s: a payload names replica %q, not %s-<hex>", filepath.Base(f), m[1], scrubbed)
			}
		}
		for _, m := range queue.FindAllStringSubmatch(text, -1) {
			if !strings.HasPrefix(m[1], scrubbed+"-") {
				t.Errorf("%s: task queue %s names replica %q", filepath.Base(f), m[0], m[1])
			}
		}
		for _, m := range ident.FindAllStringSubmatch(string(js), -1) {
			if m[1] != scrubbed {
				t.Errorf("%s: worker identity %q", filepath.Base(f), m[1])
			}
		}
	}
}
