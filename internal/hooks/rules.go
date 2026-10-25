package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxMapBytes bounds every metadata or mcpMeta map an operator writes (spec §13.1–§13.3).
const MaxMapBytes = 16 << 10

// KeyRetainUntil is the one kontra. key outside the operator prefixes an operator may always write
// (spec §13.3).
const KeyRetainUntil = "kontra.keyRetainUntil"

// Reserved key prefixes (spec §2.5, D27).
const (
	KontraPrefix  = "kontra."    // A2A metadata keys only the gate or the operator may assert
	MCPMetaPrefix = "sh.kontra/" // MCP _meta keys, the same rule
)

// The sealing keys a caller may write (ext-sealed.md), and the one a service may write on a part of the
// sealed envelope type.
const (
	KeySealed        = "kontra.sealed"
	KeySealedReplyTo = "kontra.sealed.replyTo"
	// SealedMediaType is the sealed envelope's part mediaType (ext-sealed.md §3).
	SealedMediaType = "application/vnd.kontra.sealed+age;v=1"
)

// Refusals of the §13.2 rules.
var (
	ErrForbiddenKey = errors.New("hooks: metadata key outside the operator's prefixes")
	ErrMapTooLarge  = errors.New("hooks: metadata map over 16 KiB")
)

// KeyRules are the §13.2 key and size rules applied to Admission decisions, kontra.approval,
// kontra.annotate and Lifecycle patches, and the §2.5 caller rules.
type KeyRules struct {
	OperatorPrefixes []string // operator_metadata_prefixes
	CallerMetadata   []string // caller_metadata
}

// IsKontraKey reports whether a metadata key is one of Kontra's: it starts with "kontra.", compared
// without regard to case, so a seller or a caller cannot slip "Kontra.approve.url" past a consumer that
// folds case.
func IsKontraKey(k string) bool {
	return len(k) >= len(KontraPrefix) && strings.EqualFold(k[:len(KontraPrefix)], KontraPrefix)
}

// CheckOperatorMaps refuses a metadata key outside the operator prefixes (other than KeyRetainUntil), an
// mcpMeta key outside sh.kontra/, or a map over MaxMapBytes.
func (r KeyRules) CheckOperatorMaps(metadata, mcpMeta map[string]any) error {
	for k := range metadata {
		if k == KeyRetainUntil || slices.ContainsFunc(r.OperatorPrefixes, func(p string) bool { return p != "" && strings.HasPrefix(k, p) }) {
			continue
		}
		return fmt.Errorf("%w: %q (allowed: %q and %s)", ErrForbiddenKey, k, r.OperatorPrefixes, KeyRetainUntil)
	}
	for k := range mcpMeta {
		if !strings.HasPrefix(k, MCPMetaPrefix) {
			return fmt.Errorf("%w: mcpMeta key %q is not under %s", ErrForbiddenKey, k, MCPMetaPrefix)
		}
	}
	for name, m := range map[string]map[string]any{"metadata": metadata, "mcpMeta": mcpMeta} {
		if len(m) == 0 {
			continue
		}
		b, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("hooks: %s is not JSON: %w", name, err)
		}
		if len(b) > MaxMapBytes {
			return fmt.Errorf("%w: %s is %d bytes", ErrMapTooLarge, name, len(b))
		}
	}
	return nil
}

// callerAllowed reports whether a caller may write the kontra. key k.
func (r KeyRules) callerAllowed(k string) bool {
	return k == KeySealed || k == KeySealedReplyTo || slices.Contains(r.CallerMetadata, k)
}

// StripCaller removes every kontra.-prefixed key from caller metadata except kontra.sealed,
// kontra.sealed.replyTo and the caller_metadata keys (spec §2.5). md is not modified: a map with keys
// removed is a copy, nil when nothing is left; a map with nothing to remove is returned as is.
func (r KeyRules) StripCaller(md map[string]any) map[string]any {
	return strip(md, r.callerAllowed)
}

// CallerMetadataOnly returns the caller_metadata keys of md (what OnReviewMessage receives, spec §13.3),
// nil when there are none.
func (r KeyRules) CallerMetadataOnly(md map[string]any) map[string]any {
	var out map[string]any
	for _, k := range r.CallerMetadata {
		if v, ok := md[k]; ok {
			if out == nil {
				out = map[string]any{}
			}
			out[k] = v
		}
	}
	return out
}

// StripService removes every kontra.-prefixed key from service metadata (spec §2.5); on a part of the
// sealed envelope type, kontra.sealed is kept. md is not modified.
func StripService(md map[string]any, sealedPart bool) map[string]any {
	return strip(md, func(k string) bool { return sealedPart && k == KeySealed })
}

func strip(md map[string]any, keep func(string) bool) map[string]any {
	drop := false
	for k := range md {
		if IsKontraKey(k) && !keep(k) {
			drop = true
			break
		}
	}
	if !drop {
		return md
	}
	out := make(map[string]any, len(md))
	for k, v := range md {
		if IsKontraKey(k) && !keep(k) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
