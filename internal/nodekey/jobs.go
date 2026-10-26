package nodekey

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// JobsDirName is the per-job keys' directory under ~/.kontra (spec/tunnel.md §2.3).
const JobsDirName = "jobs"

// jobKeySuffix is a job key file's extension.
const jobKeySuffix = ".key"

// JobKeys are a hiring node's per-job keys for one gate: ~/.kontra/jobs/<gate host>/<job id>.key, 0600, in
// the identity file's format (spec/tunnel.md §2.3, spec/ext-sealed.md §4). A job is an A2A task, an MCP task,
// or an MCP session for its plain calls. The public half travels as replyTo inside the sealed request header
// and as metadata["kontra.sealed.replyTo"]; the private half never leaves the node.
//
// The directories jobs and jobs/<gate host> are created 0700, and one that exists with any access for group or
// others is refused. A file is written in full to a temporary file in the same directory and linked into place,
// so a key is never read half written and never replaced. `age -d -i <job id>.key` opens a reply sealed to it.
//
// Work package: M3 WP-01 (the store), M3 WP-08 (minting at send time, deletion at terminal + retention).
type JobKeys struct {
	dir string
}

// OpenJobKeys opens (creating, 0700) the jobs directory of one gate host under home's ~/.kontra.
//
// gateHost and every job id are one path element: not empty, not "." or "..", no "/" or "\", no control
// character, at most 200 bytes; a job id also starts with neither "." nor "-" (the temporary files start with
// ".").
func OpenJobKeys(home, gateHost string) (*JobKeys, error) {
	if home == "" {
		return nil, errors.New("nodekey: jobs: no home directory")
	}
	if err := pathElement("gate host", gateHost); err != nil {
		return nil, err
	}
	kontra := filepath.Join(home, DirName)
	if err := ensureDir(kontra); err != nil {
		return nil, err
	}
	jobs := filepath.Join(kontra, JobsDirName)
	dir := filepath.Join(jobs, gateHost)
	for _, d := range []string{jobs, dir} {
		if err := privateDir(d); err != nil {
			return nil, err
		}
	}
	return &JobKeys{dir: dir}, nil
}

// Dir is the gate host's jobs directory.
func (j *JobKeys) Dir() string { return j.dir }

// Path is where jobID's key is kept.
func (j *JobKeys) Path(jobID string) (string, error) {
	if err := jobElement(jobID); err != nil {
		return "", err
	}
	return filepath.Join(j.dir, jobID+jobKeySuffix), nil
}

// Mint creates a new X25519 key for jobID and writes it before returning; an existing jobID is an error (a
// job's key is never replaced: replies already sealed to it must still open).
//
// The file has the identity file's three comment lines (created, public key, kontra key id) above the
// AGE-SECRET-KEY-1 line.
func (j *JobKeys) Mint(jobID string, now time.Time) (*age.X25519Identity, error) {
	path, err := j.Path(jobID)
	if err != nil {
		return nil, err
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("nodekey: generate: %w", err)
	}
	f := &keyFile{blocks: []block{newBlock(key, now)}}
	switch err := createNew(path, f.bytes()); {
	case errors.Is(err, fs.ErrExist):
		return nil, fmt.Errorf("nodekey: job %s already has a key (%s): a job's key is never replaced", jobID, path)
	case err != nil:
		return nil, err
	}
	return key, nil
}

// Load returns jobID's key, refusing a file readable by group or others.
//
// The file must be a regular file (not a symbolic link) whose first key is an X25519 one.
func (j *JobKeys) Load(jobID string) (*age.X25519Identity, error) {
	path, err := j.Path(jobID)
	if err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("nodekey: job key: %w", err)
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return nil, fmt.Errorf("nodekey: job key: %w", err)
	}
	if err := checkInfo(path, fi); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(fh, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("nodekey: job key: %w", err)
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("nodekey: %s is over %d bytes", path, maxFileSize)
	}
	id, _, err := parse(path, data)
	if err != nil {
		return nil, err
	}
	return id.Active, nil
}

// Rename moves a key minted under a provisional id (a new task's first message, sent before the gate's task
// id exists) to the gate's job id once the first answer names it.
//
// It never replaces a key: a to that already has another key is an error and from's key stays where it was. A
// Rename interrupted after linking to and before removing from (both names on one file) is finished by the next
// Rename(from, to). On a file system without hard links it renames instead, unless to exists, as Mint does
// (createNew); a Rename retried after from is gone is an error, since to's key cannot be told to be from's.
func (j *JobKeys) Rename(from, to string) error {
	src, err := j.Path(from)
	if err != nil {
		return err
	}
	dst, err := j.Path(to)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	exists := func() error {
		return fmt.Errorf("nodekey: job %s already has a key (%s): a job's key is never replaced", to, dst)
	}
	if err := link(src, dst); err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			// An earlier Rename may have linked and died before the removal: the same file under both names.
			si, serr := os.Lstat(src)
			di, derr := os.Lstat(dst)
			if serr != nil || derr != nil || !os.SameFile(si, di) {
				return exists()
			}
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("nodekey: job key: %w", err)
		default:
			// A file system without hard links: rename, unless to appeared meanwhile.
			if _, serr := os.Lstat(dst); serr == nil {
				return exists()
			} else if !errors.Is(serr, fs.ErrNotExist) {
				return fmt.Errorf("nodekey: job key: %w", serr)
			}
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("nodekey: job key: %w", err)
			}
			return syncDir(j.dir)
		}
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("nodekey: job key: %w", err)
	}
	return syncDir(j.dir)
}

// link is os.Link, replaced in tests by a file system without hard links.
var link = os.Link

// Delete removes jobID's key: when the job is terminal and any kontra.keyRetainUntil has passed; for an MCP
// session, when it closes and every task created in it is done (spec/tunnel.md §2.3).
//
// Deleting a key that is not there is not an error, so a retried cleanup succeeds.
func (j *JobKeys) Delete(jobID string) error {
	path, err := j.Path(jobID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("nodekey: job key: %w", err)
	}
	return syncDir(j.dir)
}

// privateDir creates dir 0700 when missing and refuses one that exists with access for group or others, or is
// not a directory (a symbolic link to one is followed).
func privateDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, DirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("nodekey: %w", err)
		}
		if err := os.Chmod(dir, DirMode); err != nil {
			return fmt.Errorf("nodekey: %w", err)
		}
		fi, err = os.Stat(dir)
		if err != nil {
			return fmt.Errorf("nodekey: %w", err)
		}
	case err != nil:
		return fmt.Errorf("nodekey: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("nodekey: %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("nodekey: %s has mode %04o: the per-job keys' directory must not be open to group or others "+
			"(chmod 700 %s)", dir, perm, dir)
	}
	return nil
}

func pathElement(what, s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 200 || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("nodekey: %s %q is not one path element", what, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("nodekey: %s %q has a control character", what, s)
		}
	}
	return nil
}

func jobElement(jobID string) error {
	if err := pathElement("job id", jobID); err != nil {
		return err
	}
	if strings.HasPrefix(jobID, ".") || strings.HasPrefix(jobID, "-") {
		return fmt.Errorf("nodekey: job id %q starts with %q", jobID, jobID[:1])
	}
	return nil
}
