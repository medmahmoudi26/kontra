package temporalhost

// Which Worker is this — the Go arm of `runtime/python/internals/workerid.py`.
//
// ── GO ALREADY WROTE THE RIGHT SHAPE, AND THAT IS WHY IT IS STATED HERE ─────────────────────────
//
// The Go SDK's default identity is `<pid>@<hostname>@<queue>`, which is exactly the shape both
// halves of this system now use — the Python side was the one that had to widen. So this file does
// not change what a Go Worker calls itself in the ordinary case. It states it, for two reasons
// that are not cosmetic:
//
//   - A DEFAULT IS NOT AVAILABLE TO THE CODE THAT NEEDS IT. `stream.go` has to put the Worker's
//     identity on a stream record, and `publishRecord` cannot ask the SDK what it decided. A value
//     the process computed is a value the process can also send.
//   - THE PYTHON AND GO ARMS MUST AGREE BY CONSTRUCTION. Two hosts publishing through entirely
//     different code is the failure the `gocanary` actor exists to catch (see the streaming
//     commit): a green Python canary was never evidence that a Go actor streams. The same applies
//     to the label on the record.
//
// BuildID is the Bundle's own sha256 — the OCI layer digest the Machine verified before unpacking
// — so "which build is this Worker running" is a server-side fact rather than something inferred
// from a deploy log. Worker Versioning STAYS OFF: `UseBuildIDForVersioning` is not set and
// `DeploymentOptions` is not used, because both change which Worker is handed which task and that
// is a routing decision with its own blast radius, not a side effect of wanting a label.

import (
	"fmt"
	"os"
)

// WorkerIdentity is `<pid>@<hostname>@<queue>` — what this process passes to Temporal as its
// identity and what it stamps on the records it streams.
//
// ONE HOSTNAME DERIVATION IN THIS PACKAGE, and it is `Machine` in host.go. A second
// `os.Hostname()` here would be a second answer to "which Machine", free to disagree with the one
// a Batch reports as its own — which is the precise class of bug this whole file exists to close.
//
// `Machine` is EMPTY when the lookup fails, and that is right where it is used: an unrecorded
// Machine on a Batch is honest. It is wrong in field two of an identity, where empty makes
// `shared/core/src/queues.ts::identityHost` return undefined and the Monitor draw the Worker as
// unattributable — a fleet-shaped symptom for a hostname-lookup cause. So the substitution happens
// HERE, at the one place the distinction matters.
func WorkerIdentity(queue string) string {
	host := Machine
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%d@%s@%s", os.Getpid(), host, queue)
}

// BuildID names the CODE, where the identity names the process.
//
// Empty when neither variable is set, which is the local-dev case: there is no Bundle, so there is
// no honest build id, and the SDK's own default is then better than a wrong label.
func BuildID() string {
	if sha := os.Getenv("KONTRA_BUNDLE_SHA"); sha != "" {
		return sha
	}
	return os.Getenv("KONTRA_ACTOR_VERSION")
}
