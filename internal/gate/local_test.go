package gate

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The profile's start warning names what it does not protect and gives the console's URL.
func TestLocalBanner(t *testing.T) {
	var logs bytes.Buffer
	LocalBanner(slog.New(slog.NewTextHandler(&logs, nil)), "http://127.0.0.1:8080")
	out := logs.String()
	for _, want := range []string{"level=WARN", "without credentials", "every conversation", "console=http://127.0.0.1:8080/"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner %q lacks %q", out, want)
		}
	}
}
