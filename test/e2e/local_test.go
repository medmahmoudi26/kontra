package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// startLocal starts one gate in the local development profile (profile: local, spec/relay.md §18) on a fresh
// database and its own Temporal development server: no API key, no enrollment token, no TLS. extra is appended to
// its kontra.yaml. Close (registered on t) stops everything.
func startLocal(t *testing.T, extra string) (*Harness, *Gate) {
	t.Helper()
	ctx := ctxFor(t, 3*time.Minute)
	h := &Harness{run: randomHex(6), Upstreams: map[string]*slowagent.Agent{}}
	t.Cleanup(func() {
		if t.Failed() && h.Gates[0] != nil {
			t.Logf("gate log:\n%s", tail(h.Gates[0].LogPath, 60))
		}
		if err := h.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	root, err := TempRoot()
	if err != nil {
		t.Fatal(err)
	}
	if h.Dir, err = os.MkdirTemp(root, "local-"+h.run+"-"); err != nil {
		t.Fatal(err)
	}
	if h.cli, err = temporalCLI(""); err != nil {
		t.Fatal(err)
	}
	if h.bin, err = gateBinary(); err != nil {
		t.Fatal(err)
	}
	if h.pg, err = startPostgres(ctx, "", h.run); err != nil {
		t.Fatal(err)
	}
	h.PostgresURL = h.pg.url
	if out, err := h.gateCommand(ctx, "migrate", "--postgres", h.PostgresURL, "up"); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if h.Pool, err = pgxpool.New(ctx, h.PostgresURL); err != nil {
		t.Fatal(err)
	}
	if err := h.startTemporal(ctx); err != nil {
		t.Fatal(err)
	}
	ports, err := freePorts(2)
	if err != nil {
		t.Fatal(err)
	}
	h.configPath = filepath.Join(h.Dir, "kontra.yaml")
	cfg := fmt.Sprintf("profile: local\npostgres: %q\ntemporal: { address: %q }\n%s", h.PostgresURL, h.Temporal, extra)
	if err := os.WriteFile(h.configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := h.startGate("local", h.configPath, ports[0], ports[1], 0)
	h.Gates[0] = g
	if err != nil {
		t.Fatal(err)
	}
	if err := h.waitReady(ctx, g, nil); err != nil {
		t.Fatal(err)
	}
	return h, g
}

// The local development profile end to end (tasks.md 2.5): `kontra connect --gate http://127.0.0.1:<port> --serve
// a2a:hello=<agent>` connects with no credential and serves the agent; a stock A2A client (a2a-go's), with no
// credential and then with a bogus one, completes a task on it; the gate logs the profile's warning with the console's
// URL; the console lists the task and its exchange; a request naming another Host is 421.
func TestLocalProfile(t *testing.T) {
	h, g := startLocal(t, "")
	ctx := ctxFor(t, 3*time.Minute)

	agent := slowagent.New(slowagent.Config{Chunks: 1, Interval: 50 * time.Millisecond})
	agentURL, err := agent.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.agents = append(h.agents, agent)
	n, err := h.StartNode(ctx, NodeOptions{Name: "dev", Gate: g.URL, Serves: map[string]string{"hello": agentURL}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.waitReady(ctx, g, []string{"hello"}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"", "kk_bogus_00000000"} {
		c := (&Buyer{Key: key}).Client(t, g, "hello")
		res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi from a stock client"))})
		if err != nil {
			t.Fatalf("key %q: SendMessage: %v", key, err)
		}
		task, ok := res.(*a2a.Task)
		if !ok {
			t.Fatalf("key %q: answer %T", key, res)
		}
		eventually(t, 30*time.Second, "the task completes", func() bool {
			st, err := h.TaskState(ctx, task.ID)
			return err == nil && st == "completed"
		})
	}

	// The start warning names the console's URL.
	b, _ := os.ReadFile(g.LogPath)
	if !strings.Contains(string(b), "local development profile") || !strings.Contains(string(b), `"console":"`+g.URL+`/"`) {
		t.Fatalf("no profile warning with the console URL in the gate log")
	}

	// The console: the tasks, every one as the shared caller, and their exchanges.
	var tasks struct {
		Items []struct {
			Caller, Via, State string
		}
	}
	getJSON(t, g.URL+"/v1/tasks", &tasks)
	if len(tasks.Items) != 2 || tasks.Items[0].Caller != "anonymous" || tasks.Items[1].Caller != "anonymous" {
		t.Fatalf("console tasks %+v", tasks)
	}
	var ex struct {
		Items []struct{ Method, Principal string }
	}
	eventually(t, 10*time.Second, "two SendMessage exchanges", func() bool {
		getJSON(t, g.URL+"/v1/exchanges?method=SendMessage", &ex)
		return len(ex.Items) == 2
	})

	// DNS rebinding: another Host on the same listener is 421 before any route.
	req, _ := http.NewRequest(http.MethodGet, g.URL+"/v1/tasks", nil)
	req.Host = "attacker.example:" + strconv.Itoa(portOf(g.Addr))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("a foreign Host = %d, want 421", resp.StatusCode)
	}

	// The node leaves: its service stays, enabled, and a new task fails no_session, the earlier tasks remain.
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the session is gone", func() bool {
		var k int
		_ = h.Pool.QueryRow(ctx, `select count(*) from kontra.tunnels`).Scan(&k)
		return k == 0
	})
	var disabled bool
	if err := h.Pool.QueryRow(ctx, `select disabled_at is not null from kontra.services where name = 'hello'`).Scan(&disabled); err != nil || disabled {
		t.Fatalf("hello after the node left: disabled %v (%v)", disabled, err)
	}
	c := (&Buyer{}).Client(t, g, "hello")
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("anyone there?"))})
	if err != nil {
		t.Fatalf("SendMessage with no node: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage with no node answered %T", res)
	}
	var state, snap string
	eventually(t, 60*time.Second, "the task fails no_session", func() bool {
		_ = h.Pool.QueryRow(ctx, `select state, snapshot::text from kontra.tasks where id = $1`, string(task.ID)).Scan(&state, &snap)
		return state == "failed" && strings.Contains(snap, "no_session")
	})
	getJSON(t, g.URL+"/v1/tasks", &tasks)
	if len(tasks.Items) < 2 {
		t.Fatalf("the earlier tasks are gone: %+v", tasks)
	}
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", u, resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
}

func portOf(addr string) int {
	i := strings.LastIndex(addr, ":")
	p, _ := strconv.Atoi(addr[i+1:])
	return p
}
