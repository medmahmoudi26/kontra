package unitstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The commit-object layout is a CROSS-SDK contract (ADR 0060): the key is what a checkpoint's
// manifest_ref names and a retry reads back, and the decode table decides whether a body may be
// folded into a batch at all. Python's internals/test_commit_conformance.py drives the SAME corpus.
//
// Run with -count=1: the corpus lives outside this module, so `go test`'s cache does not notice an
// edit to it (shared/conformance/README.md, measured).

type commitCorpus struct {
	Version int `json:"version"`
	Keys    []struct {
		Why     string `json:"why"`
		Actor   string `json:"actor"`
		Run     string `json:"run"`
		Node    string `json:"node"`
		BatchID string `json:"batch_id"`
		Unit    int    `json:"unit"`
		Prefix  string `json:"prefix"`
		Key     string `json:"key"`
	} `json:"keys"`
	Encode []struct {
		Why      string          `json:"why"`
		BatchID  string          `json:"batch_id"`
		Unit     int             `json:"unit"`
		Out      []any           `json:"out"`
		Error    *CommitError    `json:"error"`
		Category string          `json:"category"`
		Expect   json.RawMessage `json:"expect"`
	} `json:"encode"`
	Decode []struct {
		Why     string          `json:"why"`
		BatchID string          `json:"batch_id"`
		Unit    int             `json:"unit"`
		Body    json.RawMessage `json:"body"`
		Expect  struct {
			Outcome  string       `json:"outcome"`
			Out      []any        `json:"out"`
			Error    *CommitError `json:"error"`
			Category string       `json:"category"`
		} `json:"expect"`
	} `json:"decode"`
}

func loadCommitCorpus(t *testing.T) commitCorpus {
	t.Helper()
	b, err := os.ReadFile("../../../shared/conformance/commit.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx commitCorpus
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	// NON-VACUOUS, and the interesting rows are still there.
	if len(fx.Keys) == 0 || len(fx.Encode) == 0 || len(fx.Decode) == 0 {
		t.Fatal("fixture is empty — a vacuously passing conformance test is worse than none")
	}
	refused, wide := 0, 0
	for _, c := range fx.Decode {
		if c.Expect.Outcome == "refused" {
			refused++
		}
	}
	for _, c := range fx.Keys {
		if c.Unit > 99999 {
			wide++
		}
	}
	if refused == 0 || wide == 0 {
		t.Fatalf("fixture lost its refusal rows (%d) or its wide-index row (%d)", refused, wide)
	}
	return fx
}

func TestCommitKeyMatchesCrossSDKFixture(t *testing.T) {
	fx := loadCommitCorpus(t)
	if fx.Version != CommitVersion {
		t.Errorf("corpus version %d, this reader knows %d", fx.Version, CommitVersion)
	}
	for _, c := range fx.Keys {
		prefix := CommitPrefix(c.Actor, c.Run, c.Node, c.BatchID)
		if prefix != c.Prefix {
			t.Errorf("%s:\n  got  %s\n  want %s", c.Why, prefix, c.Prefix)
		}
		if key := CommitKey(prefix, c.Unit); key != c.Key {
			t.Errorf("%s:\n  got  %s\n  want %s", c.Why, key, c.Key)
		}
		// Everything under `units/` ending in `.json` is a ROW to the live row tail; a commit
		// object there would inflate a run's count by its Unit count.
		if strings.HasPrefix(prefix, "units/") {
			t.Errorf("%s: commit prefix %s is under the row prefix", c.Why, prefix)
		}
	}
}

func TestCommitEncodingMatchesCrossSDKFixture(t *testing.T) {
	fx := loadCommitCorpus(t)
	for _, c := range fx.Encode {
		raw, err := EncodeCommit(c.BatchID, c.Unit, Commit{Out: c.Out, Error: c.Error, Category: c.Category})
		if err != nil {
			t.Fatalf("%s: %v", c.Why, err)
		}
		// Compared as parsed JSON: the reader is the same SDK as the writer, so key ORDER is not the
		// contract — the field set and the values are.
		var got, want any
		_ = json.Unmarshal(raw, &got)
		_ = json.Unmarshal(c.Expect, &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n  got  %s\n  want %s", c.Why, raw, c.Expect)
		}
		back, err := DecodeCommit(raw, c.BatchID, c.Unit)
		if err != nil {
			t.Errorf("%s: an encoded body must decode back: %v", c.Why, err)
		}
		if (c.Error != nil) != (back.Error != nil) {
			t.Errorf("%s: committed/isolated did not survive the round trip: %+v", c.Why, back)
		}
	}
}

func TestCommitDecodeTable(t *testing.T) {
	fx := loadCommitCorpus(t)
	for _, c := range fx.Decode {
		got, err := DecodeCommit(c.Body, c.BatchID, c.Unit)
		switch c.Expect.Outcome {
		case "refused":
			if err == nil {
				t.Errorf("%s: decoded %+v, want a refusal", c.Why, got)
			} else if !errors.Is(err, ErrCommitInvalid) {
				t.Errorf("%s: refused with %v, want it to wrap ErrCommitInvalid", c.Why, err)
			}
		case "committed":
			if err != nil || got.Error != nil {
				t.Errorf("%s: got %+v / %v, want committed", c.Why, got, err)
				continue
			}
			if !reflect.DeepEqual(normalise(got.Out), normalise(c.Expect.Out)) || got.Out == nil {
				t.Errorf("%s: out = %v, want %v (and never nil)", c.Why, got.Out, c.Expect.Out)
			}
		case "isolated":
			if err != nil || got.Error == nil {
				t.Errorf("%s: got %+v / %v, want isolated", c.Why, got, err)
				continue
			}
			if *got.Error != *c.Expect.Error || got.Category != c.Expect.Category {
				t.Errorf("%s: got %+v %q, want %+v %q", c.Why, *got.Error, got.Category, *c.Expect.Error, c.Expect.Category)
			}
		default:
			t.Fatalf("%s: unknown outcome %q", c.Why, c.Expect.Outcome)
		}
	}
}

// normalise compares two decoded JSON lists by value, treating nil and empty alike — the
// "never nil" half is asserted separately, where it matters.
func normalise(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

// fakeObjects is an in-memory Putter+Getter, the shape the S3 store has.
type fakeObjects struct{ m map[string][]byte }

func (f *fakeObjects) Put(_ context.Context, k string, b []byte) error {
	if f.m == nil {
		f.m = map[string][]byte{}
	}
	f.m[k] = b
	return nil
}

func (f *fakeObjects) Get(_ context.Context, k string) ([]byte, error) {
	b, ok := f.m[k]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

// The store spells its prefix in front of the corpus layout exactly as PutSubunit does, and a
// commit written through it reads back through it.
func TestAStoreRoundTripsACommitUnderItsOwnPrefix(t *testing.T) {
	t.Setenv("KONTRA_ACTOR_NAME", "crawler")
	objs := &fakeObjects{}
	s := New(objs, "tenant-a/")
	prefix := s.CommitPrefix("r1", "n7", "b1")
	if want := "tenant-a/commits/run=r1/actor=crawler/shard=0007/batch=b1/"; prefix != want {
		t.Fatalf("prefix = %s, want %s", prefix, want)
	}
	key := CommitKey(prefix, 3)
	if err := s.PutCommit(context.Background(), key, "b1", 3, Commit{Out: []any{"x"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.GetCommit(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := DecodeCommit(raw, "b1", 3); err != nil || len(c.Out) != 1 {
		t.Fatalf("round trip = %+v / %v", c, err)
	}
	if _, err := s.GetCommit(context.Background(), CommitKey(prefix, 4)); !errors.Is(err, ErrNotFound) {
		t.Errorf("an absent commit must read as ErrNotFound, got %v", err)
	}
}

// A write-only object client is a wiring mistake, and it must say so rather than report every key
// missing — which would surface as CommitLost and blame the data.
func TestAWriteOnlyStoreRefusesToReadByName(t *testing.T) {
	s := New(&writeOnly{}, "")
	_, err := s.GetCommit(context.Background(), "k")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a named refusal that is not ErrNotFound", err)
	}
}

type writeOnly struct{}

func (writeOnly) Put(context.Context, string, []byte) error { return nil }
