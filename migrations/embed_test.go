package migrations

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// The embedded set is exactly the files in this directory, numbered from 0001 with no gap (one per milestone that
// changes the schema: 0001 M1, 0002 M3, 0003 M4), and each is a goose single-file migration.
func TestEmbeddedMigrations(t *testing.T) {
	names, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 || names[0] != "0001_init.sql" {
		t.Fatalf("embedded migrations = %v, want 0001_init.sql first", names)
	}
	want := []string{"0001_init.sql", "0002_m3.sql", "0003_m4.sql"}
	if len(names) != len(want) {
		t.Fatalf("embedded migrations = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] || !strings.HasPrefix(n, fmt.Sprintf("%04d_", i+1)) {
			t.Errorf("migration %d = %s, want %s", i+1, n, want[i])
		}
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
