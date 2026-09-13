// Package codec is the claim-check Temporal PayloadCodec: payloads over a threshold are
// offloaded to the CAS and replaced by a small ref payload, transparently rehydrated on
// decode. It is the Go peer of python kontra.codec + orchestrator claimCheck.ts and
// must pass the SAME shared/conformance/codec/fixtures.json — any byte disagreement silently
// drops data across the namespace boundary. It uses the deep cas module for the
// store/verify protocol and owns only the Temporal-Payload concerns (threshold, marker,
// meta base64 round-trip, the double-encode guard).
package codec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/objectstore"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/wire"
)

// Marker is the encoding-metadata value stamped on an offloaded (ref) payload. VERBATIM —
// must stay byte-identical across languages.
const Marker = "binary/claim-check-v1"

// DefaultThreshold: offload iff len(data) > threshold (128 KiB).
const DefaultThreshold = 128 * 1024

// Codec implements converter.PayloadCodec. A nil cas (no object store) => passthrough.
type Codec struct {
	cas       *cas.CAS
	threshold int
}

// New builds a codec over a store (nil store => passthrough) with the given threshold.
func New(store *objectstore.Store, threshold int) *Codec {
	if store == nil {
		return &Codec{threshold: threshold}
	}
	return &Codec{cas: cas.New(store), threshold: threshold}
}

// ThresholdFromEnv reads KONTRA_S3_THRESHOLD (base-10), else DefaultThreshold.
func ThresholdFromEnv() int {
	if v := os.Getenv("KONTRA_S3_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return DefaultThreshold
}

var _ converter.PayloadCodec = (*Codec)(nil)

// Encode offloads each over-threshold, not-already-marked payload to the CAS.
func (c *Codec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	if c.cas == nil {
		return payloads, nil // passthrough (store disabled)
	}
	ctx := context.Background()
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		if string(p.GetMetadata()[converter.MetadataEncoding]) == Marker {
			out[i] = p // double-encode guard: already a ref
			continue
		}
		data := p.GetData()
		if len(data) <= c.threshold {
			out[i] = p // inline (strictly-greater offload rule)
			continue
		}
		digest, err := c.cas.Put(ctx, data)
		if err != nil {
			return payloads, err
		}
		meta := make(map[string]string, len(p.GetMetadata()))
		for k, v := range p.GetMetadata() {
			meta[k] = base64.StdEncoding.EncodeToString(v) // preserve original metadata
		}
		refData, err := json.Marshal(wire.BareRef{Sha256: digest, Size: uint64(len(data)), Meta: meta})
		if err != nil {
			return payloads, err
		}
		out[i] = &commonpb.Payload{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte(Marker)},
			Data:     refData,
		}
	}
	return out, nil
}

// Decode rehydrates each ref payload from the CAS, restoring the original metadata.
func (c *Codec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	if c.cas == nil {
		return payloads, nil
	}
	ctx := context.Background()
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		if string(p.GetMetadata()[converter.MetadataEncoding]) != Marker {
			out[i] = p
			continue
		}
		var ref wire.BareRef
		if err := json.Unmarshal(p.GetData(), &ref); err != nil {
			return payloads, err
		}
		// VALIDATE BEFORE THE DIGEST IS SLICED INTO A PATH. cas.RelKey's own header states the
		// rule — "Every path that can see caller input runs ValidateDigest first" — and this is
		// such a path: the ref comes off Temporal history, not from us. Without it a ref carrying
		// a one-character sha256 reaches CasKey's `sha[:2]` and PANICS.
		//
		// It surfaced when the CLI's fifth claim-check implementation was folded into this one:
		// that copy answered "claim-check ref has no usable sha256" for the same input, so the
		// unification would have traded an error a human can read for a crash. Fixed here rather
		// than in the caller, because every Go arm of this codec had the same exposure and the
		// point of the fold is that there is one implementation to fix.
		if err := cas.ValidateDigest(ref.Sha256); err != nil {
			return payloads, fmt.Errorf("claim-check ref is unusable: %w", err)
		}
		data, err := c.cas.GetVerified(ctx, ref.Sha256, "claim-check")
		if err != nil {
			return payloads, err
		}
		meta := make(map[string][]byte, len(ref.Meta))
		for k, v := range ref.Meta {
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
