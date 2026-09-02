package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fleet CLI's job is to turn flags plus an actor.json into ONE desired state for a Pulumi
// stack. Every test here pins a way that assembly can be wrong without failing: the stack
// converges happily and the fleet is subtly not what was asked for.

func writeActor(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "actor.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const webcrawlManifest = `{
  "schemaVersion": "kontra.actor.v1",
  "name": "webcrawl",
  "version": "0.1.0",
  "targets": {
    "container": {"memory": "3g", "cpus": 2, "shmSize": "1g"},
    "machine": {"size": "s-2vcpu-4gb", "region": "sfo3", "image": "ubuntu-22-04-x64"}
  }
}`

func TestActorJSONSuppliesTheHardware(t *testing.T) {
	// The point of putting hardware in actor.json: the size a crawler needs is a property of
	// the crawler, not of whoever last typed a `fleet up`.
	spec, err := readActorTargets(writeActor(t, webcrawlManifest))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Machine.Size != "s-2vcpu-4gb" || spec.Machine.Region != "sfo3" {
		t.Fatalf("machine target not read: %+v", spec.Machine)
	}
	// `cpus` is a NUMBER in the manifest and a string on the docker flag. Authors must not have
	// to quote it, and it must not arrive as "2.000000".
	if spec.Container.Cpus != "2" {
		t.Fatalf("cpus = %q, want \"2\"", spec.Container.Cpus)
	}
	if spec.Container.Memory != "3g" || spec.Container.ShmSize != "1g" {
		t.Fatalf("container target not read: %+v", spec.Container)
	}
}

func TestActorWithNoTargetsIsNotAnError(t *testing.T) {
	// Most actors declare no hardware; they get the fleet defaults. A missing block must not
	// fail the parse, or every pre-existing actor stops being deployable.
	spec, err := readActorTargets(writeActor(t,
		`{"schemaVersion":"kontra.actor.v1","name":"beacon","version":"1.0.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Machine.Size != "" || spec.Container.Memory != "" {
		t.Fatalf("invented hardware from a manifest that declared none: %+v", spec)
	}
}

// fakeBundle stands in for a real build. Assembling the desired state is a pure function of
// its inputs; making these tests compile a binary and upload 20 MiB to prove that would test
// the network, not the assembly.
func fakeBundle() *bundle {
	return &bundle{
		Name: "webcrawl", Version: "0.1.0", Engine: "py",
		SHA: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
}

func TestUpSendsNoPlacementButDeployDoes(t *testing.T) {
	// `up` brings Machines into existence; `deploy` places an Artifact. One verb never does
	// both (infra/CONTEXT.md) — and since the stack is declarative, an `up` that quietly
	// included placement would make the two indistinguishable in state.
	dir := writeActor(t, webcrawlManifest)

	f := fleetFlagSet("up")
	if err := f.fs.Parse([]string{"--count", "2", "--tag", "crawl", "--actor", dir}); err != nil {
		t.Fatal(err)
	}
	up, err := f.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := up["bundleUrl"]; ok {
		t.Fatal("`fleet up` sent a placement — it must only size Machines")
	}
	if up["size"] != "s-2vcpu-4gb" || up["machines"] != 2 {
		t.Fatalf("up args wrong: %+v", up)
	}

	place, err := f.args(fakeBundle())
	if err != nil {
		t.Fatal(err)
	}
	if place["actorName"] != "webcrawl" || place["actorEngine"] != "py" {
		t.Fatalf("deploy args dropped the actor's identity: %+v", place)
	}
	if place["bundleSha"] != fakeBundle().SHA {
		t.Fatalf("the Machines would verify the wrong sha: %+v", place["bundleSha"])
	}
}

func TestMachinesFetchFromTheControllerNotLocalhost(t *testing.T) {
	// `localhost:5000` is right on the Controller and meaningless on a Machine, which would
	// fetch from its own loopback and fail with a connection error that says nothing about the
	// registry living on another host.
	t.Setenv("KONTRA_REGISTRY", "")
	t.Setenv("KONTRA_CONTROLLER", "")
	dir := writeActor(t, webcrawlManifest)
	f := fleetFlagSet("deploy")
	if err := f.fs.Parse([]string{"--tag", "crawl", "--actor", dir, "--controller", "10.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	a, err := f.args(fakeBundle())
	if err != nil {
		t.Fatal(err)
	}
	want := "http://10.9.9.9:5000/v2/bundles/webcrawl/blobs/sha256:" + fakeBundle().SHA
	if a["bundleUrl"] != want {
		t.Fatalf("bundleUrl = %v, want %v", a["bundleUrl"], want)
	}
	if a["controller"] != "10.9.9.9" {
		t.Fatalf("controller = %v", a["controller"])
	}
	// THE URL AND THE SHA ARE ONE PAIR, and this is where they are minted. `machineInstall` curls
	// the first and checks the second; a placement whose two halves disagree is a 65 MiB download
	// that fails on every Machine at once, which is why `validateMachineActor` refuses it and why
	// this asserts the pairing rather than the two strings separately.
	if !strings.HasSuffix(a["bundleUrl"].(string), a["bundleSha"].(string)) {
		t.Fatalf("bundleUrl %v does not end with bundleSha %v", a["bundleUrl"], a["bundleSha"])
	}
}

func TestExplicitFlagsBeatTheManifest(t *testing.T) {
	// The actor says what it needs; the operator says what it gets.
	dir := writeActor(t, webcrawlManifest)
	f := fleetFlagSet("up")
	if err := f.fs.Parse([]string{"--tag", "crawl", "--actor", dir, "--size", "s-8vcpu-16gb"}); err != nil {
		t.Fatal(err)
	}
	a, err := f.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	if a["size"] != "s-8vcpu-16gb" {
		t.Fatalf("size = %v, want the flag to win", a["size"])
	}
}

func TestContainerLimitsAreNoLongerSentToAFleet(t *testing.T) {
	// Nothing on a fleet Machine runs in a container any more. actor.json still declares
	// container limits — a Worker run locally with `docker run` uses them — but sending them
	// to a Fleet would describe a Target that no longer exists there.
	dir := writeActor(t, webcrawlManifest)
	f := fleetFlagSet("deploy")
	if err := f.fs.Parse([]string{"--tag", "crawl", "--actor", dir}); err != nil {
		t.Fatal(err)
	}
	a, err := f.args(fakeBundle())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"containerMemory", "containerShmSize", "containerCpus", "actorImage"} {
		if _, ok := a[k]; ok {
			t.Fatalf("fleet args still carry %q — the container Target is gone from the fleet", k)
		}
	}
}

func TestRoleIsBoundedButNotAnEnum(t *testing.T) {
	// A hardcoded crawl|bust list meant every new actor had to edit this file or mislabel its
	// infrastructure. What matters is that the value is safe to interpolate into a tag, a
	// group name and a machine name.
	for _, ok := range []string{"crawl", "bust", "leaks", "desync", "a1", "x-y-z"} {
		if err := validTag(ok); err != nil {
			t.Fatalf("validTag(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "A", "1abc", "has space", "has_underscore", "x", "../etc"} {
		if err := validTag(bad); err == nil {
			t.Fatalf("validTag(%q) accepted", bad)
		}
	}
}

func TestFqnIsTheStackAndTheWorkflowId(t *testing.T) {
	f := fleetFlagSet("up")
	if err := f.fs.Parse([]string{"--fleet", "playwright"}); err != nil {
		t.Fatal(err)
	}
	if got := f.fqn(); got != "kontra-fleet/playwright" {
		t.Fatalf("fqn = %q", got)
	}
}

func TestProgressLineRendersTheResourceNotTheUrn(t *testing.T) {
	// The engine reports a full URN per resource; the tail is the only part an operator reads.
	got := progressLine(map[string]any{
		"op":  "create",
		"urn": "urn:pulumi:playwright::kontra-fleet::digitalocean:index/droplet:Droplet::kf-crawl-01",
	})
	if got != "create kf-crawl-01" {
		t.Fatalf("progressLine = %q", got)
	}
	if progressLine(nil) != "" {
		t.Fatal("an empty heartbeat must render nothing, not a blank line")
	}
}

// Density is a NUMBER on a surface that is otherwise strings, and it reaches the Machines only
// through the desired state assembled here. Both ways of getting it wrong are silent: omitted, a
// Machine quietly runs at the host default of 4; sent as 0, the declarative program reads a
// request to REMOVE the cap.
func TestSessionsIsSentOnlyWhenAskedFor(t *testing.T) {
	f := fleetFlagSet("deploy")
	if err := f.fs.Parse([]string{"--tag", "dns", "--count", "4"}); err != nil {
		t.Fatal(err)
	}
	quiet, err := f.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := quiet["maxSessions"]; ok {
		t.Fatal("an unset --sessions must be ABSENT, not 0: 0 is a request to remove the cap")
	}

	g := fleetFlagSet("deploy")
	if err := g.fs.Parse([]string{"--tag", "dns", "--count", "4", "--sessions", "12"}); err != nil {
		t.Fatal(err)
	}
	loud, err := g.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	if loud["maxSessions"] != 12 {
		t.Fatalf("maxSessions = %v, want 12 — density never reaches worker.env otherwise", loud["maxSessions"])
	}
}

// `TestBundlePointerKeyIsTheContract` STOOD HERE, and it is deleted rather than translated.
//
// It pinned `<name>/<version>/latest.json` against a literal, while `backend/src/activities/
// fleet.test.ts` pinned the same layout against a second literal that did not know about it —
// the hand-copied golden `shared/conformance/README.md` describes. The contract survives and is
// stronger, because it now covers the whole address on both sides at once:
// `shared/conformance/bundleref.json`, driven from `cli/bundleref_conformance_test.go` here and
// `backend/src/activities/bundleref.conformance.test.ts` there.
