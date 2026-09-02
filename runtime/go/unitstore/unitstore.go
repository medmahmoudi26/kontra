// Package unitstore is the Go actor host's per-unit blob plane — the peer of Python's
// internals/unitstore. A STREAMED record (session.Emit) is written to its own sub-unit blob
// under the unit's prefix, and the durable commit holds only a small `$ref`, so neither actor
// memory nor the actor's state store ever carries batch payloads.
//
// The blob contract is shared cross-SDK (ADR 0015). The key is HIVE-PARTITIONED:
//
//	units/run={run}/dt=2026-08-02/actor=cachebuster/shard=0007/unit=00011/{sha}.json
//
// Every path segment is a `key=value` partition, which DuckDB reads natively with
// `hive_partitioning=true` — so run, dt, actor, shard and unit arrive as typed COLUMNS and a
// `WHERE dt = ...` prunes at the file level (measured: 4 files read of 8 with the filter).
//
// run= comes FIRST, and that ordering is load-bearing rather than aesthetic. Every interactive
// query — `kontra monitor`, its live --state refresh loop — filters by run, and an object store
// can only prune a LIST by literal prefix. With dt= first, finding one run means listing the
// whole bucket: measured on this bucket (188,275 objects) at 8.4s versus 0.17s when run= leads,
// a 50x penalty that grows with total stored objects forever. Date-first is the conventional
// hive order, and it is the wrong one here: dt is an analytics filter, run is the access path.
//
// This replaces `units/{run}/{node}/u{i}/{sha}.json`, which was positional and unsearchable:
//   - a run id is a bare UUID, so "what ran on the 1st" was unanswerable
//   - node ids were `n1..nK`, and the reader's glob `n1*` also matched n10-n19, silently
//     returning a plausible undercount rather than an error (this produced two published
//     numbers that had to be retracted)
//   - `u{i}` is an index into a chunk, so "which units produced no output" required inferring
//     coverage from the data instead of an anti-join
//
// Zero-padding shard/unit means a prefix glob can no longer collide.
// body `[record]`, ref `{"$ref": {key, size, sha256}}`. The sha is of the exact bytes written, so
// a re-emit of the same record on a resume overwrites idempotently, and the orchestrator resolves
// by the carried key+sha (never recomputing an independent digest) — so Go's JSON encoding need
// not be byte-identical to Python's for consumption; only within-Go re-emits must be stable, which
// they are (same bytes -> same sha -> same key).
package unitstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Putter is the minimal object-put the store needs. The real impl (s3.go) wraps aws-sdk-go-v2
// s3; tests inject a fake, so this file stays free of the AWS dependency.
type Putter interface {
	Put(ctx context.Context, key string, data []byte) error
}

// Store writes sub-unit blobs to an object store under a shared prefix.
type Store struct {
	put    Putter
	prefix string
}

// New builds a Store over a Putter with a key prefix (KONTRA_S3_PREFIX).
func New(put Putter, prefix string) *Store { return &Store{put: put, prefix: prefix} }

// PutSubunit writes ONE streamed record to its own sub-unit blob and returns the `$ref` entry
// that rides in its place — the Go peer of Python's unitstore.put_subunit. Body is `[record]`
// (a 1-element JSON array, so the orchestrator's array-flattening consumer reads it uniformly),
// keyed by the sha256 of those exact bytes.
func (s *Store) PutSubunit(ctx context.Context, runDate, run, node string, i int, record map[string]any) (map[string]any, error) {
	data, err := json.Marshal([]map[string]any{record})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	key := s.prefix + BlobKey(runDate, actorName(), run, node, i, sha)
	if err := s.put.Put(ctx, key, data); err != nil {
		return nil, err
	}
	return map[string]any{"$ref": map[string]any{"key": key, "size": len(data), "sha256": sha}}, nil
}

// BlobKey builds the hive-partitioned key. runDate is the RUN's date (YYYY-MM-DD), not the
// writing worker's clock: a run is one thing and belongs in one dt partition, but its workers
// write independently, so a run crossing midnight would otherwise scatter across two partitions
// and disagree between hosts. It is passed down from the handler with run/node. An empty
// runDate falls back to today, which keeps blobs writable against an older handler that does
// not send it — degraded, but never a lost blob.
//
// Exported so the reader and any migration tool derive the layout from one definition rather
// than re-deriving the format.
func BlobKey(runDate, actor, run, node string, unit int, sha string) string {
	if runDate == "" {
		runDate = time.Now().UTC().Format(dateLayout)
	}
	return fmt.Sprintf("units/run=%s/dt=%s/actor=%s/shard=%s/unit=%05d/%s.json",
		partSafe(nonEmpty(run, "run")),
		partSafe(runDate),
		partSafe(nonEmpty(actor, "unknown")),
		shardOf(node),
		unit, sha)
}

// BlobKeyAt is the same key from a time.Time, for callers holding a clock rather than a string.
func BlobKeyAt(dt time.Time, actor, run, node string, unit int, sha string) string {
	return BlobKey(dt.UTC().Format(dateLayout), actor, run, node, unit, sha)
}

const dateLayout = "2006-01-02"

// shardOf renders a node id as a sortable, glob-safe partition value. `n7` -> `0007`; a
// non-numeric graph node id is kept but sanitised.
//
// A streaming chunk-run appends `.k` to the node id (`n1.2`), and that suffix is split off
// before padding rather than defeating it: `n1.2` -> `0001.2`. Padding only the un-chunked form
// would leave `n1.2` and `n10.2` sharing a prefix, reintroducing by a side door the exact
// collision the padding exists to prevent.
func shardOf(node string) string {
	// Default FIRST, then derive from the defaulted value. Deriving from the raw arg meant an
	// empty node produced `shard=` with no value — a malformed path the reader can never
	// match, i.e. a silently invisible blob, which is the exact class of bug this change is
	// meant to remove.
	name := nonEmpty(node, "node")
	base, chunk, chunked := strings.Cut(name, ".")
	if v, err := strconv.Atoi(strings.TrimPrefix(base, "n")); err == nil && v >= 0 {
		if chunked {
			return fmt.Sprintf("%04d.%s", v, partSafe(chunk))
		}
		return fmt.Sprintf("%04d", v)
	}
	return partSafe(name)
}

// partSafe strips characters that would break a hive path or a glob.
func partSafe(v string) string {
	r := strings.NewReplacer("/", "_", "=", "_", " ", "_", "*", "_", "?", "_")
	return r.Replace(v)
}

// actorName is the actor this worker serves; the writer needs it for the actor= partition.
func actorName() string { return os.Getenv("KONTRA_ACTOR_NAME") }

func nonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
