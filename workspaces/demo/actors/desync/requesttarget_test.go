package main

import "testing"

// The endpoint is substituted as `${endpoint}?cb=${random}`, so anything it carries past the path
// lands INSIDE the cachebuster's parameter — which stops it busting a cache and stops the canary
// being where the oracle looks. Root is the floor, never an empty request-target.
func TestRequestTargetIsAlwaysASafeAbsolutePath(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v1/session": "/api/v1/session",
		"/search?q=1&x=2": "/search",
		"/page#frag":      "/page",
		"/a?b#c":          "/a",
		"":                "/",
		"/":               "/",
		"api/v1":          "/", // relative: not a request-target
		"https://x/y":     "/", // absolute-form is not what these templates render
		"?cb=1":           "/",
	} {
		if got := requestTarget(in); got != want {
			t.Errorf("requestTarget(%q) = %q, want %q", in, got, want)
		}
	}
}
