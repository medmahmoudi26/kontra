package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/blob/blobtest"
)

func TestNewRef(t *testing.T) {
	seen := map[blob.Ref]bool{}
	for range 200 {
		r, err := blob.NewRef()
		if err != nil {
			t.Fatal(err)
		}
		if !r.Valid() || len(r) != 26 || strings.ToLower(string(r)) != string(r) {
			t.Fatalf("ref %q: want 26 lowercase base32 characters", r)
		}
		if seen[r] {
			t.Fatalf("ref %q minted twice", r)
		}
		seen[r] = true
		if got, ok := blob.RefOf(r.URL()); !ok || got != r {
			t.Fatalf("RefOf(%q) = %q, %v", r.URL(), got, ok)
		}
	}
}

func TestRefOfAndReserved(t *testing.T) {
	const ref = "abcdefghijklmnopqrstuvwxyz"
	for _, tc := range []struct {
		url           string
		ref, reserved bool
	}{
		{"kontra-blob:" + ref, true, true},
		{"kontra-blob:" + ref + "a", false, true},
		{"kontra-blob:ABCDEFGHIJKLMNOPQRSTUVWXYZ", false, true},
		{"KONTRA-BLOB:" + ref, false, true},
		{"Kontra-Blob:x", false, true},
		{"  kontra-blob:" + ref, false, true},
		{"kontra-blob:", false, true},
		{"https://example.com/kontra-blob:" + ref, false, false},
		{"kontra-blo", false, false},
		{"", false, false},
	} {
		if _, ok := blob.RefOf(tc.url); ok != tc.ref {
			t.Errorf("RefOf(%q) = %v, want %v", tc.url, ok, tc.ref)
		}
		if got := blob.Reserved(tc.url); got != tc.reserved {
			t.Errorf("Reserved(%q) = %v, want %v", tc.url, got, tc.reserved)
		}
	}
}

func TestRefsIn(t *testing.T) {
	a, b := "abcdefghijklmnopqrstuvwxyz", "a234567abcdefghijklmnopqrs"
	payload := []byte(`{"parts":[{"url":"kontra-blob:` + a + `"},{"url":"kontra-blob:` + b + `"},{"url":"kontra-blob:` + a +
		`"},{"text":"kontra-blob:short"},{"url":"kontra-blob:` + a + `zz"}]}`)
	got := blob.RefsIn(payload)
	if len(got) != 2 || got[0] != blob.Ref(a) || got[1] != blob.Ref(b) {
		t.Fatalf("RefsIn = %q, want [%s %s]", got, a, b)
	}
	if blob.RefsIn([]byte(`{"text":"hello"}`)) != nil {
		t.Fatal("refs in a payload without any")
	}
}

func TestParseURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want blob.Config
	}{
		{"s3://parts", blob.Config{Bucket: "parts"}},
		{"s3://parts/", blob.Config{Bucket: "parts"}},
		{"s3://parts/team/a", blob.Config{Bucket: "parts", Prefix: "team/a"}},
		{"s3://kontra-parts/x?endpoint=https://s3.fr-par.scw.cloud&region=fr-par",
			blob.Config{Bucket: "kontra-parts", Prefix: "x", Endpoint: "https://s3.fr-par.scw.cloud", Region: "fr-par"}},
		{"s3://parts?endpoint=http://127.0.0.1:9000/", blob.Config{Bucket: "parts", Endpoint: "http://127.0.0.1:9000"}},
	} {
		got, err := blob.ParseURL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"https://parts", "s3://", "s3://Parts", "s3://p", "s3://AKIA:secret@parts", "s3://parts#x",
		"s3://parts/a/../b", "s3://parts/a//b", "s3://parts/a b", "s3://parts?endpoint=ftp://x",
		"s3://parts?endpoint=https://u:p@x", "s3://parts?endpoint=https://x/path", "s3://parts?endpoint=https://x?y=1",
		"s3://parts?region=EU WEST", "s3://parts?bucket=x", "s3://parts?region=a&region=b",
	} {
		if _, err := blob.ParseURL(in); err == nil {
			t.Errorf("ParseURL(%q) succeeded", in)
		}
	}
	// The error never quotes credentials.
	if _, err := blob.ParseURL("s3://AKIAEXAMPLE:s3cr3t@parts"); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("ParseURL with credentials: %v", err)
	}
}

func TestObjectName(t *testing.T) {
	tenant, task := uuid.MustParse("0192f0b4-0000-7000-8000-000000000001"), uuid.MustParse("0192f0b4-0000-7000-8000-000000000002")
	k := blob.Key{Tenant: tenant, Task: task, Ref: "abcdefghijklmnopqrstuvwxyz"}
	if got := blob.ObjectName("", k); got != tenant.String()+"/"+task.String()+"/abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("ObjectName = %q", got)
	}
	if got := blob.ObjectName("team/a", k); got != "team/a/"+tenant.String()+"/"+task.String()+"/abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("ObjectName with a prefix = %q", got)
	}
}

func TestOpenNeedsCredentialsAndTheBucket(t *testing.T) {
	s := blobtest.New(t, "")
	t.Setenv(blob.EnvSecretKey, "")
	if _, err := blob.Open(context.Background(), s.Config); err == nil || !strings.Contains(err.Error(), blob.EnvSecretKey) {
		t.Errorf("Open without a secret key = %v", err)
	}
	t.Setenv(blob.EnvSecretKey, "kontra-test-secret")
	cfg := s.Config
	cfg.Bucket = "no-such-bucket"
	if _, err := blob.Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Open of a missing bucket = %v", err)
	}
	// The write check left nothing behind.
	if keys := s.Keys(t); len(keys) != 0 {
		t.Errorf("objects after Open: %v", keys)
	}
}

// The bucket over the S3 API: Put stores exactly the bytes under <prefix>/<tenant>/<task>/<ref>, Open reads them
// back with their size, a presigned GET fetches the same bytes over plain HTTP, and DeleteTask removes the
// task's objects only.
func TestStoreRoundTrip(t *testing.T) {
	s := blobtest.New(t, "team")
	ctx := context.Background()
	tenant := uuid.Must(uuid.NewV7())
	task, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	data := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16) // 300 KiB
	var keys []blob.Key
	for _, tk := range []uuid.UUID{task, task, other} {
		ref, err := blob.NewRef()
		if err != nil {
			t.Fatal(err)
		}
		k := blob.Key{Tenant: tenant, Task: tk, Ref: ref}
		if err := s.Store.Put(ctx, k, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if got := s.Keys(t); len(got) != 3 || !strings.HasPrefix(got[0], "team/"+tenant.String()+"/") {
		t.Fatalf("keys = %v", got)
	}
	rc, n, err := s.Store.Open(ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if n != int64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("Open read %d bytes (size %d), want the %d stored", len(got), n, len(data))
	}
	u, err := s.Store.Presign(ctx, keys[0], blob.PresignTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, s.Endpoint+"/"+blobtest.Bucket+"/team/") || !strings.Contains(u, "X-Amz-Expires=900") || !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("presigned URL %s", u)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	fetched, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(fetched, data) {
		t.Fatalf("presigned GET: %d, %d bytes", resp.StatusCode, len(fetched))
	}
	// A short reader is an error, not a short object.
	ref, _ := blob.NewRef()
	if err := s.Store.Put(ctx, blob.Key{Tenant: tenant, Task: task, Ref: ref}, bytes.NewReader(data[:10]), 20); err == nil {
		t.Error("Put of fewer bytes than announced succeeded")
	}
	if err := s.Store.Put(ctx, blob.Key{Tenant: tenant, Task: task, Ref: "BAD"}, bytes.NewReader(data), int64(len(data))); err == nil {
		t.Error("Put under a malformed ref succeeded")
	}
	// DeleteTask removes the task's objects and leaves the other task's.
	if err := s.Store.DeleteTask(ctx, tenant, task); err != nil {
		t.Fatal(err)
	}
	left := s.Keys(t)
	if len(left) != 1 || !strings.Contains(left[0], other.String()) {
		t.Fatalf("after DeleteTask: %v", left)
	}
	if _, _, err := s.Store.Open(ctx, keys[0]); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Open of a deleted object = %v, want ErrNotFound", err)
	}
	if err := s.Store.DeleteTask(ctx, tenant, task); err != nil {
		t.Errorf("DeleteTask of a task without objects = %v", err)
	}
}

// Externalize stores a part's content and returns the url part that replaces it, keeping the part's filename,
// media type and metadata (a sealed part keeps its envelope), and the row.
func TestExternalize(t *testing.T) {
	s := blobtest.New(t, "")
	ctx := context.Background()
	tenant, task := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	raw := bytes.Repeat([]byte{7}, 300<<10)
	sealed := &a2a.Part{Content: a2a.Raw(raw), MediaType: "application/vnd.kontra.sealed+age", Filename: "x.bin",
		Metadata: map[string]any{"kontra.sealed": map[string]any{"original": "image/png"}}}
	for _, tc := range []struct {
		in       *a2a.Part
		bytes    []byte
		wantType string
	}{
		{sealed, raw, "application/vnd.kontra.sealed+age"},
		{a2a.NewTextPart(strings.Repeat("é", 10)), []byte(strings.Repeat("é", 10)), "text/plain; charset=utf-8"},
		{a2a.NewDataPart(map[string]any{"k": "v"}), []byte(`{"k":"v"}`), "application/json"},
	} {
		out, row, err := blob.Externalize(ctx, s.Store, tenant, task, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		u, ok := out.Content.(a2a.URL)
		if !ok || string(u) != row.Ref.URL() || out.MediaType != tc.wantType || out.Filename != tc.in.Filename {
			t.Fatalf("Externalize = %+v", out)
		}
		if tc.in.Metadata != nil && out.Metadata["kontra.sealed"] == nil {
			t.Errorf("the sealed part lost its metadata: %+v", out.Metadata)
		}
		if row.Task != task || row.Tenant != tenant || row.Size != int64(len(tc.bytes)) {
			t.Errorf("row = %+v", row)
		}
		rc, _, err := s.Store.Open(ctx, row.Key())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, tc.bytes) {
			t.Errorf("object holds %d bytes, want %d", len(got), len(tc.bytes))
		}
		if _, isRaw := tc.in.Content.(a2a.Raw); isRaw && blob.PartSize(tc.in) != int64(len(raw)) {
			t.Errorf("PartSize = %d", blob.PartSize(tc.in))
		}
	}
	if _, _, err := blob.Externalize(ctx, nil, tenant, task, sealed); err == nil {
		t.Error("Externalize without a bucket succeeded")
	}
}

// Pending rows are those a payload names, of its task only.
func TestPending(t *testing.T) {
	task, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	p := blob.NewPending()
	a, _ := blob.NewRef()
	b, _ := blob.NewRef()
	c, _ := blob.NewRef()
	p.Add(blob.Row{Ref: a, Task: task, Size: 1})
	p.Add(blob.Row{Ref: b, Task: task, Size: 2})
	p.Add(blob.Row{Ref: c, Task: other, Size: 3})
	ctx := blob.WithPending(context.Background(), p)
	got := blob.PendingRows(ctx, task, []byte(`{"url":"`+a.URL()+`"}`), []byte(`{"url":"`+c.URL()+`","x":"`+a.URL()+`"}`))
	if len(got) != 1 || got[0].Ref != a {
		t.Fatalf("PendingRows = %+v, want only %s", got, a)
	}
	if blob.PendingRows(context.Background(), task, []byte(a.URL())) != nil {
		t.Fatal("rows pending on a context without a set")
	}
}
