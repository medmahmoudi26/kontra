// backing.go — the store as a flat key -> bytes surface, for the one in-process reader that has
// no business speaking S3 to us.
//
// THIS FILE IS THE SEAM THE SPLIT ADDED. The payload codec used to reach `S3Server.store` — an
// unexported `*objectStore` — from the same flat package, which is exactly the coupling one
// `package appliance` could not refuse. It cannot now, so the access it actually needs is written
// down here: four whole-object operations against one bucket, no ranges, no listing, no multipart.
// A claim-check is read and written entire.
//
// IT IS STILL NOT THE S3 API, and that is the point the codec's header makes at length: the
// container fetched claim-checks with a signed HTTP GET through the host gateway, and in-process
// the two are the same program. Same bytes, same key layout (cas/<sha[:2]>/<sha>, pinned across
// all three SDKs by conformance/codec/fixtures.json), one fewer network hop, no SigV4 round trip
// against ourselves.
//
// NOTHING HERE NAMES THE CODEC. The four methods happen to satisfy `codecserver.Backing`
// structurally, which is how the codec takes one of these without this package importing it —
// the object store does not know what a claim-check is, and should not.
package objstore

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// Backing is one bucket of the store, addressed by key.
type Backing struct {
	store  *objectStore
	bucket string
}

// Backing returns the named bucket as a key -> bytes surface, creating it if it is not there.
//
// The create is the same best-effort ensureBucket the container's S3 backing did on its first
// write, minus the round trip: Start has already made DefaultS3Bucket, so this only does anything
// when a caller points KONTRA_S3_BUCKET somewhere else.
func (s *Server) Backing(bucket string) (*Backing, error) {
	if bucket == "" {
		bucket = DefaultS3Bucket
	}
	if err := s.store.createBucket(bucket); err != nil {
		return nil, err
	}
	return &Backing{store: s.store, bucket: bucket}, nil
}

// Get reads a whole object. The bool is "it was there"; an absent key is not an error, because a
// first write has to find nothing and store.
func (b *Backing) Get(_ context.Context, key string) ([]byte, bool, error) {
	f, _, err := b.store.open(b.bucket, key)
	if err != nil {
		if objectMissing(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Put writes a whole object.
func (b *Backing) Put(_ context.Context, key string, data []byte) error {
	_, err := b.store.put(b.bucket, key, bytes.NewReader(data))
	return err
}

// Exists is Get without the read.
func (b *Backing) Exists(_ context.Context, key string) (bool, error) {
	if _, err := b.store.stat(b.bucket, key); err != nil {
		if objectMissing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Delete removes an object. A key that is not there is not an error, for the same reason Get's
// absence is not.
func (b *Backing) Delete(_ context.Context, key string) error {
	return b.store.delete(b.bucket, key)
}

// objectMissing reports the store's two absence codes — the pair a caller should treat as "not
// present" rather than as a failure.
func objectMissing(err error) bool {
	var e *s3Error
	if errors.As(err, &e) {
		return e.Code == "NoSuchKey" || e.Code == "NoSuchBucket"
	}
	return false
}
