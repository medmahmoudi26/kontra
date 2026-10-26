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
// How a part gets here (Externalize), how its row reaches the event's transaction (Pending, InsertPending) and how
// it leaves the gate (Renderer) are in the package's other files.
//
// Work packages: M3 WP-02 (the store, the blobs table, rendering), M3 WP-03 (blob streams, Fetch).
package blob

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
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

// Ref names one object: 128 random bits, base32 without padding, lowercase (26 characters).
type Ref string

// refEncoding is base32 (RFC 4648) without padding; refs are written lowercase.
var refEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// refRE is the shape of a ref (the blobs.ref CHECK in migration 0002).
var refRE = regexp.MustCompile(`^[a-z2-7]{26}$`)

// NewRef mints a ref.
func NewRef() (Ref, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("blob: mint a ref: %w", err)
	}
	return Ref(strings.ToLower(refEncoding.EncodeToString(b[:]))), nil
}

// Valid reports whether r has a ref's shape.
func (r Ref) Valid() bool { return refRE.MatchString(string(r)) }

// URL is the part URL that names r: kontra-blob:<ref>.
func (r Ref) URL() string { return URIScheme + string(r) }

// RefOf returns the ref a part URL names when it is exactly the gate's form, kontra-blob:<ref>.
func RefOf(u string) (Ref, bool) {
	rest, ok := strings.CutPrefix(u, URIScheme)
	if !ok || !Ref(rest).Valid() {
		return "", false
	}
	return Ref(rest), true
}

// Reserved reports whether a part URL uses the gate's reserved scheme, in any case and after leading white space
// (D181): such a part from a caller is InvalidRequest, from a service upstream_error, whether or not it is a
// well-formed ref, so every kontra-blob: part in the log is one the gate wrote. A URL scheme is case-insensitive
// (RFC 3986 §3.1), so KONTRA-BLOB: is the same scheme.
func Reserved(u string) bool {
	u = strings.TrimLeft(u, " \t\r\n\f\v")
	return len(u) >= len(URIScheme) && strings.EqualFold(u[:len(URIScheme)], URIScheme)
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
	Prefix   string // without a leading or trailing slash; empty for the bucket's root
	Endpoint string // empty: the provider's default (AWS); else scheme://host[:port]
	Region   string
}

// bucketRE is an S3 bucket name: 3 to 63 lowercase letters, digits, dots and hyphens, starting and ending with
// a letter or a digit.
var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// prefixRE is one segment of a prefix: what an object key may hold without escaping.
var prefixRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// regionRE is an S3 region name.
var regionRE = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ParseURL parses blobs: s3://<bucket>[/<prefix>][?endpoint=<URL>&region=<region>] (validated at start by
// internal/config too). Credentials never appear in it: they come from AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY. The endpoint is http:// or https:// with a host, an optional port and nothing else.
func ParseURL(s string) (Config, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Config{}, fmt.Errorf("blob: blobs: not a URL")
	}
	switch {
	case u.Scheme != "s3":
		return Config{}, errors.New("blob: blobs: want s3://<bucket>[/<prefix>]")
	case u.User != nil:
		return Config{}, errors.New("blob: blobs: credentials do not belong in the URL (they come from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)")
	case u.Fragment != "" || u.Opaque != "":
		return Config{}, errors.New("blob: blobs: no fragment")
	case !bucketRE.MatchString(u.Host) || strings.Contains(u.Host, ".."):
		return Config{}, fmt.Errorf("blob: blobs: %q is not an S3 bucket name", u.Host)
	}
	cfg := Config{Bucket: u.Host}
	if p := strings.Trim(u.Path, "/"); p != "" {
		for seg := range strings.SplitSeq(p, "/") {
			if !prefixRE.MatchString(seg) || seg == "." || seg == ".." {
				return Config{}, fmt.Errorf("blob: blobs: prefix %q: segments of letters, digits, '.', '_' and '-' only", p)
			}
		}
		cfg.Prefix = p
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Config{}, fmt.Errorf("blob: blobs: query: %w", err)
	}
	for k, v := range q {
		if len(v) != 1 {
			return Config{}, fmt.Errorf("blob: blobs: %s given %d times", k, len(v))
		}
		switch k {
		case "endpoint":
			e, err := url.Parse(v[0])
			if err != nil || (e.Scheme != "http" && e.Scheme != "https") || e.Host == "" || e.Hostname() == "" ||
				e.User != nil || (e.Path != "" && e.Path != "/") || e.RawQuery != "" || e.Fragment != "" {
				return Config{}, errors.New("blob: blobs: endpoint: want http(s)://host[:port] and nothing else")
			}
			cfg.Endpoint = e.Scheme + "://" + e.Host
		case "region":
			if !regionRE.MatchString(v[0]) {
				return Config{}, fmt.Errorf("blob: blobs: region %q", v[0])
			}
			cfg.Region = v[0]
		default:
			return Config{}, fmt.Errorf("blob: blobs: unknown parameter %q (endpoint and region only)", k)
		}
	}
	return cfg, nil
}

// ObjectName is k's object key under prefix: <prefix>/<tenant>/<task>/<ref>.
func ObjectName(prefix string, k Key) string {
	return taskPrefix(prefix, k.Tenant, k.Task) + string(k.Ref)
}

// taskPrefix is the key prefix of every object of a task, slash included.
func taskPrefix(prefix string, tenant, task uuid.UUID) string {
	p := tenant.String() + "/" + task.String() + "/"
	if prefix != "" {
		p = prefix + "/" + p
	}
	return p
}

// Store is the bucket.
type Store interface {
	// Put stores exactly size bytes from r under k.
	Put(ctx context.Context, k Key, r io.Reader, size int64) error
	// Open returns the object and its size.
	Open(ctx context.Context, k Key) (io.ReadCloser, int64, error)
	// Presign returns a GET URL valid for ttl, for a caller (never for a node).
	Presign(ctx context.Context, k Key, ttl time.Duration) (string, error)
	// Delete removes one object: one a refused request stored, which nothing will ever read. An object the bucket
	// does not hold is no error.
	Delete(ctx context.Context, k Key) error
	// DeleteTask removes every object of a task (retention).
	DeleteTask(ctx context.Context, tenant, task uuid.UUID) error
}

// ErrNotFound is an object the bucket does not hold.
var ErrNotFound = errors.New("blob: no such object")

// Resolver finds the task a ref belongs to, for a blob stream's authorization (spec/tunnel.md §8.3).
type Resolver interface {
	TaskOf(ctx context.Context, ref Ref) (tenant, task uuid.UUID, err error)
}
