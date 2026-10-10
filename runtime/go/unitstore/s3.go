package unitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3Putter is the real Putter over aws-sdk-go-v2 s3 (path-style, custom endpoint) — the same
// client shape the Go handler's object store uses.
type s3Putter struct {
	cli    *s3.Client
	bucket string
}

func (p *s3Putter) Put(ctx context.Context, key string, data []byte) error {
	_, err := p.cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &p.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// Get reads one object back — the commit path's read half (see commit.go). A missing key is
// ErrNotFound and nothing else is: every other error (a refused credential, a dropped connection)
// must stay retryable rather than be mistaken for data the store lost.
//
// "Missing" is the code set the handler's object store already uses for the same question
// (runtime/handler/internal/objectstore.isNotFound, and Python's get_commit): NoSuchKey, NotFound,
// 404. Matched on the API error CODE rather than only on the typed *types.NoSuchKey, because a
// gateway that answers a bare 404 surfaces as a generic API error with code "NotFound" — and it is
// asked through the one-method interface rather than smithy.APIError so this package does not take
// a direct dependency it does not have today.
func (p *s3Putter) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := p.cli.GetObject(ctx, &s3.GetObjectInput{Bucket: &p.bucket, Key: &key})
	if err != nil {
		var missing *types.NoSuchKey
		var coded interface{ ErrorCode() string }
		if errors.As(err, &missing) || (errors.As(err, &coded) && isMissingCode(coded.ErrorCode())) {
			return nil, fmt.Errorf("%w: s3://%s/%s", ErrNotFound, p.bucket, key)
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// List returns every key under prefix — a fresh execution's resume (commit.go, ListCommits).
// Paginated, because a LIST page stops at 1,000 keys and a batch can hold more Units than that. Any
// error returns as itself, retryable: unlike a GET, a LIST has no "absent" answer to tell apart.
func (p *s3Putter) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	pages := s3.NewListObjectsV2Paginator(p.cli, &s3.ListObjectsV2Input{Bucket: &p.bucket, Prefix: &prefix})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			if o.Key != nil {
				keys = append(keys, *o.Key)
			}
		}
	}
	return keys, nil
}

func isMissingCode(code string) bool {
	switch code {
	case "NoSuchKey", "NotFound", "404":
		return true
	}
	return false
}

// FromEnv builds a Store from the shared KONTRA_S3_* contract (identical to the Python actorkit
// unitstore and the Go handler object store). Returns (nil, nil) when KONTRA_S3_ENDPOINT is unset
// — the no-S3 dev/test mode, where the host collects emitted records inline instead.
func FromEnv(ctx context.Context) (*Store, error) {
	endpoint := os.Getenv("KONTRA_S3_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	bucket := envOr("KONTRA_S3_BUCKET", "kontra")
	region := envOr("KONTRA_S3_REGION", "us-east-1")
	access := envOr("KONTRA_S3_ACCESS_KEY", "kontra")
	secret := envOr("KONTRA_S3_SECRET_KEY", "kontra")
	// Raw, and concatenated verbatim by PutSubunit — matching Python's unitstore, which does the
	// same. NOT the CAS rule: `runtime/go/codec.objectKey` joins the prefix as a path segment
	// because a claim-check key is DERIVED from a digest independently on both sides, so the two
	// derivations have to agree. A sub-unit key is CARRIED, in the `{"$ref": {key,…}}` entry the
	// reader is handed (`control/orchestrator/src/activities/datasets.ts` resolveBatch, Python's get_subunit),
	// so the writer's spelling round-trips whatever it is and nothing re-derives it. Changing this
	// one is a separate decision with a separate blast radius — `data/parquet.ts` builds its blob
	// URI as `s3://<bucket>/` + the carried key, on exactly that assumption — and it belongs with
	// prefix rows in shared/conformance/blobkey.json, which has none either.
	prefix := os.Getenv("KONTRA_S3_PREFIX")

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
	// Best-effort bucket bootstrap (like the Go handler + Python's head/create). A concurrent
	// or already-owned bucket just errors here and is ignored; a real failure surfaces on Put.
	_, _ = cli.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
	return New(&s3Putter{cli: cli, bucket: bucket}, prefix), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
