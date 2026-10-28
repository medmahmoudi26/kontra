package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/upstream"
)

func TestPolicyAllows(t *testing.T) {
	for _, tc := range []struct {
		p    Policy
		tool string
		want bool
	}{
		{Policy{}, "anything", true},
		{Policy{Deny: []string{"ec*"}}, "echo", false},
		{Policy{Deny: []string{"ec*"}}, "ec", false},
		{Policy{Deny: []string{"ec*"}}, "Echo", true}, // case-sensitive, as tool names are
		{Policy{Deny: []string{"*-file"}}, "write-file", false},
		{Policy{Deny: []string{"*-file"}}, "write-files", true},
		{Policy{Allow: []string{"read_*"}}, "read_file", true},
		{Policy{Allow: []string{"read_*"}}, "write_file", false},
		{Policy{Allow: []string{"*"}, Deny: []string{"get-env"}}, "get-env", false}, // deny wins
		{Policy{Allow: []string{"a*b*c"}}, "abc", true},
		{Policy{Allow: []string{"a*b*c"}}, "aXbYc", true},
		{Policy{Allow: []string{"a*b*c"}}, "acb", false},
		{Policy{Allow: []string{"a*a"}}, "a", false},
		{Policy{Allow: []string{"x?y"}}, "xay", false}, // ? is not a wildcard
		{Policy{Allow: []string{"x?y"}}, "x?y", true},
		{Policy{Allow: []string{}}, "anything", false}, // an empty allow allows none; an absent one all
	} {
		got, err := tc.p.Allows(tc.tool)
		if err != nil || got != tc.want {
			t.Errorf("%+v.Allows(%q) = %v, %v", tc.p, tc.tool, got, err)
		}
	}
	if _, err := (Policy{Deny: []string{""}}).Allows("x"); !errors.Is(err, ErrBadGlob) {
		t.Errorf("empty glob: %v", err)
	}
}

// pages returns a Pager over n pages of size tools each, counting its calls.
func pages(n, size int, calls *atomic.Int32) Pager {
	return func(_ context.Context, cursor string) (json.RawMessage, error) {
		calls.Add(1)
		p := 0
		if cursor != "" {
			fmt.Sscanf(cursor, "p%d", &p)
		}
		var tools []string
		for i := range size {
			tools = append(tools, fmt.Sprintf(`{"name":"t%d-%d","_meta":{"sh.kontra/price":"seller","io.x/k":1},"execution":{"taskSupport":"optional"}}`, p, i))
		}
		res := `{"tools":[` + strings.Join(tools, ",") + `]`
		if p+1 < n {
			res += fmt.Sprintf(`,"nextCursor":"p%d"`, p+1)
		}
		return json.RawMessage(res + "}"), nil
	}
}

func TestToolsCache(t *testing.T) {
	c := NewToolsCache()
	key := ToolsKey{Service: "fs"}
	var calls atomic.Int32
	v := ToolView{
		Policy: Policy{Deny: []string{"t1-*"}},
		Meta: map[string]map[string]json.RawMessage{
			"*":    {"sh.kontra/price": json.RawMessage(`"1 USD"`)},
			"t2-0": {"sh.kontra/price": json.RawMessage(`"2 USD"`), "sh.kontra/x": json.RawMessage(`true`)},
		},
		DropExecution: true,
	}
	list, err := c.List(context.Background(), key, mcpwire.EraLegacy, v, pages(3, 2, &calls))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("drained %d pages, want 3", calls.Load())
	}
	var names []string
	for _, raw := range list {
		var tool struct {
			Name      string         `json:"name"`
			Meta      map[string]any `json:"_meta"`
			Execution any            `json:"execution"`
		}
		_ = json.Unmarshal(raw, &tool)
		names = append(names, tool.Name)
		want := "1 USD"
		if tool.Name == "t2-0" {
			want = "2 USD"
		}
		if tool.Meta["sh.kontra/price"] != want || tool.Meta["io.x/k"] == nil || tool.Execution != nil {
			t.Errorf("%s: %s", tool.Name, raw)
		}
	}
	if strings.Join(names, ",") != "t0-0,t0-1,t2-0,t2-1" {
		t.Errorf("names %v", names)
	}
	// Cached: no page fetched again; a denied tool is unknown, a listed one known.
	if _, ok, _ := c.Tool(context.Background(), key, mcpwire.EraLegacy, v.Policy, "t1-0", pages(3, 2, &calls)); ok {
		t.Error("a denied tool is known")
	}
	if _, ok, _ := c.Tool(context.Background(), key, mcpwire.EraLegacy, v.Policy, "nope", pages(3, 2, &calls)); ok {
		t.Error("an unknown tool is known")
	}
	if raw, ok, _ := c.Tool(context.Background(), key, mcpwire.EraLegacy, v.Policy, "t0-1", pages(3, 2, &calls)); !ok ||
		bytes.Contains(raw, []byte("sh.kontra")) {
		t.Errorf("t0-1: %s %v", raw, ok)
	}
	if calls.Load() != 3 {
		t.Errorf("the cache fetched again: %d", calls.Load())
	}
	// Invalidate drains again; so does age.
	c.Invalidate(key)
	if _, err := c.List(context.Background(), key, mcpwire.EraLegacy, v, pages(3, 2, &calls)); err != nil || calls.Load() != 6 {
		t.Errorf("after Invalidate: %v %d", err, calls.Load())
	}
	c.now = func() time.Time { return time.Now().Add(RefreshTools + time.Second) }
	if _, err := c.List(context.Background(), key, mcpwire.EraLegacy, v, pages(3, 2, &calls)); err != nil || calls.Load() != 9 {
		t.Errorf("after RefreshTools: %v %d", err, calls.Load())
	}
}

// §6.6: 100 pages or 4 MiB in all.
func TestToolsCacheLimits(t *testing.T) {
	var calls atomic.Int32
	c := NewToolsCache()
	if _, err := c.List(context.Background(), ToolsKey{Service: "a"}, mcpwire.EraLegacy, ToolView{}, pages(101, 1, &calls)); !errors.Is(err, ErrToolsLimit) {
		t.Errorf("101 pages: %v", err)
	}
	if calls.Load() != MaxToolPages {
		t.Errorf("fetched %d pages", calls.Load())
	}
	big := func(_ context.Context, cursor string) (json.RawMessage, error) {
		return json.RawMessage(`{"tools":[{"name":"x","description":"` + strings.Repeat("a", 3<<20) + `"}],"nextCursor":"n` + cursor + `"}`), nil
	}
	if _, err := c.List(context.Background(), ToolsKey{Service: "b"}, mcpwire.EraLegacy, ToolView{}, big); !errors.Is(err, ErrToolsLimit) {
		t.Errorf("over 4 MiB: %v", err)
	}
	if _, err := c.List(context.Background(), ToolsKey{Service: "c"}, mcpwire.EraLegacy, ToolView{}, func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"tools":{"not":"an array"}}`), nil
	}); !errors.Is(err, ErrToolsLimit) {
		t.Errorf("tools not an array: %v", err)
	}
}

// One drain per service at a time: concurrent requests wait for it.
func TestToolsCacheOneDrain(t *testing.T) {
	c := NewToolsCache()
	var calls atomic.Int32
	release := make(chan struct{})
	slow := func(ctx context.Context, cursor string) (json.RawMessage, error) {
		calls.Add(1)
		<-release
		return json.RawMessage(`{"tools":[{"name":"a"}]}`), nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if l, err := c.List(context.Background(), ToolsKey{Service: "s"}, mcpwire.EraLegacy, ToolView{}, slow); err != nil || len(l) != 1 {
				t.Errorf("%v %d", err, len(l))
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d drains", calls.Load())
	}
}

// §6.6: a drain runs on the gate's context within drainTimeout, not on the request that started it. A hung upstream
// fails it with ErrToolsLimit at the bound although its starter waits on, and another request waits no longer than
// that before it drains through its own pager; a starter that leaves does not end the drain, whose list is cached.
func TestToolsCacheDrainIsBounded(t *testing.T) {
	c := NewToolsCache()
	c.drainTimeout = 200 * time.Millisecond
	key := ToolsKey{Service: "s"}
	var hungCalls atomic.Int32
	hung := func(ctx context.Context, _ string) (json.RawMessage, error) {
		hungCalls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	answer := func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"tools":[{"name":"a"}]}`), nil
	}
	first := make(chan error, 1)
	go func() {
		_, err := c.List(context.Background(), key, mcpwire.EraLegacy, ToolView{}, hung)
		first <- err
	}()
	eventually(t, 2*time.Second, "the hung drain is in flight", func() bool { return hungCalls.Load() == 1 })
	second := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l, err := c.List(ctx, key, mcpwire.EraLegacy, ToolView{}, answer)
		if err == nil && len(l) != 1 {
			err = fmt.Errorf("%d tools", len(l))
		}
		second <- err
	}()
	select {
	case err := <-first:
		if !errors.Is(err, ErrToolsLimit) {
			t.Errorf("the hung drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hung drain was not bounded")
	}
	select {
	case err := <-second:
		if err != nil {
			t.Errorf("the request waiting on the hung drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request waiting on the hung drain is still waiting")
	}

	// The starter leaves: it returns at its own context, and the drain goes on and is cached.
	c = NewToolsCache()
	release := make(chan struct{})
	var calls atomic.Int32
	slow := func(ctx context.Context, cursor string) (json.RawMessage, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return answer(ctx, cursor)
	}
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() {
		_, err := c.List(ctx, key, mcpwire.EraLegacy, ToolView{}, slow)
		gone <- err
	}()
	eventually(t, 2*time.Second, "the drain is in flight", func() bool { return calls.Load() == 1 })
	cancel()
	select {
	case err := <-gone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the starter that left: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the starter that left did not return at its context")
	}
	close(release)
	if l, err := c.List(context.Background(), key, mcpwire.EraLegacy, ToolView{}, slow); err != nil || len(l) != 1 || calls.Load() != 1 {
		t.Errorf("after the starter left: %d tools, %v, %d drains (want the detached drain's list, 1 drain)", len(l), err, calls.Load())
	}
}

func TestSSEReader(t *testing.T) {
	in := "id: 1\nevent: message\ndata: {\"a\":\ndata: 1}\n\n: comment\r\n\r\nid: 2\r\ndata: x\r\n\r\ndata: unterminated"
	r := newSSEReader(strings.NewReader(in))
	ev, err := r.next()
	if err != nil || ev.data != "{\"a\":\n1}" || len(ev.lines) != 4 {
		t.Fatalf("%+v %v", ev, err)
	}
	if got := string(ev.encode(true, `{"a":2}`)); got != "id: 1\nevent: message\ndata: {\"a\":2}\n\n" {
		t.Errorf("encode: %q", got)
	}
	if ev, err = r.next(); err != nil || ev.hasData || ev.lines[0] != ": comment" {
		t.Fatalf("comment: %+v %v", ev, err)
	}
	if ev, err = r.next(); err != nil || ev.data != "x" {
		t.Fatalf("CRLF: %+v %v", ev, err)
	}
	if _, err = r.next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unterminated: %v", err)
	}
	big := newSSEReader(strings.NewReader("data: " + strings.Repeat("a", upstream.MaxSSEEvent) + "\n\n"))
	if _, err := big.next(); !errors.Is(err, upstream.ErrLimit) {
		t.Errorf("over the limit: %v", err)
	}
}

func TestIDs(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{{`1`, `1`, true}, {`"1"`, `1`, true}, {`1`, `"1"`, false}, {`"a"`, `"a"`, true}, {`1`, `2`, false}, {``, ``, false}} {
		// "1" answers request 1 as the TypeScript SDK reads it (Number("1")); a string request id only its string (M3
		// review mcp-spec-2, TestReviewSameIDAsClients).
		if got := sameID(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.same {
			t.Errorf("sameID(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
	if idString(json.RawMessage(`"abc"`)) != "abc" || idString(json.RawMessage(`42`)) != "42" {
		t.Error("idString")
	}
	// §11.7: the gate's request ids are exactly k.<taskId>.<seq>; an upstream's own id that merely starts with "k."
	// is not one (§11.2: upstream ids pass unchanged).
	const task = "0192f0c4-8a5e-7c3b-9f1d-2b3c4d5e6f70"
	for id, want := range map[string]bool{
		`"k.` + task + `.4"`:                  true,
		`"k.` + task + `.0"`:                  true,
		`"k.` + task + `.1234"`:               true,
		`"k.1"`:                               false,
		`"k.0193.4"`:                          false,
		`"k.elicit-7"`:                        false,
		`"k.` + task + `"`:                    false,
		`"k.` + task + `."`:                   false,
		`"k.` + task + `.04"`:                 false,
		`"k.` + task + `.-1"`:                 false,
		`"k.` + task + `.+1"`:                 false,
		`"k.` + task + `.4.5"`:                false,
		`"k.` + task + `.4 "`:                 false,
		`"k.` + strings.ToUpper(task) + `.4"`: false,
		`"k.{` + task + `}.4"`:                false,
		`"k.urn:uuid:` + task + `.4"`:         false,
		`"k.` + strings.ReplaceAll(task, "-", "") + `.4"`: false,
		`"K.` + task + `.4"`:                              false,
		`"x.` + task + `.4"`:                              false,
		`7`:                                               false,
	} {
		if got := isGateRequestID(json.RawMessage(id)); got != want {
			t.Errorf("isGateRequestID(%s) = %v", id, got)
		}
	}
	id, h, err := MintID()
	if err != nil || len(id) != 22 || h != HashID(id) {
		t.Errorf("MintID: %q %v", id, err)
	}
}
