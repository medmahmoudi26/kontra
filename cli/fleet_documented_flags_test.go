// fleet_documented_flags_test.go — EVERY `--flag` THIS REPO TELLS SOMEBODY TO TYPE IS A FLAG THAT
// EXISTS.
//
// Documentation and error text rot in a direction no compiler and no ordinary test can see. A flag
// removed from `fleetFlagSet` keeps working everywhere it is *mentioned*: the markdown still renders,
// the error string still formats, `go vet` is happy, and the only thing that changes is that a
// person who follows the instruction gets `flag provided but not defined` and has no idea whether
// they typed it wrong or the docs did.
//
// THREE DEAD FLAGS WERE FOUND BY WRITING THIS, all of them in copy-pasteable position:
//
//	README.md               kontra fleet up --count 2 --role crawl --campaign c1 --actor …
//	webcrawl/README.md      the same two lines, in the example's own quick-start
//	cli/fleet.go            "watch it with `kontra fleet status --campaign %s`" — on a 409, so the
//	                        ONE remedy offered for "a converge is already running" did not parse
//
// `--role` and `--campaign` had both been gone since a Fleet's name became derived from the
// Artifact it places. The replacements are `--tag` and `--fleet`.
//
// SCOPE. This checks the `fleet` command family, because that is one flag set with one registration
// point and it is where the rot was. It is deliberately not a repo-wide sweep: the other commands
// build their flag sets differently, and a guard that half-covers a repo while reading as though it
// covers all of it is worse than one that says what it holds.
package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A `kontra fleet …` command line, and the flags on it. The `[a-z]+` after `fleet` is the
// subcommand; all of them share `fleetFlagSet`.
var fleetCmdLine = regexp.MustCompile(`kontra fleet ([a-z]+)((?:\s+--?[a-zA-Z][-\w]*(?:[= ][^\s` + "`" + `]*)?)*)`)
var flagToken = regexp.MustCompile(`--([a-zA-Z][-\w]*)`)

// registeredFleetFlags is the truth this test compares against: the flag set itself, enumerated,
// never a list written beside it.
func registeredFleetFlags() map[string]bool {
	f := fleetFlagSet("up")
	out := map[string]bool{}
	f.fs.VisitAll(func(fl *flag.Flag) { out[fl.Name] = true })
	return out
}

// docSources are the files that tell a person what to type: the repo's front page, every example's
// own README, the wiki, and this package's own strings — an error message is documentation that
// arrives at the worst possible moment.
func docSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	add := func(p string) {
		b, err := os.ReadFile(p)
		if err == nil {
			out[p] = string(b)
		}
	}
	add("../README.md")
	add("fleet.go")
	for _, glob := range []string{
		"../examples/*/*/README.md",
		"../examples/*/*/*/description.md",
		"../docs/wiki/*.md",
		"../infra/README.md",
	} {
		paths, _ := filepath.Glob(glob)
		for _, p := range paths {
			add(p)
		}
	}
	// THE GUARD AGAINST A SWEEP THAT FINDS NOTHING. Every path above is relative to the package
	// directory, and a repo reshuffle that moved `examples/` would leave this test walking an empty
	// set and reporting success over zero files.
	if len(out) < 5 {
		t.Fatalf("only %d documentation sources found (%v) — the globs no longer match this repo's "+
			"layout, and this test would pass by reading nothing", len(out), keysOf(out))
	}
	return out
}

func keysOf(m map[string]string) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

func TestEveryDocumentedFleetFlagExists(t *testing.T) {
	registered := registeredFleetFlags()
	if len(registered) == 0 {
		t.Fatal("fleetFlagSet registered no flags — the comparison below would accept anything")
	}

	type site struct{ file, line, flag string }
	var dead []site
	found := 0

	for path, body := range docSources(t) {
		for _, line := range strings.Split(body, "\n") {
			for _, m := range fleetCmdLine.FindAllStringSubmatch(line, -1) {
				for _, f := range flagToken.FindAllStringSubmatch(m[2], -1) {
					found++
					if !registered[f[1]] {
						dead = append(dead, site{path, strings.TrimSpace(line), f[1]})
					}
				}
			}
		}
	}

	// THE SECOND VACUITY GUARD. The regexp above is the kind that can stop matching after an
	// innocuous formatting change, and a zero-match sweep reports exactly like a clean one.
	if found == 0 {
		t.Fatal("no `kontra fleet … --flag` command line was found in any documentation source — " +
			"the regexp has stopped matching, and this test is now asserting nothing")
	}

	for _, d := range dead {
		t.Errorf("%s documents `--%s`, which `fleetFlagSet` does not register.\n"+
			"    %s\n"+
			"  A person following this gets `flag provided but not defined` and cannot tell whether\n"+
			"  they mistyped or the docs did. If the flag was renamed, the line here has to move with\n"+
			"  it — that is what this test is for.", d.file, d.flag, d.line)
	}
	t.Logf("checked %d documented `kontra fleet` flags across %d sources", found, len(docSources(t)))
}
