package unitstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

type fakePutter struct{ blobs map[string][]byte }

func (f *fakePutter) Put(_ context.Context, key string, data []byte) error {
	if f.blobs == nil {
		f.blobs = map[string][]byte{}
	}
	f.blobs[key] = append([]byte(nil), data...)
	return nil
}

func TestPutSubunitKeyBodyAndRef(t *testing.T) {
	fp := &fakePutter{}
	s := New(fp, "")
	record := map[string]any{"url": "https://x/a", "sev": "high"}
	ref, err := s.PutSubunit(context.Background(), "", "run1", "node1", 3, record)
	if err != nil {
		t.Fatal(err)
	}

	wantBody, _ := json.Marshal([]map[string]any{record}) // body is [record]
	sum := sha256.Sum256(wantBody)
	sha := hex.EncodeToString(sum[:])
	// derive from BlobKey rather than hardcoding the layout: the format lives in ONE place, so
	// a future change breaks the dedicated blobkey tests rather than silently drifting here
	wantKey := BlobKey("", "", "run1", "node1", 3, sha)

	if got, ok := fp.blobs[wantKey]; !ok || string(got) != string(wantBody) {
		t.Fatalf("blob at %q = %q (present=%v), want %q", wantKey, got, ok, wantBody)
	}
	r := ref["$ref"].(map[string]any)
	if r["key"] != wantKey || r["sha256"] != sha || r["size"] != len(wantBody) {
		t.Fatalf("ref = %v; want key=%q sha=%q size=%d", r, wantKey, sha, len(wantBody))
	}
}

func TestPutSubunitDefaultsAndPrefix(t *testing.T) {
	fp := &fakePutter{}
	s := New(fp, "p/") // KONTRA_S3_PREFIX
	if _, err := s.PutSubunit(context.Background(), "", "", "", 0, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	for k := range fp.blobs {
		// empty run/node/actor must still yield fully-formed partitions — a blob written to a
		// malformed path is invisible to the reader
		for _, seg := range []string{"p/units/run=run/", "/dt=", "/actor=unknown/", "/shard=node/", "/unit=00000/"} {
			if !strings.Contains(k, seg) {
				t.Fatalf("key = %q, missing %q", k, seg)
			}
		}
	}
}

func TestPutSubunitIdempotentBySha(t *testing.T) {
	fp := &fakePutter{}
	s := New(fp, "")
	rec := map[string]any{"x": 1, "y": 2}
	r1, _ := s.PutSubunit(context.Background(), "", "r", "n", 5, rec)
	r2, _ := s.PutSubunit(context.Background(), "", "r", "n", 5, rec)
	// same record -> same content sha -> same key -> idempotent overwrite (one blob)
	if r1["$ref"].(map[string]any)["key"] != r2["$ref"].(map[string]any)["key"] {
		t.Fatal("same record must produce the same key")
	}
	if len(fp.blobs) != 1 {
		t.Fatalf("expected 1 blob after re-emitting the same record, got %d", len(fp.blobs))
	}
}
