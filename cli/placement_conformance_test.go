// placement_conformance_test.go — THE GO ARM of shared/conformance/placement.json.
//
// This side is a WRITER, and it is the SECOND one: `sdk/python/kontra/fleet.py` builds the same
// Fleet desired state out of `hold()`/`place()`/`up()`, `cli/fleet.go` builds it out of
// `kontra fleet up|deploy`, and `control/orchestrator/src/infra/stacks.ts:coerceFleetArgs` is the only reader.
// Nothing imports anything across the three, and the reader DISCARDS WITHOUT A WORD every key it
// does not recognise — which is how `--tmux` rode as a boolean for a release, narrowed away before
// the program saw it, letting a Machine deploy "successfully" and never be viewable.
//
// THE KEY SET IS THE CONTRACT, NOT THE VALUES. This side's placement fields come from an Artifact
// on disk and Python's come from `resolveBundle` over the network, so the bytes genuinely differ
// for the same shape. What must not differ is which keys are there — and in particular `machines`,
// whose ABSENCE is not "leave the count alone" but `Number(undefined ?? 0)` = 0, a converge that
// builds no Droplets and deletes the ones that are there while reporting success.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type placementCorpus struct {
	Keys struct {
		Why      string `json:"why"`
		Sections map[string]struct {
			Keys []string `json:"keys"`
			Why  string   `json:"why"`
		} `json:"sections"`
	} `json:"keys"`
	PlacementKeys struct {
		Required []string `json:"required"`
		Optional []string `json:"optional"`
		NotHere  []string `json:"not_here"`
	} `json:"placement_keys"`
	Types struct {
		Rules struct {
			String       []string `json:"string"`
			Number       []string `json:"number"`
			StringArray  []string `json:"string_array"`
			ObjectArray  []string `json:"object_array"`
			NeverBoolean string   `json:"never_boolean"`
		} `json:"rules"`
	} `json:"types"`
	WriterCases []struct {
		Name      string   `json:"name"`
		Why       string   `json:"why"`
		Keys      []string `json:"keys"`
		Forbidden []string `json:"forbidden"`
		// Placements is how many entries the case's converge carries; nil for a converge that
		// sends no list at all. A COUNT, because a converge naming one of two Artifacts deletes
		// the other and reports success.
		Placements *int `json:"placements"`
		// PlacementValues pins the VALUE of a key inside the entry — `spread` needs it, because a
		// writer that dropped `workers` would place on every Machine anyway and pass any assertion
		// about the key alone.
		PlacementValues map[string]int `json:"placement_values"`
	} `json:"writer_cases"`
}

func loadPlacementCorpus(t *testing.T) placementCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "shared", "conformance", "placement.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c placementCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	// A CORPUS THAT SILENTLY SHRANK TO NOTHING PASSES EVERYTHING (shared/conformance/README.md §3).
	if len(c.WriterCases) != 5 {
		t.Fatalf("writer_cases = %d, want the five this arm drives", len(c.WriterCases))
	}
	if len(c.Keys.Sections["required_always"].Keys) == 0 || len(c.Keys.Sections["from_resolver"].Keys) == 0 {
		t.Fatal("the corpus key lists are empty; every assertion below would be vacuous")
	}
	if len(c.PlacementKeys.Required) == 0 || len(c.PlacementKeys.Optional) == 0 {
		t.Fatal("the corpus placement key lists are empty; the nested assertions would be vacuous")
	}
	return c
}

// placementCases builds each corpus case the way `kontra fleet` builds it.
//
// `machines_only` is `kontra fleet up`, `placed` is `kontra fleet deploy`, and
// `placed_with_density` is the same with `--sessions`. Those are the same three converges
// `fleet.hold()`, `f.place()` and `f.place(sessions=…)` produce on the Python side, which is what
// makes this a shared contract rather than two independent tests that happen to agree.
func placementCases(t *testing.T) map[string]map[string]any {
	t.Helper()
	dir := writeActor(t, webcrawlManifest)
	build := func(argv []string, art *bundle) map[string]any {
		f := fleetFlagSet("conformance")
		if err := f.fs.Parse(argv); err != nil {
			t.Fatal(err)
		}
		a, err := f.args(art)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	base := []string{"--count", "2", "--tag", "crawl", "--actor", dir, "--controller", "10.9.9.9"}

	// ═══ THE TWO PACKED CASES COME FROM THE INHERIT PATH, WHICH IS THIS WRITER'S ONLY WAY TO ═══
	//
	// `kontra fleet` has no `--workers` and no second `--actor`: an operator PLACES one Artifact with
	// `fleet deploy`, and packing is a caller-SDK act (ADR 0037's `f.place`). What this writer must
	// nonetheless get right is the converge it sends over a Fleet somebody else packed — `fleet up
	// --count 6` on a two-Artifact Fleet — because a scale-up that inherited one of two would delete
	// the other, run its teardown, and report success. So the packed cases are driven through
	// `inheritPlacement` against the outputs such a Fleet echoes.
	scale := func(sessions int, outputs map[string]any) map[string]any {
		a := build(base, nil)
		inheritPlacement(a, outputs, sessions)
		return a
	}
	echoed := func(entries ...map[string]any) map[string]any {
		list := make([]any, 0, len(entries))
		for _, e := range entries {
			list = append(list, e)
		}
		// FLOAT64 AND NOT INT, because these came back through JSON. `placementsOutput` is what has
		// to turn them into the numbers the corpus names, and a fixture built from `int` would test
		// the conversion against a value that never needed converting.
		return map[string]any{"tag": "crawl", "machines": float64(2), "placements": list}
	}
	entry := func(name, sha string, extra map[string]any) map[string]any {
		out := map[string]any{
			"actorName":    name,
			"actorVersion": "0.1.0",
			"actorEngine":  "py",
			"bundleUrl":    "http://10.9.9.9:5000/v2/bundles/" + name + "/blobs/sha256:" + sha,
			"bundleSha":    sha,
			"controller":   "10.9.9.9",
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	shaA, shaB := strings.Repeat("a", 64), strings.Repeat("b", 64)

	return map[string]map[string]any{
		"machines_only":       build(base, nil),
		"placed":              build(base, fakeBundle()),
		"placed_with_density": build(append(append([]string{}, base...), "--sessions", "8"), fakeBundle()),
		"packed": scale(0, echoed(
			entry("nscheck", shaA, nil),
			entry("subfinder", shaB, nil),
		)),
		"spread": scale(0, echoed(entry("subfinder", shaB, map[string]any{"workers": float64(2)}))),
	}
}

// placementEntries is every placement one converge DESCRIBES, in whichever spelling its writer used.
//
// ═══ IT FOLDS, BECAUSE THE READER FOLDS ═══
//
// There are two legal spellings for one placement — the `placements` array, and the pre-packing
// scalars `kontra fleet deploy` still sends — and `programs/fleet.ts:placementsOf` turns the second
// into the first on arrival. An arm that counted only the array would report this writer as placing
// NOTHING on every converge it has ever sent, which is the opposite of what the corpus says about
// the very case it is checking. So the fold lives here too, and mirroring the reader is the point.
func placementEntries(t *testing.T, args map[string]any) []map[string]any {
	t.Helper()
	raw, present := args["placements"]
	if !present {
		if url, _ := args["bundleUrl"].(string); url == "" {
			return nil
		}
		folded := map[string]any{}
		for _, k := range []string{"actorName", "actorVersion", "actorEngine", "bundleUrl", "bundleSha", "controller", "maxSessions"} {
			if v, ok := args[k]; ok {
				folded[k] = v
			}
		}
		return []map[string]any{folded}
	}
	switch list := raw.(type) {
	case []map[string]any:
		return list
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			e, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("a placement entry is %T, and the reader drops anything that is not an object", item)
			}
			out = append(out, e)
		}
		return out
	default:
		t.Fatalf("placements is %T; the reader keeps it only as an array", raw)
		return nil
	}
}

func TestGoWriterSendsThePlacementCountEachCaseNames(t *testing.T) {
	// THE ARM PACKING NEEDED. `placements` carries the whole desired state, so the failure worth
	// pinning is a COUNT: a converge naming one of two Artifacts deletes the other, runs its
	// teardown, stops its Worker, and Pulumi reports success either way.
	c := loadPlacementCorpus(t)
	cases := placementCases(t)
	for _, wc := range c.WriterCases {
		args := cases[wc.Name]
		entries := placementEntries(t, args)
		if wc.Placements == nil {
			if len(entries) > 0 {
				t.Errorf("%s places nothing and must describe no placement", wc.Name)
			}
			continue
		}
		if len(entries) != *wc.Placements {
			t.Errorf("%s sent %d placement(s), want %d: %s", wc.Name, len(entries), *wc.Placements, wc.Why)
		}
		for key, want := range wc.PlacementValues {
			if got, _ := entries[0][key].(int); got != want {
				t.Errorf("%s sent %s=%v inside its placement, corpus says %d", wc.Name, key, entries[0][key], want)
			}
		}
	}
}

func TestGoWriterSendsNoKeyInsideAPlacementTheReaderWouldDrop(t *testing.T) {
	// THE SAME SENTENCE ONE LEVEL DOWN. `coercePlacement` is a second narrowing with a second key
	// list, and a key it does not know is dropped exactly as silently.
	c := loadPlacementCorpus(t)
	known := map[string]bool{}
	for _, k := range append(append([]string{}, c.PlacementKeys.Required...), c.PlacementKeys.Optional...) {
		known[k] = true
	}
	seen := 0
	for name, args := range placementCases(t) {
		for _, e := range placementEntries(t, args) {
			seen++
			var unknown []string
			for k := range e {
				if !known[k] {
					unknown = append(unknown, k)
				}
			}
			sort.Strings(unknown)
			if len(unknown) > 0 {
				t.Errorf("%s sends %v inside a placement, which the reader drops", name, unknown)
			}
			for _, k := range c.PlacementKeys.Required {
				if _, present := e[k]; !present {
					t.Errorf("%s: an entry without %q is dropped ENTIRELY", name, k)
				}
			}
			for _, k := range c.PlacementKeys.NotHere {
				if _, present := e[k]; present {
					t.Errorf("%s puts %q inside a placement, where it is a second answer", name, k)
				}
			}
		}
	}
	if seen < 3 {
		t.Fatalf("examined %d placement entries; this sweep proves nothing", seen)
	}
}

func TestGoWriterProducesTheKeysEachPlacementCaseNames(t *testing.T) {
	c := loadPlacementCorpus(t)
	cases := placementCases(t)
	for _, wc := range c.WriterCases {
		args, ok := cases[wc.Name]
		if !ok {
			t.Fatalf("this arm does not drive corpus case %q", wc.Name)
		}
		for _, k := range wc.Keys {
			if _, present := args[k]; !present {
				t.Errorf("%s is missing %q: %s", wc.Name, k, wc.Why)
			}
		}
		for _, k := range wc.Forbidden {
			if _, present := args[k]; present {
				t.Errorf("%s sent %q, which that converge must not carry", wc.Name, k)
			}
		}
	}
}

func TestEveryGoConvergeCarriesTheKeysWhoseAbsenceIsATeardown(t *testing.T) {
	// `required_always` on the PLACEMENT converge as much as on the capacity one. A deploy written
	// as a patch — placement keys only — deletes every Droplet it was placing onto.
	c := loadPlacementCorpus(t)
	for name, args := range placementCases(t) {
		for _, k := range c.Keys.Sections["required_always"].Keys {
			if _, present := args[k]; !present {
				t.Errorf("%s omits %q: %s", name, k, c.Keys.Sections["required_always"].Why)
			}
		}
	}
}

func TestGoWriterSendsNoKeyTheReaderWouldDrop(t *testing.T) {
	c := loadPlacementCorpus(t)
	known := map[string]bool{}
	for _, section := range []string{"required_always", "provider", "from_resolver", "density", "packing", "not_in_fleet_args"} {
		for _, k := range c.Keys.Sections[section].Keys {
			known[k] = true
		}
	}
	for name, args := range placementCases(t) {
		var unknown []string
		for k := range args {
			if !known[k] {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 0 {
			t.Errorf("%s sends %v, which coerceFleetArgs drops silently", name, unknown)
		}
	}
}

func TestGoWriterSendsNoBooleans(t *testing.T) {
	// `--tmux` was one, and it never reached the program. The NUMBER keys are worse than dropped:
	// `Number(true)` is 1, so a boolean count is a one-Machine fleet nothing refuses.
	//
	// IT SWEEPS INSIDE THE PLACEMENTS TOO, because that is where the temptation now is: `spread=` is
	// a boolean at the caller SDK's call site and resolves to the number `workers` before anything
	// crosses. A writer that forwarded the flag would be dropped in silence and one Worker per
	// Machine would quietly become whatever the default was.
	c := loadPlacementCorpus(t)
	seen := 0
	for name, args := range placementCases(t) {
		for k, v := range args {
			if _, isBool := v.(bool); isBool {
				t.Errorf("%s sends %q as a boolean: %s", name, k, c.Types.Rules.NeverBoolean)
			}
		}
		for _, e := range placementEntries(t, args) {
			seen++
			for k, v := range e {
				if _, isBool := v.(bool); isBool {
					t.Errorf("%s sends %q as a boolean inside a placement: %s", name, k, c.Types.Rules.NeverBoolean)
				}
			}
		}
	}
	if seen < 3 {
		t.Fatalf("examined %d placement entries; this sweep proves nothing", seen)
	}
}

func TestGoWriterTypesMatchTheCorpus(t *testing.T) {
	// Not merely "no booleans": a string key sent as a number is dropped by the reader just as
	// silently, and this is the assertion that would catch it.
	c := loadPlacementCorpus(t)
	wantString := map[string]bool{}
	for _, k := range c.Types.Rules.String {
		wantString[k] = true
	}
	wantNumber := map[string]bool{}
	for _, k := range c.Types.Rules.Number {
		wantNumber[k] = true
	}
	check := func(where string, m map[string]any) {
		for k, v := range m {
			switch {
			case wantString[k]:
				if _, ok := v.(string); !ok {
					t.Errorf("%s sends %q as %T; the reader keeps it only as a string", where, k, v)
				}
			case wantNumber[k]:
				if _, ok := v.(int); !ok {
					t.Errorf("%s sends %q as %T; the reader keeps it only as a number", where, k, v)
				}
			}
		}
	}
	for name, args := range placementCases(t) {
		check(name, args)
		// THE ENTRIES TOO, and this is the one the JSON round trip makes real: outputs come back
		// with every number a `float64`, so a writer that forwarded them verbatim would send
		// `workers: 2.0`. `coerceFleetArgs` survives that by accident (`Number()` of a float is that
		// float) and `machinesFor` does not — `Number.isInteger` is the check on the other side and
		// this is the one that keeps the two honest.
		for _, e := range placementEntries(t, args) {
			check(name+" placement", e)
		}
	}
}
