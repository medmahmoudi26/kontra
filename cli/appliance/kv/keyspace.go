// keyspace.go — the data model behind the embedded key-value store, in place of the `redis`
// container (ADR 0031, issue 06). `kv.go` is the wire; this file is the keyspace.
//
// WHAT KONTRA ACTUALLY STORES, which is the whole reason this is 400 lines and not a Redis:
//
//   - `kontra-actor:<actorId>` — ONE HASH per actor id, one field per state key, with a 24 h TTL
//     on the hash (runtime/{python/internals,go}/statekv). Tiers 1 and 2.
//   - `kontra-global:<actor>:<key>` — one hash per entry, fields `data` and `ver`, NO TTL
//     (actorkit's redis_kv.py / rediskv.go). Tier 3, and `ver` is the ETag the compare-and-set
//     turns on.
//
// Two shapes, and both of them are hashes. There are no strings, no lists, no sets and no sorted
// sets in kontra's keyspace, so there are none here — an unimplemented type cannot be silently
// mis-served if the model has no room for it (see `kv.go`'s dispatch, which refuses by name).
//
// EVERY MUTATION IS LOGGED BEFORE IT IS ANSWERED. The keyspace is memory; `kvlog.go` is the
// durability, and the two are kept in lockstep here rather than in the command handlers, so a
// future command cannot forget. `redis-data` was a named volume precisely so the shared
// global_state tier survived a recreate, and a store that loses tier 3 on restart is a different
// product, not a faster one.
//
// ONE LOCK, LIKE REDIS'S ONE THREAD. Every method below takes `mu` for its whole duration, which
// is what makes the compare-and-set in `kv.go` atomic ON THE SERVER — the property both SDKs' Lua
// scripts exist to get, and the one a read-then-write from the client cannot have.
package kv

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBytes bounds the dataset, carried from the compose service's `--maxmemory 512mb`.
// It is a bound on DATA, not on the process — the same distinction that file drew.
const DefaultMaxBytes int64 = 512 * 1024 * 1024

// Per-key and per-field overhead added to the raw byte counts, so the accounting approximates
// what the map actually costs rather than what the payloads weigh. Both are estimates and are
// meant to be: the number they feed is a bound to trip, not a measurement to report.
const (
	perKeyOverhead   = 96
	perFieldOverhead = 48
)

// errOOM is the answer to a write that cannot be made room for. It is deliberately a REFUSAL and
// not a silent success: Redis under `maxmemory` with nothing evictable answers exactly this way,
// and the alternative — accepting the write and growing — ends as an OOM kill that takes the
// embedded Temporal and object store down with it.
var errOOM = errors.New("OOM command not allowed when used memory > 'maxmemory'")

// kvHash is one key. Both of kontra's shapes are hashes; see the file header.
type kvHash struct {
	fields map[string]string
	// expireAtMs is an ABSOLUTE deadline in unix milliseconds; 0 means no expiry.
	//
	// Absolute rather than "seconds remaining" because it is what gets logged. A relative EXPIRE
	// replayed from disk an hour after it was issued would slide the TTL forward by an hour every
	// restart, and the 24 h ceiling that keeps abandoned runs from accumulating would stop being
	// a ceiling. Redis rewrites EXPIRE to PEXPIREAT in its AOF for the same reason.
	expireAtMs int64
	// bytes is this key's contribution to the dataset estimate, maintained incrementally so a
	// write never walks the keyspace.
	bytes int64
}

func (h *kvHash) volatile() bool { return h.expireAtMs > 0 }

// keyspace is the whole store: the keys, their durability, and the memory bound.
type keyspace struct {
	mu   sync.Mutex
	keys map[string]*kvHash

	// order is the key set in sorted order, and it is what makes SCAN's cursor mean something.
	// Rebuilt lazily: a sweep with no concurrent key churn sorts once, not once per call.
	order      []string
	orderStale bool

	bytes    int64
	maxBytes int64
	evicted  uint64

	log  *kvLog
	logf func(string, ...any)
	// now is injectable so a TTL test does not have to sleep for 24 hours.
	now func() time.Time
}

func newKeyspace(log *kvLog, maxBytes int64, logf func(string, ...any)) *keyspace {
	return &keyspace{
		keys:     map[string]*kvHash{},
		maxBytes: maxBytes,
		log:      log,
		logf:     logf,
		now:      time.Now,
	}
}

func (k *keyspace) nowMs() int64 { return k.now().UnixNano() / int64(time.Millisecond) }

// --- the model, under the lock -----------------------------------------------------------

// live returns the key if it exists and has not expired, dropping it if it has.
//
// LAZY EXPIRY IS NOT LOGGED, on purpose. The deadline itself is in the log as an absolute time,
// so a replay drops the key by arithmetic; writing a DEL here would double the log's write volume
// for state the replay already discards.
func (k *keyspace) live(key string) *kvHash {
	h, ok := k.keys[key]
	if !ok {
		return nil
	}
	if h.expireAtMs > 0 && h.expireAtMs <= k.nowMs() {
		k.drop(key, h)
		return nil
	}
	return h
}

// drop removes a key from the in-memory model only. Callers that must also record the removal
// append to the log themselves — `del` does, lazy expiry does not.
func (k *keyspace) drop(key string, h *kvHash) {
	delete(k.keys, key)
	k.bytes -= h.bytes
	k.orderStale = true
}

func hashBytes(key string, h *kvHash) int64 {
	n := int64(len(key)) + perKeyOverhead
	for f, v := range h.fields {
		n += int64(len(f)+len(v)) + perFieldOverhead
	}
	return n
}

// --- reads -------------------------------------------------------------------------------

func (k *keyspace) hget(key, field string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.live(key)
	if h == nil {
		return "", false
	}
	v, ok := h.fields[field]
	return v, ok
}

func (k *keyspace) hexists(key, field string) bool {
	_, ok := k.hget(key, field)
	return ok
}

// exists counts how many of the given keys are live, COUNTING DUPLICATES — `EXISTS k k` is 2 in
// Redis, and a caller passing the same key twice is asking a question this answers rather than
// one it corrects.
func (k *keyspace) exists(keys []string) int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	var n int64
	for _, key := range keys {
		if k.live(key) != nil {
			n++
		}
	}
	return n
}

// typeOf answers Redis's TYPE. There is exactly one answer besides "none", because there is
// exactly one value shape in this keyspace (see the file header).
func (k *keyspace) typeOf(key string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.live(key) == nil {
		return "none"
	}
	return "hash"
}

// kvField is one field/value pair, returned in sorted field order.
//
// Redis returns hash order, which is an implementation detail — and one that makes two identical
// dashboard refreshes look like the state changed (stateStore.ts sorts for exactly that reason).
// Sorting here is not a compatibility claim; it is choosing the stable member of a set of
// answers Redis leaves unspecified.
type kvField struct{ Field, Value string }

func (k *keyspace) hgetall(key string) []kvField {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.live(key)
	if h == nil {
		return nil
	}
	out := make([]kvField, 0, len(h.fields))
	for f, v := range h.fields {
		out = append(out, kvField{f, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// hmget answers one entry per requested field; a nil entry means the field is absent, which is
// distinct from an empty value. `global_state` reads `data` and `ver` in one call and decides
// "key does not exist yet" from that nil, so collapsing the two would turn a missing key into an
// empty one and break first-write-wins-on-create.
func (k *keyspace) hmget(key string, fields []string) []*string {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.live(key)
	out := make([]*string, len(fields))
	if h == nil {
		return out
	}
	for i, f := range fields {
		if v, ok := h.fields[f]; ok {
			v := v
			out[i] = &v
		}
	}
	return out
}

// ttl answers in Redis's three-valued convention: -2 no such key, -1 no expiry, else seconds
// remaining, ROUNDED UP. Rounding up is Redis's own behaviour and it matters: a hash written 200
// ms ago with a 24 h TTL must not report 86399, because the operator projection renders that
// number next to the value.
func (k *keyspace) ttl(key string) int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.live(key)
	switch {
	case h == nil:
		return -2
	case h.expireAtMs == 0:
		return -1
	default:
		remaining := h.expireAtMs - k.nowMs()
		if remaining < 0 {
			remaining = 0
		}
		return (remaining + 999) / 1000
	}
}

// --- writes ------------------------------------------------------------------------------

// hset writes field/value pairs and returns how many fields were NEW, which is what Redis
// returns and what `bool(await c.hdel(...))`-style call sites read.
func (k *keyspace) hset(key string, pairs []kvField) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.hsetLocked(key, pairs)
}

func (k *keyspace) hsetLocked(key string, pairs []kvField) (int, error) {
	h := k.live(key)
	created := h == nil
	if created {
		h = &kvHash{fields: map[string]string{}}
	}

	// Cost the write BEFORE applying it. A write that would breach the bound must not land and
	// then be rolled back — a partially applied HSET of several fields is a corrupt hash.
	delta := int64(0)
	if created {
		delta += int64(len(key)) + perKeyOverhead
	}
	added := 0
	for _, p := range pairs {
		if old, ok := h.fields[p.Field]; ok {
			delta += int64(len(p.Value)) - int64(len(old))
		} else {
			delta += int64(len(p.Field)+len(p.Value)) + perFieldOverhead
			added++
		}
	}
	if err := k.makeRoom(key, delta); err != nil {
		return 0, err
	}

	args := make([]string, 0, 2+2*len(pairs))
	args = append(args, "HSET", key)
	for _, p := range pairs {
		args = append(args, p.Field, p.Value)
	}
	if err := k.log.append(args...); err != nil {
		return 0, err
	}

	for _, p := range pairs {
		h.fields[p.Field] = p.Value
	}
	h.bytes += delta
	k.bytes += delta
	if created {
		k.keys[key] = h
		k.orderStale = true
	}
	return added, nil
}

func (k *keyspace) hdel(key string, fields []string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.live(key)
	if h == nil {
		return 0, nil
	}
	removed := 0
	delta := int64(0)
	for _, f := range fields {
		if v, ok := h.fields[f]; ok {
			removed++
			delta -= int64(len(f)+len(v)) + perFieldOverhead
		}
	}
	if removed == 0 {
		return 0, nil
	}
	if err := k.log.append(append([]string{"HDEL", key}, fields...)...); err != nil {
		return 0, err
	}
	for _, f := range fields {
		delete(h.fields, f)
	}
	h.bytes += delta
	k.bytes += delta
	// An emptied hash is a deleted key in Redis, and the difference is observable: EXISTS, TTL
	// and SCAN all stop seeing it.
	if len(h.fields) == 0 {
		k.drop(key, h)
	}
	return removed, nil
}

func (k *keyspace) del(keys []string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, key := range keys {
		h := k.live(key)
		if h == nil {
			continue
		}
		if err := k.log.append("DEL", key); err != nil {
			return n, err
		}
		k.drop(key, h)
		n++
	}
	return n, nil
}

// expire sets an absolute deadline `seconds` from now, and returns whether a key was there to
// set it on. One call slides an entire actor's state forward, which is the whole reason tiers 1
// and 2 are a hash rather than a key per field.
func (k *keyspace) expire(key string, seconds int64) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.live(key)
	if h == nil {
		return false, nil
	}
	// A non-positive TTL deletes the key, which is Redis's behaviour and not an edge case worth
	// inventing a different answer for.
	if seconds <= 0 {
		if err := k.log.append("DEL", key); err != nil {
			return false, err
		}
		k.drop(key, h)
		return true, nil
	}
	at := k.nowMs() + seconds*1000
	if err := k.log.append("PEXPIREAT", key, strconv.FormatInt(at, 10)); err != nil {
		return false, err
	}
	h.expireAtMs = at
	return true, nil
}

// --- global_state's two atomics ----------------------------------------------------------
//
// These are the Go bodies of the Lua scripts both actorkit SDKs send (see kv.go's allowlist).
// They are methods on the keyspace, and not helpers over hget/hset, for the one property the
// scripts exist to provide: they run under the SAME lock every other command takes, so no other
// connection can interleave between the read of `ver` and the write that depends on it. A
// compare-and-set that can interleave is not one.

// errLuaNotANumber reproduces what Redis's Lua does when the version counter is not a number:
// `tonumber(cur)` yields nil and `nil + 1` raises. It is an ERROR and not a lost race, and the
// distinction is the point — the scripts return 0 or 1 so that a conflict is a VALUE and a broken
// store is an error, which is exactly what globalstore's retry loop branches on.
var errLuaNotANumber = errors.New("hash value is not an integer")

// evalCAS is `_CAS_LUA`: write only if the stored etag still matches. An empty expected etag
// means "this key must not exist yet", and a concurrent creator is a CONFLICT rather than a
// silent overwrite — the first-write-wins-on-create rule, without which two actors can both
// believe they created a dedupe set.
func (k *keyspace) evalCAS(key, data, etag string) (int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var cur string
	var hasCur bool
	if h := k.live(key); h != nil {
		cur, hasCur = h.fields["ver"]
	}
	if etag == "" {
		if hasCur {
			return 0, nil
		}
		_, err := k.hsetLocked(key, []kvField{{"data", data}, {"ver", "1"}})
		return 1, err
	}
	if !hasCur || cur != etag {
		return 0, nil
	}
	n, err := strconv.ParseInt(cur, 10, 64)
	if err != nil {
		return 0, errLuaNotANumber
	}
	_, err = k.hsetLocked(key, []kvField{{"data", data}, {"ver", strconv.FormatInt(n+1, 10)}})
	return 1, err
}

// evalPut is `_PUT_LUA`: an unconditional write that still BUMPS `ver`, so a CAS holder that
// missed a last-write-wins Set loses its race instead of clobbering it.
//
// The Lua does HSET then HINCRBY; this does both fields in one HSET. Same end state, and one log
// record instead of two — with the small improvement that a write refused at the memory ceiling
// cannot land half of itself.
func (k *keyspace) evalPut(key, data string) (int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var n int64
	if h := k.live(key); h != nil {
		if cur, ok := h.fields["ver"]; ok {
			parsed, err := strconv.ParseInt(cur, 10, 64)
			if err != nil {
				return 0, errLuaNotANumber
			}
			n = parsed
		}
	}
	_, err := k.hsetLocked(key, []kvField{{"data", data}, {"ver", strconv.FormatInt(n+1, 10)}})
	return 1, err
}

// --- SCAN --------------------------------------------------------------------------------

// scan walks the keyspace in SORTED key order, which is a stronger guarantee than Redis's and a
// deliberate one.
//
// Redis's cursor is a reverse-binary-increment over hash buckets, whose contract is "every key
// present for the whole iteration is returned at least once" — at least, because a rehash can
// hand the same key back twice. Ordering by key instead makes the cursor a POSITION IN A TOTAL
// ORDER: a key present throughout is returned exactly once, a key added behind the cursor is
// missed and one added ahead is seen, which is precisely the freedom Redis's contract leaves.
// The caller (stateStore.ts's scanBounded) needs the at-least-once half and de-duplicates
// nothing, so the exactly-once half is a free improvement rather than a dependency.
//
// `count` is a budget of keys WALKED, not of keys returned — the same thing Redis's COUNT is, and
// the reason stateStore's ceiling counts cursor progress rather than matches.
func (k *keyspace) scan(cursor string, match string, count int) (string, []string, error) {
	if count <= 0 {
		count = 10
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	start, err := decodeCursor(cursor)
	if err != nil {
		return "", nil, err
	}
	k.reorder()

	i := 0
	if start != "" {
		// Resume strictly AFTER the last key handed out. sort.SearchStrings finds the first key
		// >= start; a key equal to start has already been returned.
		i = sort.SearchStrings(k.order, start)
		for i < len(k.order) && k.order[i] <= start {
			i++
		}
	}

	var matched []string
	walked := 0
	last := ""
	for ; i < len(k.order) && walked < count; i++ {
		key := k.order[i]
		walked++
		last = key
		if k.live(key) == nil { // expired between sorts; skip it rather than hand out a dead key
			continue
		}
		if match == "" || globMatch(match, key) {
			matched = append(matched, key)
		}
	}
	if i >= len(k.order) {
		return "0", matched, nil
	}
	return encodeCursor(last), matched, nil
}

// reorder rebuilds the sorted key slice when the key SET has changed. Field writes do not
// invalidate it, so the common case — an actor rewriting its commit map — never re-sorts.
func (k *keyspace) reorder() {
	if !k.orderStale && len(k.order) == len(k.keys) {
		return
	}
	k.order = make([]string, 0, len(k.keys))
	for key := range k.keys {
		k.order = append(k.order, key)
	}
	sort.Strings(k.order)
	k.orderStale = false
}

// The cursor is the last key handed out, hex-encoded. Hex because a cursor travels back through
// clients that treat it as an opaque token but log it, and because it cannot collide with "0":
// the encoding of any non-empty key is at least two characters.
func encodeCursor(key string) string {
	var b strings.Builder
	const hex = "0123456789abcdef"
	for i := 0; i < len(key); i++ {
		b.WriteByte(hex[key[i]>>4])
		b.WriteByte(hex[key[i]&0x0f])
	}
	return b.String()
}

func decodeCursor(cursor string) (string, error) {
	if cursor == "0" || cursor == "" {
		return "", nil
	}
	if len(cursor)%2 != 0 {
		return "", errors.New("invalid cursor")
	}
	out := make([]byte, len(cursor)/2)
	for i := 0; i < len(out); i++ {
		hi, err1 := hexNibble(cursor[2*i])
		lo, err2 := hexNibble(cursor[2*i+1])
		if err1 != nil || err2 != nil {
			return "", errors.New("invalid cursor")
		}
		out[i] = hi<<4 | lo
	}
	return string(out), nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, errors.New("invalid cursor")
}

// globMatch is Redis's `stringmatchlen`, and it is a port rather than a call to `path.Match`
// because the two disagree on the case that matters: `path.Match`'s `*` stops at a `/`, and
// every kontra key is path-shaped (`kontra-global:<actor>:<key>`). The backslash escape is not
// optional either — stateStore.ts neutralises `* ? [ ] \` in caller-supplied ids before building
// the pattern, so an escaped metacharacter is the normal case, not a curiosity.
func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Collapse runs of '*', then try every split point.
			for len(pattern) > 1 && pattern[1] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			s = s[1:]
			pattern = pattern[1:]
		case '[':
			if len(s) == 0 {
				// A class always consumes a character, including a negated one — `[^a]` matches
				// "b", never "". Without this the negated branch below would slice an empty s.
				return false
			}
			p := pattern[1:]
			negate := len(p) > 0 && p[0] == '^'
			if negate {
				p = p[1:]
			}
			hit := false
			for {
				if len(p) == 0 {
					// An unterminated class matches literally, as Redis's does.
					return false
				}
				if p[0] == '\\' && len(p) >= 2 {
					if len(s) > 0 && p[1] == s[0] {
						hit = true
					}
					p = p[2:]
					continue
				}
				if p[0] == ']' {
					break
				}
				if len(p) >= 3 && p[1] == '-' && p[2] != ']' {
					lo, hi := p[0], p[2]
					if lo > hi {
						lo, hi = hi, lo
					}
					if len(s) > 0 && s[0] >= lo && s[0] <= hi {
						hit = true
					}
					p = p[3:]
					continue
				}
				if len(s) > 0 && p[0] == s[0] {
					hit = true
				}
				p = p[1:]
			}
			if negate {
				hit = !hit
			}
			if !hit {
				return false
			}
			s = s[1:]
			pattern = p[1:]
		case '\\':
			if len(pattern) >= 2 {
				pattern = pattern[1:]
			}
			fallthrough
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
			s = s[1:]
			pattern = pattern[1:]
		}
		if len(s) == 0 {
			// Trailing '*'s can still match the empty remainder.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			break
		}
	}
	return len(pattern) == 0 && len(s) == 0
}

// --- the memory bound --------------------------------------------------------------------

// makeRoom frees space for a growing write, and the policy is the one the compose file argued
// for in its own comment: only TTL'd keys are ever evicted.
//
// `volatile-lru` there, nearest-deadline here, and the difference is not a downgrade. Every
// volatile key in this keyspace is one actor's state hash carrying the same 24 h TTL, so
// "least recently used" and "expiring soonest" rank the same set by nearly the same clock — and
// nearest-deadline needs no access-time bookkeeping on the read path.
//
// The half that is NOT negotiable is which keys are exempt. `kontra-global:*` has no TTL because
// it is a dedupe set or a counter, and evicting one does not degrade a run, it silently corrupts
// it. So when nothing volatile is left, this REFUSES the write. A refusal reaches the actor as an
// error it can retry or fail on; growing past the bound reaches it as an OOM kill of the whole
// appliance, taking the embedded Temporal and object store with it.
func (k *keyspace) makeRoom(writing string, delta int64) error {
	if k.maxBytes <= 0 || delta <= 0 || k.bytes+delta <= k.maxBytes {
		return nil
	}
	type candidate struct {
		key string
		at  int64
	}
	var vols []candidate
	for key, h := range k.keys {
		if key != writing && h.volatile() {
			vols = append(vols, candidate{key, h.expireAtMs})
		}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].at < vols[j].at })
	for _, c := range vols {
		if k.bytes+delta <= k.maxBytes {
			break
		}
		h, ok := k.keys[c.key]
		if !ok {
			continue
		}
		if err := k.log.append("DEL", c.key); err != nil {
			return err
		}
		k.drop(c.key, h)
		k.evicted++
		k.logf("evicted %s to stay under maxmemory (%d bytes); %d evicted since start",
			c.key, k.maxBytes, k.evicted)
	}
	if k.bytes+delta > k.maxBytes {
		return fmt.Errorf("%w: %d bytes used, %d is the limit, and every remaining key is "+
			"global_state with no TTL — evicting one would corrupt a run rather than slow it",
			errOOM, k.bytes, k.maxBytes)
	}
	return nil
}

// --- durability --------------------------------------------------------------------------

// snapshot renders the live keyspace as the log records that would reproduce it, which is what a
// rewrite writes. Expired keys are dropped on the way out, so a compaction is also a sweep.
func (k *keyspace) snapshot() [][]string {
	out := make([][]string, 0, len(k.keys))
	k.reorder()
	for _, key := range k.order {
		h := k.live(key)
		if h == nil {
			continue
		}
		args := make([]string, 0, 2+2*len(h.fields))
		args = append(args, "HSET", key)
		fields := make([]string, 0, len(h.fields))
		for f := range h.fields {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		for _, f := range fields {
			args = append(args, f, h.fields[f])
		}
		out = append(out, args)
		if h.volatile() {
			out = append(out, []string{"PEXPIREAT", key, strconv.FormatInt(h.expireAtMs, 10)})
		}
	}
	return out
}

// applyReplay puts one logged record back, WITHOUT logging it again and without the memory
// bound. The bound is deliberately not enforced here: refusing to load state that was already
// accepted would turn a lowered limit into data loss at startup, which an operator would
// discover as a run that lost its dedupe set.
func (k *keyspace) applyReplay(rec []string) error {
	if len(rec) == 0 {
		return errors.New("empty record")
	}
	switch strings.ToUpper(rec[0]) {
	case "HSET":
		if len(rec) < 4 || len(rec)%2 != 0 {
			return fmt.Errorf("HSET record has %d arguments", len(rec))
		}
		key := rec[1]
		h, ok := k.keys[key]
		if !ok {
			h = &kvHash{fields: map[string]string{}}
			k.keys[key] = h
			k.orderStale = true
		}
		for i := 2; i < len(rec); i += 2 {
			h.fields[rec[i]] = rec[i+1]
		}
	case "HDEL":
		if len(rec) < 3 {
			return fmt.Errorf("HDEL record has %d arguments", len(rec))
		}
		if h, ok := k.keys[rec[1]]; ok {
			for _, f := range rec[2:] {
				delete(h.fields, f)
			}
			if len(h.fields) == 0 {
				delete(k.keys, rec[1])
				k.orderStale = true
			}
		}
	case "DEL":
		if len(rec) != 2 {
			return fmt.Errorf("DEL record has %d arguments", len(rec))
		}
		delete(k.keys, rec[1])
		k.orderStale = true
	case "PEXPIREAT":
		if len(rec) != 3 {
			return fmt.Errorf("PEXPIREAT record has %d arguments", len(rec))
		}
		at, err := strconv.ParseInt(rec[2], 10, 64)
		if err != nil {
			return fmt.Errorf("PEXPIREAT deadline %q: %w", rec[2], err)
		}
		if h, ok := k.keys[rec[1]]; ok {
			h.expireAtMs = at
		}
	default:
		// Not a torn tail and not a truncation: a well-formed record naming an operation this
		// version does not know. Refusing is the only safe answer — applying the rest of the log
		// without it would silently produce a keyspace that never existed.
		return fmt.Errorf("unknown record %q", rec[0])
	}
	return nil
}

// finishReplay drops what the deadlines say is already gone and recomputes the size estimate,
// which is cheaper and more honest than maintaining it record by record through a replay.
func (k *keyspace) finishReplay() {
	now := k.nowMs()
	k.bytes = 0
	for key, h := range k.keys {
		if h.expireAtMs > 0 && h.expireAtMs <= now {
			delete(k.keys, key)
			continue
		}
		h.bytes = hashBytes(key, h)
		k.bytes += h.bytes
	}
	k.orderStale = true
}
