package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// The helloworld seller (examples/sellers/helloworld, WP-07): a2a-samples' samples/python/agents/helloworld at
// this commit, run unmodified by run.sh with its pinned Python toolchain.
const (
	samplesPin    = "6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3"
	samplesRepo   = "https://github.com/a2aproject/a2a-samples"
	samplesSample = "samples/python/agents/helloworld"
	// SamplesEnv names an a2a-samples checkout at samplesPin; unset, the test clones that commit under
	// TempRoot.
	SamplesEnv = "KONTRA_A2A_SAMPLES"
	// helloAddr is where the sample listens: its __main__.py fixes it.
	helloAddr = "127.0.0.1:9999"
	// helloAnswer is what the sample answers "hi" with (its artifact).
	helloAnswer = "Hello, World! I have received your request (hi)"
)

var (
	samplesOnce sync.Once
	samplesDir  string
	samplesErr  error
)

// samplesCheckout returns an a2a-samples checkout at samplesPin: $KONTRA_A2A_SAMPLES (which must be at it),
// or a clone of that one commit made once under TempRoot.
func samplesCheckout() (string, error) {
	samplesOnce.Do(func() {
		if dir := os.Getenv(SamplesEnv); dir != "" {
			out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
			switch {
			case err != nil:
				samplesErr = err
			case strings.TrimSpace(string(out)) != samplesPin:
				samplesErr = &os.PathError{Op: "checkout", Path: dir, Err: os.ErrInvalid}
			default:
				samplesDir = dir
			}
			return
		}
		root, err := TempRoot()
		if err != nil {
			samplesErr = err
			return
		}
		dir := filepath.Join(root, "a2a-samples")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, args := range [][]string{
			{"init", "-q", dir},
			{"-C", dir, "fetch", "-q", "--depth", "1", samplesRepo, samplesPin},
			{"-C", dir, "-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
		} {
			if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
				samplesErr = &os.PathError{Op: "git " + args[len(args)-1], Path: dir, Err: errorString(strings.TrimSpace(string(out)))}
				return
			}
		}
		samplesDir = dir
	})
	return samplesDir, samplesErr
}

type errorString string

func (e errorString) Error() string { return string(e) }

// sampleChanges is git status of the sample's directory, ignored and untracked files included.
func sampleChanges(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--ignored", "--untracked-files=all", "--", samplesSample).Output()
	return strings.TrimSpace(string(out)), err
}

// startSeller runs examples/sellers/helloworld/run.sh in its own process group, carrying the harness's
// environment (so scripts/test_e2e.sh sees a leftover), and stops it with the harness.
func startSeller(t *testing.T, h *Harness, uv, checkout string) {
	t.Helper()
	logPath := filepath.Join(h.Dir, "seller.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "sellers", "helloworld", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = append(h.env(), "A2A_SAMPLES="+checkout, "UV="+uv)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = procAttr()
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); f.Close(); close(done) }()
	h.OnClose(func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
			return nil
		case <-time.After(10 * time.Second):
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return nil
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seller log:\n%s", tail(logPath, 30))
		}
	})
	deadline := time.Now().Add(3 * time.Minute) // uv may resolve and download the pinned requirements first
	for status(context.Background(), http.MethodGet, "http://"+helloAddr+"/.well-known/agent-card.json", "") != http.StatusOK {
		select {
		case <-done:
			t.Fatalf("the helloworld seller exited:\n%s", tail(logPath, 30))
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helloworld seller does not answer on %s within 3 minutes:\n%s", helloAddr, tail(logPath, 30))
		}
	}
}

// TestHelloworldTunnel is the helloworld example through the tunnel (PRD §14's M2 deliverable; WP-07's
// examples/sellers/helloworld): the a2a-samples helloworld seller at 6603ba3, unmodified, run by run.sh
// with its pinned toolchain on 127.0.0.1:9999, hosted as tunnel service `hello` by `kontra connect --serve
// a2a:hello=http://127.0.0.1:9999 --enroll <kontra-gate enroll hello>`.
//
// The node starts before its seller, as a compose or systemd sidecar does: its first card fetch fails
// (Kontra-Node: target_unreachable), the session is a candidate anyway, and the card arrives on the keeper's
// retry once the seller is up (M2 plan audit, gap 3; the test waits on services.card before the card GETs
// and the first task). Both replicas serve the seller's card (name "Hello World Agent") with only the
// gate's interfaces at public_url, and services.upstream_card holds the seller's own bytes. a2a-go's
// SendMessage returns a completed task whose artifact is the seller's answer, "Hello, World! I have
// received your request (hi)", and a stream shows working, then the artifact, then completed. The checkout
// is still unmodified, and the node refused no preamble.
//
// It needs uv, the checkout ($KONTRA_A2A_SAMPLES, or a clone of the pinned commit) and a free
// 127.0.0.1:9999; without one of them it is skipped, saying which.
func TestHelloworldTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: needs Postgres, the temporal CLI, two gate replicas and the Python seller; skipped under -short")
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("the helloworld seller needs uv (https://docs.astral.sh/uv/), which is not on PATH")
	}
	if l, err := net.Listen("tcp", helloAddr); err != nil {
		t.Skipf("%s is taken (the sample listens there and only there): %v", helloAddr, err)
	} else {
		l.Close()
	}
	checkout, err := samplesCheckout()
	if err != nil {
		t.Skipf("no a2a-samples checkout at %s (set %s, or let the test clone %s): %v", samplesPin[:7], SamplesEnv, samplesRepo, err)
	}
	if changes, err := sampleChanges(checkout); err != nil || changes != "" {
		t.Fatalf("the checkout %s is not the unmodified sample: %v\n%s", checkout, err, changes)
	}

	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 1, Interval: 10 * time.Millisecond, Streaming: true},
		Tunnel:   map[string]TunnelService{"hello": {}},
	})
	ctx := ctxFor(t, 8*time.Minute)

	// The node first, then its seller.
	n, _ := startNode(t, ctx, h, "hello", map[string]string{"hello": "http://" + helloAddr})
	startSeller(t, h, uv, checkout)
	if err := h.WaitCard(ctx, "hello", 2*time.Minute); err != nil {
		t.Fatal(err)
	}

	// The cards: the seller's, served with the gate's interfaces only, on both replicas.
	resp, err := http.Get("http://" + helloAddr + "/.well-known/agent-card.json") // the test, not the gate, reads the seller
	if err != nil {
		t.Fatal(err)
	}
	direct, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := h.Pool.QueryRow(ctx, `select upstream_card from kontra.services where name = 'hello'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, direct) {
		t.Fatalf("services.upstream_card is not the seller's card byte for byte:\n got %s\nwant %s", stored, direct)
	}
	svc := h.Front.URL + "/agents/hello"
	for _, g := range h.Gates {
		served := getCard(t, ctx, g.Svc("hello")+"/.well-known/agent-card.json", h.APIKey)
		var c struct {
			Name                string               `json:"name"`
			SupportedInterfaces []a2a.AgentInterface `json:"supportedInterfaces"`
		}
		if err := json.Unmarshal(served, &c); err != nil {
			t.Fatal(err)
		}
		if c.Name != "Hello World Agent" || len(c.SupportedInterfaces) != 2 {
			t.Fatalf("%s serves %s, want the seller's card with the gate's two interfaces", g.Name, served)
		}
		for _, i := range c.SupportedInterfaces {
			if i.URL != svc || i.ProtocolVersion != a2a.Version {
				t.Fatalf("%s: a served interface %+v, want the gate's %s at %s", g.Name, i, svc, a2a.Version)
			}
		}
		if namesSeller(served) {
			t.Fatalf("%s's served card names the seller's address: %s", g.Name, served)
		}
	}

	// SendMessage (blocking), with a2a-go's client.
	res, err := h.BuyerA().Client(t, h.Gates[1], "hello").SendMessage(ctx, newMessage("hi"))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage through the tunnel: %#v, want a completed task", res)
	}
	if parts := artifactTexts(task); len(parts) != 1 || parts[0] != helloAnswer {
		t.Fatalf("the completed task's artifacts %q, want [%q]", parts, helloAnswer)
	}
	if as, err := h.Attempts(ctx, task.ID); err != nil || len(as) != 1 || as[0].Backend != "tunnel" || as[0].TunnelSession != n.Session() {
		t.Fatalf("its attempts %+v %v: want one, on the node's session", as, err)
	}
	checkHistory(t, h, task.ID, "e2e_helloworld_tunnel", h.APIKey, h.APIKeyB, helloAnswer)

	// SendStreamingMessage: working, then the artifact, then completed.
	s, err := h.BuyerA().SendStreaming(ctx, h.Gates[0], newMessage("hi"), Call{Service: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	working, artifact, completed := -1, -1, -1
	for i, ev := range evs {
		switch {
		case ev.Kind == "status" && ev.State == a2a.TaskStateWorking && working < 0:
			working = i
		case ev.Kind == "artifact" && artifact < 0:
			artifact = i
		case ev.State == a2a.TaskStateCompleted:
			completed = i
		}
	}
	if working < 0 || artifact < working || completed < artifact || completed != len(evs)-1 {
		t.Fatalf("the stream's events %v: want working, then the artifact, then completed last", kinds(evs))
	}
	final, err := fold(evs)
	if err != nil {
		t.Fatal(err)
	}
	if parts := artifactTexts(final); len(parts) != 1 || parts[0] != helloAnswer {
		t.Fatalf("the streamed task's artifacts %q, want [%q]", parts, helloAnswer)
	}
	checkCarried(t, h, s.TaskID(), ids(evs))

	if changes, err := sampleChanges(checkout); err != nil || changes != "" {
		t.Fatalf("the sample was modified by the run: %v\n%s", err, changes)
	}
	// The node's refusals (internal/node proxy.go); its target_unreachable answers before the seller was up
	// are not refusals of a preamble.
	for _, line := range append(n.Log("tunnel exchange refused"), n.Log("tunnel stream refused")...) {
		t.Fatalf("the node refused a preamble: %s", line)
	}
}

// sellerAddrs are the forms the seller's own address takes in a card: the sample's card names
// http://127.0.0.1:9999 (its __main__.py), and localhost or [::1] would be the same listener.
var sellerAddrs = []string{helloAddr, "localhost:9999", "[::1]:9999"}

// namesSeller reports whether a card the gate serves names the seller's own address. It looks for the
// address, never the bare port: every served interface names public_url, the Front, whose ephemeral port
// may hold 9999 (39999, 49999, 59999), and the interface loop already checks each URL is the Front's (M2
// review of WP-08).
func namesSeller(card []byte) bool {
	for _, a := range sellerAddrs {
		if bytes.Contains(card, []byte(a)) {
			return true
		}
	}
	return false
}

// artifactTexts are the texts of a task's artifact parts.
func artifactTexts(task *a2a.Task) []string {
	var out []string
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			out = append(out, p.Text())
		}
	}
	return out
}

// kinds describes events for a message.
func kinds(evs []Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
		if ev.State != "" {
			out[i] += ":" + string(ev.State)
		}
	}
	return out
}
