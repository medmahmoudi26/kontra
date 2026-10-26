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

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	gowebpki "github.com/gowebpki/jcs"
)

// maxDepth bounds the nesting of arrays and objects, as the library and encoding/json do.
const maxDepth = 10000

// Transform returns the JCS form of one JSON value. It refuses input that is not exactly one JSON value, an
// object with a duplicate key, and a number JCS cannot represent (outside IEEE 754 binary64).
//
// The input is scanned with encoding/json's tokenizer first (one value, no duplicate member names after
// unescaping, nothing but white space after it), then canonicalized by github.com/gowebpki/jcs v1.0.2, which
// refuses the same and also invalid UTF-8, lone surrogates and NaN-like literals.
func Transform(b []byte) ([]byte, error) {
	if err := prescan(b); err != nil {
		return nil, err
	}
	out, err := gowebpki.Transform(b)
	if err != nil {
		return nil, fmt.Errorf("jcs: %w", err)
	}
	return out, nil
}

// Canonical reports whether b is already in JCS form (Transform(b) equals b). The sealed envelope's header is
// refused unless it is (spec/ext-sealed.md §3: a non-canonical header, a duplicate key or an unknown field).
// Input Transform refuses is false with Transform's error.
func Canonical(b []byte) (bool, error) {
	out, err := Transform(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(out, b), nil
}

// prescan checks that b is exactly one JSON value with no duplicate member name in any object.
func prescan(b []byte) error {
	if b == nil {
		return errors.New("jcs: no JSON value")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := scanValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return errors.New("jcs: data after the JSON value")
		}
		return fmt.Errorf("jcs: %w", err)
	}
	return nil
}

func scanValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("jcs: %w", err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxDepth {
		return fmt.Errorf("jcs: nesting deeper than %d", maxDepth)
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return fmt.Errorf("jcs: %w", err)
			}
			key, ok := tok.(string)
			if !ok {
				return errors.New("jcs: a member name is not a string")
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("jcs: duplicate key %q", truncate(key))
			}
			seen[key] = struct{}{}
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("jcs: unexpected %q", rune(d))
	}
	if _, err := dec.Token(); err != nil { // the closing delimiter
		return fmt.Errorf("jcs: %w", err)
	}
	return nil
}

// truncate bounds what an error quotes of the input.
func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "…"
	}
	return s
}
