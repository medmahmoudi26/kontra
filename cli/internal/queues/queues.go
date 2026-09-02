// Package queues derives the Temporal task-queue names, for the two halves that need them.
//
// ONE OF FOUR ARMS OF `shared/conformance/queues.json`. The same string is derived in Go here, in
// the Python host, in TypeScript (`@kontra/core`) and in the Warden — and the corpus is what holds
// them to one answer. Its header names the failure a drift produces: "the actor registers, polls a
// queue nobody schedules onto, and reports as a healthy idle Worker while every run hangs to
// StartToClose."
//
// It is a package rather than a function in `package main` because the Warden derives the same
// name on a Machine, and a second copy there would be a fifth arm of a four-arm contract.
package queues

// Shared is the (name, version) → task-queue contract: "{name}-{version}", or
// "{name}-shared" when version is empty.
//
// UNTIL 2026-08-28 THIS DERIVATION HAD NO CONGRUENCE TEST AT ALL. It appeared in the suite once,
// in workflow_test.go, as the CONTRAST half of an assertion about something else — so the eight
// places that must agree on this string were seven places and a comment. queues_conformance_test.go
// is the CLI's arm of shared/conformance/queues.json now.
func Shared(name, version string) string {
	if version != "" {
		return name + "-" + version
	}
	return name + "-shared"
}
