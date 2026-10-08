package hostengine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realProgram is `control/pulumi/Pulumi.yaml` ITSELF, not a copy of it.
//
// EVERY ASSERTION IN THIS FILE THAT MATTERS IS AGAINST THE COMMITTED PROGRAM, for the reason
// `table_test.go` gives about the provider sweep: a test that re-states the thing under test in Go
// passes forever after somebody edits the file. The path is relative — `cli/internal/hostengine` to the
// repository root — and a missing file is `t.Fatal` and never `t.Skip`, because a skip here is a green
// run that checked nothing.
func realProgram(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "control", "pulumi", "Pulumi.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the host program is what these assertions are about and it could not be read: %v", err)
	}
	return b
}

// ADR 0052 §7's acceptance criterion, as the exact set.
//
// IT ASSERTS BOTH DIRECTIONS AND THAT IS THE POINT. A volume that quietly LOSES `protect: true` is
// data somebody can delete with an edit; a volume that quietly GAINS it is a decision §7 requires to be
// visible ("an unprotected volume must read as a decision, not an oversight" — and so must a protected
// one). So the test names the three that are unprotected rather than counting them, and fails on any
// change in either direction with a message saying which way it moved.
func TestExactlyTheseVolumesAreUnprotected(t *testing.T) {
	set, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.ByName) != 13 {
		t.Fatalf("the program declares %d volumes and this test knows about 13: %v", len(set.ByName), set.Names())
	}
	want := map[string]string{
		// The only genuinely rebuildable one: a config file that lives in the repository, written by
		// temporal-config and read by temporal.
		"kontra_temporal-dynamicconfig": "rebuilt by the next converge",
		// Retention-bounded by the program's own config (metricsRetention/logsRetention = 6): losing
		// either costs diagnosis and not integrity, and ADR 0025 puts a Run's durable story in history/
		// on seaweed precisely because logs age out.
		"kontra_victoriametrics-data": "retention-bounded observability data",
		"kontra_victorialogs-data":    "retention-bounded observability data",
		// Written from scratch on every converge by the `registry-config` one-shot: the rendered zot
		// config, its htpasswd and the static busybox the healthcheck runs. Protecting it would protect
		// a derived file that the next converge overwrites anyway.
		"kontra_registry-config": "rebuilt by the next converge",
	}
	got := map[string]bool{}
	for _, n := range set.Unprotected() {
		got[n] = true
		if _, ok := want[n]; !ok {
			t.Errorf("%s is NOT protected and this test does not know why. Either it lost `protect: true` "+
				"— which is a volume an edit can now delete — or it is a deliberate decision that has to be "+
				"added here with its reason, because §7 requires an unprotected volume to read as a decision",
				n)
		}
	}
	for n, why := range want {
		if !got[n] {
			if _, ok := set.ByName[n]; !ok {
				t.Errorf("%s is gone from the program entirely", n)
				continue
			}
			t.Errorf("%s gained `protect: true`. That is safer and it is still a change somebody decided: "+
				"it was unprotected because it is %s, and a protected one can only be removed with "+
				"`pulumi state unprotect` typed against a URN", n, why)
		}
	}
	if p := set.Protected(); p != 9 {
		t.Errorf("%d volumes carry protect: true, want 9", p)
	}
}

// THE NINE ARE THE ONES THAT HOLD SOMETHING NOTHING ELSE HOLDS, and two of them are one unit.
//
// §7: "`seaweed-data` and `postgres-data` are ONE unit… Protect one and not the other and you get a
// catalog pointing at nothing, or gigabytes of files nothing can find." This asserts the pair together,
// separately from the set above, because that is the invariant a future edit is most likely to break by
// halves.
func TestTheDuckLakePairIsProtectedTogether(t *testing.T) {
	set, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	cat, par := set.ByName["kontra_postgres-data"], set.ByName["kontra_seaweed-data"]
	if cat.Protected != par.Protected {
		t.Fatalf("postgres-data protected=%v and seaweed-data protected=%v. They are one unit: the "+
			"DuckLake catalog says which files are live and seaweed holds the files. `data/maintenance.ts`'s "+
			"reclamation chain assumes the catalog is the authority for liveness, so an out-of-band loss on "+
			"either side breaks that SILENTLY", cat.Protected, par.Protected)
	}
	if !cat.Protected {
		t.Error("neither half of the DuckLake unit is protected")
	}
}

// The attachment half of the identity: read out of the program's own mounts, through the `${vol….name}`
// interpolation every one of them uses.
func TestDeclaredVolumesReadsWhatMountsEachOne(t *testing.T) {
	set, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	// The container NAMES the program gives, not the resource names: `kontra-api`, not
	// `orchestratorApi`. That distinction is the whole reason the recorded side is keyed the same way —
	// a gate comparing resource names on one side and container names on the other would refuse every
	// converge.
	for name, wantOne := range map[string]string{
		"kontra_postgres-data":   "kontra-postgres",
		"kontra_seaweed-data":    "kontra-seaweed",
		"kontra_pulumi-state":    "kontra-infra",
		"kontra_secrets-store":   "kontra-api",
		"kontra_orchestrator-db": "kontra-api",
	} {
		v, ok := set.ByName[name]
		if !ok {
			t.Errorf("%s is not declared", name)
			continue
		}
		if len(v.Services) == 0 {
			t.Errorf("%s is mounted by nothing, so the attachment half of the gate would be vacuous for it", name)
			continue
		}
		found := false
		for _, s := range v.Services {
			if s == wantOne {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is mounted by %v and %s is not among them — either the mount moved or the "+
				"${vol….name} resolution is broken", name, v.Services, wantOne)
		}
	}
	// kontra-home is mounted by THREE containers, and a gate that only remembered the first would call a
	// dropped mount "unchanged".
	if home := set.ByName["kontra_kontra-home"]; len(home.Services) < 3 {
		t.Errorf("kontra-home is mounted by %v; the program mounts it in three containers", home.Services)
	}
}

// THE ONE THING IN DeclaredVolumes THAT HAS TO BE RIGHT, and it is the same failure mode ParseSummary
// refuses for `changeSummary`: every check in volumes.go is "everything recorded is still declared",
// which is trivially true of nothing declared. A program the extraction cannot read must refuse.
func TestAProgramWithNoVolumesIsRefusedAndNotReadAsSafe(t *testing.T) {
	for _, prog := range []string{
		"name: kontra-control\nruntime: yaml\n",
		"name: kontra-control\nresources:\n  net:\n    type: docker:index:Network\n",
	} {
		if set, err := DeclaredVolumes([]byte(prog)); err == nil {
			t.Errorf("a program with no volumes was accepted (%d found) — every assertion downstream would "+
				"then pass, which is the one wrong answer this gate cannot notice", len(set.ByName))
		}
	}
	if _, err := DeclaredVolumes([]byte("name: [oh dear\n")); err == nil {
		t.Error("unparseable YAML must be an error and not an empty volume set")
	}
	if _, err := DeclaredVolumes([]byte("resources:\n  volX:\n    type: docker:index:Volume\n")); err == nil {
		t.Error("a volume with no `name:` must refuse — docker would invent one and the operator's data " +
			"would not be adopted")
	}
}

// --- the recorded side ---------------------------------------------------------------------------

// exportWith builds a `pulumi stack export` in the shape MEASURED on 3.244.0 (see exportDoc): one
// volume resource per entry, plus one container mounting each.
func exportWith(t *testing.T, vols map[string]string, protect map[string]bool, mounts map[string][]string) []byte {
	t.Helper()
	type res map[string]any
	out := []res{{
		"urn":    "urn:pulumi:local::kontra-control::pulumi:pulumi:Stack::kontra-control-local",
		"type":   "pulumi:pulumi:Stack",
		"custom": false,
	}}
	for logical, name := range vols {
		r := res{
			"urn":     "urn:pulumi:local::kontra-control::docker:index/volume:Volume::" + logical,
			"type":    stateVolumeType,
			"custom":  true,
			"inputs":  map[string]any{"name": name},
			"outputs": map[string]any{"name": name, "id": name},
		}
		if protect[logical] {
			r["protect"] = true
		}
		out = append(out, r)
	}
	for svc, names := range mounts {
		var mm []map[string]any
		for _, n := range names {
			mm = append(mm, map[string]any{"volumeName": n, "containerPath": "/data"})
		}
		out = append(out, res{
			"urn":     "urn:pulumi:local::kontra-control::docker:index/container:Container::" + svc,
			"type":    stateContainerType,
			"custom":  true,
			"inputs":  map[string]any{"name": svc, "volumes": mm},
			"outputs": map[string]any{"name": svc},
		})
	}
	b, err := json.Marshal(map[string]any{"version": 3, "deployment": map[string]any{"resources": out}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// MEASURED: a stack created by `stack select --create` and never converged exports a deployment with
// `manifest`, `secrets_providers` and `metadata` and NO `resources` key. That is an install that has
// not happened, not an install with nothing to lose.
func TestAnUnconvergedStackIsNotAnInstallWithNothingToLose(t *testing.T) {
	const empty = `{"version":3,"deployment":{"manifest":{"time":"2026-09-26T23:14:21Z","version":"v3.244.0"},
	  "secrets_providers":{"type":"passphrase","state":{"salt":"v1:x"}},"metadata":{}}}`
	set, err := RecordedVolumes([]byte(empty))
	if err != nil {
		t.Fatal(err)
	}
	if set.Converged {
		t.Error("a deployment with no resources was read as a converged install")
	}
	if len(set.ByName) != 0 {
		t.Errorf("volumes = %v, want none", set.Names())
	}
}

func TestACorruptExportIsRefusedRatherThanReadAsEmpty(t *testing.T) {
	if _, err := RecordedVolumes([]byte("warning: something\n{")); err == nil {
		t.Error("an unreadable `stack export` must refuse: it is the only record of which volumes this " +
			"installation owns, and reading it as 'nothing to protect' is how an update walks past the " +
			"thing it exists to check")
	}
}

// --- the gate ------------------------------------------------------------------------------------

// The happy path, and it has to be the same program on both sides or every refusal below is vacuous.
func TestTheGatePassesWhenNothingMoved(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	vols, protect, mounts := mirrorOf(declared)
	recorded, err := RecordedVolumes(exportWith(t, vols, protect, mounts))
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.Converged || len(recorded.ByName) != len(declared.ByName) {
		t.Fatalf("the mirror is wrong: %d recorded vs %d declared", len(recorded.ByName), len(declared.ByName))
	}
	if err := CheckVolumeIdentity(declared, recorded); err != nil {
		t.Fatalf("an installation matching the program was refused:\n%v", err)
	}
}

// mirrorOf turns the declared set into the state an installation of that program would have. Derived
// from the program rather than typed, so the three refusals below are each ONE mutation away from a
// passing case — which is what makes them mean something.
func mirrorOf(set VolumeSet) (vols map[string]string, protect map[string]bool, mounts map[string][]string) {
	vols, protect, mounts = map[string]string{}, map[string]bool{}, map[string][]string{}
	for _, v := range set.ByName {
		vols[v.Logical] = v.Name
		protect[v.Logical] = v.Protected
		for _, s := range v.Services {
			mounts[s] = append(mounts[s], v.Name)
		}
	}
	return
}

// §7's dangerous case: "A converge that creates a volume under a new name and orphans the old one
// contains no delete at all." The AC also requires BOTH names in the refusal.
func TestARenamedVolumeIsRefusedWithBothNames(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	vols, protect, mounts := mirrorOf(declared)
	// The installation has `kontra_seaweed-data`; the program has been edited to declare
	// `kontra_seaweed-data-v2` instead. No delete appears in such a plan — the old volume is simply
	// never mentioned again.
	renamed := newVolumeSet()
	for _, v := range declared.ByName {
		if v.Name == "kontra_seaweed-data" {
			v.Name = "kontra_seaweed-data-v2"
		}
		renamed.add(v)
	}
	recorded, err := RecordedVolumes(exportWith(t, vols, protect, mounts))
	if err != nil {
		t.Fatal(err)
	}
	err = CheckVolumeIdentity(renamed, recorded)
	if err == nil {
		t.Fatal("a renamed volume was accepted — the stack would come up healthy and EMPTY, with 34 GB of " +
			"parquet on disk where nothing looks")
	}
	for _, want := range []string{"kontra_seaweed-data", "kontra_seaweed-data-v2", "no delete at all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must print the old name and the new one; %q missing from:\n%v", want, err)
		}
	}
}

// The same docker volume under a different PULUMI resource name. Pulumi addresses by URN, so this is a
// delete plus a create — and with retainOnDelete the bytes survive while the installation stops
// knowing it owns them.
func TestARekeyedVolumeIsRefused(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	vols, protect, mounts := mirrorOf(declared)
	rekeyed := newVolumeSet()
	for _, v := range declared.ByName {
		if v.Logical == "volPulumiState" {
			v.Logical = "volInfraState"
		}
		rekeyed.add(v)
	}
	recorded, err := RecordedVolumes(exportWith(t, vols, protect, mounts))
	if err != nil {
		t.Fatal(err)
	}
	err = CheckVolumeIdentity(rekeyed, recorded)
	if err == nil {
		t.Fatal("a re-keyed volume resource was accepted")
	}
	for _, want := range []string{"volPulumiState", "volInfraState", "kontra_pulumi-state"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from the refusal:\n%v", want, err)
		}
	}
}

// "still attached to the same service" — the half of the identity that no delete and no rename can
// account for, and the one that comes up healthy.
func TestAVolumeMovedToAnotherServiceIsRefused(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	vols, protect, mounts := mirrorOf(declared)
	moved := newVolumeSet()
	for _, v := range declared.ByName {
		if v.Name == "kontra_seaweed-data" {
			v.Services = []string{"kontra-victorialogs"}
		}
		moved.add(v)
	}
	recorded, err := RecordedVolumes(exportWith(t, vols, protect, mounts))
	if err != nil {
		t.Fatal(err)
	}
	err = CheckVolumeIdentity(moved, recorded)
	if err == nil {
		t.Fatal("a volume moved to another service was accepted — no container fails, no step is " +
			"destructive, and the old contents sit where nothing looks")
	}
	for _, want := range []string{"kontra_seaweed-data", "kontra-seaweed", "kontra-victorialogs", "comes up healthy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from the refusal:\n%v", want, err)
		}
	}
}

// A volume the program adds is ORDINARY — an upgrade may bring one — and must not be refused, or the
// first `kontra update` after any new service is a dead end.
func TestAnAddedVolumeIsNotARefusal(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	vols, protect, mounts := mirrorOf(declared)
	delete(vols, "volRegistryData") // the installation predates that volume
	for svc, names := range mounts {
		var keep []string
		for _, n := range names {
			if n != "kontra_registry-data" {
				keep = append(keep, n)
			}
		}
		mounts[svc] = keep
	}
	recorded, err := RecordedVolumes(exportWith(t, vols, protect, mounts))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckVolumeIdentity(declared, recorded); err != nil {
		t.Fatalf("a volume the program ADDS was treated as a loss:\n%v", err)
	}
}

// --- the plan check ------------------------------------------------------------------------------

// THE DOCUMENT IS THE REAL ONE. Measured on 3.244.0: previewing a program with a protected volume
// deleted out of it exits 1, writes nothing to stderr, and writes this — note `changeSummary` of
// `{"same": 3}` WITH NO DELETE IN IT, which is why WouldChange asks the steps too.
const protectedDeletePreview = `{
  "steps": [
    {"op":"same","urn":"urn:pulumi:local::kontra-control::pulumi:pulumi:Stack::kontra-control-local",
     "oldState":{"type":"pulumi:pulumi:Stack"}},
    {"op":"delete","urn":"urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData",
     "oldState":{"type":"docker:index/volume:Volume"}}
  ],
  "diagnostics": [
    {"urn":"urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData",
     "message":"error: Preview failed: resource \"urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData\" cannot be deleted\nbecause it is protected. To unprotect the resource, either remove the ` + "`protect`" + ` flag from the resource in your Pulumi program and run ` + "`pulumi up`" + `, or use the command:\n` + "`pulumi state unprotect '...'`" + `\n","severity":"error"},
    {"message":"error: preview failed\n","severity":"error"}
  ],
  "changeSummary": {"same": 3}
}`

// A PLANNED DELETE OF A VOLUME IS NAMED, IN KONTRA'S WORDS. Pulumi's own refusal names a URN and
// offers `pulumi state unprotect` as the remedy; the operator needs the docker volume and what is in
// it.
func TestThePlanCheckNamesTheVolumeAndNotTheURN(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ParseSummary([]byte(protectedDeletePreview))
	if err != nil {
		t.Fatal(err)
	}
	if !sum.WouldChange() {
		t.Fatal("a plan whose one step deletes a volume reported no change — `changeSummary` omits the " +
			"delete on a refused preview, which is exactly the measurement WouldChange exists for")
	}
	err = CheckPlanKeepsEveryVolume(sum, declared, VolumeSet{ByName: map[string]Volume{}, ByLogical: map[string]Volume{}})
	if err == nil {
		t.Fatal("a plan deleting a volume was accepted")
	}
	for _, want := range []string{"kontra_seaweed-data", "volSeaweedData", "protect: true", "docker volume rm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from the refusal:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "urn:pulumi") {
		t.Errorf("the refusal leads with a URN, which is the message this function exists to replace:\n%v", err)
	}
}

// AN UNPROTECTED VOLUME IS THE CASE WHERE THIS CHECK IS THE ONLY THING IN THE WAY, and the refusal has
// to say so — pulumi will execute that plan without complaining.
func TestThePlanCheckSaysWhenPulumiWouldNotStopIt(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	sum := &Summary{Counts: map[string]int{"delete": 1}, Total: 1, Changes: []Change{
		{Op: "delete", Name: "volVictoriaLogsData", Type: stateVolumeType},
	}}
	err = CheckPlanKeepsEveryVolume(sum, declared, VolumeSet{ByName: map[string]Volume{}, ByLogical: map[string]Volume{}})
	if err == nil {
		t.Fatal("a planned delete of an unprotected volume was accepted")
	}
	if !strings.Contains(err.Error(), "DELIBERATELY NOT PROTECTED") {
		t.Errorf("the refusal must say that nothing else is in the way for this one:\n%v", err)
	}
}

// A REPLACE COUNTS. §7: "The dangerous case is replacement, not deletion" — and a replaced docker
// volume is destroyed and recreated, so the bytes do not come back.
func TestThePlanCheckCatchesAReplacement(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"replace", "delete-replaced", "create-replacement"} {
		sum := &Summary{Counts: map[string]int{op: 1}, Total: 1, Changes: []Change{
			{Op: op, Name: "volPostgresData", Type: stateVolumeType},
		}}
		if err := CheckPlanKeepsEveryVolume(sum, declared, VolumeSet{}); err == nil {
			t.Errorf("op %q on a volume was accepted", op)
		}
	}
}

// AND A CONTAINER REPLACEMENT IS NOT A REFUSAL. That is what an update IS: thirteen containers
// destroyed and rebuilt while every volume stays. A gate that refused this would make `kontra update`
// impossible, which is the failure that hides behind "refuse anything destructive".
func TestReplacingEveryContainerIsNotARefusal(t *testing.T) {
	declared, err := DeclaredVolumes(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	var changes []Change
	for _, name := range []string{"postgres", "temporal", "redis", "seaweed", "orchestratorApi", "orchestratorInfra"} {
		changes = append(changes, Change{Op: "replace", Name: name, Type: stateContainerType})
	}
	sum := &Summary{Counts: map[string]int{"replace": len(changes)}, Total: 40, Changes: changes}
	if err := CheckPlanKeepsEveryVolume(sum, declared, VolumeSet{}); err != nil {
		t.Fatalf("replacing containers was refused, which is what an update does:\n%v", err)
	}
}

// ── A VOLUME MAY GAIN A READER AND MAY NOT LOSE ONE ───────────────────────────────────────────────
//
// The hazard this gate describes is one-directional: a volume handed to a service that did not write
// it reads as an empty disk there. A service added BESIDE the ones that already mount it cannot
// produce that. Comparing the two lists for equality refused both, which made a read-only reader
// impossible to add without stopping the converge.

func recordedWith(name string, services ...string) VolumeSet {
	s := newVolumeSet()
	s.add(Volume{Logical: "volThing", Name: name, Services: services})
	s.Converged = true
	return s
}

func declaredWith(name string, services ...string) VolumeSet {
	s := newVolumeSet()
	s.add(Volume{Logical: "volThing", Name: name, Services: services})
	return s
}

func TestAVolumeMayGainAReader(t *testing.T) {
	// The real case: `kontra_registry-data` stays on the registry and is mounted read-only into the
	// one-shot that refuses to start an unmigrated install. Nothing is renamed and no bytes move.
	err := CheckVolumeIdentity(
		declaredWith("kontra_registry-data", "kontra-registry", "kontra-registry-config"),
		recordedWith("kontra_registry-data", "kontra-registry"),
	)
	if err != nil {
		t.Fatalf("adding a reader was refused:\n%v", err)
	}
}

func TestAVolumeMayNotLoseAReader(t *testing.T) {
	// The hazard itself, and the direction that must stay refused: the service that wrote it starts on
	// an empty disk while its data sits where nothing looks.
	err := CheckVolumeIdentity(
		declaredWith("kontra_seaweed-data", "kontra-other"),
		recordedWith("kontra_seaweed-data", "kontra-seaweed"),
	)
	if err == nil {
		t.Fatal("a volume taken away from the service that wrote it was accepted")
	}
	for _, want := range []string{"NO LONGER DOES", "kontra-seaweed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q:\n%v", want, err)
		}
	}
}

func TestSwappingOneReaderForAnotherIsStillALoss(t *testing.T) {
	// Equal lengths, different members — the case a length check would wave through.
	err := CheckVolumeIdentity(
		declaredWith("kontra_postgres-data", "kontra-a", "kontra-c"),
		recordedWith("kontra_postgres-data", "kontra-a", "kontra-b"),
	)
	if err == nil {
		t.Fatal("exchanging a mounter for a different one was accepted")
	}
	if !strings.Contains(err.Error(), "kontra-b") {
		t.Errorf("the refusal should name the one it lost:\n%v", err)
	}
}

func TestAnUnconvergedStackHasNothingToLose(t *testing.T) {
	// Nothing recorded is not an install with zero volumes; it is an install that has not happened.
	rec := newVolumeSet()
	rec.add(Volume{Logical: "volThing", Name: "kontra_thing", Services: []string{"kontra-old"}})
	// Converged deliberately left false.
	if err := CheckVolumeIdentity(declaredWith("kontra_thing", "kontra-new"), rec); err != nil {
		t.Fatalf("an unconverged stack was gated:\n%v", err)
	}
}
