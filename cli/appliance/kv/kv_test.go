package kv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a client, written here on purpose ---------------------------------------------------
//
// The tests below drive the store with a RESP client of their own rather than importing
// go-redis. Two reasons, and neither is squeamishness about dependencies: the cli module does not
// have go-redis and this slice adds no third-party code to it, and — more usefully — a
// hand-written client can assert the BYTES. "HGETALL answered with a RESP3 map" and "HGETALL
// answered with an array that a client happened to coerce into the same object" are different
// facts, and only one of them is what a real client will meet.
//
// The real clients get their own file: kv_clients_test.go drives ioredis, go-redis and redis-py
// against this server, which is where compatibility is actually established.

type testClient struct {
	t    *testing.T
	c    net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	prot int
}

func dialKV(t *testing.T, addr string) *testClient {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return &testClient{t: t, c: c, r: bufio.NewReader(c), w: bufio.NewWriter(c), prot: 2}
}

// send writes a command and returns the decoded reply. Errors come back as respError.
func (tc *testClient) send(args ...string) any {
	tc.t.Helper()
	if _, err := tc.w.Write(appendRESPArray(nil, args...)); err != nil {
		tc.t.Fatalf("write %v: %v", args, err)
	}
	if err := tc.w.Flush(); err != nil {
		tc.t.Fatalf("flush %v: %v", args, err)
	}
	v, err := readReply(tc.r)
	if err != nil {
		tc.t.Fatalf("reply to %v: %v", args, err)
	}
	return v
}

// sendRaw writes bytes verbatim, for protocol-level tests.
func (tc *testClient) sendRaw(b []byte) {
	tc.t.Helper()
	if _, err := tc.w.Write(b); err != nil {
		tc.t.Fatalf("write: %v", err)
	}
	if err := tc.w.Flush(); err != nil {
		tc.t.Fatalf("flush: %v", err)
	}
}

type respError string

// respMap is what a RESP3 `%` frame decodes to, kept distinct from a plain array so a test can
// tell the two apart — which is the whole point of decoding the wire here.
type respMap []any

func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("reply is not CRLF-terminated: %q", line)
	}
	body := line[1 : len(line)-2]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return respError(body), nil
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '_':
		return nil, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*', '%':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		items := n
		if line[0] == '%' {
			items = n * 2
		}
		out := make([]any, 0, items)
		for i := 0; i < items; i++ {
			v, err := readReply(r)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if line[0] == '%' {
			return respMap(out), nil
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown reply type %q", line[0])
}

// --- fixtures ----------------------------------------------------------------------------

func startKVForTest(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	if opts.Port == 0 {
		opts.Port = freeTestKVPort(t)
	}
	if opts.Logf == nil {
		opts.Logf = func(format string, args ...any) { t.Logf("kv: "+format, args...) }
	}
	s, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// freeTestKVPort borrows the same OS-port dance the embedded Temporal uses, so a test never
// collides with a developer's own redis on 6379.
func freeTestKVPort(t *testing.T) int {
	t.Helper()
	p, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	return p
}

func wantStr(t *testing.T, got any, want string) {
	t.Helper()
	if s, ok := got.(string); !ok || s != want {
		t.Fatalf("got %#v, want %q", got, want)
	}
}

func wantInt(t *testing.T, got any, want int64) {
	t.Helper()
	if n, ok := got.(int64); !ok || n != want {
		t.Fatalf("got %#v, want %d", got, want)
	}
}

func wantErrContaining(t *testing.T, got any, substr string) {
	t.Helper()
	e, ok := got.(respError)
	if !ok {
		t.Fatalf("got %#v, want an error containing %q", got, substr)
	}
	if !strings.Contains(string(e), substr) {
		t.Fatalf("error %q does not contain %q", string(e), substr)
	}
}

// --- the commands ------------------------------------------------------------------------

// THE ACTOR STATE TIER, end to end: the exact call sequence statekv.py and statekv.go make.
func TestActorStateTierRoundTrip(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	key := "kontra-actor:sess-1"

	wantInt(t, c.send("HSET", key, "u0", `{"ok":true}`), 1)
	wantInt(t, c.send("EXPIRE", key, "86400"), 1)
	wantStr(t, c.send("HGET", key, "u0"), `{"ok":true}`)
	wantInt(t, c.send("HEXISTS", key, "u0"), 1)
	wantInt(t, c.send("HEXISTS", key, "u1"), 0)

	// A re-write of an existing field returns 0 NEW fields, which is what Redis returns and what
	// a caller reading the count would branch on.
	wantInt(t, c.send("HSET", key, "u0", `{"ok":false}`), 0)

	ttl, ok := c.send("TTL", key).(int64)
	if !ok || ttl < 86_390 || ttl > 86_400 {
		t.Fatalf("TTL after EXPIRE 86400 = %v, want ~86400", ttl)
	}

	// A missing field is a NULL, not an empty string. statekv.py's `get` returns its default on
	// None and json.loads("") would raise, so the two are not interchangeable.
	if v := c.send("HGET", key, "nope"); v != nil {
		t.Fatalf("HGET of a missing field = %#v, want nil", v)
	}
	wantInt(t, c.send("HDEL", key, "u0"), 1)
	wantInt(t, c.send("HDEL", key, "u0"), 0)
	// The hash emptied itself out of existence, exactly as Redis's does.
	wantInt(t, c.send("TTL", key), -2)
	wantInt(t, c.send("DEL", key), 0)
}

// TTL's three-valued answer is a contract the operator projection renders directly.
func TestTTLDistinguishesMissingFromEternal(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	wantInt(t, c.send("TTL", "kontra-global:a:absent"), -2)
	wantInt(t, c.send("HSET", "kontra-global:a:k", "data", "v"), 1)
	wantInt(t, c.send("TTL", "kontra-global:a:k"), -1)
	// EXPIRE on a key that is not there does not create one.
	wantInt(t, c.send("EXPIRE", "kontra-global:a:absent", "60"), 0)
}

// HGETALL is a MAP on RESP3 and a flat array on RESP2, and the difference is on the wire rather
// than in a client's coercion. ioredis 6 negotiates RESP3 by default, so the map branch is the
// one stateStore.ts actually meets.
func TestHGETALLFramingFollowsTheNegotiatedProtocol(t *testing.T) {
	s := startKVForTest(t, Options{})
	key := "kontra-actor:sess-2"

	c2 := dialKV(t, s.Address())
	c2.send("HSET", key, "b", "2", "a", "1")
	got := c2.send("HGETALL", key)
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("RESP2 HGETALL = %#v, want a flat array", got)
	}
	// Sorted field order: two identical reads must not look like a change.
	if fmt.Sprint(arr) != "[a 1 b 2]" {
		t.Fatalf("RESP2 HGETALL = %v, want [a 1 b 2]", arr)
	}

	c3 := dialKV(t, s.Address())
	c3.send("HELLO", "3")
	got = c3.send("HGETALL", key)
	m, ok := got.(respMap)
	if !ok {
		t.Fatalf("RESP3 HGETALL = %#v (%T), want a map frame", got, got)
	}
	if fmt.Sprint([]any(m)) != "[a 1 b 2]" {
		t.Fatalf("RESP3 HGETALL = %v, want [a 1 b 2]", []any(m))
	}

	// An empty hash is an empty map, not a null.
	if m, ok := c3.send("HGETALL", "kontra-actor:missing").(respMap); !ok || len(m) != 0 {
		t.Fatalf("HGETALL of a missing key = %#v, want an empty map", m)
	}
}

// A null inside an array is `_` on RESP3 and `$-1` on RESP2. global_state reads `data` and `ver`
// with one HMGET and decides "this key does not exist yet" from the nils, so a null that decoded
// as an empty string would break first-write-wins-on-create.
func TestHMGETNullsAreNullsInBothProtocols(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	got := c.send("HMGET", "kontra-global:a:absent", "data", "ver")
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 || arr[0] != nil || arr[1] != nil {
		t.Fatalf("HMGET of a missing key = %#v, want [nil nil]", got)
	}
	c.send("HSET", "kontra-global:a:k", "data", "payload")
	got = c.send("HMGET", "kontra-global:a:k", "data", "ver")
	arr, _ = got.([]any)
	if len(arr) != 2 || arr[0] != "payload" || arr[1] != nil {
		t.Fatalf("HMGET = %#v, want [payload nil]", got)
	}
}

// --- the refusal, which is the point of the design ---------------------------------------

// AN UNIMPLEMENTED COMMAND MUST NOT PRODUCE SOMETHING THAT LOOKS LIKE DATA.
//
// Every command below has a plausible wrong answer that a client would believe: GET on a hash key
// returns nil in real Redis (stateStore.ts's own header records that bug — "an empty result
// indistinguishable from an empty store"), EXISTS returns 0, LRANGE returns an empty array. All
// of those read as "no state". None of them is true here.
func TestUnsupportedCommandsAreRefusedByName(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	c.send("HSET", "kontra-global:a:k", "data", "payload")

	for _, cmd := range [][]string{
		{"GET", "kontra-global:a:k"},
		{"SET", "kontra-global:a:k", "x"},
		{"KEYS", "*"},
		{"LRANGE", "kontra-global:a:k", "0", "-1"},
		{"SMEMBERS", "kontra-global:a:k"},
		{"INCR", "counter"},
		{"MULTI"},
		{"FLUSHALL"},
		{"SUBSCRIBE", "ch"},
		{"HRANDFIELD", "kontra-global:a:k"},
		{"PERSIST", "kontra-global:a:k"},
	} {
		got := c.send(cmd...)
		wantErrContaining(t, got, "unknown command")
		wantErrContaining(t, got, cmd[0])
	}
	// …and the store is still usable afterwards. A refusal is an answer, not a desync.
	wantStr(t, c.send("HGET", "kontra-global:a:k", "data"), "payload")
}

// EXISTS and TYPE are the two commands here whose only callers are the SDKs' cross-language
// layout suites, and both must be EXACT rather than approximately right. `n, _ := c.Exists(...)`
// reads an error as zero, so a wrong answer to either is one that a suite would believe.
func TestExistsAndTypeAnswerExactly(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	c.send("HSET", "kontra-global:a:k", "data", "payload")

	wantInt(t, c.send("EXISTS", "kontra-global:a:k"), 1)
	wantInt(t, c.send("EXISTS", "kontra-global:a:absent"), 0)
	// Duplicates COUNT, as they do in Redis: `EXISTS k k` asks a question rather than making a
	// mistake worth correcting.
	wantInt(t, c.send("EXISTS", "kontra-global:a:k", "kontra-global:a:k", "kontra-global:a:absent"), 2)

	wantStr(t, c.send("TYPE", "kontra-global:a:k"), "hash")
	wantStr(t, c.send("TYPE", "kontra-global:a:absent"), "none")

	// A key that expired reports gone through both, not merely empty.
	c.send("HSET", "kontra-actor:doomed", "u0", "x")
	c.send("EXPIRE", "kontra-actor:doomed", "60")
	s.keys.mu.Lock()
	s.keys.now = func() time.Time { return time.Now().Add(time.Hour) }
	s.keys.mu.Unlock()
	wantInt(t, c.send("EXISTS", "kontra-actor:doomed"), 0)
	wantStr(t, c.send("TYPE", "kontra-actor:doomed"), "none")
}

// SELECT is the sharpest version of the same rule: `+OK` to `SELECT 5` would serve db 0's keys
// under another database's name, and nothing about the reply would say so.
func TestSelectRefusesEveryDatabaseButZero(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	wantStr(t, c.send("SELECT", "0"), "OK")
	wantErrContaining(t, c.send("SELECT", "1"), "ONE keyspace")
}

// SCAN options that NARROW a result are refused rather than ignored, for the same reason: a
// caller that asked for less and got more has been told something false.
func TestScanRefusesOptionsItDoesNotImplement(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	wantErrContaining(t, c.send("SCAN", "0", "TYPE", "hash"), "unsupported SCAN option")
	wantErrContaining(t, c.send("SCAN", "0", "MATCH"), "syntax error")
}

func TestArityErrorsUseRedisWording(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	wantErrContaining(t, c.send("HGET", "k"), "wrong number of arguments for 'hget' command")
	wantErrContaining(t, c.send("HSET", "k", "f"), "wrong number of arguments for 'hset' command")
	// A field without its value: refused rather than stored with an invented empty value.
	wantErrContaining(t, c.send("HSET", "k", "f", "v", "g"), "wrong number of arguments for 'hset' command")
	wantErrContaining(t, c.send("TTL"), "wrong number of arguments for 'ttl' command")
}

// --- the scripts -------------------------------------------------------------------------

// THE DIGESTS ARE A CROSS-LANGUAGE CONTRACT. Both SDKs compute SHA1 over their own copy of the
// script text and send EVALSHA with it; if this file's copy drifts by one byte, every
// compare-and-set in the fleet falls back to EVAL and is then refused. So the constants are
// pinned against the SDK sources themselves, not against a remembered digest.
func TestScriptDigestsMatchBothSDKSources(t *testing.T) {
	root := repoRoot(t)
	py := readFileForTest(t, filepath.Join(root, "runtime/python/internals/redis_kv.py"))
	go_ := readFileForTest(t, filepath.Join(root, "runtime/go/rediskv/rediskv.go"))

	pyCAS := between(t, py, `_CAS_LUA = """`, `"""`)
	pyPUT := between(t, py, `_PUT_LUA = """`, `"""`)
	goCAS := between(t, go_, "var casScript = redis.NewScript(`", "`)")
	goPUT := between(t, go_, "var putScript = redis.NewScript(`", "`)")

	if pyCAS != goCAS {
		t.Error("the CAS script differs between redis_kv.py and rediskv.go — the two SDKs would " +
			"send different digests for the same operation")
	}
	if pyPUT != goPUT {
		t.Error("the PUT script differs between redis_kv.py and rediskv.go")
	}
	if casLua != pyCAS {
		t.Errorf("kv.go's casLua is not byte-identical to the SDKs':\n got %q\nwant %q", casLua, pyCAS)
	}
	if putLua != pyPUT {
		t.Errorf("kv.go's putLua is not byte-identical to the SDKs':\n got %q\nwant %q", putLua, pyPUT)
	}
}

// The compare-and-set, exercised through the wire the way rediskv.go and redis_kv.py drive it.
func TestGlobalStateCompareAndSet(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	key := "kontra-global:cachebuster:seen"

	// Create: an empty expected etag means "must not exist yet".
	wantInt(t, c.send("EVALSHA", casSHA, "1", key, "first", ""), 1)
	// A second creator LOSES rather than overwriting — first-write-wins-on-create.
	wantInt(t, c.send("EVALSHA", casSHA, "1", key, "second", ""), 0)

	got := c.send("HMGET", key, "data", "ver")
	arr, _ := got.([]any)
	if len(arr) != 2 || arr[0] != "first" || arr[1] != "1" {
		t.Fatalf("after create, HMGET = %#v, want [first 1]", got)
	}

	// A stale etag loses; the current one wins and bumps the version.
	wantInt(t, c.send("EVALSHA", casSHA, "1", key, "third", "99"), 0)
	wantInt(t, c.send("EVALSHA", casSHA, "1", key, "third", "1"), 1)
	arr, _ = c.send("HMGET", key, "data", "ver").([]any)
	if arr[0] != "third" || arr[1] != "2" {
		t.Fatalf("after CAS, HMGET = %#v, want [third 2]", arr)
	}

	// The unconditional write bumps `ver` too, so a CAS holding etag 2 now loses. That bump is
	// the whole reason the Set path is a script rather than an HSET.
	wantInt(t, c.send("EVALSHA", putSHA, "1", key, "clobbered"), 1)
	arr, _ = c.send("HMGET", key, "data", "ver").([]any)
	if arr[0] != "clobbered" || arr[1] != "3" {
		t.Fatalf("after PUT, HMGET = %#v, want [clobbered 3]", arr)
	}
	wantInt(t, c.send("EVALSHA", casSHA, "1", key, "lost", "2"), 0)

	// PUT against a key that does not exist yet creates it at version 1, matching the create
	// branch of the CAS.
	wantInt(t, c.send("EVALSHA", putSHA, "1", "kontra-global:cachebuster:fresh", "x"), 1)
	arr, _ = c.send("HMGET", "kontra-global:cachebuster:fresh", "data", "ver").([]any)
	if arr[0] != "x" || arr[1] != "1" {
		t.Fatalf("PUT on a new key = %#v, want [x 1]", arr)
	}
}

// The compare-and-set must be atomic ON THE SERVER: concurrent writers each read-modify-write the
// same key, and exactly the ones that held the current etag may win. A store that let two
// interleave would let both believe they won, which is the lost update the ETag exists to stop.
func TestCompareAndSetIsAtomicUnderConcurrency(t *testing.T) {
	s := startKVForTest(t, Options{})
	key := "kontra-global:contended:counter"

	seed := dialKV(t, s.Address())
	wantInt(t, seed.send("EVALSHA", casSHA, "1", key, "0", ""), 1)

	const writers, each = 8, 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dialKV(t, s.Address())
			for i := 0; i < each; i++ {
				arr, _ := c.send("HMGET", key, "data", "ver").([]any)
				etag, _ := arr[1].(string)
				n, _ := strconv.Atoi(arr[0].(string))
				if v, ok := c.send("EVALSHA", casSHA, "1", key, strconv.Itoa(n+1), etag).(int64); ok && v == 1 {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	arr, _ := seed.send("HMGET", key, "data", "ver").([]any)
	// Every winner incremented the value by one and bumped the version by one. If two writers had
	// ever interleaved, one increment would be missing while both reported a win.
	if got, want := arr[0], strconv.Itoa(wins); got != want {
		t.Fatalf("data = %v after %d reported wins, want %s — an interleaved CAS lost an update", got, wins, want)
	}
	if got, want := arr[1], strconv.Itoa(wins+1); got != want {
		t.Fatalf("ver = %v, want %s", got, want)
	}
	if wins == writers*each {
		t.Fatal("every write won, so nothing was actually contended — the test proves nothing")
	}
}

// An unknown digest gets NOSCRIPT, which is the one string both SDKs act on: go-redis retries with
// EVAL and redis-py SCRIPT LOADs. The retry then meets the refusal, which names the two digests
// this store has.
func TestUnknownScriptsAreRefusedWithTheirDigest(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())

	wantErrContaining(t, c.send("EVALSHA", strings.Repeat("ab", 20), "1", "k"), "NOSCRIPT")

	body := "return redis.call('DEL', KEYS[1])"
	got := c.send("EVAL", body, "1", "kontra-global:a:k")
	wantErrContaining(t, got, "no interpreter")
	wantErrContaining(t, got, scriptSHA(body))

	// SCRIPT LOAD verifies rather than caches: a body it cannot run is refused at load time
	// instead of being accepted and then refused at EVALSHA.
	wantErrContaining(t, c.send("SCRIPT", "LOAD", body), "no interpreter")
	wantStr(t, c.send("SCRIPT", "LOAD", casLua), casSHA)
	wantStr(t, c.send("SCRIPT", "LOAD", putLua), putSHA)
	wantErrContaining(t, c.send("SCRIPT", "FLUSH"), "not supported")

	// EVAL with the right body but the wrong shape is refused, not approximated.
	wantErrContaining(t, c.send("EVALSHA", casSHA, "2", "a", "b", "c", "d"), "1 key and 2 arguments")
}

// A `ver` that is not a number makes Redis's Lua raise; reproducing that keeps a broken store an
// ERROR rather than a lost race, which is the distinction globalstore's retry loop branches on.
func TestCorruptVersionIsAnErrorNotALostRace(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	key := "kontra-global:a:weird"
	c.send("HSET", key, "data", "x", "ver", "not-a-number")
	wantErrContaining(t, c.send("EVALSHA", casSHA, "1", key, "y", "not-a-number"), "not an integer")
	wantErrContaining(t, c.send("EVALSHA", putSHA, "1", key, "y"), "not an integer")
}

// --- SCAN --------------------------------------------------------------------------------

// The global tier's read path: SCAN with the pattern stateStore.ts builds, walked to completion.
func TestScanWalksTheWholeKeyspaceExactlyOnce(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	for i := 0; i < 250; i++ {
		c.send("HSET", fmt.Sprintf("kontra-global:cachebuster:k%03d", i), "data", "v")
	}
	for i := 0; i < 50; i++ {
		c.send("HSET", fmt.Sprintf("kontra-global:other:k%03d", i), "data", "v")
		c.send("HSET", fmt.Sprintf("kontra-actor:sess-%03d", i), "u0", "v")
	}

	seen := map[string]int{}
	cursor := "0"
	rounds := 0
	for {
		reply, _ := c.send("SCAN", cursor, "MATCH", "kontra-global:cachebuster:*", "COUNT", "50").([]any)
		cursor = reply[0].(string)
		for _, k := range reply[1].([]any) {
			seen[k.(string)]++
		}
		rounds++
		if cursor == "0" || rounds > 100 {
			break
		}
	}
	if len(seen) != 250 {
		t.Fatalf("SCAN matched %d keys, want 250", len(seen))
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("SCAN returned %s %d times", k, n)
		}
	}
	if rounds < 2 {
		t.Fatalf("the whole keyspace came back in %d round(s); COUNT was not honoured as a work budget", rounds)
	}
}

// The escape stateStore.ts applies to caller-supplied ids must survive as an escape. `escapeGlob`
// neutralises `* ? [ ] \` so an actor id cannot widen the pattern that is meant to constrain it —
// and a matcher that ignored the backslash would hand a caller another actor's global state.
func TestScanPatternEscapesConstrainRatherThanWiden(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	c.send("HSET", "kontra-global:a*b:k", "data", "escaped")
	c.send("HSET", "kontra-global:axxb:k", "data", "widened")

	reply, _ := c.send("SCAN", "0", "MATCH", `kontra-global:a\*b:*`, "COUNT", "100").([]any)
	keys, _ := reply[1].([]any)
	if len(keys) != 1 || keys[0] != "kontra-global:a*b:k" {
		t.Fatalf("escaped pattern matched %v, want only the literal key", keys)
	}
}

func TestScanRejectsAMalformedCursor(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	wantErrContaining(t, c.send("SCAN", "zz"), "invalid cursor")
}

// --- the handshake -----------------------------------------------------------------------

func TestHelloNegotiatesAndRefusesAuth(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())

	m, ok := c.send("HELLO").([]any)
	if !ok {
		t.Fatalf("HELLO before negotiation = %#v, want the flat RESP2 pair list", m)
	}
	if fmt.Sprint(m[0]) != "server" || fmt.Sprint(m[1]) != "kontra-appliance" {
		t.Fatalf("HELLO said %v; it must not claim to be redis", m)
	}

	// After HELLO 3 the SAME reply comes back framed as a map, which is the negotiation actually
	// taking effect rather than being acknowledged and ignored.
	if _, ok := c.send("HELLO", "3").(respMap); !ok {
		t.Fatal("HELLO 3 was acknowledged but the connection kept answering in RESP2")
	}
	wantErrContaining(t, c.send("HELLO", "4"), "NOPROTO")
	wantErrContaining(t, c.send("HELLO", "3", "AUTH", "default", "hunter2"), "false claim of authentication")
}

// HELLO on RESP2 is a flat array; the test above reads it through the same decoder, so assert the
// framing explicitly here instead.
func TestHelloFramingIsFlatOnRESP2(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	c.sendRaw(appendRESPArray(nil, "HELLO"))
	line, err := c.r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "*14\r\n" {
		t.Fatalf("RESP2 HELLO framed as %q, want *14 (7 pairs flattened)", line)
	}
}

// ioredis will not call a connection ready until INFO answers with a parseable string carrying
// `loading:0`. Without it the client sits in `connect` forever and every read times out.
func TestInfoAnswersTheReadyCheck(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	info, ok := c.send("INFO").(string)
	if !ok {
		t.Fatalf("INFO = %#v, want a string", info)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(info, "\r\n") {
		if name, value, found := strings.Cut(line, ":"); found {
			fields[name] = value
		}
	}
	if fields["loading"] != "0" {
		t.Fatalf("INFO has loading=%q, want 0 — ioredis's ready check never completes without it", fields["loading"])
	}
	if _, claimed := fields["redis_version"]; claimed {
		t.Fatal("INFO claims a redis_version; this store has twenty commands and must not invite a client to gate on one")
	}
	if !strings.Contains(fields["supported_commands"], "HGETALL") {
		t.Fatalf("INFO does not list what it serves: %q", fields["supported_commands"])
	}

	// A section argument is honoured or refused, never ignored. `INFO kontra` answering with the
	// whole report would read as "there is no such section, here is everything" — the same class
	// of silently-wrong answer this store exists to avoid.
	one, ok := c.send("INFO", "kontra").(string)
	if !ok || !strings.Contains(one, "# Kontra") {
		t.Fatalf("INFO kontra = %#v", one)
	}
	if strings.Contains(one, "# Persistence") {
		t.Fatal("INFO kontra returned every section; the argument was ignored")
	}
	wantErrContaining(t, c.send("INFO", "replication"), "no INFO section")
}

func TestPingAndQuit(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	wantStr(t, c.send("PING"), "PONG")
	wantStr(t, c.send("PING", "hello"), "hello")
	wantStr(t, c.send("CLIENT", "SETNAME", "probe"), "OK")
	wantStr(t, c.send("CLIENT", "GETNAME"), "probe")
	wantStr(t, c.send("CLIENT", "SETINFO", "LIB-NAME", "ioredis"), "OK")
	wantErrContaining(t, c.send("CLIENT", "KILL", "id", "1"), "not supported")
	wantStr(t, c.send("QUIT"), "OK")
	if _, err := readReply(c.r); !errors.Is(err, io.EOF) {
		t.Fatalf("the connection stayed open after QUIT (err=%v)", err)
	}
}

// A pipeline is one write and one read burst, which is how stateStore.ts's readScan issues N
// HGETs and N TTLs — "so N keys cost one round trip rather than 2N". Replies must come back in
// order and all of them must arrive.
func TestPipelinedCommandsAnswerInOrder(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	var buf []byte
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("kontra-global:a:k%02d", i)
		buf = appendRESPArray(buf, "HSET", key, "data", strconv.Itoa(i))
	}
	c.sendRaw(buf)
	for i := 0; i < 100; i++ {
		if _, err := readReply(c.r); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	buf = buf[:0]
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("kontra-global:a:k%02d", i)
		buf = appendRESPArray(buf, "HGET", key, "data")
		buf = appendRESPArray(buf, "TTL", key)
	}
	c.sendRaw(buf)
	for i := 0; i < 100; i++ {
		v, err := readReply(c.r)
		if err != nil {
			t.Fatalf("HGET reply %d: %v", i, err)
		}
		if v != strconv.Itoa(i) {
			t.Fatalf("pipelined reply %d = %#v, want %d — the replies are out of order", i, v, i)
		}
		if v, err := readReply(c.r); err != nil || v != int64(-1) {
			t.Fatalf("TTL reply %d = %#v (%v)", i, v, err)
		}
	}
}

// A protocol error has no recoverable boundary, so the store says so and hangs up rather than
// resynchronising onto whatever byte comes next.
func TestProtocolErrorClosesTheConnection(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	c.sendRaw([]byte("+PING\r\n"))
	v, err := readReply(c.r)
	if err != nil {
		t.Fatalf("expected an error reply, got %v", err)
	}
	wantErrContaining(t, v, "Protocol error")
	if _, err := readReply(c.r); !errors.Is(err, io.EOF) {
		t.Fatalf("the connection stayed open after a protocol error (err=%v)", err)
	}
}

// --- startup and shutdown ----------------------------------------------------------------

func TestStartKVRefusesRatherThanGuesses(t *testing.T) {
	if _, err := Start(Options{}); err == nil || !strings.Contains(err.Error(), "DataDir is required") {
		t.Fatalf("Start with no DataDir: %v", err)
	}
	if _, err := Start(Options{DataDir: t.TempDir(), BindIP: "localhost"}); err == nil ||
		!strings.Contains(err.Error(), "is not an IP") {
		t.Fatalf("Start with a hostname bind: %v", err)
	}
}

// The port collision names the likely culprit. 6379 is the port a developer's own redis-server
// already owns, which makes this the most-fired of the three appliance port checks.
func TestPortCollisionNamesTheOtherStore(t *testing.T) {
	s := startKVForTest(t, Options{})
	_, port, _ := net.SplitHostPort(s.Address())
	p, _ := strconv.Atoi(port)
	_, err := Start(Options{DataDir: t.TempDir(), Port: p})
	if err == nil {
		t.Fatal("a second store bound the same port")
	}
	if !strings.Contains(err.Error(), "redis") || !strings.Contains(err.Error(), port) {
		t.Fatalf("collision error does not name the port or the likely culprit: %v", err)
	}
}

// A connection drop must arrive as a closed socket, which is what makes ioredis emit `'error'` on
// its EventEmitter rather than hang. stateStore.ts registers a listener for exactly that; the
// Node half of this is asserted in kv_clients_test.go.
func TestStopDropsConnectionsImmediately(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)
	s := startKVForTest(t, Options{DataDir: dir, Port: port})
	c := dialKV(t, s.Address())
	wantStr(t, c.send("PING"), "PONG")

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readReply(c.r); err == nil {
		t.Fatal("the connection survived Stop")
	}
	// Stop is idempotent, which matters because `kontra up` unwinds its services on any one of
	// them failing to start.
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// --- helpers -----------------------------------------------------------------------------

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("could not find %q", open)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("could not find %q after %q", close, open)
	}
	return rest[:j]
}

// repoRoot walks up from the test's directory to the checkout root, so a test can read the SDK
// sources this package has a contract with.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "CONTEXT-MAP.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("not running inside the kontra checkout")
	return ""
}
