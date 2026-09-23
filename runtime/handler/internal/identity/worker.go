package identity

// The handler's own name, in the same three fields every other Worker in this system uses.
//
// THIS PACKAGE ALREADY OWNS "how (name, version) becomes the strings the handler routes on", and a
// Worker's identity is one more of those strings — the one the server records on
// `WorkflowTaskStarted` and returns from `DescribeTaskQueue`. `panels/pollers.ts` reads it today
// and says so in a comment that this file makes obsolete:
//
//	Attribution comes from the poller identity, which neither `runtime/handler/main.go`
//	(`client.Dial` with no Identity) nor the Python host sets, so both are SDK defaults.
//
// Both set it now. The VALUE is unchanged for Go — `<pid>@<hostname>@<queue>` is what the SDK
// already wrote — which is the point: this is Temporal's convention, adopted deliberately, not a
// kontra scheme wearing Temporal's field. Nothing in `identityHost` changes.
//
// Unlike the queue derivations above, this is NOT part of the cross-language conformance corpus.
// A queue name is an address and a mismatch routes work into a void; an identity is a label and a
// mismatch costs a column in the Monitor. Holding it to the corpus would price a label like an
// address.

import (
	"fmt"
	"os"
)

var machineName = func() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		// Legible absence. An empty field two makes `identityHost` return undefined, which the
		// Monitor draws as `unknown` — the right answer, but for a reason nobody can see.
		return "unknown"
	}
	return h
}()

// WorkerIdentity is `<pid>@<hostname>@<queue>`.
func WorkerIdentity(queue string) string {
	return fmt.Sprintf("%d@%s@%s", os.Getpid(), machineName, queue)
}

// BuildID names the code this process is running: the Bundle's own sha256 when there is one, the
// actor version when there is not, and empty in a checkout — where the SDK's default is the better
// answer than a label that would be a guess.
func BuildID() string {
	if sha := os.Getenv("KONTRA_BUNDLE_SHA"); sha != "" {
		return sha
	}
	return os.Getenv("KONTRA_ACTOR_VERSION")
}
