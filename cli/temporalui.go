// temporalui.go — `kontra up --temporal-ui`, the wiring half (ADR 0031, issue 16).
//
// The machinery is `cli/appliance/temporalui.go`: a pinned release of temporalio/ui-server,
// hydrated from the same CAS the orchestrator bundle comes out of and supervised as a child.
// This file is only the two things `kontra up` itself has to do — decide whether the flag asked
// for one, and say where it went.
//
// THE FLAG'S OFF ANSWER IS THE FIRST LINE OF startTemporalUI, and it is written that way on
// purpose. "No flag means no UI port bound and no UI artifact hydrated" is then a property of one
// function that a test can call, rather than a property of the ORDER of the lines in cmdUp, which
// nothing can check and any later edit can quietly break.
package main

import (
	"context"
	"fmt"
	"io"

	"github.com/medmahmoudi26/kontra/cli/appliance"
)

// startTemporalUI starts Temporal's Web UI if the flag asked for one, and otherwise does nothing
// whatsoever — no fetch, no hydration, no listener, no child process.
//
// It returns (nil, nil) for the off case rather than an error or a zero value, because "there is
// no UI" is the ordinary state of this appliance and the caller's `if ui != nil` is the whole
// handling it needs.
func startTemporalUI(ctx context.Context, on bool, opts appliance.TemporalUIOptions) (*appliance.TemporalUI, error) {
	if !on {
		return nil, nil
	}
	return appliance.StartTemporalUI(ctx, opts)
}

// reportTemporalUIExit says so, once, if the UI dies on its own.
//
// A DEAD UI DOES NOT STOP THE APPLIANCE, and that is the one place this child differs from the
// orchestrator: the orchestrator IS the control plane and `kontra up` outliving it is the hollow
// failure child.go exists to prevent, while this is a window onto a control plane that keeps
// working without it. But it must not be SILENT either — the tab simply stops loading, and
// nothing anywhere says the process went. Nothing is printed for a stop we asked for: Child.Err
// is nil in that case by design.
func reportTemporalUIExit(w io.Writer, ui *appliance.TemporalUI) {
	go func() {
		<-ui.Done()
		if err := ui.Err(); err != nil {
			fmt.Fprintf(w, "\nthe Temporal Web UI is gone: %v\n"+
				"  the rest of the appliance is unaffected; `kontra up --temporal-ui` again to get it back\n", err)
		}
	}()
}

// temporalUIBanner is what printTemporalUI needs of a started UI.
//
// AN INTERFACE, SO THE BANNER IS CHECKABLE WITHOUT ONE. `kontra up --temporal-ui` printing its
// address is an acceptance criterion, and the alternative to this three-line seam is proving it
// by hydrating 26 MB and starting a process — which the appliance package already does once, and
// which nothing is served by doing twice. `*appliance.TemporalUI` satisfies it as it stands.
type temporalUIBanner interface {
	URL() string
	PID() int
	CodecEndpoint() string
	Artifact() *appliance.HydratedTemporalUI
}

// printTemporalUI says where the UI is, what it is, and what it can read.
//
// THE CODEC LINE IS NOT DECORATION. The endpoint printed here is the one the browser will call,
// read off the codec's own listener — so an operator looking at a page full of undecodable `$ref`
// payloads can compare this line with the `Payload codec:` line above it and see that they are
// the same string. In compose those were two independently-configured values and their
// disagreement was invisible; printing the derived one is what makes the derivation checkable by
// eye as well as by test.
func printTemporalUI(w io.Writer, ui temporalUIBanner) {
	art := ui.Artifact()
	fmt.Fprintf(w, "%-22s %s  (pid %d)\n", "Temporal Web UI:", ui.URL(), ui.PID())
	fmt.Fprintf(w, "%-22s ui-server %s, sha256:%s\n", "Web UI artifact:", art.Version, shortDigest(art.Digest))
	if ui.CodecEndpoint() != "" {
		fmt.Fprintf(w, "%-22s %s\n", "Web UI decodes via:", ui.CodecEndpoint())
	} else {
		// Worth one line, because the symptom is silent: every offloaded payload renders as a
		// `$ref` the page cannot open, and nothing on it says why.
		fmt.Fprintf(w, "%-22s %s\n", "Web UI decodes via:", "(no codec — offloaded payloads will not render)")
	}
}
