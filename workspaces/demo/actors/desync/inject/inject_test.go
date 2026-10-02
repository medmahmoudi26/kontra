package inject

import (
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

func testUnit(t *testing.T) unit.Exchange {
	t.Helper()
	u := unit.Exchange{
		URL: "https://api.example.com/v2/orders?id=7&lang=en",
		Request: unit.Message{
			Method: "POST", Proto: "HTTP/1.1",
			Headers: []unit.Header{
				{Name: "Host", Value: "api.example.com"},
				{Name: "Content-Type", Value: "application/json"},
				{Name: "X-Request-Id", Value: "abc123"},
				{Name: "Cookie", Value: "sid=deadbeef; theme=dark"},
			},
			Body: `{"note":"hello","qty":3}`,
		},
		Response: unit.Message{
			Status: 200,
			Headers: []unit.Header{
				{Name: "Content-Type", Value: "application/json"},
				{Name: "Access-Control-Allow-Headers", Value: "Content-Type, X-Tenant-Id, X-Api-Key"},
				{Name: "Vary", Value: "Origin, X-Region"},
				{Name: "Set-Cookie", Value: "csrf=zzz; Path=/"},
				{Name: "X-Served-By", Value: "edge-04"},
				{Name: "X-Trace", Value: "abc123"}, // echoes X-Request-Id
			},
		},
	}
	if err := u.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return u
}

func byID(pts []Point) map[string]Point {
	m := map[string]Point{}
	for _, p := range pts {
		m[p.ID] = p
	}
	return m
}

// TestResponseNamesPointsTheRequestNeverMentions is the whole reason the input is a unit
// and not a URL. Every header asserted here appears ONLY in the response.
func TestResponseNamesPointsTheRequestNeverMentions(t *testing.T) {
	u := testUnit(t)
	pts := byID(Enumerate(u, DefaultOptions()))

	for _, tc := range []struct {
		id  string
		src Source
		why string
	}{
		{"header:X-Tenant-Id:value", SrcRespACAH, "advertised in Access-Control-Allow-Headers"},
		{"header:X-Api-Key:value", SrcRespACAH, "advertised in Access-Control-Allow-Headers"},
		{"header:X-Region:value", SrcRespVary, "the cache keys on it"},
		{"header:X-Served-By:value", SrcRespOnlyHeader, "response-only, so proxy-set"},
		{"cookie:csrf", SrcRespSetCookie, "the app sets it, so it reads it back"},
	} {
		p, ok := pts[tc.id]
		if !ok {
			t.Errorf("missing point %s (%s)", tc.id, tc.why)
			continue
		}
		if p.Source != tc.src {
			t.Errorf("%s: source = %s, want %s", tc.id, p.Source, tc.src)
		}
	}

	// A request header whose value is echoed into a response header is a known-good
	// path from input to output.
	if p, ok := pts["header:X-Request-Id:value"]; !ok || p.Source != SrcRespEcho {
		t.Errorf("X-Request-Id should be flagged as echoed, got %+v (present=%v)", p.Source, ok)
	}
}

// TestResponseDerivedPointsComeFirst — they carry evidence that the app reads the header,
// where a request-derived point carries only the fact that we sent it. Under a point cap
// the evidence-backed ones must not be the ones dropped.
func TestResponseDerivedPointsComeFirst(t *testing.T) {
	pts := Enumerate(testUnit(t), DefaultOptions())
	if len(pts) < 5 {
		t.Fatalf("only %d points", len(pts))
	}
	for i, p := range pts[:5] {
		if !strings.HasPrefix(string(p.Source), "response:") {
			t.Fatalf("point %d (%s) is %s; response-derived points must sort first", i, p.ID, p.Source)
		}
	}
}

// TestRenderingMatchesContext. Percent-encoding in a header value arrives as seven ASCII
// characters and tests nothing; raw bytes in a query string are unescaped before anything
// sees them. Getting this wrong yields a scanner that is busy and blind.
func TestRenderingMatchesContext(t *testing.T) {
	pts := byID(Enumerate(testUnit(t), DefaultOptions()))
	for id, want := range map[string]Rendering{
		"path:suffix":              RenderPercent,
		"query:id:value":           RenderPercent,
		"header:X-Tenant-Id:value": RenderWire,
		"cookie:sid":               RenderPercent,
		"body:json:note":           RenderJSON,
		"request-line:version":     RenderWire,
	} {
		if p, ok := pts[id]; !ok {
			t.Errorf("missing point %s", id)
		} else if p.Render != want {
			t.Errorf("%s: render = %s, want %s", id, p.Render, want)
		}
	}
}

// TestApplyPutsTheRightBytesInTheRightPlace.
func TestApplyPutsTheRightBytesInTheRightPlace(t *testing.T) {
	u := testUnit(t)
	pts := byID(Enumerate(u, DefaultOptions()))
	var v vectors.Vector
	for _, c := range vectors.Generate(vectors.TierQuick, nil) {
		if c.Encoded == "%DC%8A" {
			v = c
		}
	}
	if v.Encoded == "" {
		t.Fatal("U+070A vector not generated")
	}

	// Path: the percent-encoded form, literally, in the request target.
	raw := string(Apply(u, pts["path:suffix"], Render(v, RenderPercent)))
	if !strings.Contains(raw, "/v2/orders%DC%8A?") {
		t.Errorf("path splice wrong:\n%s", firstLine(raw))
	}

	// Header: the raw bytes, because nothing will unescape them for us.
	raw = string(Apply(u, pts["header:X-Tenant-Id:value"], Render(v, RenderWire)))
	if !strings.Contains(raw, "X-Tenant-Id: 1\xdc\x8a") {
		t.Errorf("header splice wrong; want raw dc 8a bytes:\n%q", raw)
	}
	if strings.Contains(raw, "X-Tenant-Id: 1%DC%8A") {
		t.Error("header got the percent-encoded form, which tests nothing")
	}

	// JSON: the \u ESCAPE goes on the wire, not the decoded character. The server's
	// parser turns it back into U+070A and the narrowing happens downstream of that;
	// sending the decoded bytes instead would just be the wire rendering by another name.
	raw = string(Apply(u, pts["body:json:note"], Render(v, RenderJSON)))
	wantBody := `{"note":"hello\u070a","qty":3}`
	if !strings.Contains(raw, wantBody) {
		t.Errorf("json splice wrong:\n%s", raw)
	}
	// ...and the body's length must follow it.
	if !strings.Contains(raw, "Content-Length: "+itoa(len(wantBody))) {
		t.Errorf("Content-Length not recomputed (want %d) after a body mutation:\n%s", len(wantBody), raw)
	}

	// Cookie: only the named cookie moves.
	raw = string(Apply(u, pts["cookie:sid"], Render(v, RenderPercent)))
	if !strings.Contains(raw, "sid=deadbeef%DC%8A") || !strings.Contains(raw, "theme=dark") {
		t.Errorf("cookie splice wrong:\n%q", raw)
	}
}

// TestBaselineIsUnmodified — the zero point must produce a sendable request, since it is
// what the stability gate measures.
func TestBaselineIsUnmodified(t *testing.T) {
	u := testUnit(t)
	raw := string(Apply(u, Point{}, ""))
	if !strings.HasPrefix(raw, "POST /v2/orders?id=7&lang=en HTTP/1.1\r\n") {
		t.Fatalf("baseline request line wrong: %s", firstLine(raw))
	}
	if !strings.Contains(raw, "Connection: close") || !strings.Contains(raw, "Accept-Encoding: identity") {
		t.Error("transport headers not applied to the baseline")
	}
	if !strings.HasSuffix(raw, `{"note":"hello","qty":3}`) {
		t.Error("body lost")
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\r\n"); i > 0 {
		return s[:i]
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A REAL CAP, ON A REAL SHAPE. `hunt` used to send `max_points: 12`, and the question this
// pins is what survives that cut on a page with a lot of query string — which is most pages.
//
// Before `kindRank`, `SrcRequest` was one flat tier in enumeration order (path, query, headers),
// so twenty parameters consumed every remaining slot and not one header was probed. The
// response-derived points were already safe: `sourceRank` has always put them first.
func TestTheCapKeepsHeadersOnAQueryHeavyPage(t *testing.T) {
	u := testUnit(t)
	var q []string
	for i := 0; i < 20; i++ {
		q = append(q, "p"+string(rune('a'+i))+"=1")
	}
	u.Request.Target = "/v2/orders?" + strings.Join(q, "&")
	if err := u.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	const cap = 12
	pts := Enumerate(u, DefaultOptions())
	if len(pts) <= cap {
		t.Fatalf("fixture must over-fill the cap, got %d points", len(pts))
	}
	kept := pts[:cap] // exactly what main.go does

	// COUNT ON SOURCE **AND** KIND. The first spelling of this test counted every point whose
	// Kind was a header, which quietly includes the Vary / echo / Set-Cookie derived ones — those
	// are response-derived, `sourceRank` has always kept them, and the assertion passed with and
	// without the fix. The claim here is about headers the REQUEST carried.
	var headers, acah, query int
	for _, p := range kept {
		isHeader := p.Kind == KindHeaderValue || p.Kind == KindHeaderName
		switch {
		case p.Source == SrcRespACAH:
			acah++
		case p.Source == SrcRequest && isHeader:
			headers++
		case p.Source == SrcRequest && (p.Kind == KindQueryValue || p.Kind == KindQueryName):
			query++
		}
	}
	if acah == 0 {
		t.Error("no Access-Control-Allow-Headers point survived the cap — the app TOLD us it reads those")
	}
	if headers == 0 {
		t.Errorf("no request-header point survived the cap (headers=%d query=%d): a header value "+
			"reaches a backend request line far more often than a query parameter", headers, query)
	}
	if query > headers {
		t.Errorf("query points (%d) outrank header points (%d) inside the cap", query, headers)
	}
}

// The proven vector is not traded away for the promising one: every confirmed voapi.8x8.com
// bypass was a percent-encoded codepoint in the PATH.
func TestPathSuffixOutranksHeadersWithinTheRequestTier(t *testing.T) {
	pts := Enumerate(testUnit(t), DefaultOptions())
	var suffixAt, headerAt = -1, -1
	for i, p := range pts {
		if p.Source != SrcRequest {
			continue
		}
		if suffixAt < 0 && p.Kind == KindPathSuffix {
			suffixAt = i
		}
		if headerAt < 0 && (p.Kind == KindHeaderValue || p.Kind == KindHeaderName) {
			headerAt = i
		}
	}
	if suffixAt < 0 || headerAt < 0 {
		t.Fatalf("fixture lost a point kind: suffix=%d header=%d", suffixAt, headerAt)
	}
	if suffixAt > headerAt {
		t.Errorf("path:suffix at %d is behind the first header at %d", suffixAt, headerAt)
	}
}
