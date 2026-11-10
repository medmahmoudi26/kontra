// Package web holds the console's files (README.md, "Provenance") and serves them from the binary: the page at
// "/" and its assets by path, nothing from another origin. Which routes reach it, and the security headers of the
// page, are internal/console's.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var files embed.FS

// FS is the console's files, rooted at static/: index.html, console.css, favicon.svg, js/ and vendor/.
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embedded tree always has static/
	}
	return sub
}

// Handler serves the console: "/" is index.html; a path naming an embedded file serves it; anything else,
// including a directory, is 404 (no listing). Only GET and HEAD.
func Handler() http.Handler {
	root := FS()
	server := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			http.ServeFileFS(w, r, root, "index.html")
			return
		}
		if st, err := fs.Stat(root, name); err != nil || st.IsDir() || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		server.ServeHTTP(w, r)
	})
}
