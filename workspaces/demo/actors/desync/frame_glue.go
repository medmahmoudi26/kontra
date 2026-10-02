package main

// Glue between the framing axis and the actor: technique selection, the cache-buster, and the
// map-shaping helpers that mirror observationUnit for the two new row types.

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/technique"
)

// expandCorpus renders every family ONCE per Session, in Load, for the same reason the vector
// sets are generated there: the CL-obfuscation family alone is 3,630 rows, and regenerating it
// per Unit would dominate the CPU of a scan whose cost is supposed to be network-bound.
func expandCorpus() (all, tier1 []technique.Row) {
	for _, f := range technique.Families {
		for _, r := range f.Expand() {
			all = append(all, r)
			if r.Tier == 1 {
				tier1 = append(tier1, r)
			}
		}
	}
	return all, tier1
}

// techniquesFor is the two-phase screen, at technique grain.
//
// PHASE screen sends only tier 1 — the rows that regenerate a request somebody was actually paid
// for. That is 5 rows today, so the screen is 5 probes per host and can be re-run over the whole
// scope on a schedule.
//
// PHASE sweep sends the rest of the class the host reacted to, and NOT the other classes. The
// unit of escalation is (host, class), not host: sweeping every class at a host that moved on one
// is how a 180x saving collapses to a 9x one.
func (r *resolved) techniquesFor(phase, class string) []technique.Row {
	if phase == "screen" {
		if class == "" {
			return r.tier1
		}
		return filterClass(r.tier1, class)
	}
	// sweep: everything in the class, minus the tier-1 rows already sent in the screen.
	var out []technique.Row
	for _, t := range r.techs {
		if t.Tier == 1 {
			continue
		}
		if class != "" && t.Class != class {
			continue
		}
		out = append(out, t)
	}
	if max := r.p.MaxTechniques; max > 0 && len(out) > max {
		// A capped enumeration is REPORTED by the caller, not silently truncated here — but the
		// cap itself lives here because the corpus is the actor's.
		out = out[:max]
	}
	return out
}

func filterClass(in []technique.Row, class string) []technique.Row {
	var out []technique.Row
	for _, t := range in {
		if t.Class == class {
			out = append(out, t)
		}
	}
	return out
}

// cacheBuster is the ${random} binding. Crypto/rand rather than math/rand because two Workers
// starting in the same second with a seeded PRNG would send the same "unique" value, and a CDN
// would serve the second one a cached answer to the first one's probe — which reads as a stable
// host that is anything but.
func (r *resolved) cacheBuster() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000"
	}
	return fmt.Sprintf("%d", binary.BigEndian.Uint32(b[:4])%1_000_000)
}

// rowUnit shapes the ONE row type both axes emit (detect/row.go).
//
// THE ELISION HAPPENS HERE, at the single choke point every row passes through on its way to the
// lake, rather than in the four `ds.Push` sites that call it. Putting it in `RowFromSmuggle`
// would be wrong for a different reason: that function's job is to LIFT an observation, and a
// lift that also destroys part of its input is one the tests cannot check the lift of. A row is
// complete until the moment it is serialised; `Elide` is what serialisation costs.
func rowUnit(r detect.Row, keepRaw bool) map[string]any {
	r.Elide(keepRaw)
	return asMap(r, detect.SchemaRow)
}

func corpusUnit(r technique.Row) map[string]any { return asMap(r, "technique/v1") }

func asMap(v any, schema string) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"schema": schema, "error": "marshal: " + err.Error()}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{"schema": schema, "error": "unmarshal: " + err.Error()}
	}
	m["schema"] = schema
	return m
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
