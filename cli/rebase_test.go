package main

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// "Rebase available" is a COMPARISON: the runtime digest a build recorded against what that runtime's
// major resolves to now. The failures worth testing are the two directions of being wrong — rewriting
// a manifest nobody asked to move, and missing one that is behind.

const (
	oldRT = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	newRT = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	imgD  = "sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"
)

func rec(name, version, rtName string, major uint32, rtDigest, digest string) actorRecord {
	a := actorRecord{Key: name + "@" + version, Name: name, Version: version, Digest: digest}
	if rtName != "" {
		a.Runtime = &struct {
			Name   string `json:"name"`
			Major  uint32 `json:"major"`
			Digest string `json:"digest"`
		}{Name: rtName, Major: major, Digest: rtDigest}
	}
	return a
}

// resolver returns one runtime for every declaration, which is the "a patch was published" world.
func resolver(digest string) func(string) (resolvedRuntime, error) {
	return func(decl string) (resolvedRuntime, error) {
		name, major, _, err := parseRuntimeDeclaration(decl, "127.0.0.1:5000")
		if err != nil {
			return resolvedRuntime{}, err
		}
		return resolvedRuntime{Name: name, Major: major, Ref: "r/" + name + ":1", Digest: digest}, nil
	}
}

func TestAVersionOnTheCurrentRuntimeDigestIsNotBehind(t *testing.T) {
	// The common case by a wide margin, and rewriting a manifest here would produce a new digest for
	// an image that did not change — which the catalog would then record as a move.
	got, err := behindRuntimes(
		[]actorRecord{rec("demo", "1.0.0", "python", 1, newRT, imgD)},
		rebaseFilter{}, "reg:5000", resolver(newRT), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a version already on the current digest was listed as behind: %+v", got)
	}
}

func TestAVersionOnAnOlderRuntimeDigestIsBehind(t *testing.T) {
	got, err := behindRuntimes(
		[]actorRecord{rec("demo", "1.0.0", "python", 1, oldRT, imgD)},
		rebaseFilter{}, "reg:5000", resolver(newRT), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Was != oldRT || got[0].Runtime.Digest != newRT {
		t.Errorf("the row should carry both digests: was=%s now=%s", short(got[0].Was), short(got[0].Runtime.Digest))
	}
	// The image reference is what `pack rebase` is given, so it has to be the ACTOR's, not the runtime's.
	if want := "reg:5000/demo:1.0.0"; got[0].Image != want {
		t.Errorf("image: got %s want %s", got[0].Image, want)
	}
}

func TestAnActorWithNoRecordedRuntimeIsSkippedRatherThanGuessedAt(t *testing.T) {
	// Built before the field existed. Rebasing it would mean choosing a runtime on its author's behalf
	// and putting an OS under their actor that they never named.
	got, err := behindRuntimes(
		[]actorRecord{rec("legacy", "1.0.0", "", 0, "", imgD)},
		rebaseFilter{}, "reg:5000", resolver(newRT), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an actor with no recorded runtime was offered for rebase: %+v", got)
	}
}

func TestAnActorWithNoImageDigestIsSkipped(t *testing.T) {
	// A dev actor that was never pushed. There is no manifest to rewrite.
	got, _ := behindRuntimes(
		[]actorRecord{rec("demo", "1.0.0", "python", 1, oldRT, "")},
		rebaseFilter{}, "reg:5000", resolver(newRT), io.Discard)
	if len(got) != 0 {
		t.Errorf("an actor with no image digest was offered for rebase: %+v", got)
	}
}

func TestAnUnresolvableRuntimeIsNotedAndNotRebased(t *testing.T) {
	// An actor running on an image its own registry can no longer name. It is not a candidate — there
	// is nothing to move it onto — and it must not be silence either.
	var notes strings.Builder
	got, err := behindRuntimes(
		[]actorRecord{rec("demo", "1.0.0", "python", 1, oldRT, imgD)},
		rebaseFilter{}, "reg:5000",
		func(string) (resolvedRuntime, error) { return resolvedRuntime{}, fmt.Errorf("manifest unknown") },
		&notes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an unresolvable runtime produced a rebase candidate: %+v", got)
	}
	for _, want := range []string{"demo@1.0.0", "cannot be resolved"} {
		if !strings.Contains(notes.String(), want) {
			t.Errorf("the note should mention %q: %s", want, notes.String())
		}
	}
}

func TestFilteringByActorAndByVersion(t *testing.T) {
	all := []actorRecord{
		rec("alpha", "1.0.0", "python", 1, oldRT, imgD),
		rec("alpha", "2.0.0", "python", 1, oldRT, imgD),
		rec("beta", "1.0.0", "python", 1, oldRT, imgD),
	}
	byActor, _ := behindRuntimes(all, rebaseFilter{Actor: "alpha"}, "reg:5000", resolver(newRT), io.Discard)
	if len(byActor) != 2 {
		t.Errorf("--actor alpha: got %d want 2", len(byActor))
	}
	byVersion, _ := behindRuntimes(all, rebaseFilter{Actor: "alpha", Version: "2.0.0"}, "reg:5000", resolver(newRT), io.Discard)
	if len(byVersion) != 1 || byVersion[0].Version != "2.0.0" {
		t.Errorf("alpha@2.0.0: got %+v", byVersion)
	}
}

func TestFilteringByRuntime(t *testing.T) {
	all := []actorRecord{
		rec("alpha", "1.0.0", "python", 1, oldRT, imgD),
		rec("beta", "1.0.0", "python-browser", 1, oldRT, imgD),
	}
	got, _ := behindRuntimes(all, rebaseFilter{Runtime: "python-browser", Major: 1}, "reg:5000", resolver(newRT), io.Discard)
	if len(got) != 1 || got[0].Actor != "beta" {
		t.Errorf("--runtime python-browser:1: got %+v", got)
	}
	// A different MAJOR of the same name is a different runtime and must not be swept in.
	none, _ := behindRuntimes(all, rebaseFilter{Runtime: "python", Major: 2}, "reg:5000", resolver(newRT), io.Discard)
	if len(none) != 0 {
		t.Errorf("python:2 matched versions built on python:1: %+v", none)
	}
}

func TestTheListIsOrderedSoTwoRunsReadTheSame(t *testing.T) {
	all := []actorRecord{
		rec("zeta", "1.0.0", "python", 1, oldRT, imgD),
		rec("alpha", "2.0.0", "python", 1, oldRT, imgD),
		rec("alpha", "1.0.0", "python", 1, oldRT, imgD),
	}
	got, _ := behindRuntimes(all, rebaseFilter{}, "reg:5000", resolver(newRT), io.Discard)
	var names []string
	for _, r := range got {
		names = append(names, r.Actor+"@"+r.Version)
	}
	want := []string{"alpha@1.0.0", "alpha@2.0.0", "zeta@1.0.0"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("order:\n  got  %v\n  want %v", names, want)
	}
}

func TestNamingBothAnActorAndARuntimeIsRefused(t *testing.T) {
	// Two answers to one question. Resolving it by precedence is how an operator rebases a fleet when
	// they meant one actor.
	err := cmdRebase([]string{"--runtime", "python:1", "demo"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("naming both was accepted: %v", err)
	}
}
