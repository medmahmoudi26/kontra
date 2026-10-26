package nodekey

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/tunnel"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestJobKeysMint(t *testing.T) {
	home := t.TempDir()
	j, err := OpenJobKeys(home, "gate.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, DirName, JobsDirName, "gate.example.com:8443")
	if j.Dir() != dir {
		t.Fatalf("Dir = %s, want %s", j.Dir(), dir)
	}
	for _, d := range []string{filepath.Join(home, DirName), filepath.Join(home, DirName, JobsDirName), dir} {
		if m := mode(t, d); m != 0o700 {
			t.Fatalf("%s has mode %04o, want 0700", d, m)
		}
	}

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	key, err := j.Mint("pending-1", now)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pending-1.key")
	if m := mode(t, path); m != 0o600 {
		t.Fatalf("job key has mode %04o, want 0600", m)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rcpt := key.Recipient().String()
	want := "# created: 2026-10-26T12:00:00Z\n# public key: " + rcpt + "\n# kontra key id: " + tunnel.KeyID(rcpt) + "\n" + key.String() + "\n"
	if string(data) != want {
		t.Fatalf("job key file:\n%s\nwant:\n%s", data, want)
	}
	// The identity file's format: plain age parses it, so `age -d -i pending-1.key` opens a reply sealed to it.
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil || len(ids) != 1 {
		t.Fatalf("ParseIdentities: %v", err)
	}
	var sealed bytes.Buffer
	w, _ := age.Encrypt(&sealed, key.Recipient())
	w.Write([]byte("reply"))
	w.Close()
	r, err := age.Decrypt(&sealed, ids...)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "reply" {
		t.Fatalf("decrypted %q", b)
	}
	// No temporary file is left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries", len(entries))
	}

	// A second Mint of the same id is refused, and the key stays the one replies are sealed to.
	if _, err := j.Mint("pending-1", now); err == nil || !strings.Contains(err.Error(), "never replaced") {
		t.Fatalf("second Mint: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, data) {
		t.Fatal("the key file changed")
	}

	got, err := j.Load("pending-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != key.String() {
		t.Fatal("Load returns another key")
	}
}

func TestJobKeysLoadRefusals(t *testing.T) {
	j, err := OpenJobKeys(t.TempDir(), "gate.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Mint("t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	p, _ := j.Path("t1")
	for _, m := range []os.FileMode{0o640, 0o604, 0o660, 0o644} {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Load("t1"); err == nil || !strings.Contains(err.Error(), "group or others") {
			t.Fatalf("mode %04o: Load = %v, want a refusal", m, err)
		}
	}
	os.Chmod(p, 0o600)
	if _, err := j.Load("t1"); err != nil {
		t.Fatal(err)
	}
	// A symbolic link is not a key the node wrote.
	if err := os.Symlink(p, filepath.Join(j.Dir(), "t2.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Load("t2"); err == nil {
		t.Fatal("Load follows a symbolic link")
	}
	if _, err := j.Load("missing"); err == nil {
		t.Fatal("Load of a missing key succeeds")
	}
	// A file whose first key is not X25519.
	if err := os.WriteFile(filepath.Join(j.Dir(), "bad.key"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Load("bad"); err == nil {
		t.Fatal("Load of a garbage file succeeds")
	}
}

func TestJobKeysRenameDelete(t *testing.T) {
	j, err := OpenJobKeys(t.TempDir(), "gate.example.com")
	if err != nil {
		t.Fatal(err)
	}
	prov, err := j.Mint("p-7f3a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	task := "0199f1e2-3c4d-7e8f-9a0b-1c2d3e4f5a6b"
	if err := j.Rename("p-7f3a", task); err != nil {
		t.Fatal(err)
	}
	got, err := j.Load(task)
	if err != nil || got.String() != prov.String() {
		t.Fatalf("Load after Rename: %v", err)
	}
	if _, err := j.Load("p-7f3a"); err == nil {
		t.Fatal("the provisional id still has a key")
	}
	p, _ := j.Path(task)
	if m := mode(t, p); m != 0o600 {
		t.Fatalf("renamed key mode %04o", m)
	}

	// Rename never replaces a key.
	other, err := j.Mint("p-other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Rename("p-other", task); err == nil {
		t.Fatal("Rename replaced an existing job's key")
	}
	if k, _ := j.Load(task); k.String() != prov.String() {
		t.Fatal("the job's key was replaced")
	}
	if k, _ := j.Load("p-other"); k.String() != other.String() {
		t.Fatal("the provisional key was lost")
	}
	if err := j.Rename("missing", "t9"); err == nil {
		t.Fatal("Rename of a missing key succeeds")
	}

	if err := j.Delete(task); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the key is still there: %v", err)
	}
	if err := j.Delete(task); err != nil {
		t.Fatalf("a second Delete: %v", err)
	}
}

func TestJobKeysNames(t *testing.T) {
	home := t.TempDir()
	for _, host := range []string{"", ".", "..", "a/b", `a\b`, "../etc", "a\x00b", "a\nb", strings.Repeat("h", 201)} {
		if _, err := OpenJobKeys(home, host); err == nil {
			t.Errorf("OpenJobKeys(%q) succeeds", host)
		}
	}
	if _, err := OpenJobKeys("", "gate"); err == nil {
		t.Error("an empty home is accepted")
	}
	j, err := OpenJobKeys(home, "[::1]:8443")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../x", "a/b", ".hidden", "-flag", "a\x01"} {
		if _, err := j.Mint(id, time.Now()); err == nil {
			t.Errorf("Mint(%q) succeeds", id)
		}
		if _, err := j.Load(id); err == nil {
			t.Errorf("Load(%q) succeeds", id)
		}
		if err := j.Delete(id); err == nil {
			t.Errorf("Delete(%q) succeeds", id)
		}
	}
	// What a gate session id (base64url) or a session hash (hex) looks like is a fine job id.
	for _, id := range []string{"AbC-_09xyzAbC-_09xyzAb", strings.Repeat("ab", 32)} {
		if _, err := j.Mint(id, time.Now()); err != nil {
			t.Errorf("Mint(%q): %v", id, err)
		}
	}
}

func TestJobKeysRefuseOpenDirectory(t *testing.T) {
	home := t.TempDir()
	jobs := filepath.Join(home, DirName, JobsDirName)
	if err := os.MkdirAll(filepath.Join(jobs, "gate"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(jobs, "gate"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJobKeys(home, "gate"); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("a group-readable gate directory: %v", err)
	}
	if err := os.Chmod(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJobKeys(home, "other"); err == nil {
		t.Fatal("a world-readable jobs directory is accepted")
	}
}
