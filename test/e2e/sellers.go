package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// M4 (WP-07): the sellers of the M4 tests (spec/relay.md §16 gate_mcp_*; spec/tunnel.md §16 anon_*). Two are the
// repository's example sellers, built once per test binary from ./examples/sellers/<name> and run unmodified as
// processes on 127.0.0.1 with the run's markers: tck (examples/sellers/tck: a stock a2a-go seller; a hire's message
// id is the gate's, so it answers "Unhandled messageId prefix" completed, a fast seller) and booking
// (examples/sellers/booking: one question, then the booking). The third, Seller, is an a2a-go v2.7.0 agent in the test
// process, keyed on a message's text as a2a-cli's exec mode is on its command's output: it stands in for hello-exec
// (a2a-cli v0.3.1, which neither this host nor CI's e2e job installs) and adds what no exec can do:
//
//   - "hi": completed, "Hello World";
//   - "kinds": the TCK seller's artifact kinds (a text, a data, a raw file and a file URL artifact) and a final message
//     with a text and a data part;
//   - "ask": input-required, "Which date?"; the follow-up completes it, "booked for <text>";
//   - "slow": working until it is cancelled (a sleeping exec);
//   - "fail": failed, "the exec failed: exit status 1" (a failing exec);
//   - "big:<n>[:<KiB>]": n text artifacts of 300 KiB (or KiB) each, then completed (parts past a limit, parts in the
//     bucket);
//   - anything else: completed, echoing the text.
//
// It serves its card at /.well-known/agent-card.json and JSON-RPC at its root, as `kontra connect --serve
// a2a:<name>=<root URL>` hosts an agent, and records the methods, turns and A2A-Extensions headers it receives.

// Seller is the in-test seller.
type Seller struct {
	URL string // its root URL

	mu      sync.Mutex
	methods []string
	turns   []SellerTurn
	exts    []string // each A2A-Extensions header value received
	srv     *http.Server
}

// Activations are the A2A-Extensions header values the seller received.
func (s *Seller) Activations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.exts...)
}

// SellerTurn is one Execute call.
type SellerTurn struct {
	TaskID   string
	Text     string
	FollowUp bool
}

// BigPart is the size of each artifact a "big:<n>" message asks for.
const BigPart = 300 << 10

// StartSeller serves a Seller on 127.0.0.1 until the test ends.
func StartSeller(t testing.TB) *Seller {
	t.Helper()
	return StartSellerCaps(t, `{"streaming":true}`)
}

// StartSellerCaps is StartSeller with caps (a JSON object) as its card's capabilities, e.g. one that requires an
// extension of its own.
func StartSellerCaps(t testing.TB, caps string) *Seller {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Seller{URL: "http://" + l.Addr().String()}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(s, a2asrv.WithTaskStore(taskstore.NewInMemory(nil))))
	card := `{"name":"kinds","description":"answers every kind of part","version":"1.0.0",
"supportedInterfaces":[{"url":"` + s.URL + `/","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":` + caps + `,"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
"skills":[{"id":"kinds","name":"kinds","description":"kinds","tags":["k"]}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, card)
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		for _, m := range []string{"SendStreamingMessage", "SendMessage", "CancelTask", "GetTask", "SubscribeToTask"} {
			if bytes.Contains(body, []byte(`"method":"`+m+`"`)) {
				s.mu.Lock()
				s.methods = append(s.methods, m)
				s.mu.Unlock()
				break
			}
		}
		if v := r.Header.Values("A2A-Extensions"); len(v) > 0 {
			s.mu.Lock()
			s.exts = append(s.exts, v...)
			s.mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rpc.ServeHTTP(w, r)
	})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(l) }()
	t.Cleanup(func() { _ = s.srv.Close() })
	return s
}

// Count is how many requests of method the seller received.
func (s *Seller) Count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.methods {
		if m == method {
			n++
		}
	}
	return n
}

// Messages is how many messages (SendMessage and SendStreamingMessage) the seller received.
func (s *Seller) Messages() int { return s.Count("SendMessage") + s.Count("SendStreamingMessage") }

// Turns are the turns the seller ran, in order.
func (s *Seller) Turns() []SellerTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SellerTurn(nil), s.turns...)
}

// Execute implements a2asrv.AgentExecutor.
func (s *Seller) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := ""
		if ec.Message != nil && len(ec.Message.Parts) > 0 {
			text = ec.Message.Parts[0].Text()
		}
		s.mu.Lock()
		s.turns = append(s.turns, SellerTurn{TaskID: string(ec.TaskID), Text: text, FollowUp: ec.StoredTask != nil})
		s.mu.Unlock()
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		say := func(parts ...*a2a.Part) *a2a.Message {
			return a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)
		}
		status := func(st a2a.TaskState, m *a2a.Message) bool { return yield(a2a.NewStatusUpdateEvent(ec, st, m), nil) }
		switch {
		case ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired:
			status(a2a.TaskStateCompleted, say(a2a.NewTextPart("booked for "+text)))
		case text == "hi":
			status(a2a.TaskStateCompleted, say(a2a.NewTextPart("Hello World")))
		case text == "kinds":
			file := a2a.NewRawPart([]byte("tck"))
			file.MediaType, file.Filename = "text/plain", "output.txt"
			link := a2a.NewFileURLPart("https://example.com/output.txt", "text/plain")
			link.Filename = "output.txt"
			for _, p := range []*a2a.Part{a2a.NewTextPart("Generated text content"), a2a.NewDataPart(map[string]any{"key": "value", "count": 42.0}), file, link} {
				if !yield(a2a.NewArtifactEvent(ec, p), nil) {
					return
				}
			}
			status(a2a.TaskStateCompleted, say(a2a.NewTextPart("done"), a2a.NewDataPart(map[string]any{"total": 4.0})))
		case text == "ask":
			status(a2a.TaskStateInputRequired, say(a2a.NewTextPart("Which date?")))
		case text == "fail":
			status(a2a.TaskStateFailed, say(a2a.NewTextPart("the exec failed: exit status 1")))
		case strings.HasPrefix(text, "big:"):
			spec := strings.Split(strings.TrimPrefix(text, "big:"), ":")
			n, _ := strconv.Atoi(spec[0])
			size := BigPart
			if len(spec) > 1 {
				kib, _ := strconv.Atoi(spec[1])
				size = kib << 10
			}
			for i := range n {
				if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(fmt.Sprintf("%d:", i)+strings.Repeat("b", size))), nil) {
					return
				}
			}
			status(a2a.TaskStateCompleted, say(a2a.NewTextPart("big done")))
		case text == "slow":
			if !status(a2a.TaskStateWorking, nil) {
				return
			}
			<-ctx.Done()
		default:
			status(a2a.TaskStateCompleted, say(a2a.NewTextPart("echo: "+text)))
		}
	}
}

// Cancel implements a2asrv.AgentExecutor.
func (s *Seller) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// --- the repository's example sellers ---

var (
	exampleMu   sync.Mutex
	exampleBins = map[string]string{}
)

// exampleBinary builds ./examples/sellers/<name> once per test binary, under TempRoot.
func exampleBinary(name string) (string, error) {
	exampleMu.Lock()
	defer exampleMu.Unlock()
	if b, ok := exampleBins[name]; ok {
		return b, nil
	}
	root, err := TempRoot()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "bin-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, name)
	if out, err := exec.Command("go", "build", "-o", bin, "kontra.sh/kontra/examples/sellers/"+name).CombinedOutput(); err != nil {
		return "", fmt.Errorf("e2e: build examples/sellers/%s: %v\n%s", name, err, out)
	}
	exampleBins[name] = bin
	return bin, nil
}

// ExampleSeller is one example seller process.
type ExampleSeller struct {
	Name    string
	URL     string // its root URL, as it printed it
	LogPath string
	cmd     *exec.Cmd
}

// StartExampleSeller builds and runs examples/sellers/<name> --port 0 on 127.0.0.1 in a process group of its own,
// with the run's markers, and returns its root URL (the first line it prints); the harness stops it.
func (h *Harness) StartExampleSeller(ctx context.Context, name string) (*ExampleSeller, error) {
	bin, err := exampleBinary(name)
	if err != nil {
		return nil, err
	}
	s := &ExampleSeller{Name: name, LogPath: filepath.Join(h.Dir, "seller-"+name+".log")}
	logf, err := os.Create(s.LogPath)
	if err != nil {
		return nil, err
	}
	s.cmd = exec.Command(bin, "--host", "127.0.0.1", "--port", "0")
	s.cmd.Env = h.env()
	s.cmd.Stderr = logf
	s.cmd.SysProcAttr = procAttr()
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		logf.Close()
		return nil, err
	}
	if err := s.cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	h.mu.Lock()
	h.pgids = append(h.pgids, s.cmd.Process.Pid)
	h.mu.Unlock()
	h.OnClose(func() error {
		err := stopProcess(s.cmd, 5*time.Second)
		logf.Close()
		return err
	})
	first := make(chan string, 1)
	go func() {
		br := bufio.NewReader(out)
		line, _ := br.ReadString('\n')
		first <- strings.TrimSpace(line)
		_, _ = io.Copy(io.Discard, br)
	}()
	select {
	case line := <-first:
		if !strings.HasPrefix(line, "http://127.0.0.1:") {
			return nil, fmt.Errorf("e2e: examples/sellers/%s printed %q, not its root URL:\n%s", name, line, tail(s.LogPath, 20))
		}
		s.URL = strings.TrimSuffix(line, "/")
	case <-time.After(30 * time.Second):
		return nil, errors.New("e2e: examples/sellers/" + name + " printed no URL within 30 s")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s, nil
}
