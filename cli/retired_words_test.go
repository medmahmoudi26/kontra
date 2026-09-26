package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A RETIRED WORD MUST NOT REACH AN OPERATOR — and the one that did survived two renames to do it.
//
// `campaign` was retired 2026-08-30 (`control/orchestrator/src/infra/CONTEXT.md` keeps the
// tombstone). The struct field, the wire key and the conformance corpus all moved to `fleet`;
// `panels_conformance_test.go` exists BECAUSE the word survived that rename in a JSON tag.
//
// It survived a second time, one line away, in the place that matters most: `kontra panels list`
// printed a `CAMPAIGN` column header over a cell holding `t.Fleet`, until 2026-09-08. A header is a
// bare string that nothing derives and no test asserted, so the one place a person actually READS
// the vocabulary outlived every place the code states it.
//
// STRING LITERALS ONLY, AND NOT TESTS, which is what makes this guard livable rather than a sweep
// people delete. The word is REQUIRED in prose: `cli/fleet.go` explains that `--campaign` was
// removed, `fleet_documented_flags_test.go`'s whole subject is that it stayed in the README after
// removal, and the tombstone itself has to say it. None of those is a shipped string.
var retiredWords = map[string]string{
	"campaign": "retired 2026-08-30 — a Fleet is named after what it places; say `fleet`",
}

// stringLiterals returns the Go string and raw-string literals in `src`, and NOTHING inside a
// comment.
//
// A REGEX WAS NOT ENOUGH, and the first version proved it on itself. This codebase writes markdown
// in its comments, so `// It used to be ` + "`--campaign`" + “ looks exactly like a raw-string
// literal to any pattern that only knows about quotes — and the guard fired on the very prose that
// explains the retirement, which is the one thing it must never do. So this is a small scanner that
// tracks the four states Go has, and a comment is simply not one of the two it collects.
func stringLiterals(src string) []string {
	var out []string
	const (
		code = iota
		lineComment
		blockComment
		quoted
		raw
	)
	state, start := code, 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state, i = lineComment, i+1
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state, i = blockComment, i+1
			case c == '"':
				state, start = quoted, i
			case c == '`':
				state, start = raw, i
			}
		case lineComment:
			if c == '\n' {
				state = code
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state, i = code, i+1
			}
		case quoted:
			if c == '\\' {
				i++ // an escaped character cannot end the literal
			} else if c == '"' || c == '\n' {
				out = append(out, src[start:i+1])
				state = code
			}
		case raw:
			if c == '`' {
				out = append(out, src[start:i+1])
				state = code
			}
		}
	}
	return out
}

func cliSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		out[path] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func TestTheRetiredWordSweepReadsTheTree(t *testing.T) {
	// NON-VACUOUS. A walk returning nothing passes the sweep below, which is how a guard reports
	// success for a tree it never read — this repository's most repeated failure shape.
	sources := cliSources(t)
	if len(sources) < 20 {
		t.Fatalf("only %d non-test Go sources under cli/ — the walk is broken", len(sources))
	}
	// THE ANCHOR MOVED WHEN ITS FILE DID. It was `panels.go` — where the word actually survived, in
	// a `CAMPAIGN` column header — and that file went with the Monitor. `fleet.go` is the successor
	// for the same reason the guard exists: it is where `--campaign` was removed, where the prose
	// explaining the retirement lives, and the file a reader looks in for the word's replacement.
	if _, ok := sources["fleet.go"]; !ok {
		t.Error("the walk did not reach fleet.go, which is where the retirement is explained")
	}

	// …and the pattern must find the literals it is about to judge.
	found := 0
	for _, body := range sources {
		found += len(stringLiterals(body))
	}
	if found < 500 {
		t.Fatalf("only %d string literals found across cli/ — the pattern stopped matching", found)
	}
}

func TestTheRetiredWordCanStillBeCaught(t *testing.T) {
	// The guard on the guard: the exact line that shipped for nine days must be a failure.
	shipped := `header := []string{"  TERMINAL", "CAMPAIGN", "ACTOR"}`
	var hit bool
	for _, lit := range stringLiterals(shipped) {
		if strings.Contains(strings.ToLower(lit), "campaign") {
			hit = true
		}
	}
	if !hit {
		t.Error("the sweep would not have caught the header it was written for")
	}
	// …and does NOT fire on the prose that has to keep saying it.
	prose := `// A FLEET IS NAMED AFTER WHAT IT PLACES. It used to be ` + "`--campaign`" + `, defaulting to`
	for _, lit := range stringLiterals(prose) {
		if strings.Contains(strings.ToLower(lit), "campaign") {
			t.Errorf("the sweep fires on a comment explaining the retirement: %s", lit)
		}
	}
}

func TestNoRetiredWordReachesAnOperator(t *testing.T) {
	var offenders []string
	for path, body := range cliSources(t) {
		for _, lit := range stringLiterals(body) {
			lower := strings.ToLower(lit)
			for word, why := range retiredWords {
				if strings.Contains(lower, word) {
					offenders = append(offenders, path+": "+lit+" — "+why)
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("a retired word is in a string an operator can read:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
