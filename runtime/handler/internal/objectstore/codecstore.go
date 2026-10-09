package objectstore

import (
	"context"

	"github.com/medmahmoudi26/kontra/runtime/go/codec"
)

// AsCodecStore adapts a *Store to the two-method `Store` interface `runtime/go/codec` takes
// (kontra#11).
//
// WHY AN ADAPTER AND NOT A SIGNATURE CHANGE. `Store.Get` answers `([]byte, bool, error)` — the bool
// is "present", which every caller in this module reads and acts on, and it is a real distinction:
// a key that is absent is not a key that failed. The codec does not need it (an absent claim IS an
// error to the codec, since the payload it was told about is gone), so the adapter collapses the
// two exactly once, here, rather than widening the interface or narrowing the store.
//
// IT EXISTS SO THERE IS ONE CLAIM-CHECK CODEC IN GO. `runtime/handler/internal/codec` was a second
// implementation of `runtime/go/codec` — same `binary/claim-check-v1` marker, same 128 KiB default,
// same env var — kept honest with the first by its own arm of the cross-language conformance corpus.
// Two implementations plus a test proving they agree is strictly more to maintain than one.
// IT RETURNS THE INTERFACE, AND nil STAYS nil. `codec.New(nil, …)` is PASSTHROUGH — the codec
// serves unchanged payloads when no store is configured, which is how the handler runs with
// `KONTRA_S3_ENDPOINT` unset (`FromEnv` answers a nil store, and main.go passes it straight
// here). Returning a concrete struct here wrapped a nil `*Store` into a NON-nil interface, so
// that check stopped firing and the first encode dereferenced it.
// `TestPassthroughWithoutAStore` caught exactly that; the nil must survive the conversion.
func AsCodecStore(s *Store) codec.Store {
	if s == nil {
		return nil
	}
	return codecStore{s}
}

type codecStore struct{ s *Store }

// THE PREFIX IS APPLIED HERE, AND IT IS THE REASON THE TWO GO CODECS WERE NOT THE SAME PROGRAM.
//
// The shared codec addresses a blob at a PREFIX-LESS `cas/<sha[:2]>/<sha>` — `codec.CasKey`. The
// handler's store is prefix-aware (`Store.CasKey` goes through `Store.Key`), because a deployment
// sets `KONTRA_S3_PREFIX` to give a tenant its own namespace in one bucket. Handing the codec's key
// straight to `Put` dropped that prefix and wrote every blob to the bucket root — silently, since a
// write to the wrong key succeeds.
//
// THE CORPUS CAUGHT IT: `TestStorePrefixIsAPathSegment/nested-prefix-with-a-trailing-slash` and
// `TestPrefixIsTheWritersPrefix`. That is also the correction to the premise of this consolidation —
// the duplicate was not byte-for-byte, it differed in exactly this, and the difference is
// multi-tenant addressing. Mapping the key through `Key` is what makes ONE codec serve both.
func (c codecStore) Put(ctx context.Context, key string, data []byte) error {
	return c.s.Put(ctx, c.s.Key(key), data)
}

// A MISSING BLOB IS AN ERROR HERE, and that is the collapse this adapter exists to make. The codec
// is decoding a payload whose claim says the bytes were stored; if they are not there, the payload
// cannot be reconstructed and returning `(nil, nil)` would hand the caller an empty value as though
// it were the real one.
func (c codecStore) Get(ctx context.Context, key string) ([]byte, error) {
	b, ok, err := c.s.Get(ctx, c.s.Key(key))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound{Key: c.s.Key(key)}
	}
	return b, nil
}

// ErrNotFound says which key was missing, because "not found" without the key is a sentence nobody
// can act on.
type ErrNotFound struct{ Key string }

func (e ErrNotFound) Error() string {
	return "claim-check blob not found: " + e.Key + " — the payload it addresses is gone from the object store"
}
