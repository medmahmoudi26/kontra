package codec

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// objectKey joins a store prefix and key parts as PATH SEGMENTS: strip leading and trailing '/'
// from EVERY segment, the prefix included; drop the ones that are then empty; join what is left
// with a single '/'. An empty prefix yields a key with no leading slash.
//
// WHAT IT REPLACES. Put and Get spelled the address `s.prefix + key`, plain concatenation, under
// a comment that read "raw; every SDK concatenates it verbatim" — and that comment was the
// defect, not a description of one. It was true of Python's casstore and of nothing
// else: `handler/internal/objectstore.Key` and `backend/src/codec/objectStore.ts:key`
// both join, so with KONTRA_S3_PREFIX=`slice11` a Go actor wrote `slice11cas/df/df5b…` while the
// handler, the orchestrator and the CLI asked for `slice11/cas/df/df5b…`. actorkit and the
// handler are separate modules and never import each other, so what holds the four
// implementations to one answer is the `prefixCases` rows of shared/conformance/codec/fixtures.json.
func objectKey(prefix string, parts ...string) string {
	var bits []string
	if p := strings.Trim(prefix, "/"); p != "" {
		bits = append(bits, p)
	}
	for _, part := range parts {
		if seg := strings.Trim(part, "/"); seg != "" {
			bits = append(bits, seg)
		}
	}
	return strings.Join(bits, "/")
}

// prefixed addresses another Store under a key prefix.
//
// THE TRANSPORT IS ONE THING AND THE ADDRESS IS ANOTHER — the split handler/claimcheck's header
// argues for, made here for the same reason: a backing knows how to move bytes to a bucket, and
// where in that bucket they land is a contract with three other languages. It is also what lets
// the conformance arm drive the real join over an in-memory store, with no AWS client and no S3.
type prefixed struct {
	inner  Store
	prefix string
}

func (p prefixed) Put(ctx context.Context, key string, data []byte) error {
	return p.inner.Put(ctx, objectKey(p.prefix, key), data)
}

func (p prefixed) Get(ctx context.Context, key string) ([]byte, error) {
	return p.inner.Get(ctx, objectKey(p.prefix, key))
}

// WithPrefix wraps a Store so every key it is handed is addressed under `prefix`
// (KONTRA_S3_PREFIX; empty is the common case and is a no-op).
func WithPrefix(inner Store, prefix string) Store { return prefixed{inner: inner, prefix: prefix} }

// s3Store is the real Store over aws-sdk-go-v2 (path-style, custom endpoint) — the same client
// shape runtime/go/unitstore and the Go handler's object store use. Separate from unitstore's
// because that one is write-only: the codec must also READ, to rehydrate a ref.
//
// It takes keys VERBATIM; the prefix is prefixed's, not this type's.
type s3Store struct {
	cli    *s3.Client
	bucket string
}

func (s *s3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/octet-stream"),
	})
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.cli.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// StoreFromEnv builds the CAS store from the shared KONTRA_S3_* contract, or returns nil when
// KONTRA_S3_ENDPOINT is unset.
//
// nil means the codec runs in PASSTHROUGH, which is correct for a local run with no object store
// and safe because the handler's codec is passthrough under the same condition: neither side
// offloads, so neither side has anything to fetch. It NEVER falls back to a default endpoint — a
// codec pointed at the wrong store fails at decode time, on another machine.
func StoreFromEnv(ctx context.Context) (Store, error) {
	endpoint := os.Getenv("KONTRA_S3_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	region := envOr("KONTRA_S3_REGION", "us-east-1")
	access := envOr("KONTRA_S3_ACCESS_KEY", "kontra")
	secret := envOr("KONTRA_S3_SECRET_KEY", "kontra")

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		return nil, err
	}
	cli := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true // SeaweedFS / MinIO path-style addressing
	})
	return WithPrefix(
		&s3Store{cli: cli, bucket: envOr("KONTRA_S3_BUCKET", "kontra")},
		os.Getenv("KONTRA_S3_PREFIX"),
	), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
