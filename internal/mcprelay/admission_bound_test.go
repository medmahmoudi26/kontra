package mcprelay

import (
	"testing"

	"kontra.sh/kontra/internal/hooks"
)

// A modern client's shared Admission session decision runs on a context bounded by admitFlightTimeout, so the hook's
// own timeout, which hooks.admission.timeout sets and MaxAdmissionTimeout bounds, must end first: a longer flight
// bound would otherwise cut a hook the A2A and legacy MCP paths still wait for (spec/relay.md §13.1, D284).
func TestAdmitFlightOutlastsTheHookTimeout(t *testing.T) {
	if admitFlightTimeout < hooks.MaxAdmissionTimeout {
		t.Errorf("admitFlightTimeout %v is shorter than hooks.MaxAdmissionTimeout %v", admitFlightTimeout, hooks.MaxAdmissionTimeout)
	}
}
