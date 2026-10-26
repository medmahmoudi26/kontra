package blob

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// The reasons a gate refuses a blob stream (spec/tunnel.md §8.3, D181).
const (
	ReasonNotFound  = "not_found" // no blobs row for the ref, or no object behind it
	ReasonForbidden = "forbidden" // the row's task is neither served on nor hired by the asking session
	ReasonTooLarge  = "too_large" // an object over MaxObject
)

// maxSizeLine bounds the one JSON line that opens a blob stream's answer ({"size": 67108864} is 18 bytes).
const maxSizeLine = 256

// eofWait bounds the wait for the gate's close after the announced bytes, on a stream with deadlines: the gate
// closes the stream once it has written them (§8.3), and a byte more than announced is refused.
const eofWait = 10 * time.Second

// FetchError is the gate's refusal of a blob stream: "not_found", "forbidden" or "too_large".
type FetchError struct {
	Reason string
}

func (e *FetchError) Error() string { return "blob: refused: " + e.Reason }

// WriteSize writes a blob stream's size line, {"size": n} (the gate's half of §8.3); exactly n bytes follow.
func WriteSize(w io.Writer, n int64) error {
	_, err := io.WriteString(w, `{"size":`+strconv.FormatInt(n, 10)+"}\n")
	return err
}

// WriteRefusal writes a blob stream's error line, {"error": reason}; nothing follows it.
func WriteRefusal(w io.Writer, reason string) error {
	b, err := json.Marshal(map[string]string{"error": reason})
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Fetch reads one object on a node-opened blob stream (the node's half of spec/tunnel.md §8.3, D181): it writes
// the preamble {"v": 1, "op": "blob", "ref": ref} and reads one JSON line, {"size": n} or {"error": "not_found"
// | "forbidden" | "too_large"} (a FetchError), then exactly n bytes into w; it refuses an announced size over
// MaxObject before reading any of them, fewer bytes than announced, and more (a byte after the n, before the
// gate's close). The line is read with exact keys (D172): a repeated or case-variant member, a member besides
// size or error, or a size that is not a positive integer is refused. On an error w may hold part of the
// object, which the caller discards. Fetch closes stream, at once when ctx ends.
//
// The caller authenticates a sealed part's whole age file before acting on it (internal/sealing, WP-08). The
// node never dials a URL taken from a part (D56): the stream is one it opened in its own tunnel.
func Fetch(ctx context.Context, stream io.ReadWriteCloser, ref Ref, w io.Writer) (int64, error) {
	defer stream.Close()
	if !ref.Valid() {
		return 0, fmt.Errorf("blob: fetch: %s is not a ref", strictjson.Quote(string(ref)))
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	fail := func(err error) (int64, error) {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("blob: fetch %s: %w", ref, ctx.Err())
		}
		return 0, err
	}
	if err := tunnel.WritePreamble(stream, tunnel.Preamble{Op: tunnel.OpBlob, Ref: string(ref)}); err != nil {
		return fail(fmt.Errorf("blob: fetch %s: %w", ref, err))
	}
	br := bufio.NewReaderSize(stream, 32<<10)
	line, err := readSizeLine(br)
	if err != nil {
		return fail(fmt.Errorf("blob: fetch %s: the gate's answer: %w", ref, err))
	}
	n, err := parseSizeLine(line)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			return 0, fe
		}
		return 0, fmt.Errorf("blob: fetch %s: %w", ref, err)
	}
	if n > MaxObject {
		return 0, fmt.Errorf("blob: fetch %s: the gate announced %d bytes, over %d (spec/tunnel.md §14)", ref, n, int64(MaxObject))
	}
	got, err := io.CopyN(w, br, n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return fail(fmt.Errorf("blob: fetch %s: %d of %d bytes: %w", ref, got, n, err))
	}
	if d, ok := stream.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now().Add(eofWait))
	}
	var one [1]byte
	for {
		k, err := br.Read(one[:])
		if k > 0 {
			return 0, fmt.Errorf("blob: fetch %s: the gate sent more than the %d bytes it announced", ref, n)
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return fail(fmt.Errorf("blob: fetch %s: waiting for the gate's close after %d bytes: %w", ref, n, err))
		}
	}
}

// readSizeLine reads the answer's first line, without its '\n', within maxSizeLine bytes.
func readSizeLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if b == '\n' {
			return line, nil
		}
		if len(line) >= maxSizeLine {
			return nil, fmt.Errorf("a first line over %d bytes", maxSizeLine)
		}
		line = append(line, b)
	}
}

// parseSizeLine reads {"size": n} (n ≥ 1) or {"error": reason} (a *FetchError), with exact keys.
func parseSizeLine(line []byte) (int64, error) {
	m, err := strictjson.Members(line)
	if err != nil {
		return 0, fmt.Errorf("the first line is not one JSON object with exact keys: %w", err)
	}
	if len(m) != 1 {
		return 0, fmt.Errorf("the first line has %d members, want size or error alone", len(m))
	}
	if raw, ok := m["error"]; ok {
		var reason string
		if err := json.Unmarshal(raw, &reason); err != nil {
			return 0, errors.New("the error member is not a string")
		}
		switch reason {
		case ReasonNotFound, ReasonForbidden, ReasonTooLarge:
			return 0, &FetchError{Reason: reason}
		}
		return 0, fmt.Errorf("an unknown refusal %s", strictjson.Quote(reason))
	}
	raw, ok := m["size"]
	if !ok {
		return 0, errors.New("the first line has neither size nor error")
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("size %s is not a positive integer", strictjson.Quote(string(raw)))
	}
	return n, nil
}
