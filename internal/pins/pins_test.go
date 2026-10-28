package pins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const gate = "https://gate.example.com"

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".kontra", FileName)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func changeErr(t *testing.T, err error) *ChangeError {
	t.Helper()
	var ce *ChangeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *ChangeError", err)
	}
	return ce
}

var base = Pin{Sealed: true, Recipients: []string{"k1_b", "k1_a"}, SellerKeys: []string{"tp1"}, Signed: true}

func TestFirstSight(t *testing.T) {
	s, p := open(t)
	clock := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	if _, ok, err := s.Get(gate, "booking"); ok || err != nil {
		t.Fatalf("Get before any check = %v, %v", ok, err)
	}
	out, err := s.Check(gate, "booking", base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.First || len(out.Changes) != 0 || !slices.Equal(out.Pin.Recipients, []string{"k1_a", "k1_b"}) {
		t.Fatalf("Outcome = %+v", out)
	}
	pin, ok, err := s.Get(gate, "booking")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if !pin.Sealed || !pin.Signed || !slices.Equal(pin.Recipients, []string{"k1_a", "k1_b"}) ||
		!slices.Equal(pin.SellerKeys, []string{"tp1"}) || !pin.At.Equal(clock) {
		t.Fatalf("recorded %+v", pin)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("pins file mode %04o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("~/.kontra created with mode %04o", fi.Mode().Perm())
	}
	var doc struct {
		V    int                       `json:"v"`
		Pins map[string]map[string]Pin `json:"pins"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil || doc.V != 1 || len(doc.Pins[gate]) != 1 {
		t.Fatalf("file = %s", b)
	}

	// The same thing seen again changes nothing and writes nothing.
	clock = clock.Add(time.Hour)
	before, _ := os.ReadFile(p)
	out, err = s.Check(gate, "booking", Pin{Sealed: true, Recipients: []string{"k1_a", "k1_b", "k1_a"}, SellerKeys: []string{"tp1"}, Signed: true}, Options{})
	if err != nil || out.First || len(out.Changes) != 0 {
		t.Fatalf("unchanged: %+v, %v", out, err)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("an unchanged check rewrote the file")
	}
	// A pin is per gate: the same service at another gate is a first sight.
	if out, err := s.Check("https://other.example", "booking", Pin{}, Options{}); err != nil || !out.First {
		t.Fatalf("another gate: %+v, %v", out, err)
	}
}

func TestDowngradesAlwaysRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		now  Pin
		want Change
	}{
		{"unsealed", Pin{Sealed: false, Recipients: base.Recipients, SellerKeys: base.SellerKeys, Signed: true}, ChangeUnsealed},
		{"unsigned", Pin{Sealed: true, Recipients: base.Recipients, SellerKeys: base.SellerKeys, Signed: false}, ChangeUnsigned},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := open(t)
			if _, err := s.Check(gate, "booking", base, Options{}); err != nil {
				t.Fatal(err)
			}
			for _, opts := range []Options{{}, {Accept: []string{"booking"}}, {PinRecipients: true, Accept: []string{"booking"}}} {
				_, err := s.Check(gate, "booking", c.now, opts)
				ce := changeErr(t, err)
				if ce.Gate != gate || ce.Service != "booking" || !slices.Contains(ce.Changes, c.want) {
					t.Fatalf("opts %+v: ChangeError %+v", opts, ce)
				}
				if !strings.Contains(ce.Detail(opts), "always refused") {
					t.Fatalf("Detail = %s", ce.Detail(opts))
				}
			}
			pin, _, _ := s.Get(gate, "booking")
			if !pin.Sealed || !pin.Signed {
				t.Fatalf("a refused downgrade was recorded: %+v", pin)
			}
		})
	}
}

func TestNewRecipientAndSellerKey(t *testing.T) {
	for _, c := range []struct {
		name string
		now  Pin
		want Change
	}{
		{"recipient", Pin{Sealed: true, Recipients: []string{"k1_a", "k1_b", "k1_c"}, SellerKeys: []string{"tp1"}, Signed: true}, ChangeNewRecipient},
		{"seller key", Pin{Sealed: true, Recipients: []string{"k1_a", "k1_b"}, SellerKeys: []string{"tp1", "tp2"}, Signed: true}, ChangeNewSellerKey},
		{"substituted recipient", Pin{Sealed: true, Recipients: []string{"k1_a", "k1_evil"}, SellerKeys: []string{"tp1"}, Signed: true}, ChangeNewRecipient},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := open(t)
			if _, err := s.Check(gate, "booking", base, Options{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Check(gate, "recon", base, Options{}); err != nil {
				t.Fatal(err)
			}
			// Refused under --pin-recipients, even with --accept-change.
			for _, opts := range []Options{{PinRecipients: true}, {PinRecipients: true, Accept: []string{"booking"}}} {
				ce := changeErr(t, func() error { _, err := s.Check(gate, "booking", c.now, opts); return err }())
				if !slices.Contains(ce.Changes, c.want) || !strings.Contains(ce.Detail(opts), "--pin-recipients") {
					t.Fatalf("PinRecipients: %+v, %s", ce, ce.Detail(opts))
				}
			}
			// Refused without --accept-change, and with one naming another service.
			for _, opts := range []Options{{}, {Accept: []string{"recon"}}} {
				ce := changeErr(t, func() error { _, err := s.Check(gate, "booking", c.now, opts); return err }())
				if !strings.Contains(ce.Detail(opts), "--accept-change booking") {
					t.Fatalf("Detail = %s", ce.Detail(opts))
				}
			}
			if pin, _, _ := s.Get(gate, "booking"); !slices.Equal(pin.Recipients, []string{"k1_a", "k1_b"}) || !slices.Equal(pin.SellerKeys, []string{"tp1"}) {
				t.Fatalf("a refused change was recorded: %+v", pin)
			}
			// Accepted for that service only, and recorded.
			out, err := s.Check(gate, "booking", c.now, Options{Accept: []string{"booking"}})
			if err != nil || !slices.Contains(out.Changes, c.want) || out.First {
				t.Fatalf("accepted: %+v, %v", out, err)
			}
			pin, _, _ := s.Get(gate, "booking")
			if !slices.Equal(pin.Recipients, normalize(c.now).Recipients) || !slices.Equal(pin.SellerKeys, normalize(c.now).SellerKeys) {
				t.Fatalf("recorded %+v, want %+v", pin, c.now)
			}
			if out.Prev.Recipients == nil || !slices.Equal(out.Prev.Recipients, []string{"k1_a", "k1_b"}) {
				t.Fatalf("Outcome.Prev = %+v", out.Prev)
			}
			// Now the pin: the same set again is no change, without --accept-change.
			if out, err := s.Check(gate, "booking", c.now, Options{}); err != nil || len(out.Changes) != 0 {
				t.Fatalf("after acceptance: %+v, %v", out, err)
			}
			// The other service at the gate still has its own pin, and the same change there is refused.
			if _, err := s.Check(gate, "recon", c.now, Options{Accept: []string{"booking"}}); err == nil {
				t.Fatal("acceptance for booking applied to recon")
			}
		})
	}
}

func TestAcceptedChanges(t *testing.T) {
	s, _ := open(t)
	start := Pin{Sealed: false, Recipients: nil, SellerKeys: []string{"tp1", "tp2"}, Signed: false}
	if _, err := s.Check(gate, "svc", start, Options{}); err != nil {
		t.Fatal(err)
	}
	// Upgrades pass without --accept-change, even under --pin-recipients, and are recorded; a removed seller key does
	// not (round 2 sealing-fixes-5, D228): it is refused under --pin-recipients, and otherwise until accepted.
	now := Pin{Sealed: true, Recipients: nil, SellerKeys: []string{"tp1"}, Signed: true}
	if _, err := s.Check(gate, "svc", now, Options{PinRecipients: true}); err == nil {
		t.Fatal("a removed seller key was accepted under --pin-recipients")
	}
	if _, err := s.Check(gate, "svc", now, Options{}); err == nil {
		t.Fatal("a removed seller key was accepted without --accept-change")
	}
	out, err := s.Check(gate, "svc", now, Options{Accept: []string{"svc"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Changes, []Change{ChangeSealed, ChangeSigned, ChangeLessSellerKey}) {
		t.Fatalf("Changes = %v", out.Changes)
	}
	if out, err := s.Check(gate, "svc", Pin{Sealed: true, SellerKeys: []string{"tp1"}, Signed: true}, Options{PinRecipients: true}); err != nil || len(out.Changes) != 0 {
		t.Fatalf("the accepted pin again: %+v %v", out, err)
	}
	if _, err := s.Check(gate, "svc2", Pin{SellerKeys: []string{"tp1"}}, Options{}); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Check(gate, "svc2", Pin{Sealed: true, SellerKeys: []string{"tp1"}, Signed: true}, Options{PinRecipients: true}); err != nil ||
		!slices.Equal(out.Changes, []Change{ChangeSealed, ChangeSigned}) {
		t.Fatalf("upgrades under --pin-recipients: %+v %v", out, err)
	}
	// Going from no recipients (unsealed) to sealed with recipients is a new recipient: the first recipients of a
	// service that was unsealed are exactly what an active gate would substitute.
	if _, err := s.Check(gate, "svc", Pin{Sealed: true, Recipients: []string{"k1_a"}, SellerKeys: []string{"tp1"}, Signed: true}, Options{}); err == nil {
		t.Fatal("the first recipients after a pin without any were accepted silently")
	}
	if _, err := s.Check(gate, "svc", Pin{Sealed: true, Recipients: []string{"k1_a", "k1_b"}, SellerKeys: []string{"tp1"}, Signed: true}, Options{Accept: []string{"svc"}}); err != nil {
		t.Fatal(err)
	}
	out, err = s.Check(gate, "svc", Pin{Sealed: true, Recipients: []string{"k1_b"}, SellerKeys: []string{"tp1"}, Signed: true}, Options{PinRecipients: true})
	if err != nil || !slices.Equal(out.Changes, []Change{ChangeLessRecipient}) {
		t.Fatalf("a removed recipient: %+v, %v", out, err)
	}
	if pin, _, _ := s.Get(gate, "svc"); !slices.Equal(pin.Recipients, []string{"k1_b"}) {
		t.Fatalf("recorded %+v", pin)
	}
}

// TestConcurrentWriters: writers on separate opens of one file (as kontra connect per gate and kontra seal are)
// never lose each other's pins.
func TestConcurrentWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	const writers, each = 8, 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(p)
			if err != nil {
				errs <- err
				return
			}
			for i := range each {
				svc := fmt.Sprintf("svc-%d-%d", w, i)
				if _, err := s.Check(gate, svc, Pin{Sealed: true, Recipients: []string{"k1_" + svc}}, Options{}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for w := range writers {
		for i := range each {
			svc := fmt.Sprintf("svc-%d-%d", w, i)
			pin, ok, err := s.Get(gate, svc)
			if err != nil || !ok || !slices.Equal(pin.Recipients, []string{"k1_" + svc}) {
				t.Fatalf("%s lost: %+v, %v, %v", svc, pin, ok, err)
			}
		}
	}
}

func TestFileModes(t *testing.T) {
	for _, m := range []os.FileMode{0o604, 0o640, 0o660, 0o644, 0o606} {
		p := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(p, []byte(`{"v":1,"pins":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, m)
		if _, err := Open(p); err == nil || !strings.Contains(err.Error(), "group or others") {
			t.Fatalf("mode %04o: Open = %v", m, err)
		}
	}
	// A file that became readable after Open is refused by Check and Get too.
	s, p := open(t)
	if _, err := s.Check(gate, "svc", base, Options{}); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o644)
	if _, err := s.Check(gate, "svc", base, Options{}); err == nil {
		t.Fatal("Check reads an others-readable file")
	}
	if _, _, err := s.Get(gate, "svc"); err == nil {
		t.Fatal("Get reads an others-readable file")
	}
	// Garbage and other versions are refused.
	for _, content := range []string{"not json", `{"v":2,"pins":{}}`} {
		p := filepath.Join(t.TempDir(), FileName)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := Open(p); err == nil {
			t.Fatalf("Open(%q) succeeds", content)
		}
	}
	if _, err := s.Check("", "svc", base, Options{}); err == nil {
		t.Fatal("a pin without a gate")
	}
}
