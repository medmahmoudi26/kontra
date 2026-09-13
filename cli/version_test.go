package main

// version_test.go — the release train's two silent failure modes.
//
// Both are the same shape: a value written in one file and consumed in another, where a mismatch
// produces NO error, just a release that quietly does the wrong thing. `release.yml` had already
// been sitting armed and unfired for its whole existence because nothing ever created a tag; these
// exist so the next way it can fail to fire is a red build.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// repoFile reads a path relative to the repository root and FAILS if it is empty.
//
// The emptiness check is the point. A guard that reads a file it cannot find, gets `""` and then
// asserts things about `""` reports success — this repo has been bitten by exactly that, by a
// fixture path one directory short of where the fixture was.
func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{".."}, rel...)...)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v — this test is worthless if it cannot find the file", p, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		t.Fatalf("%s is empty; nothing below is checking anything", p)
	}
	return string(raw)
}

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// version.txt and the release-please manifest are two files holding one number, and release-please
// writes both. If they disagree, the next release bumps from the wrong base.
func TestVersionFileMatchesManifest(t *testing.T) {
	version := strings.TrimSpace(repoFile(t, "version.txt"))
	if !semver.MatchString(version) {
		t.Fatalf("version.txt holds %q, which is not a semver release-please can bump", version)
	}
	// No leading `v`: release-please stores the bare version and adds the prefix to the TAG only.
	// A `v` here would produce the tag `vv0.1.0`, which `release.yml` still matches — so this would
	// ship, under a name nobody would search for.
	if strings.HasPrefix(version, "v") {
		t.Fatalf("version.txt holds %q; it must be bare (0.1.0), the `v` belongs to the tag alone", version)
	}

	var manifest map[string]string
	if err := json.Unmarshal([]byte(repoFile(t, ".release-please-manifest.json")), &manifest); err != nil {
		t.Fatalf("the release-please manifest is not valid JSON: %v", err)
	}
	got, ok := manifest["."]
	if !ok {
		t.Fatalf("the manifest has no entry for the root package; it has %v", manifestKeys(manifest))
	}
	if got != version {
		t.Fatalf("version.txt says %q and the release-please manifest says %q — "+
			"the next release would bump from the wrong base", version, got)
	}
}

// THE DRIFT THAT BUILDS NOTHING. `release.yml` fires on a tag glob; release-please decides the tag's
// shape from `include-component-in-tag`. They live in different files, and a mismatch means a tag is
// created, no workflow starts, and there is no error anywhere — the release simply does not happen.
func TestReleaseTriggerMatchesReleasePleaseTag(t *testing.T) {
	var cfg struct {
		Packages map[string]struct {
			IncludeComponentInTag *bool  `json:"include-component-in-tag"`
			PackageName           string `json:"package-name"`
		} `json:"packages"`
	}
	if err := json.Unmarshal([]byte(repoFile(t, "release-please-config.json")), &cfg); err != nil {
		t.Fatalf("release-please-config.json is not valid JSON: %v", err)
	}
	root, ok := cfg.Packages["."]
	if !ok {
		t.Fatal("release-please-config.json has no root package")
	}
	if root.IncludeComponentInTag == nil || *root.IncludeComponentInTag {
		t.Fatalf("include-component-in-tag must be explicitly false, or the tag becomes "+
			"%q-v0.1.0 and `release.yml`'s `v*` never matches it", root.PackageName)
	}

	version := strings.TrimSpace(repoFile(t, "version.txt"))
	tag := "v" + version // what release-please creates with include-component-in-tag: false

	for _, pattern := range releaseTagPatterns(t) {
		if ok, err := filepath.Match(pattern, tag); err == nil && ok {
			return // some trigger pattern matches the tag release-please will push
		}
	}
	t.Fatalf("no tag pattern in release.yml matches %q (patterns: %v) — "+
		"release-please would tag and nothing would build", tag, releaseTagPatterns(t))
}

// releaseTagPatterns pulls `on.push.tags` out of the release workflow.
//
// `on` IS PARSED AS THE BOOLEAN TRUE. YAML 1.1 treats `on`, `off`, `yes` and `no` as booleans, so a
// workflow's top-level `on:` key arrives as `true` rather than the string — and a lookup of "on"
// alone silently finds nothing, which here would mean "no patterns" and a test that passes by
// checking an empty list. Both spellings are tried, and an empty result is fatal.
func releaseTagPatterns(t *testing.T) []string {
	t.Helper()
	var doc map[interface{}]interface{}
	if err := yaml.Unmarshal([]byte(repoFile(t, ".github", "workflows", "release.yml")), &doc); err != nil {
		t.Fatalf("release.yml is not valid YAML: %v", err)
	}
	on, ok := doc["on"]
	if !ok {
		on, ok = doc[true]
	}
	if !ok {
		t.Fatal("release.yml has no `on:` trigger under either spelling")
	}
	onMap, ok := on.(map[interface{}]interface{})
	if !ok {
		t.Fatalf("release.yml's `on:` is %T, not a mapping", on)
	}
	push, ok := onMap["push"].(map[interface{}]interface{})
	if !ok {
		t.Fatal("release.yml does not trigger on push at all")
	}
	raw, ok := push["tags"].([]interface{})
	if !ok || len(raw) == 0 {
		t.Fatal("release.yml's push trigger names no tags; it would never fire on a release")
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func manifestKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
