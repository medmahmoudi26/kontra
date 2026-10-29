package e2e

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"kontra.sh/kontra/internal/blob"
)

// The claim-check bucket of an M3 run (spec/relay.md §10.4, D167; Options.Blobs): a gofakes3 server in the test
// process, on 127.0.0.1 (D185), its bucket created, its credentials given to the replicas only (Harness.gateEnv).
// gofakes3 checks no request signature (internal/blob/blobtest says the same); the pentest's SeaweedFS does.

// BucketName is the run's bucket.
const BucketName = "kontra-e2e"

// Bucket is one fake S3 server.
type Bucket struct {
	URL      string // the blobs: value
	Endpoint string // http://127.0.0.1:<port>

	backend *s3mem.Backend
	srv     *http.Server
	l       net.Listener
	access  string
	secret  string
}

// StartBucket starts the server with the bucket.
func StartBucket() (*Bucket, error) {
	backend := s3mem.New()
	if err := backend.CreateBucket(BucketName); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	faker := gofakes3.New(backend, gofakes3.WithLogger(gofakes3.DiscardLog()))
	b := &Bucket{Endpoint: "http://" + l.Addr().String(), backend: backend, l: l,
		srv: &http.Server{Handler: faker.Server()}, access: "e2e-" + randomHex(8), secret: randomHex(20)}
	b.URL = "s3://" + BucketName + "?endpoint=" + b.Endpoint + "&region=us-east-1"
	go func() { _ = b.srv.Serve(l) }()
	return b, nil
}

// Env is what a replica needs to reach the bucket: its credentials.
func (b *Bucket) Env() []string {
	return []string{blob.EnvAccessKey + "=" + b.access, blob.EnvSecretKey + "=" + b.secret}
}

// Objects returns every object in the bucket, by key.
func (b *Bucket) Objects() (map[string][]byte, error) {
	list, err := b.backend.ListBucket(BucketName, &gofakes3.Prefix{}, gofakes3.ListBucketPage{})
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, c := range list.Contents {
		obj, err := b.backend.GetObject(BucketName, c.Key, nil)
		if err != nil {
			return nil, fmt.Errorf("e2e: bucket object %s: %w", c.Key, err)
		}
		data, err := io.ReadAll(obj.Contents)
		obj.Contents.Close()
		if err != nil {
			return nil, err
		}
		out[c.Key] = data
	}
	return out, nil
}

// Keys lists the object keys, sorted.
func (b *Bucket) Keys() ([]string, error) {
	objs, err := b.Objects()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(objs))
	for k := range objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// Close stops the server.
func (b *Bucket) Close() {
	_ = b.srv.Close()
	_ = b.l.Close()
}
