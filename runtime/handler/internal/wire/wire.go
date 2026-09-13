// Package wire holds the JSON structs that cross the Temporal/orchestrator boundary.
// The envelope stays JSON on the wire (ADR 0002); proto is only the type-of-record, so
// these are hand-written with exact snake_case tags (protoc-gen-go's camelCase jsonName
// tags would not match the wire) — the Go peer of Python's hand-written dataclasses. A
// congruence test (wire_congruence_test.go) holds EntryInput/BareRef to the proto field
// sets, exactly like Python's tests/test_workflows_client.py.
//
// Optional messages are POINTERS so "absent" is distinct from the zero value: the
// orchestrator omits unset optionals from the JSON entirely (interpreter.ts), so an
// InputRef nil means "no ref" (the batch is inline in Units).
package wire

// BareRef is the content-addressed claim-check ref {sha256, size, meta} (entry.proto
// BareRef). The run workflow RETURNS one of these when EntryInput.return_ref is set
// (always, from the orchestrator); the orchestrator integrity-checks it against the CAS.
type BareRef struct {
	Sha256 string            `json:"sha256"`
	Size   uint64            `json:"size"` // byte length of the JSON-serialized payload
	Meta   map[string]string `json:"meta"`
}

// EntryInput is the single argument to the "run" workflow (entry.proto EntryInput).
// Exactly one of Units / InputRef carries the batch. The actor runs ONE batch per run
// (ADR 0012 — the dispatcher shards, not the actor), so this is EXACTLY the proto
// field set: no session/chunk sharding knobs, no continue-as-new internals.
type EntryInput struct {
	Units          []any    `json:"units"`
	InputRef       *BareRef `json:"input_ref,omitempty"`
	ReturnRef      bool     `json:"return_ref"`
	RunID          string   `json:"run_id"`
	IdempotencyKey string   `json:"idempotency_key"`
	NodeID         string   `json:"node_id"`
	ExpectedDigest string   `json:"expected_digest"`
	// Run-wide config the dispatcher sets (reaches the author as self.params); also
	// where param-bound concurrency reads its value (entry.proto).
	Params map[string]any `json:"params,omitempty"`
	// Which Method of the actor this dispatch calls (ADR 0023 §5). Passed through to the
	// RunBatch payload, where the actor's registry resolves it per batch — one loaded
	// session serves several. "" leaves the actor to take its sole Method, or refuse.
	Method string `json:"method"`
	// Which Session this dispatch belongs to (ADR 0023 §6). "" = no scope was opened.
	SessionID string `json:"session_id"`
	// How long a Unit may go silent before Temporal calls the attempt dead, in seconds.
	// 0 = the production default (see `runActivityOptions`). Carried in the INPUT rather than read
	// from the environment because it is consumed by WORKFLOW code, which must replay identically.
	DebugHeartbeatSeconds int32 `json:"debug_heartbeat_seconds,omitempty"`
}
