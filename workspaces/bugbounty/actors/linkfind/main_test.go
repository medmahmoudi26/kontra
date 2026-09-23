package main

import (
	"fmt"
	"testing"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
)

// loaded is a Session with the patterns Load compiled — the same resource both Methods share.
func loaded(t *testing.T) *kontra.Session {
	t.Helper()
	s := kontra.NewSession(nil)
	if err := load(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A Method must be testable without a worker: the whole point of the author owning the loop is
// that the loop is ordinary code.
func TestUrlsHarvestsEveryAbsoluteLink(t *testing.T) {
	b := kontra.TestBatch(map[string]any{
		"url":  "https://acme.test/",
		"body": `see https://a.acme.test/x and http://b.acme.test:8080/y?q=1 plus /relative junk`,
	})
	ds := kontra.TestDataset()
	if err := urls(loaded(t), b, ds); err != nil {
		t.Fatal(err)
	}
	want := "[map[from:https://acme.test/ url:https://a.acme.test/x] " +
		"map[from:https://acme.test/ url:http://b.acme.test:8080/y?q=1]]"
	if got := fmt.Sprint(ds.Records()); got != want {
		t.Errorf("Records() = %s\nwant %s", got, want)
	}
}

// One Unit in, N out — the shape a per-Unit callback returning one value could not express, and
// each record is attributed to the page it came from.
func TestOnePageEmitsAsManyLinksAsItContains(t *testing.T) {
	b := kontra.TestBatch(map[string]any{
		"url":  "https://acme.test/",
		"body": "https://one.test/ https://two.test/ https://three.test/",
	})
	ds := kontra.TestDataset()
	if err := urls(loaded(t), b, ds); err != nil {
		t.Fatal(err)
	}
	if got := ds.Records(); len(got) != 3 {
		t.Fatalf("pushed %d records from one Unit, want 3: %v", len(got), got)
	}
}

// secrets reports the CAPTURE, not the whole match: the value is what matters, and emitting
// `api_key = "…"` verbatim would make every downstream comparison depend on the surrounding text.
func TestSecretsEmitsTheCapturedValueNotTheWholeMatch(t *testing.T) {
	b := kontra.TestBatch(map[string]any{
		"url":  "https://acme.test/app.js",
		"body": `const api_key = "sk_live_0123456789abcdef", short = "token: abc"`,
	})
	ds := kontra.TestDataset()
	if err := secrets(loaded(t), b, ds); err != nil {
		t.Fatal(err)
	}
	want := "[map[from:https://acme.test/app.js secret:sk_live_0123456789abcdef]]"
	if got := fmt.Sprint(ds.Records()); got != want {
		t.Errorf("Records() = %s\nwant %s — the short one is below the length floor", got, want)
	}
}

// Both Methods run against ONE loaded Session, which is what many-Methods-per-Actor bought: the
// patterns are compiled once and `secrets` sees exactly what `urls` saw.
func TestBothMethodsShareTheOneLoadedResource(t *testing.T) {
	s := loaded(t)
	page := map[string]any{
		"url":  "https://acme.test/",
		"body": `link https://a.test/ and token = "0123456789abcdefghij"`,
	}

	links := kontra.TestBatch(page)
	linksOut := kontra.TestDataset()
	if err := urls(s, links, linksOut); err != nil {
		t.Fatal(err)
	}
	creds := kontra.TestBatch(page)
	credsOut := kontra.TestDataset()
	if err := secrets(s, creds, credsOut); err != nil {
		t.Fatal(err)
	}

	if len(linksOut.Records()) != 1 || len(credsOut.Records()) != 1 {
		t.Fatalf("urls pushed %v, secrets pushed %v — both should find one",
			linksOut.Records(), credsOut.Records())
	}
}

// A page with nothing to find emits nothing and fails nothing: an empty result is an answer.
func TestAPageWithNothingToFindEmitsNothing(t *testing.T) {
	b := kontra.TestBatch(map[string]any{"url": "https://acme.test/", "body": "plain text"})
	ds := kontra.TestDataset()
	if err := urls(loaded(t), b, ds); err != nil {
		t.Fatal(err)
	}
	if got := ds.Records(); len(got) != 0 {
		t.Errorf("Records() = %v, want nothing", got)
	}
}
