package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinAdmissionAllows(t *testing.T) {
	d, err := BuiltinAdmission{}.Admit(context.Background(), AdmitRequest{
		Operation: OpTask, TaskID: "0192f0b4-0000-7000-8000-000000000001", Protocol: "a2a",
		Principal: Principal{Kind: "key", ID: "key:kk_a_00000001", BuyerID: "b", Via: "api_key"},
	})
	if err != nil || d.Outcome != Allow || d.Refusal != nil || d.Quote != nil || d.Metadata != nil {
		t.Errorf("built-in admission: %+v %v", d, err)
	}
	if _, err := (BuiltinAdmission{}).Admit(context.Background(), AdmitRequest{}); err == nil {
		t.Error("an admission request without its operation was allowed")
	}
	// The payload is hooks.json's camelCase shape.
	b, _ := json.Marshal(AdmitRequest{Operation: OpTask, TaskID: "t", ContextID: "c", Message: &MessageInfo{MessageID: "m", Parts: []PartInfo{{Size: 3}}}})
	for _, k := range []string{`"operation":"task"`, `"taskId":"t"`, `"contextId":"c"`, `"messageId":"m"`, `"size":3`, `"sealed":false`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("%s missing from %s", k, b)
		}
	}
}

func TestAdmittedContext(t *testing.T) {
	if _, ok := AdmittedFrom(context.Background()); ok {
		t.Fatal("an admission out of nowhere")
	}
	ctx := WithAdmitted(context.Background(), Admitted{TaskID: "id-1", Decision: Decision{Outcome: Allow}})
	a, ok := AdmittedFrom(ctx)
	if !ok || a.TaskID != "id-1" || a.Decision.Outcome != Allow {
		t.Errorf("%+v %v", a, ok)
	}
}

func TestCheckOperatorMaps(t *testing.T) {
	r := KeyRules{OperatorPrefixes: []string{"kontra.escrow.", "acme."}}
	ok := []struct{ md, meta map[string]any }{
		{nil, nil},
		{map[string]any{"kontra.escrow.state": "held", "acme.x": 1, KeyRetainUntil: "2026-10-26T00:00:00Z"}, map[string]any{"sh.kontra/escrow": map[string]any{"s": 1}}},
	}
	for _, c := range ok {
		if err := r.CheckOperatorMaps(c.md, c.meta); err != nil {
			t.Errorf("%v %v: %v", c.md, c.meta, err)
		}
	}
	bad := []struct {
		md, meta map[string]any
		want     error
	}{
		{map[string]any{"kontra.approve.url": "x"}, nil, ErrForbiddenKey},
		{map[string]any{"note": "x"}, nil, ErrForbiddenKey},
		{map[string]any{"kontra.escrowX": "x"}, nil, ErrForbiddenKey},
		{nil, map[string]any{"price": 1}, ErrForbiddenKey},
		{map[string]any{"acme.big": strings.Repeat("x", MaxMapBytes)}, nil, ErrMapTooLarge},
		{nil, map[string]any{"sh.kontra/big": strings.Repeat("x", MaxMapBytes)}, ErrMapTooLarge},
	}
	for _, c := range bad {
		if err := r.CheckOperatorMaps(c.md, c.meta); !errors.Is(err, c.want) {
			t.Errorf("%v %v: %v, want %v", c.md, c.meta, err, c.want)
		}
	}
	// No prefixes configured (open source): only kontra.keyRetainUntil.
	if err := (KeyRules{}).CheckOperatorMaps(map[string]any{"anything": 1}, nil); !errors.Is(err, ErrForbiddenKey) {
		t.Errorf("no prefixes: %v", err)
	}
}

func TestStripCaller(t *testing.T) {
	r := KeyRules{CallerMetadata: []string{"kontra.escrow.dispute"}}
	in := map[string]any{
		"kontra.approve.url": "https://evil", "Kontra.Escrow.State": "released", "kontra.failure": "x",
		KeySealed: map[string]any{"original": "text/plain"}, KeySealedReplyTo: "k1_x", "kontra.escrow.dispute": true,
		"user.note": "kept", "kontraX": "not a kontra. key",
	}
	orig := map[string]any{}
	for k, v := range in {
		orig[k] = v
	}
	got := r.StripCaller(in)
	want := map[string]any{
		KeySealed: map[string]any{"original": "text/plain"}, KeySealedReplyTo: "k1_x", "kontra.escrow.dispute": true,
		"user.note": "kept", "kontraX": "not a kontra. key",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Error("StripCaller modified its argument")
	}
	if got := r.StripCaller(map[string]any{"kontra.approve.url": "x"}); got != nil {
		t.Errorf("all keys stripped: %v, want nil", got)
	}
	if got := r.CallerMetadataOnly(in); !reflect.DeepEqual(got, map[string]any{"kontra.escrow.dispute": true}) {
		t.Errorf("caller_metadata only: %v", got)
	}
}

func TestStripService(t *testing.T) {
	md := map[string]any{"kontra.approve.url": "x", KeySealed: map[string]any{"original": "x"}, "seller": 1}
	if got := StripService(md, false); !reflect.DeepEqual(got, map[string]any{"seller": 1}) {
		t.Errorf("not a sealed part: %v", got)
	}
	if got := StripService(md, true); !reflect.DeepEqual(got, map[string]any{KeySealed: map[string]any{"original": "x"}, "seller": 1}) {
		t.Errorf("a sealed part keeps kontra.sealed: %v", got)
	}
	if got := StripService(map[string]any{"a": 1}, false); !reflect.DeepEqual(got, map[string]any{"a": 1}) {
		t.Errorf("nothing to strip: %v", got)
	}
}
