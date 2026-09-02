package catalog

import "testing"

func TestTheContractStringsAreWhatTheOtherSideRegistered(t *testing.T) {
	// Scheduled BY NAME across a process boundary, so nothing catches a rename at build time.
	for _, c := range []struct{ got, want string }{
		{ServiceName, "kontra.actor"},
		{RunOperation, "run"},
		{FetchBlobActivity, "kontra.fetch_blob"},
		{OpenSessionActivity, "OpenSession"},
		{CloseSessionActivity, "CloseSession"},
		{PageDatasetActivity, "pageDataset"},
		{SplitBatchActivity, "splitBatch"},
		{PublishBatchActivity, "publishBatch"},
		{CloseDatasetActivity, "closeDataset"},
		{DatasetStateActivity, "datasetState"},
		{DatasetQueue, "kontra-datasets"},
	} {
		if c.got != c.want {
			t.Errorf("contract string = %q, want %q", c.got, c.want)
		}
	}
}

func TestBatchReadsItsCountsOffTheRefsMeta(t *testing.T) {
	// The whole reason counts ride on meta: Err() must answer without a fetch.
	clean := batchFromRef(BareRef{Meta: map[string]string{"kind": "units", "n": "5", "done": "true"}}, "a", "1")
	if clean.N != 5 || clean.Isolated != 0 || !clean.Done {
		t.Fatalf("clean batch = %+v", clean)
	}
	// The Dropped half of a Call's answer reports no drops on a clean Batch, and costs no fetch.
	if d := (&Dropped{batch: clean}); d.Any() || d.Len() != 0 {
		t.Errorf("a clean batch dropped nothing: Len=%d Any=%v", d.Len(), d.Any())
	}

	dropped := batchFromRef(BareRef{Meta: map[string]string{
		"n": "1", "isolated": "3", "failures": "deadbeef", "done": "false",
	}}, "a", "1")
	if dropped.Isolated != 3 || dropped.Done || dropped.FailuresSha != "deadbeef" {
		t.Fatalf("dropped batch = %+v", dropped)
	}
	// Isolation is NOT an error (ADR 0023 §14); it surfaces as Dropped, a distinct type the caller
	// reads. The count rides on the ref meta, so Len is answered with no fetch.
	if d := (&Dropped{batch: dropped}); !d.Any() || d.Len() != 3 {
		t.Errorf("a batch that dropped 3 units: Len=%d Any=%v", d.Len(), d.Any())
	}

	// A ref minted before the split carries no meta: degrade to zero, never panic.
	bare := batchFromRef(BareRef{Sha256: "x"}, "a", "1")
	if bare.N != 0 || bare.Isolated != 0 || !bare.Done {
		t.Fatalf("meta-less ref = %+v", bare)
	}
}

func TestBatchNamesTheMachineThatProducedIt(t *testing.T) {
	// The provenance a published row carries: the actor host stamps it into the envelope, the
	// handler copies it onto the ref's meta, and Write forwards it. Read here with no fetch,
	// like every other meta field. Peer of Python's `Batch.machine`.
	b := batchFromRef(BareRef{Meta: map[string]string{"n": "2", "machine": "kf-dns-01"}}, "a", "1")
	if b.Machine != "kf-dns-01" {
		t.Errorf("machine = %q, want kf-dns-01", b.Machine)
	}

	// EMPTY MEANS UNRECORDED, and it has to stay that way down the whole path: a Dataset page
	// was produced by the lake and not by a Machine, and an actor host older than this contract
	// names none. The publish activity is what turns that into SQL NULL — a value nothing can
	// mistake for a hostname.
	page := batchFromRef(BareRef{Meta: map[string]string{"kind": "units", "n": "2"}}, "", "")
	if page.Machine != "" {
		t.Errorf("an unnamed Machine must stay empty, got %q", page.Machine)
	}
}
