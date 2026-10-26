package bech32

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"filippo.io/age"
)

// BIP-173's valid strings, all of which carry whole bytes, decode, and encode back to themselves.
func TestDecodeBIP173Valid(t *testing.T) {
	cases := []struct {
		s, hrp, dataHex string
	}{
		{"A12UEL5L", "A", ""},
		{"a12uel5l", "a", ""},
		{"an83characterlonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1tt5tgs",
			"an83characterlonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio", ""},
		{"abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw", "abcdef", "00443214c74254b635cf84653a56d7c675be77df"},
		{"11qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqc8247j",
			"1", strings.Repeat("00", 51)},
		{"?1ezyfcl", "?", ""},
		// BIP-173's 90-character limit does not apply (an age1pq1… recipient is 1,959 characters).
		{"an84characterslonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1569pvx",
			"an84characterslonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio", ""},
	}
	for _, c := range cases {
		hrp, data, err := Decode(c.s)
		if err != nil {
			t.Fatalf("Decode(%q): %v", c.s, err)
		}
		if hrp != c.hrp || hex.EncodeToString(data) != c.dataHex {
			t.Fatalf("Decode(%q) = %q, %x; want %q, %s", c.s, hrp, data, c.hrp, c.dataHex)
		}
		back, err := Encode(hrp, data)
		if err != nil {
			t.Fatalf("Encode(%q, %x): %v", hrp, data, err)
		}
		if !strings.EqualFold(back, c.s) {
			t.Fatalf("Encode(Decode(%q)) = %q", c.s, back)
		}
	}
}

// BIP-173's invalid strings are refused, with mixed case and padding added.
func TestDecodeBIP173Invalid(t *testing.T) {
	for _, s := range []string{
		"\x201nwldj5",   // hrp character out of range
		"\x7f1axkwrx",   // hrp character out of range
		"\x801eym55h",   // hrp character out of range
		"pzry9x0s0muk",  // no separator
		"1pzry9x0s0muk", // empty hrp
		"x1b4n0q5v",     // invalid data character
		"li1dgmt3",      // checksum too short
		"de1lg7wt\xff",  // invalid character in the checksum
		"A1G7SGD8",      // checksum computed with the uppercase hrp
		"10a06t8",       // empty hrp
		"1qzzfhee",      // empty hrp
		"a12UEL5L",      // mixed case
		"a12uel5m",      // bad checksum
		"",
	} {
		if hrp, data, err := Decode(s); err == nil {
			t.Fatalf("Decode(%q) = %q, %x; want an error", s, hrp, data)
		}
	}
	// One data group of non-zero padding: 5 bits that are not a whole byte.
	bad, err := encodeGroups("a", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(bad); err == nil {
		t.Fatalf("Decode(%q): non-zero padding accepted", bad)
	}
}

// encodeGroups is Encode over 5-bit groups as given (for padding tests).
func encodeGroups(hrp string, groups []byte) (string, error) {
	sum := polymod(append(append(hrpExpand(hrp), groups...), make([]byte, checksumLen)...)) ^ 1
	var b strings.Builder
	b.WriteString(hrp + "1")
	for _, v := range groups {
		b.WriteByte(charset[v])
	}
	for i := range checksumLen {
		b.WriteByte(charset[sum>>(5*(5-i))&31])
	}
	return b.String(), nil
}

// age's own strings: the secret key's scalar and the recipient's point are 32 bytes each, and re-encode
// to exactly what age wrote.
func TestAgeStrings(t *testing.T) {
	for range 20 {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{id.String(), id.Recipient().String()} {
			hrp, data, err := Decode(s)
			if err != nil {
				t.Fatalf("Decode(%q): %v", s, err)
			}
			if len(data) != 32 {
				t.Fatalf("Decode(%q): %d bytes", s, len(data))
			}
			back, err := Encode(hrp, data)
			if err != nil || back != s {
				t.Fatalf("Encode(Decode(%q)) = %q, %v", s, back, err)
			}
		}
		if hrp, _, _ := Decode(id.String()); hrp != "AGE-SECRET-KEY-" {
			t.Fatalf("secret key hrp %q", hrp)
		}
		if hrp, _, _ := Decode(id.Recipient().String()); hrp != "age" {
			t.Fatalf("recipient hrp %q", hrp)
		}
	}
	hy, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	r := hy.Recipient().String()
	hrp, data, err := Decode(r)
	if err != nil || hrp != "age1pq" || len(r) < 1900 {
		t.Fatalf("Decode(hybrid recipient of %d characters) = %q, %d bytes, %v", len(r), hrp, len(data), err)
	}
}

func TestEncodeRefuses(t *testing.T) {
	for _, hrp := range []string{"", "Age", "a b", "a\x7f"} {
		if s, err := Encode(hrp, []byte{1, 2}); err == nil {
			t.Fatalf("Encode(%q) = %q; want an error", hrp, s)
		}
	}
}

// FuzzDecode: Decode never panics, and what it accepts re-encodes to the same string up to case and
// decodes again to the same bytes.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{"a12uel5l", "A12UEL5L", "abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw", "?1ezyfcl",
		"age1q4fswtsuh49t6q030nkj696mjz2ss022uta9x2cm4vkx254vs3asl09jlq",
		"AGE-SECRET-KEY-1HAKG2L0NEG9H8QSNNZ5R40GDNGSLJTPXERSY6UVTSUFFLHEXZEES7RZLD7", "1", "a1", "a1qqqqqq"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		hrp, data, err := Decode(s)
		if err != nil {
			return
		}
		back, err := Encode(hrp, data)
		if err != nil {
			t.Fatalf("Encode(Decode(%q)): %v", s, err)
		}
		if !strings.EqualFold(back, s) {
			t.Fatalf("Encode(Decode(%q)) = %q", s, back)
		}
		hrp2, data2, err := Decode(back)
		if err != nil || !strings.EqualFold(hrp2, hrp) || !bytes.Equal(data, data2) {
			t.Fatalf("Decode(%q) = %q, %x, %v; want %x", back, hrp2, data2, err, data)
		}
	})
}
