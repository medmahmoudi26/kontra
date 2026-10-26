// Package bech32 is BIP-173 Bech32 as age uses it: no 90-character limit (an age1pq1… recipient is 1,959
// characters), a lowercase or an uppercase string but never a mixed-case one, and the checksum computed
// over the lowercase form. age keeps its own codec internal (filippo.io/age/internal/bech32), so the
// tunnel's key id and recipient handling (spec/tunnel.md §2.2) and the identity file's raw scalar
// (internal/nodekey, §2.1) decode here.
package bech32

import (
	"errors"
	"fmt"
	"strings"
)

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var generator = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

// checksumLen is the six 5-bit groups of the checksum.
const checksumLen = 6

func polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range 5 {
			if top>>i&1 == 1 {
				chk ^= generator[i]
			}
		}
	}
	return chk
}

// hrpExpand expands an already lowercase human-readable part.
func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, 2*len(hrp)+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint
	maxv := uint32(1)<<to - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for i, v := range data {
		if uint32(v)>>from != 0 {
			return nil, fmt.Errorf("invalid data range: data[%d]=%d", i, v)
		}
		acc = acc<<from | uint32(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	switch {
	case pad:
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	case bits >= from:
		return nil, errors.New("illegal zero padding")
	case acc<<(to-bits)&maxv != 0:
		return nil, errors.New("non-zero padding")
	}
	return out, nil
}

func checkHRP(hrp string) error {
	if hrp == "" {
		return errors.New("empty human-readable part")
	}
	for i := 0; i < len(hrp); i++ {
		if c := hrp[i]; c < 33 || c > 126 {
			return fmt.Errorf("invalid character in human-readable part: hrp[%d]=%d", i, c)
		}
	}
	return nil
}

// Encode is the Bech32 string of hrp and data: lowercase, or uppercase when hrp is uppercase (age's
// AGE-SECRET-KEY-1…). A mixed-case hrp is refused.
func Encode(hrp string, data []byte) (string, error) {
	if err := checkHRP(hrp); err != nil {
		return "", err
	}
	lower := strings.ToLower(hrp)
	upper := strings.ToUpper(hrp)
	if hrp != lower && hrp != upper {
		return "", fmt.Errorf("mixed-case human-readable part %q", hrp)
	}
	values, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	sum := polymod(append(append(hrpExpand(lower), values...), make([]byte, checksumLen)...)) ^ 1
	var b strings.Builder
	b.Grow(len(hrp) + 1 + len(values) + checksumLen)
	b.WriteString(lower)
	b.WriteByte('1')
	for _, v := range values {
		b.WriteByte(charset[v])
	}
	for i := range checksumLen {
		b.WriteByte(charset[sum>>(5*(5-i))&31])
	}
	if hrp == lower {
		return b.String(), nil
	}
	return strings.ToUpper(b.String()), nil
}

// Decode splits a Bech32 string at its last '1' and returns the human-readable part as written (so an
// uppercase string gives an uppercase hrp) and the data. A mixed-case string, a character outside the
// alphabet, a bad checksum or non-zero padding is an error. There is no length limit.
func Decode(s string) (hrp string, data []byte, err error) {
	if strings.ToLower(s) != s && strings.ToUpper(s) != s {
		return "", nil, errors.New("mixed case")
	}
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+1+checksumLen > len(s) {
		return "", nil, fmt.Errorf("separator '1' at an invalid position (%d of %d)", pos, len(s))
	}
	hrp = s[:pos]
	if err := checkHRP(hrp); err != nil {
		return "", nil, err
	}
	values := make([]byte, 0, len(s)-pos-1)
	for i := pos + 1; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		d := strings.IndexByte(charset, c)
		if d < 0 {
			return "", nil, fmt.Errorf("invalid character in data part at %d", i)
		}
		values = append(values, byte(d))
	}
	if polymod(append(hrpExpand(strings.ToLower(hrp)), values...)) != 1 {
		return "", nil, errors.New("invalid checksum")
	}
	data, err = convertBits(values[:len(values)-checksumLen], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, data, nil
}
