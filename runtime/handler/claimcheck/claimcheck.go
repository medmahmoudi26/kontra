// Package claimcheck is the claim-check Temporal PayloadCodec, exported for the install and
// the CLI.
//
// The implementation is runtime/handler/internal/codec over runtime/handler/internal/objectstore, and `internal`
// is the right home for it — the marker, the ref shape, the metadata round-trip and the
// content-addressed key layout are this module's business. This file exists for the same reason
// `casstore` and `hydratestore` do, and it is the third of the same kind. The CLI is a DIFFERENT
// MODULE, and a codec the sharer cannot import is not shared: `cli/claimcheck.go` held a FIFTH
// implementation of this wire format — its own marker constant, its own `{sha256,size,meta}`
// struct, its own base64 metadata decode and its own `cas/<ab>/<sha>` string — and cited
// shared/conformance/codec/fixtures.json in a comment while no test of it ever opened the file.
//
// TYPE ALIASES AND A CONSTRUCTOR, NOT A WRAPPER, for the reason `casstore` gives: a wrapper would
// be a second implementation of the one thing that must exist exactly once. `= codec.Codec` makes
// handler and cli name the same type, so there is nothing left to keep in step.
//
// THE STORE IS THE CALLER'S AND THE KEY LAYOUT IS NOT. `Backing` is four whole-object operations
// against one bucket, so a caller brings whatever transport it has — the handler brings an S3
// client, the install brings its own object store in-process, and the CLI brings an unsigned
// HTTP GET because it carries no S3 SDK. What none of them brings is the ADDRESS: `New` wraps the
// backing in the prefix-aware layout, so `cas/<sha[:2]>/<sha>` under the store prefix is derived
// here, once, for all three. That is the difference between a transport and a fifth codec.
//
// THIS IS A CLAIM-CHECK, NOT ENCRYPTION (ADR 0007 §4, ADR 0034 §6). A payload under the threshold
// rides inline in workflow history in the clear and an offloaded one is just as readable to
// anyone who can reach the store. See the codecserver package header; nothing here changes it.
package claimcheck

import (
	"github.com/medmahmoudi26/kontra/runtime/go/codec"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/objectstore"
)

// Marker is the encoding-metadata value stamped on an offloaded (ref) payload. The ONE Go
// definition of it outside this module's internals — a drift reads as a payload that "isn't a
// ref" and passes through unfetched, silently, as a JSON blob of the ref itself.
const Marker = codec.Marker

// DefaultThreshold is the offload line: strictly more than this many bytes is offloaded, this
// many or fewer stays inline (128 KiB).
const DefaultThreshold = codec.DefaultThreshold

// Codec is the converter.PayloadCodec itself. A caller that only decodes embeds this and
// overrides Encode; it does not write a second Decode.
type Codec = codec.Codec

// Backing is the raw key -> bytes store the claim-check objects live in. The alias, rather than
// a restatement, so a caller in another module implements the same four methods this module's
// own S3 and in-memory backings do.
type Backing = objectstore.Backing

// New builds the codec over a backing and the key prefix its writers used (KONTRA_S3_PREFIX;
// empty is the common case). A nil backing is PASSTHROUGH — legal, never what you want, and the
// state the container ran in whenever KONTRA_S3_ENDPOINT was unset.
//
// A ref carries a digest and not a location, so the prefix has to be the one the WRITERS used or
// this answers "claim-check object missing" for an object that is right there.
func New(store Backing, prefix string, threshold int) *Codec {
	if store == nil {
		return codec.New(nil, threshold)
	}
	return codec.New(objectstore.AsCodecStore(objectstore.New(store, prefix)), threshold)
}

// ThresholdFromEnv reads KONTRA_S3_THRESHOLD, else DefaultThreshold — the same read the three SDK
// codecs make, so the variable is named once in Go.
func ThresholdFromEnv() int { return codec.ThresholdFromEnv() }
