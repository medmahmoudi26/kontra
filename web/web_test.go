package web

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, method, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Result()
}

// "/" is the page, and every embedded file is served from the binary at its own path with its type.
func TestServesPageAndEveryAsset(t *testing.T) {
	res := get(t, http.MethodGet, "/")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(string(body), `<script type="module" src="js/main.js">`) {
		t.Fatalf("GET / = %d %q:\n%.200s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	types := map[string]string{".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".html": "text/html"}
	n := 0
	err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		if p == "index.html" { // the page's own address is "/" (FileServer's redirect)
			if res := get(t, http.MethodGet, "/index.html"); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "./" {
				t.Errorf("GET /index.html = %d to %q, want 301 to ./", res.StatusCode, res.Header.Get("Location"))
			}
			return nil
		}
		res := get(t, http.MethodGet, "/"+p)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET /%s = %d", p, res.StatusCode)
		}
		for ext, ct := range types {
			if strings.HasSuffix(p, ext) && !strings.HasPrefix(res.Header.Get("Content-Type"), ct) {
				t.Errorf("GET /%s Content-Type = %q, want %s", p, res.Header.Get("Content-Type"), ct)
			}
		}
		return nil
	})
	if err != nil || n < 20 {
		t.Fatalf("walked %d files: %v", n, err)
	}
}

// No directory listing, no file outside the tree, no write method.
func TestRefusesTheRest(t *testing.T) {
	for _, p := range []string{"/js/", "/js", "/vendor/a2ui/", "/nope.js", "/../web.go", "/static/index.html"} {
		if res := get(t, http.MethodGet, p); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, res.StatusCode)
		}
	}
	if res := get(t, http.MethodPost, "/"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", res.StatusCode)
	}
	if res := get(t, http.MethodHead, "/console.css"); res.StatusCode != http.StatusOK {
		t.Errorf("HEAD /console.css = %d", res.StatusCode)
	}
}

// The page loads its scripts, styles and icon from its own origin only (no CDN): every src and href in
// index.html is a relative path, and names a file the binary holds.
func TestPageLoadsNothingFromAnotherOrigin(t *testing.T) {
	b, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?i)<(?:script|link)\b[^>]*\b(?:src|href)="([^"]+)"`).FindAllStringSubmatch(string(b), -1)
	if len(refs) == 0 {
		t.Fatal("index.html loads nothing")
	}
	for _, m := range refs {
		ref := m[1]
		if strings.Contains(ref, "//") || strings.HasPrefix(ref, "/") {
			t.Errorf("index.html loads %q, not a relative path of its own", ref)
			continue
		}
		if _, err := fs.Stat(FS(), ref); err != nil {
			t.Errorf("index.html loads %q, which is not embedded", ref)
		}
	}
}
