package version

import (
	"runtime"
	"runtime/debug"
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

// A binary built by `go install kontra.sh/kontra/cmd/kontra@<version>` has no VCS stamp; it reports the module's
// version, whose pseudo-version names the commit (DM4-31). A local build ("(devel)") and a VCS stamp keep "dev[+rev]".
func TestFromBuildInfo(t *testing.T) {
	const pseudo = "v0.0.0-20261029101500-1a2b3c4d5e6f"
	for _, tc := range []struct {
		main string
		set  []debug.BuildSetting
		want string
	}{
		{pseudo, nil, pseudo},
		{"v4.0.0", nil, "v4.0.0"},
		{"(devel)", nil, "dev"},
		{"", nil, "dev"},
		{"(devel)", []debug.BuildSetting{{Key: "vcs.revision", Value: "208519cd53b0ed41a59688adc29c00bab2ea2220"}}, "dev+208519cd53b0"},
		{pseudo, []debug.BuildSetting{{Key: "vcs.revision", Value: "208519cd53b0ed41a59688adc29c00bab2ea2220"},
			{Key: "vcs.modified", Value: "true"}}, "dev+208519cd53b0-dirty"},
	} {
		info := &debug.BuildInfo{Main: debug.Module{Path: "kontra.sh/kontra", Version: tc.main}, Settings: tc.set}
		if got := fromBuildInfo("dev", info); got != tc.want {
			t.Errorf("fromBuildInfo(main %q, %v) = %q, want %q", tc.main, tc.set, got, tc.want)
		}
	}
}
