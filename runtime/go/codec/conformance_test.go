package codec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

// The cross-language gate: this codec must pass the SAME shared/conformance/codec/fixtures.json the
// handler's Go codec, the Python codec and the TS codec pass.
//
// Four independent implementations is not an accident — actorkit and the handler are separate
// modules that never import each other, and the orchestrator is another language entirely. This
// corpus is the only thing holding them to one wire format, and it is load-bearing: a byte of
// disagreement drops data silently across a language boundary rather than erroring.
//
// Threshold is PER CASE here, not the 128 KiB default, so the boundary rules are testable with
// 16-byte payloads.

type memStore struct {
	mu sync.Mutex
	d  map[string][]byte
}

func newMemStore() *memStore { return &memStore{d: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.d[key] = append([]byte(nil), data...)
	return nil
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.d[key]
	if !ok {
		return nil, fmt.Errorf("claim-check object missing: %s", key)
	}
	return append([]byte(nil), v...), nil
}

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

func b64d(t *testing.T, s string) []byte {
	t.Helper()
	v, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return v
}

func TestCodecConformance(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/conformance/codec/fixtures.json")
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

			store := newMemStore()
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
				if len(store.d) != 0 {
					t.Errorf("expected nothing stored for %s, got %d objects", c.Expect.Action, len(store.d))
				}

			case "offload":
				if string(got.GetMetadata()[converter.MetadataEncoding]) != Marker {
					t.Fatalf("expected marker metadata, got %v", got.GetMetadata())
				}
				var ref map[string]any
				if err := json.Unmarshal(got.GetData(), &ref); err != nil {
					t.Fatalf("parse ref: %v", err)
				}
				if !reflect.DeepEqual(normalizeRef(ref), normalizeRef(c.Expect.Ref)) {
					t.Errorf("ref mismatch:\n got  %v\n want %v", normalizeRef(ref), normalizeRef(c.Expect.Ref))
				}
				if k := CasKey(ref["sha256"].(string)); k != c.Expect.CasKey {
					t.Errorf("cas key mismatch: got %s want %s", k, c.Expect.CasKey)
				}
				stored, err := store.Get(context.Background(), c.Expect.CasKey)
				if err != nil {
					t.Fatalf("expected object at %s: %v", c.Expect.CasKey, err)
				}
				if !reflect.DeepEqual(stored, b64d(t, c.Expect.ObjectB64)) {
					t.Errorf("stored object bytes mismatch")
				}
				// Round-trip: decode must reconstruct the data AND the original metadata
				// byte-for-byte — the metadata is what the SDK reads to pick a converter, so
				// losing it is how a rehydrated payload becomes undeserializable.
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
// store: encode a payload, then assert the object is in the backing under the key the corpus
// names, and that decode derives the same address back.
//
// This arm did not exist, and its absence is why this file was green while the code under it was
// wrong. Every `cases` row above runs with no prefix, and with no prefix `prefix + key` and
// `join(prefix, key)` produce identical bytes — the one input class in which the four
// implementations CANNOT differ (ADR 0035 finding 1). Under KONTRA_S3_PREFIX=slice11 this store
// wrote `slice11cas/df/df5b…` while the handler and the orchestrator read `slice11/cas/df/df5b…`.
func TestStorePrefixIsAPathSegment(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/conformance/codec/fixtures.json")
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

			backing := newMemStore()
			cdc := New(WithPrefix(backing, c.Prefix), c.Threshold)

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

			keys := make([]string, 0, len(backing.d))
			for k := range backing.d {
				keys = append(keys, k)
			}
			if len(keys) != 1 || keys[0] != c.Expect.Key {
				t.Fatalf("with prefix %q the object landed at %v, want [%q]", c.Prefix, keys, c.Expect.Key)
			}

			// The read side has to derive the same address, or the outage is the same one in
			// the other direction.
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

			// And the join itself, so a failure says whether the address or the plumbing moved.
			if got := objectKey(c.Prefix, CasKey(c.Expect.Sha256)); got != c.Expect.Key {
				t.Errorf("objectKey(%q, …) = %q, want %q", c.Prefix, got, c.Expect.Key)
			}
		})
	}
}

// normalizeRef coerces size to float64 so parsed-vs-expected comparison is number-type agnostic.
func normalizeRef(r map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		switch n := v.(type) {
		case int:
			out[k] = float64(n)
		case int64:
			out[k] = float64(n)
		default:
			out[k] = v
		}
	}
	return out
}

// TestPassthroughWithoutAStore pins the local-dev path: no KONTRA_S3_ENDPOINT means no store,
// which must be a no-op rather than an error. The handler behaves the same way under the same
// condition, so neither side offloads and neither has anything to fetch.
func TestPassthroughWithoutAStore(t *testing.T) {
	cdc := New(nil, 1) // threshold 1: everything would offload if a store existed
	in := []*commonpb.Payload{{
		Metadata: map[string][]byte{converter.MetadataEncoding: []byte("json/plain")},
		Data:     []byte(`{"units":["a","b"]}`),
	}}
	out, err := cdc.Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Error("passthrough encode must return the payloads unchanged")
	}
	back, err := cdc.Decode(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Error("passthrough decode must return the payloads unchanged")
	}
}

// TestDecodeRejectsATamperedObject: the digest is derived from the ref, never trusted from the
// store. A silent mismatch is the failure the whole corpus exists to prevent.
func TestDecodeRejectsATamperedObject(t *testing.T) {
	store := newMemStore()
	cdc := New(store, 4)

	big := []byte("this is comfortably over the four byte threshold")
	out, err := cdc.Encode([]*commonpb.Payload{{
		Metadata: map[string][]byte{converter.MetadataEncoding: []byte("json/plain")},
		Data:     big,
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Corrupt the stored object, leaving the ref (and therefore the expected digest) intact.
	for k := range store.d {
		store.d[k] = []byte("tampered")
	}
	if _, err := cdc.Decode(out); err == nil {
		t.Fatal("decode must fail when the stored bytes do not match the ref digest")
	}
}
