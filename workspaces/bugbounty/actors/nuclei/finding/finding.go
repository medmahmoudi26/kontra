// Package finding is the pure, nuclei-free core of the nuclei actor: the output record plus the
// dedupe-identity and host-routing helpers. Kept apart from main so it unit-tests without the
// heavy nuclei dependency.
package finding

import (
	"net/url"
	"strings"
)

// Phases is the severity order a target is scanned in — one phase at a time, with the completed
// phase checkpointed into the Unit's scratch so a death mid-scan RESUMES at the next phase.
var Phases = []string{"critical", "high", "medium", "low"}

// Finding is one nuclei result — the actor's output unit.
type Finding struct {
	Target    string `json:"target"`
	Template  string `json:"template"`
	Name      string `json:"name"`
	Severity  string `json:"severity"`
	MatchedAt string `json:"matched_at"`
}

// ID is the stable dedupe identity (target + template + where it matched). It is the
// global_state SET member, so the whole fleet reports each finding exactly once.
func (f Finding) ID() string {
	return f.Target + "|" + f.Template + "|" + f.MatchedAt
}

// Unit renders the finding as a kontra output unit.
func (f Finding) Unit() map[string]any {
	return map[string]any{
		"target": f.Target, "template": f.Template, "name": f.Name,
		"severity": f.Severity, "matched_at": f.MatchedAt,
	}
}

// HostKey normalizes a target URL or a result's host to a comparison key (scheme, port and path
// stripped), so nuclei's ONE global callback can route each finding to the Unit that scanned its
// target even while several targets scan concurrently.
func HostKey(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return raw
}
