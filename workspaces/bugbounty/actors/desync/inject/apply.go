package inject

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

// Render returns the form of a vector appropriate to a point's context. Sending the
// percent-encoded form into a header value tests nothing — it arrives as seven ASCII
// characters and no layer will ever unescape it.
func Render(v vectors.Vector, r Rendering) string { return render(v.Encoded, v.JSONEsc, r) }

// RenderControl does the same for the vector's matched control, so both arms of the
// differential are delivered through the identical mechanism.
func RenderControl(v vectors.Vector, r Rendering) string {
	return render(v.Control, jsonEscapeOf(decodePercent(v.Control)), r)
}

func render(encoded, jsonEsc string, r Rendering) string {
	switch r {
	case RenderWire:
		return string(decodePercent(encoded))
	case RenderJSON:
		return jsonEsc
	default:
		return encoded
	}
}

// Apply splices payload into the unit at the point and returns exact wire bytes.
//
// The request is serialised by hand rather than through net/http because the payload is
// frequently not valid in the field it lands in — that is the entire experiment. Anything
// that normalises, reorders or validates on the way out destroys the test silently.
func Apply(u unit.Exchange, p Point, payload string) []byte {
	method, proto := u.Request.Method, u.Request.Proto
	path, query := splitTarget(u.Request.Target)
	body := u.Request.Body
	headers := make([]unit.Header, len(u.Request.Headers))
	copy(headers, u.Request.Headers)

	setHeader := func(name, value string) {
		for i := range headers {
			if strings.EqualFold(headers[i].Name, name) {
				headers[i].Value = value
				return
			}
		}
		headers = append(headers, unit.Header{Name: name, Value: value})
	}
	getHeader := func(name string) (string, bool) {
		for _, h := range headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value, true
			}
		}
		return "", false
	}

	switch p.Kind {
	case KindPathSuffix:
		path += payload

	case KindPathSegment:
		segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if p.Index < len(segs) {
			segs[p.Index] += payload
			path = "/" + strings.Join(segs, "/")
		}

	case KindQueryValue:
		query = mapQuery(query, p.Name, func(k, v string) (string, string) { return k, v + payload })

	case KindQueryName:
		query = mapQuery(query, p.Name, func(k, v string) (string, string) { return k + payload, v })

	case KindHeaderValue:
		// A point derived from the response names a header the request never sent. Add
		// it: the evidence that the app reads it came from the response, not from us.
		base, ok := getHeader(p.Name)
		if !ok {
			base = "1"
		}
		setHeader(p.Name, base+payload)

	case KindHeaderName:
		for i := range headers {
			if strings.EqualFold(headers[i].Name, p.Name) {
				headers[i].Name = p.Name + payload
				break
			}
		}

	case KindCookieValue:
		cur, ok := getHeader("Cookie")
		if !ok {
			setHeader("Cookie", p.Name+"=1"+payload)
			break
		}
		parts := strings.Split(cur, ";")
		hit := false
		for i, c := range parts {
			name, val, _ := strings.Cut(strings.TrimSpace(c), "=")
			if name == p.Name {
				parts[i] = " " + name + "=" + val + payload
				hit = true
			}
		}
		if !hit {
			parts = append(parts, " "+p.Name+"=1"+payload)
		}
		setHeader("Cookie", strings.TrimSpace(strings.Join(parts, ";")))

	case KindBodyForm:
		body = mapQuery(body, p.Name, func(k, v string) (string, string) { return k, v + payload })

	case KindBodyJSON:
		body = appendJSONString(body, p.Name, payload)

	case KindMethod:
		method += payload

	case KindVersion:
		proto += payload
	}

	target := path
	if query != "" {
		target += "?" + query
	}

	// Transport headers are ours, not the unit's: identity encoding keeps bodies
	// comparable between arms, and close keeps one probe per connection so a
	// contamination signal cannot leak across samples.
	setHeader("Connection", "close")
	setHeader("Accept-Encoding", "identity")
	if body != "" || u.Request.Has("Content-Length") {
		setHeader("Content-Length", strconv.Itoa(len(body)))
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s %s\r\n", method, target, proto)
	for _, h := range headers {
		if strings.EqualFold(h.Name, "Transfer-Encoding") {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h.Name, h.Value)
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}

// mapQuery rewrites the named key/value pair of an &-separated list, preserving order and
// leaving every other pair byte-identical.
func mapQuery(q, name string, f func(k, v string) (string, string)) string {
	if q == "" {
		return q
	}
	parts := strings.Split(q, "&")
	for i, kv := range parts {
		k, v, had := strings.Cut(kv, "=")
		if k != name {
			continue
		}
		nk, nv := f(k, v)
		if had {
			parts[i] = nk + "=" + nv
		} else {
			parts[i] = nk + nv
		}
	}
	return strings.Join(parts, "&")
}

// appendJSONString appends payload inside the string value of key. The payload is already
// a JSON escape sequence, so it goes in literally.
func appendJSONString(body, key, payload string) string {
	needle := `"` + key + `"`
	i := strings.Index(body, needle)
	if i < 0 {
		return body
	}
	j := i + len(needle)
	for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == ':') {
		j++
	}
	if j >= len(body) || body[j] != '"' {
		return body
	}
	k := j + 1
	for k < len(body) && body[k] != '"' {
		if body[k] == '\\' {
			k++
		}
		k++
	}
	if k >= len(body) {
		return body
	}
	return body[:k] + payload + body[k:]
}

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

func jsonEscapeOf(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		fmt.Fprintf(&sb, "\\u%04x", c)
	}
	return sb.String()
}
