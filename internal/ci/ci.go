// Package ci holds the checks on the repository's CI configuration (.github/workflows) that make check-go
// runs: every action pinned by commit, the tools the jobs install pinned, the race detector run, and the
// end-to-end suite (the milestone gates of spec/relay.md §16) run before a merge.
package ci
