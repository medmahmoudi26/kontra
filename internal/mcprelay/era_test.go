package mcprelay

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
)

// The era cache (D177): one probe per service at a time, which every waiting request shares; a modern or legacy
// answer is kept RefreshTools; an inconclusive one is not kept, and is not probed again within probeBackoff; a
// client's server/discover result makes the service modern; DropEra forgets it, and a probe that was in flight then
// installs nothing.
func TestEraCache(t *testing.T) {
	c := NewToolsCache()
	now := time.Unix(1_800_000_000, 0)
	var nowMu sync.Mutex
	c.now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	advance := func(d time.Duration) { nowMu.Lock(); now = now.Add(d); nowMu.Unlock() }
	key := ToolsKey{TenantID: uuid.New(), Service: "svc"}

	var probes atomic.Int32
	release := make(chan struct{})
	answer := mcpwire.AnswerLegacy
	probe := func(ctx context.Context) (mcpwire.DiscoverAnswer, json.RawMessage) {
		probes.Add(1)
		<-release
		if answer == mcpwire.AnswerModern {
			return answer, json.RawMessage(`{"supportedVersions":["2026-07-28"],"capabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}`)
		}
		return answer, nil
	}
	// Ten requests find the era unknown at once: one probe, all wait for it.
	var wg sync.WaitGroup
	eras := make([]mcpwire.Era, 10)
	for i := range eras {
		wg.Go(func() {
			era, ok, err := c.Era(context.Background(), key, probe)
			if err != nil || !ok {
				t.Errorf("Era: %v %v", ok, err)
			}
			eras[i] = era
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if probes.Load() != 1 {
		t.Fatalf("%d probes, want 1", probes.Load())
	}
	for _, e := range eras {
		if e != mcpwire.EraLegacy {
			t.Fatalf("eras %v", eras)
		}
	}
	// Kept RefreshTools; then probed again.
	advance(RefreshTools - time.Second)
	if era, ok, _ := c.Era(context.Background(), key, probe); !ok || era != mcpwire.EraLegacy || probes.Load() != 1 {
		t.Fatalf("within RefreshTools: %v %v, %d probes", era, ok, probes.Load())
	}
	advance(2 * time.Second)
	answer = mcpwire.AnswerUnknown
	if _, ok, _ := c.Era(context.Background(), key, probe); ok || probes.Load() != 2 {
		t.Fatalf("an inconclusive probe: ok %v, %d probes", ok, probes.Load())
	}
	// Not probed again within probeBackoff: the request is forwarded with the era unknown.
	if _, ok, _ := c.Era(context.Background(), key, probe); ok || probes.Load() != 2 {
		t.Fatalf("within the backoff: ok %v, %d probes", ok, probes.Load())
	}
	advance(probeBackoff)
	answer = mcpwire.AnswerModern
	if era, ok, _ := c.Era(context.Background(), key, probe); !ok || era != mcpwire.EraModern || probes.Load() != 3 {
		t.Fatalf("after the backoff: %v %v, %d probes", era, ok, probes.Load())
	}
	if caps, ok := c.Capabilities(key); !ok || !declaresTasks(caps) {
		t.Fatalf("capabilities %s %v", caps, ok)
	}
	// Dropped: unknown again (no probe given: none is started).
	c.DropEra(key)
	if _, ok, _ := c.Era(context.Background(), key, nil); ok {
		t.Fatal("known after DropEra")
	}
	if _, ok := c.Capabilities(key); ok {
		t.Fatal("capabilities after DropEra")
	}
	// A client's server/discover result makes it modern without a probe.
	c.LearnModern(key, json.RawMessage(`{"supportedVersions":["2026-07-28"],"capabilities":{}}`))
	if era, ok, _ := c.Era(context.Background(), key, nil); !ok || era != mcpwire.EraModern {
		t.Fatalf("after LearnModern: %v %v", era, ok)
	}
	if caps, ok := c.Capabilities(key); !ok || declaresTasks(caps) {
		t.Fatalf("capabilities after LearnModern %s", caps)
	}
	// A probe in flight when the era is dropped installs nothing.
	c.Forget(key)
	answer = mcpwire.AnswerLegacy
	started, again := make(chan struct{}), make(chan struct{})
	hold, hold2 := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	slow := func(ctx context.Context) (mcpwire.DiscoverAnswer, json.RawMessage) {
		if calls.Add(1) == 1 {
			close(started)
			<-hold
		} else {
			close(again)
			<-hold2
		}
		return mcpwire.AnswerLegacy, nil
	}
	done := make(chan mcpwire.Era, 1)
	go func() {
		era, _, _ := c.Era(context.Background(), key, slow)
		done <- era
	}()
	<-started
	c.DropEra(key)
	close(hold)
	<-again // the waiter found nothing installed, and probes again
	c.mu.Lock()
	_, known := c.eras[key]
	c.mu.Unlock()
	if known {
		t.Fatal("a probe begun before DropEra installed its era")
	}
	close(hold2)
	if era := <-done; era != mcpwire.EraLegacy {
		t.Fatalf("the second probe's era: %v", era)
	}
	// A request whose context ends while it waits returns the context's error; the probe goes on.
	c.DropEra(key)
	block := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, _, err := c.Era(ctx, key, func(context.Context) (mcpwire.DiscoverAnswer, json.RawMessage) {
		<-block
		return mcpwire.AnswerLegacy, nil
	})
	if err == nil {
		t.Fatal("no error when the request left")
	}
	close(block)
	eventually(t, 5*time.Second, "the detached probe installs its answer", func() bool {
		era, ok, _ := c.Era(context.Background(), key, nil)
		return ok && era == mcpwire.EraLegacy
	})
}

// The probe is bounded by probeTimeout on a context of the cache's own.
func TestEraProbeIsBounded(t *testing.T) {
	c := NewToolsCache()
	c.probeTimeout = 100 * time.Millisecond
	key := ToolsKey{TenantID: uuid.New(), Service: "svc"}
	start := time.Now()
	_, ok, err := c.Era(context.Background(), key, func(ctx context.Context) (mcpwire.DiscoverAnswer, json.RawMessage) {
		<-ctx.Done()
		return mcpwire.AnswerUnknown, nil
	})
	if err != nil || ok || time.Since(start) > 5*time.Second {
		t.Fatalf("ok %v err %v after %v", ok, err, time.Since(start))
	}
}

// The modern tools/list's ttlMs is the least of the upstream's pages', at most 600000, and 0 when none had one.
func TestModernListTTL(t *testing.T) {
	key := ToolsKey{TenantID: uuid.New(), Service: "svc"}
	for _, tc := range []struct {
		pages []string
		want  int64
	}{
		{[]string{`{"tools":[{"name":"a"}],"ttlMs":900000}`}, 600000},
		{[]string{`{"tools":[{"name":"a"}],"ttlMs":30000,"nextCursor":"1"}`, `{"tools":[{"name":"b"}],"ttlMs":60000}`}, 30000},
		{[]string{`{"tools":[{"name":"a"}]}`}, 0},
	} {
		c := NewToolsCache()
		i := 0
		list, ttl, err := c.ListModern(context.Background(), key, ToolView{}, func(context.Context, string) (json.RawMessage, error) {
			p := tc.pages[i]
			i++
			return json.RawMessage(p), nil
		})
		if err != nil || ttl != tc.want || len(list) != len(tc.pages) {
			t.Errorf("%v: %d tools, ttl %d, %v; want ttl %d", tc.pages, len(list), ttl, err, tc.want)
		}
	}
}
