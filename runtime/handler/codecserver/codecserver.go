// Package codecserver serves Temporal's remote-codec HTTP contract — POST /encode and
// POST /decode — over the claim-check codec, so any reader that speaks that contract renders
// an offloaded payload instead of the opaque `$ref` blob Temporal stored. Its first caller is
// Temporal's Web UI; `temporal workflow show --codec-endpoint` speaks the same contract.
//
// IT USED TO BE A CONTAINER, and this package is the whole of what it was. `cmd/codec-server`
// plus `infra/Dockerfile.codec-server` plus a compose service with a published port existed to
// give ~40 lines of handler an address. ADR 0031 §1 settles it: "it is an HTTP handler in a
// container; it becomes an HTTP handler." The appliance (`cli/appliance/codec.go`) serves this
// on a listener it already owns, which is also what collapses the codec's port and the address
// the UI is told to call from two hand-synced settings into one derived one.
//
// THE CODEC IS A CLAIM-CHECK, NOT ENCRYPTION (ADR 0007 §4, ADR 0034 §6). What it does is
// OFFLOAD: a payload over the threshold (128 KiB) is written to the object store and replaced
// in history by a small ref, and a payload under it rides INLINE IN WORKFLOW HISTORY IN THE
// CLEAR, for the namespace's whole retention, readable by anything that can read history. There
// is no size at which a payload becomes secret — an offloaded one is just as readable to anyone
// who can reach the store. That is why `kontra secret` passes a NAME through history rather than
// a value, why a fleet credential is resolved at the last hop, and why a HITL `ask` redacts its
// context. Serving this endpoint does not weaken any of that, and a bigger threshold would not
// strengthen it: this is a size decision about history, not a confidentiality boundary.
//
// THE STORE IT READS MUST BE THE ONE THE WRITERS WROTE TO. A ref carries a digest, not a
// location, so a codec pointed at a different bucket or prefix answers "claim-check object
// missing" for a payload that exists. `Options.Store` and `Options.Prefix` are that pairing, and
// in the appliance both come from the object store in the same process.
package codecserver

import (
	"context"
	"net/http"

	"go.temporal.io/sdk/converter"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/codec"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/objectstore"
)

// DefaultThreshold is the offload line: strictly more than this many bytes is offloaded, this
// many or fewer stays inline. Re-exported from the codec so a caller outside this module names
// the same 128 KiB the Python and TypeScript peers do.
const DefaultThreshold = codec.DefaultThreshold

// DefaultUIOrigin is the origin Temporal's Web UI is served from by default. It is a CORS
// allow-list entry, not an address anything is fetched from.
const DefaultUIOrigin = "http://localhost:8233"

// Backing is the key -> bytes store the claim-check objects live in: objectstore.Backing,
// restated because that package is internal to this module and the appliance — a different
// module — has to be able to pass one. The two are structurally identical on purpose, which is
// also how they stay that way: New assigns one to the other, so a method added to either and
// not to this stops compiling here rather than at some caller.
type Backing interface {
	Get(ctx context.Context, key string) (data []byte, found bool, err error)
	Put(ctx context.Context, key string, data []byte) error
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
}

// Options configures the served codec.
type Options struct {
	// Store holds the offloaded objects. nil means PASSTHROUGH: /decode returns the ref
	// unchanged and the UI keeps rendering `$ref`. It is a legal state (the container ran that
	// way whenever KONTRA_S3_ENDPOINT was unset) and it is never the state you want, so a caller
	// that can tell should say so out loud.
	Store Backing

	// Prefix is the object-store key prefix (KONTRA_S3_PREFIX) the writers used. Empty is the
	// default and the common case.
	Prefix string

	// Threshold in bytes; 0 means ThresholdFromEnv. Only /encode consults it — a decode follows
	// the marker on the payload — so it matters for payloads submitted THROUGH this endpoint,
	// and it must agree with what the actors and the orchestrator use or a signal sent from the
	// UI is offloaded on a different rule from the same signal sent by a worker.
	Threshold int

	// Origin is the CORS origin admitted on /encode and /decode; empty means DefaultUIOrigin.
	Origin string
}

// ThresholdFromEnv reads KONTRA_S3_THRESHOLD, else DefaultThreshold. It is the same read the
// three SDK codecs make, kept here so the variable is named once in Go.
func ThresholdFromEnv() int { return codec.ThresholdFromEnv() }

// New builds the HTTP handler. It serves POST /encode and POST /decode and 404s everything
// else, which is the SDK handler's own behaviour, plus the CORS preflight the SDK omits.
func New(opts Options) http.Handler {
	var store *objectstore.Store
	if opts.Store != nil {
		store = objectstore.New(opts.Store, opts.Prefix)
	}
	threshold := opts.Threshold
	if threshold == 0 {
		threshold = ThresholdFromEnv()
	}
	origin := opts.Origin
	if origin == "" {
		origin = DefaultUIOrigin
	}
	return withCORS(origin, converter.NewPayloadCodecHTTPHandler(codec.New(store, threshold)))
}

// withCORS adds the CORS headers + OPTIONS preflight the native SDK handler does not provide.
// The Temporal UI fetches /encode and /decode from the BROWSER, so without these the browser
// blocks the response — and the UI's fallback is to render the undecoded payload, which is
// indistinguishable from the codec not running at all. The origin is echoed EXACTLY (never
// "*") because the UI may send credentials, and Access-Control-Allow-Credentials: true forbids
// the wildcard.
func withCORS(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-Namespace, Authorization, X-CSRF-Token, Caller-Type")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
