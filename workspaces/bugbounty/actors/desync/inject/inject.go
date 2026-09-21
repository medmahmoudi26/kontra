// Package inject enumerates where a payload can go in a unit, and splices it in.
//
// The path is one injection point out of many, and not the most interesting one. The
// points that matter most are named by the RESPONSE, not the request: a header the
// application advertises in Access-Control-Allow-Headers is one it actually reads, and no
// static wordlist can know that this host takes X-Tenant-Id.
//
// DELIVERY DIFFERS BY CONTEXT, which is why every point carries a rendering:
//
//	percent  URL contexts — path, query, form bodies, cookies. The server unescapes,
//	         and the narrowing conversion runs on what it unescaped.
//	wire     header values and names. Nothing URL-decodes a header for you, so the
//	         multi-byte sequence goes on the wire literally.
//	json     JSON string values. The parser turns "܊" into U+070A, and the
//	         narrowing happens downstream of the parser.
//
// Sending %DC%8A into a header value tests nothing: it arrives as seven ASCII characters.
// Sending the bytes DC 8A into a query string usually tests nothing either, because the
// server unescapes first and never sees them as an escape. Getting this wrong produces a
// scanner that is busy and blind.
package inject

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
)

type Kind string

const (
	KindPathSuffix  Kind = "path_suffix"
	KindPathSegment Kind = "path_segment"
	KindQueryValue  Kind = "query_value"
	KindQueryName   Kind = "query_name"
	KindHeaderValue Kind = "header_value"
	KindHeaderName  Kind = "header_name"
	KindCookieValue Kind = "cookie_value"
	KindBodyForm    Kind = "body_form"
	KindBodyJSON    Kind = "body_json"
	KindMethod      Kind = "method"
	KindVersion     Kind = "version"
)

type Rendering string

const (
	RenderPercent Rendering = "percent"
	RenderWire    Rendering = "wire"
	RenderJSON    Rendering = "json"
)

// Source records WHY a point is being probed. It is the provenance that makes a finding
// explainable — "the response told us the app accepts this header" is a different claim
// from "we guessed".
type Source string

const (
	SrcRequest        Source = "request"
	SrcRespACAH       Source = "response:access-control-allow-headers"
	SrcRespVary       Source = "response:vary"
	SrcRespSetCookie  Source = "response:set-cookie"
	SrcRespOnlyHeader Source = "response:header-absent-from-request"
	SrcRespEcho       Source = "response:echoes-a-request-header"
)

// Point is one place a payload can be spliced.
type Point struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Name      string    `json:"name,omitempty"`
	Index     int       `json:"index,omitempty"`
	Source    Source    `json:"source"`
	Render    Rendering `json:"render"`
	Rationale string    `json:"rationale,omitempty"`
}

// hopByHop are headers whose mutation breaks the transport rather than testing the
// application, so they are never injection points.
var hopByHop = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true,
	"connection": true, "accept-encoding": true, "upgrade": true,
}

type Options struct {
	MaxPathSegments int
	MaxHeaders      int
	MaxParams       int
	IncludeNames    bool // also mutate header/param NAMES, not just values
}

func DefaultOptions() Options {
	return Options{MaxPathSegments: 3, MaxHeaders: 24, MaxParams: 8, IncludeNames: true}
}

// Enumerate returns the injection points for a unit, request-derived first, then the ones
// only the response could have told us about.
func Enumerate(u unit.Exchange, o Options) []Point {
	var pts []Point
	at := map[string]int{}
	// A point can be discovered twice — X-Request-Id is in the request AND its value is
	// echoed into a response header. On collision the stronger provenance wins: dropping
	// the second sighting silently downgrades "the response proves the app reads this"
	// to "we sent it", and provenance is what decides which points survive the cap.
	add := func(p Point) {
		if p.ID == "" {
			return
		}
		if i, dup := at[p.ID]; dup {
			if sourceRank(p.Source) < sourceRank(pts[i].Source) {
				pts[i].Source = p.Source
				pts[i].Rationale = p.Rationale
			}
			return
		}
		at[p.ID] = len(pts)
		pts = append(pts, p)
	}

	path, query := splitTarget(u.Request.Target)

	add(Point{ID: "path:suffix", Kind: KindPathSuffix, Source: SrcRequest, Render: RenderPercent,
		Rationale: "appended to the path — the classic secondary-context gadget"})

	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, s := range segs {
		if i >= o.MaxPathSegments || s == "" {
			break
		}
		add(Point{ID: fmt.Sprintf("path:seg%d", i), Kind: KindPathSegment, Index: i,
			Source: SrcRequest, Render: RenderPercent,
			Rationale: "a routed segment may be reforwarded into a backend request line"})
	}

	for i, kv := range splitQuery(query) {
		if i >= o.MaxParams {
			break
		}
		add(Point{ID: "query:" + kv + ":value", Kind: KindQueryValue, Name: kv,
			Source: SrcRequest, Render: RenderPercent})
		if o.IncludeNames {
			add(Point{ID: "query:" + kv + ":name", Kind: KindQueryName, Name: kv,
				Source: SrcRequest, Render: RenderPercent})
		}
	}

	// ---- request headers -----------------------------------------------------------
	n := 0
	for _, h := range u.Request.Headers {
		if hopByHop[strings.ToLower(h.Name)] || n >= o.MaxHeaders {
			continue
		}
		n++
		if strings.EqualFold(h.Name, "Cookie") {
			for i, c := range strings.Split(h.Value, ";") {
				name, _, _ := strings.Cut(strings.TrimSpace(c), "=")
				if name == "" || i >= o.MaxParams {
					continue
				}
				add(Point{ID: "cookie:" + name, Kind: KindCookieValue, Name: name,
					Source: SrcRequest, Render: RenderPercent,
					Rationale: "cookie values are commonly URL-decoded by the framework"})
			}
			continue
		}
		add(Point{ID: "header:" + h.Name + ":value", Kind: KindHeaderValue, Name: h.Name,
			Source: SrcRequest, Render: RenderWire})
		if o.IncludeNames {
			add(Point{ID: "header:" + h.Name + ":name", Kind: KindHeaderName, Name: h.Name,
				Source: SrcRequest, Render: RenderWire})
		}
	}

	// ---- response-derived: the points nothing else can find -------------------------
	//
	// Order matters: these carry the highest prior, because the response is evidence
	// that the application reads the header, not a guess that it might.
	for _, name := range splitList(u.Response.Get("Access-Control-Allow-Headers")) {
		if hopByHop[strings.ToLower(name)] || name == "*" {
			continue
		}
		add(Point{ID: "header:" + name + ":value", Kind: KindHeaderValue, Name: name,
			Source: SrcRespACAH, Render: RenderWire,
			Rationale: "the app advertises that it accepts this header, so it reads it"})
	}
	for _, name := range splitList(u.Response.Get("Vary")) {
		if hopByHop[strings.ToLower(name)] || name == "*" {
			continue
		}
		add(Point{ID: "header:" + name + ":value", Kind: KindHeaderValue, Name: name,
			Source: SrcRespVary, Render: RenderWire,
			Rationale: "the cache keys on this header — poisoning here reaches other users"})
	}
	for _, name := range splitList(u.Response.Get("Access-Control-Expose-Headers")) {
		if hopByHop[strings.ToLower(name)] || name == "*" {
			continue
		}
		add(Point{ID: "header:" + name + ":value", Kind: KindHeaderValue, Name: name,
			Source: SrcRespACAH, Render: RenderWire, Rationale: "exposed to script, so read by the app"})
	}
	for _, h := range u.Response.Headers {
		if strings.EqualFold(h.Name, "Set-Cookie") {
			if name, _, ok := strings.Cut(h.Value, "="); ok {
				add(Point{ID: "cookie:" + strings.TrimSpace(name), Kind: KindCookieValue,
					Name: strings.TrimSpace(name), Source: SrcRespSetCookie, Render: RenderPercent,
					Rationale: "the app sets this cookie, so it reads it back"})
			}
			continue
		}
		if hopByHop[strings.ToLower(h.Name)] || isBoilerplate(h.Name) {
			continue
		}
		// A header the server emits but the client never sent is usually written by its
		// own proxy layer — and a proxy that writes a header commonly also reads it.
		if !u.Request.Has(h.Name) {
			add(Point{ID: "header:" + h.Name + ":value", Kind: KindHeaderValue, Name: h.Name,
				Source: SrcRespOnlyHeader, Render: RenderWire,
				Rationale: "present in the response, absent from the request — likely proxy-set"})
		}
		// A response header carrying a request header's value is a known-good path from
		// input to output.
		for _, rh := range u.Request.Headers {
			if rh.Value != "" && rh.Value == h.Value && !strings.EqualFold(rh.Name, h.Name) {
				add(Point{ID: "header:" + rh.Name + ":value", Kind: KindHeaderValue, Name: rh.Name,
					Source: SrcRespEcho, Render: RenderWire,
					Rationale: "its value is echoed into response header " + h.Name})
			}
		}
	}

	// ---- body ----------------------------------------------------------------------
	ct := strings.ToLower(u.Request.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "json") && u.Request.Body != "":
		for i, k := range jsonStringKeys(u.Request.Body) {
			if i >= o.MaxParams {
				break
			}
			add(Point{ID: "body:json:" + k, Kind: KindBodyJSON, Name: k,
				Source: SrcRequest, Render: RenderJSON,
				Rationale: "the JSON parser decodes \\uXXXX before the narrowing runs"})
		}
	case strings.Contains(ct, "form-urlencoded") && u.Request.Body != "":
		for i, kv := range splitQuery(u.Request.Body) {
			if i >= o.MaxParams {
				break
			}
			add(Point{ID: "body:form:" + kv, Kind: KindBodyForm, Name: kv,
				Source: SrcRequest, Render: RenderPercent})
		}
	}

	add(Point{ID: "request-line:method", Kind: KindMethod, Source: SrcRequest, Render: RenderWire,
		Rationale: "some stacks copy the method into a backend request line unvalidated"})
	add(Point{ID: "request-line:version", Kind: KindVersion, Source: SrcRequest, Render: RenderWire,
		Rationale: "PortSwigger's status-line injection: the protocol string copied verbatim"})

	// TWO KEYS, AND THE SECOND ONE IS WHAT THE CAP ACTUALLY EATS. `sourceRank` alone put every
	// response-derived point in front, which is right — but it leaves `SrcRequest` as one flat
	// tier held in ENUMERATION order, and that order is path, then query, then headers. At
	// `max_points: 12` a host with a handful of query parameters therefore spent its whole
	// request-derived budget before reaching a single header, and a header value is copied into a
	// backend request line far more often than a query parameter is.
	//
	// `kindRank` orders that tier. Ties fall back to enumeration order because the sort is
	// STABLE, which is what keeps `path:seg0` ahead of `path:seg1`.
	sort.SliceStable(pts, func(i, j int) bool {
		if a, b := sourceRank(pts[i].Source), sourceRank(pts[j].Source); a != b {
			return a < b
		}
		return kindRank(pts[i].Kind) < kindRank(pts[j].Kind)
	})
	return pts
}

// kindRank breaks a tie inside one provenance tier, and the order is the measured one.
//
// `path_suffix` stays FIRST and that is deliberate: the URL-decode-and-reforward gadget in the
// path is the vector this scanner has actually confirmed — every voapi.8x8.com bypass was a
// percent-encoded codepoint in the path, not in a header. Demoting it to make room for headers
// would trade a proven vector for a promising one.
//
// Headers come next, ahead of path SEGMENTS and query. A segment is only reachable when routing
// copies it, and a query parameter is the thing generic scanners already fuzz; a per-target
// header name is the half a wordlist cannot know.
func kindRank(k Kind) int {
	switch k {
	case KindPathSuffix:
		return 0
	case KindHeaderValue, KindHeaderName:
		return 1
	case KindPathSegment:
		return 2
	case KindCookieValue:
		return 3
	case KindQueryValue, KindQueryName:
		return 4
	case KindBodyForm, KindBodyJSON:
		return 5
	default: // KindMethod, KindVersion — the request line, cheapest to reach and rarely routed
		return 6
	}
}

// sourceRank puts response-derived points first: they carry evidence, not a guess.
func sourceRank(s Source) int {
	switch s {
	case SrcRespACAH:
		return 0
	case SrcRespVary:
		return 1
	case SrcRespEcho:
		return 2
	case SrcRespSetCookie:
		return 3
	case SrcRespOnlyHeader:
		return 4
	default:
		return 5
	}
}

// isBoilerplate filters response headers that are never application input.
func isBoilerplate(name string) bool {
	switch strings.ToLower(name) {
	case "date", "content-type", "content-encoding", "server", "expires", "age",
		"last-modified", "etag", "cache-control", "pragma", "vary", "location",
		"strict-transport-security", "content-security-policy", "x-content-type-options",
		"x-frame-options", "x-xss-protection", "accept-ranges", "alt-svc", "report-to",
		"nel", "cf-ray", "cf-cache-status", "access-control-allow-origin",
		"access-control-allow-credentials", "access-control-allow-methods":
		return true
	}
	return false
}

func splitTarget(t string) (path, query string) {
	if i := strings.IndexByte(t, '?'); i >= 0 {
		return t[:i], t[i+1:]
	}
	return t, ""
}

func splitQuery(q string) []string {
	var out []string
	for _, kv := range strings.Split(q, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// jsonStringKeys finds top-level keys whose value is a string. Deliberately a scanner,
// not a parser: the body may be malformed, and a key list is all that is needed.
func jsonStringKeys(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(body) && body[j] != '"' {
			if body[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(body) {
			break
		}
		key := body[i+1 : j]
		k := j + 1
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if k < len(body) && body[k] == ':' {
			v := k + 1
			for v < len(body) && (body[v] == ' ' || body[v] == '\t') {
				v++
			}
			if v < len(body) && body[v] == '"' && key != "" {
				out = append(out, key)
			}
		}
		i = j
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func quoteInt(n int) string { return strconv.Itoa(n) }
