// Package identity is the single owner of how an actor's (name, version) becomes the
// strings the handler routes on — task queue, Nexus service + endpoint, and the blob-plane
// activity names. handler and the SDKs derive these independently (the decoupling rule), so
// every literal here is part of the cross-language contract.
//
// WHAT HOLDS IT: shared/conformance/queues.json, which every language executes. Not a count of peers
// written into a comment — that count was wrong in three files at once before the corpus
// existed, and a comment cannot fail. identity_conformance_test.go is this package's arm.
package identity

import "strings"

// Blob-plane activity names (registered on the handler worker).
const (
	StoreBlobActivity = "kontra.store_blob"
	FetchBlobActivity = "kontra.fetch_blob"
)

// SharedQueue is the task queue an actor's worker binds and the orchestrator routes to.
// VERBATIM: "{actor}-{version}", or "{actor}-shared" when version is empty (the DIY / CLI path).
//
// NOT SANITISED, and that is a decision the corpus carries adversarial rows for: Temporal accepts
// a space and a non-ASCII rune in a queue name, so a derivation that cleaned this up would route
// to a queue nobody polls. The endpoint name below is the one that sanitises.
func SharedQueue(actor, version string) string {
	if version != "" {
		return actor + "-" + version
	}
	return actor + "-shared"
}

// SessionsQueue is the queue THE ACTOR PROCESS ITSELF polls (ADR 0018) — it registers RunBatch
// and Close on this one, while the handler keeps the workflow and the blob activities on
// SharedQueue. Splitting them is what lets the two halves be different languages: Temporal
// routes activities per queue, so the Python actor and the Go handler never share a worker.
//
// It is also where a Session is OPENED (ADR 0023 §6): every worker of this version polls it, so
// Temporal's dispatch of the open IS the placement decision, and the host that takes it starts
// polling that Session's own queue — see SessionQueue below.
//
// It is no longer the session admission plane. The cap (KONTRA_MAX_PARALLEL_SESSIONS) was this
// worker's activity-execution slot count while a live Session meant a slot held for the length
// of a batch; an open frees its slot as soon as the Session's worker is up, so the cap is now a
// count of live Sessions held by the actor process itself.
//
// VERBATIM: "{SharedQueue}-sessions". shared/conformance/queues.json §sessions.
func SessionsQueue(actor, version string) string {
	return SessionsQueueOf(SharedQueue(actor, version))
}

// SessionsQueueOf is SessionsQueue for a caller that already holds the shared queue and cannot
// derive it — workflow code, which reads its own task queue off the workflow context because it
// may not read env and stay deterministic.
//
// It exists so the "-sessions" literal is written ONCE in this module. workflow.go spelled it
// inline, which made the suffix a two-writer contract inside a single package: the one shape
// ADR 0035 §3 says has no business being a contract at all.
func SessionsQueueOf(sharedQueue string) string {
	return sharedQueue + "-sessions"
}

// SessionQueue is the queue ONE LIVE SESSION is addressed on (ADR 0023 §6), given the actor's
// shared queue and the Session id the dispatch named. `{shared}-s-{sessionId}`.
//
// The singular of SessionsQueue above and a different plane. That one is polled by every worker
// of this actor version and is where an OPEN lands, so Temporal's dispatch is the placement
// decision; this one is polled by the single worker that answered that open, which is what pins
// every call of the scope to the process holding the loaded resource. The queue name is the whole
// addressing scheme: no placement directory, no lease, no idle policy — and an orphaned queue is
// a ScheduleToStart timeout, which is what makes "losing the host fails the scope" a decision.
//
// It takes the shared queue rather than (actor, version) because the only caller is workflow
// code, which reads its own task queue from the workflow context — it cannot read env and stay
// deterministic. shared/conformance/queues.json §session.
//
// NO ID, NO QUEUE. An empty session id would otherwise derive "{shared}-s-", a real queue that
// every Session of this actor would share: the pinning gone, nothing failing, and the scope's
// calls answered by whichever worker polled first. Empty is the refusal every side makes in its
// own idiom (Python raises), and runActivityOptions guards before it schedules.
func SessionQueue(sharedQueue, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return sharedQueue + "-s-" + sessionID
}

// NexusServiceName is the one Nexus service every actor serves — the orchestrator
// dispatches the single way (ADR 0001). Must match the actorkit + orchestrator sides.
const NexusServiceName = "kontra.actor"

// EndpointName is the Nexus endpoint an actor registers on boot and the orchestrator
// calls — the single Go source of truth both the worker (creates it) and any caller
// derive identically. "kontra-{name}-{version}" with every non-alphanumeric collapsed to '-'
// (the running dev server enforces ^[a-zA-Z][a-zA-Z0-9-]*[a-zA-Z0-9]$, so it wants '-', not the
// proto .pyi's '_'), doubles collapsed, ends stripped (echo 0.1.0 -> "kontra-echo-0-1-0").
//
// THE ONE DERIVATION HERE THAT SANITISES. shared/conformance/queues.json §endpoint runs the same
// inputs through this and through §shared, which passes them verbatim: a space is legal in a
// queue name and illegal in an endpoint name, and the two rules must not be shared.
func EndpointName(name, version string) string {
	// ponytail: separators collapse to '-', so punctuated name/version could alias
	// (a.b/c vs a/b.c); fine for local single-author, revisit if endpoint names collide.
	var b strings.Builder
	for _, r := range "kontra-" + name + "-" + version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	safe := b.String()
	for strings.Contains(safe, "--") {
		safe = strings.ReplaceAll(safe, "--", "-")
	}
	return strings.Trim(safe, "-")
}
