package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/sealed"
)

// The gate's half of the sealed extension for A2A (spec/ext-sealed.md §3, §5 "Gate"; relay.md §2.5): it never
// decrypts, re-encrypts or alters a sealed part. It refuses a part that claims the envelope type in any form but
// raw (InvalidRequest from a caller, upstream_error from a service); for a service with sealed: required it
// refuses a message carrying an unsealed part before any task or stream exists (InvalidRequest, -32600,
// sealed_unsealed_part); for sealed: {required: false} it passes an unsealed part with metadata["kontra.sealed"]
// = false. Sealed modes are services.sealed's values.
const (
	sealedOff      = "off"
	sealedOptional = "optional"
	sealedRequired = "required"
)

// errSealedForm is a part that claims the envelope media type in a form other than raw (ext-sealed.md §3).
var errSealedForm = errors.New("relay: a part with the sealed envelope media type is not in raw form")

// sealedPart reports whether p is a sealed part: ok is true when it claims the envelope media type, and err then
// says it is not exactly the raw form (sealed.ParsePart, read from the part a2a-go decoded, re-encoded in its 1.0
// shape: a2a-go's own decoding already took one member per name).
func sealedPart(p *a2a.Part) (ok bool, err error) {
	if p == nil {
		return false, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return true, fmt.Errorf("%w: %v", errSealedForm, err)
	}
	_, ok, err = sealed.ParsePart(b)
	if ok && err != nil {
		return true, fmt.Errorf("%w: %v", errSealedForm, err)
	}
	return ok, nil
}

// sealedCallerParts applies the gate's rules to a caller's message parts for a service whose sealed mode is mode,
// after Kontra's keys were stripped (§2.5): it returns the parts to forward, an unsealed part of an optional
// service carrying metadata["kontra.sealed"] = false, or an InvalidRequest refusal.
func sealedCallerParts(mode string, ps a2a.ContentParts) (a2a.ContentParts, error) {
	var out a2a.ContentParts
	for n, p := range ps {
		if p == nil {
			continue
		}
		isSealed, err := sealedPart(p)
		if err != nil {
			return nil, a2a.NewError(a2a.ErrInvalidRequest,
				"a part with the media type "+sealed.MediaType+" must be the sealed envelope in raw form (ext-sealed.md §3)")
		}
		if isSealed {
			continue
		}
		switch mode {
		case sealedRequired:
			return nil, a2a.NewError(a2a.ErrInvalidRequest,
				"this service is sealed: every part must be a sealed envelope ("+sealed.MediaType+"; ext-sealed.md §5)")
		case sealedOptional:
			if out == nil {
				out = append(a2a.ContentParts(nil), ps...)
			}
			cp := *p
			cp.Metadata = maps.Clone(p.Metadata)
			if cp.Metadata == nil {
				cp.Metadata = map[string]any{}
			}
			cp.Metadata[hooks.KeySealed] = false
			out[n] = &cp
		}
	}
	if out == nil {
		return ps, nil
	}
	return out, nil
}

// CheckSealedForm is the service-side rule: an upstream event with a part that claims the envelope media type in
// a form other than raw fails the attempt upstream_error (ext-sealed.md §3). The event is the gate's rewrite
// (ToGate), whose messages and artifacts it reads.
func CheckSealedForm(ev a2a.Event) error {
	check := func(ps a2a.ContentParts) error {
		for _, p := range ps {
			if _, err := sealedPart(p); err != nil {
				return err
			}
		}
		return nil
	}
	msg := func(m *a2a.Message) error {
		if m == nil {
			return nil
		}
		return check(m.Parts)
	}
	switch e := ev.(type) {
	case *a2a.Task:
		if err := msg(e.Status.Message); err != nil {
			return err
		}
		for _, h := range e.History {
			if err := msg(h); err != nil {
				return err
			}
		}
		for _, a := range e.Artifacts {
			if a != nil {
				if err := check(a.Parts); err != nil {
					return err
				}
			}
		}
	case *a2a.TaskStatusUpdateEvent:
		return msg(e.Status.Message)
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact != nil {
			return check(e.Artifact.Parts)
		}
	case *a2a.Message:
		return msg(e)
	}
	return nil
}
