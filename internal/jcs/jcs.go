// Package jcs is RFC 8785 JSON Canonicalization (JCS) for the two places the module signs or binds JSON: the
// gate's card signature over the served card without `signatures` (spec/relay.md §3, D13) and the sealed
// envelope's header, which a receiver re-serializes and compares byte for byte (spec/ext-sealed.md §3).
//
// It is one seam over the vetted library (D166: github.com/gowebpki/jcs v1.0.2), so the module names that
// dependency once and a2a-go's unexported implementation is never copied. encoding/json is not JCS: key order,
// number form, and the escaping of U+2028/U+2029 and of <, > and & all differ.
//
// Work package: M3 WP-01.
package jcs

import "kontra.sh/kontra/internal/unimpl"

// Transform returns the JCS form of one JSON value. It refuses input that is not exactly one JSON value, an
// object with a duplicate key, and a number JCS cannot represent (outside IEEE 754 binary64).
func Transform(b []byte) ([]byte, error) {
	return nil, unimpl.Error("jcs.Transform", "M3 WP-01")
}

// Canonical reports whether b is already in JCS form (Transform(b) equals b). The sealed envelope's header is
// refused unless it is (spec/ext-sealed.md §3: a non-canonical header, a duplicate key or an unknown field).
func Canonical(b []byte) (bool, error) {
	return false, unimpl.Error("jcs.Canonical", "M3 WP-01")
}
