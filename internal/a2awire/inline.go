package a2awire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"kontra.sh/kontra/internal/blob"
)

// Inlining bounds (spec/tunnel.md §8.3, §14).
const (
	// SpillOver is how much of a fetched object is held in memory: a larger one is spilled to a temporary
	// file (0600, removed when the body is closed).
	SpillOver = 8 << 20
	// MaxInlineTotal bounds what one body inlines in all (each object is at most blob.MaxObject): the gate's
	// default bytes per task (spec/relay.md §6.6).
	MaxInlineTotal = 256 << 20
	// MaxBody bounds a JSON body read whole to inline its parts; a longer one is passed on as it came.
	MaxBody = 64 << 20
	// MaxEvent bounds one SSE event read whole; a longer one ends the stream.
	MaxEvent = 64 << 20
)

// Fetcher writes the object a ref names to w: blob.Fetch on a blob stream the node opened in its own tunnel
// (the session the exchange came on, for the hosting node; the current session, for the hiring node).
type Fetcher func(ctx context.Context, ref blob.Ref, w io.Writer) (int64, error)

// Inliner replaces the url parts kontra-blob:<ref> of A2A 1.0 bodies with raw parts holding the object's
// bytes, fetched on blob streams, so that a stock agent or client never sees the scheme (spec/relay.md §10.4,
// spec/tunnel.md §8.3; D56: the node never dials a URL taken from a part). The part keeps its filename,
// mediaType and metadata.
type Inliner struct {
	Fetch Fetcher
	// TempDir holds spilled objects ("" is os.TempDir()).
	TempDir string
	// MaxTotal replaces MaxInlineTotal when positive (tests).
	MaxTotal int64
}

// ErrReservedURL is a url part with the kontra-blob: scheme that does not name a ref in the gate's form
// (D181: only the gate writes the scheme).
var ErrReservedURL = errors.New("a2awire: a kontra-blob: url part that is not a ref")

// HasRefs reports whether b may hold a kontra-blob: part: it holds the scheme in any case (blob.Reserved's rule).
// A body without it is passed on without being parsed.
func HasRefs(b []byte) bool {
	const scheme = blob.URIScheme
	for i := 0; i+len(scheme) <= len(b); i++ {
		if (b[i] == 'k' || b[i] == 'K') && bytes.EqualFold(b[i:i+len(scheme)], []byte(scheme)) {
			return true
		}
	}
	return false
}

// Inline returns body with every kontra-blob:<ref> url part replaced by a raw part. A body that holds no
// kontra-blob: is returned as it came, unparsed. The result must be closed (its spilled objects are removed).
// An error leaves nothing behind: a fetch the gate refused (*blob.FetchError), a body that is not an A2A 1.0
// body at a level read (ErrShape, *strictjson.KeyError), a malformed kontra-blob: part (ErrReservedURL), more
// than MaxInlineTotal in all.
func (in *Inliner) Inline(ctx context.Context, body []byte) (*Body, error) {
	if !HasRefs(body) {
		return &Body{segs: []segment{{lit: body}}}, nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	marker := "kontra-inline-" + hex.EncodeToString(nonce[:]) + "-"
	out := &Body{}
	total, limit := int64(0), in.MaxTotal
	if limit <= 0 {
		limit = MaxInlineTotal
	}
	rewritten, changed, err := Rewrite(body, func(p Part) (Part, error) {
		u, ok := p.URL()
		if !ok || !blob.Reserved(u) {
			return nil, nil
		}
		ref, ok := blob.RefOf(u)
		if !ok {
			return nil, ErrReservedURL
		}
		sp := &spill{dir: in.TempDir}
		n, err := in.Fetch(ctx, ref, sp)
		if err != nil {
			sp.Close()
			return nil, fmt.Errorf("a2awire: inline %s: %w", ref, err)
		}
		if total += n; total > limit {
			sp.Close()
			return nil, fmt.Errorf("a2awire: inlining more than %d bytes in one body", limit)
		}
		key := fmt.Sprintf("%s%d", marker, len(out.spills))
		out.spills = append(out.spills, sp)
		np := Part{}
		for k, v := range p {
			if k != "url" {
				np[k] = v
			}
		}
		np["raw"] = json.RawMessage(`"` + key + `"`)
		return np, nil
	})
	if err != nil {
		out.Close()
		return nil, err
	}
	if !changed {
		return &Body{segs: []segment{{lit: body}}}, nil
	}
	// Splice: each marker (a JSON string the walk wrote, in the order of the visits, which the re-encoding's key
	// order need not keep) becomes the base64 of its object.
	type place struct {
		at, end int
		sp      *spill
	}
	places := make([]place, 0, len(out.spills))
	for i, sp := range out.spills {
		quoted := []byte(fmt.Sprintf("\"%s%d\"", marker, i))
		at := bytes.Index(rewritten, quoted)
		if at < 0 {
			out.Close()
			return nil, errors.New("a2awire: a spliced part was lost") // never reached
		}
		places = append(places, place{at: at, end: at + len(quoted), sp: sp})
	}
	sort.Slice(places, func(i, j int) bool { return places[i].at < places[j].at })
	from := 0
	for _, pl := range places {
		out.segs = append(out.segs, segment{lit: rewritten[from : pl.at+1]}, segment{spill: pl.sp}, segment{lit: []byte(`"`)})
		from = pl.end
	}
	out.segs = append(out.segs, segment{lit: rewritten[from:]})
	return out, nil
}

// Body is an inlined body: Len bytes that WriteTo writes, objects streamed from memory or their spill files as
// base64. Close removes the spill files.
type Body struct {
	segs   []segment
	spills []*spill
}

type segment struct {
	lit   []byte
	spill *spill
}

// Len is the body's length in bytes.
func (b *Body) Len() int64 {
	var n int64
	for _, s := range b.segs {
		if s.spill != nil {
			n += int64(base64.StdEncoding.EncodedLen(int(s.spill.n)))
			continue
		}
		n += int64(len(s.lit))
	}
	return n
}

// Changed reports whether a part was inlined.
func (b *Body) Changed() bool { return len(b.spills) > 0 }

// WriteTo writes the body to w.
func (b *Body) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for _, s := range b.segs {
		if s.spill == nil {
			n, err := w.Write(s.lit)
			total += int64(n)
			if err != nil {
				return total, err
			}
			continue
		}
		cw := &countWriter{w: w}
		enc := base64.NewEncoder(base64.StdEncoding, cw)
		r, err := s.spill.reader()
		if err != nil {
			return total, err
		}
		_, err = io.Copy(enc, r)
		if err == nil {
			err = enc.Close()
		}
		total += cw.n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Bytes returns the whole body in memory (tests, small bodies).
func (b *Body) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := b.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Close removes the spill files.
func (b *Body) Close() error {
	if b == nil {
		return nil
	}
	var errs []error
	for _, s := range b.spills {
		errs = append(errs, s.Close())
	}
	b.spills = nil
	return errors.Join(errs...)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// spill holds one fetched object: in memory up to SpillOver bytes, then in a temporary file.
type spill struct {
	dir  string
	mem  bytes.Buffer
	file *os.File
	n    int64
}

func (s *spill) Write(p []byte) (int, error) {
	if s.file == nil && s.mem.Len()+len(p) > SpillOver {
		f, err := os.CreateTemp(s.dir, "kontra-blob-*")
		if err != nil {
			return 0, fmt.Errorf("a2awire: spill: %w", err)
		}
		s.file = f
		if _, err := f.Write(s.mem.Bytes()); err != nil {
			return 0, fmt.Errorf("a2awire: spill: %w", err)
		}
		s.mem = bytes.Buffer{}
	}
	var (
		n   int
		err error
	)
	if s.file != nil {
		n, err = s.file.Write(p)
	} else {
		n, err = s.mem.Write(p)
	}
	s.n += int64(n)
	return n, err
}

// Spilled reports whether the object went to a file.
func (s *spill) Spilled() bool { return s.file != nil }

func (s *spill) reader() (io.Reader, error) {
	if s.file == nil {
		return bytes.NewReader(s.mem.Bytes()), nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.LimitReader(s.file, s.n), nil
}

func (s *spill) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	err := s.file.Close()
	s.file = nil
	return errors.Join(err, os.Remove(name))
}

// InlineSSE copies an SSE stream from r to w, event by event, inlining the parts of every event whose data holds
// kontra-blob:, and calls flush after each event (text/event-stream is flushed per event, spec/tunnel.md §8.2).
// An event without the scheme, comments and every field but data are written as they came, in their order; a
// rewritten event's data is written as one data line per line of its new JSON (a JSON body holds line breaks only
// between tokens). An event over MaxEvent, or one whose inlining fails, ends the stream with the error.
func (in *Inliner) InlineSSE(ctx context.Context, r io.Reader, w io.Writer, flush func()) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var (
		lines [][]byte // the event's lines as read, line ends included
		size  int
	)
	for {
		line, err := readLine(br, MaxEvent-size)
		if len(line) > 0 {
			lines = append(lines, line)
			size += len(line)
			if isBlank(line) {
				if werr := in.writeEvent(ctx, w, lines); werr != nil {
					return werr
				}
				if flush != nil {
					flush()
				}
				lines, size = nil, 0
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(lines) > 0 { // a last event without its blank line
					if werr := in.writeEvent(ctx, w, lines); werr != nil {
						return werr
					}
					if flush != nil {
						flush()
					}
				}
				return nil
			}
			return err
		}
	}
}

// readLine reads one line with its end ("\n", "\r\n" or a lone "\r" is not split: SSE ends lines with any; the
// gate writes "\n"), within max bytes.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(line)+len(frag) > max {
			return nil, fmt.Errorf("a2awire: an SSE event over %d bytes", MaxEvent)
		}
		line = append(line, frag...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func isBlank(line []byte) bool {
	return len(bytes.TrimRight(line, "\r\n")) == 0
}

// writeEvent writes one event's lines, inlining its data when it holds kontra-blob:.
func (in *Inliner) writeEvent(ctx context.Context, w io.Writer, lines [][]byte) error {
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
	if first < 0 || !HasRefs(joined) {
		for _, l := range lines {
			if _, err := w.Write(l); err != nil {
				return err
			}
		}
		return nil
	}
	body, err := in.Inline(ctx, joined)
	if err != nil {
		return err
	}
	defer body.Close()
	for i, l := range lines {
		if _, ok := dataValue(l); !ok {
			if _, err := w.Write(l); err != nil {
				return err
			}
			continue
		}
		if i != first {
			continue
		}
		if err := writeDataLines(w, body); err != nil {
			return err
		}
	}
	return nil
}

// dataValue is a data line's value: after "data:" and one optional space, without the line end.
func dataValue(line []byte) ([]byte, bool) {
	l := bytes.TrimRight(line, "\r\n")
	v, ok := bytes.CutPrefix(l, []byte("data"))
	if !ok {
		return nil, false
	}
	if len(v) == 0 {
		return v, true // "data" alone: an empty value
	}
	if v[0] != ':' {
		return nil, false // another field whose name starts with data
	}
	v = v[1:]
	return bytes.TrimPrefix(v, []byte(" ")), true
}

// writeDataLines writes body as "data: " lines, one per line of the body.
func writeDataLines(w io.Writer, body *Body) error {
	lw := &dataLineWriter{w: w, start: true}
	if _, err := body.WriteTo(lw); err != nil {
		return err
	}
	if !lw.start {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}

// dataLineWriter prefixes every line it writes with "data: ".
type dataLineWriter struct {
	w     io.Writer
	start bool
}

func (d *dataLineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if d.start {
			if _, err := io.WriteString(d.w, "data: "); err != nil {
				return written, err
			}
			d.start = false
		}
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i+1]
			d.start = true
		}
		n, err := d.w.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		p = p[len(chunk):]
	}
	return written, nil
}

// IsEventStream reports a text/event-stream media type.
func IsEventStream(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "text/event-stream")
}

// IsJSON reports a JSON media type (application/json, or any +json).
func IsJSON(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}
