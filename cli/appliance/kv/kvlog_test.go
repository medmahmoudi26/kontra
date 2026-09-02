package kv

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A RESTART MUST NOT BE A RESET, and this is the test that says so.
//
// `redis-data` is a named volume in docker-compose.yml with a one-line comment explaining why:
// "durable shared global_state tier — survives recreate". Losing that on the way into the binary
// would be the easiest regression in this slice to ship and the hardest to notice, because an
// empty store answers every read successfully. So: write through the wire, stop the server, start
// a NEW one over the same data directory, and read it back through the wire.
//
// Note what is asserted after the restart: not that a file exists, but that the VALUES, the
// version counters and the remaining TTL all come back.
func TestRestartPreservesTheKeyspace(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)

	first := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, first.Address())

	// Tier 3: no TTL, and a version counter that a later CAS depends on.
	wantInt(t, c.send("EVALSHA", casSHA, "1", "kontra-global:cachebuster:seen", "payload", ""), 1)
	wantInt(t, c.send("EVALSHA", putSHA, "1", "kontra-global:cachebuster:seen", "payload-2"), 1)
	// Tiers 1+2: an actor hash under a TTL.
	wantInt(t, c.send("HSET", "kontra-actor:sess-1", "u0", `{"done":1}`, "u1", `{"done":2}`), 2)
	wantInt(t, c.send("EXPIRE", "kontra-actor:sess-1", "86400"), 1)
	// …and something deleted, which must STAY deleted rather than being resurrected by a replay
	// that only knows how to add.
	wantInt(t, c.send("HSET", "kontra-actor:sess-2", "u0", "x"), 1)
	wantInt(t, c.send("DEL", "kontra-actor:sess-2"), 1)
	wantInt(t, c.send("HSET", "kontra-actor:sess-1", "gone", "x"), 1)
	wantInt(t, c.send("HDEL", "kontra-actor:sess-1", "gone"), 1)

	if err := first.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	second := startKVForTest(t, Options{DataDir: dir, Port: port})
	c = dialKV(t, second.Address())

	arr, _ := c.send("HMGET", "kontra-global:cachebuster:seen", "data", "ver").([]any)
	if len(arr) != 2 || arr[0] != "payload-2" || arr[1] != "2" {
		t.Fatalf("global_state after restart = %#v, want [payload-2 2]", arr)
	}
	// The version survived, so a CAS holding the pre-restart etag still loses — the restart did
	// not quietly reset the ETag sequence and hand a stale writer a win.
	wantInt(t, c.send("EVALSHA", casSHA, "1", "kontra-global:cachebuster:seen", "stale", "1"), 0)
	wantInt(t, c.send("EVALSHA", casSHA, "1", "kontra-global:cachebuster:seen", "fresh", "2"), 1)

	wantStr(t, c.send("HGET", "kontra-actor:sess-1", "u0"), `{"done":1}`)
	wantStr(t, c.send("HGET", "kontra-actor:sess-1", "u1"), `{"done":2}`)
	if v := c.send("HGET", "kontra-actor:sess-1", "gone"); v != nil {
		t.Fatalf("a field deleted before the restart came back as %#v", v)
	}
	wantInt(t, c.send("TTL", "kontra-actor:sess-2"), -2)

	// The TTL is a REMAINING one, not a fresh 24 hours. A relative EXPIRE replayed at boot would
	// slide the deadline forward on every restart and the ceiling would stop being a ceiling.
	ttl, _ := c.send("TTL", "kontra-actor:sess-1").(int64)
	if ttl < 86_000 || ttl > 86_400 {
		t.Fatalf("TTL after restart = %d, want a remaining slice of the original 86400", ttl)
	}
}

// Binary and multi-line values must survive the round trip through the log. Actor state is
// author-controlled JSON — a crawl frontier, a serialized payload — and a line-oriented log
// format would corrupt exactly the values that are hardest to notice losing.
func TestLogPreservesAwkwardValues(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)
	awkward := map[string]string{
		"empty":     "",
		"newlines":  "a\r\nb\nc",
		"nul":       "before\x00after",
		"binary":    string([]byte{0x00, 0xff, 0xfe, 0x0d, 0x0a, 0x24, 0x2a}),
		"resp-bait": "*2\r\n$4\r\nHSET\r\n",
		"unicode":   "héllo — ✓",
	}

	first := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, first.Address())
	for field, value := range awkward {
		c.send("HSET", "kontra-actor:sess-1", field, value)
	}
	first.Stop()

	second := startKVForTest(t, Options{DataDir: dir, Port: port})
	c = dialKV(t, second.Address())
	for field, want := range awkward {
		got := c.send("HGET", "kontra-actor:sess-1", field)
		if got != want {
			t.Errorf("field %q came back as %q, want %q", field, got, want)
		}
	}
}

// A key whose TTL elapsed while the appliance was DOWN must be gone when it comes back. The
// deadline is absolute in the log precisely so this is arithmetic rather than a timer that did
// not exist for the duration.
func TestExpiryIsHonouredAcrossADowntime(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)

	first := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, first.Address())
	c.send("HSET", "kontra-actor:short", "u0", "x")
	c.send("EXPIRE", "kontra-actor:short", "1")
	c.send("HSET", "kontra-actor:long", "u0", "x")
	c.send("EXPIRE", "kontra-actor:long", "86400")
	first.Stop()

	// The downtime, simulated by moving the clock rather than by sleeping through it.
	second, err := Start(Options{DataDir: dir, Port: port, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Stop()
	second.keys.mu.Lock()
	second.keys.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	second.keys.mu.Unlock()

	c = dialKV(t, second.Address())
	wantInt(t, c.send("TTL", "kontra-actor:short"), -2)
	if v := c.send("HGET", "kontra-actor:short", "u0"); v != nil {
		t.Fatalf("an expired key answered %#v", v)
	}
	wantStr(t, c.send("HGET", "kontra-actor:long", "u0"), "x")
}

// A CRASH CAUGHT A WRITE IN FLIGHT is not corruption, it is the boundary — and refusing to open
// would lose a whole keyspace to save one record. The tail is truncated, the operator is told,
// and everything before it is intact.
func TestATornTailIsTruncatedAndReported(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)

	first := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, first.Address())
	c.send("HSET", "kontra-global:a:one", "data", "1")
	c.send("HSET", "kontra-global:a:two", "data", "2")
	first.Stop()

	path := filepath.Join(dir, "kv", kvLogName)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Append half of a third record — the shape a SIGKILL mid-write leaves behind.
	torn := append(append([]byte{}, full...), []byte("*4\r\n$4\r\nHSET\r\n$18\r\nkontra-global:a:th")...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	var logged []string
	second, err := Start(Options{DataDir: dir, Port: port,
		Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }})
	if err != nil {
		t.Fatalf("a torn tail must not stop the store from opening: %v", err)
	}
	defer second.Stop()

	c = dialKV(t, second.Address())
	wantStr(t, c.send("HGET", "kontra-global:a:one", "data"), "1")
	wantStr(t, c.send("HGET", "kontra-global:a:two", "data"), "2")
	if !strings.Contains(strings.Join(logged, "\n"), "half-written") {
		t.Fatalf("the truncation was silent; logged: %v", logged)
	}
	// And the store is writable afterwards, at a clean offset rather than appending onto a
	// half-record that would corrupt the NEXT replay.
	wantInt(t, c.send("HSET", "kontra-global:a:three", "data", "3"), 1)
	second.Stop()

	third := startKVForTest(t, Options{DataDir: dir, Port: port})
	c = dialKV(t, third.Address())
	wantStr(t, c.send("HGET", "kontra-global:a:three", "data"), "3")
}

// Damage that is NOT at the end is a different thing entirely: the file is not what this code
// thinks it is, and applying the rest of it would build a keyspace nobody wrote. That refuses,
// with the offset — because the only thing worse than a store that will not open is one that
// opens holding state that never existed.
func TestCorruptionInTheMiddleRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)

	first := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, first.Address())
	for i := 0; i < 5; i++ {
		c.send("HSET", fmt.Sprintf("kontra-global:a:k%d", i), "data", strconv.Itoa(i))
	}
	first.Stop()

	path := filepath.Join(dir, "kv", kvLogName)
	full := readFileForTest(t, path)
	half := len(full) / 2
	// A record header claiming a length the payload does not have, in the middle of the file.
	broken := full[:half] + "*2\r\n$99\r\nshort\r\n" + full[half:]
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Start(Options{DataDir: dir, Port: port, Logf: func(string, ...any) {}})
	if err == nil {
		t.Fatal("a store opened over a log it could not read")
	}
	if !strings.Contains(err.Error(), "byte ") || !strings.Contains(err.Error(), kvLogName) {
		t.Fatalf("the refusal names neither the file nor the offset: %v", err)
	}
}

// A record naming an operation this version does not know is refused for the same reason. It is
// the forward-compatibility trap: skipping it would apply the rest of the log and produce a
// keyspace that is missing exactly the change the unknown record described.
func TestAnUnknownRecordRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "kv")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := appendRESPArray(nil, "HSET", "kontra-global:a:k", "data", "1")
	body = appendRESPArray(body, "HSETEX", "kontra-global:a:k", "data", "2")
	if err := os.WriteFile(filepath.Join(logDir, kvLogName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Start(Options{DataDir: dir, Port: freeTestKVPort(t), Logf: func(string, ...any) {}})
	if err == nil || !strings.Contains(err.Error(), "HSETEX") {
		t.Fatalf("an unknown record was applied or skipped rather than refused: %v", err)
	}
}

// Compaction must be a shortening, not a rewriting. The log is replaced through a rename, so an
// interrupted rewrite leaves the old file intact — and the state after it is byte-for-byte the
// state before.
func TestRewriteShortensTheLogWithoutChangingTheKeyspace(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)
	s := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, s.Address())

	// One key rewritten many times: the log grows, the keyspace does not.
	for i := 0; i < 500; i++ {
		c.send("HSET", "kontra-actor:hot", "u0", strings.Repeat("x", 200)+strconv.Itoa(i))
	}
	c.send("EXPIRE", "kontra-actor:hot", "86400")
	if err := s.log.flush(); err != nil {
		t.Fatal(err)
	}
	before := logSize(t, s.LogPath())

	s.keys.mu.Lock()
	err := s.log.rewrite(s.keys.snapshot())
	s.keys.mu.Unlock()
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after := logSize(t, s.LogPath())
	if after >= before/2 {
		t.Fatalf("the rewrite shortened the log from %d to %d bytes; it should be a fraction", before, after)
	}

	// Writable after the swap — the reopened handle appends to the NEW file, not the unlinked one.
	wantInt(t, c.send("HSET", "kontra-actor:hot", "u1", "after"), 1)
	s.Stop()

	again := startKVForTest(t, Options{DataDir: dir, Port: port})
	c = dialKV(t, again.Address())
	wantStr(t, c.send("HGET", "kontra-actor:hot", "u0"), strings.Repeat("x", 200)+"499")
	wantStr(t, c.send("HGET", "kontra-actor:hot", "u1"), "after")
	if ttl, _ := c.send("TTL", "kontra-actor:hot").(int64); ttl < 86_000 {
		t.Fatalf("the TTL did not survive the rewrite: %d", ttl)
	}
}

func logSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// The reply must not outrun the record. A client that has been told a write succeeded and then
// loses it to a crash one instruction later has been lied to — so the log is flushed to the
// kernel before the reply is flushed to the socket.
func TestTheRecordReachesTheKernelBeforeTheReply(t *testing.T) {
	dir := t.TempDir()
	s := startKVForTest(t, Options{DataDir: dir, Port: freeTestKVPort(t)})
	c := dialKV(t, s.Address())
	wantInt(t, c.send("HSET", "kontra-global:a:k", "data", "acknowledged"), 1)

	// Read the file with a handle that is not the server's. Anything still sitting in the
	// server's userspace buffer would not be here.
	raw := readFileForTest(t, s.LogPath())
	if !strings.Contains(raw, "acknowledged") {
		t.Fatal("the write was acknowledged before its record left the process; a crash here loses it")
	}
}
