package main

// fleet_inherit_test.go — what `kontra fleet up` keeps when it is not being asked to change it.
//
// `fleetUp` sends the WHOLE desired state, so a converge that said nothing about the placement would
// REMOVE it — which is why the inherit exists at all. Packing (ADR 0037, slice 11) gives a Fleet
// several placements, and the scalars in the stack outputs describe only the FIRST, so inheriting
// them on a packed Fleet is that same un-deploy, halved and silent: the second Worker's
// `command.remote.Command` leaves the program, Pulumi runs its teardown, and the operator's
// `--count 6` reports success.
//
// `cli/placement_conformance_test.go` drives the same function against the corpus, which is what
// keeps the KEYS honest across languages; this file is about the BEHAVIOUR — which branch runs, what
// the operator reads, and what a Fleet placed before packing still gets.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestScalingAPackedFleetKeepsBothPlacements(t *testing.T) {
	a := scaleArgs(t)
	line := inheritPlacement(a, packedOutputs(), 0)

	placed, _ := a["placements"].([]map[string]any)
	if len(placed) != 2 {
		t.Fatalf("inherited %d placement(s) from a packed Fleet, want 2: %v", len(placed), a["placements"])
	}
	names := []string{fmt.Sprint(placed[0]["actorName"]), fmt.Sprint(placed[1]["actorName"])}
	sort.Strings(names)
	if names[0] != "nscheck" || names[1] != "subfinder" {
		t.Fatalf("inherited %v, want both Artifacts", names)
	}
	// THE SCALARS MUST NOT RIDE ALONG. `placementsOf` prefers the array, so a scalar left here would
	// be a second desired state that disagrees with the true one and that nothing reads.
	for _, k := range []string{"actorName", "bundleUrl", "bundleSha", "maxSessions"} {
		if _, present := a[k]; present {
			t.Errorf("the packed converge also carries the single-placement key %q", k)
		}
	}
	// AND THE LINE NAMES BOTH. "keeping the placed actor nscheck@0.1.0" on a Fleet running two is
	// the exact sentence an operator would read as "the other one is gone" — on the command whose
	// whole risk is that it might be.
	for _, want := range []string{"nscheck@0.1.0", "subfinder@0.2.0", "2 placed actors"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line an operator reads does not mention %q: %s", want, line)
		}
	}
}

// The control, and it is about every stack this repo has ever converged: they were created from the
// single-placement keys and their outputs echo no `placements`, so the old branch has to still run.
func TestScalingAFleetPlacedBeforePackingStillInheritsTheScalars(t *testing.T) {
	a := scaleArgs(t)
	line := inheritPlacement(a, map[string]any{
		"bundleUrl":    "http://10.9.9.9:5000/v2/bundles/nscheck/blobs/sha256:" + strings.Repeat("a", 64),
		"bundleSha":    strings.Repeat("a", 64),
		"actorName":    "nscheck",
		"actorVersion": "0.1.0",
		"actorEngine":  "py",
		"controller":   "10.9.9.9",
		"maxSessions":  float64(8),
	}, 0)
	if _, present := a["placements"]; present {
		t.Error("a Fleet that echoed no placements must not acquire an array here")
	}
	if a["actorName"] != "nscheck" || a["maxSessions"] != 8 {
		t.Fatalf("the pre-packing inherit lost something: %v", a)
	}
	if !strings.Contains(line, "the placed actor nscheck@0.1.0") {
		t.Errorf("line = %q", line)
	}
}

// `fleet up --sessions` names no actor, so on a Fleet running two there is nothing else it could
// honestly mean than both of them.
func TestScalingAPackedFleetWithSessionsRedensifiesEveryPlacement(t *testing.T) {
	f := fleetFlagSet("up")
	if err := f.fs.Parse([]string{"--tag", "dns", "--count", "6", "--sessions", "12"}); err != nil {
		t.Fatal(err)
	}
	a, err := f.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	inheritPlacement(a, packedOutputs(), 12)
	placed, _ := a["placements"].([]map[string]any)
	if len(placed) != 2 {
		t.Fatalf("inherited %d placement(s), want 2", len(placed))
	}
	for _, p := range placed {
		if p["maxSessions"] != 12 {
			t.Errorf("%v kept maxSessions=%v; --sessions on a packed Fleet applies to all of them",
				p["actorName"], p["maxSessions"])
		}
	}
	// AND THE CONVERGE-LEVEL ONE `args()` PUT THERE IS GONE, or it would be a half-filled second
	// desired state beside the true one.
	if _, present := a["maxSessions"]; present {
		t.Error("the converge still carries a top-level maxSessions beside the placements")
	}
}

// A Fleet that has never been placed on echoes `placements: []` beside empty scalars, and neither
// branch should claim it is running anything.
func TestAnEmptyPlacementsArrayIsNotAPackedFleet(t *testing.T) {
	a := map[string]any{"tag": "dns", "machines": 6}
	if line := inheritPlacement(a, map[string]any{"placements": []any{}, "bundleUrl": ""}, 0); line != "" {
		t.Fatalf("a Fleet placing nothing produced %q", line)
	}
	if _, present := a["placements"]; present {
		t.Error("an empty echo must not put an empty array in the converge")
	}
}

// A HALF-BUILT ENTRY IS DROPPED AND THE REST SURVIVE, matching `coercePlacement` on the reader's
// side. An entry with no `bundleUrl` would place a Worker that fetches the empty string; one with no
// `actorName` would write a co-tenant's unit, env, root and scrape target.
func TestAHalfBuiltEchoedPlacementIsDroppedWithoutTakingTheOthers(t *testing.T) {
	a := scaleArgs(t)
	out := packedOutputs()
	list := out["placements"].([]any)
	out["placements"] = append(list,
		map[string]any{"actorName": "no-bundle"},
		map[string]any{"bundleUrl": "http://10.9.9.9/x"},
		"not an object",
	)
	inheritPlacement(a, out, 0)
	placed, _ := a["placements"].([]map[string]any)
	if len(placed) != 2 {
		t.Fatalf("inherited %d placement(s); the malformed entries must go and the good ones stay: %v",
			len(placed), a["placements"])
	}
}

// --- what `fleet status` shows ------------------------------------------------------------------

// TestFleetStatusNamesEveryPlacedActor — the surface where a co-tenant would otherwise vanish.
//
// This printed `outputs["actorName"]`, which on a packed Fleet is the FIRST placement and nothing
// else. An operator running `kontra fleet status` to check what is on their Machines would read one
// `actor:` line on a Fleet running two and conclude the other had gone — on exactly the command they
// ran to find out.
func TestFleetStatusNamesEveryPlacedActor(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printFleet(statusOf(packedOutputs())); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"nscheck@0.1.0", "subfinder@0.2.0", "PACKED", "share their egress"} {
		if !strings.Contains(out, want) {
			t.Errorf("`fleet status` does not mention %q:\n%s", want, out)
		}
	}
	// The two axes are printed rather than assumed: nothing else says what a Worker's cap is, and
	// nothing else says a placement is on two of the Machines rather than all of them.
	if !strings.Contains(out, "sessions/worker 8") {
		t.Errorf("the density is not shown:\n%s", out)
	}
	if !strings.Contains(out, "on 2 machine(s)") {
		t.Errorf("a short placement reads as if it were on every Machine:\n%s", out)
	}
}

// TestFleetStatusStillDescribesAFleetPlacedBeforePacking is the control, and it is the one a reader
// who only understood the array would break: those stacks echo no `placements`, so `fleet status`
// would print nothing about the actor at all — a surface that goes BLANK rather than wrong, which
// reads as "nothing is placed" on a Fleet that is working.
func TestFleetStatusStillDescribesAFleetPlacedBeforePacking(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printFleet(statusOf(map[string]any{
			"actorName":    "nscheck",
			"actorVersion": "0.1.0",
			"bundleSha":    strings.Repeat("a", 64),
			"maxSessions":  float64(8),
			"inventory": map[string]any{
				"kf-dns-01": map[string]any{"name": "kf-dns-01", "host": "10.124.0.9"},
			},
		})); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "nscheck@0.1.0") || !strings.Contains(out, "sessions/worker 8") {
		t.Fatalf("a pre-packing Fleet lost its actor line:\n%s", out)
	}
	if strings.Contains(out, "PACKED") {
		t.Errorf("one placement must not claim to be packed:\n%s", out)
	}
}

// TestFleetStatusOnAMachinesOnlyFleetClaimsNoActor — `fleet up` with no placement is a real state,
// and a status that invented an actor line for it would be the opposite failure.
func TestFleetStatusOnAMachinesOnlyFleetClaimsNoActor(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printFleet(statusOf(map[string]any{
			"tag": "dns",
			"inventory": map[string]any{
				"kf-dns-01": map[string]any{"name": "kf-dns-01", "host": "10.124.0.9"},
			},
		})); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "actor:") {
		t.Fatalf("a Machines-only Fleet reported an actor:\n%s", out)
	}
	if !strings.Contains(out, "kf-dns-01") {
		t.Fatalf("the inventory is missing, so this test proves nothing:\n%s", out)
	}
}

// statusOf wraps stack outputs as a completed operation, which is what `printFleet` takes.
func statusOf(outputs map[string]any) *stackOpStatus {
	if _, ok := outputs["inventory"]; !ok {
		outputs["inventory"] = map[string]any{
			"kf-dns-01": map[string]any{"name": "kf-dns-01", "host": "10.124.0.9"},
			"kf-dns-02": map[string]any{"name": "kf-dns-02", "host": "10.124.0.10"},
		}
	}
	st := &stackOpStatus{FQN: "kontra-fleet/dns", Status: "COMPLETED"}
	st.Result = &struct {
		FQN     string         `json:"fqn"`
		Result  string         `json:"result"`
		Changes map[string]int `json:"changes"`
		Outputs map[string]any `json:"outputs"`
	}{FQN: st.FQN, Result: "succeeded", Outputs: outputs}
	return st
}

// scaleArgs is `kontra fleet up --tag dns --count 6` — a scale that says nothing about placement,
// which is the only command this inherit runs under.
func scaleArgs(t *testing.T) map[string]any {
	t.Helper()
	f := fleetFlagSet("up")
	if err := f.fs.Parse([]string{"--tag", "dns", "--count", "6"}); err != nil {
		t.Fatal(err)
	}
	a, err := f.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// packedOutputs is what a two-Artifact Fleet echoes, AS JSON DECODES IT — every number a float64.
// Building it with `int` would test `placementsOutput`'s conversion against values that never needed
// converting, which is the shape of fixture this repo has already been caught by.
func packedOutputs() map[string]any {
	entry := func(name, version, sha string, extra map[string]any) map[string]any {
		out := map[string]any{
			"actorName":    name,
			"actorVersion": version,
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
	return map[string]any{
		// The scalars a packed Fleet still echoes, describing the FIRST placement. They are the trap
		// this whole file exists for: non-empty, correct about one Artifact, wrong about the Fleet.
		"actorName":    "nscheck",
		"actorVersion": "0.1.0",
		"bundleUrl":    "http://10.9.9.9:5000/v2/bundles/nscheck/blobs/sha256:" + strings.Repeat("a", 64),
		"bundleSha":    strings.Repeat("a", 64),
		"maxSessions":  float64(8),
		"placements": []any{
			entry("nscheck", "0.1.0", strings.Repeat("a", 64), map[string]any{"maxSessions": float64(8)}),
			entry("subfinder", "0.2.0", strings.Repeat("b", 64), map[string]any{"workers": float64(2)}),
		},
	}
}
