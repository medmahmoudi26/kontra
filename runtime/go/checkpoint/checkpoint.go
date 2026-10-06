// Package checkpoint is the Checkpoint an actor carries in its Temporal activity heartbeat — the
// Go peer of runtime/python/internals/checkpoint.py, and a BYTE-COMPATIBLE one.
//
// WHY THIS EXISTS. The record of which Units had committed lived in Redis, in a hash under a 24 h
// TTL that `maxmemory-policy volatile-lru` evicted FIRST under memory pressure (ADR 0059). Losing
// it is not a slowdown: a retry reads an absent commit map and re-runs finished work, or reads a
// partly-evicted one and skips work that never ran, and nothing raises either way.
//
// A heartbeat's details live in the activity's own history. Temporal hands them to the next attempt,
// so the commit map travels with the retry rather than being looked up in a cache that may have
// dropped it.
//
// # Why a range set
//
// Heartbeat details are a Temporal payload with a size limit, so a per-unit list of 10,000 integers
// is a batch-size ceiling wearing a different hat. One contiguous run is one pair.
//
// # Canonical form is part of the contract
//
// Inclusive [lo, hi] pairs, sorted ascending, merged so no two ranges touch or overlap. The Python
// peer and the orchestrator (control/orchestrator/src/checkpoint.ts) encode the same way, and
// shared/conformance/checkpoint.json holds all three to it. Two implementations that agree on
// membership and disagree on encoding produce different bytes for the same facts — this repository
// has shipped that bug before, which is why there is a corpus rather than three reviews.
package checkpoint

import "sort"

// Version is the only version this reader understands. A checkpoint that does not say 1 is
// discarded whole.
const Version = 1

// Range is one inclusive span of unit indices.
//
// MARSHALS AS A TWO-ELEMENT ARRAY, not an object: the corpus spells it `[0, 4]`, and the Python and
// TS peers both encode a pair. A struct with named fields would be the same facts in different
// bytes, which is precisely the drift the corpus exists to catch.
type Range struct {
	Lo int
	Hi int
}

// MarshalJSON renders [lo, hi].
func (r Range) MarshalJSON() ([]byte, error) {
	return []byte("[" + itoa(r.Lo) + "," + itoa(r.Hi) + "]"), nil
}

// UnmarshalJSON reads [lo, hi]. Anything else is an error rather than a zero Range, because a
// silently-zero range claims unit 0 committed.
func (r *Range) UnmarshalJSON(b []byte) error {
	var pair []int
	if err := jsonUnmarshal(b, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return errBadRange
	}
	r.Lo, r.Hi = pair[0], pair[1]
	return nil
}

// RangeSet is a set of unit indices stored as merged inclusive ranges. Canonical at every moment,
// not only after an explicit normalise somebody can forget.
type RangeSet struct {
	r []Range
}

// NewRangeSet normalises whatever it is given.
func NewRangeSet(ranges []Range) *RangeSet {
	return &RangeSet{r: merge(append([]Range(nil), ranges...))}
}

// Add records one index. Re-adding a member is a no-op — a retry re-commits what it re-ran.
func (s *RangeSet) Add(i int) {
	s.r = merge(append(s.r, Range{i, i}))
}

// Has reports membership.
//
// Linear rather than a binary search: a batch has a handful of ranges, not thousands, and the
// bisecting version is the one place an off-by-one hides without failing a test.
func (s *RangeSet) Has(i int) bool {
	for _, r := range s.r {
		if i >= r.Lo && i <= r.Hi {
			return true
		}
	}
	return false
}

// Len is how many INDICES the set holds, not how many ranges.
func (s *RangeSet) Len() int {
	n := 0
	for _, r := range s.r {
		n += r.Hi - r.Lo + 1
	}
	return n
}

// Ranges is the canonical encoding, as it goes into a heartbeat. Never nil: `[]` and `null` are
// different bytes, and the corpus says `[]`.
func (s *RangeSet) Ranges() []Range {
	if s.r == nil {
		return []Range{}
	}
	return append([]Range(nil), s.r...)
}

// merge sorts and coalesces. ADJACENCY COUNTS: [0,2] and [3,5] touch, so they become [0,5].
// Without that, Add-ing one index at a time yields one range per index and the compaction this
// type exists for never happens.
func merge(in []Range) []Range {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(a, b int) bool {
		if in[a].Lo != in[b].Lo {
			return in[a].Lo < in[b].Lo
		}
		return in[a].Hi < in[b].Hi
	})
	out := []Range{in[0]}
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r.Lo <= last.Hi+1 {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// Details is the heartbeat payload. THE FIELD NAMES ARE THE CONTRACT — see the Python and TS peers.
type Details struct {
	V           int     `json:"v"`
	BatchID     string  `json:"batch_id"`
	Done        []Range `json:"done"`
	Failed      []int   `json:"failed"`
	ManifestRef string  `json:"manifest_ref"`
}

// Checkpoint is how far one Batch got.
//
// BatchID IS A GUARD AND NOT A LABEL. Unit indices are positions WITHIN ONE BATCH, so applying a
// checkpoint from a different batch would skip units by index in a batch that never ran them.
// ResumeFrom discards a mismatch outright rather than taking the parts that look plausible.
type Checkpoint struct {
	BatchID     string
	Done        *RangeSet
	Failed      map[int]bool
	ManifestRef string
}

// New builds an empty Checkpoint for one batch.
func New(batchID string) *Checkpoint {
	return &Checkpoint{BatchID: batchID, Done: NewRangeSet(nil), Failed: map[int]bool{}}
}

// Commit records a Unit as finished.
func (c *Checkpoint) Commit(i int) { c.Done.Add(i) }

// Isolate records a Unit as a decided failure.
func (c *Checkpoint) Isolate(i int) {
	if c.Failed == nil {
		c.Failed = map[int]bool{}
	}
	c.Failed[i] = true
}

// ToDetails renders the heartbeat payload. `failed` is SORTED, because a Go map iterates in random
// order and an unsorted list would make two encodings of the same checkpoint differ.
func (c *Checkpoint) ToDetails() Details {
	failed := make([]int, 0, len(c.Failed))
	for i := range c.Failed {
		failed = append(failed, i)
	}
	sort.Ints(failed)
	done := c.Done
	if done == nil {
		done = NewRangeSet(nil)
	}
	return Details{
		V:           Version,
		BatchID:     c.BatchID,
		Done:        done.Ranges(),
		Failed:      failed,
		ManifestRef: c.ManifestRef,
	}
}

// FromDetails decodes one heartbeat payload, or nil if it may not be trusted.
//
// REFUSED RATHER THAN PARTIALLY READ. A version this code does not know may have moved a field's
// meaning, and a reader that ignores what it does not recognise resumes from a checkpoint it only
// half understood. nil means "start from the beginning", which is always safe: the work is
// idempotent by position.
func FromDetails(d *Details) *Checkpoint {
	if d == nil || d.V != Version {
		return nil
	}
	c := New(d.BatchID)
	c.Done = NewRangeSet(d.Done)
	for _, i := range d.Failed {
		c.Failed[i] = true
	}
	c.ManifestRef = d.ManifestRef
	return c
}

// ResumeFrom is which unit indices still need running, given what a previous attempt reported.
//
// `d` is nil on a first attempt. The result is every index in [0, units) that is neither committed
// nor isolated, and it is never nil — `[]` and `null` mean the same thing to a caller that ranges
// over it, but not to a test that compares them.
//
// AN EMPTY BatchID MATCHES NOTHING, including another empty one. An unidentified checkpoint is not
// evidence about any particular batch, and treating two blanks as equal is how a checkpoint from an
// unrelated dispatch gets applied.
func ResumeFrom(d *Details, batchID string, units int) []int {
	if units < 0 {
		units = 0
	}
	all := make([]int, 0, units)
	for i := 0; i < units; i++ {
		all = append(all, i)
	}
	c := FromDetails(d)
	if c == nil || batchID == "" || c.BatchID != batchID {
		return all
	}
	todo := make([]int, 0, units)
	for _, i := range all {
		if !c.Done.Has(i) && !c.Failed[i] {
			todo = append(todo, i)
		}
	}
	return todo
}
