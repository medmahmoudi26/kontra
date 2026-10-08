// images.go — what `kontra update --to <tag>` changes, and what "re-resolve the digests" can and
// cannot mean on this engine.
//
// ── THE REFS ARE READ OUT OF THE PROGRAM AND RETAGGED, NEVER TYPED HERE ─────────────────────────
//
// `control/pulumi/Pulumi.yaml` declares one config key per kontra-owned image and each default is a
// full reference (`ghcr.io/medmahmoudi26/kontra-orchestrator:dev`). A Go table of those repositories
// would be a second statement of which registry an install pulls from, and the two would agree right
// up until somebody moved the registry in the program — which is the one edit that must reach every
// image at once. So `--to v1.4.0` READS the program's own defaults and replaces the tag on each,
// leaving the registry, the repository and the count of images to the program.
//
// ── AND WHAT A BARE `kontra update` CAN ACTUALLY RE-RESOLVE, STATED HONESTLY ────────────────────
//
// `Pulumi.yaml:169-175` records the measurement the whole image design rests on: `docker.RemoteImage`
// RESOLVES A LOCALLY PRESENT IMAGE WITHOUT CONSULTING A REGISTRY (0.15/0.35/0.53s against tags that
// cannot exist on docker.io). That is what makes one set of refs serve both the local build and the
// registry pull — and it is also the reason a bare `kontra update` against a FLOATING tag is not, by
// itself, a way to get new bytes: if `:dev` is already in this daemon at an older digest, the provider
// resolves that one and the converge is a no-op. Two honest consequences, both printed by the command:
//
//   - `--to <tag>` IS THE RELIABLE FORM. It changes the reference, so the name the provider resolves
//     is one the daemon does not have, and it pulls.
//   - a bare update re-resolves against THE DAEMON, which is why it passes `--refresh`: the recorded
//     digest is compared with what the daemon holds now rather than with the checkpoint. Getting new
//     bytes for an unchanged floating tag is a `docker pull` first, and that is the operator's, not
//     this command's — a CLI that pulled on your behalf would be deciding when to take an upgrade.
package hostengine

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// configDoc is the program's `config:` block. Only the default matters here: the key's presence is
// what makes it settable, and the default is the reference whose tag is being replaced.
type configDoc struct {
	Config map[string]struct {
		Type    string `yaml:"type"`
		Default string `yaml:"default"`
	} `yaml:"config"`
}

// ImageKeySuffix is what makes a config key an image reference. `kontraImage`, `orchestratorImage`,
// `logshipImage` — the program's own naming, and the reason this is a suffix rather than a list is
// that the set MOVES: `logshipImage` arrived while this slice was being written, and `hostImage`
// and `workerBaseImage` left with the classic build path. A list would have missed each change
// silently and `--to` would have moved some images and not others, which is worse than none.
const ImageKeySuffix = "Image"

// ImageRefs is every kontra-owned image reference the program declares, key -> default reference.
func ImageRefs(program []byte) (map[string]string, error) {
	var doc configDoc
	if err := yaml.Unmarshal(program, &doc); err != nil {
		return nil, fmt.Errorf("reading the image refs out of %s: %w", ProgramFile, err)
	}
	out := map[string]string{}
	for k, v := range doc.Config {
		if strings.HasSuffix(k, ImageKeySuffix) && v.Default != "" {
			out[k] = v.Default
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no *%s config keys with defaults found in %s, so `--to` has nothing to "+
			"retag. It is refused rather than converging the program's own defaults under the name of an "+
			"upgrade", ImageKeySuffix, ProgramFile)
	}
	return out, nil
}

// ImageKeys is ImageRefs' key set, sorted — for the line the command prints.
func ImageKeys(refs map[string]string) []string {
	out := make([]string, 0, len(refs))
	for k := range refs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Retag replaces a reference's tag, and it does it through the repository's OWN grammar.
//
// `internal/ociref` IS THAT GRAMMAR AND THERE IS NOT A SECOND ONE. Its header is explicit that
// splitting and judging are separate so a refusal can name the part that is wrong, and its `Split`
// already holds the two rules this function would otherwise have got wrong: the tag is the last `:`
// WITH NO `/` AFTER IT (so `127.0.0.1:5000/kontra:dev` does not lose its port) and the digest is cut
// at the first `@`. Re-deriving either here is how `--to` turns `localhost:5000/x` into
// `localhost:5000` plus a tag called `5000/x`.
//
// A DIGEST IS DROPPED, DELIBERATELY. `--to <tag>` names a release; a reference pinned to
// `@sha256:…` cannot also be at that tag, and keeping both would send the provider the digest and
// ignore what the operator asked for.
func Retag(ref, tag string) (string, error) {
	r := ociref.Split(ref)
	r.Digest = ""
	r.Tag, r.TagSet = strings.TrimSpace(tag), true
	out := r.Tagged()
	if err := r.Check(out); err != nil {
		return "", err
	}
	return out, nil
}

// RetagAll retags every reference, refusing on the first one that will not take the tag.
//
// ALL OR NOTHING. A partial retag is an install running four images from one release and one from
// another, which is the shape of failure nobody debugs quickly: ADR 0038's single tag exists so the
// control plane's pieces are known to have been built together.
func RetagAll(refs map[string]string, tag string) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	for _, k := range ImageKeys(refs) {
		v, err := Retag(refs[k], tag)
		if err != nil {
			return nil, fmt.Errorf("--to %q cannot be applied to %s (%s): %w", tag, k, refs[k], err)
		}
		out[k] = v
	}
	return out, nil
}
