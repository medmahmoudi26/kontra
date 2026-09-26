package hostengine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// THE PATH IS THE DELIVERABLE, so its shape is a test rather than a convention: sortable, UTC, no
// colon in it. A name with a colon does not survive being copied to a filesystem that will not take
// one, and a backup an operator cannot copy off the box is not a backup.
func TestSaveExportWritesASortableTimestampedFile(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 26, 23, 14, 21, 0, time.UTC)
	path, err := SaveExport(dir, []byte(`{"version":3}`), at)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(path); got != "state-20260926T231421Z.json" {
		t.Errorf("file name %q, want state-20260926T231421Z.json", got)
	}
	if filepath.Dir(path) != filepath.Join(dir, ExportDirName) {
		t.Errorf("path %q is not under %s/", path, ExportDirName)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != `{"version":3}` {
		t.Fatalf("the export was not written verbatim: %q %v", b, err)
	}
	// A LOCAL time would make two exports from two machines unorderable and, worse, would go backwards
	// once a year.
	later, err := SaveExport(dir, []byte("{}"), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(later) <= filepath.Base(path) {
		t.Errorf("%q does not sort after %q", filepath.Base(later), filepath.Base(path))
	}
}

// THE CHECKPOINT CARRIES THE ENCRYPTED FORM OF EVERY STACK SECRET, and this copy of it is going to sit
// in a directory for months. `backend.go`'s rule for the directory that holds ciphertext applies here:
// 0600 in a 0700 directory.
func TestSaveExportIsNotReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	dir := t.TempDir()
	path, err := SaveExport(dir, []byte("{}"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if bad := fi.Mode().Perm() & 0o077; bad != 0 {
		t.Errorf("the export is mode %04o — it holds the ciphertext of every stack secret", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if bad := di.Mode().Perm() & 0o077; bad != 0 {
		t.Errorf("%s is mode %04o, want 0700", filepath.Dir(path), di.Mode().Perm())
	}
}

// NOTHING IS PRUNED, and that is the decision rather than an oversight — a program that deleted the
// backup it just told an operator to keep is not a backup at all. If this ever grows a bound, the bound
// belongs to a verb somebody types.
func TestSaveExportKeepsEveryCopy(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := SaveExport(dir, []byte("{}"), base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := os.ReadDir(filepath.Join(dir, ExportDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 5 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("%d exports survived five converges: %s", len(ents), strings.Join(names, " "))
	}
}
