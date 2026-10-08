// index.go — the on-disk naming: which manifests a repository holds, what each tag points at,
// and the address this registry bound.
//
// THE ONLY MUTABLE THING IN THE REGISTRY. Every byte of content is in the CAS, addressed by its
// digest, and a digest does not move; a TAG does. So this is the whole of what can be wrong on
// disk, which is why it is a module of its own rather than the bottom third of the HTTP surface —
// its correctness question is "does the directory say what the last successful push meant", and
// answering that should not require reading a route.
//
//	<root>/repositories/<name>/_manifests/<hex>   {"mediaType":…,"size":…}
//	<root>/repositories/<name>/_tags/<tag>        sha256:<hex>
//	<root>/_uploads/<id>                          an in-flight blob
//	<root>/address                                the bound host:port
//
// `_manifests`, `_tags` and `_uploads` begin with an underscore, which no OCI repository-name
// component may — so no repository can ever be named such that its directory collides with the
// bookkeeping of another. That is why the real registry spells them that way too.
//
// EVERY WRITE IS ALL OR NOTHING (writeFileAtomic), and a tag is the reason: a half-written
// `_tags/<tag>` is a digest that resolves to a truncated address, and the pull it breaks happens
// on a different machine at a different time.
package testregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// registryIndex is the NAMING half of the registry: which manifests a repository holds, and what
// each of its tags points at. It is small on purpose — every byte of content is in the CAS, and
// this is the part that is mutable, because a tag moves and a digest does not.
//
//	<root>/repositories/<name>/_manifests/<hex>   {"mediaType":…,"size":…}
//	<root>/repositories/<name>/_tags/<tag>        sha256:<hex>
//	<root>/_uploads/<id>                          an in-flight blob
//	<root>/address                                the bound host:port
//
// `_manifests`, `_tags` and `_uploads` begin with an underscore, which no OCI repository-name
// component may — so no repository can ever be named such that its directory collides with the
// bookkeeping of another. That is why the real registry spells them that way too.
type registryIndex struct{ root string }

func newRegistryIndex(root string) (*registryIndex, error) {
	x := &registryIndex{root: root}
	for _, d := range []string{filepath.Join(root, "repositories"), x.uploadsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create registry directory %s: %w", d, err)
		}
	}
	return x, nil
}

func (x *registryIndex) uploadsDir() string { return filepath.Join(x.root, "_uploads") }

func (x *registryIndex) repoDir(name string) string {
	return filepath.Join(x.root, "repositories", filepath.FromSlash(name))
}

func (x *registryIndex) publishAddress(addr string) error {
	return writeFileAtomic(filepath.Join(x.root, addressFileName), []byte(addr+"\n"))
}

func (x *registryIndex) withdrawAddress() error {
	err := os.Remove(filepath.Join(x.root, addressFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (x *registryIndex) clearUploads() (int, error) {
	entries, err := os.ReadDir(x.uploadsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(x.uploadsDir(), e.Name())); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (x *registryIndex) putRevision(name, sum string, rev revision) error {
	b, err := json.Marshal(rev)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(x.repoDir(name), "_manifests", sum), b)
}

func (x *registryIndex) revision(name, sum string) (revision, bool, error) {
	var rev revision
	b, err := os.ReadFile(filepath.Join(x.repoDir(name), "_manifests", sum))
	if errors.Is(err, fs.ErrNotExist) {
		return rev, false, nil
	}
	if err != nil {
		return rev, false, err
	}
	if err := json.Unmarshal(b, &rev); err != nil {
		return rev, false, err
	}
	return rev, true, nil
}

func (x *registryIndex) putTag(name, tag, digest string) error {
	return writeFileAtomic(filepath.Join(x.repoDir(name), "_tags", tag), []byte(digest))
}

func (x *registryIndex) tag(name, tag string) (string, bool, error) {
	b, err := os.ReadFile(filepath.Join(x.repoDir(name), "_tags", tag))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(b)), true, nil
}

func (x *registryIndex) tags(name string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(x.repoDir(name), "_tags"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (x *registryIndex) hasRepository(name string) (bool, error) {
	st, err := os.Stat(filepath.Join(x.repoDir(name), "_manifests"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st.IsDir(), nil
}

func (x *registryIndex) repositories() ([]string, error) {
	base := filepath.Join(x.root, "repositories")
	var out []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); name == "_manifests" || name == "_tags" {
			return fs.SkipDir
		}
		if p == base {
			return nil
		}
		if st, serr := os.Stat(filepath.Join(p, "_manifests")); serr == nil && st.IsDir() {
			rel, rerr := filepath.Rel(base, p)
			if rerr != nil {
				return rerr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// writeFileAtomic is temp-then-rename, because a tag flip must be all or nothing: a half-written
// `_tags/<tag>` is a digest that resolves to a truncated address, and the pull it breaks happens
// on a different machine at a different time.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
