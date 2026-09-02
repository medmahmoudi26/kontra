package objstore

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAWSChunkedBodiesAreUnwrapped is the silent-corruption guard, and no SDK test can reach it:
// an AWS SDK only switches to `aws-chunked` framing for an unseekable body over TLS, and the
// appliance serves plain HTTP to seekable ones. So the framing is written by hand here.
//
// What is being prevented: a store that ignores `x-amz-content-sha256: STREAMING-…` stores the
// hex length lines and per-chunk signatures AS the object, answers 200, and the damage surfaces
// somewhere else entirely — as a sha256 mismatch in the CAS, or as a parquet file DuckDB cannot
// open. A boto3 bump or an https endpoint is enough to turn that framing on with no change here.
func TestAWSChunkedBodiesAreUnwrapped(t *testing.T) {
	srv := startTestS3(t)
	payload := bytes.Repeat([]byte("kontra"), 5000) // 30 KB, several chunks

	for _, tc := range []struct {
		name   string
		sha256 string
		body   func([]byte) string
	}{
		{
			name:   "signed chunks",
			sha256: "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
			body: func(p []byte) string {
				var b strings.Builder
				for off := 0; off < len(p); off += 8192 {
					end := min(off+8192, len(p))
					fmt.Fprintf(&b, "%x;chunk-signature=%064x\r\n%s\r\n", end-off, 0, p[off:end])
				}
				fmt.Fprintf(&b, "0;chunk-signature=%064x\r\n\r\n", 0)
				return b.String()
			},
		},
		{
			name:   "unsigned chunks with a trailer",
			sha256: "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
			body: func(p []byte) string {
				var b strings.Builder
				fmt.Fprintf(&b, "%x\r\n%s\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n", len(p), p)
				return b.String()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "cas/ab/" + strings.ReplaceAll(tc.name, " ", "-")
			framed := tc.body(payload)
			req, err := http.NewRequest(http.MethodPut, srv.Endpoint()+"/"+DefaultS3Bucket+"/"+key, strings.NewReader(framed))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("x-amz-content-sha256", tc.sha256)
			req.Header.Set("Content-Encoding", "aws-chunked")
			req.Header.Set("x-amz-decoded-content-length", fmt.Sprint(len(payload)))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PUT returned %d: %s", resp.StatusCode, body)
			}

			f, info, err := srv.store.open(DefaultS3Bucket, key)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, _ := io.ReadAll(f)
			if !bytes.Equal(got, payload) {
				t.Fatalf("stored %d bytes, want %d — the chunk framing was stored as the object",
					info.Size, len(payload))
			}
		})
	}

	// A body that ends early must not be accepted as a complete object. Silently storing a short
	// object is the failure this whole file is written against.
	t.Run("a truncated chunked body is refused", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, srv.Endpoint()+"/"+DefaultS3Bucket+"/cas/ab/short",
			strings.NewReader("4\r\nabcd\r\n0\r\n\r\n"))
		req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
		req.Header.Set("x-amz-decoded-content-length", "9999")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("a body that ended short of its declared length was stored as a complete object")
		}
		if _, err := srv.store.stat(DefaultS3Bucket, "cas/ab/short"); err == nil {
			t.Fatal("a truncated write left an object behind")
		}
	})
}

// TestKeysThatCannotBecomePathsAreRefused. The store maps keys onto paths, so every key that a
// filesystem would resolve somewhere other than where the key says has to be refused BY NAME —
// not normalised into something else, which is how a traversal becomes a write outside the
// bucket.
func TestKeysThatCannotBecomePathsAreRefused(t *testing.T) {
	store, err := newObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ why, key string }{
		{"parent traversal", "cas/../../etc/passwd"},
		{"current directory", "cas/./ab"},
		{"empty segment", "cas//ab"},
		{"leading slash", "/cas/ab"},
		{"trailing slash", "cas/ab/"},
		{"a NUL byte", "cas/a\x00b"},
		{"empty key", ""},
	} {
		if _, err := store.put("kontra", tc.key, strings.NewReader("x")); err == nil {
			t.Errorf("%s: %q was accepted", tc.why, tc.key)
		}
	}

	// And nothing escaped: the only thing in the root is the bucket the writes should have gone
	// to, if it was created at all.
	ents, _ := os.ReadDir(store.root)
	for _, e := range ents {
		if e.Name() != "kontra" {
			t.Errorf("a refused key created %q in the store root", e.Name())
		}
	}
}

// TestFailedWritesLeaveNoPartialObject — the write path is temp-file → fsync → rename, so a
// failure at any step leaves the previous object (or no object), never a truncated one. A
// truncated CAS object would fail its sha256 check on read and report corruption rather than the
// write failure that actually happened.
func TestFailedWritesLeaveNoPartialObject(t *testing.T) {
	store, err := newObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.put("kontra", "cas/ab/abcd", strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	store.injectWriteError = func(string) error { return io.ErrUnexpectedEOF }
	if _, err := store.put("kontra", "cas/ab/abcd", strings.NewReader("replacement")); err == nil {
		t.Fatal("a failing write reported success")
	}
	store.injectWriteError = nil

	f, _, err := store.open("kontra", "cas/ab/abcd")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "original" {
		t.Fatalf("a failed write damaged the existing object: %q", got)
	}
	matches, _ := filepath.Glob(filepath.Join(store.root, "kontra", "cas", "ab", ".partial-*"))
	if len(matches) != 0 {
		t.Errorf("temporary files survived a failed write: %v", matches)
	}
}

// TestBucketNamesAreValidated — a bucket is a directory, so a name that is a path is a write
// outside the store.
func TestBucketNamesAreValidated(t *testing.T) {
	store, err := newObjectStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"..", ".hidden", "UPPER", "a", "with/slash", "trailing-", strings.Repeat("x", 64)} {
		if err := store.createBucket(name); err == nil {
			t.Errorf("bucket name %q was accepted", name)
		}
	}
	for _, name := range []string{"kontra", "kontra-datasets", "a.b.c"} {
		if err := store.createBucket(name); err != nil {
			t.Errorf("bucket name %q was refused: %v", name, err)
		}
	}
}
