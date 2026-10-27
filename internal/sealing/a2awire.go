package sealing

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
)

// The A2A 1.0 part codec both nodes seal and open with (spec/ext-sealed.md §2, §3): a part's content is the one
// field of text, raw, url or data it carries; mediaType says what it is; filename and metadata stay outside the
// envelope (PRD §8, the lifecycle reads metadata). A part with anything else (an A2A 0.3 file, a member this
// version does not know) is not sealed or opened: its content could not be told apart from what is outside.

// plainOf is a plaintext part's sealed content: the header's kind and mediaType, and the body.
type plainOf struct {
	kind      sealed.Kind
	mediaType string
	body      []byte
	hash      string // sha256 of kind, mediaType and body: an artifact's unchanged parts are found by it
}

// partMembers are the members of an A2A 1.0 part.
var partMembers = map[string]bool{"text": true, "raw": true, "url": true, "data": true, "mediaType": true,
	"filename": true, "metadata": true}

// plainOfPart reads a plaintext part's content (bad_header for one that is not an A2A 1.0 part with exactly one of
// text, raw, url and data).
func plainOfPart(p a2awire.Part) (plainOf, error) {
	bad := func(why string) (plainOf, error) {
		return plainOf{}, reject(sealed.RejectBadHeader, "a part that is not an A2A 1.0 part: "+why)
	}
	var pl plainOf
	n := 0
	for k, v := range p {
		if !partMembers[k] {
			return bad("member " + strictjson.Quote(k))
		}
		switch k {
		case "text", "url":
			var s string
			if firstByte(v) != '"' || json.Unmarshal(v, &s) != nil {
				return bad(k + " is not a string")
			}
			pl.kind, pl.body = sealed.Kind(k), []byte(s)
			n++
		case "raw":
			var s string
			if firstByte(v) != '"' || json.Unmarshal(v, &s) != nil {
				return bad("raw is not a string")
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return bad("raw is not base64")
			}
			pl.kind, pl.body = sealed.KindRaw, b
			n++
		case "data":
			b, err := jcs.Transform(v)
			if err != nil {
				return bad("data has no canonical form")
			}
			pl.kind, pl.body = sealed.KindData, b
			n++
		case "mediaType":
			if firstByte(v) != '"' || json.Unmarshal(v, &pl.mediaType) != nil {
				return bad("mediaType is not a string")
			}
		case "filename":
			var s string
			if firstByte(v) != '"' || json.Unmarshal(v, &s) != nil {
				return bad("filename is not a string")
			}
		}
	}
	if n != 1 {
		return bad("not exactly one of text, raw, url and data")
	}
	for i := 0; i < len(pl.mediaType); i++ {
		if pl.mediaType[i] >= 0x80 {
			return bad("a mediaType that is not ASCII") // the header is ASCII only
		}
	}
	h := sha256.New()
	h.Write([]byte(pl.kind))
	h.Write([]byte{0})
	h.Write([]byte(pl.mediaType))
	h.Write([]byte{0})
	h.Write(pl.body)
	pl.hash = hex.EncodeToString(h.Sum(nil))
	return pl, nil
}

// sealedPartOf is the sealed part for ciphertext ct of orig's content (§3): raw ciphertext, the envelope type, the
// original filename, and the original metadata with "kontra.sealed": {"original": <mediaType, or the kind>}.
func sealedPartOf(ct []byte, orig a2awire.Part, pl plainOf) a2awire.Part {
	out := a2awire.Part{
		"raw":       a2awire.Quote(base64.StdEncoding.EncodeToString(ct)),
		"mediaType": a2awire.Quote(sealed.MediaType),
	}
	if fn, ok := orig["filename"]; ok {
		out["filename"] = fn
	}
	md := map[string]json.RawMessage{}
	if raw, ok := orig["metadata"]; ok {
		if m, err := strictjson.Members(raw); err == nil {
			for k, v := range m {
				md[k] = v
			}
		}
	}
	original := pl.mediaType
	if original == "" {
		original = string(pl.kind)
	}
	md[sealed.MetadataKey] = a2awire.Encode(map[string]json.RawMessage{"original": a2awire.Quote(original)})
	out["metadata"] = a2awire.Encode(md)
	return out
}

// plainPart is the plaintext part a sealed part opens to: the header's kind and mediaType with the body, the sealed
// part's filename, and its metadata without "kontra.sealed" (absent when nothing else is left).
func plainPart(hd sealed.Header, body []byte, sp a2awire.Part) (a2awire.Part, error) {
	out := a2awire.Part{}
	switch hd.Kind {
	case sealed.KindText, sealed.KindURL:
		if !utf8.Valid(body) {
			return nil, reject(sealed.RejectBadHeader, "a text or url body that is not UTF-8")
		}
		out[string(hd.Kind)] = a2awire.Quote(string(body))
	case sealed.KindRaw:
		out["raw"] = a2awire.Quote(base64.StdEncoding.EncodeToString(body))
	case sealed.KindData:
		if !json.Valid(body) {
			return nil, reject(sealed.RejectBadHeader, "a data body that is not JSON")
		}
		var c bytes.Buffer
		if err := json.Compact(&c, body); err != nil {
			return nil, reject(sealed.RejectBadHeader, "a data body that is not JSON")
		}
		out["data"] = c.Bytes()
	default:
		return nil, reject(sealed.RejectBadHeader, "an unknown kind")
	}
	if hd.MediaType != "" {
		out["mediaType"] = a2awire.Quote(hd.MediaType)
	}
	if fn, ok := sp["filename"]; ok {
		out["filename"] = fn
	}
	if raw, ok := sp["metadata"]; ok {
		if m, err := strictjson.Members(raw); err == nil {
			delete(m, sealed.MetadataKey)
			if len(m) > 0 {
				out["metadata"] = a2awire.Encode(m)
			}
		}
	}
	return out, nil
}

func firstByte(raw []byte) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c
	}
	return 0
}

// maxEvent bounds one SSE event the nodes read whole to seal or open it.
const maxEvent = a2awire.MaxEvent

// errEventTooLarge ends a stream with an event over maxEvent.
var errEventTooLarge = errors.New("sealing: an SSE event over the node's bound")

// sseTransform copies an SSE stream from r to w event by event: an event with data has its data (the data lines
// joined) replaced by fn's result, written as one data line, every other field kept in its place; an event without
// data, and comments, pass as they came. fn returning nil drops the event whole. An error from fn ends the stream
// before that event, as does an event over maxEvent; a last event left unterminated is dropped (as WHATWG's parser
// does). Each event is flushed when w is an http.Flusher (text/event-stream is flushed per event, spec/tunnel.md
// §8.2).
//
// Lines are split as WHATWG's parser splits them (CRLF, LF or a lone CR; a UTF-8 BOM at the start dropped) and
// written with LF: what passes unread (a comment, an event or id field) is then no data line to any reader, a
// client splitting on LF only (a2a-go) included.
func sseTransform(r io.Reader, w io.Writer, fn func(data []byte) ([]byte, error)) error {
	return sseTransformMulti(r, w, func(data []byte) ([][]byte, error) {
		out, err := fn(data)
		if err != nil || out == nil {
			return nil, err
		}
		return [][]byte{out}, nil
	})
}

// sseTransformMulti is sseTransform whose fn may answer an event with several: the first takes the event's place,
// the others follow it as events of their own (data only).
func sseTransformMulti(r io.Reader, w io.Writer, fn func(data []byte) ([][]byte, error)) error {
	lr := &sseLines{br: bufio.NewReaderSize(r, 64<<10), start: true}
	var (
		lines [][]byte
		size  int
	)
	flush := func() {
		if f, ok := w.(interface{ Flush() }); ok {
			f.Flush()
		}
	}
	for {
		line, err := lr.next(maxEvent - size)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return nil // a last line or event without its blank line is dropped
		}
		if len(line) > 0 {
			lines = append(lines, line)
			size += len(line) + 1
			continue
		}
		if len(lines) == 0 {
			// A blank line with no event before it: nothing to dispatch.
			if _, werr := w.Write([]byte("\n")); werr != nil {
				return werr
			}
			continue
		}
		if werr := writeEvent(w, lines, fn); werr != nil {
			if errors.Is(werr, errStopAfter) {
				flush()
				return nil
			}
			return werr
		}
		flush()
		lines, size = nil, 0
	}
}

// sseLines reads the lines of an SSE stream as WHATWG's parser does (§9.2.6 "Interpreting an event stream"): a line
// ends at CRLF, LF or a lone CR, and a UTF-8 BOM at the start of the stream is dropped. It peeks only at bytes the
// reader holds, or at the next one when it needs one, so an event is never held back waiting for more.
type sseLines struct {
	br      *bufio.Reader
	start   bool // nothing read yet
	afterCR bool // the last line ended with CR: an LF first is the rest of its CRLF
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// next returns the next line without its terminator, at most max bytes long (errEventTooLarge beyond), or io.EOF at
// the end of the stream (a last line without its terminator is dropped with it).
func (l *sseLines) next(max int) ([]byte, error) {
	if l.start {
		l.start = false
		if b, err := l.br.Peek(1); err == nil && b[0] == utf8BOM[0] {
			if b, err := l.br.Peek(3); err == nil && bytes.Equal(b, utf8BOM) {
				_, _ = l.br.Discard(3)
			}
		}
	}
	if l.afterCR {
		l.afterCR = false
		if b, err := l.br.Peek(1); err == nil && b[0] == '\n' {
			_, _ = l.br.Discard(1)
		}
	}
	var line []byte
	for {
		n := l.br.Buffered()
		if n == 0 {
			n = 1
		}
		buf, err := l.br.Peek(n)
		if len(buf) == 0 {
			if err == nil {
				err = io.EOF
			}
			return nil, err
		}
		i := bytes.IndexAny(buf, "\r\n")
		if i < 0 {
			i = len(buf)
		}
		if len(line)+i > max {
			return nil, errEventTooLarge
		}
		line = append(line, buf[:i]...)
		if i == len(buf) {
			_, _ = l.br.Discard(i)
			continue
		}
		l.afterCR = buf[i] == '\r'
		_, _ = l.br.Discard(i + 1)
		return line, nil
	}
}

// writeEvent writes one event (its lines, without terminators) with fn's data, and the blank line that ends it.
func writeEvent(w io.Writer, lines [][]byte, fn func([]byte) ([][]byte, error)) error {
	var data [][]byte
	first := -1
	for i, l := range lines {
		if v, ok := dataValue(l); ok {
			if first < 0 {
				first = i
			}
			data = append(data, v)
		}
	}
	joined := bytes.Join(data, []byte("\n"))
	if first < 0 || len(bytes.TrimSpace(joined)) == 0 {
		var b bytes.Buffer
		for _, l := range lines {
			if _, ok := dataValue(l); ok {
				continue // an empty data line: nothing to read, and nothing to pass unread
			}
			b.Write(l)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		_, err := w.Write(b.Bytes())
		return err
	}
	outs, err := fn(joined)
	stop := errors.Is(err, errStopAfter)
	if err != nil && !stop {
		return err
	}
	if len(outs) == 0 {
		return err
	}
	var b bytes.Buffer
	for n, out := range outs {
		var compact bytes.Buffer
		if err := json.Compact(&compact, out); err != nil {
			return fmt.Errorf("sealing: an event that is not JSON: %w", err)
		}
		if n > 0 {
			b.WriteString("data: ")
			b.Write(compact.Bytes())
			b.WriteString("\n\n")
			continue
		}
		for i, l := range lines {
			if _, ok := dataValue(l); ok {
				if i == first {
					b.WriteString("data: ")
					b.Write(compact.Bytes())
					b.WriteByte('\n')
				}
				continue
			}
			b.Write(l)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if _, werr := w.Write(b.Bytes()); werr != nil {
		return werr
	}
	if stop {
		return errStopAfter
	}
	return nil
}

// errStopAfter, from an sseTransformMulti fn, writes what it returned and ends the stream there.
var errStopAfter = errors.New("sealing: the stream ends after this event")

// dataValue is a data line's value: after "data:" and one optional space (a line is "data" alone or starts with
// "data:", WHATWG's field name).
func dataValue(line []byte) ([]byte, bool) {
	v, ok := bytes.CutPrefix(line, []byte("data"))
	if !ok {
		return nil, false
	}
	if len(v) == 0 {
		return v, true
	}
	if v[0] != ':' {
		return nil, false
	}
	return bytes.TrimPrefix(v[1:], []byte(" ")), true
}
