package hydrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/medmahmoudi26/kontra-local/handler/internal/cas"
)

// A RECEIPT IS WHAT EXISTENCE WAS PRETENDING TO BE.
//
// The failure this package is built against is a working directory that exists and is
// incomplete, and the reason it is worse than a missing one is that every cheap question
// about it answers yes. So the cheap question is moved: nothing asks whether the directory
// is there, everything asks whether its receipt is there and still true.
//
// A RECEIPT NEVER PRECEDES THE THING IT VOUCHES FOR, and the two writers get there by
// opposite orderings, which is worth stating because the discrepancy looks like a bug:
//
//   - A WORKING COPY's receipt is written LAST, after checkAgainst has passed over the copy.
//     There is a real verification to do — the clone is a second act of copying — so the
//     receipt is a record of it having succeeded.
//   - A GOLDEN TREE's receipt is written BEFORE the rename that publishes the tree. There is
//     nothing to verify at that instant: the inventory was derived FROM the expansion, by the
//     same pass that wrote it, so checking one against the other would only prove that the
//     filesystem can read back what it just wrote. Writing it first is what closes the window
//     in which a complete tree carries no receipt — see ensureTree, where a second process
//     inside that window would throw away a directory a third is cloning.
//
// Either way a process killed mid-hydration leaves at worst something with no receipt, which
// reads as "not vouched for" and is checked or rebuilt, and never a receipt for something
// half-written. The one leftover the tree's ordering can produce — a receipt naming a tree
// that is not there — is read as damage, which is exactly what it is.
//
// NOT FSYNC'D, deliberately. A receipt is derived: losing one to a power cut costs a
// verification (an adoption re-reads the copy) and never a rebuild, and one fsync per receipt
// on a path that already avoids 31,823 of them would be a strange place to start.
//
// IT LIVES BESIDE WHAT IT DESCRIBES, not inside it. Inside would mean the golden tree
// carries a file the archive never had, and {@link cloneTree} would copy that file into
// every working directory, where it would have to be excluded from the listing it is the
// receipt for. Beside is `<dest>.hydrated`, which an operator sees next to the thing it
// vouches for and which cannot be confused for part of the artifact.

// receiptSchema versions the file. A receipt written by a future binary is not read by
// guesswork: an unrecognised schema means "not hydrated by me", which re-verifies and
// re-receipts rather than trusting a shape it does not know.
const receiptSchema = "kontra.hydration/v1"

// receiptSuffix is appended to the path of the thing being vouched for.
const receiptSuffix = ".hydrated"

// receipt is the proof that a working copy was whole.
type receipt struct {
	Schema   string `json:"schema"`
	Artifact string `json:"artifact"`
	Digest   string `json:"digest"` // the artifact's own address
	Kind     Kind   `json:"kind"`

	// Listing is the CAS address of the inventory this copy was checked against, or "" for
	// a single-file artifact, whose inventory is its own digest.
	Listing string `json:"listing,omitempty"`

	Files    int   `json:"files,omitempty"`
	Symlinks int   `json:"symlinks,omitempty"`
	Dirs     int   `json:"dirs,omitempty"`
	Bytes    int64 `json:"bytes"`

	Mode     string     `json:"mode"`
	Method   cas.Method `json:"method,omitempty"`
	Verified time.Time  `json:"verified"`
}

func receiptPath(target string) string { return target + receiptSuffix }

// readReceipt reads the receipt beside target. A receipt that is missing, unparseable, of an
// unknown schema, or about a DIFFERENT artifact is reported as no receipt at all — every one
// of those means the same thing to the caller, which is "this has not been vouched for", and
// the answer to all of them is the same.
func readReceipt(target, digest string) (*receipt, error) {
	body, err := os.ReadFile(receiptPath(target))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hydration receipt %s: %w", receiptPath(target), err)
	}
	var r receipt
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil
	}
	if r.Schema != receiptSchema || r.Digest != digest {
		return nil, nil
	}
	return &r, nil
}

// writeReceipt publishes the receipt by rename, so that a receipt is never half a file. A
// truncated receipt would be read as "not hydrated" and cost a re-hydration, which is safe
// but is 450 MB of safe.
func writeReceipt(target string, r receipt) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := receiptPath(target)
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".writing-")
	if err != nil {
		return cas.DiskError("write hydration receipt in", filepath.Dir(path), err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(body); err != nil {
		return cas.DiskError("write hydration receipt", name, err)
	}
	if err := tmp.Close(); err != nil {
		return cas.DiskError("close hydration receipt", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return cas.DiskError("publish hydration receipt", path, err)
	}
	return nil
}

// dropReceipt removes a receipt without removing what it describes. Used before a copy is
// discarded, so that a crash between "throw the copy away" and "throw its receipt away"
// cannot leave a receipt vouching for a directory that is being deleted.
func dropReceipt(target string) error {
	if err := os.Remove(receiptPath(target)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove hydration receipt %s: %w", receiptPath(target), err)
	}
	return nil
}
