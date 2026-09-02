// kv.go — the key-value store the appliance serves in place of the `redis` container
// (ADR 0031, issue 06). `keyspace.go` is the data; `kvlog.go` is the durability; this file is the
// wire and the refusal.
//
// WHY IT SPEAKS RESP INSTEAD OF BEING AN INTERFACE. The orchestrator stays a Node child process
// and `backend/src/stateStore.ts` talks to this tier through `ioredis` — with a `lazyConnect`
// dance, `enableOfflineQueue: false`, a `retryStrategy`, and an `'error'` listener that exists
// because an unhandled EventEmitter error kills the API. Two SDKs reach the same tier from
// actor processes that may not even be on this machine (actorkit's statekv and rediskv, in Python
// and Go). Speaking the protocol leaves all of that untouched, which is what makes this a
// SUBSTITUTION rather than a rewrite of the global state tier: nothing above the socket changes,
// so nothing above the socket has to be re-proven.
//
// # The command set, and why it is exactly this long
//
// Every command below has a caller in this repository, and the list was taken from those callers
// rather than from Redis's command table:
//
//	HGET HGETALL TTL SCAN              backend/src/stateStore.ts (the operator's read path)
//	HGET HSET HDEL HEXISTS EXPIRE DEL   actorkit statekv.py / statekv.go (tiers 1+2)
//	HMGET EVALSHA EVAL SCRIPT LOAD      actorkit redis_kv.py / rediskv.go (tier 3's ETag CAS)
//	EXISTS TYPE                         the SDKs' cross-language layout suites, which assert the
//	                                    exact key shape both tiers agree on
//	HELLO PING QUIT SELECT CLIENT INFO  the client libraries' own handshakes, not kontra's code
//
// EXISTS and TYPE are the two whose only callers are TESTS, and they are in for a reason worth
// stating: runtime/go's rediskv and statekv suites are the things that pin `kontra-actor:<id>`
// and `kontra-global:<actor>:<key>` across three languages, and refusing two exactly-implementable
// commands would mean those suites could never be run against the appliance at all. Worse, they
// spell it `n, _ := c.Exists(...)` — an error there reads as zero, so a refusal would have made
// "Drop left the actor hash behind" pass without ever asking.
//
// ANYTHING ELSE IS AN ERROR NAMING ITSELF. That is the load-bearing decision in this file. A
// store that answers `LPUSH` with `:0`, or `GET` on a hash key with a nil, has told a caller
// something false in a shape it will believe — and "the value is not there" is indistinguishable
// from "this store does not do that" exactly when it matters most. `stateStore.ts`'s own header
// records the same bug from the other side: a plain GET against a global key returned nil, which
// older code read as "no state", "an empty result indistinguishable from an empty store".
//
// # The scripts are an allowlist, not an interpreter
//
// Tier 3's compare-and-set is a Lua script, because it must be atomic ON THE SERVER — a
// read-then-write from the client is the lost-update race the ETag exists to prevent. There is no
// Lua interpreter in here and there will not be one: the two scripts are matched by the SHA1 of
// their exact bodies (the same identity EVALSHA already uses) and executed as Go, under the same
// single lock every other command takes. A script that is not one of the two is refused with its
// digest. Guessing at an unknown script's semantics is the same failure as guessing at an unknown
// command's, with a bigger blast radius.
//
// # What this is not
//
// No replication, no pub/sub, no cluster, no keyspace notifications, no MULTI. Nothing in kontra
// uses them. `kontra-global` is a shared tier across a fleet, and the sharing is done by pointing
// every Machine's KONTRA_REDIS_HOST at one Controller — the same topology the container had.
package kv

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPort is Redis's port, and it is deliberately the SAME number the compose service
// published — for the reason temporalsrv.DefaultPort and objstore.DefaultPort are kept.
// `KONTRA_REDIS_HOST` defaults to `localhost:6379` in both actorkit SDKs and `redis:6379` in
// compose; a store on another port
// is one every existing client fails to find.
const DefaultPort = 6379

// kvSyncInterval is how often the log is fsynced. See kvlog.go's `sync`.
const kvSyncInterval = time.Second

// Options configures the embedded key-value store. DataDir is the only required field.
type Options struct {
	// DataDir is the directory the store owns; the log lives at DataDir/kv/keyspace.log.
	// Required, for the reason temporalsrv.Options.DataDir and objstore.Options.DataDir are:
	// `redis-data` was a named volume so this tier survived a recreate, and a store whose
	// location is
	// defaulted somewhere is one an operator cannot find or back up.
	DataDir string

	// BindIP is the address to listen on. Empty means 127.0.0.1 (ADR 0031 §3). A fleet needs the
	// controller's VPC address here — the same decision `${KONTRA_REDIS_BIND}` encoded in
	// compose, and for the same reason: this store is unauthenticated and the VPC is the
	// boundary. It is never 0.0.0.0 by default.
	BindIP string

	// Port is the port to listen on; 0 means DefaultPort.
	Port int

	// MaxBytes bounds the DATASET, carried from `--maxmemory 512mb`. 0 means DefaultMaxBytes;
	// negative means unbounded. See keyspace.go's makeRoom for what happens at the ceiling and
	// which keys are exempt from eviction.
	MaxBytes int64

	// Logf receives what the store could not serve, plus evictions and log truncations. nil
	// means stderr. It exists for the same reason objstore.Options.Logf does: a failure nothing
	// logs is a failure nobody knows about.
	Logf func(format string, args ...any)
}

// Server is a started store. It is returned already listening and already holding whatever the
// last run left behind.
type Server struct {
	keys *keyspace
	log  *kvLog

	ln      net.Listener
	address string
	dataDir string
	logf    func(string, ...any)

	done     chan struct{}
	stopOnce sync.Once

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	clientIDs atomic.Uint64
	commands  atomic.Uint64
	refused   atomic.Uint64
	// resp3 counts connections that negotiated RESP3. It is a real operational fact — ioredis 6
	// and redis-py 8 default to protocol 3 and go-redis falls back to 2, so this is how an
	// operator sees WHICH protocol their clients are actually on — and it is what lets a test
	// prove the negotiation happened rather than assuming it did.
	resp3 atomic.Uint64
}

// Address is host:port — what goes in KONTRA_REDIS_HOST.
func (s *Server) Address() string { return s.address }

// DataDir is the directory holding the log. Named so an operator can find the thing to back up
// and so a test can assert state survived a restart.
func (s *Server) DataDir() string { return s.dataDir }

// LogPath is the append-only file itself.
func (s *Server) LogPath() string { return s.log.path }

// Keys reports how many live keys the store holds. Exported because "is my state actually in
// there" is the first question of every incident, and answering it should not need a client.
func (s *Server) Keys() int {
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	n := 0
	for key := range s.keys.keys {
		if s.keys.live(key) != nil {
			n++
		}
	}
	return n
}

// Start boots the store and returns once it is listening, with the previous run's keyspace
// already replayed — so the first command after a restart sees the state the last one wrote.
func Start(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, errors.New("appliance: DataDir is required (the embedded key-value store owns its own data directory)")
	}
	if opts.BindIP == "" {
		opts.BindIP = "127.0.0.1"
	}
	if net.ParseIP(opts.BindIP) == nil {
		return nil, fmt.Errorf("appliance: bind address %q is not an IP (use 127.0.0.1, not a hostname)", opts.BindIP)
	}
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "kontra-kv: "+format+"\n", args...) }
	}

	dir := filepath.Join(opts.DataDir, "kv")
	log, err := openKVLog(dir, logf)
	if err != nil {
		return nil, err
	}
	keys := newKeyspace(log, opts.MaxBytes, logf)
	applied, err := replayKVLog(log, keys)
	if err != nil {
		log.close()
		return nil, err
	}
	// Compact on the way up. The replay has just built the shortest keyspace the log describes,
	// so writing it back makes the NEXT boot cheap and drops everything that expired while the
	// appliance was down — which is also what makes a torn tail a one-time event rather than a
	// warning on every start.
	if applied > 0 {
		keys.mu.Lock()
		err := log.rewrite(keys.snapshot())
		keys.mu.Unlock()
		if err != nil {
			log.close()
			return nil, err
		}
	}

	addr := hostPort(opts.BindIP, opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// THE PORT CHECK IS THE ERROR MESSAGE, as it is for the embedded Temporal and the object
		// store — and here it is the likeliest of the three to fire, because 6379 is the port a
		// developer's own local Redis already owns. "The appliance is broken" and "you are
		// running two key-value stores" must not look the same.
		log.close()
		return nil, fmt.Errorf("appliance: cannot listen on %s for the key-value store — "+
			"another process (a local redis-server, or the compose `redis` service?) has it: %w", addr, err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		addr = hostPort(opts.BindIP, tcp.Port)
	}

	s := &Server{
		keys: keys, log: log, ln: ln, address: addr, dataDir: dir, logf: logf,
		done: make(chan struct{}), conns: map[net.Conn]struct{}{},
	}
	go s.maintain()
	go s.accept()
	return s, nil
}

// Stop closes the listener, drops every connection, and leaves the log fsynced and compacted.
//
// Connections are CLOSED rather than drained. Redis's protocol has no goodbye and every client
// here treats a dropped socket as a reconnect — ioredis emits `'error'` on the EventEmitter,
// which is precisely what stateStore.ts's listener is for. Waiting on an idle keep-alive from a
// dashboard nobody is looking at would make shutdown hang for no benefit.
func (s *Server) Stop() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.done)
		err = s.ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.conns = map[net.Conn]struct{}{}
		s.mu.Unlock()

		s.keys.mu.Lock()
		rerr := s.log.rewrite(s.keys.snapshot())
		s.keys.mu.Unlock()
		if rerr != nil {
			s.logf("%v", rerr)
		}
		if cerr := s.log.close(); cerr != nil && err == nil {
			err = cerr
		}
	})
	return err
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
			default:
				s.logf("listener stopped: %v", err)
			}
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serve(c)
	}
}

// maintain runs the fsync cadence and the log rewrite trigger. Both are here rather than on the
// command path so a write never waits on a disk the reply does not need.
func (s *Server) maintain() {
	t := time.NewTicker(kvSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if err := s.log.sync(); err != nil {
				s.logf("%v", err)
			}
			if s.log.shouldRewrite() {
				s.keys.mu.Lock()
				// Re-checked under the lock: Stop takes the same lock to do its own final
				// rewrite, and a compaction that started in the gap would find a closed log and
				// report a failure that is really just a shutdown.
				select {
				case <-s.done:
					s.keys.mu.Unlock()
					return
				default:
				}
				err := s.log.rewrite(s.keys.snapshot())
				s.keys.mu.Unlock()
				if err != nil {
					s.logf("%v", err)
				}
			}
		}
	}
}

// --- the connection ----------------------------------------------------------------------

// conn is one client. `proto` is per-connection because HELLO negotiates it per-connection, which
// is how a Node client on RESP3 and a Go client that fell back to RESP2 share this process.
type conn struct {
	s    *Server
	c    net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	id   uint64
	prot int
	name string
}

func (s *Server) serve(c net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	// A panic on one connection must not take the appliance down with it — the store shares a
	// process with Temporal and the object store now, which the container it replaces did not.
	defer func() {
		if r := recover(); r != nil {
			s.logf("connection from %v panicked: %v", c.RemoteAddr(), r)
		}
	}()

	cn := &conn{
		s: s, c: c,
		r:    bufio.NewReaderSize(c, 32<<10),
		w:    bufio.NewWriterSize(c, 32<<10),
		id:   s.clientIDs.Add(1),
		prot: 2,
	}
	for {
		args, _, err := readRESPRecord(cn.r)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, errTornTail) || isClosedConn(err) {
				return
			}
			// A protocol error is not recoverable mid-stream: the reader has no way to find the
			// next record boundary. Say so and hang up, which is what Redis does.
			cn.writeError("ERR Protocol error: %s", err)
			cn.flush()
			return
		}
		if len(args) == 0 {
			continue
		}
		if quit := cn.dispatch(args); quit {
			cn.flush()
			return
		}
		// Flush only at the end of a pipeline burst, and flush the LOG FIRST. A reply that
		// reaches the client before the record reaches the kernel is an acknowledgement of a
		// write that a crash one instruction later would lose.
		if cn.r.Buffered() == 0 {
			cn.flush()
		}
	}
}

func (cn *conn) flush() {
	if err := cn.s.log.flush(); err != nil {
		cn.s.logf("%v", err)
	}
	if err := cn.w.Flush(); err != nil && !isClosedConn(err) {
		cn.s.logf("writing to %v: %v", cn.c.RemoteAddr(), err)
	}
}

func isClosedConn(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "broken pipe")
}

// --- replies -----------------------------------------------------------------------------
//
// RESP2 and RESP3 differ, for this command set, in exactly two places: how a null is spelled and
// how a map is framed. Both are written through here so no handler has to know which protocol the
// connection negotiated — the way a handler would eventually get it wrong.

func (cn *conn) writeSimple(s string) { cn.w.WriteString("+" + s + "\r\n") }

// writeError writes a RESP error. The message must be one line; the format string is checked for
// that at the seam rather than trusted at every call site, because a CRLF inside an error desyncs
// the protocol and the symptom shows up as a wrong reply to a LATER command.
func (cn *conn) writeError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r", " "), "\n", " ")
	cn.w.WriteString("-" + msg + "\r\n")
}

func (cn *conn) writeInt(n int64) {
	cn.w.WriteString(":")
	cn.w.WriteString(strconv.FormatInt(n, 10))
	cn.w.WriteString("\r\n")
}

func (cn *conn) writeBulk(s string) {
	cn.w.WriteString("$")
	cn.w.WriteString(strconv.Itoa(len(s)))
	cn.w.WriteString("\r\n")
	cn.w.WriteString(s)
	cn.w.WriteString("\r\n")
}

func (cn *conn) writeNull() {
	if cn.prot >= 3 {
		cn.w.WriteString("_\r\n")
		return
	}
	cn.w.WriteString("$-1\r\n")
}

func (cn *conn) writeArrayHeader(n int) {
	cn.w.WriteString("*")
	cn.w.WriteString(strconv.Itoa(n))
	cn.w.WriteString("\r\n")
}

// writeMapHeader frames `n` FIELD/VALUE PAIRS. RESP3 has a map type; RESP2 flattens to an array
// of 2n items, which is what HGETALL has always looked like there.
func (cn *conn) writeMapHeader(n int) {
	if cn.prot >= 3 {
		cn.w.WriteString("%")
		cn.w.WriteString(strconv.Itoa(n))
		cn.w.WriteString("\r\n")
		return
	}
	cn.writeArrayHeader(2 * n)
}

// --- dispatch ----------------------------------------------------------------------------

// dispatch runs one command and reports whether the connection should close afterwards.
func (cn *conn) dispatch(args []string) bool {
	cn.s.commands.Add(1)
	name := strings.ToUpper(args[0])
	a := args[1:]

	switch name {
	// ---- the hash tier: actor state (tiers 1+2) and global_state's ETag hashes (tier 3) ----
	case "HGET":
		if !cn.arity(name, a, 2, 2) {
			return false
		}
		if v, ok := cn.s.keys.hget(a[0], a[1]); ok {
			cn.writeBulk(v)
		} else {
			cn.writeNull()
		}
	case "HGETALL":
		if !cn.arity(name, a, 1, 1) {
			return false
		}
		fields := cn.s.keys.hgetall(a[0])
		cn.writeMapHeader(len(fields))
		for _, f := range fields {
			cn.writeBulk(f.Field)
			cn.writeBulk(f.Value)
		}
	case "HMGET":
		if !cn.arity(name, a, 2, -1) {
			return false
		}
		vals := cn.s.keys.hmget(a[0], a[1:])
		cn.writeArrayHeader(len(vals))
		for _, v := range vals {
			if v == nil {
				cn.writeNull()
			} else {
				cn.writeBulk(*v)
			}
		}
	case "HSET":
		// key plus at least one field/value pair, so an ODD count. An even one means a field
		// arrived without its value, and inventing an empty one for it would store a lie.
		if len(a) < 3 || len(a)%2 == 0 {
			cn.writeError("ERR wrong number of arguments for 'hset' command")
			return false
		}
		pairs := make([]kvField, 0, (len(a)-1)/2)
		for i := 1; i < len(a); i += 2 {
			pairs = append(pairs, kvField{a[i], a[i+1]})
		}
		n, err := cn.s.keys.hset(a[0], pairs)
		cn.intOrError(int64(n), err)
	case "HDEL":
		if !cn.arity(name, a, 2, -1) {
			return false
		}
		n, err := cn.s.keys.hdel(a[0], a[1:])
		cn.intOrError(int64(n), err)
	case "HEXISTS":
		if !cn.arity(name, a, 2, 2) {
			return false
		}
		cn.writeInt(boolToInt(cn.s.keys.hexists(a[0], a[1])))
	case "DEL":
		if !cn.arity(name, a, 1, -1) {
			return false
		}
		n, err := cn.s.keys.del(a)
		cn.intOrError(int64(n), err)
	case "EXISTS":
		if !cn.arity(name, a, 1, -1) {
			return false
		}
		cn.writeInt(cn.s.keys.exists(a))
	case "TYPE":
		if !cn.arity(name, a, 1, 1) {
			return false
		}
		cn.writeSimple(cn.s.keys.typeOf(a[0]))
	case "EXPIRE":
		if !cn.arity(name, a, 2, 2) {
			return false
		}
		secs, err := strconv.ParseInt(a[1], 10, 64)
		if err != nil {
			cn.writeError("ERR value is not an integer or out of range")
			return false
		}
		ok, err := cn.s.keys.expire(a[0], secs)
		cn.intOrError(boolToInt(ok), err)
	case "TTL":
		if !cn.arity(name, a, 1, 1) {
			return false
		}
		cn.writeInt(cn.s.keys.ttl(a[0]))
	case "SCAN":
		cn.scan(a)

	// ---- global_state's compare-and-set, as an allowlist of two scripts ----
	case "EVAL":
		cn.eval(a, false)
	case "EVALSHA":
		cn.eval(a, true)
	case "SCRIPT":
		cn.script(a)

	// ---- the client libraries' own handshakes ----
	case "HELLO":
		cn.hello(a)
	case "PING":
		switch len(a) {
		case 0:
			cn.writeSimple("PONG")
		case 1:
			cn.writeBulk(a[0])
		default:
			cn.writeError("ERR wrong number of arguments for 'ping' command")
		}
	case "QUIT":
		cn.writeSimple("OK")
		return true
	case "SELECT":
		if !cn.arity(name, a, 1, 1) {
			return false
		}
		// ONE KEYSPACE, and SELECT 1 is refused rather than accepted-and-ignored. Numbered
		// databases are a Redis feature kontra has never used — every client here connects with
		// db 0 — and a store that answered OK to `SELECT 5` and then served db 0 would give a
		// caller its own keyspace's data under another name.
		if a[0] != "0" {
			cn.writeError("ERR DB index is out of range: this store has ONE keyspace (db 0). "+
				"kontra has never used numbered databases and answering OK here would serve db 0's data as db %s", a[0])
			return false
		}
		cn.writeSimple("OK")
	case "CLIENT":
		cn.client(a)
	case "INFO":
		if len(a) > 1 {
			cn.writeError("ERR wrong number of arguments for 'info' command")
			return false
		}
		body, err := cn.s.info(a...)
		if err != nil {
			cn.writeError("ERR %s", err)
			return false
		}
		cn.writeBulk(body)

	default:
		cn.s.refused.Add(1)
		cn.writeError("ERR unknown command '%s' — the kontra appliance's embedded key-value store "+
			"(cli/appliance/kv/kv.go) implements only the commands kontra's own clients issue: "+
			"%s. It refuses rather than answering something that would look like data", args[0],
			strings.Join(supportedCommands, " "))
	}
	return false
}

// supportedCommands is the refusal message's own list, and it is the list a `redis-cli info` shows
// too — so "what does this thing implement" has one answer in one place.
var supportedCommands = []string{
	"HGET", "HGETALL", "HMGET", "HSET", "HDEL", "HEXISTS", "DEL", "EXISTS", "TYPE", "EXPIRE",
	"TTL", "SCAN", "EVAL", "EVALSHA", "SCRIPT LOAD", "HELLO", "PING", "QUIT", "SELECT",
	"CLIENT", "INFO",
}

// arity checks argument counts the way Redis does — max of -1 means "no maximum" — and writes
// Redis's exact wording, because both SDKs surface that string to a developer verbatim.
func (cn *conn) arity(name string, a []string, min, max int) bool {
	if len(a) < min || (max >= 0 && len(a) > max) {
		cn.writeError("ERR wrong number of arguments for '%s' command", strings.ToLower(name))
		return false
	}
	return true
}

func (cn *conn) intOrError(n int64, err error) {
	if err != nil {
		if errors.Is(err, errOOM) {
			cn.writeError("%s", err)
			return
		}
		cn.s.logf("%v", err)
		cn.writeError("ERR the embedded key-value store could not complete the write: %s", err)
		return
	}
	cn.writeInt(n)
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// scan implements SCAN cursor [MATCH pattern] [COUNT n].
//
// TYPE and NOVALUES are NOT accepted. stateStore.ts issues MATCH and COUNT and nothing else, and
// a silently ignored TYPE filter would widen a scan the caller believes it narrowed.
func (cn *conn) scan(a []string) {
	if len(a) < 1 {
		cn.writeError("ERR wrong number of arguments for 'scan' command")
		return
	}
	match, count := "", 10
	for i := 1; i < len(a); i += 2 {
		if i+1 >= len(a) {
			cn.writeError("ERR syntax error")
			return
		}
		switch strings.ToUpper(a[i]) {
		case "MATCH":
			match = a[i+1]
		case "COUNT":
			n, err := strconv.Atoi(a[i+1])
			if err != nil || n < 1 {
				cn.writeError("ERR value is not an integer or out of range")
				return
			}
			count = n
		default:
			cn.writeError("ERR unsupported SCAN option %q — this store implements MATCH and COUNT; "+
				"ignoring an option that narrows a scan would return more keys than the caller asked for", a[i])
			return
		}
	}
	next, keys, err := cn.s.keys.scan(a[0], match, count)
	if err != nil {
		cn.writeError("ERR %s", err)
		return
	}
	cn.writeArrayHeader(2)
	cn.writeBulk(next)
	cn.writeArrayHeader(len(keys))
	for _, k := range keys {
		cn.writeBulk(k)
	}
}

// --- HELLO, and the rest of the handshake ------------------------------------------------

// hello negotiates the protocol version, and it is IMPLEMENTED rather than refused because two of
// the three clients would not connect otherwise.
//
// MEASURED against the versions this repo pins: ioredis 6.0.0 defaults to `protocol: 3` and
// redis-py 8.1.0's DEFAULT_RESP_VERSION is 3. ioredis treats a HELLO error as a downgrade signal
// and retries on RESP2, and go-redis v9 falls back the same way — but redis-py's `on_connect`
// reads the HELLO reply with no try/except around it, so an error there is a ConnectionError and
// every Python actor fails to reach its state store. Answering HELLO is not politeness; it is the
// difference between a working fleet and one that cannot commit a unit.
func (cn *conn) hello(a []string) {
	if len(a) > 0 {
		v, err := strconv.Atoi(a[0])
		if err != nil {
			cn.writeError("NOPROTO unsupported protocol version")
			return
		}
		if v != 2 && v != 3 {
			cn.writeError("NOPROTO unsupported protocol version")
			return
		}
		// AUTH inside HELLO is refused rather than ignored: this store is unauthenticated (the
		// binding is the boundary, exactly as it was for the container's published port), and
		// accepting a credential it does not check would tell a caller it had authenticated.
		if len(a) > 1 {
			cn.writeError("ERR HELLO with AUTH is not supported — this store authenticates nobody " +
				"(ADR 0031 §1a: the bind address is the boundary), and accepting a credential it " +
				"does not verify would be a false claim of authentication")
			return
		}
		cn.prot = v
		if v >= 3 {
			cn.s.resp3.Add(1)
		}
	}
	cn.writeMapHeader(7)
	cn.writeBulk("server")
	cn.writeBulk("kontra-appliance")
	cn.writeBulk("version")
	cn.writeBulk(kvVersion)
	cn.writeBulk("proto")
	cn.writeInt(int64(cn.prot))
	cn.writeBulk("id")
	cn.writeInt(int64(cn.id))
	cn.writeBulk("mode")
	cn.writeBulk("standalone")
	cn.writeBulk("role")
	cn.writeBulk("master")
	cn.writeBulk("modules")
	cn.writeArrayHeader(0)
}

// kvVersion is this store's own version, reported by HELLO and INFO. It is NOT a Redis version:
// claiming one would invite a client to use a command this store does not have.
const kvVersion = "1.0.0"

// client implements the CLIENT subcommands the client libraries send unprompted.
//
// SETINFO and SETNAME are connection metadata — a label this store records and shows in INFO, and
// answering OK to them is truthful because recording the label is the whole of what they ask for.
// Every other subcommand is refused; CLIENT KILL and CLIENT NO-EVICT mean things here that they
// do not mean in Redis, and an OK to either would be a lie about what happened.
func (cn *conn) client(a []string) {
	if len(a) == 0 {
		cn.writeError("ERR wrong number of arguments for 'client' command")
		return
	}
	switch strings.ToUpper(a[0]) {
	case "SETNAME":
		if len(a) != 2 {
			cn.writeError("ERR wrong number of arguments for 'client|setname' command")
			return
		}
		cn.name = a[1]
		cn.writeSimple("OK")
	case "SETINFO":
		if len(a) != 3 {
			cn.writeError("ERR wrong number of arguments for 'client|setinfo' command")
			return
		}
		cn.writeSimple("OK")
	case "GETNAME":
		if cn.name == "" {
			cn.writeNull()
			return
		}
		cn.writeBulk(cn.name)
	case "ID":
		cn.writeInt(int64(cn.id))
	default:
		cn.s.refused.Add(1)
		cn.writeError("ERR CLIENT %s is not supported by the kontra appliance's key-value store — "+
			"it implements SETNAME, SETINFO, GETNAME and ID, which are the ones the client libraries send", a[0])
	}
}

// info answers the ready check and the operator.
//
// ioredis's `enableReadyCheck` sends INFO before it will call a connection ready, and parses
// `loading` out of it — so this has to exist and has to be a string. `redis_version` is
// deliberately ABSENT: this store has twenty commands, and a client that gated a feature on a
// version number here would be gating on a lie. What is present instead is the list of commands
// it actually serves, so `redis-cli info` answers "what is this" without reading the source.
func (s *Server) info(section ...string) (string, error) {
	s.keys.mu.Lock()
	keys, bytes, maxBytes, evicted := len(s.keys.keys), s.keys.bytes, s.keys.maxBytes, s.keys.evicted
	s.keys.mu.Unlock()
	s.mu.Lock()
	conns := len(s.conns)
	s.mu.Unlock()

	sections := []struct {
		name, body string
	}{
		{"Server", fmt.Sprintf("server_name:kontra-appliance-kv\r\nkontra_kv_version:%s\r\ntcp_port:%s\r\nprocess_id:%d\r\n",
			kvVersion, portOf(s.address), os.Getpid())},
		{"Clients", fmt.Sprintf("connected_clients:%d\r\nresp3_negotiations:%d\r\n", conns, s.resp3.Load())},
		{"Memory", fmt.Sprintf("used_memory:%d\r\nmaxmemory:%d\r\nmaxmemory_policy:volatile-ttl\r\nevicted_keys:%d\r\n",
			bytes, maxBytes, evicted)},
		{"Persistence", fmt.Sprintf("loading:0\r\naof_enabled:1\r\naof_path:%s\r\n", s.log.path)},
		{"Keyspace", fmt.Sprintf("db0:keys=%d\r\n", keys)},
		{"Kontra", fmt.Sprintf("commands_processed:%d\r\ncommands_refused:%d\r\nsupported_commands:%s\r\n",
			s.commands.Load(), s.refused.Load(), strings.Join(supportedCommands, ","))},
	}

	// A SECTION ARGUMENT IS HONOURED OR REFUSED, never ignored. `INFO kontra` returning the whole
	// report reads as "there is no such section and here is everything", and the operator who
	// typed a section name they half-remembered would never learn they got the wrong thing.
	want := ""
	if len(section) == 1 {
		want = strings.ToLower(section[0])
	}
	all := want == "" || want == "all" || want == "default" || want == "everything"
	var b strings.Builder
	var names []string
	for _, sec := range sections {
		names = append(names, strings.ToLower(sec.name))
		if all || want == strings.ToLower(sec.name) {
			fmt.Fprintf(&b, "# %s\r\n%s\r\n", sec.name, sec.body)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no INFO section %q — this store reports %s", section[0], strings.Join(names, ", "))
	}
	return b.String(), nil
}

func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return ""
}

// --- the two scripts ---------------------------------------------------------------------

// casLua and putLua are BYTE-IDENTICAL copies of `_CAS_LUA` / `_PUT_LUA` in
// runtime/python/internals/redis_kv.py and `casScript` / `putScript` in
// runtime/go/rediskv/rediskv.go. They are here as their exact source text because the
// SHA1 of that text is the identity EVALSHA arrives with — change a byte and the digest stops
// matching, which fails loudly at the first CAS rather than quietly executing the wrong thing.
const casLua = `
local cur = redis.call('HGET', KEYS[1], 'ver')
if ARGV[2] == '' then
  if cur then return 0 end
  redis.call('HSET', KEYS[1], 'data', ARGV[1], 'ver', '1')
  return 1
end
if not cur or cur ~= ARGV[2] then return 0 end
redis.call('HSET', KEYS[1], 'data', ARGV[1], 'ver', tostring(tonumber(cur) + 1))
return 1
`

const putLua = `
redis.call('HSET', KEYS[1], 'data', ARGV[1])
redis.call('HINCRBY', KEYS[1], 'ver', 1)
return 1
`

var (
	casSHA = scriptSHA(casLua)
	putSHA = scriptSHA(putLua)
)

func scriptSHA(body string) string {
	sum := sha1.Sum([]byte(body))
	return hex.EncodeToString(sum[:])
}

// eval runs EVAL or EVALSHA. `byDigest` selects which of the two the first argument is.
func (cn *conn) eval(a []string, byDigest bool) {
	name := "eval"
	if byDigest {
		name = "evalsha"
	}
	if len(a) < 2 {
		cn.writeError("ERR wrong number of arguments for '%s' command", name)
		return
	}
	numKeys, err := strconv.Atoi(a[1])
	if err != nil || numKeys < 0 {
		cn.writeError("ERR value is not an integer or out of range")
		return
	}
	if numKeys > len(a)-2 {
		cn.writeError("ERR Number of keys can't be greater than number of args")
		return
	}
	keys, argv := a[2:2+numKeys], a[2+numKeys:]

	digest := strings.ToLower(a[0])
	if !byDigest {
		digest = scriptSHA(a[0])
	}
	switch digest {
	case casSHA:
		if numKeys != 1 || len(argv) != 2 {
			cn.writeError("ERR the global_state compare-and-set script takes 1 key and 2 arguments, got %d and %d", numKeys, len(argv))
			return
		}
		n, err := cn.s.keys.evalCAS(keys[0], argv[0], argv[1])
		cn.intOrError(n, err)
	case putSHA:
		if numKeys != 1 || len(argv) != 1 {
			cn.writeError("ERR the global_state unconditional-write script takes 1 key and 1 argument, got %d and %d", numKeys, len(argv))
			return
		}
		n, err := cn.s.keys.evalPut(keys[0], argv[0])
		cn.intOrError(n, err)
	default:
		cn.s.refused.Add(1)
		if byDigest {
			// NOSCRIPT is the one error string both SDKs act on: go-redis's Script.Run retries
			// with EVAL, and redis-py's Script catches NoScriptError and SCRIPT LOADs. Sending it
			// for an unknown digest is what lets a client that cached a digest from a REAL Redis
			// resend the body — and then get the refusal below, which names it.
			cn.writeError("NOSCRIPT No matching script. Please use EVAL.")
			return
		}
		cn.writeError("ERR this store runs two scripts and no interpreter: global_state's "+
			"compare-and-set (%s) and its unconditional write (%s). The script sent hashes to %s. "+
			"There is no Lua in cli/appliance/kv/kv.go, so an unknown script is refused rather than approximated",
			casSHA, putSHA, digest)
	}
}

// script implements SCRIPT LOAD, which is how redis-py recovers from the NOSCRIPT above.
//
// It VERIFIES rather than caches. A real Redis stores whatever body it is given; this one
// recomputes the digest and accepts it only if it is one of the two, because "loaded" here would
// otherwise promise an execution that EVALSHA will refuse a millisecond later.
func (cn *conn) script(a []string) {
	if len(a) == 0 {
		cn.writeError("ERR wrong number of arguments for 'script' command")
		return
	}
	if strings.ToUpper(a[0]) != "LOAD" {
		cn.s.refused.Add(1)
		cn.writeError("ERR SCRIPT %s is not supported — this store implements SCRIPT LOAD, which is "+
			"how redis-py recovers from NOSCRIPT; there is no script cache to FLUSH or EXISTS against", a[0])
		return
	}
	if len(a) != 2 {
		cn.writeError("ERR wrong number of arguments for 'script|load' command")
		return
	}
	digest := scriptSHA(a[1])
	if digest != casSHA && digest != putSHA {
		cn.s.refused.Add(1)
		cn.writeError("ERR this store runs two scripts and no interpreter: global_state's "+
			"compare-and-set (%s) and its unconditional write (%s). The script offered hashes to %s",
			casSHA, putSHA, digest)
		return
	}
	cn.writeBulk(digest)
}

// hostPort is net.JoinHostPort with an int, as every listening role in this tree spells it. One
// line, copied rather than shared: the package that would hold it for all of them is the parent,
// and the parent imports the roles, not the other way round.
func hostPort(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }
