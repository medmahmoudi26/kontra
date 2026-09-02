package codec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"

	"github.com/medmahmoudi26/kontra-local/cli/appliance/objstore"
	"github.com/medmahmoudi26/kontra-local/handler/codecserver"
)

// startTestS3 brings up the object store this codec reads, on its own temp directory and a free
// port. A COPY of objstore's own helper, and a short one, because a test helper is not part of a
// package's surface: the seam this suite exercises is objstore.Server.Backing, not whatever
// objstore's tests happen to share with each other.
func startTestS3(t *testing.T) *objstore.Server {
	t.Helper()
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := objstore.Start(objstore.Options{
		DataDir: t.TempDir(),
		Port:    port,
		Logf:    func(format string, args ...any) { t.Logf("store: "+format, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

// claims is the store as the codec takes it: one bucket, addressed by key.
func claims(t *testing.T, store *objstore.Server) *objstore.Backing {
	t.Helper()
	b, err := store.Backing(objstore.DefaultS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// startTestCodec brings a codec up on a free port over the given store. Never the default port:
// the compose `codec-server` service published 18234 and a developer box may still have it.
func startTestCodec(t *testing.T, store *objstore.Server, opts ...func(*Options)) *Server {
	t.Helper()
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Store: claims(t, store),
		Port:  port,
		Logf:  func(format string, args ...any) { t.Logf("codec: "+format, args...) },
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

// remoteCodec is Temporal's own client for the remote-codec contract, pointed at the appliance's
// endpoint. The tests go through it rather than through the in-process handler because the thing
// under test is what a BROWSER (or `temporal --codec-endpoint`) gets: the wire format, the paths
// and the address are all part of the answer.
func remoteCodec(t *testing.T, srv *Server) converter.PayloadCodec {
	t.Helper()
	return converter.NewRemotePayloadCodec(converter.RemotePayloadCodecOptions{Endpoint: srv.Endpoint()})
}

// codecFixtures is conformance/codec/fixtures.json — the SAME corpus the Python and TypeScript
// codecs are held to, and the Go codec's own suite in handler/internal/codec. Kept in this shape
// (and not shared) because that file is the contract; a struct that could not read it verbatim
// would be a second contract.
type codecFixtures struct {
	WireFormat struct {
		Marker string `json:"marker"`
	} `json:"wireFormat"`
	Cases []struct {
		Name      string `json:"name"`
		Threshold int    `json:"threshold"`
		Input     struct {
			DataB64 string            `json:"data_b64"`
			MetaB64 map[string]string `json:"meta_b64"`
		} `json:"input"`
		Expect struct {
			Action    string         `json:"action"`
			Ref       map[string]any `json:"ref"`
			CasKey    string         `json:"cas_key"`
			ObjectB64 string         `json:"object_b64"`
		} `json:"expect"`
	} `json:"cases"`
	// PrefixCases pins where an object lands under a non-empty KONTRA_S3_PREFIX. Same payload
	// every row; only the prefix varies.
	PrefixCases []struct {
		Name      string `json:"name"`
		Prefix    string `json:"prefix"`
		Threshold int    `json:"threshold"`
		Input     struct {
			DataB64 string            `json:"data_b64"`
			MetaB64 map[string]string `json:"meta_b64"`
		} `json:"input"`
		Expect struct {
			Sha256 string `json:"sha256"`
			Key    string `json:"key"`
		} `json:"expect"`
	} `json:"prefixCases"`
}

// loadCodecFixtures reads the corpus. One reader, so the marker below and the cases in
// TestCodecConformanceThroughTheAppliance cannot end up looking at different files.
func loadCodecFixtures(t *testing.T) codecFixtures {
	t.Helper()
	raw, err := os.ReadFile("../../../conformance/codec/fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fx codecFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no fixture cases")
	}
	return fx
}

// claimCheckMarker is the marker THE CORPUS RECORDS, read rather than restated.
//
// This was `const claimCheckMarker = "binary/claim-check-v1"`, here and again in
// appliance/temporalui_test.go, each under a header arguing that restating it was deliberate: "so
// a drift in the moved code shows up as a failing test and not as a test that agrees with the
// bug". That argument is right about IMPORTING and wrong about the alternative. The value is
// defined for all of the implementations in conformance/codec/fixtures.json, so reading it keeps
// every bit of the drift-detection — this test still fails if the code moves — while removing a
// copy that had to be kept in step by hand (ADR 0035 §3).
func claimCheckMarker(t *testing.T) string {
	t.Helper()
	m := loadCodecFixtures(t).WireFormat.Marker
	if m == "" {
		t.Fatal("the corpus no longer records wireFormat.marker")
	}
	return m
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	v, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return v
}

// TestCodecConformanceThroughTheAppliance is the acceptance test for this file.
//
// It is the same fixture, unchanged, that pinned the codec when it was a container: every case
// encoded through the appliance's HTTP endpoint must produce the ref the corpus names, store the
// object under the key the corpus names, and decode back to the byte-identical payload. The
// corpus is what makes "byte-identical to the container's behaviour" a claim a test can settle
// rather than an assertion in a commit message — the Python and TypeScript codecs are held to it
// too, and a drift here is a payload one language writes and another silently cannot read.
//
// It also proves the OBJECT reaches the appliance's own store, on disk, at the same key: the
// codec no longer fetches over the S3 API, so "same bytes, same names" is the property that
// replaced the round trip.
func TestCodecConformanceThroughTheAppliance(t *testing.T) {
	fx := loadCodecFixtures(t)
	marker := fx.WireFormat.Marker

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			data := mustB64(t, c.Input.DataB64)
			meta := map[string][]byte{}
			for k, v := range c.Input.MetaB64 {
				meta[k] = mustB64(t, v)
			}

			// A store and a codec per case, because the threshold is per case and a shared store
			// would let one case's object satisfy another's assertion that nothing was written.
			store := startTestS3(t)
			threshold := c.Threshold
			srv := startTestCodec(t, store, func(o *Options) { o.Threshold = threshold })
			cdc := remoteCodec(t, srv)

			out, err := cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: data}})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := out[0]

			switch c.Expect.Action {
			case "inline", "unchanged":
				if !bytes.Equal(got.GetData(), data) {
					t.Errorf("data changed: got %q want %q", got.GetData(), data)
				}
				if !reflect.DeepEqual(got.GetMetadata(), meta) {
					t.Errorf("metadata changed: got %v want %v", got.GetMetadata(), meta)
				}
				if keys := storeKeys(t, store); len(keys) != 0 {
					t.Errorf("expected nothing stored for %s, found %v", c.Expect.Action, keys)
				}

			case "offload":
				if enc := string(got.GetMetadata()["encoding"]); enc != marker {
					t.Fatalf("expected the claim-check marker, got encoding %q", enc)
				}
				var ref map[string]any
				if err := json.Unmarshal(got.GetData(), &ref); err != nil {
					t.Fatalf("parse ref: %v", err)
				}
				if !reflect.DeepEqual(ref, c.Expect.Ref) {
					t.Errorf("ref mismatch:\n got  %v\n want %v", ref, c.Expect.Ref)
				}
				// The object is on the appliance's own disk, under the corpus's key.
				stored, found, err := claims(t, store).Get(context.Background(), c.Expect.CasKey)
				if err != nil {
					t.Fatalf("read %s: %v", c.Expect.CasKey, err)
				}
				if !found {
					t.Fatalf("expected an object at %s, found %v", c.Expect.CasKey, storeKeys(t, store))
				}
				if !bytes.Equal(stored, mustB64(t, c.Expect.ObjectB64)) {
					t.Errorf("stored object bytes mismatch at %s", c.Expect.CasKey)
				}

				back, err := cdc.Decode(out)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !bytes.Equal(back[0].GetData(), data) {
					t.Errorf("round-trip data mismatch")
				}
				if !reflect.DeepEqual(back[0].GetMetadata(), meta) {
					t.Errorf("round-trip metadata mismatch:\n got  %v\n want %v", back[0].GetMetadata(), meta)
				}

			default:
				t.Fatalf("unknown fixture action %q", c.Expect.Action)
			}
		})
	}
}

// TestCodecPrefixIsAPathSegment drives the corpus's `prefixCases` rows all the way through: an
// encode over the REAL HTTP endpoint, under a real KONTRA_S3_PREFIX, landing on the appliance's
// own disk at the key the corpus names, and decoding back from there.
//
// It is the strongest arm of these rows and it is here for a reason none of the others can
// cover: `Options.Prefix` is the value `kontra up` reads out of the environment and hands down,
// so this is the only place the operator-facing variable and the on-disk address are checked
// against each other in one test. Before these rows existed the corpus exercised only the empty
// prefix — the one input class where concatenating and segment-joining produce identical keys —
// and two of the four key derivations behind these six arms concatenated (ADR 0035 finding 1).
func TestCodecPrefixIsAPathSegment(t *testing.T) {
	fx := loadCodecFixtures(t)
	if len(fx.PrefixCases) == 0 {
		t.Fatal("no prefixCases rows: the corpus no longer pins the prefix join")
	}

	for _, c := range fx.PrefixCases {
		t.Run(c.Name, func(t *testing.T) {
			data := mustB64(t, c.Input.DataB64)
			meta := map[string][]byte{}
			for k, v := range c.Input.MetaB64 {
				meta[k] = mustB64(t, v)
			}

			store := startTestS3(t)
			prefix, threshold := c.Prefix, c.Threshold
			srv := startTestCodec(t, store, func(o *Options) {
				o.Prefix = prefix
				o.Threshold = threshold
			})
			cdc := remoteCodec(t, srv)

			out, err := cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: data}})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var ref map[string]any
			if err := json.Unmarshal(out[0].GetData(), &ref); err != nil {
				t.Fatalf("parse ref: %v", err)
			}
			if ref["sha256"] != c.Expect.Sha256 {
				t.Fatalf("digest = %v, want %s", ref["sha256"], c.Expect.Sha256)
			}

			// On disk, at the corpus's address, and at no other. A ref carries a digest and
			// not a location, so this key is the whole of what a reader has to reconstruct.
			if keys := storeKeys(t, store); len(keys) != 1 || keys[0] != c.Expect.Key {
				t.Fatalf("with KONTRA_S3_PREFIX=%q the object landed at %v, want [%q]",
					c.Prefix, keys, c.Expect.Key)
			}
			stored, found, err := claims(t, store).Get(context.Background(), c.Expect.Key)
			if err != nil || !found {
				t.Fatalf("read %s: found=%v err=%v", c.Expect.Key, found, err)
			}
			if !bytes.Equal(stored, data) {
				t.Errorf("stored bytes mismatch at %s", c.Expect.Key)
			}

			back, err := cdc.Decode(out)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(back[0].GetData(), data) {
				t.Errorf("round-trip data mismatch under prefix %q", c.Prefix)
			}
			if !reflect.DeepEqual(back[0].GetMetadata(), meta) {
				t.Errorf("round-trip metadata mismatch under prefix %q", c.Prefix)
			}
		})
	}
}

// TestCodecThresholdIsTheContainersThreshold pins the two sides of the 128 KiB line at the real
// default, which the fixture corpus deliberately does not exercise (its thresholds are 8 and 16
// bytes, so the cases stay readable). One byte over offloads; exactly at the line stays inline.
// The rule is strictly-greater, and it is the one an actor's Batch crosses in production.
func TestCodecThresholdIsTheContainersThreshold(t *testing.T) {
	store := startTestS3(t)
	srv := startTestCodec(t, store)
	cdc := remoteCodec(t, srv)

	if codecserver.DefaultThreshold != 128*1024 {
		t.Fatalf("the claim-check threshold moved: %d", codecserver.DefaultThreshold)
	}
	meta := map[string][]byte{"encoding": []byte("json/plain")}

	atLine := bytes.Repeat([]byte("k"), codecserver.DefaultThreshold)
	out, err := cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: atLine}})
	if err != nil {
		t.Fatalf("encode at the threshold: %v", err)
	}
	if enc := string(out[0].GetMetadata()["encoding"]); enc != "json/plain" {
		t.Errorf("a payload AT the threshold was offloaded (encoding %q)", enc)
	}
	if !bytes.Equal(out[0].GetData(), atLine) {
		t.Errorf("a payload at the threshold came back changed")
	}
	if keys := storeKeys(t, store); len(keys) != 0 {
		t.Errorf("a payload at the threshold wrote objects: %v", keys)
	}

	over := append(bytes.Repeat([]byte("k"), codecserver.DefaultThreshold), 'k')
	out, err = cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: over}})
	if err != nil {
		t.Fatalf("encode over the threshold: %v", err)
	}
	if enc := string(out[0].GetMetadata()["encoding"]); enc != claimCheckMarker(t) {
		t.Fatalf("a payload over the threshold stayed inline (encoding %q)", enc)
	}
	keys := storeKeys(t, store)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "cas/") {
		t.Fatalf("expected one cas/ object, got %v", keys)
	}
	// The ref is small — that is the entire point of the claim check.
	if len(out[0].GetData()) > 512 {
		t.Errorf("the ref is %d bytes; it stands in for %d", len(out[0].GetData()), len(over))
	}

	back, err := cdc.Decode(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(back[0].GetData(), over) {
		t.Errorf("round trip changed %d bytes", len(over))
	}
	if !reflect.DeepEqual(back[0].GetMetadata(), meta) {
		t.Errorf("round trip lost the original metadata: %v", back[0].GetMetadata())
	}
}

// TestCodecEndpointIsDerivedFromTheListener is the other half of this slice: the address the
// Temporal UI is told to call is READ OFF the listener, not configured a second time. The
// container needed three values that had to agree (a listen port, a published port, and
// `--ui-codec-endpoint`); this asserts there is one, and that what Endpoint() returns is the
// server that actually answers.
func TestCodecEndpointIsDerivedFromTheListener(t *testing.T) {
	store := startTestS3(t)
	srv := startTestCodec(t, store)

	if want := "http://" + srv.Address(); srv.Endpoint() != want {
		t.Errorf("Endpoint() = %q, want %q", srv.Endpoint(), want)
	}
	if strings.HasSuffix(srv.Address(), ":0") || strings.Contains(srv.Address(), "0.0.0.0") {
		t.Errorf("Endpoint() %q is not an address a browser can call", srv.Endpoint())
	}
	// Reachable, at that exact URL, on the contract's own path.
	resp, err := http.Post(srv.Endpoint()+"/decode", "application/json", strings.NewReader(`{"payloads":[]}`))
	if err != nil {
		t.Fatalf("POST %s/decode: %v", srv.Endpoint(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s/decode: HTTP %d %s", srv.Endpoint(), resp.StatusCode, body)
	}
}

// TestCodecCORSAdmitsTheTemporalUI keeps the reason the container had custom code at all. The UI
// calls /encode and /decode FROM THE BROWSER, and the SDK's native handler sets no CORS headers
// and 404s the preflight — with the browser's refusal showing up as the UI rendering undecoded
// payloads, which looks exactly like no codec running at all.
func TestCodecCORSAdmitsTheTemporalUI(t *testing.T) {
	store := startTestS3(t)

	t.Run("default origin is Temporal's UI", func(t *testing.T) {
		srv := startTestCodec(t, store)
		if srv.UIOrigin() != codecserver.DefaultUIOrigin {
			t.Errorf("UIOrigin() = %q, want %q", srv.UIOrigin(), codecserver.DefaultUIOrigin)
		}
		h := preflight(t, srv)
		if got := h.Get("Access-Control-Allow-Origin"); got != codecserver.DefaultUIOrigin {
			t.Errorf("Allow-Origin = %q, want %q", got, codecserver.DefaultUIOrigin)
		}
	})

	t.Run("the UI slice's origin is echoed", func(t *testing.T) {
		const origin = "http://127.0.0.1:9233"
		srv := startTestCodec(t, store, func(o *Options) { o.UIOrigin = origin })
		h := preflight(t, srv)
		if got := h.Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("Allow-Origin = %q, want %q", got, origin)
		}
		// Never "*": the UI may send credentials, and the browser rejects the wildcard with
		// Allow-Credentials on.
		if h.Get("Access-Control-Allow-Credentials") != "true" {
			t.Errorf("Allow-Credentials = %q", h.Get("Access-Control-Allow-Credentials"))
		}
		if !strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") {
			t.Errorf("Allow-Methods = %q", h.Get("Access-Control-Allow-Methods"))
		}
		if h.Get("Vary") != "Origin" {
			t.Errorf("Vary = %q", h.Get("Vary"))
		}
	})
}

// preflight sends the OPTIONS the browser sends before a cross-origin POST and returns the
// headers. A 404 here is the failure mode: the SDK handler answers every non-POST that way.
func preflight(t *testing.T, srv *Server) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, srv.Endpoint()+"/decode", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", srv.UIOrigin())
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: HTTP %d (the browser will block the POST)", resp.StatusCode)
	}
	return resp.Header
}

// TestCodecNeedsTheObjectStore refuses the passthrough state the container could fall into. With
// KONTRA_S3_ENDPOINT unset it ran, logged a WARN, and answered every decode with the `$ref` the
// UI could already not read — a codec that is up and useless. In one process the store is right
// there, so not having one is a programming error and says so.
func TestCodecNeedsTheObjectStore(t *testing.T) {
	if _, err := Start(Options{}); err == nil {
		t.Fatal("expected Start to refuse a nil store")
	} else if !strings.Contains(err.Error(), "object store") {
		t.Errorf("the refusal does not name the store: %v", err)
	}
}

// storeKeys lists every object in the store's default bucket, so a test can assert that nothing
// was offloaded as well as that something was.
//
// READ OFF THE DISK, not out of the store's internals, which this package can no longer see. The
// layout it walks — <data>/objects/<bucket>/<key> — is the same one objstore's own tests assert
// against by path, so this is not a second claim about where an object lives.
func storeKeys(t *testing.T, srv *objstore.Server) []string {
	t.Helper()
	root := filepath.Join(srv.DataDir(), "objects", objstore.DefaultS3Bucket)
	var keys []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return keys
}
