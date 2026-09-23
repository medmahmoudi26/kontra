package detect_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/inject"
	"github.com/medmahmoudi26/kontra-actors/go/desync/lab"
	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

// scan runs the validator's two-phase differential over a lab instance and returns, per
// vector encoding, whether it produced a signal that survived the matched control.
func scan(t *testing.T, mode string) (map[string]bool, map[string]string, bool) {
	t.Helper()
	l, err := lab.Start(mode)
	if err != nil {
		t.Fatalf("lab: %v", err)
	}
	defer l.Close()

	o := detect.DefaultOptions()
	o.Repeats, o.Rate, o.Timeout = 3, 0, 4*time.Second
	sc := detect.New(o)
	u := unit.Exchange{URL: l.URL()}
	if err := u.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	pt := inject.Point{ID: "path:suffix", Kind: inject.KindPathSuffix, Render: inject.RenderPercent}
	ctx := context.Background()

	base := sc.SampleRaw(ctx, u, inject.Apply(u, inject.Point{}, ""), "baseline")
	if !base.Stable {
		return nil, nil, true // erratic: the gate refused to scan
	}

	signals := map[string]bool{}
	reasons := map[string]string{}
	for _, v := range vectors.Generate(vectors.TierQuick, nil) {
		mut := sc.SampleRaw(ctx, u, inject.Apply(u, pt, inject.Render(v, pt.Render)), v.ID)
		sig := detect.Analyze(base, mut, v)
		if sig.Count > 0 {
			ctrl := sc.SampleRaw(ctx, u, inject.Apply(u, pt, inject.RenderControl(v, pt.Render)), v.ID+"/ctl")
			sig = detect.Analyze(ctrl, mut, v)
		}
		signals[v.Encoded] = sig.Count > 0
		reasons[v.Encoded] = mut.BodyPreview
	}
	return signals, reasons, false
}

// TestVulnerableLabFindsEveryFold is the end-to-end proof: the rule generates vectors the
// scanner then confirms against a target whose ground truth is known. The five two-byte
// folds that were never reported anywhere must be found alongside the ones that were.
func TestVulnerableLabFindsEveryFold(t *testing.T) {
	sig, body, erratic := scan(t, "vulnerable")
	if erratic {
		t.Fatal("vulnerable lab was judged erratic")
	}
	// Not blacklisted by the lab, so the fold must land and cost the request its Host.
	for _, enc := range []string{"%DC%8A", "%C8%8A", "%CC%8A", "%D0%8A", "%D4%8A", "%D8%8A", "%F0%9F%98%8A"} {
		if !sig[enc] {
			t.Errorf("%s: no signal — the scanner missed a live fold", enc)
		}
		if !strings.Contains(body[enc], "No Host") {
			t.Errorf("%s: body = %q, want the No Host oracle", enc, body[enc])
		}
	}
	// Blacklisted by the lab exactly as the real target was patched: still a signal
	// (403 is a deviation) but never the No Host oracle.
	for _, enc := range []string{"%0A", "%C4%8A", "%E5%98%8A"} {
		if strings.Contains(body[enc], "No Host") {
			t.Errorf("%s should have been blocked, got %q", enc, body[enc])
		}
	}
}

// TestPatchedLabIsSilent is the false-positive gate. A correctly-fixed server does not
// narrow at all, so every vector is an ordinary path character and nothing may fire.
// Before the matched control and body redaction existed this reported 26 findings.
func TestPatchedLabIsSilent(t *testing.T) {
	sig, _, erratic := scan(t, "patched")
	if erratic {
		t.Fatal("patched lab was judged erratic")
	}
	var fp []string
	for enc, fired := range sig {
		if fired {
			fp = append(fp, enc)
		}
	}
	if len(fp) > 0 {
		t.Fatalf("%d false positives against a patched target: %v", len(fp), fp)
	}
}

// TestErraticLabIsRefused — a host whose own baseline is not reproducible cannot support
// a differential, and scanning it produces nothing but noise.
func TestErraticLabIsRefused(t *testing.T) {
	if _, _, erratic := scan(t, "erratic"); !erratic {
		t.Fatal("erratic target was not refused by the stability gate")
	}
}

// unitFromLab does what the crawler does: issue the request, keep the RAW response, and
// hand both halves on as a unit. The response is what names the injection points.
func unitFromLab(t *testing.T, l *lab.Lab) unit.Exchange {
	t.Helper()
	u := unit.Exchange{URL: l.URL()}
	if err := u.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	po := probe.DefaultOptions()
	po.KeepRawResponses = true
	obs := probe.Run(context.Background(), probe.Target{Host: "127.0.0.1", Port: u.Port(), Scheme: "http"},
		[]probe.Step{{Label: "seed", Raw: inject.Apply(u, inject.Point{}, "")}}, po)
	if obs.DialErr != "" || len(obs.Steps) == 0 {
		t.Fatalf("seed request failed: %s", obs.DialErr)
	}
	u.Response = unit.Message{Raw: string(obs.Steps[0].RespRaw)}
	if err := u.Normalize(); err != nil {
		t.Fatalf("normalize with response: %v", err)
	}
	return u
}

// TestResponseAdvertisedHeaderIsFoundAndProbed is the end-to-end case for the whole
// unit-based design. X-Tenant-Id appears NOWHERE in the request. The only way to learn
// the application accepts it is to read Access-Control-Allow-Headers off the response —
// and the only way to exploit it is to deliver raw UTF-8 bytes, since nothing
// percent-decodes a header value.
func TestResponseAdvertisedHeaderIsFoundAndProbed(t *testing.T) {
	l, err := lab.Start("vulnerable")
	if err != nil {
		t.Fatalf("lab: %v", err)
	}
	defer l.Close()

	u := unitFromLab(t, l)
	if got := u.Response.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Tenant-Id") {
		t.Fatalf("lab did not advertise the header: %q", got)
	}
	if u.Request.Has("X-Tenant-Id") {
		t.Fatal("precondition broken: the request must not mention the header")
	}

	var point *inject.Point
	for _, p := range inject.Enumerate(u, inject.DefaultOptions()) {
		if p.ID == "header:X-Tenant-Id:value" {
			pp := p
			point = &pp
			break
		}
	}
	if point == nil {
		t.Fatal("enumerator missed the header the response advertised")
	}
	if point.Source != inject.SrcRespACAH {
		t.Errorf("source = %s, want %s", point.Source, inject.SrcRespACAH)
	}
	if point.Render != inject.RenderWire {
		t.Fatalf("render = %s; a percent-encoded payload in a header value tests nothing", point.Render)
	}

	o := detect.DefaultOptions()
	o.Repeats, o.Rate, o.Timeout = 3, 0, 4*time.Second
	sc := detect.New(o)
	ctx := context.Background()
	base := sc.SampleRaw(ctx, u, inject.Apply(u, inject.Point{}, ""), "baseline")

	var v vectors.Vector
	for _, c := range vectors.Generate(vectors.TierQuick, nil) {
		if c.Encoded == "%DC%8A" {
			v = c
		}
	}
	mut := sc.SampleRaw(ctx, u, inject.Apply(u, *point, inject.Render(v, point.Render)), "mut")
	ctrl := sc.SampleRaw(ctx, u, inject.Apply(u, *point, inject.RenderControl(v, point.Render)), "ctl")

	if sig := detect.Analyze(ctrl, mut, v); sig.Count == 0 {
		t.Fatalf("no signal on the advertised header: ctrl=%d %q  mut=%d %q",
			ctrl.Status, ctrl.BodyPreview, mut.Status, mut.BodyPreview)
	}
	_ = base

	// And the confirmation: a folded newline in that header value introduces a Host into
	// the backend's request.
	fold := inject.Render(v, point.Render)
	conf := sc.SampleRaw(ctx, u, inject.Apply(u, *point, fold+"Host:canary-hdr"+fold), "confirm")
	if !strings.Contains(conf.BodyPreview, "canary-hdr") {
		t.Fatalf("header injection not confirmed: %q", conf.BodyPreview)
	}
}
