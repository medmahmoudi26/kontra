package hostengine

import (
	"strings"
	"testing"
)

// THE IMAGE KEYS ARE READ OUT OF THE COMMITTED PROGRAM, not listed here.
//
// The assertion is a PROPERTY and a floor, never an enumeration: `logshipImage` arrived while this
// slice was being written (logship stopped being stock `python:3.12-alpine` and became a kontra-owned
// image), and a test that had listed four keys would have gone green while `--to` moved four images out
// of five. The floor catches an extraction that stops matching; the property catches a reference that
// is not one.
func TestImageRefsComeOutOfTheProgram(t *testing.T) {
	refs, err := ImageRefs(realProgram(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 4 {
		t.Fatalf("only %d *Image config keys found in the program (%v) — the extraction is not reading it, "+
			"and `--to` would then retag a subset", len(refs), ImageKeys(refs))
	}
	for _, k := range ImageKeys(refs) {
		if !strings.HasSuffix(k, ImageKeySuffix) {
			t.Errorf("%s is not an image key", k)
		}
		if _, err := Retag(refs[k], "probe"); err != nil {
			t.Errorf("%s = %q is not a reference `--to` can retag: %v", k, refs[k], err)
		}
	}
	// The four the program has had since it was written; a fifth or a sixth is fine, one of these
	// disappearing is a service whose image `--to` would silently stop moving.
	for _, k := range []string{"kontraImage", "orchestratorImage", "hostImage", "workerBaseImage"} {
		if _, ok := refs[k]; !ok {
			t.Errorf("%s is gone from the program's config keys", k)
		}
	}
}

// A PROGRAM WITH NO IMAGE KEYS MUST REFUSE. `--to v1.4.0` that found nothing to retag would converge
// the program's own defaults and report it as having moved to that release, which is the wrong answer
// and the quiet one.
func TestRetaggingNothingIsRefused(t *testing.T) {
	if _, err := ImageRefs([]byte("name: kontra-control\nconfig:\n  bind:\n    default: 127.0.0.1\n")); err == nil {
		t.Error("a program with no image keys was accepted, so `--to` would have been a no-op described " +
			"as an upgrade")
	}
}

// THE REGISTRY PORT IS THE TRAP, and `internal/ociref` already holds the rule that survives it: the tag
// is the last `:` with no `/` after it. Re-deriving that here is how `--to` turns
// `127.0.0.1:5000/kontra` into a repository called `127.0.0.1` at tag `5000/kontra`.
func TestRetagUsesTheRepositorysOwnGrammar(t *testing.T) {
	for _, c := range []struct{ in, tag, want string }{
		{"ghcr.io/medmahmoudi26/kontra:dev", "v1.4.0", "ghcr.io/medmahmoudi26/kontra:v1.4.0"},
		{"kontra-host:1", "v1.4.0", "kontra-host:v1.4.0"},
		{"kontra", "v1.4.0", "kontra:v1.4.0"}, // no tag written at all
		{"127.0.0.1:5000/kontra:dev", "v1.4.0", "127.0.0.1:5000/kontra:v1.4.0"},
		{"localhost:5000/a/b", "sha-abc", "localhost:5000/a/b:sha-abc"},
		// A DIGEST IS DROPPED: `--to` names a release, and a reference pinned to a digest cannot also be
		// at that tag. Keeping both sends the provider the digest and ignores what was asked for.
		{"ghcr.io/x/kontra@sha256:" + strings.Repeat("a", 64), "v2", "ghcr.io/x/kontra:v2"},
	} {
		got, err := Retag(c.in, c.tag)
		if err != nil {
			t.Errorf("Retag(%q, %q): %v", c.in, c.tag, err)
			continue
		}
		if got != c.want {
			t.Errorf("Retag(%q, %q) = %q, want %q", c.in, c.tag, got, c.want)
		}
	}
}

// A TAG THAT IS NOT A TAG IS REFUSED BY THE GRAMMAR, not by a second opinion invented here — and the
// refusal is ociref's, which names the part that is wrong.
func TestRetagRefusesSomethingThatIsNotATag(t *testing.T) {
	for _, bad := range []string{"", "  ", "a/b", "v1:2", "-leading-dash", strings.Repeat("x", 200)} {
		if got, err := Retag("ghcr.io/x/kontra:dev", bad); err == nil {
			t.Errorf("--to %q was accepted and produced %q", bad, got)
		}
	}
}

// ALL OR NOTHING. A partial retag is an install running four images from one release and one from
// another — ADR 0038's single tag exists so the control plane's pieces are known to have been built
// together.
func TestRetagAllIsAllOrNothing(t *testing.T) {
	refs := map[string]string{
		"kontraImage":       "ghcr.io/x/kontra:dev",
		"orchestratorImage": "ghcr.io/x/kontra-orchestrator:dev",
	}
	out, err := RetagAll(refs, "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range out {
		if !strings.HasSuffix(v, ":v1.4.0") {
			t.Errorf("%s = %q did not move", k, v)
		}
	}
	if _, err := RetagAll(refs, "not a tag"); err == nil {
		t.Fatal("a bad tag was applied to some images")
	} else if !strings.Contains(err.Error(), "orchestratorImage") && !strings.Contains(err.Error(), "kontraImage") {
		t.Errorf("the refusal must name the key it failed on: %v", err)
	}
}
