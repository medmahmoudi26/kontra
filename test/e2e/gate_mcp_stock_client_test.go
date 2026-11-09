package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/gatemcp"
)

// gate_mcp_stock_client (spec/relay.md §16; §12.2, §12.3; D324). The gate's own /mcp called by the stock MCP
// TypeScript SDK, unmodified, whose Client validates every tool result's structuredContent against the tool's
// outputSchema with its own Ajv and throws when it does not match: @modelcontextprotocol/client 2.4.0 on Node 24,
// pinned to 2026-07-28 and declaring URL and form elicitation without the tasks extension (as Claude Code does), and
// @modelcontextprotocol/sdk 1.32.2 on the 2025-11-25 era (examples/mcp/everything's lock), which validates the
// structuredContent of an isError result too. The service free is hosted by a node. For each client, read from
// structuredContent alone:
//
//   - tools/list gives hire, reply and cancel the outputSchema of spec/schema/mcp-tools.json's hireResult (the tools
//     that follow a task their own), and the client's own validator accepts every shape of its fixtures
//     (examples/mcp-tools.hireResult.*.json) and refuses bad ones;
//   - a hire of the seller's artifact kinds: the answer's text, data and file links; a failing one is isError with the
//     failure's text; a question is its text with the reply-tool hint, and reply's answer is the follow-up's text.
func TestGateMCPStockClient(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped under -short")
	}
	node24 := Node24(t)
	seller := StartSeller(t)
	h := start(t, Options{Tunnel: map[string]TunnelService{"free": {}}})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	hostOn(t, ctx, h, b, map[string]string{"free": seller.URL})
	v2 := installed(t, "test/e2e/testdata/mcp-client-2", "@modelcontextprotocol/client@2.4.0")
	v1 := installed(t, "examples/mcp/everything", "@modelcontextprotocol/sdk@1.32.2")
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(mcpToolsDef(t, "hireResult"), &schema); err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Glob(filepath.Join(repoRoot, "spec", "schema", "examples", "mcp-tools.hireResult.*.json"))
	if err != nil || len(fixtures) < 7 {
		t.Fatalf("the hireResult fixtures: %v %v", fixtures, err)
	}
	var good []json.RawMessage
	for _, f := range fixtures {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		good = append(good, b)
	}
	const task = `"taskId":"019a0000-0000-7000-8000-000000000001"`
	bad := []string{`{` + task + `,"state":"TASK_STATE_WORKING","approveUrl":"https://a.example"}`, `{` + task + `}`,
		`{` + task + `,"state":"working"}`, `{"taskId":"t1","state":"TASK_STATE_WORKING"}`,
		`{` + task + `,"state":"TASK_STATE_WORKING","url":"https://a.example"}`,
		`{` + task + `,"state":"TASK_STATE_COMPLETED","data":[]}`}
	goodJSON, _ := json.Marshal(good)
	check := `{"good":` + string(goodJSON) + `,"bad":[` + strings.Join(bad, ",") + `]}`
	goods, bads := len(good), len(bad)

	for _, c := range []struct {
		name, node, dir, sdk string
	}{
		{"client 2.4.0, 2026-07-28", node24, v2, "v2"},
		{"sdk 1.32.2, 2025-11-25", nodeBin, v1, "v1"},
	} {
		run := func(calls string) []stockLine {
			t.Helper()
			lines := h.runStockClient(t, ctx, c.node, c.dir, c.sdk, a.URL+gatemcp.Path, `{"calls":`+calls+`,"check":`+check+`}`)
			// The control: the client's own validator, the one each call went through, accepts every shape of
			// spec/schema/examples/mcp-tools.hireResult.*.json and refuses the bad ones.
			last := lines[len(lines)-1]
			if last.Check == nil || len(last.Check.Good) != goods || len(last.Check.Bad) != bads ||
				strings.Contains(fmt.Sprint(last.Check.Good), "false") || strings.Contains(fmt.Sprint(last.Check.Bad), "true") {
				t.Fatalf("%s: the stock validator's control: %+v", c.name, last.Check)
			}
			return lines[:len(lines)-1]
		}
		lines := run(`[{"tool":"hire","arguments":{"service":"free","message":"kinds"}},
			{"tool":"hire","arguments":{"service":"free","message":"fail"}},
			{"tool":"hire","arguments":{"service":"free","message":"ask"}},
			{"tool":"reply","arguments":{"taskId":"$taskId","message":"friday"}}]`)
		if len(lines) != 5 || lines[0].Tools == nil {
			t.Fatalf("%s: %d lines: %+v", c.name, len(lines), lines)
		}
		// hire, reply and cancel declare hireResult; the tools that follow a task their own (D332..D337); search and
		// describe none.
		outDefs := map[string]string{"hire": "hireResult", "reply": "hireResult", "cancel": "hireResult", "status": "statusResult",
			"tasks": "tasksResult", "artifact": "artifactResult", "push_set": "pushResult", "push_list": "pushResult",
			"push_delete": "pushResult"}
		for _, tool := range lines[0].Tools {
			def, want := outDefs[tool.Name]
			var wantSchema map[string]any
			if want {
				if err := json.Unmarshal(mcpToolsDef(t, def), &wantSchema); err != nil {
					t.Fatal(err)
				}
			}
			if got := tool.OutputSchema != nil; got != want || (got && fmt.Sprint(tool.OutputSchema) != fmt.Sprint(wantSchema)) {
				t.Errorf("%s: tools/list's %s outputSchema %v", c.name, tool.Name, tool.OutputSchema)
			}
		}
		for i, l := range lines[1:] {
			if l.Error != "" || l.Result == nil {
				t.Fatalf("%s: call %d: the stock client refused the answer: %s (%v)", c.name, i, l.Error, l.Code)
			}
		}
		if f := lines[1].structured(t); f.State != "TASK_STATE_COMPLETED" || f.Text != "done\nGenerated text content" ||
			fmt.Sprint(f.Data) != "[map[total:4] map[count:42 key:value]]" || len(f.Files) != 2 {
			t.Errorf("%s: a hire's structuredContent %+v", c.name, f)
		}
		if f := lines[2].structured(t); !lines[2].Result.IsError || f.State != "TASK_STATE_FAILED" || f.Text != "the exec failed: exit status 1" {
			t.Errorf("%s: a failing hire: %+v", c.name, f)
		}
		ask := lines[3].structured(t)
		if ask.State != "TASK_STATE_INPUT_REQUIRED" || ask.Text != "Which date? (answer with the reply tool, taskId "+ask.TaskID+")" {
			t.Errorf("%s: a question: %+v", c.name, ask)
		}
		if r := lines[4].structured(t); r.TaskID != ask.TaskID || r.State != "TASK_STATE_COMPLETED" || r.Text != "booked for friday" {
			t.Errorf("%s: reply: %+v", c.name, r)
		}
	}
}

// stockLine is one line the stock client printed: the tools it listed, or a call's result or error.
type stockLine struct {
	Tools []struct {
		Name         string         `json:"name"`
		OutputSchema map[string]any `json:"outputSchema"`
	} `json:"tools"`
	Result *struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	} `json:"result"`
	Error string `json:"error"`
	Code  any    `json:"code"`
	Check *struct {
		Good []bool `json:"good"`
		Bad  []bool `json:"bad"`
	} `json:"check"`
}

// structured is the line's structuredContent, read with exact keys: what a client that shows nothing else sees.
func (l stockLine) structured(t *testing.T) gatemcp.HireResult {
	t.Helper()
	var hr gatemcp.HireResult
	if l.Result == nil || len(l.Result.StructuredContent) == 0 {
		t.Fatalf("no structuredContent: %+v", l)
	}
	dec := json.NewDecoder(bytes.NewReader(l.Result.StructuredContent))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hr); err != nil {
		t.Fatalf("structuredContent %s: %v", l.Result.StructuredContent, err)
	}
	return hr
}

// runStockClient runs testdata/stock_mcp_client.mjs with node from the npm install dir, against endpoint with the
// harness's API key, sending calls on stdin, and returns the lines it printed; the process dies with the test.
func (h *Harness) runStockClient(t *testing.T, ctx context.Context, node, dir, sdk, endpoint, calls string) []stockLine {
	t.Helper()
	script, err := filepath.Abs(filepath.Join(repoRoot, "test", "e2e", "testdata", "stock_mcp_client.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(h.Dir, "home-stock-client-")
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, node, script, dir, sdk, endpoint)
	cmd.Dir = home
	cmd.Env = h.stockEnv(home, "KONTRA_E2E_MCP_KEY="+h.APIKey)
	cmd.Stdin = strings.NewReader(calls)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = procAttr()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the stock client (%s): %v\n%s\n%s", sdk, err, out, lastLines(stderr.Bytes(), 20))
	}
	var lines []stockLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var l stockLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("the stock client printed %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	return lines
}

// mcpToolsDef is spec/schema/mcp-tools.json's $defs entry name.
func mcpToolsDef(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "spec", "schema", "mcp-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	return file.Defs[name]
}
