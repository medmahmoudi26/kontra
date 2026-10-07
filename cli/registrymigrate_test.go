package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// THE MIGRATION'S FAILURE MODE IS A SUCCESSFUL PARTIAL COPY. Nothing downstream can tell an empty or
// half-populated registry from a fresh one — `GET /api/images` answers 200 with `[]`, the console
// draws `?`, and `versionDeployed()` silently stops guarding version immutability. So the things
// asserted here are the ones whose failure would be reported as success.

func TestPagingIsFollowedOrTheMigrationIsSilentlyPartial(t *testing.T) {
	// A registry that pages and a client that reads only the first page copies a prefix and reports
	// done. registry:2 pages at its own `n`, so this is not hypothetical.
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.RequestURI())
		switch r.URL.Query().Get("last") {
		case "":
			w.Header().Set("Link", `</v2/_catalog?n=200&last=b>; rel="next"`)
			fmt.Fprint(w, `{"repositories":["a","b"]}`)
		case "b":
			w.Header().Set("Link", `</v2/_catalog?n=200&last=d>; rel="next"`)
			fmt.Fprint(w, `{"repositories":["c","d"]}`)
		default:
			fmt.Fprint(w, `{"repositories":["e"]}`)
		}
	}))
	defer srv.Close()

	got, err := registryCatalog(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c", "d", "e"}; !reflect.DeepEqual(got, want) {
		t.Errorf("repositories:\n  got  %v\n  want %v", got, want)
	}
	if len(hits) != 3 {
		t.Errorf("followed %d pages, want 3: %v", len(hits), hits)
	}
}

func TestTagPagingIsFollowedToo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/demo/tags/list?n=200&last=1.0.0>; rel="next"`)
			fmt.Fprint(w, `{"tags":["1.0.0"]}`)
			return
		}
		fmt.Fprint(w, `{"tags":["2.0.0"]}`)
	}))
	defer srv.Close()

	got, err := registryTags(context.Background(), srv.URL, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.0.0", "2.0.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tags: got %v want %v", got, want)
	}
}

func TestNextLinkPath(t *testing.T) {
	cases := []struct {
		why, header, want string
	}{
		{"a next link", `</v2/_catalog?n=2&last=b>; rel="next"`, "/v2/_catalog?n=2&last=b"},
		{"no header at all", "", ""},
		// `rel="prev"` is not a next page, and following it would loop.
		{"a link that is not next", `</v2/_catalog?n=2>; rel="prev"`, ""},
		{"next among several", `</a>; rel="prev", </v2/x?n=1>; rel="next"`, "/v2/x?n=1"},
		{"malformed, no brackets", `rel="next"`, ""},
	}
	for _, c := range cases {
		if got := nextLinkPath(c.header); got != c.want {
			t.Errorf("%s: got %q want %q", c.why, got, c.want)
		}
	}
}

// A page the client cannot decode must be an ERROR, not an empty list — an empty list is a complete
// migration of nothing.
func TestAnUndecodablePageIsAnErrorNotAnEmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer srv.Close()
	if _, err := registryCatalog(context.Background(), srv.URL); err == nil {
		t.Fatal("a registry answering garbage reported as an empty catalog")
	}
}

func TestANon200IsNotAnEmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := registryCatalog(context.Background(), srv.URL); err == nil {
		t.Fatal("a 500 reported as an empty catalog")
	}
}

func TestMigrateRefusesTheSameRegistryTwice(t *testing.T) {
	// It would copy every tag onto itself, report success, and leave the operator believing the old
	// store had been drained — the one outcome worse than an error.
	err := cmdRegistryMigrate([]string{"--from", "127.0.0.1:5000", "--to", "http://127.0.0.1:5000"})
	if err == nil {
		t.Fatal("migrating a registry onto itself was accepted")
	}
	if !strings.Contains(err.Error(), "same registry") {
		t.Errorf("the refusal should name the problem: %v", err)
	}
}

func TestMigrateNeedsASource(t *testing.T) {
	if err := cmdRegistryMigrate(nil); err == nil {
		t.Fatal("--from is required")
	}
}

func TestShortIsTwelveHexAndSurvivesAnOddInput(t *testing.T) {
	// Twelve is the width the console and the `inuse-` tag scheme both use, so this is a shared
	// spelling and not a display choice.
	if got := short("sha256:0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("got %q", got)
	}
	if got := short("sha256:abc"); got != "abc" {
		t.Errorf("a short digest must not panic or pad: got %q", got)
	}
	if got := short(""); got != "" {
		t.Errorf("got %q", got)
	}
}

// The usage text is what an operator reads when they get the invocation wrong, so it has to name the
// two addresses and the escape hatch rather than just the verb.
func TestUsageNamesWhatItNeeds(t *testing.T) {
	err := cmdRegistry([]string{"frobnicate"})
	if err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	for _, want := range []string{"--from", "--to", "--dry-run", "migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("usage does not mention %q:\n%v", want, err)
		}
	}
}
