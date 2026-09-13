package main

// tokenmint_test.go — that `kontra token mint` is REACHABLE and FINDABLE.
//
// The command itself is tested in `internal/config`. What is tested here is the pair of things that
// package cannot see: whether the verb reaches it, and whether anyone is ever told the verb exists.
//
// THE SECOND ONE IS THE POINT. This command's whole reason for being is that a blank `run` token
// leaves the Run surface open and every message reporting it named no remedy. A remedy that is
// itself unadvertised would repeat the bug one level up.

import (
	"strings"
	"testing"
)

// Help is the one surface nothing else checks — see `usageText`'s own comment.
//
// `init` and `user add` are asserted here beside `token mint` because they were BOTH missing from
// this block: three commands that create or repair an installation's credentials, and no way to
// discover any of them from `kontra` with no arguments.
func TestSetupCommandsAreDocumented(t *testing.T) {
	for _, want := range []string{"kontra init", "kontra user add <name>", "kontra token mint"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText does not advertise %q", want)
		}
	}
	// Every key the command accepts is named, or an operator has to read the source to find out
	// which word goes after `mint`.
	for _, key := range []string{"state", "explore", "panel", "run"} {
		if !strings.Contains(usageText, key) {
			t.Errorf("usageText does not name the %q token", key)
		}
	}
	// AND WHAT A BLANK MEANS, in both directions — the asymmetry is the entire hazard. `run` blank
	// is a surface left OPEN; `state` blank is a surface DISABLED. Advertising the command without
	// that distinction invites an operator to mint `state` believing they are locking something
	// down, when they are enabling the routes that destroy Droplets.
	if !strings.Contains(usageText, "OPEN") {
		t.Error("usageText does not say that a blank `run` leaves the Run surface OPEN")
	}
	if !strings.Contains(usageText, "DISABLED") {
		t.Error("usageText does not say that a blank `state`/`panel` leaves that surface DISABLED")
	}
}

// The verb reaches the command. A `case` wired to nothing is the switch-statement bug `dispatch`'s
// own comment describes, and it is invisible to every test in `internal/config`.
func TestTokenVerbIsWired(t *testing.T) {
	// A bare `kontra token` is a usage error, NOT the CLI's "unknown command" — the word exists.
	err := dispatch([]string{"token"})
	if err == nil {
		t.Fatal("bare `kontra token` must be a usage error")
	}
	if !strings.Contains(err.Error(), "kontra token mint") {
		t.Errorf("the usage line does not name the subcommand: %v", err)
	}

	// A wrong subcommand is the same, rather than falling through to something that runs.
	if err := dispatch([]string{"token", "rotate", "run"}); err == nil ||
		!strings.Contains(err.Error(), "kontra token mint") {
		t.Errorf("`token rotate` should be a usage error, got %v", err)
	}

	// And `mint` REACHES the real command: with no config present it fails the way
	// CmdTokenMint fails, which no usage line would produce.
	t.Setenv("KONTRA_HOME", t.TempDir()+"/.kontra")
	err = dispatch([]string{"token", "mint", "run"})
	if err == nil {
		t.Fatal("mint against an empty KONTRA_HOME must fail")
	}
	if strings.Contains(err.Error(), "usage:") {
		t.Errorf("`token mint run` did not reach the command, it hit the usage line: %v", err)
	}
	if !strings.Contains(err.Error(), "kontra init") {
		t.Errorf("expected the missing-config error from CmdTokenMint, got %v", err)
	}
}
