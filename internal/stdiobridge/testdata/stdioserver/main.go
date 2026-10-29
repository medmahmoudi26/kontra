// Command stdioserver is a Go stdio MCP server (2025-11-25) for internal/stdiobridge's tests and test/e2e: one
// JSON-RPC message per line on stdin and stdout, as the TypeScript SDK's StdioServerTransport speaks.
//
// It records what it sees under -record DIR, each file written whole (a temporary file renamed): start-<pid>.json
// ({pid, pgid, grandchild}), init-<pid>.json (the initialize's params), roots-<pid>.json (the answer to the
// roots/list it asks right after notifications/initialized), sample-<pid>.json (the answer to its
// sampling/createMessage) and received-<pid>.jsonl (every line it read, in order).
//
// Tools (tools/call name, arguments):
//
//	echo {text}               the text back
//	progress {n}              n notifications/progress with the request's _meta.progressToken, then the answer
//	notify {n}                n notifications/message (routed to the GET stream), then the answer
//	sample {}                 a sampling/createMessage request to the client, answered at once
//	ask {text}                a sampling/createMessage request whose prompt quotes text, answered once the client
//	                          answers it (as server-everything's sampleLLM)
//	related {taskId}          a notifications/message related to taskId, then the answer, then the answer to the
//	                          tasks/result of that task it holds
//	longrun {text, delayMs}   with params.task, a task (task-<n>, completed, statusMessage "status of <text>") whose
//	                          CreateTaskResult comes delayMs later, and whose tasks/get, tasks/list and tasks/result
//	                          answer at once ("result of <text>"); without, "result of <text>"
//	stderr {bytes}            bytes written to stderr in lines of 1 KiB, then the answer
//	env {}                    its environment, one variable per line
//	pid {}                    its pid and process group
//	crash {}                  exits 3 without an answer
//
// Flags: -sample-on-init sends sampling/createMessage right after notifications/initialized; -fork starts a
// grandchild in its process group that ignores SIGTERM; -ignore-term ignores SIGTERM; -hang keeps running after
// stdin closes (the default is to exit 0). It is in testdata so that `go build ./...` leaves it out; tests build it.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	record       = flag.String("record", "", "directory for the records")
	sampleOnInit = flag.Bool("sample-on-init", false, "send sampling/createMessage after notifications/initialized")
	fork         = flag.Bool("fork", false, "start a grandchild that ignores SIGTERM")
	ignoreTerm   = flag.Bool("ignore-term", false, "ignore SIGTERM")
	hang         = flag.Bool("hang", false, "keep running after stdin closes")
	grandchild   = flag.Bool("grandchild", false, "be the grandchild: ignore SIGTERM and sleep")
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

var (
	outMu sync.Mutex
	out   = bufio.NewWriter(os.Stdout)
	pid   = os.Getpid()
	// held is the id of the tasks/result request held for each task.
	held = map[string]json.RawMessage{}
	// asked is the id of the ask call waiting for each sampling request's answer.
	asked = map[string]json.RawMessage{}
	// tasks are the tasks longrun created, in order.
	tasks []*task
)

// task is a task longrun created: completed at once, its CreateTaskResult sent later.
type task struct {
	id, text string
}

func (t *task) object() map[string]any {
	return map[string]any{"taskId": t.id, "status": "completed", "statusMessage": "status of " + t.text,
		"createdAt": "2026-10-28T00:00:00Z", "lastUpdatedAt": "2026-10-28T00:00:00Z", "ttl": 60000}
}

func taskOf(params json.RawMessage) *task {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(params, &p)
	for _, t := range tasks {
		if t.id == p.TaskID {
			return t
		}
	}
	return nil
}

func main() {
	flag.Parse()
	if *grandchild {
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}
	if *ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	gpid := 0
	if *fork {
		// The grandchild holds the pipes too, as npx's node does: the leader's exit is not the pipes' end.
		cmd := exec.Command(os.Args[0], "-grandchild")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fork:", err)
			os.Exit(1)
		}
		gpid = cmd.Process.Pid
	}
	pgid, _ := syscall.Getpgid(pid)
	writeRecord("start", map[string]int{"pid": pid, "pgid": pgid, "grandchild": gpid})
	fmt.Fprintln(os.Stderr, "stdioserver: started")

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			appendRecord("received", line)
			var m message
			if json.Unmarshal(line, &m) != nil {
				fmt.Fprintln(os.Stderr, "stdioserver: not JSON:", string(line))
			} else {
				handle(m)
			}
		}
		if err != nil {
			break
		}
	}
	if *hang {
		for {
			time.Sleep(time.Hour)
		}
	}
}

func handle(m message) {
	switch {
	case m.Method == "" && m.ID != nil:
		// A response from the client.
		var id string
		_ = json.Unmarshal(m.ID, &id)
		switch {
		case strings.HasPrefix(id, "roots-"):
			writeRecord("roots", json.RawMessage(orNull(m.Result)))
		case strings.HasPrefix(id, "sample-"):
			writeRecord("sample", json.RawMessage(orNull(m.Result)))
			if call, ok := asked[id]; ok {
				delete(asked, id)
				reply(call, map[string]any{"content": []any{map[string]any{"type": "text", "text": "answered"}}, "isError": false})
			}
		}
	case m.Method == "initialize":
		writeRecord("init", json.RawMessage(orNull(m.Params)))
		reply(m.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "stdioserver", "version": "1.0.0"}})
	case m.Method == "notifications/initialized":
		send(map[string]any{"jsonrpc": "2.0", "id": "roots-1", "method": "roots/list"})
		if *sampleOnInit {
			sample("sample-1")
		}
	case m.Method == "tools/list":
		var tools []map[string]any
		for _, n := range []string{"echo", "progress", "notify", "sample", "ask", "related", "longrun", "stderr", "env", "pid", "crash"} {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		reply(m.ID, map[string]any{"tools": tools})
	case m.Method == "tools/call":
		call(m)
	case m.Method == "tasks/result" && taskOf(m.Params) != nil:
		t := taskOf(m.Params)
		reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "result of " + t.text}}, "isError": false,
			"_meta": map[string]any{"io.modelcontextprotocol/related-task": map[string]any{"taskId": t.id}}})
	case m.Method == "tasks/result":
		var p struct {
			TaskID string `json:"taskId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		held[p.TaskID] = m.ID
	case m.Method == "tasks/get" && taskOf(m.Params) != nil:
		reply(m.ID, taskOf(m.Params).object())
	case m.Method == "tasks/list":
		list := []any{}
		for _, t := range tasks {
			list = append(list, t.object())
		}
		reply(m.ID, map[string]any{"tasks": list})
	case m.Method == "ping":
		reply(m.ID, map[string]any{})
	case m.ID != nil:
		send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
	}
}

var samples int

func sample(id string) { samplePrompt(id, "hi") }

func samplePrompt(id, prompt string) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "sampling/createMessage", "params": map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": prompt}}}, "maxTokens": 10}})
}

func call(m message) {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Text    string `json:"text"`
			N       int    `json:"n"`
			TaskID  string `json:"taskId"`
			Bytes   int    `json:"bytes"`
			DelayMs int    `json:"delayMs"`
		} `json:"arguments"`
		Task json.RawMessage `json:"task"`
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(m.Params, &p)
	text := func(s string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": false}
	}
	switch p.Name {
	case "echo":
		reply(m.ID, text(p.Arguments.Text))
	case "progress":
		for i := 1; i <= p.Arguments.N; i++ {
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress",
				"params": map[string]any{"progressToken": p.Meta.ProgressToken, "progress": i, "total": p.Arguments.N}})
		}
		reply(m.ID, text("progressed"))
	case "notify":
		for i := 1; i <= p.Arguments.N; i++ {
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/message",
				"params": map[string]any{"level": "info", "data": fmt.Sprintf("note %d", i)}})
		}
		reply(m.ID, text("notified"))
	case "sample":
		samples++
		sample(fmt.Sprintf("sample-%d", samples+1))
		reply(m.ID, text("asked"))
	case "ask":
		samples++
		id := fmt.Sprintf("sample-%d", samples+1)
		asked[id] = m.ID
		samplePrompt(id, "prompt built from the arguments: "+p.Arguments.Text)
	case "related":
		send(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info",
			"data": "about " + p.Arguments.TaskID, "_meta": map[string]any{"io.modelcontextprotocol/related-task": map[string]any{"taskId": p.Arguments.TaskID}}}})
		reply(m.ID, text("related"))
		if id, ok := held[p.Arguments.TaskID]; ok {
			delete(held, p.Arguments.TaskID)
			reply(id, text("result of "+p.Arguments.TaskID))
		}
	case "longrun":
		if len(p.Task) == 0 {
			reply(m.ID, text("result of "+p.Arguments.Text))
			break
		}
		t := &task{id: fmt.Sprintf("task-%d", len(tasks)+1), text: p.Arguments.Text}
		tasks = append(tasks, t)
		obj, delay := t.object(), time.Duration(p.Arguments.DelayMs)*time.Millisecond
		go func() {
			time.Sleep(delay)
			reply(m.ID, map[string]any{"task": obj})
		}()
	case "stderr":
		line := strings.Repeat("e", 1023) + "\n"
		for written := 0; written < p.Arguments.Bytes; written += len(line) {
			os.Stderr.WriteString(line)
		}
		reply(m.ID, text("wrote stderr"))
	case "env":
		env := os.Environ()
		sort.Strings(env)
		reply(m.ID, text(strings.Join(env, "\n")))
	case "pid":
		pgid, _ := syscall.Getpgid(pid)
		reply(m.ID, text(fmt.Sprintf("%d %d", pid, pgid)))
	case "crash":
		outMu.Lock()
		out.Flush()
		os.Exit(3)
	default:
		reply(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "unknown tool " + p.Name}}, "isError": true})
	}
}

func reply(id json.RawMessage, result any) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	outMu.Lock()
	defer outMu.Unlock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

func orNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func writeRecord(kind string, v any) {
	if *record == "" {
		return
	}
	b, _ := json.Marshal(v)
	name := filepath.Join(*record, fmt.Sprintf("%s-%d.json", kind, pid))
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, name)
	}
}

func appendRecord(kind string, line []byte) {
	if *record == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(*record, fmt.Sprintf("%s-%d.jsonl", kind, pid)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append([]byte(strings.TrimRight(string(line), "\r\n")), '\n'))
}
