// nuclei — a Go kontra actor that runs ProjectDiscovery's nuclei as a WARM, thread-safe engine
// over a fleet of targets, using the SAME author surface as the Python actors:
//
//	Load        -> open ONE nuclei engine for the whole session (stream mode: a global result
//	               callback fires per finding as they stream in).
//	scan        -> the Method: it takes the WHOLE Batch and runs `concurrency` targets at once
//	               over the one engine (NewThreadSafeNucleiEngine is safe for concurrent scans).
//	               The concurrency is the AUTHOR's, written here as a semaphore, because the
//	               framework stopped owning the window (ADR 0023 §18).
//	unit.State() -> RESUMABLE PER UNIT: a severity-phase cursor; a death mid-scan resumes at the
//	               NEXT severity (safe because findings STREAM to durable blobs — see below).
//	session_state -> the last target this (reload-surviving) session scanned.
//	global_state  -> add_to_set maintains a fleet-wide DISTINCT-findings index (idempotent) — the
//	                 flagship dedupe-set use.
//	close       -> shut the engine down.
//
// WHAT TAKING THE BATCH COSTS. Ranging over a Batch commits each Unit as the loop moves past it;
// taking every Unit at once (b.Units()) gives that up — with no position there is nothing to
// commit by, so the Batch commits when the Method returns. That is the honest price of owning the
// concurrency and the only thing it costs: Emit still attributes exactly, because it names its
// Unit, and each Unit's phase cursor is its own, so a death still resumes per target rather than
// re-scanning from zero.
//
// STREAMING. Each finding is emitted on its Unit as its OWN durable blob the instant it's found
// (units/{run}/{node}/u{i}/{sha}.json), so a downstream streaming cursor consumes findings before
// the target's scan finishes, AND a mid-scan death loses nothing: the Unit's phase cursor resumes
// at the next severity, and already-scanned phases' findings are already durable. The Method
// returns nothing — the findings ARE the stream. It needs KONTRA_S3_* configured for the blob
// plane (else the host collects emitted records inline). A Go actor is run by Go:
//
//	./nuclei      # a Temporal activity worker; set KONTRA_ADDRESS + KONTRA_REDIS_HOST
package main

import (
	"errors"
	"fmt"
	"sync"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor
	// host behind a.Serve(). The SDK declares the seam and never imports across it —
	// runtime/ -> sdk/ is one-way — so this line is what puts a host in the binary.
	"github.com/medmahmoudi26/kontra-actors/go/nuclei/finding"
	_ "github.com/medmahmoudi26/kontra/runtime/go"
	nuclei "github.com/projectdiscovery/nuclei/v3/lib"
	"github.com/projectdiscovery/nuclei/v3/pkg/output"
)

// Target is one input unit: a URL to scan.
type Target struct {
	URL string `json:"url"`
}

// Params are the run-wide dials the dispatcher/UI sees.
type Params struct {
	// Concurrency is how many targets this Method scans at once. It is the AUTHOR's dial now,
	// not a framework knob: the loop below is what honours it.
	Concurrency int `json:"concurrency"`
}

// defaultConcurrency: targets scanned at once over the one warm engine.
const defaultConcurrency = 4

func main() {
	a := kontra.New()
	a.Load(loadEngine)
	a.Method("scan", scan, kontra.Takes(Target{}), kontra.Emits(finding.Finding{}))
	a.Healthcheck(engineAlive)
	a.Close(closeEngine)
	a.Params(Params{})
	a.Serve()
}

// sink collects nuclei's ONE global result callback, keyed by target host, so each concurrently
// scanned Unit can drain the findings for ITS target after that target's scan returns.
type sink struct {
	mu sync.Mutex
	by map[string][]*output.ResultEvent
}

func (s *sink) push(ev *output.ResultEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := finding.HostKey(ev.Host)
	s.by[k] = append(s.by[k], ev)
}

func (s *sink) drain(target string) []*output.ResultEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := finding.HostKey(target)
	out := s.by[k]
	delete(s.by, k)
	return out
}

func loadEngine(s *kontra.Session) error {
	ne, err := nuclei.NewThreadSafeNucleiEngine()
	if err != nil {
		return err
	}
	sk := &sink{by: map[string][]*output.ResultEvent{}}
	ne.GlobalResultCallback(sk.push)
	s.Set("engine", ne)
	s.Set("sink", sk)
	return nil
}

// engineAlive is THE liveness authority for the session's nuclei engine: the host probes it on
// every Method failure (and on a beat) to decide reload (engine dead) vs isolate-the-Unit (engine
// alive → a bad target). It checks the ENGINE directly — its presence in the session — never by
// string-matching a scan error. nuclei's engine is IN-PROCESS (no subprocess), so its presence is
// the honest liveness signal; a subprocess-backed resource (a browser, a spawned tool) would
// probe the process / PID here instead.
func engineAlive(s *kontra.Session) (any, error) {
	if ne, ok := getEngine(s); !ok || ne == nil {
		return nil, fmt.Errorf("nuclei engine not loaded")
	}
	return "alive", nil
}

func closeEngine(s *kontra.Session) error {
	if v, ok := s.Get("engine"); ok {
		if ne, ok := v.(*nuclei.ThreadSafeNucleiEngine); ok {
			ne.Close()
		}
	}
	return nil
}

// scan is the Method. It takes the whole Batch and scans `concurrency` targets at once over the
// one warm engine — the author's loop, the author's window.
func scan(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	units := b.Units() // taking them all: see WHAT TAKING THE BATCH COSTS above
	sem := make(chan struct{}, concurrency(s))
	var wg sync.WaitGroup
	errs := make([]error, len(units))
	for i, unit := range units {
		wg.Add(1)
		go func(i int, unit *kontra.Unit) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = scanOne(s, unit, ds)
		}(i, unit)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// concurrency is how many targets to scan at once (the author's dial, not the framework's).
func concurrency(s *kontra.Session) int {
	if s != nil && s.Params != nil {
		if n, ok := s.Params["concurrency"].(float64); ok && n >= 1 {
			return int(n)
		}
	}
	return defaultConcurrency
}

// scanOne scans ONE target and pushes each finding to the output Dataset.
func scanOne(s *kontra.Session, unit *kontra.Unit, ds *kontra.Dataset) error {
	target := unit.Str("url")
	if target == "" {
		return kontra.NonRetryable("target unit has no url") // bad input -> terminal, no retry
	}
	ne, okE := getEngine(s)
	sk, okS := getSink(s)
	if !okE || !okS || ne == nil {
		// The warm resource is missing. Return a plain error and let @actor.healthcheck rule —
		// the host probes it and reloads (re-runs Load) because the engine is gone. The Method
		// never decides "this is a reload" itself.
		return errors.New("nuclei engine not available")
	}

	// RESUMABLE PER UNIT — but the phase cursor is only safe when emits are DURABLE (an S3 blob
	// plane is configured). Skipping already-scanned severities on a resume is correct precisely
	// because each finding was emitted as its own durable blob the instant it was found, so a
	// skipped phase's findings are already durable. Without a store (inline dev/test mode) emits
	// aren't durable until the Unit commits — a durable cursor that skipped them would silently
	// lose them — so we DISABLE the cursor and re-scan atomically (re-emitting everything on a
	// retry). s.EmitDurable() is the gate. Downstream reads the Unit's blob prefix via the
	// streaming cursor; nothing is returned because the findings ARE the stream.
	resumable := s.EmitDurable()
	var cur struct {
		Phase int `json:"phase"`
	}
	if resumable {
		_, _ = unit.State().Get("phase", &cur)
	}

	for i := cur.Phase; i < len(finding.Phases); i++ {
		sev := finding.Phases[i]
		if err := ne.ExecuteNucleiWithOpts([]string{target},
			nuclei.WithTemplateFilters(nuclei.TemplateFilters{Severity: sev})); err != nil {
			// Surface the error; @actor.healthcheck (the liveness authority) decides reload (engine
			// dead) vs isolate (engine alive → bad target). The Method never guesses from the string.
			return err
		}
		for _, ev := range sk.drain(target) {
			f := toFinding(ev, target)
			// global_state.AddToSet maintains a fleet-wide DISTINCT-findings index — idempotent, so
			// a re-scan re-adds harmlessly (it never gates the emit below).
			_, _ = s.GlobalState().AddToSet("findings", f.ID())
			// STREAM the finding — durable at push time when a store is configured. It names no
			// Unit (its provenance is in the record); under Units() there is no current Unit, so it
			// rides the Batch tail under an explicit key (ADR 0028). f.ID() is the finding's own
			// stable identity — target|template|matched — so a re-scan re-pushes onto the same slot.
			ds.Push(f.Unit(), kontra.Key(f.ID()))
		}
		if resumable {
			cur.Phase = i + 1
			_ = unit.State().Set("phase", cur) // checkpoint only when emits are durable
		}
	}

	// The most recent target this session scanned. session_state (tier 2) retired with ADR 0023
	// §19 — the Session's own in-memory fields serve, since every reader runs in this process and
	// the one path that loses them (host death) fails the scope. Re-setting on a re-run is
	// idempotent (LWW), so it is resume-safe.
	s.Set("last_target", target)
	return nil // the findings were STREAMED via push
}

func getEngine(s *kontra.Session) (*nuclei.ThreadSafeNucleiEngine, bool) {
	if v, ok := s.Get("engine"); ok {
		ne, ok := v.(*nuclei.ThreadSafeNucleiEngine)
		return ne, ok
	}
	return nil, false
}

func getSink(s *kontra.Session) (*sink, bool) {
	if v, ok := s.Get("sink"); ok {
		sk, ok := v.(*sink)
		return sk, ok
	}
	return nil, false
}

func toFinding(ev *output.ResultEvent, target string) finding.Finding {
	matched := firstNonEmpty(ev.Matched, ev.Host, target)
	return finding.Finding{
		Target:    target,
		Template:  ev.TemplateID,
		Name:      ev.Info.Name,
		Severity:  ev.Info.SeverityHolder.Severity.String(),
		MatchedAt: matched,
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
