package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MarkerForms finds a marker wherever it sits in a byte string that was base64 or base64url encoded (each
// alignment, with and without padding), JSON-escaped or hex-encoded, so ScanMarker's absence means something; and
// a string without the marker matches no form.
func TestMarkerForms(t *testing.T) {
	m := NewMarker()
	if !strings.ContainsAny(m, "<>") {
		t.Fatalf("the marker %q has no character JSON escapes", m)
	}
	forms := MarkerForms(m)
	found := func(data []byte) bool {
		for _, f := range forms {
			if bytes.Contains(data, []byte(f)) {
				return true
			}
		}
		return false
	}
	for before := 0; before < 7; before++ {
		for after := 0; after < 4; after++ {
			pre, post := make([]byte, before), make([]byte, after)
			_, _ = rand.Read(pre)
			_, _ = rand.Read(post)
			data := append(append(append([]byte(nil), pre...), m...), post...)
			for name, enc := range map[string]func([]byte) string{
				"base64":      base64.StdEncoding.EncodeToString,
				"base64-raw":  base64.RawStdEncoding.EncodeToString,
				"base64url":   base64.URLEncoding.EncodeToString,
				"base64url-r": base64.RawURLEncoding.EncodeToString,
				"hex":         hex.EncodeToString,
			} {
				if !found([]byte(enc(data))) {
					t.Errorf("%s of the marker after %d bytes and before %d: not found", name, before, after)
				}
			}
		}
	}
	j, _ := json.Marshal(map[string]string{"x": "a" + m + "b"})
	if !found(j) || !bytes.Contains(j, []byte("\\u003c")) {
		t.Errorf("JSON-escaped: %s", j)
	}
	other := NewMarker()
	if found([]byte(other)) || found([]byte(base64.StdEncoding.EncodeToString([]byte(other)))) {
		t.Errorf("another marker matches %q's forms", m)
	}
}

// The preload the harness starts npm servers with is examples/mcp/everything's (D185): the two never drift apart.
func TestLoopbackPreloadIsTheExamples(t *testing.T) {
	a, err := os.ReadFile(filepath.Join("testdata", "loopback.cjs"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, "examples", "mcp", "everything", "loopback.cjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("test/e2e/testdata/loopback.cjs differs from examples/mcp/everything/loopback.cjs")
	}
}
