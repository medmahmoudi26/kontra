package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
)

// THE BUG THIS PINS. `split` used `errors.Join`, which concatenates every Unit's message. Units in
// one Batch fail in CORRELATED ways — an unavailable scanner, a class that selects nothing — so a
// few hundred exchanges produced a few hundred identical paragraphs and Temporal refused to record
// the result at all: "Failure exceeds size limit." The size limit then HID THE CAUSE, the retries
// reproduced it exactly, the Batch voided, and the splitting axis returned nothing while CL.0
// returned 243 observations on the same run.
func TestSummarizeBatchErrsIsBounded(t *testing.T) {
	errs := make([]error, 500)
	for i := range errs {
		errs[i] = errors.New("scanner not available")
	}

	err := summarizeBatchErrs(errs)
	if err == nil {
		t.Fatal("500 failing units summarised to nil")
	}
	msg := err.Error()

	// errors.Join would be ~500 x the message. The point of the fix is that this does not scale
	// with the Batch.
	if len(msg) > 1024 {
		t.Fatalf("summary is %d bytes; it must not grow with the Batch", len(msg))
	}
	// It must still say how bad it was, and still name the cause.
	if !strings.Contains(msg, "500 of 500") {
		t.Errorf("summary does not report the failure count: %q", msg)
	}
	if !strings.Contains(msg, "scanner not available") {
		t.Errorf("summary dropped the cause: %q", msg)
	}
	if !strings.Contains(msg, "[x500]") {
		t.Errorf("summary does not collapse the repeat: %q", msg)
	}
}

// Distinct causes are capped too, but the cap is REPORTED — a reader must be able to tell that
// they are looking at a sample rather than the whole set.
func TestSummarizeBatchErrsCapsDistinctCauses(t *testing.T) {
	errs := make([]error, 40)
	for i := range errs {
		errs[i] = fmt.Errorf("distinct failure %d", i)
	}

	msg := summarizeBatchErrs(errs).Error()
	if !strings.Contains(msg, "40 distinct causes") {
		t.Errorf("did not report how many causes were elided: %q", msg)
	}
	if len(msg) > 1024 {
		t.Fatalf("summary is %d bytes", len(msg))
	}
}

// A Batch where nothing failed is not an error, however many Units it had.
func TestSummarizeBatchErrsNilWhenClean(t *testing.T) {
	if err := summarizeBatchErrs(make([]error, 128)); err != nil {
		t.Fatalf("clean batch produced an error: %v", err)
	}
}

// One failure among many successes still names itself, without a useless "[x1]".
func TestSummarizeBatchErrsSingle(t *testing.T) {
	errs := make([]error, 10)
	errs[4] = errors.New("bad input unit: truncated")

	msg := summarizeBatchErrs(errs).Error()
	if !strings.Contains(msg, "1 of 10") || !strings.Contains(msg, "truncated") {
		t.Errorf("unexpected summary: %q", msg)
	}
	if strings.Contains(msg, "[x1]") {
		t.Errorf("a single failure should not be annotated with a count: %q", msg)
	}
}

// A Batch whose Units all failed TERMINALLY must not be retried. `splitOne` marks malformed input
// non-retryable ("a retry re-parses the same bytes and fails identically"), and folding the
// per-Unit errors into one used to discard that, so a Batch of permanently-bad Units re-sent
// itself against live hosts for no possible gain.
func TestSummarizeBatchErrsPreservesTerminal(t *testing.T) {
	type nonRetryable interface{ KontraNonRetryable() }

	errs := make([]error, 100)
	for i := range errs {
		errs[i] = kontra.NonRetryable("bad input unit: cannot unmarshal string into []unit.Header")
	}
	var nr nonRetryable
	if !errors.As(summarizeBatchErrs(errs), &nr) {
		t.Fatal("all-terminal batch lost its non-retryable marker; it will burn the retry budget")
	}

	// ...but one recoverable failure among them means a retry can still save that Unit.
	errs[7] = errors.New("connection reset by peer")
	if errors.As(summarizeBatchErrs(errs), &nr) {
		t.Fatal("a batch with a transient failure was marked terminal; that Unit loses its retry")
	}
}
