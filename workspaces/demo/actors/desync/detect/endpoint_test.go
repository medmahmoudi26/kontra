package detect

import "testing"

/*
THE FAILURE THIS PINS, because it survived an entire campaign without anything raising it.

`scope_<program>.endpoint` is blank on every row — bbscope emits authorisation, not paths — so
`orDefault(h.Endpoint, "/")` chose root for every host. The hunt workflow's sweep query then
spelled `'/' AS endpoint` literally, and `observations` had no endpoint column at all. The result
was a campaign that probed 40 hosts, reported 833 observations and 225 leads, and sent every
single byte to `/`, with nothing in the output able to say so.

Three things had to be true for that to be invisible, and one test each:

  1. the row must CARRY the endpoint          (below)
  2. the split axis must mean the same thing by it  (below)
  3. the dedup key must include it                  (frame.go — a comment, because the key is
     built at the push site and there is no row to assert on)
*/

func TestTheRowSaysWhichPathItProbed(t *testing.T) {
	r := RowFromSmuggle(FramingObs{
		Host: "voapi.8x8.com", Endpoint: "/api/v1/session", Variant: "obs-fold-20",
	})
	if r.Endpoint != "/api/v1/session" {
		t.Fatalf("endpoint = %q, want /api/v1/session — a row that cannot say where it looked "+
			"cannot be asked what it missed", r.Endpoint)
	}
}

// ONE COLUMN, ONE MEANING, ACROSS BOTH AXES. The smuggling axis renders its own request-target;
// the splitting axis inherits one from a crawled URL. If those two land in `endpoint` meaning
// different things then "which paths has this program been tested on" is not answerable, which is
// the entire reason the column was added.
func TestBothAxesSpellTheEndpointTheSameWay(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://voapi.8x8.com/api/v1/session", "/api/v1/session"},
		// THE QUERY IS PART OF THE ENDPOINT. `/search?q=` and `/search` routinely reach different
		// code, and on this axis the query string is frequently where the payload went.
		{"https://voapi.8x8.com/search?q=1&lang=en", "/search?q=1&lang=en"},
		// A bare origin means root, spelled the way the smuggling axis spells it — not "".
		{"https://voapi.8x8.com", "/"},
	} {
		got := RowFromSplit(Observation{URL: tc.url}).Endpoint
		if got != tc.want {
			t.Fatalf("%s -> %q, want %q", tc.url, got, tc.want)
		}
	}
}
