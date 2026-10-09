package unitstore

// Per-Unit commit objects (ADR 0060) — the Go peer of the commit half of Python's
// internals/unitstore.py, held to it by shared/conformance/commit.json.
//
// THE HEARTBEAT SAYS WHICH UNITS FINISHED; THIS SAYS WHAT THEY PRODUCED. A checkpoint is a range set
// because a heartbeat is small, and a range set cannot carry a Unit's output refs. So each finished
// Unit is ONE object, written synchronously BEFORE the beat that reports it, and the checkpoint's
// `manifest_ref` names the batch's prefix. A retry folds the finished Units back from these objects
// instead of re-running them. They used to be fields in the actor's Redis hash, which was one TTL or
// one eviction away from a retry re-running finished work or skipping unfinished work with nothing
// raised (ADR 0059).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CommitVersion is the only commit-object version this reader understands.
const CommitVersion = 1

// ErrNotFound is what a Getter returns for a key with no object behind it. ABSENCE IS A VALUE, not a
// failure of the store: the engine decides it is loud (a Unit the checkpoint calls finished with
// nothing at its key is data the store lost), while every OTHER read error stays retryable.
var ErrNotFound = errors.New("unitstore: no object at key")

// Getter is the read half the commit path needs. Separate from Putter so a write-only fake — the
// failing store the push tests use — does not have to grow a read it never exercises; the real S3
// store implements both.
type Getter interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// CommitPrefix is where every commit object of one batch lives — byte-identical to Python's
// commit_prefix.
//
//	commits/run={run}/actor={actor}/shard={shard}/batch={batchID}/
//
// ITS OWN TOP-LEVEL PREFIX, NOT A CORNER OF `units/`. Everything under `units/run=<id>/` that ends
// in `.json` is a ROW to the live row tail (control/orchestrator/src/rowTail.ts counts them) and to a
// DuckDB glob over the run, so a commit object there would inflate every run's row count by its Unit
// count. `run=` still leads, for the measured reason BlobKey gives: an object store prunes a LIST
// only by literal prefix, and whatever sweeps this tree asks by run.
func CommitPrefix(actor, run, node, batchID string) string {
	return fmt.Sprintf("commits/run=%s/actor=%s/shard=%s/batch=%s/",
		partSafe(nonEmpty(run, "run")),
		partSafe(nonEmpty(actor, "unknown")),
		shardOf(node),
		partSafe(nonEmpty(batchID, "batch")))
}

// CommitKey is Unit `unit`'s commit object under a batch's prefix. Five digits is a floor, not a
// width: an index past 99999 widens rather than truncating into a neighbour's key.
func CommitKey(prefix string, unit int) string {
	return fmt.Sprintf("%sunit=%05d.json", prefix, unit)
}

// CommitError is an isolated Unit's {type, message} — the same shape as wire.PerUnitFailure.error.
type CommitError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Commit is one Unit's outcome as its commit object holds it: Out for a committed Unit, Error and
// Category for an isolated one.
type Commit struct {
	Out      []any
	Error    *CommitError
	Category string
}

// commitBody is the object's bytes. The body NAMES its batch and its Unit even though the key
// already does — that redundancy is the integrity check, so a body at the wrong key (a copy, a
// skewed prefix, a hand edit) is refused on read instead of folded into a batch it does not describe.
//
// V and Unit are POINTERS so absence is visible: a body that does not say which Unit it is must not
// decode as Unit 0, and Python's reader tells the two apart without trying.
type commitBody struct {
	V        *int         `json:"v"`
	BatchID  string       `json:"batch_id"`
	Unit     *int         `json:"unit"`
	Out      []any        `json:"out"`
	Error    *CommitError `json:"error,omitempty"`
	Category string       `json:"category,omitempty"`
}

// EncodeCommit renders one Unit's commit object. `out` is always a list: an isolated Unit has `[]`,
// and a committed Unit that pushed nothing has `[]` too, which a reader must tell apart from a
// missing `out` (refused, never "empty").
func EncodeCommit(batchID string, unit int, c Commit) ([]byte, error) {
	v := CommitVersion
	out := c.Out
	if out == nil {
		out = []any{}
	}
	body := commitBody{V: &v, BatchID: batchID, Unit: &unit, Out: out, Error: c.Error}
	if c.Error != nil {
		body.Category = nonEmpty(c.Category, "exhausted")
	}
	return json.Marshal(body)
}

// ErrCommitInvalid wraps every reason DecodeCommit refuses a body.
var ErrCommitInvalid = errors.New("commit object refused")

// DecodeCommit validates one commit object read back for (batchID, unit).
//
// REFUSED RATHER THAN PARTLY READ, the checkpoint's rule one level down: a version this code does not
// know, a body naming another batch or another Unit, or a committed body with no `out` list. The
// safe-looking alternatives are both wrong — treating it as "not finished" re-runs silently, and
// folding what it says puts another Unit's output in this one.
func DecodeCommit(raw []byte, batchID string, unit int) (Commit, error) {
	var b commitBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return Commit{}, fmt.Errorf("%w: %v", ErrCommitInvalid, err)
	}
	switch {
	case b.V == nil || *b.V != CommitVersion:
		return Commit{}, fmt.Errorf("%w: version %s; this reader knows %d", ErrCommitInvalid, intOrAbsent(b.V), CommitVersion)
	case b.BatchID != batchID:
		return Commit{}, fmt.Errorf("%w: names batch %q, expected %q", ErrCommitInvalid, b.BatchID, batchID)
	case b.Unit == nil || *b.Unit != unit:
		return Commit{}, fmt.Errorf("%w: names unit %s, expected %d", ErrCommitInvalid, intOrAbsent(b.Unit), unit)
	case b.Out == nil:
		// json.Unmarshal leaves a nil slice for both `"out": null` and an absent `out`, and a
		// non-nil empty one for `[]` — which is exactly the line the corpus draws.
		return Commit{}, fmt.Errorf("%w: has no `out` list", ErrCommitInvalid)
	}
	if b.Error != nil {
		return Commit{Out: []any{}, Error: b.Error, Category: nonEmpty(b.Category, "exhausted")}, nil
	}
	return Commit{Out: b.Out}, nil
}

func intOrAbsent(p *int) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprint(*p)
}

// CommitPrefix is this batch's commit prefix as THIS store spells it — the value a checkpoint
// carries as `manifest_ref`. Concatenated with the store prefix exactly as PutSubunit does, for the
// same reason: the prefix is CARRIED (in the heartbeat) and read back verbatim.
func (s *Store) CommitPrefix(run, node, batchID string) string {
	return s.prefix + CommitPrefix(actorName(), run, node, batchID)
}

// PutCommit writes one Unit's commit object. SYNCHRONOUS ON PURPOSE: the beat that reports the Unit
// finished is sent only after this returns, so a checkpoint can never name a Unit whose outcome is
// not already in the store.
func (s *Store) PutCommit(ctx context.Context, key, batchID string, unit int, c Commit) error {
	data, err := EncodeCommit(batchID, unit, c)
	if err != nil {
		return err
	}
	return s.put.Put(ctx, key, data)
}

// GetCommit reads one commit object's bytes, or ErrNotFound. A store whose object client cannot
// read says so by name rather than reporting every key as missing — which would turn a wiring
// mistake into a CommitLost that blames the data.
func (s *Store) GetCommit(ctx context.Context, key string) ([]byte, error) {
	g, ok := s.put.(Getter)
	if !ok {
		return nil, fmt.Errorf("unitstore: this store's object client (%T) cannot read", s.put)
	}
	return g.Get(ctx, key)
}
