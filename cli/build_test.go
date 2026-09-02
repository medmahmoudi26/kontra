package main

// build_test.go — `kontra build --push <ref>`, and the flag that stopped meaning anything.
//
// Three things are pinned here and each one is a failure this repo has actually had:
//
//  1. A RETIRED FLAG EXPLAINS ITSELF. `--target` is gone (ADR 0036), and `flag provided but not
//     defined: -target` reads as a typo — so `--target container` names `kontra deploy` and
//     `--target machine` names `--push`. `fleet_documented_flags_test.go` was written after three
//     dead flags were found in copy-pasteable position; this is the other half of that rule.
//  2. TWO ANSWERS TO ONE QUESTION ARE REFUSED, not silently resolved. `--push` is the whole
//     reference and `--registry` is only its first component.
//  3. EVERY `--flag` THE CI TEMPLATES TYPE IS A FLAG THAT EXISTS. The GitHub Action and the GitLab
//     template are shipped as product surface and are the one place a rename cannot be caught by a
//     compiler.

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTargetIsRetiredAndSaysWhereItWent — a retired flag is not a typo and must not read as one.
func TestTargetIsRetiredAndSaysWhereItWent(t *testing.T) {
	dir := t.TempDir()

	// Each value went somewhere different, and the refusal has to point at the right place.
	for _, tc := range []struct {
		target string
		wants  []string
	}{
		{"container", []string{"retired", "kontra deploy --actor"}},
		{"machine", []string{"retired", "--push", "process"}},
		{"nonsense", []string{"retired", "--push"}},
	} {
		err := cmdBuild([]string{"--actor", dir, "--target", tc.target})
		if err == nil {
			t.Errorf("--target %s was accepted; a retired flag that still works builds the other thing "+
				"quietly", tc.target)
			continue
		}
		for _, want := range tc.wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--target %s: the refusal does not say %q:\n%v", tc.target, want, err)
			}
		}
		// The one thing it must never be: the stock `flag` message, which sends an operator to check
		// their spelling for a flag that was deliberately removed.
		if strings.Contains(err.Error(), "not defined") {
			t.Errorf("--target %s got the stock flag error, so the retirement is invisible:\n%v", tc.target, err)
		}
	}

	// AND IT IS ONLY REFUSED WHEN WRITTEN. An empty `--target` is the zero value of a declared flag
	// and must not refuse a build nobody asked to target anything.
	if err := cmdBuild([]string{"--actor", dir}); err != nil && strings.Contains(err.Error(), "retired") {
		t.Errorf("a build with no --target was refused as though one had been passed: %v", err)
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
// `backend/src/activities/fleet.ts:resolveBundle` can derive. Changing this silently makes every
// build publish somewhere a Fleet placement then 404s on.
func TestDefaultDestinationIsTheConventionalOne(t *testing.T) {
	t.Setenv("KONTRA_HOME", t.TempDir())
	t.Setenv("KONTRA_REGISTRY", "")
	t.Setenv("KONTRA_CONTROLLER", "")

	dest, err := pushDestination("", "", "10.124.0.2", "nscheck", "0.1.0")
	if err != nil {
		t.Fatalf("the conventional destination was refused: %v", err)
	}
	if got, want := dest.tagged(), "10.124.0.2:5000/bundles/nscheck:0.1.0"; got != want {
		t.Errorf("default destination = %q, want %q", got, want)
	}
	if !dest.PlainHTTP {
		t.Error("the Controller's own registry is plain HTTP; a build that spoke https to it would " +
			"fail on every Fleet")
	}

	// AN ACTOR WHOSE NAME CANNOT BE A REPOSITORY IS REFUSED ON THE DEFAULT PATH TOO, and with the
	// shared answer rather than a fourth message. This is the path every doc example takes.
	if _, err := pushDestination("", "", "10.124.0.2", "café", "0.1.0"); !errors.Is(err, errImageUnrepresentable) {
		t.Errorf("the default destination for `café` = %v, want errImageUnrepresentable", err)
	}

	// A VERSION IS NOT ALWAYS A TAG — conformance/queues.json carries `1:2`, which Temporal takes
	// verbatim. Without the tag rule this builds a reference the registry rejects after the push.
	if _, err := pushDestination("", "", "10.124.0.2", "nscheck", "1:2"); !errors.Is(err, errImageUnrepresentable) {
		t.Errorf("version `1:2` = %v, want errImageUnrepresentable", err)
	}
}

// ═══ EVERY FLAG THE CI TEMPLATES TYPE IS A FLAG THAT EXISTS ═══
//
// The GitHub Action and the GitLab template are shipped surface: somebody's pipeline runs those
// exact strings. Nothing joins them to `buildFlagSet` but matching literals — no compiler, no vet,
// no ordinary test — and the failure is silent on this side and loud on theirs, arriving as `flag
// provided but not defined` in a pipeline that was green yesterday.
//
// SCOPE, stated the way fleet_documented_flags_test.go states its own: this covers `kontra build`
// command lines, in the files that tell somebody what to type. It is not a repo-wide sweep.
func TestEveryDocumentedBuildFlagExists(t *testing.T) {
	registered := map[string]bool{}
	buildFlagSet().fs.VisitAll(func(f *flag.Flag) { registered[f.Name] = true })
	// RETIRED FLAGS ARE STILL REGISTERED, WHICH IS THE POINT AND ALSO THE TRAP. `--target` is
	// declared so that passing it produces a sentence instead of `flag provided but not defined` — so
	// "does the flag exist" is TRUE for it and cannot be the question. What has to be false is that
	// anything still TELLS somebody to type it.
	retired := map[string]bool{"target": true}
	if len(registered) < 4 {
		t.Fatalf("buildFlagSet has %d flags; this test is reading the wrong flag set", len(registered))
	}

	sources := map[string]string{}
	for _, p := range []string{
		"../.github/actions/build-actor/action.yml",
		"../.gitlab/kontra-build-actor.yml",
		"../README.md",
		"../infra/README.md",
		"main.go",
		"build.go",
	} {
		b, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			t.Errorf("%s is named here as a place `kontra build` is documented and could not be read: %v", p, err)
			continue
		}
		sources[p] = string(b)
	}
	// THE GUARD AGAINST A SWEEP THAT FINDS NOTHING: a repo reshuffle that moved these files would
	// leave this test reading an empty set and reporting success.
	if len(sources) < 6 {
		t.Fatalf("only %d of the 6 documented sources were readable, so this test would pass by reading "+
			"almost nothing", len(sources))
	}

	buildCmdLine := regexp.MustCompile(`kontra build((?:\s+--?[a-zA-Z][-\w]*(?:[= ][^\s|"]*)?)*)`)
	flagTok := regexp.MustCompile(`--([a-zA-Z][-\w]*)`)

	seen := 0
	for path, body := range sources {
		for _, m := range buildCmdLine.FindAllStringSubmatch(body, -1) {
			for _, f := range flagTok.FindAllStringSubmatch(m[1], -1) {
				seen++
				name := f[1]
				// The exemption is scoped to the one file allowed to write a retired flag: build.go's own
				// refusal text, which has to quote the flag it is refusing. Anywhere else — a README, a
				// CI template — a mention is an INSTRUCTION, and following it now produces an error.
				if retired[name] {
					if path != "build.go" {
						t.Errorf("%s tells somebody to type `kontra build --%s`, which is retired "+
							"(ADR 0036) and now refuses. A retired flag left in copy-pasteable position is "+
							"exactly what fleet_documented_flags_test.go was written after.", path, name)
					}
					continue
				}
				if !registered[name] {
					t.Errorf("%s tells somebody to type `kontra build --%s`, and there is no such flag.\n"+
						"  Registered: %v", path, name, keysOfBool(registered))
				}
			}
		}
	}
	if seen < 4 {
		t.Fatalf("only %d `kontra build --flag` mentions were found across %d files; the regexp no "+
			"longer matches how this repo writes a command line, and this test asserts nothing",
			seen, len(sources))
	}
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
