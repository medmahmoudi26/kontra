// Go half of the cross-SDK Checkpoint contract.
//
// The Python peer (runtime/python/internals/test_checkpoint_conformance.py) and the orchestrator
// (control/orchestrator/src/checkpoint.test.ts) assert the SAME fixture. A divergence means a
// checkpoint one side writes is one the other mis-reads — and because the safe failure is "start
// over", a drift shows up as work silently re-run or silently skipped rather than as an error.
package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

type fixture struct {
	Canonical []struct {
		Why    string `json:"why"`
		Add    []int  `json:"add"`
		Expect []Range
		Count  int `json:"count"`
	} `json:"canonical"`
	Members []struct {
		Why    string  `json:"why"`
		Ranges []Range `json:"ranges"`
		In     []int   `json:"in"`
		Out    []int   `json:"out"`
	} `json:"members"`
	Wire []struct {
		Why         string `json:"why"`
		BatchID     string `json:"batch_id"`
		Commit      []int  `json:"commit"`
		Isolate     []int  `json:"isolate"`
		ManifestRef string `json:"manifest_ref"`
		Bytes       string `json:"bytes"`
	} `json:"wire"`
	Resume []struct {
		Why        string  `json:"why"`
		Checkpoint Details `json:"checkpoint"`
		BatchID    string  `json:"batch_id"`
		Units      int     `json:"units"`
		Todo       []int   `json:"todo"`
		Discarded  bool    `json:"discarded"`
	} `json:"resume"`
}

// repoRoot walks up from this file: checkpoint/ -> go/ -> runtime/ -> <root>. The same depth the
// Python peer counts, and wrong by one resolves outside the checkout, where the fixture silently
// disappears.
func repoRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..")
}

func load(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "shared", "conformance", "checkpoint.json"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	// NON-VACUOUS. Every test below loops over one of these, so an empty fixture passes all of them.
	if len(fx.Canonical) == 0 || len(fx.Members) == 0 || len(fx.Resume) == 0 || len(fx.Wire) == 0 {
		t.Fatal("fixture is empty — a vacuously passing conformance test is worse than none")
	}
	return fx
}

func TestRangeSetCanonicalEncoding(t *testing.T) {
	for _, c := range load(t).Canonical {
		s := NewRangeSet(nil)
		for _, i := range c.Add {
			s.Add(i)
		}
		if got := s.Ranges(); !reflect.DeepEqual(got, c.Expect) {
			t.Errorf("%s:\n  got  %v\n  want %v", c.Why, got, c.Expect)
		}
		if got := s.Len(); got != c.Count {
			t.Errorf("%s: count got %d want %d", c.Why, got, c.Count)
		}
	}
}

// A set built FROM ranges must encode back to the same ranges, or a checkpoint changes shape every
// time it passes through a heartbeat.
func TestRangeSetIsStableUnderReEncoding(t *testing.T) {
	for _, c := range load(t).Canonical {
		once := NewRangeSet(c.Expect).Ranges()
		twice := NewRangeSet(once).Ranges()
		if !reflect.DeepEqual(once, c.Expect) {
			t.Errorf("%s: re-encoding changed it:\n  got  %v\n  want %v", c.Why, once, c.Expect)
		}
		if !reflect.DeepEqual(twice, once) {
			t.Errorf("%s: not stable under a second pass", c.Why)
		}
	}
}

func TestRangeSetMembership(t *testing.T) {
	for _, c := range load(t).Members {
		s := NewRangeSet(c.Ranges)
		for _, i := range c.In {
			if !s.Has(i) {
				t.Errorf("%s: %d should be a member of %v", c.Why, i, c.Ranges)
			}
		}
		for _, i := range c.Out {
			if s.Has(i) {
				t.Errorf("%s: %d should NOT be a member of %v", c.Why, i, c.Ranges)
			}
		}
	}
}

func TestResumeFrom(t *testing.T) {
	for _, c := range load(t).Resume {
		d := c.Checkpoint
		got := ResumeFrom(&d, c.BatchID, c.Units)
		want := c.Todo
		if want == nil {
			want = []int{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n  got  %v\n  want %v", c.Why, got, want)
		}
	}
}

// The `discarded` cases are the ones where resuming would LOSE work, so they are asserted twice:
// once for the todo list, and once for the property that makes them safe.
func TestADiscardedCheckpointYieldsEveryUnit(t *testing.T) {
	fx := load(t)
	n := 0
	for _, c := range fx.Resume {
		if !c.Discarded {
			continue
		}
		n++
		d := c.Checkpoint
		got := ResumeFrom(&d, c.BatchID, c.Units)
		want := make([]int, 0, c.Units)
		for i := 0; i < c.Units; i++ {
			want = append(want, i)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a discard must start from the beginning:\n  got  %v\n  want %v", c.Why, got, want)
		}
	}
	if n < 4 {
		t.Errorf("the fixture carried %d discard cases; it should carry every reason", n)
	}
}

// A first attempt has no heartbeat details at all, and nil must not be mistaken for an empty
// checkpoint that happens to match.
func TestNoDetailsMeansStartFromTheBeginning(t *testing.T) {
	if got := ResumeFrom(nil, "b1", 3); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("nil details: got %v", got)
	}
	if got := ResumeFrom(&Details{}, "b1", 3); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("zero details (v=0): got %v", got)
	}
}

func TestDetailsRoundTrip(t *testing.T) {
	c := New("b1")
	c.ManifestRef = "s3://m"
	for _, i := range []int{0, 1, 2, 5} {
		c.Commit(i)
	}
	c.Isolate(3)

	d := c.ToDetails()
	back := FromDetails(&d)
	if back == nil {
		t.Fatal("a checkpoint this package wrote must be one it can read")
	}
	if back.BatchID != "b1" || back.ManifestRef != "s3://m" {
		t.Errorf("identity lost: %+v", back)
	}
	if got := back.Done.Ranges(); !reflect.DeepEqual(got, []Range{{0, 2}, {5, 5}}) {
		t.Errorf("done: got %v", got)
	}
	if !back.Failed[3] || len(back.Failed) != 1 {
		t.Errorf("failed: got %v", back.Failed)
	}
}

// THE BYTES, not just the values. An encoder that rendered `done` as `[{"Lo":0,"Hi":1}]` agrees with
// its peers on every membership question in this file and is unreadable to them.
//
// THE EXPECTED BYTES LIVE IN THE CORPUS rather than as a literal here. A literal in one language is
// a hand-copied golden, which `shared/conformance/README.md` names as a mistake this repo has
// already paid for: two implementations cannot both be wrong against a shared file, but they can
// both be wrong against a constant one of them typed.
func TestWireBytes(t *testing.T) {
	for _, c := range load(t).Wire {
		ck := New(c.BatchID)
		ck.ManifestRef = c.ManifestRef
		for _, i := range c.Commit {
			ck.Commit(i)
		}
		for _, i := range c.Isolate {
			ck.Isolate(i)
		}
		raw, err := json.Marshal(ck.ToDetails())
		if err != nil {
			t.Fatalf("%s: %v", c.Why, err)
		}
		if string(raw) != c.Bytes {
			t.Errorf("%s:\n  got  %s\n  want %s", c.Why, raw, c.Bytes)
		}
	}
}

// And the other direction: every byte string in the corpus must decode to the same facts.
func TestWireBytesDecodeBack(t *testing.T) {
	for _, c := range load(t).Wire {
		var d Details
		if err := json.Unmarshal([]byte(c.Bytes), &d); err != nil {
			t.Fatalf("%s: %v", c.Why, err)
		}
		ck := FromDetails(&d)
		if ck == nil {
			t.Fatalf("%s: a checkpoint in the corpus must be readable", c.Why)
		}
		if ck.BatchID != c.BatchID || ck.ManifestRef != c.ManifestRef {
			t.Errorf("%s: identity lost: %+v", c.Why, ck)
		}
		for _, i := range c.Commit {
			if !ck.Done.Has(i) {
				t.Errorf("%s: %d should be committed", c.Why, i)
			}
		}
		for _, i := range c.Isolate {
			if !ck.Failed[i] {
				t.Errorf("%s: %d should be isolated", c.Why, i)
			}
		}
		if len(ck.Failed) != len(c.Isolate) {
			t.Errorf("%s: failed has %d entries, want %d", c.Why, len(ck.Failed), len(c.Isolate))
		}
	}
}

func TestARangeMustBeExactlyTwoElements(t *testing.T) {
	// A malformed range is an error rather than a zero Range, because a silently-zero range claims
	// unit 0 committed — which would make a retry skip real work.
	for _, bad := range []string{`[1]`, `[1,2,3]`, `[]`, `{"Lo":1,"Hi":2}`} {
		var r Range
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("%s decoded to %+v instead of erroring", bad, r)
		}
	}
}
