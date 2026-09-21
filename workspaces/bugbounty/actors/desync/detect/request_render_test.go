package detect

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The two renderings have OPPOSITE jobs and this pins both, because getting either wrong is
// silent: a readable field that looks resendable produces PoCs that do not reproduce, and a
// base64 field that lost a byte produces a finding nobody can confirm.
func TestRequestRenderings(t *testing.T) {
	// A CL.0 shape: a proper header block, then a BARE LF where a CRLF belongs, then an obs-fold
	// continuation. Every one of those bytes is a thing a clipboard normalises away.
	raw := []byte("POST /x HTTP/1.1\r\nHost: t\r\nContent-Length:\n 5\r\n\tX-Fold: 1\r\n\r\nhello")

	t.Run("base64 is byte-exact", func(t *testing.T) {
		got, err := base64.StdEncoding.DecodeString(b64(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(got) != string(raw) {
			t.Fatalf("round trip lost bytes:\n want %q\n got  %q", raw, got)
		}
	})

	t.Run("readable form keeps CRLF but escapes the bare LF", func(t *testing.T) {
		s := renderRequest(raw)
		// The real line endings survive as line endings — the header block keeps its shape.
		if !strings.Contains(s, "POST /x HTTP/1.1\r\nHost: t\r\n") {
			t.Errorf("real CRLF should stay a line break: %q", s)
		}
		// The bare LF — the bug — is escaped, NOT normalised into the CRLFs around it.
		if !strings.Contains(s, `Content-Length:\n 5`) {
			t.Errorf("bare LF must be visible as \\n: %q", s)
		}
		// The obs-fold tab likewise.
		if !strings.Contains(s, `\tX-Fold: 1`) {
			t.Errorf("tab must be visible as \\t: %q", s)
		}
	})

	t.Run("readable form is NOT resendable, which is why b64 exists", func(t *testing.T) {
		// If this ever becomes equal, the readable field has started claiming to be the wire form
		// and somebody will paste it into a replay tab.
		if renderRequest(raw) == string(raw) {
			t.Fatal("renderRequest must not be byte-identical to the wire form")
		}
	})

	t.Run("empty in, empty out", func(t *testing.T) {
		if b64(nil) != "" || renderRequest(nil) != "" {
			t.Fatal("a sample that never sent must produce no request columns")
		}
	})
}
