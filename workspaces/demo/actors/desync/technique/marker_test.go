package technique

import (
	"strings"
	"testing"
)

// THE MARKER HAS TO SURVIVE RENDERING, and this is the only place that can prove it.
//
// Both programs require a per-request identifying header — PayPal `X-PP-BB`, a program its username
// appended to `User-Agent` — and the way this scan carries them is the `${header_block}` slot,
// filled from the scope row. A slot that silently dropped its binding would send the whole sweep
// unattributed while every dataset column still said the marker was configured.
func TestTheHeaderBlockSlotReachesTheWire(t *testing.T) {
	const marker = "X-HackerOne: medmahmoudi\r\n"
	if len(Families) == 0 {
		t.Fatal("no families registered")
	}
	rows := Families[0].Expand()
	if len(rows) == 0 {
		t.Fatal("family expanded to nothing")
	}

	raw, _, err := rows[0].RenderAutoCL(Bindings{
		Host:        "api.example.com",
		Endpoint:    "/",
		HeaderBlock: marker,
		Random:      "847213",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "X-HackerOne: medmahmoudi") {
		t.Fatalf("the marker never reached the rendered request:\n%q", got)
	}
	// It must land in the HEADER SECTION of the outer request, before the body — a marker after
	// the blank line is part of the smuggled prefix's payload, not a header anyone parses.
	head, _, ok := strings.Cut(got, "\r\n\r\n")
	if !ok {
		t.Fatalf("rendered request has no header/body boundary:\n%q", got)
	}
	if !strings.Contains(head, "X-HackerOne: medmahmoudi") {
		t.Fatalf("the marker landed outside the header section:\n%q", got)
	}
	// And it must not have broken the request line.
	if !strings.HasPrefix(got, "POST ") || strings.Count(head, "\r\n\r\n") != 0 {
		t.Fatalf("header block malformed the request:\n%q", got)
	}
}
