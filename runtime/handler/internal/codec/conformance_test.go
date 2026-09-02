package codec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/objectstore"
)

// The cross-language gate: the Go codec must pass the SAME shared/conformance/codec/fixtures.json
// the Python and TS codecs pass. Threshold is per-case, not the 128 KiB default.

type fixtures struct {
	PrefixCases []prefixCase `json:"prefixCases"`
	Cases       []struct {
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
}

// prefixCase is one `prefixCases` row: the same payload every time, only the store prefix
// varying, and the full object key it must land under.
type prefixCase struct {
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
}

// memBacking is an in-memory objectstore.Backing. objectstore.NewMem() exists but hard-codes an
// EMPTY prefix, which is precisely the case these rows are here to stop being the only one.
type memBacking struct{ m map[string][]byte }

func newMemBacking() *memBacking { return &memBacking{m: map[string][]byte{}} }

func (b *memBacking) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := b.m[key]
	return v, ok, nil
}
func (b *memBacking) Put(_ context.Context, key string, data []byte) error {
	b.m[key] = append([]byte(nil), data...)
	return nil
}
func (b *memBacking) Exists(_ context.Context, key string) (bool, error) {
	_, ok := b.m[key]
	return ok, nil
}
func (b *memBacking) Delete(_ context.Context, key string) error {
	delete(b.m, key)
	return nil
}

func b64d(t *testing.T, s string) []byte {
	t.Helper()
	v, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return v
}

func TestCodecConformance(t *testing.T) {
	raw, err := os.ReadFile("../../../../shared/conformance/codec/fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fx fixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no fixture cases")
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			data := b64d(t, c.Input.DataB64)
			meta := map[string][]byte{}
			for k, v := range c.Input.MetaB64 {
				meta[k] = b64d(t, v)
			}
			in := &commonpb.Payload{Metadata: meta, Data: data}

			store := objectstore.NewMem()
			cdc := New(store, c.Threshold)

			out, err := cdc.Encode([]*commonpb.Payload{in})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := out[0]

			switch c.Expect.Action {
			case "inline", "unchanged":
				if !reflect.DeepEqual(got.GetData(), data) {
					t.Errorf("expected data unchanged")
				}
				if !reflect.DeepEqual(got.GetMetadata(), meta) {
					t.Errorf("expected metadata unchanged")
				}
				// nothing offloaded
				if len(data) > 0 {
					if _, found, _ := store.Get(context.Background(), store.CasKey(cas.Sha256Hex(data))); found {
						t.Errorf("expected nothing stored for %s", c.Expect.Action)
					}
				}

			case "offload":
				if string(got.GetMetadata()[converter.MetadataEncoding]) != Marker {
					t.Fatalf("expected marker metadata, got %v", got.GetMetadata())
				}
				var ref map[string]any
				if err := json.Unmarshal(got.GetData(), &ref); err != nil {
					t.Fatalf("parse ref: %v", err)
				}
				// ref structure-equal (size compared numerically; JSON numbers are float64)
				wantRef := normalizeRef(c.Expect.Ref)
				if !reflect.DeepEqual(normalizeRef(ref), wantRef) {
					t.Errorf("ref mismatch:\n got  %v\n want %v", normalizeRef(ref), wantRef)
				}
				// cas key byte-equal
				if store.CasKey(ref["sha256"].(string)) != c.Expect.CasKey {
					t.Errorf("cas key mismatch: got %s want %s", store.CasKey(ref["sha256"].(string)), c.Expect.CasKey)
				}
				// stored object bytes byte-equal
				stored, found, _ := store.Get(context.Background(), c.Expect.CasKey)
				if !found {
					t.Fatalf("expected object at %s", c.Expect.CasKey)
				}
				if !reflect.DeepEqual(stored, b64d(t, c.Expect.ObjectB64)) {
					t.Errorf("stored object bytes mismatch")
				}
				// round-trip decode reconstructs byte-identical data + metadata
				back, err := cdc.Decode(out)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !reflect.DeepEqual(back[0].GetData(), data) {
					t.Errorf("round-trip data mismatch")
				}
				if !reflect.DeepEqual(back[0].GetMetadata(), meta) {
					t.Errorf("round-trip metadata mismatch:\n got  %v\n want %v", back[0].GetMetadata(), meta)
				}

			default:
				t.Fatalf("unknown action %q", c.Expect.Action)
			}
		})
	}
}

// TestStorePrefixIsAPathSegment drives the corpus's `prefixCases` rows through a REAL prefixed
// store: encode a payload, assert the object is in the backing at the key the corpus names, and
// decode it back from the same address.
//
// The handler already joined the prefix as a segment, so the plain-prefix rows pass here
// unchanged and it is the two SDK arms they turned red. What DID change for this side is the
// trailing-slash rows: `Key` used to push the prefix in un-stripped while stripping every other
// segment, so KONTRA_S3_PREFIX=`p/` addressed `p//cas/…` — a second namespace, reached by a
// spelling an operator reads as identical to `p`. See wireFormat.prefixTrailingSlash.
func TestStorePrefixIsAPathSegment(t *testing.T) {
	raw, err := os.ReadFile("../../../../shared/conformance/codec/fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fx fixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fx.PrefixCases) == 0 {
		t.Fatal("no prefixCases rows: the corpus no longer pins the prefix join")
	}

	for _, c := range fx.PrefixCases {
		t.Run(c.Name, func(t *testing.T) {
			data := b64d(t, c.Input.DataB64)
			meta := map[string][]byte{}
			for k, v := range c.Input.MetaB64 {
				meta[k] = b64d(t, v)
			}

			backing := newMemBacking()
			store := objectstore.New(backing, c.Prefix)
			cdc := New(store, c.Threshold)

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

			keys := make([]string, 0, len(backing.m))
			for k := range backing.m {
				keys = append(keys, k)
			}
			if len(keys) != 1 || keys[0] != c.Expect.Key {
				t.Fatalf("with prefix %q the object landed at %v, want [%q]", c.Prefix, keys, c.Expect.Key)
			}

			back, err := cdc.Decode(out)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(back[0].GetData(), data) {
				t.Error("round-trip data mismatch through the prefixed store")
			}
			if !reflect.DeepEqual(back[0].GetMetadata(), meta) {
				t.Error("round-trip metadata mismatch through the prefixed store")
			}

			// And the derivation on its own, so a failure names the address or the plumbing.
			if got := store.CasKey(c.Expect.Sha256); got != c.Expect.Key {
				t.Errorf("CasKey under prefix %q = %q, want %q", c.Prefix, got, c.Expect.Key)
			}
		})
	}
}

// normalizeRef coerces the size to float64 so the parsed-vs-expected comparison is
// number-type agnostic (json.Unmarshal yields float64 for both).
func normalizeRef(r map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		switch n := v.(type) {
		case int:
			out[k] = float64(n)
		default:
			out[k] = v
		}
	}
	return out
}
