// objects.go — the object store under the appliance's S3 surface: a directory tree, plus the
// three things a directory tree is not.
//
// THE LAYOUT IS THE POINT. An object at key `datasets/actor=crawl/dt=2026-08-26/part-0.parquet`
// in bucket `kontra` is the file `<data-dir>/objects/kontra/datasets/actor=crawl/…/part-0.parquet`
// and nothing else — no sidecars, no manifest, no index. `ls` is a working `aws s3 ls`, `du -sh`
// is a working storage report, and a backup is `tar`. SeaweedFS's needle files were none of those,
// and the cost of that opacity was measured: when its volume-slot ceiling was hit, every write to
// a new bucket returned a bare HTTP 500 and there was nothing on disk an operator could look at.
//
// The three things a directory tree is not, and what this file does about each:
//
//  1. A tree has no ordering guarantee, and S3 LIST does. A depth-first walk emits `a/b` before
//     `a-c` because the kernel-visible names sort that way, while S3 orders by the KEY, where '-'
//     (0x2D) precedes '/' (0x2F). listEntries fixes that by sorting directory entries under their
//     key spelling — a directory `a` sorts as `a/` — which makes the walk globally lexicographic
//     and therefore makes continuation tokens mean something.
//
//  2. A tree loses writes quietly. write() below is temp-file → fsync → rename → fsync(dir), and
//     the fsync is not belt-and-braces: on ext4 with delayed allocation a full disk reports ENOSPC
//     at FLUSH, not at write(2), so a store that skips it returns 200 for bytes that never landed.
//     That is one half of the bug this store exists to not inherit; the other half is that the
//     error must arrive at the caller NAMING ITS CAUSE, which is storageError's job.
//
//  3. A tree cannot hold two objects whose keys are `a` and `a/b`. S3 can; a filesystem cannot
//     have `a` be a file and a directory at once. Rather than escape key bytes and lose (1)'s
//     legibility, this store REFUSES that write and says exactly which existing key blocks it.
//     No kontra key layout can produce the collision — CAS keys are fixed-depth, blob keys are
//     hive-partitioned, bundle keys end in a filename — so the refusal is a guard, not a limit
//     anyone is expected to hit.
package objstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// uploadsDir holds in-flight multipart parts. It lives beside the buckets rather than inside one
// because a partial upload is not an object: a leading dot cannot be a legal S3 bucket name, so
// nothing a client can name will ever collide with it or list its contents.
const uploadsDir = ".uploads"

// objectStore is the filesystem object store. Construct it with newObjectStore.
type objectStore struct {
	root string

	// injectWriteError is a TEST SEAM and is nil in every production path. A real ENOSPC needs a
	// full filesystem, which needs a loopback mount and root, which no test suite should require
	// — and leaving the disk-full path untested is how it stayed broken in SeaweedFS long enough
	// to cost a run. Set it and every object write returns what the filesystem would have.
	injectWriteError func(pathOnDisk string) error
}

// etagFor is this store's entity tag, and it is deliberately NOT the MD5 of the object's bytes.
//
// An ETag has exactly one job that anything in kontra depends on: being STABLE while the content
// is unchanged and DIFFERENT after a write, because DuckDB's httpfs reads it on its first HEAD of
// a parquet file and re-checks it on every later range GET to notice the file moving under a
// running query. A fingerprint of (key, size, mtime-ns) does that in constant time.
//
// The MD5 would not. LIST returns an ETag per object, so `ObjectStore.list('datasets/')` over a
// run's outputs would hash every parquet file in the bucket to answer one page — hours of
// I/O for a listing, and there is no cache that survives the first cold call. Real S3 does not
// promise MD5 either: multipart and encrypted objects already break that equivalence, and the
// AWS docs say so.
//
// NOTHING IN KONTRA CHECKS INTEGRITY THROUGH THE ETag. The CAS addresses objects by sha256 and
// verifies that hash on read (runtime/handler/internal/cas), which is the property `sha256sum <file>`
// checks against a key on disk. If some future consumer ever needs ETag==MD5, it needs a stored
// digest, not a recomputation — that is the change to make, not this one.
func etagFor(key string, size int64, mod time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d", key, size, mod.UnixNano())
	return `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
}

func newObjectStore(root string) (*objectStore, error) {
	if root == "" {
		return nil, errors.New("appliance: the object store needs a root directory")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("appliance: object store root %s: %w", root, err)
	}
	return &objectStore{root: root}, nil
}

// --- errors -------------------------------------------------------------------------------

// s3Error is a failure the way an S3 client will see it: a code it can switch on, an HTTP status,
// and a message written for whoever has to fix it.
type s3Error struct {
	Code    string
	Message string
	Status  int
	Err     error
}

func (e *s3Error) Error() string { return e.Code + ": " + e.Message }
func (e *s3Error) Unwrap() error { return e.Err }

func errNoSuchKey(bucket, key string) *s3Error {
	return &s3Error{Code: "NoSuchKey", Status: 404,
		Message: fmt.Sprintf("no object %q in bucket %q", key, bucket)}
}

func errNoSuchBucket(bucket string) *s3Error {
	return &s3Error{Code: "NoSuchBucket", Status: 404,
		Message: fmt.Sprintf("no bucket %q", bucket)}
}

func errInvalid(format string, a ...any) *s3Error {
	return &s3Error{Code: "InvalidArgument", Status: 400, Message: fmt.Sprintf(format, a...)}
}

// storageError is where a write failure becomes something an operator can act on.
//
// THIS IS THE FUNCTION THE ISSUE IS ABOUT. SeaweedFS answered a full disk with a bare HTTP 500
// and no body, and the run's own counters reported nothing wrong, so "the store is full" arrived
// looking like "the store is broken" — days later. Every branch below carries the errno's meaning
// in words and the path it happened to, and the two that are permanent get a status the AWS SDKs
// do NOT retry, so a full disk fails in one round trip instead of four.
func storageError(op, pathOnDisk string, err error) *s3Error {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return &s3Error{Code: "InsufficientStorage", Status: 507, Err: err,
			Message: fmt.Sprintf("%s %s: no space left on device — the appliance's data directory is full. "+
				"Free space or move the data directory; nothing was written.", op, pathOnDisk)}
	case errors.Is(err, syscall.EDQUOT):
		return &s3Error{Code: "InsufficientStorage", Status: 507, Err: err,
			Message: fmt.Sprintf("%s %s: disk quota exceeded for the user running the appliance; nothing was written.", op, pathOnDisk)}
	case errors.Is(err, syscall.EROFS):
		return &s3Error{Code: "InsufficientStorage", Status: 507, Err: err,
			Message: fmt.Sprintf("%s %s: the filesystem is mounted read-only; nothing was written.", op, pathOnDisk)}
	case errors.Is(err, syscall.ENOTDIR):
		// The one shape a directory tree cannot represent — see this file's header, point 3.
		return &s3Error{Code: "KeyPathConflict", Status: 409, Err: err,
			Message: fmt.Sprintf("%s %s: a prefix of this key already exists as an object, so the key cannot also be a "+
				"directory. Delete the conflicting object or choose a key that is not a prefix of another.", op, pathOnDisk)}
	case errors.Is(err, syscall.ENAMETOOLONG):
		return &s3Error{Code: "KeyTooLongError", Status: 400, Err: err,
			Message: fmt.Sprintf("%s %s: the key is longer than this filesystem allows for one path component (255 bytes).", op, pathOnDisk)}
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return &s3Error{Code: "InternalError", Status: 500, Err: err,
			Message: fmt.Sprintf("%s %s: permission denied — the appliance cannot write its own data directory.", op, pathOnDisk)}
	default:
		return &s3Error{Code: "InternalError", Status: 500, Err: err,
			Message: fmt.Sprintf("%s %s: %v", op, pathOnDisk, err)}
	}
}

// --- names --------------------------------------------------------------------------------

// validBucket applies S3's bucket-name rules, minus the ones that only matter for DNS names in a
// global namespace. The point is not compatibility theatre: it is that a bucket is a directory
// here, so `..`, `/` and a leading dot must never reach the filesystem.
func validBucket(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return &s3Error{Code: "InvalidBucketName", Status: 400,
			Message: fmt.Sprintf("bucket name %q must be 3–63 characters", name)}
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return &s3Error{Code: "InvalidBucketName", Status: 400,
				Message: fmt.Sprintf("bucket name %q may hold only lowercase letters, digits, '-' and '.'", name)}
		}
	}
	if name[0] == '-' || name[0] == '.' || name[len(name)-1] == '-' || name[len(name)-1] == '.' {
		return &s3Error{Code: "InvalidBucketName", Status: 400,
			Message: fmt.Sprintf("bucket name %q may not start or end with '-' or '.'", name)}
	}
	return nil
}

// objectPath maps bucket+key onto a file, refusing every key a directory tree would resolve
// somewhere other than where the key says.
func (s *objectStore) objectPath(bucket, key string) (string, error) {
	if err := validBucket(bucket); err != nil {
		return "", err
	}
	if key == "" {
		return "", errInvalid("empty object key")
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return "", errInvalid("object key %q may not begin or end with '/' (this store has no directory objects)", key)
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return "", errInvalid("object key %q has an empty path segment ('//')", key)
		case ".", "..":
			return "", errInvalid("object key %q contains a relative path segment (%q)", key, seg)
		}
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] == 0x7f {
			return "", errInvalid("object key contains a control byte at offset %d", i)
		}
	}
	full := filepath.Join(s.root, bucket, filepath.FromSlash(key))
	// Defence in depth: after Join has cleaned the path, it must still be inside the bucket.
	base := filepath.Join(s.root, bucket)
	if full != base && !strings.HasPrefix(full, base+string(filepath.Separator)) {
		return "", errInvalid("object key %q escapes its bucket", key)
	}
	return full, nil
}

// --- buckets ------------------------------------------------------------------------------

func (s *objectStore) createBucket(bucket string) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	p := filepath.Join(s.root, bucket)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return storageError("creating bucket", p, err)
	}
	return nil
}

func (s *objectStore) hasBucket(bucket string) bool {
	if validBucket(bucket) != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(s.root, bucket))
	return err == nil && fi.IsDir()
}

type bucketInfo struct {
	Name    string
	Created time.Time
}

func (s *objectStore) buckets() ([]bucketInfo, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, storageError("listing buckets", s.root, err)
	}
	var out []bucketInfo
	for _, e := range ents {
		if !e.IsDir() || validBucket(e.Name()) != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, bucketInfo{Name: e.Name(), Created: fi.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// --- objects ------------------------------------------------------------------------------

type objectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
	ETag     string // quoted, as S3 renders it
}

// put writes an object and returns its metadata. `r` is streamed to disk, never buffered, because
// the appliance shares a heap with a Node orchestrator and a Temporal server on a 4 GB controller
// and a parquet file is not a thing to hold in memory.
func (s *objectStore) put(bucket, key string, r io.Reader) (objectInfo, error) {
	p, err := s.objectPath(bucket, key)
	if err != nil {
		return objectInfo{}, err
	}
	size, err := s.write(p, r)
	if err != nil {
		return objectInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return objectInfo{}, storageError("stat", p, err)
	}
	mod := fi.ModTime().UTC()
	return objectInfo{Key: key, Size: size, Modified: mod, ETag: etagFor(key, fi.Size(), mod)}, nil
}

// write is the ONE syscall path every object write takes: temp file in the destination directory,
// fsync, atomic rename, fsync the directory. Each step's error is checked and named.
//
// DO NOT DROP THE fsync TO MAKE WRITES FASTER. It is what turns a full disk into an error: ext4's
// delayed allocation defers the ENOSPC to flush time, so without it write(2) and close(2) both
// succeed on a full filesystem and the object silently is not there. That is the exact failure
// this store was written to stop inheriting.
func (s *objectStore) write(p string, r io.Reader) (size int64, err error) {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, storageError("creating", dir, err)
	}
	if s.injectWriteError != nil {
		if e := s.injectWriteError(p); e != nil {
			return 0, storageError("writing", p, e)
		}
	}
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return 0, storageError("creating a temporary file in", dir, err)
	}
	tmpName := tmp.Name()
	// 0644, not CreateTemp's 0600. The directories around these files are 0755 and the store
	// serves every object to anyone who can reach the port, so a 0600 file is not a security
	// boundary — it is only a `tar` that fails, or a bind-mounted container reading as another
	// uid that cannot, which is the class of problem ADR 0031 exists to remove.
	if err = tmp.Chmod(0o644); err != nil {
		return 0, storageError("setting permissions on", p, err)
	}
	// Any exit before the rename leaves nothing behind. A half-written object that survives a
	// failed PUT is worse than no object: the CAS would read it back and fail its sha256 check
	// with an integrity error, pointing at corruption rather than at a full disk.
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	n, err := io.Copy(tmp, r)
	if err != nil {
		return 0, storageError("writing", p, err)
	}
	if err = tmp.Sync(); err != nil {
		return 0, storageError("flushing", p, err)
	}
	if err = tmp.Close(); err != nil {
		return 0, storageError("closing", p, err)
	}
	if err = os.Rename(tmpName, p); err != nil {
		return 0, storageError("renaming into place", p, err)
	}
	if d, derr := os.Open(dir); derr == nil {
		// Best effort, and only this one: the bytes are already durable, and a directory that has
		// not been synced costs an object on a power cut, not a wrong answer.
		_ = d.Sync()
		d.Close()
	}
	return n, nil
}

// open returns the object as a seekable file plus its metadata. The caller closes it. Seekable
// because Range requests are served straight off it — DuckDB reads a parquet footer before it
// reads anything else, and a store that could not seek would stream the whole file to answer it.
func (s *objectStore) open(bucket, key string) (*os.File, objectInfo, error) {
	p, err := s.objectPath(bucket, key)
	if err != nil {
		return nil, objectInfo{}, err
	}
	if !s.hasBucket(bucket) {
		return nil, objectInfo{}, errNoSuchBucket(bucket)
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, objectInfo{}, errNoSuchKey(bucket, key)
		}
		return nil, objectInfo{}, storageError("reading", p, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, objectInfo{}, storageError("stat", p, err)
	}
	if fi.IsDir() {
		f.Close()
		return nil, objectInfo{}, errNoSuchKey(bucket, key)
	}
	mod := fi.ModTime().UTC()
	return f, objectInfo{Key: key, Size: fi.Size(), Modified: mod, ETag: etagFor(key, fi.Size(), mod)}, nil
}

func (s *objectStore) stat(bucket, key string) (objectInfo, error) {
	f, info, err := s.open(bucket, key)
	if err != nil {
		return objectInfo{}, err
	}
	f.Close()
	return info, nil
}

func (s *objectStore) delete(bucket, key string) error {
	p, err := s.objectPath(bucket, key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return storageError("deleting", p, err)
	}
	// Directories left empty by the delete are pruned back up to the bucket, so a retention sweep
	// leaves a tree that reflects what is actually stored. RemoveAll is never used here — only
	// os.Remove, which refuses a non-empty directory, so a concurrent write cannot be swept away.
	for d := filepath.Dir(p); d != filepath.Join(s.root, bucket); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
	return nil
}

// --- listing ------------------------------------------------------------------------------

type listParams struct {
	Prefix    string
	Delimiter string
	MaxKeys   int
	After     string // continuation token or start-after; both mean "keys strictly greater than"
}

type listResult struct {
	Objects        []objectInfo
	CommonPrefixes []string
	Truncated      bool
	NextToken      string
}

const (
	defaultMaxKeys = 1000
	maxMaxKeys     = 1000
)

// list is ListObjectsV2 over the tree.
//
// It walks rather than collecting-then-sorting, which is what makes it usable on a bucket holding
// a run: `kontra explore` lists one run's partitions out of a bucket with millions of blob
// keys, and a listing that materialised every key first would allocate hundreds of megabytes to
// return a thousand. Both the prefix and the continuation token PRUNE the walk — a subtree that
// cannot contain a matching key, or that lies entirely behind the token, is never opened.
func (s *objectStore) list(bucket string, p listParams) (listResult, error) {
	if err := validBucket(bucket); err != nil {
		return listResult{}, err
	}
	if !s.hasBucket(bucket) {
		return listResult{}, errNoSuchBucket(bucket)
	}
	if p.Delimiter != "" && p.Delimiter != "/" {
		return listResult{}, &s3Error{Code: "NotImplemented", Status: 501,
			Message: fmt.Sprintf("delimiter %q: this store groups by %q only, which is the only delimiter kontra uses", p.Delimiter, "/")}
	}
	if p.MaxKeys <= 0 || p.MaxKeys > maxMaxKeys {
		p.MaxKeys = defaultMaxKeys
	}

	var res listResult
	// One over the limit: seeing the extra entry is how "is there more" is answered without
	// guessing, and guessing here would mean a client that stops one page early.
	limit := p.MaxKeys + 1
	err := s.walk(bucket, "", p, limit, &res)
	if err != nil {
		return listResult{}, err
	}
	if n := len(res.Objects) + len(res.CommonPrefixes); n > p.MaxKeys {
		res.Truncated = true
		// Drop the probe entry, whichever list it landed in, and hand back the last key we kept.
		if len(res.Objects) > 0 && (len(res.CommonPrefixes) == 0 ||
			res.Objects[len(res.Objects)-1].Key > res.CommonPrefixes[len(res.CommonPrefixes)-1]) {
			res.Objects = res.Objects[:len(res.Objects)-1]
		} else {
			res.CommonPrefixes = res.CommonPrefixes[:len(res.CommonPrefixes)-1]
		}
		res.NextToken = lastEmitted(res)
	}
	return res, nil
}

func lastEmitted(r listResult) string {
	var last string
	if n := len(r.Objects); n > 0 {
		last = r.Objects[n-1].Key
	}
	if n := len(r.CommonPrefixes); n > 0 && r.CommonPrefixes[n-1] > last {
		last = r.CommonPrefixes[n-1]
	}
	return last
}

// walk emits keys under `dir` (a key prefix ending in '/', or "" for the bucket root) in S3's
// lexicographic order, stopping once `limit` entries have been emitted.
func (s *objectStore) walk(bucket, dir string, p listParams, limit int, res *listResult) error {
	if len(res.Objects)+len(res.CommonPrefixes) >= limit {
		return nil
	}
	onDisk := filepath.Join(s.root, bucket, filepath.FromSlash(dir))
	ents, err := os.ReadDir(onDisk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return storageError("listing", onDisk, err)
	}

	// Sort by the KEY spelling, not the filename: a directory's keys all begin with its name plus
	// a '/', so it sorts as if it were that string. Without this the walk is not lexicographic
	// and a continuation token cannot be trusted (see this file's header, point 1).
	type entry struct {
		name  string
		dir   bool
		sortK string
	}
	list := make([]entry, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".partial-") {
			continue // an in-flight write, not an object
		}
		isDir := e.IsDir()
		k := dir + name
		if isDir {
			k += "/"
		}
		list = append(list, entry{name: name, dir: isDir, sortK: k})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].sortK < list[j].sortK })

	for _, e := range list {
		if len(res.Objects)+len(res.CommonPrefixes) >= limit {
			return nil
		}
		full := e.sortK
		if !e.dir {
			if !strings.HasPrefix(full, p.Prefix) || full <= p.After {
				continue
			}
			fi, err := os.Stat(filepath.Join(onDisk, e.name))
			if err != nil {
				continue // vanished under us; a listing is a snapshot, not a lock
			}
			mod := fi.ModTime().UTC()
			res.Objects = append(res.Objects, objectInfo{
				Key: full, Size: fi.Size(), Modified: mod, ETag: etagFor(full, fi.Size(), mod),
			})
			continue
		}

		// A directory. Everything under it starts with `full`, which ends in '/'.
		//
		// Skip the whole subtree when the token is already past it: the token is a key, so if it
		// sorts at or after `full` without being inside it, nothing under it can be new.
		if p.After != "" && p.After >= full && !strings.HasPrefix(p.After, full) {
			continue
		}
		switch {
		case strings.HasPrefix(p.Prefix, full):
			// The prefix reaches INTO this directory — descend, still narrowing.
			if err := s.walk(bucket, full, p, limit, res); err != nil {
				return err
			}
		case strings.HasPrefix(full, p.Prefix):
			// The directory is inside the prefix. With a delimiter it collapses to one common
			// prefix and is not opened at all — which is what makes a delimited listing of a
			// hive-partitioned bucket cheap.
			if p.Delimiter == "/" {
				if full > p.After {
					res.CommonPrefixes = append(res.CommonPrefixes, full)
				}
				continue
			}
			if err := s.walk(bucket, full, p, limit, res); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- multipart ----------------------------------------------------------------------------

// Multipart exists for ONE customer and it is worth naming precisely, because the imprecise
// version is wrong: DuckDB's httpfs writes a parquet file to S3 as a multipart upload ONCE IT
// EXCEEDS ITS PART SIZE, and below that as a single PutObject. MEASURED against this store with
// DuckDB 1.5.5: a 12.5 MB parquet went as one PUT at the default settings (part size is
// `s3_uploader_max_filesize` / `s3_uploader_max_parts_per_file`, so ~80 MB out of the box), and
// the same file went as three parts with `SET s3_uploader_max_filesize='50MB'`.
//
// That matters because it says who this code is for. It is not on the path of every dataset
// write; it is on the path of the BIG ones — a run's partition over ~80 MB — which are
// exactly the writes that must not be re-done from the start. The codec's 128 KiB claim-check
// offload never comes through here at all; that is a single PUT.
//
// Parts are files under .uploads/<id>/, so an interrupted upload costs disk and not memory, and
// an abandoned one is visible to `ls` rather than hidden in a heap.
type completedPart struct {
	PartNumber int
	ETag       string
}

func (s *objectStore) newUpload(bucket, key string) (string, error) {
	if _, err := s.objectPath(bucket, key); err != nil {
		return "", err
	}
	if err := s.createBucket(bucket); err != nil {
		return "", err
	}
	base := filepath.Join(s.root, uploadsDir)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", storageError("creating", base, err)
	}
	dir, err := os.MkdirTemp(base, "u-")
	if err != nil {
		return "", storageError("creating an upload directory in", base, err)
	}
	// The target rides in a file rather than in the id, so an upload id stays an opaque token and
	// a client cannot redirect a completion at another key by editing it.
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(bucket+"\n"+key), 0o644); err != nil {
		return "", storageError("writing", target, err)
	}
	return filepath.Base(dir), nil
}

func (s *objectStore) uploadTarget(uploadID string) (bucket, key string, err error) {
	dir, err := s.uploadDir(uploadID)
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "target"))
	if err != nil {
		return "", "", errNoSuchUpload(uploadID)
	}
	bucket, key, _ = strings.Cut(string(b), "\n")
	return bucket, key, nil
}

func errNoSuchUpload(id string) *s3Error {
	return &s3Error{Code: "NoSuchUpload", Status: 404,
		Message: fmt.Sprintf("no multipart upload %q — it was completed, aborted, or never started here", id)}
}

// uploadDir resolves an upload id to its directory, refusing anything that is not one of ours.
func (s *objectStore) uploadDir(uploadID string) (string, error) {
	if uploadID == "" || strings.ContainsAny(uploadID, "/\\") || strings.Contains(uploadID, "..") {
		return "", errNoSuchUpload(uploadID)
	}
	dir := filepath.Join(s.root, uploadsDir, uploadID)
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", errNoSuchUpload(uploadID)
	}
	return dir, nil
}

func (s *objectStore) putPart(uploadID string, partNumber int, r io.Reader) (string, error) {
	if partNumber < 1 || partNumber > 10000 {
		return "", errInvalid("part number %d is outside 1–10000", partNumber)
	}
	dir, err := s.uploadDir(uploadID)
	if err != nil {
		return "", err
	}
	// Zero-padded so the parts sort in part-number order on disk too — an operator looking at an
	// interrupted upload sees it in the order it will be assembled.
	pp := filepath.Join(dir, "part-"+pad5(partNumber))
	size, err := s.write(pp, r)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(pp)
	if err != nil {
		return "", storageError("stat", pp, err)
	}
	// The part ETag is not decoration: DuckDB's httpfs collects these and sends them back in the
	// CompleteMultipartUpload body, and an UploadPart response without one aborts the write.
	return etagFor(fmt.Sprintf("%s/%d", uploadID, partNumber), size, fi.ModTime().UTC()), nil
}

func pad5(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 5-len(s)) + s
}

// completeUpload concatenates the named parts, in the order given, into the object.
//
// It streams part-by-part into one temp file rather than reading them into a buffer: a DuckLake
// parquet file is routinely hundreds of megabytes and the appliance shares its heap with the
// Temporal server.
func (s *objectStore) completeUpload(uploadID string, parts []completedPart) (objectInfo, error) {
	bucket, key, err := s.uploadTarget(uploadID)
	if err != nil {
		return objectInfo{}, err
	}
	dir, err := s.uploadDir(uploadID)
	if err != nil {
		return objectInfo{}, err
	}
	if len(parts) == 0 {
		return objectInfo{}, errInvalid("completing upload %s: no parts listed", uploadID)
	}
	for i := 1; i < len(parts); i++ {
		if parts[i].PartNumber <= parts[i-1].PartNumber {
			return objectInfo{}, errInvalid("completing upload %s: parts must be listed in ascending part-number order (saw %d after %d)",
				uploadID, parts[i].PartNumber, parts[i-1].PartNumber)
		}
	}

	readers := make([]io.Reader, 0, len(parts))
	files := make([]*os.File, 0, len(parts))
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, p := range parts {
		pp := filepath.Join(dir, "part-"+pad5(p.PartNumber))
		f, err := os.Open(pp)
		if err != nil {
			return objectInfo{}, &s3Error{Code: "InvalidPart", Status: 400, Err: err,
				Message: fmt.Sprintf("completing upload %s: part %d was never uploaded", uploadID, p.PartNumber)}
		}
		files = append(files, f)
		readers = append(readers, f)
	}

	target, err := s.objectPath(bucket, key)
	if err != nil {
		return objectInfo{}, err
	}
	size, err := s.write(target, io.MultiReader(readers...))
	if err != nil {
		return objectInfo{}, err
	}
	// Parts are removed only once the object is in place. An upload that dies during the
	// concatenation is resumable from the same parts; one that deleted them first would not be.
	_ = os.RemoveAll(dir)
	fi, err := os.Stat(target)
	if err != nil {
		return objectInfo{}, storageError("stat", target, err)
	}
	mod := fi.ModTime().UTC()
	// The "-<n>" suffix is S3's signal that an object arrived in parts, and it is kept because a
	// client that reads ETags at all reads that. What precedes it is this store's fingerprint,
	// not a digest of digests — see etagFor.
	etag := strings.TrimSuffix(etagFor(key, size, mod), `"`) + "-" + strconv.Itoa(len(parts)) + `"`
	return objectInfo{Key: key, Size: size, Modified: mod, ETag: etag}, nil
}

func (s *objectStore) abortUpload(uploadID string) error {
	dir, err := s.uploadDir(uploadID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return storageError("aborting upload", dir, err)
	}
	return nil
}

// copy duplicates one object onto another key. The orchestrator uses it to give a CAS blob a
// run-addressed name (`datasets/…`) that DuckDB can glob, since a content address carries no
// identity to glob by.
func (s *objectStore) copyObject(srcBucket, srcKey, dstBucket, dstKey string) (objectInfo, error) {
	f, _, err := s.open(srcBucket, srcKey)
	if err != nil {
		return objectInfo{}, err
	}
	defer f.Close()
	if err := s.createBucket(dstBucket); err != nil {
		return objectInfo{}, err
	}
	return s.put(dstBucket, dstKey, f)
}
