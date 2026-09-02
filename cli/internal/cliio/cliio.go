// Package cliio holds the three streams the CLI writes to and reads from, as variables.
//
// THEY ARE VARIABLES SO TESTS CAN CAPTURE THEM, which is what `package main` already did — this
// package only moves them somewhere every part of the CLI can reach. Before the Warden became its
// own package they were `var stdout io.Writer = os.Stdout` in `api.go`, and a test wanting to read
// what a command printed assigned to that. That works for exactly one package.
//
// WHY NOT ONE PER PACKAGE. Each package could declare its own and be simpler. Then a test that
// captures the CLI's output would see whatever `package main` wrote and silently miss whatever the
// Warden wrote through its own copy — two streams that both call themselves stdout, and a test
// asserting on the wrong one passes by seeing nothing. One set, reachable from everywhere, is the
// only arrangement where "capture the output" means all of it.
//
// NOT A LOGGER. There is no level, no prefix and no formatting here on purpose: the CLI's output
// IS its interface — `--json` documents on stdout, prose on stderr — and a logger would invite
// both to go through one filter that reorders them.
package cliio

import (
	"io"
	"os"
)

// Stdout is where a command's ANSWER goes: JSONL, a digest, a table. Redirectable by the operator,
// so nothing that is not an answer may be written here.
var Stdout io.Writer = os.Stdout

// Stderr is where everything else goes — progress, warnings, refusals. It is the stream chosen so
// that piping stdout to `jq` still shows the reader what happened.
var Stderr io.Writer = os.Stderr

// Stdin is a Reader so `-` can be fed from a test without a pipe.
var Stdin io.Reader = os.Stdin
