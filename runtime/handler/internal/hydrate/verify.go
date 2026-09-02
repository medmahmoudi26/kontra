package hydrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
)

// WHAT LEAVES THE STORE (issue 15). The rest of this package makes sure nothing wrong ever
// ENTERS it: bytes are hashed on the way in, a tree is published by one rename, a working
// directory by another. This file assumes all of that worked and asks the question it cannot
// answer — is what is on disk NOW still the artifact — and then does something about the
// answer.
//
// THREE FAILURES, AND THE MIDDLE ONE IS WHY THIS FILE IS NOT A ONE-LINER:
//
//   - A TRUNCATED DOWNLOAD. Caught on the way in by cas.Local.PutExpecting, which is why the
//     work here is to RETRY it rather than to detect it: a fetch that died at 60 MB of 125
//     stored nothing, so the store is correct and the appliance is unusable, and the only
//     useful response is to fetch again.
//
//   - A PARTIAL MATERIALIZATION: a working directory that exists and is incomplete, because
//     something died between the first file of a copy-on-write expansion and the last. It is
//     worse than a missing one precisely because it is not missing — `os.Stat` reports
//     success, `resolveEntrypoint` finds the two files it checks, and the failure surfaces
//     thirty seconds later as a Node stack trace about a module. Detection is a LIST, not a
//     stat: see listing.go.
//
//   - A MUTATED STORE. Detected on the read that was going to happen anyway — the object is
//     hashed while it is being expanded (tree.go), so a tar.gz artifact whose bytes changed
//     under the store is caught by the pass that was already reading every one of them.
//
// AND THE RESPONSE TO ALL THREE IS THE SAME: throw the bad copy away and hydrate again. A
// warning would be the wrong shape — the caller of this package is a binary that is about to
// exec what it was handed, and there is nothing it could usefully do with a warning except
// exec anyway.

// Level is how hard a verification looks.
type Level int

const (
	// Structure checks that everything the artifact's listing names is present, of the right
	// type, the right length and the right executability. One lstat per entry, no reads —
	// except for a single-file artifact, which has no structure to check and is therefore
	// always read (see checkFile).
	//
	// THIS IS THE DEFAULT AND IT IS THE RIGHT DEFAULT, because of what it is a check ON: a
	// copy this binary made and receipted after checking. The receipt narrows the question
	// from "are these the artifact's bytes" — already answered, once — to "is all of it still
	// here", and that question is answered by the length and the presence of every part.
	Structure Level = iota

	// Contents additionally re-hashes every file. A full read of the working copy, and it is
	// what ADOPTION costs: a directory with no receipt was written by something this binary
	// has no evidence about, so nothing about it may be assumed, including that its files
	// contain what their lengths suggest.
	Contents
)

func (l Level) String() string {
	if l == Contents {
		return "contents"
	}
	return "structure"
}

// ErrDamaged is "this hydrated copy is not the artifact". Callers branch on it to re-hydrate;
// nothing branches on it to continue.
var ErrDamaged = errors.New("the hydrated copy does not match the artifact")

// Damage names the ONE thing that was wrong and the path it was wrong at, because "integrity
// check failed" over a tree of 31,823 files tells an operator nothing they can act on. The
// check stops at the first finding deliberately: the answer is the same for one missing file
// as for ten thousand — hydrate it again — and enumerating the rest costs a full walk of a
// tree that is about to be deleted.
type Damage struct {
	Artifact string
	Root     string
	Path     string // relative to Root; "" when the damage is the copy as a whole
	Reason   string
}

func (d *Damage) Error() string {
	if d.Path == "" {
		return fmt.Sprintf("the hydrated %s at %s %s", d.Artifact, d.Root, d.Reason)
	}
	return fmt.Sprintf("the hydrated %s at %s is incomplete: %s %s", d.Artifact, d.Root, d.Path, d.Reason)
}

func (d *Damage) Is(target error) bool { return target == ErrDamaged }

// RepairFailed is the end of the line: something was wrong, it was hydrated again, and that
// failed too. BOTH causes are in the message, because they are usually different facts — "the
// working copy was missing a file" and "and the mirror is now 404" is a story an operator can
// act on, and either half alone is not.
type RepairFailed struct {
	Artifact string
	Dest     string
	Found    error // what was wrong to begin with
	Then     error // what happened when it was hydrated again
}

func (e *RepairFailed) Error() string {
	if e.Found == nil {
		return fmt.Sprintf("hydrate %s at %s: %v", e.Artifact, e.Dest, e.Then)
	}
	if e.Found.Error() == e.Then.Error() {
		return fmt.Sprintf("hydrate %s at %s: %v (hydrated again, and it failed the same way)", e.Artifact, e.Dest, e.Found)
	}
	return fmt.Sprintf("hydrate %s at %s: %v; hydrated again and that failed too: %v", e.Artifact, e.Dest, e.Found, e.Then)
}

func (e *RepairFailed) Unwrap() error { return e.Then }

// Check answers whether dest is a complete, correct working copy of the artifact, WITHOUT
// hydrating anything. It is the question `kontra up` asks on its second run.
//
// A copy with no receipt is damaged as far as this call is concerned, and the word is exact:
// it is not "absent" — it may be an entire, perfectly good working directory — it is
// unvouched-for, and nothing here may exec an unvouched-for copy. EnsureHydrated is what
// knows how to turn one into a vouched-for one without rebuilding it.
func (s *Store) Check(a Artifact, dest string, level Level) error {
	if err := a.Validate(); err != nil {
		return err
	}
	r, err := readReceipt(dest, a.Digest)
	if err != nil {
		return err
	}
	if r == nil {
		if _, statErr := os.Lstat(dest); errors.Is(statErr, fs.ErrNotExist) {
			return &Damage{Artifact: a.Name, Root: dest, Reason: "is not there"}
		}
		return &Damage{Artifact: a.Name, Root: dest, Reason: "has no hydration receipt, so nothing has ever checked that it is complete"}
	}
	return s.checkAgainst(a, dest, r.Listing, level)
}

// checkAgainst verifies dest against a listing named by digest. An empty listing digest means
// a single-file artifact, whose whole inventory is its own address.
func (s *Store) checkAgainst(a Artifact, dest, listingDigest string, level Level) error {
	if a.Kind == KindFile {
		return checkFile(a, dest, level)
	}
	if listingDigest == "" {
		return &Damage{Artifact: a.Name, Root: dest, Reason: "was hydrated without an inventory, so there is nothing to check it against"}
	}
	l, err := s.readListing(listingDigest)
	if err != nil {
		// THE INVENTORY ITSELF IS DAMAGED, which is the same operator situation as a damaged
		// copy and gets the same answer. Reported as Damage rather than as a store error so
		// the repair path picks it up instead of giving up.
		return &Damage{Artifact: a.Name, Root: dest, Reason: "cannot be checked: " + err.Error()}
	}
	return l.check(dest, level, a.Name)
}

// checkFile verifies a single-file working copy. There is no listing: the artifact's digest IS
// its inventory.
//
// IT IGNORES THE LEVEL AND ALWAYS READS. Structure exists because a tree of 31,823 files is
// expensive to hash and its parts are cheap to count; a single file has no parts to count, so
// the structural approximation of "is all of it there" would be a length — and the only place a
// length could be read from is a receipt, which makes the check a comparison against something
// this binary wrote rather than against the artifact. One file is small enough to answer the
// real question, so it answers the real question.
func checkFile(a Artifact, dest string, _ Level) error {
	info, err := os.Lstat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return &Damage{Artifact: a.Name, Root: dest, Reason: "is not there"}
	}
	if err != nil {
		return &Damage{Artifact: a.Name, Root: dest, Reason: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return &Damage{Artifact: a.Name, Root: dest, Reason: "is " + describe(info.Mode()) + ", and the artifact is a file"}
	}
	if (info.Mode().Perm()&0o111 != 0) != a.Executable {
		return &Damage{Artifact: a.Name, Root: dest, Reason: fmt.Sprintf("has execute bits %v and the artifact says %v", info.Mode().Perm()&0o111 != 0, a.Executable)}
	}
	got, err := sha256File(dest)
	if err != nil {
		return &Damage{Artifact: a.Name, Root: dest, Reason: err.Error()}
	}
	if got != a.Digest {
		return &Damage{Artifact: a.Name, Root: dest, Reason: fmt.Sprintf("hashes to sha256:%s and the artifact is sha256:%s", got, a.Digest)}
	}
	return nil
}

// EnsureHydrated is Hydrate with the guarantee the appliance actually needs: when it returns
// without an error, dest is a COMPLETE working copy of the artifact and has been checked
// against the artifact's own inventory on this call.
//
// The path it takes, in the order it takes it:
//
//  1. Verify what is already there. A receipted, complete copy costs one lstat per entry and
//     nothing else — no archive is opened, no digest is recomputed, no byte is copied.
//  2. If there is no receipt but the directory is there, try to ADOPT it: check every file's
//     CONTENTS against the artifact's inventory, and if it all matches, write the receipt. A
//     complete copy made by a binary that did not receipt anything is still a complete copy,
//     and rebuilding 450 MB to learn that would be an upgrade nobody would forgive.
//  3. Otherwise hydrate: discard whatever is there — by rename, so a process still running
//     out of it keeps its open files — and materialize again, then verify, then receipt.
//  4. If that hydration failed, do it ONCE more, having also thrown away the derived tree and
//     any object the failure proved was wrong. If the second one fails too, return
//     *RepairFailed, which names both causes.
//
// The one thing it never does is return dest without having checked it.
func (s *Store) EnsureHydrated(ctx context.Context, a Artifact, dest string, mode cas.Mode) (Result, error) {
	started := time.Now()
	if err := a.Validate(); err != nil {
		return Result{Artifact: a, Digest: a.Digest, Dest: dest}, err
	}

	// 1 & 2: is what is there already good, or adoptable?
	res, found, err := s.reuse(a, dest, mode)
	if err != nil {
		return res, err
	}
	if found == nil {
		res.Elapsed = time.Since(started)
		return res, nil
	}

	// A COPY THAT WAS NEVER THERE IS NOT A REPAIR. The distinction is not cosmetic: a first
	// run would otherwise report "hydrated again: the working copy is not there" on every
	// fresh install, and an operator who is told about a repair every single time is an
	// operator who stops reading the line that matters.
	_, statErr := os.Lstat(dest)
	repaired := statErr == nil

	res, err = s.rehydrate(ctx, a, dest, mode, false)
	if err == nil {
		res.Repaired = repaired
		if repaired {
			res.Damage = found
		}
		res.Elapsed = time.Since(started)
		return res, nil
	}
	if !worthAnotherTry(ctx, err) {
		res.Elapsed = time.Since(started)
		return res, err
	}

	// ONE MORE, AND IT IS THE LAST ONE. See rehydrate for what "harder" throws away.
	second, secondErr := s.rehydrate(ctx, a, dest, mode, true)
	if secondErr != nil {
		second.Elapsed = time.Since(started)
		return second, &RepairFailed{Artifact: a.Name, Dest: dest, Found: err, Then: secondErr}
	}
	second.Repaired, second.Damage = true, err
	second.Elapsed = time.Since(started)
	return second, nil
}

// reuse is steps 1 and 2: a receipted copy, or an unreceipted but provably complete one.
//
// TWO ERRORS, AND THEY ARE NOT THE SAME KIND OF THING. `found` is what is WRONG WITH DEST —
// nil when dest can be used as it stands, and otherwise the finding, which the caller reports
// because the reason a machine is re-hydrating is the fact worth knowing about it. `err` is
// this function failing to do its job, which is not something a re-hydration would fix.
func (s *Store) reuse(a Artifact, dest string, mode cas.Mode) (res Result, found, err error) {
	nope := func(found error) (Result, error, error) {
		return Result{Artifact: a, Digest: a.Digest, Dest: dest}, found, nil
	}
	res = Result{Artifact: a, Digest: a.Digest, Dest: dest, Existing: true, Verified: true}

	r, err := readReceipt(dest, a.Digest)
	if err != nil {
		return res, nil, err
	}
	if r != nil {
		if found := s.checkAgainst(a, dest, r.Listing, Structure); found != nil {
			return nope(found)
		}
		res.Method, res.Files, res.Bytes = r.Method, r.Files, r.Bytes
		return res, nil, nil
	}

	if _, err := os.Lstat(dest); err != nil {
		return nope(&Damage{Artifact: a.Name, Root: dest, Reason: "is not there"})
	}

	// ADOPTION. There is something at dest and no evidence about it, so it pays the full
	// price: every file read and hashed. What it buys is not having to write 450 MB to
	// discover that 450 MB was already correct — the ordinary case on the first start after
	// this verification shipped, and on any machine whose data directory was restored from a
	// backup.
	listingDigest, err := s.inventoryFor(a)
	if err != nil {
		// Cannot adopt without an inventory, and the reason is worth carrying: it is usually
		// "the object is not in the store either", which is what a hydration fixes.
		return nope(err)
	}
	if found := s.checkAgainst(a, dest, listingDigest, Contents); found != nil {
		return nope(found)
	}
	if err := s.receipt(a, dest, listingDigest, "", mode); err != nil {
		return res, nil, err
	}
	res.Adopted = true
	if l, err := s.readListing(listingDigest); err == nil {
		res.Files, res.Bytes = l.files, l.bytes
	}
	return res, nil, nil
}

// rehydrate discards what is at dest and materializes it again, then verifies THAT and
// receipts it.
//
// harder is the second and final attempt, and it throws away more: the derived tree, and the
// stored object if the first failure proved the object was the problem. It is deliberately
// not "throw away everything" — an object is only discarded when it has been shown not to
// hash to its own address, because a working copy that is damaged for some other reason plus
// a source URL that is no longer reachable is a situation where deleting the one good copy of
// the bytes turns a repairable appliance into an unstartable one.
func (s *Store) rehydrate(ctx context.Context, a Artifact, dest string, mode cas.Mode, harder bool) (Result, error) {
	res := Result{Artifact: a, Digest: a.Digest, Dest: dest}
	if err := dropReceipt(dest); err != nil {
		return res, err
	}
	if err := discard(dest); err != nil {
		return res, err
	}
	if harder {
		if err := discard(filepath.Join(s.trees, a.Digest)); err != nil {
			return res, err
		}
		if err := dropReceipt(filepath.Join(s.trees, a.Digest)); err != nil {
			return res, err
		}
	}

	res, err := s.Hydrate(ctx, a, dest, mode)
	if err != nil {
		// A PROVEN-WRONG OBJECT IS THE ONE THING WORTH DELETING. cas.ErrMutated means the
		// bytes under the address do not hash to the address, which is not an opinion; the
		// next attempt has to fetch rather than re-read them.
		if errors.Is(err, cas.ErrMutated) {
			if derr := s.cas.Discard(a.Digest); derr != nil {
				return res, errors.Join(err, derr)
			}
			if derr := discard(filepath.Join(s.trees, a.Digest)); derr != nil {
				return res, errors.Join(err, derr)
			}
			if derr := dropReceipt(filepath.Join(s.trees, a.Digest)); derr != nil {
				return res, errors.Join(err, derr)
			}
		}
		return res, err
	}

	listingDigest, err := s.inventoryFor(a)
	if err != nil {
		return res, err
	}
	// VERIFY WHAT WE JUST WROTE, BEFORE ANYTHING CAN RUN IT. Structure, not Contents: the
	// bytes came from a tree that was itself expanded out of a digest-verified object minutes
	// ago, so the question is whether the copy is all there, and a second full hash of 450 MB
	// would answer a question nobody asked.
	if err := s.checkAgainst(a, dest, listingDigest, Structure); err != nil {
		return res, err
	}
	if err := s.receipt(a, dest, listingDigest, res.Method, mode); err != nil {
		return res, err
	}
	res.Verified = true
	return res, nil
}

// receipt writes the proof, last, after the check that makes it true.
func (s *Store) receipt(a Artifact, dest, listingDigest string, method cas.Method, mode cas.Mode) error {
	r := receipt{
		Schema:   receiptSchema,
		Artifact: a.Name,
		Digest:   a.Digest,
		Kind:     a.Kind,
		Listing:  listingDigest,
		Mode:     mode.String(),
		Method:   method,
		Verified: time.Now().UTC(),
	}
	if a.Kind == KindFile {
		if st, err := os.Stat(dest); err == nil {
			r.Files, r.Bytes = 1, st.Size()
		}
	} else if l, err := s.readListing(listingDigest); err == nil {
		r.Files, r.Symlinks, r.Dirs, r.Bytes = l.files, l.symlinks, l.dirs, l.bytes
	}
	return writeReceipt(dest, r)
}

// inventoryFor answers the CAS address of the artifact's listing, building it if this store
// has never been asked. For a single-file artifact there is no listing and the empty string
// is the right answer, not an error: its digest is its inventory.
func (s *Store) inventoryFor(a Artifact) (string, error) {
	if a.Kind == KindFile {
		return "", nil
	}
	if r, err := readReceipt(filepath.Join(s.trees, a.Digest), a.Digest); err == nil && r != nil && r.Listing != "" {
		return r.Listing, nil
	}
	// No vouched-for tree. Read the archive and produce the inventory WITHOUT writing a tree:
	// the caller may only be trying to adopt an existing working directory, and expanding 450
	// MB to check something that is already on disk would defeat the point of checking it.
	return s.inventoryFromObject(a)
}

// readListing fetches and re-verifies the inventory object. ReadVerified rather than a plain
// read, because a listing that was itself tampered with would otherwise be a way to make a
// damaged working copy verify.
func (s *Store) readListing(digest string) (*listing, error) {
	body, err := s.cas.ReadVerified(digest, "artifact listing")
	if err != nil {
		return nil, err
	}
	return parseListing(body)
}

// worthAnotherTry decides whether a failed hydration gets the one retry.
//
// The refusals are the failures a second attempt cannot change, and each of them is a
// message that gets WORSE for being repeated: a full disk is still full a millisecond later,
// a cancelled context is still cancelled, and a pin that is not a pin is a bug in the source
// tree. Everything else — a dropped transfer, a mirror mid-deploy, a half-written working
// directory — is exactly the kind of thing that works the second time.
func worthAnotherTry(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, cas.ErrDiskFull) || errors.Is(err, cas.ErrBadDigest) {
		return false
	}
	return true
}

// discard removes a directory or file by RENAMING IT ASIDE FIRST.
//
// The rename is the point. os.RemoveAll is not atomic — it is a walk that deletes as it goes
// — so a repair that called it directly would spend its first second turning a partial
// working directory into a MORE partial one under the name a concurrent `kontra up` is
// reading. One rename frees the name instantly; a process that is already exec'ing out of the
// old directory keeps every file it has open, because unlinking a path does not disturb an
// open file or a running image.
func discard(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	aside := path + ".discarded-" + hex.EncodeToString(b[:])
	if err := os.Rename(path, aside); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // somebody else discarded it first, which is the outcome we wanted
		}
		return fmt.Errorf("set aside %s: %w", path, err)
	}
	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("remove %s: %w", aside, err)
	}
	return nil
}
