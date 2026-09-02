// Package casstore is the on-disk content-addressed store, exported for the appliance.
//
// The store is runtime/handler/internal/cas: sha256 -> store-if-absent on write, publish by
// os.Link so nothing partial is ever visible under an address, and a working copy by
// reflink where the filesystem allows it. `internal` is the right home for it — the
// codec's and the blob plane's addressing rules are this module's business — and it is
// exactly why this file has to exist. The appliance is a DIFFERENT MODULE (cli/), and
// issue 11 decided its embedded OCI registry stores layers in this store rather than
// opening a second one. A shared store the sharer cannot import is not shared.
//
// TYPE ALIASES, NOT A WRAPPER, and the contrast with codecserver is the reason. That
// package RESTATES an interface because bytes flow the other way: it accepts a store FROM
// the appliance, so structural identity is all it needs. Here the appliance takes the
// store itself, and a restatement would be a second implementation of the publish-by-link
// concurrency design — the one thing in that file that must exist exactly once. `= cas.Local`
// makes handler and cli name the same type, so there is nothing left to keep in step.
//
// THE SURFACE IS DELIBERATELY SMALL: what a registry needs to put a layer in, ask whether
// it already holds one, and stream one out. Materialization, copy-on-write and the mode
// ladder are hydration's, and are not re-exported, because nothing outside this module has
// asked for them and an unused export is a promise nobody checked.
package casstore

import "github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"

// Local is the store rooted at a directory: objects at <root>/cas/<ab>/<digest>, in-flight
// writes at <root>/tmp.
type Local = cas.Local

// DigestMismatch is what a write refused because the bytes did not hash to what was
// promised. It names BOTH sides — a registry turns it into the spec's DIGEST_INVALID, and
// the client that sent the wrong bytes is told which they were.
type DigestMismatch = cas.DigestMismatch

// NewLocal opens (and creates) the store rooted at root.
var NewLocal = cas.NewLocal

// ValidateDigest refuses anything that is not 64 lower-hex characters. Every caller-supplied
// digest goes through it before it can address anything, because the address is sliced into
// a path.
var ValidateDigest = cas.ValidateDigest

// Sha256Hex is the lower-hex sha256 every CAS address is.
var Sha256Hex = cas.Sha256Hex

// ErrMissing is "this address holds nothing" — the registry's BLOB_UNKNOWN.
var ErrMissing = cas.ErrMissing

// ErrBadDigest is a digest that is not a digest — the registry's DIGEST_INVALID on a
// reference it was handed rather than on bytes it read.
var ErrBadDigest = cas.ErrBadDigest
