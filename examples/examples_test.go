// Package examples_test holds the checks that apply to every example (examples/README.md).
package examples_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// module is this repository's Go module path.
const module = "kontra.sh/kontra"

// TestNoExampleImportsKontra: every example runs unmodified behind the gate, so none of them imports Kontra
// code (examples/README.md; PRD §18). The examples that are Go programs live in this module only so that
// `go build ./...` covers them, which also lets them reach its internal packages: this test is what stops
// that.
func TestNoExampleImportsKontra(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files = append(files, filepath.ToSlash(path))
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if p == module || strings.HasPrefix(p, module+"/") {
				t.Errorf("%s imports %s: an example imports nothing from Kontra", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that saw nothing proves nothing: the booking seller must be among the files read.
	if !slices.Contains(files, "sellers/booking/main.go") {
		t.Fatalf("walked %q under examples/, want sellers/booking/main.go among them", files)
	}
}
