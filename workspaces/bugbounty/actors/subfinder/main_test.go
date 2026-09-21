package main

import "testing"

// isSubdomainOf is the last line of defence before a host becomes a scan target. Passive
// sources (certificate transparency especially) routinely return hosts unrelated to the queried
// apex, and a naive strings.HasSuffix would accept "evil-example.com" for apex "example.com" —
// a domain nobody put in scope.
func TestIsSubdomainOfIsLabelAligned(t *testing.T) {
	in := []struct{ host, apex string }{
		{"example.com", "example.com"},
		{"api.example.com", "example.com"},
		{"a.b.c.example.com", "example.com"},
	}
	for _, c := range in {
		if !isSubdomainOf(c.host, c.apex) {
			t.Errorf("isSubdomainOf(%q,%q) = false, want true", c.host, c.apex)
		}
	}
	out := []struct{ host, apex string }{
		{"evil-example.com", "example.com"},   // the classic suffix-match escape
		{"notexample.com", "example.com"},
		{"example.com.evil.net", "example.com"}, // apex as a PREFIX of an attacker domain
		{"example.co", "example.com"},
		{"", "example.com"},
	}
	for _, c := range out {
		if isSubdomainOf(c.host, c.apex) {
			t.Errorf("isSubdomainOf(%q,%q) = true, want false — OUT OF SCOPE", c.host, c.apex)
		}
	}
}

// normalizeApex must accept the messy forms real scope lists contain, and must REFUSE the ones
// that cannot be expanded without guessing what the program meant.
func TestNormalizeApex(t *testing.T) {
	ok := map[string]string{
		"*.example.com":         "example.com",
		".example.com":          "example.com",
		"example.com":           "example.com",
		"EXAMPLE.COM":           "example.com",
		"  *.Example.com  ":     "example.com",
		"https://example.com/":  "example.com",
		"http://example.com:8443/a?b=1": "example.com",
	}
	for in, want := range ok {
		if got := normalizeApex(in); got != want {
			t.Errorf("normalizeApex(%q) = %q, want %q", in, got, want)
		}
	}
	// Globs that are NOT a leading "*." cannot be enumerated from an apex: subfinder takes a
	// domain, and picking one for `dev*.example.com` would mean scanning hosts the program did
	// not list. Refusing is the only safe answer.
	refuse := []string{
		"", "   ", "localhost", "*uat.example.com", "dev*.example.com",
		"dcfgateway*.example.com", "example.com/hz/mycd/*", "*",
	}
	for _, in := range refuse {
		if got := normalizeApex(in); got != "" {
			t.Errorf("normalizeApex(%q) = %q, want \"\" (unsafe to expand)", in, got)
		}
	}
}
