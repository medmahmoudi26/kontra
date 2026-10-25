package card

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func served(t *testing.T) *Served {
	t.Helper()
	body := []byte(`{"name":"hello","capabilities":{"streaming":true}}`)
	return &Served{Body: body, Upstream: []byte(`{"name":"hello","url":"http://seller"}`), ETag: ETag(body),
		LastModified: time.Date(2026, 10, 25, 9, 30, 15, 500_000_000, time.UTC)}
}

func get(h http.Handler, method string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/agents/hello", nil)
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Card responses carry Cache-Control: max-age=60, an ETag (the hash of the body) and Last-Modified (the
// TCK hard-fails without them), as JSON; the upstream card is served verbatim with its own tag.
func TestHandlerHeaders(t *testing.T) {
	s := served(t)
	h := Handler(func(*http.Request) (*Served, error) { return s, nil }, false)
	w := get(h, http.MethodGet, nil)
	if w.Code != 200 || w.Body.String() != string(s.Body) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for k, want := range map[string]string{
		"Cache-Control": "max-age=60", "ETag": s.ETag, "Last-Modified": "Sun, 25 Oct 2026 09:30:15 GMT",
		"Content-Type": "application/json",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	up := get(Handler(func(*http.Request) (*Served, error) { return s, nil }, true), http.MethodGet, nil)
	if up.Code != 200 || up.Body.String() != string(s.Upstream) || up.Header().Get("ETag") != ETag(s.Upstream) || up.Header().Get("Cache-Control") != "max-age=60" {
		t.Errorf("upstream: %d %s %v", up.Code, up.Body, up.Header())
	}
	// HEAD: the headers, no body.
	head := get(h, http.MethodHead, nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("ETag") != s.ETag {
		t.Errorf("HEAD: %d %q", head.Code, head.Body)
	}
	// A card without a time still says Last-Modified.
	s2 := served(t)
	s2.LastModified = time.Time{}
	if lm := get(Handler(func(*http.Request) (*Served, error) { return s2, nil }, false), http.MethodGet, nil).Header().Get("Last-Modified"); lm == "" {
		t.Error("no Last-Modified")
	}
}

// Conditional requests: a matching If-None-Match (or *) and an If-Modified-Since at or after
// Last-Modified are 304; anything else is the card.
func TestHandlerConditional(t *testing.T) {
	s := served(t)
	h := Handler(func(*http.Request) (*Served, error) { return s, nil }, false)
	for _, c := range []struct {
		h    http.Header
		want int
	}{
		{http.Header{"If-None-Match": {s.ETag}}, 304},
		{http.Header{"If-None-Match": {`"other", ` + s.ETag}}, 304},
		{http.Header{"If-None-Match": {"W/" + s.ETag}}, 304},
		{http.Header{"If-None-Match": {"*"}}, 304},
		{http.Header{"If-None-Match": {`"other"`}}, 200},
		{http.Header{"If-Modified-Since": {"Sun, 25 Oct 2026 09:30:15 GMT"}}, 304},
		{http.Header{"If-Modified-Since": {"Sun, 25 Oct 2026 09:30:14 GMT"}}, 200},
		{http.Header{"If-None-Match": {`"other"`}, "If-Modified-Since": {"Sun, 25 Oct 2026 10:00:00 GMT"}}, 200},
	} {
		w := get(h, http.MethodGet, c.h)
		if w.Code != c.want {
			t.Errorf("%v: %d, want %d", c.h, w.Code, c.want)
		}
		if w.Code == 304 && (w.Body.Len() != 0 || w.Header().Get("ETag") != s.ETag || w.Header().Get("Cache-Control") != "max-age=60") {
			t.Errorf("%v: 304 with %q %v", c.h, w.Body, w.Header())
		}
	}
	// The upstream card answers If-Modified-Since in full: its bytes can change under one served body.
	up := Handler(func(*http.Request) (*Served, error) { return s, nil }, true)
	if w := get(up, http.MethodGet, http.Header{"If-Modified-Since": {"Mon, 26 Oct 2026 00:00:00 GMT"}}); w.Code != 200 {
		t.Errorf("upstream If-Modified-Since: %d", w.Code)
	}
	if w := get(up, http.MethodGet, http.Header{"If-None-Match": {ETag(s.Upstream)}}); w.Code != 304 {
		t.Errorf("upstream If-None-Match: %d", w.Code)
	}
}

// A service with no servable card answers 503 with an empty body (spec §3); other methods are the
// closed router's empty 404.
func TestHandlerUnavailable(t *testing.T) {
	s := served(t)
	stale := *s
	stale.Unavailable = true
	for name, get := range map[string]func(*http.Request) (*Served, error){
		"none yet":    func(*http.Request) (*Served, error) { return nil, ErrUnavailable },
		"stale":       func(*http.Request) (*Served, error) { return &stale, ErrUnavailable },
		"other error": func(*http.Request) (*Served, error) { return s, errors.New("db") },
	} {
		w := httptest.NewRecorder()
		Handler(get, false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != 503 || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", name, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	Handler(func(*http.Request) (*Served, error) { return s, nil }, false).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != 404 || w.Body.Len() != 0 {
		t.Errorf("POST: %d %q", w.Code, w.Body)
	}
}
