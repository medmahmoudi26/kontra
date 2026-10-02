// Package unit is the input contract: one crawled HTTP exchange — a request AND the
// response it produced.
//
// WHY BOTH HALVES. The request tells you where you can inject. The response tells you
// where you SHOULD, and it names injection points that appear nowhere in the request:
//
//	Access-Control-Allow-Headers  the headers the application is willing to accept from
//	                              a browser — i.e. headers it actually reads. A static
//	                              wordlist cannot know that this host takes X-Tenant-Id.
//	Vary                          the headers the cache keys on, which is the difference
//	                              between poisoning one response and poisoning everyone's.
//	Set-Cookie                    cookie names the application reads back on the next hit.
//	response-only header names    a header the server emits but the client never sent is
//	                              usually one its own proxy layer sets — and proxies that
//	                              write a header commonly also read it.
//	echoed values                 a response header carrying a request header's value is a
//	                              reflection point with a known path to the output.
//
// This is the structural advantage a crawler-fed scanner has over a generic one, and it
// is why the input is a unit rather than a URL.
package unit

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Header is one header line, in wire order with case preserved. A map would lose both,
// and both are load-bearing: order fingerprints the emitting stack, and case survives or
// does not survive a proxy rewrite.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Message is one half of a unit. Supply Raw (base64 of the wire bytes) or the structured
// fields; Parse fills in whichever is missing.
type Message struct {
	Raw string `json:"raw,omitempty"` // base64 of exact wire bytes

	Method  string   `json:"method,omitempty"`
	Target  string   `json:"target,omitempty"` // request-target exactly as it appears on the wire
	Proto   string   `json:"proto,omitempty"`
	Status  int      `json:"status,omitempty"`
	Headers []Header `json:"headers,omitempty"`
	Body    string   `json:"body,omitempty"`
}

// Exchange is one crawled exchange.
type Exchange struct {
	ID       string  `json:"id,omitempty"`
	URL      string  `json:"url"` // absolute; supplies scheme, host and port
	Request  Message `json:"request"`
	Response Message `json:"response"`
	Notes    string  `json:"notes,omitempty"`
}

// Get returns the first value of a header, case-insensitively.
func (m Message) Get(name string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// Names returns every header name in wire order.
func (m Message) Names() []string {
	out := make([]string, 0, len(m.Headers))
	for _, h := range m.Headers {
		out = append(out, h.Name)
	}
	return out
}

// Has reports whether the message carries the named header.
func (m Message) Has(name string) bool {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			return true
		}
	}
	return false
}

// Normalize fills structured fields from Raw (or Raw from the structured fields) and
// applies defaults, so everything downstream can assume both forms are present.
func (u *Exchange) Normalize() error {
	if err := u.Request.parseRaw(true); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := u.Response.parseRaw(false); err != nil {
		return fmt.Errorf("response: %w", err)
	}
	if u.Request.Method == "" {
		u.Request.Method = "GET"
	}
	if u.Request.Proto == "" {
		u.Request.Proto = "HTTP/1.1"
	}
	if u.URL == "" {
		return fmt.Errorf("unit has no url")
	}
	pu, err := url.Parse(u.URL)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Request.Target == "" {
		t := pu.EscapedPath()
		if t == "" {
			t = "/"
		}
		if pu.RawQuery != "" {
			t += "?" + pu.RawQuery
		}
		u.Request.Target = t
	}
	if !u.Request.Has("Host") {
		u.Request.Headers = append([]Header{{Name: "Host", Value: pu.Host}}, u.Request.Headers...)
	}
	return nil
}

// Scheme, Hostname and Port describe where to send the mutated request.
func (u Exchange) Scheme() string { p, _ := url.Parse(u.URL); return p.Scheme }
func (u Exchange) Hostname() string {
	p, _ := url.Parse(u.URL)
	return p.Hostname()
}
func (u Exchange) Port() int {
	p, _ := url.Parse(u.URL)
	if s := p.Port(); s != "" {
		n, _ := strconv.Atoi(s)
		return n
	}
	if p.Scheme == "http" {
		return 80
	}
	return 443
}

func (m *Message) parseRaw(isRequest bool) error {
	if m.Raw == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(m.Raw)
	if err != nil {
		// Tolerate a literal wire string, which is what a hand-written unit contains.
		raw = []byte(m.Raw)
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("empty message")
	}
	f := strings.Fields(strings.TrimRight(line, "\r\n"))
	if isRequest {
		if len(f) > 0 && m.Method == "" {
			m.Method = f[0]
		}
		if len(f) > 1 && m.Target == "" {
			m.Target = f[1]
		}
		if len(f) > 2 && m.Proto == "" {
			m.Proto = f[2]
		}
	} else {
		if len(f) > 0 && m.Proto == "" {
			m.Proto = f[0]
		}
		if len(f) > 1 && m.Status == 0 {
			m.Status, _ = strconv.Atoi(f[1])
		}
	}
	if len(m.Headers) == 0 {
		for {
			h, err := br.ReadString('\n')
			if err != nil && h == "" {
				break
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" {
				break
			}
			name, val, ok := strings.Cut(h, ":")
			if !ok {
				continue
			}
			m.Headers = append(m.Headers, Header{Name: name, Value: strings.TrimLeft(val, " ")})
		}
	}
	if m.Body == "" {
		rest, _ := io.ReadAll(br)
		m.Body = string(rest)
	}
	return nil
}
