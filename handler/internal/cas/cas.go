// Package cas is the content-addressed store protocol as ONE deep module: sha256 ->
// store-if-absent on write, fetch-and-integrity-check on read. The claim-check codec and
// the blob activities both call it, so the address+integrity rules live in exactly one
// place — the deepening the architecture review flagged the Python seam for smearing
// across codec.py + blob.py (finding #01). The caller supplies only the error "noun"
// ("claim-check" / "node-result") for its surface; the protocol is owned here.
//
// TWO BACKINGS, ONE ADDRESS. CAS is the store over objectstore.Store — S3/SeaweedFS, the
// codec's claim-checks and the blob plane. Local (local.go) is the same protocol over a
// directory on the machine, and it is the appliance's artifact store: the hydrated Node
// runtime, the native addons, the built SPA, the opt-in Temporal Web UI, and the embedded
// registry's OCI layers. ADR 0031 §2 is explicit that this is ONE store with two customers
// and never a second one, so both address an object at cas/<sha[:2]>/<sha>. RelKey is that
// layout; TestKeyLayoutIsCongruent pins it against objectstore.Store.CasKey, because the
// day the two disagree is the day the registry and the object store hold the same bytes
// under two names and neither of them notices.
//
// COPY-ON-WRITE IS NOT A SECURITY BOUNDARY. Local.Materialize hands out reflinked,
// hardlinked or copied working copies, and the only thing any of those rungs guarantees is
// that writing to the working copy does not corrupt the stored object — a property about
// OUR bytes, not about the writer's reach. A COW overlay shares a page cache, a kernel, a
// network namespace and a filesystem view with the process that made it. This sentence is
// here because the proposal it refuses is attractive: running untrusted actor code directly
// against a COW overlay is cheap, it is fast, and it removes a container start. It is also
// wrong. Actors are the one part of this system running code we did not write, against
// targets hostile by definition; they get isolation from the container runtime, exactly as
// they do today. Deduplication and startup cost are what this mechanism is for.
package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/medmahmoudi26/kontra/handler/internal/objectstore"
)

type CAS struct{ store *objectstore.Store }

func New(store *objectstore.Store) *CAS { return &CAS{store: store} }

// Sha256Hex is the lower-hex sha256 used for every CAS address.
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RelKey is the ONE content-addressed layout as a slash-relative path: cas/<sha[:2]>/<sha>.
// objectstore.Store.CasKey is this same string with the store's prefix in front, and Local
// is this same string under a directory — three surfaces, one address.
//
// It slices the digest, so it PANICS on anything shorter than two characters. Every path
// that can see caller input runs ValidateDigest first; this is deliberately the raw layout
// with no opinion, so that the key it produces cannot drift from CasKey's.
func RelKey(digest string) string { return "cas/" + digest[:2] + "/" + digest }

// Put content-addresses data: digest = sha256(data); store at cas/<sha[:2]>/<sha> iff
// absent (dedup is free under content-addressing). Returns the digest.
func (c *CAS) Put(ctx context.Context, data []byte) (string, error) {
	digest := Sha256Hex(data)
	key := c.store.CasKey(digest)
	exists, err := c.store.Exists(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := c.store.Put(ctx, key, data); err != nil {
			return "", err
		}
	}
	return digest, nil
}

// GetVerified fetches by digest (derived, never carried) and re-checks integrity. noun
// shapes the exact error string for the caller's surface ("claim-check object missing:
// <key>" / "node-result integrity check failed for <key>").
func (c *CAS) GetVerified(ctx context.Context, digest, noun string) ([]byte, error) {
	key := c.store.CasKey(digest)
	data, found, err := c.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%s object missing: %s", noun, key)
	}
	if Sha256Hex(data) != digest {
		return nil, fmt.Errorf("%s integrity check failed for %s", noun, key)
	}
	return data, nil
}
