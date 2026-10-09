package main

// build_test.go — `kontra build --push <ref>`, and the flag that stopped meaning anything.
//
// Two things are pinned here and each one is a failure this repo has actually had:
//
//  1. A RETIRED FLAG IS REFUSED, AND WHAT THE REFUSAL TELLS YOU TO TYPE RUNS. `--target` is gone
//     (ADR 0036), and `flag provided but not defined: -target` reads as a typo — so the refusal
//     names the command that does what was meant. That command is checked by handing it to the
//     CLI (`advice_test.go`), not by reading its words.
//  2. TWO ANSWERS TO ONE QUESTION ARE REFUSED, not silently resolved. `--push` is the whole
//     reference and `--registry` is only its first component.
//
// The GitHub Action and the GitLab template are product surface too, and they are checked the way
// a pipeline meets them: ci.yml's `build-actor` job RUNS both, as written, against a local
// registry. A flag either of them stops having fails that job.

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// TestTargetIsRefusedAndItsAdviceRuns — a retired flag is not a typo and must not read as one, and
// the command it sends you to has to be one the CLI accepts.
func TestTargetIsRefusedAndItsAdviceRuns(t *testing.T) {
	dir := t.TempDir()

	for _, value := range []string{"container", "machine", "nonsense"} {
		err := cmdBuild([]string{"--actor", dir, "--target", value})
		// THE REFUSAL FIRED, and fired FIRST. `dir` holds no actor.json, so any later error — the
		// manifest read, or the stock `flag` error had the flag been dropped — is a different value.
		// A retired flag that still works builds the other thing quietly; one that falls through to
		// the stock error sends an operator to check their spelling.
		if want := retiredTarget(value); err == nil || err.Error() != want.Error() {
			t.Errorf("--target %s = %v, want the retirement refusal", value, err)
			continue
		}
		advice := adviceIn(err.Error())
		if len(advice) == 0 {
			t.Errorf("--target %s is refused with no command to run instead: %v", value, err)
		}
		for _, argv := range advice {
			if e := adviceRuns(t, argv); e != nil {
				t.Errorf("--target %s advises a command that does not run: %v", value, e)
			}
			// …and the advice must not lead back here: a `kontra build` it names, parsed by the
			// command's own flag set, sets no --target.
			if argv[0] == "build" {
				f := buildFlagSet()
				f.fs.SetOutput(io.Discard)
				if e := f.fs.Parse(argv[1:]); e != nil || *f.target != "" {
					t.Errorf("--target %s advises `kontra %s`, which is this refusal again (%v)",
						value, strings.Join(argv, " "), e)
				}
			}
		}
	}

	// AND IT IS ONLY REFUSED WHEN WRITTEN. An empty `--target` is the zero value of a declared flag
	// and must not refuse a build nobody asked to target anything.
	err := cmdBuild([]string{"--actor", dir})
	for _, value := range []string{"", "container", "machine"} {
		if err != nil && err.Error() == retiredTarget(value).Error() {
			t.Errorf("a build with no --target was refused as though one had been passed: %v", err)
		}
	}
}

// TestPushAndRegistryAreTwoAnswersToOneQuestion. Resolving them by precedence would mean an operator
// who set KONTRA_REGISTRY months ago and types `--push ghcr.io/...` today gets one of the two
// silently, and the wrong one is a 65 MiB push to the wrong registry.
func TestPushAndRegistryAreTwoAnswersToOneQuestion(t *testing.T) {
	_, err := pushDestination("ghcr.io/acme/bundles/nscheck:0.1.0", "localhost:5000", "", "nscheck", "0.1.0")
	if err == nil {
		t.Fatal("--push and --registry together were accepted; one of them was silently ignored")
	}
	for _, want := range []string{"ghcr.io/acme/bundles/nscheck:0.1.0", "localhost:5000", "one question"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

// TestDefaultDestinationIsTheConventionalOne — with no `--push`, the address is the rung ladder
// `bundleRegistry` owns and the repository is `bundles/<name>`, which is the only address
// `control/orchestrator/src/activities/fleet.ts:resolveBundle` can cliutil.Derive. Changing this silently makes every
// build publish somewhere a Fleet placement then 404s on.
func TestDefaultDestinationIsTheConventionalOne(t *testing.T) {
	t.Setenv("KONTRA_HOME", t.TempDir())
	t.Setenv("KONTRA_REGISTRY", "")
	t.Setenv("KONTRA_CONTROLLER", "")

	dest, err := pushDestination("", "", "10.124.0.2", "nscheck", "0.1.0")
	if err != nil {
		t.Fatalf("the conventional destination was refused: %v", err)
	}
	if got, want := dest.Tagged(), "10.124.0.2:5000/bundles/nscheck:0.1.0"; got != want {
		t.Errorf("default destination = %q, want %q", got, want)
	}
	if !dest.PlainHTTP {
		t.Error("the Controller's own registry is plain HTTP; a build that spoke https to it would " +
			"fail on every Fleet")
	}

	// AN ACTOR WHOSE NAME CANNOT BE A REPOSITORY IS REFUSED ON THE DEFAULT PATH TOO, and with the
	// shared answer rather than a fourth message. This is the path every doc example takes.
	if _, err := pushDestination("", "", "10.124.0.2", "café", "0.1.0"); !errors.Is(err, ociref.ErrImageUnrepresentable) {
		t.Errorf("the default destination for `café` = %v, want ociref.ErrImageUnrepresentable", err)
	}

	// A VERSION IS NOT ALWAYS A TAG — shared/conformance/queues.json carries `1:2`, which Temporal takes
	// verbatim. Without the tag rule this builds a reference the registry rejects after the push.
	if _, err := pushDestination("", "", "10.124.0.2", "nscheck", "1:2"); !errors.Is(err, ociref.ErrImageUnrepresentable) {
		t.Errorf("version `1:2` = %v, want ociref.ErrImageUnrepresentable", err)
	}
}
