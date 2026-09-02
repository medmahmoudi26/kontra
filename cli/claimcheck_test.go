// claimcheck_test.go — THE CLI'S ARM OF shared/conformance/codec/fixtures.json, and the arm that did
// not exist.
//
// The codec's own README calls that corpus "an executable gate" and lists the implementations
// that run it. This binary held a fifth one — its own marker, its own ref struct, its own base64
// metadata round-trip, its own `cas/<ab>/<sha>` — and its header cited the corpus by name while
// every test of it was written against values typed into the test. That is the shape ADR 0035 §3
// calls a finding: the fixture was available and the test compared against a memory of it.
//
// So the assertions below take EVERY byte they check out of the corpus: the marker, the ref, the
// address, the stored object and the metadata. The CLI's codec is the handler's now, so what these
// really gate is the one part that is still this binary's — the unsigned HTTP transport, and
// whether the URL it builds lands on the address the corpus records.
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"

	"github.com/medmahmoudi26/kontra/runtime/handler/claimcheck"
)

type codecCorpus struct {
	WireFormat struct {
		Marker        string `json:"marker"`
		CasKeyLayout  string `json:"casKeyLayout"`
		ThresholdRule string `json:"thresholdRule"`
	} `json:"wireFormat"`
	Cases []struct {
		Name      string `json:"name"`
		Note      string `json:"note"`
		Threshold int    `json:"threshold"`
		Input     struct {
			DataB64 string            `json:"data_b64"`
			MetaB64 map[string]string `json:"meta_b64"`
		} `json:"input"`
		Expect struct {
			Action string `json:"action"`
			Ref    *struct {
				Sha256 string            `json:"sha256"`
				Size   uint64            `json:"size"`
				Meta   map[string]string `json:"meta"`
			} `json:"ref"`
			CasKey    string `json:"cas_key"`
			ObjectB64 string `json:"object_b64"`
		} `json:"expect"`
	} `json:"cases"`
	// PrefixCases: the same payload under a set of KONTRA_S3_PREFIX spellings, and the full key
	// each must resolve to. See TestTheCLIFetchesUnderTheStorePrefix.
	PrefixCases []struct {
		Name   string `json:"name"`
		Why    string `json:"why"`
		Prefix string `json:"prefix"`
		Input  struct {
			DataB64 string            `json:"data_b64"`
			MetaB64 map[string]string `json:"meta_b64"`
		} `json:"input"`
		Expect struct {
			Sha256 string `json:"sha256"`
			Key    string `json:"key"`
		} `json:"expect"`
	} `json:"prefixCases"`
}

// ../shared/conformance/codec/fixtures.json — cli -> <repo root>.
func loadCodecCorpus(t *testing.T) *codecCorpus {
	t.Helper()
	raw, err := os.ReadFile("../shared/conformance/codec/fixtures.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc codecCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus that silently shrank to nothing passes every case in it — the queue corpus's
	// drivers make the same assertion for the same reason.
	if len(doc.Cases) < 7 {
		t.Fatalf("the corpus shrank to %d cases", len(doc.Cases))
	}
	if len(doc.PrefixCases) < 8 {
		t.Fatalf("the corpus carries %d prefixCases rows; a prefix join with fewer than the "+
			"eight spellings is the empty-prefix blind spot coming back", len(doc.PrefixCases))
	}
	blob := string(raw)
	for _, want := range []string{
		`"skip-already-marked"`,            // the double-encode guard a real divergence was caught by
		`"offload-payload-looks-like-ref"`, // the MARKER decides, not the shape of the data
		`"x-bin"`,                          // metadata values that are not utf8
		`"inline-empty"`,                   // zero bytes is not "no payload"
	} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer exercises %s", want)
		}
	}
	return &doc
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	v, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return v
}

// THE MARKER COMES FROM THE CORPUS, WHICH IS WHY THERE IS NO COPY OF IT IN THIS PACKAGE.
//
// `cli/workflow.go` used to carry `const claimCheckMarker = "binary/claim-check-v1"` with a comment
// calling it "VERBATIM the peer of codec.MARKER". A constant that must equal another constant is a
// contract with two writers, and ADR 0035 §3 says what that gets: a fixture both sides execute. The
// CLI's side is now an import, and the corpus is what holds the import to the wire.
func TestTheClaimCheckMarkerIsTheCorpusMarker(t *testing.T) {
	fx := loadCodecCorpus(t)
	if fx.WireFormat.Marker == "" {
		t.Fatal("the corpus no longer records the marker")
	}
	if claimcheck.Marker != fx.WireFormat.Marker {
		t.Fatalf("the CLI decodes on %q, the corpus says %q — a payload from any other "+
			"implementation would read as 'not a ref' and pass through unfetched",
			claimcheck.Marker, fx.WireFormat.Marker)
	}
}

// cliCodecAgainst points the REAL constructor at a stand-in store, through the REAL environment.
// Building `decodeOnlyCodec{claimcheck.New(...)}` by hand here would test the codec and skip the
// wiring — and the wiring is where this binary's remaining bugs live, because it is the half that
// is still the CLI's own.
func cliCodecAgainst(t *testing.T, serverURL, bucket, prefix string) converter.PayloadCodec {
	t.Helper()
	t.Setenv("KONTRA_S3_PUBLIC_ENDPOINT", "")
	t.Setenv("KONTRA_S3_ENDPOINT", serverURL)
	t.Setenv("KONTRA_S3_BUCKET", bucket)
	t.Setenv("KONTRA_S3_PREFIX", prefix)
	return cliClaimCheckCodec()
}

// TestTheCLIDecodesTheCodecCorpus is the gate. For every offloaded case the corpus records, the
// object is served at the address the corpus records, and the CLI must fetch THAT address and
// hand back the ORIGINAL bytes and the ORIGINAL metadata — including metadata values that are not
// valid utf8, which is the whole reason the base64 round-trip is in the contract.
func TestTheCLIDecodesTheCodecCorpus(t *testing.T) {
	fx := loadCodecCorpus(t)
	offloaded := 0
	for _, c := range fx.Cases {
		if c.Expect.Action != "offload" {
			continue
		}
		offloaded++
		t.Run(c.Name, func(t *testing.T) {
			var asked atomic.Value
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				asked.Store(r.URL.Path)
				if r.URL.Path != "/kontra/"+c.Expect.CasKey {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write(b64(t, c.Expect.ObjectB64))
			}))
			t.Cleanup(srv.Close)

			cdc := cliCodecAgainst(t, srv.URL, "kontra", "")

			// The ref exactly as the corpus records it. Whitespace is not part of the contract
			// (the object is addressed by the DATA's sha, not the ref's bytes), so this side
			// marshals its own — what must match is the FIELD SET and the values.
			ref, err := json.Marshal(c.Expect.Ref)
			if err != nil {
				t.Fatal(err)
			}
			out, err := cdc.Decode([]*commonpb.Payload{{
				Metadata: map[string][]byte{"encoding": []byte(fx.WireFormat.Marker)},
				Data:     ref,
			}})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			// THE ADDRESS. `cas/<sha[:2]>/<sha>` under the store prefix, path-style under the
			// bucket. This is the assertion the old suite could not make, because the old codec
			// built this string itself and the old test rebuilt it the same way.
			if got := asked.Load(); got != "/kontra/"+c.Expect.CasKey {
				t.Errorf("fetched %v, want the corpus address /kontra/%s", got, c.Expect.CasKey)
			}
			if !reflect.DeepEqual(out[0].GetData(), b64(t, c.Input.DataB64)) {
				t.Errorf("payload not rehydrated to the corpus's original bytes")
			}
			wantMeta := map[string][]byte{}
			for k, v := range c.Input.MetaB64 {
				wantMeta[k] = b64(t, v)
			}
			if !reflect.DeepEqual(out[0].GetMetadata(), wantMeta) {
				t.Errorf("metadata not restored:\n got  %v\n want %v", out[0].GetMetadata(), wantMeta)
			}
		})
	}
	if offloaded == 0 {
		t.Fatal("the corpus records no offloaded case — this gate asserted nothing")
	}
}

// The other half of the corpus: a payload that is NOT marked must be handed straight back, and
// nothing may be fetched for it. `offload-payload-looks-like-ref` is the sharp one — its data IS a
// `{sha256,size,meta}` object and its encoding is `json/plain`, so an implementation that sniffed
// the shape instead of reading the marker would try to fetch `deadbeef` and fail on a payload that
// was never offloaded.
func TestTheCLILeavesUnmarkedCorpusPayloadsAlone(t *testing.T) {
	fx := loadCodecCorpus(t)
	for _, c := range fx.Cases {
		meta := map[string][]byte{}
		for k, v := range c.Input.MetaB64 {
			meta[k] = b64(t, v)
		}
		if string(meta["encoding"]) == fx.WireFormat.Marker {
			continue // marked: that is the case above, and skip-already-marked below
		}
		t.Run(c.Name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(srv.Close)

			in := []*commonpb.Payload{{Metadata: meta, Data: b64(t, c.Input.DataB64)}}
			out, err := cliCodecAgainst(t, srv.URL, "kontra", "").Decode(in)
			if err != nil {
				t.Fatalf("an unmarked payload must not be fetched: %v", err)
			}
			if out[0] != in[0] {
				t.Error("an unmarked payload must pass through untouched")
			}
			if hits.Load() != 0 {
				t.Errorf("%d fetches for a payload that was never offloaded", hits.Load())
			}
		})
	}
}

// `skip-already-marked` carries the marker over data that is not a ref at all ("already a
// claim-check ref!!"). On the ENCODE side the corpus calls it `unchanged` — the double-encode
// guard. On the DECODE side it is a corrupt payload, and the CLI must REFUSE it rather than hand
// a human back the marker bytes as if they were a result.
func TestTheCLIRefusesAMarkedPayloadThatIsNotARef(t *testing.T) {
	fx := loadCodecCorpus(t)
	for _, c := range fx.Cases {
		if c.Name != "skip-already-marked" {
			continue
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		_, err := cliCodecAgainst(t, srv.URL, "kontra", "").Decode([]*commonpb.Payload{{
			Metadata: map[string][]byte{"encoding": []byte(fx.WireFormat.Marker)},
			Data:     b64(t, c.Input.DataB64),
		}})
		if err == nil {
			t.Fatal("a marked payload whose data is not a ref must be refused, not returned")
		}
		return
	}
	t.Fatal("the corpus no longer carries skip-already-marked")
}

// THE KEY LIVES UNDER THE PREFIX, WITH A SEPARATOR — and this is the bug the fifth codec had.
//
// It built `fmt.Sprintf("%s/%s/%scas/%s/%s", endpoint, bucket, prefix, sha[:2], sha)`: the prefix
// concatenated to `cas` with nothing between them. With KONTRA_S3_PREFIX=`runs` this CLI fetched
// `/kontra/runscas/83/8378…` where the key is `/kontra/runs/cas/83/8378…`. The divergence appears
// only under a non-default prefix, only on a payload over 128 KiB, and only at `--wait` time.
//
// WHICH SIDE IS RIGHT, measured rather than assumed: `runtime/handler/internal/objectstore.Key` and
// `control/orchestrator/src/codec/objectStore.ts:key` both drop the prefix in as a SEGMENT joined with `/`,
// while `runtime/python/internals/casstore.py` AND `runtime/go/codec/s3.go` both concatenated —
// three against two, not three against one. Both SDKs join now, and the prefix is no longer a
// value this test types: the rows come out of `prefixCases`, which is where a derivation with four
// independent writers belongs (ADR 0035 §3). This test used to hard-code `"runs"` and one expected
// key, which is the shape it was written to replace.
func TestTheCLIFetchesUnderTheStorePrefix(t *testing.T) {
	fx := loadCodecCorpus(t)

	for _, p := range fx.PrefixCases {
		t.Run(p.Name, func(t *testing.T) {
			data := b64(t, p.Input.DataB64)
			want := "/kontra/" + p.Expect.Key

			// The store answers at the corpus's address and 404s everywhere else, so a CLI that
			// asked for the wrong key fails on the fetch as well as on the assertion.
			var asked atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked.Store(r.URL.Path)
				if r.URL.Path != want {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write(data)
			}))
			t.Cleanup(srv.Close)

			// The ref the codec would have written for this row: the digest the corpus records,
			// the size of its payload, its metadata base64 per value.
			ref, err := json.Marshal(map[string]any{
				"sha256": p.Expect.Sha256,
				"size":   len(data),
				"meta":   p.Input.MetaB64,
			})
			if err != nil {
				t.Fatal(err)
			}

			out, err := cliCodecAgainst(t, srv.URL, "kontra", p.Prefix).Decode([]*commonpb.Payload{{
				Metadata: map[string][]byte{"encoding": []byte(fx.WireFormat.Marker)},
				Data:     ref,
			}})
			if err != nil {
				t.Fatalf("decode under KONTRA_S3_PREFIX=%q: %v (fetched %v, want %q)",
					p.Prefix, err, asked.Load(), want)
			}
			if got := asked.Load(); got != want {
				t.Errorf("with KONTRA_S3_PREFIX=%q the CLI fetched %v, want %q — the prefix is a "+
					"key SEGMENT (%s under it), not a string glued to `cas`",
					p.Prefix, got, want, fx.WireFormat.CasKeyLayout)
			}
			if !reflect.DeepEqual(out[0].GetData(), data) {
				t.Errorf("payload not rehydrated to the corpus's bytes under prefix %q", p.Prefix)
			}
		})
	}
}

// DECODE-ONLY, PINNED. This CLI is a caller: its argument is a flag on a command line, and a flag
// big enough to need offloading is a flag that wants to be a dataset. So Encode passes everything
// through no matter how large — and it must do so WITHOUT touching the store, because the store it
// is pointed at is reached unsigned and read-only.
func TestTheCLIsEncodeNeverOffloads(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	big := make([]byte, claimcheck.DefaultThreshold+1)
	in := []*commonpb.Payload{{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     big,
	}}
	out, err := cliCodecAgainst(t, srv.URL, "kontra", "").Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if out[0] != in[0] {
		t.Error("the CLI must not offload: a payload it encodes is one an operator typed")
	}
	if hits.Load() != 0 {
		t.Errorf("%d store calls from an encode — the CLI's backing is read-only", hits.Load())
	}
}

// VERIFY RATHER THAN TRUST. The digest is derived from the ref and re-checked against the bytes
// that came back, and this is the handler's check now rather than a second one written here: the
// CLI is the one reader a human reads directly, so tampered bytes must be a refusal and not a
// result.
func TestTheCLIRefusesTamperedBytes(t *testing.T) {
	fx := loadCodecCorpus(t)
	c := fx.Cases[0]
	for _, x := range fx.Cases {
		if x.Expect.Action == "offload" {
			c = x
			break
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	t.Cleanup(srv.Close)

	ref, err := json.Marshal(c.Expect.Ref)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cliCodecAgainst(t, srv.URL, "kontra", "").Decode([]*commonpb.Payload{{
		Metadata: map[string][]byte{"encoding": []byte(fx.WireFormat.Marker)},
		Data:     ref,
	}})
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("want the shared integrity refusal, got %v", err)
	}
}

// A REF WITH AN UNUSABLE DIGEST IS A REFUSAL, NOT A CRASH.
//
// The CAS address is the digest sliced into a path (`cas/<sha[:2]>/<sha>`), and the ref comes off
// Temporal history rather than from this process — so a short or non-hex sha256 reaches a `[:2]`
// on a string that may not have two characters. Measured during the fold: a ref carrying
// `"sha256":"a"` panicked with `slice bounds out of range [:2] with length 1`, where the CLI's
// own implementation had answered "claim-check ref has no usable sha256". The guard went into
// runtime/handler/internal/codec, where every Go arm gets it; this pins that the CLI kept the behaviour
// it had.
func TestAnUnusableRefIsRefusedRatherThanCrashing(t *testing.T) {
	fx := loadCodecCorpus(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	cdc := cliCodecAgainst(t, srv.URL, "kontra", "")

	for _, bad := range []string{`"a"`, `""`, `"NOTHEX` + strings.Repeat("x", 58) + `"`} {
		_, err := cdc.Decode([]*commonpb.Payload{{
			Metadata: map[string][]byte{"encoding": []byte(fx.WireFormat.Marker)},
			Data:     []byte(`{"sha256":` + bad + `,"size":1,"meta":{}}`),
		}})
		if err == nil {
			t.Errorf("a ref with sha256 %s must be refused", bad)
		}
	}
}

// A fetch that fails has to name the URL and, when the store refused for a reason an operator can
// act on, say which. "claim-check object missing" against a store nobody can reach sends them to
// look at the bucket instead of at the endpoint they pointed this at.
func TestAFailedFetchNamesTheURLAndTheReason(t *testing.T) {
	fx := loadCodecCorpus(t)
	c := fx.Cases[0]
	for _, x := range fx.Cases {
		if x.Expect.Action == "offload" {
			c = x
			break
		}
	}
	ref, err := json.Marshal(c.Expect.Ref)
	if err != nil {
		t.Fatal(err)
	}

	for status, want := range map[int]string{
		http.StatusForbidden: "credentials",
		http.StatusNotFound:  "KONTRA_S3_BUCKET",
	} {
		code := status
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		_, err := cliCodecAgainst(t, srv.URL, "kontra", "").Decode([]*commonpb.Payload{{
			Metadata: map[string][]byte{"encoding": []byte(fx.WireFormat.Marker)},
			Data:     ref,
		}})
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d must be an error", code)
		}
		if !strings.Contains(err.Error(), c.Expect.CasKey) {
			t.Errorf("HTTP %d: the error does not name the address (%v)", code, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("HTTP %d: the error does not say %q (%v)", code, want, err)
		}
	}
}
