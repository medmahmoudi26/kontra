package hydrate

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// A streaming hasher for the real-artifact test: a 54 MB object should be re-verified
// without being read into memory whole, which is exactly why cas.Local.ReadVerified is
// documented for small objects and cas.Local.Open exists for everything else.
type streamHasher struct{ h hash.Hash }

func newHasher() *streamHasher { return &streamHasher{h: sha256.New()} }

func (s *streamHasher) hex() string { return hex.EncodeToString(s.h.Sum(nil)) }

func copyInto(s *streamHasher, r io.Reader) (int64, error) { return io.Copy(s.h, r) }
