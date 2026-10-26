// Package blobtest gives tests an S3 bucket: an in-process gofakes3 server on 127.0.0.1, its bucket created, the
// credentials in the test's environment, and the gate's Store over it (blob.Open). gofakes3 checks no request
// signature, so a presigned GET it serves proves the URL names the object, not that the signature is good; the
// pentest's SeaweedFS checks that (M3 plan, risks).
package blobtest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"kontra.sh/kontra/internal/blob"
)

// Bucket is the name of the test bucket.
const Bucket = "kontra-test"

// Server is one fake bucket.
type Server struct {
	// URL is the bucket's blobs: value (s3://kontra-test/<prefix>?endpoint=http://127.0.0.1:<port>).
	URL      string
	Config   blob.Config
	Store    blob.Store
	Endpoint string
	backend  *s3mem.Backend
}

// New starts a fake S3 server on 127.0.0.1 with the bucket, sets AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for
// the test (t.Setenv: the test must not be parallel), and opens the gate's Store over it under prefix (may be
// empty). The server stops when the test ends.
func New(t testing.TB, prefix string) *Server {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(Bucket); err != nil {
		t.Fatal(err)
	}
	faker := gofakes3.New(backend, gofakes3.WithLogger(gofakes3.DiscardLog()))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: faker.Server()}}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv(blob.EnvAccessKey, "kontra-test-access")
	t.Setenv(blob.EnvSecretKey, "kontra-test-secret")
	s := &Server{Endpoint: srv.URL, backend: backend}
	s.URL = "s3://" + Bucket
	if prefix != "" {
		s.URL += "/" + prefix
	}
	s.URL += "?endpoint=" + srv.URL + "&region=us-east-1"
	s.Config, err = blob.ParseURL(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Store, err = blob.Open(context.Background(), s.Config)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Keys lists the object keys in the bucket, sorted.
func (s *Server) Keys(t testing.TB) []string {
	t.Helper()
	list, err := s.backend.ListBucket(Bucket, &gofakes3.Prefix{}, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range list.Contents {
		out = append(out, c.Key)
	}
	sort.Strings(out)
	return out
}
