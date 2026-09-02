// kvlog.go — the durability under the embedded key-value store.
//
// `redis-data` is a NAMED VOLUME in docker-compose.yml, and its comment says why in one line:
// "durable shared global_state tier — survives recreate". The container achieved that with
// `--appendonly yes`. This file is that append-only file, and it is deliberately the same design
// rather than a snapshot: a periodic dump loses every write since the last one, and the writes
// this store holds are a dedupe set and a commit map, where a lost write does not degrade a run,
// it DUPLICATES work (see statekv.py's header — unit blob keys end in sha256(data), so a re-run
// whose records are not byte-identical writes a second blob and the child processes both).
//
// # The format
//
// One RESP array per record, exactly as `kv.go` writes replies — the codec is already there and
// is already length-prefixed and binary-safe, which a line-oriented format would not be for
// values that are arbitrary JSON.
//
//	HSET <key> <field> <value> [<field> <value> ...]
//	HDEL <key> <field> [<field> ...]
//	DEL  <key>
//	PEXPIREAT <key> <unix-millis>
//
// EXPIRE IS LOGGED AS AN ABSOLUTE DEADLINE, which is the one transformation this file makes to
// what the client sent. A relative `EXPIRE key 86400` replayed at boot would restart the 24 h
// clock on every restart, and abandoned actor state would never expire on a machine that reboots
// daily. Redis rewrites EXPIRE the same way for the same reason.
//
// # What a torn tail means, and what it does not
//
// A crash can leave a half-written record at the end of the file. That is not corruption, it is
// the boundary — the write was in flight — and refusing to start over it would lose an entire
// keyspace to save one record. It is truncated, and LOGGED, because a silent truncation is how
// an operator concludes a run lost state for no reason.
//
// A record that is well-formed but names an operation this version does not know, or a parse
// failure that is NOT at the end of the file, is a different thing entirely: the file is not what
// this code thinks it is, and applying the rest of it would build a keyspace that never existed.
// That REFUSES, with the offset.
package kv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// kvLogName is the file inside the store's directory. Named for what it is rather than
// `appendonly.aof`, so nobody mistakes it for a file `redis-server` could open.
const kvLogName = "keyspace.log"

// rewriteFloor is the smallest log that is worth compacting. Below it the rewrite costs more than
// the replay it saves.
const rewriteFloor int64 = 8 << 20

// errTornTail marks "the file ended in the middle of a record", which is recoverable. Every other
// parse failure is not.
var errTornTail = errors.New("record is truncated at end of file")

type kvLog struct {
	mu   sync.Mutex
	path string
	dir  string
	f    *os.File
	w    *bufio.Writer

	// base is the file size the last rewrite produced, and appended is what has been written
	// since. Together they are Redis's own rewrite heuristic: compact when the log has grown to
	// twice the data it represents.
	base     int64
	appended int64

	dirty  bool
	closed bool
	logf   func(string, ...any)
}

// openKVLog opens (or creates) the log in dir. It does not read it; see replayKVLog.
func openKVLog(dir string, logf func(string, ...any)) (*kvLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("appliance: key-value data directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, kvLogName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("appliance: opening %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("appliance: %s: %w", path, err)
	}
	return &kvLog{path: path, dir: dir, f: f, w: bufio.NewWriterSize(f, 64<<10), base: st.Size(), logf: logf}, nil
}

// append records one mutation. It is called with the keyspace lock held, so log order is apply
// order — the invariant a replay depends on.
func (l *kvLog) append(args ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("appliance: the key-value log is closed")
	}
	buf := appendRESPArray(nil, args...)
	n, err := l.w.Write(buf)
	l.appended += int64(n)
	if err != nil {
		return fmt.Errorf("appliance: writing to %s: %w", l.path, err)
	}
	l.dirty = true
	return nil
}

// flush pushes the buffer into the kernel. Called before the reply to the command that produced
// the record is flushed to the client, so a process that dies has still handed every
// acknowledged write to the OS. Machine loss is what sync covers.
func (l *kvLog) flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushLocked()
}

func (l *kvLog) flushLocked() error {
	if l.closed {
		return nil
	}
	if err := l.w.Flush(); err != nil {
		return fmt.Errorf("appliance: flushing %s: %w", l.path, err)
	}
	return nil
}

// sync is fsync, on the everysec cadence Redis's default appendfsync uses. Per-write fsync would
// be the stronger guarantee and it is not the right trade here: a commit map write happens once
// per unit, and paying a disk round trip on each one would make the store the slowest thing in a
// run.
func (l *kvLog) sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.dirty {
		return nil
	}
	if err := l.flushLocked(); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("appliance: fsync %s: %w", l.path, err)
	}
	l.dirty = false
	return nil
}

// shouldRewrite reports whether the log has grown to twice what it started at.
func (l *kvLog) shouldRewrite() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	size := l.base + l.appended
	return size >= rewriteFloor && size >= 2*maxInt64(l.base, rewriteFloor/2)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// rewrite replaces the log with the shortest one that reproduces `records`.
//
// Written to a sibling and renamed, so an interrupted rewrite leaves the OLD log intact — the
// rename is the commit, and there is no window in which neither file is complete.
//
// THE COST, stated rather than hidden: the caller holds the keyspace lock across this, so every
// command waits on it. Redis forks a child instead, which needs copy-on-write of the whole
// dataset — a trade that is worth it at Redis's scale and not at this one, where the ceiling is
// 512 MiB on a local disk and the trigger only fires when the log has doubled. Buffered writes
// and a conservative trigger are the mitigation; a background snapshot would need a second copy
// of the keyspace in memory, which is the resource this appliance has least of.
func (l *kvLog) rewrite(records [][]string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("appliance: the key-value log is closed")
	}
	tmp := l.path + ".rewrite"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("appliance: opening %s: %w", tmp, err)
	}
	w := bufio.NewWriterSize(f, 256<<10)
	var buf []byte
	for _, rec := range records {
		buf = appendRESPArray(buf[:0], rec...)
		if _, err := w.Write(buf); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("appliance: writing %s: %w", tmp, err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("appliance: writing %s: %w", tmp, err)
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("appliance: %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("appliance: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("appliance: closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("appliance: replacing %s: %w", l.path, err)
	}
	// The rename itself needs the directory synced, or a crash can leave the entry pointing at
	// neither file.
	if d, err := os.Open(l.dir); err == nil {
		d.Sync()
		d.Close()
	}

	old := l.f
	nf, err := os.OpenFile(l.path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("appliance: reopening %s after rewrite: %w", l.path, err)
	}
	old.Close()
	l.f = nf
	l.w = bufio.NewWriterSize(nf, 64<<10)
	l.base = size
	l.appended = 0
	l.dirty = false
	return nil
}

// close flushes, fsyncs and closes. Safe to call twice.
func (l *kvLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	err := l.flushLocked()
	if serr := l.f.Sync(); err == nil {
		err = serr
	}
	l.closed = true
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// replayKVLog reads the log back into a keyspace.
//
// Returns the number of records applied. A torn tail is truncated away and reported through logf;
// anything else is an error that stops the appliance from starting, with the byte offset, because
// the only worse thing than a store that will not open is one that opens holding a keyspace
// nobody wrote.
func replayKVLog(l *kvLog, k *keyspace) (int, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return 0, fmt.Errorf("appliance: reading %s: %w", l.path, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 256<<10)
	applied := 0
	var offset int64
	for {
		rec, n, err := readRESPRecord(r)
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if errors.Is(err, errTornTail) || (errors.Is(err, io.EOF) && n > 0) {
			l.logf("%s: the last %d bytes are a half-written record (offset %d) — a crash "+
				"caught a write in flight; truncating and continuing with %d records",
				l.path, n, offset, applied)
			if terr := os.Truncate(l.path, offset); terr != nil {
				return applied, fmt.Errorf("appliance: truncating %s to %d: %w", l.path, offset, terr)
			}
			// The append handle's offset follows the file under O_APPEND, but its recorded base
			// size does not — correct it so the rewrite heuristic is not reasoning about bytes
			// that are gone.
			l.mu.Lock()
			l.base = offset
			l.mu.Unlock()
			break
		}
		if err != nil {
			return applied, fmt.Errorf("appliance: %s is not a key-value log this version can "+
				"read — record at byte %d: %w", l.path, offset, err)
		}
		if err := k.applyReplay(rec); err != nil {
			return applied, fmt.Errorf("appliance: %s, record at byte %d: %w", l.path, offset, err)
		}
		offset += n
		applied++
	}
	k.finishReplay()
	return applied, nil
}

// --- the RESP codec, shared with the wire ------------------------------------------------

// appendRESPArray encodes one command as a RESP array of bulk strings — the same bytes a client
// sends and the same bytes the log stores.
func appendRESPArray(dst []byte, args ...string) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(len(args)), 10)
	dst = append(dst, '\r', '\n')
	for _, a := range args {
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(len(a)), 10)
		dst = append(dst, '\r', '\n')
		dst = append(dst, a...)
		dst = append(dst, '\r', '\n')
	}
	return dst
}

// Bounds, so a malformed length cannot make this allocate the machine. Redis's own limits.
const (
	maxMultiBulk = 1 << 20
	maxBulkBytes = 512 << 20
)

// readRESPRecord reads one array of bulk strings, returning the bytes consumed alongside it so a
// replay can report an offset and a truncation can find the boundary.
func readRESPRecord(r *bufio.Reader) ([]string, int64, error) {
	var read int64
	line, n, err := readRESPLine(r)
	read += n
	if err != nil {
		return nil, read, err
	}
	if len(line) == 0 || line[0] != '*' {
		return nil, read, fmt.Errorf("expected a RESP array, got %q", truncateForError(line))
	}
	count, err := strconv.Atoi(string(line[1:]))
	if err != nil || count < 0 || count > maxMultiBulk {
		return nil, read, fmt.Errorf("invalid multibulk length %q", truncateForError(line[1:]))
	}
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		line, n, err := readRESPLine(r)
		read += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, read, errTornTail
			}
			return nil, read, err
		}
		if len(line) == 0 || line[0] != '$' {
			return nil, read, fmt.Errorf("expected a bulk string, got %q", truncateForError(line))
		}
		size, err := strconv.Atoi(string(line[1:]))
		if err != nil || size < 0 || size > maxBulkBytes {
			return nil, read, fmt.Errorf("invalid bulk length %q", truncateForError(line[1:]))
		}
		buf := make([]byte, size+2) // payload plus the trailing CRLF
		got, err := io.ReadFull(r, buf)
		read += int64(got)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, read, errTornTail
			}
			return nil, read, err
		}
		if buf[size] != '\r' || buf[size+1] != '\n' {
			return nil, read, errors.New("bulk string is not terminated by CRLF")
		}
		out = append(out, string(buf[:size]))
	}
	return out, read, nil
}

// readRESPLine reads one CRLF-terminated line without its terminator. A line that ends the file
// without its CRLF is a torn tail, not a line.
func readRESPLine(r *bufio.Reader) ([]byte, int64, error) {
	line, err := r.ReadBytes('\n')
	n := int64(len(line))
	if err != nil {
		if errors.Is(err, io.EOF) && n > 0 {
			return nil, n, errTornTail
		}
		return nil, n, err
	}
	if n < 2 || line[n-2] != '\r' {
		return nil, n, errors.New("line is not terminated by CRLF")
	}
	return line[:n-2], n, nil
}

func truncateForError(b []byte) string {
	if len(b) > 64 {
		return string(b[:64]) + "…"
	}
	return string(b)
}
