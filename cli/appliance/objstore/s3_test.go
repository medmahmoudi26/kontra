package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// startTestS3 brings up a store on its own temp directory and a free port. Never the default
// port: a developer box running the compose stack has SeaweedFS on 8333 and the suite must not
// depend on, or disturb, whatever is already there.
func startTestS3(t *testing.T, opts ...func(*Options)) *Server {
	t.Helper()
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		DataDir: t.TempDir(),
		Port:    port,
		Logf:    func(format string, args ...any) { t.Logf("store: "+format, args...) },
	}
	for _, f := range opts {
		f(&o)
	}
	srv, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

// client builds the S3 client THE WAY KONTRA BUILDS IT. The construction is copied from
// handler/internal/objectstore.go's FromEnv — static credentials, an explicit BaseEndpoint, and
// path-style addressing — because that, and not a convenient client, is what has to work. The
// Python (boto3, `endpoint_url=`) and TypeScript (`forcePathStyle: true`) stores are configured
// identically; there is one wire contract and this is a Go-side witness to it.
func s3client(t *testing.T, srv *Server) *s3.Client {
	t.Helper()
	return s3.NewFromConfig(aws.Config{
		Region:      DefaultS3Region,
		Credentials: credentials.NewStaticCredentialsProvider(DefaultS3AccessKey, DefaultS3SecretKey, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.Endpoint())
		o.UsePathStyle = true
	})
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

// TestGoldenBlobKeysRoundTripThroughTheSDK is the acceptance test for this store.
//
// shared/conformance/blobkey.json is the ONE fixture that pins the object layout across the Go,
// Python and TypeScript hosts (ADR 0015): the same inputs must produce the same key in all three,
// or a reader silently sees two layouts and half a run's data goes missing. The fixture
// itself only checks key CONSTRUCTION; this checks that the store the appliance ships can hold
// every key the fixture blesses and give it back byte-for-byte, through a real AWS SDK client
// configured the way kontra configures one.
//
// The keys are the interesting part and they are why the store maps keys onto paths rather than
// hashing them: `shard=0007`, `actor=a_b_c`, `unit=00011` and a `.json` leaf are directories and a
// file, so `ls` over the data directory is a readable index of a run.
func TestGoldenBlobKeysRoundTripThroughTheSDK(t *testing.T) {
	var fx struct {
		DT    string `json:"dt"`
		Cases []struct {
			Why    string `json:"why"`
			Expect string `json:"expect"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "conformance", "blobkey.json"))
	if err != nil {
		t.Fatalf("read the cross-SDK fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("fixture is empty — a vacuously passing conformance test is worse than none")
	}

	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	for _, tc := range fx.Cases {
		body := []byte(`[{"why":` + fmt.Sprintf("%q", tc.Why) + `}]`)
		if _, err := c.PutObject(cx, &s3.PutObjectInput{
			Bucket: aws.String(DefaultS3Bucket), Key: aws.String(tc.Expect), Body: bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("%s: PUT %s: %v", tc.Why, tc.Expect, err)
		}
		out, err := c.GetObject(cx, &s3.GetObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(tc.Expect)})
		if err != nil {
			t.Fatalf("%s: GET %s: %v", tc.Why, tc.Expect, err)
		}
		got, _ := io.ReadAll(out.Body)
		out.Body.Close()
		if !bytes.Equal(got, body) {
			t.Fatalf("%s: %s round-tripped to different bytes:\n  got  %s\n  want %s", tc.Why, tc.Expect, got, body)
		}
		if _, err := c.HeadObject(cx, &s3.HeadObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(tc.Expect)}); err != nil {
			t.Fatalf("%s: HEAD %s: %v", tc.Why, tc.Expect, err)
		}
	}

	// And the layout is on disk under the key, not under a hash of it — the property that makes
	// the store legible to `ls`, `du` and `tar`.
	first := fx.Cases[0].Expect
	onDisk := filepath.Join(srv.DataDir(), "objects", DefaultS3Bucket, filepath.FromSlash(first))
	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("object is not on disk at its key: %v", err)
	}

	// The CAS layout the claim-check codec uses (cas/<sha[:2]>/<sha>) is the other half of the
	// cross-SDK contract, so it gets the same treatment.
	payload := bytes.Repeat([]byte("claim-check"), 64)
	sum := sha256.Sum256(payload)
	casKey := "cas/" + hex.EncodeToString(sum[:])[:2] + "/" + hex.EncodeToString(sum[:])
	if _, err := c.PutObject(cx, &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String(casKey), Body: bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("PUT %s: %v", casKey, err)
	}
	out, err := c.GetObject(cx, &s3.GetObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(casKey)})
	if err != nil {
		t.Fatalf("GET %s: %v", casKey, err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	// The integrity check the CAS itself performs on every read. It is the check that actually
	// matters in this repo, and it is a sha256 over the bytes — never the ETag.
	if back := sha256.Sum256(got); back != sum {
		t.Fatalf("CAS object failed its own sha256 check after a round trip")
	}
}

// TestBucketExistsOnFirstUse is the `seaweed-bucket` one-shot, deleted.
//
// The failure being designed out: a fresh SeaweedFS has no buckets and answers a write into a
// missing one with 403 AccessDenied, which reads as a credentials problem on a machine where
// nothing is wrong with the credentials. Two things make it impossible here — the configured
// buckets exist before the listener opens, and a PUT into an unconfigured one creates it.
func TestBucketExistsOnFirstUse(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	buckets, err := c.ListBuckets(cx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, b := range buckets.Buckets {
		seen[aws.ToString(b.Name)] = true
	}
	if !seen[DefaultS3Bucket] {
		t.Errorf("bucket %q was not created on the way up (a fresh store must not need a one-shot)", DefaultS3Bucket)
	}

	// No CreateBucket call anywhere before this write.
	if _, err := c.PutObject(cx, &s3.PutObjectInput{
		Bucket: aws.String("kontra-datasets"), Key: aws.String("a/b.json"), Body: strings.NewReader("{}"),
	}); err != nil {
		t.Fatalf("first write to an unconfigured bucket must not 403: %v", err)
	}

	// And CreateBucket is idempotent, because all three SDK stores call it best-effort before
	// their first write.
	for i := 0; i < 2; i++ {
		if _, err := c.CreateBucket(cx, &s3.CreateBucketInput{Bucket: aws.String(DefaultS3Bucket)}); err != nil {
			t.Fatalf("CreateBucket #%d on an existing bucket: %v", i+1, err)
		}
	}
}

// TestMultipartRoundTripsAboveTheClaimCheckThreshold covers the write path the materializer
// actually uses: DuckDB's httpfs writes every parquet file to S3 as a multipart upload whatever
// its size, so a store without multipart has no typed output at all.
//
// The payload is deliberately larger than the codec's 128 KiB claim-check threshold and split
// across parts, which is the shape a real DuckLake write has.
func TestMultipartRoundTripsAboveTheClaimCheckThreshold(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	const partSize = 96 * 1024
	// 300 KiB over 96 KiB parts: comfortably past the codec's 128 KiB claim-check threshold, and
	// a ragged last part (300 = 96+96+96+12) so an off-by-one in the concatenation shows up as
	// wrong bytes rather than only as a wrong length.
	payload := make([]byte, 300*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	key := "datasets/actor=crawl/version=0.1.0/dt=2026-08-26/part-0.parquet"

	start, err := c.CreateMultipartUpload(cx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	var parts []types.CompletedPart
	for i, off := 0, 0; off < len(payload); i, off = i+1, off+partSize {
		end := min(off+partSize, len(payload))
		up, err := c.UploadPart(cx, &s3.UploadPartInput{
			Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key),
			UploadId: start.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(payload[off:end]),
		})
		if err != nil {
			t.Fatalf("part %d: %v", i+1, err)
		}
		// An UploadPart response with no ETag aborts a DuckDB write, so its absence is a failure
		// here rather than something the completion quietly works around.
		if aws.ToString(up.ETag) == "" {
			t.Fatalf("part %d came back with no ETag", i+1)
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}
	done, err := c.CompleteMultipartUpload(cx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key), UploadId: start.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(aws.ToString(done.ETag), fmt.Sprintf("-%d\"", len(parts))) {
		t.Errorf("a multipart object's ETag should end in its part count (-%d), got %q", len(parts), aws.ToString(done.ETag))
	}

	out, err := c.GetObject(cx, &s3.GetObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("multipart object read back wrong: %d bytes, want %d", len(got), len(payload))
	}

	// Nothing is left behind. A finished upload that leaves its parts on disk doubles the cost of
	// every dataset the materializer writes.
	if ents, err := os.ReadDir(filepath.Join(srv.DataDir(), "objects", uploadsDir)); err == nil && len(ents) != 0 {
		t.Errorf("%d upload directories survived a completed multipart", len(ents))
	}

	// The range read DuckDB does before anything else: the parquet footer.
	tail, err := c.GetObject(cx, &s3.GetObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key), Range: aws.String("bytes=307192-307199"),
	})
	if err != nil {
		t.Fatal(err)
	}
	footer, _ := io.ReadAll(tail.Body)
	tail.Body.Close()
	if !bytes.Equal(footer, payload[307192:307200]) {
		t.Fatalf("range read returned the wrong bytes: %v", footer)
	}
	if aws.ToInt64(tail.ContentLength) != 8 {
		t.Errorf("range read Content-Length %d, want 8", aws.ToInt64(tail.ContentLength))
	}
}

// TestAbortedMultipartLeavesNothing — an aborted upload must not hold disk forever.
func TestAbortedMultipartLeavesNothing(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	start, err := c.CreateMultipartUpload(cx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("half/written.parquet"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadPart(cx, &s3.UploadPartInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("half/written.parquet"),
		UploadId: start.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("partial"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AbortMultipartUpload(cx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("half/written.parquet"), UploadId: start.UploadId,
	}); err != nil {
		t.Fatal(err)
	}
	if ents, err := os.ReadDir(filepath.Join(srv.DataDir(), "objects", uploadsDir)); err == nil && len(ents) != 0 {
		t.Errorf("%d upload directories survived an abort", len(ents))
	}
	if _, err := c.HeadObject(cx, &s3.HeadObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("half/written.parquet"),
	}); err == nil {
		t.Error("an aborted upload produced an object")
	}
}

// TestPresignedGetResolvesWithNoCredentials is `kontra explore`'s whole surface in one test.
//
// The orchestrator hands an operator a presigned URL and the URL IS the read credential: it is
// opened by curl, by a browser, or by DuckDB's httpfs on a workstation that holds no keys at all.
// So the check is deliberately made with a bare http.Get and NOT with the SDK — an SDK client
// would sign the request itself and prove nothing about the URL.
func TestPresignedGetResolvesWithNoCredentials(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	key := "datasets/actor=crawl/dt=2026-08-26/run_id=r1/part-0.parquet"
	want := []byte("PAR1parquet-bytes")
	if _, err := c.PutObject(cx, &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key), Body: bytes.NewReader(want),
	}); err != nil {
		t.Fatal(err)
	}

	url, err := srv.PresignGet(DefaultS3Bucket, key, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("presigned GET returned %d: %s", resp.StatusCode, body)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Fatalf("presigned GET returned %q, want %q", got, want)
	}

	// AND AN UNSIGNED GET STILL WORKS. Signatures are parsed and not enforced (s3.go), which is a
	// recorded open decision rather than an accident — so a request carrying no credential at all
	// must be served, not refused. The example that used to stand here was a fleet Machine's
	// cloud-init fetching its Bundle with a bare `curl`; ADR 0036 moved that reader to the
	// registry, and the property it depended on is asserted on its own now instead of through a
	// caller that no longer exists.
	if _, err := c.PutObject(cx, &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("public/anon.txt"),
		Body: strings.NewReader("readable"),
	}); err != nil {
		t.Fatal(err)
	}
	anon, err := http.Get(srv.Endpoint() + "/" + DefaultS3Bucket + "/public/anon.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer anon.Body.Close()
	if anon.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET returned %d — signatures are parsed here, never enforced", anon.StatusCode)
	}
}

// TestListPagesWithContinuation exercises LIST past one page.
//
// Continuation is not just "more results": it only works if the store emits keys in S3's order,
// which is lexicographic over the KEY and is NOT the order a directory walk produces. See
// objects.go's header, point 1.
func TestListPagesWithContinuation(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	// Over one default page (1000), and hive-partitioned so the walk has real depth to get wrong.
	const total = 1100
	want := make([]string, 0, total)
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("units/run=r1/dt=2026-08-26/actor=crawl/shard=%04d/unit=%05d/aa.json", i%17, i)
		want = append(want, key)
		if _, err := srv.store.put(DefaultS3Bucket, key, strings.NewReader("{}")); err != nil {
			t.Fatal(err)
		}
	}
	sortStrings(want)

	for _, pageSize := range []int32{0, 100, 1000} {
		t.Run(fmt.Sprintf("max-keys=%d", pageSize), func(t *testing.T) {
			var got []string
			var token *string
			pages := 0
			for {
				in := &s3.ListObjectsV2Input{
					Bucket: aws.String(DefaultS3Bucket), Prefix: aws.String("units/"), ContinuationToken: token,
				}
				if pageSize > 0 {
					in.MaxKeys = aws.Int32(pageSize)
				}
				out, err := c.ListObjectsV2(cx, in)
				if err != nil {
					t.Fatal(err)
				}
				pages++
				for _, o := range out.Contents {
					got = append(got, aws.ToString(o.Key))
				}
				if !aws.ToBool(out.IsTruncated) {
					break
				}
				if aws.ToString(out.NextContinuationToken) == "" {
					t.Fatal("truncated listing with no continuation token — the client cannot ask for the rest")
				}
				token = out.NextContinuationToken
				if pages > total {
					t.Fatal("listing did not terminate")
				}
			}
			if pages < 2 {
				t.Fatalf("only %d page(s) for %d keys — the test is not exercising continuation", pages, total)
			}
			if len(got) != len(want) {
				t.Fatalf("listed %d keys over %d pages, want %d", len(got), pages, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("key %d out of order or missing:\n  got  %s\n  want %s", i, got[i], want[i])
				}
			}
		})
	}
}

// TestListOrdersByKeyNotByFilename is the ordering bug a directory walk has for free, isolated.
//
// '-' is 0x2D and '/' is 0x2F, so the key `a-c` sorts BEFORE `a/b`. A depth-first walk over the
// tree visits the directory `a` first and emits them the other way round — and a listing in the
// wrong order makes a continuation token skip or repeat keys.
func TestListOrdersByKeyNotByFilename(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	keys := []string{"a/b", "a-c", "a.d", "ab/e", "a/b2"}
	for _, k := range keys {
		if _, err := srv.store.put(DefaultS3Bucket, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	want := append([]string(nil), keys...)
	sortStrings(want)

	out, err := c.ListObjectsV2(cx, &s3.ListObjectsV2Input{Bucket: aws.String(DefaultS3Bucket)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range out.Contents {
		got = append(got, aws.ToString(o.Key))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("listing is not in S3 key order:\n  got  %v\n  want %v", got, want)
	}
}

// TestListDelimiterGroupsPartitions is how the dataset browser and DuckDB's glob walk a
// hive-partitioned bucket without reading every key under it.
func TestListDelimiterGroupsPartitions(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	for _, k := range []string{
		"datasets/actor=crawl/dt=2026-08-25/part-0.parquet",
		"datasets/actor=crawl/dt=2026-08-26/part-0.parquet",
		"datasets/actor=nscheck/dt=2026-08-26/part-0.parquet",
		"units/run=r1/x.json",
	} {
		if _, err := srv.store.put(DefaultS3Bucket, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	out, err := c.ListObjectsV2(cx, &s3.ListObjectsV2Input{
		Bucket: aws.String(DefaultS3Bucket), Prefix: aws.String("datasets/"), Delimiter: aws.String("/"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var prefixes []string
	for _, p := range out.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	if strings.Join(prefixes, ",") != "datasets/actor=crawl/,datasets/actor=nscheck/" {
		t.Fatalf("common prefixes %v", prefixes)
	}
	if len(out.Contents) != 0 {
		t.Errorf("a delimited listing at a directory boundary should return no objects, got %d", len(out.Contents))
	}
}

// TestDiskFullFailsLoudlyNamingItsCause is the second failure this store exists to not inherit.
//
// SeaweedFS's volume-slot ceiling turned every write to a new bucket into a bare HTTP 500 with no
// body, while the run's own isolation counters reported nothing wrong — so "the store is full"
// arrived looking like "the store is broken", days late. The requirement is not that a full disk
// is survivable; it is that the caller is TOLD, in words, what happened.
//
// A real ENOSPC needs a full filesystem, which needs a loopback mount and root privileges, so the
// error the filesystem would return is injected instead. What is under test is everything after
// the syscall: the classification, the status code, and the sentence that reaches the client.
func TestDiskFullFailsLoudlyNamingItsCause(t *testing.T) {
	var logged []string
	srv := startTestS3(t, func(o *Options) {
		o.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	})
	srv.store.injectWriteError = func(string) error { return syscall.ENOSPC }
	c := s3client(t, srv)

	_, err := c.PutObject(ctx(t), &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("cas/ab/abcd"), Body: strings.NewReader("payload"),
	})
	if err == nil {
		t.Fatal("a write onto a full disk returned success")
	}
	var api smithy.APIError
	if !errors.As(err, &api) {
		t.Fatalf("the SDK did not surface an API error: %v", err)
	}
	if api.ErrorCode() != "InsufficientStorage" {
		t.Errorf("error code %q — a full disk must not read as a credentials or a not-found problem", api.ErrorCode())
	}
	for _, want := range []string{"no space left on device", "nothing was written"} {
		if !strings.Contains(api.ErrorMessage(), want) {
			t.Errorf("the error reaching the caller does not say %q: %s", want, api.ErrorMessage())
		}
	}
	// And the store says so itself. A 5xx that only the caller sees is a 5xx nobody looks for.
	if len(logged) == 0 || !strings.Contains(strings.Join(logged, "\n"), "no space left on device") {
		t.Errorf("the store logged nothing about a failed write: %v", logged)
	}

	// Nothing half-written survives: a truncated object would be read back by the CAS and fail its
	// sha256 check, reporting corruption instead of a full disk.
	srv.store.injectWriteError = nil
	if _, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("cas/ab/abcd"),
	}); err == nil {
		t.Error("a failed write left an object behind")
	}
	ents, _ := filepath.Glob(filepath.Join(srv.DataDir(), "objects", DefaultS3Bucket, "cas", "ab", ".partial-*"))
	if len(ents) != 0 {
		t.Errorf("a failed write left temporary files: %v", ents)
	}
}

// TestWriteFailureNamesItsCauseWithoutInjection is the same requirement with no test seam in the
// way: a genuine filesystem refusal, produced by the one key shape a directory tree cannot hold.
func TestWriteFailureNamesItsCauseWithoutInjection(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	if _, err := c.PutObject(cx, &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("a"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.PutObject(cx, &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("a/b"), Body: strings.NewReader("y"),
	})
	if err == nil {
		t.Fatal("writing a key under an existing object silently succeeded")
	}
	var api smithy.APIError
	if !errors.As(err, &api) {
		t.Fatalf("the SDK did not surface an API error: %v", err)
	}
	if api.ErrorCode() != "KeyPathConflict" {
		t.Errorf("error code %q, want KeyPathConflict", api.ErrorCode())
	}
	if !strings.Contains(api.ErrorMessage(), "prefix of this key already exists") {
		t.Errorf("the error does not explain the conflict: %s", api.ErrorMessage())
	}
}

// TestObjectsSurviveARestart — the appliance owns a data directory, so stopping it is a restart
// and not a reset. This is the same property the embedded Temporal's SQLite file has.
func TestObjectsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := Start(Options{DataDir: dir, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	c := s3client(t, first)
	if _, err := c.PutObject(ctx(t), &s3.PutObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("checkpoints/run-1.json"), Body: strings.NewReader(`{"n":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("checkpoints/run-1.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first.Stop()

	second, err := Start(Options{DataDir: dir, Port: port})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer second.Stop()
	c2 := s3client(t, second)
	out, err := c2.GetObject(ctx(t), &s3.GetObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String("checkpoints/run-1.json"),
	})
	if err != nil {
		t.Fatalf("object did not survive a restart: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(got) != `{"n":1}` {
		t.Fatalf("read back %q", got)
	}
	// The ETag has to survive too: DuckDB re-checks it across range reads to notice a file
	// changing under a query, and a tag that changed on restart would look exactly like that.
	if aws.ToString(out.ETag) != aws.ToString(before.ETag) {
		t.Errorf("ETag changed across a restart: %s → %s", aws.ToString(before.ETag), aws.ToString(out.ETag))
	}
}

// TestDeleteRemovesTheObjectAndItsEmptyPartitions — retention sweeps delete keys, and a tree left
// full of empty hive directories stops being a readable index of what is stored.
func TestDeleteRemovesTheObjectAndItsEmptyPartitions(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	key := "units/run=r1/dt=2026-08-26/actor=crawl/shard=0001/unit=00001/aa.json"
	if _, err := srv.store.put(DefaultS3Bucket, key, strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteObject(cx, &s3.DeleteObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srv.DataDir(), "objects", DefaultS3Bucket, "units")); !os.IsNotExist(err) {
		t.Errorf("empty partition directories survived the delete: %v", err)
	}
	// Deleting what is not there is not an error, matching S3 and matching what a retention sweep
	// re-run has to be able to do.
	if _, err := c.DeleteObject(cx, &s3.DeleteObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(key)}); err != nil {
		t.Errorf("deleting a missing key: %v", err)
	}
}

// TestMissingThingsAreMissing — the codes all three SDK stores treat as "not present"
// (NoSuchKey / NotFound / 404 / NoSuchBucket). A store that answers 500 here would make the
// codec's passthrough check and the CAS's store-if-absent both retry forever.
func TestMissingThingsAreMissing(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	_, err := c.GetObject(cx, &s3.GetObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String("nope/nothing")})
	var noKey *types.NoSuchKey
	if !errors.As(err, &noKey) {
		t.Errorf("GET of a missing key: %v (want NoSuchKey)", err)
	}
	_, err = c.ListObjectsV2(cx, &s3.ListObjectsV2Input{Bucket: aws.String("never-made")})
	var noBucket *types.NoSuchBucket
	if !errors.As(err, &noBucket) {
		t.Errorf("LIST of a missing bucket: %v (want NoSuchBucket)", err)
	}
	_, err = c.HeadObject(cx, &s3.HeadObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String("nope/nothing")})
	var api smithy.APIError
	if !errors.As(err, &api) || api.ErrorCode() != "NotFound" {
		t.Errorf("HEAD of a missing key: %v (want NotFound)", err)
	}
}

// TestCopyGivesACASBlobARunAddressedName — the orchestrator copies a content-addressed blob onto
// a `datasets/…` key so DuckDB can glob it by actor/version/run, because a content address
// carries no identity to glob by.
func TestCopyGivesACASBlobARunAddressedName(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)
	cx := ctx(t)

	src := "cas/ab/abcdef"
	dst := "datasets/actor=crawl/version=0.1.0/run_id=r1/part-0.parquet"
	if _, err := srv.store.put(DefaultS3Bucket, src, strings.NewReader("PAR1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CopyObject(cx, &s3.CopyObjectInput{
		Bucket: aws.String(DefaultS3Bucket), Key: aws.String(dst),
		CopySource: aws.String(DefaultS3Bucket + "/" + src),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetObject(cx, &s3.GetObjectInput{Bucket: aws.String(DefaultS3Bucket), Key: aws.String(dst)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(got) != "PAR1" {
		t.Fatalf("copy read back %q", got)
	}
}

// TestUnsupportedOperationsNameThemselves — a store that answers an operation it does not
// implement with 404 sends whoever hits it looking for a missing object instead.
func TestUnsupportedOperationsNameThemselves(t *testing.T) {
	srv := startTestS3(t)
	c := s3client(t, srv)

	_, err := c.PutBucketCors(ctx(t), &s3.PutBucketCorsInput{
		Bucket: aws.String(DefaultS3Bucket),
		CORSConfiguration: &types.CORSConfiguration{
			CORSRules: []types.CORSRule{{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"*"}}},
		},
	})
	var api smithy.APIError
	if !errors.As(err, &api) || api.ErrorCode() != "NotImplemented" {
		t.Fatalf("PutBucketCors: %v (want NotImplemented)", err)
	}
	if !strings.Contains(api.ErrorMessage(), "PUT") {
		t.Errorf("the refusal does not name the request: %s", api.ErrorMessage())
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
