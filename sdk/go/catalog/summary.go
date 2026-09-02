package catalog

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// SUMMARY_SEP in Python. The separator the reduced log already puts between an event's own
// `key=value` pairs, so a Summary reads like the line it lands beside.
const summarySep = " · "

// SummaryBudget is how many UTF-8 BYTES one Summary may take, in total — the peer of Python's
// `catalog.SUMMARY_BUDGET`, and ONE budget for every Summary this SDK writes: a dispatch's Method
// line, a Session's open, an author's sentence (`lib/narrate`).
//
// NOTHING VALIDATES IT — not this SDK, which takes any string, and not the server's reply. An
// overrun is therefore never an error; it is a line cut somewhere downstream, by a server limit or
// by a UI that elides, with nothing said about it anywhere. Which is why {@link DispatchSummary} is
// BUILT to fit rather than composed and trimmed: trimming afterwards loses whichever field happened
// to be last, silently, on exactly the dispatches worth reading.
//
// 200 bytes renders on a Temporal bar label without eliding and is some 650× under the codec's
// 128 KiB offload threshold, which is what keeps a Summary inline and readable without a fetch —
// `summaryOf` in backend/src/history.ts REFUSES a claim-checked Summary rather than
// dereferencing it.
const SummaryBudget = 200

// The most of the budget one Method name may take, and one `actor@version`. Generous for an
// identifier and a ceiling, so that whatever a Method is called the Actor still fits.
const (
	methodCap = 64
	actorCap  = 80
	// Below this there is no room to say anything TRUE about a key, so it is left out altogether
	// rather than rendered as a pair of brackets with an ellipsis inside them.
	keyMin   = 8
	ellipsis = "…"
)

// DispatchSummary is the one line a dispatch puts on its own scheduling event.
//
//	crawl · crawler@0.1.0[acme.com] · 12 units
//
// THE PEER OF PYTHON'S `catalog.dispatch_summary`, byte for byte, and pinned against its goldens in
// summary_test.go. Not shared code and not translatable to "roughly the same": the reader on the
// other side (`backend/src/transcript.ts`, `methodOf`) recovers the METHOD by taking the first
// field and testing its shape, so a Go dispatch that wrote its own format would put a Method name
// nothing reads into a transcript that then says the run named none.
//
// THE METHOD IS PAID FIRST, because it is the one fact here that nothing else in the log carries.
// The Actor and its version are recoverable from the Nexus endpoint and exactly from the shared
// task queue; the unit count is at least plausible from elsewhere; the Method is in the payload and
// nowhere else. So it goes at the front of the line, which is also where a bar label in Temporal's
// own UI is read from.
//
// THE COUNT IS RESERVED BEFORE THE KEY, and that ordering is the point of the budget. The key is
// the one field whose length nobody in this repo controls — `crawler["https://…"]` is a legal
// binding — and an operator's URL must not be what pushes the unit count off the end.
//
// A dispatch that names no Method — a single-Method Actor accepts an unnamed one — simply starts
// with the Actor instead. `units < 0` leaves the count out, which is the peer of Python's
// `units=None`.
func DispatchSummary(actor, version, method, key string, units int) string {
	return dispatchSummary(actor, version, method, key, units, SummaryBudget)
}

func dispatchSummary(actor, version, method, key string, units, budget int) string {
	var fields []string
	left := budget
	sep := len(summarySep)

	// Reserved, not appended: it goes on the end, but its room is taken out of the budget here so
	// that everything discretionary below is spent against what is genuinely left over.
	tail := ""
	if units >= 0 {
		tail = strconv.Itoa(units) + " units"
	}
	if tail != "" && len(tail)+sep <= left {
		left -= len(tail) + sep
	} else {
		tail = ""
	}

	if method != "" {
		if fitted := fit(method, min(methodCap, left)); fitted != "" {
			fields = append(fields, fitted)
			left -= len(fitted)
		}
	}

	// THE `@` IS ALWAYS THERE, EVEN WITH NO VERSION, and it is not decoration: it is the whole
	// reason the reader can tell a Method name from an Actor name by looking at one field. A
	// version-less Actor (the DIY path, `echo-shared`) would otherwise render as a bare `echo` —
	// and on a dispatch that named no Method that is the FIRST field, which the reader would then
	// read as a Method called `echo`. `echo@` says the true thing instead: an Actor, no version.
	joinCost := 0
	if len(fields) > 0 {
		joinCost = sep
	}
	who := fit(actor+"@"+version, min(actorCap, left-joinCost))
	if who != "" {
		left -= len(who) + joinCost
		// The brackets cost two of the bytes the key is being given, so they come out of its room
		// rather than out of the budget after the fact.
		if key != "" && left-2 >= keyMin {
			if fitted := fit(key, left-2); fitted != "" {
				who += "[" + fitted + "]"
				left -= len(fitted) + 2
			}
		}
		fields = append(fields, who)
	}

	if tail != "" {
		fields = append(fields, tail)
	}
	return strings.Join(fields, summarySep)
}

// fit is `text`, or as much of it as fits in `room` UTF-8 BYTES with an ellipsis marking the cut.
//
// CUT ON A CHARACTER BOUNDARY — the peer of Python's `_fit`, which slices the encoded form and
// decodes the prefix with `ignore`. Same rule spelled the way Go spells it: keep whole runes.
//
// An empty answer means "there was no room to say this", and every caller leaves the field out
// entirely when it gets one. A lone `…` is never returned: a field that says only that it was cut
// is a label pretending to be a name.
func fit(text string, room int) string {
	if room <= 0 {
		return ""
	}
	if len(text) <= room {
		return text
	}
	mark := len(ellipsis)
	if room <= mark {
		return ""
	}
	kept := 0
	for i, r := range text {
		if i+utf8.RuneLen(r) > room-mark {
			break
		}
		kept = i + utf8.RuneLen(r)
	}
	if kept == 0 {
		return ""
	}
	return text[:kept] + ellipsis
}
