package warden

// warden_images.go — the **Machine's** image store: what the Warden names, and what it reclaims.
//
// ═══ THE ONLY PATH IN KONTRA THAT RECLAIMS DISK, SO ITS DEFAULT ANSWER IS "KEEP" ═══
//
// Every other low-disk path here REFUSES work instead (`cli/appliance/bundle/stage.go`,
// `runtime/handler/internal/cas`), and that asymmetry is the shape of the rules below: a removal has to
// be argued for, and anything the Warden does not fully understand is kept. A prune that removes a
// PLACED image is unrecoverable from the Machine — the Worker stops, the next pull needs the network and
// the registry, and ADR 0037 gives the Machine no way to re-provision itself.
//
// ═══ THE LABEL IS A LOCAL TAG, BECAUSE A LABEL CANNOT BE ADDED WITHOUT A NEW IMAGE ═══
//
// An OCI label lives in the image's CONFIG BLOB, so adding one produces a new manifest with a new
// digest — and the digest IS the identity every gate in this system rests on (ADR 0032,
// trustpolicy.Admit, KONTRA_ACTOR_DIGEST). A Warden that relabelled an Artifact would have changed what
// it was asked to run. So §12's two label values ride in the one piece of naming an engine offers
// without touching the image, a tag:
//
//	localhost/dev.kontra.actor/<actor>/<first 12 of the manifest digest>:<version>
//
// THE DIGEST IS IN THE NAME BECAUSE A REBASE WOULD OTHERWISE STEAL IT. Keyed `<actor>:<version>`, a new
// digest for the same version would MOVE the tag and leave the old digest unlabelled — invisible to the
// prune for ever. Several digests of one actor being labelled at once is the whole input to "the 2 most
// recent digests per actor".
//
// ═══ N WARDENS, ONE IMAGE STORE: A SHARED STORE IS NEVER PRUNED ═══
//
// A docker Fleet's Machines are containers on ONE host daemon (ADR 0047), so every Warden there
// bind-mounts the same socket and reads the same images — while each is told only ITS OWN slice of the
// placements. On such a store "not placed here" is not "not placed", and ranking the whole store by
// actor would let Warden 1 remove the digest Machine 2 is running or about to run. A Warden cannot ask
// another Warden what it was assigned, and the kontra tag is ONE namespace shared by all of them, so a
// Warden cannot prove a digest is its own to remove either.
//
// So a shared store is reported and never reclaimed: its disk belongs to whoever runs the daemon, not
// to this Machine. Reclaiming happens on the EXCLUSIVE store — the podman Machine of ADR 0037, one
// Warden per box, where this Warden's assignment is the whole truth about what is placed here.
// `localImages.exclusive` is spelled positively so THE ZERO VALUE IS THE SAFE ONE: a store nobody
// declared exclusive removes nothing.
//
// ═══ WHAT A PRUNE WILL NOT TOUCH ═══
//
//	anything, on a shared store       see above: this Warden cannot see the other Wardens' placements.
//	anything, with no assignment      a Warden that cannot name what is placed here cannot keep it, so
//	                                  it keeps everything — and no surface may call such an image
//	                                  removable.
//	an image with no kontra tag       this Warden did not pull it. Nothing on a Machine carries a
//	                                  kontra-specific name until this change has run, so the first prune
//	                                  after an upgrade may remove nothing and says so.
//	an image with no creation time    what cannot be ranked by age cannot be one of the two newest.
//	a name this Warden did not write  removal untags the kontra name; the image goes by id only when
//	                                  that name was its last. `rmi <id>` on an image whose one remaining
//	                                  name is an operator's own untags AND deletes it.
//
// Shared layers are not this file's problem: both engines refuse to delete an image a container uses and
// reference-count layers underneath. Nothing here passes `--force`, which is what leaves those refusals
// in charge.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// --- the policy -----------------------------------------------------------------------------------

const (
	// pruneEvery is the timer. IT IS AN IN-PROCESS INTERVAL AND NOT A systemd `.timer`: the only timer
	// this system ever had is `kontra-watchdog.timer`, which every converge now disables and deletes
	// (control/orchestrator/src/infra/programs/machine.ts), with tests pinning both. A Warden that
	// installed a second unit would be re-introducing the thing that was retired.
	pruneEvery = 6 * time.Hour

	// pruneDiskFloorPct is the free-space floor that brings the next prune forward.
	pruneDiskFloorPct = 15.0

	// pruneKeepPerActor is how many digests of one actor survive on recency alone, on top of every
	// placed one. Two, so a rollback to the previous digest does not need the network.
	pruneKeepPerActor = 2

	// imageTagRepo is the label namespace, spelled as a repository path so that `podman images` reads
	// as the two labels §12 asks for. `localhost/` explicitly because podman prepends it to an
	// unqualified tag and docker does not, and the two engines have to agree on the string this file
	// reads back.
	imageTagRepo = "localhost/dev.kontra.actor"

	// imageTagDigestLen is how much of the manifest digest goes in the name — the 12 both engines
	// print, so an operator comparing `kontra fleet` to `podman images` compares the same characters.
	imageTagDigestLen = 12
)

// --- one image, as the local engine holds it ------------------------------------------------------

// imageFact is an image in the Machine's own store. It is the only thing the prune decision reads, so
// that the decision is a pure function of facts a test can write down (see warden_images_test.go).
type imageFact struct {
	// ID is the engine's local image id as bare hex, no `sha256:` prefix — podman's JSON omits the
	// prefix and docker's carries it, and `rmi` on both takes either.
	ID string

	// Digests are the MANIFEST digests the engine recorded for this image (`RepoDigests`), which is
	// what a placement's `<repo>@sha256:…` names. Empty for an image that never came from a registry.
	Digests []string

	// Names is every local name the engine holds for this image, kontra's included. Its length is what
	// decides whether removing kontra's name may also delete the image.
	Names []string

	// Actor, Version and Short are read back out of the kontra tag. Actor empty means the image
	// carries no kontra name, which is the first of the keep-unconditionally classes in the file header.
	Actor   string
	Version string
	Short   string

	// Tag is the kontra name itself, which is what `rmi` is given.
	Tag string

	Size    int64
	Created time.Time
}

// labelled reports whether this Warden's naming scheme can speak about the image at all.
func (f imageFact) labelled() bool { return f.Actor != "" && f.Tag != "" }

// soleName reports whether the kontra tag is the only name the engine holds, which is the one condition
// under which removing the image BY ID is this Warden's to do — see `localImages.drop`.
func (f imageFact) soleName() bool { return len(f.Names) == 1 && f.Names[0] == f.Tag }

// --- the tag --------------------------------------------------------------------------------------

// imageTagFor is the local name for one actor's one digest.
//
// IT REFUSES RATHER THAN SANITISES, and the refusal is expected rather than exceptional. kontra names
// Actors more freely than OCI names repositories — `shared/conformance/queues.json` pins `a/b`,
// `my actor` and `café` as names that must keep working — so `café` has no expressible tag and never
// gets one. The consequence is the safe direction and is stated where it lands: an unlabelled image is
// never removed, so such an actor's images accumulate rather than being deleted by a name this file
// guessed at.
func imageTagFor(actor, version, digest string) (string, error) {
	hex := strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	if !ociref.Digest.MatchString("sha256:" + hex) {
		return "", fmt.Errorf("%q is not a sha256 manifest digest, so there is no digest to name an "+
			"image by", digest)
	}
	ref := imageTagRepo + "/" + actor + "/" + hex[:imageTagDigestLen] + ":" + version
	if err := ociref.Check(ref); err != nil {
		return "", fmt.Errorf("%s@%s cannot be named locally, so its images are never labelled and "+
			"never pruned: %w", actor, version, err)
	}
	return ref, nil
}

// imageTagRead is imageTagFor backwards.
//
// THE ACTOR IS THE FREE FIELD, so the short digest is taken from the RIGHT — exactly the rule
// driver.go's `parseWorkerLabel` states for the same reason: an actor called `a/b` must round-trip, and
// a left-to-right split would read the name as `a` and lose the Worker.
func imageTagRead(ref string) (actor, version, short string, ok bool) {
	r := ociref.Split(ref)
	if !r.TagSet {
		return "", "", "", false
	}
	rest, found := strings.CutPrefix(r.Repository(), imageTagRepo+"/")
	if !found {
		return "", "", "", false
	}
	cut := strings.LastIndex(rest, "/")
	if cut <= 0 {
		return "", "", "", false
	}
	actor, short = rest[:cut], rest[cut+1:]
	if actor == "" || len(short) != imageTagDigestLen || r.Tag == "" {
		return "", "", "", false
	}
	return actor, r.Tag, short, true
}

// --- what is placed here --------------------------------------------------------------------------

// placedImage is one entry of the assignment, reduced to the three fields the decision needs.
type placedImage struct {
	Actor   string
	Version string
	Digest  string
}

// holds reports whether this placement is running from that image.
//
// TWO WAYS TO MATCH, AND THE SECOND IS NOT REDUNDANT. The repo digest is the authority; the kontra
// tag's short digest is what survives an image the engine holds with no `RepoDigests` at all (loaded
// from a tar, or imported), which would otherwise look unplaced and be removed under a Worker.
func (p placedImage) holds(f imageFact) bool {
	want := strings.TrimPrefix(p.Digest, "sha256:")
	if want == "" {
		return false
	}
	for _, d := range f.Digests {
		if strings.TrimPrefix(d, "sha256:") == want {
			return true
		}
	}
	return len(f.Short) >= imageTagDigestLen && strings.HasPrefix(want, f.Short)
}

// placementsOf reduces the turn's desired Workers to their images.
//
// A SPEC WHOSE IMAGE WILL NOT PARSE IS DROPPED, and that cannot cost a placed image. An unparseable or
// unpinned reference is one no image driver would have started (trustpolicy.Admit refuses it before
// anything is pulled), so there is no local image for it, nothing was ever labelled, and the prune has
// nothing it could remove. The `process` driver's specs carry no Image at all and land here the same
// way.
func placementsOf(specs []Spec) []placedImage {
	out := make([]placedImage, 0, len(specs))
	for _, s := range specs {
		digest, err := ociref.ImageDigest(s.Image)
		if err != nil {
			continue
		}
		out = append(out, placedImage{Actor: s.Name, Version: s.Version, Digest: digest})
	}
	return out
}

// --- the decision ---------------------------------------------------------------------------------

// The reasons an image survives a prune. The plan carries one with every kept image, because a plan that
// only said HOW MANY were kept would not be reviewable — and `warden status` counts by reason.
//
// THE FIRST TWO ARE WHOLE-STORE ANSWERS and the rest are about one image: when this Warden cannot speak
// for the store, every named image gets `keepShared` or `keepUnassigned` and the removal list is EMPTY.
// An empty removal list is the only honest one there, because the alternative is a count of "removable"
// that includes images placed on a Machine this Warden cannot see.
const (
	keepPlaced     = "placed"
	keepRecent     = "recent"
	keepUnlabelled = "unlabelled"
	keepUnreadable = "undatable"
	keepShared     = "shared"
	keepUnassigned = "unassigned"
)

// pruneInput is everything the decision reads. Three groups: what is placed, what is held, and whether
// it is time.
type pruneInput struct {
	Now       time.Time
	LastPrune time.Time
	Every     time.Duration

	// FreePct is free space on the filesystem holding the image store, and HaveFree is whether it could
	// be read at all. An unreadable disk must not look like a full one: it would make every turn a
	// prune turn.
	FreePct  float64
	HaveFree bool
	FloorPct float64

	// HaveAssignment is whether this Warden has ever been told what belongs here.
	//
	// NO ASSIGNMENT, NO PRUNE, and it is a property of the DECISION rather than of the call site. A
	// Warden that cannot name its placements cannot keep them, and the one thing this must never remove
	// is a placed image. It is the same rule `run` already applies to starting Workers: a Machine that
	// cannot ask does not act.
	HaveAssignment bool
	Placements     []placedImage

	// Exclusive is whether this Warden is the ONLY one on this image store — see the file header. False
	// means the store is shared and nothing in it may be removed, so the zero value is the safe one.
	Exclusive bool

	Images       []imageFact
	KeepPerActor int
}

// pruneKept is one image and why it survives.
type pruneKept struct {
	Image  imageFact
	Reason string
}

// pruneVerdict is the whole answer: whether to act, and what the act is. The plan is computed whether
// or not the TIMER says to act, because `kontra warden status` shows an operator what the next prune
// would take — but `Remove` is empty whenever this Warden cannot name every placement the store serves,
// because a removal list that cannot see the placements lists the placed images.
type pruneVerdict struct {
	Run    bool
	Reason string
	Keep   []pruneKept
	Remove []imageFact
}

// removeBytes is what the plan would reclaim if every removal took. It is an UPPER BOUND and not a
// measurement: untagging an image that another name still holds reclaims nothing, which is why
// `warden.pruneImages` reports what actually disappeared instead of this.
func (v pruneVerdict) removeBytes() int64 {
	var n int64
	for _, f := range v.Remove {
		n += f.Size
	}
	return n
}

// kept is how many images survive for one reason, and how many bytes they hold. One function over the
// reason rather than a field per reason, because the reasons are what `warden status` prints and a
// counter per reason would be a second place to forget one.
func (v pruneVerdict) kept(reason string) (count int, bytes int64) {
	for _, k := range v.Keep {
		if k.Reason == reason {
			count++
			bytes += k.Image.Size
		}
	}
	return count, bytes
}

// prunePlan is THE decision, and it is a pure function so that the invariant that matters can be
// tested without a container runtime: a prune that removes a placed image is unrecoverable on a fleet
// Machine.
//
// THE POLICY DOES NOT CHANGE UNDER PRESSURE. A prune triggered by low disk removes exactly what the
// timer's prune would remove — low disk only makes it happen sooner. A Machine that deleted MORE when
// it was short of space would delete the digest a rollback needs at precisely the moment an operator is
// most likely to want one, and it would do it without anybody asking.
func prunePlan(in pruneInput) pruneVerdict {
	keepN := in.KeepPerActor
	if keepN < 0 {
		keepN = 0
	}

	v := pruneVerdict{}
	byActor := map[string][]imageFact{}
	for _, f := range in.Images {
		switch {
		case !f.labelled():
			v.Keep = append(v.Keep, pruneKept{f, keepUnlabelled})
		case !in.Exclusive:
			// NOTHING IS RANKED ON A SHARED STORE. The ranking is per actor across the whole store, and
			// on a store several Wardens share that store is not the one this Warden was assigned —
			// `byActor` would rank another Machine's placements against this one's recency budget.
			//
			// BUT PLACED IS STILL TRUE, and saying otherwise was a regression in the one fact this
			// Warden does hold: `in.Placements` IS its own assignment. Reporting zero placed on a
			// Machine that is running three made the status surface lie about the thing it exists to
			// show. The ranking is what sharing makes unsafe; the placement check is not.
			if placedAmong(in.Placements, f) {
				v.Keep = append(v.Keep, pruneKept{f, keepPlaced})
				continue
			}
			v.Keep = append(v.Keep, pruneKept{f, keepShared})
		case !in.HaveAssignment:
			// AND NOTHING IS RANKED WITHOUT AN ASSIGNMENT, for the same reason one step closer to home:
			// every placement here is unknown, so every named image would rank as unplaced.
			v.Keep = append(v.Keep, pruneKept{f, keepUnassigned})
		case f.Created.IsZero():
			v.Keep = append(v.Keep, pruneKept{f, keepUnreadable})
		default:
			byActor[f.Actor] = append(byActor[f.Actor], f)
		}
	}

	for _, actor := range sortedKeysOf(byActor) {
		fs := byActor[actor]
		// NEWEST FIRST, TIES BROKEN BY ID. Two images made in the same instant are a real thing (a
		// rebase rewrites a config and keeps the layers), and a plan that depended on map order would
		// remove a different one each turn.
		sort.Slice(fs, func(i, j int) bool {
			if !fs[i].Created.Equal(fs[j].Created) {
				return fs[i].Created.After(fs[j].Created)
			}
			return fs[i].ID < fs[j].ID
		})
		for rank, f := range fs {
			switch {
			case placedAmong(in.Placements, f):
				// PLACED IS ADDITIVE TO RECENCY, not counted against it: §12 keeps "the images of every
				// current placement, PLUS the 2 most recent digests per actor". A placement that is also
				// one of the two newest is kept once, as placed.
				v.Keep = append(v.Keep, pruneKept{f, keepPlaced})
			case rank < keepN:
				v.Keep = append(v.Keep, pruneKept{f, keepRecent})
			default:
				v.Remove = append(v.Remove, f)
			}
		}
	}

	v.Run, v.Reason = pruneDue(in)
	return v
}

// placedAmong is the keep-a-placement rule, isolated so the test that matters reads as one line.
func placedAmong(ps []placedImage, f imageFact) bool {
	for _, p := range ps {
		if p.holds(f) {
			return true
		}
	}
	return false
}

// pruneDue answers whether this is a turn to act on, and says which of the two triggers fired. The
// REASON is returned alongside the verdict rather than logged here, because a decision that explains
// itself is one an operator can read in `warden status` without the Warden's log.
func pruneDue(in pruneInput) (bool, string) {
	if !in.HaveAssignment {
		return false, "this Warden has no assignment, so it cannot name what is placed here and will " +
			"not remove anything"
	}
	// THE FLOOR ONLY BRINGS A PRUNE FORWARD WHERE A PRUNE CAN RECLAIM SOMETHING. On a shared store
	// nothing is removable, so a floor breach would never clear — and the floor arm is checked every
	// five-second turn, so it would turn a full-disk Machine into a full store read and a log line
	// every five seconds, for ever, reclaiming nothing each time.
	if in.Exclusive && in.HaveFree && in.FloorPct > 0 && in.FreePct < in.FloorPct {
		return true, fmt.Sprintf("%.0f%% of the image store's filesystem is free, under the %.0f%% floor",
			in.FreePct, in.FloorPct)
	}
	if in.Every <= 0 {
		return false, "the prune timer is off"
	}
	if in.LastPrune.IsZero() {
		return true, "this Warden has not pruned yet"
	}
	elapsed := in.Now.Sub(in.LastPrune)
	if elapsed >= in.Every {
		return true, fmt.Sprintf("the last prune was %s ago and the timer is %s",
			elapsed.Truncate(time.Minute), in.Every)
	}
	return false, fmt.Sprintf("the next prune is in %s", (in.Every - elapsed).Truncate(time.Minute))
}

// --- the engine's image store ---------------------------------------------------------------------

// localImages is podman's or docker's image store, reached the way everything else in this package
// reaches a runtime: `exec`. There is no library client here on purpose — `cli/deploy.go`'s `imageAPI`
// is in `package main` and boundary_test.go enforces that this package cannot see it.
type localImages struct {
	// bin is the engine executable, taken from the driver that is already holding this Machine's
	// Workers so that the prune and the placements speak to the same runtime.
	bin string

	// engine is "podman" or "docker", needed only because the two spell the storage root's `info`
	// field differently.
	engine string

	// exclusive is whether this Warden is the only one on this store, which is the one condition under
	// which anything may be removed from it (see the file header). Positively spelled so the zero value
	// — a store nobody declared exclusive — reclaims nothing.
	exclusive bool

	// rootPath caches the storage root once resolved. Empty until asked.
	rootPath string
}

// imageHolder is the OPTIONAL capability "this driver's runtime has an image store".
//
// A SEPARATE INTERFACE, NOT A FIFTH VERB ON `workerDriver`. driver.go is explicit that the seam is four
// verbs and that "nothing else belongs here"; an image store is a different question from running a
// Worker, and the `process` driver — which pulls nothing and has no store at all (ADR 0036) — would
// have to implement a verb it cannot answer.
type imageHolder interface {
	localImages() *localImages
}

// THE PODMAN STORE IS THIS MACHINE'S OWN. A podman Warden is the systemd unit on an enrolled Machine
// (ADR 0037) — one box, one Warden, one store — so its assignment names every placement the store serves
// and the two newest digests per actor is a statement about this Machine.
func (d *podmanDriver) localImages() *localImages {
	return &localImages{bin: d.bin, engine: "podman", exclusive: true}
}

// THE DOCKER STORE IS SHARED AND IS THEREFORE NEVER RECLAIMED. Every Machine of a docker Fleet is a
// container holding the same host socket (ADR 0047), and `devDriver` embeds this one onto an operator's
// own box — neither is a store this Warden can speak for. See the file header.
func (d *dockerDriver) localImages() *localImages {
	return &localImages{bin: d.bin, engine: "docker"}
}

// imageInspect is the subset of `<engine> image inspect`'s JSON this file reads. The two engines agree
// on all five field names and both render `Created` as RFC3339; they differ only in whether `Id` carries
// the `sha256:` prefix, which is why `fact` trims it. The JSON is read rather than a `--format` template
// because podman's template renders `Created` as a Go `time.Time` and docker's as the raw string.
type imageInspect struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     string   `json:"Created"`
	Size        int64    `json:"Size"`
}

// imageInspectChunk bounds one `image inspect` argv. A dev box holds low hundreds of images and both
// engines take every id in one call; the chunk is there so the argv cannot grow past what an exec
// accepts on a Machine nobody has looked at in a year.
const imageInspectChunk = 50

// facts is the whole store, read from the engine.
//
// EVERY IMAGE, NOT JUST THE LABELLED ONES. The unlabelled half is what makes the first prune after an
// upgrade explicable — "nothing here is labelled yet, so nothing may be removed" — and it is the
// number an operator wants when a Machine's disk is full of images kontra did not pull.
func (s *localImages) facts(ctx context.Context) ([]imageFact, error) {
	out, err := exec.CommandContext(ctx, s.bin, "images", "--no-trunc", "--format", "{{.ID}}").Output()
	if err != nil {
		return nil, fmt.Errorf("%s images: %w", s.bin, err)
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id := strings.TrimPrefix(strings.TrimSpace(line), "sha256:")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	var facts []imageFact
	for start := 0; start < len(ids); start += imageInspectChunk {
		end := min(start+imageInspectChunk, len(ids))
		args := append([]string{"image", "inspect"}, ids[start:end]...)
		raw, err := exec.CommandContext(ctx, s.bin, args...).Output()
		var rows []imageInspect
		if jsonErr := json.Unmarshal(raw, &rows); jsonErr != nil {
			// AN IMAGE THAT VANISHED BETWEEN THE TWO CALLS IS NORMAL and both engines then exit
			// non-zero with the rest of the array still on stdout — so the exit status is only the
			// error when the output does not parse either.
			if err != nil {
				return nil, fmt.Errorf("%s image inspect: %w", s.bin, err)
			}
			return nil, fmt.Errorf("%s image inspect: %w", s.bin, jsonErr)
		}
		for _, r := range rows {
			facts = append(facts, r.fact())
		}
	}
	return facts, nil
}

// fact turns one inspect row into the decision's input.
func (r imageInspect) fact() imageFact {
	f := imageFact{
		ID:      strings.TrimPrefix(r.ID, "sha256:"),
		Digests: r.RepoDigests,
		Names:   r.RepoTags,
		Size:    r.Size,
		Created: parseImageTime(r.Created),
	}
	for _, name := range r.RepoTags {
		if actor, version, short, ok := imageTagRead(name); ok {
			f.Actor, f.Version, f.Short, f.Tag = actor, version, short, name
			break
		}
	}
	return f
}

// parseImageTime reads an engine's creation timestamp, and returns the zero time when it cannot.
// A zero time is not a fallback to "old": `prunePlan` keeps such an image unconditionally, because an
// image whose age is unknown cannot be ranked by age.
func parseImageTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// tag puts this Warden's name on an image it has pulled. Idempotent: both engines move or re-assert a
// tag without complaint, so a placement that is re-asserted every prune costs one exec and changes
// nothing.
func (s *localImages) tag(ctx context.Context, image, name string) error {
	out, err := exec.CommandContext(ctx, s.bin, "tag", image, name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s tag %s %s: %v: %s", s.bin, image, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// drop removes this Warden's name from an image, and the image itself only when that name was its last.
//
// ═══ TWO CALLS, BECAUSE A NAME AND AN IMAGE ARE DIFFERENT THINGS, AND THE ENGINES DISAGREE ═══
//
//	podman, kontra's tag the only name    `rmi <tag>` untags AND deletes; the call by id then reports
//	                                      "image not known", which is success wearing an error.
//	docker, image pulled BY DIGEST        `rmi <tag>` untags and the bytes STAY, held by the repo digest
//	                                      the pull recorded. `rmi <id>` is what reclaims them.
//	either engine, a second name present  only the kontra name goes. `rmi <id>` is not attempted,
//	                                      because on an image whose one remaining name is an operator's
//	                                      own it untags AND deletes it.
//
// So THE EXIT STATUS IS NOT THE ANSWER, exactly as `podmanDriver.Stop` says of a stop: the question is
// whether the engine is still holding the reference, and that is a READ. Trusting the status would report
// half of all successful removals as failures, for ever.
//
// NO `--force`, EVER. Both engines refuse to delete an image a container is using and one held under
// several names, and those refusals ARE §12's "never remove a layer a kept image still uses".
// ONE REMOVAL IS NOT INTERRUPTIBLE, THOUGH THE PRUNE IS. Cancelled between the two calls — a
// `systemctl stop` arriving mid-prune — the image would be left untagged, which makes it an image this
// Warden has no name for and therefore one no later prune may remove. So each removal runs to its own
// end and the LOOP above is what stops; the shape is `warden.start`'s, for the same reason.
//
// ═══ THE SEQUENCE EITHER COMPLETES OR LEAVES NO TRACE ═══
//
// A REFUSED ID-DELETE PUTS THE NAME BACK, because the untag can succeed on its own: an image pulled by
// digest is still held by its repo digest after `rmi <tag>`, so the untag is not the last reference and
// the engine allows it — and then refuses `rmi <id>` while a container uses the image. That leaves bytes
// with no kontra name, which is an image no later prune may ever remove: the one state in this file that
// is permanent. So the name is re-asserted and the refusal is reported, which leaves the store exactly
// as it was and lets the next prune try again.
func (s *localImages) drop(ctx context.Context, f imageFact) error {
	if f.Tag == "" {
		return fmt.Errorf("image %s carries no kontra name, so there is nothing here to remove", f.ID)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, s.bin, "rmi", f.Tag).CombinedOutput()
	if err != nil && s.held(ctx, f.Tag) {
		return fmt.Errorf("%s rmi %s: %v: %s", s.bin, f.Tag, err, strings.TrimSpace(string(out)))
	}
	if !f.soleName() {
		// The bytes stay, under somebody else's name, and that is the answer rather than a failure.
		return nil
	}
	out, err = exec.CommandContext(ctx, s.bin, "rmi", f.ID).CombinedOutput()
	if err == nil || !s.held(ctx, f.ID) {
		return nil
	}
	refused := fmt.Errorf("%s rmi %s: %v: %s", s.bin, f.ID, err, strings.TrimSpace(string(out)))
	if back := s.tag(ctx, f.ID, f.Tag); back != nil {
		return fmt.Errorf("%w — and its kontra name could not be put back (%v), so these bytes are now "+
			"invisible to every later prune", refused, back)
	}
	return fmt.Errorf("%w — the kontra name is back on it, so the next prune can try again", refused)
}

// held asks the engine whether a reference still resolves. `image inspect`'s exit status rather than
// `image exists`, which docker does not have.
func (s *localImages) held(ctx context.Context, ref string) bool {
	return exec.CommandContext(ctx, s.bin, "image", "inspect", ref).Run() == nil
}

// root is the filesystem the image store is on, which is the one whose free space matters. Asked of the
// engine rather than assumed, because `/var/lib/containers` and `/var/lib/docker` are frequently a
// separate volume on a Machine and a floor measured on `/` would never fire.
func (s *localImages) root(ctx context.Context) string {
	if s.rootPath != "" {
		return s.rootPath
	}
	format := "{{.DockerRootDir}}"
	if s.engine == "podman" {
		format = "{{.Store.GraphRoot}}"
	}
	s.rootPath = "/"
	if out, err := exec.CommandContext(ctx, s.bin, "info", "--format", format).Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" && strings.HasPrefix(p, "/") {
			s.rootPath = p
		}
	}
	return s.rootPath
}

// diskReading is what the prune floor fires on and what `warden status` prints: how much is free,
// whether it could be read at all, the path it was measured on, and whether that path is the engine's own
// storage root. The reading travels WITH the path so a turn that is not a prune turn takes one `statfs`
// and not two.
type diskReading struct {
	FreePct float64
	Have    bool
	Path    string
	OwnRoot bool
}

// disk measures the filesystem the prune floor is about.
//
// ═══ THE ENGINE ANSWERS FOR THE DAEMON, NOT FOR THE PROCESS ASKING ═══
//
// `info` reports a path in the DAEMON's mount namespace — `/var/lib/docker` on the host — and a Warden
// that is itself a container holding that daemon's socket (ADR 0047) has no such path of its own. The
// `statfs` then fails, `Have` is false, and the low-disk trigger silently never fires: the one trigger
// that exists for a Machine already short of space.
//
// SO THE FALLBACK IS THIS PROCESS'S OWN ROOT, which is the honest reading available to it. A container's
// writable layer lives inside the daemon's storage, so `/` and the store are the same filesystem there;
// where they are not, the number is about the Machine's disk rather than the store's, which is still a
// disk a full Machine is full of. It is reported AS a proxy — `warden status` prints the path measured —
// because a percentage about the wrong filesystem must not be read as one about the store.
func (s *localImages) disk(ctx context.Context) diskReading {
	root := s.root(ctx)
	if pct, ok := diskFreePercent(root); ok {
		return diskReading{FreePct: pct, Have: true, Path: root, OwnRoot: true}
	}
	pct, ok := diskFreePercent("/")
	return diskReading{FreePct: pct, Have: ok, Path: "/"}
}

// --- the Warden's side ----------------------------------------------------------------------------

// imagePrune is the Warden's image housekeeping: the store, the policy, and when it last tried.
//
// THE CLOCK IS IN MEMORY AND DIES WITH THE PROCESS, exactly like `warden.restarts` and for the same
// reason stated there: "a Machine that has just rebooted should retry immediately, not serve out a
// backoff earned by a previous life". And there is nowhere else for it: the state directory holds an
// identity and nothing else, pinned by name in warden_test.go, because "a file here is the local
// database driver.go refuses". That is also why `kontra warden status` reports the STORE and the next
// prune's plan rather than a history — see `imageStatus`.
type imagePrune struct {
	store *localImages

	every time.Duration
	floor float64
	keep  int

	// last is when a prune was last ATTEMPTED, not when one last succeeded. An attempt that cannot read
	// the engine still restarts the timer, so a broken engine is one log line every six hours instead of
	// one every five seconds — and a Machine actually short of disk is unaffected, because `pruneDue`
	// checks the floor before the timer and does not look at this field at all.
	last time.Time

	// freePct is a seam so a test can state a disk without having one. Nil asks the engine's store root.
	freePct func() (float64, bool)
}

// newImagePrune is the housekeeping for a driver that has an image store, or nil for one that has not.
func newImagePrune(drv workerDriver) *imagePrune {
	holder, ok := drv.(imageHolder)
	if !ok {
		return nil
	}
	return &imagePrune{
		store: holder.localImages(),
		every: pruneEvery,
		floor: pruneDiskFloorPct,
		keep:  pruneKeepPerActor,
	}
}

func (p *imagePrune) free(ctx context.Context) (float64, bool) {
	if p.freePct != nil {
		return p.freePct()
	}
	d := p.store.disk(ctx)
	return d.FreePct, d.Have
}

const (
	// pruneTimeout bounds one prune. UNLIKE `start`, A PRUNE IS INTERRUPTIBLE: `warden.start` takes no
	// context because cancelling it orphans a half mid-launch, while abandoning a prune costs nothing —
	// the next turn asks again, against a store that has only got closer to needing it.
	pruneTimeout = 5 * time.Minute

	// dropTimeout bounds ONE removal, which is the part that is not interruptible — see `localImages.drop`
	// for the untagged-but-undeletable image a cancellation in the middle would leave behind. A removal is
	// two `rmi` calls and finishes in well under a second; the cap is for an engine that has wedged.
	dropTimeout = 30 * time.Second
)

// label puts this Warden's name on the image of one Worker it has just started.
//
// §12's "label every image the Warden pulls" lands here because `start` is the pull: both image drivers
// let `run` pull what the Machine does not hold (driver_podman.go), and there is no separate pull verb
// to hang this on.
func (w *warden) labelImage(spec Spec) {
	if w.prune == nil || spec.Image == "" {
		return
	}
	digest, err := ociref.ImageDigest(spec.Image)
	if err != nil {
		return
	}
	name, err := imageTagFor(spec.Name, spec.Version, digest)
	if err != nil {
		w.logf("not labelling %s@%s's image: %v", spec.Name, spec.Version, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.prune.store.tag(ctx, spec.Image, name); err != nil {
		w.logf("could not label %s@%s's image: %v", spec.Name, spec.Version, err)
	}
}

// pruneImages is the end of a reconcile turn: re-assert this Warden's names, then reclaim.
//
// IT RUNS INSIDE THE LOOP RATHER THAN ON ITS OWN GOROUTINE. The decision reads the turn's DESIRED state
// — the one field `run` carries between turns — and a second goroutine reading it would be a data race
// on the loop's hot path, which is the objection `newServeWarden` already records against setting the
// watchpoint from the attach goroutine. The loop's own five-second tick is the in-process ticker;
// `pruneDue` is what makes six hours out of it.
//
// AFTER THE STOPS AND STARTS, so a placement never waits on housekeeping.
//
// A TURN THAT IS NOT A PRUNE TURN COSTS ONE `statfs` AND NOTHING ELSE. The trigger is asked BEFORE the
// store is read, because reading the store means enumerating and inspecting every image on the Machine
// — fine every six hours, wrong every five seconds.
func (w *warden) pruneImages(ctx context.Context, desired []Spec) {
	if w.prune == nil {
		return
	}
	p := w.prune
	ctx, cancel := context.WithTimeout(ctx, pruneTimeout)
	defer cancel()

	freePct, haveFree := p.free(ctx)
	in := pruneInput{
		Now:            w.now(),
		LastPrune:      p.last,
		Every:          p.every,
		FreePct:        freePct,
		HaveFree:       haveFree,
		FloorPct:       p.floor,
		HaveAssignment: true,
		Exclusive:      p.store.exclusive,
		KeepPerActor:   p.keep,
	}
	if due, _ := pruneDue(in); !due {
		return
	}
	// THE TIMER RESTARTS ON THE ATTEMPT, not on the success — see `imagePrune.last`. Set before any
	// engine call, so an engine that will not answer is one log line every six hours.
	p.last = in.Now

	// EVERY CURRENT PLACEMENT IS RE-LABELLED FIRST, and that is what makes a Worker this Warden ADOPTED
	// — one left running by the previous Warden on this Machine (warden.go's rule 3) — visible to the
	// prune at all. Without it the only labelled images would be the ones this process happened to
	// start, and a Machine that has not placed anything since a restart would look empty.
	//
	// THE SOURCE IS THE SPEC'S OWN REFERENCE, which is the exact string the driver handed the engine, so
	// a tag can never name bytes other than the ones the Worker is running. A placement whose image is
	// not held locally fails here and is meant to: there is nothing to label.
	for _, s := range desired {
		digest, err := ociref.ImageDigest(s.Image)
		if err != nil {
			continue
		}
		name, err := imageTagFor(s.Name, s.Version, digest)
		if err != nil {
			continue
		}
		_ = p.store.tag(ctx, s.Image, name)
	}

	before, err := p.store.facts(ctx)
	if err != nil {
		w.logf("cannot read this Machine's image store (%v) — nothing was pruned this turn", err)
		return
	}
	in.Images, in.Placements = before, placementsOf(desired)
	v := prunePlan(in)
	if !v.Run {
		// `in` has not changed since the check above except in the fields `pruneDue` does not read, so
		// this cannot fire. It is here rather than asserted because a `prunePlan` that grew a reason to
		// refuse should refuse, not have its verdict ignored.
		return
	}

	// THE SHARED STORE IS REPORTED AND LEFT ALONE, and the turn still happens so that this Warden's names
	// stay on the images it placed — which is the only thing `warden status` has to read there.
	if !p.store.exclusive {
		shared, sharedBytes := v.kept(keepShared)
		placed, placedBytes := v.kept(keepPlaced)
		w.logf("this %s image store is shared with every other Warden on the same engine, so nothing in "+
			"it may be removed: %d placed here (%s) and %d other named image(s) (%s), all left in place "+
			"because this Warden cannot see what is placed on the other Machines (ADR 0047)",
			p.store.engine, placed, humanBytes(placedBytes), shared, humanBytes(sharedBytes))
		return
	}

	placed, _ := v.kept(keepPlaced)
	recent, _ := v.kept(keepRecent)
	unlabelled, unlabelledBytes := v.kept(keepUnlabelled)
	w.logf("pruning this Machine's image store: %s — keeping %d placed and %d recent, removing %d (%s); "+
		"%d image(s) carry no kontra name and are never removed (%s)",
		v.Reason, placed, recent, len(v.Remove), humanBytes(v.removeBytes()), unlabelled,
		humanBytes(unlabelledBytes))

	for _, f := range v.Remove {
		// THE LOOP IS WHERE A CANCELLED PRUNE STOPS, not the middle of a removal (`localImages.drop`).
		// Checked before each one rather than after, so a shutdown costs at most the removal in flight.
		if err := ctx.Err(); err != nil {
			w.logf("the prune stopped early (%v); the rest is left for the next turn", err)
			return
		}
		if err := p.store.drop(ctx, f); err != nil {
			// A REMOVAL THAT WILL NOT TAKE IS A LOG LINE, NOT AN EXIT — the same shape as every other
			// failure in this loop. The engine refusing is usually the engine being right.
			w.logf("could not remove %s (%s@%s): %v", f.Short, f.Actor, f.Version, err)
			continue
		}
		w.logf("removed %s@%s %s (%s)", f.Actor, f.Version, f.Short, humanBytes(f.Size))
	}

	// WHAT WAS RECLAIMED IS READ BACK, NOT ASSUMED. Untagging an image another name still holds frees
	// nothing, so the honest number is the difference between two readings of the store — and this log
	// line is the one place §12's `pruned_bytes` is reported, because a fact about a running process
	// belongs in that process's log and this one keeps no database to put it in.
	after, err := p.store.facts(ctx)
	if err != nil {
		w.logf("pruned, but could not re-read the image store to say how much it freed: %v", err)
		return
	}
	gone, bytes := storeDelta(before, after)
	w.logf("prune done: %d image(s) gone, %s reclaimed", gone, humanBytes(bytes))
}

// storeDelta is what disappeared between two readings of the store, by id.
func storeDelta(before, after []imageFact) (gone int, bytes int64) {
	held := make(map[string]bool, len(after))
	for _, f := range after {
		held[f.ID] = true
	}
	for _, f := range before {
		if !held[f.ID] {
			gone++
			bytes += f.Size
		}
	}
	return gone, bytes
}

// humanBytes is for log lines and `warden status`. Binary units, because that is what both engines and
// `df` print and a reader comparing them should not have to convert.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, suffix := float64(n), ""
	for _, s := range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= unit
		suffix = s
		if v < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", v, suffix)
}

// --- `kontra warden status` -----------------------------------------------------------------------

// imageStatus is the image-store block of `kontra warden status`, and `status` is the ONLY surface a
// Warden reports its image store on.
//
// ═══ NOT THE HEALTH SIGNAL §12 ASKS FOR, BECAUSE THERE IS NO SUCH CHANNEL ═══
//
// Exhaustively, a Machine sends one `POST /warden/enrol` at join, one `GET /warden/assignment` per turn
// (inbound data only), and the watch activity's `wardenDecision` — four string/time fields delivered as
// a ≤200-byte Summary, beside a `RecordHeartbeat` whose details are empty on purpose: "A heartbeat
// payload is a place telemetry would arrive by accident" (warden_workflow.go). Nothing numeric leaves
// this process. A sixth `wardenDecisionKind` is refused from the other side too — that list is closed
// against exactly this class of value and a cross-language test holds it at five.
//
// WHAT A SECOND PROCESS CAN HONESTLY SAY is what the ENGINE is the authority on — how many images and
// how many bytes — plus the plan the next prune would take, derived with the same pure function the loop
// uses. `last_prune_at` and `pruned_bytes` belong to the SERVING process, which keeps no local database
// (see `imagePrune`), so they are log lines there and are absent here rather than guessed at.
//
// THE PLACEMENTS COME FROM THE ASSIGNMENT, so the caller fetches one. Without it the plan is not
// computable, and this says so rather than printing a removal list derived from no placements — which
// would invite an operator to run a prune that stops their Workers.
func imageStatus(ctx context.Context, drv workerDriver, desired []Spec, haveAssignment bool) []string {
	holder, ok := drv.(imageHolder)
	if !ok {
		return []string{fmt.Sprintf("none — the %s driver pulls no images, so this Machine has no "+
			"image store to prune", drv.driverName())}
	}
	store := holder.localImages()
	facts, err := store.facts(ctx)
	if err != nil {
		return []string{"unknown: " + err.Error()}
	}

	d := store.disk(ctx)
	freePct, haveFree := d.FreePct, d.Have
	// THE TIMER ARM IS OFF HERE, because "is it due" is a question about the SERVING Warden's clock and
	// this process does not have it. What is left is the disk arm, which is a fact about the Machine
	// and is true for whoever asks.
	v := prunePlan(pruneInput{
		Now:            time.Now(),
		Every:          0,
		FreePct:        freePct,
		HaveFree:       haveFree,
		FloorPct:       pruneDiskFloorPct,
		HaveAssignment: haveAssignment,
		Exclusive:      store.exclusive,
		Placements:     placementsOf(desired),
		Images:         facts,
		KeepPerActor:   pruneKeepPerActor,
	})

	var total int64
	named := 0
	for _, f := range facts {
		total += f.Size
		if f.labelled() {
			named++
		}
	}
	placed, placedBytes := v.kept(keepPlaced)
	recent, recentBytes := v.kept(keepRecent)
	unlabelled, unlabelledBytes := v.kept(keepUnlabelled)
	unreadable, _ := v.kept(keepUnreadable)

	lines := []string{
		fmt.Sprintf("%d held, %s (%s)", len(facts), humanBytes(total), store.engine),
		// NAMED IS COUNTED FROM THE FACTS, NOT FROM THE PLAN'S BUCKETS, so a reason added to the decision
		// cannot quietly drop images out of this number.
		namedLine(store, named, placed, placedBytes, recent, recentBytes),
	}

	// ═══ "REMOVABLE" IS PRINTED ONLY WHERE IT IS A NUMBER ═══
	//
	// Derived from an empty placement list it counts the PLACED images as removable — the opposite of
	// what this block exists to say, and the one way a diagnostic here could cost a Fleet its Workers by
	// inviting an operator to act on it. So the two cases where this Warden cannot name every placement
	// the store serves print the reason instead of a count.
	switch {
	case !store.exclusive:
		lines = append(lines, fmt.Sprintf("nothing here is removable: this %s store is shared with every "+
			"other Warden on the same engine, and no Warden on it can see what the others placed (ADR 0047)",
			store.engine))
	case !haveAssignment:
		lines = append(lines, "nothing here is removable: the Controller did not answer, so nothing can "+
			"say what is placed on this Machine — a prune refuses for the same reason")
	default:
		lines = append(lines, fmt.Sprintf("%d removable, %s — what the next prune takes",
			len(v.Remove), humanBytes(v.removeBytes())))
	}

	lines = append(lines, fmt.Sprintf("%d unlabelled, %s — never removed by a prune",
		unlabelled, humanBytes(unlabelledBytes)))
	if unreadable > 0 {
		lines = append(lines, fmt.Sprintf("%d with no creation time, kept because they cannot be ranked "+
			"by age", unreadable))
	}
	switch {
	case d.Have && d.OwnRoot:
		lines = append(lines, fmt.Sprintf("disk %.0f%% free on %s (prune floor %.0f%%, timer %s)",
			d.FreePct, d.Path, pruneDiskFloorPct, pruneEvery))
	case d.Have:
		// THE PATH MEASURED IS NAMED BECAUSE IT IS NOT THE STORE'S. See `localImages.disk`: the engine's
		// own root belongs to the daemon's mount namespace and is not reachable from here.
		lines = append(lines, fmt.Sprintf("disk %.0f%% free on %s (prune floor %.0f%%, timer %s) — the %s "+
			"store is at %s, which this process cannot stat, so the floor is measured on %s instead",
			d.FreePct, d.Path, pruneDiskFloorPct, pruneEvery, store.engine, store.root(ctx), d.Path))
	default:
		lines = append(lines, fmt.Sprintf("disk free on %s is unreadable, so only the %s timer brings a "+
			"prune forward", d.Path, pruneEvery))
	}
	// THE VERDICT IS PRINTED ONLY WHEN IT MEANS SOMETHING HERE. With the timer arm off, `Run` is exactly
	// "the disk arm fired", and that is a fact about the Machine that the serving Warden will act on
	// within one interval; the timer's own reason belongs to that process's clock and not to this one. On
	// a shared store it would promise a prune that is going to remove nothing.
	if v.Run && store.exclusive {
		lines = append(lines, "the serving Warden prunes on its next turn — "+v.Reason)
	}
	return lines
}

// namedLine renders the named-image breakdown, and it does not print a RECENT count on a shared store.
//
// "Recent" is the per-actor ranking, and the ranking is the thing a shared store makes unsafe — so
// there is no number to print there. Printing `0 recent` would read as "none of them are recent",
// which is a claim, rather than "this Warden did not rank them", which is the fact.
func namedLine(store *localImages, named, placed int, placedBytes int64, recent int, recentBytes int64) string {
	if !store.exclusive {
		return fmt.Sprintf("%d named %s — %d placed here (%s); not ranked, because the store is shared",
			named, imageTagRepo+"/<actor>/<digest>:<version>", placed, humanBytes(placedBytes))
	}
	return fmt.Sprintf("%d named %s — %d placed (%s), %d recent (%s)",
		named, imageTagRepo+"/<actor>/<digest>:<version>",
		placed, humanBytes(placedBytes), recent, humanBytes(recentBytes))
}
