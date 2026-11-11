package traffic

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// What an exchange keeps of its bodies (D358): each body or frame cut at body_max, credential members redacted first,
// never a header. Both legs use these.

// Redacted replaces every credential member's value.
const Redacted = `"[redacted]"`

// KeepBody is b as an exchange keeps it: redacted (Redact), then cut at max bytes; nil for an empty body.
func KeepBody(b []byte, max int) *Body {
	if len(b) == 0 || max <= 0 {
		return nil
	}
	return cut(Redact(b), int64(len(b)), max)
}

// KeepPrefix is a body as an exchange keeps it when only its first bytes were held: prefix (at most max bytes) of a
// body of size bytes, redacted.
func KeepPrefix(prefix []byte, size int64, max int) *Body {
	if size <= 0 || max <= 0 {
		return nil
	}
	return cut(Redact(prefix), size, max)
}

// cut keeps the redacted data of a body of size bytes as sent: all of it when the body fits in max (a redaction may
// make it a few bytes longer), else its first max bytes, marked truncated.
func cut(data []byte, size int64, max int) *Body {
	out := &Body{Size: size}
	if size > int64(max) {
		out.Truncated = true
		if len(data) > max {
			data = data[:max]
		}
	}
	out.Data = bytes.Clone(data)
	return out
}

// Redact replaces, in a JSON body, the value of every member the A2A specification defines as a credential: a push
// notification config's token (a member token of an object that has a url, the shape of 1.0's
// TaskPushNotificationConfig and 0.3's PushNotificationConfig) and an AuthenticationInfo's credentials (a member
// credentials of an object that is the value of an authentication member). Everything else is kept byte for byte. A
// body that is not JSON holds no such member and is returned as it is; one that looks like JSON but does not parse
// (cut, or broken) has every string value of a token or credentials member replaced instead, since its structure
// cannot be read.
func Redact(b []byte) []byte {
	t := bytes.TrimLeft(b, " \t\r\n")
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return b
	}
	var spans [][2]int
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := walk(dec, b, false, &spans); err != nil || dec.More() {
		return lenient(b)
	}
	if _, err := dec.Token(); err != io.EOF {
		return lenient(b)
	}
	if len(spans) == 0 {
		return b
	}
	var out bytes.Buffer
	last := 0
	for _, sp := range spans {
		if sp[0] < last { // inside a value already redacted
			continue
		}
		out.Write(b[last:sp[0]])
		out.WriteString(Redacted)
		last = sp[1]
	}
	out.Write(b[last:])
	return out.Bytes()
}

// walk reads one JSON value from dec, appending the byte spans of the values to redact. inAuth says the value is
// that of an authentication member.
func walk(dec *json.Decoder, src []byte, inAuth bool, spans *[][2]int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '[':
		for dec.More() {
			if err := walk(dec, src, false, spans); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '{':
		var token, creds [][2]int
		hasURL := false
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			start := valueStart(src, int(dec.InputOffset()))
			if err := walk(dec, src, key == "authentication", spans); err != nil {
				return err
			}
			end := int(dec.InputOffset())
			switch key {
			case "url":
				hasURL = true
			case "token":
				token = append(token, [2]int{start, end})
			case "credentials":
				if inAuth {
					creds = append(creds, [2]int{start, end})
				}
			}
		}
		if hasURL {
			*spans = append(*spans, token...)
		}
		*spans = append(*spans, creds...)
		sortSpans(*spans)
		_, err = dec.Token()
		return err
	}
	return nil
}

// valueStart is where a member's value begins: after the key's end at, past whitespace and the colon.
func valueStart(src []byte, at int) int {
	for at < len(src) && (src[at] == ' ' || src[at] == '\t' || src[at] == '\r' || src[at] == '\n' || src[at] == ':') {
		at++
	}
	return at
}

func sortSpans(s [][2]int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j][0] < s[j-1][0]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// lenientRE is a token or credentials member with a string value, possibly cut before its closing quote.
var lenientRE = regexp.MustCompile(`("(?:token|credentials)"\s*:\s*)"(?:[^"\\]|\\.)*("|\\?$)`)

func lenient(b []byte) []byte {
	return lenientRE.ReplaceAll(b, []byte(`${1}`+Redacted))
}

// Frames splits a stream's answer into its SSE events as it is written, keeping each event's data (its data: lines,
// joined) and id, redacted and cut at max bytes, and its offset from start; comment-only events (keep-alives) are not
// frames. At most FramesTimes × max bytes of frames are kept; later frames are counted in Dropped.
type Frames struct {
	start   time.Time
	max     int
	kept    int
	frames  []Frame
	dropped int

	cur     []byte // the current event, up to max plus room for its id line
	curSize int64
	at      time.Duration // when the current event began
	nl      bool          // the last byte was a newline
}

// NewFrames starts splitting a stream that began at start.
func NewFrames(start time.Time, max int) *Frames { return &Frames{start: start, max: max} }

// Write takes the next bytes of the stream; it never fails.
func (f *Frames) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\r' {
			continue
		}
		if c == '\n' && f.nl {
			f.finish()
			f.nl = false
			continue
		}
		if f.curSize == 0 {
			f.at = time.Since(f.start)
		}
		f.nl = c == '\n'
		f.curSize++
		if len(f.cur) < f.max+256 {
			f.cur = append(f.cur, c)
		}
	}
	return len(p), nil
}

// finish ends the current event.
func (f *Frames) finish() {
	defer func() { f.cur, f.curSize = f.cur[:0], 0 }()
	if f.curSize == 0 {
		return
	}
	var id string
	var data []string
	cutShort := int64(len(f.cur)) < f.curSize
	for _, line := range strings.Split(string(f.cur), "\n") {
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if data == nil {
		if cutShort { // an event whose data line began past what was kept
			f.dropped++
		}
		return
	}
	raw := []byte(strings.Join(data, "\n"))
	size := int64(len(raw))
	if cutShort { // the event was longer than what was kept: its size is the event's
		size = max(f.curSize, int64(f.max)+1)
	}
	body := cut(Redact(raw), size, f.max)
	if f.kept+len(body.Data) > FramesTimes*f.max {
		f.dropped++
		return
	}
	f.kept += len(body.Data)
	f.frames = append(f.frames, Frame{Offset: f.at, SSEID: id, Data: body.Data, Size: body.Size, Truncated: body.Truncated})
}

// Done ends the stream: an event left without its blank line is kept too. It returns the frames and how many were
// dropped.
func (f *Frames) Done() ([]Frame, int) {
	f.finish()
	return f.frames, f.dropped
}
