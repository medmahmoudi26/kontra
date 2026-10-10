// actorresources.go — what an Actor says it needs to run: `resources` in actor.json, with `needs`
// accepted as its alias.
//
// TWO SPELLINGS, ONE FIELD. ADR 0040 named it `needs` and the shipped examples use that; the PRD
// (§6) writes `resources: {memory, cpus}`, which is what a Kubernetes Deployment calls the same
// thing and what D3's Deployment builder reads. Neither spelling was read by anything until now, so
// the cheapest correct move is to accept both and normalise both to one shape, rather than to
// rewrite every manifest in this repo and in kontra-actors in the same change.
//
// THE NORMALISED SHAPE IS `{cpus, memory}` AND ITS UNITS ARE KUBERNETES'. The examples were written
// in Docker's units, where `256m` is 256 MiB; in Kubernetes a lowercase `m` is MILLI, so the same
// string handed on verbatim is a 256-millibyte limit that the API server accepts and no container
// can start under. `shared/conformance/resources.json` is the unit table and the refusals, and
// `runtime/python/internals/manifest.py` reads the same file — neither reader is allowed an opinion
// the corpus does not state.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// actorResources is the normalised block. A zero field is UNSTATED, never a value: zero CPUs and an
// empty memory are both refused on the way in, so a consumer filling a default can tell the two apart.
type actorResources struct {
	CPUs   float64 `json:"cpus,omitempty"`
	Memory string  `json:"memory,omitempty"`
}

// memoryQuantity is the whole grammar: digits, an optional fraction, an optional suffix from one of
// the two tables below. No sign, no exponent, no whitespace — see the corpus for why each is out.
var memoryQuantity = regexp.MustCompile(`^([0-9]+)(\.[0-9]+)?([A-Za-z]*)$`)

// memorySuffix maps what an author may write to what Kubernetes reads. The Docker rows are the ones
// that change; the Kubernetes rows are listed so that "not in this table" is the whole refusal rule.
var memorySuffix = map[string]string{
	// Docker's binary suffixes, lowercase — the spelling `actor.json` was first written in.
	"b": "", "k": "Ki", "m": "Mi", "g": "Gi", "t": "Ti",
	// Kubernetes' own, untouched.
	"": "", "Ki": "Ki", "Mi": "Mi", "Gi": "Gi", "Ti": "Ti", "Pi": "Pi", "Ei": "Ei",
	"M": "M", "G": "G", "T": "T", "P": "P", "E": "E",
}

// resourcesOf reads the block from a decoded manifest. `field` is the spelling the author used, so
// every refusal names the key that is actually in their file.
func resourcesOf(resources, needs json.RawMessage) (*actorResources, error) {
	stated := func(raw json.RawMessage) bool {
		s := strings.TrimSpace(string(raw))
		return s != "" && s != "null"
	}
	field, raw := "resources", resources
	switch {
	case stated(resources) && stated(needs):
		return nil, fmt.Errorf("both \"resources\" and \"needs\" are stated, and they are one field " +
			"under two names. Keep \"resources\" and delete \"needs\"")
	case stated(needs):
		field, raw = "needs", needs
	case !stated(resources):
		return nil, nil
	}

	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil || block == nil {
		return nil, fmt.Errorf("%q must be an object like {\"cpus\": 1, \"memory\": \"1Gi\"}, got %s",
			field, strings.TrimSpace(string(raw)))
	}
	var unknown []string
	for k := range block {
		if k != "cpus" && k != "memory" {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%q has unknown key(s) %s: it takes \"cpus\" and \"memory\" and "+
			"nothing else, and a key that is ignored is an actor run on the default with nothing said",
			field, strings.Join(unknown, ", "))
	}

	out := &actorResources{}
	if v, ok := block["cpus"]; ok {
		var n any
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, err
		}
		cpus, isNum := n.(float64)
		if !isNum || cpus <= 0 {
			return nil, fmt.Errorf("%q.cpus must be a number above zero (1, or 0.5), got %s",
				field, strings.TrimSpace(string(v)))
		}
		out.CPUs = cpus
	}
	if v, ok := block["memory"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("%q.memory must be a string like \"1Gi\" or \"512m\", got %s",
				field, strings.TrimSpace(string(v)))
		}
		mem, err := normaliseMemory(s)
		if err != nil {
			return nil, fmt.Errorf("%q.memory: %w", field, err)
		}
		out.Memory = mem
	}
	if *out == (actorResources{}) {
		return nil, nil
	}
	return out, nil
}

// normaliseMemory turns one author-written quantity into the Kubernetes spelling of the same amount.
func normaliseMemory(s string) (string, error) {
	m := memoryQuantity.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("%q is not a memory quantity — write a whole number with Ki/Mi/Gi (or "+
			"Docker's k/m/g), e.g. \"1Gi\" or \"512m\"", s)
	}
	whole, frac, suffix := m[1], m[2], m[3]
	unit, known := memorySuffix[suffix]
	if !known {
		return "", fmt.Errorf("%q: the unit %q is in neither Kubernetes' table (Ki Mi Gi Ti Pi Ei, M G T P E) "+
			"nor Docker's (b k m g t)", s, suffix)
	}
	if strings.Trim(whole+strings.TrimPrefix(frac, "."), "0") == "" {
		return "", fmt.Errorf("%q is no memory at all", s)
	}
	if frac != "" && unit == "" {
		return "", fmt.Errorf("%q is a fraction of a byte", s)
	}
	return whole + frac + unit, nil
}
