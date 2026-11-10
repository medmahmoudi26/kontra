package console

import (
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// CSP is the page's Content-Security-Policy (design §9), Kontra Cloud's console's: its own scripts, styles, images and
// fonts only, fetches to its own origin, no worker, no form target, nothing framing it, no base URL, no plugin, and
// Trusted Types with the one policy the A2UI renderer's lit-html makes. Agent content is drawn with DOM APIs, never
// parsed as HTML (web/test/static.test.mjs), so a script in it has nowhere to run.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"connect-src 'self'; worker-src 'none'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'; " +
	"object-src 'none'; require-trusted-types-for 'script'; trusted-types lit-html"

// pageHeaders adds the page's security headers to every file of it.
func pageHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// guard is the API's browser hardening: no CORS header is ever sent (so another site's script cannot read an
// answer); a request whose Origin is not the gate's own, or that a browser marks cross-site, is 403 before any handler
// runs (so another site cannot make the console act, even with a simple request a browser sends without a
// preflight); a state-changing request must be application/json, which a cross-site form cannot send and a
// cross-site fetch can send only after a preflight the API never answers.
func (c *Console) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "cross_origin",
				"message": "the console API answers only the console's own page"}})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "not_json",
					"message": "a request that changes something must be application/json"}})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether r comes from the gate's own page, or from no page at all (curl, a test): its Origin,
// when present, is http://<the Host it was sent to>; Sec-Fetch-Site, when present, is same-origin or none.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	o := r.Header.Values("Origin")
	if len(o) == 0 {
		return true
	}
	if len(o) > 1 {
		return false
	}
	u, err := url.Parse(o[0])
	if err != nil || u.Scheme != "http" || u.Path != "" || u.User != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
