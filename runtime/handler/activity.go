package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/wire"
)

// Activities holds the handler-side activity implementations. The CAS handle is
// nil-safe: when the object store is unconfigured, blob is nil and the blob-plane
// activities error out (the codec falls back to passthrough elsewhere).
type Activities struct {
	blob *cas.CAS
}

// RunBatchInput is the argument to the actor's RunBatch activity: the one batch of units +
// params, keyed by the actor id (which the workflow derives so retries land on the same stateful
// instance and its committed map skips already-done units — exactly-once reload).
// RunID/NodeID/RunDate ride along so the actor can key its per-unit blobs
// (units/run={run}/dt={run_date}/actor={actor}/shard=…).
//
// The activity is implemented in the ACTOR's process, not here (ADR 0018), so this struct is one
// half of a cross-language wire contract: the peer is the `payload` dict read by
// runtime/python/internals/engine.py's run_batch.
type RunBatchInput struct {
	ActorID string         `json:"actor_id"`
	Units   []any          `json:"units"`
	Params  map[string]any `json:"params"`
	RunID   string         `json:"run_id"`
	NodeID  string         `json:"node_id"`
	RunDate string         `json:"run_date"`
	// Which Method of the actor to run (ADR 0023 §5), forwarded verbatim from EntryInput.
	// The actor resolves it against its registry per batch — which is what lets one loaded
	// session serve several Methods. "" leaves the actor to take its sole one, or refuse.
	Method string `json:"method"`
}

// Done and Isolated used to be carried here: per-turn running totals the workflow plumbed
// forward so each turn's activity could resume the progress count. The turn loop is gone (one
// activity per batch), and the actor now owns both counters for the whole batch and reports
// them on its OWN heartbeat — see _beat in runtime/python/internals/engine.py, decoded by
// control/orchestrator/src/heartbeat.ts. Nothing was lost; the accounting moved to the process that
// actually has the numbers.

// handlerActorName is the actor this worker serves (KONTRA_ACTOR_NAME), snapshotted at
// process start. Read here (not in workflow code) so the workflow's KontraActor search-
// attribute upsert (#04) stays deterministic: the value is fixed at boot and uniform
// across every worker of this one-per-actor deployment, so it replays identically.
var handlerActorName = os.Getenv("KONTRA_ACTOR_NAME")

// StoreBlob content-addresses a payload into the CAS and returns the ref, STORING IT
// VERBATIM.
//
// It used to wrap a bare list into `{results, failures}` before storing. That wrap is what
// made a Method's result ref un-chainable: an input ref must address a bare list of units
// (RunWorkflow rehydrates it into []any), so every ref the deployed handler could mint was
// the wrong shape by construction, and a caller had to fetch and re-send the rows — the
// ADR 0007 violation this exists to remove.
//
// `kind` is the discriminator a reader uses to tell the two bodies apart, and it is the only
// extension point here that costs no proto change: BareRef.Meta is already map<string,string>
// (entry.proto:37). NOTE it is a DIFFERENT convention from the claim-check codec's meta
// (internal/codec/codec.go:81), which base64-encodes its values; these are plain strings and
// the two never meet — the codec's refs live inside the converter, this one is an operation's
// return value.
func (a *Activities) StoreBlob(ctx context.Context, payload any) (wire.BareRef, error) {
	if a.blob == nil {
		return wire.BareRef{}, fmt.Errorf("object store not configured")
	}
	kind := "envelope"
	if _, ok := payload.([]any); ok {
		kind = "units"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return wire.BareRef{}, err
	}
	sha, err := a.blob.Put(ctx, data)
	if err != nil {
		return wire.BareRef{}, err
	}
	return wire.BareRef{
		Sha256: sha,
		Size:   uint64(len(data)),
		Meta:   map[string]string{"kind": kind},
	}, nil
}

// FetchBlob rehydrates a batch (or result) payload from the CAS by ref.
func (a *Activities) FetchBlob(ctx context.Context, ref wire.BareRef) (any, error) {
	if a.blob == nil {
		return nil, fmt.Errorf("object store not configured")
	}
	data, err := a.blob.GetVerified(ctx, ref.Sha256, "node-result")
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RunBatch and Close are NOT implemented here any more (ADR 0018).
//
// The actor process serves them itself — it is a Temporal activity worker — so the workflow
// schedules them BY NAME onto the actor's own task queue. What is left in this file is the blob
// plane, which is genuinely the handler's: it is what makes a Unit's payload a content-addressed
// ref.
