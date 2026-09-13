package main

// actorschema_test.go — that the verb is reachable and findable.
//
// The derivation itself is tested where it lives, in `runtime/python/internals/test_schemadump.py`,
// against `operations_of`. What cannot be seen from there is whether `kontra actor schema` reaches
// it at all, or whether anyone is ever told the verb exists.

import (
	"strings"
	"testing"
)

// `schema` must be read as a subcommand, not as the NAME of an Actor. The grammar here is
// `kontra actor <ref> <verb>`, so a word in the first position is otherwise an Actor reference —
// which is how `register` had to be special-cased before it, and why this is checked the same way.
func TestActorSchemaIsASubcommandNotAnActorName(t *testing.T) {
	err := cmdActor([]string{"schema"})
	if err == nil {
		t.Fatal("bare `kontra actor schema` must be a usage error")
	}
	if !strings.Contains(err.Error(), "kontra actor schema <dir>") {
		t.Errorf("it did not reach the schema command's own usage: %v", err)
	}

	// And it is NOT treated as an Actor called "schema" needing a verb.
	if strings.Contains(err.Error(), "register <dir>") && !strings.Contains(err.Error(), "schema <dir>") {
		t.Errorf("`schema` fell through to the actor-ref grammar: %v", err)
	}
}

// A flag AFTER the directory must parse. Go's `flag` stops at the first non-flag argument, so the
// obvious implementation rejects `<dir> --method NAME` — with a usage line that itself shows the
// flag after the directory, which is the worst version of the bug.
func TestActorSchemaAcceptsFlagsAfterTheDirectory(t *testing.T) {
	err := cmdActorSchema([]string{"/nonexistent-actor-dir", "--method", "echo"})
	if err == nil {
		t.Fatal("a nonexistent directory should fail")
	}
	// The point: it failed on the DIRECTORY, not on argument parsing.
	if strings.Contains(err.Error(), "usage:") {
		t.Errorf("the flag after the directory was not parsed; got a usage error: %v", err)
	}
	if !strings.Contains(err.Error(), "no actor at") {
		t.Errorf("expected the missing-directory error, got: %v", err)
	}
}

func TestActorSchemaRejectsAStrayArgument(t *testing.T) {
	err := cmdActorSchema([]string{"somedir", "extra"})
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("a second positional must be refused, got: %v", err)
	}
}

// Help is the one surface nothing else checks.
func TestActorSchemaIsDocumented(t *testing.T) {
	if !strings.Contains(usageText, "kontra actor schema") {
		t.Error("usageText does not advertise `kontra actor schema`")
	}
	// And it says the thing that makes it worth reaching for: no deploy needed.
	if !strings.Contains(usageText, "no orchestrator") {
		t.Error("usageText does not say the schema comes from the code on disk without a deploy")
	}
}
