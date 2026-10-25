package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

// The embedded set is exactly the files in this directory, and each is a goose single-file migration.
func TestEmbeddedMigrations(t *testing.T) {
	names, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 || names[0] != "0001_init.sql" {
		t.Fatalf("embedded migrations = %v, want 0001_init.sql first", names)
	}
	for _, n := range names {
		b, err := fs.ReadFile(FS, n)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		up, down := strings.Index(s, "-- +goose Up"), strings.Index(s, "-- +goose Down")
		if up < 0 || down < 0 || down < up {
			t.Errorf("%s: want one -- +goose Up section followed by one -- +goose Down section", n)
		}
	}
}
