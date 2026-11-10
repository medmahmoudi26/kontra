package relay

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/store"
)

// A hire activates, for its buyer, the required URIs the operator's card_extensions entries assert, and refuses the
// others: the seller's own, and the sealed extension's whoever asserts it (spec §2.4, §12.2; D294, D300). What it
// activates reaches Admission's extensions through the context, never a service parameter.
func TestHireActivate(t *testing.T) {
	const opExt, own, other = "https://operator.example.com/ext/audit", "https://seller.example.com/ext/own", "https://operator.example.com/ext/other"
	for _, tc := range []struct {
		name      string
		card      CardExtensions
		on, unmet []string
	}{
		{"nothing required", CardExtensions{Operator: []string{opExt}}, nil, nil},
		{"the operator's entry", CardExtensions{Required: []string{opExt}, Operator: []string{other, opExt}}, []string{opExt}, nil},
		{"two of the operator's", CardExtensions{Required: []string{other, opExt}, Operator: []string{opExt, other}}, []string{other, opExt}, nil},
		{"the seller's own", CardExtensions{Required: []string{own}}, nil, []string{own}},
		{"the seller's beside the operator's", CardExtensions{Required: []string{opExt, own}, Operator: []string{opExt}}, nil, []string{own}},
		{"sealed, as the operator's entry", CardExtensions{Required: []string{sealed.URI, opExt}, Operator: []string{sealed.URI, opExt}}, nil, []string{sealed.URI}},
	} {
		h := &Hirer{Extensions: func(store.ServiceRef) CardExtensions { return tc.card }}
		on, err := h.activate(store.ServiceRef{Name: "svc"})
		var xe *ExtensionsRequiredError
		switch {
		case tc.unmet == nil && (err != nil || !slices.Equal(on, tc.on)):
			t.Errorf("%s: activated %q, %v; want %q", tc.name, on, err, tc.on)
		case tc.unmet != nil && (!errors.As(err, &xe) || !errors.Is(err, a2a.ErrExtensionSupportRequired) || !slices.Equal(xe.URIs, tc.unmet) || on != nil):
			t.Errorf("%s: activated %q, %v; want ExtensionsRequiredError %q", tc.name, on, err, tc.unmet)
		}
	}
	if on, err := (&Hirer{}).activate(store.ServiceRef{}); on != nil || err != nil {
		t.Errorf("a hirer that reads no card: %q, %v", on, err)
	}

	ctx := withHireActivated(context.Background(), []string{opExt})
	if got := hireActivated(ctx); !slices.Equal(got, []string{opExt}) {
		t.Errorf("hireActivated = %q", got)
	}
	if got := hireActivated(withHireActivated(context.Background(), nil)); got != nil {
		t.Errorf("hireActivated of nothing = %q", got)
	}
}
