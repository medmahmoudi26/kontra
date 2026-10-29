package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
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

// The preload binds 127.0.0.1 for every TCP listen Node would bind on every interface, whatever its form (a backlog,
// no port, a null or empty host, an options object), and leaves a named host, a path and a handle alone (D185).
// testdata/loopback_check.cjs records what the preload passes to the real listen, which it never calls.
func TestLoopbackPreloadBindsLoopback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	out, err := exec.Command(node, filepath.Join("testdata", "loopback_check.cjs"), filepath.Join("testdata", "loopback.cjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("the preload leaves a listen on every interface (%v):\n%s", err, out)
	}
	t.Logf("%s", bytes.TrimSpace(out))
}
