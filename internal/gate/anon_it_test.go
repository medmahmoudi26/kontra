package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/blob/blobtest"
)

// anonPost posts body to url as an anonymous caller (the services admit anonymous) and returns the status, the
// headers and the body.
func anonPost(t *testing.T, url, body string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func sendBody(method, text string, returnImmediately bool) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": map[string]any{
		"message":       map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}},
		"configuration": map[string]any{"returnImmediately": returnImmediately},
	}})
	return string(b)
}

func restSendBody(text string) string {
	b, _ := json.Marshal(map[string]any{
		"message":       map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}},
		"configuration": map[string]any{"returnImmediately": true},
	})
	return string(b)
}

// seedAnonymousService writes an anonymous tenant and an a2a service of it on the seller (allow: anonymous), as the
// admission of an anonymous hello with --public would, then NOTIFY kontra_services, and waits until the gate serves it.
func (e *gateEnv) seedAnonymousService(base, slug, name string) uuid.UUID {
	e.t.Helper()
	return e.seedAnonymousServiceAt(base, slug, name, e.seller.srv.URL+"/base")
}

// seedAnonymousServiceAt is seedAnonymousService with the service's backend at upstream.
func (e *gateEnv) seedAnonymousServiceAt(base, slug, name, upstream string) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind, expires_at, created_from)
		values ($1, $2, 'anonymous', now() + interval '1 hour', '203.0.113.0/24')`, id, slug); err != nil {
		e.t.Fatal(err)
	}
	backend := `{"a2a": {"url": "` + upstream + `", "allow_private": true}}`
	ann, err := anon.CardAnnotations(time.Now().Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, allow, source, annotations)
		values ($1, $2, 'a2a', $3::jsonb, '{anonymous,org:`+slug+`}', 'db', $4)`, id, name, backend, ann); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `select pg_notify('kontra_services', $1)`, slug+"/"+name); err != nil {
		e.t.Fatal(err)
	}
	eventually(e.t, 20*time.Second, "the anonymous service served", func() bool {
		resp, err := http.Get(base + "/agents/" + slug + "/" + name)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return id
}

// spec/tunnel.md §6.2 (D277, D287) through a multi-tenant gate with tunnel.anonymous: an anonymous tenant's
// concurrent tasks as host are at most tasks; one more SendMessage is HTTP 429 with Retry-After on JSON-RPC (-32600
// "rate limited"), on its streaming method and on HTTP+JSON, and creates no task; once a task ends there is room again.
// A caller's message that would take the tenant's stored bytes past parts_bytes is InvalidParams "anonymous storage
// limit" before any task row; upstream bytes past it fail the task upstream_error. At the tenant's end the sweep
// disables its services: every route answers 404.
func TestAnonymousTenantLimitsThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	port := freePort(t)
	path := e.replicaConfig(port, time.Minute, "tenancy: multi\ntunnel: { anonymous: { tasks: 2, parts_bytes: 16KiB } }\n")
	timing := fastTiming
	timing.AnonExpire = 200 * time.Millisecond
	run := e.start("a", path, timing)
	slugA, slugB := "anon-00000000000000a1", "anon-00000000000000b2"
	tenantA := e.seedAnonymousService(run.url, slugA, "slow")
	tenantB := e.seedAnonymousService(run.url, slugB, "store")
	urlA := run.url + "/agents/" + slugA + "/slow"
	// The served card, signed by the gate, says the service is an anonymous session's (the corrections' WP-03
	// recommendation): capabilities.extensions carries the gate's anonymous extension with the session's end.
	resp, err := http.Get(urlA + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	cardBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var served struct {
		Capabilities struct {
			Extensions []struct {
				URI    string         `json:"uri"`
				Params map[string]any `json:"params"`
			} `json:"extensions"`
		} `json:"capabilities"`
		Signatures []json.RawMessage `json:"signatures"`
	}
	if err := json.Unmarshal(cardBody, &served); err != nil || resp.StatusCode != 200 || len(served.Signatures) == 0 {
		t.Fatalf("card: %d %s", resp.StatusCode, cardBody)
	}
	found := false
	for _, x := range served.Capabilities.Extensions {
		found = found || (x.URI == anon.ExtensionURI && x.Params["expiresAt"] != nil)
	}
	if !found {
		t.Fatalf("the served card lacks the anonymous extension: %s", cardBody)
	}
	tasks := func(tenant uuid.UUID) int64 {
		return scalar[int64](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1`, tenant)
	}

	// Two tasks as host, working until released.
	for i := range 2 {
		st, _, body := anonPost(t, urlA, sendBody("SendMessage", fmt.Sprintf("slow-%d", i), true))
		if st != 200 || !strings.Contains(string(body), `"result"`) {
			t.Fatalf("task %d: %d %s", i, st, body)
		}
	}
	eventually(t, 20*time.Second, "both tasks working", func() bool {
		return scalar[int64](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1 and state = 'working'`, tenantA) == 2
	})
	// The third: 429 with Retry-After, -32600 "rate limited", no task.
	st, hdr, body := anonPost(t, urlA, sendBody("SendMessage", "slow-2", true))
	var rpc struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil || st != http.StatusTooManyRequests || hdr.Get("Retry-After") != "5" ||
		rpc.Error.Code != -32600 || rpc.Error.Message != "rate limited" || rpc.ID != 7 {
		t.Fatalf("SendMessage over the cap: %d %v %s", st, hdr, body)
	}
	st, hdr, body = anonPost(t, urlA, sendBody("SendStreamingMessage", "slow-2", false))
	if st != http.StatusTooManyRequests || hdr.Get("Retry-After") != "5" || !strings.Contains(string(body), `"code":-32600`) ||
		!strings.Contains(string(body), `"rate limited"`) {
		t.Fatalf("SendStreamingMessage over the cap: %d %v %s", st, hdr, body)
	}
	for _, route := range []string{"/message:send", "/message:stream"} {
		st, hdr, body = anonPost(t, urlA+route, restSendBody("slow-2"))
		if st != http.StatusTooManyRequests || hdr.Get("Retry-After") != "5" || !strings.Contains(string(body), "RESOURCE_EXHAUSTED") ||
			!strings.Contains(string(body), "rate limited") {
			t.Fatalf("HTTP+JSON %s over the cap: %d %v %s", route, st, hdr, body)
		}
	}
	if n := tasks(tenantA); n != 2 {
		t.Fatalf("%d tasks after the refusals, want 2", n)
	}
	// A task ends: room for one more.
	e.seller.release("slow-0")
	eventually(t, 20*time.Second, "the first task completed", func() bool {
		return scalar[int64](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1 and state = 'completed'`, tenantA) == 1
	})
	if st, _, body := anonPost(t, urlA, sendBody("SendMessage", "slow-3", true)); st != 200 || !strings.Contains(string(body), `"result"`) {
		t.Fatalf("after a task ended: %d %s", st, body)
	}

	// parts_bytes (16 KiB) on tenant B: a caller's message over it, before any task row.
	urlB := run.url + "/agents/" + slugB + "/store"
	st, _, body = anonPost(t, urlB, sendBody("SendMessage", strings.Repeat("x", 17<<10), true))
	if err := json.Unmarshal(body, &rpc); err != nil || st != 200 || rpc.Error.Code != -32602 || rpc.Error.Message != "anonymous storage limit" {
		t.Fatalf("a message over parts_bytes: %d %s", st, body)
	}
	if n := tasks(tenantB); n != 0 {
		t.Fatalf("%d tasks after the storage refusal", n)
	}
	// Upstream bytes past it: the seller answers with the message's 10 KiB again, which passes the tenant's 16 KiB.
	big := strings.Repeat("y", 10<<10)
	st, _, body = anonPost(t, urlB, sendBody("SendMessage", big, true))
	if st != 200 || !strings.Contains(string(body), `"result"`) {
		t.Fatalf("a message within parts_bytes: %d %s", st, body)
	}
	e.seller.release(big)
	eventually(t, 30*time.Second, "the task failed upstream_error", func() bool {
		return scalar[int64](t, e.pool, `select count(*) from kontra.tasks t join kontra.attempts a on a.task_id = t.id
			where t.tenant_id = $1 and t.state = 'failed' and a.outcome = 'upstream_error'`, tenantB) == 1
	})
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.task_events e join kontra.tasks t on t.id = e.task_id
		where t.tenant_id = $1 and e.kind = 'artifact'`, tenantB); n != 0 {
		t.Fatalf("%d artifacts stored past parts_bytes", n)
	}

	// The end: the sweep disables the services; every route answers 404.
	if _, err := e.pool.Exec(ctx, `update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, tenantA); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "the expired tenant's URLs answer 404", func() bool {
		resp, err := http.Get(urlA)
		if err != nil {
			return false
		}
		resp.Body.Close()
		st, _, _ := anonPost(t, urlA, sendBody("SendMessage", "hi", false))
		return resp.StatusCode == http.StatusNotFound && st == http.StatusNotFound
	})
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, tenantA); n != 0 {
		t.Fatal("the expired tenant's service is enabled")
	}
	e.seller.releaseAll()
}

// bigSeller is an a2a-go v2.7.0 agent that answers every message with one artifact of two parts of size bytes each.
type bigSeller struct{ size int }

func (b bigSeller) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		part := strings.Repeat("z", b.size)
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(part), a2a.NewTextPart(part)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

func (bigSeller) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// newBigSeller serves bigSeller behind /base and returns that URL.
func newBigSeller(t *testing.T, size int) string {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	card := `{"name":"big","description":"answers big parts","version":"1.0.0",
"supportedInterfaces":[{"url":"` + srv.URL + `/base/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
"skills":[{"id":"big","name":"big","description":"big","tags":["b"]}]}`
	mux.HandleFunc("/base/.well-known/agent-card.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, card)
	})
	mux.Handle("/base/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(bigSeller{size: size})))
	return srv.URL + "/base"
}

// spec/tunnel.md §6.1, §6.2, D167 (M4 WP-03 review): an anonymous tenant's upstream part stored in the bucket for an
// event that is then refused (the next part would pass parts_bytes) leaves an object and no blobs row; the task fails
// upstream_error. The purge, purge_after after the tenant's end, deletes that object too, by the task's prefix, before
// the rows: nothing of the tenant is left in the bucket.
func TestAnonymousPurgeDeletesObjectsWithoutRows(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	bt := blobtest.New(t, "anon")
	port := freePort(t)
	path := e.replicaConfig(port, time.Minute, `tenancy: multi
blobs: "`+bt.URL+`"
tunnel: { anonymous: { parts_bytes: 512KiB, purge_after: 1s } }
`)
	timing := fastTiming
	timing.AnonExpire, timing.AnonPurge = 200*time.Millisecond, 200*time.Millisecond
	run := e.start("a", path, timing)
	slug := "anon-00000000000000c3"
	tenantID := e.seedAnonymousServiceAt(run.url, slug, "big", newBigSeller(t, 300<<10))

	st, _, body := anonPost(t, run.url+"/agents/"+slug+"/big", sendBody("SendMessage", "two big parts", true))
	if st != 200 || !strings.Contains(string(body), `"result"`) {
		t.Fatalf("SendMessage: %d %s", st, body)
	}
	eventually(t, 30*time.Second, "the task failed upstream_error", func() bool {
		return scalar[int64](t, e.pool, `select count(*) from kontra.tasks t join kontra.attempts a on a.task_id = t.id
			where t.tenant_id = $1 and t.state = 'failed' and a.outcome = 'upstream_error'`, tenantID) == 1
	})
	prefix := "anon/" + tenantID.String() + "/"
	objectsOfTenant := func() []string {
		var out []string
		for _, k := range bt.Keys(t) {
			if strings.HasPrefix(k, prefix) {
				out = append(out, k)
			}
		}
		return out
	}
	if objs := objectsOfTenant(); len(objs) != 1 {
		t.Fatalf("the bucket holds %v of the tenant, want the first part's object", objs)
	}
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.blobs where tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("%d blobs rows: the refused event's object should have none", n)
	}

	// The tenant ends; purge_after (1 s) later the purge leaves only its tombstone, and nothing in the bucket.
	if _, err := e.pool.Exec(ctx, `update kontra.tenants set expires_at = now() - interval '1 minute' where id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the tenant purged", func() bool {
		return scalar[int64](t, e.pool, `select count(*) from kontra.tenants where id = $1 and deleted_at is not null`, tenantID) == 1
	})
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("%d tasks after the purge", n)
	}
	if objs := objectsOfTenant(); len(objs) != 0 {
		t.Fatalf("the purge left the tenant's objects %v in the bucket", objs)
	}
}
