package detect

import (
	"strings"
	"testing"
)

// The elision policy is the only code in this actor that DESTROYS captured evidence, so what it
// keeps has to be pinned rather than trusted. The rule it exists to enforce, in one sentence:
// a row that found nothing keeps every FACT an oracle read and none of the BYTES those facts were
// derived from; a row that found something keeps both.

// obs builds a framing observation whose three steps carry distinguishable, oversized bodies.
func obs(sigCount int) FramingObs {
	body := func(status, tag string, n int) []byte {
		return []byte("HTTP/1.1 " + status + "\r\nServer: " + tag + "\r\n\r\n" + strings.Repeat("x", n))
	}
	return FramingObs{
		Schema: SchemaFraming, Axis: AxisSmuggle, Host: "api.example.com", Port: 443,
		Variant: "obs-fold-20-preserve", HeaderLine: "Content-Length \r\n : 33",
		SentRaw:     []byte("POST /?cb=1 HTTP/1.1\r\nContent-Length \r\n : 33\r\n\r\nGET /1-kontra HTTP/1.1\r\nX: X\r\n\r\n"),
		NormalRaw:   []byte("GET /?cb=1 HTTP/1.1\r\nHost: api.example.com\r\n\r\n"),
		Pre:         StepSummary{Label: "pre", Status: 200, BodySHA: "sha-pre", BodyLen: 200_000, RespRaw: body("200 OK", "pre", 200_000)},
		Attack:      StepSummary{Label: "attack", Status: 400, BodySHA: "sha-atk", BodyLen: 20_000, RespRaw: body("400 Bad Request", "attack", 20_000)},
		Post:        StepSummary{Label: "post", Status: 200, BodySHA: "sha-post", BodyLen: 200_000, RespRaw: body("200 OK", "post", 200_000)},
		Signals:     FramingSignals{Count: sigCount, CanaryReflected: sigCount > 0},
		SignalCount: sigCount,
	}
}

/*
THE MEASUREMENT THAT MOTIVATED THIS. The first campaign wrote 833 rows and 18.8 MB of payload, of
which `pre.resp_raw` alone was 13.8 MB — the benign request's answer, a full HTML page, captured
once per technique. At sweep grain (3,630 CL.0 techniques against one host) that is gigabytes per
program, which is why the sweep phase had never actually been run to completion.
*/
func TestACleanRowKeepsTheFactsAndDropsTheBytes(t *testing.T) {
	r := RowFromSmuggle(obs(0))
	before := len(r.Smuggle.Pre.RespRaw) + len(r.Smuggle.Post.RespRaw)
	r.Elide(false)

	// THE FACTS. Every one of these is read by a query that decides whether something happened,
	// so every one of them survives — this is the whole claim the policy rests on.
	for _, st := range []StepSummary{r.Smuggle.Pre, r.Smuggle.Attack, r.Smuggle.Post} {
		if st.Status == 0 || st.BodySHA == "" || st.BodyLen == 0 {
			t.Fatalf("%s lost a fact an oracle reads: %+v", st.Label, st)
		}
	}

	// THE BYTES. The baseline's answer and its repeat are the largest thing in the lake and
	// neither is evidence of anything `BodySHA` has not already settled.
	if len(r.Smuggle.Pre.RespRaw) != 0 || len(r.Smuggle.Post.RespRaw) != 0 {
		t.Fatalf("clean row kept baseline bytes: pre=%d post=%d",
			len(r.Smuggle.Pre.RespRaw), len(r.Smuggle.Post.RespRaw))
	}
	if got := r.Smuggle.Pre.RespRawElided + r.Smuggle.Post.RespRawElided; got != before {
		t.Fatalf("elided count lies: dropped %d bytes, reported %d", before, got)
	}

	// The attack step keeps a head, because "what does this edge say to a malformed request" is
	// the one question asked in bulk across rows that found nothing.
	if len(r.Smuggle.Attack.RespRaw) != CleanCap {
		t.Fatalf("attack head = %d, want %d", len(r.Smuggle.Attack.RespRaw), CleanCap)
	}
	if !strings.HasPrefix(string(r.Smuggle.Attack.RespRaw), "HTTP/1.1 400") {
		t.Fatal("the head is not the head — a trimmed response must still start at the status line")
	}

	// REPLAYABLE, STILL. The rendered form goes because it is a pure function of the base64 one;
	// dropping the bytes themselves would make the row unable to say what it sent.
	if r.TriggeredRequestB64 == "" || r.OriginalRequestB64 == "" {
		t.Fatal("a clean row must still be replayable")
	}
	if r.TriggeredRequest != "" || r.OriginalRequest != "" {
		t.Fatal("rendered requests should be dropped — they are derivable from the _b64 columns")
	}
	if r.Elided == "" {
		t.Fatal("a row that dropped something must say so: an empty resp_raw and an unkept one " +
			"are different facts and a reader cannot tell them apart without this")
	}
}

func TestARowThatFiredKeepsItsEvidence(t *testing.T) {
	r := RowFromSmuggle(obs(2))
	r.Elide(false)

	// Capped, because a 200 KB marketing page is not more probative than its first 64 KB — but
	// present, because on THIS row the bytes are the finding.
	for _, st := range []StepSummary{r.Smuggle.Pre, r.Smuggle.Attack, r.Smuggle.Post} {
		if len(st.RespRaw) == 0 {
			t.Fatalf("%s: a signalling row lost its stream entirely", st.Label)
		}
		if len(st.RespRaw) > EvidenceCap {
			t.Fatalf("%s: %d bytes exceeds the %d cap", st.Label, len(st.RespRaw), EvidenceCap)
		}
	}
	if r.TriggeredRequest == "" || r.OriginalRequest == "" {
		t.Fatal("a lead must carry both requests in readable form — the finding IS the difference")
	}
}

// `erratic` and `voided` are NOT "clean". They are the two ways a probe says it could not make a
// claim, and the bytes are how a human works out why — which is the moment they are most needed.
func TestRefusalsKeepTheirBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Row)
	}{
		{"erratic", func(r *Row) { r.Erratic = true }},
		{"voided", func(r *Row) { r.Voided = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := RowFromSmuggle(obs(0))
			tc.mut(&r)
			r.Elide(false)
			if len(r.Smuggle.Pre.RespRaw) == 0 {
				t.Fatal("a refusal was elided as if it were a clean scan — 'we could not test " +
					"this' and 'this is clean' are different facts")
			}
		})
	}
}

/*
THE THREE-WAY DUPLICATE. `Row.SentRaw` was `[]byte`, so encoding/json wrote it as base64 and it
came out byte-identical to `triggered_request_b64`; `smuggle.sent_raw` was a third copy. Measured:
820 of 833 rows carried all three, identical. The field is gone from `Row` (so this test can only
check the nested pair), and the nested copies are cleared on every row including ones that fired.
*/
func TestTheNestedRequestCopiesGoOnEveryRow(t *testing.T) {
	for _, sig := range []int{0, 3} {
		r := RowFromSmuggle(obs(sig))
		r.Elide(false)
		if r.Smuggle.SentRaw != nil || r.Smuggle.NormalRaw != nil {
			t.Fatalf("signals=%d: the nested request copies survived; they are the same bytes as "+
				"the _b64 columns and cost 300 KB per campaign to say so twice", sig)
		}
	}
}

// The operator override, for a deep dive where the whole stream is the point.
func TestKeepRawSkipsTheCleanRowRules(t *testing.T) {
	r := RowFromSmuggle(obs(0))
	r.Elide(true)
	if len(r.Smuggle.Pre.RespRaw) == 0 {
		t.Fatal("keep_raw did not keep the baseline stream")
	}
	if r.TriggeredRequest == "" {
		t.Fatal("keep_raw did not keep the rendered request")
	}
	// It is an override of the POLICY, not of the caps: nothing wants a 200 KB cell.
	if len(r.Smuggle.Pre.RespRaw) > EvidenceCap {
		t.Fatalf("keep_raw bypassed the evidence cap: %d bytes", len(r.Smuggle.Pre.RespRaw))
	}
}

// A row whose peer genuinely sent nothing must not claim an elision — that is the reading this
// whole mechanism exists to keep available.
func TestNothingSentIsNotAnElision(t *testing.T) {
	o := obs(0)
	o.Pre = StepSummary{Label: "pre", Status: 200, BodySHA: "sha", BodyLen: 1}
	o.Attack = StepSummary{Label: "attack"}
	o.Post = StepSummary{Label: "post", Status: 200, BodySHA: "sha", BodyLen: 1}
	r := RowFromSmuggle(o)
	r.Elide(false)
	if r.Smuggle.Pre.RespRawElided != 0 || r.Smuggle.Attack.RespRawElided != 0 {
		t.Fatal("reported dropping bytes that never existed")
	}
}

// A SPLIT-AXIS ROW HAS NO `Smuggle` BLOCK AT ALL, and `Elide` runs on every row whichever axis
// produced it. Every dereference in it is guarded; this is what keeps that true, because the
// campaign's later passes turn the splitting axis back on and a nil dereference there would take
// the worker down mid-sweep rather than failing one probe.
func TestElideIsSafeOnASplitAxisRow(t *testing.T) {
	for _, sig := range []int{0, 2} {
		r := RowFromSplit(Observation{
			URL:     "https://api.example.com/search?q=1",
			Signals: Signals{Count: sig, ControlUsed: sig > 0, StatusChanged: sig > 0},
		})
		if r.Smuggle != nil {
			t.Fatal("a split row must not carry a framing block")
		}
		r.Elide(false) // must not panic
		if r.Endpoint != "/search?q=1" {
			t.Fatalf("endpoint lost through Elide: %q", r.Endpoint)
		}
	}
}
