package unitstore

import (
	"bytes"
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	// reader is handed (`backend/src/activities/datasets.ts` resolveBatch, Python's get_subunit),
	// so the writer's spelling round-trips whatever it is and nothing re-derives it. Changing this
	// one is a separate decision with a separate blast radius — `data/parquet.ts` builds its blob
	// URI as `s3://<bucket>/` + the carried key, on exactly that assumption — and it belongs with
	// prefix rows in conformance/blobkey.json, which has none either.
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
