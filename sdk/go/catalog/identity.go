// Package catalog is the Go CALLER's side — the peer of Python's `actorkit.catalog`.
//
// Everything in `lib/kontra` is the CALLEE: you write an Actor, register Methods, call Serve().
// This package is the other half. It lets you write a plain Temporal workflow in Go and drive
// deployed Actors and Datasets from it with ordinary control flow:
//
//	func Sweep(ctx workflow.Context, req Req) (Out, error) {
//		crawler := catalog.Actor("crawl4ai", "0.3.0")
//		s, err := crawler.Open(ctx)
//		if err != nil { return Out{}, err }
//		defer s.Close(ctx)
//
//		for batch, err := range catalog.Dataset("targets").Batches(ctx, 200, catalog.OrderBy("host")) {
//			if err != nil { return Out{}, err }
//			found, _, err := s.Call(ctx, "crawl", batch)
//			if err != nil { return Out{}, err }
//			if _, _, err := s.Call(ctx, "extract", found); err != nil { return Out{}, err }
//		}
//		return Out{}, nil
//	}
//
// ADR 0023 §22 originally reserved this side for Python. It was reversed on 2026-08-14: the
// cost estimate behind it was wrong — go.temporal.io/sdk is already a direct requirement of this
// module, so nothing is downloaded and no module gains a dependency. What survives from the
// original reasoning is only that the two sides are written twice, never shared.
//
// WHY THE SHAPE PORTS AT ALL. §18 is what makes it possible: emit names its Unit rather than a
// position in control flow, so no part of this needs `async`. `for batch, err := range ...` is
// the peer of `async for`, and `defer s.Close(ctx)` the peer of `async with`. Each is the plain
// idiom of its own language rather than a translation of the other's.
package catalog

import "strings"

// The Nexus service every deployed actor serves, and its one operation. Re-declared here rather
// than imported: `runtime/handler/internal/identity` is in another module and the decoupling rule
// forbids reaching into it, the same rule `runtime/go/registrar` states for the callee side.
const (
	ServiceName  = "kontra.actor"
	RunOperation = "run"
)

// Activity names scheduled BY NAME across a process boundary — a two-writer contract with no
// registration step to catch a rename.
const (
	// Served by the handler on every deployed actor's shared queue.
	FetchBlobActivity = "kontra.fetch_blob"
	// Served by the ACTOR's own process (actorkit's temporalhost), bracketing a Session.
	OpenSessionActivity  = "OpenSession"
	CloseSessionActivity = "CloseSession"
	// Served by the orchestrator's dataset pager, on DatasetQueue.
	PageDatasetActivity = "pageDataset"
)

// DatasetQueue is where the orchestrator serves dataset paging. Mirrors
// control/orchestrator/src/queues.ts and sdk/python/actorkit/catalog.py.
const DatasetQueue = "kontra-datasets"

// SharedQueue is the actor's own task queue, where its handler serves the workflow and the blob
// activities: `{name}-{version}`, or `{name}-shared` when a version is absent.
//
// THE FOUR DERIVATIONS BELOW ARE A CROSS-LANGUAGE CONTRACT with no shared code, by design, and
// a drift in any of them has no loud failure mode: the call goes to a queue nobody polls and the
// workflow waits until ScheduleToStart fires.
//
// WHAT HOLDS THEM TO ONE ANSWER IS shared/conformance/queues.json, which every language executes —
// identity_conformance_test.go is this package's arm. It is deliberately NOT a comment counting
// the peers: the count in this file said "a SIXTH", the one in pollers.ts said "a fourth", the
// one in nexusRegistry.ts said "THE FIFTH", and all three were wrong at the same time.
//
// NOT SANITISED. Temporal accepts a space and a non-ASCII rune in a queue name, so a derivation
// that cleaned this up would route to a queue nobody polls. EndpointName below is the one that
// sanitises, and the corpus runs the same inputs through both.
func SharedQueue(name, version string) string {
	if version == "" {
		return name + "-shared"
	}
	return name + "-" + version
}

// SessionsQueue is where the actor PROCESS polls for RunBatch/Close. You do not schedule onto it
// directly — the handler's workflow does — but an OpenSession lands here.
func SessionsQueue(name, version string) string {
	return SharedQueue(name, version) + "-sessions"
}

// SessionQueue is where ONE live Session is addressed: `{shared}-s-{sessionID}` (ADR 0023 §6).
//
// Not the plural above: that is the actor's standing queue, polled by every worker of that
// version. This one comes into existence when a worker activates a Session and is polled by that
// worker alone, which is what pins every call in a scope to one process. The queue name IS the
// address — no placement directory, no lease, no idle policy — and an orphaned queue is a
// ScheduleToStart timeout rather than a silent re-activation somewhere else.
//
// NO ID, NO QUEUE: the empty string rather than `{shared}-s-`, which is a real queue every
// Session of this actor would share. shared/conformance/queues.json §session records that refusal and
// what each language's idiom for it is.
func SessionQueue(name, version, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return SharedQueue(name, version) + "-s-" + sessionID
}

// EndpointName is the Nexus endpoint an actor's worker creates on boot: `kontra-{name}-{version}`
// with every non-alphanumeric collapsed to '-', doubles collapsed, ends stripped
// (echo 0.1.0 -> "kontra-echo-0-1-0").
func EndpointName(name, version string) string {
	raw := "kontra-" + name + "-" + version
	var b strings.Builder
	for _, r := range raw {
		if r < 128 && (r == '-' || ('0' <= r && r <= '9') ||
			('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	safe := b.String()
	for strings.Contains(safe, "--") {
		safe = strings.ReplaceAll(safe, "--", "-")
	}
	return strings.Trim(safe, "-")
}
