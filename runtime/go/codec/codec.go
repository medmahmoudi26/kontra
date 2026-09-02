// Package codec is the claim-check Temporal PayloadCodec for the Go actor host: payloads over a
// threshold are offloaded to the CAS and replaced by a small ref payload, transparently
// rehydrated on decode.
//
// It is an INDEPENDENT implementation of the same wire format as handler/runtime/go/codec, the
// Python actorkit internals/codec.py and the orchestrator's claimCheck.ts — deliberately, per the
// decoupling rule: actorkit and the handler are separate modules and never import each other.
// What keeps four implementations honest is the shared corpus in conformance/codec/, because any
// byte disagreement here silently drops data across a language boundary.
//
// # Why the actor needs this — do not remove it
//
// The handler's client encodes anything over 128 KiB as a `binary/claim-check-v1` ref, and the
// worker executing that activity must be able to decode it. Without this codec, every
// over-threshold batch fails with
//
//	Failed decoding arguments: 'Unknown payload encoding binary/claim-check-v1'
//
// retried to exhaustion. It is a loud failure, but only once a batch is big enough, so small runs
// pass and a real one does not.
package codec

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

// Marker is the encoding-metadata value stamped on an offloaded (ref) payload. VERBATIM — it must
// stay byte-identical across all four implementations.
const Marker = "binary/claim-check-v1"

// DefaultThreshold: offload iff len(data) > threshold (128 KiB). STRICTLY greater — a payload of
// exactly the threshold stays inline, which the conformance corpus pins.
const DefaultThreshold = 128 * 1024

// Store is the blob plane the codec addresses. Satisfied by runtime/go/unitstore's putter plus a
// getter; kept minimal so a test can fake it with a map.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// ref is the on-the-wire body of an offloaded payload. Field names are part of the contract.
type ref struct {
	Sha256 string            `json:"sha256"`
	Size   uint64            `json:"size"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// Codec implements converter.PayloadCodec. A nil store means passthrough.
type Codec struct {
	store     Store
	threshold int
}

// New builds a codec over a store (nil => passthrough) with the given threshold.
func New(store Store, threshold int) *Codec {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	return &Codec{store: store, threshold: threshold}
}

// ThresholdFromEnv reads KONTRA_S3_THRESHOLD (base-10), else DefaultThreshold. Mirrors the Go
// handler's ThresholdFromEnv and Python's threshold_from_env.
func ThresholdFromEnv() int {
	if v := os.Getenv("KONTRA_S3_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return DefaultThreshold
}

// CasKey is the content address: `cas/<sha[:2]>/<sha>`. The two-char shard keeps any one
// directory small, and the lower-hex case is part of the key.
func CasKey(sha string) string { return "cas/" + sha[:2] + "/" + sha }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ converter.PayloadCodec = (*Codec)(nil)

// Encode offloads each over-threshold, not-already-marked payload to the CAS.
func (c *Codec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	if c.store == nil {
		return payloads, nil // passthrough (store disabled)
	}
	ctx := context.Background()
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		// Double-encode guard: an already-marked payload is a ref, never re-offloaded.
		if string(p.GetMetadata()[converter.MetadataEncoding]) == Marker {
			out[i] = p
			continue
		}
		data := p.GetData()
		if len(data) <= c.threshold {
			out[i] = p
			continue
		}
		digest := sha256Hex(data)
		if err := c.store.Put(ctx, CasKey(digest), data); err != nil {
			return payloads, err
		}
		// The ORIGINAL metadata rides in the ref, base64-std per value, so a non-UTF-8 value
		// survives the JSON round-trip (the corpus pins an `x-bin` case for exactly this).
		meta := make(map[string]string, len(p.GetMetadata()))
		for k, v := range p.GetMetadata() {
			meta[k] = base64.StdEncoding.EncodeToString(v)
		}
		body, err := json.Marshal(ref{Sha256: digest, Size: uint64(len(data)), Meta: meta})
		if err != nil {
			return payloads, err
		}
		out[i] = &commonpb.Payload{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte(Marker)},
			Data:     body,
		}
	}
	return out, nil
}

// Decode rehydrates each ref payload from the CAS, restoring the original metadata.
func (c *Codec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	if c.store == nil {
		return payloads, nil
	}
	ctx := context.Background()
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		// Only the MARKER triggers a fetch, never the payload's shape — a non-ref payload that
		// happens to look like JSON must pass through untouched.
		if string(p.GetMetadata()[converter.MetadataEncoding]) != Marker {
			out[i] = p
			continue
		}
		var r ref
		if err := json.Unmarshal(p.GetData(), &r); err != nil {
			return payloads, err
		}
		data, err := c.store.Get(ctx, CasKey(r.Sha256))
		if err != nil {
			return payloads, err
		}
		// Verify. A silent mismatch here is the failure the whole conformance corpus exists to
		// prevent, so it is checked rather than trusted.
		if got := sha256Hex(data); got != r.Sha256 {
			return payloads, fmt.Errorf("claim-check integrity check failed for %s: want %s, got %s",
				CasKey(r.Sha256), r.Sha256, got)
		}
		meta := make(map[string][]byte, len(r.Meta))
		for k, v := range r.Meta {
			raw, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return payloads, err
			}
			meta[k] = raw
		}
		out[i] = &commonpb.Payload{Metadata: meta, Data: data}
	}
	return out, nil
}

// DataConverter is what an actor's Temporal client must be built with. Equivalent to the
// handler's converter.NewCodecDataConverter(default, codec).
func DataConverter(store Store) converter.DataConverter {
	return converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), New(store, ThresholdFromEnv()))
}
