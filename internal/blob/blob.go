// Package blob is the claim-check bucket (spec/relay.md §10.4, spec/tunnel.md §8.3, D56, D167; it lifts D104).
//
// A part over 256 KiB (measured on its decoded bytes; a sealed part on its ciphertext) is stored in the bucket
// that blobs: names and logged in its event's payload as a url part `kontra-blob:<ref>`, a name and never a URL
// anyone dials; the blobs table (migration 0002) maps each ref to its task, since one task_events.payload_ref
// cannot name the several large parts of one message (D167). A part reaches a caller, a push receiver or a direct
// backend as a url part with a presigned GET valid PresignTTL, and a node as `kontra-blob:<ref>`, which the node
// fetches on a blob stream inside its own tunnel: a node never dials a URL taken from a part (D56). The gate
// stores bytes it does not interpret. The bucket carries A2A parts only.
//
// blobs: is s3://<bucket>[/<prefix>][?endpoint=<URL>&region=<region>], spoken through the S3 API (minio-go v7;
// Scaleway Object Storage in Cloud, any S3-compatible store for a team gate), with credentials from
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. An object's key is <prefix>/<tenant>/<task>/<ref>; retention
// deletes a task's objects, by prefix, before its rows (spec/relay.md §5.4). A blob stream's ref is resolved
// through the blobs table and checked against the session asking: a task one of its attempts serves, or a task
// its hiring principal's buyer owns.
//
// The scheme is reserved (D181): a url part starting with kontra-blob: is refused from a caller (InvalidRequest)
// and from a service (upstream_error), and every rendering resolves a ref through the blobs table for the task
// its row names only, never for another task that carries the same ref.
//
// Work packages: M3 WP-02 (the store, the blobs table, rendering), M3 WP-03 (blob streams, Fetch).
package blob

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/unimpl"
)

// Fixed values (spec/relay.md §10.4, spec/tunnel.md §8.3, §14).
const (
	// URIScheme prefixes a part URL that names an object for a node.
	URIScheme = "kontra-blob:"
	// PresignTTL is how long a caller's presigned GET is valid.
	PresignTTL = 15 * time.Minute
	// Inline is the largest part stored in task_events.payload.
	Inline = 256 << 10
	// MaxObject is the largest object a blob stream carries (tunnel.md §14).
	MaxObject = 64 << 20
)

// Ref names one object: 128 random bits, base32 without padding, lowercase.
type Ref string

// NewRef mints a ref.
func NewRef() (Ref, error) {
	return "", unimpl.Error("blob.NewRef", "M3 WP-03")
}

// Key is where an object lives.
type Key struct {
	Tenant uuid.UUID
	Task   uuid.UUID
	Ref    Ref
}

// Config is a parsed blobs: URL.
type Config struct {
	Bucket   string
	Prefix   string
	Endpoint string // empty: the provider's default (AWS)
	Region   string
}

// ParseURL parses blobs: (validated at start by internal/config).
func ParseURL(s string) (Config, error) {
	return Config{}, unimpl.Error("blob.ParseURL", "M3 WP-03")
}

// Store is the bucket.
type Store interface {
	// Put stores exactly size bytes from r under k.
	Put(ctx context.Context, k Key, r io.Reader, size int64) error
	// Open returns the object and its size.
	Open(ctx context.Context, k Key) (io.ReadCloser, int64, error)
	// Presign returns a GET URL valid for ttl, for a caller (never for a node).
	Presign(ctx context.Context, k Key, ttl time.Duration) (string, error)
	// DeleteTask removes every object of a task (retention).
	DeleteTask(ctx context.Context, tenant, task uuid.UUID) error
}

// Open connects to the bucket cfg names and checks that it exists and is writable.
func Open(ctx context.Context, cfg Config) (Store, error) {
	return nil, unimpl.Error("blob.Open", "M3 WP-03")
}

// Resolver finds the task a ref belongs to, for a blob stream's authorization (spec/tunnel.md §8.3).
type Resolver interface {
	TaskOf(ctx context.Context, ref Ref) (tenant, task uuid.UUID, err error)
}

// Fetch reads one object on a node-opened blob stream (the node's half of spec/tunnel.md §8.3, D181): it writes
// the preamble {"v": 1, "op": "blob", "ref": ref} and reads one JSON line, {"size": n} or {"error": "not_found"
// | "forbidden" | "too_large"} (a FetchError), then exactly n bytes; it refuses an announced size over MaxObject
// and more bytes than announced. The caller authenticates a sealed part's whole age file before acting on it.
func Fetch(ctx context.Context, stream io.ReadWriteCloser, ref Ref, w io.Writer) (int64, error) {
	return 0, unimpl.Error("blob.Fetch", "M3 WP-03")
}

// FetchError is the gate's refusal of a blob stream: "not_found", "forbidden" or "too_large".
type FetchError struct {
	Reason string
}

func (e *FetchError) Error() string { return "blob: refused: " + e.Reason }
