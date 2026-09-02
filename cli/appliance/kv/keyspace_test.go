package kv

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The glob is Redis's, not Go's, and the difference is not academic: `path.Match`'s `*` stops at
// a `/`, and every key this store holds is path-shaped. The escape cases are the ones that
// matter for safety — stateStore.ts neutralises `* ? [ ] \` in caller-supplied ids so an actor id
// cannot widen the pattern meant to constrain it.
func TestGlobMatchIsRedisSemantics(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"", "", true},
		{"", "a", false},
		{"a", "", false},
		{"*", "", true},
		{"*", "anything", true},
		{"**", "anything", true},
		{"kontra-global:a:*", "kontra-global:a:k", true},
		{"kontra-global:a:*", "kontra-global:b:k", false},
		// The case path.Match gets wrong: a `*` that has to cross a separator.
		{"kontra-global:*", "kontra-global:a:b:c", true},
		{"a*c", "ac", true},
		{"a*c", "abbbc", true},
		{"a*c", "abbb", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "abbc", false},
		// Escapes: the metacharacter must match itself and nothing else.
		{`a\*c`, "a*c", true},
		{`a\*c`, "abc", false},
		{`a\*c`, "ac", false},
		{`a\?c`, "a?c", true},
		{`a\?c`, "abc", false},
		{`a\\c`, `a\c`, true},
		{`kontra-global:a\*b:*`, "kontra-global:a*b:k", true},
		{`kontra-global:a\*b:*`, "kontra-global:axxb:k", false},
		// Character classes, including the negation that must still consume a character.
		{"[abc]d", "ad", true},
		{"[abc]d", "dd", false},
		{"[a-c]d", "bd", true},
		{"[a-c]d", "dd", false},
		{"[^a]", "b", true},
		{"[^a]", "a", false},
		{"[^a]", "", false},
		{`[\]]x`, "]x", true},
		// A trailing star still matches an exhausted subject.
		{"abc*", "abc", true},
		{"abc**", "abc", true},
		{"abc*d", "abc", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// The cursor is a position in a total order, so a key that is present for the whole sweep comes
// back EXACTLY once even while other keys are being created and deleted underneath it. Redis
// promises only at-least-once here; the caller (scanBounded in stateStore.ts) de-duplicates
// nothing, so the stronger guarantee is the one worth pinning.
func TestScanReturnsEveryStableKeyExactlyOnceUnderChurn(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	for i := 0; i < 200; i++ {
		c.send("HSET", fmt.Sprintf("kontra-global:stable:k%03d", i), "data", "v")
	}

	churn := dialKV(t, s.Address())
	seen := map[string]int{}
	cursor := "0"
	round := 0
	for {
		reply, _ := c.send("SCAN", cursor, "MATCH", "kontra-global:stable:*", "COUNT", "20").([]any)
		cursor = reply[0].(string)
		for _, k := range reply[1].([]any) {
			seen[k.(string)]++
		}
		// Churn between rounds: keys the sweep has no obligation to report either way.
		churn.send("HSET", fmt.Sprintf("kontra-global:noise:n%03d", round), "data", "v")
		if round > 0 {
			churn.send("DEL", fmt.Sprintf("kontra-global:noise:n%03d", round-1))
		}
		round++
		if cursor == "0" || round > 200 {
			break
		}
	}
	if len(seen) != 200 {
		t.Fatalf("the sweep saw %d stable keys, want 200", len(seen))
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("%s came back %d times", k, n)
		}
	}
}

// SCAN must not hand out a key whose TTL has elapsed, or the caller pipelines an HGET against it
// and reads the absence as an empty value.
func TestScanSkipsExpiredKeys(t *testing.T) {
	s := startKVForTest(t, Options{})
	c := dialKV(t, s.Address())
	c.send("HSET", "kontra-actor:alive", "u0", "x")
	c.send("HSET", "kontra-actor:doomed", "u0", "x")
	c.send("EXPIRE", "kontra-actor:doomed", "60")

	s.keys.mu.Lock()
	s.keys.now = func() time.Time { return time.Now().Add(time.Hour) }
	s.keys.mu.Unlock()

	reply, _ := c.send("SCAN", "0", "MATCH", "kontra-actor:*", "COUNT", "100").([]any)
	keys, _ := reply[1].([]any)
	if len(keys) != 1 || keys[0] != "kontra-actor:alive" {
		t.Fatalf("SCAN returned %v, want only the live key", keys)
	}
}

// THE EVICTION POLICY'S ONE NON-NEGOTIABLE HALF. docker-compose.yml chose volatile-lru over
// allkeys-lru with a comment saying why: `kontra-global:*` has no TTL because it is a dedupe set
// or a counter, and evicting one "would silently corrupt correctness". So a write that can only
// be made room for by dropping a TTL-less key is REFUSED instead.
func TestEvictionNeverTouchesTheTTLLessTier(t *testing.T) {
	// A ceiling small enough to hit in a handful of writes, so the test is about the policy
	// rather than about filling memory.
	s := startKVForTest(t, Options{MaxBytes: 12 * 1024})
	c := dialKV(t, s.Address())
	value := strings.Repeat("g", 1024)

	// Fill with TTL-less global state until the store refuses.
	refusedAt := -1
	for i := 0; i < 40; i++ {
		got := c.send("HSET", fmt.Sprintf("kontra-global:a:k%02d", i), "data", value)
		if _, isErr := got.(respError); isErr {
			wantErrContaining(t, got, "OOM")
			wantErrContaining(t, got, "no TTL")
			refusedAt = i
			break
		}
	}
	if refusedAt < 0 {
		t.Fatal("the store never reached its ceiling; the bound is not enforced")
	}
	// Everything written before the refusal is still there. A refusal is not a rollback of
	// earlier writes, and it is not a partial one either.
	for i := 0; i < refusedAt; i++ {
		wantStr(t, c.send("HGET", fmt.Sprintf("kontra-global:a:k%02d", i), "data"), value)
	}
	if s.keys.evicted != 0 {
		t.Fatalf("%d keys were evicted; every key in the store had no TTL", s.keys.evicted)
	}
}

// The other half: a volatile key IS evictable, nearest deadline first, and the write it made room
// for lands.
func TestEvictionFreesVolatileKeysNearestDeadlineFirst(t *testing.T) {
	s := startKVForTest(t, Options{MaxBytes: 12 * 1024})
	c := dialKV(t, s.Address())
	value := strings.Repeat("a", 1024)

	// Six actor hashes, with deliberately different deadlines.
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("kontra-actor:sess-%d", i)
		wantInt(t, c.send("HSET", key, "u0", value), 1)
		// sess-0 expires soonest, sess-5 last.
		wantInt(t, c.send("EXPIRE", key, fmt.Sprint(3600*(i+1))), 1)
	}
	// A TTL-less write that needs room made for it.
	for i := 0; i < 6; i++ {
		got := c.send("HSET", fmt.Sprintf("kontra-global:a:k%d", i), "data", value)
		if _, isErr := got.(respError); isErr {
			t.Fatalf("write %d was refused although volatile keys were still evictable: %v", i, got)
		}
	}
	if s.keys.evicted == 0 {
		t.Fatal("nothing was evicted, so the ceiling was never reached and the test proves nothing")
	}

	// Whatever survived, it is a SUFFIX of the deadline order: the soonest-expiring went first.
	var survivors []int
	for i := 0; i < 6; i++ {
		if v := c.send("HGET", fmt.Sprintf("kontra-actor:sess-%d", i), "u0"); v != nil {
			survivors = append(survivors, i)
		}
	}
	for j := 1; j < len(survivors); j++ {
		if survivors[j] != survivors[j-1]+1 {
			t.Fatalf("survivors %v are not a suffix of the deadline order; eviction did not go nearest-deadline-first", survivors)
		}
	}
	if len(survivors) > 0 && survivors[len(survivors)-1] != 5 {
		t.Fatalf("survivors %v do not end at the latest deadline", survivors)
	}
	// And the global tier is untouched and complete.
	for i := 0; i < 6; i++ {
		wantStr(t, c.send("HGET", fmt.Sprintf("kontra-global:a:k%d", i), "data"), value)
	}
}

// A lowered ceiling must not turn into data loss at startup: state that was already accepted is
// loaded back whether or not it would fit under today's limit. Discovering a shrunken ceiling as
// a run that lost its dedupe set is worse than discovering it as a refused write.
func TestReplayIsNotSubjectToTheCurrentCeiling(t *testing.T) {
	dir := t.TempDir()
	port := freeTestKVPort(t)
	value := strings.Repeat("z", 4096)

	first := startKVForTest(t, Options{DataDir: dir, Port: port, MaxBytes: 1 << 20})
	c := dialKV(t, first.Address())
	for i := 0; i < 10; i++ {
		wantInt(t, c.send("HSET", fmt.Sprintf("kontra-global:a:k%d", i), "data", value), 1)
	}
	first.Stop()

	second := startKVForTest(t, Options{DataDir: dir, Port: port, MaxBytes: 4096})
	c = dialKV(t, second.Address())
	for i := 0; i < 10; i++ {
		wantStr(t, c.send("HGET", fmt.Sprintf("kontra-global:a:k%d", i), "data"), value)
	}
	// New writes are refused, which is the loud half of the same fact.
	wantErrContaining(t, c.send("HSET", "kontra-global:a:new", "data", value), "OOM")
}

// The cursor encoding cannot collide with the "0" that means begin-and-end, whatever a key
// contains — a cursor that decoded to "start over" would make a sweep loop forever.
func TestCursorEncodingNeverCollidesWithZero(t *testing.T) {
	for _, key := range []string{"0", "\x00", "a", "kontra-global:a:k", "\xff\xfe"} {
		enc := encodeCursor(key)
		if enc == "0" {
			t.Fatalf("the cursor for %q encodes to the terminal cursor", key)
		}
		got, err := decodeCursor(enc)
		if err != nil || got != key {
			t.Fatalf("decodeCursor(encodeCursor(%q)) = %q, %v", key, got, err)
		}
	}
	if got, err := decodeCursor("0"); err != nil || got != "" {
		t.Fatalf(`decodeCursor("0") = %q, %v`, got, err)
	}
	for _, bad := range []string{"z", "abc", "gg"} {
		if _, err := decodeCursor(bad); err == nil {
			t.Fatalf("decodeCursor(%q) accepted a malformed cursor", bad)
		}
	}
}
