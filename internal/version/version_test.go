package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "v4.0.0-m1"
	if got, want := String("kontra-gate"), "kontra-gate v4.0.0-m1 ("+runtime.Version()+", "+runtime.GOOS+"/"+runtime.GOARCH+")"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	Version = "dev"
	if got := Get(); !strings.HasPrefix(got, "dev") {
		t.Errorf("Get = %q, want dev[+rev]", got)
	}
}

func TestWithRevision(t *testing.T) {
	for _, tc := range []struct {
		rev   string
		dirty bool
		want  string
	}{
		{"", false, "dev"},
		{"208519cd53b0ed41a59688adc29c00bab2ea2220", false, "dev+208519cd53b0"},
		{"208519cd53b0ed41a59688adc29c00bab2ea2220", true, "dev+208519cd53b0-dirty"},
		{"abc", false, "dev+abc"},
	} {
		if got := withRevision("dev", tc.rev, tc.dirty); got != tc.want {
			t.Errorf("withRevision(%q, %v) = %q, want %q", tc.rev, tc.dirty, got, tc.want)
		}
	}
}
