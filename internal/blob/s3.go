package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Credentials' environment variables (D167).
const (
	EnvAccessKey = "AWS_ACCESS_KEY_ID"
	EnvSecretKey = "AWS_SECRET_ACCESS_KEY"
)

// defaultEndpoint is the provider's endpoint when blobs: names none (Amazon S3).
const defaultEndpoint = "https://s3.amazonaws.com"

// s3Store is Store over the S3 API (minio-go v7).
type s3Store struct {
	c      *minio.Client
	bucket string
	prefix string
}

var _ Store = (*s3Store)(nil)

// Open connects to the bucket cfg names, with the credentials of AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY,
// and checks that it exists and is writable (it writes and removes one small object under the prefix), so a
// gate with a bucket it cannot use refuses to start instead of failing the first large part.
//
// SPEC-CONFLICT (spec/relay.md §10.3 "Every other operator URL follows this section", D67): read literally, the
// bucket's endpoint would go through the SSRF dialer, https only and globally reachable addresses only, with no
// allow_private for blobs:. The bucket is the gate's own storage, like its Postgres and Temporal, and a team
// gate's is usually on a private network (MinIO or SeaweedFS beside the gate, the e2e harness's gofakes3 on
// 127.0.0.1); nothing a caller or a seller writes ever chooses the address. The closest faithful behaviour:
// the endpoint is dialed as configured (http:// only when the operator wrote it), the environment's proxy is
// ignored and redirects are never followed, as §10.3 says of every connection.
func Open(ctx context.Context, cfg Config) (Store, error) {
	access, secret := os.Getenv(EnvAccessKey), os.Getenv(EnvSecretKey)
	if access == "" || secret == "" {
		return nil, fmt.Errorf("blob: blobs: %s and %s must be set", EnvAccessKey, EnvSecretKey)
	}
	return open(ctx, cfg, access, secret)
}

func open(ctx context.Context, cfg Config, access, secret string) (Store, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	e, err := url.Parse(endpoint)
	if err != nil || e.Host == "" {
		return nil, fmt.Errorf("blob: blobs: endpoint %q", endpoint)
	}
	secure := e.Scheme == "https"
	tr, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, fmt.Errorf("blob: transport: %w", err)
	}
	tr.Proxy = nil // never the environment's proxy (spec §10.3)
	c, err := minio.New(e.Host, &minio.Options{
		Creds:     credentials.NewStaticV4(access, secret, ""),
		Secure:    secure,
		Region:    cfg.Region,
		Transport: tr,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: client: %w", err)
	}
	s := &s3Store{c: c, bucket: cfg.Bucket, prefix: cfg.Prefix}
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenTimeout bounds Open's check of the bucket. minio-go retries a request that gets no answer (ten times, each
// waiting for a dial or for response headers), so a bucket the gate cannot reach held the gate's start for minutes,
// never healthy and with nothing logged (M3 WP-10 pentest finding F-3); past this bound the gate refuses to start.
var OpenTimeout = 30 * time.Second

// check is Open's test: the bucket exists, and an object can be written and removed under the prefix.
func (s *s3Store) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("blob: bucket %s: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("blob: bucket %s does not exist", s.bucket)
	}
	ref, err := NewRef()
	if err != nil {
		return err
	}
	name := ".kontra-write-check/" + string(ref)
	if s.prefix != "" {
		name = s.prefix + "/" + name
	}
	probe := []byte("kontra")
	if _, err := s.c.PutObject(ctx, s.bucket, name, bytes.NewReader(probe), int64(len(probe)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return fmt.Errorf("blob: bucket %s is not writable: %w", s.bucket, err)
	}
	if err := s.c.RemoveObject(ctx, s.bucket, name, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("blob: bucket %s: remove the write check: %w", s.bucket, err)
	}
	return nil
}

func (s *s3Store) name(k Key) (string, error) {
	if !k.Ref.Valid() || k.Tenant == uuid.Nil || k.Task == uuid.Nil {
		return "", fmt.Errorf("blob: key %+v", k)
	}
	return ObjectName(s.prefix, k), nil
}

// Put stores exactly size bytes from r under k: fewer is an error, and r is not read past size.
func (s *s3Store) Put(ctx context.Context, k Key, r io.Reader, size int64) error {
	name, err := s.name(k)
	if err != nil {
		return err
	}
	if size <= 0 || size > MaxObject {
		return fmt.Errorf("blob: an object of %d bytes (1 to %d)", size, MaxObject)
	}
	cr := &countReader{r: io.LimitReader(r, size)}
	if _, err := s.c.PutObject(ctx, s.bucket, name, cr, size, minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return fmt.Errorf("blob: put %s: %w", name, err)
	}
	if cr.n != size {
		return fmt.Errorf("blob: put %s: read %d of %d bytes", name, cr.n, size)
	}
	return nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Open returns the object and its size; an object the bucket does not hold is ErrNotFound.
func (s *s3Store) Open(ctx context.Context, k Key) (io.ReadCloser, int64, error) {
	name, err := s.name(k)
	if err != nil {
		return nil, 0, err
	}
	o, err := s.c.GetObject(ctx, s.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, s.mapErr(name, err)
	}
	st, err := o.Stat()
	if err != nil {
		_ = o.Close()
		return nil, 0, s.mapErr(name, err)
	}
	return o, st.Size, nil
}

func (s *s3Store) mapErr(name string, err error) error {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.Code == minio.NoSuchKey || resp.StatusCode == http.StatusNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return fmt.Errorf("blob: %s: %w", name, err)
}

// Presign returns a GET URL valid for ttl (at most 7 days, S3's limit). It is computed locally once the
// bucket's region is known (blobs: names it, or the client asked the bucket once).
func (s *s3Store) Presign(ctx context.Context, k Key, ttl time.Duration) (string, error) {
	name, err := s.name(k)
	if err != nil {
		return "", err
	}
	u, err := s.c.PresignedGetObject(ctx, s.bucket, name, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("blob: presign %s: %w", name, err)
	}
	return u.String(), nil
}

// Delete removes one object; an object the bucket does not hold is no error (S3's DeleteObject answers 204 for a
// missing key).
func (s *s3Store) Delete(ctx context.Context, k Key) error {
	name, err := s.name(k)
	if err != nil {
		return err
	}
	if err := s.c.RemoveObject(ctx, s.bucket, name, minio.RemoveObjectOptions{}); err != nil {
		if errors.Is(s.mapErr(name, err), ErrNotFound) {
			return nil
		}
		return fmt.Errorf("blob: delete %s: %w", name, err)
	}
	return nil
}

// DeleteTask removes every object under the task's prefix, <prefix>/<tenant>/<task>/. A task without objects is
// no error.
func (s *s3Store) DeleteTask(ctx context.Context, tenant, task uuid.UUID) error {
	if tenant == uuid.Nil || task == uuid.Nil {
		return errors.New("blob: delete: no tenant or task")
	}
	prefix := taskPrefix(s.prefix, tenant, task)
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	objects := make(chan minio.ObjectInfo)
	var listErr error
	listed := make(chan struct{})
	go func() {
		defer close(listed)
		defer close(objects)
		for o := range s.c.ListObjects(lctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if o.Err != nil {
				listErr = o.Err
				return
			}
			if !strings.HasPrefix(o.Key, prefix) {
				continue
			}
			select {
			case objects <- o:
			case <-lctx.Done():
				return
			}
		}
	}()
	var firstErr error
	for e := range s.c.RemoveObjects(lctx, s.bucket, objects, minio.RemoveObjectsOptions{}) {
		if e.Err != nil && firstErr == nil {
			firstErr = fmt.Errorf("blob: delete %s: %w", e.ObjectName, e.Err)
		}
	}
	cancel()
	<-listed
	if firstErr != nil {
		return firstErr
	}
	if listErr != nil {
		return fmt.Errorf("blob: list %s: %w", prefix, listErr)
	}
	return nil
}
