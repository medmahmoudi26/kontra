package finding

import "testing"

func TestHostKeyNormalizes(t *testing.T) {
	cases := map[string]string{
		"https://example.com/a/b": "example.com",
		"http://example.com:8080": "example.com",
		"example.com":             "example.com",
		"HTTPS://Example.COM/x":   "example.com",
	}
	for in, want := range cases {
		if got := HostKey(in); got != want {
			t.Errorf("HostKey(%q) = %q, want %q", in, got, want)
		}
	}
	// a target and a result's host for the same site MUST route to the same key, or the sink
	// would never hand a finding back to the Unit that scanned it.
	if HostKey("https://sub.example.com/scan") != HostKey("sub.example.com") {
		t.Error("target and result host must share a routing key")
	}
}

func TestFindingIDDedupes(t *testing.T) {
	f := Finding{Target: "https://x", Template: "cve-2021-1", MatchedAt: "https://x/a"}
	if f.ID() != "https://x|cve-2021-1|https://x/a" {
		t.Errorf("ID = %q", f.ID())
	}
	// same finding -> same id (deduped); different matched-at -> different id (both reported).
	g := Finding{Target: "https://x", Template: "cve-2021-1", MatchedAt: "https://x/b"}
	if f.ID() == g.ID() {
		t.Error("distinct matched-at must yield distinct ids")
	}
}

func TestUnitCarriesFields(t *testing.T) {
	u := Finding{Target: "t", Template: "tpl", Name: "n", Severity: "high", MatchedAt: "m"}.Unit()
	if u["severity"] != "high" || u["template"] != "tpl" || u["matched_at"] != "m" || u["target"] != "t" {
		t.Errorf("Unit = %v", u)
	}
}
