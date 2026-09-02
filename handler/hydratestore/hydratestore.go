// Package hydratestore is first-run hydration — a pinned, digest-verified artifact turned into
// a working directory — exported for the appliance.
//
// The implementation is handler/internal/hydrate over handler/internal/cas, and `internal` is
// the right home for both: the digest rules, the tar policy and the copy-on-write ladder are
// this module's business. This file exists for the same reason `casstore` does, and the pair of
// them is the whole story. The appliance is a DIFFERENT MODULE (cli/), ADR 0031 §2 says it must
// not open a second store, and a store the sharer cannot import is not shared.
//
// TWO EXPORT FILES, NOT ONE, BECAUSE THEY HAVE TWO CUSTOMERS. `casstore` is what the embedded
// OCI registry needs: put a layer, ask whether one is held, stream one out. This is what `kontra
// up` needs: fetch a pin once, expand it once, and clone it into a working directory cheaply.
// The overlap is exactly the `*cas.Local` underneath, which both name as the same type — so the
// registry and the hydrator address one set of bytes on disk without either package knowing the
// other exists.
//
// TYPE ALIASES, NOT WRAPPERS, for the reason `casstore` gives: a wrapper would be a second
// implementation of the publish-by-rename concurrency design, and that one must exist exactly
// once. `= hydrate.Store` makes handler and cli name the same type, so there is nothing left to
// keep in step.
//
// COPY-ON-WRITE IS NOT A SECURITY BOUNDARY — see the cas and hydrate package headers. A hydrated
// working directory deduplicates bytes and removes a copy from a cold start. Actors, which run
// code we did not write, get containers.
package hydratestore

import (
	"github.com/medmahmoudi26/kontra/handler/internal/cas"
	"github.com/medmahmoudi26/kontra/handler/internal/hydrate"
)

// Store is the appliance's artifact store: the CAS for fetched bytes, plus the golden trees
// archives expand to.
type Store = hydrate.Store

// Open opens (and creates) the store under root. Root is the appliance's data directory, so the
// hydrated artifacts and the registry's layers land in the one `cas/` beneath it.
var Open = hydrate.Open

// Artifact is a pin: a URL to look at and a digest that says what counts as having found it.
type Artifact = hydrate.Artifact

// Kind is what the bytes are — one file, or an archive that expands to a tree.
type Kind = hydrate.Kind

const (
	KindFile  = hydrate.KindFile
	KindTarGz = hydrate.KindTarGz
)

// FileURL is the pin for an artifact this machine already has. The appliance's own bundle is
// built locally by `kontra bundle orchestrator` and has no release URL until issue 17 gives it
// one; the digest promise is unchanged either way.
var FileURL = hydrate.FileURL

// Result is what happened: whether the bytes had to be acquired, whether the working directory
// was already there, and which copy-on-write rung this filesystem actually gave us.
type Result = hydrate.Result

// Progress is one report from a hydration in flight. A first run is not instant and a binary
// that says nothing for thirty seconds is indistinguishable from a hung one.
type Progress = hydrate.Progress

// Phase is which part of a hydration is running.
type Phase = hydrate.Phase

const (
	PhaseFetch       = hydrate.PhaseFetch
	PhaseExpand      = hydrate.PhaseExpand
	PhaseMaterialize = hydrate.PhaseMaterialize
)

// Mode is the caller's promise about the working copy, and it decides whether hardlinking is on
// the table. Shared means "this is read and exec'd, never written" — which is what a runtime and
// a compiled orchestrator are, and what makes a second hydration of the same digest cost nothing.
type Mode = cas.Mode

const (
	Writable = cas.Writable
	Shared   = cas.Shared
)

// Method is the rung that was actually used: reflink, hardlink or a full copy. Reported rather
// than logged internally, because "did this machine give us copy-on-write or a 450 MB copy" is a
// question about the operator's filesystem that only the operator can be told.
type Method = cas.Method

const (
	MethodReflink  = cas.MethodReflink
	MethodHardlink = cas.MethodHardlink
	MethodCopy     = cas.MethodCopy
)

// ErrDiskFull is what a hydration that ran out of space answers. Named here because "hydration
// failed" and "this box has no disk left" are different operator actions, and the second one is
// a lesson this repo has already paid for.
var ErrDiskFull = cas.ErrDiskFull

// --- verified hydration (issue 15) ------------------------------------------------------------
//
// Everything above makes sure the right bytes ENTER the store. This is the half that makes sure
// the right bytes LEAVE it, and it exists because the appliance's next act after hydrating is to
// exec what it hydrated. A warning would have nowhere to go.

// Level is how hard a verification looks: Structure is one lstat per entry of the artifact's
// inventory, Contents re-reads and re-hashes every file. Structure is what a receipted copy
// costs on every start; Contents is what a copy nobody vouched for costs, once.
type Level = hydrate.Level

const (
	Structure = hydrate.Structure
	Contents  = hydrate.Contents
)

// ErrDamaged is "the hydrated copy is not the artifact" — a truncated file, a missing one, a
// working directory that exists and is half there. Callers branch on it to hydrate again;
// nothing branches on it to carry on.
//
// THE SENTINEL AND NOT THE TYPES, for the reason casstore gives about its own surface: an
// unused export is a promise nobody checked. `hydrate.Damage` names the one file that was
// wrong and `hydrate.RepairFailed` carries both causes of a final failure, and both of them
// reach the appliance already — as the `error` in Result.Damage and as the error
// EnsureHydrated returns, whose Error() strings are what an operator reads. A caller that
// needs to take the two apart can be given the types when it asks for them.
