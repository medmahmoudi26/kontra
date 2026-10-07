// volumes.go — ADR 0052 §7's OTHER mechanism: the one that covers what `protect: true` cannot see.
//
// ── THE TWO MECHANISMS ARE NOT REDUNDANT, AND THIS IS THE ONE THAT NEEDS CODE ────────────────────
//
// `protect: true` in the program is the GUARANTEE. Measured on pulumi 3.244.0 — the real program's
// volume block, converged, then one protected volume hand-deleted out of a copy of it:
//
//   - docker:index:Volume volSeaweedData delete[retain]
//     error: Preview failed: resource
//     "urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData" cannot be
//     deleted because it is protected.
//     Resources: 12 unchanged, 2 errored                                             (exit 1)
//
// The same edit against the UNPROTECTED `volTemporalDynamicconfig` previews as `- 1 to delete` and
// exits 0, which is the control that makes the measurement mean something.
//
// AND IT REFUSES A REPLACE TOO, which is worth measuring separately because §7's sharpest sentence is
// "the dangerous case is replacement, not deletion". Changing a declared volume's docker `name`
// forces a replacement rather than a delete, and on the same stack:
//
//	+- docker:index:Volume volSeaweedData replace[retain] [diff: ~name]
//	   error: unable to replace resource "urn:…::volSeaweedData" as it is currently marked for
//	   protection. To unprotect the resource, remove the `protect` flag …
//
// So for the eight, the program cannot express either shape, for any caller, and nothing in Go is
// load-bearing for that. THAT IS WHY THE LIST BELOW IS SHORT AND SPECIFIC rather than a restatement of
// what protection already does.
//
// ── WHAT IS ACTUALLY LEFT, MEASURED RATHER THAN ASSUMED ─────────────────────────────────────────
//
//  1. THE THREE DELIBERATELY UNPROTECTED VOLUMES. Any plan that deletes or replaces
//     `temporal-dynamicconfig`, `victorialogs-data` or `victoriametrics-data` executes without a word
//     from pulumi — measured above, exit 0. Two of those hold data that is not rebuildable, only
//     cheap; the gate is the whole of what stands in front of them, and it says so when it refuses.
//
//  2. A CHANGE IN WHAT MOUNTS A VOLUME, WHICH PRODUCES NO VOLUME STEP AT ALL. This is §7's "comes up
//     healthy" case and it is the one protection structurally cannot reach: move
//     `${volSeaweedData.name}` from the seaweed container to another service and the plan is a
//     container update. No delete, no replace, nothing to protect, nothing to refuse — and 34 GB of
//     parquet is now attached to something that will never read it while the service that wrote it
//     starts on an empty disk. Nothing fails. So this file asserts the invariant DIRECTLY — every
//     volume this installation has is still declared, still under the same resource name, still
//     mounted by the same services — rather than inferring safety from what a plan does not contain.
//
//  3. A STATE WHOSE PROTECTION WAS REMOVED BY HAND. `pulumi state unprotect <urn>` is exactly what
//     pulumi's own message recommends, and it is recorded in the checkpoint rather than in the program
//     — so the program can say `protect: true` while the installation no longer enforces it. This
//     reads both sides and the report names the difference.
//
//  4. LEGIBILITY, FOR ALL OF THE ABOVE AND FOR THE PROTECTED EIGHT AS WELL. Read the measured errors
//     again: they name a URN, and the remedy they offer is the command that REMOVES the protection. A
//     person who trips this has to be told which docker volume, what is in it, and that the answer is
//     a migration rather than an `unprotect`. And they must be told BEFORE the converge starts, not by
//     a failure part-way through it.
//
// ── WHERE EACH SIDE COMES FROM, AND WHY NEITHER IS TYPED IN GO ───────────────────────────────────
//
// DECLARED comes from the program's own YAML — the same bytes the engine is about to converge, so
// there is no second list to drift. RECORDED comes from `pulumi stack export`, which is the
// installation's own answer to "what do I have". A Go table of expected volume names would pass
// forever after somebody renamed one in both places, which is precisely the edit that loses the data.
package hostengine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The type names, in BOTH spellings, because a Pulumi type is written one way in a program and
// another in state. MEASURED on 3.244.0: the program says `docker:index:Volume` and the checkpoint
// and every preview step say `docker:index/volume:Volume`. Reading only one of them is how a sweep
// silently matches nothing.
const (
	programVolumeType    = "docker:index:Volume"
	stateVolumeType      = "docker:index/volume:Volume"
	programContainerType = "docker:index:Container"
	stateContainerType   = "docker:index/container:Container"
)

// Volume is one docker volume's IDENTITY — the three things that have to still be true of it.
type Volume struct {
	// Logical is the Pulumi resource name: `volSeaweedData` in the program, the last segment of the
	// URN in state. IT IS PART OF THE IDENTITY AND NOT A LABEL: Pulumi addresses a resource by URN,
	// so renaming the resource and keeping the docker name is a DELETE plus a CREATE of the same
	// underlying volume — which `retainOnDelete` turns into "dropped from state, left on disk", i.e.
	// an install that no longer knows its own data exists.
	Logical string
	// Name is the docker volume name — `kontra_seaweed-data`. This is what holds the bytes.
	Name string
	// Protected is `protect: true`. Read from both sides: the program's declaration is the intent, and
	// state's copy is what pulumi will actually enforce, and `pulumi state unprotect` can separate
	// them.
	Protected bool
	// Services are the CONTAINER NAMES that mount it, sorted. `kontra-postgres`, not `postgres`.
	Services []string
}

// VolumeSet is one side of the comparison, indexed both ways because the two failures are found by
// different keys: a rename of the docker name is found by logical name, and a rename of the resource
// is found by docker name.
type VolumeSet struct {
	ByName    map[string]Volume
	ByLogical map[string]Volume
	// Converged belongs to the RECORDED side only, and is false for a stack that has never run.
	// MEASURED: `pulumi stack export` on a stack created by `stack select --create` and never converged
	// returns a deployment with `manifest`, `secrets_providers` and `metadata` and NO `resources` key
	// at all. That is not an install with zero volumes, it is an install that has not happened — and
	// the difference decides whether `kontra update` has anything to protect or is being asked to do an
	// install under another name.
	Converged bool
}

func newVolumeSet() VolumeSet {
	return VolumeSet{ByName: map[string]Volume{}, ByLogical: map[string]Volume{}}
}

func (s VolumeSet) add(v Volume) {
	sort.Strings(v.Services)
	s.ByName[v.Name] = v
	s.ByLogical[v.Logical] = v
}

// Names lists the docker volume names, sorted, so every message this file produces is in a stable
// order.
func (s VolumeSet) Names() []string {
	out := make([]string, 0, len(s.ByName))
	for n := range s.ByName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Protected counts the volumes carrying `protect: true`.
func (s VolumeSet) Protected() int {
	n := 0
	for _, v := range s.ByName {
		if v.Protected {
			n++
		}
	}
	return n
}

// Unprotected lists the docker names that do not carry `protect: true`, sorted.
//
// EXPORTED FOR A TEST, and the test is the point: ADR 0052 §7 requires an unprotected volume to be a
// DECISION, so `volumes_test.go` asserts the exact set and fails when it changes in either direction.
// A volume that quietly loses its protection and a volume that quietly gains it are both edits
// somebody has to see.
func (s VolumeSet) Unprotected() []string {
	var out []string
	for n, v := range s.ByName {
		if !v.Protected {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// --- the declared side: the program's own YAML ---------------------------------------------------

// mountRe reads `${volSeaweedData.name}` — a Pulumi YAML interpolation of one resource's output —
// back to the resource it names.
//
// THE MOUNT IS THE ATTACHMENT, and it is an interpolation rather than a literal in every one of the
// program's 14 mounts. Resolving it is what makes "still attached to the same service" a question
// this file can answer about the DECLARED side at all; state answers it with the resolved docker
// name, which is why both are normalised to the docker name below.
var mountRe = regexp.MustCompile(`^\$\{([A-Za-z0-9_]+)\.name\}$`)

// programDoc is the part of a Pulumi YAML program this file reads. Nothing else is touched: a program
// is 1200 lines of topology and the four fields below are the whole of what a volume's identity is
// made of.
type programDoc struct {
	Name      string                  `yaml:"name"`
	Resources map[string]programResrc `yaml:"resources"`
}

type programResrc struct {
	Type       string `yaml:"type"`
	Properties struct {
		Name    string `yaml:"name"`
		Volumes []struct {
			VolumeName string `yaml:"volumeName"`
		} `yaml:"volumes"`
	} `yaml:"properties"`
	Options struct {
		Protect        bool `yaml:"protect"`
		RetainOnDelete bool `yaml:"retainOnDelete"`
	} `yaml:"options"`
}

// DeclaredVolumes reads the volumes and their mounts out of the program.
//
// A PROGRAM WITH NO VOLUMES IN IT IS AN ERROR AND NOT AN EMPTY ANSWER. Every check in this file is of
// the form "everything recorded is still declared", and that is trivially true of nothing declared —
// so a program the extraction cannot read would turn the whole gate into a pass, silently, forever.
// That is the same failure mode `ParseSummary` refuses for `changeSummary`, for the same reason: the
// failure mode of a gate is passing.
func DeclaredVolumes(program []byte) (VolumeSet, error) {
	var doc programDoc
	if err := yaml.Unmarshal(program, &doc); err != nil {
		return VolumeSet{}, fmt.Errorf("reading the volumes out of %s: %w", ProgramFile, err)
	}
	set := newVolumeSet()
	for name, r := range doc.Resources {
		if r.Type != programVolumeType {
			continue
		}
		if r.Properties.Name == "" {
			return VolumeSet{}, fmt.Errorf("%s declares volume %s with no `name:` — a docker volume "+
				"with no name is one Pulumi invents a name for, and the next converge would not adopt "+
				"the operator's data", ProgramFile, name)
		}
		set.add(Volume{Logical: name, Name: r.Properties.Name, Protected: r.Options.Protect})
	}
	if len(set.ByName) == 0 {
		return VolumeSet{}, fmt.Errorf("no %s resources found in %s. The volume gate compares what this "+
			"installation HAS against what the program declares, so an extraction that finds nothing "+
			"declared makes every check below pass — which is the one wrong answer this gate cannot "+
			"notice, so it refuses instead", programVolumeType, ProgramFile)
	}
	// The attachments, in a second pass, because a container may be declared above the volume it
	// mounts and a map has no order anyway.
	for _, r := range doc.Resources {
		if r.Type != programContainerType || r.Properties.Name == "" {
			continue
		}
		for _, m := range r.Properties.Volumes {
			ref := m.VolumeName
			if ref == "" {
				continue
			}
			// An interpolation resolves through the logical name; anything else is read as a literal
			// docker name, which is what a hand-edited program would carry.
			if g := mountRe.FindStringSubmatch(ref); g != nil {
				v, ok := set.ByLogical[g[1]]
				if !ok {
					continue // a mount of something that is not a volume of ours (a bind mount, say)
				}
				ref = v.Name
			}
			if v, ok := set.ByName[ref]; ok {
				v.Services = append(v.Services, r.Properties.Name)
				set.add(v)
			}
		}
	}
	return set, nil
}

// ProgramDeclares is the program's own `name:`, so a caller that already has the bytes does not have
// to re-read the file to find out which project they are.
func ProgramDeclares(program []byte) string { return programName(program) }

// --- the recorded side: `pulumi stack export` ----------------------------------------------------

// exportDoc is the shape MEASURED on 3.244.0: `{"version":3,"deployment":{"manifest":…,
// "secrets_providers":…,"metadata":…,"resources":[…]}}`, each resource carrying `urn`, `type`,
// `custom`, `inputs`, `outputs`, and `protect`/`retainOnDelete` only when they are true.
type exportDoc struct {
	Version    int `json:"version"`
	Deployment *struct {
		Resources []exportResrc `json:"resources"`
	} `json:"deployment"`
}

type exportResrc struct {
	URN     string `json:"urn"`
	Type    string `json:"type"`
	Protect bool   `json:"protect"`
	// Delete marks a resource pending deletion — a converge that was interrupted between deleting the
	// thing and writing the checkpoint. It is NOT part of what this installation has, and counting it
	// would refuse an update for the leftovers of a Ctrl-C.
	Delete             bool `json:"delete"`
	PendingReplacement bool `json:"pendingReplacement"`
	Inputs             struct {
		Name    string `json:"name"`
		Volumes []struct {
			VolumeName string `json:"volumeName"`
		} `json:"volumes"`
	} `json:"inputs"`
	Outputs struct {
		Name string `json:"name"`
	} `json:"outputs"`
}

func (r exportResrc) name() string {
	if r.Outputs.Name != "" {
		return r.Outputs.Name
	}
	return r.Inputs.Name
}

// RecordedVolumes reads what this installation actually has out of a `pulumi stack export`.
//
// AN EXPORT WITH NO `resources` IS A STACK THAT HAS NEVER CONVERGED, and that is reported as
// `Converged: false` rather than as zero volumes — measured, see VolumeSet.Converged. An
// UNPARSEABLE export is an error: it is the only record of what this install has, and reading a
// corrupt one as "nothing to protect" is how an update proceeds past the thing it exists to check.
func RecordedVolumes(export []byte) (VolumeSet, error) {
	var doc exportDoc
	if err := json.Unmarshal(export, &doc); err != nil {
		return VolumeSet{}, fmt.Errorf("`pulumi stack export` produced something this cannot read: %w.\n"+
			"  That file is the only record of which docker volumes this installation owns, so it is "+
			"refused rather than read as an install with nothing to lose", err)
	}
	set := newVolumeSet()
	if doc.Deployment == nil || len(doc.Deployment.Resources) == 0 {
		return set, nil // Converged stays false
	}
	set.Converged = true
	for _, r := range doc.Deployment.Resources {
		if r.Type != stateVolumeType || r.Delete || r.PendingReplacement {
			continue
		}
		if n := r.name(); n != "" {
			set.add(Volume{Logical: urnName(r.URN), Name: n, Protected: r.Protect})
		}
	}
	for _, r := range doc.Deployment.Resources {
		if r.Type != stateContainerType || r.Delete {
			continue
		}
		for _, m := range r.Inputs.Volumes {
			// IN STATE THE MOUNT IS ALREADY RESOLVED — measured: a container's `inputs.volumes[]`
			// carries `"volumeName": "kontra_seaweed-data"`, the docker name, never the `${…}` the
			// program wrote. That is why both sides of the comparison are keyed by docker name.
			if v, ok := set.ByName[m.VolumeName]; ok {
				v.Services = append(v.Services, r.name())
				set.add(v)
			}
		}
	}
	return set, nil
}

// --- the gate ------------------------------------------------------------------------------------

// CheckVolumeIdentity refuses a converge that would not leave every recorded volume exactly where it
// is.
//
// THE DIRECTION OF THE COMPARISON IS THE WHOLE DESIGN. It walks what the installation HAS and asks
// the program about each one — never the other way round. A declared volume that is not recorded is
// ordinary (it is a first converge, or a volume added by an upgrade) and is reported as a candidate
// rather than refused; a RECORDED volume the program no longer accounts for is data with no home.
//
// THREE FINDINGS, and each is a different way to lose a volume without a delete appearing anywhere:
//
//  1. GONE — recorded, and the program declares no volume with that docker name. The plan for this is
//     a delete (which protection refuses for eight of the eleven) or, if a same-purpose volume was
//     added under a new name, a CREATE with the old one simply unmentioned. Both names are printed.
//  2. RE-KEYED — same docker name, different Pulumi resource name. No data is at risk on disk and the
//     install stops knowing it owns the volume: Pulumi plans a delete of the old URN and a create of
//     the new one, `retainOnDelete` leaves the bytes behind, and the next `down` has nothing to keep.
//  3. MOVED — same volume, different services mounting it. This is the one that comes up healthy:
//     `seaweed-data` attached to a container that is not seaweed is 34 GB of parquet nothing reads.
func CheckVolumeIdentity(declared, recorded VolumeSet) error {
	if !recorded.Converged {
		return nil // nothing recorded yet; there is nothing to lose and nothing to assert
	}
	var findings []string
	// The rename suspects: declared, not recorded. Computed once so a "gone" finding can name them.
	var added []string
	for _, n := range declared.Names() {
		if _, ok := recorded.ByName[n]; !ok {
			added = append(added, n)
		}
	}
	for _, n := range recorded.Names() {
		rec := recorded.ByName[n]
		dec, ok := declared.ByName[n]
		if !ok {
			f := fmt.Sprintf("%s is RECORDED IN THIS INSTALLATION AND THE PROGRAM NO LONGER DECLARES IT.\n"+
				"      Pulumi's desired state is total: a volume the program stops mentioning is a volume it\n"+
				"      deletes (ADR 0052 §1). The data is in docker volume %q right now.", n, n)
			if len(added) > 0 {
				f += fmt.Sprintf("\n      The program DOES declare %d volume(s) this installation has never had: %s.\n"+
					"      If one of those is meant to be %s under a new name, then this converge creates an\n"+
					"      empty volume and orphans the full one — which is a plan containing no delete at all,\n"+
					"      and is the exact failure this gate exists for. Renaming a volume is a migration:\n"+
					"      copy the data across and then change the program, in that order.",
					len(added), strings.Join(added, ", "), n)
			}
			findings = append(findings, f)
			continue
		}
		if dec.Logical != rec.Logical {
			findings = append(findings, fmt.Sprintf("%s is still declared and its PULUMI RESOURCE NAME changed: "+
				"%s -> %s.\n"+
				"      Pulumi addresses a resource by URN, so that is a delete of %s and a create of %s for one\n"+
				"      docker volume. With retainOnDelete the bytes survive and the installation stops knowing\n"+
				"      it owns them: nothing adopts %q on the next converge, and `kontra control down` has\n"+
				"      nothing to keep. Rename the resource in a migration or not at all.",
				n, rec.Logical, dec.Logical, rec.Logical, dec.Logical, n))
			continue
		}
		// WHAT IS REFUSED IS A VOLUME LEAVING THE SERVICE THAT WROTE IT, not every change to the list.
		//
		// The hazard this branch describes is one-directional: a volume handed to a service that did not
		// write it reads as an empty disk there and as missing data to nobody. A service ADDED beside the
		// ones that already mount it cannot produce that — the writer keeps its volume, no bytes move,
		// and the new reader sees exactly what is there.
		//
		// The distinction is load-bearing rather than fastidious. Comparing the two lists for EQUALITY
		// makes "mount the old registry store read-only into the one-shot that checks whether it has been
		// migrated" indistinguishable from "move the registry's data to a different service" — so the
		// only way to add a read-only reader is to stop converging. A gate that cannot be satisfied is one
		// that gets turned off.
		if lost := servicesLost(dec.Services, rec.Services); len(lost) > 0 {
			findings = append(findings, fmt.Sprintf("%s is still declared and A SERVICE THAT MOUNTS IT NO "+
				"LONGER DOES: %s -> %s, losing %s.\n"+
				"      This is the failure that comes up healthy. A volume attached to a service that is not the\n"+
				"      one that wrote it reads as an empty disk to the new service and as missing data to\n"+
				"      nobody — no container fails, no step is destructive, and the old contents sit where\n"+
				"      nothing looks.",
				n, mountList(rec.Services), mountList(dec.Services), mountList(lost)))
		}
	}
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to converge: %d of this installation's %d volumes would not survive it as "+
		"themselves.\n", len(findings), len(recorded.ByName))
	for _, f := range findings {
		fmt.Fprintf(&b, "\n  - %s\n", f)
	}
	b.WriteString("\n  Nothing has been applied. A `pulumi stack export` is this installation's own record " +
		"of which\n  docker volumes it owns and what mounts them; `kontra update` writes one before every " +
		"converge\n  and prints the path.")
	return fmt.Errorf("%s", b.String())
}

// CheckPlanKeepsEveryVolume refuses a plan that would delete or replace a volume, and NAMES THE
// VOLUME.
//
// THIS IS THE LEGIBILITY HALF AND NOT THE SAFETY HALF. `protect: true` already makes the plan
// unexecutable for the eight volumes that hold something — measured, it fails in `preview`, before any
// converge — so this function is almost never the thing that stops the damage. What it is for is the
// sentence. Pulumi's own refusal reads
//
//	resource "urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData"
//	cannot be deleted because it is protected. To unprotect the resource … `pulumi state unprotect`
//
// which names a URN and offers, as its remedy, the command that removes the protection. A person who
// trips this needs to be told which docker volume, what is in it, and that the remedy is not
// `unprotect`. It also covers the three volumes that are deliberately unprotected, where pulumi will
// not refuse at all.
//
// A REPLACE COUNTS. Replacing a docker volume destroys and recreates it — the bytes do not come back
// — and Pulumi spells a replacement three ways in a step stream, so the match is on the substring
// rather than on an enumeration of a vocabulary this repository does not version (same rule as
// Summary.Replaced).
func CheckPlanKeepsEveryVolume(sum *Summary, declared, recorded VolumeSet) error {
	if sum == nil {
		return nil
	}
	seen := map[string]bool{}
	var hits []string
	for _, c := range sum.Changes {
		dec, isDeclared := declared.ByLogical[c.Name]
		// A step's type is the authority; the logical name is the fallback for a document whose steps
		// carry no state (ParseSummary's own fallback path), where a volume is only recognisable by
		// being one of the program's volume resources.
		if !isVolumeType(c.Type) && !isDeclared {
			continue
		}
		if !strings.HasPrefix(c.Op, "delete") && !strings.Contains(c.Op, "replac") {
			continue
		}
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		// THE DOCKER NAME COMES FROM WHICHEVER SIDE STILL HAS IT, and the recorded side is asked first.
		// A delete step for a volume the program DROPPED has no declaration left to read, and naming
		// the docker volume the bytes are in is the whole point of this sentence — so the case with no
		// declaration is the case that matters most.
		dockerName := c.Name
		if v, ok := recorded.ByLogical[c.Name]; ok && v.Name != "" {
			dockerName = v.Name
		}
		protected := ""
		if isDeclared {
			dockerName = dec.Name
			if dec.Protected {
				protected = "\n      It carries `protect: true`, so pulumi refuses this plan as well — that is the\n" +
					"      diagnostic below, and it names a URN where this names the volume."
			} else {
				protected = "\n      IT IS DELIBERATELY NOT PROTECTED in the program, so pulumi will execute this plan\n" +
					"      without complaint. This refusal is the only thing in the way."
			}
		}
		hits = append(hits, fmt.Sprintf("%s (docker volume %q) would be %s.%s",
			c.Name, dockerName, opPhrase(c.Op), protected))
	}
	if len(hits) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to converge: the plan destroys %d volume(s).\n", len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "\n  - %s\n", h)
	}
	b.WriteString("\n  A volume is where this installation's data is: the DuckLake catalog and the parquet " +
		"it points\n  at, every credential, the console's login hashes, and the record of every cloud Machine " +
		"the\n  control plane owns. Nothing has been applied.\n" +
		"  If a volume really is meant to go, `docker volume rm` is a person's decision and not a " +
		"converge's\n  (control/pulumi/README.md), and unprotecting one to let a converge do it removes the " +
		"guarantee\n  for every future converge as well.")
	return fmt.Errorf("%s", b.String())
}

func isVolumeType(t string) bool {
	return t == stateVolumeType || t == programVolumeType
}

// opPhrase says what a step DOES to a volume, in a sentence rather than in pulumi's vocabulary.
//
// PULUMI SPELLS A REPLACEMENT THREE WAYS in a step stream and each of them means "these bytes do not
// come back", which is the only thing the reader needs. An unrecognised op is quoted verbatim — the
// default is deliberately not a guess, because the whole file's rule is that pulumi's words are quoted
// and never parsed, and a vocabulary this repository does not version WILL grow a word.
func opPhrase(op string) string {
	switch op {
	case "delete":
		return "DELETED"
	case "delete-replaced":
		return "DELETED, as the old half of a replacement"
	case "create-replacement":
		return "RECREATED EMPTY, as the new half of a replacement"
	case "replace":
		return "REPLACED — destroyed and created again, empty"
	default:
		return "the subject of pulumi's step \"" + op + "\", which destroys it"
	}
}

// servicesLost reports the services in `rec` that `dec` no longer lists — the recorded mounters a
// converge would take the volume away from. An empty result means every service that had it keeps it,
// whatever else was added.
func servicesLost(dec, rec []string) []string {
	keep := make(map[string]struct{}, len(dec))
	for _, s := range dec {
		keep[s] = struct{}{}
	}
	var lost []string
	for _, s := range rec {
		if _, ok := keep[s]; !ok {
			lost = append(lost, s)
		}
	}
	return lost
}

func mountList(s []string) string {
	if len(s) == 0 {
		return "(nothing)"
	}
	return strings.Join(s, "+")
}
