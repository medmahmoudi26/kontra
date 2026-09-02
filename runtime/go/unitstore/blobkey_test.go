package unitstore

import (
	"regexp"
	"strings"
	"testing"
)

// The glob collision is the whole reason for padding. Under the old scheme a prefix match on
// shard 1 also matched 10-19, which silently returned a plausible undercount instead of an
// error — it produced two published numbers that had to be retracted.
func TestShardPaddingPreventsPrefixCollision(t *testing.T) {
	one, ten := shardOf("n1"), shardOf("n10")
	if one != "0001" || ten != "0010" {
		t.Fatalf("padding wrong: n1=%q n10=%q", one, ten)
	}
	if strings.HasPrefix(ten, one) {
		t.Fatalf("shard=%s still prefix-matches shard=%s — the collision survives", ten, one)
	}
}

func TestNonNumericNodeIdsSurvive(t *testing.T) {
	// graph nodes carry author-chosen ids like "crawl"; they must not become 0000
	if got := shardOf("crawl"); got != "crawl" {
		t.Errorf("non-numeric node should be preserved, got %q", got)
	}
}

func TestPathInjectionIsNeutralised(t *testing.T) {
	// an actor name or node id containing / or = would otherwise forge partition segments
	for _, bad := range []string{"a/b", "a=b", "a*b"} {
		if got := partSafe(bad); strings.ContainsAny(got, "/=*") {
			t.Errorf("partSafe(%q) = %q — still contains a path/glob metacharacter", bad, got)
		}
	}
}

// run= must LEAD. Every interactive query filters by run, and an object store prunes a LIST
// only by literal prefix — with dt= first, finding one run means listing the entire bucket
// (measured: 8.4s vs 0.17s over 188k objects). Asserting the order keeps a later "tidy-up"
// from reintroducing a 50x regression that no functional test would notice.
func TestKeyIsFullyHivePartitioned(t *testing.T) {
	k := BlobKey("2026-08-02", "cachebuster", "run-abc", "n7", 11, "deadbeef")
	want := regexp.MustCompile(`^units/run=run-abc/dt=\d{4}-\d{2}-\d{2}/actor=cachebuster/shard=0007/unit=00011/deadbeef\.json$`)
	if !want.MatchString(k) {
		t.Fatalf("key not in the partitioned form:\n  got  %s", k)
	}
}

// Empty inputs must still yield a parseable key — a blob written to a malformed path is
// invisible to the reader, which is the failure mode this whole change exists to remove.
func TestEmptyFieldsStillProduceAValidKey(t *testing.T) {
	k := BlobKey("", "", "", "", 0, "sha")
	for _, seg := range []string{"actor=unknown", "run=run", "shard=node", "unit=00000"} {
		if !strings.Contains(k, seg) {
			t.Errorf("missing %s in %s", seg, k)
		}
	}
}
