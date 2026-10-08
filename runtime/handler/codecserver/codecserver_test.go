package codecserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

// memBacking is the store the install provides for real: a map here, a directory there. It is
// declared in the test rather than exported because the point of Backing is that a caller in
// another module can satisfy it, and a test that used an implementation from this module would
// not be evidence of that.
type memBacking struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMem() *memBacking { return &memBacking{m: map[string][]byte{}} }

func (b *memBacking) Get(_ context.Context, key string) ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.m[key]
	return v, ok, nil
}

func (b *memBacking) Put(_ context.Context, key string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[key] = bytes.Clone(data)
	return nil
}

func (b *memBacking) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[key]
	return ok, nil
}

func (b *memBacking) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, key)
	return nil
}

func (b *memBacking) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.m))
	for k := range b.m {
		out = append(out, k)
	}
	return out
}

// serve starts the handler on a test listener and returns Temporal's own client for the
// remote-codec contract, which is what the Web UI speaks.
func serve(t *testing.T, opts Options) (converter.PayloadCodec, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(New(opts))
	t.Cleanup(srv.Close)
	return converter.NewRemotePayloadCodec(converter.RemotePayloadCodecOptions{Endpoint: srv.URL}), srv
}

// TestServesTheClaimCheckOverHTTP is the contract in one test: over the threshold offloads to the
// store and comes back byte-identical; under it stays inline and touches no store. The corpus
// that pins the bytes across three languages runs in ../internal/codec and, for the install's
// own listener, in cli/install/codec/codec_test.go.
func TestServesTheClaimCheckOverHTTP(t *testing.T) {
	store := newMem()
	cdc, _ := serve(t, Options{Store: store, Threshold: 32})
	meta := map[string][]byte{"encoding": []byte("json/plain")}

	small := []byte(strings.Repeat("s", 32))
	out, err := cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: small}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(out[0].GetData(), small) {
		t.Errorf("a payload AT the threshold was rewritten")
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("a payload at the threshold wrote %v", keys)
	}

	big := []byte(strings.Repeat("b", 33))
	out, err = cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: big}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc := string(out[0].GetMetadata()["encoding"]); enc != "binary/claim-check-v1" {
		t.Fatalf("a payload over the threshold stayed inline (encoding %q)", enc)
	}
	var ref struct {
		Sha256 string `json:"sha256"`
		Size   int    `json:"size"`
	}
	if err := json.Unmarshal(out[0].GetData(), &ref); err != nil {
		t.Fatalf("the ref is not a ref: %v", err)
	}
	if ref.Size != len(big) {
		t.Errorf("ref size %d, want %d", ref.Size, len(big))
	}
	if keys := store.keys(); len(keys) != 1 || keys[0] != "cas/"+ref.Sha256[:2]+"/"+ref.Sha256 {
		t.Errorf("stored under %v, want the cas/<sha[:2]>/<sha> layout for %s", keys, ref.Sha256)
	}

	back, err := cdc.Decode(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(back[0].GetData(), big) {
		t.Errorf("round trip changed the payload")
	}
	if string(back[0].GetMetadata()["encoding"]) != "json/plain" {
		t.Errorf("round trip lost the original metadata: %v", back[0].GetMetadata())
	}
}

// TestPrefixIsTheWritersPrefix: a ref carries a digest, not a location, so the prefix the writers
// used is the one thing a reader has to be told. Getting it wrong reads as "claim-check object
// missing" for an object that is right there.
func TestPrefixIsTheWritersPrefix(t *testing.T) {
	store := newMem()
	cdc, _ := serve(t, Options{Store: store, Prefix: "kontra-dev", Threshold: 4})

	out, err := cdc.Encode([]*commonpb.Payload{{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     []byte("offload me"),
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	keys := store.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "kontra-dev/cas/") {
		t.Fatalf("stored under %v, want kontra-dev/cas/…", keys)
	}
	if _, err := cdc.Decode(out); err != nil {
		t.Fatalf("decode from the prefixed store: %v", err)
	}
}

// TestPassthroughWithoutAStore documents the state that is legal and never wanted: no store means
// a decode hands the `$ref` straight back, which renders in the UI exactly as it did before the
// codec existed. cli/install refuses to start one this way; the SDK's own default is why the
// case is here at all.
func TestPassthroughWithoutAStore(t *testing.T) {
	cdc, _ := serve(t, Options{Threshold: 4})
	in := []*commonpb.Payload{{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     []byte("well over the threshold"),
	}}
	out, err := cdc.Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(out[0].GetData(), in[0].GetData()) {
		t.Errorf("passthrough rewrote the payload")
	}
}

// TestCORSPreflight keeps the only reason this was ever more than the SDK's handler. The UI calls
// /encode and /decode FROM THE BROWSER; the native handler sets no CORS headers and 404s every
// non-POST, including the preflight, and the browser's refusal shows up as undecoded payloads —
// indistinguishable from the codec not running.
func TestCORSPreflight(t *testing.T) {
	_, srv := serve(t, Options{Store: newMem(), Origin: "http://localhost:8233"})

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/decode", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:8233")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: HTTP %d, want 204", resp.StatusCode)
	}
	// Echoed exactly, never "*": Allow-Credentials: true forbids the wildcard, and the UI may
	// send credentials.
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:8233" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q", got)
	}
	// X-Namespace is on the list because Temporal's UI sends it; dropping it from
	// Allow-Headers fails the preflight for that request only, which is a subtle way to break.
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Namespace") {
		t.Errorf("Allow-Headers = %q", got)
	}
}

// TestDefaultsAreTheContainers: the container's defaults were an env-read threshold and
// http://localhost:8233 as the origin. Neither moved.
func TestDefaultsAreTheContainers(t *testing.T) {
	if DefaultThreshold != 128*1024 {
		t.Errorf("DefaultThreshold = %d, want 128 KiB", DefaultThreshold)
	}
	if DefaultUIOrigin != "http://localhost:8233" {
		t.Errorf("DefaultUIOrigin = %q", DefaultUIOrigin)
	}
	if got := ThresholdFromEnv(); got != DefaultThreshold {
		t.Errorf("ThresholdFromEnv() = %d with KONTRA_S3_THRESHOLD unset, want %d", got, DefaultThreshold)
	}
	t.Setenv("KONTRA_S3_THRESHOLD", "4096")
	if got := ThresholdFromEnv(); got != 4096 {
		t.Errorf("ThresholdFromEnv() = %d, want 4096", got)
	}
	store := newMem()
	cdc, _ := serve(t, Options{Store: store}) // Threshold 0 => the env read, as the container did
	if _, err := cdc.Encode([]*commonpb.Payload{{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     bytes.Repeat([]byte("x"), 4097),
	}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if keys := store.keys(); len(keys) != 1 {
		t.Errorf("KONTRA_S3_THRESHOLD was not applied: stored %v", keys)
	}
}

// TestNonPOSTIs404 is the SDK handler's own behaviour, kept: the endpoint is two POST paths and
// nothing else, so a GET to it is not a health check and must not look like one.
func TestNonPOSTIs404(t *testing.T) {
	_, srv := serve(t, Options{Store: newMem()})
	for _, path := range []string{"/", "/encode", "/decode", "/healthz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: HTTP %d, want 404", path, resp.StatusCode)
		}
	}
}
