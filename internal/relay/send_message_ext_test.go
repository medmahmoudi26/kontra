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

// A send_message activates, for its caller, the required URIs the operator's card_extensions entries assert, and refuses the
// others: the service's own, and the sealed extension's whoever asserts it (spec §2.4, §12.2; D294, D300). What it
// activates reaches Admission's extensions through the context, never a service parameter.
func TestSendMessageActivate(t *testing.T) {
	const opExt, own, other = "https://operator.example.com/ext/audit", "https://service.example.com/ext/own", "https://operator.example.com/ext/other"
	for _, tc := range []struct {
		name      string
		card      CardExtensions
		on, unmet []string
	}{
		{"nothing required", CardExtensions{Operator: []string{opExt}}, nil, nil},
		{"the operator's entry", CardExtensions{Required: []string{opExt}, Operator: []string{other, opExt}}, []string{opExt}, nil},
		{"two of the operator's", CardExtensions{Required: []string{other, opExt}, Operator: []string{opExt, other}}, []string{other, opExt}, nil},
		{"the service's own", CardExtensions{Required: []string{own}}, nil, []string{own}},
		{"the service's beside the operator's", CardExtensions{Required: []string{opExt, own}, Operator: []string{opExt}}, nil, []string{own}},
		{"sealed, as the operator's entry", CardExtensions{Required: []string{sealed.URI, opExt}, Operator: []string{sealed.URI, opExt}}, nil, []string{sealed.URI}},
	} {
		h := &Sender{Extensions: func(store.ServiceRef) CardExtensions { return tc.card }}
		on, err := h.activate(store.ServiceRef{Name: "svc"})
		var xe *ExtensionsRequiredError
		switch {
		case tc.unmet == nil && (err != nil || !slices.Equal(on, tc.on)):
			t.Errorf("%s: activated %q, %v; want %q", tc.name, on, err, tc.on)
		case tc.unmet != nil && (!errors.As(err, &xe) || !errors.Is(err, a2a.ErrExtensionSupportRequired) || !slices.Equal(xe.URIs, tc.unmet) || on != nil):
			t.Errorf("%s: activated %q, %v; want ExtensionsRequiredError %q", tc.name, on, err, tc.unmet)
		}
	}
	if on, err := (&Sender{}).activate(store.ServiceRef{}); on != nil || err != nil {
		t.Errorf("a caller that reads no card: %q, %v", on, err)
	}

	ctx := withSendMessageActivated(context.Background(), []string{opExt})
	if got := sendMessageActivated(ctx); !slices.Equal(got, []string{opExt}) {
		t.Errorf("sendMessageActivated = %q", got)
	}
	if got := sendMessageActivated(withSendMessageActivated(context.Background(), nil)); got != nil {
		t.Errorf("sendMessageActivated of nothing = %q", got)
	}
}
