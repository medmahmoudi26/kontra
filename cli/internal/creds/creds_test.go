package creds

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAPasswordVerifiesAgainstItsOwnHash(t *testing.T) {
	pw, err := GeneratePassword(20)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Verify(pw, encoded)
	if err != nil || !ok {
		t.Fatalf("the password it was derived from did not verify: ok=%v err=%v", ok, err)
	}
	// NON-VACUOUS: a Verify that returned true unconditionally would pass the line above.
	if wrong, _ := Verify(pw+"x", encoded); wrong {
		t.Error("a different password verified — Verify is not comparing anything")
	}
}

func TestTwoHashesOfOnePasswordDiffer(t *testing.T) {
	// A fresh salt per hash, or the config leaks which two accounts share a password.
	a, _ := Hash("same-password")
	b, _ := Hash("same-password")
	if a == b {
		t.Fatal("two hashes of one password are identical — the salt is not random")
	}
	for _, h := range []string{a, b} {
		if ok, _ := Verify("same-password", h); !ok {
			t.Error("a salted hash stopped verifying")
		}
	}
}

func TestTheEncodedFormCarriesItsOwnCost(t *testing.T) {
	// Self-describing, so raising the cost later does not invalidate hashes already written.
	encoded, _ := Hash("x")
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != Scheme {
		t.Fatalf("unexpected encoding: %q", encoded)
	}
	if parts[1] != "32768" || parts[2] != "8" || parts[3] != "1" {
		t.Errorf("cost parameters are not in the string: %v", parts[1:4])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != SaltLen {
		t.Errorf("salt is %d bytes, want %d (err=%v)", len(salt), SaltLen, err)
	}
	sum, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(sum) != KeyLen {
		t.Errorf("hash is %d bytes, want %d (err=%v)", len(sum), KeyLen, err)
	}
}

func TestAMalformedHashLocksRatherThanOpens(t *testing.T) {
	// `false, error` and never `true`. A config whose hash somebody hand-edited into nonsense must
	// lock the console, not open it — the failure mode `watchdog.sh` shipped for years here.
	for _, bad := range []string{
		"", "garbage", "scrypt$x$8$1$AA$AA", "scrypt$32768$8$1$!!!$AA",
		"scrypt$32768$8$1$AA$!!!", "bcrypt$32768$8$1$AA$AA", "scrypt$1$8$1$AA$AA",
		"scrypt$32768$8$1$AA", // too few fields
	} {
		ok, err := Verify("anything", bad)
		if ok {
			t.Errorf("a malformed hash %q ADMITTED a password", bad)
		}
		if err == nil {
			t.Errorf("a malformed hash %q returned no error", bad)
		}
	}
}

func TestAGeneratedPasswordIsUnambiguousAndUnbiased(t *testing.T) {
	// It is PRINTED ONCE and typed off a terminal, so characters two fonts disagree about are a
	// support conversation: no O/0, no l/1/I.
	seen := map[rune]int{}
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword(24)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 24 {
			t.Fatalf("asked for 24 characters, got %d", len(pw))
		}
		for _, c := range pw {
			if strings.ContainsRune("O0lI1", c) {
				t.Fatalf("generated an ambiguous character %q", c)
			}
			if !strings.ContainsRune(passwordAlphabet, c) {
				t.Fatalf("generated %q, which is outside the alphabet", c)
			}
			seen[c]++
		}
	}
	// Rejection sampling, not modulo. With 4,800 characters over a 56-symbol alphabet every symbol
	// should appear; a modulo-biased generator still would, so this checks the SPREAD instead —
	// the biased form over-weights the first 256%56 = 32 symbols by ~2x.
	if len(seen) < len(passwordAlphabet) {
		t.Errorf("only %d of %d symbols ever appeared", len(seen), len(passwordAlphabet))
	}
	lo, hi := 1<<30, 0
	for _, n := range seen {
		if n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
	}
	if hi > lo*2 {
		t.Errorf("symbol frequency spread %d..%d looks biased — modulo rather than rejection?", lo, hi)
	}
}
