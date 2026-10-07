package warden

// warden_images_test.go — the prune's DECISION, held as a pure function.
//
// WHY THESE TESTS AND NOT A LIVE RUNTIME. The failure this guards is unrecoverable and asymmetric: a
// prune that removes a PLACED image stops a Worker on a Machine that cannot re-provision itself
// (ADR 0037), and the fix needs the network, the registry and a credential the Machine may not have.
// Every other outcome — a prune that removes too little, one that refuses, one that cannot read the
// disk — costs disk space and nothing else. So the invariant worth a test is "a placed image is never
// in Remove", and it is worth testing against inputs a test can STATE rather than inputs a container
// runtime happens to produce: five digests of one actor, one of them placed and oldest, is two lines
// here and a ten-minute fixture there.
//
// The engine-shaped tests (`TestImageFactsReadBothEngines`) exist for the one thing a pure function
// cannot cover: podman and docker describe an image with the same field NAMES and two different
// spellings of the id, and a parser that silently dropped one would make every image unlabelled and
// every prune a no-op — which looks exactly like a Machine with nothing to reclaim.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// --- fixtures -------------------------------------------------------------------------------------

// digestOf is a deterministic, well-formed sha256 for a test to name an image by.
func digestOf(n int) string {
	return "sha256:" + strings.Repeat(fmt.Sprintf("%x", n&0xf), 64)
}

// labelled is one image as the engine would hold it after this Warden named it: pulled by digest, so
// the kontra tag is its only name.
func labelled(t *testing.T, actor, version string, n int, created time.Time, size int64) imageFact {
	t.Helper()
	digest := digestOf(n)
	tag, err := imageTagFor(actor, version, digest)
	if err != nil {
		t.Fatalf("imageTagFor(%q, %q): %v", actor, version, err)
	}
	actorBack, versionBack, short, ok := imageTagRead(tag)
	if !ok || actorBack != actor || versionBack != version {
		t.Fatalf("%q did not read back as %s@%s", tag, actor, version)
	}
	return imageFact{
		ID:      strings.TrimPrefix(digest, "sha256:")[:12] + strings.Repeat("0", 52),
		Digests: []string{"localhost:5000/actors/" + actor + "@" + digest},
		Names:   []string{tag},
		Actor:   actor,
		Version: version,
		Short:   short,
		Tag:     tag,
		Size:    size,
		Created: created,
	}
}

func placement(actor, version string, n int) placedImage {
	return placedImage{Actor: actor, Version: version, Digest: digestOf(n)}
}

// ready is an input that would prune: this Warden owns the store, an assignment exists, the timer has
// never run, the disk is fine.
func ready(images []imageFact, placements []placedImage) pruneInput {
	return pruneInput{
		Now:            time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Every:          pruneEvery,
		FreePct:        80,
		HaveFree:       true,
		FloorPct:       pruneDiskFloorPct,
		HaveAssignment: true,
		Exclusive:      true,
		Placements:     placements,
		Images:         images,
		KeepPerActor:   pruneKeepPerActor,
	}
}

func removedShorts(v pruneVerdict) []string {
	out := make([]string, 0, len(v.Remove))
	for _, f := range v.Remove {
		out = append(out, f.Short)
	}
	return out
}

func keptBy(v pruneVerdict, reason string) []string {
	out := []string{}
	for _, k := range v.Keep {
		if k.Reason == reason {
			out = append(out, k.Image.Short)
		}
	}
	return out
}

// --- acceptance test 15 ----------------------------------------------------------------------------

// §13.15, verbatim: "On a Machine with 5 digests of one actor, one of them placed, a prune keeps the
// placed digest and the 2 most recent, and removes the others."
//
// THE PLACED ONE IS THE OLDEST, which is the only arrangement that distinguishes the rule from "keep
// the 3 most recent". With the placement anywhere in the newest two, every keep-N policy passes.
func TestPruneKeepsThePlacedDigestAndTheTwoMostRecent(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	images := []imageFact{
		labelled(t, "beacon", "0.5.0", 5, base.Add(5*time.Hour), 900<<20), // newest
		labelled(t, "beacon", "0.4.0", 4, base.Add(4*time.Hour), 900<<20),
		labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 900<<20),
		labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 900<<20),
		labelled(t, "beacon", "0.1.0", 1, base.Add(1*time.Hour), 900<<20), // oldest, and PLACED
	}
	v := prunePlan(ready(images, []placedImage{placement("beacon", "0.1.0", 1)}))

	if !v.Run {
		t.Fatalf("the prune did not run: %s", v.Reason)
	}
	if got := keptBy(v, keepPlaced); len(got) != 1 || got[0] != images[4].Short {
		t.Errorf("kept as placed: %v; want exactly the placed digest %s", got, images[4].Short)
	}
	if got, want := keptBy(v, keepRecent), []string{images[0].Short, images[1].Short}; !sameSet(got, want) {
		t.Errorf("kept as recent: %v; want the two newest %v", got, want)
	}
	if got, want := removedShorts(v), []string{images[2].Short, images[3].Short}; !sameSet(got, want) {
		t.Errorf("removed: %v; want the three-in-the-middle minus the placed one, %v", got, want)
	}
	if want := int64(2 * (900 << 20)); v.removeBytes() != want {
		t.Errorf("removeBytes = %d; want %d", v.removeBytes(), want)
	}
}

// --- the invariant that matters --------------------------------------------------------------------

// A PLACED IMAGE IS NEVER REMOVED, whatever else is true. This is the one failure that cannot be undone
// from the Machine, so it is asserted over the whole cross product of the things that could tempt a
// prune into it: the placement being the oldest of many, the disk being nearly full, the keep budget
// being zero, the placement being matched only by its short digest because the engine kept no repo
// digest, and the actor's name carrying a slash.
func TestPruneNeverRemovesAPlacedImage(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	noRepoDigest := labelled(t, "beacon", "0.1.0", 1, base, 10)
	noRepoDigest.Digests = nil // loaded from a tar: the kontra tag's short digest is all there is

	slashed := labelled(t, "a/b", "0.1.0", 2, base, 10)

	cases := []struct {
		name  string
		in    pruneInput
		watch imageFact
	}{
		{
			name: "oldest of six, keep budget 2",
			in: ready([]imageFact{
				labelled(t, "beacon", "0.6.0", 6, base.Add(6*time.Hour), 10),
				labelled(t, "beacon", "0.5.0", 5, base.Add(5*time.Hour), 10),
				labelled(t, "beacon", "0.4.0", 4, base.Add(4*time.Hour), 10),
				labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
				labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
				labelled(t, "beacon", "0.1.0", 1, base.Add(1*time.Hour), 10),
			}, []placedImage{placement("beacon", "0.1.0", 1)}),
			watch: labelled(t, "beacon", "0.1.0", 1, base.Add(1*time.Hour), 10),
		},
		{
			name: "keep budget zero",
			in: func() pruneInput {
				in := ready([]imageFact{labelled(t, "beacon", "0.1.0", 1, base, 10)},
					[]placedImage{placement("beacon", "0.1.0", 1)})
				in.KeepPerActor = 0
				return in
			}(),
			watch: labelled(t, "beacon", "0.1.0", 1, base, 10),
		},
		{
			name: "disk at 1% free",
			in: func() pruneInput {
				in := ready([]imageFact{
					labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
					labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
					labelled(t, "beacon", "0.1.0", 1, base.Add(time.Hour), 10),
				}, []placedImage{placement("beacon", "0.1.0", 1)})
				in.FreePct = 1
				return in
			}(),
			watch: labelled(t, "beacon", "0.1.0", 1, base.Add(time.Hour), 10),
		},
		{
			name: "matched only by the tag's short digest",
			in: ready([]imageFact{
				noRepoDigest,
				labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
				labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
			}, []placedImage{placement("beacon", "0.1.0", 1)}),
			watch: noRepoDigest,
		},
		{
			name: "an actor whose name carries a slash",
			in: ready([]imageFact{
				slashed,
				labelled(t, "a/b", "0.4.0", 4, base.Add(4*time.Hour), 10),
				labelled(t, "a/b", "0.3.0", 3, base.Add(3*time.Hour), 10),
			}, []placedImage{placement("a/b", "0.1.0", 2)}),
			watch: slashed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := prunePlan(c.in)
			for _, f := range v.Remove {
				if f.Short == c.watch.Short && f.Actor == c.watch.Actor {
					t.Fatalf("the prune would remove %s@%s %s, which is PLACED on this Machine — a "+
						"Worker stops and the Machine cannot re-provision itself to get it back",
						f.Actor, f.Version, f.Short)
				}
			}
			// CONTROL: the plan is not vacuous. A decision that removed nothing at all would pass the
			// assertion above for the wrong reason, which is how this class of test rots.
			if len(v.Remove) == 0 && len(c.in.Images) > c.in.KeepPerActor+1 {
				t.Fatalf("nothing was removed from %d images with a keep budget of %d, so the assertion "+
					"above proved nothing", len(c.in.Images), c.in.KeepPerActor)
			}
			if got := keptBy(v, keepPlaced); len(got) != 1 {
				t.Errorf("kept as placed: %v; want exactly one", got)
			}
		})
	}
}

// --- blocker 35: the first prune after an upgrade --------------------------------------------------

// NOTHING ON THE MACHINE CARRIES A KONTRA NAME UNTIL THIS CHANGE HAS RUN, so the first prune after an
// upgrade sees a store it has no name for. The two wrong answers are symmetrical and both are real
// temptations: remove everything unlabelled (which deletes `kontra-host:1`, the base image every actor
// build needs, and the Machine's whole pulled store), or treat an unlabelled store as a reason to
// refuse forever. The rule is neither — unlabelled is KEPT, by name, and counted so an operator can see
// why a prune reclaimed nothing.
func TestPruneRemovesNothingWhenNothingIsLabelledYet(t *testing.T) {
	unlabelled := []imageFact{
		{ID: "aaaa000000000000", Names: []string{"kontra-host:1"}, Size: 2 << 30, Created: time.Now()},
		{ID: "bbbb000000000000", Names: []string{"localhost:5000/actors/beacon:0.1.0"}, Size: 900 << 20,
			Digests: []string{"localhost:5000/actors/beacon@" + digestOf(1)}, Created: time.Now()},
		{ID: "cccc000000000000", Size: 400 << 20, Created: time.Now()}, // dangling
	}
	v := prunePlan(ready(unlabelled, []placedImage{placement("beacon", "0.1.0", 1)}))

	if !v.Run {
		t.Fatalf("the prune did not run, so this says nothing about what it would have removed: %s", v.Reason)
	}
	if len(v.Remove) != 0 {
		t.Fatalf("the first prune would remove %v; an image with no kontra name is one this Warden did "+
			"not pull and must not delete", removedShorts(v))
	}
	count, bytes := v.kept(keepUnlabelled)
	if count != len(unlabelled) {
		t.Errorf("kept %d unlabelled image(s); want all %d, each with the reason spelled out", count, len(unlabelled))
	}
	if want := int64(2<<30) + int64(900<<20) + int64(400<<20); bytes != want {
		t.Errorf("unlabelled bytes = %d; want %d — the number an operator needs to see why nothing was freed",
			bytes, want)
	}
}

// AN IMAGE WHOSE AGE THE ENGINE WILL NOT STATE CANNOT BE RANKED BY AGE, so it is kept rather than
// treated as the oldest. The zero time sorting last would make an unreadable timestamp the FIRST thing
// removed, which is the opposite of what not knowing should buy.
func TestPruneKeepsAnImageWithNoCreationTime(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mystery := labelled(t, "beacon", "0.1.0", 1, time.Time{}, 10)
	images := []imageFact{
		mystery,
		labelled(t, "beacon", "0.4.0", 4, base.Add(4*time.Hour), 10),
		labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
	}
	v := prunePlan(ready(images, nil))

	for _, f := range v.Remove {
		if f.Short == mystery.Short {
			t.Fatalf("removed %s, whose creation time the engine did not report", f.Short)
		}
	}
	if got := keptBy(v, keepUnreadable); len(got) != 1 || got[0] != mystery.Short {
		t.Errorf("kept with no creation time: %v; want %s", got, mystery.Short)
	}
	// CONTROL: the two it CAN rank are still ranked, so the rule above is a carve-out and not an
	// accidental "keep everything".
	if len(v.Remove) != 1 || v.Remove[0].Short != images[3].Short {
		t.Errorf("removed %v; want only the oldest datable image %s", removedShorts(v), images[3].Short)
	}
}

// --- the policy under pressure ---------------------------------------------------------------------

// LOW DISK CHANGES WHEN, NEVER WHAT. A Machine that deleted more when it was short of space would
// delete the digest a rollback needs at exactly the moment somebody is most likely to want one, and it
// would do it without being asked. So the two plans are compared field for field and only `Run` and
// `Reason` may differ.
func TestPruneKeepsTheSamePolicyUnderDiskPressure(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	images := []imageFact{
		labelled(t, "beacon", "0.4.0", 4, base.Add(4*time.Hour), 10),
		labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
		labelled(t, "beacon", "0.1.0", 1, base.Add(time.Hour), 10),
	}
	placements := []placedImage{placement("beacon", "0.1.0", 1)}

	roomy := ready(images, placements)
	roomy.FreePct, roomy.LastPrune = 90, roomy.Now.Add(-7*time.Hour)

	tight := ready(images, placements)
	tight.FreePct, tight.LastPrune = 3, tight.Now.Add(-time.Minute) // nowhere near the timer

	a, b := prunePlan(roomy), prunePlan(tight)
	if !a.Run || !b.Run {
		t.Fatalf("both should run: roomy=%v (%s) tight=%v (%s)", a.Run, a.Reason, b.Run, b.Reason)
	}
	if !sameSet(removedShorts(a), removedShorts(b)) {
		t.Errorf("a prune under disk pressure removes %v and one on the timer removes %v; the policy "+
			"must not change under pressure", removedShorts(b), removedShorts(a))
	}
	if !sameSet(keptBy(a, keepPlaced), keptBy(b, keepPlaced)) || !sameSet(keptBy(a, keepRecent), keptBy(b, keepRecent)) {
		t.Errorf("the keep sets differ: timer kept %v/%v, pressure kept %v/%v",
			keptBy(a, keepPlaced), keptBy(a, keepRecent), keptBy(b, keepPlaced), keptBy(b, keepRecent))
	}
	if !strings.Contains(b.Reason, "floor") {
		t.Errorf("the low-disk prune's reason is %q; it should name the floor that fired", b.Reason)
	}
}

// --- the trigger ----------------------------------------------------------------------------------

func TestPruneDue(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   pruneInput
		want bool
		says string
	}{
		{
			name: "no assignment, so nothing may be removed",
			in:   pruneInput{Now: now, Every: pruneEvery, HaveFree: true, FreePct: 1, FloorPct: 15},
			want: false,
			says: "no assignment",
		},
		{
			name: "never pruned, so now",
			in:   pruneInput{Now: now, Every: pruneEvery, HaveAssignment: true, HaveFree: true, FreePct: 80, FloorPct: 15},
			want: true,
			says: "has not pruned yet",
		},
		{
			name: "the timer has not elapsed",
			in: pruneInput{Now: now, LastPrune: now.Add(-5 * time.Hour), Every: pruneEvery,
				HaveAssignment: true, HaveFree: true, FreePct: 80, FloorPct: 15},
			want: false,
			says: "next prune is in",
		},
		{
			name: "the timer has elapsed",
			in: pruneInput{Now: now, LastPrune: now.Add(-6*time.Hour - time.Minute), Every: pruneEvery,
				HaveAssignment: true, HaveFree: true, FreePct: 80, FloorPct: 15},
			want: true,
			says: "timer is 6h",
		},
		{
			// THE FLOOR ONLY BRINGS A PRUNE FORWARD WHERE A PRUNE CAN RECLAIM SOMETHING, so this case
			// carries `Exclusive` — every other field was already right.
			name: "under the floor, well inside the timer",
			in: pruneInput{Now: now, LastPrune: now.Add(-time.Minute), Every: pruneEvery,
				HaveAssignment: true, Exclusive: true, HaveFree: true, FreePct: 14.9, FloorPct: 15},
			want: true,
			says: "floor",
		},
		{
			// …AND THE SAME BREACH ON A SHARED STORE MUST NOT. Nothing there is removable, so the
			// breach could never clear — and this arm is read on every five-second turn, which would
			// make a full-disk Machine do a full store read and a log line every five seconds, for
			// ever, reclaiming nothing each time.
			name: "under the floor on a store this Warden shares",
			in: pruneInput{Now: now, LastPrune: now.Add(-time.Minute), Every: pruneEvery,
				HaveAssignment: true, Exclusive: false, HaveFree: true, FreePct: 3, FloorPct: 15},
			want: false,
			says: "next prune is in",
		},
		{
			name: "exactly at the floor is not under it",
			in: pruneInput{Now: now, LastPrune: now.Add(-time.Minute), Every: pruneEvery,
				HaveAssignment: true, HaveFree: true, FreePct: 15, FloorPct: 15},
			want: false,
			says: "next prune is in",
		},
		{
			// UNREADABLE IS NOT FULL. A disk nobody could measure must not make every turn a prune turn.
			name: "an unreadable disk falls back to the timer",
			in: pruneInput{Now: now, LastPrune: now.Add(-time.Minute), Every: pruneEvery,
				HaveAssignment: true, HaveFree: false, FreePct: 0, FloorPct: 15},
			want: false,
			says: "next prune is in",
		},
		{
			name: "the timer off leaves only the disk arm",
			in: pruneInput{Now: now, LastPrune: now.Add(-100 * time.Hour), Every: 0,
				HaveAssignment: true, HaveFree: true, FreePct: 80, FloorPct: 15},
			want: false,
			says: "timer is off",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := pruneDue(c.in)
			if got != c.want {
				t.Errorf("pruneDue = %v (%s); want %v", got, why, c.want)
			}
			if !strings.Contains(why, c.says) {
				t.Errorf("reason %q does not mention %q", why, c.says)
			}
		})
	}
}

// AN ASSIGNMENT WITH NO WORKERS IS NOT THE SAME AS NO ASSIGNMENT. A Machine the control plane has
// emptied should reclaim down to the recency budget; a Machine that cannot ASK must not reclaim at all.
// The two are one boolean apart and the difference is the whole safety property.
func TestPruneDistinguishesAnEmptyAssignmentFromNone(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	images := []imageFact{
		labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
		labelled(t, "beacon", "0.1.0", 1, base.Add(time.Hour), 10),
	}

	empty := ready(images, nil)
	if v := prunePlan(empty); !v.Run || len(v.Remove) != 1 {
		t.Errorf("an empty assignment: run=%v removed=%v (%s); want a prune down to the 2 most recent",
			v.Run, removedShorts(v), v.Reason)
	}

	none := ready(images, nil)
	none.HaveAssignment = false
	if v := prunePlan(none); v.Run {
		t.Errorf("a Warden with no assignment pruned: %s", v.Reason)
	}
}

// --- per-actor grouping ----------------------------------------------------------------------------

// THE BUDGET IS PER ACTOR AND THE ACTORS DO NOT COMPETE. A plan that ranked the whole store by recency
// would keep the two newest images on the Machine and delete every other actor's current one.
func TestPruneBudgetsEachActorSeparately(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	images := []imageFact{
		// beacon is newer than everything of layer-scan, so a global ranking would eat layer-scan whole.
		labelled(t, "beacon", "0.3.0", 3, base.Add(30*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, base.Add(29*time.Hour), 10),
		labelled(t, "beacon", "0.1.0", 1, base.Add(28*time.Hour), 10),
		labelled(t, "layer-scan", "0.3.0", 6, base.Add(3*time.Hour), 10),
		labelled(t, "layer-scan", "0.2.0", 5, base.Add(2*time.Hour), 10),
		labelled(t, "layer-scan", "0.1.0", 4, base.Add(time.Hour), 10),
	}
	v := prunePlan(ready(images, nil))

	byActor := map[string]int{}
	for _, k := range v.Keep {
		byActor[k.Image.Actor]++
	}
	for _, actor := range []string{"beacon", "layer-scan"} {
		if byActor[actor] != pruneKeepPerActor {
			t.Errorf("kept %d image(s) of %s; want %d — the budget is per actor", byActor[actor], actor, pruneKeepPerActor)
		}
	}
	if len(v.Remove) != 2 {
		t.Errorf("removed %v; want the oldest of each actor", removedShorts(v))
	}
}

// TWO IMAGES MADE IN THE SAME INSTANT ARE A REAL THING — a rebase rewrites a config and keeps the
// layers — so the plan has to be the same plan twice. A tie broken by map order would remove a
// different image on every turn, which is the kind of bug that only shows up as a missing image.
func TestPrunePlanIsDeterministicWhenCreationTimesTie(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	images := []imageFact{
		labelled(t, "beacon", "0.1.0", 1, at, 10),
		labelled(t, "beacon", "0.2.0", 2, at, 10),
		labelled(t, "beacon", "0.3.0", 3, at, 10),
		labelled(t, "beacon", "0.4.0", 4, at, 10),
	}
	first := removedShorts(prunePlan(ready(images, nil)))
	for i := 0; i < 20; i++ {
		if got := removedShorts(prunePlan(ready(images, nil))); !sameOrder(got, first) {
			t.Fatalf("run %d removed %v; the first run removed %v", i, got, first)
		}
	}
	if len(first) != 2 {
		t.Errorf("removed %v; want 2 of 4", first)
	}
}

// --- the tag --------------------------------------------------------------------------------------

// THE LABELS ARE A TAG, SO THE TAG HAS TO ROUND-TRIP OR THE LABELS DO NOT EXIST. An actor whose name
// carries a slash is in `shared/conformance/queues.json` on purpose, and reading the digest from the
// LEFT would turn `a/b` into `a` — the same bug driver.go's `parseWorkerLabel` is written from the
// right to avoid, in the same package, for the same reason.
func TestImageTagRoundTrips(t *testing.T) {
	ok := []struct{ actor, version string }{
		{"beacon", "0.2.0"},
		{"a/b", "1.0.0"},
		{"layer-scan", "1.0.0-rc1"},
		{"layer-scan", "1.0.0-RC1"}, // uppercase is legal in a TAG and not in a path component
		{"deep/nested/actor", "2026.10.07"},
	}
	for _, c := range ok {
		t.Run(c.actor+"@"+c.version, func(t *testing.T) {
			ref, err := imageTagFor(c.actor, c.version, digestOf(7))
			if err != nil {
				t.Fatalf("imageTagFor: %v", err)
			}
			actor, version, short, got := imageTagRead(ref)
			if !got {
				t.Fatalf("%q did not read back at all", ref)
			}
			if actor != c.actor || version != c.version {
				t.Errorf("%q read back as %s@%s; want %s@%s", ref, actor, version, c.actor, c.version)
			}
			if want := strings.TrimPrefix(digestOf(7), "sha256:")[:imageTagDigestLen]; short != want {
				t.Errorf("%q read back short digest %q; want %q", ref, short, want)
			}
		})
	}

	// REFUSED, NOT SANITISED. These are the Actors `queues.json` carries to prove a queue name is not
	// sanitised, and OCI cannot name any of them. The consequence is the safe one and is tested below.
	refused := []struct{ actor, version, why string }{
		{"café", "0.1.0", "a path component is ASCII"},
		{"my actor", "0.1.0", "a path component has no spaces"},
		{"Foo", "0.1.0", "a path component is lowercase"},
		{"beacon", "1:2", "a tag has no colon"},
		{"beacon", "", "a tag is not empty"},
	}
	for _, c := range refused {
		t.Run("refuses "+c.actor+"@"+c.version, func(t *testing.T) {
			if ref, err := imageTagFor(c.actor, c.version, digestOf(7)); err == nil {
				t.Fatalf("imageTagFor produced %q; %s", ref, c.why)
			}
		})
	}

	// A DIGEST IS REQUIRED, because the digest is what keeps a rebase from stealing the name off the
	// previous digest of the same version.
	for _, bad := range []string{"", "latest", "sha256:deadbeef", "md5:" + strings.Repeat("a", 32)} {
		if ref, err := imageTagFor("beacon", "0.1.0", bad); err == nil {
			t.Errorf("imageTagFor(…, %q) produced %q; only a sha256 manifest digest names an image", bad, ref)
		}
	}
}

// AN UNNAMEABLE ACTOR'S IMAGES ARE KEPT, NOT GUESSED AT. `café` can never carry a kontra tag, so its
// images are unlabelled forever — and unlabelled is kept. The cost is disk; the alternative is a prune
// acting on a name it invented.
func TestPruneKeepsImagesOfAnActorItCannotName(t *testing.T) {
	if _, err := imageTagFor("café", "0.1.0", digestOf(1)); err == nil {
		t.Fatal("café is nameable after all, so this test is about nothing")
	}
	images := []imageFact{
		{ID: "aaaa", Names: []string{"localhost:5000/actors/cafe:0.1.0"}, Size: 10, Created: time.Now()},
		{ID: "bbbb", Names: []string{"localhost:5000/actors/cafe:0.2.0"}, Size: 10, Created: time.Now()},
		{ID: "cccc", Names: []string{"localhost:5000/actors/cafe:0.3.0"}, Size: 10, Created: time.Now()},
	}
	if v := prunePlan(ready(images, nil)); len(v.Remove) != 0 {
		t.Errorf("removed %d image(s) of an actor this Warden cannot name", len(v.Remove))
	}
}

// Nothing but this Warden's own naming scheme is read as a kontra label. A tag that merely looks
// similar must not make somebody else's image prunable.
func TestImageTagReadRefusesEverythingElse(t *testing.T) {
	for _, ref := range []string{
		"",
		"kontra-host:1",
		"localhost:5000/actors/beacon:0.1.0",
		imageTagRepo,                          // no actor, no tag
		imageTagRepo + "/beacon:0.1.0",        // no short digest
		imageTagRepo + "/beacon/abc:0.1.0",    // short digest the wrong length
		imageTagRepo + "/beacon/0123456789ab", // no tag
		"localhost/dev.kontra.actors/b/0123456789ab:1", // not this repository
	} {
		if _, _, _, ok := imageTagRead(ref); ok {
			t.Errorf("imageTagRead(%q) claimed it was a kontra label", ref)
		}
	}
}

// --- two engines, one parser ----------------------------------------------------------------------

// MEASURED FIELD NAMES, FROM BOTH ENGINES ON THIS BOX. podman 4.3.1 writes `Id` as bare hex and docker
// 24.0.5 writes it with the `sha256:` prefix; everything else agrees. A parser that handled one would
// make every image on the other look unlabelled, and a prune that removes nothing is indistinguishable
// from a Machine with nothing to remove.
func TestImageFactsReadBothEngines(t *testing.T) {
	tag, err := imageTagFor("beacon", "0.2.0", digestOf(3))
	if err != nil {
		t.Fatal(err)
	}
	docker := `[{"Id":"sha256:` + strings.Repeat("b", 64) + `",
	  "RepoTags":["` + tag + `"],
	  "RepoDigests":["localhost:5000/actors/beacon@` + digestOf(3) + `"],
	  "Created":"2026-10-06T22:49:20.944114351Z","Size":7807590}]`
	podman := `[{"Id":"` + strings.Repeat("b", 64) + `",
	  "RepoTags":["` + tag + `"],
	  "RepoDigests":["localhost:5000/actors/beacon@` + digestOf(3) + `"],
	  "Created":"2026-10-06T22:49:20.944114351Z","Size":7807590}]`

	for name, raw := range map[string]string{"docker": docker, "podman": podman} {
		t.Run(name, func(t *testing.T) {
			var rows []imageInspect
			if err := json.Unmarshal([]byte(raw), &rows); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows", len(rows))
			}
			f := rows[0].fact()
			if f.ID != strings.Repeat("b", 64) {
				t.Errorf("ID = %q; want bare hex on both engines", f.ID)
			}
			if f.Actor != "beacon" || f.Version != "0.2.0" || f.Tag != tag {
				t.Errorf("read back %s@%s tag=%q; want beacon@0.2.0 tag=%q", f.Actor, f.Version, f.Tag, tag)
			}
			if !f.labelled() {
				t.Error("the image is not labelled, so a prune would never touch it")
			}
			if !f.soleName() {
				t.Error("the kontra tag is the only name, so removing it may delete the image")
			}
			if f.Created.IsZero() {
				t.Error("the creation time did not parse, so the image cannot be ranked by age")
			}
			if f.Size != 7807590 {
				t.Errorf("Size = %d", f.Size)
			}
			if len(f.Digests) != 1 {
				t.Errorf("Digests = %v", f.Digests)
			}
		})
	}
}

// AN IMAGE SOMEBODY ELSE ALSO NAMED IS NOT DELETED BY ID. Measured on this box: `docker rmi <id>` on an
// image whose last remaining name is an operator's own untags AND deletes it, so `soleName` is what
// stands between a prune and somebody else's image.
func TestSoleNameIsFalseWhenAnotherNameExists(t *testing.T) {
	tag, err := imageTagFor("beacon", "0.2.0", digestOf(3))
	if err != nil {
		t.Fatal(err)
	}
	f := imageFact{Tag: tag, Names: []string{tag, "my-own-build:wip"}}
	if f.soleName() {
		t.Error("soleName said yes while an operator's own name is on the image")
	}
	if !(imageFact{Tag: tag, Names: []string{tag}}).soleName() {
		t.Error("soleName said no for an image the kontra tag alone names")
	}
}

// --- the loop's side -------------------------------------------------------------------------------

// A WARDEN WITH NO IMAGE STORE RECLAIMS NOTHING AND SAYS NOTHING. `process` pulls no image at all
// (ADR 0036), and every test in this package builds a Warden by hand — so the nil case has to be the
// quiet one, or the whole suite acquires a thing that shells out to an engine.
func TestPruneIsAbsentWithoutAnImageStore(t *testing.T) {
	drv := &ProcessDriver{}
	if p := newImagePrune(drv); p != nil {
		t.Errorf("the process driver was given an image store: %+v", p)
	}
	w := &warden{driver: drv, now: time.Now}
	// Must not panic and must not reach an engine.
	w.pruneImages(context.Background(), []Spec{{Name: "a", Version: "1", Image: "r/a@" + digestOf(1)}})
	w.labelImage(Spec{Name: "a", Version: "1", Image: "r/a@" + digestOf(1)})

	for _, d := range []workerDriver{&podmanDriver{bin: "podman"}, &dockerDriver{bin: "docker"}} {
		if newImagePrune(d) == nil {
			t.Errorf("the %s driver has an image store and was not given one", d.driverName())
		}
	}
}

// --- what the status block says --------------------------------------------------------------------

// `kontra warden status` IS THE ONLY PLACE PRUNE STATS ARE REPORTED, so the lines it produces are
// asserted rather than left to be read. In particular: a status that could not learn what is placed
// says so, instead of listing placed images as removable.
func TestImageStatusWithoutAnAssignmentNamesNothingRemovable(t *testing.T) {
	lines := imageStatus(context.Background(), &ProcessDriver{}, nil, false)
	if len(lines) != 1 || !strings.Contains(lines[0], "no image store") {
		t.Errorf("the process driver's status is %v; it has no image store to report on", lines)
	}

	// The decision underneath it, which is what the block renders. WITHOUT PLACEMENTS THERE IS NO
	// REMOVAL LIST: every named image would rank as unplaced, so a plan that still listed removals would
	// be listing the placed ones — and the status block's "N removable" would be inviting an operator to
	// stop their own Workers.
	images := []imageFact{
		labelled(t, "beacon", "0.3.0", 3, time.Now().Add(-3*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, time.Now().Add(-2*time.Hour), 10),
		labelled(t, "beacon", "0.1.0", 1, time.Now().Add(-time.Hour), 10),
	}
	in := ready(images, nil)
	in.HaveAssignment = false
	v := prunePlan(in)
	if v.Run {
		t.Error("the plan would run without an assignment")
	}
	if len(v.Remove) != 0 {
		t.Errorf("the plan names %v as removable while it cannot say what is placed here", removedShorts(v))
	}
	if got, want := keptBy(v, keepUnassigned), []string{images[0].Short, images[1].Short, images[2].Short}; !sameSet(got, want) {
		t.Errorf("kept as unassigned: %v; want every named image %v", got, want)
	}
	if v.removeBytes() != 0 {
		t.Errorf("removeBytes = %d; a status that cannot see the placements has no byte figure to print",
			v.removeBytes())
	}
	if !strings.Contains(v.Reason, "no assignment") {
		t.Errorf("reason %q should say why nothing will happen", v.Reason)
	}
	// CONTROL: the same three images WITH an assignment do produce a removal, so the assertions above
	// are about the missing assignment and not about a decision that removes nothing ever.
	if told := prunePlan(ready(images, nil)); len(told.Remove) != 1 {
		t.Fatalf("with an assignment the plan removes %v; want the one image past the recency budget, so "+
			"the no-assignment case above is not vacuous", removedShorts(told))
	}
}

// THE RENDERED BLOCK IS THE THING AN OPERATOR ACTS ON, so the LINES are asserted and not only the
// decision underneath them: "N removable" next to "the Controller did not answer" is a number nobody
// should be given, whatever the plan says.
func TestImageStatusLinesRefuseToCountRemovablesItCannotSee(t *testing.T) {
	facts := []imageFact{
		labelled(t, "beacon", "0.3.0", 3, time.Now().Add(-3*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, time.Now().Add(-2*time.Hour), 10),
		labelled(t, "beacon", "0.1.0", 1, time.Now().Add(-time.Hour), 10),
	}
	// A podman driver, because the exclusive store is the only one on which "removable" is a number at
	// all — the shared store has its own arm, asserted below.
	drv := &podmanDriver{bin: fakeEngine(t, t.TempDir(), facts)}

	told := strings.Join(imageStatus(context.Background(), drv, []Spec{{
		Name: "beacon", Version: "0.1.0", Image: "127.0.0.1:5000/actors/beacon@" + digestOf(1),
	}}, true), "\n")
	if !strings.Contains(told, "1 removable") {
		t.Fatalf("with an assignment the block does not report what the next prune takes, so the rest of "+
			"this test is vacuous:\n%s", told)
	}

	untold := strings.Join(imageStatus(context.Background(), drv, nil, false), "\n")
	if strings.Contains(untold, "removable,") {
		t.Errorf("the block counts removables without an assignment:\n%s", untold)
	}
	if !strings.Contains(untold, "nothing here is removable") || !strings.Contains(untold, "Controller did not answer") {
		t.Errorf("the block does not say why it will not count:\n%s", untold)
	}
}

// A RECONCILE TURN THAT IS NOT A PRUNE TURN MUST NOT TOUCH THE ENGINE. Reading the store means
// enumerating and inspecting every image on the Machine; at the loop's five-second interval that is a
// hundred execs a minute for ever, and it would arrive silently because the result is simply discarded.
//
// The same test holds the other half: an engine that will not answer restarts the timer anyway, so the
// failure is one log line every six hours — EXCEPT on a Machine actually under the disk floor, where it
// retries every turn, because there the Warden is the only thing that can fix it and somebody needs to
// see that it cannot.
func TestPruneAsksTheTimerBeforeItAsksTheEngine(t *testing.T) {
	var log bytes.Buffer
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	w := &warden{out: &log, now: func() time.Time { return at }}
	w.prune = &imagePrune{
		// A BIN THAT DOES NOT EXIST IS THE SEAM. Every engine call fails, so "did it call the engine" is
		// answerable from the log — and no test here needs a container runtime to find out.
		store: &localImages{bin: filepath.Join(t.TempDir(), "no-such-engine"), engine: "podman",
			exclusive: true},
		every:   pruneEvery,
		floor:   pruneDiskFloorPct,
		keep:    pruneKeepPerActor,
		freePct: func() (float64, bool) { return 80, true },
	}

	w.pruneImages(context.Background(), nil)
	if !strings.Contains(log.String(), "cannot read this Machine's image store") {
		t.Fatalf("the first turn did not try to read the store, so the rest of this test is vacuous: %q", log.String())
	}
	if w.prune.last != at {
		t.Errorf("last = %v; an ATTEMPT restarts the timer, or a broken engine is a log line every interval",
			w.prune.last)
	}

	log.Reset()
	w.pruneImages(context.Background(), nil)
	if log.Len() != 0 {
		t.Errorf("a turn inside the %s timer touched the engine again: %q", pruneEvery, log.String())
	}

	log.Reset()
	w.prune.freePct = func() (float64, bool) { return 3, true }
	w.pruneImages(context.Background(), nil)
	if !strings.Contains(log.String(), "cannot read this Machine's image store") {
		t.Errorf("a Machine under the %.0f%% floor waited out the timer: %q", pruneDiskFloorPct, log.String())
	}
}

// --- against a real engine -------------------------------------------------------------------------

// THE PURE TESTS ABOVE CANNOT PROVE THE EXECS ARE RIGHT, and that is the other half of this change: a
// template that read `{{.Id}}` from the wrong field, an `image inspect` whose JSON did not decode, or a
// `rmi` that took the wrong argument would all leave every image unlabelled — a prune that silently
// reclaims nothing, which is indistinguishable from a Machine with nothing to reclaim.
//
// IT ONLY EVER TOUCHES AN IMAGE IT MADE. The image is imported from a one-file tar under a probe actor
// name, carries no other name, and is removed on cleanup whatever happens. Nothing here enumerates by
// name and deletes; `drop` is called on the one fact the test created.
//
// BOTH ENGINES, BECAUSE THEY DISAGREE. podman's `rmi <sole tag>` deletes the image and docker's leaves
// it when a repo digest still holds it; the same code has to be right on both, and a test that ran on
// one would be a test of whichever box it happened to be on.
func TestImageStoreNamesReadsAndRemovesARealImage(t *testing.T) {
	for _, bin := range []string{"podman", "docker"} {
		t.Run(bin, func(t *testing.T) { imageStoreRoundTrip(t, bin) })
	}
}

func imageStoreRoundTrip(t *testing.T, bin string) {
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s is not on this box, so the engine seam cannot be exercised here", bin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("kontra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "rootfs.tar")
	if out, err := exec.CommandContext(ctx, "tar", "-cf", tarball, "-C", dir, "hello").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	// IMPORTED WITH NO NAME, so the kontra tag below is the image's ONLY one — which is the state a
	// Worker's image is in after a pull by digest, and the only state in which `drop` deletes anything.
	out, err := exec.CommandContext(ctx, bin, "import", tarball).CombinedOutput()
	if err != nil {
		t.Skipf("%s import is not usable here (%v): %s", bin, err, out)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	id := strings.TrimPrefix(fields[len(fields)-1], "sha256:")
	t.Cleanup(func() {
		_, _ = exec.Command(bin, "rmi", "--force", id).CombinedOutput()
	})

	store := &localImages{bin: bin, engine: bin}
	digest := digestOf(9)
	name, err := imageTagFor("kontra-prune-probe", "0.0.1", digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.tag(ctx, id, name); err != nil {
		t.Fatalf("tag: %v", err)
	}

	facts, err := store.facts(ctx)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	var got *imageFact
	for i := range facts {
		if facts[i].ID == id {
			got = &facts[i]
		}
	}
	if got == nil {
		t.Fatalf("%s is not in the %d image(s) the store reported, so `facts` cannot see what `tag` wrote",
			id, len(facts))
	}
	if got.Actor != "kontra-prune-probe" || got.Version != "0.0.1" {
		t.Errorf("read back %s@%s from %v; want kontra-prune-probe@0.0.1", got.Actor, got.Version, got.Names)
	}
	if got.Created.IsZero() {
		t.Errorf("%s reported no usable creation time (%v), so no image on this engine can be ranked by age",
			bin, got.Names)
	}
	if got.Size <= 0 {
		t.Errorf("size = %d, so no prune could ever report what it freed", got.Size)
	}
	if !got.soleName() {
		t.Fatalf("names = %v; the probe should carry the kontra name and nothing else", got.Names)
	}

	// THE PLACEMENT RULE, AGAINST A REAL FACT. The engine recorded no repo digest for an imported image,
	// so this is the path that matches on the tag's short digest alone.
	if !(placedImage{Actor: "kontra-prune-probe", Version: "0.0.1", Digest: digest}).holds(*got) {
		t.Errorf("a placement naming %s does not match the image it is running from (digests=%v short=%q)",
			digest, got.Digests, got.Short)
	}

	// A CANCELLED PARENT MUST NOT STOP A REMOVAL HALFWAY. Between `rmi <tag>` and `rmi <id>` the image is
	// untagged, which makes it an image this Warden has no name for — and an unlabelled image is one no
	// later prune may remove, so the bytes would be stranded for the life of the Machine. `drop` runs each
	// removal to its own end for that reason, and this is the assertion that holds it.
	dead, stop := context.WithCancel(ctx)
	stop()
	if err := store.drop(dead, *got); err != nil {
		t.Fatalf("drop under a cancelled parent: %v", err)
	}
	after, err := store.facts(ctx)
	if err != nil {
		t.Fatalf("facts after drop: %v", err)
	}
	for _, f := range after {
		if f.ID == id {
			t.Fatalf("%s survived `drop` with names %v", id, f.Names)
		}
	}
	if gone, bytes := storeDelta(facts, after); gone != 1 || bytes != got.Size {
		t.Errorf("storeDelta = %d image(s), %d bytes; want 1 and %d", gone, bytes, got.Size)
	}

	// THE FIRST DISK READ IN THE WARDEN, against the filesystem the engine actually stores on.
	root := store.root(ctx)
	if !strings.HasPrefix(root, "/") {
		t.Errorf("the %s store root is %q; a prune floor measured on nothing never fires", bin, root)
	}
	free, ok := diskFreePercent(root)
	if !ok || free <= 0 || free > 100 {
		t.Errorf("diskFreePercent(%q) = %v, %v; want a plausible percentage", root, free, ok)
	}
}

// ACCEPTANCE TEST 15, END TO END ON A REAL ENGINE. The pure version above proves the decision; this
// proves the whole turn — that `pruneImages` labels, reads, decides and REMOVES the images the decision
// named, and leaves the placed one and the two newest alone.
//
// EVERY IMAGE IT TOUCHES IS ONE IT IMPORTED, under an actor name no deploy produces, and all five are
// force-removed on cleanup. The prune is run against the Machine's whole store, so the other images on
// the box are also an assertion: they are unlabelled, and none of them may go.
func TestPruneRemovesTheRightDigestsOnARealEngine(t *testing.T) {
	const bin, actor = "podman", "kontra-prune-probe"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s is not on this box", bin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// EXCLUSIVE, BECAUSE THAT IS THE SHAPE THIS TEST IS ABOUT: one Warden, one box, one store. The shared
	// store removes nothing at all — see TestTwoWardensOverOneStoreRemoveNothing.
	store := &localImages{bin: bin, engine: bin, exclusive: true}
	versions := []string{"0.1.0", "0.2.0", "0.3.0", "0.4.0", "0.5.0"} // imported oldest first
	ids := map[string]string{}
	for i, version := range versions {
		id := importProbeImage(t, ctx, bin, i)
		tag, err := imageTagFor(actor, version, digestOf(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.tag(ctx, id, tag); err != nil {
			t.Fatalf("tag %s: %v", tag, err)
		}
		ids[version] = id
	}

	beforeAll, err := store.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// THE PLACED ONE IS THE OLDEST, which is the arrangement that distinguishes this rule from "keep the
	// three newest" — the same reason the pure test uses it.
	w := &warden{out: &testLog{t: t}, now: time.Now}
	w.prune = &imagePrune{
		store:   store,
		every:   pruneEvery,
		floor:   pruneDiskFloorPct,
		keep:    pruneKeepPerActor,
		freePct: func() (float64, bool) { return 80, true },
	}
	w.pruneImages(ctx, []Spec{{
		Name: actor, Version: "0.1.0",
		Image: "127.0.0.1:5000/actors/" + actor + "@" + digestOf(1),
	}})

	after, err := store.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]imageFact{}
	for _, f := range after {
		held[f.ID] = f
	}
	for _, version := range []string{"0.1.0", "0.4.0", "0.5.0"} {
		if _, ok := held[ids[version]]; !ok {
			t.Errorf("%s@%s is gone; it is placed or among the two most recent", actor, version)
		}
	}
	for _, version := range []string{"0.2.0", "0.3.0"} {
		if f, ok := held[ids[version]]; ok {
			t.Errorf("%s@%s survived the prune with names %v", actor, version, f.Names)
		}
	}

	// NOTHING ELSE ON THE MACHINE MOVED. An unlabelled image is one this Warden did not pull, and the
	// store is full of them — this is the assertion that a prune cannot reach the host image every actor
	// build needs.
	for _, f := range beforeAll {
		if f.Actor == actor {
			continue
		}
		if _, ok := held[f.ID]; !ok {
			t.Errorf("the prune removed %s (%v), which carries no kontra name", f.ID, f.Names)
		}
	}
}

// importProbeImage makes one throwaway image with no name, so the kontra tag the test adds is its only
// one. A distinct payload per call so the engine gives each a distinct id.
func importProbeImage(t *testing.T, ctx context.Context, bin string, n int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte(fmt.Sprintf("kontra probe %d\n", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "rootfs.tar")
	if out, err := exec.CommandContext(ctx, "tar", "-cf", tarball, "-C", dir, "probe").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	out, err := exec.CommandContext(ctx, bin, "import", tarball).CombinedOutput()
	if err != nil {
		t.Skipf("%s import is not usable here (%v): %s", bin, err, out)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	id := strings.TrimPrefix(fields[len(fields)-1], "sha256:")
	t.Cleanup(func() { _, _ = exec.Command(bin, "rmi", "--force", id).CombinedOutput() })
	return id
}

// --- N Wardens, ONE image store --------------------------------------------------------------------

// WHICH STORES ARE THIS MACHINE'S OWN, PINNED AT THE SOURCE. The whole shared-store rule rests on these
// two constructors, and the field is spelled so the zero value is the safe one — which means a driver
// that FORGOT to declare exclusivity is safe and a driver that declared it wrongly is not. So the
// declaration is asserted rather than left to be read.
func TestOnlyThePodmanStoreIsThisMachinesOwn(t *testing.T) {
	if !(&podmanDriver{bin: "podman"}).localImages().exclusive {
		t.Error("the podman store is one Warden on one box (ADR 0037) and nothing would ever be reclaimed")
	}
	if (&dockerDriver{bin: "docker"}).localImages().exclusive {
		t.Error("the docker store is the host daemon every Machine of a docker Fleet shares (ADR 0047); " +
			"declaring it exclusive lets one Warden remove another Machine's image")
	}
	// serve-dev IS a dockerDriver (driver_dev.go), so it inherits the answer. Asserted because the
	// embedding is what makes it true, and an embedding is easy to replace with a method.
	dev := &devDriver{dockerDriver: &dockerDriver{bin: "docker"}}
	if dev.localImages().exclusive {
		t.Error("serve-dev prunes an operator's own box")
	}
}

// ═══ THE INVARIANT, ON A STORE THAT IS NOT THIS WARDEN'S ALONE ═══
//
// Ranking per actor across the whole store is only meaningful when the whole store serves THIS Warden's
// assignment. Where it does not, the rank of another Machine's placement is computed against this
// Machine's recency budget, and the oldest digest on the box — which is exactly what a long-running
// placement is — is the first thing to go.
func TestASharedImageStoreIsNeverPruned(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	images := []imageFact{
		labelled(t, "beacon", "0.5.0", 5, base.Add(5*time.Hour), 10), // placed on THIS Machine
		labelled(t, "beacon", "0.4.0", 4, base.Add(4*time.Hour), 10),
		labelled(t, "beacon", "0.3.0", 3, base.Add(3*time.Hour), 10),
		labelled(t, "beacon", "0.2.0", 2, base.Add(2*time.Hour), 10),
		labelled(t, "beacon", "0.1.0", 1, base.Add(1*time.Hour), 10), // placed on the OTHER Machine
	}
	mine := []placedImage{placement("beacon", "0.5.0", 5)}

	// CONTROL FIRST, so the assertion below cannot pass because the decision removes nothing ever: on a
	// store this Warden owns, the digest the other Machine is running IS the first thing it takes.
	solo := prunePlan(ready(images, mine))
	if !containsShort(removedShorts(solo), images[4].Short) {
		t.Fatalf("an exclusive store keeps %v and removes %v; this fixture is supposed to tempt the prune "+
			"into the other Machine's digest %s", keptBy(solo, keepRecent), removedShorts(solo), images[4].Short)
	}

	in := ready(images, mine)
	in.Exclusive = false
	v := prunePlan(in)
	if len(v.Remove) != 0 {
		t.Errorf("a Warden sharing its store would remove %v, and it cannot see which of those another "+
			"Machine is running", removedShorts(v))
	}
	if v.removeBytes() != 0 {
		t.Errorf("removeBytes = %d; a shared store has no removal list to measure", v.removeBytes())
	}
	// EVERY NAMED IMAGE IS ACCOUNTED FOR, under one of two reasons. `shared` is the new one; `placed`
	// is a fact this Warden still holds — `mine` IS its own assignment, and sharing the store means it
	// cannot see the OTHER Machines' placements, not that it has forgotten its own. Reporting zero
	// placed on a Machine running one is the status surface lying about the only thing it knows.
	shared, _ := v.kept(keepShared)
	placed, _ := v.kept(keepPlaced)
	if shared+placed != len(images) {
		t.Errorf("%d shared + %d placed != %d named images; something fell out of the plan",
			shared, placed, len(images))
	}
	if placed != len(mine) {
		t.Errorf("%d placed; this Warden was assigned %d and must still say so", placed, len(mine))
	}
	// And the reason the ranking did NOT run: not one image carries `recent`, which is the only reason
	// the per-actor ranking produces.
	if n, _ := v.kept(keepRecent); n != 0 {
		t.Errorf("%d image(s) kept as recent; the per-actor ranking must not run on a shared store", n)
	}
}

// TWO WARDENS, ONE ENGINE — the docker Fleet's actual shape (ADR 0047), and per BLOCKERS #32 the only
// shape where a placement is written by code at all. Each Warden holds its own slice of the placements
// and neither can ask the other what it was assigned.
//
// EVERY IMAGE IT TOUCHES IS ONE IT IMPORTED, under an actor name no deploy produces, and all five are
// force-removed on cleanup.
func TestTwoWardensOverOneStoreRemoveNothing(t *testing.T) {
	const actor = "kontra-share-probe"
	// DOCKER IS THE FLEET'S ENGINE AND PODMAN IS THE FALLBACK, because what makes the store shared is the
	// `exclusive` field the decision reads (pinned per driver in TestOnlyThePodmanStoreIsThisMachinesOwn)
	// and not which binary is on the box. One engine, so that the invariant is never untested here.
	bin := ""
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(candidate); err == nil {
			bin = candidate
			break
		}
	}
	if bin == "" {
		t.Skip("neither docker nor podman is on this box, so two Wardens cannot be put on one engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// TWO STORES, ONE ENGINE: two processes, two structs, one daemon.
	store1, store2 := sharedStore(t, bin), sharedStore(t, bin)

	versions := []string{"0.1.0", "0.2.0", "0.3.0", "0.4.0", "0.5.0"} // imported oldest first
	ids := map[string]string{}
	for i, version := range versions {
		id := importProbeImage(t, ctx, bin, i)
		tag, err := imageTagFor(actor, version, digestOf(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if err := store1.tag(ctx, id, tag); err != nil {
			t.Fatalf("tag %s: %v", tag, err)
		}
		ids[version] = id
	}
	ref := func(n int) string { return "127.0.0.1:5000/actors/" + actor + "@" + digestOf(n) }

	// MACHINE 1 RUNS THE NEWEST AND MACHINE 2 RUNS THE OLDEST, which is the arrangement where Warden 1's
	// own ranking makes Machine 2's digest the first thing it would take.
	on1 := []Spec{{Name: actor, Version: "0.5.0", Image: ref(5)}}
	on2 := []Spec{{Name: actor, Version: "0.1.0", Image: ref(1)}}

	before, err := store1.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short := strings.TrimPrefix(digestOf(1), "sha256:")[:imageTagDigestLen]
	if solo := prunePlan(ready(before, placementsOf(on1))); !containsShort(removedShorts(solo), short) {
		t.Fatalf("on an exclusive store Warden 1 would remove %v, which does not include Machine 2's "+
			"digest %s — so this test would pass without proving anything", removedShorts(solo), short)
	}

	var log1, log2 bytes.Buffer
	warden1 := &warden{out: &log1, now: time.Now, prune: sharedPrune(store1)}
	warden2 := &warden{out: &log2, now: time.Now, prune: sharedPrune(store2)}
	warden1.pruneImages(ctx, on1)
	warden2.pruneImages(ctx, on2)

	after, err := store1.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]imageFact{}
	for _, f := range after {
		held[f.ID] = f
	}
	for _, version := range versions {
		if _, ok := held[ids[version]]; !ok {
			t.Errorf("%s@%s is gone from a store two Wardens share — one of them removed a digest it could "+
				"not see the placement for", actor, version)
		}
	}
	// AND IT SAID SO. A Warden that silently declines is one an operator reads as a Warden that pruned.
	for i, got := range []string{log1.String(), log2.String()} {
		if !strings.Contains(got, "shared with every other Warden") {
			t.Errorf("warden %d did not report why it removed nothing: %q", i+1, got)
		}
	}
}

// sharedStore is one Warden's handle on an engine it does not own, which is every Machine of a docker
// Fleet. The zero value is asserted rather than assumed: a store nobody declared exclusive has to be the
// shared one, or a driver that forgets the declaration prunes somebody else's images.
func sharedStore(t *testing.T, bin string) *localImages {
	t.Helper()
	s := &localImages{bin: bin, engine: bin}
	if s.exclusive {
		t.Fatal("a store nobody declared exclusive must be the shared one")
	}
	return s
}

// sharedPrune is the housekeeping of one Warden of a docker Fleet: a store it shares, a disk that is
// fine, so the only thing holding the prune back is the sharing.
func sharedPrune(store *localImages) *imagePrune {
	return &imagePrune{
		store:   store,
		every:   pruneEvery,
		floor:   pruneDiskFloorPct,
		keep:    pruneKeepPerActor,
		freePct: func() (float64, bool) { return 80, true },
	}
}

// --- a removal completes or leaves no trace --------------------------------------------------------

// ═══ THE UNTAG SUCCEEDS AND THE ID-DELETE IS REFUSED ═══
//
// An image pulled by digest is still held by its repo digest after `rmi <tag>`, so the untag is not the
// last reference and the engine allows it — and then refuses `rmi <id>` because a container is using the
// image. That leaves bytes with NO kontra name, which is an image no later prune may ever remove: the
// only permanent state in this file. So the name goes back on.
//
// A SCRIPTED ENGINE RATHER THAN A LIVE ONE, because the two engines refuse at different points and what
// is being pinned is the SEQUENCE: which calls are made, in which order, and what is true afterwards.
func TestDropPutsTheNameBackWhenTheEngineRefusesTheIdDelete(t *testing.T) {
	f := labelled(t, "beacon", "0.2.0", 2, time.Now(), 10)

	t.Run("refused", func(t *testing.T) {
		store, names := scriptedEngine(t, f, true)
		err := store.drop(context.Background(), f)
		if err == nil {
			t.Fatal("drop reported success while the engine refused to delete the image")
		}
		if !strings.Contains(err.Error(), "name is back on it") {
			t.Errorf("the error does not say the store was left as it was: %v", err)
		}
		if got := names(); !containsShort(got, f.Tag) {
			t.Fatalf("the image now carries %v — with no kontra name it is invisible to every later prune, "+
				"so the bytes are stranded for the life of the Machine", got)
		}
	})

	t.Run("completed", func(t *testing.T) {
		store, names := scriptedEngine(t, f, false)
		if err := store.drop(context.Background(), f); err != nil {
			t.Fatalf("drop: %v", err)
		}
		if store.held(context.Background(), f.ID) {
			t.Error("the engine still holds the image, so nothing was reclaimed")
		}
		if got := names(); len(got) != 0 {
			t.Errorf("names left behind: %v", got)
		}
	})
}

// --- the disk the floor is measured on -------------------------------------------------------------

// ═══ THE ENGINE ANSWERS FOR THE DAEMON, NOT FOR THE PROCESS ASKING ═══
//
// `info` reports `/var/lib/docker` — a path in the daemon's mount namespace — and a Warden that is
// itself a container holding that socket has no such path. The `statfs` fails, `HaveFree` goes false and
// the low-disk trigger never fires: silent, and silent exactly on a Machine already short of space.
func TestTheDiskFloorIsMeasuredOnAPathThisProcessCanStat(t *testing.T) {
	f := labelled(t, "beacon", "0.1.0", 1, time.Now(), 10)

	// The daemon's path, as a Warden in a container sees it: absent.
	daemonOnly := &localImages{bin: fakeEngine(t, "/var/lib/docker-no-such-root", []imageFact{f}),
		engine: "docker"}
	d := daemonOnly.disk(context.Background())
	if d.OwnRoot {
		t.Errorf("disk claimed %q is the store's own root while this process cannot stat it", d.Path)
	}
	if !d.Have || d.FreePct <= 0 || d.FreePct > 100 {
		t.Fatalf("disk = %+v; the floor has nothing to fire on and the trigger is dead", d)
	}
	p := &imagePrune{store: daemonOnly, every: pruneEvery, floor: pruneDiskFloorPct, keep: pruneKeepPerActor}
	if _, ok := p.free(context.Background()); !ok {
		t.Error("the prune cannot read a disk figure, so only the 6h timer would ever bring a prune forward")
	}
	// AND IT IS REPORTED AS A PROXY. A percentage about another filesystem must not be read as one about
	// the store.
	lines := strings.Join(imageStatus(context.Background(), &dockerDriver{bin: daemonOnly.bin}, nil, true), "\n")
	if !strings.Contains(lines, "cannot stat") || !strings.Contains(lines, "/var/lib/docker-no-such-root") {
		t.Errorf("the status block does not name the path it measured or why:\n%s", lines)
	}

	// CONTROL: a root this process CAN stat is used as itself, and says nothing about a proxy.
	root := t.TempDir()
	own := &localImages{bin: fakeEngine(t, root, []imageFact{f}), engine: "podman", exclusive: true}
	if got := own.disk(context.Background()); !got.OwnRoot || got.Path != root {
		t.Errorf("disk = %+v for a root it could read; want %s measured as the store's own", got, root)
	}
}

// --- `kontra warden status` reaches the image block ------------------------------------------------

// ═══ A MACHINE WITH NO WORKERS IS THE MACHINE WHOSE STORE IS WORTH READING ═══
//
// Nothing is placed, so everything named is a candidate and the image block is the only thing left to
// report. Asserted THROUGH `wardenStatus` and not through `imageStatus`, because the defect was the
// command returning early: every assertion on the inner function passed while the block was unreachable.
func TestWardenStatusReachesTheImageBlockWithNoWorkers(t *testing.T) {
	dir := t.TempDir()
	// A CONTROLLER THAT REFUSES INSTANTLY. `status` fetches the assignment, and what is under test is the
	// path taken when it cannot be had.
	if _, err := bootstrapLocalIdentity(dir, "acme", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}

	// A SCRIPTED ENGINE ON PATH, because "no Workers" is not a state a box can be asked for — this one is
	// running them. The driver is the real `docker` one, resolved by name through PATH.
	bin := fakeEngine(t, "/var/lib/docker-no-such-root", nil)
	engineDir := t.TempDir()
	if err := os.Symlink(bin, filepath.Join(engineDir, "docker")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", engineDir)

	var out bytes.Buffer
	prev := cliio.Stdout
	cliio.Stdout = &out
	t.Cleanup(func() { cliio.Stdout = prev })

	if err := wardenStatus([]string{"--state", dir, "--driver", "docker"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "workers    none") {
		t.Fatalf("the scripted engine reported Workers, so this test is not about the empty Machine:\n%s", got)
	}
	if !strings.Contains(got, "images ") {
		t.Fatalf("`warden status` on a Machine with no Workers never printed its image store:\n%s", got)
	}
	if !strings.Contains(got, "shared with every") {
		t.Errorf("the block does not say a docker store is nobody's to prune:\n%s", got)
	}
}

// --- helpers --------------------------------------------------------------------------------------

// fakeEngine is a read-only engine: the images it is given, and one storage root. It exists so a test
// can state a Machine's store — including a store root that is NOT in this process's mount namespace,
// which is every containerised Warden and is not a state a box can be asked for.
func fakeEngine(t *testing.T, root string, facts []imageFact) string {
	t.Helper()
	dir := t.TempDir()
	rows := make([]imageInspect, 0, len(facts))
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, "sha256:"+f.ID)
		created := ""
		if !f.Created.IsZero() {
			created = f.Created.Format(time.RFC3339Nano)
		}
		rows = append(rows, imageInspect{ID: "sha256:" + f.ID, RepoTags: f.Names, RepoDigests: f.Digests,
			Created: created, Size: f.Size})
	}
	blob, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	idFile := write("ids", strings.Join(ids, "\n")+"\n")
	inspectFile := write("inspect", string(blob))
	rootFile := write("root", root+"\n")

	bin := filepath.Join(dir, "engine")
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "images "*) cat %q ;;
  "info "*) cat %q ;;
  "image inspect") cat %q ;;
  "version "*) ;;
  "ps "*) ;;
  *) exit 1 ;;
esac
exit 0
`, idFile, rootFile, inspectFile)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// scriptedEngine is an engine that holds ONE image under the names it is given, and either allows or
// refuses the delete by id. It returns the store and a reader for the names the image currently carries,
// which is the thing `drop` must not strand.
func scriptedEngine(t *testing.T, f imageFact, refuseIDDelete bool) (*localImages, func() []string) {
	t.Helper()
	dir := t.TempDir()
	names := filepath.Join(dir, "names")
	if err := os.WriteFile(names, []byte(strings.Join(f.Names, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refuse := ""
	if refuseIDDelete {
		refuse = `echo "conflict: unable to delete $ref (must be forced) - image is being used by stopped container" >&2
      exit 1`
	}
	bin := filepath.Join(dir, "engine")
	// THE IMAGE OUTLIVES ITS NAMES, which is what makes the stranded state reachable: `rmi <tag>` is not
	// the last reference (a repo digest still holds it), so the untag succeeds on its own.
	script := fmt.Sprintf(`#!/bin/sh
D=%q
ID=%q
cmd="$1"; shift
if [ "$cmd" = image ]; then cmd="image-$1"; shift; fi
ref="$1"
case "$cmd" in
  rmi)
    if [ "$ref" = "$ID" ]; then
      %s
      if [ -f "$D/gone" ]; then echo "image not known" >&2; exit 1; fi
      : > "$D/gone"
      exit 0
    fi
    if grep -qxF "$ref" "$D/names"; then
      grep -vxF "$ref" "$D/names" > "$D/next"
      mv "$D/next" "$D/names"
      exit 0
    fi
    echo "image not known: $ref" >&2
    exit 1
    ;;
  image-inspect)
    if [ "$ref" = "$ID" ]; then
      [ -f "$D/gone" ] && exit 1
      exit 0
    fi
    grep -qxF "$ref" "$D/names" || exit 1
    exit 0
    ;;
  tag)
    [ -f "$D/gone" ] && { echo "no such image: $ref" >&2; exit 1; }
    echo "$2" >> "$D/names"
    exit 0
    ;;
esac
exit 1
`, dir, f.ID, refuse)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &localImages{bin: bin, engine: "docker"}, func() []string {
		t.Helper()
		raw, err := os.ReadFile(names)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
		return out
	}
}

func containsShort(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── THE TWO THINGS THE SHARED-STORE FIX BROKE, AND WHY EACH IS A TEST ──────────────────────────────
//
// Refusing to prune a shared store is right. Both of these came from implementing that refusal by
// routing every named image into one bucket, which threw away a fact the Warden does hold.

func TestASharedStoreStillReportsWhatIsPlacedOnThisMachine(t *testing.T) {
	// `in.Placements` IS this Warden's own assignment. Sharing the store means it cannot see the OTHER
	// Machines' placements — it does not mean it has forgotten its own. Reporting zero placed on a
	// Machine running three is the status surface lying about the one fact it has, and an operator
	// reading it concludes the Warden has lost its Workers.
	placedDigest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	in := pruneInput{
		Now:            time.Now(),
		Every:          6 * time.Hour,
		KeepPerActor:   2,
		HaveAssignment: true,
		Exclusive:      false, // a docker Fleet: N Wardens, one engine
		Placements:     []placedImage{{Actor: "demo", Version: "1.0.0", Digest: placedDigest}},
		Images: []imageFact{
			{ID: "i1", Actor: "demo", Version: "1.0.0", Digests: []string{placedDigest},
				Short: strings.TrimPrefix(placedDigest, "sha256:")[:12], Names: []string{"t1"}, Tag: "t1",
				Size: 10, Created: time.Now()},
			{ID: "i2", Actor: "demo", Version: "0.9.0", Digests: []string{other},
				Short: strings.TrimPrefix(other, "sha256:")[:12], Names: []string{"t2"}, Tag: "t2",
				Size: 20, Created: time.Now().Add(-time.Hour)},
		},
	}
	v := prunePlan(in)

	// Nothing is removable — that is the shared-store rule and it must not have regressed.
	if len(v.Remove) != 0 {
		t.Fatalf("a shared store offered %d image(s) for removal", len(v.Remove))
	}
	placed, placedBytes := v.kept(keepPlaced)
	if placed != 1 || placedBytes != 10 {
		t.Errorf("placed on a shared store: got %d image(s) / %d bytes, want 1 / 10", placed, placedBytes)
	}
	shared, _ := v.kept(keepShared)
	if shared != 1 {
		t.Errorf("the unplaced image should still read as shared: got %d", shared)
	}
	// And on an EXCLUSIVE store the same facts must still produce a removal, or this test would pass
	// against a prune that never removes anything.
	in.Exclusive = true
	in.KeepPerActor = 1
	if ex := prunePlan(in); len(ex.Remove) == 0 {
		t.Error("control: an exclusive store with the same facts removed nothing, so this test is vacuous")
	}
}

func TestTheDiskFloorDoesNotFireOnAStoreThatCanReclaimNothing(t *testing.T) {
	// The floor is read on every five-second reconcile turn. On a shared store a prune reclaims
	// nothing, so a breach can never clear — leaving the floor armed there turns a full disk into a
	// full store read and a log line every five seconds, for ever.
	in := pruneInput{
		Now:            time.Now(),
		LastPrune:      time.Now(), // the timer is NOT due, so the floor is the only arm that could fire
		Every:          6 * time.Hour,
		HaveAssignment: true,
		HaveFree:       true,
		FreePct:        3,
		FloorPct:       15,
	}
	in.Exclusive = false
	if due, why := pruneDue(in); due {
		t.Errorf("the floor fired on a shared store: %s", why)
	}
	// Control: the same breach on this Machine's own store MUST bring a prune forward, or the floor is
	// dead everywhere and this test proves nothing.
	in.Exclusive = true
	due, why := pruneDue(in)
	if !due {
		t.Fatalf("the floor did not fire on an exclusive store under the floor: %s", why)
	}
	if !strings.Contains(why, "floor") {
		t.Errorf("the reason should name the floor: %s", why)
	}
}
