package hooks

import (
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// MaxMapBytes bounds every metadata or mcpMeta map an operator writes (spec §13.1–§13.3).
const MaxMapBytes = 16 << 10

// KeyRetainUntil is the one kontra. key outside the operator prefixes an operator may always write
// (spec §13.3).
const KeyRetainUntil = "kontra.keyRetainUntil"

// KeyRules are the §13.2 key and size rules applied to Admission decisions, kontra.approval,
// kontra.annotate and Lifecycle patches, and the §2.5 caller rules.
type KeyRules struct {
	OperatorPrefixes []string // operator_metadata_prefixes
	CallerMetadata   []string // caller_metadata
}

// CheckOperatorMaps refuses a metadata key outside the operator prefixes (other than KeyRetainUntil), an
// mcpMeta key outside sh.kontra/, or a map over MaxMapBytes.
func (r KeyRules) CheckOperatorMaps(metadata, mcpMeta map[string]any) error {
	return unimpl.Error("hooks.KeyRules.CheckOperatorMaps", "WP-04")
}

// StripCaller removes every kontra.-prefixed key from caller metadata except kontra.sealed,
// kontra.sealed.replyTo and the caller_metadata keys (spec §2.5).
func (r KeyRules) StripCaller(md map[string]any) map[string]any {
	panic(unimpl.Error("hooks.KeyRules.StripCaller", "WP-04"))
}
