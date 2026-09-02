// Package objectstore is the S3/SeaweedFS object store + the prefix-aware key layout.
// One bucket holds both the content-addressed cas/ objects (codec offload + the blob
// plane) and the mutable checkpoints/ files. Config is env-driven, matching the Python
// actorkit.objectstore byte-for-byte so keys line up across languages.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Backing is the raw key->bytes store under a Store. Two impls: in-memory (tests) and S3.
type Backing interface {
	Get(ctx context.Context, key string) (data []byte, found bool, err error)
	Put(ctx context.Context, key string, data []byte) error
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
}

// Store wraps a Backing with the prefix-aware key joiner + the one content-addressed
// layout (CasKey). nil Store == disabled (codec passthrough).
type Store struct {
	backing Backing
	prefix  string
}

// New builds a Store over a backing. THE PREFIX IS NORMALISED HERE, ONCE, and not at each use:
// `s.prefix` is what every reader of the field sees, so a spelling can never leak past this line.
func New(backing Backing, prefix string) *Store {
	return &Store{backing: backing, prefix: strings.Trim(prefix, "/")}
}

// Key joins parts under the prefix. VERBATIM algorithm, pinned by the `prefixCases` rows of
// shared/conformance/codec/fixtures.json and shared with runtime/go/codec.objectKey,
// runtime/python/internals/casstore.object_key and control/orchestrator/src/codec/objectStore.ts:key: strip
// leading/trailing '/' from EVERY segment, the prefix included; drop the segments that are then
// empty; join what is left with a single '/'. No leading slash when the prefix is empty.
//
// THE PREFIX USED TO BE THE ONE SEGMENT THAT WAS NOT STRIPPED, which meant KONTRA_S3_PREFIX=`p/`
// addressed `p//cas/…` — a second namespace, reached by a spelling an operator reads as identical
// to `p`, with no error either way. See wireFormat.prefixTrailingSlash in the corpus for the
// decision and what it costs.
func (s *Store) Key(parts ...string) string {
	var bits []string
	if s.prefix != "" {
		bits = append(bits, s.prefix)
	}
	for _, p := range parts {
		if t := strings.Trim(p, "/"); t != "" {
			bits = append(bits, t)
		}
	}
	return strings.Join(bits, "/")
}

// CasKey is the ONE content-addressed layout: cas/<sha[:2]>/<sha>, prefix-aware.
func (s *Store) CasKey(sha string) string { return s.Key("cas", sha[:2], sha) }

func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return s.backing.Get(ctx, key)
}
func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	return s.backing.Put(ctx, key, data)
}
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	return s.backing.Exists(ctx, key)
}
func (s *Store) Delete(ctx context.Context, key string) error { return s.backing.Delete(ctx, key) }

// --- in-memory backing (tests + the conformance harness) ---

type memBacking struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMem returns an in-memory Store (no prefix).
func NewMem() *Store { return New(&memBacking{m: map[string][]byte{}}, "") }

func (b *memBacking) Get(_ context.Context, key string) ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.m[key]
	return v, ok, nil
}
func (b *memBacking) Put(_ context.Context, key string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	b.m[key] = cp
	return nil
}
func (b *memBacking) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[key]
	return ok, nil
}
func (b *memBacking) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, key)
	return nil
}

// --- env config + S3 backing ---

// Config is the env-driven object-store config (defaults match python actorkit).
type Config struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	Region    string
	AccessKey string
	SecretKey string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ConfigFromEnv reads the KONTRA_S3_* env vars (same names/defaults as Python).
func ConfigFromEnv() Config {
	return Config{
		Endpoint:  os.Getenv("KONTRA_S3_ENDPOINT"),
		Bucket:    env("KONTRA_S3_BUCKET", "kontra"),
		Prefix:    os.Getenv("KONTRA_S3_PREFIX"),
		Region:    env("KONTRA_S3_REGION", "us-east-1"),
		AccessKey: env("KONTRA_S3_ACCESS_KEY", "kontra"),
		SecretKey: env("KONTRA_S3_SECRET_KEY", "kontra"),
	}
}

// FromEnv builds the real S3-backed Store from env, or returns enabled=false when
// KONTRA_S3_ENDPOINT is unset (the codec then runs in passthrough). It NEVER falls
// through to real AWS without an explicit endpoint.
func FromEnv(ctx context.Context) (store *Store, enabled bool, err error) {
	c := ConfigFromEnv()
	if c.Endpoint == "" {
		return nil, false, nil
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(c.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")),
	)
	if err != nil {
		return nil, false, err
	}
	cli := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(c.Endpoint)
		o.UsePathStyle = true // SeaweedFS / path-style addressing
	})
	b := &s3Backing{cli: cli, bucket: c.Bucket}
	_ = b.ensureBucket(ctx) // best-effort; ignore "already exists"
	return New(b, c.Prefix), true, nil
}

type s3Backing struct {
	cli    *s3.Client
	bucket string
	once   sync.Once
}

func (b *s3Backing) ensureBucket(ctx context.Context) error {
	var err error
	b.once.Do(func() {
		_, e := b.cli.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &b.bucket})
		if e != nil && !isAlreadyOwned(e) {
			err = e
		}
	})
	return err
}

func (b *s3Backing) Get(ctx context.Context, key string) ([]byte, bool, error) {
	out, err := b.cli.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	return data, err == nil, err
}

func (b *s3Backing) Put(ctx context.Context, key string, data []byte) error {
	_ = b.ensureBucket(ctx)
	_, err := b.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(data),
	})
	return err
}

func (b *s3Backing) Exists(ctx context.Context, key string) (bool, error) {
	_, err := b.cli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *s3Backing) Delete(ctx context.Context, key string) error {
	_, err := b.cli.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &key})
	return err
}

// isNotFound treats the missing-object error codes as "not present" (matches python's
// {NoSuchKey, NotFound, 404} plus NoSuchBucket).
func isNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404", "NoSuchBucket":
			return true
		}
	}
	return false
}

func isAlreadyOwned(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}
